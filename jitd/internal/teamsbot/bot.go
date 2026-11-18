package teamsbot

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/atoonk/tailscale-jit/jitd/internal/jit"
	"github.com/atoonk/tailscale-jit/jitd/internal/teams"
)

// Bot is the Microsoft Teams integration. It is safe to use before Start
// and while Teams is not set up: it then does nothing.
type Bot struct {
	Svc     *jit.Service
	Log     *slog.Logger
	BaseURL string // e.g. http://jit; request links point here
	// EnvFile is jitd's env file; TEAMS_APP_ID, TEAMS_APP_PASSWORD and
	// TEAMS_TENANT_ID in it (or in the environment) take the place of
	// credentials saved on the page.
	EnvFile string
	// NewAPI makes the Teams client; teams.New if nil.
	NewAPI func(appID, secret, tenant string) teams.API
	// NewInbound makes the activity transport; a teams.Socket if nil. The
	// preview and the tests put a fake here.
	NewInbound func(api teams.API, h teams.Handler) teams.Inbound
	Now        func() time.Time

	// applyMu serialises apply, so two connects can't interleave. It is
	// held while the old socket stops; mu is not (see apply).
	applyMu sync.Mutex

	mu       sync.Mutex
	parent   context.Context
	settings Settings
	source   string // "ui", "env" or ""
	api      teams.API
	inbound  teams.Inbound
	stopSock context.CancelFunc
	sockDone chan struct{}

	// queue and kick are made once (init) and never change, so
	// RequestChanged, which jit.Service calls with its lock held, needs no
	// lock of the bot's.
	initOnce sync.Once
	queue    chan string // request IDs whose cards need a look
	kick     chan struct{}

	ids identities

	rmu      sync.Mutex
	rendered map[string]string // request ID -> digest of the card last sent

	cmu       sync.Mutex
	channels  []Channel
	channelAt time.Time

	// postFailed holds, per approval flow, why its last channel post
	// failed; a success clears it. It feeds the sidebar warning and the
	// request page.
	pmu        sync.Mutex
	postFailed map[string]string
}

// Channel is a channel the bot can post to, with the team it belongs to.
type Channel struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	TeamID   string `json:"team_id"`
	TeamName string `json:"team_name,omitempty"`
}

// MaxDMs caps the direct messages sent for one request.
const MaxDMs = 25

// reconcileEvery is how often the bot catches up on what it missed (jitd
// or Teams was down when a request was filed, a card update failed).
const reconcileEvery = 10 * time.Minute

func (b *Bot) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func (b *Bot) log() *slog.Logger {
	if b.Log == nil {
		return slog.Default()
	}
	return b.Log
}

func (b *Bot) newAPI(appID, secret, tenant string) teams.API {
	if b.NewAPI != nil {
		return b.NewAPI(appID, secret, tenant)
	}
	return teams.New(appID, secret, tenant)
}

func (b *Bot) newInbound(api teams.API, h teams.Handler) teams.Inbound {
	if b.NewInbound != nil {
		return b.NewInbound(api, h)
	}
	return &teams.Socket{API: api, Handle: h,
		Logf: func(f string, a ...any) { b.log().Info(fmt.Sprintf(f, a...)) }}
}

func (b *Bot) init() {
	b.initOnce.Do(func() {
		b.queue = make(chan string, 512)
		b.kick = make(chan struct{}, 1)
	})
}

// Start loads the connection and keeps it until ctx ends.
func (b *Bot) Start(ctx context.Context) {
	b.init()
	b.mu.Lock()
	b.parent = ctx
	b.mu.Unlock()

	if err := b.load(); err != nil {
		b.log().Error("teams: reading settings", "err", err)
	}
	go b.worker(ctx)
}

// load reads the settings (the env file wins over the page) and connects
// if there are credentials.
func (b *Bot) load() error {
	env, err := envSettings(b.EnvFile)
	if err != nil {
		return err
	}
	var saved Settings
	if _, err := b.Svc.Store.GetSetting(settingKey, &saved); err != nil {
		return err
	}
	var s Settings
	source := ""
	switch {
	case env.configured():
		// The teams the app is installed in are learned, not configured, so
		// they are kept from the store even when the credentials are not.
		s, source = saved, "env"
		s.AppID, s.AppPassword, s.TenantID = env.AppID, env.AppPassword, env.TenantID
		if len(env.AllowedDomains) > 0 {
			s.AllowedDomains = env.AllowedDomains
		}
	case saved.configured():
		s, source = saved, "ui"
	}
	if source == "" {
		return nil
	}
	b.apply(s, source)
	return nil
}

// apply switches to new settings and (re)starts the socket.
//
// The old socket is stopped without holding b.mu: its reader may be inside
// a handler that needs b.mu (client, settingsCopy), and waiting for it
// while holding b.mu would deadlock the bot and, through RequestChanged,
// every request action.
func (b *Bot) apply(s Settings, source string) {
	b.applyMu.Lock()
	defer b.applyMu.Unlock()
	b.init()

	b.mu.Lock()
	stop, done := b.stopSock, b.sockDone
	b.api, b.inbound, b.stopSock, b.sockDone = nil, nil, nil, nil
	b.settings, b.source = s, source
	parent := b.parent
	b.mu.Unlock()
	if stop != nil {
		stop()
		<-done
	}

	b.ids.reset()
	b.rmu.Lock()
	b.rendered = nil
	b.rmu.Unlock()
	b.cmu.Lock()
	b.channels = nil
	b.cmu.Unlock()
	b.pmu.Lock()
	b.postFailed = nil
	b.pmu.Unlock()
	if !s.configured() || parent == nil {
		return
	}
	api := b.newAPI(s.AppID, s.AppPassword, s.TenantID)
	in := b.newInbound(api, b.handle)
	ctx, cancel := context.WithCancel(parent)
	done = make(chan struct{})
	b.mu.Lock()
	b.api, b.inbound, b.stopSock, b.sockDone = api, in, cancel, done
	b.mu.Unlock()
	go func() {
		defer close(done)
		in.Run(ctx)
	}()
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

// client returns the Teams client, or nil when Teams is not set up.
func (b *Bot) client() teams.API {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.api
}

func (b *Bot) settingsCopy() Settings {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.settings
	s.AllowedDomains = slices.Clone(s.AllowedDomains)
	s.Installed = slices.Clone(s.Installed)
	return s
}

// Status describes the connection, without the client secret.
func (b *Bot) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	st := Status{
		Source: b.source, EnvFile: b.EnvFile, State: "off", ManifestPath: ManifestPath,
		AppID: b.settings.AppID, TenantID: b.settings.TenantID,
		PasswordHint:   hint(b.settings.AppPassword),
		AllowedDomains: jit.OrEmpty(slices.Clone(b.settings.AllowedDomains)),
		Installed:      slices.Clone(b.settings.Installed),
		ConnectedBy:    b.settings.ConnectedBy, ConnectedAt: b.settings.ConnectedAt,
	}
	if b.inbound != nil {
		s := b.inbound.Status()
		st.State, st.Since, st.LastError = cmp.Or(s.State, "connecting"), s.Since, s.LastError
		for _, g := range s.Geos {
			st.Geos = append(st.Geos, GeoState{Geo: g.Geo, State: g.State, LastError: g.LastError})
		}
	}
	return st
}

// Problem says what is wrong with a configured connection, for the warning
// dot in the sidebar, or "": the connection is down, requests could not be
// posted to a flow's channel, or the app is installed nowhere.
func (b *Bot) Problem() string {
	st := b.Status()
	if !st.Configured() {
		return ""
	}
	if st.State == "error" {
		return st.LastError
	}
	if len(st.Installed) == 0 {
		return "the JIT access app is not installed in any team yet, so there is nowhere to post"
	}
	b.pmu.Lock()
	defer b.pmu.Unlock()
	ids := slices.Sorted(maps.Keys(b.postFailed))
	if len(ids) == 0 {
		return ""
	}
	name := ids[0]
	if w, err := b.Svc.GetWorkflow(ids[0]); err == nil {
		name = w.Name
	}
	return fmt.Sprintf("requests for %q can't be posted to its Teams channel: %s", name, b.postFailed[ids[0]])
}

// RequestNote says why a request is missing from its flow's Teams channel,
// for the request page, or "".
func (b *Bot) RequestNote(r *jit.AccessRequest) string {
	if r.Status != jit.StatusPending || slices.ContainsFunc(r.Teams, func(m jit.TeamsMessage) bool { return m.Kind == "channel" }) {
		return ""
	}
	b.pmu.Lock()
	defer b.pmu.Unlock()
	if why := b.postFailed[r.WorkflowID]; why != "" {
		return "Not posted to Teams: " + why
	}
	return ""
}

func (b *Bot) setPostFailed(workflowID, why string) {
	b.pmu.Lock()
	defer b.pmu.Unlock()
	if why == "" {
		delete(b.postFailed, workflowID)
		return
	}
	if b.postFailed == nil {
		b.postFailed = map[string]string{}
	}
	b.postFailed[workflowID] = why
}

// ConnectInput is what an admin fills in on the Teams page.
type ConnectInput struct {
	AppID          string   `json:"app_id"`
	AppPassword    string   `json:"app_password"`
	TenantID       string   `json:"tenant_id"`
	AllowedDomains []string `json:"allowed_domains"`
}

// Connect checks the credentials against Microsoft, saves them and
// connects. It replaces an earlier connection made on the page; one set
// from the env file can only be changed there.
func (b *Bot) Connect(ctx context.Context, admin jit.Identity, in ConnectInput) (Status, error) {
	if b.Status().Source == "env" {
		return Status{}, conflictf("Teams is set up in %s (%s, %s and %s); change it there", b.EnvFile, EnvAppID, EnvPassword, EnvTenantID)
	}
	appID := strings.TrimSpace(in.AppID)
	secret := strings.TrimSpace(in.AppPassword)
	tenant := strings.TrimSpace(in.TenantID)
	switch {
	case !validGUID(appID):
		return Status{}, badf("the app ID is a GUID (Microsoft Entra admin centre → App registrations → your app → Application (client) ID)")
	case secret == "":
		return Status{}, badf("paste the client secret value (Entra → your app → Certificates & secrets → Client secrets → Value, not Secret ID)")
	case !validGUID(tenant):
		return Status{}, badf("the tenant ID is a GUID (Entra → your app → Directory (tenant) ID)")
	}
	domains := CleanDomains(in.AllowedDomains)
	if err := validDomains(domains); err != nil {
		return Status{}, badf("%v", err)
	}
	api := b.newAPI(appID, secret, tenant)
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	// Microsoft issues the token, so this proves the app ID, the secret and
	// the tenant go together. Whether socket mode is on shows up on the
	// first connect, which the page reports as the connection state.
	if _, err := api.Token(cctx); err != nil {
		return Status{}, badf("Microsoft refused these credentials: %v", err)
	}
	var saved Settings
	if _, err := b.Svc.Store.GetSetting(settingKey, &saved); err != nil {
		return Status{}, err
	}
	s := Settings{
		AppID: appID, AppPassword: secret, TenantID: tenant, AllowedDomains: domains,
		Installed:   saved.Installed, // learned, so it survives a reconnect
		ConnectedBy: admin.Login, ConnectedAt: b.now().UTC(),
	}
	if err := b.Svc.Store.PutSetting(settingKey, s); err != nil {
		return Status{}, err
	}
	b.log().Info("teams connected", "app_id", appID, "tenant", tenant, "by", admin.Login)
	b.apply(s, "ui")
	return b.Status(), nil
}

// SetDomains changes the allowed email domains.
func (b *Bot) SetDomains(admin jit.Identity, domains []string) (Status, error) {
	domains = CleanDomains(domains)
	if err := validDomains(domains); err != nil {
		return Status{}, badf("%v", err)
	}
	st := b.Status()
	if !st.Configured() {
		return Status{}, conflictf("Teams is not connected")
	}
	var saved Settings
	if _, err := b.Svc.Store.GetSetting(settingKey, &saved); err != nil {
		return Status{}, err
	}
	saved.AllowedDomains = domains
	if err := b.Svc.Store.PutSetting(settingKey, saved); err != nil {
		return Status{}, err
	}
	b.mu.Lock()
	b.settings.AllowedDomains = domains
	b.mu.Unlock()
	b.ids.reset()
	b.log().Info("teams allowed domains changed", "domains", domains, "by", admin.Login)
	return b.Status(), nil
}

// Disconnect forgets the credentials and stops.
//
// Unlike Slack, there is nothing to revoke: Microsoft has no call that
// invalidates a client secret, so the secret stays valid until someone
// deletes it in Entra. The page and the README both say so, because
// forgetting a live secret is not the same as retiring it.
func (b *Bot) Disconnect(ctx context.Context, admin jit.Identity) error {
	st := b.Status()
	if st.Source == "env" {
		return conflictf("Teams is set up in %s; remove %s, %s and %s there, then restart jitd", b.EnvFile, EnvAppID, EnvPassword, EnvTenantID)
	}
	if err := b.Svc.Store.DeleteSetting(settingKey); err != nil {
		return err
	}
	b.apply(Settings{}, "")
	b.log().Info("teams disconnected", "by", admin.Login)
	return nil
}

// rememberTeam records a team the app was installed in, so its channels
// can be listed and its roster read. ACS tells jitd this once, when the
// app is added, so it has to be kept.
func (b *Bot) rememberTeam(t InstalledTeam) {
	if t.ID == "" || t.ServiceURL == "" {
		return
	}
	t.SeenAt = b.now().UTC()
	b.mu.Lock()
	cur := slices.Clone(b.settings.Installed)
	b.mu.Unlock()

	changed := true
	if i := slices.IndexFunc(cur, func(x InstalledTeam) bool { return x.ID == t.ID }); i >= 0 {
		// Keep the name ACS last gave us, and notice a moved service URL.
		changed = cur[i].ServiceURL != t.ServiceURL || (t.Name != "" && cur[i].Name != t.Name)
		if t.Name == "" {
			t.Name = cur[i].Name
		}
		cur[i] = t
	} else {
		cur = append(cur, t)
	}
	if !changed {
		return
	}
	b.mu.Lock()
	b.settings.Installed = cur
	b.mu.Unlock()

	var saved Settings
	if _, err := b.Svc.Store.GetSetting(settingKey, &saved); err != nil {
		b.log().Warn("teams: reading settings to record an installation", "err", err)
		return
	}
	saved.Installed = cur
	if err := b.Svc.Store.PutSetting(settingKey, saved); err != nil {
		b.log().Warn("teams: recording an installation", "err", err)
		return
	}
	b.ids.reset() // the roster just grew
	b.cmu.Lock()
	b.channels = nil
	b.cmu.Unlock()
	b.log().Info("teams: app installed in a team", "team", cmp.Or(t.Name, t.ID))
}

// serviceURLFor is the ACS endpoint for a team, "" if jitd has not seen
// the app installed there.
func (b *Bot) serviceURLFor(teamID string) string {
	for _, t := range b.settingsCopy().Installed {
		if t.ID == teamID {
			return t.ServiceURL
		}
	}
	return ""
}

// anyServiceURL is an ACS endpoint for the tenant, for calls that are not
// tied to a particular team.
func (b *Bot) anyServiceURL() string {
	for _, t := range b.settingsCopy().Installed {
		if t.ServiceURL != "" {
			return t.ServiceURL
		}
	}
	return ""
}

// Channels lists the channels the bot can post to, across every team the
// app is installed in, cached for a few minutes.
func (b *Bot) Channels(ctx context.Context) ([]Channel, error) {
	api := b.client()
	if api == nil {
		return nil, nil
	}
	b.cmu.Lock()
	defer b.cmu.Unlock()
	if b.channels != nil && b.now().Sub(b.channelAt) < 5*time.Minute {
		return b.channels, nil
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var out []Channel
	var firstErr error
	for _, t := range b.settingsCopy().Installed {
		cs, err := api.Channels(cctx, t.ServiceURL, t.ID)
		if err != nil {
			b.log().Warn("teams: listing channels", "team", cmp.Or(t.Name, t.ID), "err", err)
			firstErr = cmp.Or(firstErr, err)
			continue
		}
		for _, c := range cs {
			out = append(out, Channel{ID: c.ID, Name: c.DisplayName(), TeamID: t.ID, TeamName: t.Name})
		}
	}
	if out == nil && firstErr != nil {
		return nil, firstErr
	}
	slices.SortFunc(out, func(a, b Channel) int {
		return cmp.Or(strings.Compare(a.TeamName, b.TeamName), strings.Compare(a.Name, b.Name))
	})
	b.channels, b.channelAt = out, b.now()
	return out, nil
}

// TestMessage posts a hello to a channel.
func (b *Bot) TestMessage(ctx context.Context, admin jit.Identity, channel string) error {
	api := b.client()
	if api == nil {
		return conflictf("Teams is not connected")
	}
	serviceURL := b.serviceURLForChannel(ctx, channel)
	if serviceURL == "" {
		return badf("jitd does not know that channel; pick one from the list")
	}
	card := teams.NewCard(
		teams.Heading("👋 Test message from JIT access"),
		teams.Text(fmt.Sprintf("Sent by %s. Access requests for flows that use this channel show up here.", esc(admin.Login))),
		teams.Actions(teams.OpenURL("Open JIT access", b.BaseURL)),
	)
	if _, err := api.SendActivity(ctx, serviceURL, channel, teams.Message(card)); err != nil {
		return badf("%s", explainPostError(err))
	}
	return nil
}

// serviceURLForChannel finds the endpoint for a channel by the team it is
// in.
func (b *Bot) serviceURLForChannel(ctx context.Context, channel string) string {
	cs, err := b.Channels(ctx)
	if err != nil {
		b.log().Warn("teams: looking up a channel's team", "err", err)
	}
	for _, c := range cs {
		if c.ID == channel {
			return b.serviceURLFor(c.TeamID)
		}
	}
	return ""
}

func explainPostError(err error) string {
	switch teams.ErrorCode(err) {
	case "BotNotInConversationRoster":
		return "the JIT access app is not in that team any more; add it again in Teams"
	case "ConversationNotFound":
		return "Teams can't find that channel; it may have been deleted"
	case "ForbiddenOperationException":
		return "Teams refused the message: the app may have been removed from that team"
	}
	return err.Error()
}

// ---- notifications ----

// RequestChanged queues a look at the request's Teams cards. It never
// blocks (see jit.Notifier): if the queue is full the change is dropped,
// and the next reconcile catches up.
//
// The event itself is not used: the worker re-reads the request and
// redraws every card from its current state, which is what lets reconcile
// catch up after downtime. Anything that must happen once per transition
// needs its own record on the request.
func (b *Bot) RequestChanged(r *jit.AccessRequest, ev jit.Event) {
	b.init()
	select {
	case b.queue <- r.ID:
	default:
		b.log().Warn("teams: event queue full; the next reconcile will catch up", "id", r.ID)
	}
}

// worker handles changes one at a time, in order, so a card is posted
// before it is updated.
func (b *Bot) worker(ctx context.Context) {
	t := time.NewTicker(reconcileEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-b.queue:
			if api := b.client(); api != nil {
				b.sync(ctx, api, id)
			}
		case <-b.kick:
			b.reconcile(ctx)
		case <-t.C:
			b.reconcile(ctx)
		}
	}
}

// sync brings a request's Teams cards up to date: it posts them if the
// request is pending and was not announced yet, and updates the ones that
// were.
func (b *Bot) sync(ctx context.Context, api teams.API, id string) {
	r := b.Svc.Request(id)
	if r == nil {
		return
	}
	w, err := b.Svc.GetWorkflow(r.WorkflowID)
	if err != nil {
		w = nil
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	switch {
	case w == nil || w.Teams == nil:
	case r.Status == jit.StatusPending && !w.AutoApprove:
		b.announce(cctx, api, r, w)
	case (r.Status == jit.StatusApproved || r.Status == jit.StatusScheduled) && r.AutoApproved() && len(r.Teams) == 0:
		// An automatic flow asks nobody: no card with Approve and Decline
		// while the grant is being made, and one card in the channel, for
		// the record, once it is (or, for a request that starts later,
		// once it is scheduled).
		b.announce(cctx, api, r, w)
	}
	if r.GroupsStuck() {
		b.alertAdmins(cctx, api, r)
	}
	if len(r.Teams) > 0 {
		b.refresh(cctx, api, r, w)
	}
}

// requestURL is the page of request r.
func (b *Bot) requestURL(r *jit.AccessRequest) string {
	return strings.TrimSuffix(b.BaseURL, "/") + "/requests/" + r.ID
}

func (b *Bot) card(ctx context.Context, r *jit.AccessRequest, w *jit.Workflow) teams.Card {
	in := renderInput{R: r, W: w, URL: b.requestURL(r), Buttons: buttonsFor(w), Counted: len(r.Approvals)}
	if w != nil {
		cr := *r
		b.Svc.CountVotes(ctx, &cr)
		in.Counted = cr.ApprovalsCounted
	}
	return render(in)
}

// announce posts a new request to the flow's channel and, if the flow says
// so, to each approver, unless that was done already. r.Teams is updated.
func (b *Bot) announce(ctx context.Context, api teams.API, r *jit.AccessRequest, w *jit.Workflow) {
	has := func(kind string) bool {
		return slices.ContainsFunc(r.Teams, func(m jit.TeamsMessage) bool { return m.Kind == kind })
	}
	wantChannel := w.Teams.Channel != "" && !has("channel")
	wantDMs := w.Teams.DMApprovers && !has("dm") && r.Status == jit.StatusPending // nothing to decide otherwise
	if !wantChannel && !wantDMs {
		return
	}
	card := b.card(ctx, r, w)
	var posted []jit.TeamsMessage
	if wantChannel {
		serviceURL := b.serviceURLFor(w.Teams.TeamID)
		switch {
		case serviceURL == "":
			why := "the JIT access app is not installed in that team"
			b.log().Warn("teams: posting a request to its channel", "id", r.ID, "team", w.Teams.TeamID, "err", why)
			b.setPostFailed(w.ID, why)
		default:
			id, err := api.SendActivity(ctx, serviceURL, w.Teams.Channel, teams.Message(card))
			if err != nil {
				why := explainPostError(err)
				b.log().Warn("teams: posting a request to its channel", "id", r.ID, "channel", w.Teams.Channel, "err", why)
				b.setPostFailed(w.ID, why)
			} else {
				b.setPostFailed(w.ID, "")
				posted = append(posted, jit.TeamsMessage{Conversation: w.Teams.Channel, ActivityID: id, ServiceURL: serviceURL, Kind: "channel"})
			}
		}
	}
	if wantDMs {
		posted = append(posted, b.dmApprovers(ctx, api, r, w, card)...)
	}
	if len(posted) == 0 {
		return
	}
	if err := b.Svc.RecordTeamsMessages(r.ID, posted); err != nil {
		b.log().Error("teams: recording posted cards", "id", r.ID, "err", err)
	}
	r.Teams = append(r.Teams, posted...)
	b.setRendered(r.ID, digest(card))
}

// dmApprovers sends the request to each approver jitd can reach in Teams,
// except the requester.
//
// "Can reach" is narrower than in Slack: ACS refuses a 1:1 with anyone who
// has not installed the app and shares no team with it, so an approver in
// none of the installed teams simply does not get one. The Teams page
// lists who that is.
func (b *Bot) dmApprovers(ctx context.Context, api teams.API, r *jit.AccessRequest, w *jit.Workflow, card teams.Card) []jit.TeamsMessage {
	var out []jit.TeamsMessage
	logins, err := b.Svc.ApproverLogins(ctx, w)
	if err != nil {
		b.log().Warn("teams: direct messages to approvers", "id", r.ID, "err", err)
	}
	serviceURL := cmp.Or(b.serviceURLFor(w.Teams.TeamID), b.anyServiceURL())
	if serviceURL == "" {
		return nil
	}
	set := b.settingsCopy()
	for _, login := range logins {
		if len(out) >= MaxDMs {
			b.log().Warn("teams: more approvers than direct messages allowed; the rest see the channel or the browser", "id", r.ID, "max", MaxDMs)
			break
		}
		if !r.Requester.Tagged() && strings.EqualFold(login, r.Requester.Login) {
			continue
		}
		m, ok := b.teamsMember(ctx, api, login)
		if !ok || b.memberProblem(m) != "" {
			continue
		}
		conv, err := api.CreateConversation(ctx, serviceURL, cmp.Or(m.TenantID, set.TenantID), m.ID)
		if err != nil {
			b.log().Warn("teams: opening a direct message", "id", r.ID, "approver", login, "err", explainPostError(err))
			continue
		}
		id, err := api.SendActivity(ctx, serviceURL, conv, teams.Message(card))
		if err != nil {
			b.log().Warn("teams: direct message to an approver", "id", r.ID, "approver", login, "err", explainPostError(err))
			continue
		}
		out = append(out, jit.TeamsMessage{Conversation: conv, ActivityID: id, ServiceURL: serviceURL, Kind: "dm", User: m.ID})
	}
	return out
}

// kindGroupAlert marks, in a request's Teams cards, the direct messages
// that told the admins its group memberships (Okta, Google) could not be
// removed. The stored value matches Slack's, which dates from when Okta was
// the only kind.
const kindGroupAlert = "okta_alert"

func hasGroupAlert(r *jit.AccessRequest) bool {
	return slices.ContainsFunc(r.Teams, func(m jit.TeamsMessage) bool { return m.Kind == kindGroupAlert })
}

// alertAdmins tells every admin jitd can reach in Teams, once, that r has
// ended but its group memberships are still in place: one card per request,
// which covers every kind of group it is about. The flow's own Teams
// settings do not matter here: this is about jitd, not about the request.
func (b *Bot) alertAdmins(ctx context.Context, api teams.API, r *jit.AccessRequest) {
	if hasGroupAlert(r) {
		return
	}
	logins, err := b.Svc.AdminLogins(ctx)
	if err != nil {
		b.log().Warn("teams: direct messages to admins", "id", r.ID, "err", err)
	}
	serviceURL := b.anyServiceURL()
	if serviceURL == "" {
		return // the app is in no team: there is nobody to reach
	}
	card := renderGroupAlert(r, b.requestURL(r))
	set := b.settingsCopy()
	var posted []jit.TeamsMessage
	for _, login := range logins {
		if len(posted) >= MaxDMs {
			break
		}
		m, ok := b.teamsMember(ctx, api, login)
		if !ok || b.memberProblem(m) != "" {
			continue
		}
		conv, err := api.CreateConversation(ctx, serviceURL, cmp.Or(m.TenantID, set.TenantID), m.ID)
		if err != nil {
			b.log().Warn("teams: opening a direct message", "id", r.ID, "admin", login, "err", explainPostError(err))
			continue
		}
		id, err := api.SendActivity(ctx, serviceURL, conv, teams.Message(card))
		if err != nil {
			b.log().Warn("teams: direct message to an admin", "id", r.ID, "admin", login, "err", explainPostError(err))
			continue
		}
		posted = append(posted, jit.TeamsMessage{Conversation: conv, ActivityID: id, ServiceURL: serviceURL, Kind: kindGroupAlert, User: m.ID})
	}
	if len(posted) == 0 {
		return
	}
	if err := b.Svc.RecordTeamsMessages(r.ID, posted); err != nil {
		b.log().Error("teams: recording posted cards", "id", r.ID, "err", err)
	}
	r.Teams = append(r.Teams, posted...)
}

// refresh updates every card posted about r, unless it would not change.
func (b *Bot) refresh(ctx context.Context, api teams.API, r *jit.AccessRequest, w *jit.Workflow) {
	card := b.card(ctx, r, w)
	d := digest(card)
	var alert teams.Card
	if hasGroupAlert(r) {
		alert = renderGroupAlert(r, b.requestURL(r))
		d += digest(alert)
	}
	if b.renderedDigest(r.ID) == d {
		return
	}
	failed := false
	for _, m := range r.Teams {
		card := card
		if m.Kind == kindGroupAlert {
			card = alert
		}
		if err := api.UpdateActivity(ctx, m.ServiceURL, m.Conversation, m.ActivityID, teams.Message(card)); err != nil {
			switch teams.ErrorCode(err) {
			case "ActivityNotFound", "ConversationNotFound", "MessageNotFound":
				continue // deleted in Teams: nothing to update
			}
			failed = true
			b.log().Warn("teams: updating a request card", "id", r.ID, "conversation", m.Conversation, "err", err)
		}
	}
	if !failed {
		b.setRendered(r.ID, d)
	}
}

func (b *Bot) renderedDigest(id string) string {
	b.rmu.Lock()
	defer b.rmu.Unlock()
	return b.rendered[id]
}

func (b *Bot) setRendered(id, d string) {
	b.rmu.Lock()
	defer b.rmu.Unlock()
	if b.rendered == nil {
		b.rendered = map[string]string{}
	}
	b.rendered[id] = d
}

// reconcile catches up: pending requests not yet announced get posted, and
// recently changed requests get their cards updated if they are out of
// date (after a restart, every one of them is looked at once).
func (b *Bot) reconcile(ctx context.Context) {
	api := b.client()
	if api == nil {
		return
	}
	since := b.now().Add(-48 * time.Hour)
	rs := b.Svc.Requests(func(r *jit.AccessRequest) bool {
		if r.GroupsStuck() {
			return true // the admins may not have been told yet
		}
		for _, set := range r.GroupSets() {
			if g := set.Grant; g != nil && g.AlertedAt != nil && g.RemovedAt != nil && g.RemovedAt.After(since) && len(r.Teams) > 0 {
				return true // nor that it went through
			}
		}
		if (r.Status == jit.StatusApproved || r.Status == jit.StatusScheduled) && len(r.Teams) == 0 && r.AutoApproved() && r.UpdatedAt.After(since) {
			return true // an automatic grant is posted once it is made, and that may have been missed
		}
		return r.Status == jit.StatusPending || (len(r.Teams) > 0 && r.UpdatedAt.After(since))
	})
	slices.SortFunc(rs, func(a, b *jit.AccessRequest) int { return a.CreatedAt.Compare(b.CreatedAt) })
	for _, r := range rs {
		if ctx.Err() != nil {
			return
		}
		b.sync(ctx, api, r.ID)
	}
}

// ---- errors ----

func badf(format string, args ...any) error {
	return &jit.Error{Code: "bad_request", Status: 400, Message: fmt.Sprintf(format, args...)}
}

func conflictf(format string, args ...any) error {
	return &jit.Error{Code: "conflict", Status: 409, Message: fmt.Sprintf(format, args...)}
}

// errNotConnected is returned by checks that need Teams.
var errNotConnected = errors.New("Teams is not connected")

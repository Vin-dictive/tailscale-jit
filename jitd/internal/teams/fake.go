package teams

// Fake is an in-memory Teams: a tenant with some people in it, a team or
// two with channels, and a socket a test drives by hand. It is in the
// normal build, not just tests, because the preview runs the whole Teams
// integration against it with no Microsoft tenant anywhere.
//
// The socket is faked at the Inbound seam rather than at the WebSocket:
// the SignalR protocol has its own tests against a real WebSocket in
// signalr_test.go, and everything above it only cares that envelopes
// arrive and replies come back.

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

// Fake implements API.
type Fake struct {
	// TenantID is the tenant everything in this fake belongs to.
	TenantID string
	// ServiceURL is the ACS endpoint activities claim to come from.
	ServiceURL string

	mu       sync.Mutex
	appID    string
	users    map[string]Member   // by "29:…" ID
	rosters  map[string][]string // conversation ID -> user IDs
	channels map[string][]Channel
	dms      map[string]string // user ID -> 1:1 conversation ID
	posts    map[string][]*Post
	fail     map[string]*APIError
	nextID   int
	inbound  *FakeInbound
}

// Post is one activity the fake was asked to deliver.
type Post struct {
	Conversation string
	ActivityID   string
	ServiceURL   string
	Activity     Activity
	// Updates counts how many times the activity was replaced afterwards.
	Updates int
}

// Card returns the Adaptive Card on the post as JSON, empty if it has none.
// Tests assert against this the way the Slack ones assert on blocks.
func (p Post) Card() string {
	for _, a := range p.Activity.Attachments {
		if a.ContentType == InvokeCard {
			b, err := json.Marshal(a.Content)
			if err == nil {
				return string(b)
			}
		}
	}
	return ""
}

// NewFake is an empty Teams tenant.
func NewFake() *Fake {
	return &Fake{
		TenantID:   "11111111-2222-3333-4444-555555555555",
		ServiceURL: "https://smba.trafficmanager.net/amer/",
		appID:      "00000000-aaaa-bbbb-cccc-000000000001",
		users:      map[string]Member{},
		rosters:    map[string][]string{},
		channels:   map[string][]Channel{},
		dms:        map[string]string{},
		posts:      map[string][]*Post{},
		fail:       map[string]*APIError{},
	}
}

// AddUser adds a member of the organisation.
func (f *Fake) AddUser(id, aadObjectID, email, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[id] = Member{
		ID:                id,
		AADObjectID:       aadObjectID,
		Name:              name,
		Email:             email,
		UserPrincipalName: email,
		TenantID:          f.TenantID,
	}
}

// MakeGuest turns a user into a guest from another organisation, which is
// what a "#EXT#" principal name means.
func (f *Fake) MakeGuest(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return
	}
	local, domain, _ := strings.Cut(u.Email, "@")
	u.UserPrincipalName = local + "_" + domain + "#EXT#@contoso.onmicrosoft.com"
	f.users[id] = u
}

// SetUser changes one user, for tests that need an odd one.
func (f *Fake) SetUser(id string, change func(*Member)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	u, ok := f.users[id]
	if !ok {
		return
	}
	change(&u)
	f.users[id] = u
}

// AddTeam registers a team with its channels. The team's own ID doubles as
// the conversation ID of its General channel, as it does in Teams.
func (f *Fake) AddTeam(teamID string, channels ...Channel) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.channels[teamID] = append(f.channels[teamID], channels...)
}

// SetRoster says who is in a conversation. A user has to be in one for the
// bot to reach them: ACS refuses a direct message to anyone who has not
// installed the app and shares no team with it.
func (f *Fake) SetRoster(conversation string, userIDs ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rosters[conversation] = slices.Clone(userIDs)
}

// SetFail makes the named call fail until it is cleared. Call names are
// the ones in APIError.Call: "send", "update", "create conversation",
// "member", "members", "channels", "connect", "token".
func (f *Fake) SetFail(call, code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if code == "" {
		delete(f.fail, call)
		return
	}
	f.fail[call] = &APIError{Call: call, Status: http.StatusForbidden, Code: code, Message: code}
}

// SetFailStatus is SetFail with a status, for retry and terminal paths.
func (f *Fake) SetFailStatus(call, code string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[call] = &APIError{Call: call, Status: status, Code: code, Message: code}
}

func (f *Fake) failure(call string) error {
	if e, ok := f.fail[call]; ok {
		return e
	}
	return nil
}

func (f *Fake) id(prefix string) string {
	f.nextID++
	return fmt.Sprintf("%s%d", prefix, f.nextID)
}

func (f *Fake) AppID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.appID
}

// SetAppID changes the bot's app ID.
func (f *Fake) SetAppID(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.appID = id
}

func (f *Fake) Token(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("token"); err != nil {
		return "", err
	}
	return "fake-access-token", nil
}

func (f *Fake) Connect(ctx context.Context, geo, replaces string) (Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("connect"); err != nil {
		return Session{}, err
	}
	return Session{
		URL:             "https://fake.service.signalr.net/client/?hub=bothub",
		AccessToken:     f.id("socket-token-"),
		ExpiresIn:       3600,
		ProtocolVersion: 1,
	}, nil
}

func (f *Fake) SendActivity(ctx context.Context, serviceURL, conversation string, a Activity) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("send"); err != nil {
		return "", err
	}
	if conversation == "" {
		return "", &APIError{Call: "send", Status: http.StatusBadRequest, Code: "BadArgument", Message: "no conversation"}
	}
	p := &Post{Conversation: conversation, ActivityID: f.id("a"), ServiceURL: serviceURL, Activity: a}
	f.posts[conversation] = append(f.posts[conversation], p)
	return p.ActivityID, nil
}

func (f *Fake) UpdateActivity(ctx context.Context, serviceURL, conversation, activityID string, a Activity) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("update"); err != nil {
		return err
	}
	for _, p := range f.posts[conversation] {
		if p.ActivityID == activityID {
			p.Activity = a
			p.Updates++
			return nil
		}
	}
	return &APIError{Call: "update", Status: http.StatusNotFound, Code: "ActivityNotFound", Message: "no such activity"}
}

func (f *Fake) CreateConversation(ctx context.Context, serviceURL, tenantID, userID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("create conversation"); err != nil {
		return "", err
	}
	if id, ok := f.dms[userID]; ok {
		return id, nil
	}
	// ACS refuses a 1:1 with someone the app cannot reach. Sharing a
	// conversation with the bot is what the fake takes as "reachable".
	if !f.inAnyRoster(userID) {
		return "", &APIError{
			Call: "create conversation", Status: http.StatusForbidden,
			Code:    "ForbiddenOperationException",
			Message: "the app is not installed for this user and shares no team with them",
		}
	}
	id := f.id("19:dm")
	f.dms[userID] = id
	f.rosters[id] = []string{userID}
	return id, nil
}

func (f *Fake) inAnyRoster(userID string) bool {
	for _, ids := range f.rosters {
		if slices.Contains(ids, userID) {
			return true
		}
	}
	return false
}

func (f *Fake) Member(ctx context.Context, serviceURL, conversation, userID string) (Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("member"); err != nil {
		return Member{}, err
	}
	u, ok := f.users[userID]
	if !ok {
		return Member{}, &APIError{Call: "member", Status: http.StatusNotFound, Code: "MemberNotFoundInConversation", Message: "no such member"}
	}
	return u, nil
}

func (f *Fake) Members(ctx context.Context, serviceURL, conversation string) ([]Member, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("members"); err != nil {
		return nil, err
	}
	var out []Member
	for _, id := range f.rosters[conversation] {
		if u, ok := f.users[id]; ok {
			out = append(out, u)
		}
	}
	return out, nil
}

func (f *Fake) Channels(ctx context.Context, serviceURL, teamID string) ([]Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failure("channels"); err != nil {
		return nil, err
	}
	cs, ok := f.channels[teamID]
	if !ok {
		return nil, &APIError{Call: "channels", Status: http.StatusNotFound, Code: "TeamNotFound", Message: "no such team"}
	}
	return slices.Clone(cs), nil
}

// PostsTo returns what was posted to a conversation, in order.
//
// The posts are copies: the bot keeps updating the originals from its own
// goroutine, so handing out the pointers would race with any test that
// looked at one.
func (f *Fake) PostsTo(conversation string) []Post {
	f.mu.Lock()
	defer f.mu.Unlock()
	return copyPosts(f.posts[conversation])
}

// AllPosts returns every conversation that has posts, with them.
func (f *Fake) AllPosts() map[string][]Post {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string][]Post{}
	for k, v := range f.posts {
		out[k] = copyPosts(v)
	}
	return out
}

func copyPosts(ps []*Post) []Post {
	out := make([]Post, 0, len(ps))
	for _, p := range ps {
		out = append(out, *p)
	}
	return out
}

// DMTo is the 1:1 conversation opened with a user, empty if there is none.
func (f *Fake) DMTo(userID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dms[userID]
}

// FakeInbound stands in for socket mode. A test calls Deliver to hand the
// bot an activity and gets back whatever it would have replied to ACS.
type FakeInbound struct {
	mu      sync.Mutex
	handle  Handler
	running bool
	stopped chan struct{}
	seq     int
}

// Inbound returns the fake's socket, delivering to h.
func (f *Fake) Inbound(h Handler) Inbound {
	f.mu.Lock()
	defer f.mu.Unlock()
	in := &FakeInbound{handle: h, stopped: make(chan struct{})}
	f.inbound = in
	return in
}

// Socket is the fake's current inbound, nil until Inbound has been called.
func (f *Fake) Socket() *FakeInbound {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.inbound
}

func (i *FakeInbound) Run(ctx context.Context) {
	i.mu.Lock()
	i.running = true
	i.mu.Unlock()
	<-ctx.Done()
	i.mu.Lock()
	i.running = false
	i.mu.Unlock()
	close(i.stopped)
}

func (i *FakeInbound) Status() Status {
	i.mu.Lock()
	defer i.mu.Unlock()
	state := "off"
	if i.running {
		state = "connected"
	}
	out := Status{State: state}
	for _, geo := range Geos {
		out.Geos = append(out.Geos, GeoStatus{Geo: geo, State: state})
	}
	return out
}

// Connected reports whether Run is serving.
func (i *FakeInbound) Connected() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.running
}

// Deliver hands one envelope to the handler and returns its reply.
func (i *FakeInbound) Deliver(ctx context.Context, env Envelope) *InvokeResponse {
	i.mu.Lock()
	h := i.handle
	i.seq++
	if env.EnvelopeID == "" {
		env.EnvelopeID = fmt.Sprintf("env-%d", i.seq)
	}
	i.mu.Unlock()
	if h == nil {
		return nil
	}
	if env.DeadlineMs == 0 {
		env.DeadlineMs = int(maxReplyDeadline / time.Millisecond)
	}
	return h(ctx, env)
}

// Envelope builders, so tests say what happened rather than assembling
// wire structures.

// MessageTo is someone sending the bot a message.
func (f *Fake) MessageTo(userID, conversation, text string) Envelope {
	return f.envelope("activity", Activity{
		Type:         "message",
		Text:         text,
		From:         f.account(userID),
		Conversation: Conversation{ID: conversation, ConversationType: f.conversationType(conversation), TenantID: f.TenantID},
	})
}

// CardActionBy is someone pressing an Action.Execute button on a card.
func (f *Fake) CardActionBy(userID, conversation, verb string, data map[string]any) Envelope {
	if data == nil {
		data = map[string]any{}
	}
	data["verb"] = verb
	value, _ := json.Marshal(map[string]any{
		"action":  map[string]any{"type": "Action.Execute", "verb": verb, "data": data},
		"trigger": "manual",
	})
	return f.envelope("invoke", Activity{
		Type:         "invoke",
		Name:         "adaptiveCard/action",
		Value:        value,
		From:         f.account(userID),
		Conversation: Conversation{ID: conversation, ConversationType: f.conversationType(conversation), TenantID: f.TenantID},
	})
}

// InstalledIn is the conversationUpdate ACS sends when the app is added to
// a team: how jitd learns the team exists and where to answer it.
func (f *Fake) InstalledIn(teamID, teamName, conversation string) Envelope {
	return f.envelope("activity", Activity{
		Type:         "conversationUpdate",
		From:         Account{ID: "28:" + f.AppID()},
		Recipient:    Account{ID: "28:" + f.AppID()},
		MembersAdded: []Account{{ID: "28:" + f.AppID()}},
		Conversation: Conversation{ID: conversation, ConversationType: "channel", TenantID: f.TenantID},
		ChannelData: ChannelData{
			Team:   &TeamInfo{ID: teamID, Name: teamName},
			Tenant: &TenantInfo{ID: f.TenantID},
		},
	})
}

func (f *Fake) envelope(kind string, a Activity) Envelope {
	a.ServiceURL = f.ServiceURL
	if a.Recipient.ID == "" {
		a.Recipient = Account{ID: "28:" + f.AppID()}
	}
	if a.ChannelData.Tenant == nil {
		a.ChannelData.Tenant = &TenantInfo{ID: f.TenantID}
	}
	return Envelope{
		ProtocolVersion: 1,
		Type:            kind,
		BotKey:          f.AppID(),
		DeadlineMs:      int(maxReplyDeadline / time.Millisecond),
		AckRequired:     kind != "invoke",
		Payload:         a,
	}
}

func (f *Fake) account(userID string) Account {
	f.mu.Lock()
	defer f.mu.Unlock()
	u := f.users[userID]
	return Account{ID: userID, Name: u.Name, AADObjectID: u.AADObjectID}
}

// conversationType guesses from the ID, the way the IDs are shaped: a
// channel thread, or a 1:1.
func (f *Fake) conversationType(conversation string) string {
	switch {
	case strings.Contains(conversation, "thread.tacv2"):
		return "channel"
	case strings.HasPrefix(conversation, "19:dm"):
		return "personal"
	}
	return "personal"
}

// Users returns everyone in the fake tenant, for the preview to list.
func (f *Fake) Users() []Member {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Collect(maps.Values(f.users))
}

package teamsbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/atoonk/tailscale-jit/jitd/internal/jit"
	"github.com/atoonk/tailscale-jit/jitd/internal/teams"
)

// slowCall caps the work done after a message, which is answered by
// posting back rather than in the reply.
const slowCall = 20 * time.Second

// handle serves one activity (see teams.Handler).
func (b *Bot) handle(ctx context.Context, env teams.Envelope) *teams.InvokeResponse {
	api := b.client()
	if api == nil {
		return nil
	}
	a := &env.Payload
	// Everything jitd acts on comes from the connected tenant. ACS already
	// authenticated the bot, but a shared channel can carry activities from
	// elsewhere, and those are nobody jitd knows.
	if set := b.settingsCopy(); set.TenantID != "" && a.Tenant() != "" && !strings.EqualFold(a.Tenant(), set.TenantID) {
		b.log().Warn("teams: ignoring an activity from another tenant", "tenant", a.Tenant())
		return nil
	}
	switch a.Type {
	case "invoke":
		if a.Name != "adaptiveCard/action" {
			return nil
		}
		return b.onCardAction(ctx, api, a)
	case "message":
		// Answering needs Teams and maybe Tailscale calls and the service's
		// data. ACS counts a late reply as a failed delivery, so the
		// activity is acknowledged now and answered by posting back.
		go func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), slowCall)
			defer cancel()
			b.onMessage(ctx, api, a)
		}()
	case "conversationUpdate":
		b.onConversationUpdate(a)
	}
	return nil
}

// actionData is what a card's buttons carry back, with any inputs on the
// card folded in by Teams.
type actionData struct {
	Verb      string `json:"verb"`
	RequestID string `json:"request_id"`
	Reason    string `json:"reason"`
	Flow      string `json:"flow"`
	Device    string `json:"device"`
	Duration  string `json:"duration"`
}

// onCardAction handles a button on a card. Its return value replaces the
// card, or shows a line of text, for whoever pressed the button and
// nobody else: that is what lets Decline ask for a reason in a channel
// without the rest of the channel seeing the form.
func (b *Bot) onCardAction(ctx context.Context, api teams.API, a *teams.Activity) *teams.InvokeResponse {
	var v teams.CardAction
	if err := json.Unmarshal(a.Value, &v); err != nil {
		b.log().Warn("teams: bad card action", "err", err)
		return errResponse(400, "That button sent something jitd could not read.")
	}
	var d actionData
	json.Unmarshal(v.Action.Data, &d)
	verb := v.Action.Verb
	if verb == "" {
		verb = d.Verb // an Action.Submit fallback from an older client
	}

	switch verb {
	case verbRequest:
		return b.requestForm(ctx, api, a, "")
	case verbRequestSubmit:
		return b.onRequestSubmit(ctx, api, a, d)
	case verbApprove, verbDecline, verbDeclineSubmit:
	default:
		return nil
	}

	link := b.requestLink(d.RequestID)
	id, err := b.resolve(ctx, api, a.ServiceURL, a.Conversation.ID, a.From.ID)
	if err != nil {
		return errResponse(403, fmt.Sprintf("You can't act on this request from Teams: %s. You can still decide in the browser: %s", err, link))
	}
	r, err := b.Svc.GetRequest(ctx, id, d.RequestID)
	if err != nil {
		return errResponse(403, fmt.Sprintf("You can't act on this request: Teams matched you to %s, who isn't an approver for it.", id.Login))
	}
	if w, err := b.Svc.GetWorkflow(r.WorkflowID); err == nil && !buttonsFor(w) {
		// The buttons should not have been there; redraw so they go away.
		b.RequestChanged(r, jit.Event{})
		return errResponse(403, "This approval flow is decided in the browser only: "+link)
	}

	switch verb {
	case verbApprove:
		if _, err := b.Svc.Approve(ctx, id, d.RequestID, ""); err != nil {
			return errResponse(400, fmt.Sprintf("Couldn't approve: %s (Teams matched you to %s.)", jit.AsError(err).Message, id.Login))
		}
		b.log().Info("teams: approved", "id", d.RequestID, "login", id.Login, "teams_user", id.TeamsUser)
		// The card updates itself through the notifier.
		return msgResponse("Approved.")

	case verbDecline:
		if can := b.Svc.Permissions(ctx, id, r); !can.Decline {
			why := "you can't decline it"
			if r.Status != jit.StatusPending {
				why = "it is " + string(r.Status)
			} else if strings.EqualFold(r.Requester.Login, id.Login) {
				why = "it is your own request; withdraw it in the browser instead"
			}
			return errResponse(403, "Couldn't decline: "+why+".")
		}
		return cardResponse(declineForm(r, ""))

	default: // verbDeclineSubmit
		reason := strings.TrimSpace(d.Reason)
		if reason == "" {
			return cardResponse(declineForm(r, "A reason is required to decline."))
		}
		if _, err := b.Svc.Decline(ctx, id, d.RequestID, reason); err != nil {
			return errResponse(400, fmt.Sprintf("Couldn't decline: %s.", jit.AsError(err).Message))
		}
		b.log().Info("teams: declined", "id", d.RequestID, "login", id.Login, "teams_user", id.TeamsUser)
		return cardResponse(noticeCard("Declined", fmt.Sprintf("You declined %s for %s.", r.WorkflowName, r.Requester.Actor())))
	}
}

// onConversationUpdate notices the app being added to a team, which is the
// only way jitd learns the team exists and which endpoint answers it.
func (b *Bot) onConversationUpdate(a *teams.Activity) {
	self := "28:" + b.client().AppID()
	added := false
	for _, m := range a.MembersAdded {
		if strings.EqualFold(m.ID, self) {
			added = true
			break
		}
	}
	if !added || a.TeamID() == "" {
		return
	}
	name := ""
	if a.ChannelData.Team != nil {
		name = a.ChannelData.Team.Name
	}
	b.rememberTeam(InstalledTeam{
		ID: a.TeamID(), Name: name, ServiceURL: a.ServiceURL, TenantID: a.Tenant(),
	})
}

// onMessage answers a message to the bot: the /jit commands.
func (b *Bot) onMessage(ctx context.Context, api teams.API, a *teams.Activity) {
	word, _, _ := strings.Cut(commandText(a), " ")
	switch strings.ToLower(word) {
	case "request":
		b.say(ctx, api, a, b.requestFormCard(ctx, api, a, ""))
	case "help":
		b.say(ctx, api, a, commandList())
	case "", "status":
		b.say(ctx, api, a, b.summary(ctx, api, a))
	default:
		b.say(ctx, api, a, teams.NewCard(
			teams.Text(fmt.Sprintf("`%s` isn't a JIT access command. These are:", esc(word))),
			commandListBody(),
		))
	}
}

// say posts a card back into the conversation the activity came from.
func (b *Bot) say(ctx context.Context, api teams.API, a *teams.Activity, card teams.Card) {
	if _, err := api.SendActivity(ctx, a.ServiceURL, a.Conversation.ID, teams.Message(card)); err != nil {
		b.log().Warn("teams: answering a command", "err", err)
	}
}

// atTag matches a whole <at>…</at> mention. The name inside goes with it:
// stripping only the tags would leave "JIT access" in the text, and the
// command parser would then read the bot's own name as the command.
var atTag = regexp.MustCompile(`(?is)<at\b[^>]*>.*?</at>`)

// anyTag matches whatever markup is left over.
var anyTag = regexp.MustCompile(`<[^>]*>`)

// commandText is a message's text with the bot's @mention and a leading
// "/jit" taken off, so "@JIT access request" and "/jit request" and
// "request" all mean the same.
func commandText(a *teams.Activity) string {
	t := a.Text
	// Strip the mention by its own text, which the entity gives verbatim,
	// then any leftover markup.
	for _, e := range a.Entities {
		if strings.EqualFold(e.Type, "mention") && e.Text != "" {
			t = strings.ReplaceAll(t, e.Text, " ")
		}
	}
	t = atTag.ReplaceAllString(t, " ")
	t = anyTag.ReplaceAllString(t, " ")
	t = strings.TrimSpace(strings.Join(strings.Fields(t), " "))
	for _, prefix := range []string{"/jit", "jit"} {
		if rest, ok := cutWord(t, prefix); ok {
			t = rest
			break
		}
	}
	return strings.TrimSpace(t)
}

// cutWord takes a leading whole word off s, case-insensitively.
func cutWord(s, word string) (string, bool) {
	if !strings.HasPrefix(strings.ToLower(s), strings.ToLower(word)) {
		return s, false
	}
	rest := s[len(word):]
	if rest != "" && !strings.HasPrefix(rest, " ") {
		return s, false
	}
	return strings.TrimSpace(rest), true
}

// summary answers /jit: what is waiting for the caller and where their own
// requests are.
func (b *Bot) summary(ctx context.Context, api teams.API, a *teams.Activity) teams.Card {
	base := strings.TrimSuffix(b.BaseURL, "/")
	id, err := b.resolve(ctx, api, a.ServiceURL, a.Conversation.ID, a.From.ID)
	if err != nil {
		var ie *IdentityError
		if !errors.As(err, &ie) {
			err = errors.New("Teams could not be asked who you are; try again")
		}
		return teams.NewCard(
			teams.Heading("JIT access"),
			teams.Text("JIT access can't tell who you are: "+esc(err.Error())+"."),
			teams.Actions(teams.OpenURL("Open JIT access", base)),
		)
	}
	now := b.now()
	waiting := b.Svc.ListRequests(ctx, jit.ListFilter{Viewer: id, Scope: jit.ScopeApprovals, Status: jit.StatusPending})
	var mine []*jit.AccessRequest
	for _, r := range b.Svc.ListRequests(ctx, jit.ListFilter{Viewer: id, Scope: jit.ScopeMine}) {
		if r.Status == jit.StatusPending || ((r.Status == jit.StatusApproved || r.Status == jit.StatusScheduled) && r.ExpiresAt != nil && r.ExpiresAt.After(now)) {
			mine = append(mine, r)
		}
	}
	body := []teams.Element{
		teams.Heading("JIT access"),
		teams.Subtle(fmt.Sprintf("You are %s (from your Teams address).", esc(id.Login))),
		teams.Text(fmt.Sprintf("**📥 Waiting for your approval (%d)**", len(waiting))),
	}
	if len(waiting) == 0 {
		body = append(body, teams.Subtle("Nothing right now."))
	}
	for i, r := range waiting {
		if i == 8 {
			body = append(body, teams.Subtle(fmt.Sprintf("…and %d more.", len(waiting)-8)))
			break
		}
		body = append(body, teams.Text(fmt.Sprintf("• [%s](%s) · %s for %s, on `%s`\n\n  %s",
			esc(r.WorkflowName), b.requestLink(r.ID), esc(r.Requester.Actor()), r.Duration.Human(),
			esc(r.Requester.DeviceName), esc(oneLine(r.Reason, 120)))))
	}
	body = append(body, teams.Text(fmt.Sprintf("**🔑 Your requests (%d)**", len(mine))))
	if len(mine) == 0 {
		body = append(body, teams.Subtle("No pending or active requests."))
	}
	for i, r := range mine {
		if i == 8 {
			body = append(body, teams.Subtle(fmt.Sprintf("…and %d more.", len(mine)-8)))
			break
		}
		state := "⏳ waiting for approval"
		switch {
		case r.Status == jit.StatusApproved:
			state = "✅ active until " + cardTime(*r.ExpiresAt)
		case r.Status == jit.StatusScheduled:
			state = "📅 scheduled: " + cardTime(*r.StartAt) + " to " + cardTime(*r.ExpiresAt)
		case r.StartAt != nil:
			state += ", for " + cardTime(*r.StartAt)
		}
		body = append(body, teams.Text(fmt.Sprintf("• [%s](%s) on `%s` — %s",
			esc(r.WorkflowName), b.requestLink(r.ID), esc(r.Requester.DeviceName), state)))
	}
	body = append(body, commandListBody(),
		teams.Actions(
			teams.Execute(verbRequest, "Request access", "positive", nil),
			teams.OpenURL("Open JIT access", base),
		))
	return teams.NewCard(body...)
}

// commandList is the card /jit help answers with.
func commandList() teams.Card {
	return teams.NewCard(
		teams.Heading("JIT access"),
		commandListBody(),
		teams.Actions(teams.Execute(verbRequest, "Request access", "positive", nil)),
	)
}

func commandListBody() teams.Element {
	return teams.Facts(
		teams.Fact{Title: "/jit request", Value: "ask for access for one of your devices"},
		teams.Fact{Title: "/jit", Value: "what is waiting for your approval, and your own requests"},
		teams.Fact{Title: "/jit help", Value: "this list"},
	)
}

func (b *Bot) requestLink(id string) string {
	return strings.TrimSuffix(b.BaseURL, "/") + "/requests/" + id
}

func cardResponse(c teams.Card) *teams.InvokeResponse {
	r := teams.CardResponse(c)
	return &r
}

func msgResponse(s string) *teams.InvokeResponse {
	r := teams.MessageResponse(s)
	return &r
}

func errResponse(status int, s string) *teams.InvokeResponse {
	r := teams.ErrorResponse(status, s)
	return &r
}

func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// ---- checks for the admin page ----

// ApproverCheck is whether one approver can act from Teams.
type ApproverCheck struct {
	Login     string `json:"login"`
	TeamsUser string `json:"teams_user,omitempty"` // their Teams display name, if found
	Problem   string `json:"problem,omitempty"`
}

// FlowCheck is a flow's Teams setup as Teams sees it.
type FlowCheck struct {
	WorkflowID   string          `json:"workflow_id"`
	WorkflowName string          `json:"workflow_name"`
	Channel      string          `json:"channel,omitempty"`
	ChannelName  string          `json:"channel_name,omitempty"`
	ChannelIssue string          `json:"channel_issue,omitempty"`
	DMApprovers  bool            `json:"dm_approvers"`
	BrowserOnly  bool            `json:"browser_only"`
	Approvers    []ApproverCheck `json:"approvers"`
	// ApproverIssue says why the list of approvers may be incomplete
	// (group: approvers whose members could not be read).
	ApproverIssue string `json:"approver_issue,omitempty"`
	// Members is set when autogroup:member approves: any tailnet member,
	// from Teams too if their work address is their Tailscale login.
	Members bool `json:"members,omitempty"`
	// OtherApprovers are tag: approvers: devices, which decide in the
	// browser or through the API only.
	OtherApprovers []string `json:"other_approvers,omitempty"`
}

// Check looks at every flow: can the bot post to its channel, and which of
// its approvers Teams can match to their Tailscale login.
func (b *Bot) Check(ctx context.Context) ([]FlowCheck, error) {
	api := b.client()
	if api == nil {
		return nil, errNotConnected
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Teams users must own a device on the tailnet (see resolve); one
	// device list serves every approver.
	owners, err := b.Svc.DeviceOwners(ctx)
	if err != nil {
		b.log().Warn("teams: checking approvers' devices", "err", err)
	}
	channels, err := b.Channels(ctx)
	if err != nil {
		b.log().Warn("teams: listing channels for the check", "err", err)
	}
	known := map[string]Channel{}
	for _, c := range channels {
		known[c.ID] = c
	}
	var out []FlowCheck
	for _, w := range b.Svc.ListWorkflows() {
		fc := FlowCheck{WorkflowID: w.ID, WorkflowName: w.Name, Approvers: []ApproverCheck{}}
		if w.Teams != nil {
			fc.Channel, fc.ChannelName, fc.DMApprovers, fc.BrowserOnly = w.Teams.Channel, w.Teams.ChannelName, w.Teams.DMApprovers, w.Teams.BrowserOnly
		}
		if fc.Channel != "" {
			switch c, ok := known[fc.Channel]; {
			case ok:
				fc.ChannelName = c.Name
			case len(channels) == 0:
				fc.ChannelIssue = "jitd cannot list channels; is the JIT access app still installed?"
			default:
				fc.ChannelIssue = "this channel is gone, or the app was removed from its team"
			}
		}
		for _, p := range w.Approvers {
			switch {
			case p == jit.AutogroupMember:
				fc.Members = true
			case !p.IsLogin() && !p.IsOkta() && !strings.HasPrefix(string(p), "group:"):
				fc.OtherApprovers = append(fc.OtherApprovers, string(p))
			}
		}
		logins, err := b.Svc.ApproverLogins(ctx, w)
		if err != nil {
			fc.ApproverIssue = err.Error()
		}
		for _, login := range logins {
			ac := ApproverCheck{Login: login}
			m, ok := b.teamsMember(ctx, api, login)
			switch {
			case !ok:
				// The roster is the only way to find somebody, so this is
				// also the answer to "why did they not get a message?".
				ac.Problem = "not in any team the JIT access app is installed in, so jitd can't reach them in Teams"
			default:
				ac.TeamsUser = m.Name
				ac.Problem = b.memberProblem(m)
				if ac.Problem == "" && owners != nil && !owners[login] {
					ac.Problem = "no device on the tailnet, so Teams can't act as this login"
				}
			}
			fc.Approvers = append(fc.Approvers, ac)
		}
		out = append(out, fc)
	}
	return out, nil
}

package teamsbot

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/atoonk/tailscale-jit/jitd/internal/jit"
	"github.com/atoonk/tailscale-jit/jitd/internal/teams"
	"github.com/atoonk/tailscale-jit/jitd/internal/tsapi"
)

// fakeTS is the Tailscale API for these tests. Sam, Maya and Priya are on
// the tailnet (they own devices); policyErr makes the policy unreadable.
type fakeTS struct{ policyErr error }

func (fakeTS) SetAttribute(context.Context, string, string, any, time.Time, string) error { return nil }
func (fakeTS) GetAttributes(context.Context, string) (tsapi.Attributes, error) {
	return tsapi.Attributes{Attributes: map[string]any{}}, nil
}
func (fakeTS) DeleteAttribute(context.Context, string, string) error { return nil }
func (fakeTS) ListDevices(context.Context) ([]tsapi.Device, error) {
	return []tsapi.Device{
		{NodeID: "nSAMOLD", Name: "sam-old.ts.net", User: "sam@example.com", Authorized: true, LastSeen: time.Now().Add(-48 * time.Hour)},
		{NodeID: "nSAM", Name: "sam-mac.ts.net", User: "sam@example.com", OS: "macOS", Authorized: true, LastSeen: time.Now()},
		{NodeID: "nSAMTAG", Name: "sam-server.ts.net", User: "sam@example.com", Tags: []string{"tag:server"}, Authorized: true},
		{NodeID: "nMAYA", Name: "maya-mac.ts.net", User: "maya@example.com", Authorized: true},
		{NodeID: "nPRIYA", Name: "priya-pc.ts.net", User: "priya@example.com", Authorized: true},
	}, nil
}
func (f fakeTS) GetPolicy(context.Context) (tsapi.Policy, error) {
	if f.policyErr != nil {
		return tsapi.Policy{}, f.policyErr
	}
	return tsapi.Policy{Groups: map[string][]string{"group:sre": {"sam@example.com", "priya@example.com"}}}, nil
}

var (
	maya = jit.Identity{Login: "maya@example.com", DisplayName: "Maya Chen", DeviceID: "nMAYA", DeviceName: "maya-mac"}
	leo  = jit.Identity{Login: "leo@example.com"}
)

// The fake tenant: one team with a channel, and the cast in its roster.
const (
	teamID    = "19:engineering@thread.tacv2"
	channelID = "19:sre-oncall@thread.tacv2"
	samID     = "29:sam"
	priyaID   = "29:priya"
	mayaID    = "29:maya"
	outID     = "29:mallory"
)

type env struct {
	bot  *Bot
	svc  *jit.Service
	fake *teams.Fake
	flow *jit.Workflow
	ctx  context.Context
}

func newEnv(t *testing.T, flowTeams *jit.WorkflowTeams) *env {
	t.Helper()
	return newEnvTS(t, flowTeams, fakeTS{})
}

func newEnvTS(t *testing.T, flowTeams *jit.WorkflowTeams, ts fakeTS) *env {
	t.Helper()
	st, err := jit.Open(filepath.Join(t.TempDir(), "jit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := &jit.Service{Store: st, TS: ts, Log: log, Admins: []jit.Principal{"leo@example.com"}}
	svc.PolicyCache = &jit.PolicyCache{TS: ts, TTL: time.Minute, Log: log}

	f := teams.NewFake()
	f.AddUser(samID, "aad-sam", "sam@example.com", "Sam Rivera")
	f.AddUser(priyaID, "aad-priya", "Priya@Example.com", "Priya Natarajan")
	f.AddUser(mayaID, "aad-maya", "maya@example.com", "Maya Chen")
	f.AddUser(outID, "aad-mallory", "mallory@elsewhere.org", "Mallory")
	f.AddTeam(teamID, teams.Channel{ID: teamID}, teams.Channel{ID: channelID, Name: "sre-oncall"})
	f.SetRoster(teamID, samID, priyaID, mayaID, outID)
	f.SetRoster(channelID, samID, priyaID, mayaID, outID)

	b := &Bot{Svc: svc, Log: log, BaseURL: "http://jit", EnvFile: filepath.Join(t.TempDir(), "env"),
		NewAPI:     func(string, string, string) teams.API { return f },
		NewInbound: func(_ teams.API, h teams.Handler) teams.Inbound { return f.Inbound(h) },
	}
	svc.Notify = b
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	b.Start(ctx)
	if _, err := b.Connect(ctx, leo, ConnectInput{
		AppID: "00000000-aaaa-bbbb-cccc-000000000001", AppPassword: "secret-value-1234",
		TenantID: f.TenantID, AllowedDomains: []string{"example.com"},
	}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "socket connected", func() bool { return b.Status().Connected() })
	// The app is installed in the team, which is how jitd learns it exists.
	f.Socket().Deliver(ctx, f.InstalledIn(teamID, "Engineering", teamID))
	waitFor(t, "the team to be recorded", func() bool { return len(b.Status().Installed) == 1 })

	w, err := svc.CreateWorkflow(jit.Workflow{
		Name: "Kubernetes (read-only)", Attribute: "k8s_ro",
		Resources:  []jit.Resource{{Name: "svc:kubernetes-prod-cluster"}},
		Requesters: []jit.Principal{"autogroup:member", "tag:sre-agent"}, Approvers: []jit.Principal{"group:sre"},
		MaxDuration: jit.Duration(time.Hour), Teams: flowTeams,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &env{bot: b, svc: svc, fake: f, flow: w, ctx: ctx}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *env) submit(t *testing.T, who jit.Identity) *jit.AccessRequest {
	t.Helper()
	r, err := e.svc.Submit(context.Background(), who, jit.NewRequest{
		WorkflowID: e.flow.ID, Duration: jit.Duration(30 * time.Minute),
		Reason: "checkout p99 is 4.8s; read pods and logs",
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// press clicks a button on the request's card, as the given Teams user.
func (e *env) press(t *testing.T, user, verb string, data map[string]any) *teams.InvokeResponse {
	t.Helper()
	if data == nil {
		data = map[string]any{}
	}
	return e.fake.Socket().Deliver(e.ctx, e.fake.CardActionBy(user, channelID, verb, data))
}

// channelFlow is a flow that posts to the test channel.
func channelFlow() *jit.WorkflowTeams {
	return &jit.WorkflowTeams{Channel: channelID, ChannelName: "sre-oncall", TeamID: teamID}
}

func respJSON(t *testing.T, r *teams.InvokeResponse) string {
	t.Helper()
	if r == nil {
		return ""
	}
	b, _ := json.Marshal(r)
	return string(b)
}

// TestPostThenApproveFromTeams is the whole flow: a request shows up in
// the channel, an approver presses Approve, and the card follows.
func TestPostThenApproveFromTeams(t *testing.T) {
	e := newEnv(t, channelFlow())
	r := e.submit(t, maya)

	waitFor(t, "the request to be posted", func() bool { return len(e.fake.PostsTo(channelID)) == 1 })
	post := e.fake.PostsTo(channelID)[0]
	card := post.Card()
	for _, want := range []string{"Kubernetes", "Maya Chen", "30 minutes", "maya-mac", "checkout p99", verbApprove, verbDecline, "http://jit/requests/" + r.ID} {
		if !strings.Contains(card, want) {
			t.Errorf("the card lacks %q:\n%s", want, card)
		}
	}

	if resp := e.press(t, samID, verbApprove, map[string]any{dataRequestID: r.ID}); resp == nil || resp.StatusCode != 200 {
		t.Fatalf("approve response = %s", respJSON(t, resp))
	}
	got := e.svc.Request(r.ID)
	if got.Status != jit.StatusApproved {
		t.Fatalf("status = %s", got.Status)
	}
	if len(got.Approvals) != 1 || got.Approvals[0].Via != jit.ViaTeams {
		t.Errorf("approvals = %+v, want one recorded via Teams", got.Approvals)
	}
	if got.Approvals[0].TeamsUser == "" {
		t.Error("the vote should name the Teams account that cast it")
	}

	// The card updates itself rather than being re-posted.
	waitFor(t, "the card to be updated", func() bool { return e.fake.PostsTo(channelID)[0].Updates > 0 })
	if n := len(e.fake.PostsTo(channelID)); n != 1 {
		t.Errorf("posted %d cards, want the first one updated in place", n)
	}
	after := e.fake.PostsTo(channelID)[0].Card()
	if !strings.Contains(after, "Approved by") || !strings.Contains(after, "via Teams") {
		t.Errorf("the updated card does not say who approved:\n%s", after)
	}
	if strings.Contains(after, verbApprove) {
		t.Errorf("a decided request should have no Approve button:\n%s", after)
	}
}

// TestStrangersCannotApprove: Teams identity is an email matched to a
// Tailscale login, and it has to clear every gate.
func TestStrangersCannotApprove(t *testing.T) {
	e := newEnv(t, channelFlow())
	r := e.submit(t, maya)
	waitFor(t, "the request to be posted", func() bool { return len(e.fake.PostsTo(channelID)) == 1 })

	for _, tc := range []struct {
		name, user, want string
	}{
		{"a domain that is not allowed", outID, "not in a domain allowed"},
		{"someone who is not an approver", mayaID, "not an approver"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := e.press(t, tc.user, verbApprove, map[string]any{dataRequestID: r.ID})
			if resp == nil || resp.StatusCode == 200 {
				t.Fatalf("response = %s, want a refusal", respJSON(t, resp))
			}
			if !strings.Contains(respJSON(t, resp), tc.want) {
				t.Errorf("response = %s, want it to mention %q", respJSON(t, resp), tc.want)
			}
			if e.svc.Request(r.ID).Status != jit.StatusPending {
				t.Error("the request was decided by someone who should not be able to")
			}
		})
	}
}

// TestGuestsCannotAct: a guest's address belongs to another organisation,
// so it is not evidence of who they are here.
func TestGuestsCannotAct(t *testing.T) {
	e := newEnv(t, channelFlow())
	e.fake.MakeGuest(samID)
	r := e.submit(t, maya)
	waitFor(t, "the request to be posted", func() bool { return len(e.fake.PostsTo(channelID)) == 1 })

	resp := e.press(t, samID, verbApprove, map[string]any{dataRequestID: r.ID})
	if resp == nil || resp.StatusCode == 200 {
		t.Fatalf("a guest approved: %s", respJSON(t, resp))
	}
	if !strings.Contains(respJSON(t, resp), "guest") {
		t.Errorf("response = %s, want it to say why", respJSON(t, resp))
	}
	if e.svc.Request(r.ID).Status != jit.StatusPending {
		t.Error("a guest decided the request")
	}
}

// TestWrongTenantIsIgnored: a shared channel can carry activities from
// another organisation, and those are nobody jitd knows.
func TestWrongTenantIsIgnored(t *testing.T) {
	e := newEnv(t, channelFlow())
	r := e.submit(t, maya)
	waitFor(t, "the request to be posted", func() bool { return len(e.fake.PostsTo(channelID)) == 1 })

	env := e.fake.CardActionBy(samID, channelID, verbApprove, map[string]any{dataRequestID: r.ID})
	env.Payload.ChannelData.Tenant = &teams.TenantInfo{ID: "99999999-9999-9999-9999-999999999999"}
	env.Payload.Conversation.TenantID = "99999999-9999-9999-9999-999999999999"
	if resp := e.fake.Socket().Deliver(e.ctx, env); resp != nil {
		t.Errorf("an activity from another tenant got a response: %s", respJSON(t, resp))
	}
	if e.svc.Request(r.ID).Status != jit.StatusPending {
		t.Error("an activity from another tenant decided the request")
	}
}

// TestDeclineThroughTheCard: Teams needs no dialog. Decline swaps the card
// for a reason form, for the clicker alone, and its submit decides.
func TestDeclineThroughTheCard(t *testing.T) {
	e := newEnv(t, channelFlow())
	r := e.submit(t, maya)
	waitFor(t, "the request to be posted", func() bool { return len(e.fake.PostsTo(channelID)) == 1 })

	resp := e.press(t, samID, verbDecline, map[string]any{dataRequestID: r.ID})
	if resp == nil || resp.Type != teams.InvokeCard {
		t.Fatalf("Decline should answer with a card, got %s", respJSON(t, resp))
	}
	body := respJSON(t, resp)
	if !strings.Contains(body, inputReason) || !strings.Contains(body, verbDeclineSubmit) {
		t.Errorf("the reason form is missing its input or its button:\n%s", body)
	}
	if e.svc.Request(r.ID).Status != jit.StatusPending {
		t.Error("pressing Decline decided the request before a reason was given")
	}

	// An empty reason keeps the form up rather than declining silently.
	resp = e.press(t, samID, verbDeclineSubmit, map[string]any{dataRequestID: r.ID, inputReason: "  "})
	if resp == nil || resp.Type != teams.InvokeCard || !strings.Contains(respJSON(t, resp), "reason is required") {
		t.Errorf("an empty reason should bring the form back: %s", respJSON(t, resp))
	}
	if e.svc.Request(r.ID).Status != jit.StatusPending {
		t.Error("an empty reason declined the request")
	}

	resp = e.press(t, samID, verbDeclineSubmit, map[string]any{dataRequestID: r.ID, inputReason: "read-only is enough here"})
	if resp == nil || resp.StatusCode != 200 {
		t.Fatalf("decline = %s", respJSON(t, resp))
	}
	got := e.svc.Request(r.ID)
	if got.Status != jit.StatusDeclined {
		t.Fatalf("status = %s", got.Status)
	}
	waitFor(t, "the card to be updated", func() bool { return e.fake.PostsTo(channelID)[0].Updates > 0 })
	card := e.fake.PostsTo(channelID)[0].Card()
	if !strings.Contains(card, "Declined by") || !strings.Contains(card, "read-only is enough here") {
		t.Errorf("the card does not show the decline:\n%s", card)
	}
}

// TestBrowserOnly: the flow's cards carry no buttons, and a press from a
// card drawn before the setting changed is refused too.
func TestBrowserOnly(t *testing.T) {
	ft := channelFlow()
	ft.BrowserOnly = true
	e := newEnv(t, ft)
	r := e.submit(t, maya)
	waitFor(t, "the request to be posted", func() bool { return len(e.fake.PostsTo(channelID)) == 1 })

	card := e.fake.PostsTo(channelID)[0].Card()
	if strings.Contains(card, verbApprove) || strings.Contains(card, verbDecline) {
		t.Errorf("a browser-only flow should have no buttons:\n%s", card)
	}
	if !strings.Contains(card, "browser only") {
		t.Errorf("the card should say why there are no buttons:\n%s", card)
	}
	resp := e.press(t, samID, verbApprove, map[string]any{dataRequestID: r.ID})
	if resp == nil || resp.StatusCode == 200 {
		t.Fatalf("a browser-only flow was approved from Teams: %s", respJSON(t, resp))
	}
	if e.svc.Request(r.ID).Status != jit.StatusPending {
		t.Error("a browser-only flow was decided from Teams")
	}
}

// TestDMApprovers: approvers get the card directly, the requester does
// not, and an approver the app cannot reach is skipped rather than
// failing the whole announcement.
func TestDMApprovers(t *testing.T) {
	ft := channelFlow()
	ft.DMApprovers = true
	e := newEnv(t, ft)
	// Priya is in no team with the app, so ACS would refuse a 1:1 with her.
	e.fake.SetRoster(teamID, samID, mayaID, outID)
	e.fake.SetRoster(channelID, samID, mayaID, outID)

	r := e.submit(t, maya)
	waitFor(t, "the direct message", func() bool { return e.fake.DMTo(samID) != "" })
	waitFor(t, "the channel post", func() bool { return len(e.fake.PostsTo(channelID)) == 1 })

	if dm := e.fake.DMTo(samID); len(e.fake.PostsTo(dm)) != 1 {
		t.Errorf("Sam got %d direct messages, want 1", len(e.fake.PostsTo(dm)))
	}
	if e.fake.DMTo(mayaID) != "" {
		t.Error("the requester should not be sent her own request")
	}
	if e.fake.DMTo(priyaID) != "" {
		t.Error("an approver the app cannot reach should be skipped, not messaged")
	}
	// Both the channel card and the direct message follow the request.
	if _, err := e.svc.Approve(context.Background(), jit.Identity{Login: "sam@example.com", DeviceID: "nSAM"}, r.ID, ""); err != nil {
		t.Fatal(err)
	}
	dm := e.fake.DMTo(samID)
	waitFor(t, "both cards to be updated", func() bool {
		return e.fake.PostsTo(channelID)[0].Updates > 0 && e.fake.PostsTo(dm)[0].Updates > 0
	})
}

// TestReconcileAfterOutage: nothing is lost when jitd or Teams was down.
// A request filed while disconnected is posted when the bot comes back.
func TestReconcileAfterOutage(t *testing.T) {
	e := newEnv(t, channelFlow())
	e.fake.SetFail("send", "ServiceUnavailable")
	r := e.submit(t, maya)
	time.Sleep(50 * time.Millisecond)
	if len(e.fake.PostsTo(channelID)) != 0 {
		t.Fatal("the post should have failed")
	}
	if note := e.bot.RequestNote(e.svc.Request(r.ID)); note == "" {
		t.Error("the request page should say the post failed")
	}
	if e.bot.Problem() == "" {
		t.Error("the sidebar should warn that posting failed")
	}

	e.fake.SetFail("send", "")
	e.bot.reconcile(context.Background())
	waitFor(t, "the request to be posted after the outage", func() bool { return len(e.fake.PostsTo(channelID)) == 1 })
	if note := e.bot.RequestNote(e.svc.Request(r.ID)); note != "" {
		t.Errorf("the failure note should be gone: %q", note)
	}
}

// TestRefreshSkipsUnchangedCards: a card that would not change is left
// alone, so a reconcile does not rewrite every message every ten minutes.
func TestRefreshSkipsUnchangedCards(t *testing.T) {
	e := newEnv(t, channelFlow())
	e.submit(t, maya)
	waitFor(t, "the request to be posted", func() bool { return len(e.fake.PostsTo(channelID)) == 1 })

	e.bot.reconcile(context.Background())
	e.bot.reconcile(context.Background())
	if n := e.fake.PostsTo(channelID)[0].Updates; n != 0 {
		t.Errorf("an unchanged card was updated %d times", n)
	}
}

// TestRequestFromTeams: the form lists the caller's own devices, and the
// submit checks again that the device is still theirs.
func TestRequestFromTeams(t *testing.T) {
	e := newEnv(t, nil)
	resp := e.press(t, samID, verbRequest, nil)
	if resp == nil || resp.Type != teams.InvokeCard {
		t.Fatalf("Request should answer with a form: %s", respJSON(t, resp))
	}
	form := respJSON(t, resp)
	for _, want := range []string{"nSAM", "sam-mac", inputFlow, inputDevice, inputDuration, inputReason, verbRequestSubmit} {
		if !strings.Contains(form, want) {
			t.Errorf("the form lacks %q:\n%s", want, form)
		}
	}
	// Only Sam's own untagged devices.
	if strings.Contains(form, "nSAMTAG") {
		t.Error("the form offers a tagged device")
	}
	if strings.Contains(form, "nMAYA") {
		t.Error("the form offers somebody else's device")
	}

	// A reason is required.
	resp = e.press(t, samID, verbRequestSubmit, map[string]any{inputFlow: e.flow.ID, inputDevice: "nSAM", inputReason: ""})
	if !strings.Contains(respJSON(t, resp), "reason is required") {
		t.Errorf("an empty reason should be refused: %s", respJSON(t, resp))
	}
	// Somebody else's device is refused even though the form never offered it.
	resp = e.press(t, samID, verbRequestSubmit, map[string]any{inputFlow: e.flow.ID, inputDevice: "nMAYA", inputReason: "why not"})
	if !strings.Contains(respJSON(t, resp), "not one of yours") {
		t.Errorf("another person's device should be refused: %s", respJSON(t, resp))
	}

	resp = e.press(t, samID, verbRequestSubmit, map[string]any{
		inputFlow: e.flow.ID, inputDevice: "nSAM", inputDuration: "30m", inputReason: "need to read pod logs",
	})
	if resp == nil || resp.StatusCode != 200 || !strings.Contains(respJSON(t, resp), "Request filed") {
		t.Fatalf("submit = %s", respJSON(t, resp))
	}
	rs := e.svc.Requests(func(r *jit.AccessRequest) bool { return true })
	if len(rs) != 1 {
		t.Fatalf("filed %d requests", len(rs))
	}
	if rs[0].Requester.Login != "sam@example.com" || rs[0].TargetDeviceID != "nSAM" {
		t.Errorf("request = %+v", rs[0].Requester)
	}
	if rs[0].Requester.Via != jit.ViaTeams {
		t.Errorf("via = %q, want teams", rs[0].Requester.Via)
	}
}

// TestCommands: /jit answers with what is waiting and what is yours,
// whether it was typed to the bot or @mentioned in a channel.
func TestCommands(t *testing.T) {
	e := newEnv(t, channelFlow())
	e.submit(t, maya)
	waitFor(t, "the request to be posted", func() bool { return len(e.fake.PostsTo(channelID)) == 1 })

	for _, text := range []string{"", "status", "/jit", "<at>JIT access</at> status"} {
		t.Run("text="+text, func(t *testing.T) {
			before := len(e.fake.PostsTo(channelID))
			e.fake.Socket().Deliver(e.ctx, e.fake.MessageTo(samID, channelID, text))
			waitFor(t, "an answer", func() bool { return len(e.fake.PostsTo(channelID)) > before })
			card := e.fake.PostsTo(channelID)[before].Card()
			if !strings.Contains(card, "Waiting for your approval (1)") {
				t.Errorf("the summary does not list the waiting request:\n%s", card)
			}
			if !strings.Contains(card, "sam@example.com") {
				t.Errorf("the summary should say who jitd thinks you are:\n%s", card)
			}
		})
	}

	before := len(e.fake.PostsTo(channelID))
	e.fake.Socket().Deliver(e.ctx, e.fake.MessageTo(samID, channelID, "wibble"))
	waitFor(t, "an answer", func() bool { return len(e.fake.PostsTo(channelID)) > before })
	if card := e.fake.PostsTo(channelID)[before].Card(); !strings.Contains(card, "isn't a JIT access command") {
		t.Errorf("an unknown command should say so:\n%s", card)
	}
}

func TestCommandText(t *testing.T) {
	for _, tc := range []struct {
		in   teams.Activity
		want string
	}{
		{teams.Activity{Text: "request"}, "request"},
		{teams.Activity{Text: "/jit request"}, "request"},
		{teams.Activity{Text: "  JIT   Request  "}, "Request"},
		{teams.Activity{Text: "jitter"}, "jitter"}, // not the "jit" prefix
		{teams.Activity{Text: "<at>JIT access</at> help", Entities: []teams.Entity{{Type: "mention", Text: "<at>JIT access</at>"}}}, "help"},
		{teams.Activity{Text: "<at>JIT access</at>"}, ""},
	} {
		if got := commandText(&tc.in); got != tc.want {
			t.Errorf("commandText(%q) = %q, want %q", tc.in.Text, got, tc.want)
		}
	}
}

// TestSecretsNeverLeave: the client secret is never returned, and the env
// file wins over anything saved on the page.
func TestSecretsNeverLeave(t *testing.T) {
	e := newEnv(t, nil)
	st := e.bot.Status()
	blob, _ := json.Marshal(st)
	if strings.Contains(string(blob), "secret-value-1234") {
		t.Errorf("the status leaks the client secret: %s", blob)
	}
	if st.PasswordHint != "…1234" {
		t.Errorf("password hint = %q", st.PasswordHint)
	}
	if st.AppID == "" || st.TenantID == "" {
		t.Error("the app ID and tenant are not secrets and should be shown")
	}

	// With the env file set, the page refuses to change the connection.
	envFile := filepath.Join(t.TempDir(), "env")
	body := "TEAMS_APP_ID=00000000-aaaa-bbbb-cccc-000000000002\nTEAMS_APP_PASSWORD=from-the-env-file\nTEAMS_TENANT_ID=" + e.fake.TenantID + "\nTEAMS_ALLOWED_DOMAINS=example.com\n"
	if err := os.WriteFile(envFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	e.bot.EnvFile = envFile
	if err := e.bot.load(); err != nil {
		t.Fatal(err)
	}
	if got := e.bot.Status().Source; got != "env" {
		t.Fatalf("source = %q, want env", got)
	}
	if _, err := e.bot.Connect(context.Background(), leo, ConnectInput{
		AppID: "00000000-aaaa-bbbb-cccc-000000000003", AppPassword: "x", TenantID: e.fake.TenantID, AllowedDomains: []string{"example.com"},
	}); err == nil {
		t.Error("the page changed a connection set in the env file")
	}
	if err := e.bot.Disconnect(context.Background(), leo); err == nil {
		t.Error("the page disconnected a connection set in the env file")
	}
}

func TestConnectValidates(t *testing.T) {
	e := newEnv(t, nil)
	for _, tc := range []struct {
		name string
		in   ConnectInput
		want string
	}{
		{"no app ID", ConnectInput{AppPassword: "x", TenantID: "00000000-aaaa-bbbb-cccc-000000000001", AllowedDomains: []string{"example.com"}}, "app ID"},
		{"the secret ID instead of the value", ConnectInput{AppID: "00000000-aaaa-bbbb-cccc-000000000001", TenantID: "00000000-aaaa-bbbb-cccc-000000000001", AllowedDomains: []string{"example.com"}}, "client secret"},
		{"no tenant", ConnectInput{AppID: "00000000-aaaa-bbbb-cccc-000000000001", AppPassword: "x", AllowedDomains: []string{"example.com"}}, "tenant ID"},
		{"no domains", ConnectInput{AppID: "00000000-aaaa-bbbb-cccc-000000000001", AppPassword: "x", TenantID: "00000000-aaaa-bbbb-cccc-000000000001"}, "email domain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.bot.Connect(context.Background(), leo, tc.in)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestCheckApprovers tells an admin, per flow, who can act from Teams and
// who cannot.
func TestCheckApprovers(t *testing.T) {
	e := newEnv(t, channelFlow())
	e.fake.SetRoster(teamID, samID, mayaID) // Priya is in no team with the app
	e.bot.ids.reset()

	fcs, err := e.bot.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(fcs) != 1 {
		t.Fatalf("checked %d flows", len(fcs))
	}
	fc := fcs[0]
	if fc.ChannelIssue != "" {
		t.Errorf("channel issue = %q, want none", fc.ChannelIssue)
	}
	if fc.ChannelName != "sre-oncall" {
		t.Errorf("channel name = %q", fc.ChannelName)
	}
	got := map[string]string{}
	for _, a := range fc.Approvers {
		got[a.Login] = a.Problem
	}
	if p, ok := got["sam@example.com"]; !ok || p != "" {
		t.Errorf("Sam should be able to act, got %q (%v)", p, got)
	}
	if p := got["priya@example.com"]; !strings.Contains(p, "not in any team") {
		t.Errorf("Priya's problem = %q, want it to explain she is unreachable", p)
	}
}

// TestAppPackage: Teams has no "create app" link, so jitd builds the zip.
// It has to be a flat archive with the manifest and both icons.
func TestAppPackage(t *testing.T) {
	e := newEnv(t, nil)
	pkg, err := e.bot.AppPackage()
	if err != nil {
		t.Fatal(err)
	}
	z, err := zip.NewReader(bytes.NewReader(pkg), int64(len(pkg)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	var manifest map[string]any
	for _, f := range z.File {
		names = append(names, f.Name)
		if strings.Contains(f.Name, "/") {
			t.Errorf("the package has a folder in it (%q); Teams refuses that", f.Name)
		}
		if f.Name == "manifest.json" {
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(rc)
			rc.Close()
			if err := json.Unmarshal(b, &manifest); err != nil {
				t.Fatalf("the manifest is not JSON: %v", err)
			}
		}
	}
	for _, want := range []string{"manifest.json", "color.png", "outline.png"} {
		if !slicesContains(names, want) {
			t.Errorf("the package has no %s (has %v)", want, names)
		}
	}
	if manifest["id"] != e.bot.Status().AppID {
		t.Errorf("manifest id = %v, want the configured app ID", manifest["id"])
	}
	bots, _ := manifest["bots"].([]any)
	if len(bots) != 1 {
		t.Fatalf("manifest bots = %v", manifest["bots"])
	}
	bot0, _ := bots[0].(map[string]any)
	if bot0["botId"] != e.bot.Status().AppID {
		t.Errorf("botId = %v, want the configured app ID", bot0["botId"])
	}
	// http://jit has no dot, so Teams would refuse it as a valid domain.
	if _, ok := manifest["validDomains"]; ok {
		t.Errorf("validDomains should be left out for a dotless host: %v", manifest["validDomains"])
	}
}

func TestManifestDomain(t *testing.T) {
	for in, want := range map[string]string{
		"http://jit":                  "",
		"http://jit.tail1234.ts.net":  "jit.tail1234.ts.net",
		"https://jit.tail1234.ts.net": "jit.tail1234.ts.net",
	} {
		if got := manifestDomain(in); got != want {
			t.Errorf("manifestDomain(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestRenderEscapes: a card renders Markdown, so text from a request must
// not be able to introduce a link or formatting of its own.
func TestRenderEscapes(t *testing.T) {
	r := &jit.AccessRequest{
		ID: "req_1", WorkflowName: "Prod *DB*", Status: jit.StatusPending,
		Requester: jit.Identity{Login: "maya@example.com", DisplayName: "Maya [Chen]", DeviceName: "maya_mac"},
		Reason:    "see [this](http://evil.example.com) and `that`",
		Duration:  jit.Duration(time.Hour),
	}
	card := render(renderInput{R: r, URL: "http://jit/requests/req_1"})
	b, _ := json.Marshal(card)
	s := string(b)
	if strings.Contains(s, "[this](http://evil.example.com)") {
		t.Errorf("a reason was able to make a link:\n%s", s)
	}
	for _, raw := range []string{"Prod *DB*", "Maya [Chen]"} {
		if strings.Contains(s, raw) {
			t.Errorf("%q went in unescaped:\n%s", raw, s)
		}
	}
	// The escaped forms are still there, so nothing was dropped.
	if !strings.Contains(s, `Prod \\*DB\\*`) {
		t.Errorf("the flow name is missing from the card:\n%s", s)
	}
}

// TestManifestFields keeps the manifest to the fields the declared schema
// version defines. The schema sets additionalProperties:false, so a field
// it does not know fails the upload outright, with a schema validation
// error and no app: that is how packageName, retired after 1.16, got as
// far as a tenant.
func TestManifestFields(t *testing.T) {
	// The top-level properties of MicrosoftTeams.schema.json devPreview.
	allowed := []string{
		"$schema", "manifestVersion", "version", "id", "developer", "name",
		"description", "icons", "accentColor", "bots", "permissions",
		"supportsChannelFeatures",
		"validDomains", "localizationInfo", "staticTabs", "configurableTabs",
		"connectors", "composeExtensions", "webApplicationInfo", "showLoadingIndicator",
		"isFullScreen", "activities", "devicePermissions", "defaultInstallScope",
		"defaultGroupCapability", "subscriptionOffer", "meetingExtensionDefinition",
		"authorization", "extensions", "actions", "dashboardCards", "copilotExtensions",
		"elementRelationshipSet", "publisherDocsUrl", "defaultBlockUntilAdminAction",
		"configurableProperties", "supportedChannelTypes", "graphConnector",
	}
	for k := range Manifest("00000000-0000-0000-0000-000000000000", "https://jit.example.ts.net") {
		if !slicesContains(allowed, k) {
			t.Errorf("manifest has %q, which schema v%s does not define: Teams refuses the upload", k, manifestVersion)
		}
	}
	// Everything the schema requires is there, or the upload fails the other way.
	for _, req := range []string{"manifestVersion", "version", "id", "developer", "name", "description", "icons", "accentColor"} {
		if _, ok := Manifest("app-id", "https://jit.example.ts.net")[req]; !ok {
			t.Errorf("manifest lacks the required field %q", req)
		}
	}
}

func slicesContains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

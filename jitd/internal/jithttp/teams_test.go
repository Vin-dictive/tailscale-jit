package jithttp

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/atoonk/tailscale-jit/jitd/internal/jit"
	"github.com/atoonk/tailscale-jit/jitd/internal/teams"
	"github.com/atoonk/tailscale-jit/jitd/internal/teamsbot"
)

func TestTeamsPageAndAPI(t *testing.T) {
	const (
		teamID  = "19:engineering@thread.tacv2"
		channel = "19:sre-oncall@thread.tacv2"
		bobID   = "29:bob"
	)
	f := teams.NewFake()
	f.AddUser(bobID, "aad-bob", "bob@example.com", "Bob Example")
	f.AddTeam(teamID, teams.Channel{ID: channel, Name: "sre-oncall"})
	f.SetRoster(teamID, bobID)
	f.SetRoster(channel, bobID)

	var bot *teamsbot.Bot
	srv, _, h := newTestServerWith(t, func(s *Server) {
		bot = &teamsbot.Bot{Svc: s.Svc, Log: s.Log, BaseURL: s.BaseURL, EnvFile: filepath.Join(t.TempDir(), "env"),
			NewAPI:     func(string, string, string) teams.API { return f },
			NewInbound: func(_ teams.API, hh teams.Handler) teams.Inbound { return f.Inbound(hh) }}
		s.Svc.Notify = bot
		s.Teams = bot
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	bot.Start(ctx)

	// Not connected: the page walks through setup; non-admins are kept out.
	for _, tc := range []struct {
		user, path string
		status     int
		want       []string
	}{
		{"root", "/admin/integrations/teams", 200, []string{"Connect Microsoft Teams", "App registrations", "Socket mode", `value="example.com"`, "TEAMS_APP_ID"}},
		{"alice", "/admin/integrations/teams", 403, []string{"Admins only"}},
		{"root", "/admin/workflows/new", 200, []string{`name="teams_channel"`, "Connect Teams on the"}},
		// Teams is a tab under Integrations, not a sidebar entry of its own.
		{"root", "/admin/integrations/tailscale", 200, []string{`href="/admin/integrations/teams"`, "Microsoft Teams"}},
		{"root", "/", 200, []string{`href="/admin/integrations"`}},
	} {
		status, body := getPage(t, srv, tc.user, tc.path)
		if status != tc.status {
			t.Errorf("%s as %s: status %d", tc.path, tc.user, status)
		}
		for _, w := range tc.want {
			if !strings.Contains(body, w) {
				t.Errorf("%s as %s: missing %q", tc.path, tc.user, w)
			}
		}
	}
	if code := call(t, srv, "alice", "GET", "/v1/teams", nil, nil); code != 403 {
		t.Errorf("GET /v1/teams as alice: %d", code)
	}

	// Credentials that are not GUIDs are refused with a hint saying where
	// to find the right value.
	var e apiErr
	if code := call(t, srv, "root", "POST", "/v1/teams/connect", map[string]any{
		"app_id": "not-a-guid", "app_password": "x", "tenant_id": f.TenantID, "allowed_domains": []string{"example.com"},
	}, &e); code != 400 || !strings.Contains(e.Error.Message, "Application (client) ID") {
		t.Errorf("a bad app ID was accepted: %d %+v", code, e)
	}

	var st teamsbot.Status
	if code := call(t, srv, "root", "POST", "/v1/teams/connect", map[string]any{
		"app_id": "00000000-aaaa-bbbb-cccc-000000000001", "app_password": "secret-9999",
		"tenant_id": f.TenantID, "allowed_domains": []string{"Example.com"},
	}, &st); code != 200 {
		t.Fatalf("connect: %d", code)
	}
	if st.AllowedDomains[0] != "example.com" || st.PasswordHint != "…9999" {
		t.Errorf("status = %+v", st)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !bot.Status().Connected() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// Until the app is installed somewhere there is nowhere to post, and
	// the page says so rather than offering an empty channel list.
	if _, body := getPage(t, srv, "root", "/admin/integrations/teams"); !strings.Contains(body, "not installed in any team") {
		t.Error("the page does not say the app is installed nowhere")
	}
	f.Socket().Deliver(ctx, f.InstalledIn(teamID, "Engineering", teamID))
	for len(bot.Status().Installed) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	// A flow with a channel, set through the API as the form does.
	var wf jit.Workflow
	if code := call(t, srv, "root", "POST", "/v1/workflows", map[string]any{
		"name": "RDP flow", "attribute": "windows_rdp", "requesters": []string{"autogroup:member"}, "approvers": []string{"bob@example.com"},
		"teams": map[string]any{"channel": channel, "channel_name": "sre-oncall", "team_id": teamID, "dm_approvers": true},
	}, &wf); code != 201 || wf.Teams == nil || wf.Teams.Channel != channel {
		t.Fatalf("create with teams: %d %+v", code, wf.Teams)
	}
	// A channel without its team is refused: jitd would not know where to
	// post it.
	if code := call(t, srv, "root", "POST", "/v1/workflows", map[string]any{
		"name": "No team", "attribute": "no_team", "requesters": []string{"autogroup:member"}, "approvers": []string{"bob@example.com"},
		"teams": map[string]any{"channel": channel},
	}, &e); code != 400 || !strings.Contains(e.Error.Message, "team it belongs to") {
		t.Errorf("a channel without its team was accepted: %d %+v", code, e)
	}

	for _, tc := range []struct {
		path string
		want []string
	}{
		{"/admin/integrations/teams", []string{"connected", "RDP flow", "sre-oncall", "bob@example.com", "Bob Example", "Engineering", "Send a test message", "Disconnect", "amer"}},
		{"/admin/workflows/" + wf.ID, []string{`value="` + channel + `"`, `data-name="sre-oncall"`, `name="teams_dm" checked`}},
	} {
		_, body := getPage(t, srv, "root", tc.path)
		for _, w := range tc.want {
			if !strings.Contains(body, w) {
				t.Errorf("%s: missing %q", tc.path, w)
			}
		}
	}

	// The client secret appears nowhere.
	for _, path := range []string{"/admin/integrations/teams", "/v1/teams", "/v1/teams/check", "/api/openapi.json", "/admin/workflows/" + wf.ID} {
		_, body := getPage(t, srv, "root", path)
		if strings.Contains(body, "secret-9999") {
			t.Errorf("%s shows the client secret", path)
		}
	}

	// The app package is served, and only to admins.
	status, body := getPage(t, srv, "root", teamsbot.ManifestPath)
	if status != 200 || !strings.HasPrefix(body, "PK") {
		t.Errorf("the app package did not download: status %d, %d bytes", status, len(body))
	}
	if status, _ := getPage(t, srv, "alice", teamsbot.ManifestPath); status != 403 {
		t.Errorf("a non-admin downloaded the app package: %d", status)
	}

	// A request is posted; approving from Teams shows "via Teams" on its page.
	var r jit.AccessRequest
	call(t, srv, "alice", "POST", "/v1/requests", map[string]any{"workflow_id": wf.ID, "reason": "patch tuesday"}, &r)
	for time.Now().Before(deadline) && len(f.PostsTo(channel)) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if posts := f.PostsTo(channel); len(posts) != 1 {
		t.Fatalf("posts = %d", len(posts))
	}
	f.Socket().Deliver(ctx, f.CardActionBy(bobID, channel, "jit_approve", map[string]any{"request_id": r.ID}))
	for time.Now().Before(deadline) && h.Svc.Request(r.ID).Status != jit.StatusApproved {
		time.Sleep(10 * time.Millisecond)
	}
	if _, body := getPage(t, srv, "alice", "/requests/"+r.ID); !strings.Contains(body, "via Teams") {
		t.Errorf("the request page does not say via Teams: status %s", h.Svc.Request(r.ID).Status)
	}

	if code := call(t, srv, "root", "POST", "/v1/teams/test", map[string]any{"channel": channel}, nil); code != 200 {
		t.Errorf("test message: %d", code)
	}
	if code := call(t, srv, "root", "POST", "/v1/teams/disconnect", nil, &st); code != 200 || st.Configured() {
		t.Errorf("disconnect: %d %+v", code, st)
	}
}

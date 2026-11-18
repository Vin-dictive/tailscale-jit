package teams

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// acs is a stand-in for the Agent Communication Service and the Microsoft
// identity platform, enough to check what goes on the wire.
type acs struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	tokens   int              // how many token requests arrived
	calls    []string         // "METHOD /path" of every ACS call
	bodies   []string         // their bodies
	auth     []string         // their Authorization headers
	expires  int              // what the token endpoint claims, seconds
	tokenErr int              // status to fail the token request with, 0 for none
	throttle int              // fail this many ACS calls with 429 first
	handler  http.HandlerFunc // overrides the default ACS routing
}

func newACS(t *testing.T) *acs {
	t.Helper()
	a := &acs{t: t, expires: 3600}
	a.srv = httptest.NewTLSServer(http.HandlerFunc(a.serve))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *acs) client() *Client {
	return &Client{
		ID: "app-id", Secret: "secret", Tenant: "tenant-id",
		HTTP:      a.srv.Client(),
		LoginBase: a.srv.URL,
	}
}

func (a *acs) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token") {
		a.mu.Lock()
		a.tokens++
		fail, expires := a.tokenErr, a.expires
		a.mu.Unlock()
		if fail != 0 {
			w.WriteHeader(fail)
			json.NewEncoder(w).Encode(map[string]any{
				"error":             "invalid_client",
				"error_description": "AADSTS7000215: Invalid client secret provided.\r\nTrace ID: abc",
			})
			return
		}
		if got, want := r.URL.Path, "/tenant-id/oauth2/v2.0/token"; got != want {
			a.t.Errorf("token path = %q, want %q", got, want)
		}
		form := string(body)
		for _, want := range []string{"grant_type=client_credentials", "client_id=app-id", "client_secret=secret", "scope=" + strings.ReplaceAll(TokenScope, ":", "%3A")} {
			if !strings.Contains(form, strings.Split(want, "=")[0]+"=") {
				a.t.Errorf("token form %q lacks %q", form, want)
			}
		}
		if !strings.Contains(form, "scope=https%3A%2F%2Fapi.botframework.com%2F.default") {
			a.t.Errorf("token form %q has the wrong scope", form)
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-" + strconv.Itoa(a.tokens), "expires_in": expires})
		return
	}

	a.mu.Lock()
	a.calls = append(a.calls, r.Method+" "+r.URL.EscapedPath())
	a.bodies = append(a.bodies, string(body))
	a.auth = append(a.auth, r.Header.Get("Authorization"))
	throttle := a.throttle
	if throttle > 0 {
		a.throttle--
	}
	h := a.handler
	a.mu.Unlock()

	if throttle > 0 {
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "Throttled", "message": "slow down"}})
		return
	}
	if h != nil {
		h(w, r)
		return
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/activities") && r.Method == http.MethodPost:
		json.NewEncoder(w).Encode(map[string]any{"id": "posted-1"})
	case strings.Contains(r.URL.Path, "/activities/") && r.Method == http.MethodPut:
		w.WriteHeader(http.StatusOK)
	case strings.HasSuffix(r.URL.Path, "/v3/conversations") && r.Method == http.MethodPost:
		json.NewEncoder(w).Encode(map[string]any{"id": "19:dm-created"})
	case strings.HasSuffix(r.URL.Path, "/conversations") && strings.Contains(r.URL.Path, "/teams/"):
		json.NewEncoder(w).Encode(map[string]any{"conversations": []any{
			map[string]any{"id": "19:general@thread.tacv2"},
			map[string]any{"id": "19:eng@thread.tacv2", "name": "engineering"},
		}})
	case strings.Contains(r.URL.Path, "/members/"):
		json.NewEncoder(w).Encode(Member{ID: "29:maya", Email: "maya@example.com", UserPrincipalName: "maya@example.com"})
	default:
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "NotFound", "message": "no route " + r.URL.Path}})
	}
}

func (a *acs) serviceURL() string { return a.srv.URL + "/amer/" }

func (a *acs) lastCall() (call, body, auth string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.calls) == 0 {
		return "", "", ""
	}
	i := len(a.calls) - 1
	return a.calls[i], a.bodies[i], a.auth[i]
}

// TestClientWireFormat checks the shape of every call jitd makes, since
// nothing else will: these run against Microsoft, not against a fake.
func TestClientWireFormat(t *testing.T) {
	a := newACS(t)
	c := a.client()
	ctx := context.Background()

	id, err := c.SendActivity(ctx, a.serviceURL(), "19:room@thread.tacv2", Message(NewCard(Text("hello"))))
	if err != nil {
		t.Fatalf("SendActivity: %v", err)
	}
	if id != "posted-1" {
		t.Errorf("SendActivity returned %q", id)
	}
	call, body, auth := a.lastCall()
	if want := "POST /amer/v3/conversations/19:room@thread.tacv2/activities"; call != want {
		t.Errorf("send call = %q, want %q", call, want)
	}
	if auth != "Bearer tok-1" {
		t.Errorf("send Authorization = %q", auth)
	}
	if !strings.Contains(body, `"contentType":"application/vnd.microsoft.card.adaptive"`) {
		t.Errorf("send body has no card: %s", body)
	}

	if err := c.UpdateActivity(ctx, a.serviceURL(), "19:room@thread.tacv2", "posted-1", TextMessage("changed")); err != nil {
		t.Fatalf("UpdateActivity: %v", err)
	}
	call, body, _ = a.lastCall()
	if want := "PUT /amer/v3/conversations/19:room@thread.tacv2/activities/posted-1"; call != want {
		t.Errorf("update call = %q, want %q", call, want)
	}
	if !strings.Contains(body, `"id":"posted-1"`) {
		t.Errorf("an update must carry the activity ID: %s", body)
	}

	conv, err := c.CreateConversation(ctx, a.serviceURL(), "tenant-1", "29:maya")
	if err != nil {
		t.Fatalf("CreateConversation: %v", err)
	}
	if conv != "19:dm-created" {
		t.Errorf("CreateConversation returned %q", conv)
	}
	call, body, _ = a.lastCall()
	if want := "POST /amer/v3/conversations"; call != want {
		t.Errorf("create call = %q, want %q", call, want)
	}
	for _, want := range []string{`"id":"28:app-id"`, `"id":"29:maya"`, `"isGroup":false`, `"tenantId":"tenant-1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("create body %s lacks %s", body, want)
		}
	}

	m, err := c.Member(ctx, a.serviceURL(), "19:room@thread.tacv2", "29:maya")
	if err != nil {
		t.Fatalf("Member: %v", err)
	}
	if m.Email != "maya@example.com" {
		t.Errorf("Member email = %q", m.Email)
	}

	chans, err := c.Channels(ctx, a.serviceURL(), "19:team@thread.tacv2")
	if err != nil {
		t.Fatalf("Channels: %v", err)
	}
	if len(chans) != 2 || chans[0].DisplayName() != "General" || chans[1].DisplayName() != "engineering" {
		t.Errorf("Channels = %+v; the unnamed one is General", chans)
	}

	// One token, fetched once and reused for all of it.
	if a.tokens != 1 {
		t.Errorf("asked for %d tokens, want 1", a.tokens)
	}
}

// TestClientPagesMembers: a team roster arrives in pages, and a very large
// one must not be read without limit.
func TestClientPagesMembers(t *testing.T) {
	a := newACS(t)
	page := 0
	a.handler = func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/pagedmembers") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.URL.Query().Get("pageSize"); got != strconv.Itoa(memberPageSize) {
			t.Errorf("pageSize = %q", got)
		}
		page++
		if page > 1 && r.URL.Query().Get("continuationToken") == "" {
			t.Error("the second page was fetched without a continuation token")
		}
		members := make([]Member, memberPageSize)
		for i := range members {
			members[i] = Member{ID: fmt.Sprintf("29:u%d-%d", page, i)}
		}
		json.NewEncoder(w).Encode(map[string]any{"members": members, "continuationToken": "more"})
	}
	got, err := a.client().Members(context.Background(), a.serviceURL(), "19:team@thread.tacv2")
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(got) != MaxMembers {
		t.Errorf("read %d members, want the cap of %d", len(got), MaxMembers)
	}
}

// TestClientRetriesThrottling: ACS throttles, and one retry is enough to
// get past a short burst.
func TestClientRetriesThrottling(t *testing.T) {
	a := newACS(t)
	a.throttle = 1
	if _, err := a.client().SendActivity(context.Background(), a.serviceURL(), "19:room", TextMessage("hi")); err != nil {
		t.Fatalf("SendActivity through one 429: %v", err)
	}
	a.mu.Lock()
	calls := len(a.calls)
	a.mu.Unlock()
	if calls != 2 {
		t.Errorf("made %d calls, want a retry after the 429", calls)
	}

	// Throttled twice, the second one is reported rather than retried for
	// ever.
	a.throttle = 3
	_, err := a.client().SendActivity(context.Background(), a.serviceURL(), "19:room", TextMessage("hi"))
	if err == nil {
		t.Fatal("a persistent 429 should be reported")
	}
	if ErrorCode(err) != "Throttled" || ErrorStatus(err) != http.StatusTooManyRequests {
		t.Errorf("err = %v (code %q, status %d)", err, ErrorCode(err), ErrorStatus(err))
	}
}

// TestClientTokenCacheAndRefresh: the token is reused until it is nearly
// out, and a rejection drops it so the next call fetches a new one.
func TestClientTokenCacheAndRefresh(t *testing.T) {
	a := newACS(t)
	c := a.client()
	ctx := context.Background()

	first, err := c.Token(ctx)
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if again, _ := c.Token(ctx); again != first {
		t.Errorf("the token was not cached: %q then %q", first, again)
	}
	if a.tokens != 1 {
		t.Errorf("fetched %d tokens, want 1", a.tokens)
	}

	// A 401 from ACS means the token is no good, whatever its expiry said.
	a.handler = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "Unauthorized", "message": "nope"}})
	}
	if _, err := c.SendActivity(ctx, a.serviceURL(), "19:room", TextMessage("hi")); err == nil {
		t.Fatal("a 401 should be reported")
	}
	a.handler = nil
	if _, err := c.Token(ctx); err != nil {
		t.Fatalf("Token after a 401: %v", err)
	}
	if a.tokens < 2 {
		t.Errorf("a rejected token was kept: %d fetches", a.tokens)
	}
}

// TestClientShortLivedTokenIsNotCachedPastItsLife: a token that lives less
// than the refresh margin must still expire before it does, not after.
func TestClientShortLivedTokenIsNotCachedPastItsLife(t *testing.T) {
	a := newACS(t)
	a.expires = 60
	c := a.client()
	if _, err := c.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	c.mu.Lock()
	exp := c.tokenExp
	c.mu.Unlock()
	if d := time.Until(exp); d <= 0 || d > 60*time.Second {
		t.Errorf("a 60s token was cached for %s", d)
	}
}

// TestClientTokenError surfaces Microsoft's reason, trimmed to the first
// line: the rest is a trace ID that helps nobody on an admin page.
func TestClientTokenError(t *testing.T) {
	a := newACS(t)
	a.tokenErr = http.StatusUnauthorized
	_, err := a.client().Token(context.Background())
	if err == nil {
		t.Fatal("a rejected client secret should be reported")
	}
	if ErrorCode(err) != "invalid_client" {
		t.Errorf("code = %q", ErrorCode(err))
	}
	if strings.Contains(err.Error(), "Trace ID") {
		t.Errorf("the trace ID should be trimmed: %v", err)
	}
	if !Terminal(err) {
		t.Error("a rejected secret is terminal: retrying cannot fix it")
	}
}

func TestClientUnconfigured(t *testing.T) {
	var c Client
	if _, err := c.Token(context.Background()); err != ErrUnconfigured {
		t.Errorf("Token with no credentials = %v, want ErrUnconfigured", err)
	}
}

// TestJoin: the service URL comes off an activity, so it is checked rather
// than trusted, and conversation IDs are full of characters that need
// escaping.
func TestJoin(t *testing.T) {
	got, err := join("https://smba.trafficmanager.net/amer/", "v3", "conversations", "19:a b@thread.tacv2", "activities")
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	want := "https://smba.trafficmanager.net/amer/v3/conversations/19:a%20b@thread.tacv2/activities"
	if got != want {
		t.Errorf("join = %q, want %q", got, want)
	}
	if got, err := join("https://smba.trafficmanager.net/amer", "v3"); err != nil || got != "https://smba.trafficmanager.net/amer/v3" {
		t.Errorf("join without a trailing slash = %q, %v", got, err)
	}
	if _, err := join("http://evil.example.com/", "v3"); err == nil {
		t.Error("a plaintext service URL should be refused: the bearer token would go over it")
	}
	if _, err := join("", "v3"); err == nil {
		t.Error("an empty service URL should be refused")
	}
	// A conversation ID is attacker-adjacent data: it arrives on an
	// activity. Dot segments would climb out of the service URL's path.
	for _, bad := range []string{"..", ".", "", "a/b"} {
		if _, err := join("https://smba.trafficmanager.net/amer/", "v3", bad); err == nil {
			t.Errorf("join accepted %q as a path segment", bad)
		}
	}
}

func TestMemberGuest(t *testing.T) {
	for _, tc := range []struct {
		upn  string
		want bool
	}{
		{"maya@example.com", false},
		{"maya_other.com#EXT#@example.onmicrosoft.com", true},
		{"maya_other.com#ext#@example.onmicrosoft.com", true},
		{"", false},
	} {
		if got := (Member{UserPrincipalName: tc.upn}).Guest(); got != tc.want {
			t.Errorf("Guest(%q) = %v, want %v", tc.upn, got, tc.want)
		}
	}
}

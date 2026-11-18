package teams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// stubAPI is the ACS side of socket mode: all the Socket asks of an API is
// a session to dial. Everything else belongs to Client, which has its own
// tests.
type stubAPI struct {
	url       string
	expiresIn int

	mu       sync.Mutex
	connects []connectCall
	err      error
	signal   chan connectCall
}

type connectCall struct{ geo, replaces string }

func newStubAPI(url string) *stubAPI {
	return &stubAPI{url: url, expiresIn: 3600, signal: make(chan connectCall, 64)}
}

func (s *stubAPI) AppID() string { return "app-id" }

func (s *stubAPI) Token(context.Context) (string, error) { return "tok", nil }

func (s *stubAPI) Connect(ctx context.Context, geo, replaces string) (Session, error) {
	s.mu.Lock()
	c := connectCall{geo, replaces}
	s.connects = append(s.connects, c)
	err := s.err
	expires := s.expiresIn
	s.mu.Unlock()
	select {
	case s.signal <- c:
	default:
	}
	if err != nil {
		return Session{}, err
	}
	return Session{URL: s.url, AccessToken: "socket-token", ExpiresIn: expires, ProtocolVersion: 1}, nil
}

func (s *stubAPI) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *stubAPI) calls() []connectCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.connects)
}

func (s *stubAPI) callsFor(geo string) []connectCall {
	var out []connectCall
	for _, c := range s.calls() {
		if c.geo == geo {
			out = append(out, c)
		}
	}
	return out
}

// The rest of API is not reached from socket mode.
func (s *stubAPI) SendActivity(context.Context, string, string, Activity) (string, error) {
	return "", errors.New("not used")
}
func (s *stubAPI) UpdateActivity(context.Context, string, string, string, Activity) error {
	return errors.New("not used")
}
func (s *stubAPI) CreateConversation(context.Context, string, string, string) (string, error) {
	return "", errors.New("not used")
}
func (s *stubAPI) Member(context.Context, string, string, string) (Member, error) {
	return Member{}, errors.New("not used")
}
func (s *stubAPI) Members(context.Context, string, string) ([]Member, error) {
	return nil, errors.New("not used")
}
func (s *stubAPI) Channels(context.Context, string, string) ([]Channel, error) {
	return nil, errors.New("not used")
}

// waitFor polls until cond holds, or fails the test.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestSocketConnectsEveryGeo: a bot connected in two regions out of three
// silently misses conversations hosted in the third, so the status is only
// "connected" once every geo is up.
func TestSocketConnectsEveryGeo(t *testing.T) {
	srv := newHubServer(t)
	srv.autoReady = true
	api := newStubAPI(srv.url())

	s := &Socket{API: api, HTTP: srv.srv.Client(), Logf: t.Logf}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()

	waitFor(t, "every geo to connect", func() bool { return s.Status().Connected() })
	st := s.Status()
	if len(st.Geos) != 3 {
		t.Fatalf("status covers %d geos, want 3", len(st.Geos))
	}
	var got []string
	for _, g := range st.Geos {
		got = append(got, g.Geo)
	}
	if !slices.Equal(got, Geos) {
		t.Errorf("connected geos = %v, want %v", got, Geos)
	}
	for _, geo := range Geos {
		if n := len(api.callsFor(geo)); n != 1 {
			t.Errorf("geo %s was connected %d times, want 1", geo, n)
		}
	}

	cancel()
	<-done
	if s.Status().State != "off" {
		t.Errorf("after shutdown state = %q, want off", s.Status().State)
	}
}

// TestSocketRepliesToActivities: ACS counts a missing or late reply as a
// failed delivery, and for an invoke the user sees an error. Every
// envelope gets a reply frame naming it.
func TestSocketRepliesToActivities(t *testing.T) {
	srv := newHubServer(t)
	srv.autoReady = true
	api := newStubAPI(srv.url())

	var seen []string
	var mu sync.Mutex
	s := &Socket{
		API: api, HTTP: srv.srv.Client(), Logf: t.Logf,
		Regions: []string{"amer"},
		Handle: func(ctx context.Context, env Envelope) *InvokeResponse {
			mu.Lock()
			seen = append(seen, env.Payload.Type)
			mu.Unlock()
			if env.Type == "invoke" {
				r := MessageResponse("done")
				return &r
			}
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	sc := srv.socket()
	defer sc.close()
	waitFor(t, "amer to connect", func() bool { return s.Status().Connected() })

	// A plain message: acknowledged with 200 and no body.
	if err := sc.send(ctx, activityFrame("i1", `{"protocolVersion":1,"type":"activity","envelopeId":"e1","ackRequired":true,"deadlineMs":25000,"payload":{"type":"message","text":"hi"}}`)); err != nil {
		t.Fatalf("send: %v", err)
	}
	m, err := sc.nextCompletion(ctx)
	if err != nil {
		t.Fatalf("completion: %v", err)
	}
	var reply Reply
	remarshal(t, m.Result, &reply)
	if reply.EnvelopeID != "e1" || reply.Status != 200 {
		t.Errorf("reply = %+v, want envelope e1 with status 200", reply)
	}
	if reply.Body != nil {
		t.Errorf("a plain activity should get no body, got %v", reply.Body)
	}
	if reply.ProtocolVersion != 1 {
		t.Errorf("reply protocolVersion = %d", reply.ProtocolVersion)
	}
	if reply.RecvAt == 0 || reply.TS == 0 {
		t.Errorf("reply should carry recvAt and ts for ACS's timing: %+v", reply)
	}

	// An invoke: the handler's response rides back in the body.
	if err := sc.send(ctx, activityFrame("i2", `{"protocolVersion":1,"type":"invoke","envelopeId":"e2","deadlineMs":25000,"payload":{"type":"invoke","name":"adaptiveCard/action"}}`)); err != nil {
		t.Fatalf("send: %v", err)
	}
	if m, err = sc.nextCompletion(ctx); err != nil {
		t.Fatalf("completion: %v", err)
	}
	raw, _ := json.Marshal(m.Result)
	if !strings.Contains(string(raw), `"envelopeId":"e2"`) {
		t.Errorf("reply = %s, want envelope e2", raw)
	}
	if !strings.Contains(string(raw), InvokeMessage) || !strings.Contains(string(raw), `"done"`) {
		t.Errorf("the invoke response did not ride back in the body: %s", raw)
	}

	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(seen, []string{"message", "invoke"}) {
		t.Errorf("handler saw %v", seen)
	}
}

// TestSocketHandlerDeadline: the handler is cut off before ACS stops
// waiting, so a slow one fails the activity rather than the connection.
func TestSocketHandlerDeadline(t *testing.T) {
	srv := newHubServer(t)
	srv.autoReady = true
	api := newStubAPI(srv.url())

	deadlines := make(chan time.Duration, 1)
	s := &Socket{
		API: api, HTTP: srv.srv.Client(), Logf: t.Logf,
		Regions: []string{"amer"},
		Handle: func(ctx context.Context, env Envelope) *InvokeResponse {
			d, ok := ctx.Deadline()
			if !ok {
				deadlines <- 0
				return nil
			}
			deadlines <- time.Until(d)
			return nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	sc := srv.socket()
	defer sc.close()
	waitFor(t, "amer to connect", func() bool { return s.Status().Connected() })

	if err := sc.send(ctx, activityFrame("i1", `{"protocolVersion":1,"type":"activity","envelopeId":"e1","deadlineMs":2000,"payload":{"type":"message"}}`)); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case d := <-deadlines:
		// 2s deadline less the slack reserved for getting the reply out.
		if d <= 0 || d > 2*time.Second-replyDeadlineSlack {
			t.Errorf("handler deadline = %s, want a little under 2s", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the handler never ran")
	}
}

// TestSocketTerminalFailureStopsRetrying: a bad credential or socket mode
// switched off will not fix itself, and retrying only hammers ACS. The
// admin page gets the reason instead.
func TestSocketTerminalFailureStopsRetrying(t *testing.T) {
	srv := newHubServer(t)
	srv.autoReady = true
	api := newStubAPI(srv.url())
	api.setErr(&APIError{
		Call: "connect", Status: http.StatusForbidden,
		Code: "Forbidden", Message: "Socket mode is not enabled for this bot.",
	})

	s := &Socket{API: api, HTTP: srv.srv.Client(), Logf: t.Logf, Regions: []string{"amer"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run kept retrying a 403")
	}
	if n := len(api.calls()); n != 1 {
		t.Errorf("tried to connect %d times after a 403, want 1", n)
	}
	// Run sets every geo to off as it leaves; the point is that it left at
	// all rather than looping.
	if got := len(api.callsFor("amer")); got != 1 {
		t.Errorf("amer was retried %d times", got)
	}
}

// TestSocketRetriesTransientFailure: a 503 is ACS having a moment, so the
// socket backs off and comes back.
func TestSocketRetriesTransientFailure(t *testing.T) {
	srv := newHubServer(t)
	srv.autoReady = true
	api := newStubAPI(srv.url())
	api.setErr(&APIError{Call: "connect", Status: http.StatusServiceUnavailable, Code: "Unavailable", Message: "try later"})

	s := &Socket{API: api, HTTP: srv.srv.Client(), Logf: t.Logf, Regions: []string{"amer"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	waitFor(t, "the first attempt", func() bool { return len(api.calls()) >= 1 })
	if s.Status().Connected() {
		t.Error("reported connected while ACS was failing")
	}
	api.setErr(nil)
	waitFor(t, "amer to recover", func() bool { return s.Status().Connected() })
}

// TestSocketReplacesBeforeExpiry: the socket dies when its token does, so
// a replacement is opened first, naming the old connection. Without the
// header ACS would count the overlap against the socket limit and keep
// sending to the socket that is about to go.
func TestSocketReplacesBeforeExpiry(t *testing.T) {
	srv := newHubServer(t)
	srv.autoReady = true
	api := newStubAPI(srv.url())
	// Shorter than replaceBefore, so the refresh is already due and the
	// test does not wait a minute for it.
	api.expiresIn = 1

	s := &Socket{API: api, HTTP: srv.srv.Client(), Logf: t.Logf, Regions: []string{"amer"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	// The first connect replaces nothing; the next one must name the
	// connection ACS gave us in SocketReady.
	first := <-api.signal
	if first.replaces != "" {
		t.Errorf("the first connect claimed to replace %q", first.replaces)
	}
	select {
	case second := <-api.signal:
		if second.replaces == "" {
			t.Error("the replacement connect did not name the socket it replaces")
		}
		if !strings.HasPrefix(second.replaces, "conn-") {
			t.Errorf("replaces = %q, want the connection ID from SocketReady", second.replaces)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the socket was never replaced before its token expired")
	}
}

// TestSocketModeDisabledStops: when socket mode is turned off, ACS says so
// and then refuses new connections. Reconnecting would just collect 403s.
func TestSocketModeDisabledStops(t *testing.T) {
	srv := newHubServer(t)
	srv.autoReady = true
	api := newStubAPI(srv.url())

	s := &Socket{API: api, HTTP: srv.srv.Client(), Logf: t.Logf, Regions: []string{"amer"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); s.Run(ctx) }()

	sc := srv.socket()
	waitFor(t, "amer to connect", func() bool { return s.Status().Connected() })

	notice := fmt.Sprintf(`{"type":1,"target":"SocketClosing","arguments":[{"protocolVersion":1,"connectionId":%q,"code":%q,"reason":"Socket mode is no longer enabled for this bot.","reconnect":false}]}`,
		sc.connectionID, closeDisabled)
	if err := sc.send(ctx, notice, `{"type":7}`); err != nil {
		t.Fatalf("send: %v", err)
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the socket kept reconnecting after socket mode was disabled")
	}
	if n := len(api.calls()); n != 1 {
		t.Errorf("connected %d times, want no reconnect after SocketModeDisabled", n)
	}
	if st := s.Status(); st.LastError == "" {
		t.Error("the admin page would show no reason for the disconnection")
	}
}

// TestSocketReconnectsAfterAnUnexpectedClose: most closes come with no
// notice at all, and every one of them is temporary.
func TestSocketReconnectsAfterAnUnexpectedClose(t *testing.T) {
	srv := newHubServer(t)
	srv.autoReady = true
	api := newStubAPI(srv.url())

	s := &Socket{API: api, HTTP: srv.srv.Client(), Logf: t.Logf, Regions: []string{"amer"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	sc := srv.socket()
	waitFor(t, "amer to connect", func() bool { return s.Status().Connected() })
	sc.close() // the connection simply drops

	waitFor(t, "a reconnect", func() bool { return len(api.callsFor("amer")) >= 2 })
	waitFor(t, "amer to be connected again", func() bool { return s.Status().Connected() })
}

// activityFrame wraps an ACS envelope as the SignalR invocation ACS
// sends it in. The invocation ID is what the reply must name back.
func activityFrame(invocationID, envelope string) string {
	return fmt.Sprintf(`{"type":1,"invocationId":%q,"target":"Activity","arguments":[%s]}`, invocationID, envelope)
}

// remarshal decodes a value that arrived as `any` into a typed struct.
func remarshal(t *testing.T, in any, out any) {
	t.Helper()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
}

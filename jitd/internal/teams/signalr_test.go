package teams

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// hubServer is a stand-in for Azure SignalR: it negotiates, accepts one
// WebSocket, answers the protocol handshake and then does what the test
// tells it to.
type hubServer struct {
	t    *testing.T
	srv  *httptest.Server
	sock chan *serverConn

	mu         sync.Mutex
	negotiates int
	lastToken  string
	lastQuery  map[string][]string
	redirectTo string
	noWS       bool
	badHello   string
	// autoReady makes the server send SocketReady as soon as a socket is
	// up, which is what ACS does and what the Socket tests need.
	autoReady bool
	readySeq  int
}

func newHubServer(t *testing.T) *hubServer {
	t.Helper()
	h := &hubServer{t: t, sock: make(chan *serverConn, 8)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /client/negotiate", h.negotiate)
	mux.HandleFunc("POST /moved/negotiate", h.negotiate)
	mux.HandleFunc("GET /client/", h.accept)
	h.srv = httptest.NewServer(mux)
	t.Cleanup(h.srv.Close)
	return h
}

// url is what ACS's connect call would hand back.
func (h *hubServer) url() string { return h.srv.URL + "/client/?hub=bothub&asrs.op=%2Fclient" }

func (h *hubServer) negotiate(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.negotiates++
	h.lastToken = r.Header.Get("Authorization")
	h.lastQuery = r.URL.Query()
	redirect, noWS := h.redirectTo, h.noWS
	h.mu.Unlock()

	if redirect != "" {
		h.mu.Lock()
		h.redirectTo = "" // only redirect once
		h.mu.Unlock()
		writeJSON(w, map[string]any{"url": redirect, "accessToken": "redirected-token"})
		return
	}
	if noWS {
		writeJSON(w, map[string]any{
			"connectionToken":     "ct",
			"availableTransports": []any{map[string]any{"transport": "LongPolling"}},
		})
		return
	}
	writeJSON(w, map[string]any{
		"connectionToken":     "connection-token-1",
		"connectionId":        "conn-1",
		"negotiateVersion":    1,
		"availableTransports": []any{map[string]any{"transport": "WebSockets"}},
	})
}

func (h *hubServer) accept(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		h.t.Errorf("accept: %v", err)
		return
	}
	sc := &serverConn{c: c, query: r.URL.Query()}

	// The protocol handshake: the client speaks first.
	msgs, err := sc.read(r.Context())
	if err != nil || len(msgs) == 0 {
		h.t.Errorf("reading the handshake: %v", err)
		return
	}
	sc.handshake = string(msgs[0])
	h.mu.Lock()
	bad := h.badHello
	h.mu.Unlock()
	hello := "{}"
	if bad != "" {
		hello = fmt.Sprintf(`{"error":%q}`, bad)
	}
	if err := sc.writeRaw(r.Context(), []byte(hello)); err != nil {
		h.t.Errorf("writing the handshake response: %v", err)
		return
	}
	if bad != "" {
		// A server that rejects the handshake closes the connection.
		c.Close(websocket.StatusNormalClosure, "no")
		return
	}
	h.mu.Lock()
	auto := h.autoReady
	h.readySeq++
	connID := fmt.Sprintf("conn-%d", h.readySeq)
	h.mu.Unlock()
	if auto {
		if err := sc.send(r.Context(), fmt.Sprintf(`{"type":1,"target":"SocketReady","arguments":[{"connectionId":%q}]}`, connID)); err != nil {
			h.t.Errorf("sending SocketReady: %v", err)
			return
		}
		sc.connectionID = connID
	}
	// Accept hijacked the connection, so it outlives this handler and
	// httptest stops tracking it. Returning here keeps Close from blocking.
	h.sock <- sc
}

// socket waits for the client to connect.
func (h *hubServer) socket() *serverConn {
	h.t.Helper()
	select {
	case sc := <-h.sock:
		return sc
	case <-time.After(5 * time.Second):
		h.t.Fatal("the client never connected")
		return nil
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// serverConn is the server's end of one connection.
type serverConn struct {
	c            *websocket.Conn
	query        map[string][]string
	handshake    string
	connectionID string
	fr           framer
}

func (s *serverConn) writeRaw(ctx context.Context, b []byte) error {
	return s.c.Write(ctx, websocket.MessageText, append(bytes.Clone(b), recordSeparator))
}

// send writes messages, all in one WebSocket frame, as SignalR may.
func (s *serverConn) send(ctx context.Context, msgs ...string) error {
	var buf bytes.Buffer
	for _, m := range msgs {
		buf.WriteString(m)
		buf.WriteByte(recordSeparator)
	}
	return s.c.Write(ctx, websocket.MessageText, buf.Bytes())
}

func (s *serverConn) read(ctx context.Context) ([][]byte, error) {
	for {
		if msgs := s.fr.pending(); len(msgs) > 0 {
			return msgs, nil
		}
		_, b, err := s.c.Read(ctx)
		if err != nil {
			return nil, err
		}
		msgs, err := s.fr.push(b)
		if err != nil {
			return nil, err
		}
		if len(msgs) > 0 {
			return msgs, nil
		}
	}
}

// nextCompletion reads until a completion arrives, skipping pings.
func (s *serverConn) nextCompletion(ctx context.Context) (hubMessage, error) {
	for {
		msgs, err := s.read(ctx)
		if err != nil {
			return hubMessage{}, err
		}
		for _, raw := range msgs {
			var m hubMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				return hubMessage{}, err
			}
			if m.Type == msgCompletion {
				return m, nil
			}
		}
	}
}

// close drops the connection without the closing handshake: by the time a
// test calls it the client has usually stopped reading, and waiting for a
// reply that will not come costs five seconds a test.
func (s *serverConn) close() { s.c.CloseNow() }

// TestHubClientResults is the whole point of this client: ACS invokes
// "Activity" and waits for the reply on the same call. It also covers the
// negotiate handshake, the protocol handshake, several messages in one
// frame, and pings.
func TestHubClientResults(t *testing.T) {
	srv := newHubServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	h, err := dialHub(ctx, srv.srv.Client(), DialWebSocket, srv.url(), "entra-token", t.Logf)
	if err != nil {
		t.Fatalf("dialHub: %v", err)
	}
	defer h.close()

	ready := make(chan struct{})
	served := make(chan error, 1)
	go func() {
		served <- h.serve(ctx, func(ctx context.Context, call hubCall) (any, error) {
			switch call.Target {
			case "SocketReady":
				close(ready)
				return nil, nil
			case "Activity":
				var env struct {
					EnvelopeID string `json:"envelopeId"`
				}
				if err := json.Unmarshal(call.Arg, &env); err != nil {
					return nil, err
				}
				if !call.WantsResult {
					t.Errorf("Activity should want a result")
				}
				return map[string]any{"protocolVersion": 1, "envelopeId": env.EnvelopeID, "status": 200}, nil
			case "Boom":
				return nil, fmt.Errorf("handling failed")
			}
			return nil, nil
		})
	}()

	sc := srv.socket()
	defer sc.close()

	if sc.handshake != `{"protocol":"json","version":1}` {
		t.Errorf("handshake = %q", sc.handshake)
	}
	if got := sc.query["id"]; len(got) != 1 || got[0] != "connection-token-1" {
		t.Errorf("the connection token did not reach the socket URL: id=%v", got)
	}
	if got := sc.query["access_token"]; len(got) != 1 || got[0] != "entra-token" {
		t.Errorf("the access token did not reach the socket URL: access_token=%v", got)
	}
	if got := sc.query["hub"]; len(got) != 1 || got[0] != "bothub" {
		t.Errorf("ACS's own query was dropped: hub=%v", got)
	}
	if srv.lastToken != "Bearer entra-token" {
		t.Errorf("negotiate Authorization = %q", srv.lastToken)
	}
	if got := srv.lastQuery["negotiateVersion"]; len(got) != 1 || got[0] != "1" {
		t.Errorf("negotiateVersion = %v", got)
	}

	// SocketReady with no invocation ID (fire and forget) and a ping, in one
	// frame, as SignalR is allowed to send them.
	if err := sc.send(ctx, `{"type":1,"target":"SocketReady","arguments":[{"connectionId":"conn-1"}]}`, `{"type":6}`); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("SocketReady never reached the handler")
	}

	// Two activities without waiting for the first reply: ACS does this, and
	// the replies may come back in either order.
	if err := sc.send(ctx,
		`{"type":1,"invocationId":"i1","target":"Activity","arguments":[{"envelopeId":"e1"}]}`,
		`{"type":1,"invocationId":"i2","target":"Activity","arguments":[{"envelopeId":"e2"}]}`,
	); err != nil {
		t.Fatalf("send: %v", err)
	}
	got := map[string]string{}
	for range 2 {
		m, err := sc.nextCompletion(ctx)
		if err != nil {
			t.Fatalf("reading a completion: %v", err)
		}
		b, _ := json.Marshal(m.Result)
		got[m.InvocationID] = string(b)
	}
	for id, envelope := range map[string]string{"i1": "e1", "i2": "e2"} {
		if !strings.Contains(got[id], `"envelopeId":"`+envelope+`"`) {
			t.Errorf("completion %s = %s, want envelopeId %s", id, got[id], envelope)
		}
		if !strings.Contains(got[id], `"status":200`) {
			t.Errorf("completion %s = %s, want status 200", id, got[id])
		}
	}

	// A handler that fails becomes an error completion, not a dropped call.
	if err := sc.send(ctx, `{"type":1,"invocationId":"i3","target":"Boom","arguments":[{}]}`); err != nil {
		t.Fatalf("send: %v", err)
	}
	m, err := sc.nextCompletion(ctx)
	if err != nil {
		t.Fatalf("reading a completion: %v", err)
	}
	if m.InvocationID != "i3" || m.Error != "handling failed" {
		t.Errorf("error completion = %+v", m)
	}

	// A clean close ends serve without an error, so the supervisor reconnects
	// rather than reporting a fault.
	if err := sc.send(ctx, `{"type":7}`); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Errorf("serve after a clean close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return after the close frame")
	}
}

// TestHubPanicInHandlerStillReplies: a panic must not take the connection
// down, and ACS must still get a reply or it reports the activity failed.
func TestHubPanicInHandlerStillReplies(t *testing.T) {
	srv := newHubServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	h, err := dialHub(ctx, srv.srv.Client(), DialWebSocket, srv.url(), "tok", t.Logf)
	if err != nil {
		t.Fatalf("dialHub: %v", err)
	}
	defer h.close()
	go h.serve(ctx, func(ctx context.Context, call hubCall) (any, error) {
		panic("boom")
	})

	sc := srv.socket()
	defer sc.close()
	if err := sc.send(ctx, `{"type":1,"invocationId":"i1","target":"Activity","arguments":[{}]}`); err != nil {
		t.Fatalf("send: %v", err)
	}
	m, err := sc.nextCompletion(ctx)
	if err != nil {
		t.Fatalf("reading a completion: %v", err)
	}
	if m.InvocationID != "i1" || m.Error == "" {
		t.Errorf("after a panic, completion = %+v, want an error completion", m)
	}
}

// TestHubCloseWithError: ACS closing with a reason is a fault, so the
// supervisor can log it and back off.
func TestHubCloseWithError(t *testing.T) {
	srv := newHubServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	h, err := dialHub(ctx, srv.srv.Client(), DialWebSocket, srv.url(), "tok", t.Logf)
	if err != nil {
		t.Fatalf("dialHub: %v", err)
	}
	defer h.close()
	served := make(chan error, 1)
	go func() {
		served <- h.serve(ctx, func(context.Context, hubCall) (any, error) { return nil, nil })
	}()

	sc := srv.socket()
	defer sc.close()
	if err := sc.send(ctx, `{"type":7,"error":"server is going away"}`); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case err := <-served:
		if err == nil || !strings.Contains(err.Error(), "server is going away") {
			t.Errorf("serve = %v, want the close reason", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not return")
	}
}

// TestHubRejectedHandshake: a server that will not speak the JSON protocol
// must fail the dial, not hang.
func TestHubRejectedHandshake(t *testing.T) {
	srv := newHubServer(t)
	srv.badHello = "Requested protocol 'json' is not available."
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := dialHub(ctx, srv.srv.Client(), DialWebSocket, srv.url(), "tok", t.Logf); err == nil {
		t.Fatal("dialHub succeeded against a server that rejected the handshake")
	} else if !strings.Contains(err.Error(), "not available") {
		t.Errorf("dialHub = %v, want the server's reason", err)
	}
}

// TestNegotiateRedirect: Azure SignalR may send the client somewhere else,
// with a token for the new endpoint.
func TestNegotiateRedirect(t *testing.T) {
	srv := newHubServer(t)
	srv.redirectTo = srv.srv.URL + "/moved/"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	n, token, err := negotiate(ctx, srv.srv.Client(), srv.url(), "first-token")
	if err != nil {
		t.Fatalf("negotiate: %v", err)
	}
	if token != "redirected-token" {
		t.Errorf("token = %q, want the redirect's token", token)
	}
	if n.ConnectionToken != "connection-token-1" {
		t.Errorf("connection token = %q", n.ConnectionToken)
	}
	if srv.negotiates != 2 {
		t.Errorf("negotiated %d times, want 2", srv.negotiates)
	}
	if srv.lastToken != "Bearer redirected-token" {
		t.Errorf("the second negotiate used %q", srv.lastToken)
	}
}

// TestNegotiateWithoutWebSockets: long polling is not implemented, so say so
// rather than dialling something that cannot work.
func TestNegotiateWithoutWebSockets(t *testing.T) {
	srv := newHubServer(t)
	srv.noWS = true
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, _, err := negotiate(ctx, srv.srv.Client(), srv.url(), "tok"); err == nil {
		t.Fatal("negotiate succeeded with no WebSockets transport offered")
	} else if !strings.Contains(err.Error(), "WebSockets") {
		t.Errorf("negotiate = %v", err)
	}
}

func TestNegotiateURL(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://x.service.signalr.net/client/?hub=bothub", "https://x.service.signalr.net/client/negotiate?hub=bothub&negotiateVersion=1"},
		{"https://x.service.signalr.net/client?hub=bothub", "https://x.service.signalr.net/client/negotiate?hub=bothub&negotiateVersion=1"},
		{"https://x.service.signalr.net/client/?negotiateVersion=0", "https://x.service.signalr.net/client/negotiate?negotiateVersion=0"},
	} {
		got, err := negotiateURL(tc.in)
		if err != nil {
			t.Errorf("negotiateURL(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("negotiateURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSocketURL(t *testing.T) {
	got, err := socketURL("https://x.service.signalr.net/client/?hub=bothub", "ct", "tok")
	if err != nil {
		t.Fatalf("socketURL: %v", err)
	}
	want := "wss://x.service.signalr.net/client/?access_token=tok&hub=bothub&id=ct"
	if got != want {
		t.Errorf("socketURL = %q, want %q", got, want)
	}
	if got, _ := socketURL("http://127.0.0.1:1/client/", "", ""); got != "ws://127.0.0.1:1/client/" {
		t.Errorf("plain http should become ws, got %q", got)
	}
}

// TestFramer covers the two shapes the record separator allows: several
// messages in one frame, and one message split across frames.
func TestFramer(t *testing.T) {
	var f framer
	msgs, err := f.push([]byte("{\"a\":1}\x1e{\"b\":2}\x1e"))
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(msgs) != 2 || string(msgs[0]) != `{"a":1}` || string(msgs[1]) != `{"b":2}` {
		t.Fatalf("two in one frame: %q", msgs)
	}

	if msgs, err = f.push([]byte(`{"c":`)); err != nil || len(msgs) != 0 {
		t.Fatalf("a partial message yielded %q, %v", msgs, err)
	}
	if msgs, err = f.push([]byte("3}\x1e")); err != nil {
		t.Fatalf("push: %v", err)
	}
	if len(msgs) != 1 || string(msgs[0]) != `{"c":3}` {
		t.Fatalf("split across frames: %q", msgs)
	}

	// An empty record (two separators in a row) is not a message.
	if msgs, err = f.push([]byte("\x1e")); err != nil || len(msgs) != 0 {
		t.Fatalf("an empty record yielded %q, %v", msgs, err)
	}

	// A peer that never sends a separator must not grow memory without end.
	if _, err = f.push(bytes.Repeat([]byte("x"), maxFrame+1)); err == nil {
		t.Error("an oversized message was accepted")
	}
}

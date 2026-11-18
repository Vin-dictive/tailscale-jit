package teams

// Socket mode is SignalR underneath. This file is as much of the SignalR
// JSON hub protocol as ACS needs, written against the wire format for the
// same reason the Slack package is: the protocol is small, and there is no
// Microsoft-supported SignalR client for Go to depend on anyway.
//
// What ACS uses, and so what is here: the negotiate handshake, the protocol
// handshake, record-separated JSON frames, pings, and *client results* —
// ACS invokes the client method "Activity" and waits for the return value,
// which is the reply frame. What is not here: streaming (types 2, 4 and 5),
// client-to-server invocations, MessagePack, and stateful reconnect. None of
// them appear in the socket mode protocol.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// recordSeparator ends every message in the JSON hub protocol, so several
// can share one WebSocket frame.
const recordSeparator = 0x1e

// Hub message types. Only these four appear on a socket mode connection.
const (
	msgInvocation = 1
	msgCompletion = 3
	msgPing       = 6
	msgClose      = 7
)

const (
	// keepAlive is how often to ping when nothing else is being sent. The
	// SignalR default, and what the Teams SDK uses.
	keepAlive = 15 * time.Second
	// serverTimeout is how long to wait for anything at all before calling
	// the connection dead. The SignalR default: two missed keep-alives.
	serverTimeout = 30 * time.Second
	// maxFrame caps what one unterminated message may buffer, so a peer
	// that never sends a separator cannot grow memory without bound.
	maxFrame = 8 << 20
)

// Conn is one WebSocket connection, as the hub needs it. The Fake
// implements it too, so tests drive a socket without a server.
type Conn interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, b []byte) error
	Close() error
}

// Dialer opens a WebSocket to url.
type Dialer func(ctx context.Context, url string, header http.Header) (Conn, error)

// DialWebSocket is the real Dialer.
func DialWebSocket(ctx context.Context, url string, header http.Header) (Conn, error) {
	dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(dctx, url, &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(4 << 20)
	return &wsConn{c}, nil
}

type wsConn struct{ c *websocket.Conn }

func (w *wsConn) Read(ctx context.Context) ([]byte, error) {
	_, b, err := w.c.Read(ctx)
	return b, err
}
func (w *wsConn) Write(ctx context.Context, b []byte) error {
	return w.c.Write(ctx, websocket.MessageText, b)
}

// Close drops the connection without the closing handshake. By the time
// anything here closes a socket it is finished with it and usually about
// to open its replacement, and a graceful close waits up to five seconds
// for a courtesy frame from a peer that is often already gone — five
// seconds added to every reconnect.
func (w *wsConn) Close() error { return w.c.CloseNow() }

// hubMessage is a JSON hub protocol frame. Which fields matter depends on
// Type; the rest are absent.
type hubMessage struct {
	Type         int               `json:"type"`
	InvocationID string            `json:"invocationId,omitempty"`
	Target       string            `json:"target,omitempty"`
	Arguments    []json.RawMessage `json:"arguments,omitempty"`
	Result       any               `json:"result,omitempty"`
	Error        string            `json:"error,omitempty"`
}

// hubCall is one server-to-client invocation.
type hubCall struct {
	// Target is the client method: "Activity", "SocketReady" or
	// "SocketClosing".
	Target string
	// Arg is the first argument, which is all ACS ever sends.
	Arg json.RawMessage
	// WantsResult says the server asked for a return value (it sent an
	// invocation ID). Only "Activity" does.
	WantsResult bool
}

// hubHandler handles one invocation. Its return value becomes the completion
// when the server asked for a result; an error becomes an error completion.
// It may be called concurrently: ACS does not wait for one reply before
// sending the next activity.
type hubHandler func(ctx context.Context, call hubCall) (any, error)

// negotiation is the part of a SignalR negotiate response that matters here.
type negotiation struct {
	ConnectionToken     string `json:"connectionToken"`
	ConnectionID        string `json:"connectionId"`
	NegotiateVersion    int    `json:"negotiateVersion"`
	AvailableTransports []struct {
		Transport string `json:"transport"`
	} `json:"availableTransports"`
	// Set when the service redirects to another endpoint.
	URL         string `json:"url"`
	AccessToken string `json:"accessToken"`
	Error       string `json:"error"`
}

// negotiate runs the SignalR negotiate handshake against raw, returning the
// negotiation and the token to use from then on.
//
// Azure SignalR does not allow skipping negotiate, so this always runs, even
// though ACS already handed us a URL that looks ready to dial.
func negotiate(ctx context.Context, hc *http.Client, raw, token string) (negotiation, string, error) {
	// The service may redirect once to the endpoint that will actually hold
	// the connection. More than a couple of hops is a loop.
	for range 3 {
		u, err := negotiateURL(raw)
		if err != nil {
			return negotiation{}, "", err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, nil)
		if err != nil {
			return negotiation{}, "", err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Length", "0")
		resp, err := hc.Do(req)
		if err != nil {
			return negotiation{}, "", fmt.Errorf("negotiate: %w", err)
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if err != nil {
			return negotiation{}, "", fmt.Errorf("negotiate: reading the response: %w", err)
		}
		if resp.StatusCode/100 != 2 {
			return negotiation{}, "", fmt.Errorf("negotiate: %s: %s", resp.Status, strings.TrimSpace(string(body)))
		}
		var n negotiation
		if err := json.Unmarshal(body, &n); err != nil {
			return negotiation{}, "", fmt.Errorf("negotiate: the response is not JSON: %w", err)
		}
		if n.Error != "" {
			return negotiation{}, "", fmt.Errorf("negotiate: %s", n.Error)
		}
		if n.URL != "" { // a redirect: start again at the new endpoint
			raw = n.URL
			if n.AccessToken != "" {
				token = n.AccessToken
			}
			continue
		}
		if !supportsWebSockets(n) {
			return negotiation{}, "", errors.New("negotiate: the service does not offer the WebSockets transport")
		}
		return n, token, nil
	}
	return negotiation{}, "", errors.New("negotiate: too many redirects")
}

func supportsWebSockets(n negotiation) bool {
	// An empty list is what a service that only speaks WebSockets sends.
	if len(n.AvailableTransports) == 0 {
		return true
	}
	for _, t := range n.AvailableTransports {
		if strings.EqualFold(t.Transport, "WebSockets") {
			return true
		}
	}
	return false
}

// negotiateURL is raw with "negotiate" appended to the path and
// negotiateVersion added, keeping the query ACS put there.
func negotiateURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("negotiate: bad URL %q: %w", raw, err)
	}
	if strings.HasSuffix(u.Path, "/") {
		u.Path += "negotiate"
	} else {
		u.Path += "/negotiate"
	}
	q := u.Query()
	if !q.Has("negotiateVersion") {
		q.Set("negotiateVersion", "1")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// socketURL is raw as a ws:// or wss:// URL carrying the connection token
// and the access token, the way the JavaScript client builds it.
func socketURL(raw, connectionToken, token string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("bad socket URL %q: %w", raw, err)
	}
	switch u.Scheme {
	case "https":
		u.Scheme = "wss"
	case "http":
		u.Scheme = "ws"
	}
	q := u.Query()
	if connectionToken != "" {
		q.Set("id", connectionToken)
	}
	// Browsers cannot set headers on a WebSocket, so SignalR takes the token
	// in the query. Go could use a header, but ACS is only known to accept
	// what the supported clients send.
	if token != "" {
		q.Set("access_token", token)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// hub is one live SignalR connection.
type hub struct {
	conn Conn
	logf func(format string, args ...any)

	wmu sync.Mutex // writes from handlers and the pinger must not interleave
	fr  framer
}

// dialHub negotiates, dials and completes the protocol handshake, returning
// a connection ready to serve.
func dialHub(ctx context.Context, hc *http.Client, dial Dialer, raw, token string, logf func(string, ...any)) (*hub, error) {
	n, token, err := negotiate(ctx, hc, raw, token)
	if err != nil {
		return nil, err
	}
	wsURL, err := socketURL(raw, n.ConnectionToken, token)
	if err != nil {
		return nil, err
	}
	if dial == nil {
		dial = DialWebSocket
	}
	conn, err := dial(ctx, wsURL, http.Header{"Authorization": {"Bearer " + token}})
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	h := &hub{conn: conn, logf: logf}
	if err := h.handshake(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	return h, nil
}

// handshake sends the protocol handshake and waits for the server to accept
// it. It must be the first thing on the connection.
func (h *hub) handshake(ctx context.Context) error {
	hctx, cancel := context.WithTimeout(ctx, serverTimeout)
	defer cancel()
	if err := h.writeRaw(hctx, []byte(`{"protocol":"json","version":1}`)); err != nil {
		return fmt.Errorf("handshake: %w", err)
	}
	for {
		msgs, err := h.read(hctx)
		if err != nil {
			return fmt.Errorf("handshake: %w", err)
		}
		if len(msgs) == 0 {
			continue
		}
		// The handshake response is {} on success, {"error":"..."} if the
		// server will not speak the JSON protocol. Anything after it in the
		// same frame is a real message, which serve will pick up because the
		// framer keeps it.
		var resp struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(msgs[0], &resp); err != nil {
			return fmt.Errorf("handshake: the response is not JSON: %w", err)
		}
		if resp.Error != "" {
			return fmt.Errorf("handshake: %s", resp.Error)
		}
		h.fr.unread(msgs[1:])
		return nil
	}
}

// serve reads until the connection ends, dispatching invocations to handle.
// It returns nil when the server closed cleanly and an error otherwise.
func (h *hub) serve(ctx context.Context, handle hubHandler) error {
	// Order matters: the deferred calls run last-registered first, so the
	// context is cancelled before the wait. The other way round, the pinger
	// would still be waiting on it and serve would never return.
	var wg sync.WaitGroup
	defer wg.Wait() // let in-flight replies finish before the caller closes conn
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	wg.Add(1)
	go func() {
		defer wg.Done()
		h.ping(ctx)
	}()

	for {
		msgs, err := h.read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for _, raw := range msgs {
			var m hubMessage
			if err := json.Unmarshal(raw, &m); err != nil {
				h.logf("teams socket: bad frame: %v", err)
				continue
			}
			switch m.Type {
			case msgPing:
				// Nothing to do: reading it is the proof the peer is alive.
			case msgClose:
				if m.Error != "" {
					return fmt.Errorf("the service closed the connection: %s", m.Error)
				}
				return nil
			case msgInvocation:
				call := hubCall{Target: m.Target, WantsResult: m.InvocationID != ""}
				if len(m.Arguments) > 0 {
					call.Arg = m.Arguments[0]
				}
				id := m.InvocationID
				wg.Add(1)
				go func() {
					defer wg.Done()
					h.dispatch(ctx, handle, call, id)
				}()
			case msgCompletion:
				// Only sent in reply to a client-to-server invocation, which
				// this client never makes.
			default:
				h.logf("teams socket: ignoring hub message type %d", m.Type)
			}
		}
	}
}

// dispatch runs one invocation and sends its completion, keeping a panic in
// the handler from taking the connection down.
func (h *hub) dispatch(ctx context.Context, handle hubHandler, call hubCall, invocationID string) {
	var (
		result any
		err    error
	)
	func() {
		defer func() {
			if p := recover(); p != nil {
				h.logf("teams socket: handler panic: %v", p)
				result, err = nil, fmt.Errorf("the agent failed to handle %s", call.Target)
			}
		}()
		result, err = handle(ctx, call)
	}()
	if invocationID == "" {
		return // fire and forget: the server asked for no result
	}
	done := hubMessage{Type: msgCompletion, InvocationID: invocationID}
	if err != nil {
		done.Error = err.Error()
	} else {
		done.Result = result
	}
	if werr := h.send(ctx, done); werr != nil && ctx.Err() == nil {
		h.logf("teams socket: sending the reply to %s: %v", call.Target, werr)
	}
}

// ping keeps the connection alive until ctx ends.
func (h *hub) ping(ctx context.Context) {
	t := time.NewTicker(keepAlive)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := h.send(ctx, hubMessage{Type: msgPing}); err != nil {
				return // the reader will see the same failure and end serve
			}
		}
	}
}

// send writes one hub message.
func (h *hub) send(ctx context.Context, m hubMessage) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return h.writeRaw(ctx, b)
}

func (h *hub) writeRaw(ctx context.Context, b []byte) error {
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	h.wmu.Lock()
	defer h.wmu.Unlock()
	return h.conn.Write(wctx, append(b, recordSeparator))
}

// read returns the complete messages in the next WebSocket frame, which may
// be none when a message spans frames.
func (h *hub) read(ctx context.Context) ([][]byte, error) {
	if msgs := h.fr.pending(); len(msgs) > 0 {
		return msgs, nil
	}
	rctx, cancel := context.WithTimeout(ctx, serverTimeout)
	defer cancel()
	b, err := h.conn.Read(rctx)
	if err != nil {
		return nil, err
	}
	return h.fr.push(b)
}

func (h *hub) close() error { return h.conn.Close() }

// framer splits the record-separated stream into whole messages. A WebSocket
// frame can carry several of them, and a message can in principle span
// frames, so the remainder is kept for next time.
type framer struct {
	buf  []byte
	held [][]byte
}

// push adds a frame and returns the messages it completes.
func (f *framer) push(b []byte) ([][]byte, error) {
	f.buf = append(f.buf, b...)
	var out [][]byte
	start := 0
	for {
		i := bytes.IndexByte(f.buf[start:], recordSeparator)
		if i < 0 {
			break
		}
		if msg := f.buf[start : start+i]; len(msg) > 0 {
			out = append(out, bytes.Clone(msg))
		}
		start += i + 1
	}
	f.buf = append(f.buf[:0], f.buf[start:]...) // copy handles the overlap
	if len(f.buf) > maxFrame {
		return nil, fmt.Errorf("a hub message grew past %d bytes with no record separator", maxFrame)
	}
	return out, nil
}

// unread puts messages back, for the handshake to hand on what shared its
// frame.
func (f *framer) unread(msgs [][]byte) {
	f.held = append(f.held, msgs...)
}

// pending takes whatever unread is holding.
func (f *framer) pending() [][]byte {
	msgs := f.held
	f.held = nil
	return msgs
}

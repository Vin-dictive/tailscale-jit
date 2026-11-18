package teams

// Socket mode delivery. ACS does not send activities to one connection: a
// bot opens one socket per geo and conversations arrive on whichever geo
// hosts them, so a bot connected only in amer silently misses everything in
// emea and apac. Each geo is therefore run on its own, with its own
// reconnects.
//
// Each socket also dies on a schedule: the token ACS hands out with it
// expires, and the socket goes with it. To avoid a gap, a replacement is
// opened a minute early, naming the old connection so ACS moves new
// activities across and does not count the overlap against the socket
// limit. The old one stays up briefly to finish what it already had.

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"time"
)

// Handler handles one activity. For an invoke, what it returns becomes the
// invoke response and the user sees the result, so it has to come back
// inside the envelope's deadline. For anything else the return value is
// ignored and handling it is the whole job.
type Handler func(ctx context.Context, env Envelope) *InvokeResponse

// Inbound is how activities reach jitd. Socket mode is the only
// implementation today; an HTTPS messaging endpoint would be another, and
// the bot would not know the difference.
type Inbound interface {
	// Run connects and serves until ctx ends.
	Run(ctx context.Context)
	// Status is what the admin page shows.
	Status() Status
}

const (
	// readyWait is how long to wait for SocketReady before giving up on a
	// socket and opening another.
	readyWait = 30 * time.Second
	// replaceBefore is how long before its token expires to replace a
	// socket.
	replaceBefore = 60 * time.Second
	// retireAfter is how long a replaced socket stays up, finishing the
	// activities it already received.
	retireAfter = 5 * time.Second
	// retireDrain caps that wait when a reply is stuck.
	retireDrain = 25 * time.Second
	// maxBackoff is the longest wait between reconnection attempts.
	maxBackoff = 15 * time.Second
	// replyDeadlineSlack is taken off the envelope's deadline, so the reply
	// is on the wire before ACS stops waiting.
	replyDeadlineSlack = 500 * time.Millisecond
	// maxReplyDeadline is what ACS allows at most.
	maxReplyDeadline = 25 * time.Second
)

// Status is the connection's state, for the admin page.
type Status struct {
	// State is the worst of the geos: "off", "connecting", "connected" or
	// "error". A bot connected in two geos out of three is not working, so
	// it does not get to say "connected".
	State     string      `json:"state"`
	Since     time.Time   `json:"since"`
	LastError string      `json:"last_error,omitempty"`
	Geos      []GeoStatus `json:"geos,omitempty"`
}

// GeoStatus is one region's state.
type GeoStatus struct {
	Geo       string `json:"geo"`
	State     string `json:"state"`
	LastError string `json:"last_error,omitempty"`
}

// Connected reports whether every geo is connected.
func (s Status) Connected() bool { return s.State == "connected" }

// Socket keeps one socket open per geo: it asks ACS for a session, dials,
// hands every activity to the handler and replies, replaces each socket
// before its token expires, and reconnects with backoff when one ends.
type Socket struct {
	API API
	// Dial opens the WebSocket; DialWebSocket if nil.
	Dial Dialer
	// HTTP is used for the SignalR negotiate call.
	HTTP *http.Client
	// Handle is called for every activity.
	Handle Handler
	// Geos to connect in; the package's Geos if nil.
	Regions []string
	// Logf gets connection events; may be nil.
	Logf func(format string, args ...any)

	mu    sync.Mutex
	geos  map[string]GeoStatus
	since time.Time
}

func (s *Socket) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

func (s *Socket) httpClient() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return http.DefaultClient
}

func (s *Socket) regions() []string {
	if len(s.Regions) > 0 {
		return s.Regions
	}
	return Geos
}

// Status returns the aggregate state across the geos.
func (s *Socket) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Status{State: "off", Since: s.since}
	for _, geo := range s.regions() {
		g, ok := s.geos[geo]
		if !ok {
			g = GeoStatus{Geo: geo, State: "off"}
		}
		out.Geos = append(out.Geos, g)
	}
	// The worst state wins: anything short of every geo connected means
	// some conversations never arrive.
	rank := map[string]int{"connected": 0, "connecting": 1, "off": 2, "error": 3}
	worst := -1
	for _, g := range out.Geos {
		if r := rank[g.State]; r > worst {
			worst, out.State = r, g.State
		}
		if g.LastError != "" && out.LastError == "" {
			out.LastError = g.Geo + ": " + g.LastError
		}
	}
	return out
}

func (s *Socket) setState(geo, state, lastErr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.geos == nil {
		s.geos = map[string]GeoStatus{}
		s.since = time.Now()
	}
	g := s.geos[geo]
	if g.State != state {
		s.since = time.Now()
	}
	g.Geo, g.State = geo, state
	if lastErr != "" || state == "connected" {
		g.LastError = lastErr
	}
	s.geos[geo] = g
}

// Run connects in every geo and serves until ctx ends.
func (s *Socket) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, geo := range s.regions() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.runGeo(ctx, geo)
		}()
	}
	wg.Wait()
	for _, geo := range s.regions() {
		s.setState(geo, "off", "")
	}
}

// runGeo keeps one geo connected: open, serve, replace before the token
// expires, reconnect when it ends.
func (s *Socket) runGeo(ctx context.Context, geo string) {
	cur, err := s.openWithRetry(ctx, geo, "")
	if err != nil {
		return // terminal, or ctx ended; openWithRetry recorded why
	}
	defer func() { cur.stop() }()

	for {
		refresh := time.NewTimer(max(time.Until(cur.expiresAt)-replaceBefore, 0))
		select {
		case <-ctx.Done():
			refresh.Stop()
			return

		case <-cur.closed:
			refresh.Stop()
			if err := cur.result(); err != nil {
				s.setState(geo, "error", err.Error())
				s.logf("teams socket (%s): %v; reconnecting", geo, err)
			} else {
				s.logf("teams socket (%s): closed; reconnecting", geo)
			}
			if code := cur.closingCode(); code == closeDisabled {
				// Socket mode was turned off for this bot. Reconnecting
				// only earns a 403; someone has to turn it back on.
				s.setState(geo, "error", "socket mode is no longer enabled for this bot")
				return
			}
			next, err := s.openWithRetry(ctx, geo, "")
			if err != nil {
				return
			}
			cur = next

		case <-refresh.C:
			// The token is nearly up. Open the replacement first, so there
			// is no window where activities have nowhere to go.
			next, err := s.openWithRetry(ctx, geo, cur.connectionID)
			if err != nil {
				return
			}
			go retire(cur)
			cur = next
		}
	}
}

// openWithRetry opens a socket, retrying until it works, ctx ends, or the
// failure is one that retrying cannot fix.
func (s *Socket) openWithRetry(ctx context.Context, geo, replaces string) (*socketConn, error) {
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		s.setState(geo, "connecting", "")
		sc, err := s.open(ctx, geo, replaces)
		if err == nil {
			s.setState(geo, "connected", "")
			s.logf("teams socket (%s): connected", geo)
			return sc, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		s.setState(geo, "error", err.Error())
		if Terminal(err) {
			// A bad credential or socket mode switched off. Retrying would
			// hammer ACS for nothing; the admin page shows the reason.
			s.logf("teams socket (%s): %v; not retrying until the configuration changes", geo, err)
			return nil, err
		}
		wait := RetryAfter(err)
		if wait <= 0 {
			wait = min(time.Duration(1<<min(attempt, 3))*time.Second, maxBackoff)
		}
		wait = time.Duration(float64(wait) * (0.8 + 0.4*rand.Float64()))
		s.logf("teams socket (%s): %v; retrying in %s", geo, err, wait.Round(time.Millisecond))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		// A replacement that failed is no longer replacing anything: the
		// old socket is gone by the time we get back here.
		replaces = ""
	}
}

// open gets a session, dials it and waits for SocketReady.
func (s *Socket) open(ctx context.Context, geo, replaces string) (*socketConn, error) {
	sess, err := s.API.Connect(ctx, geo, replaces)
	if err != nil {
		return nil, err
	}
	h, err := dialHub(ctx, s.httpClient(), s.Dial, sess.URL, sess.AccessToken, s.Logf)
	if err != nil {
		return nil, fmt.Errorf("connect (%s): %w", geo, err)
	}

	cctx, cancel := context.WithCancel(ctx)
	sc := &socketConn{
		geo:       geo,
		hub:       h,
		cancel:    cancel,
		closed:    make(chan struct{}),
		expiresAt: time.Now().Add(time.Duration(sess.ExpiresIn) * time.Second),
	}
	ready := make(chan string, 1)
	go func() {
		defer close(sc.closed)
		err := h.serve(cctx, func(hctx context.Context, call hubCall) (any, error) {
			switch call.Target {
			case "SocketReady":
				var f socketReady
				json.Unmarshal(call.Arg, &f)
				select {
				case ready <- f.ConnectionID:
				default:
				}
				return nil, nil
			case "SocketClosing":
				var f socketClosing
				json.Unmarshal(call.Arg, &f)
				sc.setClosing(f)
				s.logf("teams socket (%s): ACS is closing the socket: %s %s", geo, f.Code, f.Reason)
				return nil, nil
			case "Activity":
				return s.activity(hctx, sc, call)
			}
			return nil, nil
		})
		sc.setResult(err)
		h.close()
	}()

	select {
	case id := <-ready:
		sc.connectionID = id // written before anyone else can read it
		return sc, nil
	case <-sc.closed:
		cancel()
		if err := sc.result(); err != nil {
			return nil, fmt.Errorf("connect (%s): %w", geo, err)
		}
		return nil, fmt.Errorf("connect (%s): the socket closed before it was ready", geo)
	case <-time.After(readyWait):
		sc.stop()
		return nil, fmt.Errorf("connect (%s): no SocketReady within %s", geo, readyWait)
	case <-ctx.Done():
		sc.stop()
		return nil, ctx.Err()
	}
}

// activity hands one envelope to the handler and builds the reply frame.
func (s *Socket) activity(ctx context.Context, sc *socketConn, call hubCall) (any, error) {
	recvAt := time.Now()
	var env Envelope
	if err := json.Unmarshal(call.Arg, &env); err != nil {
		return nil, fmt.Errorf("the envelope is not JSON: %w", err)
	}
	sc.began()
	defer sc.ended()

	// ACS counts a late reply as a failed delivery, and for an invoke the
	// user sees an error, so the handler runs against its deadline.
	d := time.Duration(env.DeadlineMs) * time.Millisecond
	if d <= 0 || d > maxReplyDeadline {
		d = maxReplyDeadline
	}
	hctx, cancel := context.WithTimeout(ctx, max(d-replyDeadlineSlack, time.Second))
	defer cancel()

	reply := Reply{
		ProtocolVersion: 1,
		EnvelopeID:      env.EnvelopeID,
		Status:          http.StatusOK,
		RecvAt:          recvAt.UnixMilli(),
	}
	if s.Handle != nil {
		if resp := s.Handle(hctx, env); resp != nil && env.Type == "invoke" {
			reply.Body = resp
		}
	}
	reply.TS = time.Now().UnixMilli()
	return reply, nil
}

// retire closes a socket that has been replaced, after giving it a moment
// to finish the activities it already had.
func retire(sc *socketConn) {
	time.Sleep(retireAfter)
	giveUp := time.Now().Add(retireDrain)
	for sc.inFlight() > 0 && time.Now().Before(giveUp) {
		time.Sleep(250 * time.Millisecond)
	}
	sc.stop()
}

// socketConn is one live socket to ACS.
type socketConn struct {
	geo string
	hub *hub
	// connectionID is ACS's name for this socket, needed to replace it. It
	// is set once, before the socket is handed to anyone else.
	connectionID string
	expiresAt    time.Time
	cancel       context.CancelFunc
	// closed is closed when serve has returned.
	closed chan struct{}

	mu       sync.Mutex
	err      error
	closing  socketClosing
	inflight int
	stopped  bool
}

func (c *socketConn) setResult(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = err
}

// result is why serve ended, nil for a clean close.
func (c *socketConn) result() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *socketConn) setClosing(f socketClosing) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closing = f
}

func (c *socketConn) closingCode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closing.Code
}

func (c *socketConn) began() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inflight++
}

func (c *socketConn) ended() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inflight--
}

func (c *socketConn) inFlight() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.inflight
}

// stop ends the socket and waits for its goroutine. It is safe to call
// more than once, which matters because both the refresh path and the
// deferred cleanup in runGeo can reach it.
func (c *socketConn) stop() {
	c.mu.Lock()
	already := c.stopped
	c.stopped = true
	c.mu.Unlock()
	if already {
		<-c.closed
		return
	}
	c.cancel()
	<-c.closed
}

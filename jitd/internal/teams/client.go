// Package teams is the little of Microsoft Teams that jitd needs: the
// Agent Communication Service (ACS) calls that post and update a message,
// socket mode (jitd dials out; Teams never connects in) and the Adaptive
// Card pieces of an approval message. Like the slack package it is written
// against the wire format on purpose: the protocol is small, the Microsoft
// SDKs are not available for Go, and it keeps jitd's dependencies as they
// are.
package teams

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultLoginBase is Microsoft's identity platform, where the bot's token
// comes from.
const DefaultLoginBase = "https://login.microsoftonline.com"

// DefaultConnectBase is where socket mode connections are opened, with the
// geo filled in.
const DefaultConnectBase = "https://%s.botapi.skype.com"

// TokenScope is the only scope ACS accepts.
const TokenScope = "https://api.botframework.com/.default"

// MultiTenant is the tenant to ask for a token as when the app registration
// is multi-tenant rather than tied to one organisation.
const MultiTenant = "botframework.com"

// Geos are the regions ACS delivers from. A bot has to connect in every one
// of them: a socket in amer alone misses conversations hosted in emea.
var Geos = []string{"amer", "emea", "apac"}

// MaxMembers caps how much of a roster jitd will read, so a huge team
// cannot exhaust memory. Matches the cap the okta package puts on a group.
const MaxMembers = 2000

// memberPageSize is the largest page ACS allows.
const memberPageSize = 500

// API is what jitd calls on Teams. The real implementation is Client; the
// Fake stands in for tests and the preview.
type API interface {
	// AppID is the bot's Microsoft app ID, which is also its identity in a
	// conversation ("28:<app ID>").
	AppID() string
	// Token returns a current access token for ACS.
	Token(ctx context.Context) (string, error)
	// Connect opens a socket mode session in geo. When replaces names an
	// existing connection, this session takes its place rather than
	// counting against the socket limit.
	Connect(ctx context.Context, geo, replaces string) (Session, error)
	// SendActivity posts a to a conversation and returns the activity ID,
	// which is what UpdateActivity needs later.
	SendActivity(ctx context.Context, serviceURL, conversation string, a Activity) (string, error)
	// UpdateActivity replaces an activity already posted. This is how a
	// request card follows the request.
	UpdateActivity(ctx context.Context, serviceURL, conversation, activityID string, a Activity) error
	// CreateConversation opens the 1:1 chat between the bot and a user,
	// returning its conversation ID. It fails unless the app is installed
	// for that user or they are in a team where it is installed.
	CreateConversation(ctx context.Context, serviceURL, tenantID, userID string) (string, error)
	// Member returns one member of a conversation, with the email address
	// jitd matches to a Tailscale login.
	Member(ctx context.Context, serviceURL, conversation, userID string) (Member, error)
	// Members returns a conversation's roster, capped at MaxMembers.
	Members(ctx context.Context, serviceURL, conversation string) ([]Member, error)
	// Channels lists the channels of a team.
	Channels(ctx context.Context, serviceURL, teamID string) ([]Channel, error)
}

// Session is one socket mode connection, as ACS hands it over.
type Session struct {
	// URL is where to open the socket. It differs every time; it is not
	// worth storing.
	URL string `json:"url"`
	// AccessToken opens exactly one socket. Every reconnect needs a fresh
	// Connect call.
	AccessToken string `json:"accessToken"`
	// ExpiresIn is how many seconds the socket has before it is closed.
	ExpiresIn int `json:"expiresIn"`
	// ProtocolVersion is 1 for the protocol this package implements.
	ProtocolVersion int `json:"protocolVersion"`
}

// Member is one person in a conversation.
type Member struct {
	// ID is the "29:…" ID, specific to this bot.
	ID string `json:"id"`
	// AADObjectID is the person's Entra object ID, stable across apps.
	AADObjectID string `json:"aadObjectId"`
	Name        string `json:"name"`
	GivenName   string `json:"givenName"`
	Surname     string `json:"surname"`
	Email       string `json:"email"`
	// UserPrincipalName carries "#EXT#" for a guest from another tenant.
	UserPrincipalName string `json:"userPrincipalName"`
	TenantID          string `json:"tenantId"`
}

// Guest reports whether the member is a guest invited from elsewhere,
// rather than a member of the organisation. Their email is somebody else's
// to control, so jitd will not take it as a Tailscale login.
func (m Member) Guest() bool {
	return strings.Contains(strings.ToUpper(m.UserPrincipalName), "#EXT#")
}

// Channel is one channel of a team.
type Channel struct {
	ID string `json:"id"`
	// Name is empty for General, which ACS leaves unnamed on purpose so
	// clients can localise it.
	Name string `json:"name"`
}

// DisplayName is the channel's name, naming General since ACS does not.
func (c Channel) DisplayName() string {
	if c.Name == "" {
		return "General"
	}
	return c.Name
}

// APIError is a failed ACS or token call.
type APIError struct {
	Call    string
	Status  int
	Code    string
	Message string
	// RetryAfter is what the service asked for on a 429, zero otherwise.
	RetryAfter time.Duration
}

func (e *APIError) Error() string {
	var b strings.Builder
	b.WriteString(e.Call)
	b.WriteString(": ")
	if e.Code != "" {
		b.WriteString(e.Code)
		if e.Message != "" {
			b.WriteString(": ")
		}
	}
	if e.Message != "" {
		b.WriteString(e.Message)
	}
	if e.Code == "" && e.Message == "" {
		fmt.Fprintf(&b, "HTTP %d", e.Status)
	}
	return b.String()
}

// ErrorCode is the service's error code for err, empty if it has none.
func ErrorCode(err error) string {
	var e *APIError
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// ErrorStatus is the HTTP status behind err, zero if it has none.
func ErrorStatus(err error) int {
	var e *APIError
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// Terminal reports whether err will keep happening until someone fixes the
// configuration: a bad credential, or socket mode not turned on. Retrying
// one of these only burns requests.
func Terminal(err error) bool {
	switch ErrorStatus(err) {
	case http.StatusUnauthorized, http.StatusForbidden:
		return true
	}
	return false
}

// RetryAfter is how long the service asked us to wait, zero if it did not.
func RetryAfter(err error) time.Duration {
	var e *APIError
	if errors.As(err, &e) {
		return e.RetryAfter
	}
	return 0
}

// ErrUnconfigured is returned when the credentials are not set.
var ErrUnconfigured = errors.New("Microsoft Teams is not configured")

// Client talks to ACS as one bot.
type Client struct {
	// ID is the Microsoft app ID of the bot's Entra app registration.
	ID string
	// Secret is that registration's client secret.
	Secret string
	// Tenant is the app registration's tenant, or MultiTenant.
	Tenant string
	// HTTP is the client to use; http.DefaultClient if nil.
	HTTP *http.Client
	// LoginBase and ConnectBase are the service endpoints, overridden by
	// tests. ConnectBase takes the geo.
	LoginBase   string
	ConnectBase string

	// mu is held across the token request as well as around the cached
	// value, which makes it single-flight: a hundred goroutines finding the
	// token expired at once still cause one request.
	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

// New is a Client for an app registration.
func New(appID, secret, tenant string) *Client {
	return &Client{ID: appID, Secret: secret, Tenant: tenant}
}

func (c *Client) AppID() string { return c.ID }

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) loginBase() string {
	if c.LoginBase != "" {
		return c.LoginBase
	}
	return DefaultLoginBase
}

func (c *Client) connectBase() string {
	if c.ConnectBase != "" {
		return c.ConnectBase
	}
	return DefaultConnectBase
}

func (c *Client) tenant() string {
	if c.Tenant != "" {
		return c.Tenant
	}
	return MultiTenant
}

// tokenSkew is how long before expiry to fetch a new token, so a call never
// starts with one that dies in flight.
const tokenSkew = 5 * time.Minute

// Token returns a current access token, fetching one when the cached token
// is gone or nearly expired.
func (c *Client) Token(ctx context.Context) (string, error) {
	if c.ID == "" || c.Secret == "" {
		return "", ErrUnconfigured
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenExp) {
		return c.token, nil
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.ID},
		"client_secret": {c.Secret},
		"scope":         {TokenScope},
	}
	endpoint := c.loginBase() + "/" + url.PathEscape(c.tenant()) + "/oauth2/v2.0/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http().Do(req)
	if err != nil {
		return "", fmt.Errorf("getting a Teams access token: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("getting a Teams access token: %w", err)
	}
	var out struct {
		AccessToken      string `json:"access_token"`
		ExpiresIn        int    `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	// A failed token request answers with JSON too, so decode either way.
	json.Unmarshal(body, &out)
	if resp.StatusCode/100 != 2 || out.AccessToken == "" {
		e := &APIError{Call: "token", Status: resp.StatusCode, Code: out.Error, Message: out.ErrorDescription}
		if e.Code == "" && e.Message == "" {
			e.Message = strings.TrimSpace(string(body))
		}
		// Microsoft's error descriptions run to several lines with a
		// correlation ID; the first line is the part worth showing.
		e.Message, _, _ = strings.Cut(e.Message, "\r\n")
		return "", e
	}
	c.token = out.AccessToken
	ttl := time.Duration(out.ExpiresIn) * time.Second
	if ttl <= tokenSkew {
		// A short-lived token is unusual; keep it, just do not cache it for
		// longer than it lives.
		c.tokenExp = time.Now().Add(ttl / 2)
	} else {
		c.tokenExp = time.Now().Add(ttl - tokenSkew)
	}
	return c.token, nil
}

// forgetToken drops the cached token, so the next call fetches a new one.
// Used when ACS rejects the token we thought was good.
func (c *Client) forgetToken() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.token, c.tokenExp = "", time.Time{}
}

// Connect opens a socket mode session in geo.
func (c *Client) Connect(ctx context.Context, geo, replaces string) (Session, error) {
	tok, err := c.Token(ctx)
	if err != nil {
		return Session{}, err
	}
	endpoint := fmt.Sprintf(c.connectBase(), geo) + "/v3/websockets/connect"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return Session{}, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Length", "0")
	if replaces != "" {
		req.Header.Set("x-ms-socket-replaces-connection-id", replaces)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return Session{}, fmt.Errorf("connect (%s): %w", geo, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Session{}, fmt.Errorf("connect (%s): %w", geo, err)
	}
	if resp.StatusCode/100 != 2 {
		if resp.StatusCode == http.StatusUnauthorized {
			c.forgetToken()
		}
		return Session{}, httpError("connect ("+geo+")", resp, body)
	}
	var s Session
	if err := json.Unmarshal(body, &s); err != nil {
		return Session{}, fmt.Errorf("connect (%s): the response is not JSON: %w", geo, err)
	}
	if s.URL == "" || s.AccessToken == "" {
		return Session{}, fmt.Errorf("connect (%s): the response has no socket URL", geo)
	}
	return s, nil
}

// SendActivity posts an activity to a conversation.
func (c *Client) SendActivity(ctx context.Context, serviceURL, conversation string, a Activity) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	u, err := join(serviceURL, "v3", "conversations", conversation, "activities")
	if err != nil {
		return "", err
	}
	if err := c.call(ctx, "send", http.MethodPost, u, a, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// UpdateActivity replaces an activity already posted.
func (c *Client) UpdateActivity(ctx context.Context, serviceURL, conversation, activityID string, a Activity) error {
	u, err := join(serviceURL, "v3", "conversations", conversation, "activities", activityID)
	if err != nil {
		return err
	}
	a.ID = activityID
	return c.call(ctx, "update", http.MethodPut, u, a, nil)
}

// CreateConversation opens the 1:1 chat between the bot and one user.
func (c *Client) CreateConversation(ctx context.Context, serviceURL, tenantID, userID string) (string, error) {
	u, err := join(serviceURL, "v3", "conversations")
	if err != nil {
		return "", err
	}
	body := map[string]any{
		"bot":         map[string]any{"id": "28:" + c.ID},
		"members":     []map[string]any{{"id": userID}},
		"channelData": map[string]any{"tenant": map[string]any{"id": tenantID}},
		"isGroup":     false,
		"tenantId":    tenantID,
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := c.call(ctx, "create conversation", http.MethodPost, u, body, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

// Member returns one member of a conversation.
func (c *Client) Member(ctx context.Context, serviceURL, conversation, userID string) (Member, error) {
	u, err := join(serviceURL, "v3", "conversations", conversation, "members", userID)
	if err != nil {
		return Member{}, err
	}
	var m Member
	if err := c.call(ctx, "member", http.MethodGet, u, nil, &m); err != nil {
		return Member{}, err
	}
	return m, nil
}

// Members returns a conversation's roster. ACS pages channel and team
// rosters, and the unpaged call is documented as unreliable on large teams,
// so this always pages.
func (c *Client) Members(ctx context.Context, serviceURL, conversation string) ([]Member, error) {
	base, err := join(serviceURL, "v3", "conversations", conversation, "pagedmembers")
	if err != nil {
		return nil, err
	}
	var all []Member
	token := ""
	for {
		q := url.Values{"pageSize": {strconv.Itoa(memberPageSize)}}
		if token != "" {
			q.Set("continuationToken", token)
		}
		var page struct {
			Members           []Member `json:"members"`
			ContinuationToken string   `json:"continuationToken"`
		}
		if err := c.call(ctx, "members", http.MethodGet, base+"?"+q.Encode(), nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Members...)
		if len(all) >= MaxMembers {
			return all[:MaxMembers], nil
		}
		if page.ContinuationToken == "" || len(page.Members) == 0 {
			return all, nil
		}
		token = page.ContinuationToken
	}
}

// Channels lists a team's channels.
func (c *Client) Channels(ctx context.Context, serviceURL, teamID string) ([]Channel, error) {
	u, err := join(serviceURL, "v3", "teams", teamID, "conversations")
	if err != nil {
		return nil, err
	}
	var out struct {
		Conversations []Channel `json:"conversations"`
	}
	if err := c.call(ctx, "channels", http.MethodGet, u, nil, &out); err != nil {
		return nil, err
	}
	return out.Conversations, nil
}

// call makes one ACS request, retrying once when the service asks us to
// slow down. Everything else is reported to the caller.
func (c *Client) call(ctx context.Context, name, method, endpoint string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	for attempt := range 2 {
		tok, err := c.Token(ctx)
		if err != nil {
			return err
		}
		var rdr io.Reader
		if payload != nil {
			rdr = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, endpoint, rdr)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http().Do(req)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if readErr != nil {
			return fmt.Errorf("%s: reading the response: %w", name, readErr)
		}
		if resp.StatusCode/100 == 2 {
			if out == nil || len(bytes.TrimSpace(respBody)) == 0 {
				return nil
			}
			if err := json.Unmarshal(respBody, out); err != nil {
				return fmt.Errorf("%s: the response is not JSON: %w", name, err)
			}
			return nil
		}
		if resp.StatusCode == http.StatusUnauthorized {
			c.forgetToken() // the next attempt, if any, gets a fresh one
		}
		apiErr := httpError(name, resp, respBody)
		if resp.StatusCode == http.StatusTooManyRequests && attempt == 0 {
			wait := apiErr.RetryAfter
			if wait <= 0 || wait > 30*time.Second {
				wait = time.Second
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		return apiErr
	}
	return fmt.Errorf("%s: gave up after a retry", name)
}

// httpError turns a failed response into an APIError. ACS reports failures
// as {"error":{"code":…,"message":…}}, but not always, so the raw body is
// the fallback.
func httpError(name string, resp *http.Response, body []byte) *APIError {
	e := &APIError{Call: name, Status: resp.StatusCode}
	if s := resp.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			e.RetryAfter = time.Duration(n) * time.Second
		}
	}
	var out struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &out) == nil {
		e.Code, e.Message = out.Error.Code, out.Error.Message
		if e.Message == "" {
			e.Message = out.Message
		}
	}
	if e.Code == "" && e.Message == "" {
		e.Message = strings.TrimSpace(string(body))
		if len(e.Message) > 300 {
			e.Message = e.Message[:300] + "…"
		}
	}
	return e
}

// join builds an ACS URL from a service URL and path segments. The service
// URL comes off an activity and may or may not end in a slash, and a
// conversation ID is full of characters that need escaping.
func join(serviceURL string, parts ...string) (string, error) {
	if serviceURL == "" {
		return "", errors.New("no service URL: jitd has not seen this conversation yet")
	}
	u, err := url.Parse(serviceURL)
	if err != nil {
		return "", fmt.Errorf("bad service URL %q: %w", serviceURL, err)
	}
	if u.Scheme != "https" {
		return "", fmt.Errorf("refusing a service URL that is not HTTPS: %q", serviceURL)
	}
	// Escape each segment, then hand the escaped strings to JoinPath, which
	// is what it expects. Assigning to u.Path instead would escape them a
	// second time and turn a space in a conversation ID into %2520.
	esc := make([]string, 0, len(parts))
	for _, part := range parts {
		// JoinPath resolves dot segments, so a conversation ID of ".." would
		// otherwise climb out of the service URL's path.
		if part == "" || part == "." || part == ".." || strings.Contains(part, "/") {
			return "", fmt.Errorf("refusing %q as a path segment", part)
		}
		esc = append(esc, url.PathEscape(part))
	}
	u = u.JoinPath(esc...)
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

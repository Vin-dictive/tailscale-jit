package teamsbot

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/atoonk/tailscale-jit/jitd/internal/jit"
	"github.com/atoonk/tailscale-jit/jitd/internal/teams"
)

// How long a Teams user's identity is remembered. Short, so a removed
// account or a changed address stops working soon; long enough that a
// click costs no Teams call most of the time.
const (
	identityTTL = 10 * time.Minute
	failureTTL  = time.Minute
)

// identities maps Teams users to Tailscale logins and back, with a cache.
type identities struct {
	mu      sync.Mutex
	byUser  map[string]cachedIdentity // Teams user ID ("29:…") -> identity
	byLogin map[string]cachedMember   // lowercased login -> Teams member
	// rosterAt is when byLogin was last built. Unlike Slack, there is no
	// "look a user up by email" call, so the whole roster of every
	// installed team is read at once and kept.
	rosterAt time.Time
}

type cachedIdentity struct {
	id  jit.Identity
	err error
	at  time.Time
}

type cachedMember struct {
	member teams.Member
	at     time.Time
}

func (c *identities) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.byUser, c.byLogin = nil, nil
	c.rosterAt = time.Time{}
}

// IdentityError says why a Teams user cannot act in jitd.
type IdentityError struct{ Msg string }

func (e *IdentityError) Error() string { return e.Msg }

// resolve matches a Teams user to a Tailscale login by their work email.
// Only members of the configured tenant (no guests from another
// organisation) with an address in an allowed domain are matched, and only
// if that login is on the tailnet: it owns at least one device there.
// Without that, Teams would let people act who could never use jitd in the
// browser (someone offboarded from Tailscale but still in Teams would
// count as autogroup:member).
func (b *Bot) resolve(ctx context.Context, api teams.API, serviceURL, conversation, userID string) (jit.Identity, error) {
	now := b.now()
	c := &b.ids
	c.mu.Lock()
	if e, ok := c.byUser[userID]; ok {
		ttl := identityTTL
		if e.err != nil {
			ttl = failureTTL
		}
		if now.Sub(e.at) < ttl {
			c.mu.Unlock()
			return e.id, e.err
		}
	}
	c.mu.Unlock()

	id, err := b.lookupIdentity(ctx, api, serviceURL, conversation, userID)
	// Remember answers, and definite refusals (IdentityError); don't
	// remember trouble reaching Teams or Tailscale.
	var definite *IdentityError
	if err == nil || errors.As(err, &definite) {
		c.mu.Lock()
		if c.byUser == nil {
			c.byUser = map[string]cachedIdentity{}
		}
		c.byUser[userID] = cachedIdentity{id: id, err: err, at: now}
		c.mu.Unlock()
	}
	return id, err
}

func (b *Bot) lookupIdentity(ctx context.Context, api teams.API, serviceURL, conversation, userID string) (jit.Identity, error) {
	m, err := api.Member(ctx, serviceURL, conversation, userID)
	if err != nil {
		return jit.Identity{}, fmt.Errorf("looking up your Teams account: %w", err)
	}
	if problem := b.memberProblem(m); problem != "" {
		return jit.Identity{}, &IdentityError{Msg: problem}
	}
	login := memberLogin(m)
	devices, err := b.Svc.UserDevices(ctx, login)
	if err != nil {
		return jit.Identity{}, fmt.Errorf("checking that %s is on the tailnet: %w", login, err)
	}
	if len(devices) == 0 {
		return jit.Identity{}, &IdentityError{Msg: fmt.Sprintf("%s has no device on the tailnet; Teams users act in JIT access as their Tailscale login, so log in to Tailscale as %s first", login, login)}
	}
	set := b.settingsCopy()
	return jit.Identity{
		Login:       login,
		DisplayName: m.Name,
		Via:         jit.ViaTeams,
		TeamsUser:   cmp.Or(m.TenantID, set.TenantID, "?") + "/" + cmp.Or(m.AADObjectID, m.ID),
	}, nil
}

// memberLogin is the address jitd takes as the person's Tailscale login.
// Teams reports both an email and a principal name, and for a normal
// member of the organisation they agree; the email is the one people
// recognise, so it wins when both are set.
func memberLogin(m teams.Member) string {
	return strings.ToLower(strings.TrimSpace(cmp.Or(m.Email, m.UserPrincipalName)))
}

// memberProblem says why m cannot act in jitd, or "".
func (b *Bot) memberProblem(m teams.Member) string {
	set := b.settingsCopy()
	email := memberLogin(m)
	switch {
	case m.Guest():
		// A guest's address belongs to another organisation, which is free
		// to hand it to somebody else. It is not evidence of who they are
		// here.
		return "guest accounts can't act on access requests; only members of this organisation can"
	case email == "":
		return "your Teams account has no work address, so it can't be matched to a Tailscale login"
	case set.TenantID != "" && m.TenantID != "" && !strings.EqualFold(m.TenantID, set.TenantID):
		return "your Teams account belongs to another organisation; only members of the connected tenant can act on access requests"
	}
	if !slices.Contains(set.AllowedDomains, emailDomain(email)) {
		return fmt.Sprintf("your Teams address (%s) is not in a domain allowed for JIT access (%s)", email, strings.Join(set.AllowedDomains, ", "))
	}
	return ""
}

// teamsMember finds the Teams account of a Tailscale login.
//
// This is where Teams costs more than Slack. Slack has
// users.lookupByEmail; Teams has nothing of the kind, so the only way to
// find somebody is to read the roster of a conversation the bot shares
// with them. jitd reads the roster of every team the app is installed in
// and keeps the result. The practical consequence, which the admin page
// and the README both say out loud: an approver who is in no team with
// the app cannot be sent a direct message.
func (b *Bot) teamsMember(ctx context.Context, api teams.API, login string) (teams.Member, bool) {
	key := strings.ToLower(strings.TrimSpace(login))
	now := b.now()
	c := &b.ids
	c.mu.Lock()
	fresh := c.byLogin != nil && now.Sub(c.rosterAt) < identityTTL
	if fresh {
		e, ok := c.byLogin[key]
		c.mu.Unlock()
		return e.member, ok
	}
	c.mu.Unlock()

	byLogin := map[string]cachedMember{}
	for _, t := range b.settingsCopy().Installed {
		ms, err := api.Members(ctx, t.ServiceURL, t.ID)
		if err != nil {
			b.log().Warn("teams: reading a team roster", "team", cmp.Or(t.Name, t.ID), "err", err)
			continue
		}
		for _, m := range ms {
			if l := memberLogin(m); l != "" {
				byLogin[l] = cachedMember{member: m, at: now}
			}
		}
	}
	c.mu.Lock()
	// Only keep an empty roster out of the cache: if every team failed to
	// read, retry on the next call rather than claiming nobody exists.
	if len(byLogin) > 0 {
		c.byLogin, c.rosterAt = byLogin, now
	}
	c.mu.Unlock()
	e, ok := byLogin[key]
	return e.member, ok
}

package teamsbot

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/atoonk/tailscale-jit/jitd/internal/jit"
	"github.com/atoonk/tailscale-jit/jitd/internal/okta"
)

// An ended request whose Okta memberships jitd cannot remove: after a while
// every admin jitd can reach in Teams gets one direct message, the
// request's own cards carry a warning, and both change when the removal
// goes through.
func TestOktaStuckRemovalTellsAdmins(t *testing.T) {
	e := newEnv(t, nil)
	const group = "00gADMIN000000000001"
	of := okta.NewFake()
	of.AddUser("00uMAYA", "maya@example.com")
	of.AddGroup(group, "AWS-Prod-Admin", "")
	var mu sync.Mutex
	now := time.Now().UTC()
	clock := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
	}
	e.svc.Okta = of
	e.svc.Now = func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	// leo is an admin but is in no team jitd can see, so Teams cannot reach
	// them; sam is in the roster.
	e.svc.Admins = []jit.Principal{"leo@example.com", "sam@example.com"}

	w, err := e.svc.CreateWorkflow(jit.Workflow{
		Name: "AWS admin", OktaGroups: []jit.OktaGroup{{ID: group}},
		Requesters: []jit.Principal{"autogroup:member"}, Approvers: []jit.Principal{"priya@example.com"},
		MaxDuration: jit.Duration(time.Hour), Teams: &jit.WorkflowTeams{TeamID: teamID, DMApprovers: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r, err := e.svc.Submit(ctx, maya, jit.NewRequest{WorkflowID: w.ID, Reason: "rotate the deploy role"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the direct message to the approver", func() bool { return len(e.svc.Request(r.ID).Teams) == 1 })
	priya := jit.Identity{Login: "priya@example.com"}
	if _, err := e.svc.Approve(ctx, priya, r.ID, ""); err != nil {
		t.Fatal(err)
	}
	of.Fail["remove:"+group] = errors.New("403 forbidden")
	if _, err := e.svc.Revoke(ctx, priya, r.ID, "done"); err != nil {
		t.Fatal(err)
	}
	e.svc.ExpireDue()
	if n := len(e.fake.PostsTo(e.fake.DMTo(samID))); n != 0 {
		t.Fatalf("an admin was told at the first failure (%d cards)", n)
	}

	clock(6 * time.Minute)
	e.svc.ExpireDue()
	waitFor(t, "the direct message to the admin", func() bool { return len(e.fake.PostsTo(e.fake.DMTo(samID))) == 1 })
	card := lastCard(t, e, samID)
	for _, want := range []string{"Okta removal is failing", "maya@example.com", "AWS-Prod-Admin", "403 forbidden", "http://jit/requests/" + r.ID} {
		if !strings.Contains(card, want) {
			t.Errorf("the admin's card lacks %q: %s", want, card)
		}
	}
	waitFor(t, "the warning on the approver's card", func() bool {
		return strings.Contains(lastCard(t, e, priyaID), `still in the Okta group AWS-Prod-Admin`)
	})

	// Told once: more failing ticks and a catch-up post nothing new.
	clock(time.Minute)
	e.svc.ExpireDue()
	e.bot.reconcile(ctx)
	time.Sleep(100 * time.Millisecond)
	if n := len(e.fake.PostsTo(e.fake.DMTo(samID))); n != 1 {
		t.Errorf("the admin got %d cards", n)
	}
	var kinds []string
	for _, m := range e.svc.Request(r.ID).Teams {
		kinds = append(kinds, m.Kind+":"+m.User)
	}
	if strings.Join(kinds, " ") != "dm:"+priyaID+" "+kindGroupAlert+":"+samID {
		t.Errorf("recorded cards = %v", kinds)
	}

	delete(of.Fail, "remove:"+group)
	clock(time.Minute)
	e.svc.ExpireDue()
	waitFor(t, "the all-clear to the admin", func() bool {
		return strings.Contains(lastCard(t, e, samID), "Okta removal went through")
	})
	waitFor(t, "the warning to go from the approver's card", func() bool {
		return !strings.Contains(lastCard(t, e, priyaID), "still in the Okta group")
	})
	if n := len(e.fake.PostsTo(e.fake.DMTo(samID))); n != 1 {
		t.Errorf("the all-clear was a new card (%d)", n)
	}
}

// lastCard is the newest card in the direct message to a Teams user, as
// JSON. Updates replace a card in place, so this is its current state.
func lastCard(t *testing.T, e *env, user string) string {
	t.Helper()
	posts := e.fake.PostsTo(e.fake.DMTo(user))
	if len(posts) == 0 {
		return ""
	}
	return posts[len(posts)-1].Card()
}

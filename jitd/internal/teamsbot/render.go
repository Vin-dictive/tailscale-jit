package teamsbot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/atoonk/tailscale-jit/jitd/internal/jit"
	"github.com/atoonk/tailscale-jit/jitd/internal/teams"
)

// Verbs on the request card's buttons. An Action.Execute carries one, and
// it is what the handler switches on.
const (
	verbApprove = "jit_approve"
	verbDecline = "jit_decline"
	// verbDeclineSubmit comes from the reason form that replaces the card
	// for whoever pressed Decline.
	verbDeclineSubmit = "jit_decline_submit"
	// verbRequest and verbRequestSubmit are the /jit request form.
	verbRequest       = "jit_request"
	verbRequestSubmit = "jit_request_submit"
	// Input IDs on those forms.
	inputReason   = "reason"
	inputFlow     = "flow"
	inputDevice   = "device"
	inputDuration = "duration"
	// dataRequestID carries which request a button belongs to.
	dataRequestID = "request_id"
)

// renderInput is what a request card is made of.
type renderInput struct {
	R       *jit.AccessRequest
	W       *jit.Workflow // nil if the flow is gone
	URL     string        // the request's page
	Mention string        // the requester's display name in Teams, if known
	Counted int           // valid votes so far
	Buttons bool          // Approve and Decline, for a pending request
}

// render builds the Teams card for a request. It is the same in a channel
// and in direct messages, and it is rebuilt from the request every time,
// so it always shows where the request is now.
func render(in renderInput) teams.Card {
	r := in.R
	who := esc(r.Requester.Actor())
	if r.Requester.DisplayName != "" && !r.Requester.Tagged() {
		who = esc(r.Requester.DisplayName) + " (" + esc(r.Requester.Login) + ")"
	}
	device := r.Requester.DeviceName
	if device == "" {
		device = r.TargetDeviceID
	}

	body := []teams.Element{
		teams.Heading(statusMark(r) + " Access request · " + esc(r.WorkflowName)),
		teams.Text(fmt.Sprintf("%s asks for **%s** of access from `%s`.", who, r.Duration.Human(), esc(device))),
		teams.Quote(quote(r.Reason)),
	}

	facts := []teams.Fact{}
	if len(r.Resources) > 0 {
		var names []string
		for _, res := range r.Resources {
			names = append(names, res.Name)
		}
		facts = append(facts, teams.Fact{Title: "Gives access to", Value: esc(strings.Join(names, ", "))})
	}
	facts = append(facts, teams.Fact{Title: "Status", Value: statusLine(r, in.Counted)})
	body = append(body, teams.Facts(facts...))

	footer := fmt.Sprintf("%s · requested %s", r.ID, cardTime(r.CreatedAt))
	if r.Status == jit.StatusPending && in.W != nil && in.W.Teams != nil && in.W.Teams.BrowserOnly {
		footer += "\n\nThis flow is approved in the browser only."
	}
	body = append(body, teams.Subtle(footer))

	// "Open" is the one thing that cannot happen in the card: the request
	// page is on the tailnet.
	open := teams.OpenURL("Open", in.URL)
	if in.Buttons && r.Status == jit.StatusPending {
		body = append(body, teams.Actions(
			teams.Execute(verbApprove, "Approve", "positive", map[string]any{dataRequestID: r.ID}),
			teams.Execute(verbDecline, "Decline", "destructive", map[string]any{dataRequestID: r.ID}),
			open))
	} else {
		body = append(body, teams.Actions(open))
	}
	return teams.NewCard(body...)
}

// declineForm replaces the card, for whoever pressed Decline only, with a
// box for the reason. Slack needs a modal for this; in Teams the invoke
// response can hand the clicker a different card, so there is no dialog
// and no second round trip.
func declineForm(r *jit.AccessRequest, note string) teams.Card {
	body := []teams.Element{
		teams.Heading("Decline · " + esc(r.WorkflowName)),
		teams.Text(fmt.Sprintf("%s asked for **%s** of access.", esc(r.Requester.Actor()), r.Duration.Human())),
	}
	if note != "" {
		body = append(body, teams.Text("⚠️ "+esc(note)))
	}
	body = append(body,
		teams.Text("Why are you declining? The requester sees this."),
		teams.TextInput(inputReason, "Reason", true),
		teams.Actions(teams.Execute(verbDeclineSubmit, "Decline", "destructive", map[string]any{dataRequestID: r.ID})),
	)
	return teams.NewCard(body...)
}

// noticeCard is a one-line answer in place of a card.
func noticeCard(title, msg string) teams.Card {
	return teams.NewCard(teams.Heading(esc(title)), teams.Text(esc(msg)))
}

// buttonsFor says whether a request's card gets Approve and Decline. An
// automatic flow asks nobody, so its card is a record, not a question.
func buttonsFor(w *jit.Workflow) bool {
	return w != nil && !w.Disabled && !w.AutoApprove && (w.Teams == nil || !w.Teams.BrowserOnly)
}

// statusMark is the emoji in front of the title. Teams renders these in
// cards, and they survive a plain-text fallback.
func statusMark(r *jit.AccessRequest) string {
	switch r.Status {
	case jit.StatusPending:
		return "⏳"
	case jit.StatusApproved:
		return "✅"
	case jit.StatusScheduled:
		return "📅"
	case jit.StatusDeclined:
		return "❌"
	case jit.StatusRevoked:
		return "⛔"
	default:
		return "⚪"
	}
}

// statusLine says where the request is, who decided and when.
func statusLine(r *jit.AccessRequest, counted int) string {
	by := func(types ...string) string {
		for i := len(r.Timeline) - 1; i >= 0; i-- {
			e := r.Timeline[i]
			for _, t := range types {
				if e.Type == t {
					s := "**" + esc(e.Actor) + "**"
					switch e.Via {
					case jit.ViaTeams:
						s += " via Teams"
					case jit.ViaSlack:
						s += " via Slack"
					}
					return s
				}
			}
		}
		return ""
	}
	note := func(t string) string {
		for i := len(r.Timeline) - 1; i >= 0; i-- {
			if r.Timeline[i].Type == t && r.Timeline[i].Note != "" {
				return r.Timeline[i].Note
			}
		}
		return ""
	}
	switch r.Status {
	case jit.StatusPending:
		s := "Waiting for approval"
		if r.StartAt != nil {
			s += " for " + cardTime(*r.StartAt)
		}
		if r.ApprovalsRequired > 1 {
			s += fmt.Sprintf(" (%d of %d)", counted, r.ApprovalsRequired)
		}
		if r.LastError != "" {
			s += "\n\n⚠️ the last approval failed: " + esc(r.LastError)
		}
		return s
	case jit.StatusScheduled:
		s := "Approved by " + by("approved") + "; starts " + cardTime(*r.StartAt) + ", until " + cardTime(*r.ExpiresAt)
		if r.LastError != "" {
			s += "\n\n⚠️ could not start yet: " + esc(r.LastError) + " (retrying)"
		}
		return s
	case jit.StatusApproved:
		s := "Approved by " + by("approved")
		if r.ExpiresAt != nil {
			s += ", until " + cardTime(*r.ExpiresAt)
		}
		if r.Enforcement.ConfirmedAt != nil {
			s += "\n\nConfirmed on the device"
		} else {
			s += "\n\nNot yet confirmed on the device"
		}
		return s
	case jit.StatusDeclined:
		s := "Declined by " + by("declined")
		if n := note("declined"); n != "" {
			s += ": " + esc(n)
		}
		return s
	case jit.StatusCancelled:
		return "Withdrawn by the requester"
	case jit.StatusRevoked:
		s := "Ended early by " + by("revoked")
		if r.Enforcement.RemovedAt != nil {
			s += "\n\nAccess confirmed gone from the device"
		}
		return s + groupsStuckLine(r) + groupsKeptLine(r)
	case jit.StatusExpired:
		if !r.Granted() {
			return "Expired without a decision"
		}
		s := "Access ended"
		if r.ExpiresAt != nil {
			s += " " + cardTime(*r.ExpiresAt)
		}
		if r.Enforcement.RemovedAt != nil {
			s += "\n\nAccess confirmed gone from the device"
		}
		return s + groupsStuckLine(r) + groupsKeptLine(r)
	}
	return string(r.Status)
}

// groupsStuckLine warns, on an ended request, that its Okta or Google
// memberships are still in place after jitd kept failing to remove them.
func groupsStuckLine(r *jit.AccessRequest) string {
	s := ""
	for _, set := range r.GroupSets() {
		if set.Stuck() {
			s += "\n\n⚠️ still in the " + set.Label + " group " + esc(strings.Join(set.Added(), ", ")) + "; jitd keeps trying to remove it"
		}
	}
	return s
}

// groupsKeptLine says, on an ended request, which groups the requester
// stayed in when it ended because another running request of theirs held
// them. It is a record of that moment, so it is worded in the past.
func groupsKeptLine(r *jit.AccessRequest) string {
	s := ""
	for _, set := range r.GroupSets() {
		if kept := set.Kept(); len(kept) > 0 {
			s += "\n\nWhen this ended, another running request still held the " + set.Label + " group " + esc(strings.Join(kept, ", ")) + ", so the membership stayed; that request removes it when it ends"
		}
	}
	return s
}

// renderGroupAlert builds the direct message to an admin about a request
// whose Okta or Google memberships jitd cannot remove. Like the request
// card it is rebuilt from the request, so it turns into the all-clear once
// the removal goes through. It speaks of every kind the admins were
// alerted about, each under its own name.
func renderGroupAlert(r *jit.AccessRequest, url string) teams.Card {
	var texts []string
	sets := r.GroupSets()
	alerted := func(set jit.GroupSet) bool { return set.Grant != nil && set.Grant.AlertedAt != nil }
	some := slices.ContainsFunc(sets, alerted)
	for _, set := range sets {
		if set.Grant == nil || some && !alerted(set) {
			continue
		}
		texts = append(texts, groupAlert(r, set, esc(r.WorkflowName)))
	}
	if len(texts) == 0 {
		// The record of the alert is gone; the request still says how it ended.
		texts = []string{"✅ **Group removal is no longer failing · " + esc(r.WorkflowName) + "**"}
	}
	return teams.NewCard(
		teams.Text(strings.Join(texts, "\n\n")),
		teams.Subtle(r.ID),
		teams.Actions(teams.OpenURL("Open", url)),
	)
}

// groupAlert words the alert, or the all-clear, for one kind of group.
func groupAlert(r *jit.AccessRequest, set jit.GroupSet, name string) string {
	l, g := set.Label, set.Grant
	who, added := "`"+esc(g.Login)+"`", strings.Join(set.Added(), ", ")
	if set.Stuck() {
		text := fmt.Sprintf("⚠️ **%s removal is failing · %s**\n\nThe access of %s has ended, but the %s user is still in **%s**. jitd keeps trying; take the user out in %s if this cannot wait.",
			l, name, who, l, esc(added), l)
		if e := g.Error; e != "" {
			if len(e) > 600 {
				e = e[:600] + "…"
			}
			text += "\n\n" + quote(e)
		}
		return text
	}
	// Say only what is true: a group another running request holds was
	// not removed, and the user is still in it.
	removed, kept := strings.Join(set.Removed(), ", "), strings.Join(set.Kept(), ", ")
	later := "another running request of theirs holds it and removes it when it ends"
	switch {
	case kept == "":
		return fmt.Sprintf("✅ **%s removal went through · %s**\n\nThe %s user %s is out of **%s**.", l, name, l, who, esc(added))
	case removed == "":
		return fmt.Sprintf("✅ **%s removal is no longer failing · %s**\n\nThe %s user %s is still in **%s**: %s.",
			l, name, l, who, esc(kept), later)
	default:
		return fmt.Sprintf("✅ **%s removal went through · %s**\n\nThe %s user %s is out of **%s**, and still in **%s**: %s.",
			l, name, l, who, esc(removed), esc(kept), later)
	}
}

// cardTime shows t in the reader's own time zone. Teams renders this
// {{DATE}} form in cards; anything that does not understand it shows the
// text as written, which is still a readable UTC timestamp.
func cardTime(t time.Time) string {
	return fmt.Sprintf("{{DATE(%s, SHORT)}} {{TIME(%s)}}",
		t.UTC().Format(time.RFC3339), t.UTC().Format(time.RFC3339))
}

// esc makes text safe to drop into a card. Teams renders a subset of
// Markdown in a TextBlock, so the characters that start a link, a bold run
// or a list have to be held back; otherwise a device called "*prod*" would
// come out in italics, and a reason containing "[click](http://evil)"
// would come out as a link.
var escaper = strings.NewReplacer(
	`\`, `\\`,
	"*", `\*`,
	"_", `\_`,
	"[", `\[`,
	"]", `\]`,
	"(", `\(`,
	")", `\)`,
	"`", "\\`",
	"~", `\~`,
	"#", `\#`,
	"|", `\|`,
	"<", "&lt;",
	">", "&gt;",
)

func esc(s string) string { return escaper.Replace(s) }

// quote trims a reason to what a card will show.
func quote(s string) string {
	out := esc(s)
	if len(out) > 2000 {
		out = out[:2000] + "…"
	}
	return out
}

// digest is a short hash of a card, to skip updates that change nothing.
func digest(c teams.Card) string {
	b, _ := json.Marshal(c)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:8])
}

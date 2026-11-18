package teamsbot

import (
	"context"
	"fmt"
	"strings"

	"github.com/atoonk/tailscale-jit/jitd/internal/jit"
	"github.com/atoonk/tailscale-jit/jitd/internal/teams"
	"github.com/atoonk/tailscale-jit/jitd/internal/tsapi"
)

// durationChoices are the durations the form offers. A flow's maximum
// still applies; Submit rejects anything longer.
var durationChoices = []string{"15m", "30m", "1h", "2h", "4h", "8h", "1d"}

// requestForm answers the Request button with the form, as a card that
// replaces the one that was clicked.
//
// Slack needs a modal here, opened inside a three-second trigger window
// and filled in afterwards. Teams does not: the invoke response carries
// the whole form, it is shown to the clicker alone, and there is no
// deadline shorter than the envelope's.
func (b *Bot) requestForm(ctx context.Context, api teams.API, a *teams.Activity, note string) *teams.InvokeResponse {
	return cardResponse(b.requestFormCard(ctx, api, a, note))
}

// requestFormCard is the form for whoever sent a, or a card saying why
// there is none.
func (b *Bot) requestFormCard(ctx context.Context, api teams.API, a *teams.Activity, note string) teams.Card {
	base := strings.TrimSuffix(b.BaseURL, "/")
	id, err := b.resolve(ctx, api, a.ServiceURL, a.Conversation.ID, a.From.ID)
	if err != nil {
		return noticeCard("Request access", fmt.Sprintf("JIT access can't tell who you are: %s.", err))
	}
	flows := b.Svc.Requestable(ctx, id)
	if len(flows) == 0 {
		return noticeCard("Request access", fmt.Sprintf("There is no approval flow %s can request access through.", id.Login))
	}
	devices, err := b.Svc.UserDevices(ctx, id.Login)
	if err != nil {
		b.log().Warn("teams: listing devices", "login", id.Login, "err", err)
		return teams.NewCard(
			teams.Heading("Request access"),
			teams.Text(fmt.Sprintf("Couldn't get your devices from Tailscale (%s). Try again, or request access on the device itself.", esc(err.Error()))),
			teams.Actions(teams.OpenURL("Request in the browser", base+"/requests/new")),
		)
	}
	if len(devices) == 0 {
		return noticeCard("Request access", fmt.Sprintf("Tailscale has no devices for %s. Access is given to a device, so log in to Tailscale as %s on the device that needs it, then try again.", id.Login, id.Login))
	}

	var flowOpts []teams.Choice
	for _, f := range flows {
		flowOpts = append(flowOpts, teams.Choice{
			Title: fmt.Sprintf("%s (up to %s)", f.WorkflowName, f.MaxDuration.Human()),
			Value: f.WorkflowID,
		})
	}
	var devOpts []teams.Choice
	for _, d := range devices {
		devOpts = append(devOpts, teams.Choice{Title: deviceLabel(d), Value: d.NodeID})
	}
	var durOpts []teams.Choice
	for _, d := range durationChoices {
		v, _ := jit.ParseDuration(d)
		durOpts = append(durOpts, teams.Choice{Title: jit.Duration(v).Human(), Value: d})
	}

	body := []teams.Element{teams.Heading("Request access")}
	if note != "" {
		body = append(body, teams.Text("⚠️ "+esc(note)))
	}
	// In a browser jitd knows the device from the connection; from Teams it
	// does not, so the caller picks one of their own, and the submit checks
	// again that it is still theirs.
	body = append(body,
		teams.Subtle(fmt.Sprintf("Requesting as %s. Access is given to the device you pick, once an approver says yes.", esc(id.Login))),
		teams.Text("**Approval flow**"),
		teams.ChoiceSet(inputFlow, "Pick a flow", flowOpts, flowOpts[0].Value),
		teams.Text("**Device**"),
		teams.ChoiceSet(inputDevice, "Pick a device", devOpts, devOpts[0].Value),
		teams.Text("**For how long**"),
		teams.ChoiceSet(inputDuration, "The flow's default", durOpts, ""),
		teams.Text("**Reason**"),
		teams.TextInput(inputReason, "What do you need it for? Approvers see this.", true),
		teams.Actions(teams.Execute(verbRequestSubmit, "Request", "positive", nil)),
	)
	return teams.NewCard(body...)
}

// onRequestSubmit files the request from the form. Everything is checked
// here, including that the device is still the caller's own untagged
// device, which takes a Tailscale call.
func (b *Bot) onRequestSubmit(ctx context.Context, api teams.API, a *teams.Activity, d actionData) *teams.InvokeResponse {
	again := func(note string) *teams.InvokeResponse {
		return b.requestForm(ctx, api, a, note)
	}
	reason := strings.TrimSpace(d.Reason)
	if reason == "" {
		return again("A reason is required.")
	}
	if d.Flow == "" {
		return again("Pick an approval flow.")
	}
	w, err := b.Svc.GetWorkflow(d.Flow)
	if err != nil {
		return again("That approval flow is gone. Pick another.")
	}
	var dur jit.Duration
	if d.Duration != "" {
		v, err := jit.ParseDuration(d.Duration)
		if err != nil {
			return again(err.Error())
		}
		dur = jit.Duration(v)
	}
	if dur > w.MaxDuration {
		return again("This flow allows up to " + w.MaxDuration.Human() + ".")
	}
	if d.Device == "" {
		return again("Pick a device.")
	}

	id, err := b.resolve(ctx, api, a.ServiceURL, a.Conversation.ID, a.From.ID)
	if err != nil {
		return errResponse(403, fmt.Sprintf("JIT access can't tell who you are: %s.", err))
	}
	devices, err := b.Svc.UserDevices(ctx, id.Login)
	if err != nil {
		return errResponse(500, fmt.Sprintf("Couldn't check your devices with Tailscale: %s.", err))
	}
	var dev *tsapi.Device
	for i := range devices {
		if devices[i].NodeID == d.Device {
			dev = &devices[i]
		}
	}
	if dev == nil {
		return again("That device is not one of yours (any more).")
	}
	id.DeviceID, id.DeviceName = dev.NodeID, dev.ShortName()
	r, err := b.Svc.Submit(ctx, id, jit.NewRequest{WorkflowID: w.ID, Duration: dur, Reason: reason})
	if err != nil {
		e := jit.AsError(err)
		if e.RequestID != "" {
			return cardResponse(teams.NewCard(
				teams.Heading("Not filed"),
				teams.Text(esc(e.Message)+"."),
				teams.Actions(teams.OpenURL("Open that request", b.requestLink(e.RequestID))),
			))
		}
		return again(e.Message + ".")
	}
	b.log().Info("teams: request filed", "id", r.ID, "by", id.Login, "device", dev.ShortName())
	if r.Status == jit.StatusApproved { // an automatic flow
		return cardResponse(teams.NewCard(
			teams.Heading("✅ Access granted"),
			teams.Text(fmt.Sprintf("**%s** for `%s`, %s. It was approved automatically.",
				esc(r.WorkflowName), esc(dev.ShortName()), r.Duration.Human())),
			teams.Actions(teams.OpenURL("Open the request", b.requestLink(r.ID))),
		))
	}
	return cardResponse(teams.NewCard(
		teams.Heading("⏳ Request filed"),
		teams.Text(fmt.Sprintf("**%s** for `%s`, %s. It's waiting for approval.",
			esc(r.WorkflowName), esc(dev.ShortName()), r.Duration.Human())),
		teams.Actions(teams.OpenURL("Open the request", b.requestLink(r.ID))),
	))
}

// deviceLabel names a device in the picker: name, OS, and when it was last
// online.
func deviceLabel(d tsapi.Device) string {
	s := d.ShortName()
	if d.OS != "" {
		s += " · " + d.OS
	}
	if d.ConnectedToControl {
		s += " · online"
	} else if !d.LastSeen.IsZero() {
		s += " · seen " + d.LastSeen.Format("Jan 2")
	}
	return s
}

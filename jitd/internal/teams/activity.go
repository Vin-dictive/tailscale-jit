package teams

import "encoding/json"

// An activity is the unit ACS moves in both directions: a message jitd
// posts, a message someone sent the bot, a button press, or a notice that
// the app was installed somewhere. These are the parts of it jitd reads or
// writes; ACS sends a great deal more, and ignoring it is fine.

// Activity is one ACS activity.
type Activity struct {
	// Type is "message", "invoke", "conversationUpdate" or one of the many
	// jitd ignores.
	Type string `json:"type,omitempty"`
	// Name is the invoke's name, such as "adaptiveCard/action".
	Name string `json:"name,omitempty"`
	// ID identifies a posted activity, so it can be updated later.
	ID string `json:"id,omitempty"`
	// Text is the message text. In a channel it still carries the @mention
	// of the bot; see StripMention.
	Text string `json:"text,omitempty"`
	// Value is the invoke payload, left raw because its shape depends on
	// Name.
	Value json.RawMessage `json:"value,omitempty"`
	// ServiceURL is the ACS endpoint to answer on. It varies by tenant and
	// may change, so it is taken from the activity and stored with anything
	// that must be updated later, never hardcoded.
	ServiceURL string `json:"serviceUrl,omitempty"`

	From         Account      `json:"from,omitzero"`
	Recipient    Account      `json:"recipient,omitzero"`
	Conversation Conversation `json:"conversation,omitzero"`
	ChannelData  ChannelData  `json:"channelData,omitzero"`
	Entities     []Entity     `json:"entities,omitempty"`
	Attachments  []Attachment `json:"attachments,omitempty"`

	// MembersAdded says who joined, on a conversationUpdate. The bot itself
	// being in the list is how jitd learns it was installed somewhere.
	MembersAdded []Account `json:"membersAdded,omitempty"`
}

// Account is a user or the bot. A user's ID is a "29:…" string that is
// specific to this bot; AADObjectID is the same person across apps.
type Account struct {
	ID          string `json:"id,omitempty"`
	Name        string `json:"name,omitempty"`
	AADObjectID string `json:"aadObjectId,omitempty"`
}

// Conversation is where an activity happened: a 1:1 chat, a group chat, or
// a channel in a team.
type Conversation struct {
	ID string `json:"id,omitempty"`
	// ConversationType is "personal", "groupChat" or "channel".
	ConversationType string `json:"conversationType,omitempty"`
	TenantID         string `json:"tenantId,omitempty"`
	IsGroup          bool   `json:"isGroup,omitempty"`
}

// ChannelData is the Teams-specific part of an activity.
type ChannelData struct {
	Team    *TeamInfo    `json:"team,omitempty"`
	Channel *ChannelInfo `json:"channel,omitempty"`
	Tenant  *TenantInfo  `json:"tenant,omitempty"`
}

type TeamInfo struct {
	ID         string `json:"id,omitempty"`
	Name       string `json:"name,omitempty"`
	AADGroupID string `json:"aadGroupId,omitempty"`
}

type ChannelInfo struct {
	ID string `json:"id,omitempty"`
	// Name is empty for the General channel, which ACS leaves unnamed so
	// clients can localise it.
	Name string `json:"name,omitempty"`
}

type TenantInfo struct {
	ID string `json:"id,omitempty"`
}

// Entity is an annotation on a message. jitd only reads mentions, to take
// the bot's name back out of the text.
type Entity struct {
	Type      string  `json:"type,omitempty"`
	Text      string  `json:"text,omitempty"`
	Mentioned Account `json:"mentioned,omitzero"`
}

// Attachment carries a card on a message.
type Attachment struct {
	ContentType string `json:"contentType"`
	Content     any    `json:"content,omitempty"`
}

// Tenant is the tenant the activity came from, wherever ACS put it.
func (a *Activity) Tenant() string {
	if a.ChannelData.Tenant != nil && a.ChannelData.Tenant.ID != "" {
		return a.ChannelData.Tenant.ID
	}
	return a.Conversation.TenantID
}

// TeamID is the team an activity came from, empty outside a team.
func (a *Activity) TeamID() string {
	if a.ChannelData.Team != nil {
		return a.ChannelData.Team.ID
	}
	return ""
}

// InvokeResponse is the body jitd returns for an invoke activity. The
// status code inside it is what the client acts on: an HTTP 200 carrying a
// statusCode of 500 still means the action failed.
type InvokeResponse struct {
	StatusCode int    `json:"statusCode"`
	Type       string `json:"type,omitempty"`
	Value      any    `json:"value,omitempty"`
}

// The response types an adaptiveCard/action invoke may carry.
const (
	// InvokeCard replaces the card, for the user who clicked only. This is
	// what lets a decline ask for a reason without a dialog, and without
	// everyone else in the channel seeing the form.
	InvokeCard = "application/vnd.microsoft.card.adaptive"
	// InvokeMessage shows a short message to the user who clicked.
	InvokeMessage = "application/vnd.microsoft.activity.message"
	// InvokeError reports a failure.
	InvokeError = "application/vnd.microsoft.error"
)

// CardResponse replaces the clicker's copy of the card.
func CardResponse(card Card) InvokeResponse {
	return InvokeResponse{StatusCode: 200, Type: InvokeCard, Value: card}
}

// MessageResponse shows the clicker a line of text.
func MessageResponse(s string) InvokeResponse {
	return InvokeResponse{StatusCode: 200, Type: InvokeMessage, Value: s}
}

// ErrorResponse reports that the action failed. Teams shows the message.
func ErrorResponse(status int, s string) InvokeResponse {
	return InvokeResponse{StatusCode: status, Type: InvokeError, Value: map[string]any{
		"code":    "BadRequest",
		"message": s,
	}}
}

// CardAction is the value of an adaptiveCard/action invoke: the action that
// was taken, with the card's inputs folded into Data.
type CardAction struct {
	Action struct {
		Type string          `json:"type"`
		ID   string          `json:"id"`
		Verb string          `json:"verb"`
		Data json.RawMessage `json:"data"`
	} `json:"action"`
	// Trigger is "manual" when someone clicked and "automatic" on a refresh.
	Trigger string `json:"trigger"`
}

// Envelope wraps one activity delivered over socket mode.
type Envelope struct {
	ProtocolVersion int    `json:"protocolVersion"`
	Type            string `json:"type"` // "invoke", or "activity" for the rest
	EnvelopeID      string `json:"envelopeId"`
	BotKey          string `json:"botKey"`
	// CV is a correlation ID, worth logging when reporting a problem.
	CV string `json:"cv"`
	// DeadlineMs is how long ACS waits for the reply. Past it, the delivery
	// counts as failed and the user sees an error.
	DeadlineMs int `json:"deadlineMs"`
	// AckRequired is set on anything that is not an invoke.
	AckRequired bool                `json:"ackRequired"`
	Headers     map[string][]string `json:"headers"`
	Payload     Activity            `json:"payload"`
}

// Reply is what goes back to ACS for an Envelope, on the same call.
type Reply struct {
	ProtocolVersion int    `json:"protocolVersion"`
	EnvelopeID      string `json:"envelopeId"`
	Status          int    `json:"status"`
	Body            any    `json:"body,omitempty"`
	BotKey          string `json:"botKey,omitempty"`
	RecvAt          int64  `json:"recvAt,omitempty"`
	TS              int64  `json:"ts,omitempty"`
}

// socketReady is the frame ACS sends when a socket may receive activities.
type socketReady struct {
	ProtocolVersion int    `json:"protocolVersion"`
	BotKey          string `json:"botKey"`
	ConnectionID    string `json:"connectionId"`
}

// socketClosing is the warning ACS sends before closing a socket on purpose.
type socketClosing struct {
	ProtocolVersion int    `json:"protocolVersion"`
	BotKey          string `json:"botKey"`
	ConnectionID    string `json:"connectionId"`
	Code            string `json:"code"`
	Reason          string `json:"reason"`
	Reconnect       bool   `json:"reconnect"`
}

// The reasons ACS gives for closing a socket.
const (
	// closeDisabled: socket mode was turned off. Reconnecting gets a 403.
	closeDisabled = "SocketModeDisabled"
	// closeReplaced: a newer socket took over and this one lingered.
	closeReplaced = "Replaced"
	// closeTokenReused: two sockets used one access token.
	closeTokenReused = "TokenReused"
	// closeNoIdentity: the access token carried no agent identity.
	closeNoIdentity = "NoIdentity"
)

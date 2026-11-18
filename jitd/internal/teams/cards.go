package teams

// Adaptive Cards, built as maps for the same reason the Slack package
// builds Block Kit as maps: only a handful of element types are ever used,
// and a map keeps the wire format in plain sight.
//
// Two rules hold everything here together, both from Microsoft's
// compatibility guidance. Cards declare version 1.4, the version that
// introduced Action.Execute. And every Action.Execute is wrapped in an
// ActionSet and carries an Action.Submit fallback, because older Teams
// clients drop an Action.Execute they do not understand, and some of them
// only honour the fallback inside an ActionSet.

// CardVersion is the Adaptive Card schema version jitd emits.
const CardVersion = "1.4"

// Card is an Adaptive Card.
type Card map[string]any

// Element is one item in a card's body.
type Element map[string]any

// Action is a button.
type Action map[string]any

// NewCard is an Adaptive Card with the given body.
func NewCard(body ...Element) Card {
	return Card{
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"type":    "AdaptiveCard",
		"version": CardVersion,
		"body":    body,
	}
}

// Attach wraps a card so it can ride on an activity.
func Attach(c Card) Attachment {
	return Attachment{ContentType: InvokeCard, Content: c}
}

// Text is a line of text. Teams renders a subset of Markdown in these, so
// callers pass text that has already been made safe.
func Text(s string) Element {
	return Element{"type": "TextBlock", "text": s, "wrap": true}
}

// Heading is the card's title line.
func Heading(s string) Element {
	return Element{"type": "TextBlock", "text": s, "wrap": true, "size": "Medium", "weight": "Bolder"}
}

// Subtle is small, greyed-out text, for the footer.
func Subtle(s string) Element {
	return Element{"type": "TextBlock", "text": s, "wrap": true, "size": "Small", "isSubtle": true}
}

// Quote sets text off from the rest, the way a block quote does in Slack.
// Adaptive Cards have no block quote, so this is a bordered container.
func Quote(s string) Element {
	return Element{
		"type":  "Container",
		"style": "emphasis",
		"items": []Element{Text(s)},
	}
}

// Fact is one name and value in a Facts block.
type Fact struct {
	Title string
	Value string
}

// Facts is a label-and-value list.
func Facts(fs ...Fact) Element {
	out := make([]map[string]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, map[string]string{"title": f.Title, "value": f.Value})
	}
	return Element{"type": "FactSet", "facts": out}
}

// Actions is a row of buttons. Action.Execute only falls back reliably on
// older clients inside one of these, so every button goes through here.
func Actions(as ...Action) Element {
	return Element{"type": "ActionSet", "actions": as}
}

// Execute is a button that calls back to the bot and can replace the card.
// Style is "positive", "destructive" or empty.
//
// The data carries the verb as well, so the Action.Submit fallback on an
// older client still says which button was pressed: a fallback submit
// arrives as a plain message with this data and no verb of its own.
func Execute(verb, title, style string, data map[string]any) Action {
	d := map[string]any{"verb": verb}
	for k, v := range data {
		d[k] = v
	}
	a := Action{
		"type":  "Action.Execute",
		"title": title,
		"verb":  verb,
		"data":  d,
		"fallback": Action{
			"type":  "Action.Submit",
			"title": title,
			"data":  d,
		},
	}
	if style != "" {
		a["style"] = style
	}
	return a
}

// OpenURL is a button that opens a link. jitd uses it for the one thing
// Teams cannot do in the card: the full request page on the tailnet.
func OpenURL(title, url string) Action {
	return Action{"type": "Action.OpenUrl", "title": title, "url": url}
}

// TextInput is a text field.
func TextInput(id, placeholder string, multiline bool) Element {
	e := Element{"type": "Input.Text", "id": id, "placeholder": placeholder}
	if multiline {
		e["isMultiline"] = true
	}
	return e
}

// Choice is one option in a ChoiceSet.
type Choice struct {
	Title string
	Value string
}

// ChoiceSet is a dropdown.
func ChoiceSet(id, placeholder string, choices []Choice, value string) Element {
	out := make([]map[string]string, 0, len(choices))
	for _, c := range choices {
		out = append(out, map[string]string{"title": c.Title, "value": c.Value})
	}
	e := Element{
		"type":        "Input.ChoiceSet",
		"id":          id,
		"style":       "compact",
		"choices":     out,
		"placeholder": placeholder,
	}
	if value != "" {
		e["value"] = value
	}
	return e
}

// Message is an activity carrying one card.
func Message(c Card) Activity {
	return Activity{Type: "message", Attachments: []Attachment{Attach(c)}}
}

// TextMessage is an activity carrying plain text.
func TextMessage(s string) Activity {
	return Activity{Type: "message", Text: s}
}

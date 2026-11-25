package teamsbot

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/url"
	"strings"
)

// AppName is what the app is called in Teams.
const AppName = "JIT access"

// ManifestPath is where jitd serves the app package.
//
// Slack can be bootstrapped with a link that opens its "create app" page
// already filled in. Teams has nothing of the kind: an app is a zip of a
// manifest and two icons that someone uploads, so jitd builds that zip
// with the configured app ID already in it.
const ManifestPath = "/admin/teams/manifest.zip"

// manifestVersion is the Teams app manifest schema this targets. Held
// deliberately a little behind the newest: a manifest a tenant's Teams
// does not understand is refused at upload, which is a worse failure than
// missing the newest field.
//
// The schema sets additionalProperties:false, so a field that has been
// retired is not ignored at upload, it is rejected: packageName was valid
// up to 1.16 and gone by 1.17. Anything added here has to exist in this
// version of the schema; TestManifestFields guards the list.
const manifestVersion = "1.17"

// projectURL stands in for the developer URLs. Teams requires https URLs
// that it can parse, and jitd's own address is usually http://jit, which
// is neither public nor https.
const projectURL = "https://github.com/atoonk/tailscale-jit"

// Manifest is the Teams app manifest for a bot with appID.
func Manifest(appID, baseURL string) map[string]any {
	m := map[string]any{
		"$schema":         fmt.Sprintf("https://developer.microsoft.com/en-us/json-schemas/teams/v%s/MicrosoftTeams.schema.json", manifestVersion),
		"manifestVersion": manifestVersion,
		"version":         "1.0.0",
		"id":              appID,
		"developer": map[string]any{
			"name":          "tailscale-jit",
			"websiteUrl":    projectURL,
			"privacyUrl":    projectURL,
			"termsOfUseUrl": projectURL,
		},
		"icons": map[string]any{"color": "color.png", "outline": "outline.png"},
		"name":  map[string]any{"short": AppName, "full": "Just-in-time access for Tailscale"},
		"description": map[string]any{
			"short": "Approve just-in-time access requests in Teams.",
			"full": "Posts access requests from jitd to a channel and lets approvers approve or decline " +
				"from the message. Decisions go through the same checks as the browser. " +
				"Type \"help\" to the bot to see what it understands.",
		},
		"accentColor": "#4F46E5",
		"bots": []any{map[string]any{
			"botId":              appID,
			"scopes":             []string{"personal", "team", "groupChat"},
			"supportsFiles":      false,
			"isNotificationOnly": false,
			"commandLists": []any{map[string]any{
				"scopes": []string{"personal", "team", "groupChat"},
				"commands": []any{
					map[string]any{"title": "request", "description": "Ask for access for one of your devices"},
					map[string]any{"title": "status", "description": "What is waiting for your approval, and your own requests"},
					map[string]any{"title": "help", "description": "What this bot understands"},
				},
			}},
		}},
		"permissions": []string{"identity", "messageTeamMembers"},
	}
	if d := manifestDomain(baseURL); d != "" {
		m["validDomains"] = []string{d}
	}
	return m
}

// manifestDomain is the host of jitd's own URL, for validDomains. A
// tailnet name such as "jit" has no dot and Teams refuses it, so it is
// left out rather than making the package invalid.
func manifestDomain(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return ""
	}
	h := u.Hostname()
	if !strings.Contains(h, ".") {
		return ""
	}
	return h
}

// AppPackage builds the zip to upload to Teams: the manifest and the two
// icons, with no enclosing folder, which Teams requires.
func (b *Bot) AppPackage() ([]byte, error) {
	appID := b.Status().AppID
	if appID == "" {
		return nil, badf("set the app ID first: the package has to name the bot it belongs to")
	}
	manifest, err := json.MarshalIndent(Manifest(appID, b.BaseURL), "", "  ")
	if err != nil {
		return nil, err
	}
	colorIcon, err := encodePNG(keyhole(192, accent, white))
	if err != nil {
		return nil, err
	}
	outlineIcon, err := encodePNG(keyhole(32, color.RGBA{}, white))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	for _, f := range []struct {
		name string
		data []byte
	}{
		{"manifest.json", manifest},
		{"color.png", colorIcon},
		{"outline.png", outlineIcon},
	} {
		w, err := z.Create(f.name)
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(f.data); err != nil {
			return nil, err
		}
	}
	if err := z.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

var (
	accent = color.RGBA{0x4F, 0x46, 0xE5, 0xFF}
	white  = color.RGBA{0xFF, 0xFF, 0xFF, 0xFF}
)

// keyhole draws the app icon: a keyhole in fg on bg, at size square. The
// icons are drawn rather than embedded so there are no binary assets to
// keep in step with the code, and so the outline icon, which Teams wants
// transparent, is the same shape as the colour one.
//
// A transparent bg (zero alpha) gives the outline icon.
func keyhole(size int, bg, fg color.RGBA) image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	s := float64(size)
	// Geometry as fractions of the icon, so both sizes look the same.
	cx, cy, r := s/2, s*0.40, s*0.17
	stemTop, stemBottom := s*0.46, s*0.74
	topHalf, bottomHalf := s*0.055, s*0.105

	// 3x3 supersampling: at 32 pixels a hard edge looks broken.
	const ss = 3
	inside := func(x, y float64) bool {
		if math.Hypot(x-cx, y-cy) <= r {
			return true
		}
		if y >= stemTop && y <= stemBottom {
			t := (y - stemTop) / (stemBottom - stemTop)
			if math.Abs(x-cx) <= topHalf+t*(bottomHalf-topHalf) {
				return true
			}
		}
		return false
	}
	for py := range size {
		for px := range size {
			hits := 0
			for sy := range ss {
				for sx := range ss {
					x := float64(px) + (float64(sx)+0.5)/ss
					y := float64(py) + (float64(sy)+0.5)/ss
					if inside(x, y) {
						hits++
					}
				}
			}
			cov := float64(hits) / (ss * ss)
			img.Set(px, py, blend(bg, fg, cov))
		}
	}
	return img
}

// blend mixes fg over bg by cov, keeping transparency where bg is clear.
func blend(bg, fg color.RGBA, cov float64) color.NRGBA {
	mix := func(b, f uint8) uint8 {
		return uint8(math.Round(float64(b)*(1-cov) + float64(f)*cov))
	}
	return color.NRGBA{
		R: mix(bg.R, fg.R),
		G: mix(bg.G, fg.G),
		B: mix(bg.B, fg.B),
		A: mix(bg.A, fg.A),
	}
}

func encodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

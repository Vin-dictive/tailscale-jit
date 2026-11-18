// Package teamsbot is jitd's Microsoft Teams integration: it announces
// access requests in Teams, lets approvers approve or decline from the
// card, and answers /jit. Teams is only another way in: every decision
// goes through the same jit.Service methods, with the same rules, as one
// made in the browser. The one new thing is identity: a Teams user is
// matched to a Tailscale login by their work email, and every decision
// made that way is recorded as "via Teams".
//
// It is a near-twin of the slackbot package on purpose. Where the two
// differ it is because Teams does, and those places say so.
package teamsbot

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/atoonk/tailscale-jit/jitd/internal/setup"
)

// settingKey is where the connection is kept in the store's settings.
const settingKey = "teams"

// Env variables that set the connection from the env file instead of the
// admin page, for people who manage secrets outside jitd.
const (
	EnvAppID    = "TEAMS_APP_ID"
	EnvPassword = "TEAMS_APP_PASSWORD"
	EnvTenantID = "TEAMS_TENANT_ID"
	EnvDomains  = "TEAMS_ALLOWED_DOMAINS" // comma-separated
)

var envKeys = []string{EnvAppID, EnvPassword, EnvTenantID, EnvDomains}

// Settings is the Teams connection. The client secret never leaves jitd:
// the API and the pages only ever show its last four characters.
type Settings struct {
	// AppID is the Microsoft app ID of the bot's Entra app registration.
	// It is not a secret: it goes in the app package everyone installs.
	AppID string `json:"app_id"`
	// AppPassword is that registration's client secret.
	AppPassword string `json:"app_password"`
	// TenantID is the organisation's Entra tenant. It is both where the
	// token comes from and the only tenant whose people may act in jitd,
	// which is why a single-tenant app registration is the configuration
	// this expects.
	TenantID string `json:"tenant_id"`
	// AllowedDomains are the email domains a Teams user's address must be
	// in to be matched to a Tailscale login, e.g. "example.com".
	AllowedDomains []string `json:"allowed_domains"`
	// Installed are the teams jitd has seen the app installed in. Teams has
	// no "list my channels" call, so this is learned from the
	// conversationUpdate ACS sends on install, and it is what makes the
	// channel picker and approver direct messages possible at all.
	Installed   []InstalledTeam `json:"installed,omitempty"`
	ConnectedBy string          `json:"connected_by,omitempty"`
	ConnectedAt time.Time       `json:"connected_at,omitzero"`
}

// InstalledTeam is a team the app is installed in.
type InstalledTeam struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	// ServiceURL is the ACS endpoint for this team. It varies by tenant and
	// can change, so it is remembered from the activity rather than guessed.
	ServiceURL string    `json:"service_url"`
	TenantID   string    `json:"tenant_id,omitempty"`
	SeenAt     time.Time `json:"seen_at,omitzero"`
}

func (s Settings) configured() bool {
	return s.AppID != "" && s.AppPassword != "" && s.TenantID != ""
}

// Status is what the admin page and GET /v1/teams show.
type Status struct {
	// Source is where the credentials come from: "ui" (saved on the Teams
	// page), "env" (the env file or environment) or "" (not set up).
	Source    string    `json:"source"`
	EnvFile   string    `json:"env_file,omitempty"`
	State     string    `json:"state"` // off, connecting, connected, error
	Since     time.Time `json:"since,omitzero"`
	LastError string    `json:"last_error,omitempty"`
	// Geos is the per-region state. A bot connected in two regions out of
	// three misses conversations hosted in the third, so the page shows all
	// of them rather than one summary.
	Geos []GeoState `json:"geos,omitempty"`
	// AppID is shown in full: it is in the app package, not a secret.
	AppID          string          `json:"app_id,omitempty"`
	TenantID       string          `json:"tenant_id,omitempty"`
	PasswordHint   string          `json:"password_hint,omitempty"` // "…a1b2"
	AllowedDomains []string        `json:"allowed_domains"`
	Installed      []InstalledTeam `json:"installed,omitempty"`
	ConnectedBy    string          `json:"connected_by,omitempty"`
	ConnectedAt    time.Time       `json:"connected_at,omitzero"`
	// ManifestPath downloads the app package to upload to Teams. Slack can
	// be bootstrapped with a link that pre-fills its "create app" page;
	// Teams has no equivalent, so jitd builds the package itself.
	ManifestPath string `json:"manifest_path"`
}

// GeoState is one ACS region's connection state.
type GeoState struct {
	Geo       string `json:"geo"`
	State     string `json:"state"`
	LastError string `json:"last_error,omitempty"`
}

// Connected reports whether every region is up.
func (s Status) Connected() bool { return s.State == "connected" }

// Configured reports whether there are credentials.
func (s Status) Configured() bool { return s.Source != "" }

// hint shows the last four characters of a secret.
func hint(secret string) string {
	if len(secret) < 8 {
		return ""
	}
	return "…" + secret[len(secret)-4:]
}

// envSettings reads the Teams variables from the env file, then the
// environment for anything the file lacks.
func envSettings(file string) (Settings, error) {
	vars := map[string]string{}
	if file != "" {
		m, err := setup.ReadEnvFile(file)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Settings{}, err
		}
		for _, k := range envKeys {
			if m[k] != "" {
				vars[k] = m[k]
			}
		}
	}
	for _, k := range envKeys {
		if vars[k] == "" {
			vars[k] = os.Getenv(k)
		}
	}
	return Settings{
		AppID:          strings.TrimSpace(vars[EnvAppID]),
		AppPassword:    strings.TrimSpace(vars[EnvPassword]),
		TenantID:       strings.TrimSpace(vars[EnvTenantID]),
		AllowedDomains: CleanDomains(strings.Split(vars[EnvDomains], ",")),
	}, nil
}

// CleanDomains lower-cases, trims and de-duplicates email domains,
// dropping a leading "@".
func CleanDomains(in []string) []string {
	var out []string
	for _, d := range in {
		d = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(d), "@"))
		if d != "" && !slices.Contains(out, d) {
			out = append(out, d)
		}
	}
	return out
}

// validDomains checks email domains for the allow list.
func validDomains(ds []string) error {
	if len(ds) == 0 {
		return fmt.Errorf("add at least one email domain (the part after @ in your people's work addresses)")
	}
	for _, d := range ds {
		if strings.ContainsAny(d, " @/,") || !strings.Contains(d, ".") {
			return fmt.Errorf("invalid email domain %q: use e.g. example.com", d)
		}
	}
	return nil
}

// emailDomain returns the part after the @.
func emailDomain(email string) string {
	_, d, _ := strings.Cut(strings.ToLower(email), "@")
	return d
}

// guidRE is the shape of an Entra app ID or tenant ID. Checking it here
// turns a pasted-the-wrong-box mistake into a message rather than a token
// request that fails for no obvious reason.
func validGUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

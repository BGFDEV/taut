package slackapi

import (
	"slices"
	"strings"
)

// Scope is one OAuth scope Taut uses and why.
type Scope struct {
	Name string `json:"name"`
	Why  string `json:"why"`
	// Feature is empty for scopes that are always required, or names the
	// optional feature that needs it.
	Feature string `json:"feature,omitempty"`
}

// Features select which optional scopes are required.
type Features struct {
	PrivateChannels bool
	// DirectMessages is whether direct messages are migrated. It selects
	// no scope: Taut reads direct messages from the export only, and does
	// not move their files, so it never asks Slack for one. A
	// bot token could not read people's conversations with each other
	// anyway. The field stays so callers can say what the migration does.
	DirectMessages bool
}

// Feature names.
const (
	FeaturePrivate = "private channels"
)

// Scopes is the catalog of bot token scopes, least privilege, read-only.
// Taut never writes to Slack except to join public channels so it can read
// them.
var Scopes = []Scope{
	{Name: "channels:read", Why: "List public channels"},
	{Name: "channels:history", Why: "Read public channel messages and threads"},
	{Name: "channels:join", Why: "Join public channels so their history is readable"},
	{Name: "users:read", Why: "List members"},
	{Name: "users:read.email", Why: "Match members to Entra ID users by email"},
	{Name: "files:read", Why: "Download files shared in conversations"},
	{Name: "emoji:read", Why: "Resolve custom emoji"},
	{Name: "team:read", Why: "Identify the workspace"},
	{Name: "groups:read", Why: "List private channels", Feature: FeaturePrivate},
	{Name: "groups:history", Why: "Read private channel messages", Feature: FeaturePrivate},
}

// ScopeStatus is the comparison of one scope against what was granted.
type ScopeStatus struct {
	Scope
	Granted  bool `json:"granted"`
	Required bool `json:"required"`
}

func (f Features) needs(feature string) bool {
	switch feature {
	case "":
		return true
	case FeaturePrivate:
		return f.PrivateChannels
	}
	return false
}

// Compare reports, for every scope in the catalog, whether it is required
// for the selected features and whether it was granted.
func Compare(granted []string, f Features) []ScopeStatus {
	out := make([]ScopeStatus, 0, len(Scopes))
	for _, s := range Scopes {
		out = append(out, ScopeStatus{Scope: s, Granted: slices.Contains(granted, s.Name), Required: f.needs(s.Feature)})
	}
	return out
}

// Required returns the scope names needed for the selected features.
func Required(f Features) []string {
	var out []string
	for _, s := range Scopes {
		if f.needs(s.Feature) {
			out = append(out, s.Name)
		}
	}
	return out
}

// AppManifest returns a Slack app manifest (YAML) that an administrator can
// paste into https://api.slack.com/apps → "Create New App" → "From a
// manifest" to create the internal app Taut reads through. Creating the app
// inside the customer's own workspace keeps it on Slack's standard rate
// limits (see "A Slack app per customer from a manifest" in
// docs/design-notes.md).
func AppManifest(f Features) string {
	var b strings.Builder
	b.WriteString(`# Slack app manifest for Taut (read-only).
# Create the app at https://api.slack.com/apps -> Create New App -> From a manifest,
# install it to the workspace, then copy the Bot User OAuth Token (xoxb-...)
# into the secret your Taut config file references.
display_information:
  name: Taut Migration
  description: Read-only access used to migrate this workspace to Microsoft Teams.
features:
  bot_user:
    display_name: Taut Migration
    always_online: false
oauth_config:
  scopes:
    bot:
`)
	for _, s := range Required(f) {
		b.WriteString("      - " + s + "\n")
	}
	b.WriteString(`settings:
  org_deploy_enabled: false
  socket_mode_enabled: false
  token_rotation_enabled: false
`)
	return b.String()
}

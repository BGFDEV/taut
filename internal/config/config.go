// Package config defines the config file: Taut's declarative migration plan.
//
// A config file is a YAML file (taut.yaml by default) that says which Slack
// export to read, which Entra tenant and Teams to write to, and how to map
// identities and content. It never contains secrets; credentials are named by
// reference (secret.Ref) and anything that looks like a literal token is
// rejected by Validate.
//
// Parse and Load decode strictly (unknown fields are errors with line
// numbers), apply defaults and remember the file's directory so relative
// paths resolve against it. Validate reports every problem at once.
package config

import (
	"path"
	"path/filepath"
	"time"

	"github.com/bgfdev/taut/internal/secret"
)

const (
	// DefaultPath is where Taut looks for the config file when --config is
	// not given.
	DefaultPath = "taut.yaml"
	// APIVersion is the schema version this package reads and writes.
	APIVersion = "taut.dev/v1alpha1"
	// Kind is the only document kind this package understands.
	Kind = "Config"
)

// Entra national clouds (target.entra.cloud).
const (
	CloudGlobal    = "global"
	CloudUSGov     = "usgov"
	CloudUSGovHigh = "usgovhigh"
	CloudDoD       = "dod"
	CloudChina     = "china"
)

// Team visibility (target.teams[].visibility).
const (
	VisibilityPrivate = "private"
	VisibilityPublic  = "public"
)

// Slack channel kinds (target.teams[].channels.types).
const (
	ChannelPublic  = "public"
	ChannelPrivate = "private"
)

// Policies for Slack users with no Entra match (identity.unmapped.policy).
const (
	PolicyFormerMember   = "former-member"
	PolicyServiceAccount = "service-account"
	PolicyBlock          = "block"
)

// Reaction handling (content.reactions).
const (
	ReactionsFooter = "footer"
	ReactionsNative = "native"
	ReactionsDrop   = "drop"
)

// EmojiText is the one way custom emoji are handled (content.customEmoji):
// each is written as its ":name:" text. Showing it as an image would need
// the image, which a Slack export does not hold and which a worker, having
// no access to Slack's Web API, cannot fetch.
const EmojiText = "text"

// Load strategies (load.strategy).
const (
	StrategyModern = "modern"
	StrategyLegacy = "legacy"
)

// DateLayout is the format of content.since and content.until.
const DateLayout = "2006-01-02"

// Config is the root of the migration plan.
type Config struct {
	APIVersion string     `yaml:"apiVersion" json:"apiVersion"`
	Kind       string     `yaml:"kind" json:"kind"`
	Metadata   Metadata   `yaml:"metadata" json:"metadata"`
	Source     Source     `yaml:"source" json:"source"`
	Target     Target     `yaml:"target" json:"target"`
	Identity   Identity   `yaml:"identity" json:"identity"`
	Content    Content    `yaml:"content" json:"content"`
	Load       LoadSpec   `yaml:"load" json:"load"`
	Staging    Staging    `yaml:"staging" json:"staging"`
	Controller Controller `yaml:"controller" json:"controller"`
	// Local, when enabled, runs the whole migration in the one taut process
	// on the operator's machine. Staging and Controller are then not used.
	Local Local `yaml:"local,omitempty" json:"local,omitzero"`

	// src remembers where the config file was read from. It is nil for
	// Config files built in memory.
	src *source
}

// Metadata identifies the migration.
type Metadata struct {
	// Name is a DNS-label style identifier, such as "acme-migration".
	Name string `yaml:"name" json:"name"`
}

// Source describes where the data comes from.
type Source struct {
	Slack SlackSource `yaml:"slack" json:"slack"`
}

// SlackSource is a Slack workspace export, optionally enriched through the
// Slack Web API.
type SlackSource struct {
	// Export is the path of the export ZIP or its extracted directory,
	// relative to the config file.
	Export string   `yaml:"export" json:"export"`
	API    SlackAPI `yaml:"api" json:"api"`
}

// SlackAPI configures optional enrichment via the Slack Web API.
type SlackAPI struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// Token references the Slack token; required when Enabled.
	Token secret.Ref `yaml:"token,omitempty" json:"token,omitzero"`
	// BaseURL overrides the Slack API endpoint. Tests only.
	BaseURL string `yaml:"baseURL,omitempty" json:"baseURL,omitempty"`
}

// Target describes where the data goes.
type Target struct {
	Entra          Entra          `yaml:"entra" json:"entra"`
	Teams          []TeamSpec     `yaml:"teams" json:"teams"`
	DirectMessages DirectMessages `yaml:"directMessages" json:"directMessages"`
}

// Entra identifies the tenant and the app registration Taut signs in as.
type Entra struct {
	TenantID string `yaml:"tenantId" json:"tenantId"`
	ClientID string `yaml:"clientId" json:"clientId"`
	// Cloud is one of the Cloud* constants.
	Cloud      string     `yaml:"cloud" json:"cloud"`
	Credential Credential `yaml:"credential" json:"credential"`
	// Authority overrides the token authority. Tests only.
	Authority string `yaml:"authority,omitempty" json:"authority,omitempty"`
	// GraphBaseURL overrides the Microsoft Graph endpoint. Tests only.
	GraphBaseURL string `yaml:"graphBaseURL,omitempty" json:"graphBaseURL,omitempty"`
}

// Credential is how the app registration authenticates. Exactly one of
// Certificate or ClientSecret must be set; a certificate is preferred.
type Credential struct {
	Certificate  secret.Ref `yaml:"certificate,omitempty" json:"certificate,omitzero"`
	ClientSecret secret.Ref `yaml:"clientSecret,omitempty" json:"clientSecret,omitzero"`
}

// TeamSpec describes one Team to create and the Slack channels that go into
// it.
type TeamSpec struct {
	// Name is the Teams display name.
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	// Owner is the UPN of the initial owner.
	Owner string `yaml:"owner" json:"owner"`
	// Visibility is VisibilityPrivate or VisibilityPublic.
	Visibility string           `yaml:"visibility" json:"visibility"`
	Channels   ChannelSelection `yaml:"channels" json:"channels"`
}

// ChannelSelection picks Slack channels by name and kind.
type ChannelSelection struct {
	// Include and Exclude are path.Match-style glob patterns on Slack
	// channel names. Exclude wins.
	Include []string `yaml:"include" json:"include"`
	Exclude []string `yaml:"exclude,omitempty" json:"exclude,omitempty"`
	// Types lists the channel kinds to consider: ChannelPublic and/or
	// ChannelPrivate.
	Types []string `yaml:"types" json:"types"`
	// Rename maps a Slack channel name to its Teams channel name.
	Rename map[string]string `yaml:"rename,omitempty" json:"rename,omitempty"`
}

// DirectMessages controls migration of DMs and group DMs. They are private
// conversations, so they are off unless asked for.
//
// When enabled, each conversation whose every participant has a Microsoft
// 365 account of their own is imported into a Teams chat: a one-on-one DM
// into the chat the two people already have, a group DM into a new group
// chat. It needs the modern load strategy and the global cloud.
type DirectMessages struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
}

// Identity configures the mapping from Slack users to Entra users.
type Identity struct {
	// Overrides is the path of an optional override CSV, relative to the
	// config file.
	Overrides string `yaml:"overrides,omitempty" json:"overrides,omitempty"`
	// Match turns on matchers weaker than an exact email address. Both are
	// off unless set: a wrong match attributes one person's messages to
	// another.
	Match    Match    `yaml:"match,omitempty" json:"match,omitzero"`
	Unmapped Unmapped `yaml:"unmapped" json:"unmapped"`
}

// Match selects the optional identity matchers. They run after the override
// CSV and the exact email match, in the order of the fields, and each only
// matches a Slack user to an Entra user when exactly one of each answers to
// the name.
type Match struct {
	// EmailLocalPart matches on the part of the email address before "@".
	EmailLocalPart bool `yaml:"emailLocalPart,omitempty" json:"emailLocalPart,omitempty"`
	// DisplayName matches on the display name.
	DisplayName bool `yaml:"displayName,omitempty" json:"displayName,omitempty"`
}

// Unmapped says what to do with Slack users that have no Entra match.
type Unmapped struct {
	// Policy is one of the Policy* constants.
	Policy string `yaml:"policy" json:"policy"`
	// Account is the UPN messages are attributed to. Required unless Policy
	// is PolicyBlock.
	Account string `yaml:"account,omitempty" json:"account,omitempty"`
}

// Content controls how messages are converted.
type Content struct {
	// Reactions is one of the Reactions* constants.
	Reactions string `yaml:"reactions" json:"reactions"`
	// CustomEmoji is EmojiText.
	CustomEmoji string `yaml:"customEmoji" json:"customEmoji"`
	// Since and Until are optional inclusive date bounds in DateLayout.
	Since string `yaml:"since,omitempty" json:"since,omitempty"`
	Until string `yaml:"until,omitempty" json:"until,omitempty"`
}

// LoadSpec selects the import mechanism (the `load` section).
type LoadSpec struct {
	// Strategy is StrategyModern or StrategyLegacy.
	Strategy string `yaml:"strategy" json:"strategy"`
}

// Staging is the S3-compatible object store that holds staged data.
type Staging struct {
	Endpoint      string     `yaml:"endpoint" json:"endpoint"`
	Region        string     `yaml:"region" json:"region"`
	Bucket        string     `yaml:"bucket" json:"bucket"`
	AccessKey     secret.Ref `yaml:"accessKey" json:"accessKey"`
	SecretKey     secret.Ref `yaml:"secretKey" json:"secretKey"`
	EncryptionKey secret.Ref `yaml:"encryptionKey" json:"encryptionKey"`
}

// Controller locates the control plane.
type Controller struct {
	URL string `yaml:"url" json:"url"`
}

// Local configures local mode: one taut process does the work of the
// controller, a worker, the token broker and the egress proxy, with its
// ledger in a PostgreSQL server it starts and stops itself and its staging
// in an encrypted directory.
type Local struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	// StateDir is where the ledger, the staged data and their key live,
	// relative to the config file. Default ".taut".
	StateDir string `yaml:"stateDir,omitempty" json:"stateDir,omitempty"`
	// Postgres says where the PostgreSQL server programs come from.
	Postgres LocalPostgres `yaml:"postgres,omitempty" json:"postgres,omitzero"`
}

// LocalPostgres locates the PostgreSQL programs local mode runs.
type LocalPostgres struct {
	// Binaries is the directory of a PostgreSQL installation (the one that
	// holds bin/postgres). Empty means: look on PATH and in the usual
	// places.
	Binaries string `yaml:"binaries,omitempty" json:"binaries,omitempty"`
	// Download allows taut to download PostgreSQL when none is installed.
	// Nothing is downloaded without it.
	Download bool `yaml:"download,omitempty" json:"download,omitempty"`
}

// DefaultStateDir is local.stateDir when the config file does not set it.
const DefaultStateDir = ".taut"

// StateDir returns the absolute path of local mode's state directory.
func (m *Config) StateDir() string {
	dir := m.Local.StateDir
	if dir == "" {
		dir = DefaultStateDir
	}
	dir = m.ResolvePath(dir)
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return dir
}

// source records where a config file came from.
type source struct {
	// dir is the directory relative paths resolve against.
	dir string
	// file is the path the config file was loaded from, if any.
	file string
	// lines maps a field path such as "target.teams[0].owner" to its line.
	lines map[string]int
}

// Default returns a config file with every generic setting filled in and every
// workspace-specific one (name, export path, tenant, credential, teams,
// placeholder account) left empty for the caller, or `taut init`, to supply.
func Default() *Config {
	return &Config{
		APIVersion: APIVersion,
		Kind:       Kind,
		Target: Target{
			Entra: Entra{Cloud: CloudGlobal},
			Teams: []TeamSpec{},
		},
		Identity: Identity{Unmapped: Unmapped{Policy: PolicyFormerMember}},
		Content:  Content{Reactions: ReactionsFooter, CustomEmoji: EmojiText},
		Load:     LoadSpec{Strategy: StrategyModern},
		Staging: Staging{
			Endpoint:      "http://localhost:9000",
			Region:        "us-east-1",
			Bucket:        "taut-staging",
			AccessKey:     secret.Ref{Env: "TAUT_S3_ACCESS_KEY"},
			SecretKey:     secret.Ref{Env: "TAUT_S3_SECRET_KEY"},
			EncryptionKey: secret.Ref{Env: "TAUT_STAGING_KEY"},
		},
		Controller: Controller{URL: "http://localhost:7070"},
	}
}

// DefaultTeam returns a TeamSpec with the per-team defaults Parse applies:
// private visibility, every public channel included.
func DefaultTeam() TeamSpec {
	return TeamSpec{
		Visibility: VisibilityPrivate,
		Channels: ChannelSelection{
			Include: []string{"*"},
			Types:   []string{ChannelPublic},
		},
	}
}

// BaseDir returns the directory relative paths in the config file resolve
// against: the directory of the file it was loaded from, or "" (the working
// directory) for a config file that was parsed from bytes or built in memory.
func (m *Config) BaseDir() string {
	if m.src == nil {
		return ""
	}
	return m.src.dir
}

// SetBaseDir sets the directory relative paths resolve against.
func (m *Config) SetBaseDir(dir string) {
	if m.src == nil {
		m.src = &source{}
	}
	m.src.dir = dir
}

// File returns the path the config file was loaded from, or "".
func (m *Config) File() string {
	if m.src == nil {
		return ""
	}
	return m.src.file
}

// ResolvePath resolves a path written in the config file (the export, the
// overrides CSV, a secret file) against the config file's directory. Absolute
// paths are returned cleaned; an empty path stays empty.
func (m *Config) ResolvePath(p string) string {
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(m.BaseDir(), p)
}

// ResolveSecret reads the secret that ref points to, resolving a relative
// file reference against the config file's directory.
func (m *Config) ResolveSecret(ref secret.Ref) (secret.Value, []secret.Warning, error) {
	return ref.ResolveWithWarnings(m.BaseDir())
}

// Equal reports whether both config files describe the same plan. Where they
// were loaded from is ignored.
func (m *Config) Equal(other *Config) bool {
	if m == nil || other == nil {
		return m == other
	}
	a, errA := m.Marshal()
	b, errB := other.Marshal()
	return errA == nil && errB == nil && string(a) == string(b)
}

// Selects reports whether the team takes the Slack channel with the given
// name and kind (ChannelPublic or ChannelPrivate). The kind must be listed in
// Types, the name must match an Include pattern, and it must match no
// Exclude pattern. An empty Types means public only and an empty Include
// means every channel, matching the defaults Parse applies.
func (t TeamSpec) Selects(channelName string, kind string) bool {
	c := t.Channels
	if len(c.Types) == 0 {
		if kind != ChannelPublic {
			return false
		}
	} else if !contains(c.Types, kind) {
		return false
	}
	if len(c.Include) > 0 && !matchAny(c.Include, channelName) {
		return false
	}
	return !matchAny(c.Exclude, channelName)
}

// TargetName returns the Teams channel name for a Slack channel, applying
// the team's rename map.
func (t TeamSpec) TargetName(slackName string) string {
	if to := t.Channels.Rename[slackName]; to != "" {
		return to
	}
	return slackName
}

// SelectingTeams returns the indexes of the teams whose selection takes the
// given Slack channel. More than one index means the config file is ambiguous
// for that channel.
func (m *Config) SelectingTeams(channelName string, kind string) []int {
	var out []int
	for i, t := range m.Target.Teams {
		if t.Selects(channelName, kind) {
			out = append(out, i)
		}
	}
	return out
}

// ParseDate parses a content.since / content.until value.
func ParseDate(s string) (time.Time, error) {
	return time.Parse(DateLayout, s)
}

func matchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, err := path.Match(p, name); err == nil && ok {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

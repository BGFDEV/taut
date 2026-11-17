// Package slack defines the data model of a Slack workspace export: users,
// channels, messages and files, with JSON tags matching the field names Slack
// writes.
//
// Every field is optional. Slack has changed the export format many times and
// documents little of it, so absent fields decode to their zero value and
// unknown fields are ignored. Rich content (blocks, attachments) is kept as
// raw JSON for the transform stage to interpret.
package slack

import (
	"fmt"
	"log/slog"
	"net/url"
	"strings"
)

// User is one entry of users.json (or org_users.json on Enterprise Grid).
type User struct {
	ID                string  `json:"id"`
	TeamID            string  `json:"team_id,omitempty"`
	Name              string  `json:"name,omitempty"`
	RealName          string  `json:"real_name,omitempty"`
	Deleted           bool    `json:"deleted,omitempty"`
	IsBot             bool    `json:"is_bot,omitempty"`
	IsAppUser         bool    `json:"is_app_user,omitempty"`
	IsRestricted      bool    `json:"is_restricted,omitempty"`       // multi-channel guest
	IsUltraRestricted bool    `json:"is_ultra_restricted,omitempty"` // single-channel guest
	IsStranger        bool    `json:"is_stranger,omitempty"`         // Slack Connect user from another org
	Profile           Profile `json:"profile"`
}

// Profile is the nested profile object of a User.
type Profile struct {
	Email       string `json:"email,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	RealName    string `json:"real_name,omitempty"`
	BotID       string `json:"bot_id,omitempty"`
}

// DisplayLabel returns the best human-readable name for the user: the display
// name, then the real name, then the handle, and finally the ID.
func (u User) DisplayLabel() string {
	return firstNonBlank(u.Profile.DisplayName, u.Profile.RealName, u.RealName, u.Name, u.ID)
}

// IsGuest reports whether the user is a single- or multi-channel guest.
func (u User) IsGuest() bool { return u.IsRestricted || u.IsUltraRestricted }

// ChannelKind says which kind of conversation a Channel is. Exports do not
// carry it as a field; the reader derives it from the index file the channel
// was listed in.
type ChannelKind string

// Channel kinds, one per export index file.
const (
	KindPublic  ChannelKind = "public"  // channels.json
	KindPrivate ChannelKind = "private" // groups.json
	KindDM      ChannelKind = "dm"      // dms.json
	KindMPIM    ChannelKind = "mpim"    // mpims.json (group DMs)
)

// Channel is one conversation listed in channels.json, groups.json, dms.json
// or mpims.json.
type Channel struct {
	ID         string       `json:"id"`
	Name       string       `json:"name,omitempty"` // empty for DMs
	Created    int64        `json:"created,omitempty"`
	Creator    string       `json:"creator,omitempty"`
	IsArchived bool         `json:"is_archived,omitempty"`
	IsGeneral  bool         `json:"is_general,omitempty"`
	Members    []string     `json:"members,omitempty"`
	Topic      ChannelValue `json:"topic"`
	Purpose    ChannelValue `json:"purpose"`

	// Kind is set by the export reader, never by Slack.
	Kind ChannelKind `json:"kind,omitempty"`
}

// ChannelValue is the shape Slack uses for a channel's topic and purpose.
type ChannelValue struct {
	Value   string `json:"value,omitempty"`
	Creator string `json:"creator,omitempty"`
	LastSet int64  `json:"last_set,omitempty"`
}

// Folder returns the name of the export folder holding the channel's day
// files, which doubles as a stable key for the conversation. Slack names the
// folder after the channel, except for DMs, which have no name and use the
// conversation ID.
func (c Channel) Folder() string {
	if c.Kind == KindDM || strings.TrimSpace(c.Name) == "" {
		return c.ID
	}
	return c.Name
}

// Label returns a human-readable name for the channel: its name, or its ID
// for DMs.
func (c Channel) Label() string { return firstNonBlank(c.Name, c.ID) }

// File modes with special meaning.
const (
	FileModeHosted        = "hosted"
	FileModeExternal      = "external"
	FileModeSpace         = "space"           // canvas
	FileModeTombstone     = "tombstone"       // deleted file
	FileModeHiddenByLimit = "hidden_by_limit" // beyond the plan's history limit
)

// File is a file object attached to a message. An export holds links only,
// never file content.
//
// URLPrivate and URLPrivateDownload usually embed a secret export token
// ("?t=xoxe-..."). Treat both as credentials: log SafeURL instead, and note
// that File's String, GoString and LogValue methods never include any URL.
type File struct {
	ID                 string `json:"id"`
	Name               string `json:"name,omitempty"`
	Title              string `json:"title,omitempty"`
	Mimetype           string `json:"mimetype,omitempty"`
	Filetype           string `json:"filetype,omitempty"`
	Size               int64  `json:"size,omitempty"`
	Mode               string `json:"mode,omitempty"`
	User               string `json:"user,omitempty"`
	Created            int64  `json:"created,omitempty"`
	URLPrivate         string `json:"url_private,omitempty"`          // secret
	URLPrivateDownload string `json:"url_private_download,omitempty"` // secret
	IsExternal         bool   `json:"is_external,omitempty"`
	ExternalType       string `json:"external_type,omitempty"`
	ExternalURL        string `json:"external_url,omitempty"`
	Permalink          string `json:"permalink,omitempty"`
	HiddenByLimit      bool   `json:"hidden_by_limit,omitempty"`
	// IsHiddenByLimit is the name Slack's file object reference gives the
	// same flag; exports have been seen with either.
	IsHiddenByLimit bool `json:"is_hidden_by_limit,omitempty"`
	// FileAccess is "check_file_info" on the stub Slack leaves for a file
	// shared from another workspace: only its ID is present.
	FileAccess string `json:"file_access,omitempty"`
}

// IsHidden reports whether Slack withheld the file because it lies beyond the
// workspace plan's history limit. Such entries carry an ID and nothing else.
func (f File) IsHidden() bool {
	return f.HiddenByLimit || f.IsHiddenByLimit || f.Mode == FileModeHiddenByLimit
}

// IsAccessStub reports whether the export holds only a stub for the file,
// as it does for files shared over Slack Connect.
func (f File) IsAccessStub() bool { return f.FileAccess != "" || f.Mode == "file_access" }

// IsTombstone reports whether the file was deleted and only a marker remains.
func (f File) IsTombstone() bool { return f.Mode == FileModeTombstone }

// IsCanvas reports whether the file is a Slack canvas rather than an upload.
func (f File) IsCanvas() bool { return f.Mode == FileModeSpace || f.Mode == "quip" }

// Downloadable reports whether the file has content Taut can fetch from
// Slack: it is hosted by Slack, not withheld or deleted, and has a URL.
func (f File) Downloadable() bool {
	return !f.IsHidden() && !f.IsTombstone() && !f.IsExternal &&
		(f.URLPrivateDownload != "" || f.URLPrivate != "")
}

// SafeURL returns the file's private URL with the query string, fragment and
// any userinfo removed, so it can appear in logs and error messages without
// leaking the export token.
func (f File) SafeURL() string {
	return RedactURL(firstNonBlank(f.URLPrivate, f.URLPrivateDownload))
}

// RedactURL strips the query string, fragment and userinfo from raw. Slack
// puts export tokens in the query string of file URLs.
func RedactURL(raw string) string {
	if i := strings.IndexAny(raw, "?#"); i >= 0 {
		raw = raw[:i]
	}
	if strings.Contains(raw, "@") {
		u, err := url.Parse(raw)
		if err != nil {
			return ""
		}
		u.User = nil
		raw = u.String()
	}
	return raw
}

// String describes the file without any URL.
func (f File) String() string {
	return fmt.Sprintf("file %s (%q, %s, %d bytes)", f.ID, f.Name, firstNonBlank(f.Mode, "unknown mode"), f.Size)
}

// GoString keeps %#v from printing the secret URLs.
func (f File) GoString() string { return "slack.File{" + f.String() + "}" }

// LogValue keeps structured logs from capturing the secret URLs.
func (f File) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("id", f.ID),
		slog.String("name", f.Name),
		slog.String("mode", f.Mode),
		slog.Int64("size", f.Size),
	)
}

// Reaction is one emoji reaction on a message.
type Reaction struct {
	Name  string   `json:"name"`
	Users []string `json:"users,omitempty"`
	Count int      `json:"count,omitempty"`
}

// Edited records the last edit of a message.
type Edited struct {
	User string `json:"user,omitempty"`
	TS   string `json:"ts,omitempty"`
}

func firstNonBlank(candidates ...string) string {
	for _, s := range candidates {
		if t := strings.TrimSpace(s); t != "" {
			return t
		}
	}
	return ""
}

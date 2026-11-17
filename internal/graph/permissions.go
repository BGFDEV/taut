package graph

import (
	"slices"
	"strings"
)

// Requirement says when a permission is needed.
type Requirement string

// Requirement values.
const (
	// RequiredAlways: every migration needs it.
	RequiredAlways Requirement = "always"
	// RequiredForPrivateChannels: only when private or shared channels are migrated.
	RequiredForPrivateChannels Requirement = "private-channels"
	// RequiredForDirectMessages: only when DMs and group DMs are migrated.
	RequiredForDirectMessages Requirement = "direct-messages"
	// Recommended: never blocks a migration, but a read-only preflight
	// check is skipped without it.
	Recommended Requirement = "recommended"
)

// Permission is a Microsoft Graph application permission Taut uses.
type Permission struct {
	// Name is the permission as shown in the Entra admin center and in the
	// token's roles claim.
	Name string `json:"name"`
	// RoleID is the well-known Microsoft Graph app role GUID. It is empty
	// where Taut does not hard-code it; resolve by Name instead.
	RoleID string `json:"roleId,omitempty"`
	// Purpose is what Taut uses it for.
	Purpose string `json:"purpose"`
	// Requirement says when it is needed.
	Requirement Requirement `json:"requirement"`
}

// FeatureSet is the set of optional migration features in play; it decides
// which feature-specific permissions are required.
type FeatureSet struct {
	// PrivateChannels covers private and shared channels.
	PrivateChannels bool
	// DirectMessages covers DMs and group DMs.
	DirectMessages bool
}

// RequiredPermissions returns the catalog of application permissions, in a
// stable order: always-required first, then feature-specific, then
// recommended. The slice is a copy.
func RequiredPermissions() []Permission {
	return []Permission{
		{"Teamwork.Migrate.All", "dfb0dd15-61de-45b2-be36-d6a69fba3c79", "Migration mode and importing messages with original authors and timestamps", RequiredAlways},
		{"User.Read.All", "", "Identity mapping: matching Slack users to Entra users", RequiredAlways},
		{"Team.Create", "", "Creating teams (modern strategy)", RequiredAlways},
		{"Channel.Create", "", "Creating channels (modern strategy)", RequiredAlways},
		{"TeamMember.ReadWrite.All", "", "Adding owners and members at `taut finalize`", RequiredAlways},
		{"ChannelMessage.Read.All", "", "Reading imported messages back, to resume without duplicates", RequiredAlways},
		{"Files.ReadWrite.All", "", "Creating channel folders and uploading small files in SharePoint", RequiredAlways},
		{"Sites.ReadWrite.All", "", "Uploading large files in resumable sessions", RequiredAlways},
		{"ChannelMember.ReadWrite.All", "", "Adding the members of private channels at `taut finalize`", RequiredForPrivateChannels},
		{"Chat.Create", "", "Creating the chats that direct messages and group DMs are imported into", RequiredForDirectMessages},
		{"Chat.ReadBasic.All", "", "Reading a chat's creation time, and finding a group chat whose creation was interrupted", RequiredForDirectMessages},
		{"Chat.Read.All", "", "Reading imported chat messages back, to resume without duplicates", RequiredForDirectMessages},
		{"Organization.Read.All", "", "Showing the tenant name and verified domains in `taut auth` and `taut preflight`", Recommended},
		{"Team.ReadBasic.All", "", "Detecting team name collisions in `taut preflight`", Recommended},
		{"Channel.ReadBasic.All", "", "Detecting channel name collisions in `taut preflight`", Recommended},
	}
}

// PermissionState is the outcome of comparing one permission with a token.
type PermissionState string

// PermissionState values.
const (
	// PermissionGranted: needed (or recommended) and present.
	PermissionGranted PermissionState = "granted"
	// PermissionMissingRequired: needed for the selected features and absent.
	PermissionMissingRequired PermissionState = "missing-required"
	// PermissionMissingOptional: recommended and absent; nothing is blocked.
	PermissionMissingOptional PermissionState = "missing-optional"
	// PermissionNotNeeded: belongs to a feature that is switched off.
	PermissionNotNeeded PermissionState = "not-needed"
)

// PermissionStatus is one row of the granted-versus-required table.
type PermissionStatus struct {
	Permission
	State PermissionState `json:"state"`
	// Granted reports whether the token carries the permission, whatever
	// the State (a not-needed permission may still be granted).
	Granted bool `json:"granted"`
}

// Compare checks the granted roles (see Roles) against the catalog for the
// given features and returns one status per catalog entry, in catalog order.
func Compare(granted []string, features FeatureSet) []PermissionStatus {
	has := func(name string) bool {
		return slices.ContainsFunc(granted, func(g string) bool { return strings.EqualFold(g, name) })
	}
	catalog := RequiredPermissions()
	out := make([]PermissionStatus, 0, len(catalog))
	for _, p := range catalog {
		st := PermissionStatus{Permission: p, Granted: has(p.Name)}
		needed := p.Requirement == RequiredAlways ||
			(p.Requirement == RequiredForPrivateChannels && features.PrivateChannels) ||
			(p.Requirement == RequiredForDirectMessages && features.DirectMessages)
		switch {
		case needed && st.Granted, p.Requirement == Recommended && st.Granted:
			st.State = PermissionGranted
		case needed:
			st.State = PermissionMissingRequired
		case p.Requirement == Recommended:
			st.State = PermissionMissingOptional
		default:
			st.State = PermissionNotNeeded
		}
		out = append(out, st)
	}
	return out
}

// MissingRequired returns the names of permissions in statuses whose state
// is PermissionMissingRequired.
func MissingRequired(statuses []PermissionStatus) []string {
	var out []string
	for _, s := range statuses {
		if s.State == PermissionMissingRequired {
			out = append(out, s.Name)
		}
	}
	return out
}

// Unrecognized returns granted roles that are not in the catalog, sorted.
// They are worth surfacing: the app holds more privilege than Taut needs.
func Unrecognized(granted []string) []string {
	catalog := RequiredPermissions()
	var out []string
	for _, g := range granted {
		if !slices.ContainsFunc(catalog, func(p Permission) bool { return strings.EqualFold(p.Name, g) }) {
			out = append(out, g)
		}
	}
	slices.Sort(out)
	return out
}

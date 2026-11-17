package graph

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bgfdev/taut/internal/fail"
)

// This file is the write path: creating teams and channels, migration mode,
// importing messages and reading them back.

// The application permissions the write-path endpoints need.
const (
	permMigrate      = "Teamwork.Migrate.All"
	permMessageRead  = "ChannelMessage.Read.All"
	permTeamCreate   = "Team.Create"
	permChannelNew   = "Channel.Create"
	permTeamMember   = "TeamMember.ReadWrite.All"
	permChanMember   = "ChannelMember.ReadWrite.All"
	maxMessagePage   = 50
	maxProvisionStep = 10 * time.Second
)

// TeamSpec describes a team to create.
type TeamSpec struct {
	DisplayName string
	Description string
	// Visibility is "private" (the default when empty) or "public".
	Visibility string
	// OwnerID is the Entra object ID of the team's first owner. App-only
	// creation requires exactly one, except in migration mode.
	OwnerID string
	// MigrationCreatedAt, when set, creates the team in migration mode
	// (the "legacy" import path, and the only one in the national clouds)
	// with this creation time, which must not be in the future. Such a
	// team is created without members: nobody can be added, the owner
	// included, until CompleteTeamMigration. OwnerID is not sent.
	MigrationCreatedAt *time.Time
}

// ChannelSpec describes a channel to create.
type ChannelSpec struct {
	DisplayName string
	Description string
	// MembershipType is "standard" (the default when empty), "private" or
	// "shared".
	MembershipType string
	// OwnerID is the Entra object ID of the first owner of a private or
	// shared channel. App-only creation of such a channel requires one,
	// and the owner must already be a member of the team. It is ignored
	// for standard channels, whose members are the team's.
	OwnerID string
	// MigrationCreatedAt, when set, creates the channel in migration mode
	// with this creation time (the "legacy" import path). Graph supports
	// that for standard channels only.
	MigrationCreatedAt *time.Time
}

// creationTime formats a creation time for a team or channel created in
// migration mode.
func creationTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// MaxMembersPerAdd is the most users Graph accepts in one bulk
// members/add request.
const MaxMembersPerAdd = 200

func usage(what, why, next string) *fail.Error {
	return fail.New("graph.bad_request", what, why, next)
}

func teamPath(teamID string) string { return "/v1.0/teams/" + url.PathEscape(teamID) }

func channelPath(teamID, channelID string) string {
	return teamPath(teamID) + "/channels/" + url.PathEscape(channelID)
}

func (c *Client) bind(collection, id string) string {
	return strings.TrimRight(c.base.String(), "/") + "/v1.0/" + collection + "('" + strings.ReplaceAll(id, "'", "''") + "')"
}

var (
	teamLocRE = regexp.MustCompile(`teams\('([^']+)'\)`)
	opLocRE   = regexp.MustCompile(`operations\('([^']+)'\)`)
)

// CreateTeam creates a team from the standard template with one owner (or,
// with TeamSpec.MigrationCreatedAt, in migration mode with none) and
// waits until Graph has finished provisioning it, polling the creation
// operation within Options.Provisioning.
//
// The POST is never repeated automatically: Graph allows two teams with the
// same name, so a retry after a lost response could create a duplicate.
// Callers that may be resuming should call FindTeams first.
//
// If the team was accepted but provisioning failed
// (graph.team_provisioning_failed) or did not finish in time
// (graph.team_provisioning_timeout), the new team's ID is returned together
// with the error so the caller can record it.
func (c *Client) CreateTeam(ctx context.Context, spec TeamSpec) (string, error) {
	migration := spec.MigrationCreatedAt != nil
	if strings.TrimSpace(spec.DisplayName) == "" || (!migration && strings.TrimSpace(spec.OwnerID) == "") {
		return "", usage("A team needs a name and an owner",
			"TeamSpec.DisplayName or TeamSpec.OwnerID is empty.",
			"Set the team name and an owner (an Entra user) in the mapping with `taut map`.")
	}
	visibility := spec.Visibility
	if visibility == "" {
		visibility = "private"
	}
	body := map[string]any{
		"template@odata.bind": c.bind("teamsTemplates", "standard"),
		"displayName":         spec.DisplayName,
		"description":         spec.Description,
		"visibility":          visibility,
	}
	if migration {
		// A team in migration mode takes no members; its owner is added
		// after CompleteTeamMigration.
		body["@microsoft.graph.teamCreationMode"] = "migration"
		body["createdDateTime"] = creationTime(*spec.MigrationCreatedAt)
	} else {
		body["members"] = []map[string]any{{
			"@odata.type":     "#microsoft.graph.aadUserConversationMember",
			"roles":           []string{"owner"},
			"user@odata.bind": c.bind("users", spec.OwnerID),
		}}
	}
	var hdr http.Header
	if err := c.do(ctx, request{method: "POST", path: "/v1.0/teams", body: body, permission: permTeamCreate, header: &hdr}, nil); err != nil {
		return "", err
	}
	var teamID, opID string
	for _, h := range []string{hdr.Get("Content-Location"), hdr.Get("Location")} {
		if m := teamLocRE.FindStringSubmatch(h); m != nil && teamID == "" {
			teamID = m[1]
		}
	}
	if m := opLocRE.FindStringSubmatch(hdr.Get("Location")); m != nil {
		opID = m[1]
	}
	if teamID == "" {
		return "", fail.New("graph.bad_response",
			"Microsoft Graph accepted the team but did not say which team it created",
			"The response to POST /v1.0/teams had no usable Content-Location or Location header.",
			"Look the team up by name before retrying (a second attempt could create a duplicate), then run `taut migrate` again.")
	}
	return teamID, c.awaitTeam(ctx, teamID, opID)
}

// awaitTeam polls until the team is provisioned.
func (c *Client) awaitTeam(ctx context.Context, teamID, opID string) error {
	interval := c.provision.PollInterval
	var waited time.Duration
	for checks := 1; ; checks++ {
		var err error
		status := "inProgress"
		if opID != "" {
			var op struct {
				Status string `json:"status"`
				Error  *struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			err = c.do(ctx, request{method: "GET", path: teamPath(teamID) + "/operations/" + url.PathEscape(opID), permission: permTeamCreate}, &op)
			if err == nil {
				status = op.Status
			}
			if status == "failed" {
				why := "Graph reported the createTeam operation " + opID + " for team " + teamID + " as failed"
				if op.Error != nil && op.Error.Code != "" {
					why += " (" + op.Error.Code + ")"
				}
				return fail.New("graph.team_provisioning_failed",
					"Microsoft Teams could not finish creating the team", why+".",
					"Run `taut migrate` again to create the team anew. If it keeps failing, check https://status.cloud.microsoft and the tenant's Teams licensing.")
			}
		} else {
			// No operation to watch: the team is ready once it can be read.
			if _, err = c.GetTeam(ctx, teamID); err == nil {
				status = "succeeded"
			}
		}
		switch {
		case err != nil && !IsNotFound(err):
			return err
		case status == "succeeded":
			return nil
		}
		if waited+interval > c.provision.MaxWait {
			return fail.New("graph.team_provisioning_timeout",
				"Microsoft Teams is taking too long to create the team",
				fmt.Sprintf("Team %s was still not ready after %d status checks over %s.", teamID, checks, waited),
				"The team may still appear. Wait a few minutes and run `taut migrate` again; it picks the existing team up instead of creating another.")
		}
		if err := c.sleep(ctx, interval); err != nil {
			return canceled(ctx, "waiting for Microsoft Teams to create a team")
		}
		waited += interval
		interval = min(interval*2, maxProvisionStep)
	}
}

// FindTeams returns the teams whose display name equals displayName,
// ignoring case. Graph allows duplicates, so there may be several.
func (c *Client) FindTeams(ctx context.Context, displayName string) ([]Team, error) {
	var out []Team
	for t, err := range list[Team](ctx, c, request{
		method: "GET", path: "/v1.0/teams", permission: permTeams,
		query: url.Values{"$select": {"id,displayName,description"}, "$filter": {FilterEq("displayName", displayName)}},
	}) {
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(t.DisplayName, displayName) {
			out = append(out, t)
		}
	}
	return out, nil
}

// GetTeam fetches one team. A team that does not exist, or is still being
// provisioned, yields an error matching ErrNotFound.
func (c *Client) GetTeam(ctx context.Context, teamID string) (Team, error) {
	var t Team
	return t, c.do(ctx, request{method: "GET", path: teamPath(teamID), permission: permTeams}, &t)
}

// CreateChannel creates a channel. The POST is not repeated after an
// ambiguous failure; callers that may be resuming should call FindChannel
// first. A taken name fails with graph.channel_exists (IsChannelExists), a
// name Teams does not allow with graph.channel_name_invalid.
func (c *Client) CreateChannel(ctx context.Context, teamID string, spec ChannelSpec) (Channel, error) {
	kind := spec.MembershipType
	if kind == "" {
		kind = "standard"
	}
	body := map[string]any{"displayName": spec.DisplayName, "description": spec.Description, "membershipType": kind}
	if spec.MigrationCreatedAt != nil {
		if kind != "standard" {
			return Channel{}, usage("Only a standard channel can be created in migration mode",
				"ChannelSpec.MigrationCreatedAt is set on a "+kind+" channel; Graph supports creation in migration mode for standard channels only.",
				"This is a bug in Taut; please report it.")
		}
		body["@microsoft.graph.channelCreationMode"] = "migration"
		body["createdDateTime"] = creationTime(*spec.MigrationCreatedAt)
	}
	if kind != "standard" {
		if strings.TrimSpace(spec.OwnerID) == "" {
			return Channel{}, usage("A private or shared channel needs an owner",
				"ChannelSpec.OwnerID is empty.",
				"This is a bug in Taut; please report it.")
		}
		body["members"] = []map[string]any{{
			"@odata.type":     "#microsoft.graph.aadUserConversationMember",
			"roles":           []string{"owner"},
			"user@odata.bind": c.bind("users", spec.OwnerID),
		}}
	}
	classify := func(e *APIError) *fail.Error {
		msg := strings.ToLower(e.Message)
		switch {
		case isNotTeamMember(e):
			return notTeamMember(e, "The owner of a private channel must already be a member of the team.")
		case e.Status != http.StatusBadRequest:
		case strings.Contains(msg, "already exist"):
			e.kind = ErrChannelExists
			return fail.Wrap(e, "graph.channel_exists",
				"A channel with this name already exists in the team",
				fmt.Sprintf("Team %s already has a channel with the requested name (%d characters). %s", teamID, len([]rune(spec.DisplayName)), e.detail()+e.ref()),
				"Rename the channel in the mapping with `taut map`, or map it onto the existing channel.")
		case strings.Contains(msg, "name"):
			return fail.Wrap(e, "graph.channel_name_invalid",
				"Microsoft Teams does not allow this channel name",
				e.detail()+e.ref(),
				"Rename the channel with `taut map`. Names are 1–50 characters, without ~ # % & * { } + / \\ : < > ? | ' \" , or \"..\", "+
					"must not start with _ or ., must not end with ., and must not be a reserved word (forms, CON, PRN, AUX, NUL, COM1–COM9, LPT1–LPT9, desktop.ini, _vti_).")
		}
		return nil
	}
	var ch Channel
	return ch, c.do(ctx, request{method: "POST", path: teamPath(teamID) + "/channels", body: body, permission: permChannelNew, classify: classify}, &ch)
}

// FindChannel looks a channel up by display name, ignoring case.
func (c *Client) FindChannel(ctx context.Context, teamID, displayName string) (Channel, bool, error) {
	rq := request{
		method: "GET", path: teamPath(teamID) + "/channels", permission: permChannels,
		query: url.Values{"$filter": {FilterEq("displayName", displayName)}},
	}
	for attempt := 0; ; attempt++ {
		var badFilter bool
		for ch, err := range list[Channel](ctx, c, rq) {
			var api *APIError
			if err != nil && attempt == 0 && errors.As(err, &api) && api.Status == http.StatusBadRequest {
				// The service refused the filter; scan the whole list.
				badFilter = true
				break
			}
			if err != nil {
				return Channel{}, false, err
			}
			if strings.EqualFold(ch.DisplayName, displayName) {
				return ch, true, nil
			}
		}
		if !badFilter {
			return Channel{}, false, nil
		}
		rq.query = nil
	}
}

// GetChannel fetches one channel, including its migration mode and
// creation time.
func (c *Client) GetChannel(ctx context.Context, teamID, channelID string) (Channel, error) {
	var ch Channel
	return ch, c.do(ctx, request{method: "GET", path: channelPath(teamID, channelID), permission: permChannels}, &ch)
}

func notOwner(e *APIError) *fail.Error {
	return fail.Wrap(e, "graph.migration_not_owner",
		"Another application owns this channel's migration",
		e.detail()+e.ref(),
		"Only the app registration that called startMigration can import into the channel or complete it. "+
			"Use that app's credentials, or have it complete the migration first.").WithExit(fail.ExitPermission)
}

func isNotOwner(e *APIError) bool {
	return e.Status == http.StatusForbidden && strings.Contains(strings.ToLower(e.Message), "different application")
}

// StartMigration puts a channel into migration mode. If conversationCreation
// is non-nil the channel's creation time is moved back to it (it must be
// older than the current one), so older messages can be imported.
//
// It is safe to repeat: after an ambiguous failure (transport error, 5xx)
// the call is retried, and Graph answers a repeat with 400 "already in
// migration mode", returned as graph.already_migrating. Check it with
// IsAlreadyMigrating and treat it as success when resuming.
func (c *Client) StartMigration(ctx context.Context, teamID, channelID string, conversationCreation *time.Time) error {
	var body any
	if conversationCreation != nil {
		body = map[string]string{"conversationCreationDateTime": conversationCreation.UTC().Format("2006-01-02T15:04:05.000Z")}
	}
	classify := func(e *APIError) *fail.Error {
		if e.Status == http.StatusBadRequest && strings.Contains(strings.ToLower(e.Message), "already in migration") {
			e.kind = ErrAlreadyMigrating
			return fail.Wrap(e, "graph.already_migrating",
				"The channel is already in migration mode",
				e.detail()+e.ref(),
				"Nothing to do if Taut started it: run `taut migrate` to continue importing. Otherwise finish the other migration first.")
		}
		return nil
	}
	return c.do(ctx, request{
		method: "POST", path: channelPath(teamID, channelID) + "/startMigration", body: body,
		permission: permMigrate, retryAmbiguous: true, classify: classify,
	}, nil)
}

// CompleteChannelMigration takes a channel out of migration mode. Like
// StartMigration it is retried after ambiguous failures; a repeat on a
// channel that already left migration mode fails with graph.not_in_migration
// (IsNotInMigration).
func (c *Client) CompleteChannelMigration(ctx context.Context, teamID, channelID string) error {
	classify := func(e *APIError) *fail.Error {
		switch {
		case isNotOwner(e):
			return notOwner(e)
		case e.Status == http.StatusBadRequest && strings.Contains(strings.ToLower(e.Message), "not in migration"):
			e.kind = ErrNotInMigration
			return fail.Wrap(e, "graph.not_in_migration",
				"The channel is not in migration mode",
				e.detail()+e.ref(),
				"Nothing to complete. If the migration was already completed, continue with `taut finalize`.")
		}
		return nil
	}
	return c.do(ctx, request{
		method: "POST", path: channelPath(teamID, channelID) + "/completeMigration",
		permission: permMigrate, retryAmbiguous: true, classify: classify,
	}, nil)
}

// CompleteTeamMigration takes a team that was created in migration mode out
// of it, which is what lets members be added. Every channel of the team,
// General included, must have completed its own migration first; if one has
// not, the error matches ErrChannelsMigrating (IsChannelsMigrating).
//
// Like the channel call it is retried after ambiguous failures; a repeat on
// a team that already left migration mode (or never was in it) fails with
// graph.not_in_migration (IsNotInMigration). Completing a team is final:
// Graph documents no way to put a team back into migration mode.
func (c *Client) CompleteTeamMigration(ctx context.Context, teamID string) error {
	classify := func(e *APIError) *fail.Error {
		msg := strings.ToLower(e.Message)
		switch {
		case isNotOwner(e):
			return notOwner(e)
		case e.Status == http.StatusBadRequest && strings.Contains(msg, "not in migration"):
			e.kind = ErrNotInMigration
			return fail.Wrap(e, "graph.not_in_migration",
				"The team is not in migration mode",
				e.detail()+e.ref(),
				"Nothing to complete. If the migration was already completed, continue with `taut finalize`.")
		case e.Status == http.StatusBadRequest && strings.Contains(msg, "channel") && strings.Contains(msg, "migration"):
			e.kind = ErrChannelsMigrating
			return fail.Wrap(e, "graph.channels_still_migrating",
				"The team still has channels in migration mode",
				"A team's migration can only be completed once every one of its channels, General included, has completed its own. "+e.detail()+e.ref(),
				"Run `taut finalize` without --channel so every channel is completed, then the team.")
		}
		return nil
	}
	return c.do(ctx, request{
		method: "POST", path: teamPath(teamID) + "/completeMigration",
		permission: permMigrate, retryAmbiguous: true, classify: classify,
	}, nil)
}

// AddTeamMember adds a user to a team as a member or an owner.
func (c *Client) AddTeamMember(ctx context.Context, teamID, userID string, owner bool) error {
	roles := []string{}
	if owner {
		roles = []string{"owner"}
	}
	body := map[string]any{
		"@odata.type":     "#microsoft.graph.aadUserConversationMember",
		"roles":           roles,
		"user@odata.bind": c.bind("users", userID),
	}
	return c.do(ctx, request{method: "POST", path: teamPath(teamID) + "/members", body: body, permission: permTeamMember}, nil)
}

// TeamMember is one member of a team.
type TeamMember struct {
	ID     string   `json:"id"`
	UserID string   `json:"userId"`
	Roles  []string `json:"roles"`
}

// IsOwner reports whether the member holds the owner role.
func (m TeamMember) IsOwner() bool {
	for _, r := range m.Roles {
		if strings.EqualFold(r, "owner") {
			return true
		}
	}
	return false
}

// TeamMembers iterates a team's members.
func (c *Client) TeamMembers(ctx context.Context, teamID string) iter.Seq2[TeamMember, error] {
	return list[TeamMember](ctx, c, request{method: "GET", path: teamPath(teamID) + "/members", permission: permTeamMember})
}

// MemberResult is the outcome for one user of a bulk AddTeamMembers call.
type MemberResult struct {
	// UserID is the Entra object ID that was asked for.
	UserID string
	// Code and Message are Graph's error for this user; both are empty
	// when the user was added.
	Code    string
	Message string
}

// Added reports whether the user was added.
func (r MemberResult) Added() bool { return r.Code == "" && r.Message == "" }

// Gone reports whether Graph refused the user because the account does not
// exist (any more). The codes Graph uses per user in a bulk add are not
// documented; this recognises a not-found code or wording, which is how the
// single add reports a deleted or blocked account (404).
func (r MemberResult) Gone() bool {
	if r.Added() {
		return false
	}
	code, msg := strings.ToLower(r.Code), strings.ToLower(r.Message)
	// A message only counts when it is about the user: "team not found"
	// must not be read as a person to leave out.
	return strings.Contains(code, "notfound") || code == "404" ||
		(strings.Contains(msg, "user") && (strings.Contains(msg, "does not exist") || strings.Contains(msg, "not found")))
}

// Transient reports whether a member that was not added may well be added
// by asking again: Graph was busy, or the answer says nothing at all.
// Anything else is Graph's verdict on that person (see Gone for the one
// kind that has its own name) and asking again will get the same answer.
func (r MemberResult) Transient() bool {
	if r.Added() {
		return false
	}
	code := strings.ToLower(r.Code)
	for _, s := range []string{"throttl", "toomany", "429", "timeout", "unavailable", "internal", "generalexception", "unknown", "500", "502", "503", "504"} {
		if strings.Contains(code, s) {
			return true
		}
	}
	return code == ""
}

// Err returns the per-user failure as an error, or nil if the user was
// added. It carries the user's ID and Graph's code, never a name.
func (r MemberResult) Err() error {
	if r.Added() {
		return nil
	}
	return fmt.Errorf("user %s: %s: %s", r.UserID, r.Code, r.Message)
}

// AddTeamMembers adds users to a team as plain members in one request
// (POST /teams/{id}/members/add). Graph accepts at most MaxMembersPerAdd
// users per request; the caller splits. Single and bulk adds share a limit
// of four requests per minute per team, which is why real teams need this
// call rather than one request per person.
//
// Graph answers 200 when every user was added and 207 when some were not;
// either way the result has one entry per user Graph reported on, in the
// order Graph listed them. A user Graph did not report on at all is absent
// from the result, and the caller must not take that for success.
//
// The request is not repeated after an ambiguous failure: what Graph does
// with someone who is already a member is not documented, so the caller
// reads the membership back before deciding what is still to add.
func (c *Client) AddTeamMembers(ctx context.Context, teamID string, userIDs []string) ([]MemberResult, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}
	if len(userIDs) > MaxMembersPerAdd {
		return nil, usage("Too many members in one request",
			fmt.Sprintf("%d users were passed; Graph accepts %d per request.", len(userIDs), MaxMembersPerAdd),
			"This is a bug in Taut; please report it.")
	}
	values := make([]map[string]any, 0, len(userIDs))
	for _, id := range userIDs {
		values = append(values, map[string]any{
			"@odata.type":     "microsoft.graph.aadUserConversationMember",
			"roles":           []string{},
			"user@odata.bind": c.bind("users", id),
		})
	}
	var out struct {
		Value []struct {
			UserID string `json:"userId"`
			Error  *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"value"`
	}
	err := c.do(ctx, request{
		method: "POST", path: teamPath(teamID) + "/members/add", body: map[string]any{"values": values},
		permission: permTeamMember,
	}, &out)
	if err != nil {
		return nil, err
	}
	results := make([]MemberResult, 0, len(out.Value))
	for _, v := range out.Value {
		r := MemberResult{UserID: v.UserID}
		if v.Error != nil {
			r.Code, r.Message = v.Error.Code, v.Error.Message
			if r.Code == "" && r.Message == "" {
				r.Code = "unknown"
			}
		}
		results = append(results, r)
	}
	return results, nil
}

// ChannelMember is one member of a channel.
type ChannelMember = TeamMember

// ChannelMembers iterates the members of a channel. For a private or shared
// channel these are the channel's own members; for a standard channel
// Graph lists the team's.
func (c *Client) ChannelMembers(ctx context.Context, teamID, channelID string) iter.Seq2[ChannelMember, error] {
	return list[ChannelMember](ctx, c, request{method: "GET", path: channelPath(teamID, channelID) + "/members", permission: permChanMember})
}

// AddChannelMember adds a user to a private or shared channel as a member
// or an owner. The user must already be a member of the team: if not, the
// error matches ErrNotTeamMember (IsNotTeamMember). A deleted or blocked
// account yields an error matching ErrNotFound.
//
// Graph documents no bulk add for channels. Like AddTeamMember the POST is
// not repeated after an ambiguous failure; read the members back first.
func (c *Client) AddChannelMember(ctx context.Context, teamID, channelID, userID string, owner bool) error {
	roles := []string{}
	if owner {
		roles = []string{"owner"}
	}
	body := map[string]any{
		"@odata.type":     "#microsoft.graph.aadUserConversationMember",
		"roles":           roles,
		"user@odata.bind": c.bind("users", userID),
	}
	classify := func(e *APIError) *fail.Error {
		if isNotTeamMember(e) {
			return notTeamMember(e, "Only members of the team can be added to one of its private channels.")
		}
		return nil
	}
	return c.do(ctx, request{
		method: "POST", path: channelPath(teamID, channelID) + "/members", body: body,
		permission: permChanMember, classify: classify,
	}, nil)
}

// isNotTeamMember recognises Graph refusing a private-channel member who is
// not in the team. The status and wording of that refusal are not
// documented; a 400 or 403 that says the user is not a member of the team
// is taken to be it.
func isNotTeamMember(e *APIError) bool {
	if e.Status != http.StatusBadRequest && e.Status != http.StatusForbidden {
		return false
	}
	msg := strings.ToLower(e.Message)
	return strings.Contains(msg, "not a member of the team") || strings.Contains(msg, "not a team member") ||
		strings.Contains(msg, "must be a member of the team")
}

func notTeamMember(e *APIError, why string) *fail.Error {
	e.kind = ErrNotTeamMember
	return fail.Wrap(e, "graph.not_team_member",
		"The user is not a member of the team",
		why+" "+e.detail()+e.ref(),
		"Add the user to the team first, then try again. `taut finalize` adds a private channel's members to the team before the channel.")
}

// --- messages ---------------------------------------------------------------

// ImportTarget says where a message is imported: a channel of a team
// (TeamID and ChannelID) or a chat (ChatID alone).
type ImportTarget struct {
	TeamID    string
	ChannelID string
	// ReplyToID is the parent message's ID for a reply; empty for a
	// top-level message. Chats have no replies.
	ReplyToID string
	// ChatID names a chat instead of a channel.
	ChatID string
}

// IsChat reports whether the target is a chat.
func (t ImportTarget) IsChat() bool { return t.ChatID != "" }

// valid reports whether the target names exactly one place.
func (t ImportTarget) valid() bool {
	if t.IsChat() {
		return t.TeamID == "" && t.ChannelID == "" && t.ReplyToID == ""
	}
	return t.TeamID != "" && t.ChannelID != ""
}

// Place is the kind of conversation, "channel" or "chat", for messages to
// the operator.
func (t ImportTarget) Place() string {
	if t.IsChat() {
		return "chat"
	}
	return "channel"
}

// Identity is the author of an imported message: an Entra user.
type Identity struct {
	UserID      string
	DisplayName string
}

// Mention is one @mention. The HTML must contain a matching
// <at id="ID">Text</at> tag.
type Mention struct {
	ID     int
	Text   string
	UserID string
}

// ImportMessage is a message to import with its original time and author.
type ImportMessage struct {
	// CreatedAt is sent in UTC with millisecond precision; anything finer
	// is truncated. It must be unique within the channel.
	CreatedAt time.Time
	From      Identity
	HTML      string
	Mentions  []Mention
	// Attachments are files already uploaded to SharePoint. For each, HTML
	// must contain <attachment id="ID"></attachment>.
	Attachments []Attachment
	// HostedContents are inline images sent with the message. They get the
	// temporary IDs 1, 2, … in slice order, and HTML must reference each
	// as <img src="../hostedContents/N/$value">; the caller writes those
	// references itself.
	HostedContents []HostedContent
	// Reactions are sent in the message's reactions collection, each dated
	// CreatedAt: a Slack export does not say when a reaction was given.
	// Graph's import guide puts up to MaxImportReactions in scope.
	Reactions []Reaction
}

// MaxImportReactions is the number of reactions Microsoft's import guide
// lists as in scope for one imported message.
const MaxImportReactions = 250

// Reaction is one person's reaction to a message.
type Reaction struct {
	// Type is the chatMessageReaction reactionType: a Unicode emoji.
	Type string
	// UserID is the Entra object ID of the person who reacted.
	UserID string
}

// Attachment references an uploaded file from a message.
type Attachment struct {
	// ID is the file's DriveItem.ETagGUID().
	ID   string
	Name string
	// ContentURL is the file's DriveItem.WebDavURL. Sharing links are not
	// accepted by Graph during migration.
	ContentURL string
}

// HostedContent is an inline image carried inside the message.
type HostedContent struct {
	// ContentType must be an image type, such as "image/png".
	ContentType string
	Data        []byte
}

// ChatMessage is a message as Graph returns it.
type ChatMessage struct {
	ID string
	// ReplyToID is empty for top-level messages.
	ReplyToID       string
	CreatedAt       time.Time
	FromUserID      string
	FromDisplayName string
	// ContentType is "html" or "text". Content is customer data: do not
	// log it.
	ContentType string
	Content     string
	// Reactions are the message's reactions, in Graph's order.
	Reactions []Reaction
}

// UnmarshalJSON reads Graph's chatMessage shape.
func (m *ChatMessage) UnmarshalJSON(data []byte) error {
	var w struct {
		ID        string    `json:"id"`
		ReplyToID *string   `json:"replyToId"`
		Created   time.Time `json:"createdDateTime"`
		From      *struct {
			User *struct {
				ID          string `json:"id"`
				DisplayName string `json:"displayName"`
			} `json:"user"`
		} `json:"from"`
		Body struct {
			ContentType string `json:"contentType"`
			Content     string `json:"content"`
		} `json:"body"`
		Reactions []struct {
			Type string `json:"reactionType"`
			User *struct {
				User *struct {
					ID string `json:"id"`
				} `json:"user"`
			} `json:"user"`
		} `json:"reactions"`
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	*m = ChatMessage{ID: w.ID, CreatedAt: w.Created, ContentType: w.Body.ContentType, Content: w.Body.Content}
	if w.ReplyToID != nil {
		m.ReplyToID = *w.ReplyToID
	}
	if w.From != nil && w.From.User != nil {
		m.FromUserID, m.FromDisplayName = w.From.User.ID, w.From.User.DisplayName
	}
	for _, r := range w.Reactions {
		re := Reaction{Type: r.Type}
		if r.User != nil && r.User.User != nil {
			re.UserID = r.User.User.ID
		}
		m.Reactions = append(m.Reactions, re)
	}
	return nil
}

// MessageTime formats t the way imports are sent: UTC, exactly millisecond
// precision.
func MessageTime(t time.Time) string {
	return t.UTC().Truncate(time.Millisecond).Format("2006-01-02T15:04:05.000Z")
}

// MessageIDForTime is the ID Teams is observed (not documented) to give a
// message created at t: the timestamp in Unix milliseconds.
func MessageIDForTime(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }

func messagesPath(target ImportTarget) string {
	if target.IsChat() {
		return chatPath(target.ChatID) + "/messages"
	}
	p := channelPath(target.TeamID, target.ChannelID) + "/messages"
	if target.ReplyToID != "" {
		p += "/" + url.PathEscape(target.ReplyToID) + "/replies"
	}
	return p
}

// ImportMessage imports one message, or a reply when target.ReplyToID is
// set, into a channel in migration mode.
//
// Retry semantics differ from every other POST. Graph has no idempotency
// key, but it rejects a second message with the same millisecond
// createdDateTime in a channel with 409. That makes an import safe to send
// again after an ambiguous failure, so ImportMessage retries on 429, on
// 500/502/503/504 and on transport errors, within the client's retry
// budget. No other 4xx is retried.
//
// A 409 is returned as graph.conflict (IsConflict). It means a message with
// this timestamp already exists: either an earlier attempt of this very
// import was committed and its response lost, or a different message holds
// the timestamp. The caller must reconcile by reading the message back
// (FindMessageAt) and comparing it with what it meant to import.
//
// Other notable errors: graph.not_in_migration (IsNotInMigration),
// graph.migration_not_owner, graph.payload_too_large (body size),
// graph.invalid_author, graph.attachment_invalid,
// graph.hosted_content_invalid, graph.hosted_content_too_large and
// graph.reaction_invalid (IsReactionInvalid).
// Errors carry IDs, sizes and status codes only, never message content.
func (c *Client) ImportMessage(ctx context.Context, target ImportTarget, msg ImportMessage) (ChatMessage, error) {
	if !target.valid() || msg.CreatedAt.IsZero() || msg.From.UserID == "" {
		return ChatMessage{}, usage("A message to import is incomplete",
			"The team and channel ID (or the chat ID), the creation time or the author is missing.",
			"This is a bug in Taut; please report it.")
	}
	created := MessageTime(msg.CreatedAt)
	body := map[string]any{
		"createdDateTime": created,
		"from": map[string]any{"user": map[string]any{
			"id": msg.From.UserID, "displayName": msg.From.DisplayName, "userIdentityType": "aadUser",
		}},
		"body": map[string]any{"contentType": "html", "content": msg.HTML},
	}
	if len(msg.Mentions) > 0 {
		mentions := make([]map[string]any, 0, len(msg.Mentions))
		for _, mn := range msg.Mentions {
			mentions = append(mentions, map[string]any{
				"id": mn.ID, "mentionText": mn.Text,
				"mentioned": map[string]any{"user": map[string]any{
					"id": mn.UserID, "displayName": mn.Text, "userIdentityType": "aadUser",
				}},
			})
		}
		body["mentions"] = mentions
	}
	if len(msg.Attachments) > 0 {
		attachments := make([]map[string]any, 0, len(msg.Attachments))
		for _, a := range msg.Attachments {
			attachments = append(attachments, map[string]any{
				"id": a.ID, "contentType": "reference", "contentUrl": a.ContentURL, "name": a.Name,
			})
		}
		body["attachments"] = attachments
	}
	var hostedBytes int
	if len(msg.HostedContents) > 0 {
		hosted := make([]map[string]any, 0, len(msg.HostedContents))
		for i, h := range msg.HostedContents {
			hostedBytes += len(h.Data)
			hosted = append(hosted, map[string]any{
				"@microsoft.graph.temporaryId": strconv.Itoa(i + 1),
				"contentBytes":                 base64.StdEncoding.EncodeToString(h.Data),
				"contentType":                  h.ContentType,
			})
		}
		body["hostedContents"] = hosted
	}
	if len(msg.Reactions) > 0 {
		// The chatMessage resource's reactions collection, as the reference
		// describes a chatMessageReaction: the type, when it was given, and
		// the user in an identity set whose other members stay unset.
		reactions := make([]map[string]any, 0, len(msg.Reactions))
		for _, r := range msg.Reactions {
			if r.Type == "" || r.UserID == "" {
				return ChatMessage{}, usage("A reaction to import is incomplete",
					"Its type or the person who gave it is missing.",
					"This is a bug in Taut; please report it.")
			}
			reactions = append(reactions, map[string]any{
				"reactionType":    r.Type,
				"createdDateTime": created,
				"user":            map[string]any{"user": map[string]any{"id": r.UserID, "userIdentityType": "aadUser"}},
			})
		}
		body["reactions"] = reactions
	}
	place := target.Place()
	where := fmt.Sprintf("team %s, channel %s, createdDateTime %s", target.TeamID, target.ChannelID, created)
	if target.IsChat() {
		where = fmt.Sprintf("chat %s, createdDateTime %s", target.ChatID, created)
	}
	if target.ReplyToID != "" {
		where += ", reply to " + target.ReplyToID
	}
	classify := func(e *APIError) *fail.Error {
		msgText := strings.ToLower(e.Message)
		switch {
		case e.Status == http.StatusConflict:
			return fail.Wrap(e, "graph.conflict",
				"A message with this timestamp already exists in the "+place,
				fmt.Sprintf("Graph refused the import after %d attempt(s) (%s).%s", e.Attempts, where, e.ref()),
				"Run `taut migrate` again: it reads the existing message back, adopts it if it matches what was sent, and reports a mismatch otherwise.")
		case isNotOwner(e):
			return notOwner(e)
		case e.Status == http.StatusForbidden && strings.Contains(msgText, "migration mode"):
			e.kind = ErrNotInMigration
			return fail.Wrap(e, "graph.not_in_migration",
				"The "+place+" is not in migration mode, so messages cannot be imported",
				"Graph only accepts messages with an original time and author while the "+place+" is in migration mode ("+where+")."+e.ref(),
				"Run `taut migrate` again; it starts migration mode before importing. If the "+place+" was already completed with `taut finalize`, re-run `taut migrate` for that "+place+" to re-enter migration mode.")
		case len(msg.Reactions) > 0 && e.Status == http.StatusBadRequest && strings.Contains(msgText, "reaction"):
			e.kind = ErrReactionInvalid
			return fail.Wrap(e, "graph.reaction_invalid",
				"Microsoft Teams did not accept the reactions of a message",
				fmt.Sprintf("The message carries %d reaction(s) (%s). %s", len(msg.Reactions), where, e.detail()+e.ref()),
				"Taut sends the message again with its reactions summarized in a line of text. To summarize all reactions that way, set content.reactions to footer.")
		case e.Status == http.StatusBadRequest && strings.Contains(msgText, "hosted content") &&
			(strings.Contains(msgText, "too large") || strings.Contains(msgText, "exceeds the limit")):
			return fail.Wrap(e, "graph.hosted_content_too_large",
				"An inline image is too large for Microsoft Teams",
				fmt.Sprintf("The message carries %d inline image(s) totalling %d bytes (%s).%s", len(msg.HostedContents), hostedBytes, where, e.ref()),
				"Lower the inline image size cap in the config file so large images are attached as files instead, then run `taut migrate` again.")
		case e.Status == http.StatusBadRequest && (strings.Contains(msgText, "hosted content") || strings.Contains(msgText, "hostedcontents")):
			return fail.Wrap(e, "graph.hosted_content_invalid",
				"Microsoft Teams did not accept an inline image of a message",
				fmt.Sprintf("The message carries %d inline image(s) totalling %d bytes (%s). %s", len(msg.HostedContents), hostedBytes, where, e.detail()+e.ref()),
				"This is likely a bug in Taut. Set inline images to be attached as files in the config file to work around it, and report the request-id shown above.")
		case e.Status == http.StatusBadRequest && strings.Contains(msgText, "attachment"):
			return fail.Wrap(e, "graph.attachment_invalid",
				"Microsoft Teams did not accept a file attachment of a message",
				fmt.Sprintf("The message references %d file(s) (%s). %s", len(msg.Attachments), where, e.detail()+e.ref()),
				"The file may have been moved or deleted in SharePoint after upload. Run `taut migrate` again to upload it anew; if it persists, report the request-id shown above.")
		case e.Status == http.StatusRequestEntityTooLarge,
			e.Status == http.StatusBadRequest && (strings.Contains(msgText, "too large") || strings.Contains(msgText, "exceeds the limit")):
			return fail.Wrap(e, "graph.payload_too_large",
				"A message is too large for Microsoft Teams",
				fmt.Sprintf("The message body is %d bytes (%s).%s", len(msg.HTML), where, e.ref()),
				"Lower the message size limit in the config file so long messages are split or attached as a file, then run `taut migrate` again.")
		case target.IsChat() && e.Status == http.StatusBadRequest && strings.Contains(msgText, "mention") && strings.Contains(msgText, "member"):
			e.kind = ErrMentionInvalid
			return fail.Wrap(e, "graph.mention_invalid",
				"Microsoft Teams did not accept an @mention of a message",
				fmt.Sprintf("The message mentions %d people (%s). %s", len(msg.Mentions), where, e.detail()+e.ref()),
				"Taut sends the message again with the mention as plain text.")
		case e.Status == http.StatusBadRequest && strings.Contains(msgText, "from") && strings.Contains(msgText, "user"):
			return fail.Wrap(e, "graph.invalid_author",
				"The message author is not a user in the Microsoft 365 tenant",
				fmt.Sprintf("Graph did not accept user %s as the author (%s).%s", msg.From.UserID, where, e.ref()),
				"Fix the identity mapping for this user with `taut map` (map them to an existing Entra user or to the service account), then run `taut migrate` again.")
		}
		return nil
	}
	var out ChatMessage
	err := c.do(ctx, request{
		method: "POST", path: messagesPath(target), body: body,
		permission: permMigrate, retryAmbiguous: true, classify: classify,
	}, &out)
	return out, err
}

// GetMessage fetches a top-level message by ID. A missing message yields an
// error matching ErrNotFound.
func (c *Client) GetMessage(ctx context.Context, teamID, channelID, messageID string) (ChatMessage, error) {
	var m ChatMessage
	return m, c.do(ctx, request{
		method: "GET", path: channelPath(teamID, channelID) + "/messages/" + url.PathEscape(messageID), permission: permMessageRead,
	}, &m)
}

// GetReply fetches a reply by its parent's ID and its own.
func (c *Client) GetReply(ctx context.Context, teamID, channelID, parentID, replyID string) (ChatMessage, error) {
	var m ChatMessage
	return m, c.do(ctx, request{
		method: "GET", permission: permMessageRead,
		path: channelPath(teamID, channelID) + "/messages/" + url.PathEscape(parentID) + "/replies/" + url.PathEscape(replyID),
	}, &m)
}

func (c *Client) listMessages(ctx context.Context, path string, pageSize int) iter.Seq2[ChatMessage, error] {
	var query url.Values
	if pageSize > 0 {
		query = url.Values{"$top": {strconv.Itoa(min(pageSize, maxMessagePage))}}
	}
	return list[ChatMessage](ctx, c, request{method: "GET", path: path, query: query, permission: permMessageRead})
}

// Messages iterates the top-level messages of a channel, newest first.
// pageSize is $top (at most 50, Graph's limit); zero lets Graph choose.
func (c *Client) Messages(ctx context.Context, teamID, channelID string, pageSize int) iter.Seq2[ChatMessage, error] {
	return c.listMessages(ctx, channelPath(teamID, channelID)+"/messages", pageSize)
}

// Replies iterates the replies to one top-level message.
func (c *Client) Replies(ctx context.Context, teamID, channelID, parentID string, pageSize int) iter.Seq2[ChatMessage, error] {
	return c.listMessages(ctx, channelPath(teamID, channelID)+"/messages/"+url.PathEscape(parentID)+"/replies", pageSize)
}

// FindMessageAt finds the message (or, with target.ReplyToID set, the
// reply) whose createdDateTime is createdAt to the millisecond. It is the
// read-back step after ImportMessage returned a conflict.
//
// It first tries the ID Teams is observed to assign, the timestamp in Unix
// milliseconds. Because that ID format is not documented, a miss falls back
// to scanning the message (or reply) list for the timestamp, which costs
// one request per 50 messages.
func (c *Client) FindMessageAt(ctx context.Context, target ImportTarget, createdAt time.Time) (ChatMessage, bool, error) {
	want := createdAt.UTC().Truncate(time.Millisecond)
	if target.IsChat() {
		return c.findChatMessageAt(ctx, target.ChatID, want)
	}
	id := MessageIDForTime(want)
	var (
		m   ChatMessage
		err error
		seq iter.Seq2[ChatMessage, error]
	)
	if target.ReplyToID == "" {
		m, err = c.GetMessage(ctx, target.TeamID, target.ChannelID, id)
		seq = c.Messages(ctx, target.TeamID, target.ChannelID, maxMessagePage)
	} else {
		m, err = c.GetReply(ctx, target.TeamID, target.ChannelID, target.ReplyToID, id)
		seq = c.Replies(ctx, target.TeamID, target.ChannelID, target.ReplyToID, maxMessagePage)
	}
	switch {
	case err == nil && m.CreatedAt.Truncate(time.Millisecond).Equal(want):
		return m, true, nil
	case err != nil && !IsNotFound(err):
		return ChatMessage{}, false, err
	}
	for m, err := range seq {
		if err != nil {
			return ChatMessage{}, false, err
		}
		if m.CreatedAt.Truncate(time.Millisecond).Equal(want) {
			return m, true, nil
		}
	}
	return ChatMessage{}, false, nil
}

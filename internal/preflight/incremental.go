package preflight

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/ui/sanitize"
)

// An incremental pass imports, from a newer export of the same workspace,
// what the passes before it did not. What "the same workspace", "the same
// place in Teams" and "not yet imported" mean is decided here, in one
// place: `taut preflight --incremental` reports it and the plan stage of the
// pass enforces it.

// Base is the pass an incremental pass would follow.
type Base struct {
	// Ledger is what that pass recorded as being in Teams.
	Ledger store.Ledger
	// TenantID is the Entra tenant that pass wrote to.
	TenantID string
	// Keys calls fn for every Slack message of a conversation that has a
	// post in Teams, with its edit marker (nil when not known). Nil when
	// only the layout is being checked.
	Keys func(ctx context.Context, slackID string, fn func(slackTS string, edited *string) error) error
}

// Continuation is how a new pass lines up with the ledger of the pass
// before it.
type Continuation struct {
	Blockers []Finding
	Warnings []Finding
	// Continued are the conversations of the new pass's plan that an
	// earlier pass already put into Teams, by Slack ID. The pass imports
	// into the same channel or chat.
	Continued map[string]store.LedgerChannel
	// Dormant are conversations an earlier pass put into Teams that the
	// new pass's plan does not contain: gone from Slack, or a direct
	// message that no longer qualifies. Nothing is imported into them.
	// They stay in the ledger so that they are still finalized.
	Dormant []store.LedgerChannel
	// ExportWorkspace is the workspace ID the export states, if any, and
	// Workspace the comparison with the earlier passes.
	ExportWorkspace string
	Workspace       slack.WorkspaceMatch
}

func (c *Continuation) block(code, msg, next string) {
	c.Blockers = append(c.Blockers, Finding{Code: code, Message: msg, Next: next})
}

func (c *Continuation) warn(code, msg, next string) {
	c.Warnings = append(c.Warnings, Finding{Code: code, Message: msg, Next: next})
}

// inTeams reports whether an earlier pass left the conversation in Teams.
// A chat Microsoft refused (skipped) is not; the new pass tries it afresh.
func inTeams(c store.LedgerChannel) bool {
	if !c.InTeams {
		return false
	}
	switch c.State {
	case store.ChannelCreated, store.ChannelMigrating, store.ChannelCompleted:
		return true
	}
	return false
}

// startOver is the way out of every refusal here: a run that does not build
// on the earlier passes.
const startOver = "The alternative is `taut migrate --new`, which starts over without the ledger of the earlier passes; with a different export or config file it can import messages a second time."

// Continue lines a new pass up with its base. lay is the pass's channel
// layout, chats the Slack IDs of the direct-message conversations it would
// migrate, and channels and users the export's.
//
// The rules exist so that a pass can only add to what is there:
//
//   - The base must have finished importing. A pass that is still running
//     is resumed, not followed.
//   - The export must be of the same workspace, and the config file must
//     name the same tenant.
//   - A conversation that is in Teams stays where it is. The config file
//     may not move it to another team or leave it out while the export
//     still has it, and a team that received channels must still be in the
//     config file.
func Continue(m *config.Config, lay *PlanLayout, chats []string, channels []slack.Channel, users []slack.User, base Base) *Continuation {
	c := &Continuation{Continued: map[string]store.LedgerChannel{}, ExportWorkspace: slack.WorkspaceID(users)}
	l := base.Ledger

	if l.Status != store.RunCompleted && l.Status != store.RunFinalized {
		c.block("incremental.base_not_finished",
			fmt.Sprintf("The pass before this one (run %s) has not finished importing: it is %s.", sanitize.Text(l.RunID), sanitize.Text(string(l.Status))),
			"Run `taut migrate` to finish it. An incremental pass can only follow a pass that is complete.")
	}
	if base.TenantID != "" && !strings.EqualFold(base.TenantID, m.Target.Entra.TenantID) {
		c.block("incremental.tenant_changed",
			fmt.Sprintf("The earlier passes imported into tenant %s, and the config file now names tenant %s.", sanitize.Text(base.TenantID), sanitize.Text(m.Target.Entra.TenantID)),
			"Restore target.entra.tenantId. An incremental pass adds to the teams the earlier passes created, which exist in that tenant only.")
	}

	// --- the same workspace ---------------------------------------------
	exported := make(map[string]slack.Channel, len(channels))
	exportedIDs := make([]string, 0, len(channels))
	for _, ch := range channels {
		exported[ch.ID] = ch
		exportedIDs = append(exportedIDs, ch.ID)
	}
	var earlier []string
	for _, lc := range l.Channels {
		if inTeams(lc) {
			earlier = append(earlier, lc.SlackID)
		}
	}
	c.Workspace = slack.SameWorkspace(l.WorkspaceID, earlier, c.ExportWorkspace, exportedIDs)
	if !c.Workspace.Same {
		why := fmt.Sprintf("None of the %s the earlier passes migrated is in this export", count(c.Workspace.Earlier, "conversation", "conversations"))
		if l.WorkspaceID != "" && c.ExportWorkspace != "" && l.WorkspaceID != c.ExportWorkspace {
			why += fmt.Sprintf(", and it is of workspace %s where they were of %s", sanitize.Text(c.ExportWorkspace), sanitize.Text(l.WorkspaceID))
		}
		c.block("incremental.workspace_mismatch",
			"This export is not of the Slack workspace the earlier passes migrated. "+why+".",
			"Point source.slack.export at a newer export of the same workspace. Importing another workspace's export into these teams would mix two histories.")
		// Everything below compares conversation by conversation, which
		// means nothing across two workspaces.
		return c
	}

	// --- the same places in Teams ------------------------------------------
	teamInConfig := map[string]bool{}
	for _, t := range m.Target.Teams {
		teamInConfig[strings.ToLower(t.Name)] = true
	}
	assigned := map[string]Assignment{}
	for _, a := range lay.Assignments {
		assigned[a.Channel.ID] = a
	}
	planned := map[string]bool{}
	for _, id := range chats {
		planned[id] = true
	}

	var moved, dropped, renamed []string
	teamsGone := map[string]bool{}
	for _, lc := range l.Channels {
		if !inTeams(lc) {
			continue
		}
		if store.IsChatKind(lc.Kind) {
			if planned[lc.SlackID] {
				c.Continued[lc.SlackID] = lc
			} else {
				c.Dormant = append(c.Dormant, lc)
			}
			continue
		}
		name := "#" + sanitize.Text(lc.SlackName)
		a, ok := assigned[lc.SlackID]
		switch {
		case ok && !strings.EqualFold(m.Target.Teams[a.Team].Name, lc.Team):
			moved = append(moved, fmt.Sprintf("%s (in %q, now mapped to %q)", name, sanitize.Text(lc.Team), sanitize.Text(m.Target.Teams[a.Team].Name)))
		case ok:
			c.Continued[lc.SlackID] = lc
			if cp := lay.Teams[a.Team].Channels[a.Slot]; cp.TargetName != lc.TargetName {
				renamed = append(renamed, fmt.Sprintf("%s (stays %q)", name, sanitize.Text(lc.TargetName)))
			}
		case !teamInConfig[strings.ToLower(lc.Team)]:
			teamsGone[lc.Team] = true
		default:
			if _, still := exported[lc.SlackID]; still {
				dropped = append(dropped, name)
			} else {
				c.Dormant = append(c.Dormant, lc)
			}
		}
	}
	if len(teamsGone) > 0 {
		names := make([]string, 0, len(teamsGone))
		for n := range teamsGone {
			names = append(names, fmt.Sprintf("%q", sanitize.Text(n)))
		}
		sort.Strings(names)
		c.block("incremental.team_dropped",
			fmt.Sprintf("%s received channels in an earlier pass and %s no longer in the config file: %s.",
				count(len(names), "team", "teams"), plural(len(names), "is", "are"), strings.Join(names, ", ")),
			"Restore target.teams[] for it, with the same name. Its channels are still in the ledger and must be finalized with the rest. "+startOver)
	}
	if len(moved) > 0 {
		c.block("incremental.channel_moved",
			fmt.Sprintf("%s already in Teams %s mapped to a different team now: %s.", count(len(moved), "channel", "channels"), plural(len(moved), "is", "are"), listSome(moved)),
			"Restore the mapping in target.teams[].channels (`taut map` shows it). A channel cannot be moved between teams, and importing its new messages elsewhere would split its history. "+startOver)
	}
	if len(dropped) > 0 {
		c.block("incremental.channel_dropped",
			fmt.Sprintf("%s already in Teams %s in this export but no longer selected by the config file: %s.", count(len(dropped), "channel", "channels"), plural(len(dropped), "is", "are"), listSome(dropped)),
			"Select them again in target.teams[].channels (`taut map` shows the selection). A pass must keep the channels the earlier passes migrated, or their later messages are silently left behind. "+startOver)
	}
	if len(renamed) > 0 {
		c.warn("incremental.channel_renamed",
			fmt.Sprintf("%s already in Teams %s a different name in the config file now: %s.", count(len(renamed), "channel", "channels"), plural(len(renamed), "has", "have"), listSome(renamed)),
			"Taut imports into the existing channel and does not rename it. Rename it in Teams if you want the new name.")
	}
	sort.Slice(c.Dormant, func(i, j int) bool { return c.Dormant[i].SlackID < c.Dormant[j].SlackID })
	return c
}

// listSome joins the first few items and says how many more there are, so
// a finding about hundreds of channels stays readable.
func listSome(items []string) string {
	const n = 5
	if len(items) <= n {
		return strings.Join(items, ", ")
	}
	return strings.Join(items[:n], ", ") + fmt.Sprintf(" and %d more", len(items)-n)
}

// IncrementalCounts is what a ledger already holds.
type IncrementalCounts struct {
	// Messages are Slack messages with a post in Teams.
	Messages int `json:"messages"`
	Files    int `json:"files"`
	// Conversations are the channels and chats in Teams.
	Conversations int `json:"conversations"`
	People        int `json:"people"`
}

// IncrementalNew is what a pass would add.
type IncrementalNew struct {
	Messages int `json:"messages"`
	Replies  int `json:"replies"`
	// RepliesToMigrated are the new replies whose thread an earlier pass
	// imported.
	RepliesToMigrated int   `json:"repliesToMigratedThreads"`
	Files             int   `json:"files"`
	FileBytes         int64 `json:"fileBytes"`
	Channels          int   `json:"channels"`
	Chats             int   `json:"chats,omitempty"`
	People            int   `json:"people"`
}

// IncrementalChannel is one conversation of an incremental pass.
type IncrementalChannel struct {
	SlackID string `json:"slackId"`
	// SlackName is empty for a direct message.
	SlackName string `json:"slackName,omitempty"`
	Kind      string `json:"kind"`
	// Continued is false for a conversation no earlier pass imported.
	Continued bool `json:"continued"`
	// State is the conversation's state in the ledger: "migrating" or
	// "completed". Empty when not continued.
	State   string `json:"state,omitempty"`
	Already int    `json:"already"`
	New     int    `json:"new"`
	Edited  int    `json:"edited,omitempty"`
	Deleted int    `json:"deleted,omitempty"`
	// Reopens is true when the conversation was finalized and has new
	// messages: the pass puts it back into migration mode.
	Reopens bool `json:"reopens,omitempty"`
}

// IncrementalReport is the part of a preflight report that compares the
// export with what earlier passes already imported.
type IncrementalReport struct {
	// Pass is the number the new pass would have.
	Pass int `json:"pass"`
	// BaseRun is the pass it follows, and BaseStatus that run's status:
	// "completed" (still in migration mode) or "finalized".
	BaseRun    string `json:"baseRun"`
	BaseStatus string `json:"baseStatus"`
	// Workspace is the export's workspace ID and LedgerWorkspace the
	// earlier passes'; either may be empty. SharedConversations is how
	// many of the earlier conversations the export lists.
	Workspace           string `json:"workspace,omitempty"`
	LedgerWorkspace     string `json:"ledgerWorkspace,omitempty"`
	SameWorkspace       bool   `json:"sameWorkspace"`
	SharedConversations int    `json:"sharedConversations"`

	Already IncrementalCounts `json:"already"`
	New     IncrementalNew    `json:"new"`
	// Edited and Deleted count messages already in Teams that were edited
	// or deleted in Slack since. The import API cannot change or remove an
	// imported message, so they stay as they were imported.
	Edited  int `json:"editedSince"`
	Deleted int `json:"deletedSince"`
	// Reopened counts the finalized channels, and ReopenedChats the
	// finalized chats, that the pass puts back into migration mode.
	Reopened      int `json:"reopenedChannels"`
	ReopenedChats int `json:"reopenedChats,omitempty"`
	// Dormant counts earlier conversations that are not in this export or
	// plan; nothing changes for them.
	Dormant  int                  `json:"dormant,omitempty"`
	Channels []IncrementalChannel `json:"channels"`
}

// ledgerKeys loads the posted Slack timestamps of one conversation.
func ledgerKeys(ctx context.Context, base *Base, slackID string) (map[string]*string, error) {
	keys := map[string]*string{}
	err := base.Keys(ctx, slackID, func(ts string, edited *string) error {
		keys[ts] = edited
		return nil
	})
	return keys, err
}

// incremental finishes the incremental part of a report once every
// conversation has been scanned.
func (r *Report) incremental(m *config.Config, cont *Continuation, base *Base, users []slack.User) {
	inc := r.Incremental
	l := base.Ledger
	inc.Pass, inc.BaseRun, inc.BaseStatus = l.Pass+1, l.RunID, string(l.Status)
	inc.Workspace, inc.LedgerWorkspace = cont.ExportWorkspace, l.WorkspaceID
	inc.SameWorkspace, inc.SharedConversations = cont.Workspace.Same, cont.Workspace.Shared
	inc.Already.Messages, inc.Already.Files, inc.Already.People = l.Posted, l.Files, len(l.Users)
	for _, lc := range l.Channels {
		if inTeams(lc) {
			inc.Already.Conversations++
		}
	}
	inc.Dormant = len(cont.Dormant)
	known := make(map[string]bool, len(l.Users))
	for _, id := range l.Users {
		known[id] = true
	}
	for _, u := range users {
		if !known[u.ID] {
			inc.New.People++
		}
	}
	if inc.Channels == nil {
		inc.Channels = []IncrementalChannel{}
	}
	var reopened []string
	finalizedNew := false
	for _, ch := range inc.Channels {
		chat := store.IsChatKind(ch.Kind)
		switch {
		case !ch.Continued && chat:
			inc.New.Chats++
		case !ch.Continued:
			inc.New.Channels++
		}
		inc.Edited += ch.Edited
		inc.Deleted += ch.Deleted
		switch {
		case !ch.Reopens:
		case chat:
			inc.ReopenedChats++
		default:
			inc.Reopened++
			reopened = append(reopened, "#"+sanitize.Text(ch.SlackName))
		}
		if !ch.Continued && !chat && l.Status == store.RunFinalized {
			finalizedNew = true
		}
	}

	r.Blockers = append(r.Blockers, cont.Blockers...)
	r.Warnings = append(r.Warnings, cont.Warnings...)

	// What the import API allows after a finalize. startMigration puts a
	// channel or chat that completed migration back into migration mode;
	// it exists in the global cloud only, and the legacy strategy does not
	// use it.
	legacy := m.Load.Strategy == config.StrategyLegacy
	switch {
	case legacy && inc.Reopened > 0:
		r.block("incremental.legacy_finalized",
			fmt.Sprintf("%s finalized by an earlier pass %s new messages, and the legacy load strategy cannot import into a finalized channel: %s.",
				count(inc.Reopened, "channel", "channels"), plural(inc.Reopened, "has", "have"), listSome(reopened)),
			"A finalized channel is put back into migration mode with startMigration, which Microsoft offers in the global cloud only and which load.strategy: legacy does not use. In the global cloud, set load.strategy: modern. In a national cloud, run incremental passes before `taut finalize`, not after.")
	case legacy && finalizedNew:
		r.block("incremental.legacy_finalized",
			"The earlier passes are finalized, and the legacy load strategy cannot add a channel to a team that has left migration mode.",
			"With load.strategy: legacy a channel can only receive history if it is created in migration mode, in a team that is in migration mode. In the global cloud, set load.strategy: modern. In a national cloud, run incremental passes before `taut finalize`, not after.")
	case inc.Reopened+inc.ReopenedChats > 0:
		what := count(inc.Reopened, "finalized channel", "finalized channels")
		if inc.ReopenedChats > 0 && inc.Reopened > 0 {
			what += " and " + count(inc.ReopenedChats, "chat", "chats")
		} else if inc.ReopenedChats > 0 {
			what = count(inc.ReopenedChats, "finalized chat", "finalized chats")
		}
		msg := fmt.Sprintf("%s will be put back into migration mode to receive new messages", what)
		if len(reopened) > 0 {
			msg += ": " + listSome(reopened)
		}
		r.warn("incremental.reopens", msg+". Teams shows a banner in them that a migration is in progress, and Microsoft's reference says migration mode prevents sending messages and adding members, until `taut finalize` is run again.",
			"Tell the people in them, run the pass at a quiet time, and run `taut finalize` as soon as it has finished. Re-entering migration mode after a finalize is documented by Microsoft and has not been run by Taut against a real tenant.")
	}

	if inc.Edited+inc.Deleted > 0 {
		var parts []string
		if inc.Edited > 0 {
			parts = append(parts, count(inc.Edited, "message", "messages")+" already in Teams "+plural(inc.Edited, "was", "were")+" edited in Slack since")
		}
		if inc.Deleted > 0 {
			parts = append(parts, count(inc.Deleted, "message", "messages")+" already in Teams "+plural(inc.Deleted, "is", "are")+" no longer in the export")
		}
		r.warn("incremental.edits_not_synced",
			strings.Join(parts, ", and ")+". Teams keeps them as they were imported: Microsoft Graph's import API can add messages, and cannot change or remove one.",
			"Nothing to do in Taut. If a removed message must not stay in Teams, delete it there as the channel's owner.")
	}
	if !r.blocked() && inc.New.Messages == 0 && inc.New.Channels == 0 && inc.New.Chats == 0 {
		r.note("incremental.nothing_new",
			"The export holds no message that is not already in Teams.",
			"Nothing to import. Export the workspace again when there is more.")
	}
}

func (r *Report) blocked() bool { return len(r.Blockers) > 0 }

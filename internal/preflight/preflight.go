// Package preflight is the engine behind `taut preflight`: a read-only analysis
// of what a migration would do. It reads the Slack export, applies the
// config file's channel selection and identity policy, classifies every message
// by how it will (or won't) survive the move, and estimates how long the
// import will take. It never writes anywhere.
package preflight

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/graph"
	"github.com/bgfdev/taut/internal/identity"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/slack/export"
	"github.com/bgfdev/taut/internal/transform"
	"github.com/bgfdev/taut/internal/ui/sanitize"
)

// Import rate assumptions. Microsoft documents both 1 and 5 requests per
// second per channel, and 50 per app per tenant with a sustained ceiling
// near 83% of that, so estimates are given as a range.
const (
	conservativeChannelRPS = 1.0
	optimisticChannelRPS   = 5.0
	tenantRPS              = 40.0
	// splitThreshold is the largest message body Taut posts in one piece;
	// Teams caps a post near 100 KB and recommends staying under 80 KB.
	splitThreshold = 80 * 1000
)

// Verdicts.
const (
	VerdictReady    = "ready"
	VerdictWarnings = "ready_with_warnings"
	VerdictBlocked  = "blocked"
)

// Options are the inputs to Run.
type Options struct {
	Config *config.Config
	Export *export.Export

	// Directory is the Entra user index. Nil means identity could not be
	// (or was asked not to be) checked.
	Directory *identity.Directory
	// DirectoryErr is why Directory is nil, when it should not be.
	DirectoryErr error
	// Offline is true when the operator asked not to contact Microsoft.
	Offline bool

	Overrides []identity.Override
	// Permissions is the granted-versus-required table, when known.
	Permissions []graph.PermissionStatus
	// ExistingTeams are display names of teams already in the tenant.
	ExistingTeams []string

	Now func() time.Time
	// Progress, when set, is called as each channel finishes.
	Progress func(done, total int, channel string)
}

// Finding is a blocker or warning with the step that resolves it.
// privateConsequences is what importing direct messages means for the
// people in them. It is said wherever an import that includes them starts.
const privateConsequences = "These are private conversations. A chat is visible to its participants while the import runs, with a notice that a migration is in progress. " +
	"One-on-one history goes into the chat the two people already have in Teams, if they have one. " +
	"Files are not moved, and thread replies become ordinary messages in time order. " +
	"Everyone in a group conversation will see all of its history, including anyone who joined it late: the export does not say when a person joined."

// PrivateNext is what to do about it.
const PrivateNext = "Tell the people concerned before you start. To leave direct messages out, set target.directMessages.enabled to false."

// PrivateNotice is the dm.private warning for a run whose preflight is not
// at hand (it is resumed, or the preflight was skipped): the same thing,
// without the counts only a preflight has.
func PrivateNotice() Finding {
	return Finding{Code: "dm.private",
		Message: "Direct messages and group direct messages will be imported into Teams chats (target.directMessages.enabled). " + privateConsequences,
		Next:    PrivateNext}
}

type Finding struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Next    string `json:"next,omitempty"`
}

// Totals summarize what will be migrated.
type Totals struct {
	Teams         int   `json:"teams"`
	Channels      int   `json:"channels"`
	Messages      int   `json:"messages"`
	ThreadParents int   `json:"threadParents"`
	Replies       int   `json:"replies"`
	Files         int   `json:"files"`
	FileBytes     int64 `json:"fileBytes"`
	Authors       int   `json:"authors"`
	// Chats and ChatMessages count the direct-message conversations that
	// migrate and their messages. They are not part of Channels, Messages,
	// Replies or Files, which describe channels only.
	Chats        int        `json:"chats"`
	ChatMessages int        `json:"chatMessages"`
	First        *time.Time `json:"first,omitempty"`
	Last         *time.Time `json:"last,omitempty"`
}

// ChannelPlan is one Slack channel and where it goes.
type ChannelPlan struct {
	SlackID      string     `json:"slackId"`
	SlackName    string     `json:"slackName"`
	Kind         string     `json:"kind"`
	TargetName   string     `json:"targetName"`
	Renamed      bool       `json:"renamed,omitempty"`
	RenameReason string     `json:"renameReason,omitempty"`
	Archived     bool       `json:"archived,omitempty"`
	Messages     int        `json:"messages"`
	Replies      int        `json:"replies"`
	Files        int        `json:"files"`
	FileBytes    int64      `json:"fileBytes"`
	Malformed    int        `json:"malformed,omitempty"`
	First        *time.Time `json:"first,omitempty"`
	Last         *time.Time `json:"last,omitempty"`
	// imports is the number of Graph POSTs this channel needs.
	imports int
}

// TeamPlan is one target team and its channels.
type TeamPlan struct {
	Name     string        `json:"name"`
	Owner    string        `json:"owner"`
	Exists   bool          `json:"exists,omitempty"`
	Channels []ChannelPlan `json:"channels"`
}

// SkippedChannel is a conversation in the export that will not be migrated.
type SkippedChannel struct {
	SlackName string `json:"slackName"`
	Kind      string `json:"kind"`
	Reason    string `json:"reason"`
}

// UnmappedUser is a Slack author with no Entra match.
type UnmappedUser struct {
	SlackID  string `json:"slackId"`
	Name     string `json:"name"`
	Email    string `json:"email,omitempty"`
	Reason   string `json:"reason"`
	Detail   string `json:"detail"`
	Messages int    `json:"messages"`
}

// IdentityReport is the outcome of user mapping.
type IdentityReport struct {
	Checked          bool           `json:"checked"`
	Users            int            `json:"users"`
	MappedByEmail    int            `json:"mappedByEmail"`
	MappedByOverride int            `json:"mappedByOverride"`
	Unmapped         int            `json:"unmapped"`
	DisabledTargets  int            `json:"disabledTargets,omitempty"`
	Policy           string         `json:"policy"`
	Account          string         `json:"account,omitempty"`
	UnmappedMessages int            `json:"unmappedMessages"`
	UnmappedUsers    []UnmappedUser `json:"unmappedUsers"`
}

// ContentItem is one category of content that does not migrate verbatim.
type ContentItem struct {
	Key      string `json:"key"`
	Label    string `json:"label"`
	Count    int    `json:"count"`
	Handling string `json:"handling"`
	// Lossy is true when information is dropped rather than transformed.
	Lossy bool `json:"lossy,omitempty"`
}

// Estimate is the projected import effort.
type Estimate struct {
	Imports             int     `json:"imports"`
	ConservativeSeconds float64 `json:"conservativeSeconds"`
	OptimisticSeconds   float64 `json:"optimisticSeconds"`
	// BoundBy names what limits the conservative estimate.
	BoundBy      string   `json:"boundBy"`
	StagingBytes int64    `json:"stagingBytes"`
	Notes        []string `json:"notes"`
}

// Report is the full preflight result.
type Report struct {
	Config      string    `json:"config"`
	GeneratedAt time.Time `json:"generatedAt"`
	Verdict     string    `json:"verdict"`
	// Strategy is the load strategy the migration will use: "modern" or
	// "legacy".
	Strategy string    `json:"strategy"`
	Blockers []Finding `json:"blockers"`
	Warnings []Finding `json:"warnings"`
	// Notes are consequences of the config file the operator should know
	// before starting. They need no action and do not change the verdict.
	Notes   []Finding        `json:"notes"`
	Totals  Totals           `json:"totals"`
	Teams   []TeamPlan       `json:"teams"`
	Skipped []SkippedChannel `json:"skipped"`
	// DirectMessages says which DMs and group DMs migrate into chats and
	// which do not, with the reason. Nil when the export has none.
	DirectMessages *DMReport                `json:"directMessages,omitempty"`
	Identity       IdentityReport           `json:"identity"`
	Content        []ContentItem            `json:"content"`
	Estimate       Estimate                 `json:"estimate"`
	Permissions    []graph.PermissionStatus `json:"permissions,omitempty"`
}

type tally struct {
	items map[string]int
}

func (t *tally) add(key string) { t.items[key]++ }

// Run performs the preflight.
func Run(ctx context.Context, o Options) (*Report, error) {
	m, ex := o.Config, o.Export
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	r := &Report{
		Config: m.Metadata.Name, GeneratedAt: now().UTC(),
		Strategy: m.Load.Strategy,
		Blockers: []Finding{}, Warnings: []Finding{}, Notes: []Finding{}, Skipped: []SkippedChannel{},
		Permissions: o.Permissions,
	}

	users, err := ex.Users()
	if err != nil {
		return nil, err
	}
	channels, err := ex.Channels()
	if err != nil {
		return nil, err
	}
	if n := controlUsers(users); n > 0 {
		r.warn("export.user_control_chars",
			fmt.Sprintf("%s in the export %s control characters in a name, email address or ID.", count(n, "person", "people"), plural(n, "has", "have")),
			"Slack does not produce these, so the export was altered or is damaged. Taut shows such names without them; export the workspace again if you did not expect this.")
	}
	idmap := identity.Resolve(users, o.Directory, o.Overrides)
	// Bot messages name their author by bot ID, user ID, or both. Fold bot
	// IDs onto the bot's user so one bot is one author.
	botUser := map[string]string{}
	for _, u := range users {
		if u.Profile.BotID != "" {
			botUser[u.Profile.BotID] = u.ID
		}
	}

	since, until, err := dateBounds(m)
	if err != nil {
		return nil, err
	}

	// --- Plan: which channel goes to which team --------------------------
	lay := Layout(m, channels, o.ExistingTeams)
	r.Teams, r.Skipped = lay.Teams, lay.Skipped
	r.Blockers = append(r.Blockers, lay.Blockers...)
	r.Warnings = append(r.Warnings, lay.Warnings...)
	// One job per conversation to scan: every channel that migrates and,
	// when direct messages are on, every DM and group DM that does.
	type job struct {
		ch slack.Channel
		cp *ChannelPlan
		dm *DMPlan
	}
	jobs := make([]job, 0, len(lay.Assignments))
	for _, a := range lay.Assignments {
		jobs = append(jobs, job{ch: a.Channel, cp: &r.Teams[a.Team].Channels[a.Slot]})
	}
	if convs := DirectMessages(channels, users, idmap, o.Directory != nil); len(convs) > 0 {
		r.DirectMessages = &DMReport{
			Enabled: m.Target.DirectMessages.Enabled, InExport: len(convs),
			IdentityChecked: o.Directory != nil, Conversations: convs,
		}
		for i := range convs {
			if d := &convs[i]; r.DirectMessages.Enabled && d.Migrates() {
				jobs = append(jobs, job{ch: d.Channel, cp: &ChannelPlan{SlackID: d.SlackID, Kind: d.Kind}, dm: d})
			}
		}
	}

	// --- Scan messages ----------------------------------------------------
	t := &tally{items: map[string]int{}}
	authorMsgs := map[string]int{}
	seenFiles := map[string]bool{}
	var first, last time.Time
	var biggest *ChannelPlan

	for n, j := range jobs {
		if err := ctx.Err(); err != nil {
			return nil, fail.Wrap(err, "preflight.cancelled", "Preflight was interrupted", "", "Run `taut preflight` again.")
		}
		cp := j.cp
		seenMS := map[int64]bool{}
		var cFirst, cLast time.Time

		for msg, err := range ex.Messages(j.ch) {
			if err != nil {
				var me *export.MessageError
				if errors.As(err, &me) {
					cp.Malformed++
					t.add("malformed")
					continue
				}
				what := "#" + j.ch.Name
				if j.dm != nil {
					// A direct message is named by its ID: its participants
					// are nobody else's business in a list of problems.
					what = "the direct-message conversation " + sanitize.Text(j.ch.ID)
				}
				r.warn("export.day_unreadable", fmt.Sprintf("Part of %s could not be read: %s", what, fail.From(err).What),
					"Re-export the workspace if this conversation matters; the rest of the export is unaffected.")
				continue
			}
			ts := msg.Time()
			if (!since.IsZero() && ts.Before(since)) || (!until.IsZero() && !ts.Before(until)) {
				t.add("out_of_range")
				continue
			}

			// One classifier decides what happens to a message, here and
			// in the migration itself, so this report cannot disagree with
			// what `taut migrate` does.
			class := transform.Classify(msg)
			if class.Key != "" {
				t.add(class.Key)
			}
			if class.Disposition == transform.Skip {
				continue
			}

			// From here on the message becomes at least one Teams post.
			parts := 1
			if size := len(msg.Text); size > splitThreshold {
				parts = (size + splitThreshold - 1) / splitThreshold
				t.add("oversized")
			}
			cp.Messages++
			cp.imports += parts
			if msg.IsReply() {
				cp.Replies++
				if j.dm != nil {
					t.add("dm_thread_replies")
				}
			}
			if msg.IsThreadParent() && msg.ReplyCount > 0 && j.dm == nil {
				r.Totals.ThreadParents++
			}
			if cFirst.IsZero() || ts.Before(cFirst) {
				cFirst = ts
			}
			if ts.After(cLast) {
				cLast = ts
			}

			if ms := ts.UnixMilli(); seenMS[ms] {
				t.add("timestamp_collisions")
			} else {
				seenMS[ms] = true
			}
			if msg.Edited != nil {
				t.add("edited")
			}
			if len(msg.Reactions) > 0 {
				t.add("reactions")
			}
			if strings.Contains(msg.Text, "<!here") || strings.Contains(msg.Text, "<!channel") || strings.Contains(msg.Text, "<!everyone") {
				t.add("broadcasts")
			}
			if msg.IsBot() {
				t.add("bots")
			}

			// Author.
			author := msg.User
			if author == "" && msg.BotID != "" {
				author = msg.BotID
				if uid, ok := botUser[author]; ok {
					author = uid
				}
			}
			if author != "" {
				if idmap.Lookup(author) == nil {
					idmap.AddUnknown(author, msg.AuthorLabel())
				}
				authorMsgs[author]++
			} else {
				t.add("no_author")
			}

			// Files.
			for _, f := range msg.Files {
				cp.Files++
				if j.dm != nil {
					// Nothing is moved for a chat, whatever the file is.
					t.add("dm_files")
					continue
				}
				switch {
				case f.IsHidden():
					t.add("files_hidden")
				case f.IsTombstone():
					t.add("files_deleted")
				case f.IsExternal || f.Mode == slack.FileModeExternal:
					t.add("files_external")
				default:
					if f.IsCanvas() {
						t.add("canvases")
					}
					if f.ID == "" || !seenFiles[f.ID] {
						seenFiles[f.ID] = true
						cp.FileBytes += f.Size
					}
				}
			}
		}

		if !cFirst.IsZero() {
			cf, cl := cFirst.UTC(), cLast.UTC()
			cp.First, cp.Last = &cf, &cl
			if first.IsZero() || cFirst.Before(first) {
				first = cFirst
			}
			if cLast.After(last) {
				last = cLast
			}
		}
		if d := j.dm; d != nil {
			// The label of a direct message stays out of progress lines.
			if o.Progress != nil {
				o.Progress(n+1, len(jobs), "direct messages")
			}
			if cp.Messages == 0 {
				d.skip(DMSkipNoMessages)
				continue
			}
			d.Messages, d.Replies, d.Files, d.First, d.Last, d.imports = cp.Messages, cp.Replies, cp.Files, cp.First, cp.Last, cp.imports
			r.Totals.Chats++
			r.Totals.ChatMessages += cp.Messages
		} else {
			r.Totals.Messages += cp.Messages
			r.Totals.Replies += cp.Replies
			r.Totals.Files += cp.Files
			r.Totals.FileBytes += cp.FileBytes
			if o.Progress != nil {
				o.Progress(n+1, len(jobs), j.ch.Name)
			}
		}
		r.Estimate.Imports += cp.imports
		if biggest == nil || cp.imports > biggest.imports {
			biggest = cp
		}
	}
	if r.DirectMessages != nil {
		r.DirectMessages.summarize()
		if !r.DirectMessages.Enabled {
			// Not migrated, so not looked into: how many there are is all
			// a report says about conversations it leaves alone.
			r.DirectMessages = &DMReport{InExport: r.DirectMessages.InExport, SkippedByReason: []DMReasonCount{}, Conversations: []DMPlan{}}
		}
	}

	r.Totals.Teams = len(r.Teams)
	r.Totals.Channels = len(lay.Assignments)
	r.Totals.Authors = len(authorMsgs)
	if !first.IsZero() {
		f, l := first.UTC(), last.UTC()
		r.Totals.First, r.Totals.Last = &f, &l
	}

	r.identity(o, idmap, authorMsgs)
	r.content(m, t)
	r.estimate(biggest)
	r.checks(o, ex, t)

	switch {
	case len(r.Blockers) > 0:
		r.Verdict = VerdictBlocked
	case len(r.Warnings) > 0:
		r.Verdict = VerdictWarnings
	default:
		r.Verdict = VerdictReady
	}
	return r, nil
}

func (r *Report) block(code, msg, next string) {
	r.Blockers = append(r.Blockers, Finding{Code: code, Message: msg, Next: next})
}

func (r *Report) warn(code, msg, next string) {
	r.Warnings = append(r.Warnings, Finding{Code: code, Message: msg, Next: next})
}

func (r *Report) note(code, msg, next string) {
	r.Notes = append(r.Notes, Finding{Code: code, Message: msg, Next: next})
}

// controlUsers counts the users whose identifying text contains a control
// character.
func controlUsers(users []slack.User) int {
	n := 0
	for _, u := range users {
		for _, v := range []string{u.ID, u.Name, u.RealName, u.Profile.DisplayName, u.Profile.RealName, u.Profile.Email} {
			if sanitize.HasControl(v) {
				n++
				break
			}
		}
	}
	return n
}

func selectsKind(m *config.Config, kind string) bool {
	for _, t := range m.Target.Teams {
		for _, k := range t.Channels.Types {
			if k == kind {
				return true
			}
		}
	}
	return false
}

func teamNames(m *config.Config, idx []int) string {
	names := make([]string, len(idx))
	for i, n := range idx {
		names[i] = m.Target.Teams[n].Name
	}
	return strings.Join(names, ", ")
}

func dateBounds(m *config.Config) (since, until time.Time, err error) {
	if s := m.Content.Since; s != "" {
		if since, err = config.ParseDate(s); err != nil {
			return
		}
	}
	if s := m.Content.Until; s != "" {
		if until, err = config.ParseDate(s); err != nil {
			return
		}
		until = until.AddDate(0, 0, 1) // inclusive of the whole day
	}
	return
}

func (r *Report) identity(o Options, idmap *identity.Map, authorMsgs map[string]int) {
	m := o.Config
	byEmail, byOverride, unmapped := idmap.Counts()
	ir := IdentityReport{
		Checked: o.Directory != nil, Users: byEmail + byOverride + unmapped,
		MappedByEmail: byEmail, MappedByOverride: byOverride, Unmapped: unmapped,
		Policy: m.Identity.Unmapped.Policy, Account: m.Identity.Unmapped.Account,
		UnmappedUsers: []UnmappedUser{},
	}
	for _, e := range idmap.Entries() {
		if e.TargetDisabled {
			ir.DisabledTargets++
		}
	}
	for _, e := range idmap.Unmapped() {
		n := authorMsgs[e.SlackID]
		ir.UnmappedMessages += n
		ir.UnmappedUsers = append(ir.UnmappedUsers, UnmappedUser{
			SlackID: e.SlackID, Name: e.Name, Email: e.Email,
			Reason: string(e.Reason), Detail: e.Reason.Describe(), Messages: n,
		})
	}
	// Authors with the most messages first: they matter most.
	sort.SliceStable(ir.UnmappedUsers, func(i, j int) bool { return ir.UnmappedUsers[i].Messages > ir.UnmappedUsers[j].Messages })
	r.Identity = ir

	switch {
	case o.Directory == nil && o.Offline:
		r.warn("identity.unchecked", "People were not matched against Entra because --offline was set.",
			"Run `taut preflight` without --offline before migrating.")
		return
	case o.Directory == nil:
		fe := fail.From(o.DirectoryErr)
		why, next := "the directory could not be read", "Run `taut auth` to diagnose the connection."
		if fe != nil {
			why, next = lowerFirst(fe.What), fe.Next
		}
		r.block("identity.unchecked", "People could not be matched against Entra: "+why+".", next)
		return
	}

	authors := 0
	for _, u := range ir.UnmappedUsers {
		if u.Messages > 0 {
			authors++
		}
	}
	if authors > 0 {
		msg := fmt.Sprintf("%d %s with %s have no Entra match.", authors, plural(authors, "author", "authors"), count(ir.UnmappedMessages, "message", "messages"))
		if m.Identity.Unmapped.Policy == config.PolicyBlock {
			r.block("identity.unmapped", msg+" The config file's policy is to block.",
				"Add them to the override CSV (identity.overrides), or change identity.unmapped.policy to former-member.")
		} else {
			r.warn("identity.unmapped", msg+fmt.Sprintf(" Their messages will be attributed to %s with the original name shown.", m.Identity.Unmapped.Account),
				"Review the People section. Map anyone who should keep their identity in the override CSV (identity.overrides).")
		}
	}
	if acct := m.Identity.Unmapped.Account; acct != "" && m.Identity.Unmapped.Policy != config.PolicyBlock {
		if _, ok := o.Directory.Find(acct); !ok {
			r.block("identity.account_missing", fmt.Sprintf("The account for unmapped authors, %s, does not exist in Entra.", acct),
				"Create that user in Entra, or point identity.unmapped.account at an existing one.")
		}
	}
	for _, t := range m.Target.Teams {
		if _, ok := o.Directory.Find(t.Owner); !ok {
			r.block("plan.owner_missing", fmt.Sprintf("The owner of team %q, %s, does not exist in Entra.", t.Name, t.Owner),
				"Set target.teams[].owner to the user principal name of an existing user.")
		}
	}
	if ir.DisabledTargets > 0 {
		r.warn("identity.disabled_targets", fmt.Sprintf("%d Slack %s to Entra accounts that are disabled.", ir.DisabledTargets, plural(ir.DisabledTargets, "user maps", "users map")),
			"Their messages keep their name. No action is needed unless the match is wrong.")
	}
}

func (r *Report) content(m *config.Config, t *tally) {
	reactions := map[string]string{
		config.ReactionsFooter: "Summarized in a line under the message (whether imported reactions show in Teams is unconfirmed)",
		config.ReactionsNative: "Summarized in a line under the message; importing them as Teams reactions stays off until it is verified on a real tenant",
		config.ReactionsDrop:   "Dropped, as set in the config file",
	}[m.Content.Reactions]

	add := func(key, label, handling string, lossy bool) {
		if n := t.items[key]; n > 0 {
			r.Content = append(r.Content, ContentItem{Key: key, Label: label, Count: n, Handling: handling, Lossy: lossy})
		}
	}
	r.Content = []ContentItem{}
	add("reactions", "Messages with reactions", reactions, m.Content.Reactions == config.ReactionsDrop)
	add("edited", "Edited messages", "Final text is imported with an \"edited\" marker; edit history is not available", true)
	add("bots", "Bot and app messages", "Attributed to the placeholder account with the bot's name shown", false)
	add("broadcasts", "@here / @channel / @everyone", "Converted to plain text so the import notifies nobody", false)
	add("timestamp_collisions", "Same-millisecond messages", "Nudged by 1 ms each; Teams requires unique timestamps per channel", false)
	add("oversized", "Messages over 80 KB", "Split into consecutive posts", false)
	add("files_external", "Files hosted outside Slack", "The link is carried across; the file itself is not copied", false)
	add("canvases", "Canvases", "Uploaded as HTML files", false)
	add("huddles", "Huddles", "Replaced by a one-line placeholder", true)
	add("tombstones", "Deleted thread starters", "Replaced by a placeholder so replies keep their thread", true)
	add("unknown_subtype", "Unrecognized message types", "Imported as plain text", false)
	add("pins", "Pinned items", "Not migrated; Teams cannot import pins", true)
	add("system", "Join, leave and topic notices", "Not migrated", true)
	add("deleted", "Deleted messages", "Not migrated", true)
	add("edit_log", "Edit-history records", "Not migrated", true)
	add("hidden", "Hidden messages", "Not migrated", true)
	add("files_hidden", "Files beyond the Slack plan's history limit", "Not available to migrate", true)
	add("files_deleted", "Deleted files", "Not available to migrate", true)
	add("malformed", "Unreadable entries in the export", "Skipped", true)
	add("no_author", "Messages with no author", "Attributed to the placeholder account", false)
	add("out_of_range", "Messages outside the config file's date range", "Not migrated", false)
	add("dm_thread_replies", "Thread replies in direct messages", "Imported as ordinary messages, in time order; chats have no threads", true)
	add("dm_files", "Files in direct messages", "Not moved; each message says a file was there", true)
}

func (r *Report) estimate(biggest *ChannelPlan) {
	e := &r.Estimate
	e.StagingBytes = r.Totals.FileBytes
	e.Notes = []string{
		"Microsoft documents import limits of both 1 and 5 requests per second per channel; the range reflects that.",
		"File transfer time depends on bandwidth and is not included.",
		"Microsoft's documentation says it may charge for import volume in future; it publishes no figures.",
	}
	if e.Imports == 0 || biggest == nil {
		e.BoundBy = "nothing to import"
		return
	}
	tenant := float64(e.Imports) / tenantRPS
	slowest := float64(biggest.imports)
	e.ConservativeSeconds = max(tenant, slowest/conservativeChannelRPS)
	e.OptimisticSeconds = max(tenant, slowest/optimisticChannelRPS)
	switch {
	case slowest/conservativeChannelRPS < tenant:
		e.BoundBy = "the tenant-wide rate limit"
	case biggest.Kind == string(slack.KindDM) || biggest.Kind == string(slack.KindMPIM):
		e.BoundBy = fmt.Sprintf("the largest direct-message conversation (%s)", count(biggest.imports, "post", "posts"))
	default:
		e.BoundBy = fmt.Sprintf("the largest channel, #%s (%s)", biggest.SlackName, count(biggest.imports, "post", "posts"))
	}
}

func (r *Report) checks(o Options, ex *export.Export, t *tally) {
	m := o.Config
	if r.Totals.Channels == 0 && r.Totals.Chats == 0 {
		r.block("plan.empty", "No channels are selected for migration.",
			"Check target.teams[].channels.include and .types in the config file against the channel names in the export.")
	}
	for _, tp := range r.Teams {
		// General exists already, so it does not count against the limit.
		if len(tp.Channels) > maxChannelsTeam {
			r.block("plan.too_many_channels", fmt.Sprintf("Team %q would have %d channels; Teams allows %d.", tp.Name, len(tp.Channels), maxChannelsTeam),
				"Split the channels across more teams in the config file.")
		}
	}
	if missing := graph.MissingRequired(o.Permissions); len(missing) > 0 {
		r.block("graph.permission_missing", "The app registration is missing required permissions: "+strings.Join(missing, ", ")+".",
			"Grant them and admin consent (scripts/entra-app.sh), then run `taut auth`.")
	}
	if t.items["malformed"] > 0 {
		r.warn("export.malformed", fmt.Sprintf("%s in the export could not be read and will be skipped.", count(t.items["malformed"], "entry", "entries")),
			"Usually harmless. Re-export the workspace if the count is large.")
	}
	renamed := 0
	for _, tp := range r.Teams {
		for _, c := range tp.Channels {
			if c.Renamed {
				renamed++
			}
		}
	}
	if renamed > 0 {
		r.warn("plan.renamed", fmt.Sprintf("%s will be renamed to satisfy Teams naming rules.", count(renamed, "channel", "channels")),
			"See the Plan section. Choose your own names with target.teams[].channels.rename.")
	}
	if r.Totals.Files > 0 && !m.Source.Slack.API.Enabled {
		r.warn("files.export_links", "Files will be downloaded with the links embedded in the export, and Slack does not say how long those stay valid.",
			"Run the migration soon after exporting, or enable source.slack.api so files can be fetched with a token.")
	}
	private := 0
	for _, tp := range r.Teams {
		for _, c := range tp.Channels {
			if c.Kind == config.ChannelPrivate {
				private++
			}
		}
	}
	if private > 0 {
		r.note("plan.private_members_join_team",
			fmt.Sprintf("%s will be migrated. At `taut finalize` %s Slack members are added to the team as well as to the channel, because Teams only lets members of a team into its private channels. They will see the team's other channels.",
				count(private, "private channel", "private channels"), plural(private, "its", "their")),
			"To keep them out of those, give private channels a team of their own in the config file (target.teams).")
	}
	if m.Load.Strategy == config.StrategyLegacy {
		r.note("load.legacy",
			"The legacy load strategy is in use. Each team is created in migration mode: nobody can see it or be added to it, its owner included, until `taut finalize`. "+
				"Private channels cannot be migrated, a team that already exists cannot be imported into, and finalizing completes the whole team for good.",
			"This is the only import path in the national clouds (GCC High, DoD, 21Vianet). In the global cloud, prefer load.strategy: modern.")
	}
	r.directMessages(m, ex)
	if !ex.HasFile("groups.json") && selectsKind(m, config.ChannelPrivate) {
		r.warn("export.no_private", "The config file includes private channels, but the export contains none.",
			"Private channels need an \"all channels and conversations\" export from a Business+ or Enterprise workspace.")
	}
}

// directMessages adds what an operator must know before importing private
// conversations: that they are private, where they go, what is lost, and
// whether the export has any.
func (r *Report) directMessages(m *config.Config, ex *export.Export) {
	if !m.Target.DirectMessages.Enabled {
		return
	}
	if !ex.HasFile("dms.json") && !ex.HasFile("mpims.json") {
		r.warn("export.no_dms", "The config file enables direct messages, but the export contains none.",
			"Direct messages are only in an \"all channels and conversations\" export, which Slack offers on Business+ and Enterprise plans, to an owner who applied for it. Export again that way, or set target.directMessages.enabled to false.")
		return
	}
	d := r.DirectMessages
	if d == nil {
		return
	}
	if n := d.OneOnOne + d.Group; n > 0 {
		r.warn("dm.private",
			fmt.Sprintf("%s (%s one-on-one, %s group) with %s will be imported into Teams chats. ",
				count(n, "direct-message conversation", "direct-message conversations"), Comma(int64(d.OneOnOne)), Comma(int64(d.Group)),
				count(d.Messages, "message", "messages"))+privateConsequences,
			PrivateNext)
	}
	if d.Skipped > 0 {
		r.note("dm.skipped",
			fmt.Sprintf("%s will not be migrated, because not every participant can be put into a Teams chat as themselves, or there is nothing to import.",
				count(d.Skipped, "direct-message conversation", "direct-message conversations")),
			"See the Direct messages section for the reasons. A conversation is imported with all of its participants or not at all.")
	}
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func count(n int, one, many string) string {
	return fmt.Sprintf("%s %s", Comma(int64(n)), plural(n, one, many))
}

// Comma formats n with thousands separators.
func Comma(n int64) string {
	s := fmt.Sprintf("%d", n)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

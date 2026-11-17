package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"charm.land/huh/v2"
	"github.com/spf13/cobra"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/tui/checklist"
	"github.com/bgfdev/taut/internal/tui/forms"
	"github.com/bgfdev/taut/internal/tui/theme"
	"github.com/bgfdev/taut/internal/ui"
)

// finalizeChannels resolves the channels named on the command line to Slack
// channel IDs. A name may be a Slack channel name (with or without #), its
// Teams name, or a Slack ID.
//
// A direct message is named by its Slack ID only. It has no name of its
// own: what the reports show for it is the names of its participants, and
// those say neither which conversation is meant nor are they unique.
func finalizeChannels(p *store.Progress, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	var known []store.ChannelProgress
	if p != nil {
		known = p.Channels
	}
	ids := make([]string, 0, len(names))
	seen := map[string]bool{}
	for _, raw := range names {
		name := strings.TrimPrefix(strings.TrimSpace(raw), "#")
		var match []string
		for _, c := range known {
			if c.SlackID == name || (!store.IsChatKind(c.Kind) && (strings.EqualFold(c.SlackName, name) || strings.EqualFold(c.TargetName, name))) {
				match = append(match, c.SlackID)
			}
		}
		switch {
		case len(match) == 0:
			return nil, fail.New("finalize.unknown_channel", fmt.Sprintf("No channel named %q in this migration", raw), "",
				"Run `taut status --json` to list the channels, and name one by its Slack name or ID. A direct message is named by its Slack ID.").WithExit(fail.ExitUsage)
		case len(match) > 1 && match[0] != name:
			return nil, fail.New("finalize.ambiguous_channel", fmt.Sprintf("%q matches more than one channel", raw),
				"Channels in different teams can share a name.",
				"Name it by Slack ID instead: "+strings.Join(match, ", ")+".").WithExit(fail.ExitUsage)
		}
		if !seen[match[0]] {
			seen[match[0]] = true
			ids = append(ids, match[0])
		}
	}
	return ids, nil
}

// finalizeOnly resolves --only to the IDs of every conversation of one
// kind: the channels, or the direct messages.
func finalizeOnly(p *store.Progress, only string) ([]string, error) {
	var chats bool
	switch only {
	case "channels":
	case "direct-messages":
		chats = true
	default:
		return nil, fail.New("finalize.bad_only", fmt.Sprintf("--only does not take %q", only), "",
			"Use --only channels or --only direct-messages.").WithExit(fail.ExitUsage)
	}
	var ids []string
	if p != nil {
		for _, c := range p.Channels {
			if store.IsChatKind(c.Kind) == chats {
				ids = append(ids, c.SlackID)
			}
		}
	}
	if len(ids) == 0 {
		what := "channels"
		if chats {
			what = "direct messages"
		}
		return nil, fail.New("finalize.nothing_of_kind", "This migration has no "+what, "",
			"Run `taut finalize` without --only.").WithExit(fail.ExitUsage)
	}
	return ids, nil
}

// finalizeScope describes what a finalize request covers, for the confirmation and
// the report.
type finalizeScope struct {
	// Channels and Open count every selected conversation, chats included:
	// selected, and of those still in migration mode.
	Channels int
	Open     int
	// Chats and OpenChats are the direct messages among them.
	Chats     int
	OpenChats int
	Teams     []string
	Posted    int
	Failed    int
	Held      int
	// Remaining counts the channels of the run that are not selected and
	// stay in migration mode, and RemainingChats the chats that do. They
	// are counted apart: a chat is not locked to anybody and belongs to no
	// team.
	Remaining      int
	RemainingChats int
	// Skipped counts the selected conversations Microsoft refused a chat
	// for. Nothing was created for them, so there is nothing to finalize.
	Skipped int
}

// inMigration reports whether a conversation still has something to
// finalize: it is neither completed nor skipped.
func inMigration(c store.ChannelProgress) bool {
	return c.State != store.ChannelCompleted && c.State != store.ChannelSkipped
}

// releasable reports whether a chat exists in Teams and may be in migration
// mode, which is what releasing the chats of a run that will not finish
// acts on.
func releasable(c store.ChannelProgress) bool {
	return store.IsChatKind(c.Kind) && (c.State == store.ChannelCreated || c.State == store.ChannelMigrating)
}

// chatsOnly reports whether ids name direct messages and nothing else.
func chatsOnly(p *store.Progress, ids []string) bool {
	if p == nil || len(ids) == 0 {
		return false
	}
	chat := map[string]bool{}
	for _, c := range p.Channels {
		chat[c.SlackID] = store.IsChatKind(c.Kind)
	}
	for _, id := range ids {
		if !chat[id] {
			return false
		}
	}
	return true
}

func scopeOf(p *store.Progress, ids []string) finalizeScope {
	var s finalizeScope
	if p == nil {
		return s
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	teams := map[string]bool{}
	for _, c := range p.Channels {
		if len(ids) > 0 && !want[c.SlackID] {
			switch {
			case !inMigration(c):
			case store.IsChatKind(c.Kind):
				s.RemainingChats++
			default:
				s.Remaining++
			}
			continue
		}
		s.Channels++
		if inMigration(c) {
			s.Open++
		}
		if c.State == store.ChannelSkipped {
			s.Skipped++
		}
		s.Posted += c.Posted
		s.Failed += c.Failed
		s.Held += c.Held
		if store.IsChatKind(c.Kind) {
			// A chat belongs to no team.
			s.Chats++
			if inMigration(c) {
				s.OpenChats++
			}
			continue
		}
		teams[c.Team] = true
	}
	for t := range teams {
		s.Teams = append(s.Teams, t)
	}
	sort.Strings(s.Teams)
	return s
}

// countMembers counts the members a run added, from the audit log. It gives
// up quietly: the number is a courtesy in the report, not a result.
func countMembers(ctx context.Context, c *controller.Client, runID string) (int, bool) {
	f := store.LogFilter{Op: "member.add", Outcome: "ok", Limit: 5000}
	n := 0
	for range 40 {
		page, err := c.AuditLog(ctx, runID, f)
		if err != nil {
			return 0, false
		}
		n += len(page.Entries)
		if page.Next == nil {
			return n, true
		}
		f.After = *page.Next
	}
	return 0, false
}

func newFinalizeCmd(app *App) *cobra.Command {
	var channels []string
	var only string
	var retain, detach bool
	cmd := &cobra.Command{
		Use:     "finalize",
		Short:   "Complete migration mode and add members",
		GroupID: groupRun,
		Long: `Finalize a migration that has finished importing.

finalize completes migration mode on each channel, which unlocks it for people,
then adds each team's members, and finally deletes the staged copy of the
Slack export and everything derived from it. With --retain that copy is kept,
encrypted. If direct messages are migrated it holds the export's direct
messages, including those of conversations that were not migrated; if they
are not, it holds none: they are left out before the export is uploaded.

Completing migration mode cannot be undone: after it, nothing more can be
imported into a channel with its original dates and authors. finalize therefore
shows what it is about to do and asks first. Check the result of the import
in Teams (as an owner) before you run it.

A team gets its members once all of its channels are finalized. Use
--channel to finalize some channels and leave the rest importable. The
members of a private channel are added to that channel and to its team.

Direct messages were imported into Teams chats, which their participants
could see all along. Finalizing a chat only ends its migration mode; it adds
nobody. A direct message is named by its Slack ID (taut status --json lists
them), and --only direct-messages or --only channels selects all of one kind.

A run that failed or was cancelled is not finalized. Its chats can still be
taken out of migration mode, which would otherwise last: taut finalize --only
direct-messages does that and nothing else.

With load.strategy: legacy the team itself is in migration mode and has no
members, not even its owner, until every channel is finalized; finalize then
completes the team as well, which is final.`,
		Example: `  taut finalize
  taut finalize --channel general --channel announcements
  taut finalize --only channels     # leave the chats of direct messages for later
  taut finalize --retain            # keep the staged export afterwards
  taut finalize --yes --json        # for CI`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, ctx := app.IO, cmd.Context()
			m, err := app.loadConfig()
			if err != nil {
				return err
			}
			client := app.controllerClient(m)
			run, err := client.LatestRun(ctx, m.Metadata.Name)
			if err != nil {
				if fe := fail.From(err); fe.Code == "run.not_found" {
					fe.What = "No migration has been started for " + m.Metadata.Name
					fe.Next = "Run `taut migrate` to import first; `taut finalize` finalizes an import."
					return fe
				}
				return err
			}
			t, w := out.Theme, out.Width-ui.Margin
			// resumed is true when finalizing had stopped and was retried.
			resumed := false

			if only != "" && len(channels) > 0 {
				return fail.New("finalize.only_and_channel", "--only and --channel cannot be combined", "",
					"Name conversations with --channel, or take all of one kind with --only.").WithExit(fail.ExitUsage)
			}
			ids, err := finalizeChannels(run.Progress, channels)
			if err != nil {
				return err
			}
			if only != "" {
				if ids, err = finalizeOnly(run.Progress, only); err != nil {
					return err
				}
			}
			// What a run that will not finish leaves behind in Teams that
			// matters to people: chats in migration mode.
			held := 0
			if run.Progress != nil {
				for _, c := range run.Progress.Channels {
					if releasable(c) {
						held++
					}
				}
			}
			heldAdvice := func(next string) string {
				if held == 0 {
					return next
				}
				return next + " " + countOf(held, "chat of a direct message is", "chats of direct messages are") +
					" still in migration mode; `taut finalize --only direct-messages` ends that without finishing the import."
			}

			switch run.Status {
			case store.RunCompleted, store.RunFinalizing, store.RunFinalized:
			case store.RunFailed:
				if _, finalizing := stageOf(run.Progress, store.StageFinalize); finalizing {
					// Finalizing itself stopped part-way. Continue it.
					n, err := client.Retry(ctx, run.ID)
					if err != nil {
						return err
					}
					out.Status("Continuing to finalize run %s; retrying %s that had stopped.", run.ID, countOf(n, "lane", "lanes"))
					if run, err = client.Run(ctx, run.ID); err != nil {
						return err
					}
					resumed = true
					break
				}
				if chatsOnly(run.Progress, ids) {
					return app.releaseChats(ctx, m, client, run, ids, detach)
				}
				return fail.New("finalize.import_failed", "The import has not finished", "Run "+run.ID+" stopped with failures.",
					heldAdvice("Run `taut status` to see what failed and `taut migrate` to resume, then `taut finalize`.")).WithExit(fail.ExitBlockers)
			case store.RunCancelled:
				if chatsOnly(run.Progress, ids) {
					return app.releaseChats(ctx, m, client, run, ids, detach)
				}
				return fail.New("finalize.cancelled", "The last run was cancelled", "",
					heldAdvice("Run `taut migrate` to import, then `taut finalize`.")).WithExit(fail.ExitBlockers)
			default:
				return fail.New("finalize.still_importing", "The import is still running", "Run "+run.ID+" is "+string(run.Status)+".",
					"Wait for it to finish (`taut status --watch`), then run `taut finalize`.").WithExit(fail.ExitBlockers)
			}

			if retain && run.Status == store.RunFinalized && !run.Retain {
				return fail.New("finalize.retain_too_late", "This migration is already finalized",
					"Staging is deleted as soon as a migration is finalized; --retain has to be given when finalizing starts.",
					"Nothing to do. The audit log remains: `taut audit --export audit.ndjson`.").WithExit(fail.ExitUsage)
			}
			scope := scopeOf(run.Progress, ids)
			if p := run.Progress; p != nil && p.Posts-p.Posted-p.Failed-p.Held > 0 {
				return fail.New("finalize.posts_unsettled", "Some posts were never imported",
					countOf(p.Posts-p.Posted-p.Failed-p.Held, "post is", "posts are")+" still pending although the run reports complete.",
					"Do not finalize. Run `taut status`, and `taut migrate --new` if the run cannot be resumed.").WithExit(fail.ExitBlockers)
			}

			// Reattaching to a run that is already finalizing sends no new
			// request: what was asked for stays what was asked for. Only
			// naming channels adds to it, and that is confirmed like any
			// other request.
			request := run.Status == store.RunCompleted || (len(ids) > 0 && (run.Status == store.RunFinalizing || resumed))
			if (run.Status == store.RunFinalizing || resumed) && len(ids) > 0 && scope.Open == 0 {
				request = false
			}
			if request {
				if scope.Open == 0 && run.Progress != nil && len(ids) > 0 {
					if out.IsJSON() {
						return out.JSON(map[string]any{"run": run, "changed": false})
					}
					out.Println(ui.Pad("\n" + ui.Verdict(t, theme.StatusOK, "Already finalized",
						conversations(scope.Channels-scope.Chats, scope.Chats)) + "\n\n" +
						ui.Next(t, "taut finalize", "finalize the remaining channels")))
					return nil
				}
				// The audit trail is the record of what was imported.
				// Finalizing on top of a broken one would make the damage
				// permanent.
				lctx, cancel := context.WithTimeout(ctx, 60*time.Second)
				ok, problem, verr := client.VerifyAuditLog(lctx, run.ID)
				cancel()
				if verr != nil {
					return verr
				}
				if !ok {
					return fail.New("finalize.audit_corrupt", "The audit log failed verification", problem,
						"Do not finalize. Run `taut audit --verify` and investigate before going further.").WithExit(fail.ExitFailure)
				}

				if !out.Yes {
					if !out.Interactive() {
						return fail.New("finalize.unconfirmed", "Finalizing needs confirmation",
							"Completing migration mode cannot be undone, and finalize will not decide that when it cannot ask.",
							"Review the import in Teams, then run `taut finalize --yes`.").WithExit(fail.ExitBlockers)
					}
					var b strings.Builder
					b.WriteString("\n" + ui.Banner(t, "finalize", m.Metadata.Name) + "\n\n")
					rows := finalizeRows(scope, m.Load.Strategy == config.StrategyLegacy, retain || run.Retain)
					b.WriteString(ui.KV(t, rows))
					if scope.Failed+scope.Held > 0 {
						b.WriteString("\n\n" + ui.Finding(t, theme.StatusWarn,
							countOf(scope.Failed+scope.Held, "post was", "posts were")+" not imported and cannot be added after this.",
							"`taut audit --errors` lists them.", w))
					}
					out.Println(ui.Pad(b.String()))
					out.Println()
					proceed := false
					err := huh.NewForm(huh.NewGroup(huh.NewConfirm().
						Title(finalizeQuestion(scope)).
						Description(finalizeConsequences(m.Load.Strategy == config.StrategyLegacy, scope.Open-scope.OpenChats > 0, scope.OpenChats > 0, scope.Remaining > 0)).
						Affirmative("Finalize").Negative("Not yet").Value(&proceed))).
						WithTheme(forms.Theme(out.Theme)).WithInput(out.In).RunWithContext(ctx)
					if err != nil || !proceed {
						return fail.Exit(fail.ExitOK, "not finalized") //nolint:nilerr
					}
				}
			}

			started := time.Now()
			last := run
			if run.Status != store.RunFinalized {
				if request {
					mv, err := client.Finalize(ctx, run.ID, controller.FinalizeRequest{Channels: ids, Retain: retain})
					if err != nil {
						return err
					}
					last = mv.Run
				}
				if detach {
					if out.IsJSON() {
						return out.JSON(map[string]any{"run": last})
					}
					out.Println(ui.Pad("\n" + ui.Verdict(t, theme.StatusInfo, "Finalizing started", last.ID) +
						"\n\n" + ui.Next(t, "taut finalize", "watch it finish")))
					return nil
				}
				if !stopped(last.Status) {
					v, detached, err := app.follow(ctx, client, m.Metadata.Name, last)
					if err != nil {
						return err
					}
					if detached {
						if !out.IsJSON() {
							out.Println(ui.Pad("\n" + ui.Verdict(out.Theme, theme.StatusInfo, "Detached", "finalizing continues in the background") +
								"\n\n" + ui.Next(out.Theme, "taut finalize", "reattach")))
						}
						return nil
					}
					last = v
				}
			}
			t = out.Theme // the live view may have learned the background

			// Staging is purged by the control plane shortly after the run
			// is finalized. Wait briefly so the report can say it happened.
			if last.Status == store.RunFinalized && !last.Retain && last.PurgedAt == nil {
				deadline := time.Now().Add(60 * time.Second)
				for last.PurgedAt == nil && time.Now().Before(deadline) && ctx.Err() == nil {
					select {
					case <-ctx.Done():
					case <-time.After(500 * time.Millisecond):
					}
					if v, err := client.Run(ctx, last.ID); err == nil {
						last = v
					}
				}
			}
			members, haveMembers := 0, false
			if last.Status == store.RunFinalized || last.Status == store.RunCompleted {
				members, haveMembers = countMembers(ctx, client, last.ID)
			}
			var logOK *bool
			if last.Status == store.RunFinalized || last.Status == store.RunCompleted {
				lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
				if ok, _, verr := client.VerifyAuditLog(lctx, last.ID); verr == nil {
					logOK = &ok
				}
				cancel()
			}

			if out.IsJSON() {
				res := map[string]any{"run": last, "auditLogVerified": logOK}
				if haveMembers {
					res["membersAdded"] = members
				}
				if err := out.JSON(res); err != nil {
					return err
				}
			} else {
				out.Println(ui.Pad("\n" + finalizeSummary(t, last, time.Since(started), members, haveMembers, logOK, w)))
			}
			switch {
			case last.Status == store.RunFailed:
				return fail.Exit(fail.ExitFailure, "finalizing stopped")
			case logOK != nil && !*logOK:
				return fail.Exit(fail.ExitFailure, "the audit log failed verification")
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&channels, "channel", nil, "finalize only this channel (Slack name or ID) or direct message (Slack ID); repeatable")
	cmd.Flags().StringVar(&only, "only", "", "finalize only one kind of conversation: channels, or direct-messages")
	cmd.Flags().BoolVar(&retain, "retain", false, "keep the staged export and data after finalizing instead of deleting them; with direct messages enabled that includes every direct message in the export, those not migrated too")
	cmd.Flags().BoolVar(&detach, "detach", false, "start finalizing and return without watching it")
	return cmd
}

// finalizeQuestion is what the confirmation asks. Channels are opened to
// people; the chat of a direct message was never closed to them.
func finalizeQuestion(s finalizeScope) string {
	channels := s.Open - s.OpenChats
	switch {
	case channels > 0 && s.OpenChats > 0:
		return fmt.Sprintf("Open %s to their members, and end migration mode on %s?", countOf(channels, "channel", "channels"), countOf(s.OpenChats, "chat", "chats"))
	case s.OpenChats > 0:
		return fmt.Sprintf("End migration mode on %s?", countOf(s.OpenChats, "chat", "chats"))
	}
	return fmt.Sprintf("Open %s to their members?", countOf(channels, "channel", "channels"))
}

// conversations says how many channels and direct messages something
// covers, naming only what there is.
func conversations(channels, chats int) string {
	switch {
	case chats == 0:
		return countOf(channels, "channel", "channels")
	case channels == 0:
		return countOf(chats, "direct-message conversation", "direct-message conversations")
	}
	return countOf(channels, "channel", "channels") + ", " + countOf(chats, "direct-message conversation", "direct-message conversations")
}

// progressCounts counts the channels and the direct messages of a run.
func progressCounts(p *store.Progress) (channels, chats int) {
	if p == nil {
		return 0, 0
	}
	for _, c := range p.Channels {
		if store.IsChatKind(c.Kind) {
			chats++
		} else {
			channels++
		}
	}
	return channels, chats
}

// conversationName is how a channel or a direct message is shown in a
// list: "#general", or the names of a direct message's participants.
func conversationName(c store.ChannelProgress) string {
	if store.IsChatKind(c.Kind) {
		return c.TargetName
	}
	return "#" + c.SlackName
}

// finalizeConsequences is what the confirmation says finalizing does. It is
// the last thing an operator reads before the one step that cannot be
// undone, so it says who gets access.
//
// channels and chats say what is being finalized; channelsLeft that some
// channels stay locked, in which case their teams get no members yet.
func finalizeConsequences(legacy, channels, chats, channelsLeft bool) string {
	s := "This completes migration mode. It cannot be undone, and nothing more can be imported afterwards. "
	if chats {
		s += "The chats of direct messages are already visible to their participants; finalizing only ends their migration mode and adds nobody. "
	}
	if !channels {
		// Only chats: no channel is opened and no team gets anybody.
		return strings.TrimSpace(s + "No channel is opened and no members are added.")
	}
	if legacy {
		s += "With the legacy strategy the team's own migration is completed as well, once all of its channels are: " +
			"only then does the team get its owner and members, and a completed team cannot be put back into migration mode. "
	}
	members := "Each team then gets its members: "
	if channelsLeft {
		members = "A team gets its members once all of its channels are finalized, so a team with a channel left locked gets none yet. Its members are "
	}
	return s + members + "everyone in its channels in the export who has a Microsoft 365 account, except guests, bots and deactivated users. " +
		"Members of a private channel are added to that channel, and to its team, because Teams requires that."
}

// finalizeSummary renders the closing lines of `taut finalize`.
func finalizeSummary(t theme.Theme, v controller.RunView, elapsed time.Duration, members int, haveMembers bool, logOK *bool, width int) string {
	var b strings.Builder
	p := v.Progress
	open, done, chats, skipped := 0, 0, 0, 0
	if p != nil {
		for _, c := range p.Channels {
			switch {
			case c.State == store.ChannelSkipped:
				skipped++
			case c.State != store.ChannelCompleted:
				open++
			case store.IsChatKind(c.Kind):
				chats++
			default:
				done++
			}
		}
	}
	facts := []string{countOf(done, "channel", "channels") + " open"}
	partly := "Channels finalized"
	if done == 0 && chats > 0 {
		// Only chats so far: no channel was opened, and saying "0 channels
		// open" under "Channels finalized" would read as a failure.
		facts, partly = nil, "Chats finalized"
	}
	if chats > 0 {
		// A chat was never closed to anybody; it is out of migration mode.
		facts = append(facts, countOf(chats, "chat", "chats")+" out of migration mode")
	}
	if skipped > 0 {
		facts = append(facts, countOf(skipped, "direct-message conversation", "direct-message conversations")+" skipped")
	}
	if elapsed > time.Second {
		facts = append(facts, checklist.Elapsed(elapsed))
	}
	switch v.Status {
	case store.RunFinalized:
		b.WriteString(ui.Verdict(t, theme.StatusOK, "Migration finalized", facts...))
	case store.RunCompleted:
		b.WriteString(ui.Verdict(t, theme.StatusOK, partly, append(facts, countOf(open, "conversation", "conversations")+" still in migration mode")...))
	case store.RunFailed:
		b.WriteString(ui.Verdict(t, theme.StatusFail, "Finalizing stopped", facts...))
	default:
		b.WriteString(ui.Verdict(t, theme.StatusInfo, "Finalizing", facts...))
	}

	rows := [][2]string{}
	if haveMembers {
		rows = append(rows, [2]string{"Members", comma(members) + " added"})
	}
	switch {
	case v.PurgedAt != nil:
		rows = append(rows, [2]string{"Staging", "deleted  the export and everything staged from it"})
	case v.Retain:
		rows = append(rows, [2]string{"Staging", "kept (--retain); it stays encrypted"})
	case v.Status == store.RunFinalized:
		rows = append(rows, [2]string{"Staging", "being deleted; `taut status` shows when it is gone"})
	}
	if logOK != nil {
		state := "verified  hash chain intact"
		if !*logOK {
			state = "FAILED verification; run `taut audit --verify` to investigate"
		}
		rows = append(rows, [2]string{"Audit log", state})
	}
	if len(rows) > 0 {
		b.WriteString("\n" + ui.Indent(ui.KV(t, rows), 2))
	}
	if p != nil {
		for _, pr := range p.Problems {
			if pr.Status == string(store.LaneFailed) {
				b.WriteString("\n" + ui.Finding(t, theme.StatusFail, pr.LaneID+": "+pr.Error, "", width))
			}
		}
	}
	switch v.Status {
	case store.RunFinalized:
		b.WriteString("\n\n" + ui.Next(t, "taut audit --export audit.ndjson", "keep the audit trail with your records"))
	case store.RunCompleted:
		b.WriteString("\n\n" + ui.Next(t, "taut finalize", "finalize the remaining channels when they are ready"))
	case store.RunFailed:
		b.WriteString("\n" + ui.Hint(t, "Fix the cause above, then run `taut finalize` again. It continues where it stopped.", width))
	}
	return b.String()
}

// releaseChats takes the chats of a run that failed or was cancelled out of
// migration mode. The import is not finalized: it did not finish. But a chat
// is live in front of its participants from the moment it is created, and
// one left in migration mode stays in it, so this much can be asked for on
// its own.
func (a *App) releaseChats(ctx context.Context, m *config.Config, client *controller.Client, run controller.RunView, ids []string, detach bool) error {
	out := a.IO
	t, w := out.Theme, out.Width-ui.Margin
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	open, posted := 0, 0
	if run.Progress != nil {
		for _, c := range run.Progress.Channels {
			if want[c.SlackID] && releasable(c) {
				open++
				posted += c.Posted
			}
		}
	}
	// Work queued earlier (by the cancellation itself, or by an earlier
	// request) may still be under way; then there is something to wait for
	// although nothing is asked.
	pending := func(v controller.RunView) (left, failed int) {
		s, ok := stageOf(v.Progress, store.StageRelease)
		if !ok {
			return 0, 0
		}
		return s.Lanes - s.Done - s.Failed, s.Failed
	}
	left, failed := pending(run)
	if open == 0 && left == 0 && failed == 0 {
		if out.IsJSON() {
			return out.JSON(map[string]any{"run": run, "changed": false, "released": 0})
		}
		out.Println(ui.Pad("\n" + ui.Verdict(t, theme.StatusOK, "No chat of this run is in migration mode",
			"run "+run.ID+" is "+string(run.Status)) + "\n\n" + ui.Next(t, "taut migrate", "import again")))
		return nil
	}
	if open > 0 || failed > 0 {
		if !out.Yes {
			if !out.Interactive() {
				return fail.New("finalize.unconfirmed", "Ending migration mode needs confirmation",
					"It cannot be undone: nothing more can be imported into these chats with its original date and author.",
					"Run `taut finalize --only direct-messages --yes`.").WithExit(fail.ExitBlockers)
			}
			var b strings.Builder
			b.WriteString("\n" + ui.Banner(t, "finalize", m.Metadata.Name) + "\n\n")
			b.WriteString(ui.KV(t, [][2]string{
				{"Run", run.ID + "  " + string(run.Status) + "; its import is not finalized"},
				{"Direct messages", countOf(open, "chat", "chats") + " to take out of migration mode  already visible to their participants; nobody is added"},
				{"Imported", countOf(posted, "post", "posts") + " in them, which stay"},
			}))
			out.Println(ui.Pad(b.String()))
			out.Println()
			proceed := false
			err := huh.NewForm(huh.NewGroup(huh.NewConfirm().
				Title(fmt.Sprintf("End migration mode on %s?", countOf(open, "chat", "chats"))).
				Description("The import of this run did not finish and is not finalized. This only ends migration mode on its chats, so that they are ordinary chats again. " +
					"It cannot be undone: nothing more can be imported into them with its original date and author. What was imported stays.").
				Affirmative("End migration mode").Negative("Not yet").Value(&proceed))).
				WithTheme(forms.Theme(out.Theme)).WithInput(out.In).RunWithContext(ctx)
			if err != nil || !proceed {
				return fail.Exit(fail.ExitOK, "not changed") //nolint:nilerr
			}
		}
		mv, err := client.Finalize(ctx, run.ID, controller.FinalizeRequest{Channels: ids})
		if err != nil {
			return err
		}
		run = mv.Run
	}
	if detach {
		if out.IsJSON() {
			return out.JSON(map[string]any{"run": run, "changed": true})
		}
		out.Println(ui.Pad("\n" + ui.Verdict(t, theme.StatusInfo, "Ending migration mode on the chats", run.ID) +
			"\n\n" + ui.Next(t, "taut status", "see when it is done")))
		return nil
	}
	// A handful of requests per chat: this is quick. Wait for it, so the
	// report says what happened rather than what was asked for.
	for {
		if left, failed = pending(run); left == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		v, err := client.Run(ctx, run.ID)
		if err != nil {
			return err
		}
		run = v
	}
	released, still := 0, 0
	if run.Progress != nil {
		for _, c := range run.Progress.Channels {
			switch {
			case !want[c.SlackID] || !store.IsChatKind(c.Kind):
			case c.State == store.ChannelCompleted:
				released++
			case releasable(c):
				still++
			}
		}
	}
	if out.IsJSON() {
		if err := out.JSON(map[string]any{"run": run, "changed": true, "released": released, "stillInMigrationMode": still}); err != nil {
			return err
		}
	} else {
		var b strings.Builder
		if still == 0 && failed == 0 {
			b.WriteString(ui.Verdict(t, theme.StatusOK, "Chats out of migration mode", countOf(released, "chat", "chats"), "run "+run.ID+" stays "+string(run.Status)))
		} else {
			b.WriteString(ui.Verdict(t, theme.StatusFail, "Some chats are still in migration mode", countOf(still, "chat", "chats"), countOf(released, "chat", "chats")+" out of it"))
			if run.Progress != nil {
				for _, pr := range run.Progress.Problems {
					if pr.Status == string(store.LaneFailed) && strings.HasPrefix(pr.LaneID, store.StageRelease+":") {
						b.WriteString("\n" + ui.Finding(t, theme.StatusFail, pr.LaneID+": "+pr.Error, "", w))
					}
				}
			}
			b.WriteString("\n" + ui.Hint(t, "Fix the cause above, then run `taut finalize --only direct-messages` again.", w))
		}
		if still == 0 && failed == 0 {
			b.WriteString("\n\n" + ui.Next(t, "taut migrate --new", "start the import again; it uses the same chats and does not import a message twice"))
		}
		out.Println(ui.Pad("\n" + b.String()))
	}
	if still > 0 || failed > 0 {
		return fail.Exit(fail.ExitFailure, "some chats are still in migration mode")
	}
	return nil
}

// finalizeRows is what the confirmation lists: what is finalized, what is
// left as it is, and what becomes of the staged data. Channels and chats are
// counted apart throughout: a channel left out stays locked and its team
// without members, a chat left out is in nobody's way.
func finalizeRows(scope finalizeScope, legacy, retain bool) [][2]string {
	rows := [][2]string{}
	if n := scope.Open - scope.OpenChats; n > 0 || scope.OpenChats == 0 {
		rows = append(rows, [2]string{"Channels", fmt.Sprintf("%s to open in %s", comma(n), countOf(len(scope.Teams), "team", "teams"))})
	}
	if scope.OpenChats > 0 {
		// Nothing opens here: the participants have seen
		// the chat since the import began.
		rows = append(rows, [2]string{"Direct messages", countOf(scope.OpenChats, "chat", "chats") + " to take out of migration mode  already visible to their participants; nobody is added"})
	}
	rows = append(rows, [2]string{"Imported", countOf(scope.Posted, "post", "posts")})
	if scope.Remaining > 0 {
		rows = append(rows, [2]string{"Left locked", countOf(scope.Remaining, "channel", "channels") + "  still importable; their teams get members later"})
	}
	if scope.RemainingChats > 0 {
		// Not locked: their participants see and use them.
		rows = append(rows, [2]string{"Left in migration mode", countOf(scope.RemainingChats, "chat", "chats") + " of direct messages  still importable; visible to their participants as before"})
	}
	if scope.Skipped > 0 {
		rows = append(rows, [2]string{"Skipped", countOf(scope.Skipped, "direct-message conversation", "direct-message conversations") + "  Microsoft refused a chat for them; nothing to finalize"})
	}
	if legacy {
		team := "completed too, once all of its channels are; it has no members until then"
		if scope.Remaining > 0 {
			team = "stays in migration mode, without members, until its other channels are finalized"
		}
		rows = append(rows, [2]string{"Team", team})
	}
	if retain {
		rows = append(rows, [2]string{"Staging", "kept (--retain)"})
	} else {
		rows = append(rows, [2]string{"Staging", "deleted afterwards: the export and everything staged from it"})
	}
	return rows
}

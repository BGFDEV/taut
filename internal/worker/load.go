package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/graph"
	"github.com/bgfdev/taut/internal/pipeline"
	"github.com/bgfdev/taut/internal/store"
)

// Audit log operations.
const (
	opTeamCreate     = "team.create"
	opTeamAdopt      = "team.adopt"
	opChannelCreate  = "channel.create"
	opChannelAdopt   = "channel.adopt"
	opStartMigration = "channel.start_migration"
	opImport         = "message.import"
	opAdopt          = "message.adopt"
	opFail           = "message.fail"
	opHold           = "message.hold"
)

func (d *Worker) entry(lease *store.Lease, op, source, target, hash string) store.LogEntry {
	return store.LogEntry{
		RunID: lease.RunID, Stream: lease.LaneID, At: time.Now().UTC(), Actor: d.cfg.Holder,
		Op: op, SourceID: source, TargetID: target, ContentHash: hash, Outcome: "ok",
	}
}

// team creates the target team, or adopts an existing one with the same
// name.
//
// Creating a team cannot be made strictly idempotent: Graph accepts the
// request and returns the new ID, and if the worker dies between those two
// events the ID is lost. Looking for a team by name first closes that
// window whenever the app can list teams (the recommended
// Team.ReadBasic.All permission); without it a crash in that window can
// leave an extra, empty team behind, which the audit log will not know about.
func (d *Worker) team(ctx context.Context, rs *runState, lease *store.Lease, b *store.Batch) error {
	idx, err := strconv.Atoi(lease.Key)
	if err != nil {
		return permanent(fmt.Errorf("team lane has a bad key %q", lease.Key))
	}
	teams, err := d.cfg.Store.Teams(ctx, rs.run.ID)
	if err != nil {
		return err
	}
	var tm *store.TeamMapping
	for i := range teams {
		if teams[i].Index == idx {
			tm = &teams[i]
		}
	}
	if tm == nil {
		return permanent(fmt.Errorf("team %d is not in the plan", idx))
	}
	done := func() error {
		return d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error { return tx.CompleteBatch(ctx, b.Seq, "", 1, nil) })
	}
	legacy := isLegacy(rs)
	g, err := d.client(ctx, rs)
	if err != nil {
		return err
	}
	if tm.TeamsID != "" {
		// The ID was recorded by an earlier attempt, possibly before the
		// team finished provisioning. It is done when Graph can return it.
		if _, err := g.GetTeam(ctx, tm.TeamsID); err != nil {
			return fmt.Errorf("team %q is not ready yet: %w", tm.Name, err)
		}
		return done()
	}

	teamsID, adopted := "", false
	existing, err := g.FindTeams(ctx, tm.Name)
	switch {
	case err != nil && fail.ExitCode(err) == fail.ExitPermission:
		// The app cannot list teams; proceed to create (see above).
		d.cfg.Log.Warn("cannot look for an existing team; creating one", "team", tm.Name, "error", describe(err))
	case err != nil:
		return err
	case len(existing) == 1 && legacy:
		// The legacy strategy can only import into a team that is in
		// migration mode, and a team can only be created that way. One
		// that is found by name and is in migration mode is taken to be
		// the team an interrupted attempt of this stage created: Graph
		// accepted it and the worker died before recording its ID.
		migrating, err := teamInMigration(ctx, g, existing[0].ID)
		if err != nil {
			return err
		}
		if !migrating {
			return permanent(errLegacyTeamExists(tm.Name))
		}
		teamsID, adopted = existing[0].ID, true
	case len(existing) == 1:
		teamsID, adopted = existing[0].ID, true
	case len(existing) > 1:
		return permanent(fail.New("team.ambiguous",
			fmt.Sprintf("%d teams are already named %q", len(existing), tm.Name),
			"Taut cannot tell which one to import into.",
			"Rename the team in the config file (target.teams[].name), or rename the extra teams in Teams, then run `taut migrate`."))
	}
	var createErr error
	if teamsID == "" {
		// CreateTeam returns the new ID even when provisioning then fails
		// or times out. Record it whatever happened, so a retry waits for
		// that team instead of creating another.
		spec := graph.TeamSpec{DisplayName: tm.Name, Description: tm.Description, Visibility: tm.Visibility, OwnerID: tm.OwnerID}
		if legacy {
			// In migration mode, older than anything that will be
			// imported into it. It has no members, not even its owner,
			// until `taut finalize` completes its migration.
			start, err := d.teamStart(ctx, rs, idx)
			if err != nil {
				return err
			}
			spec.MigrationCreatedAt = &start
		}
		teamsID, createErr = g.CreateTeam(ctx, spec)
		if teamsID == "" {
			return createErr
		}
	}
	op, detail := opTeamCreate, ""
	switch {
	case adopted && legacy:
		op, detail = opTeamAdopt, "found in migration mode; taken to be the team an interrupted attempt created"
	case adopted:
		op = opTeamAdopt
	case legacy:
		detail = "created in migration mode"
	}
	err = d.cfg.Store.Fenced(context.WithoutCancel(ctx), lease, func(tx store.Tx) error {
		c := context.WithoutCancel(ctx)
		if err := tx.SetTeam(c, idx, teamsID, adopted); err != nil {
			return err
		}
		e := d.entry(lease, op, "team/"+lease.Key, teamsID, "")
		e.Detail = detail
		if createErr != nil {
			e.Detail = "created; provisioning not confirmed yet"
		}
		if err := tx.Log(c, e); err != nil {
			return err
		}
		if createErr != nil {
			// Leave the batch pending; the retry confirms the team.
			return nil //nolint:nilerr // createErr is returned after the commit
		}
		return tx.CompleteBatch(c, b.Seq, "", 1, nil)
	})
	if err != nil {
		return err
	}
	return createErr
}

// isLegacy reports whether the run uses the legacy load strategy: teams and
// channels created in migration mode, with no startMigration.
func isLegacy(rs *runState) bool { return rs.m.Load.Strategy == config.StrategyLegacy }

// teamsGeneral is the channel every team is created with.
const teamsGeneral = "General"

// teamInMigration reports whether a team is in migration mode. Graph's team
// resource does not say; its General channel does, and General is in
// migration mode exactly when the team was created that way and has not
// been completed. (That General of such a team starts in migration mode is
// what Microsoft's sample implies, not something its reference states.)
func teamInMigration(ctx context.Context, g Graph, teamID string) (bool, error) {
	general, found, err := g.FindChannel(ctx, teamID, teamsGeneral)
	if err != nil {
		return false, err
	}
	return found && general.MigrationMode == graph.MigrationInProgress, nil
}

func errLegacyTeamExists(name string) error {
	return fail.New("team.exists_legacy",
		fmt.Sprintf("A team named %q already exists, and the legacy load strategy cannot import into an existing team", name),
		"With load.strategy: legacy a team can only receive history if it was created in migration mode. An existing team is not, and startMigration, which could put its channels into migration mode, is not available with this strategy.",
		"Choose a name no team has yet (target.teams[].name), or delete the existing team in Teams, then run `taut migrate --new`. In the global cloud, load.strategy: modern can import into an existing team.")
}

// errLeftMigration is the failure of a legacy channel that is no longer in
// migration mode before its import finished.
func errLeftMigration(target string) error {
	return fail.New("load.left_migration_legacy",
		fmt.Sprintf("Channel %q left migration mode before its import finished, and the legacy load strategy cannot reopen it", target),
		"Something completed the channel's migration. With load.strategy: legacy a channel is in migration mode only from its creation until that is completed; there is no way back in, so the rest of its history cannot be imported with original dates and authors.",
		"What was imported so far is intact. To import the channel in full, delete the team in Teams and run `taut migrate --new`.")
}

// teamStart picks the creation time of a team created in migration mode:
// before the creation time every one of its channels will be given, since
// the team's General channel is as old as the team and nothing can be
// imported that is older than the channel it goes into.
func (d *Worker) teamStart(ctx context.Context, rs *runState, index int) (time.Time, error) {
	channels, err := d.cfg.Store.Channels(ctx, rs.run.ID)
	if err != nil {
		return time.Time{}, err
	}
	var start time.Time
	for _, cm := range channels {
		if cm.TeamIndex != index {
			continue
		}
		at, err := d.channelStart(ctx, rs.run.ID, cm)
		if err != nil {
			return time.Time{}, err
		}
		if start.IsZero() || at.Before(start) {
			start = at
		}
	}
	if start.IsZero() {
		start = defaultStart
	}
	return start.Add(-time.Minute), nil
}

// createLegacyChannel is createChannel under the legacy strategy: the
// channel is created already in migration mode, with a creation time before
// its oldest message, and General (which the team was created with) is
// found rather than created. Either way the channel is recorded as
// migrating: there is no separate step that opens it.
func (d *Worker) createLegacyChannel(ctx context.Context, rs *runState, lease *store.Lease, b *store.Batch, g Graph, teamID string, cm store.ChannelMapping) error {
	if cm.Kind == config.ChannelPrivate {
		return permanent(fmt.Errorf("private channel %q cannot be migrated with the legacy load strategy", cm.SlackName))
	}
	want, err := d.channelStart(ctx, rs.run.ID, cm)
	if err != nil {
		return err
	}
	ch, found, err := g.FindChannel(ctx, teamID, cm.TargetName)
	if err != nil {
		return err
	}
	op, detail := opChannelAdopt, "already in migration mode"
	if !found {
		ch, err = g.CreateChannel(ctx, teamID, graph.ChannelSpec{
			DisplayName: cm.TargetName, Description: "Migrated from Slack #" + cm.SlackName,
			MembershipType: "standard", MigrationCreatedAt: &want,
		})
		if graph.IsChannelExists(err) {
			// Created by an earlier attempt whose response was lost.
			ch, found, err = g.FindChannel(ctx, teamID, cm.TargetName)
			if err == nil && !found {
				err = errors.New("the channel is reported to exist but cannot be found")
			}
		} else {
			op, detail = opChannelCreate, "created in migration mode"
		}
		if err != nil {
			return err
		}
	}
	// Trust what Graph reports, not what was asked for.
	ch, err = g.GetChannel(ctx, teamID, ch.ID)
	if err != nil {
		return err
	}
	if ch.MigrationMode != graph.MigrationInProgress {
		return permanent(fail.New("load.channel_not_migrating_legacy",
			fmt.Sprintf("Channel %q exists in the team but is not in migration mode", cm.TargetName),
			"With load.strategy: legacy a channel can only receive history if it was created in migration mode, and an existing channel cannot be put into it.",
			"Import into a new channel instead: rename it in the config file (target.teams[].channels.rename), then run `taut migrate --new`."))
	}
	if !ch.CreatedDateTime.IsZero() && ch.CreatedDateTime.After(want) {
		return permanent(fail.New("load.channel_too_new",
			fmt.Sprintf("Channel %q in Teams is newer than the Slack history to import", cm.TargetName),
			fmt.Sprintf("Its creation time is %s, the history starts before %s, and with load.strategy: legacy a creation time cannot be moved back.",
				ch.CreatedDateTime.UTC().Format(time.RFC3339), want.UTC().Format(time.RFC3339)),
			"Delete the team in Teams and run `taut migrate --new` so it is created old enough."))
	}
	start := want
	if !ch.CreatedDateTime.IsZero() {
		start = ch.CreatedDateTime.UTC()
	}
	return d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error {
		if err := tx.SetChannel(ctx, cm.SlackID, ch.ID, store.ChannelMigrating, &start); err != nil {
			return err
		}
		e := d.entry(lease, op, "channel/"+cm.SlackID, ch.ID, "")
		e.Detail = detail
		if err := tx.Log(ctx, e); err != nil {
			return err
		}
		return tx.CompleteBatch(ctx, b.Seq, "", 1, nil)
	})
}

// createChannel is the first batch of a channel's load lane: make sure the
// channel exists in Teams. It is safe to repeat.
func (d *Worker) createChannel(ctx context.Context, rs *runState, lease *store.Lease, b *store.Batch) error {
	cm, err := d.cfg.Store.Channel(ctx, rs.run.ID, lease.Key)
	if err != nil {
		return err
	}
	done := func() error {
		return d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error { return tx.CompleteBatch(ctx, b.Seq, "", 1, nil) })
	}
	if cm.IsChat() {
		return d.createChat(ctx, rs, lease, b, cm)
	}
	if cm.TeamsID != "" {
		return done()
	}
	team, err := d.teamMapping(ctx, rs, cm.TeamIndex)
	if err != nil {
		return err
	}
	teamID := team.TeamsID
	if teamID == "" {
		return fmt.Errorf("team %d has not been created yet", cm.TeamIndex)
	}
	g, err := d.client(ctx, rs)
	if err != nil {
		return err
	}
	if isLegacy(rs) {
		return d.createLegacyChannel(ctx, rs, lease, b, g, teamID, cm)
	}

	ch, found, err := g.FindChannel(ctx, teamID, cm.TargetName)
	if err != nil {
		return err
	}
	op := opChannelAdopt
	if !found {
		spec := graph.ChannelSpec{DisplayName: cm.TargetName, Description: "Migrated from Slack #" + cm.SlackName, MembershipType: "standard"}
		if cm.Kind == config.ChannelPrivate {
			// A private channel created by an app needs an owner, who
			// must be in the team: the team's owner. Its Slack members
			// are added at `taut finalize`.
			spec.MembershipType, spec.OwnerID = "private", team.OwnerID
		}
		ch, err = g.CreateChannel(ctx, teamID, spec)
		if graph.IsNotTeamMember(err) {
			return permanent(fail.New("load.private_owner_not_in_team",
				fmt.Sprintf("Private channel %q cannot be created: %s is not a member of team %q", cm.TargetName, team.OwnerUPN, team.Name),
				"A private channel needs an owner who is already in the team. Taut imported into an existing team that its configured owner does not belong to.",
				"Add "+team.OwnerUPN+" to the team in Teams, or set target.teams[].owner to one of its members and run `taut migrate --new`. Then run `taut migrate` again."))
		}
		if graph.IsChannelExists(err) {
			// Created by an earlier attempt whose response was lost.
			ch, found, err = g.FindChannel(ctx, teamID, cm.TargetName)
			if err == nil && !found {
				err = errors.New("the channel is reported to exist but cannot be found")
			}
		} else {
			op = opChannelCreate
		}
		if err != nil {
			return err
		}
	}
	return d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error {
		if err := tx.SetChannel(ctx, cm.SlackID, ch.ID, store.ChannelCreated, nil); err != nil {
			return err
		}
		if err := tx.Log(ctx, d.entry(lease, op, "channel/"+cm.SlackID, ch.ID, "")); err != nil {
			return err
		}
		return tx.CompleteBatch(ctx, b.Seq, "", 1, nil)
	})
}

// openChannel is the second batch: put the channel in migration mode,
// backdated to before its oldest message. It runs once the channel's files
// are uploaded. Under the legacy strategy the channel was created in
// migration mode and there is nothing to do.
func (d *Worker) openChannel(ctx context.Context, rs *runState, lease *store.Lease, b *store.Batch) error {
	cm, err := d.cfg.Store.Channel(ctx, rs.run.ID, lease.Key)
	if err != nil {
		return err
	}
	if cm.TeamsID == "" && cm.State != store.ChannelSkipped {
		return fmt.Errorf("%s has not been created yet", conversation(cm))
	}
	if cm.IsChat() {
		if cm.State != store.ChannelMigrating && cm.State != store.ChannelSkipped {
			g, err := d.client(ctx, rs)
			if err != nil {
				return err
			}
			_, err = d.ensureChatMigrating(ctx, rs, lease, g, cm)
			var refused *chatRefusal
			if errors.As(err, &refused) {
				return d.skipChat(ctx, lease, b, cm, refused)
			}
			if err != nil {
				return err
			}
		}
		return d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error { return tx.CompleteBatch(ctx, b.Seq, "", 1, nil) })
	}
	if isLegacy(rs) && cm.State != store.ChannelMigrating {
		// A legacy channel is in migration mode from its creation; this
		// batch only keeps the ledger the same shape for both strategies.
		// If the channel is not migrating any more, nothing can reopen it.
		return permanent(errLeftMigration(cm.TargetName))
	}
	if cm.State != store.ChannelMigrating {
		teamID, err := d.teamID(ctx, rs, cm.TeamIndex)
		if err != nil {
			return err
		}
		g, err := d.client(ctx, rs)
		if err != nil {
			return err
		}
		if err := d.ensureMigrating(ctx, rs, lease, g, teamID, cm); err != nil {
			return err
		}
	}
	return d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error { return tx.CompleteBatch(ctx, b.Seq, "", 1, nil) })
}

// ensureMigrating puts a channel that exists into migration mode with a
// creation time before its oldest message, and records that. It is safe to
// repeat, and it is also how a channel is reopened if something completed
// its migration mid-import.
func (d *Worker) ensureMigrating(ctx context.Context, rs *runState, lease *store.Lease, g Graph, teamID string, cm store.ChannelMapping) error {
	want, err := d.channelStart(ctx, rs.run.ID, cm)
	if err != nil {
		return err
	}
	ch, err := g.GetChannel(ctx, teamID, cm.TeamsID)
	if err != nil {
		return err
	}
	// Graph only moves a channel's creation time backwards, and only when
	// migration mode starts. needsBackdate says the channel is still newer
	// than the history about to be imported.
	needsBackdate := ch.CreatedDateTime.IsZero() || want.Before(ch.CreatedDateTime)

	if ch.MigrationMode == graph.MigrationInProgress && needsBackdate {
		// Already in migration mode (an earlier run, say) but not far
		// enough back. Migration mode is re-entrant, so close and reopen
		// it with the earlier time.
		if err := g.CompleteChannelMigration(ctx, teamID, cm.TeamsID); err != nil && !graph.IsNotInMigration(err) {
			return err
		}
		ch.MigrationMode = ""
	}
	if ch.MigrationMode != graph.MigrationInProgress {
		var when *time.Time
		if needsBackdate {
			when = &want
		}
		if err := g.StartMigration(ctx, teamID, cm.TeamsID, when); err != nil && !graph.IsAlreadyMigrating(err) {
			return err
		}
	}

	// Trust what Graph now reports, not what was asked for: every message
	// must be later than the channel's creation time, so proceeding on a
	// wrong assumption would get the whole channel refused.
	ch, err = g.GetChannel(ctx, teamID, cm.TeamsID)
	if err != nil {
		return err
	}
	if ch.MigrationMode != graph.MigrationInProgress {
		return fmt.Errorf("channel %q did not enter migration mode", cm.TargetName)
	}
	if !ch.CreatedDateTime.IsZero() && ch.CreatedDateTime.After(want) {
		return permanent(fail.New("load.channel_too_new",
			fmt.Sprintf("Channel %q in Teams is newer than the Slack history to import", cm.TargetName),
			fmt.Sprintf("Its creation time is %s and could not be moved back to %s.",
				ch.CreatedDateTime.UTC().Format(time.RFC3339), want.UTC().Format(time.RFC3339)),
			"Import into a new channel instead: rename it in the config file (target.teams[].channels.rename), then run `taut migrate --new`."))
	}
	start := want
	if !ch.CreatedDateTime.IsZero() {
		start = ch.CreatedDateTime.UTC()
	}
	return d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error {
		if err := tx.SetChannel(ctx, cm.SlackID, cm.TeamsID, store.ChannelMigrating, &start); err != nil {
			return err
		}
		return tx.Log(ctx, d.entry(lease, opStartMigration, "channel/"+cm.SlackID, cm.TeamsID, ""))
	})
}

// defaultStart is the creation time given to a channel the export says
// nothing about: no creation time and no messages.
var defaultStart = time.Date(2010, 1, 1, 0, 0, 0, 0, time.UTC)

// channelStart picks the creation time to give the channel: comfortably
// before its oldest message, since Graph rejects an imported message that is
// not later than its channel.
func (d *Worker) channelStart(ctx context.Context, runID string, cm store.ChannelMapping) (time.Time, error) {
	var candidates []time.Time
	if !cm.Created.IsZero() {
		candidates = append(candidates, cm.Created)
	}
	// The first day batch names the oldest day with messages. Day files are
	// bucketed in a timezone the export does not state, hence a full day's
	// margin. A channel can also be newer in Slack's records than its first
	// message, which is why both are considered.
	first, err := d.cfg.Store.GetBatch(ctx, runID, store.BatchRef{LaneID: lane(store.StageLoad, cm.SlackID), Seq: loadSeqDays})
	switch {
	case err == nil:
		if day, perr := config.ParseDate(first.Key); perr == nil {
			candidates = append(candidates, day.Add(-24*time.Hour))
		}
	case !errors.Is(err, store.ErrNotFound):
		// Guessing here could start migration mode too late, and that
		// cannot be undone cheaply. Retry instead.
		return time.Time{}, err
	}
	if len(candidates) == 0 {
		return defaultStart, nil
	}
	start := candidates[0]
	for _, c := range candidates[1:] {
		if c.Before(start) {
			start = c
		}
	}
	return start.Add(-time.Minute).Truncate(time.Second).UTC(), nil
}

// teamMapping returns a team of the run's plan as the ledger has it now.
func (d *Worker) teamMapping(ctx context.Context, rs *runState, index int) (store.TeamMapping, error) {
	teams, err := d.cfg.Store.Teams(ctx, rs.run.ID)
	if err != nil {
		return store.TeamMapping{}, err
	}
	for _, t := range teams {
		if t.Index == index {
			return t, nil
		}
	}
	return store.TeamMapping{}, permanent(fmt.Errorf("team %d is not in the plan", index))
}

func (d *Worker) teamID(ctx context.Context, rs *runState, index int) (string, error) {
	t, err := d.teamMapping(ctx, rs, index)
	if err != nil {
		return "", err
	}
	if t.TeamsID == "" {
		return "", fmt.Errorf("team %d has not been created yet", index)
	}
	return t.TeamsID, nil
}

// rejected reports whether a Graph error means this one post can never be
// imported as it is, as opposed to something being wrong with the channel
// or the service. Only errors that are unmistakably about the message
// qualify; anything ambiguous is treated as a problem with the lane, so it
// is retried and, if it persists, stops the lane where an operator will see
// it, rather than quietly failing message after message.
func rejected(err error, isReply bool) bool {
	switch code(err) {
	case "graph.payload_too_large", "graph.invalid_author":
		return true
	case "graph.not_found":
		// For a reply this is its parent having gone. For a top-level
		// post it can only be the channel or team, which is not about the
		// message at all.
		return isReply
	}
	return false
}

// maxRejectedRun is how many posts in a row Graph may refuse before the
// lane stops. A few bad messages are normal; an unbroken run of refusals
// means the channel, not the messages.
const maxRejectedRun = 5

// load imports one day of a channel's posts, in order.
//
// Exactly-once without server support. Graph has no idempotency key and an
// app cannot delete an imported message, but Graph does refuse a second
// message with the same millisecond timestamp in a channel. Every post was
// given a unique timestamp before any write (see transform), so:
//
//   - If the worker dies after Graph accepted a post but before recording
//     it, the retry's POST is refused with 409. The worker then reads the
//     existing message back, checks it is the one it meant (same timestamp,
//     same author) and adopts its ID.
//   - If two workers ever post the same message (one of them stale), one
//     wins and the other adopts. The stale one cannot record anything, since
//     its writes are fenced.
//   - A 409 that turns out to be someone else's message is reported as a
//     conflict; it is never overwritten or re-posted at a shifted time.
//
// Each post is recorded in its own transaction together with its audit log
// entry, so progress survives a crash at any point. The ledger row, not the
// staged file, is the authority for when a post happens and what it replies
// to; the staged file only supplies the body.
func (d *Worker) load(ctx context.Context, rs *runState, lease *store.Lease, b *store.Batch) (err error) {
	channelID := lease.Key
	recorded := 0
	defer func() { err = withProgress(err, recorded) }()
	cm, err := d.cfg.Store.Channel(ctx, rs.run.ID, channelID)
	if err != nil {
		return err
	}
	if cm.State == store.ChannelSkipped {
		return d.settleSkipped(ctx, rs, lease, b, cm)
	}
	if cm.TeamsID == "" {
		return fmt.Errorf("%s has not been created yet", conversation(cm))
	}
	// A direct message is imported into a chat: no team, no threads, no
	// files, and a creation time that may not reach back as far as the
	// history does. Everything else is the channel's path.
	isChat := cm.IsChat()
	where := fmt.Sprintf("%q", cm.TargetName)
	teamID := ""
	if isChat {
		where = chatLabel(cm)
	} else if teamID, err = d.teamID(ctx, rs, cm.TeamIndex); err != nil {
		return err
	}
	g, err := d.client(ctx, rs)
	if err != nil {
		return err
	}
	legacy := isLegacy(rs)
	// notBefore is the chat's creation time: Graph refuses anything that
	// is not later.
	var notBefore time.Time
	if isChat && cm.MigrationStart != nil {
		notBefore = *cm.MigrationStart
	}
	switch {
	case cm.State == store.ChannelMigrating:
	case isChat:
		// The chat fell out of migration mode after it was opened (see the
		// not-in-migration case below). Reopen it before importing.
		notBefore, err = d.ensureChatMigrating(ctx, rs, lease, g, cm)
		var refused *chatRefusal
		if errors.As(err, &refused) {
			if err := d.skipChat(ctx, lease, nil, cm, refused); err != nil {
				return err
			}
			cm.State, cm.SkipCode = store.ChannelSkipped, refused.code
			return d.settleSkipped(ctx, rs, lease, b, cm)
		}
		if err != nil {
			return err
		}
	case legacy:
		// The channel fell out of migration mode after it was opened, and
		// only the modern strategy can reopen it.
		return permanent(errLeftMigration(cm.TargetName))
	default:
		if err := d.ensureMigrating(ctx, rs, lease, g, teamID, cm); err != nil {
			return err
		}
	}
	// Day i of the channel is load batch loadSeqDays+i and transform batch i.
	in, err := d.inputKey(ctx, rs.run.ID, store.StageTransform, channelID, b.Seq-loadSeqDays)
	if err != nil {
		return err
	}
	posts, err := readBatch[pipeline.Post](ctx, rs.blobs, in)
	if err != nil {
		return err
	}
	limit := d.channelLimiter(lease.RunID + "/" + lease.LaneID)
	stats := map[string]int{}
	rejectedRun := 0
	// headless holds the messages of a chat whose earlier part was not
	// imported in this attempt. In a channel a later part hangs off the
	// first and is held without it; a chat has no such tie, so it is kept
	// here: a message never goes into a chat with its head missing.
	headless := map[string]bool{}
	// plain holds the posts whose mentions a chat refused; they are sent
	// with the names as plain text.
	plain := map[store.MessageKey]bool{}

posts:
	for _, p := range posts {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := store.MessageKey{ChannelID: channelID, SlackTS: p.SlackTS, Part: p.Part}
		source := fmt.Sprintf("%s/%s/%d", channelID, p.SlackTS, p.Part)
		row, err := d.cfg.Store.GetMessage(ctx, rs.run.ID, key)
		if err != nil {
			return err
		}
		if row.State == store.MessagePosted {
			stats["already_posted"]++
			continue
		}
		// Rows an earlier attempt marked failed or held are tried again
		// when the batch is: whatever stopped them may have been fixed.

		// The staged post must be the one the ledger allocated. A mismatch
		// would mean staging and the database disagree, and posting it
		// could put a message at the wrong time. A post of a chat may have
		// been moved off a millisecond an existing message held
		// (shiftPosts); the ledger says by how much, and its hash is still
		// the one of the allocation.
		if row.TargetMS-row.ShiftMS != p.TargetMS || row.Hash != p.Hash || (row.ShiftMS != 0 && !isChat) {
			return permanent(fmt.Errorf("staged post %s does not match the ledger", source))
		}
		p.TargetMS = row.TargetMS
		// What the audit log says about the content. For a chat, nothing:
		// a hash of a short private message can be guessed at, and the log
		// outlives everything else. The ledger keeps its own.
		auditHash := p.Hash
		if isChat {
			auditHash = ""
		}
		if isChat && p.Part > 0 && headless[p.SlackTS] {
			if row.State != store.MessageFailed {
				if err := d.settle(ctx, lease, key, store.MessageFailed, "earlier_part_not_imported",
					"an earlier part of the same message was not imported", opFail, source, auditHash); err != nil {
					return err
				}
			}
			stats["headless"]++
			continue
		}

		if isChat && !notBefore.IsZero() && !p.CreatedAt().After(notBefore) {
			// Older than the chat is, and the chat could not be made any
			// older. Graph would refuse it; say so without asking. Once
			// is enough: an attempt that was interrupted after recording
			// it does not record it again.
			headless[p.SlackTS] = true
			if row.State == store.MessageFailed && row.ErrorCode == "older_than_chat" {
				stats["older_than_chat"]++
				continue
			}
			if err := d.settle(ctx, lease, key, store.MessageFailed, "older_than_chat",
				"the message is older than the chat it belongs in, and the chat's creation time could not be moved back", opFail, source, auditHash); err != nil {
				return err
			}
			stats["older_than_chat"]++
			continue
		}

		target := graph.ImportTarget{TeamID: teamID, ChannelID: cm.TeamsID}
		if isChat {
			target = graph.ImportTarget{ChatID: cm.TeamsID}
		}
		if row.AnchorTS != "" && !isChat {
			anchor, err := d.cfg.Store.GetMessage(ctx, rs.run.ID, store.MessageKey{ChannelID: channelID, SlackTS: row.AnchorTS})
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if err != nil || anchor.State != store.MessagePosted {
				// Its thread has no anchor in Teams. Posting it at the
				// top level would misrepresent the conversation, so it
				// is held and reported instead.
				if err := d.settle(ctx, lease, key, store.MessageHeld, "anchor_missing",
					"the message it replies to was not imported", opHold, source, p.Hash); err != nil {
					return err
				}
				stats["held"]++
				continue
			}
			target.ReplyToID = anchor.TeamsID
		}

	send:
		// Every request passes the limiters, a post sent again after it
		// was moved included.
		if err := limit.Wait(ctx); err != nil {
			return err
		}
		if err := d.tenant.Wait(ctx); err != nil {
			return err
		}
		// A chat's files are not moved (where they should live is not
		// documented), so each becomes the note a file that could not be
		// moved gets, and nothing is attached or embedded.
		body, attachments, hosted, err := d.resolveFiles(ctx, rs, p, isChat)
		if err != nil {
			return err
		}
		if plain[key] {
			body = plainMentions(body)
		}
		msg := graph.ImportMessage{
			CreatedAt:      p.CreatedAt(),
			From:           graph.Identity{UserID: p.FromID, DisplayName: p.FromName},
			HTML:           body,
			Attachments:    attachments,
			HostedContents: hosted,
		}
		for _, mn := range p.Mentions {
			if plain[key] {
				break
			}
			msg.Mentions = append(msg.Mentions, graph.Mention{ID: mn.ID, Text: mn.Text, UserID: mn.UserID})
		}

		created, err := g.ImportMessage(ctx, target, msg)
		if isChat && graph.IsMentionInvalid(err) && !plain[key] {
			// The chat will not take a mention of someone who is not in
			// it. The message matters more than the mention: send it again
			// with the name as plain text.
			plain[key] = true
			stats["mentions_plain"]++
			goto send
		}
		if err != nil && len(p.Files) > 0 && aboutFiles(err) && !isChat {
			// Graph refused the post because of a file on it (an
			// attachment it will not accept, an image it cannot take).
			// The message matters more than the file: send it again with
			// its files reduced to notes, rather than block the channel.
			body, _, _, rerr := d.resolveFiles(ctx, rs, p, true)
			if rerr != nil {
				return rerr
			}
			msg.HTML, msg.Attachments, msg.HostedContents = body, nil, nil
			stats["files_dropped"]++
			created, err = g.ImportMessage(ctx, target, msg)
		}
		teamsID, adopted := created.ID, false
		switch {
		case err == nil:
		case graph.IsConflict(err):
			id, verdict, rerr := d.reconcile(ctx, g, target, p)
			if rerr != nil {
				return rerr
			}
			switch verdict {
			case occupantOurs:
				teamsID, adopted = id, true
			case occupantForeign:
				if isChat {
					// A chat people already use: somebody's own message
					// holds this millisecond. Move the post one later, in
					// the ledger first, together with whatever follows it
					// without a gap, and send it again, rather than lose it.
					moved, err := d.shiftPosts(ctx, lease, &row, &p)
					if err != nil {
						return err
					}
					if moved {
						stats["shifted"]++
						goto send
					}
					// It cannot be moved. Then none of the message goes
					// in from here on: not this part, and not the parts
					// after it on their own.
					if err := d.failRest(ctx, lease, p, "timestamp_conflict",
						"another message already occupies this timestamp in the chat, and the post could not be moved past it"); err != nil {
						return err
					}
					headless[p.SlackTS] = true
					stats["conflict"]++
					continue posts
				}
				if err := d.settle(ctx, lease, key, store.MessageFailed, "timestamp_conflict",
					"another message already occupies this timestamp in the "+target.Place(), opFail, source, auditHash); err != nil {
					return err
				}
				stats["conflict"]++
				continue posts
			default:
				// Graph says the timestamp is taken but the message
				// cannot be found yet. It may well be ours and not yet
				// readable; deciding now could record a message that is
				// in Teams as failed. Try the batch again later.
				return fmt.Errorf("post %s was refused as a duplicate, but the message holding its timestamp is not readable yet", source)
			}
		case graph.IsNotInMigration(err) && legacy && !isChat:
			// Something completed the channel's migration mid-import,
			// and a legacy channel cannot be put back into it.
			return permanent(errLeftMigration(cm.TargetName))
		case graph.IsNotInMigration(err):
			// Something completed the channel's migration mid-import.
			// Record that, so the next attempt reopens it.
			if serr := d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error {
				return tx.SetChannel(ctx, cm.SlackID, cm.TeamsID, store.ChannelCreated, nil)
			}); serr != nil {
				return serr
			}
			return fmt.Errorf("%s left migration mode during the import; it will be reopened", where)
		case rejected(err, target.ReplyToID != ""):
			if err := d.settle(ctx, lease, key, store.MessageFailed, code(err), describe(err), opFail, source, auditHash); err != nil {
				return err
			}
			stats["failed"]++
			headless[p.SlackTS] = true
			if isChat && code(err) == "graph.invalid_author" {
				// In a chat this is the expected fate of a message from
				// someone who is not in it (a bot, say), if Graph insists
				// on members. It says nothing about the chat, however many
				// there are in a row.
				continue
			}
			rejectedRun++
			if rejectedRun >= maxRejectedRun {
				return fmt.Errorf("%d posts in a row were refused by Microsoft Graph in %s; last reason: %s", rejectedRun, where, describe(err))
			}
			continue
		default:
			// Throttling that outlasted the client's retries, an outage,
			// expired credentials, or a refusal that is not clearly about
			// this message: the batch is retried later. Everything posted
			// so far is already recorded.
			return err
		}
		rejectedRun = 0

		op := opImport
		if adopted {
			op = opAdopt
			stats["adopted"]++
		} else {
			stats["posted"]++
		}
		err = d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error {
			if err := tx.MarkPosted(ctx, key, teamsID, adopted); err != nil {
				return err
			}
			e := d.entry(lease, op, source, teamsID, auditHash)
			e.Detail = shiftDetail(row.ShiftMS)
			return tx.Log(ctx, e)
		})
		if err != nil {
			return err
		}
		if adopted {
			d.cfg.Observer.Count(ctx, store.StageLoad, EventPostAdopted, 1)
		} else {
			d.cfg.Observer.Count(ctx, store.StageLoad, EventPostImported, 1)
		}
		recorded++
	}

	statsJSON, _ := json.Marshal(stats)
	return d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error {
		return tx.CompleteBatch(ctx, b.Seq, "", len(posts), statsJSON)
	})
}

// aboutFiles reports whether Graph refused a post because of a file on it
// rather than because of the message itself.
func aboutFiles(err error) bool {
	switch code(err) {
	case "graph.attachment_invalid", "graph.hosted_content_invalid", "graph.hosted_content_too_large", "graph.payload_too_large":
		return true
	}
	return false
}

// settle records a post that will not be imported, with its audit log entry.
func (d *Worker) settle(ctx context.Context, lease *store.Lease, key store.MessageKey, state store.MessageState, errCode, reason, op, source, hash string) error {
	err := d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error {
		if err := tx.MarkFailed(ctx, key, state, errCode, reason); err != nil {
			return err
		}
		e := d.entry(lease, op, source, "", hash)
		e.Outcome, e.Detail = "error", errCode
		return tx.Log(ctx, e)
	})
	if err == nil {
		event := EventPostRejected
		if state == store.MessageHeld {
			event = EventPostHeld
		}
		d.cfg.Observer.Count(ctx, store.StageLoad, event, 1)
	}
	return err
}

// occupant says whose message holds a contested timestamp.
type occupant int

const (
	occupantUnknown occupant = iota // could not be found (yet)
	occupantOurs                    // the post Taut meant to write
	occupantForeign                 // somebody else's message
)

// reconcile is called after Graph refused a post as a duplicate timestamp.
// It finds the message that holds the timestamp and says whose it is. When
// it is the post Taut meant to write, its ID is returned for adoption.
//
// A message is taken to be ours when it has exactly the allocated timestamp
// and the author Taut attributed it to. Graph may normalize HTML, so bodies
// are not compared. Within one run that is exact, because a run never
// allocates the same timestamp twice in a channel.
func (d *Worker) reconcile(ctx context.Context, g Graph, target graph.ImportTarget, p pipeline.Post) (string, occupant, error) {
	judge := func(m graph.ChatMessage) (string, occupant) {
		who := whose(m, p, target.IsChat())
		if who == occupantOurs {
			return m.ID, who
		}
		return "", who
	}

	// Graph's message IDs are the timestamp in epoch milliseconds in every
	// documented example, so try a direct read first.
	id := strconv.FormatInt(p.TargetMS, 10)
	var (
		found graph.ChatMessage
		err   error
	)
	switch {
	case target.IsChat():
		found, err = g.GetChatMessage(ctx, target.ChatID, id)
	case target.ReplyToID == "":
		found, err = g.GetMessage(ctx, target.TeamID, target.ChannelID, id)
	default:
		found, err = g.GetReply(ctx, target.TeamID, target.ChannelID, target.ReplyToID, id)
	}
	switch {
	case err == nil && found.CreatedAt.UnixMilli() == p.TargetMS:
		id, who := judge(found)
		return id, who, nil
	case err != nil && !graph.IsNotFound(err):
		return "", occupantUnknown, err
	}

	// That ID format is not guaranteed. Fall back to finding the message
	// by its timestamp.
	found, ok, err := g.FindMessageAt(ctx, target, p.CreatedAt())
	if err != nil {
		return "", occupantUnknown, err
	}
	if ok {
		id, who := judge(found)
		return id, who, nil
	}
	// Not found where this post would be. The timestamp may be held by a
	// message elsewhere in the channel (a reply's time can collide with a
	// message in another thread), or ours may not be readable yet. Either
	// way, do not decide.
	return "", occupantUnknown, nil
}

// whose says whose message m is, given that it holds the timestamp post p
// was refused at.
func whose(m graph.ChatMessage, p pipeline.Post, chat bool) occupant {
	// Object IDs are GUIDs, and Graph does not promise their case.
	if m.CreatedAt.UnixMilli() == p.TargetMS && m.FromUserID != "" && strings.EqualFold(m.FromUserID, p.FromID) {
		return occupantOurs
	}
	// A message in a chat with no author Graph will name (a system event,
	// a deleted user, an app) cannot be told from ours with certainty
	// either way. Unknown never moves a post and never adopts one; the
	// batch waits and looks again.
	if chat && strings.TrimSpace(m.FromUserID) == "" {
		return occupantUnknown
	}
	return occupantForeign
}

// channelLimiter returns the limiter for a load lane, creating it on first
// use, so the per-channel rate holds across the lane's batches.
func (d *Worker) channelLimiter(lane string) *limiter {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.limiters == nil {
		d.limiters = map[string]*limiter{}
	}
	l, ok := d.limiters[lane]
	if !ok {
		l = newLimiter(d.cfg.ChannelRPS)
		d.limiters[lane] = l
	}
	return l
}

// maxInlineImage is the largest image embedded in a message as hosted
// content, and maxInlineTotal the most embedded in one message. Anything
// beyond either is attached like any other file.
const (
	maxInlineImage = 1 << 20
	maxInlineTotal = 3 << 20
)

// noteNameBytes bounds the file name shown in a "not migrated" note. With
// it the note is never longer than the marker it replaces, which the
// transform sized for a 64-character ID (105 bytes), so swapping one for
// the other cannot push a post over the size limit.
const noteNameBytes = 64

// shortName makes a file name safe and short enough for a note: characters
// that HTML would have to escape are replaced, so the length is exact, and
// the name is cut on a character boundary.
func shortName(name string) string {
	name = strings.Map(func(r rune) rune {
		switch r {
		case '&', '<', '>', '"', '\'':
			return '_'
		}
		if r < 0x20 {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(name, "_"))
	if strings.TrimSpace(name) == "" {
		return "file"
	}
	if len(name) <= noteNameBytes {
		return name
	}
	cut := noteNameBytes - len("…")
	for cut > 0 && !utf8.RuneStart(name[cut]) {
		cut--
	}
	return name[:cut] + "…"
}

// resolveFiles turns the file markers in a post's HTML into what Graph
// needs. A file that was uploaded becomes a reference attachment (or, for a
// small image, inline hosted content). A file that could not be moved
// becomes a short note, so the message still says a file was there.
//
// Markers are matched as exact strings built from a validated file ID, and
// the transform never lets message text produce one, so user content cannot
// forge an attachment.
//
// With notesOnly, every marker becomes a note and nothing is attached: the
// fallback when Graph refuses a post because of its files.
func (d *Worker) resolveFiles(ctx context.Context, rs *runState, p pipeline.Post, notesOnly bool) (string, []graph.Attachment, []graph.HostedContent, error) {
	body := p.HTML
	if len(p.Files) == 0 {
		return body, nil, nil, nil
	}
	var (
		attachments []graph.Attachment
		hosted      []graph.HostedContent
		attached    = map[string]bool{}
		hostedBytes int64
	)
	attach := func(f store.FileMapping) string {
		if !attached[f.ETag] {
			attached[f.ETag] = true
			attachments = append(attachments, graph.Attachment{ID: f.ETag, Name: f.Name, ContentURL: f.WebDavURL})
		}
		return `<attachment id="` + f.ETag + `"></attachment>`
	}
	for _, id := range p.Files {
		if !fileIDPattern.MatchString(id) {
			continue
		}
		f, err := d.cfg.Store.GetFile(ctx, rs.run.ID, p.ChannelID, id)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return "", nil, nil, err
		}
		usable := !notesOnly && err == nil && f.State == store.FileUploaded && f.ETag != "" && f.WebDavURL != ""
		// The note must fit in the space the transform reserved for the
		// marker it replaces, or a post that was sized to fit could grow
		// past the limit; hence the short name.
		note := "<em>📎 " + shortName(f.Name) + " (not migrated)</em>"

		marker := `<attachment id="` + pipeline.FileMarkerPrefix + id + `"></attachment>`
		if strings.Contains(body, marker) {
			if usable {
				body = strings.ReplaceAll(body, marker, attach(f))
			} else {
				body = strings.ReplaceAll(body, marker, note)
			}
		}

		img := regexp.MustCompile(`<img src="` + regexp.QuoteMeta(pipeline.FileMarkerPrefix+id) + `" alt="([^"]*)">`)
		if m := img.FindStringSubmatch(body); m != nil {
			switch {
			case !usable:
				body = img.ReplaceAllLiteralString(body, note)
			case f.StagedBytes > 0 && f.StagedBytes <= maxInlineImage && f.StagingKey != "" && hostedBytes+f.StagedBytes <= maxInlineTotal:
				data, err := d.readStaged(ctx, rs, f)
				if err != nil {
					return "", nil, nil, err
				}
				hostedBytes += int64(len(data))
				hosted = append(hosted, graph.HostedContent{ContentType: strings.ToLower(f.Mimetype), Data: data})
				// Graph numbers hosted contents from 1, in order.
				ref := fmt.Sprintf(`<img src="../hostedContents/%d/$value" alt="%s">`, len(hosted), m[1])
				body = img.ReplaceAllLiteralString(body, ref)
			default:
				// Too large to embed after all: attach it.
				body = img.ReplaceAllLiteralString(body, attach(f))
			}
		}
	}
	return body, attachments, hosted, nil
}

// readStaged returns the content of a small staged file.
func (d *Worker) readStaged(ctx context.Context, rs *runState, f store.FileMapping) ([]byte, error) {
	r, err := rs.blobs.Get(ctx, f.StagingKey)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	data, err := io.ReadAll(io.LimitReader(r, maxInlineImage+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxInlineImage {
		return nil, fmt.Errorf("staged file %s is larger than recorded", f.SlackID)
	}
	return data, nil
}

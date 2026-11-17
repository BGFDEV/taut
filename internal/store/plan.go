package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

func invalidPlan(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidPlan, fmt.Sprintf(format, args...))
}

func validStage(s string) bool {
	switch s {
	case StagePlan, StageExtract, StageTransform, StageTeam, StageFiles, StageLoad, StageFinalize, StageMembers, StageRelease:
		return true
	}
	return false
}

// planDep is one dependency edge, deduplicated.
type planDep struct {
	from, to BatchRef
}

// validatePlan checks a plan before anything is written and returns the
// number of batches per lane and the dependency edges. Every failure wraps
// ErrInvalidPlan and names the offending lane or batch.
func validatePlan(runID string, p Plan) (batchCount map[string]int, deps []planDep, err error) {
	if err := checkJSON("plan totals", p.Totals); err != nil {
		return nil, nil, invalidPlan("%v", err)
	}

	users := make(map[string]struct{}, len(p.Users))
	for _, u := range p.Users {
		if u.SlackID == "" {
			return nil, nil, invalidPlan("a user has no Slack ID")
		}
		if _, dup := users[u.SlackID]; dup {
			return nil, nil, invalidPlan("user %s appears twice", u.SlackID)
		}
		users[u.SlackID] = struct{}{}
	}

	teams := make(map[int]struct{}, len(p.Teams))
	for _, t := range p.Teams {
		if t.Index < 0 {
			return nil, nil, invalidPlan("team index %d is negative", t.Index)
		}
		if _, dup := teams[t.Index]; dup {
			return nil, nil, invalidPlan("team index %d appears twice", t.Index)
		}
		teams[t.Index] = struct{}{}
	}

	channels := make(map[string]struct{}, len(p.Channels))
	for _, c := range p.Channels {
		if c.SlackID == "" {
			return nil, nil, invalidPlan("a channel has no Slack ID")
		}
		if _, dup := channels[c.SlackID]; dup {
			return nil, nil, invalidPlan("channel %s appears twice", c.SlackID)
		}
		channels[c.SlackID] = struct{}{}
		switch _, ok := teams[c.TeamIndex]; {
		case c.IsChat() && c.TeamIndex != NoTeam:
			return nil, nil, invalidPlan("%s is a %s and goes into a chat, but it names team index %d", c.SlackID, c.Kind, c.TeamIndex)
		case c.IsChat() && len(c.Members) < 2:
			return nil, nil, invalidPlan("%s is a %s with %d participants; a chat needs at least two", c.SlackID, c.Kind, len(c.Members))
		case c.IsChat():
		case len(c.Members) > 0 || c.Topic != "":
			return nil, nil, invalidPlan("channel %s has chat participants or a chat topic", c.SlackID)
		case !ok:
			return nil, nil, invalidPlan("channel %s refers to team index %d, which is not in the plan", c.SlackID, c.TeamIndex)
		}
		switch c.State {
		case "", ChannelPlanned, ChannelCreated, ChannelMigrating, ChannelCompleted:
		default:
			return nil, nil, invalidPlan("channel %s has unknown state %q", c.SlackID, c.State)
		}
		if (c.FilesDriveID == "") != (c.FilesFolderID == "") {
			return nil, nil, invalidPlan("channel %s has only one of FilesDriveID and FilesFolderID", c.SlackID)
		}
	}

	return validateLedger(runID, p.Lanes, p.Batches, func(string) bool { return true })
}

// validateLedger checks the lanes and batches of a plan (see ApplyPlan for
// the rules) and returns the number of batches per lane and the
// deduplicated dependency edges. stageOK further restricts the stages the
// plan may use.
func validateLedger(runID string, lanes []Lane, batches []Batch, stageOK func(stage string) bool) (batchCount map[string]int, deps []planDep, err error) {
	batchCount = make(map[string]int, len(lanes))
	for _, l := range lanes {
		if l.ID == "" {
			return nil, nil, invalidPlan("a lane has no ID")
		}
		if l.RunID != "" && l.RunID != runID {
			return nil, nil, invalidPlan("lane %s belongs to run %s, not %s", l.ID, l.RunID, runID)
		}
		if l.ID == PlanLaneID {
			return nil, nil, invalidPlan("lane %s is the plan lane itself and cannot be redefined", l.ID)
		}
		if _, dup := batchCount[l.ID]; dup {
			return nil, nil, invalidPlan("lane %s appears twice", l.ID)
		}
		if !validStage(l.Stage) {
			return nil, nil, invalidPlan("lane %s has unknown stage %q", l.ID, l.Stage)
		}
		if !stageOK(l.Stage) {
			return nil, nil, invalidPlan("lane %s has stage %q, which this kind of plan may not contain", l.ID, l.Stage)
		}
		if l.Status != "" && l.Status != LaneOpen {
			return nil, nil, invalidPlan("lane %s has status %q; a planned lane must be open", l.ID, l.Status)
		}
		if err := checkJSON("cursor of lane "+l.ID, l.Cursor); err != nil {
			return nil, nil, invalidPlan("%v", err)
		}
		batchCount[l.ID] = 0
	}

	seen := make(map[BatchRef]struct{}, len(batches))
	maxSeq := make(map[string]int, len(lanes))
	for _, b := range batches {
		if b.RunID != "" && b.RunID != runID {
			return nil, nil, invalidPlan("batch %s/%d belongs to run %s, not %s", b.LaneID, b.Seq, b.RunID, runID)
		}
		if _, ok := batchCount[b.LaneID]; !ok {
			return nil, nil, invalidPlan("batch %s/%d refers to a lane that is not in the plan", b.LaneID, b.Seq)
		}
		if b.Seq < 0 {
			return nil, nil, invalidPlan("batch %s/%d has a negative seq", b.LaneID, b.Seq)
		}
		if b.Status != "" && b.Status != BatchPending {
			return nil, nil, invalidPlan("batch %s/%d has status %q; a planned batch must be pending", b.LaneID, b.Seq, b.Status)
		}
		if err := checkJSON(fmt.Sprintf("stats of batch %s/%d", b.LaneID, b.Seq), b.Stats); err != nil {
			return nil, nil, invalidPlan("%v", err)
		}
		ref := BatchRef{LaneID: b.LaneID, Seq: b.Seq}
		if _, dup := seen[ref]; dup {
			return nil, nil, invalidPlan("batch %s/%d appears twice", b.LaneID, b.Seq)
		}
		seen[ref] = struct{}{}
		batchCount[b.LaneID]++
		if b.Seq > maxSeq[b.LaneID] {
			maxSeq[b.LaneID] = b.Seq
		}
	}
	// With no duplicates and no negatives, n batches whose largest seq is
	// n-1 are exactly 0..n-1.
	for _, l := range lanes {
		if n := batchCount[l.ID]; n > 0 && maxSeq[l.ID] != n-1 {
			return nil, nil, invalidPlan("lane %s has %d batches but its highest seq is %d; seqs must be 0..%d with no gaps", l.ID, n, maxSeq[l.ID], n-1)
		}
	}

	edges := make(map[planDep]struct{})
	for _, b := range batches {
		from := BatchRef{LaneID: b.LaneID, Seq: b.Seq}
		for _, to := range b.DependsOn {
			if _, ok := seen[to]; !ok {
				return nil, nil, invalidPlan("batch %s/%d depends on %s/%d, which is not in the plan", b.LaneID, b.Seq, to.LaneID, to.Seq)
			}
			if to.LaneID == from.LaneID && to.Seq >= from.Seq {
				return nil, nil, invalidPlan("batch %s/%d depends on %s/%d in its own lane, which can never be done first", b.LaneID, b.Seq, to.LaneID, to.Seq)
			}
			e := planDep{from: from, to: to}
			if _, dup := edges[e]; dup {
				continue
			}
			edges[e] = struct{}{}
			deps = append(deps, e)
		}
	}
	return batchCount, deps, nil
}

// ApplyPlan stores the plan and marks the run running.
//
// It may be called only under a lease on a plan-stage lane. The run row is
// locked first; if the run is already planned the call is a no-op, so a
// retried plan batch is harmless. Otherwise the plan is validated in full
// before anything is written (every failure wraps ErrInvalidPlan):
//
//   - user Slack IDs, team indexes, channel Slack IDs and lane IDs are
//     unique; every channel's team is in the plan; stages are known;
//   - every batch belongs to a lane of the plan, and each lane's batches
//     are exactly seq 0..n-1;
//   - every dependency names a batch of the plan (and not the batch itself
//     or a later batch of its own lane). Cycles across lanes are not
//     searched for.
//
// Lanes and batches are created open and pending; of a planned Lane only
// ID, Stage, Key and Cursor are used, and of a Batch Seq, Key, Input,
// Output, Items, Stats and DependsOn. A lane with no batches is created
// done ("every batch is done" holds trivially), so it cannot keep the run
// from completing. Rows are written with COPY, so a plan with hundreds of
// thousands of batches applies in seconds.
func (t *fencedTx) ApplyPlan(ctx context.Context, p Plan) error {
	if err := t.check(); err != nil {
		return err
	}
	if t.lease.Stage != StagePlan {
		return fmt.Errorf("store: ApplyPlan may only be called from the plan lane, not from %s", t.lease.LaneID)
	}
	runID := t.lease.RunID

	var (
		status  string
		planned *time.Time
		created time.Time
	)
	err := t.tx.QueryRow(ctx, `SELECT status, planned_at, created_at FROM runs WHERE id = $1 FOR NO KEY UPDATE`, runID).
		Scan(&status, &planned, &created)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if planned != nil {
		return nil
	}
	if s := RunStatus(status); s != RunQueued && s != RunRunning {
		return fmt.Errorf("store: run %s is %s and cannot be planned", runID, status)
	}

	batchCount, deps, err := validatePlan(runID, p)
	if err != nil {
		return err
	}

	copyIn := func(table string, cols []string, n int, row func(i int) []any) error {
		return copyRows(ctx, t.tx, table, cols, n, row)
	}

	if err := copyIn("map_users",
		[]string{"run_id", "slack_id", "name", "email", "method", "entra_id", "entra_upn", "reason"},
		len(p.Users), func(i int) []any {
			u := p.Users[i]
			return []any{runID, u.SlackID, u.Name, u.Email, u.Method, u.EntraID, u.EntraUPN, u.Reason}
		}); err != nil {
		return err
	}
	if err := copyIn("map_teams",
		[]string{"run_id", "idx", "name", "description", "visibility", "owner_upn", "owner_id", "teams_id", "adopted"},
		len(p.Teams), func(i int) []any {
			m := p.Teams[i]
			return []any{runID, int32(m.Index), m.Name, m.Description, m.Visibility, m.OwnerUPN, m.OwnerID, m.TeamsID, m.Adopted}
		}); err != nil {
		return err
	}
	if err := copyIn("map_channels",
		[]string{"run_id", "slack_id", "slack_name", "kind", "team_index", "target_name", "is_general", "created", "teams_id", "state", "migration_start", "files_drive_id", "files_folder_id", "members", "topic"},
		len(p.Channels), func(i int) []any {
			c := p.Channels[i]
			state := c.State
			if state == "" {
				state = ChannelPlanned
			}
			var createdAt any
			if !c.Created.IsZero() {
				createdAt = c.Created.UTC()
			}
			var migStart any
			if c.MigrationStart != nil {
				migStart = c.MigrationStart.UTC()
			}
			var driveID, folderID any
			if c.FilesDriveID != "" {
				driveID, folderID = c.FilesDriveID, c.FilesFolderID
			}
			var team any
			if c.TeamIndex != NoTeam {
				team = int32(c.TeamIndex)
			}
			members := c.Members
			if members == nil {
				members = []string{}
			}
			return []any{runID, c.SlackID, c.SlackName, c.Kind, team, c.TargetName, c.IsGeneral, createdAt, c.TeamsID, string(state), migStart, driveID, folderID, members, c.Topic}
		}); err != nil {
		return err
	}
	if err := copyLedger(ctx, t.tx, runID, created, p.Lanes, p.Batches, deps, batchCount); err != nil {
		return err
	}

	_, err = t.tx.Exec(ctx, `
		UPDATE runs SET status = 'running', totals = $2::json, planned_at = now(), updated_at = now()
		WHERE id = $1`, runID, jsonArg(p.Totals))
	return err
}

// copyRows writes n rows to a table with COPY.
func copyRows(ctx context.Context, tx pgx.Tx, table string, cols []string, n int, row func(i int) []any) error {
	if n == 0 {
		return nil
	}
	_, err := tx.CopyFrom(ctx, pgx.Identifier{table}, cols,
		pgx.CopyFromSlice(n, func(i int) ([]any, error) { return row(i), nil }))
	if err != nil {
		return fmt.Errorf("store: write plan (%s): %w", table, err)
	}
	return nil
}

// copyLedger inserts validated lanes, batches and dependency edges. It is
// shared by ApplyPlan and BeginFinalize so both create lanes the same way: open
// and pending, a lane without batches done. batchCount is validateLedger's.
func copyLedger(ctx context.Context, tx pgx.Tx, runID string, runCreated time.Time, lanes []Lane, batches []Batch, deps []planDep, batchCount map[string]int) error {
	if err := copyRows(ctx, tx, "lanes",
		[]string{"run_id", "id", "stage", "key", "status", "cursor", "run_created"},
		len(lanes), func(i int) []any {
			l := lanes[i]
			status := LaneOpen
			if batchCount[l.ID] == 0 {
				status = LaneDone
			}
			return []any{runID, l.ID, l.Stage, l.Key, string(status), jsonArg(l.Cursor), runCreated}
		}); err != nil {
		return err
	}
	if err := copyRows(ctx, tx, "batches",
		[]string{"run_id", "lane_id", "seq", "key", "input", "output", "items", "stats"},
		len(batches), func(i int) []any {
			b := batches[i]
			return []any{runID, b.LaneID, int32(b.Seq), b.Key, b.Input, b.Output, int32(b.Items), jsonArg(b.Stats)}
		}); err != nil {
		return err
	}
	return copyRows(ctx, tx, "batch_deps",
		[]string{"run_id", "lane_id", "seq", "dep_lane_id", "dep_seq"},
		len(deps), func(i int) []any {
			d := deps[i]
			return []any{runID, d.from.LaneID, int32(d.from.Seq), d.to.LaneID, int32(d.to.Seq)}
		})
}

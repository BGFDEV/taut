package store_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/store/storetest"
)

var allStages = []string{store.StagePlan, store.StageExtract, store.StageTransform, store.StageTeam, store.StageFiles, store.StageLoad, store.StageFinalize, store.StageMembers}

// schemaVersion is the number of embedded migrations.
const schemaVersion = 4

const ttl = time.Minute

// open returns an isolated store and marks the test parallel.
func open(t *testing.T) *store.PG {
	t.Helper()
	t.Parallel()
	return storetest.OpenPG(t)
}

func storetestOpen(t *testing.T) store.Store { return storetest.Open(t) }

// newRun creates a run and records its export, leaving it queued with a
// plan lane.
func newRun(t *testing.T, s *store.PG, name string) string {
	t.Helper()
	ctx := t.Context()
	id := store.NewRunID()
	if err := s.CreateRun(ctx, store.Run{ID: id, Name: name, Config: []byte("name: " + name), ConfigHash: "mh"}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := s.SetExport(ctx, id, "exports/"+id+".zip", "sha-"+id, 1234); err != nil {
		t.Fatalf("SetExport: %v", err)
	}
	return id
}

// mustClaim claims a lane or fails the test.
func mustClaim(t *testing.T, s *store.PG, runID, holder string, stages ...string) *store.Lease {
	t.Helper()
	if len(stages) == 0 {
		stages = allStages
	}
	l, err := s.Claim(t.Context(), store.ClaimFilter{Stages: stages, RunID: runID}, holder, ttl)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if l == nil {
		t.Fatalf("Claim(%v): nothing claimable", stages)
	}
	return l
}

// noClaim asserts nothing is claimable.
func noClaim(t *testing.T, s *store.PG, runID string, stages ...string) {
	t.Helper()
	if len(stages) == 0 {
		stages = allStages
	}
	l, err := s.Claim(t.Context(), store.ClaimFilter{Stages: stages, RunID: runID}, "nobody", ttl)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if l != nil {
		t.Fatalf("Claim(%v) = lane %s, want nothing claimable", stages, l.LaneID)
	}
}

// claimLane claims one specific lane, by claiming lanes of its stage until
// it comes up and handing the others back.
func claimLane(t *testing.T, s *store.PG, runID, stage, laneID, holder string) *store.Lease {
	t.Helper()
	var others []*store.Lease
	defer func() {
		for _, o := range others {
			release(t, s, o)
		}
	}()
	for {
		l, err := s.Claim(t.Context(), store.ClaimFilter{Stages: []string{stage}, RunID: runID}, holder, ttl)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if l == nil {
			t.Fatalf("lane %s is not claimable", laneID)
		}
		if l.LaneID == laneID {
			return l
		}
		others = append(others, l)
	}
}

func release(t *testing.T, s *store.PG, l *store.Lease) {
	t.Helper()
	if err := s.Release(t.Context(), l, 0, ""); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// complete completes one batch of the leased lane.
func complete(t *testing.T, s *store.PG, l *store.Lease, seq int) {
	t.Helper()
	err := s.Fenced(t.Context(), l, func(tx store.Tx) error {
		return tx.CompleteBatch(t.Context(), seq, fmt.Sprintf("out/%s/%d", l.LaneID, seq), 1, nil)
	})
	if err != nil {
		t.Fatalf("CompleteBatch(%s/%d): %v", l.LaneID, seq, err)
	}
}

// finishLane completes every batch of the leased lane that is runnable and
// returns how many it completed.
func finishLane(t *testing.T, s *store.PG, l *store.Lease) int {
	t.Helper()
	n := 0
	for {
		b, err := s.NextBatch(t.Context(), l)
		if err != nil {
			t.Fatalf("NextBatch(%s): %v", l.LaneID, err)
		}
		if b == nil {
			return n
		}
		complete(t, s, l, b.Seq)
		n++
	}
}

// drain works the run until nothing is claimable and returns the number of
// batches completed.
func drain(t *testing.T, s *store.PG, runID string, stages ...string) int {
	t.Helper()
	if len(stages) == 0 {
		stages = allStages
	}
	n := 0
	for {
		l, err := s.Claim(t.Context(), store.ClaimFilter{Stages: stages, RunID: runID}, "drain", ttl)
		if err != nil {
			t.Fatalf("Claim: %v", err)
		}
		if l == nil {
			return n
		}
		n += finishLane(t, s, l)
		release(t, s, l)
	}
}

// applyPlan runs the plan lane: ApplyPlan and completion of the plan batch
// in one fenced transaction.
func applyPlan(t *testing.T, s *store.PG, runID string, p store.Plan) {
	t.Helper()
	l := mustClaim(t, s, runID, "planner", store.StagePlan)
	if l.LaneID != store.PlanLaneID {
		t.Fatalf("claimed %s, want the plan lane", l.LaneID)
	}
	err := s.Fenced(t.Context(), l, func(tx store.Tx) error {
		if err := tx.ApplyPlan(t.Context(), p); err != nil {
			return err
		}
		return tx.CompleteBatch(t.Context(), 0, "", 0, nil)
	})
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	release(t, s, l)
}

// testPlan builds a realistic plan: one team; per channel an extract, a
// transform and a load lane with n day batches each. transform i depends on
// extract i; the load lane starts with an "open" batch depending on the
// team batch, then load i+1 depends on transform i.
func testPlan(channels []string, n int) store.Plan {
	p := store.Plan{
		Users: []store.UserMapping{
			{SlackID: "U1", Name: "Ada", Email: "ada@example.com", Method: "email", EntraID: "e-1", EntraUPN: "ada@example.com"},
			{SlackID: "U2", Name: "Bob", Method: "unmapped", Reason: "no email"},
		},
		Teams:  []store.TeamMapping{{Index: 0, Name: "Engineering", Visibility: "private", OwnerUPN: "ada@example.com", OwnerID: "e-1"}},
		Totals: []byte(`{"channels": ` + fmt.Sprint(len(channels)) + `}`),
		Lanes:  []store.Lane{{ID: "team:0", Stage: store.StageTeam, Key: "0"}},
		Batches: []store.Batch{
			{LaneID: "team:0", Seq: 0, Key: "team"},
		},
	}
	for i, c := range channels {
		p.Channels = append(p.Channels, store.ChannelMapping{
			SlackID: c, SlackName: "chan-" + c, Kind: "public", TeamIndex: 0, TargetName: "Chan " + c, IsGeneral: i == 0,
			Created: time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC),
		})
		p.Lanes = append(p.Lanes,
			store.Lane{ID: "extract:" + c, Stage: store.StageExtract, Key: c},
			store.Lane{ID: "transform:" + c, Stage: store.StageTransform, Key: c},
			store.Lane{ID: "load:" + c, Stage: store.StageLoad, Key: c},
		)
		p.Batches = append(p.Batches, store.Batch{
			LaneID: "load:" + c, Seq: 0, Key: "open", DependsOn: []store.BatchRef{{LaneID: "team:0", Seq: 0}},
		})
		for d := range n {
			day := fmt.Sprintf("2024-03-%02d", d+1)
			p.Batches = append(p.Batches,
				store.Batch{LaneID: "extract:" + c, Seq: d, Key: day, Input: "raw/" + c + "/" + day},
				store.Batch{LaneID: "transform:" + c, Seq: d, Key: day, DependsOn: []store.BatchRef{{LaneID: "extract:" + c, Seq: d}}},
				store.Batch{LaneID: "load:" + c, Seq: d + 1, Key: day, DependsOn: []store.BatchRef{{LaneID: "transform:" + c, Seq: d}}},
			)
		}
	}
	return p
}

// plannedRun creates a run and applies testPlan to it.
func plannedRun(t *testing.T, s *store.PG, name string, channels []string, n int) string {
	t.Helper()
	id := newRun(t, s, name)
	applyPlan(t, s, id, testPlan(channels, n))
	return id
}

// exec runs raw SQL against the test schema.
func exec(t *testing.T, s *store.PG, sql string, args ...any) int64 {
	t.Helper()
	tag, err := s.Pool().Exec(context.WithoutCancel(t.Context()), sql, args...)
	if err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
	return tag.RowsAffected()
}

// scalar reads one value with raw SQL.
func scalar[T any](t *testing.T, s *store.PG, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := s.Pool().QueryRow(t.Context(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return v
}

func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("error = %v, want %v", err, target)
	}
}

func runStatus(t *testing.T, s *store.PG, id string) store.RunStatus {
	t.Helper()
	r, err := s.GetRun(t.Context(), id)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	return r.Status
}

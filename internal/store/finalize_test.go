package store_test

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/store/storetest"
)

// finalizePlan finalizes the given channels: one finalize lane per channel, and one
// members lane whose batch waits for all of them.
func finalizePlan(membersLane string, channels ...string) store.FinalizePlan {
	var p store.FinalizePlan
	var deps []store.BatchRef
	for _, c := range channels {
		p.Lanes = append(p.Lanes, store.Lane{ID: "finalize:" + c, Stage: store.StageFinalize, Key: c})
		p.Batches = append(p.Batches, store.Batch{LaneID: "finalize:" + c, Seq: 0, Key: "complete"})
		deps = append(deps, store.BatchRef{LaneID: "finalize:" + c, Seq: 0})
	}
	p.Lanes = append(p.Lanes, store.Lane{ID: membersLane, Stage: store.StageMembers, Key: "0"})
	p.Batches = append(p.Batches, store.Batch{LaneID: membersLane, Seq: 0, Key: "members", DependsOn: deps})
	return p
}

// completedRun plans a run, works it to the end and reconciles it.
func completedRun(t *testing.T, s *store.PG, name string, channels ...string) string {
	t.Helper()
	id := plannedRun(t, s, name, channels, 1)
	drain(t, s, id)
	if _, err := s.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if st := runStatus(t, s, id); st != store.RunCompleted {
		t.Fatalf("run %s is %s, want completed", name, st)
	}
	return id
}

// finalizeChannel works one finalize lane: the channel becomes completed.
func finalizeChannel(t *testing.T, s *store.PG, runID, channel string) {
	t.Helper()
	ctx := t.Context()
	l := claimLane(t, s, runID, store.StageFinalize, "finalize:"+channel, "finalizer")
	err := s.Fenced(ctx, l, func(tx store.Tx) error {
		if err := tx.SetChannel(ctx, channel, "19:"+channel, store.ChannelCompleted, nil); err != nil {
			return err
		}
		return tx.CompleteBatch(ctx, 0, "", 1, nil)
	})
	if err != nil {
		t.Fatalf("finalize %s: %v", channel, err)
	}
	release(t, s, l)
}

func reconcileOne(t *testing.T, s *store.PG, runID string) *store.Run {
	t.Helper()
	changed, err := s.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, r := range changed {
		if r.ID == runID {
			return &r
		}
	}
	return nil
}

func TestFinalizeLifecycle(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := completedRun(t, s, "acme", "C1", "C2")
	r, _ := s.GetRun(ctx, id)
	if r.FinishedAt == nil || r.Retain || r.PurgedAt != nil {
		t.Fatalf("completed run = %+v", r)
	}
	lanesBefore := scalar[int](t, s, `SELECT count(*) FROM lanes WHERE run_id = $1`, id)

	// Refusals.
	wantErr(t, s.BeginFinalize(ctx, "missing", finalizePlan("members:0", "C1")), store.ErrNotFound)
	bad := finalizePlan("members:0", "C1")
	bad.Lanes[0].Stage = store.StageLoad
	wantErr(t, s.BeginFinalize(ctx, id, bad), store.ErrInvalidPlan)
	bad = finalizePlan("members:0", "C1")
	bad.Batches[1].DependsOn = []store.BatchRef{{LaneID: "load:C1", Seq: 0}} // outside the plan
	wantErr(t, s.BeginFinalize(ctx, id, bad), store.ErrInvalidPlan)
	bad = finalizePlan("members:0", "C1")
	bad.Batches[0].Seq = 3
	wantErr(t, s.BeginFinalize(ctx, id, bad), store.ErrInvalidPlan)
	bad = store.FinalizePlan{Lanes: []store.Lane{{ID: "load:C1", Stage: store.StageFinalize}}} // an existing lane of another stage
	wantErr(t, s.BeginFinalize(ctx, id, bad), store.ErrInvalidPlan)
	if st := runStatus(t, s, id); st != store.RunCompleted {
		t.Fatalf("a refused BeginFinalize changed the status to %s", st)
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM lanes WHERE run_id = $1`, id); n != lanesBefore {
		t.Fatalf("a refused BeginFinalize left %d lanes behind", n-lanesBefore)
	}

	// Begin.
	plan := finalizePlan("members:0", "C1", "C2")
	if err := s.BeginFinalize(ctx, id, plan); err != nil {
		t.Fatalf("BeginFinalize: %v", err)
	}
	r, _ = s.GetRun(ctx, id)
	if r.Status != store.RunFinalizing || r.FinishedAt != nil || r.Retain {
		t.Fatalf("finalizing run = %+v", r)
	}
	if ar, ok, err := s.ActiveRun(ctx, "acme"); err != nil || !ok || ar.ID != id {
		t.Fatalf("a finalizing run is not active: %v %v", ok, err)
	}
	err := s.CreateRun(ctx, store.Run{ID: store.NewRunID(), Name: "acme", ConfigHash: "h"})
	wantErr(t, err, store.ErrActiveRunExists)
	b, err := s.GetBatch(ctx, id, store.BatchRef{LaneID: "members:0", Seq: 0})
	if err != nil || len(b.DependsOn) != 2 || b.Key != "members" || b.Status != store.BatchPending {
		t.Fatalf("members batch = %+v, %v", b, err)
	}

	// Twice is a no-op; retention only ever turns on.
	snapshot := func() string {
		return scalar[string](t, s, `SELECT md5((SELECT string_agg(l::text, '|' ORDER BY id) FROM lanes l WHERE run_id = $1)
			|| (SELECT string_agg(b::text, '|' ORDER BY lane_id, seq) FROM batches b WHERE run_id = $1)
			|| (SELECT string_agg(d::text, '|' ORDER BY lane_id, seq, dep_lane_id, dep_seq) FROM batch_deps d WHERE run_id = $1))`, id)
	}
	before := snapshot()
	if err := s.BeginFinalize(ctx, id, plan); err != nil {
		t.Fatalf("second BeginFinalize: %v", err)
	}
	changedPlan := finalizePlan("members:0", "C1", "C2")
	changedPlan.Batches = append(changedPlan.Batches, store.Batch{LaneID: "finalize:C1", Seq: 1, Key: "extra"})
	changedPlan.Retain = true
	if err := s.BeginFinalize(ctx, id, changedPlan); err != nil {
		t.Fatalf("third BeginFinalize: %v", err)
	}
	if snapshot() != before {
		t.Fatal("a repeated BeginFinalize changed existing lanes or batches")
	}
	if r, _ := s.GetRun(ctx, id); !r.Retain || r.Status != store.RunFinalizing {
		t.Fatalf("after BeginFinalize with Retain: %+v", r)
	}
	if err := s.BeginFinalize(ctx, id, plan); err != nil { // Retain false again
		t.Fatal(err)
	}
	if r, _ := s.GetRun(ctx, id); !r.Retain {
		t.Fatal("a later BeginFinalize turned retention off")
	}

	// Progress shows the new stages, after load.
	p, err := s.Progress(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var stages []string
	for _, sp := range p.Stages {
		stages = append(stages, sp.Stage)
	}
	if want := []string{"plan", "extract", "transform", "team", "load", "finalize", "members"}; !reflect.DeepEqual(stages, want) {
		t.Fatalf("Progress stages = %v, want %v", stages, want)
	}
	if sp := p.Stages[5]; sp.Lanes != 2 || sp.Batches != 2 || sp.Done != 0 {
		t.Fatalf("finalize stage = %+v", sp)
	}

	// Claiming: finalize lanes first; members waits for both.
	noClaim(t, s, id, store.StageMembers)
	if changed := reconcileOne(t, s, id); changed != nil {
		t.Fatalf("Reconcile changed a finalizing run with work left: %+v", changed)
	}
	first := mustClaim(t, s, id, "h")
	if first.LaneID != "finalize:C1" || first.Stage != store.StageFinalize {
		t.Fatalf("first claim = %s", first.LaneID)
	}
	release(t, s, first)
	finalizeChannel(t, s, id, "C1")
	noClaim(t, s, id, store.StageMembers)
	finalizeChannel(t, s, id, "C2")
	// Lanes are not all done yet (members), so not finalized.
	if changed := reconcileOne(t, s, id); changed != nil {
		t.Fatalf("Reconcile = %+v before members ran", changed)
	}
	ml := mustClaim(t, s, id, "h")
	if ml.LaneID != "members:0" {
		t.Fatalf("claimed %s, want members:0", ml.LaneID)
	}
	// Leased and unfinished: still finalizing.
	if changed := reconcileOne(t, s, id); changed != nil {
		t.Fatalf("Reconcile = %+v while members is leased", changed)
	}
	complete(t, s, ml, 0)
	release(t, s, ml)

	changed := reconcileOne(t, s, id)
	if changed == nil || changed.Status != store.RunFinalized || changed.FinishedAt == nil || !changed.Retain {
		t.Fatalf("Reconcile = %+v, want finalized", changed)
	}
	if changed := reconcileOne(t, s, id); changed != nil {
		t.Fatalf("second Reconcile = %+v", changed)
	}
	noClaim(t, s, id)
	if _, ok, _ := s.ActiveRun(ctx, "acme"); ok {
		t.Fatal("a finalized run is still active")
	}
	// Finalized is final.
	wantErr(t, s.BeginFinalize(ctx, id, plan), store.ErrNotFinalizable)
	if _, err := s.RetryFailed(ctx, id); err == nil {
		t.Fatal("RetryFailed on a finalized run succeeded")
	}
	if err := s.CancelRun(ctx, id); err != nil || runStatus(t, s, id) != store.RunFinalized {
		t.Fatalf("CancelRun on a finalized run: %v, status %s", err, runStatus(t, s, id))
	}
	// The name is free again.
	if err := s.CreateRun(ctx, store.Run{ID: store.NewRunID(), Name: "acme", ConfigHash: "h"}); err != nil {
		t.Fatalf("CreateRun after finalized: %v", err)
	}
}

// BeginFinalize is refused in every status but completed and finalizing.
func TestBeginFinalizeStatuses(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	for _, st := range []store.RunStatus{store.RunAwaitingExport, store.RunQueued, store.RunRunning, store.RunFailed, store.RunCancelled, store.RunFinalized} {
		id := plannedRun(t, s, "r-"+string(st), []string{"C1"}, 1)
		exec(t, s, `UPDATE runs SET status = $2 WHERE id = $1`, id, string(st))
		err := s.BeginFinalize(ctx, id, finalizePlan("members:0", "C1"))
		if !errors.Is(err, store.ErrNotFinalizable) || !strings.Contains(err.Error(), string(st)) {
			t.Errorf("BeginFinalize on a %s run: %v, want ErrNotFinalizable naming the status", st, err)
		}
		if n := scalar[int](t, s, `SELECT count(*) FROM lanes WHERE run_id = $1 AND stage IN ('finalize', 'members')`, id); n != 0 {
			t.Errorf("%s: refused BeginFinalize created %d lanes", st, n)
		}
		if got := runStatus(t, s, id); got != st {
			t.Errorf("%s: status became %s", st, got)
		}
	}
	// An empty plan on a completed run settles at the next Reconcile.
	id := completedRun(t, s, "empty", "C1")
	if err := s.BeginFinalize(ctx, id, store.FinalizePlan{}); err != nil {
		t.Fatal(err)
	}
	if changed := reconcileOne(t, s, id); changed == nil || changed.Status != store.RunCompleted {
		t.Fatalf("Reconcile = %+v, want completed (channel not finalized)", changed)
	}
}

// Finalize and members lanes are claimable only while the run is finalizing.
func TestFinalizeLanesClaimableOnlyWhileFinalizing(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := completedRun(t, s, "acme", "C1")
	if err := s.BeginFinalize(ctx, id, finalizePlan("members:0", "C1")); err != nil {
		t.Fatal(err)
	}
	for _, st := range []string{"running", "queued", "failed", "completed", "finalized", "cancelled"} {
		exec(t, s, `UPDATE runs SET status = $2 WHERE id = $1`, id, st)
		noClaim(t, s, id)
		noClaim(t, s, "", store.StageFinalize, store.StageMembers)
	}
	exec(t, s, `UPDATE runs SET status = 'finalizing' WHERE id = $1`, id)
	l := mustClaim(t, s, id, "h", store.StageFinalize, store.StageMembers)
	if l.LaneID != "finalize:C1" {
		t.Fatalf("claimed %s", l.LaneID)
	}
	// Ordinary stages stay claimable in a running run, finalize lanes or not.
	other := plannedRun(t, s, "other", []string{"C1"}, 1)
	if l := mustClaim(t, s, other, "h"); l.Stage != store.StageExtract {
		t.Fatalf("claimed %s", l.LaneID)
	}
}

// A plan that selects only some channels: the run goes back to completed,
// and a later plan for the rest finalizes it.
func TestFinalizePartial(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := completedRun(t, s, "acme", "C1", "C2")
	if err := s.BeginFinalize(ctx, id, finalizePlan("members:first", "C1")); err != nil {
		t.Fatal(err)
	}
	if n := drainFinalize(t, s, id); n != 2 {
		t.Fatalf("worked %d batches, want 2", n)
	}
	changed := reconcileOne(t, s, id)
	if changed == nil || changed.Status != store.RunCompleted || changed.FinishedAt == nil {
		t.Fatalf("Reconcile = %+v, want completed (C2 not finalized)", changed)
	}
	noClaim(t, s, id)
	if c, _ := s.Channel(ctx, id, "C1"); c.State != store.ChannelCompleted {
		t.Fatalf("C1 = %+v", c)
	}

	// The rest, repeating C1 (done: untouched).
	if err := s.BeginFinalize(ctx, id, finalizePlan("members:second", "C1", "C2")); err != nil {
		t.Fatal(err)
	}
	if r, _ := s.GetRun(ctx, id); r.Status != store.RunFinalizing || r.FinishedAt != nil {
		t.Fatalf("run = %+v", r)
	}
	if st := scalar[string](t, s, `SELECT status FROM lanes WHERE run_id = $1 AND id = 'finalize:C1'`, id); st != "done" {
		t.Fatalf("finalize:C1 is %s after a repeat, want done", st)
	}
	// members:second depends on finalize:C1/0 (existing, done) and finalize:C2/0.
	if n := drainFinalize(t, s, id); n != 2 {
		t.Fatalf("worked %d batches, want 2 (finalize:C2 and members:second)", n)
	}
	changed = reconcileOne(t, s, id)
	if changed == nil || changed.Status != store.RunFinalized || changed.FinishedAt == nil {
		t.Fatalf("Reconcile = %+v, want finalized", changed)
	}

	// A new batch depending on a batch an existing lane does not have.
	id2 := completedRun(t, s, "other", "C1")
	if err := s.BeginFinalize(ctx, id2, finalizePlan("members:0", "C1")); err != nil {
		t.Fatal(err)
	}
	p := store.FinalizePlan{
		Lanes: []store.Lane{{ID: "finalize:C1", Stage: store.StageFinalize}, {ID: "members:late", Stage: store.StageMembers}},
		Batches: []store.Batch{
			{LaneID: "finalize:C1", Seq: 0}, {LaneID: "finalize:C1", Seq: 1},
			{LaneID: "members:late", Seq: 0, DependsOn: []store.BatchRef{{LaneID: "finalize:C1", Seq: 1}}},
		},
	}
	wantErr(t, s.BeginFinalize(ctx, id2, p), store.ErrInvalidPlan)
	if n := scalar[int](t, s, `SELECT count(*) FROM lanes WHERE run_id = $1 AND id = 'members:late'`, id2); n != 0 {
		t.Fatal("a refused BeginFinalize created a lane")
	}
}

// drainFinalize works every finalize and members lane; finalize lanes complete their
// channel.
func drainFinalize(t *testing.T, s *store.PG, runID string) int {
	t.Helper()
	ctx := t.Context()
	n := 0
	for {
		l, err := s.Claim(ctx, store.ClaimFilter{Stages: []string{store.StageFinalize, store.StageMembers}, RunID: runID}, "finalizer", ttl)
		if err != nil {
			t.Fatal(err)
		}
		if l == nil {
			return n
		}
		err = s.Fenced(ctx, l, func(tx store.Tx) error {
			if l.Stage == store.StageFinalize {
				if err := tx.SetChannel(ctx, l.Key, "19:"+l.Key, store.ChannelCompleted, nil); err != nil {
					return err
				}
			}
			return tx.CompleteBatch(ctx, 0, "", 1, nil)
		})
		if err != nil {
			t.Fatalf("%s: %v", l.LaneID, err)
		}
		n++
		release(t, s, l)
	}
}

func TestFinalizeFailureAndRetry(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := completedRun(t, s, "acme", "C1", "C2")
	plan := finalizePlan("members:0", "C1", "C2")
	if err := s.BeginFinalize(ctx, id, plan); err != nil {
		t.Fatal(err)
	}
	failLane := func() {
		t.Helper()
		l := claimLane(t, s, id, store.StageFinalize, "finalize:C1", "h")
		if err := s.Fenced(ctx, l, func(tx store.Tx) error { return tx.FailBatch(ctx, 0, "completeMigration 409") }); err != nil {
			t.Fatal(err)
		}
		if err := s.Release(ctx, l, time.Hour, "backoff too"); err != nil {
			t.Fatal(err)
		}
	}
	failLane()
	// finalize:C2 can still run, so the run is not failed yet.
	if changed := reconcileOne(t, s, id); changed != nil {
		t.Fatalf("Reconcile = %+v while finalize:C2 is runnable", changed)
	}

	// BeginFinalize again reopens the failed lane of the plan, like a retry.
	if err := s.BeginFinalize(ctx, id, plan); err != nil {
		t.Fatal(err)
	}
	var status, lastErr string
	var attempts int
	var wait time.Duration
	if err := s.Pool().QueryRow(ctx, `SELECT status, attempts, last_error, not_before - now() FROM lanes WHERE run_id = $1 AND id = 'finalize:C1'`, id).
		Scan(&status, &attempts, &lastErr, &wait); err != nil {
		t.Fatal(err)
	}
	if status != "open" || attempts != 0 || lastErr != "" || wait > 0 {
		t.Fatalf("reopened lane: status=%s attempts=%d error=%q wait=%v", status, attempts, lastErr, wait)
	}
	if b, _ := s.GetBatch(ctx, id, store.BatchRef{LaneID: "finalize:C1", Seq: 0}); b.Status != store.BatchPending || b.Error != "" {
		t.Fatalf("reopened batch = %+v", b)
	}

	// Fail it again, finish the rest: now the run is stuck and fails.
	failLane()
	finalizeChannel(t, s, id, "C2")
	changed := reconcileOne(t, s, id)
	if changed == nil || changed.Status != store.RunFailed {
		t.Fatalf("Reconcile = %+v, want failed", changed)
	}
	noClaim(t, s, id)
	wantErr(t, s.BeginFinalize(ctx, id, plan), store.ErrNotFinalizable)

	// RetryFailed returns it to finalizing, not running.
	n, err := s.RetryFailed(ctx, id)
	if err != nil || n != 1 {
		t.Fatalf("RetryFailed = %d, %v", n, err)
	}
	if st := runStatus(t, s, id); st != store.RunFinalizing {
		t.Fatalf("status after retry = %s, want finalizing", st)
	}
	if n := drainFinalize(t, s, id); n != 2 {
		t.Fatalf("worked %d batches after retry, want 2", n)
	}
	if changed := reconcileOne(t, s, id); changed == nil || changed.Status != store.RunFinalized {
		t.Fatalf("Reconcile = %+v, want finalized", changed)
	}

	// A run that failed before any finalize still returns to running.
	pre := plannedRun(t, s, "pre", []string{"C1"}, 1)
	l := mustClaim(t, s, pre, "h", store.StageExtract)
	if err := s.Fenced(ctx, l, func(tx store.Tx) error { return tx.FailBatch(ctx, 0, "x") }); err != nil {
		t.Fatal(err)
	}
	release(t, s, l)
	drain(t, s, pre)
	if changed := reconcileOne(t, s, pre); changed == nil || changed.Status != store.RunFailed {
		t.Fatalf("Reconcile = %+v", changed)
	}
	if _, err := s.RetryFailed(ctx, pre); err != nil || runStatus(t, s, pre) != store.RunRunning {
		t.Fatalf("retry before finalize: %v, status %s", err, runStatus(t, s, pre))
	}
}

// Concurrent identical finalize requests: all succeed, the ledger is created
// once.
func TestBeginFinalizeConcurrent(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := completedRun(t, s, "acme", "C1", "C2", "C3")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := s.BeginFinalize(ctx, id, finalizePlan("members:0", "C1", "C2", "C3")); err != nil {
				t.Errorf("BeginFinalize: %v", err)
			}
		})
	}
	wg.Wait()
	if n := scalar[int](t, s, `SELECT count(*) FROM lanes WHERE run_id = $1 AND stage IN ('finalize', 'members')`, id); n != 4 {
		t.Fatalf("%d finalize lanes, want 4", n)
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM batch_deps WHERE run_id = $1 AND lane_id = 'members:0'`, id); n != 3 {
		t.Fatalf("%d dependencies, want 3", n)
	}
	if n := drainFinalize(t, s, id); n != 4 {
		t.Fatalf("worked %d batches", n)
	}
	if changed := reconcileOne(t, s, id); changed == nil || changed.Status != store.RunFinalized {
		t.Fatalf("Reconcile = %+v", changed)
	}
}

func TestPurge(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	finalized := func(name string, retain bool) string {
		t.Helper()
		id := completedRun(t, s, name, "C1")
		exec(t, s, `UPDATE runs SET wrapped_key = '\x010203' WHERE id = $1`, id)
		p := finalizePlan("members:0", "C1")
		p.Retain = retain
		if err := s.BeginFinalize(ctx, id, p); err != nil {
			t.Fatal(err)
		}
		// Purge is refused while finalizing.
		wantErr(t, s.MarkPurged(ctx, id), store.ErrNotFinalized)
		drainFinalize(t, s, id)
		if changed := reconcileOne(t, s, id); changed == nil || changed.Status != store.RunFinalized {
			t.Fatalf("Reconcile = %+v", changed)
		}
		return id
	}

	if runs, err := s.Purgeable(ctx); err != nil || len(runs) != 0 {
		t.Fatalf("Purgeable on an empty store = %v, %v", runs, err)
	}
	wantErr(t, s.MarkPurged(ctx, "missing"), store.ErrNotFound)
	// Not finalized: completed, running.
	comp := completedRun(t, s, "completed", "C1")
	wantErr(t, s.MarkPurged(ctx, comp), store.ErrNotFinalized)
	running := plannedRun(t, s, "running", []string{"C1"}, 1)
	err := s.MarkPurged(ctx, running)
	if !errors.Is(err, store.ErrNotFinalized) || !strings.Contains(err.Error(), "running") {
		t.Fatalf("MarkPurged(running) = %v", err)
	}
	if r, _ := s.GetRun(ctx, running); r.PurgedAt != nil {
		t.Fatal("a refused purge set PurgedAt")
	}

	a := finalized("a", false)
	time.Sleep(3 * time.Millisecond)
	kept := finalized("kept", true)
	time.Sleep(3 * time.Millisecond)
	b := finalized("b", false)

	runs, err := s.Purgeable(ctx)
	if err != nil || len(runs) != 2 || runs[0].ID != a || runs[1].ID != b {
		t.Fatalf("Purgeable = %v, %v; want [a b], oldest first, without the retained run", ids(runs), err)
	}
	if runs[0].Status != store.RunFinalized || runs[0].Retain || runs[0].PurgedAt != nil || string(runs[0].WrappedKey) != "\x01\x02\x03" {
		t.Fatalf("run a = %+v", runs[0])
	}

	if err := s.MarkPurged(ctx, a); err != nil {
		t.Fatal(err)
	}
	r, err := s.GetRun(ctx, a)
	if err != nil || r.PurgedAt == nil || len(r.WrappedKey) != 0 || r.Status != store.RunFinalized || r.PurgedAt.Location() != time.UTC {
		t.Fatalf("purged run = %+v, %v", r, err)
	}
	if d := time.Since(*r.PurgedAt); d < -time.Minute || d > time.Minute {
		t.Fatalf("PurgedAt = %v", r.PurgedAt)
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM runs WHERE id = $1 AND wrapped_key IS NULL`, a); n != 1 {
		t.Fatal("the wrapped key is still in the database")
	}
	// Idempotent: the first PurgedAt stands.
	time.Sleep(3 * time.Millisecond)
	if err := s.MarkPurged(ctx, a); err != nil {
		t.Fatalf("second MarkPurged: %v", err)
	}
	if r2, _ := s.GetRun(ctx, a); !r2.PurgedAt.Equal(*r.PurgedAt) {
		t.Fatal("a repeated purge moved PurgedAt")
	}
	if runs, _ := s.Purgeable(ctx); len(runs) != 1 || runs[0].ID != b {
		t.Fatalf("Purgeable after purging a = %v", ids(runs))
	}
	// A retained run can still be purged explicitly.
	if err := s.MarkPurged(ctx, kept); err != nil {
		t.Fatalf("MarkPurged(retained): %v", err)
	}

	// A cancelled run is purgeable too: nothing will read its staging again.
	if active, err := s.ActiveRuns(ctx); err != nil || len(active) != 1 || active[0].ID != running {
		t.Fatalf("ActiveRuns = %v, %v; want only the running run", ids(active), err)
	}
	if err := s.CancelRun(ctx, running); err != nil {
		t.Fatal(err)
	}
	if active, _ := s.ActiveRuns(ctx); len(active) != 0 {
		t.Fatalf("ActiveRuns after cancelling = %v", ids(active))
	}
	if runs, _ := s.Purgeable(ctx); len(runs) != 2 || runs[0].ID != running || runs[1].ID != b {
		t.Fatalf("Purgeable after cancelling = %v; want the cancelled run and b", ids(runs))
	}
	if err := s.MarkPurged(ctx, running); err != nil {
		t.Fatalf("MarkPurged(cancelled): %v", err)
	}
	if runs, _ := s.Purgeable(ctx); len(runs) != 1 || runs[0].ID != b {
		t.Fatalf("Purgeable after purging the cancelled run = %v", ids(runs))
	}
	if r, _ := s.GetRun(ctx, kept); r.PurgedAt == nil || !r.Retain {
		t.Fatalf("retained run = %+v", r)
	}
	// LatestRun and Progress carry the new fields too.
	if lr, ok, _ := s.LatestRun(ctx, "a"); !ok || lr.PurgedAt == nil {
		t.Fatalf("LatestRun = %+v", lr)
	}
	if p, err := s.Progress(ctx, a); err != nil || p.Run.PurgedAt == nil {
		t.Fatalf("Progress.Run = %+v, %v", p.Run, err)
	}
}

func ids(runs []store.Run) []string {
	var out []string
	for _, r := range runs {
		out = append(out, r.Name)
	}
	return out
}

func TestRunRetainRoundTrip(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := store.NewRunID()
	if err := s.CreateRun(ctx, store.Run{ID: id, Name: "kept", ConfigHash: "h", Retain: true}); err != nil {
		t.Fatal(err)
	}
	if r, err := s.GetRun(ctx, id); err != nil || !r.Retain || r.PurgedAt != nil || r.WrappedKey != nil {
		t.Fatalf("run = %+v, %v", r, err)
	}
}

func TestHolders(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := plannedRun(t, s, "acme", []string{"C1", "C2", "C3"}, 1)
	if hs, err := s.Holders(ctx, id); err != nil || len(hs) != 0 {
		t.Fatalf("Holders with no leases = %v, %v", hs, err)
	}
	claim := func(holder, stage string, d time.Duration) *store.Lease {
		t.Helper()
		l, err := s.Claim(ctx, store.ClaimFilter{Stages: []string{stage}, RunID: id}, holder, d)
		if err != nil || l == nil {
			t.Fatalf("Claim(%s, %s) = %v, %v", holder, stage, l, err)
		}
		return l
	}
	// zed: two extract lanes and the team lane, with different expiries.
	claim("zed", store.StageExtract, time.Minute)
	z2 := claim("zed", store.StageExtract, 10*time.Minute)
	claim("zed", store.StageTeam, 2*time.Minute)
	// amy: one extract lane.
	amy := claim("amy", store.StageExtract, time.Minute)
	// A holder in another run does not show up.
	other := plannedRun(t, s, "other", []string{"C1"}, 1)
	mustClaim(t, s, other, "stranger")

	hs, err := s.Holders(ctx, id)
	if err != nil || len(hs) != 2 {
		t.Fatalf("Holders = %+v, %v", hs, err)
	}
	if h := hs[0]; h.Holder != "amy" || h.Lanes != 1 || !reflect.DeepEqual(h.Stages, []string{"extract"}) || !h.LeaseExpires.Equal(amy.Expires) {
		t.Fatalf("amy = %+v", h)
	}
	if h := hs[1]; h.Holder != "zed" || h.Lanes != 3 || !reflect.DeepEqual(h.Stages, []string{"extract", "team"}) ||
		!h.LeaseExpires.Equal(z2.Expires) || h.LeaseExpires.Location() != time.UTC {
		t.Fatalf("zed = %+v (want expiry %v)", h, z2.Expires)
	}

	// Expired leases are ignored: one of zed's, and amy's only one.
	exec(t, s, `UPDATE lanes SET lease_expires = now() - interval '1 second' WHERE run_id = $1 AND id = $2`, id, z2.LaneID)
	exec(t, s, `UPDATE lanes SET lease_expires = now() - interval '1 second' WHERE run_id = $1 AND holder = 'amy'`, id)
	hs, err = s.Holders(ctx, id)
	if err != nil || len(hs) != 1 || hs[0].Holder != "zed" || hs[0].Lanes != 2 {
		t.Fatalf("Holders after expiry = %+v, %v", hs, err)
	}
	if d := time.Until(hs[0].LeaseExpires); d < time.Minute || d > 3*time.Minute {
		t.Fatalf("zed's latest expiry is %v away, want about two minutes", d)
	}
	// Released leases disappear.
	if err := s.CancelRun(ctx, id); err != nil {
		t.Fatal(err)
	}
	if hs, _ := s.Holders(ctx, id); len(hs) != 0 {
		t.Fatalf("Holders after cancel = %+v", hs)
	}
}

func TestLogQuery(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := plannedRun(t, s, "acme", []string{"C1", "C2", "C3"}, 1)
	base := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	ops := []string{"message.import", "message.adopt", "channel.create", "message.fail"}
	// Three streams of different lengths; times interleave across streams.
	for si, ch := range []string{"C1", "C2", "C3"} {
		l := claimLane(t, s, id, store.StageExtract, "extract:"+ch, "worker-"+ch)
		n := []int{9, 4, 7}[si]
		err := s.Fenced(ctx, l, func(tx store.Tx) error {
			for i := range n {
				op := ops[(i+si)%len(ops)]
				outcome := "ok"
				if op == "message.fail" {
					outcome = "error"
				}
				detail := fmt.Sprintf("entry %d of %s", i, ch)
				switch i {
				case 1:
					detail = "100% done_ok" // LIKE wildcards, literally
				case 2:
					detail = "Graph THROTTLED the request"
				}
				if err := tx.Log(ctx, store.LogEntry{
					At: base.Add(time.Duration(i*10+si) * time.Minute), Op: op, Outcome: outcome,
					SourceID: fmt.Sprintf("%s/17000000%02d.000100", ch, i), TargetID: fmt.Sprintf("T-%s-%d", strings.ToLower(ch), i),
					Detail: detail,
				}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// Another run's log must never appear.
	other := plannedRun(t, s, "other", []string{"C1"}, 1)
	logN(t, s, claimLane(t, s, other, store.StageExtract, "extract:C1", "x"), 3)

	all, err := s.LogEntries(ctx, id, "", 0, 0)
	if err != nil || len(all) != 20 {
		t.Fatalf("%d entries, %v", len(all), err)
	}
	lower := strings.ToLower
	filters := []struct {
		name string
		f    store.LogFilter
		keep func(e store.LogEntry) bool
		want int // expected matches, as a sanity check on the test itself
	}{
		{"none", store.LogFilter{}, func(store.LogEntry) bool { return true }, 20},
		{"stream", store.LogFilter{Stream: "extract:C2"}, func(e store.LogEntry) bool { return e.Stream == "extract:C2" }, 4},
		{"op", store.LogFilter{Op: "message.import"}, func(e store.LogEntry) bool { return e.Op == "message.import" }, -1},
		{"outcome", store.LogFilter{Outcome: "error"}, func(e store.LogEntry) bool { return e.Outcome == "error" }, -1},
		{"contains detail, case-insensitive", store.LogFilter{Contains: "throttled"}, func(e store.LogEntry) bool { return strings.Contains(lower(e.Detail), "throttled") }, 3},
		{"contains source", store.LogFilter{Contains: "c3/1700000003"}, func(e store.LogEntry) bool { return strings.Contains(lower(e.SourceID), "c3/1700000003") }, 1},
		{"contains target", store.LogFilter{Contains: "T-C1-"}, func(e store.LogEntry) bool { return strings.Contains(lower(e.TargetID), "t-c1-") }, 9},
		{"contains literal percent", store.LogFilter{Contains: "100%"}, func(e store.LogEntry) bool { return strings.Contains(e.Detail, "100%") }, 3},
		{"contains literal underscore", store.LogFilter{Contains: "done_ok"}, func(e store.LogEntry) bool { return strings.Contains(e.Detail, "done_ok") }, 3},
		{"percent is not a wildcard", store.LogFilter{Contains: "1%done"}, func(store.LogEntry) bool { return false }, 0},
		{"underscore is not a wildcard", store.LogFilter{Contains: "entry_0"}, func(store.LogEntry) bool { return false }, 0},
		{"backslash", store.LogFilter{Contains: `\`}, func(store.LogEntry) bool { return false }, 0},
		{"since", store.LogFilter{Since: base.Add(30 * time.Minute)}, func(e store.LogEntry) bool { return !e.At.Before(base.Add(30 * time.Minute)) }, -1},
		{"until", store.LogFilter{Until: base.Add(30 * time.Minute)}, func(e store.LogEntry) bool { return e.At.Before(base.Add(30 * time.Minute)) }, 9},
		{"window", store.LogFilter{Since: base.Add(10 * time.Minute), Until: base.Add(21 * time.Minute)},
			func(e store.LogEntry) bool {
				return !e.At.Before(base.Add(10*time.Minute)) && e.At.Before(base.Add(21*time.Minute))
			}, 4},
		{"combined", store.LogFilter{Stream: "extract:C1", Outcome: "ok", Contains: "ENTRY"},
			func(e store.LogEntry) bool {
				return e.Stream == "extract:C1" && e.Outcome == "ok" && strings.Contains(lower(e.Detail), "entry")
			}, -1},
		{"no match", store.LogFilter{Op: "team.create"}, func(store.LogEntry) bool { return false }, 0},
	}
	for _, tc := range filters {
		var want []store.LogEntry
		for _, e := range all {
			if tc.keep(e) {
				want = append(want, e)
			}
		}
		if tc.want >= 0 && len(want) != tc.want {
			t.Fatalf("%s: the test expects %d matches but its own predicate finds %d", tc.name, tc.want, len(want))
		}
		for _, limit := range []int{1, 2, 3, 5, 20, 21} {
			f := tc.f
			f.Limit = limit
			var got []store.LogEntry
			pages := 0
			for {
				page, next, err := s.LogQuery(ctx, id, f)
				if err != nil {
					t.Fatalf("%s limit %d: %v", tc.name, limit, err)
				}
				pages++
				if len(page) > limit {
					t.Fatalf("%s limit %d: a page has %d entries", tc.name, limit, len(page))
				}
				got = append(got, page...)
				if next == (store.LogCursor{}) {
					break
				}
				if len(page) != limit {
					t.Fatalf("%s limit %d: a short page (%d) came with a cursor", tc.name, limit, len(page))
				}
				if last := page[len(page)-1]; next.Stream != last.Stream || next.Seq != last.Seq {
					t.Fatalf("%s limit %d: cursor %+v is not the last entry %s/%d", tc.name, limit, next, last.Stream, last.Seq)
				}
				f.After = next
				if pages > 100 {
					t.Fatalf("%s limit %d: paging does not end", tc.name, limit)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s limit %d: got %d entries %v, want %d %v", tc.name, limit, len(got), positions(got), len(want), positions(want))
			}
			// No empty trailing page: an exact multiple ends on a full
			// page with a zero cursor.
			if wantPages := max(1, (len(want)+limit-1)/limit); pages != wantPages {
				t.Fatalf("%s limit %d: %d pages for %d entries, want %d", tc.name, limit, pages, len(want), wantPages)
			}
		}
	}

	// After, used directly: mid-stream, and from a stream's end.
	page, _, err := s.LogQuery(ctx, id, store.LogFilter{After: store.LogCursor{Stream: "extract:C1", Seq: 7}})
	if err != nil || len(page) != 13 || page[0].Stream != "extract:C1" || page[0].Seq != 8 || page[2].Stream != "extract:C2" || page[2].Seq != 1 {
		t.Fatalf("After C1/7: %v, %v", positions(page), err)
	}
	page, _, _ = s.LogQuery(ctx, id, store.LogFilter{After: store.LogCursor{Stream: "extract:C3", Seq: 7}})
	if len(page) != 0 {
		t.Fatalf("After the last entry: %v", positions(page))
	}
	// Entries carry everything, in UTC.
	page, _, _ = s.LogQuery(ctx, id, store.LogFilter{Stream: "extract:C2", Limit: 1})
	e := page[0]
	if e.RunID != id || e.Seq != 1 || e.Actor != "worker-C2" || e.EntryHash != store.LogEntryHash(e) || e.At.Location() != time.UTC || !e.At.Equal(base.Add(time.Minute)) {
		t.Fatalf("entry = %+v", e)
	}
	if page, next, err := s.LogQuery(ctx, "missing", store.LogFilter{}); err != nil || len(page) != 0 || next != (store.LogCursor{}) {
		t.Fatalf("LogQuery for an unknown run = %v, %+v, %v", page, next, err)
	}
}

func positions(es []store.LogEntry) []string {
	var out []string
	for _, e := range es {
		out = append(out, fmt.Sprintf("%s/%d", e.Stream, e.Seq))
	}
	return out
}

// The default page is 200 and the cap 5000.
func TestLogQueryLimits(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := plannedRun(t, s, "acme", []string{"C1"}, 1)
	l := mustClaim(t, s, id, "d", store.StageExtract)
	const total = 5003
	for start := 0; start < total; start += 1000 {
		err := s.Fenced(ctx, l, func(tx store.Tx) error {
			for i := start; i < min(start+1000, total); i++ {
				if err := tx.Log(ctx, store.LogEntry{Op: "message.import", Outcome: "ok"}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ limit, want int }{{0, 200}, {-5, 200}, {200, 200}, {201, 201}, {5000, 5000}, {5001, 5000}, {1 << 30, 5000}} {
		page, next, err := s.LogQuery(ctx, id, store.LogFilter{Limit: tc.limit})
		if err != nil || len(page) != tc.want {
			t.Fatalf("Limit %d: %d entries, %v; want %d", tc.limit, len(page), err, tc.want)
		}
		if next.Stream != "extract:C1" || next.Seq != int64(tc.want) {
			t.Fatalf("Limit %d: cursor %+v", tc.limit, next)
		}
	}
	page, next, err := s.LogQuery(ctx, id, store.LogFilter{Limit: 5000, After: store.LogCursor{Stream: "extract:C1", Seq: 5000}})
	if err != nil || len(page) != 3 || next != (store.LogCursor{}) {
		t.Fatalf("last page: %d entries, cursor %+v, %v", len(page), next, err)
	}
	if err := s.VerifyLog(ctx, id); err != nil {
		t.Fatal(err)
	}
}

func TestProgressChannelFiles(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := plannedRun(t, s, "acme", []string{"C1", "C2", "C3"}, 1)
	l := mustClaim(t, s, id, "x", store.StageExtract)
	err := s.Fenced(ctx, l, func(tx store.Tx) error {
		if err := tx.PutFiles(ctx, []store.FileMapping{
			file("C1", "F1", 10, 0), file("C1", "F2", 10, 0), file("C1", "F3", 10, 0), file("C1", "F4", 10, 0), file("C1", "F5", 10, 0),
			file("C2", "F1", 10, 0),
		}); err != nil {
			return err
		}
		if err := tx.MarkFileStaged(ctx, "C1", "F1", "k1", "h1", 10); err != nil { // staged: not done
			return err
		}
		if err := tx.MarkFileUploaded(ctx, "C1", "F2", "d", "i2", "u", "e"); err != nil {
			return err
		}
		if err := tx.MarkFileSettled(ctx, "C1", "F3", store.FileSkipped, "c", "r"); err != nil {
			return err
		}
		return tx.MarkFileSettled(ctx, "C1", "F4", store.FileFailed, "c", "r")
	})
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.Progress(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][2]int{}
	for _, c := range p.Channels {
		got[c.SlackID] = [2]int{c.Files, c.FilesDone}
	}
	want := map[string][2]int{"C1": {5, 3}, "C2": {1, 0}, "C3": {0, 0}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("per-channel files = %v, want %v", got, want)
	}
	if p.Files != 6 || p.FilesUploaded != 1 || p.FilesSkipped != 1 || p.FilesFailed != 1 {
		t.Fatalf("totals = %+v", p)
	}
}

// A database at schema version 2 with rows is migrated to version 3.
func TestMigrateFromV2WithRows(t *testing.T) {
	t.Parallel()
	url, schema := storetest.NewSchema(t)
	s, err := store.Open(t.Context(), url, store.WithSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ctx := t.Context()
	if err := store.MigrateTo(ctx, s, 2); err != nil {
		t.Fatal(err)
	}
	exec(t, s, `INSERT INTO runs (id, name, config, config_hash, status, planned_at, wrapped_key, finished_at) VALUES ('r1', 'acme', '', 'h', 'completed', now(), '\x0a0b', now())`)
	exec(t, s, `INSERT INTO runs (id, name, config, config_hash, status, planned_at) VALUES ('r2', 'live', '', 'h', 'running', now())`)
	exec(t, s, `INSERT INTO map_teams (run_id, idx, name) VALUES ('r1', 0, 'T'), ('r2', 0, 'T')`)
	exec(t, s, `INSERT INTO map_channels (run_id, slack_id, team_index, state) VALUES ('r1', 'C1', 0, 'migrating'), ('r2', 'C1', 0, 'planned')`)
	stages := []string{"plan", "extract", "transform", "team", "files", "load"}
	for _, st := range stages {
		exec(t, s, `INSERT INTO lanes (run_id, id, stage, run_created, status) SELECT 'r1', $1, $1, created_at, 'done' FROM runs WHERE id = 'r1'`, st)
		exec(t, s, `INSERT INTO lanes (run_id, id, stage, run_created, fence) SELECT 'r2', $1, $1, created_at, 3 FROM runs WHERE id = 'r2'`, st)
		exec(t, s, `INSERT INTO batches (run_id, lane_id, seq) VALUES ('r2', $1, 0)`, st)
	}
	if _, err := s.Pool().Exec(ctx, `UPDATE runs SET status = 'finalizing' WHERE id = 'r1'`); err == nil {
		t.Fatal("version 2 accepted the finalizing status")
	}

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM schema_migrations`); n != schemaVersion {
		t.Fatalf("schema_migrations has %d rows", n)
	}
	for i, st := range stages {
		if got := scalar[int](t, s, `SELECT stage_rank FROM lanes WHERE run_id = 'r2' AND id = $1`, st); got != i {
			t.Errorf("stage %s: rank %d, want %d", st, got, i)
		}
	}
	// Old runs read back with the new fields at their defaults.
	r, err := s.GetRun(ctx, "r1")
	if err != nil || r.Retain || r.PurgedAt != nil || string(r.WrappedKey) != "\x0a\x0b" || r.Status != store.RunCompleted {
		t.Fatalf("r1 = %+v, %v", r, err)
	}
	// The live run still claims in order, with its fences intact.
	var got []string
	for {
		l, err := s.Claim(ctx, store.ClaimFilter{Stages: allStages, RunID: "r2"}, "h", ttl)
		if err != nil {
			t.Fatal(err)
		}
		if l == nil {
			break
		}
		if l.Fence != 4 {
			t.Errorf("lane %s: fence %d, want 4", l.LaneID, l.Fence)
		}
		got = append(got, l.LaneID)
	}
	if !reflect.DeepEqual(got, stages) {
		t.Fatalf("claim order = %v, want %v", got, stages)
	}
	// And the old completed run can be finalized: finalize before members.
	p := store.FinalizePlan{
		Lanes:   []store.Lane{{ID: "members:0", Stage: store.StageMembers}, {ID: "finalize:C1", Stage: store.StageFinalize, Key: "C1"}},
		Batches: []store.Batch{{LaneID: "members:0", Seq: 0}, {LaneID: "finalize:C1", Seq: 0}},
	}
	if err := s.BeginFinalize(ctx, "r1", p); err != nil {
		t.Fatalf("BeginFinalize: %v", err)
	}
	ranks := map[string]int{}
	for _, st := range []string{"finalize:C1", "members:0"} {
		ranks[st] = scalar[int](t, s, `SELECT stage_rank FROM lanes WHERE run_id = 'r1' AND id = $1`, st)
	}
	if ranks["finalize:C1"] != 6 || ranks["members:0"] != 7 {
		t.Fatalf("ranks = %v, want finalize 6, members 7", ranks)
	}
	l := mustClaim(t, s, "r1", "h")
	if l.LaneID != "finalize:C1" {
		t.Fatalf("first claim = %s, want finalize:C1", l.LaneID)
	}
	release(t, s, l)
	if n := drainFinalize(t, s, "r1"); n != 2 {
		t.Fatalf("worked %d batches", n)
	}
	if changed := reconcileOne(t, s, "r1"); changed == nil || changed.Status != store.RunFinalized {
		t.Fatalf("Reconcile = %+v", changed)
	}
	var names []string
	rows, _ := s.Purgeable(ctx)
	for _, r := range rows {
		names = append(names, r.ID)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"r1"}) {
		t.Fatalf("Purgeable = %v", names)
	}
}

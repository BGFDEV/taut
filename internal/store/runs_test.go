package store_test

import (
	"regexp"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/store"
)

func TestNewRunID(t *testing.T) {
	t.Parallel()
	urlSafe := regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	seen := map[string]bool{}
	var ids []string
	for range 200 {
		id := store.NewRunID()
		if !urlSafe.MatchString(id) {
			t.Fatalf("run ID %q is not URL-safe", id)
		}
		if seen[id] {
			t.Fatalf("duplicate run ID %q", id)
		}
		seen[id] = true
		ids = append(ids, id)
		time.Sleep(2 * time.Microsecond)
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatal("run IDs are not sorted by creation time")
	}
}

func TestRunLifecycle(t *testing.T) {
	s := open(t)
	ctx := t.Context()

	if _, err := s.GetRun(ctx, "missing"); err == nil {
		t.Fatal("GetRun(missing) succeeded")
	} else {
		wantErr(t, err, store.ErrNotFound)
	}
	if _, ok, err := s.ActiveRun(ctx, "acme"); err != nil || ok {
		t.Fatalf("ActiveRun on empty store = %v, %v", ok, err)
	}
	if _, ok, err := s.LatestRun(ctx, "acme"); err != nil || ok {
		t.Fatalf("LatestRun on empty store = %v, %v", ok, err)
	}

	id := store.NewRunID()
	in := store.Run{ID: id, Name: "acme", Config: []byte("apiVersion: v1\n"), ConfigHash: "abc", WrappedKey: []byte{1, 2, 3}}
	if err := s.CreateRun(ctx, in); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	r, err := s.GetRun(ctx, id)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if r.Status != store.RunAwaitingExport || r.Name != "acme" || string(r.Config) != "apiVersion: v1\n" ||
		r.ConfigHash != "abc" || string(r.WrappedKey) != "\x01\x02\x03" || r.ExportKey != "" || r.FinishedAt != nil {
		t.Fatalf("unexpected run: %+v", r)
	}
	if r.CreatedAt.Location() != time.UTC || r.UpdatedAt.Location() != time.UTC {
		t.Fatalf("timestamps not UTC: %v %v", r.CreatedAt, r.UpdatedAt)
	}
	if d := time.Since(r.CreatedAt); d < -time.Minute || d > time.Minute {
		t.Fatalf("CreatedAt %v is not now", r.CreatedAt)
	}
	// Nothing is claimable before the export exists.
	noClaim(t, s, id)

	// SetExport: queued, plan lane, idempotent.
	wantErr(t, s.SetExport(ctx, "missing", "k", "h", 1), store.ErrNotFound)
	for range 2 {
		if err := s.SetExport(ctx, id, "exports/a.zip", "deadbeef", 42); err != nil {
			t.Fatalf("SetExport: %v", err)
		}
	}
	if err := s.SetExport(ctx, id, "exports/other.zip", "cafe", 43); err == nil {
		t.Fatal("SetExport with a different export succeeded")
	}
	r, _ = s.GetRun(ctx, id)
	if r.Status != store.RunQueued || r.ExportKey != "exports/a.zip" || r.ExportSHA256 != "deadbeef" || r.ExportBytes != 42 {
		t.Fatalf("after SetExport: %+v", r)
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM lanes WHERE run_id = $1`, id); n != 1 {
		t.Fatalf("%d lanes after SetExport, want 1", n)
	}
	b, err := s.GetBatch(ctx, id, store.BatchRef{LaneID: "plan", Seq: 0})
	if err != nil {
		t.Fatalf("GetBatch(plan/0): %v", err)
	}
	if b.Key != "plan" || b.Input != "exports/a.zip" || b.Status != store.BatchPending || b.UpdatedAt.Location() != time.UTC {
		t.Fatalf("plan batch: %+v", b)
	}
	_, err = s.GetBatch(ctx, id, store.BatchRef{LaneID: "plan", Seq: 1})
	wantErr(t, err, store.ErrNotFound)

	// The plan lane is claimable, with stage plan and fence 1.
	l := mustClaim(t, s, id, "h1")
	if l.LaneID != "plan" || l.Stage != store.StagePlan || l.Fence != 1 || l.Holder != "h1" || l.RunID != id || l.Attempts != 0 {
		t.Fatalf("plan lease: %+v", l)
	}
	if l.Expires.Location() != time.UTC || time.Until(l.Expires) < 30*time.Second || time.Until(l.Expires) > 2*time.Minute {
		t.Fatalf("lease expiry %v is not about a minute away", l.Expires)
	}
	release(t, s, l)

	// Plan, work, reconcile to completed.
	applyPlan(t, s, id, testPlan([]string{"C1"}, 2))
	if st := runStatus(t, s, id); st != store.RunRunning {
		t.Fatalf("status after plan = %s", st)
	}
	if changed, err := s.Reconcile(ctx); err != nil || len(changed) != 0 {
		t.Fatalf("Reconcile mid-run = %v, %v; want no change", changed, err)
	}
	// 1 team + 2 extract + 2 transform + 3 load.
	if n := drain(t, s, id); n != 8 {
		t.Fatalf("drain completed %d batches, want 8", n)
	}
	changed, err := s.Reconcile(ctx)
	if err != nil || len(changed) != 1 || changed[0].ID != id || changed[0].Status != store.RunCompleted || changed[0].FinishedAt == nil {
		t.Fatalf("Reconcile = %+v, %v", changed, err)
	}
	if changed, err := s.Reconcile(ctx); err != nil || len(changed) != 0 {
		t.Fatalf("second Reconcile = %v, %v", changed, err)
	}
	if _, ok, _ := s.ActiveRun(ctx, "acme"); ok {
		t.Fatal("a completed run is still active")
	}
	if lr, ok, err := s.LatestRun(ctx, "acme"); err != nil || !ok || lr.ID != id {
		t.Fatalf("LatestRun = %v %v %v", lr.ID, ok, err)
	}
	// Cancelling a completed run leaves it completed.
	if err := s.CancelRun(ctx, id); err != nil {
		t.Fatalf("CancelRun(completed): %v", err)
	}
	if st := runStatus(t, s, id); st != store.RunCompleted {
		t.Fatalf("status = %s, want completed", st)
	}
	if _, err := s.RetryFailed(ctx, id); err == nil {
		t.Fatal("RetryFailed on a completed run succeeded")
	}
}

func TestActiveNameUniqueness(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	mk := func(name string) (string, error) {
		id := store.NewRunID()
		return id, s.CreateRun(ctx, store.Run{ID: id, Name: name, ConfigHash: "h"})
	}
	first, err := mk("acme")
	if err != nil {
		t.Fatal(err)
	}
	_, err = mk("acme")
	wantErr(t, err, store.ErrActiveRunExists)
	// A different name is fine.
	if _, err := mk("globex"); err != nil {
		t.Fatalf("CreateRun(globex): %v", err)
	}
	// The same ID is an error, but not ErrActiveRunExists.
	err = s.CreateRun(ctx, store.Run{ID: first, Name: "initech", ConfigHash: "h"})
	if err == nil || err == store.ErrActiveRunExists {
		t.Fatalf("duplicate ID: err = %v", err)
	}

	// Every active status blocks, including failed.
	for _, st := range []store.RunStatus{store.RunQueued, store.RunRunning, store.RunFailed, store.RunAwaitingExport} {
		exec(t, s, `UPDATE runs SET status = $2 WHERE id = $1`, first, string(st))
		if _, err := mk("acme"); err != store.ErrActiveRunExists {
			t.Fatalf("with first run %s: err = %v, want ErrActiveRunExists", st, err)
		}
	}
	ar, ok, err := s.ActiveRun(ctx, "acme")
	if err != nil || !ok || ar.ID != first {
		t.Fatalf("ActiveRun = %v %v %v", ar.ID, ok, err)
	}

	// After cancelling, a new run with the name can start.
	if err := s.CancelRun(ctx, first); err != nil {
		t.Fatal(err)
	}
	r, _ := s.GetRun(ctx, first)
	if r.Status != store.RunCancelled || r.FinishedAt == nil {
		t.Fatalf("cancelled run: %+v", r)
	}
	if err := s.CancelRun(ctx, first); err != nil {
		t.Fatalf("second CancelRun: %v", err)
	}
	wantErr(t, s.CancelRun(ctx, "missing"), store.ErrNotFound)
	if _, ok, _ := s.ActiveRun(ctx, "acme"); ok {
		t.Fatal("cancelled run still active")
	}
	second, err := mk("acme")
	if err != nil {
		t.Fatalf("CreateRun after cancel: %v", err)
	}
	ar, ok, _ = s.ActiveRun(ctx, "acme")
	if !ok || ar.ID != second {
		t.Fatalf("ActiveRun = %v, want %v", ar.ID, second)
	}
	lr, ok, _ := s.LatestRun(ctx, "acme")
	if !ok || lr.ID != second {
		t.Fatalf("LatestRun = %v, want %v", lr.ID, second)
	}

	// Racing creators: exactly one wins.
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, dups := 0, 0
	for range 8 {
		wg.Go(func() {
			_, err := mk("race")
			mu.Lock()
			defer mu.Unlock()
			switch err {
			case nil:
				wins++
			case store.ErrActiveRunExists:
				dups++
			default:
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	wg.Wait()
	if wins != 1 || dups != 7 {
		t.Fatalf("racing CreateRun: %d wins, %d duplicates; want 1 and 7", wins, dups)
	}
}

func TestCancelRunFencesHolders(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := plannedRun(t, s, "acme", []string{"C1", "C2"}, 2)
	l := mustClaim(t, s, id, "h1", store.StageExtract)
	if err := s.CancelRun(ctx, id); err != nil {
		t.Fatal(err)
	}
	called := false
	err := s.Fenced(ctx, l, func(tx store.Tx) error { called = true; return nil })
	wantErr(t, err, store.ErrFenced)
	if called {
		t.Fatal("fn ran under a cancelled run's lease")
	}
	wantErr(t, s.Renew(ctx, l, ttl), store.ErrFenced)
	_, err = s.NextBatch(ctx, l)
	wantErr(t, err, store.ErrFenced)
	if err := s.Release(ctx, l, time.Hour, "late"); err != nil {
		t.Fatalf("Release after cancel: %v", err)
	}
	noClaim(t, s, id)
	noClaim(t, s, "")
	if n := scalar[int](t, s, `SELECT count(*) FROM lanes WHERE run_id = $1 AND (holder IS NOT NULL OR attempts <> 0)`, id); n != 0 {
		t.Fatalf("%d lanes still held or touched after cancel", n)
	}
	if changed, err := s.Reconcile(ctx); err != nil || len(changed) != 0 {
		t.Fatalf("Reconcile changed a cancelled run: %v %v", changed, err)
	}
}

func TestReconcileFailedAndRetry(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := plannedRun(t, s, "acme", []string{"C1"}, 1)

	// extract:C1 fails permanently.
	l := mustClaim(t, s, id, "h1", store.StageExtract)
	err := s.Fenced(ctx, l, func(tx store.Tx) error { return tx.FailBatch(ctx, 0, "corrupt day file") })
	if err != nil {
		t.Fatal(err)
	}
	// Failing again is a no-op.
	if err := s.Fenced(ctx, l, func(tx store.Tx) error { return tx.FailBatch(ctx, 0, "again") }); err != nil {
		t.Fatalf("second FailBatch: %v", err)
	}
	release(t, s, l)
	b, _ := s.GetBatch(ctx, id, store.BatchRef{LaneID: "extract:C1", Seq: 0})
	if b.Status != store.BatchFailed || b.Error != "corrupt day file" {
		t.Fatalf("failed batch: %+v", b)
	}

	// Other lanes can still progress, so the run is not failed yet.
	if changed, _ := s.Reconcile(ctx); len(changed) != 0 {
		t.Fatalf("Reconcile failed the run while team:0 was runnable: %+v", changed)
	}
	// team:0 and load:C1's "open" batch can run; then everything left
	// waits on the failed lane.
	if n := drain(t, s, id); n != 2 {
		t.Fatalf("drain completed %d batches, want 2", n)
	}
	changed, err := s.Reconcile(ctx)
	if err != nil || len(changed) != 1 || changed[0].Status != store.RunFailed || changed[0].FinishedAt != nil {
		t.Fatalf("Reconcile = %+v, %v; want the run failed", changed, err)
	}
	// A failed run is not claimable, even though lanes are open.
	noClaim(t, s, id)
	if _, ok, _ := s.ActiveRun(ctx, "acme"); !ok {
		t.Fatal("a failed run should still be active")
	}

	n, err := s.RetryFailed(ctx, id)
	if err != nil || n != 1 {
		t.Fatalf("RetryFailed = %d, %v; want 1", n, err)
	}
	if st := runStatus(t, s, id); st != store.RunRunning {
		t.Fatalf("status after retry = %s", st)
	}
	b, _ = s.GetBatch(ctx, id, store.BatchRef{LaneID: "extract:C1", Seq: 0})
	if b.Status != store.BatchPending || b.Error != "" {
		t.Fatalf("retried batch: %+v", b)
	}
	if n, err := s.RetryFailed(ctx, id); err != nil || n != 0 {
		t.Fatalf("second RetryFailed = %d, %v", n, err)
	}
	if _, err := s.RetryFailed(ctx, "missing"); err == nil {
		t.Fatal("RetryFailed(missing) succeeded")
	}
	// extract, transform, load day.
	if n := drain(t, s, id); n != 3 {
		t.Fatalf("drain after retry completed %d batches, want 3", n)
	}
	changed, err = s.Reconcile(ctx)
	if err != nil || len(changed) != 1 || changed[0].Status != store.RunCompleted {
		t.Fatalf("Reconcile = %+v, %v; want completed", changed, err)
	}
}

// A run whose plan lane fails is reported failed, and retry puts it back
// to queued (it was never planned).
func TestReconcilePlanLaneFailure(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := newRun(t, s, "acme")
	l := mustClaim(t, s, id, "h1")
	if err := s.Fenced(ctx, l, func(tx store.Tx) error { return tx.FailBatch(ctx, 0, "bad export") }); err != nil {
		t.Fatal(err)
	}
	release(t, s, l)
	changed, err := s.Reconcile(ctx)
	if err != nil || len(changed) != 1 || changed[0].Status != store.RunFailed {
		t.Fatalf("Reconcile = %+v, %v", changed, err)
	}
	if n, err := s.RetryFailed(ctx, id); err != nil || n != 1 {
		t.Fatalf("RetryFailed = %d, %v", n, err)
	}
	if st := runStatus(t, s, id); st != store.RunQueued {
		t.Fatalf("status = %s, want queued", st)
	}
	applyPlan(t, s, id, testPlan([]string{"C1"}, 1))
	if st := runStatus(t, s, id); st != store.RunRunning {
		t.Fatalf("status = %s, want running", st)
	}
}

// Several controller replicas reconcile at once: each change is reported by
// exactly one of them.
func TestReconcileConcurrent(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	var ids []string
	for _, name := range []string{"a", "b", "c", "d"} {
		id := plannedRun(t, s, name, []string{"C1"}, 1)
		drain(t, s, id)
		ids = append(ids, id)
	}
	still := plannedRun(t, s, "still-running", []string{"C1"}, 1)

	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[string]int{}
	for range 8 {
		wg.Go(func() {
			// Loop: SKIP LOCKED means one call may leave runs for
			// another replica or a later tick.
			for range 5 {
				changed, err := s.Reconcile(ctx)
				if err != nil {
					t.Errorf("Reconcile: %v", err)
					return
				}
				mu.Lock()
				for _, r := range changed {
					if r.Status != store.RunCompleted {
						t.Errorf("run %s became %s", r.ID, r.Status)
					}
					got[r.ID]++
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	for _, id := range ids {
		if got[id] != 1 {
			t.Errorf("run %s reported completed %d times, want exactly once", id, got[id])
		}
	}
	if got[still] != 0 || runStatus(t, s, still) != store.RunRunning {
		t.Errorf("unfinished run was changed")
	}
}

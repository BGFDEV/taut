package store_test

import (
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/store"
)

func TestClaimArguments(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	if _, err := s.Claim(ctx, store.ClaimFilter{}, "h", ttl); err == nil {
		t.Error("Claim without stages succeeded")
	}
	if _, err := s.Claim(ctx, store.ClaimFilter{Stages: allStages}, "", ttl); err == nil {
		t.Error("Claim without a holder succeeded")
	}
	if _, err := s.Claim(ctx, store.ClaimFilter{Stages: allStages}, "h", 0); err == nil {
		t.Error("Claim with a zero ttl succeeded")
	}
	// An empty store has nothing to claim.
	noClaim(t, s, "")
}

func TestClaimDependencyGating(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := plannedRun(t, s, "acme", []string{"C1"}, 2)

	// Nothing downstream is claimable while extract is pending.
	noClaim(t, s, id, store.StageTransform)
	noClaim(t, s, id, store.StageLoad)

	// team:0 done -> load:C1's "open" batch becomes runnable, and only it.
	tl := mustClaim(t, s, id, "h", store.StageTeam)
	complete(t, s, tl, 0)
	release(t, s, tl)
	ll := mustClaim(t, s, id, "loader", store.StageLoad)
	b, err := s.NextBatch(ctx, ll)
	if err != nil || b == nil || b.Seq != 0 || b.Key != "open" {
		t.Fatalf("NextBatch = %+v, %v", b, err)
	}
	complete(t, s, ll, 0)
	// load:C1/1 needs transform:C1/0: the holder sees "blocked".
	if b, err := s.NextBatch(ctx, ll); err != nil || b != nil {
		t.Fatalf("NextBatch on a blocked lane = %+v, %v; want nil", b, err)
	}
	// Completing a blocked batch is refused by the store itself.
	err = s.Fenced(ctx, ll, func(tx store.Tx) error { return tx.CompleteBatch(ctx, 1, "", 0, nil) })
	wantErr(t, err, store.ErrOutOfOrder)
	release(t, s, ll)
	noClaim(t, s, id, store.StageLoad)

	// extract:C1/0 done -> transform:C1 becomes claimable right away.
	el := mustClaim(t, s, id, "h", store.StageExtract)
	complete(t, s, el, 0)
	xl := mustClaim(t, s, id, "h2", store.StageTransform)
	if xl.LaneID != "transform:C1" {
		t.Fatalf("claimed %s", xl.LaneID)
	}
	// transform:C1/0 is runnable, /1 is not (extract:C1/1 pending).
	if n := finishLane(t, s, xl); n != 1 {
		t.Fatalf("transform completed %d batches, want 1", n)
	}
	// Out-of-order completion within the lane is refused too.
	err = s.Fenced(ctx, el, func(tx store.Tx) error { return tx.CompleteBatch(ctx, 5, "", 0, nil) })
	wantErr(t, err, store.ErrNotFound)
	release(t, s, xl)
	noClaim(t, s, id, store.StageTransform)

	// transform:C1/0 done -> load:C1 claimable again, exactly one batch.
	ll = mustClaim(t, s, id, "loader", store.StageLoad)
	if ll.Fence != 2 {
		t.Fatalf("fence = %d, want 2", ll.Fence)
	}
	if n := finishLane(t, s, ll); n != 1 {
		t.Fatalf("load completed %d batches, want 1", n)
	}
	release(t, s, ll)
	noClaim(t, s, id, store.StageLoad)

	// Finish extract; the rest follows.
	complete(t, s, el, 1)
	release(t, s, el)
	if n := drain(t, s, id); n != 2 {
		t.Fatalf("drain = %d, want 2", n)
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM lanes WHERE run_id = $1 AND status <> 'done'`, id); n != 0 {
		t.Fatalf("%d lanes not done", n)
	}
	// A done lane keeps its record but is never claimable.
	noClaim(t, s, id)
}

func TestClaimOrder(t *testing.T) {
	s := open(t)
	old := plannedRun(t, s, "old", []string{"B", "A"}, 1)
	time.Sleep(5 * time.Millisecond)
	young := plannedRun(t, s, "young", []string{"A"}, 1)

	// Across runs: earlier stage first, then older run, then lane ID.
	want := []struct{ run, lane string }{
		{old, "extract:A"},
		{old, "extract:B"},
		{young, "extract:A"},
		{old, "team:0"},
		{young, "team:0"},
	}
	var leases []*store.Lease
	for i, w := range want {
		l := mustClaim(t, s, "", "h")
		if l.RunID != w.run || l.LaneID != w.lane {
			t.Fatalf("claim %d = %s %s, want %s %s", i, l.RunID, l.LaneID, w.run, w.lane)
		}
		leases = append(leases, l)
	}
	noClaim(t, s, "")

	// Completing the young run's extract makes its transform claimable,
	// and transform (rank 2) now beats the load lanes that team unblocks.
	complete(t, s, leases[2], 0)
	complete(t, s, leases[3], 0)
	complete(t, s, leases[4], 0)
	got := []string{}
	for {
		l, err := s.Claim(t.Context(), store.ClaimFilter{Stages: allStages}, "h", ttl)
		if err != nil {
			t.Fatal(err)
		}
		if l == nil {
			break
		}
		got = append(got, l.LaneID+"@"+map[string]string{old: "old", young: "young"}[l.RunID])
	}
	wantOrder := []string{"transform:A@young", "load:A@old", "load:B@old", "load:A@young"}
	if len(got) != len(wantOrder) {
		t.Fatalf("claims = %v, want %v", got, wantOrder)
	}
	for i := range got {
		if got[i] != wantOrder[i] {
			t.Fatalf("claims = %v, want %v", got, wantOrder)
		}
	}
}

func TestClaimFilters(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	a := plannedRun(t, s, "a", []string{"C1"}, 1)
	b := plannedRun(t, s, "b", []string{"C1"}, 1)

	// Stages filter: only team lanes, although extract ranks earlier.
	l := mustClaim(t, s, "", "h", store.StageTeam)
	if l.Stage != store.StageTeam || l.RunID != a {
		t.Fatalf("claimed %s of %s", l.LaneID, l.RunID)
	}
	// A stage with nothing claimable.
	noClaim(t, s, "", store.StageLoad)
	noClaim(t, s, "", store.StagePlan)
	noClaim(t, s, "", "no-such-stage")

	// Run filter: the younger run only.
	for _, want := range []string{"extract:C1", "team:0"} {
		l := mustClaim(t, s, b, "h")
		if l.RunID != b || l.LaneID != want {
			t.Fatalf("claimed %s of %s, want %s of run b", l.LaneID, l.RunID, want)
		}
	}
	noClaim(t, s, b)
	noClaim(t, s, "no-such-run")
	// Run a still has its extract lane.
	l = mustClaim(t, s, "", "h")
	if l.RunID != a || l.LaneID != "extract:C1" {
		t.Fatalf("claimed %s of %s", l.LaneID, l.RunID)
	}

	// Runs that are not queued or running are not claimable.
	for _, st := range []string{"failed", "cancelled", "completed", "awaiting_export"} {
		c := plannedRun(t, s, "c-"+st, []string{"C1"}, 1)
		exec(t, s, `UPDATE runs SET status = $2 WHERE id = $1`, c, st)
		noClaim(t, s, c)
		if lease, err := s.Claim(ctx, store.ClaimFilter{Stages: allStages}, "h", ttl); err != nil || lease != nil {
			t.Fatalf("claimed %+v from a %s run (%v)", lease, st, err)
		}
		exec(t, s, `UPDATE runs SET status = 'cancelled' WHERE id = $1`, c)
	}
}

func TestClaimBackoffAndAttempts(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := plannedRun(t, s, "acme", []string{"C1"}, 3)
	claim := func() *store.Lease { return mustClaim(t, s, id, "h", store.StageExtract) }

	l := claim()
	if l.Attempts != 0 {
		t.Fatalf("fresh lane Attempts = %d", l.Attempts)
	}
	// A failed attempt: not claimable until the backoff passes.
	if err := s.Release(ctx, l, time.Hour, "graph 503"); err != nil {
		t.Fatal(err)
	}
	noClaim(t, s, id, store.StageExtract)
	var attempts int
	var lastErr string
	var wait time.Duration
	if err := s.Pool().QueryRow(ctx, `SELECT attempts, last_error, not_before - now() FROM lanes WHERE run_id = $1 AND id = 'extract:C1'`, id).
		Scan(&attempts, &lastErr, &wait); err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || lastErr != "graph 503" || wait < 59*time.Minute || wait > 61*time.Minute {
		t.Fatalf("after failed release: attempts=%d error=%q wait=%v", attempts, lastErr, wait)
	}

	// A short real backoff expires by the database clock.
	exec(t, s, `UPDATE lanes SET not_before = now() WHERE run_id = $1 AND id = 'extract:C1'`, id)
	l = claim()
	if l.Attempts != 1 {
		t.Fatalf("Attempts = %d, want 1", l.Attempts)
	}
	if err := s.Release(ctx, l, 150*time.Millisecond, "again"); err != nil {
		t.Fatal(err)
	}
	noClaim(t, s, id, store.StageExtract)
	time.Sleep(250 * time.Millisecond)
	l = claim()
	if l.Attempts != 2 || l.Fence != 3 {
		t.Fatalf("lease = %+v, want Attempts 2, Fence 3", l)
	}

	// A completed batch means the lane is healthy again: attempts and the
	// last error are reset, and the next claim reports 0.
	complete(t, s, l, 0)
	if err := s.Pool().QueryRow(ctx, `SELECT attempts, last_error FROM lanes WHERE run_id = $1 AND id = 'extract:C1'`, id).Scan(&attempts, &lastErr); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || lastErr != "" {
		t.Fatalf("after CompleteBatch: attempts=%d error=%q, want 0 and empty", attempts, lastErr)
	}
	// Failing after progress starts counting from 1 again.
	if err := s.Release(ctx, l, time.Microsecond, "third"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	l = claim()
	if l.Attempts != 1 {
		t.Fatalf("Attempts after progress then failure = %d, want 1", l.Attempts)
	}

	// A clean release resets the counter and the error.
	release(t, s, l)
	if err := s.Pool().QueryRow(ctx, `SELECT attempts, last_error FROM lanes WHERE run_id = $1 AND id = 'extract:C1'`, id).Scan(&attempts, &lastErr); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || lastErr != "" {
		t.Fatalf("after clean release: attempts=%d error=%q", attempts, lastErr)
	}
	l = claim()
	if l.Attempts != 0 {
		t.Fatalf("Attempts after clean release = %d, want 0", l.Attempts)
	}

	// RetryFailed clears a pending backoff.
	if err := s.Release(ctx, l, time.Hour, "wait"); err != nil {
		t.Fatal(err)
	}
	noClaim(t, s, id, store.StageExtract)
	if n, err := s.RetryFailed(ctx, id); err != nil || n != 0 {
		t.Fatalf("RetryFailed = %d, %v", n, err)
	}
	l = claim()
	if l.Attempts != 0 {
		t.Fatalf("Attempts after RetryFailed = %d", l.Attempts)
	}
}

func TestCursorAndLeaseFields(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := plannedRun(t, s, "acme", []string{"C1"}, 1)
	l := mustClaim(t, s, id, "h", store.StageExtract)
	if l.Stage != store.StageExtract || l.Key != "C1" || l.Cursor != nil {
		t.Fatalf("lease = %+v", l)
	}
	cur := []byte(`{"last_ms":  1700000000123}`)
	if err := s.Fenced(ctx, l, func(tx store.Tx) error { return tx.SetCursor(ctx, cur) }); err != nil {
		t.Fatal(err)
	}
	if err := s.Fenced(ctx, l, func(tx store.Tx) error { return tx.SetCursor(ctx, []byte("{")) }); err == nil {
		t.Fatal("SetCursor accepted invalid JSON")
	}
	release(t, s, l)
	l = mustClaim(t, s, id, "h2", store.StageExtract)
	if string(l.Cursor) != string(cur) {
		t.Fatalf("cursor = %s, want it byte for byte %s", l.Cursor, cur)
	}
	// Batch output round-trips.
	stats := []byte(`{"msgs": 3}`)
	if err := s.Fenced(ctx, l, func(tx store.Tx) error { return tx.CompleteBatch(ctx, 0, "norm/C1/0", 3, stats) }); err != nil {
		t.Fatal(err)
	}
	// Completing again is a no-op and keeps the first result.
	if err := s.Fenced(ctx, l, func(tx store.Tx) error { return tx.CompleteBatch(ctx, 0, "other", 9, nil) }); err != nil {
		t.Fatalf("second CompleteBatch: %v", err)
	}
	b, err := s.GetBatch(ctx, id, store.BatchRef{LaneID: "extract:C1", Seq: 0})
	if err != nil || b.Status != store.BatchDone || b.Output != "norm/C1/0" || b.Items != 3 || string(b.Stats) != string(stats) {
		t.Fatalf("batch = %+v, %v", b, err)
	}
	if st := scalar[string](t, s, `SELECT status FROM lanes WHERE run_id = $1 AND id = 'extract:C1'`, id); st != "done" {
		t.Fatalf("lane status = %s, want done", st)
	}
	// A done batch cannot be failed.
	if err := s.Fenced(ctx, l, func(tx store.Tx) error { return tx.FailBatch(ctx, 0, "x") }); err == nil {
		t.Fatal("FailBatch on a done batch succeeded")
	}
	if b, err := s.NextBatch(ctx, l); err != nil || b != nil {
		t.Fatalf("NextBatch on a done lane = %+v, %v", b, err)
	}
}

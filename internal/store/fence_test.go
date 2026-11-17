package store_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/bgfdev/taut/internal/store"
)

// Holder A's lease expires for real and B takes the lane over. From then on
// A can do nothing, and nothing A tries disturbs B.
func TestFencingAfterTakeover(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := plannedRun(t, s, "acme", []string{"C1"}, 3)
	filter := store.ClaimFilter{Stages: []string{store.StageExtract}, RunID: id}

	a, err := s.Claim(ctx, filter, "A", 150*time.Millisecond)
	if err != nil || a == nil {
		t.Fatalf("A's claim: %v %v", a, err)
	}
	// While A's lease is live nobody else gets the lane.
	noClaim(t, s, id, store.StageExtract)
	// A works under its lease.
	complete(t, s, a, 0)

	time.Sleep(300 * time.Millisecond) // A stalls; the lease expires.

	b, err := s.Claim(ctx, filter, "B", ttl)
	if err != nil || b == nil {
		t.Fatalf("B's takeover: %v %v", b, err)
	}
	if b.LaneID != a.LaneID || b.Fence != a.Fence+1 || b.Holder != "B" {
		t.Fatalf("B's lease = %+v, want lane %s fence %d", b, a.LaneID, a.Fence+1)
	}

	// A's write is refused, fn is not called, nothing is written.
	called := false
	err = s.Fenced(ctx, a, func(tx store.Tx) error {
		called = true
		return tx.CompleteBatch(ctx, 1, "from-A", 1, nil)
	})
	wantErr(t, err, store.ErrFenced)
	if called {
		t.Fatal("fn was called under a lost lease")
	}
	bt, _ := s.GetBatch(ctx, id, store.BatchRef{LaneID: a.LaneID, Seq: 1})
	if bt.Status != store.BatchPending || bt.Output != "" {
		t.Fatalf("batch 1 was written by a fenced holder: %+v", bt)
	}
	// A cannot renew, and learns it lost the lane from NextBatch too.
	before := a.Expires
	wantErr(t, s.Renew(ctx, a, ttl), store.ErrFenced)
	if !a.Expires.Equal(before) {
		t.Fatal("a failed Renew changed the lease")
	}
	_, err = s.NextBatch(ctx, a)
	wantErr(t, err, store.ErrFenced)

	// A's release, clean or failed, is a no-op and does not disturb B.
	if err := s.Release(ctx, a, 0, ""); err != nil {
		t.Fatalf("A's release: %v", err)
	}
	if err := s.Release(ctx, a, time.Hour, "A's late error"); err != nil {
		t.Fatalf("A's release: %v", err)
	}
	var holder, lastErr string
	var fence int64
	var attempts int
	if err := s.Pool().QueryRow(ctx, `SELECT holder, fence, attempts, last_error FROM lanes WHERE run_id = $1 AND id = $2`, id, a.LaneID).
		Scan(&holder, &fence, &attempts, &lastErr); err != nil {
		t.Fatal(err)
	}
	if holder != "B" || fence != b.Fence || attempts != 0 || lastErr != "" {
		t.Fatalf("lane after A's releases: holder=%s fence=%d attempts=%d error=%q", holder, fence, attempts, lastErr)
	}

	// B is unaffected: it renews, sees A's committed work, and continues.
	old := b.Expires
	if err := s.Renew(ctx, b, 2*ttl); err != nil {
		t.Fatalf("B's renew: %v", err)
	}
	if !b.Expires.After(old) || b.Expires.Location() != time.UTC {
		t.Fatalf("Renew did not extend: %v -> %v", old, b.Expires)
	}
	nb, err := s.NextBatch(ctx, b)
	if err != nil || nb == nil || nb.Seq != 1 {
		t.Fatalf("B's next batch = %+v, %v; want seq 1", nb, err)
	}
	if n := finishLane(t, s, b); n != 2 {
		t.Fatalf("B completed %d batches, want 2", n)
	}
	release(t, s, b)

	// The same holder name with an old fence is fenced as well.
	stale := *b
	stale.Fence--
	err = s.Fenced(ctx, &stale, func(store.Tx) error { return nil })
	wantErr(t, err, store.ErrFenced)
	// And a released lease is not current any more.
	err = s.Fenced(ctx, b, func(store.Tx) error { return nil })
	wantErr(t, err, store.ErrFenced)
}

// An expired lease that nobody has taken over still works: expiry makes the
// lane available, it does not revoke the fence.
func TestExpiredButUnclaimedLeaseStillWrites(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := plannedRun(t, s, "acme", []string{"C1"}, 2)
	a := mustClaim(t, s, id, "A", store.StageExtract)
	exec(t, s, `UPDATE lanes SET lease_expires = now() - interval '1 hour' WHERE run_id = $1 AND id = $2`, id, a.LaneID)
	complete(t, s, a, 0)
	if err := s.Renew(ctx, a, ttl); err != nil {
		t.Fatalf("Renew of an expired, unclaimed lease: %v", err)
	}
	if time.Until(a.Expires) < 30*time.Second {
		t.Fatalf("Renew did not move the expiry: %v", a.Expires)
	}
	// Renewed, so no longer available.
	noClaim(t, s, id, store.StageExtract)
}

// While a Fenced transaction is open, a takeover of the same lane cannot
// happen, even though the lease has expired: Claim skips the locked lane.
// The takeover succeeds only after the transaction commits, so a write can
// never commit under a superseded fence.
func TestFencedExcludesTakeover(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := plannedRun(t, s, "acme", []string{"C1"}, 2)
	filter := store.ClaimFilter{Stages: []string{store.StageExtract}, RunID: id}
	a := mustClaim(t, s, id, "A", store.StageExtract)
	// A's lease is long expired: the lane is up for grabs.
	exec(t, s, `UPDATE lanes SET lease_expires = now() - interval '1 hour' WHERE run_id = $1 AND id = $2`, id, a.LaneID)

	inTx := make(chan struct{})
	proceed := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- s.Fenced(ctx, a, func(tx store.Tx) error {
			if err := tx.CompleteBatch(ctx, 0, "from-A", 1, nil); err != nil {
				return err
			}
			close(inTx)
			<-proceed
			return nil
		})
	}()
	<-inTx

	// A's transaction is open and holds the lane.
	for i := range 5 {
		l, err := s.Claim(ctx, filter, "B", ttl)
		if err != nil {
			t.Fatalf("B's claim: %v", err)
		}
		if l != nil {
			t.Fatalf("attempt %d: B took the lane (fence %d) while A's fenced transaction was open", i, l.Fence)
		}
	}
	// The lock is really there: a NOWAIT lock attempt is refused.
	_, err := s.Pool().Exec(ctx, `SELECT 1 FROM lanes WHERE run_id = $1 AND id = $2 FOR UPDATE NOWAIT`, id, a.LaneID)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "55P03" {
		t.Fatalf("NOWAIT lock on the lane: %v, want lock_not_available", err)
	}
	// A Renew by someone holding a copy of the lease waits for the
	// transaction rather than slipping in between.
	renewed := make(chan error, 1)
	go func() {
		c := *a
		renewed <- s.Renew(ctx, &c, time.Millisecond)
	}()
	select {
	case err := <-renewed:
		t.Fatalf("Renew returned (%v) while the fenced transaction was open", err)
	case <-time.After(150 * time.Millisecond):
	}
	// A's uncommitted write is invisible.
	if b, _ := s.GetBatch(ctx, id, store.BatchRef{LaneID: a.LaneID, Seq: 0}); b.Status != store.BatchPending {
		t.Fatalf("uncommitted batch visible as %s", b.Status)
	}

	close(proceed)
	if err := <-done; err != nil {
		t.Fatalf("A's fenced transaction: %v", err)
	}
	if err := <-renewed; err != nil {
		t.Fatalf("Renew after the transaction: %v", err)
	}
	time.Sleep(20 * time.Millisecond) // the 1 ms renewal lapses

	// Now the takeover goes through, and B sees A's committed batch.
	b, err := s.Claim(ctx, filter, "B", ttl)
	if err != nil || b == nil || b.Fence != a.Fence+1 {
		t.Fatalf("B's claim after commit = %+v, %v", b, err)
	}
	nb, err := s.NextBatch(ctx, b)
	if err != nil || nb == nil || nb.Seq != 1 {
		t.Fatalf("B's next batch = %+v, %v; want seq 1 (A's batch 0 committed)", nb, err)
	}
	err = s.Fenced(ctx, a, func(store.Tx) error { t.Error("fn called under superseded fence"); return nil })
	wantErr(t, err, store.ErrFenced)
}

// The reverse interleaving: the takeover's transaction is in flight when the
// old holder starts a fenced write. The write waits, then finds the fence
// moved and is refused.
func TestFencedWaitsForClaimInFlight(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := plannedRun(t, s, "acme", []string{"C1"}, 2)
	a := mustClaim(t, s, id, "A", store.StageExtract)

	// Stand in for a Claim that has locked the lane and bumped the fence
	// but not committed yet.
	tx, err := s.Pool().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err := tx.Exec(ctx, `UPDATE lanes SET fence = fence + 1, holder = 'B', lease_expires = now() + interval '1 minute' WHERE run_id = $1 AND id = $2`, id, a.LaneID); err != nil {
		t.Fatal(err)
	}

	var called atomic.Bool
	done := make(chan error, 1)
	go func() {
		done <- s.Fenced(ctx, a, func(tx store.Tx) error {
			called.Store(true)
			return tx.CompleteBatch(ctx, 0, "from-A", 1, nil)
		})
	}()
	select {
	case err := <-done:
		t.Fatalf("Fenced returned (%v) while the takeover was uncommitted", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wantErr(t, <-done, store.ErrFenced)
	if called.Load() {
		t.Fatal("fn ran although the takeover committed first")
	}
	if b, _ := s.GetBatch(ctx, id, store.BatchRef{LaneID: a.LaneID, Seq: 0}); b.Status != store.BatchPending {
		t.Fatalf("batch written under a superseded fence: %+v", b)
	}
}

func TestFencedRollbackAndMisuse(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := plannedRun(t, s, "acme", []string{"C1"}, 2)
	l := mustClaim(t, s, id, "A", store.StageExtract)

	// An error from fn rolls everything back and is returned as is.
	boom := errors.New("boom")
	var leaked store.Tx
	err := s.Fenced(ctx, l, func(tx store.Tx) error {
		leaked = tx
		if err := tx.CompleteBatch(ctx, 0, "x", 1, nil); err != nil {
			return err
		}
		if err := tx.SetCursor(ctx, []byte(`{"n":1}`)); err != nil {
			return err
		}
		if err := tx.Log(ctx, store.LogEntry{Op: "test"}); err != nil {
			return err
		}
		return boom
	})
	if err != boom {
		t.Fatalf("Fenced = %v, want fn's error", err)
	}
	if b, _ := s.GetBatch(ctx, id, store.BatchRef{LaneID: l.LaneID, Seq: 0}); b.Status != store.BatchPending {
		t.Fatalf("batch after rollback: %+v", b)
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM audit_log`); n != 0 {
		t.Fatalf("%d log entries after rollback", n)
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM lanes WHERE cursor IS NOT NULL`); n != 0 {
		t.Fatal("cursor survived rollback")
	}
	// A Tx kept past its Fenced call is dead.
	if err := leaked.CompleteBatch(ctx, 0, "x", 1, nil); err == nil {
		t.Fatal("a Tx worked after Fenced returned")
	}
	if err := leaked.Log(ctx, store.LogEntry{Op: "late"}); err == nil {
		t.Fatal("a Tx logged after Fenced returned")
	}

	// A panic in fn rolls back too and leaves the pool usable.
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic was swallowed")
			}
		}()
		_ = s.Fenced(ctx, l, func(tx store.Tx) error {
			_ = tx.CompleteBatch(ctx, 0, "x", 1, nil)
			panic("fn panicked")
		})
	}()
	if b, _ := s.GetBatch(ctx, id, store.BatchRef{LaneID: l.LaneID, Seq: 0}); b.Status != store.BatchPending {
		t.Fatalf("batch after panic: %+v", b)
	}
	complete(t, s, l, 0)

	// Unknown lease.
	err = s.Fenced(ctx, &store.Lease{RunID: id, LaneID: "nope", Holder: "A", Fence: 1}, func(store.Tx) error { return nil })
	wantErr(t, err, store.ErrFenced)
	wantErr(t, s.Renew(ctx, &store.Lease{RunID: "nope", LaneID: "nope", Holder: "A", Fence: 1}, ttl), store.ErrFenced)
}

// Sixteen workers hammer the ledger: 25 extract and 25 transform lanes of
// 20 batches each, every transform batch depending on its extract batch,
// with random clean and failed early releases.
func TestConcurrentWorkers(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	const (
		channels = 25
		per      = 20
		workers  = 16
	)
	id := newRun(t, s, "hammer")
	p := store.Plan{Teams: []store.TeamMapping{{Index: 0, Name: "T"}}}
	for c := range channels {
		ch := fmt.Sprintf("C%02d", c)
		p.Lanes = append(p.Lanes,
			store.Lane{ID: "extract:" + ch, Stage: store.StageExtract, Key: ch},
			store.Lane{ID: "transform:" + ch, Stage: store.StageTransform, Key: ch})
		for i := range per {
			p.Batches = append(p.Batches,
				store.Batch{LaneID: "extract:" + ch, Seq: i},
				store.Batch{LaneID: "transform:" + ch, Seq: i, DependsOn: []store.BatchRef{{LaneID: "extract:" + ch, Seq: i}}})
		}
	}
	applyPlan(t, s, id, p)

	var (
		mu        sync.Mutex
		heldBy    = map[string]string{} // lane -> holder, while leased
		lastFence = map[string]int64{}
		nextSeq   = map[string]int{} // lane -> next seq expected to complete
		completed = map[store.BatchRef]int{}
		claims    int
		failures  []string
	)
	fail := func(format string, args ...any) {
		failures = append(failures, fmt.Sprintf(format, args...))
	}
	total := 2 * channels * per
	var doneCount atomic.Int64
	deadline := time.Now().Add(60 * time.Second)

	var wg sync.WaitGroup
	for w := range workers {
		wg.Go(func() {
			holder := fmt.Sprintf("worker-%02d", w)
			rng := rand.New(rand.NewPCG(uint64(w), 42))
			for int(doneCount.Load()) < total {
				if time.Now().After(deadline) {
					mu.Lock()
					fail("%s: timed out with %d/%d batches done", holder, doneCount.Load(), total)
					mu.Unlock()
					return
				}
				l, err := s.Claim(ctx, store.ClaimFilter{Stages: []string{store.StageExtract, store.StageTransform}, RunID: id}, holder, ttl)
				if err != nil {
					mu.Lock()
					fail("%s: Claim: %v", holder, err)
					mu.Unlock()
					return
				}
				if l == nil {
					time.Sleep(time.Duration(rng.IntN(3)+1) * time.Millisecond)
					continue
				}
				mu.Lock()
				claims++
				if other, held := heldBy[l.LaneID]; held {
					fail("lane %s claimed by %s while held by %s", l.LaneID, holder, other)
				}
				if l.Fence <= lastFence[l.LaneID] {
					fail("lane %s: fence %d after %d", l.LaneID, l.Fence, lastFence[l.LaneID])
				}
				heldBy[l.LaneID] = holder
				lastFence[l.LaneID] = l.Fence
				mu.Unlock()

				for {
					if rng.IntN(5) == 0 {
						break // hand the lane back early
					}
					b, err := s.NextBatch(ctx, l)
					if err != nil {
						mu.Lock()
						fail("%s: NextBatch(%s): %v", holder, l.LaneID, err)
						mu.Unlock()
						break
					}
					if b == nil {
						break
					}
					err = s.Fenced(ctx, l, func(tx store.Tx) error {
						return tx.CompleteBatch(ctx, b.Seq, holder, 1, nil)
					})
					mu.Lock()
					if err != nil {
						fail("%s: CompleteBatch(%s/%d): %v", holder, l.LaneID, b.Seq, err)
						mu.Unlock()
						break
					}
					ref := store.BatchRef{LaneID: l.LaneID, Seq: b.Seq}
					completed[ref]++
					if completed[ref] != 1 {
						fail("batch %s/%d completed %d times", ref.LaneID, ref.Seq, completed[ref])
					}
					if b.Seq != nextSeq[l.LaneID] {
						fail("lane %s: completed seq %d, expected %d", l.LaneID, b.Seq, nextSeq[l.LaneID])
					}
					nextSeq[l.LaneID] = b.Seq + 1
					for _, dep := range b.DependsOn {
						if completed[dep] != 1 {
							fail("batch %s/%d ran before its dependency %s/%d", ref.LaneID, ref.Seq, dep.LaneID, dep.Seq)
						}
					}
					mu.Unlock()
					doneCount.Add(1)
				}

				// Give the lane up in memory first: once Release
				// returns somebody else may hold it.
				mu.Lock()
				delete(heldBy, l.LaneID)
				mu.Unlock()
				var retry time.Duration
				if rng.IntN(4) == 0 {
					retry = time.Duration(rng.IntN(3)+1) * time.Millisecond
				}
				if err := s.Release(ctx, l, retry, "simulated"); err != nil {
					mu.Lock()
					fail("%s: Release: %v", holder, err)
					mu.Unlock()
					return
				}
			}
		})
	}
	wg.Wait()

	for i, f := range failures {
		if i == 20 {
			t.Errorf("... and %d more", len(failures)-20)
			break
		}
		t.Error(f)
	}
	if len(completed) != total {
		t.Fatalf("%d distinct batches completed, want %d", len(completed), total)
	}
	t.Logf("%d claims for %d batches across %d lanes", claims, total, 2*channels)

	if n := scalar[int](t, s, `SELECT count(*) FROM lanes WHERE run_id = $1 AND status <> 'done'`, id); n != 0 {
		t.Errorf("%d lanes are not done", n)
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM batches WHERE run_id = $1 AND status <> 'done'`, id); n != 0 {
		t.Errorf("%d batches are not done", n)
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM lanes WHERE run_id = $1 AND holder IS NOT NULL`, id); n != 0 {
		t.Errorf("%d lanes still held", n)
	}
	// The database agrees with the in-memory fences.
	rows, err := s.Pool().Query(ctx, `SELECT id, fence FROM lanes WHERE run_id = $1 AND id <> 'plan'`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var lane string
		var fence int64
		if err := rows.Scan(&lane, &fence); err != nil {
			t.Fatal(err)
		}
		if fence != lastFence[lane] {
			t.Errorf("lane %s: fence %d in the database, last seen %d", lane, fence, lastFence[lane])
		}
	}
	noClaim(t, s, id)
	changed, err := s.Reconcile(ctx)
	if err != nil || len(changed) != 1 || changed[0].ID != id || changed[0].Status != store.RunCompleted {
		t.Fatalf("Reconcile = %+v, %v; want the run completed", changed, err)
	}
}

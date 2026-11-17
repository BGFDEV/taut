package store_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/store"
)

func TestProgress(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	_, err := s.Progress(ctx, "missing")
	wantErr(t, err, store.ErrNotFound)

	id := plannedRun(t, s, "acme", []string{"C1", "C2", "C3"}, 2)
	// Give the channels names whose order differs from their IDs.
	exec(t, s, `UPDATE map_channels SET slack_name = CASE slack_id WHEN 'C1' THEN 'zebra' WHEN 'C2' THEN 'alpha' ELSE 'mango' END`)

	// Extract: everything done.
	if n := drain(t, s, id, store.StageExtract); n != 6 {
		t.Fatalf("extract: %d batches", n)
	}

	// Transform C1: 5 posts, done. Transform C2: 2 posts, done.
	t1 := claimLane(t, s, id, store.StageTransform, "transform:C1", "x1")
	if err := s.Fenced(ctx, t1, func(tx store.Tx) error {
		return tx.PutMessages(ctx, []store.Message{
			msg("C1", "1.1", 0, 1001, 0), msg("C1", "1.2", 0, 1002, 0), msg("C1", "1.3", 0, 1003, 0),
			msg("C1", "1.4", 0, 1004, 1), msg("C1", "1.5", 0, 1005, 1),
		})
	}); err != nil {
		t.Fatal(err)
	}
	finishLane(t, s, t1)
	release(t, s, t1)
	t2 := claimLane(t, s, id, store.StageTransform, "transform:C2", "x2")
	if err := s.Fenced(ctx, t2, func(tx store.Tx) error {
		return tx.PutMessages(ctx, []store.Message{msg("C2", "2.1", 0, 2001, 0), msg("C2", "2.2", 0, 2002, 0)})
	}); err != nil {
		t.Fatal(err)
	}
	finishLane(t, s, t2)
	release(t, s, t2)
	// Transform C3: a failed attempt, now backing off.
	t3 := claimLane(t, s, id, store.StageTransform, "transform:C3", "x3")
	if err := s.Release(ctx, t3, time.Hour, "flaky disk"); err != nil {
		t.Fatal(err)
	}

	// Team done.
	if n := drain(t, s, id, store.StageTeam); n != 1 {
		t.Fatalf("team: %d batches", n)
	}

	// Load C2: fails permanently on its first batch.
	l2 := claimLane(t, s, id, store.StageLoad, "load:C2", "loader-2")
	if err := s.Fenced(ctx, l2, func(tx store.Tx) error { return tx.FailBatch(ctx, 0, "channel name taken") }); err != nil {
		t.Fatal(err)
	}
	release(t, s, l2)

	// Load C1: opened, mid-import, still held.
	l1 := claimLane(t, s, id, store.StageLoad, "load:C1", "loader-1")
	key := func(ts string) store.MessageKey { return store.MessageKey{ChannelID: "C1", SlackTS: ts} }
	if err := s.Fenced(ctx, l1, func(tx store.Tx) error {
		if err := tx.SetChannel(ctx, "C1", "19:c1", store.ChannelMigrating, nil); err != nil {
			return err
		}
		if err := tx.MarkPosted(ctx, key("1.1"), "1001", false); err != nil {
			return err
		}
		if err := tx.MarkPosted(ctx, key("1.2"), "1002", true); err != nil {
			return err
		}
		if err := tx.MarkFailed(ctx, key("1.3"), store.MessageFailed, "BadRequest", "no"); err != nil {
			return err
		}
		if err := tx.MarkFailed(ctx, key("1.4"), store.MessageHeld, "anchor_failed", "parent failed"); err != nil {
			return err
		}
		return tx.CompleteBatch(ctx, 0, "", 0, nil)
	}); err != nil {
		t.Fatal(err)
	}

	p, err := s.Progress(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if p.Run.ID != id || p.Run.Status != store.RunRunning || string(p.Totals) != `{"channels": 3}` {
		t.Fatalf("run/totals: %+v %s", p.Run, p.Totals)
	}
	wantStages := []store.StageProgress{
		{Stage: "plan", Lanes: 1, Done: 1, Batches: 1, BatchesDone: 1},
		{Stage: "extract", Lanes: 3, Done: 3, Batches: 6, BatchesDone: 6},
		{Stage: "transform", Lanes: 3, Done: 2, Batches: 6, BatchesDone: 4},
		{Stage: "team", Lanes: 1, Done: 1, Batches: 1, BatchesDone: 1},
		{Stage: "load", Lanes: 3, Done: 0, Failed: 1, Leased: 1, Batches: 9, BatchesDone: 1},
	}
	if !reflect.DeepEqual(p.Stages, wantStages) {
		t.Errorf("Stages\n got %+v\nwant %+v", p.Stages, wantStages)
	}
	wantChannels := []store.ChannelProgress{
		{SlackID: "C2", SlackName: "alpha", TargetName: "Chan C2", Kind: "public", Team: "Engineering", State: store.ChannelPlanned,
			Posts: 2, LaneFailed: true, LastError: "channel name taken"},
		{SlackID: "C3", SlackName: "mango", TargetName: "Chan C3", Kind: "public", Team: "Engineering", State: store.ChannelPlanned},
		{SlackID: "C1", SlackName: "zebra", TargetName: "Chan C1", Kind: "public", Team: "Engineering", State: store.ChannelMigrating,
			Posts: 5, Posted: 2, Failed: 1, Held: 1, Active: true},
	}
	if !reflect.DeepEqual(p.Channels, wantChannels) {
		t.Errorf("Channels\n got %+v\nwant %+v", p.Channels, wantChannels)
	}
	if p.Posts != 7 || p.Posted != 2 || p.Failed != 1 || p.Held != 1 || p.Adopted != 1 {
		t.Errorf("totals: posts=%d posted=%d failed=%d held=%d adopted=%d", p.Posts, p.Posted, p.Failed, p.Held, p.Adopted)
	}
	if len(p.Problems) != 2 {
		t.Fatalf("Problems = %+v", p.Problems)
	}
	pr := p.Problems[0]
	if pr.LaneID != "transform:C3" || pr.Stage != "transform" || pr.Status != "open" || pr.Attempts != 1 || pr.Error != "flaky disk" ||
		pr.NotBefore.Location() != time.UTC || time.Until(pr.NotBefore) < 59*time.Minute {
		t.Errorf("problem 0 = %+v", pr)
	}
	pr = p.Problems[1]
	if pr.LaneID != "load:C2" || pr.Stage != "load" || pr.Status != "failed" || pr.Error != "channel name taken" {
		t.Errorf("problem 1 = %+v", pr)
	}
	if _, err := json.Marshal(p); err != nil {
		t.Errorf("Progress does not marshal: %v", err)
	}

	// An expired lease no longer counts as active.
	exec(t, s, `UPDATE lanes SET lease_expires = now() - interval '1 second' WHERE run_id = $1 AND id = 'load:C1'`, id)
	p, err = s.Progress(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if p.Channels[2].Active || p.Stages[4].Leased != 0 {
		t.Errorf("expired lease still shown as active: %+v", p.Channels[2])
	}
}

// storetest.Open hands other packages a ready store behind the interface.
func TestStoretestOpen(t *testing.T) {
	t.Parallel()
	var s store.Store = storetestOpen(t)
	id := store.NewRunID()
	if err := s.CreateRun(t.Context(), store.Run{ID: id, Name: "iso", ConfigHash: "h"}); err != nil {
		t.Fatal(err)
	}
	// A second store is a separate schema: it does not see the run.
	other := storetestOpen(t)
	if _, err := other.GetRun(t.Context(), id); err != store.ErrNotFound {
		t.Fatalf("second store sees the first store's run: %v", err)
	}
	if err := other.CreateRun(t.Context(), store.Run{ID: store.NewRunID(), Name: "iso", ConfigHash: "h"}); err != nil {
		t.Fatalf("same name in another store: %v", err)
	}
}

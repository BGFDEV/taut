package store_test

import (
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/store"
)

func msg(ch, ts string, part int, ms int64, seq int) store.Message {
	return store.Message{MessageKey: store.MessageKey{ChannelID: ch, SlackTS: ts, Part: part}, TargetMS: ms, Hash: "h-" + ts, BatchSeq: seq}
}

func TestPutMessages(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := plannedRun(t, s, "acme", []string{"C1", "C2"}, 2)
	l := mustClaim(t, s, id, "x", store.StageExtract)
	put := func(msgs ...store.Message) error {
		return s.Fenced(ctx, l, func(tx store.Tx) error { return tx.PutMessages(ctx, msgs) })
	}

	// Slack ts values that a float would mangle or merge.
	first := []store.Message{
		msg("C1", "1700000000.000300", 0, 1700000000003, 0),
		msg("C1", "1700000000.000100", 0, 1700000000000, 0),
		msg("C1", "1700000000.000100", 1, 1700000000001, 0), // split message, part 1
		msg("C1", "1700000000.000200", 0, 1700000000002, 0),
		msg("C1", "1700000001.0001000", 0, 1700000001000, 1), // trailing zero kept
		msg("C2", "1700000000.000100", 0, 1700000000000, 0),  // same ts and ms, other channel
	}
	first[3].AnchorTS = "1700000000.000100"
	// State and Teams fields in the input are ignored: rows start pending.
	first[0].State, first[0].TeamsID, first[0].Adopted = store.MessagePosted, "bogus", true
	if err := put(first...); err != nil {
		t.Fatalf("PutMessages: %v", err)
	}
	if err := put(); err != nil {
		t.Fatalf("PutMessages(nothing): %v", err)
	}

	// MessagesInBatch: one channel's batch, ordered by TargetMS.
	got, err := s.MessagesInBatch(ctx, id, "C1", 0)
	if err != nil {
		t.Fatal(err)
	}
	wantTS := []string{"1700000000.000100/0", "1700000000.000100/1", "1700000000.000200/0", "1700000000.000300/0"}
	if len(got) != len(wantTS) {
		t.Fatalf("MessagesInBatch returned %d rows, want %d", len(got), len(wantTS))
	}
	for i, m := range got {
		if key := m.SlackTS + "/" + string(rune('0'+m.Part)); key != wantTS[i] {
			t.Fatalf("row %d = %s, want %s", i, key, wantTS[i])
		}
		if m.State != store.MessagePending || m.TeamsID != "" || m.Adopted || m.BatchSeq != 0 || m.ChannelID != "C1" {
			t.Fatalf("row %d = %+v", i, m)
		}
		if i > 0 && got[i-1].TargetMS >= m.TargetMS {
			t.Fatalf("rows not ordered by TargetMS")
		}
	}
	if got[2].AnchorTS != "1700000000.000100" || got[2].Hash != "h-1700000000.000200" {
		t.Fatalf("row = %+v", got[2])
	}
	if got, _ := s.MessagesInBatch(ctx, id, "C1", 1); len(got) != 1 || got[0].SlackTS != "1700000001.0001000" {
		t.Fatalf("batch 1 = %+v", got)
	}
	if got, _ := s.MessagesInBatch(ctx, id, "C1", 7); len(got) != 0 {
		t.Fatalf("empty batch = %+v", got)
	}

	// Re-inserting the same keys, even with different allocations and
	// hashes, leaves the rows completely untouched.
	before := scalar[string](t, s, `SELECT md5(string_agg(m::text, '|' ORDER BY channel_id, slack_ts, part)) FROM map_messages m`)
	again := []store.Message{
		msg("C1", "1700000000.000100", 0, 1999999999999, 5),
		msg("C1", "1700000000.000200", 0, 1700000000003, 5), // would collide with .000300's ms if inserted
		msg("C1", "1700000000.000300", 0, 1700000000003, 0),
	}
	again[0].Hash, again[0].AnchorTS = "different", "different"
	if err := put(again...); err != nil {
		t.Fatalf("re-insert: %v", err)
	}
	// A re-run that also brings new rows: old untouched, new inserted.
	if err := put(first[0], msg("C1", "1700000002.000100", 0, 1700000002000, 1)); err != nil {
		t.Fatalf("mixed re-insert: %v", err)
	}
	after := scalar[string](t, s, `SELECT md5(string_agg(m::text, '|' ORDER BY channel_id, slack_ts, part)) FROM map_messages m WHERE slack_ts <> '1700000002.000100'`)
	if before != after {
		t.Fatal("existing rows changed on re-insert")
	}
	m, err := s.GetMessage(ctx, id, store.MessageKey{ChannelID: "C1", SlackTS: "1700000000.000100", Part: 0})
	if err != nil || m.TargetMS != 1700000000000 || m.Hash != "h-1700000000.000100" || m.BatchSeq != 0 || m.AnchorTS != "" {
		t.Fatalf("GetMessage = %+v, %v", m, err)
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM map_messages`); n != 7 {
		t.Fatalf("%d rows, want 7", n)
	}

	// A different key with a taken timestamp fails the whole transaction.
	err = s.Fenced(ctx, l, func(tx store.Tx) error {
		if err := tx.SetCursor(ctx, []byte(`{"written":"before the collision"}`)); err != nil {
			return err
		}
		return tx.PutMessages(ctx, []store.Message{
			msg("C1", "1700000009.000100", 0, 1700000009000, 1),
			msg("C1", "1700000009.000200", 0, 1700000000002, 1), // taken by .000200
		})
	})
	wantErr(t, err, store.ErrTimestampTaken)
	if n := scalar[int](t, s, `SELECT count(*) FROM map_messages`); n != 7 {
		t.Fatalf("%d rows after a failed insert, want 7", n)
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM lanes WHERE cursor IS NOT NULL`); n != 0 {
		t.Fatal("the rest of the transaction survived ErrTimestampTaken")
	}
	// Even if fn swallows the error, nothing commits.
	err = s.Fenced(ctx, l, func(tx store.Tx) error {
		_ = tx.PutMessages(ctx, []store.Message{msg("C1", "1700000009.000200", 0, 1700000000002, 1)})
		return nil
	})
	if err == nil {
		t.Fatal("Fenced committed after a swallowed ErrTimestampTaken")
	}
	// Two new rows colliding with each other.
	err = put(msg("C1", "1700000010.000100", 0, 1700000010000, 1), msg("C1", "1700000010.000200", 0, 1700000010000, 1))
	wantErr(t, err, store.ErrTimestampTaken)
	// The same millisecond in another channel is fine.
	if err := put(msg("C2", "1700000009.000200", 0, 1700000000002, 1)); err != nil {
		t.Fatalf("same ms in another channel: %v", err)
	}
	// Unknown channel, missing key.
	if err := put(msg("NOPE", "1.1", 0, 1, 0)); err == nil {
		t.Fatal("PutMessages for an unplanned channel succeeded")
	}
	if err := put(msg("C1", "", 0, 1, 0)); err == nil {
		t.Fatal("PutMessages without a Slack ts succeeded")
	}
	_, err = s.GetMessage(ctx, id, store.MessageKey{ChannelID: "C1", SlackTS: "nope"})
	wantErr(t, err, store.ErrNotFound)
}

func TestMarkPostedAndFailed(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := plannedRun(t, s, "acme", []string{"C1"}, 1)
	l := mustClaim(t, s, id, "x", store.StageExtract)
	do := func(fn func(tx store.Tx) error) error { return s.Fenced(ctx, l, fn) }
	key := func(ts string) store.MessageKey { return store.MessageKey{ChannelID: "C1", SlackTS: ts} }
	if err := do(func(tx store.Tx) error {
		return tx.PutMessages(ctx, []store.Message{
			msg("C1", "1.1", 0, 1001, 0), msg("C1", "1.2", 0, 1002, 0), msg("C1", "1.3", 0, 1003, 0), msg("C1", "1.4", 0, 1004, 0),
		})
	}); err != nil {
		t.Fatal(err)
	}
	get := func(ts string) store.Message {
		m, err := s.GetMessage(ctx, id, key(ts))
		if err != nil {
			t.Fatalf("GetMessage(%s): %v", ts, err)
		}
		return m
	}

	// Missing rows.
	wantErr(t, do(func(tx store.Tx) error { return tx.MarkPosted(ctx, key("9.9"), "T", false) }), store.ErrNotFound)
	wantErr(t, do(func(tx store.Tx) error { return tx.MarkFailed(ctx, key("9.9"), store.MessageFailed, "c", "r") }), store.ErrNotFound)
	wantErr(t, do(func(tx store.Tx) error {
		return tx.MarkPosted(ctx, store.MessageKey{ChannelID: "C1", SlackTS: "1.1", Part: 1}, "T", false)
	}), store.ErrNotFound)

	// Posted.
	if err := do(func(tx store.Tx) error { return tx.MarkPosted(ctx, key("1.1"), "1001", false) }); err != nil {
		t.Fatal(err)
	}
	if m := get("1.1"); m.State != store.MessagePosted || m.TeamsID != "1001" || m.Adopted {
		t.Fatalf("posted row = %+v", m)
	}
	// Same ID again: a no-op, and the recorded Adopted flag stays.
	if err := do(func(tx store.Tx) error { return tx.MarkPosted(ctx, key("1.1"), "1001", true) }); err != nil {
		t.Fatalf("MarkPosted with the same ID: %v", err)
	}
	if m := get("1.1"); m.Adopted {
		t.Fatal("no-op MarkPosted changed Adopted")
	}
	// A different ID is refused and nothing changes.
	wantErr(t, do(func(tx store.Tx) error { return tx.MarkPosted(ctx, key("1.1"), "2002", false) }), store.ErrTargetConflict)
	if m := get("1.1"); m.TeamsID != "1001" {
		t.Fatalf("posted ID overwritten: %+v", m)
	}
	// A posted row cannot be failed or held.
	wantErr(t, do(func(tx store.Tx) error { return tx.MarkFailed(ctx, key("1.1"), store.MessageFailed, "c", "r") }), store.ErrTargetConflict)
	wantErr(t, do(func(tx store.Tx) error { return tx.MarkFailed(ctx, key("1.1"), store.MessageHeld, "c", "r") }), store.ErrTargetConflict)
	if m := get("1.1"); m.State != store.MessagePosted || m.ErrorCode != "" {
		t.Fatalf("posted row changed: %+v", m)
	}
	if err := do(func(tx store.Tx) error { return tx.MarkPosted(ctx, key("1.2"), "", false) }); err == nil {
		t.Fatal("MarkPosted without a Teams ID succeeded")
	}

	// Failed and held.
	if err := do(func(tx store.Tx) error {
		return tx.MarkFailed(ctx, key("1.2"), store.MessageFailed, "BadRequest", "body too large")
	}); err != nil {
		t.Fatal(err)
	}
	if m := get("1.2"); m.State != store.MessageFailed || m.ErrorCode != "BadRequest" || m.Error != "body too large" || m.TeamsID != "" {
		t.Fatalf("failed row = %+v", m)
	}
	if err := do(func(tx store.Tx) error {
		return tx.MarkFailed(ctx, key("1.3"), store.MessageHeld, "anchor_failed", "parent 1.2 failed")
	}); err != nil {
		t.Fatal(err)
	}
	if m := get("1.3"); m.State != store.MessageHeld || m.ErrorCode != "anchor_failed" {
		t.Fatalf("held row = %+v", m)
	}
	for _, bad := range []store.MessageState{store.MessagePosted, store.MessagePending, "weird"} {
		if err := do(func(tx store.Tx) error { return tx.MarkFailed(ctx, key("1.4"), bad, "c", "r") }); err == nil {
			t.Fatalf("MarkFailed(%q) succeeded", bad)
		}
	}
	// A failed row can still become posted (an operator retry), adopted.
	if err := do(func(tx store.Tx) error { return tx.MarkPosted(ctx, key("1.2"), "1002", true) }); err != nil {
		t.Fatal(err)
	}
	if m := get("1.2"); m.State != store.MessagePosted || m.TeamsID != "1002" || !m.Adopted || m.ErrorCode != "" || m.Error != "" {
		t.Fatalf("retried row = %+v", m)
	}
}

func TestAnchorsTeamsChannels(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := plannedRun(t, s, "acme", []string{"C1", "C2"}, 1)
	l := mustClaim(t, s, id, "x", store.StageExtract)
	do := func(fn func(tx store.Tx) error) error { return s.Fenced(ctx, l, fn) }

	// Anchors: first writer wins.
	if _, ok, err := s.Anchor(ctx, id, "C1", "10.1"); err != nil || ok {
		t.Fatalf("Anchor before SetAnchor = %v, %v", ok, err)
	}
	if err := do(func(tx store.Tx) error { return tx.SetAnchor(ctx, "C1", "10.1", "10.1") }); err != nil {
		t.Fatal(err)
	}
	if err := do(func(tx store.Tx) error { return tx.SetAnchor(ctx, "C1", "10.1", "10.5") }); err != nil {
		t.Fatalf("second SetAnchor: %v", err)
	}
	if a, ok, err := s.Anchor(ctx, id, "C1", "10.1"); err != nil || !ok || a != "10.1" {
		t.Fatalf("Anchor = %q, %v, %v; want the first writer's", a, ok, err)
	}
	if _, ok, _ := s.Anchor(ctx, id, "C2", "10.1"); ok {
		t.Fatal("anchor leaked across channels")
	}
	if err := do(func(tx store.Tx) error { return tx.SetAnchor(ctx, "NOPE", "1.1", "1.1") }); err == nil {
		t.Fatal("SetAnchor for an unplanned channel succeeded")
	}

	// Teams.
	wantErr(t, do(func(tx store.Tx) error { return tx.SetTeam(ctx, 9, "T", false) }), store.ErrNotFound)
	if err := do(func(tx store.Tx) error { return tx.SetTeam(ctx, 0, "team-guid", true) }); err != nil {
		t.Fatal(err)
	}
	if err := do(func(tx store.Tx) error { return tx.SetTeam(ctx, 0, "team-guid", false) }); err != nil {
		t.Fatalf("SetTeam with the same ID: %v", err)
	}
	wantErr(t, do(func(tx store.Tx) error { return tx.SetTeam(ctx, 0, "other-guid", false) }), store.ErrTargetConflict)
	teams, _ := s.Teams(ctx, id)
	if len(teams) != 1 || teams[0].TeamsID != "team-guid" || !teams[0].Adopted {
		t.Fatalf("Teams = %+v", teams)
	}

	// Channels.
	wantErr(t, do(func(tx store.Tx) error { return tx.SetChannel(ctx, "NOPE", "x", store.ChannelCreated, nil) }), store.ErrNotFound)
	if err := do(func(tx store.Tx) error { return tx.SetChannel(ctx, "C1", "19:abc@thread", store.ChannelCreated, nil) }); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2018, 3, 4, 5, 6, 7, 891000000, time.FixedZone("x", -7200))
	if err := do(func(tx store.Tx) error {
		return tx.SetChannel(ctx, "C1", "19:abc@thread", store.ChannelMigrating, &start)
	}); err != nil {
		t.Fatal(err)
	}
	// A state-only update keeps the ID and the migration start.
	if err := do(func(tx store.Tx) error { return tx.SetChannel(ctx, "C1", "", store.ChannelCompleted, nil) }); err != nil {
		t.Fatal(err)
	}
	c, err := s.Channel(ctx, id, "C1")
	if err != nil || c.TeamsID != "19:abc@thread" || c.State != store.ChannelCompleted ||
		c.MigrationStart == nil || !c.MigrationStart.Equal(start) || c.MigrationStart.Location() != time.UTC {
		t.Fatalf("Channel = %+v, %v", c, err)
	}
	wantErr(t, do(func(tx store.Tx) error { return tx.SetChannel(ctx, "C1", "19:other@thread", store.ChannelCreated, nil) }), store.ErrTargetConflict)
	if err := do(func(tx store.Tx) error { return tx.SetChannel(ctx, "C1", "", "bogus", nil) }); err == nil {
		t.Fatal("SetChannel accepted an unknown state")
	}
	if c2, _ := s.Channel(ctx, id, "C2"); c2.State != store.ChannelPlanned || c2.TeamsID != "" {
		t.Fatalf("C2 = %+v", c2)
	}
}

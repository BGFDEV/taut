package store_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/store"
)

// The canonical encoding is part of the audit contract: pin it byte for
// byte with an independent computation.
func TestCanonicalLogEntry(t *testing.T) {
	t.Parallel()
	e := store.LogEntry{
		RunID: "run-1", Stream: "load:C1", Seq: 12,
		At:    time.Date(2026, 11, 13, 22, 1, 2, 3456789, time.FixedZone("x", 3600)),
		Actor: "worker-7", Op: "message.import", SourceID: "C1/1700000000.000100", TargetID: "1700000000000",
		ContentHash: "abc", Outcome: "ok", Detail: "",
		PrevHash: "ff00", EntryHash: "ignored",
	}
	want := "4:ff00" + "13:taut-audit-v1" + "5:run-1" + "7:load:C1" + "2:12" +
		"27:2026-11-13T21:01:02.003456Z" + "8:worker-7" + "14:message.import" +
		"20:C1/1700000000.000100" + "13:1700000000000" + "3:abc" + "2:ok" + "0:"
	if got := string(store.CanonicalLogEntry(e)); got != want {
		t.Fatalf("canonical encoding\n got %s\nwant %s", got, want)
	}
	sum := sha256.Sum256([]byte(want))
	if got := store.LogEntryHash(e); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("LogEntryHash = %s", got)
	}
	// Moving a byte between adjacent fields changes the encoding.
	a, b := e, e
	a.Actor, a.Op = "ab", "c"
	b.Actor, b.Op = "a", "bc"
	if store.LogEntryHash(a) == store.LogEntryHash(b) {
		t.Fatal("encoding is ambiguous across field boundaries")
	}
	// First entry: empty PrevHash.
	e.PrevHash = ""
	if !strings.HasPrefix(string(store.CanonicalLogEntry(e)), "0:13:taut-audit-v1") {
		t.Fatalf("first-entry encoding = %s", store.CanonicalLogEntry(e))
	}
}

// logN appends n entries to the lease's stream, one transaction each.
func logN(t *testing.T, s *store.PG, l *store.Lease, n int) {
	t.Helper()
	for i := range n {
		err := s.Fenced(t.Context(), l, func(tx store.Tx) error {
			return tx.Log(t.Context(), store.LogEntry{
				Op: "message.import", SourceID: fmt.Sprintf("%s/%d", l.Key, i), TargetID: fmt.Sprint(1000 + i),
				ContentHash: "c", Outcome: "ok", Detail: fmt.Sprintf("entry %d", i),
			})
		})
		if err != nil {
			t.Fatalf("Log: %v", err)
		}
	}
}

func TestAuditLogChain(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := plannedRun(t, s, "acme", []string{"C1", "C2"}, 1)
	if err := s.VerifyLog(ctx, id); err != nil {
		t.Fatalf("VerifyLog on an empty log: %v", err)
	}
	l1 := claimLane(t, s, id, store.StageExtract, "extract:C1", "worker-1")
	l2 := claimLane(t, s, id, store.StageExtract, "extract:C2", "worker-2")
	logN(t, s, l1, 5)
	logN(t, s, l2, 3)
	// Several entries in one transaction, with an explicit time.
	at := time.Date(2026, 1, 2, 3, 4, 5, 678901234, time.FixedZone("x", 7200))
	err := s.Fenced(ctx, l1, func(tx store.Tx) error {
		for i := range 3 {
			if err := tx.Log(ctx, store.LogEntry{RunID: id, Stream: "extract:C1", At: at, Actor: "someone-else", Op: "team.create", Outcome: fmt.Sprint(i)}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	all, err := s.LogEntries(ctx, id, "", 0, 0)
	if err != nil || len(all) != 11 {
		t.Fatalf("LogEntries = %d entries, %v; want 11", len(all), err)
	}
	// (stream, seq) order, gapless from 1, chained, hashes correct.
	prev := map[string]store.LogEntry{}
	for i, e := range all {
		if i > 0 && (all[i-1].Stream > e.Stream || (all[i-1].Stream == e.Stream && all[i-1].Seq >= e.Seq)) {
			t.Fatalf("entries not in (stream, seq) order at %d", i)
		}
		p := prev[e.Stream]
		if e.Seq != p.Seq+1 || e.PrevHash != p.EntryHash {
			t.Fatalf("%s/%d: seq or link wrong (prev seq %d)", e.Stream, e.Seq, p.Seq)
		}
		if e.EntryHash != store.LogEntryHash(e) || len(e.EntryHash) != 64 {
			t.Fatalf("%s/%d: EntryHash does not match", e.Stream, e.Seq)
		}
		if e.RunID != id || e.At.Location() != time.UTC {
			t.Fatalf("entry = %+v", e)
		}
		prev[e.Stream] = e
	}
	if prev["extract:C1"].Seq != 8 || prev["extract:C2"].Seq != 3 {
		t.Fatalf("stream lengths: %d and %d", prev["extract:C1"].Seq, prev["extract:C2"].Seq)
	}
	first := all[0]
	if first.PrevHash != "" || first.Actor != "worker-1" || first.Op != "message.import" || first.SourceID != "C1/0" ||
		first.TargetID != "1000" || first.ContentHash != "c" || first.Outcome != "ok" || first.Detail != "entry 0" {
		t.Fatalf("first entry = %+v", first)
	}
	if d := time.Since(first.At); d < -time.Minute || d > time.Minute {
		t.Fatalf("default At = %v", first.At)
	}
	last := prev["extract:C1"]
	if !last.At.Equal(at.Truncate(time.Microsecond)) || last.Actor != "someone-else" {
		t.Fatalf("explicit At/Actor not kept: %+v", last)
	}

	// Stream filter, afterSeq, limit.
	c1, _ := s.LogEntries(ctx, id, "extract:C1", 0, 0)
	if len(c1) != 8 {
		t.Fatalf("stream filter: %d entries", len(c1))
	}
	page, _ := s.LogEntries(ctx, id, "extract:C1", 2, 3)
	if len(page) != 3 || page[0].Seq != 3 || page[2].Seq != 5 {
		t.Fatalf("page = %+v", page)
	}
	if rest, _ := s.LogEntries(ctx, id, "extract:C1", 8, 10); len(rest) != 0 {
		t.Fatalf("after the end: %d entries", len(rest))
	}
	if none, _ := s.LogEntries(ctx, "other-run", "", 0, 0); len(none) != 0 {
		t.Fatal("entries leaked across runs")
	}

	// An entry addressed to another stream or run is refused.
	err = s.Fenced(ctx, l1, func(tx store.Tx) error { return tx.Log(ctx, store.LogEntry{Stream: "extract:C2", Op: "x"}) })
	if err == nil {
		t.Fatal("Log to another lane's stream succeeded")
	}
	err = s.Fenced(ctx, l1, func(tx store.Tx) error { return tx.Log(ctx, store.LogEntry{RunID: "other", Op: "x"}) })
	if err == nil {
		t.Fatal("Log to another run succeeded")
	}
	// A rolled-back append leaves no gap.
	_ = s.Fenced(ctx, l1, func(tx store.Tx) error {
		_ = tx.Log(ctx, store.LogEntry{Op: "rolled.back"})
		return errors.New("abort")
	})
	logN(t, s, l1, 1)
	if n := scalar[int64](t, s, `SELECT max(seq) FROM audit_log WHERE stream = 'extract:C1'`); n != 9 {
		t.Fatalf("seq after a rolled-back append = %d, want 9", n)
	}
	if err := s.VerifyLog(ctx, id); err != nil {
		t.Fatalf("VerifyLog: %v", err)
	}
}

func TestAuditLogAppendOnly(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := plannedRun(t, s, "acme", []string{"C1"}, 1)
	l := mustClaim(t, s, id, "d", store.StageExtract)
	logN(t, s, l, 3)

	for _, sql := range []string{
		`UPDATE audit_log SET detail = 'edited' WHERE seq = 2`,
		`UPDATE audit_log SET entry_hash = 'x'`,
		`DELETE FROM audit_log WHERE seq = 3`,
		`DELETE FROM audit_log`,
		`TRUNCATE audit_log`,
	} {
		_, err := s.Pool().Exec(ctx, sql)
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s: err = %v, want the append-only trigger to reject it", sql, err)
		}
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM audit_log WHERE detail LIKE 'entry %'`); n != 3 {
		t.Fatalf("%d intact entries, want 3", n)
	}
	// A duplicate seq cannot be inserted either.
	_, err := s.Pool().Exec(ctx, `INSERT INTO audit_log (run_id, stream, seq, at, prev_hash, entry_hash) VALUES ($1, 'extract:C1', 2, now(), '', '')`, id)
	if err == nil {
		t.Fatal("inserted a duplicate (stream, seq)")
	}
	if err := s.VerifyLog(ctx, id); err != nil {
		t.Fatalf("VerifyLog: %v", err)
	}
}

// Tampering needs the trigger out of the way, which only the table's owner
// can do; VerifyLog must then say where the chain breaks.
func TestVerifyLogDetectsTampering(t *testing.T) {
	cases := []struct {
		name   string
		tamper string
		stream string
		seq    int64
		kind   string
	}{
		{"edited content", `UPDATE audit_log SET detail = 'edited' WHERE stream = 'extract:C2' AND seq = 3`, "extract:C2", 3, "hash"},
		{"edited target", `UPDATE audit_log SET target_id = '9999' WHERE stream = 'extract:C1' AND seq = 1`, "extract:C1", 1, "hash"},
		{"edited time", `UPDATE audit_log SET at = at + interval '1 microsecond' WHERE stream = 'extract:C1' AND seq = 4`, "extract:C1", 4, "hash"},
		{"edited and rehashed", `UPDATE audit_log SET detail = 'edited', entry_hash = repeat('a', 64) WHERE stream = 'extract:C1' AND seq = 2`, "extract:C1", 2, "hash"},
		{"deleted from the middle", `DELETE FROM audit_log WHERE stream = 'extract:C2' AND seq = 2`, "extract:C2", 2, "gap"},
		{"deleted first", `DELETE FROM audit_log WHERE stream = 'extract:C1' AND seq = 1`, "extract:C1", 1, "gap"},
		{"deleted last", `DELETE FROM audit_log WHERE stream = 'extract:C2' AND seq = 5`, "extract:C2", 5, "head"},
		{"whole stream deleted", `DELETE FROM audit_log WHERE stream = 'extract:C2'`, "extract:C2", 1, "head"},
		{"relinked", `UPDATE audit_log SET prev_hash = repeat('b', 64) WHERE stream = 'extract:C1' AND seq = 3`, "extract:C1", 3, "link"},
		{"earliest problem wins", `UPDATE audit_log SET detail = 'edited' WHERE seq IN (2, 4)`, "extract:C1", 2, "hash"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := open(t)
			ctx := t.Context()
			id := plannedRun(t, s, "acme", []string{"C1", "C2"}, 1)
			logN(t, s, claimLane(t, s, id, store.StageExtract, "extract:C1", "d1"), 5)
			logN(t, s, claimLane(t, s, id, store.StageExtract, "extract:C2", "d2"), 5)
			if err := s.VerifyLog(ctx, id); err != nil {
				t.Fatalf("VerifyLog before tampering: %v", err)
			}
			exec(t, s, `ALTER TABLE audit_log DISABLE TRIGGER USER`)
			if n := exec(t, s, tc.tamper); n == 0 {
				t.Fatal("tampering touched no rows")
			}
			exec(t, s, `ALTER TABLE audit_log ENABLE TRIGGER USER`)

			err := s.VerifyLog(ctx, id)
			var ce *store.LogCorruptError
			if !errors.As(err, &ce) || !errors.Is(err, store.ErrLogCorrupt) || !store.IsLogCorrupt(err) {
				t.Fatalf("VerifyLog = %v, want a LogCorruptError", err)
			}
			if ce.Stream != tc.stream || ce.Seq != tc.seq || ce.Kind != tc.kind {
				t.Fatalf("VerifyLog reported %s/%d (%s), want %s/%d (%s): %v", ce.Stream, ce.Seq, ce.Kind, tc.stream, tc.seq, tc.kind, err)
			}
		})
	}
}

func TestAuditLogConcurrentAppends(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	const lanes, per = 8, 25
	var chans []string
	for i := range lanes {
		chans = append(chans, fmt.Sprintf("C%d", i))
	}
	id := plannedRun(t, s, "acme", chans, 1)
	var leases []*store.Lease
	for range lanes {
		leases = append(leases, mustClaim(t, s, id, "d", store.StageExtract))
	}

	// Different streams append in parallel; and each stream is also
	// appended to by two goroutines sharing its lease, which the lane lock
	// and the head lock must serialize.
	var wg sync.WaitGroup
	for _, l := range leases {
		for g := range 2 {
			wg.Go(func() {
				for i := range per {
					err := s.Fenced(ctx, l, func(tx store.Tx) error {
						return tx.Log(ctx, store.LogEntry{Op: "message.import", Detail: fmt.Sprintf("g%d-%d", g, i), Outcome: "ok"})
					})
					if err != nil {
						t.Errorf("Log(%s): %v", l.LaneID, err)
						return
					}
				}
			})
		}
	}
	wg.Wait()

	for _, l := range leases {
		entries, err := s.LogEntries(ctx, id, l.LaneID, 0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2*per {
			t.Fatalf("stream %s has %d entries, want %d", l.LaneID, len(entries), 2*per)
		}
		for i, e := range entries {
			if e.Seq != int64(i+1) {
				t.Fatalf("stream %s: entry %d has seq %d (gap or duplicate)", l.LaneID, i, e.Seq)
			}
		}
	}
	if err := s.VerifyLog(ctx, id); err != nil {
		t.Fatalf("VerifyLog: %v", err)
	}
}

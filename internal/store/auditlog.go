package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// logTimeFormat is how LogEntry.At enters the canonical encoding: UTC,
// always exactly six fractional digits (Postgres's resolution).
const logTimeFormat = "2006-01-02T15:04:05.000000Z"

// logEncodingVersion tags the canonical encoding, so a future change of
// format cannot be confused with this one.
const logEncodingVersion = "taut-audit-v1"

// CanonicalLogEntry returns the byte string whose SHA-256 is the entry's
// EntryHash.
//
// It is the concatenation of thirteen fields, in this fixed order, each
// encoded as its length in bytes in decimal ASCII, a colon, and then the
// bytes themselves (so "ok" is "2:ok" and the empty string is "0:"):
//
//  1. PrevHash      hex EntryHash of the previous entry in the stream,
//     or the empty string for the first entry (Seq 1)
//  2. the literal "taut-audit-v1"
//  3. RunID
//  4. Stream
//  5. Seq           decimal ASCII, no padding
//  6. At            UTC, truncated to microseconds, formatted
//     2006-01-02T15:04:05.000000Z (always six digits)
//  7. Actor
//  8. Op
//  9. SourceID
//  10. TargetID
//  11. ContentHash
//  12. Outcome
//  13. Detail
//
// Strings are taken as their raw bytes; nothing is escaped or normalized.
// Because every field carries its own length, no two different entries have
// the same encoding. EntryHash itself is not part of the encoding.
func CanonicalLogEntry(e LogEntry) []byte {
	fields := [...]string{
		e.PrevHash,
		logEncodingVersion,
		e.RunID,
		e.Stream,
		strconv.FormatInt(e.Seq, 10),
		e.At.UTC().Truncate(time.Microsecond).Format(logTimeFormat),
		e.Actor,
		e.Op,
		e.SourceID,
		e.TargetID,
		e.ContentHash,
		e.Outcome,
		e.Detail,
	}
	n := 0
	for _, f := range fields {
		n += len(f) + 8
	}
	out := make([]byte, 0, n)
	for _, f := range fields {
		out = strconv.AppendInt(out, int64(len(f)), 10)
		out = append(out, ':')
		out = append(out, f...)
	}
	return out
}

// LogEntryHash returns the lowercase hex SHA-256 of CanonicalLogEntry(e):
// the value EntryHash must have.
func LogEntryHash(e LogEntry) string {
	sum := sha256.Sum256(CanonicalLogEntry(e))
	return hex.EncodeToString(sum[:])
}

// Log appends an entry to the leased lane's stream.
//
// The store sets RunID (the lease's run), Stream (the lease's lane ID), Seq
// and both hashes; a RunID or Stream in e that names something else is an
// error rather than silently overridden. At is kept if set (truncated to
// microseconds) and otherwise becomes the database's current time. Actor
// defaults to the lease's holder.
//
// Appends to one stream are serialized twice over: the Fenced transaction
// holds the lane's row lock, and Log itself locks the stream's head row
// (audit_log_heads) before reading it, so even transactions that reached the
// same stream some other way could not both extend the same predecessor.
// The head is advanced in the same statement that inserts the entry.
func (t *fencedTx) Log(ctx context.Context, e LogEntry) error {
	if err := t.check(); err != nil {
		return err
	}
	if e.RunID != "" && e.RunID != t.lease.RunID {
		return fmt.Errorf("store: Log: entry is for run %s but the lease is for run %s", e.RunID, t.lease.RunID)
	}
	if e.Stream != "" && e.Stream != t.lease.LaneID {
		return fmt.Errorf("store: Log: entry is for stream %s but the lease is for lane %s", e.Stream, t.lease.LaneID)
	}
	e.RunID, e.Stream = t.lease.RunID, t.lease.LaneID
	if e.Actor == "" {
		e.Actor = t.lease.Holder
	}

	// Create the head if this is the stream's first entry, and lock it
	// either way (the no-op DO UPDATE takes the row lock).
	var (
		headSeq  int64
		headHash string
		dbNow    time.Time
	)
	err := t.tx.QueryRow(ctx, `
		INSERT INTO audit_log_heads (run_id, stream) VALUES ($1, $2)
		ON CONFLICT (run_id, stream) DO UPDATE SET seq = audit_log_heads.seq
		RETURNING seq, hash, clock_timestamp()`,
		e.RunID, e.Stream).Scan(&headSeq, &headHash, &dbNow)
	if err != nil {
		return err
	}
	if e.At.IsZero() {
		e.At = dbNow
	}
	e.At = e.At.UTC().Truncate(time.Microsecond)
	e.Seq = headSeq + 1
	e.PrevHash = headHash
	e.EntryHash = LogEntryHash(e)

	_, err = t.tx.Exec(ctx, `
		WITH ins AS (
			INSERT INTO audit_log (run_id, stream, seq, at, actor, op, source_id, target_id,
			                     content_hash, outcome, detail, prev_hash, entry_hash)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		)
		UPDATE audit_log_heads SET seq = $3, hash = $13 WHERE run_id = $1 AND stream = $2`,
		e.RunID, e.Stream, e.Seq, e.At, e.Actor, e.Op, e.SourceID, e.TargetID,
		e.ContentHash, e.Outcome, e.Detail, e.PrevHash, e.EntryHash)
	return err
}

const logCols = `run_id, stream, seq, at, actor, op, source_id, target_id, content_hash, outcome, detail, prev_hash, entry_hash`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanLogEntry(row rowScanner) (LogEntry, error) {
	var e LogEntry
	err := row.Scan(&e.RunID, &e.Stream, &e.Seq, &e.At, &e.Actor, &e.Op, &e.SourceID, &e.TargetID,
		&e.ContentHash, &e.Outcome, &e.Detail, &e.PrevHash, &e.EntryHash)
	e.At = utc(e.At)
	return e, err
}

// LogEntries returns entries of a run in (stream, seq) order. A non-empty
// stream restricts the result to that stream. Only entries with Seq greater
// than afterSeq are returned (in every stream, when no stream is given), and
// at most limit of them; limit <= 0 means no limit.
func (s *PG) LogEntries(ctx context.Context, runID, stream string, afterSeq int64, limit int) ([]LogEntry, error) {
	var lim any
	if limit > 0 {
		lim = int64(limit)
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+logCols+` FROM audit_log
		WHERE run_id = $1 AND ($2::text = '' OR stream = $2::text) AND seq > $3
		ORDER BY stream COLLATE "C", seq
		LIMIT $4`, runID, stream, afterSeq, lim)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogEntry
	for rows.Next() {
		e, err := scanLogEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LogCorruptError is what VerifyLog returns when a chain does not verify. It
// wraps ErrLogCorrupt.
type LogCorruptError struct {
	RunID  string
	Stream string
	// Seq is the first entry that does not verify. For a gap it is the
	// first missing seq.
	Seq int64
	// Kind is "gap" (an entry is missing), "link" (PrevHash is not the
	// previous entry's EntryHash), "hash" (EntryHash does not match the
	// entry's content), or "head" (the stream's entries stop before, or run
	// past, its recorded head: entries were removed from or added to the
	// end).
	Kind   string
	Detail string
}

func (e *LogCorruptError) Error() string {
	return fmt.Sprintf("store: audit log of run %s does not verify at stream %q seq %d (%s): %s",
		e.RunID, e.Stream, e.Seq, e.Kind, e.Detail)
}

func (e *LogCorruptError) Unwrap() error { return ErrLogCorrupt }

// VerifyLog recomputes every hash chain of the run. It returns nil if all
// streams verify, and otherwise a *LogCorruptError naming the first stream
// (in byte order of the stream name) and seq that does not: a missing seq, a PrevHash that
// does not match the previous entry, an EntryHash that does not match the
// entry's canonical encoding, or a chain whose end disagrees with the
// stream's recorded head.
//
// It reads the run's entries in one ordered pass inside a single snapshot,
// so appends that happen while it runs cannot produce a false alarm.
func (s *PG) VerifyLog(ctx context.Context, runID string) error {
	tx, err := s.pool.BeginTx(ctx, repeatableRead)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	type head struct {
		seq  int64
		hash string
	}
	heads := map[string]head{}
	var streams []string
	hrows, err := tx.Query(ctx, `SELECT stream, seq, hash FROM audit_log_heads WHERE run_id = $1 ORDER BY stream COLLATE "C"`, runID)
	if err != nil {
		return err
	}
	for hrows.Next() {
		var (
			stream string
			h      head
		)
		if err := hrows.Scan(&stream, &h.seq, &h.hash); err != nil {
			hrows.Close()
			return err
		}
		heads[stream] = h
		streams = append(streams, stream)
	}
	if err := hrows.Err(); err != nil {
		return err
	}

	corrupt := func(stream string, seq int64, kind, format string, args ...any) error {
		return &LogCorruptError{RunID: runID, Stream: stream, Seq: seq, Kind: kind, Detail: fmt.Sprintf(format, args...)}
	}
	// tails records where each stream's chain ended.
	tails := map[string]head{}
	// endStream checks a finished stream against its head.
	endStream := func(stream string) error {
		t := tails[stream]
		h, ok := heads[stream]
		switch {
		case !ok:
			return corrupt(stream, 1, "head", "the stream has entries but no recorded head")
		case h.seq > t.seq:
			return corrupt(stream, t.seq+1, "head", "the head records %d entries but only %d exist", h.seq, t.seq)
		case h.seq < t.seq:
			return corrupt(stream, h.seq+1, "head", "the head records %d entries but %d exist", h.seq, t.seq)
		case h.hash != t.hash:
			return corrupt(stream, t.seq, "head", "the last entry's hash differs from the recorded head")
		}
		return nil
	}

	rows, err := tx.Query(ctx, `SELECT `+logCols+` FROM audit_log WHERE run_id = $1 ORDER BY stream COLLATE "C", seq`, runID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var (
		cur     string
		started bool
		prev    head
	)
	for rows.Next() {
		e, err := scanLogEntry(rows)
		if err != nil {
			return err
		}
		if !started || e.Stream != cur {
			if started {
				if err := endStream(cur); err != nil {
					return err
				}
			}
			cur, started, prev = e.Stream, true, head{}
		}
		if e.Seq != prev.seq+1 {
			return corrupt(e.Stream, prev.seq+1, "gap", "expected seq %d but the next entry is seq %d", prev.seq+1, e.Seq)
		}
		if e.PrevHash != prev.hash {
			return corrupt(e.Stream, e.Seq, "link", "its PrevHash is not the hash of entry %d", prev.seq)
		}
		if want := LogEntryHash(e); e.EntryHash != want {
			return corrupt(e.Stream, e.Seq, "hash", "its EntryHash does not match its content")
		}
		prev = head{seq: e.Seq, hash: e.EntryHash}
		tails[e.Stream] = prev
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if started {
		if err := endStream(cur); err != nil {
			return err
		}
	}
	// Streams that have a head but no entries at all.
	for _, st := range streams {
		if _, seen := tails[st]; !seen && heads[st].seq != 0 {
			return corrupt(st, 1, "head", "the head records %d entries but none exist", heads[st].seq)
		}
	}
	return nil
}

// IsLogCorrupt reports whether err is a VerifyLog verification failure, as
// opposed to a failure to read the log.
func IsLogCorrupt(err error) bool { return errors.Is(err, ErrLogCorrupt) }

// Package auditlogtest provides an in-memory audit log for tests.
package auditlogtest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/store"
)

// Fake is a auditlog.Source over a fixed set of entries. It filters and
// pages the way the store does: (stream, seq) order, a cursor to resume,
// and a nil Next on the last page.
type Fake struct {
	// MaxPage caps the page size whatever the filter asks for, to force
	// multi-page reads. Zero means the store's cap of 5000.
	MaxPage int
	// EagerNext returns a cursor whenever a page is full, even if nothing
	// follows it, as a store that does not look ahead would. The page
	// after the last entry is then empty.
	EagerNext bool
	// Problem, when set, makes Verify report a broken chain.
	Problem string

	mu      sync.Mutex
	entries []store.LogEntry
	err     error
	calls   []store.LogFilter
}

// New returns a Fake holding entries.
func New(entries []store.LogEntry) *Fake {
	sorted := append([]store.LogEntry(nil), entries...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Stream != sorted[j].Stream {
			return sorted[i].Stream < sorted[j].Stream
		}
		return sorted[i].Seq < sorted[j].Seq
	})
	return &Fake{entries: sorted}
}

// Fail makes every following call return err; nil restores service.
func (f *Fake) Fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err = err
}

// Calls returns the filters of the audit log calls made so far.
func (f *Fake) Calls() []store.LogFilter {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]store.LogFilter(nil), f.calls...)
}

// AuditLog implements auditlog.Source.
func (f *Fake) AuditLog(_ context.Context, q store.LogFilter) (controller.LogPage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, q)
	if f.err != nil {
		return controller.LogPage{}, f.err
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 200
	}
	limit = min(limit, 5000)
	if f.MaxPage > 0 {
		limit = min(limit, f.MaxPage)
	}
	needle := strings.ToLower(q.Contains)
	page := controller.LogPage{Entries: []store.LogEntry{}}
	for _, e := range f.entries {
		switch {
		case q.Stream != "" && e.Stream != q.Stream,
			q.Op != "" && e.Op != q.Op,
			q.Outcome != "" && e.Outcome != q.Outcome,
			!q.Since.IsZero() && e.At.Before(q.Since),
			!q.Until.IsZero() && !e.At.Before(q.Until),
			e.Stream < q.After.Stream,
			e.Stream == q.After.Stream && e.Seq <= q.After.Seq:
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(e.SourceID+"\x00"+e.TargetID+"\x00"+e.Detail), needle) {
			continue
		}
		if len(page.Entries) == limit {
			last := page.Entries[limit-1]
			page.Next = &store.LogCursor{Stream: last.Stream, Seq: last.Seq}
			break
		}
		page.Entries = append(page.Entries, e)
	}
	if f.EagerNext && page.Next == nil && len(page.Entries) == limit {
		last := page.Entries[limit-1]
		page.Next = &store.LogCursor{Stream: last.Stream, Seq: last.Seq}
	}
	return page, nil
}

// Verify implements auditlog.Source.
func (f *Fake) Verify(context.Context) (bool, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, "", f.err
	}
	return f.Problem == "", f.Problem, nil
}

// Start is the time of the first sample entry.
var Start = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)

// Sample returns a small but realistic audit log: a team, two channels with
// messages and files, some failures, a finalized channel and members. It
// is deterministic. One failure's detail carries terminal escape sequences
// and one source ID a spreadsheet formula, as hostile input would.
func Sample() []store.LogEntry {
	var out []store.LogEntry
	at := Start
	seq := map[string]int64{}
	prev := map[string]string{}
	add := func(stream, op, source, target, hash, outcome, detail string) {
		seq[stream]++
		at = at.Add(1500 * time.Millisecond)
		e := store.LogEntry{
			Stream: stream, Seq: seq[stream], At: at, Actor: "worker-7f3a/2",
			Op: op, SourceID: source, TargetID: target, ContentHash: hash,
			Outcome: outcome, Detail: detail,
			PrevHash:  prev[stream],
			EntryHash: digest(fmt.Sprintf("%s/%d", stream, seq[stream])),
		}
		prev[stream] = e.EntryHash
		out = append(out, e)
	}

	add("team:0", "team.create", "team/0", "5b1f0c7e-2a4d-4e55-9c1d-0d6f1f6b7a10", "", "ok", "")
	for _, ch := range []struct {
		id, teams string
		posts     int
	}{{"C024BE91L", "19:8f2c4b@thread.tacv2", 140}, {"C07QX3M2K", "19:a41d9e@thread.tacv2", 60}} {
		load, files := "load:"+ch.id, "files:"+ch.id
		add(load, "channel.create", "channel/"+ch.id, ch.teams, "", "ok", "")
		add(load, "channel.start_migration", "channel/"+ch.id, ch.teams, "", "ok", "")
		for i := range ch.posts {
			source := fmt.Sprintf("%s/17000%05d.%06d", ch.id, i*37, i*100)
			target := fmt.Sprintf("17000%05d%03d", i*37, i%1000)
			hash := digest(source)
			switch {
			case i == 17:
				add(load, "message.fail", source, "", hash, "error",
					"graph: 403 Forbidden: \x1b[31mInsufficient privileges\x1b[0m to complete the operation\x1b]0;pwned\x07 (request-id 9d1c)\r\nretry after consent")
			case i%41 == 40:
				add(load, "message.fail", source, "", hash, "error", "graph: 413 Request Entity Too Large: message body exceeds 4 MB after conversion")
			case i%29 == 28:
				add(load, "message.hold", source, "", hash, "ok", "held: author is not mapped to a Teams user")
			case i%13 == 12:
				add(load, "message.adopt", source, target, hash, "ok", "already present from an earlier attempt")
			default:
				add(load, "message.import", source, target, hash, "ok", "")
			}
			if i%20 == 5 {
				file := fmt.Sprintf("%s/F0%05d", ch.id, i)
				if i%60 == 45 {
					add(files, "file.fail", file, "", digest(file), "error", "slack: file was deleted before it could be downloaded")
				} else {
					add(files, "file.upload", file, fmt.Sprintf("01DRIVEITEM%05d", i), digest(file), "ok", "")
				}
			}
		}
	}
	add("load:C024BE91L", "message.import", "=HYPERLINK(\"http://evil.example\")", "1700009999001", digest("formula"), "ok", "")
	add("finalize:C024BE91L", "channel.complete_migration", "channel/C024BE91L", "19:8f2c4b@thread.tacv2", "", "ok", "")
	add("members:0", "member.add", "user/U02AB12CD", "c1a8e2d4-77b0-4f1e-8a57-3e0b9d2f6c11", "", "ok", "owner")
	add("members:0", "member.skip", "user/U09ZZ00ZZ", "", "", "ok", "no Entra match; listed in the report")
	return out
}

func digest(s string) string {
	// A stable stand-in for SHA-256: 64 hex characters derived from s.
	const hexdigits = "0123456789abcdef"
	var h uint64 = 14695981039346656037
	b := make([]byte, 64)
	for i := range b {
		for j := 0; j < len(s); j++ {
			h ^= uint64(s[j])
			h *= 1099511628211
		}
		h ^= uint64(i)
		h *= 1099511628211
		b[i] = hexdigits[h>>60]
	}
	return string(b)
}

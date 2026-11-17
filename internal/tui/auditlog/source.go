// Package auditlog browses, pages through and exports the audit log: the
// append-only, hash-chained record of everything Taut wrote, or tried to
// write, to Microsoft 365.
//
// The package is written against Source rather than the controller client, so the
// command, the full-screen browser and the tests all read the audit trail
// the same way.
package auditlog

import (
	"context"
	"slices"

	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/store"
)

// Source reads one run's audit log.
type Source interface {
	// AuditLog returns one page of entries matching f, in (stream, seq)
	// order, starting after f.After. The page's Next is nil on the last
	// page.
	AuditLog(ctx context.Context, f store.LogFilter) (controller.LogPage, error)
	// Verify recomputes the run's hash chains. When they do not verify, ok
	// is false and problem names what is wrong.
	Verify(ctx context.Context) (ok bool, problem string, err error)
}

// PageSize is how many entries Walk asks for at a time.
const PageSize = 1000

// Walk calls fn for every entry matching f, in order, paging until the
// source reports no further page. A positive limit stops after that many
// entries; more then reports whether at least one further entry matches, so
// callers never have to guess whether a listing is complete.
//
// f.After is honored as the starting position; f.Limit is ignored.
func Walk(ctx context.Context, src Source, f store.LogFilter, limit int, fn func(store.LogEntry) error) (more bool, err error) {
	seen := 0
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		f.Limit = PageSize
		if limit > 0 {
			// One extra, so reaching the limit also tells us whether
			// anything lies beyond it.
			f.Limit = min(PageSize, limit-seen+1)
		}
		page, err := src.AuditLog(ctx, f)
		if err != nil {
			return false, err
		}
		for _, e := range page.Entries {
			if limit > 0 && seen == limit {
				return true, nil
			}
			if err := fn(e); err != nil {
				return false, err
			}
			seen++
		}
		if page.Next == nil {
			return false, nil
		}
		if *page.Next == f.After {
			// A cursor that does not advance would loop forever and,
			// worse, repeat entries in an export.
			return false, stalled(f.After)
		}
		f.After = *page.Next
	}
}

func stalled(c store.LogCursor) error {
	return fail.New("audit.cursor_stalled",
		"The audit log query stopped making progress",
		"The control plane returned the same position twice ("+c.Stream+" #"+itoa(c.Seq)+").",
		"Check that `taut` and the controller are the same version (`taut doctor`), then retry.")
}

// Streams returns a Source that reads the given streams of src one after
// another, as if they were one. It is how a channel, whose entries live in
// several lanes, is read with a filter that names a single stream.
//
// Any Stream in the filter is ignored. Cursors returned by the result are
// only meaningful to it.
func Streams(src Source, streams []string) Source {
	return &multi{src: src, streams: slices.Clone(streams)}
}

type multi struct {
	src     Source
	streams []string
}

func (m *multi) Verify(ctx context.Context) (bool, string, error) { return m.src.Verify(ctx) }

func (m *multi) AuditLog(ctx context.Context, f store.LogFilter) (controller.LogPage, error) {
	after := f.After
	i := 0
	if after.Stream != "" {
		i = slices.Index(m.streams, after.Stream)
		if i < 0 {
			return controller.LogPage{}, fail.New("audit.bad_cursor",
				"The audit log query lost its place",
				"The position "+after.Stream+" is not one of the streams being read.",
				"Run the command again.")
		}
	}
	for ; i < len(m.streams); i++ {
		q := f
		q.Stream = m.streams[i]
		q.After = store.LogCursor{}
		// A cursor at seq 0 means "the start of this stream".
		if after.Stream == q.Stream && after.Seq > 0 {
			q.After = after
		}
		page, err := m.src.AuditLog(ctx, q)
		if err != nil {
			return controller.LogPage{}, err
		}
		if page.Next != nil || i == len(m.streams)-1 {
			return page, nil
		}
		// This stream is exhausted; carry on with the next one.
		if len(page.Entries) > 0 {
			page.Next = &store.LogCursor{Stream: m.streams[i+1]}
			return page, nil
		}
		after = store.LogCursor{}
	}
	return controller.LogPage{}, nil
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

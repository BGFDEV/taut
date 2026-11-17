package worker

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/pipeline"
	"github.com/bgfdev/taut/internal/staging"
)

// limiter is a small token bucket: at most rate events per second, with a
// burst of one second's worth. It is safe for concurrent use.
type limiter struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time
}

func newLimiter(perSecond float64) *limiter {
	return &limiter{interval: time.Duration(float64(time.Second) / perSecond)}
}

// Wait blocks until the next event is allowed or ctx is done.
func (l *limiter) Wait(ctx context.Context) error {
	l.mu.Lock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	wait := l.next.Sub(now)
	l.next = l.next.Add(l.interval)
	l.mu.Unlock()
	if wait <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// copyCtx copies src to dst, stopping promptly when ctx is cancelled.
func copyCtx(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 256<<10)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		r, err := src.Read(buf)
		if r > 0 {
			w, werr := dst.Write(buf[:r])
			n += int64(w)
			if werr != nil {
				return n, werr
			}
		}
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
}

// describe renders an error for the ledger and the logs: the structured
// "what" and "why" when there is one, never a stack of wrapped prefixes.
// Errors in this codebase carry identifiers and codes, not message content,
// so this is safe to store.
func describe(err error) string {
	var fe *fail.Error
	if errors.As(err, &fe) {
		s := fe.What
		if fe.Why != "" {
			s += ": " + fe.Why
		}
		if fe.Code != "" {
			s += " [" + fe.Code + "]"
		}
		return s
	}
	return err.Error()
}

// code returns the structured error code of err, or "".
func code(err error) string {
	var fe *fail.Error
	if errors.As(err, &fe) {
		return fe.Code
	}
	return ""
}

// writeBatch streams records into an encrypted staging object. fill writes
// the records; the object becomes visible only if fill succeeds.
func writeBatch(ctx context.Context, blobs staging.Blobs, key string, fill func(w *pipeline.Writer) error) (int, error) {
	pr, pw := io.Pipe()
	w := pipeline.NewWriter(pw)
	errc := make(chan error, 1)
	go func() {
		err := fill(w)
		if err == nil {
			err = w.Close()
		}
		// Closing with an error makes the reader side (the upload) fail,
		// so a half-written batch is never stored.
		pw.CloseWithError(err)
		errc <- err
	}()
	_, putErr := blobs.Put(ctx, key, pr)
	// Unblock the writer if the upload stopped early.
	pr.CloseWithError(putErr)
	if fillErr := <-errc; fillErr != nil {
		return 0, fillErr
	}
	if putErr != nil {
		return 0, putErr
	}
	return w.Count(), nil
}

// readBatch loads every record of a staging object.
func readBatch[T any](ctx context.Context, blobs staging.Blobs, key string) ([]T, error) {
	r, err := blobs.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []T
	for v, err := range pipeline.Read[T](r) {
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

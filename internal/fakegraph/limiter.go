package fakegraph

import (
	"math"
	"sync"
	"time"
)

// limiter is a set of token buckets addressed by an opaque key: one per app
// per tenant for every Graph call, and one per channel for message POSTs.
type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter() *limiter { return &limiter{buckets: map[string]*bucket{}} }

// allow takes one token from key's bucket. When the bucket is empty it
// reports how long until a token is available.
func (l *limiter) allow(key string, rps float64, burst int, now time.Time) (ok bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil {
		b = &bucket{tokens: float64(burst), last: now}
		l.buckets[key] = b
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(float64(burst), b.tokens+elapsed*rps)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, time.Duration((1 - b.tokens) / rps * float64(time.Second))
}

func tenantAppKey(tenantID, appID string) string { return "tenant/" + tenantID + "/app/" + appID }

func channelKey(teamID, channelID string) string { return "team/" + teamID + "/channel/" + channelID }

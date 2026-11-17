package egressproxy

import (
	"container/list"
	"context"
	"math"
	"time"

	"github.com/bgfdev/taut/internal/sidecar/budget"
)

// AIMD constants. See the package documentation for the reasoning.
const (
	// minMultiplier is the floor of a tenant multiplier.
	minMultiplier = 0.1
	// tenantDecrease is the factor applied on a tenant-wide throttle.
	tenantDecrease = 0.5
	// tenantHedgeDecrease is the factor applied to the tenant multiplier
	// when a channel request is throttled.
	tenantHedgeDecrease = 0.9
	// tenantHedgePause caps the tenant pause for a throttled channel request.
	tenantHedgePause = 2 * time.Second
	// tenantIncreasePerSecond is the additive increase of the multiplier
	// per second of successful traffic.
	tenantIncreasePerSecond = 0.02
	// channelDecrease is the factor applied to a throttled channel's rate.
	channelDecrease = 0.5
	// channelFloorRPS is the lowest rate a channel is slowed to.
	channelFloorRPS = 0.1
	// channelIncreaseRPS is added per channelIncreaseEvery successes.
	channelIncreaseRPS   = 0.5
	channelIncreaseEvery = 30
	// defaultRetryAfter is assumed for a 429 without Retry-After.
	defaultRetryAfter = 5 * time.Second
	// maxRetryAfter bounds any pause, however large the upstream's (or a
	// bus event's) number.
	maxRetryAfter = 10 * time.Minute
	// jitterFraction of the pause is added at random.
	jitterFraction = 0.1

	// maxSleep bounds one wait so rate and pause changes are noticed.
	maxSleep = time.Second
	minSleep = time.Millisecond
	// defaultRefresh is the lease retry interval when no TTL is known.
	defaultRefresh = 5 * time.Second
	minRefresh     = 100 * time.Millisecond
	// maxRemotePause caps a pause taken from a bus event, which nothing
	// authenticates. A 429 seen first-hand keeps the larger maxRetryAfter.
	maxRemotePause = 30 * time.Second
	// maxEventAge is how far a bus event's timestamp may be from now.
	maxEventAge = 60 * time.Second
	// maxLeaseTTL caps how long a lease from the bus is believed.
	maxLeaseTTL = 60 * time.Second
	// tenantIdle is how long an unused tenant bucket is kept.
	tenantIdle = 10 * time.Minute
)

// bucket is a token bucket. Its burst is one second of rate, at least 1.
type bucket struct {
	rate   float64
	tokens float64
	last   time.Time
}

func burstOf(rate float64) float64 { return math.Max(1, rate) }

func newBucket(rate float64, now time.Time) bucket {
	return bucket{rate: rate, tokens: burstOf(rate), last: now}
}

func (b *bucket) refill(now time.Time) {
	if el := now.Sub(b.last); el > 0 {
		b.tokens = math.Min(burstOf(b.rate), b.tokens+el.Seconds()*b.rate)
		b.last = now
	} else if el < 0 {
		b.last = now // the clock went backwards; start over from here
	}
}

// setRate changes the rate, crediting time already elapsed at the old one.
func (b *bucket) setRate(rate float64, now time.Time) {
	b.refill(now)
	b.rate = rate
	b.tokens = math.Min(b.tokens, burstOf(rate))
}

const tokenEpsilon = 1e-9

// need returns how long until one token is available; zero means now.
func (b *bucket) need() time.Duration {
	if b.tokens >= 1-tokenEpsilon {
		return 0
	}
	return time.Duration(math.Ceil((1 - b.tokens) / b.rate * float64(time.Second)))
}

func (b *bucket) take() { b.tokens = math.Max(0, b.tokens-1) }

// tenantState is the admission state of one (tenant, scope).
type tenantState struct {
	key    budget.Key
	bucket bucket
	// chats bounds message imports into chats to a share of the tenant's
	// rate (Config.ChatShare). It is created by the first such request.
	chats *bucket

	leaseRPS    float64 // last lease; 0 before the first
	leaseTTL    time.Duration
	leased      bool // the last lease attempt succeeded
	leaseAt     time.Time
	nextRefresh time.Time
	refreshing  bool
	fresh       bool // no lease result applied yet
	overflow    bool // the shared bucket for keys beyond MaxTenants

	mult         float64
	pauseUntil   time.Time
	lastDecrease time.Time
	lastIncrease time.Time
	waiting      int
	lastUsed     time.Time
}

// channelState is the admission state of one Teams channel.
type channelState struct {
	key          string
	bucket       bucket
	pauseUntil   time.Time
	lastDecrease time.Time
	successes    int
	waiting      int
	elem         *list.Element
}

// fallbackBase is the rate used when no valid lease is held.
func (g *Proxy) fallbackBase(ts *tenantState) float64 {
	r := g.cfg.FallbackRPS
	if ts.overflow {
		return r
	}
	if total, ok := scopeTotals[ts.key.Scope]; ok {
		r = math.Min(r, total)
	}
	if ts.leaseRPS > 0 {
		r = math.Min(r, ts.leaseRPS)
	}
	return r
}

var scopeTotals = budget.DefaultTotals()

// baseRate is the lease if it is valid, the fallback otherwise.
func (g *Proxy) baseRate(ts *tenantState, now time.Time) float64 {
	if ts.leased && !now.After(ts.leaseAt.Add(ts.leaseTTL)) {
		return ts.leaseRPS
	}
	return g.fallbackBase(ts)
}

// syncRate brings the tenant bucket's rate up to date. g.mu is held.
func (g *Proxy) syncRate(ts *tenantState, now time.Time) {
	if r := g.baseRate(ts, now) * ts.mult; r != ts.bucket.rate {
		ts.bucket.setRate(r, now)
	}
	if ts.chats != nil {
		if r := ts.bucket.rate * g.cfg.ChatShare; r != ts.chats.rate {
			ts.chats.setRate(r, now)
		}
	}
}

// chatsLocked returns the tenant's bucket for chat imports, creating it.
// g.mu is held.
func (g *Proxy) chatsLocked(ts *tenantState, now time.Time) *bucket {
	if ts.chats == nil {
		b := newBucket(ts.bucket.rate*g.cfg.ChatShare, now)
		ts.chats = &b
	}
	return ts.chats
}

// overflowKey names the one bucket shared by all keys beyond MaxTenants.
var overflowKey = budget.Key{Tenant: "(overflow)", Scope: "overflow"}

// tenantLocked returns the state for key, creating it. g.mu is held.
//
// The key's tenant is a string the caller chose, so the number of buckets
// is capped: once MaxTenants exist (and none is idle enough to drop), every
// further key shares a single overflow bucket at the fallback rate. It
// leases nothing, so invented tenants create neither state nor lease
// requests.
func (g *Proxy) tenantLocked(key budget.Key, now time.Time) *tenantState {
	if ts := g.tenants[key]; ts != nil {
		ts.lastUsed = now
		return ts
	}
	if len(g.tenants) >= g.cfg.MaxTenants {
		for k, old := range g.tenants {
			if old.waiting == 0 && !old.refreshing && !old.pauseUntil.After(now) && now.Sub(old.lastUsed) > tenantIdle {
				delete(g.tenants, k)
			}
		}
	}
	if len(g.tenants) >= g.cfg.MaxTenants {
		if g.overflow == nil {
			g.overflow = &tenantState{key: overflowKey, mult: 1, overflow: true, lastIncrease: now}
			g.overflow.bucket = newBucket(g.cfg.FallbackRPS, now)
			g.log.Warn("too many tenant buckets; further tenants share one bucket at the fallback rate",
				"maxTenants", g.cfg.MaxTenants, "fallbackRps", g.cfg.FallbackRPS)
		}
		g.overflow.lastUsed = now
		return g.overflow
	}
	ts := &tenantState{key: key, mult: 1, fresh: true, lastUsed: now, lastIncrease: now}
	ts.bucket = newBucket(g.fallbackBase(ts), now)
	g.tenants[key] = ts
	return ts
}

// maxRPS is the most a lease for scope is believed to grant.
func (g *Proxy) maxRPS(scope string) float64 {
	if v, ok := g.cfg.MaxRPS[scope]; ok && v > 0 {
		return v
	}
	if v, ok := scopeTotals[scope]; ok {
		return v
	}
	return budget.UnknownScopeRPS
}

// startRPS and maxConversationRPS are the rate a conversation's bucket
// starts at and the most it is raised to. A channel starts low and is
// raised while Microsoft does not object. A chat is not: its documented
// limit is its ceiling.
func (g *Proxy) startRPS(key string) float64 {
	if isChatKey(key) {
		return g.cfg.ChatRPS
	}
	return g.cfg.ChannelStartRPS
}

func (g *Proxy) maxConversationRPS(key string) float64 {
	if isChatKey(key) {
		return g.cfg.ChatRPS
	}
	return g.cfg.ChannelMaxRPS
}

// channelLocked returns the state for key, creating it and evicting the
// least recently used idle channels beyond the bound. g.mu is held.
func (g *Proxy) channelLocked(key string, now time.Time) *channelState {
	if cs := g.channels[key]; cs != nil {
		g.channelLRU.MoveToFront(cs.elem)
		return cs
	}
	cs := &channelState{key: key, bucket: newBucket(g.startRPS(key), now)}
	cs.elem = g.channelLRU.PushFront(cs)
	g.channels[key] = cs
	for e := g.channelLRU.Back(); e != nil && len(g.channels) > g.cfg.MaxChannels; {
		prev := e.Prev()
		// A channel with waiters or an active pause still carries state
		// that matters; skip it.
		if old := e.Value.(*channelState); old != cs && old.waiting == 0 && !old.pauseUntil.After(now) {
			g.channelLRU.Remove(e)
			delete(g.channels, old.key)
		}
		e = prev
	}
	return cs
}

// applyLease records the result of a lease request. g.mu is held.
func (g *Proxy) applyLease(ts *tenantState, l budget.Lease, err error, now time.Time) {
	wasLeased, oldRPS := ts.leased, ts.leaseRPS
	if err == nil && (l.RPS <= 0 || math.IsNaN(l.RPS) || math.IsInf(l.RPS, 0)) {
		err = errBadLease
	}
	if err == nil {
		// The lease came over the bus, which authenticates nobody. Believe
		// it only up to the scope's whole budget and for a bounded time, so
		// a forged reply cannot lift the limit or pin it for long.
		l.RPS = math.Min(l.RPS, g.maxRPS(ts.key.Scope))
		if l.TTL <= 0 || l.TTL > maxLeaseTTL {
			l.TTL = maxLeaseTTL
		}
	}
	interval := defaultRefresh
	if err != nil {
		ts.leased = false
		if ts.leaseTTL > 0 {
			interval = min(ts.leaseTTL/2, defaultRefresh)
		}
		if wasLeased || ts.fresh {
			g.log.Warn("rate lease unavailable; using the fallback rate",
				"tenant", ts.key.Tenant, "scope", ts.key.Scope, "fallbackRps", g.fallbackBase(ts), "error", err)
		}
	} else {
		ts.leased, ts.leaseRPS, ts.leaseTTL, ts.leaseAt = true, l.RPS, l.TTL, now
		if l.TTL > 0 {
			interval = l.TTL / 2
		}
		if !wasLeased || oldRPS != l.RPS {
			g.log.Info("rate lease changed", "tenant", ts.key.Tenant, "scope", ts.key.Scope, "rps", l.RPS, "ttl", l.TTL.String())
		}
	}
	ts.nextRefresh = now.Add(max(interval, minRefresh))
	g.syncRate(ts, now)
	if ts.fresh {
		ts.fresh = false
		ts.bucket.tokens = burstOf(ts.bucket.rate) // a new bucket starts full
	}
}

// lease asks the budget source, treating a missing source as a failure.
func (g *Proxy) lease(ctx context.Context, key budget.Key) (budget.Lease, error) {
	if g.cfg.Budgets == nil {
		return budget.Lease{}, errNoBudgets
	}
	return g.cfg.Budgets.Lease(ctx, key, g.cfg.Holder)
}

// refresh renews a lease in the background.
func (g *Proxy) refresh(ts *tenantState) {
	defer g.wg.Done()
	ctx, cancel := context.WithTimeout(g.ctx, 5*time.Second)
	defer cancel()
	l, err := g.lease(ctx, ts.key)
	g.mu.Lock()
	ts.refreshing = false
	g.applyLease(ts, l, err, g.now())
	g.mu.Unlock()
}

// admit blocks until rt may be sent, and returns when it was admitted.
func (g *Proxy) admit(ctx context.Context, rt *route) (time.Time, error) {
	if rt.scope == ScopeOther {
		return g.now(), ctx.Err()
	}
	g.mu.Lock()
	ts := g.tenantLocked(rt.key, g.now())
	g.mu.Unlock()

	var cs *channelState
	queued := false
	defer func() {
		if queued {
			g.mu.Lock()
			ts.waiting--
			if cs != nil {
				cs.waiting--
			}
			g.mu.Unlock()
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return time.Time{}, err
		}
		g.mu.Lock()
		now := g.now()
		ts.lastUsed = now
		if !ts.overflow && !ts.refreshing && !now.Before(ts.nextRefresh) && g.ctx.Err() == nil {
			ts.refreshing = true
			// The first lease, and one replacing a lease that has already
			// run out, are taken before the request proceeds, so it runs
			// at a rate somebody granted. Routine renewals at TTL/2 happen
			// in the background.
			if ts.fresh || (ts.leased && now.After(ts.leaseAt.Add(ts.leaseTTL))) || g.refreshInline {
				g.mu.Unlock()
				l, err := g.lease(ctx, ts.key)
				g.mu.Lock()
				ts.refreshing = false
				g.applyLease(ts, l, err, g.now())
				g.mu.Unlock()
				continue
			}
			g.wg.Add(1)
			go g.refresh(ts)
		}
		g.syncRate(ts, now)
		if rt.chanKey != "" && cs == nil {
			cs = g.channelLocked(rt.chanKey, now)
			if queued {
				cs.waiting++
			}
		}

		var wait time.Duration
		if d := ts.pauseUntil.Sub(now); d > 0 {
			wait = d
		}
		if cs != nil {
			if d := cs.pauseUntil.Sub(now); d > wait {
				wait = d
			}
		}
		var chats *bucket
		if isChatKey(rt.chanKey) {
			chats = g.chatsLocked(ts, now)
		}
		if wait == 0 {
			ts.bucket.refill(now)
			wait = ts.bucket.need()
			if cs != nil {
				cs.bucket.refill(now)
				wait = max(wait, cs.bucket.need())
			}
			if chats != nil {
				chats.refill(now)
				wait = max(wait, chats.need())
			}
			if wait == 0 {
				ts.bucket.take()
				if cs != nil {
					cs.bucket.take()
				}
				if chats != nil {
					chats.take()
				}
				g.mu.Unlock()
				return now, nil
			}
		}
		if !queued {
			queued = true
			ts.waiting++
			if cs != nil {
				cs.waiting++
			}
		}
		g.mu.Unlock()

		if err := g.cfg.Sleep(ctx, min(max(wait, minSleep), maxSleep)); err != nil {
			return time.Time{}, err
		}
	}
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// jittered adds up to jitterFraction of d.
func (g *Proxy) jittered(d time.Duration) time.Duration {
	j := g.cfg.Jitter()
	if j < 0 || j > 1 || math.IsNaN(j) {
		j = 0
	}
	return d + time.Duration(float64(d)*jitterFraction*j)
}

// throttled applies a 429/503 to rt's buckets and returns the tenant-scope
// pause to broadcast.
func (g *Proxy) throttled(rt *route, admittedAt time.Time, retryAfter time.Duration) (tenantPause time.Duration, mult float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	ts := g.tenantLocked(rt.key, now)
	g.throttles++
	freshT := !admittedAt.Before(ts.lastDecrease)
	factor := tenantDecrease
	tenantPause = retryAfter
	if rt.chanKey != "" {
		cs := g.channelLocked(rt.chanKey, now)
		cs.pauseUntil = later(cs.pauseUntil, now.Add(g.jittered(retryAfter)))
		cs.successes = 0
		if !admittedAt.Before(cs.lastDecrease) {
			cs.bucket.setRate(math.Max(math.Min(channelFloorRPS, g.startRPS(rt.chanKey)), cs.bucket.rate*channelDecrease), now)
			cs.lastDecrease = now
		}
		factor = tenantHedgeDecrease
		tenantPause = min(retryAfter, tenantHedgePause)
	}
	ts.pauseUntil = later(ts.pauseUntil, now.Add(g.jittered(tenantPause)))
	if freshT {
		ts.mult = math.Max(minMultiplier, ts.mult*factor)
		ts.lastDecrease = now
		g.syncRate(ts, now)
	}
	ts.lastIncrease = now
	return tenantPause, ts.mult
}

// succeeded applies additive increase after a 2xx/3xx.
func (g *Proxy) succeeded(rt *route) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if ts := g.tenants[rt.key]; ts != nil && ts.mult < 1 {
		if el := min(now.Sub(ts.lastIncrease), time.Second); el > 0 {
			ts.mult = math.Min(1, ts.mult+tenantIncreasePerSecond*el.Seconds())
			ts.lastIncrease = now
			g.syncRate(ts, now)
		}
	}
	if rt.chanKey == "" {
		return
	}
	if cs := g.channels[rt.chanKey]; cs != nil {
		cs.successes++
		if cs.successes >= channelIncreaseEvery {
			cs.successes = 0
			if r := math.Min(g.maxConversationRPS(rt.chanKey), cs.bucket.rate+channelIncreaseRPS); r > cs.bucket.rate {
				cs.bucket.setRate(r, now)
			}
		}
	}
}

// remoteThrottle applies a pause another egress proxy published.
func (g *Proxy) remoteThrottle(key budget.Key, pause time.Duration) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	ts := g.tenants[key]
	if ts == nil {
		return false
	}
	g.remoteThrottles++
	ts.pauseUntil = later(ts.pauseUntil, g.now().Add(g.jittered(min(pause, maxRemotePause))))
	return true
}

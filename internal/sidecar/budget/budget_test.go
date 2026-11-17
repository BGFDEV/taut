package budget

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/bus"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

var graphKey = Key{Tenant: "contoso", Scope: ScopeGraph}

func TestFairShares(t *testing.T) {
	for _, n := range []int{1, 2, 5} {
		c := newClock()
		a := NewAllocator(nil, 0, c.Now)
		// One round to register everybody, a second to see the settled share.
		for round := 0; round < 2; round++ {
			sum := 0.0
			for i := 0; i < n; i++ {
				l := a.Grant(graphKey, fmt.Sprintf("gov-%d", i))
				if l.TTL != DefaultTTL {
					t.Fatalf("TTL = %v", l.TTL)
				}
				if round == 1 && !near(l.RPS, 40/float64(n)) {
					t.Errorf("n=%d holder %d: share %.3f, want %.3f", n, i, l.RPS, 40/float64(n))
				}
				sum += l.RPS
			}
			if round == 1 && !near(sum, 40) {
				t.Errorf("n=%d: settled leases sum to %.3f, want 40", n, sum)
			}
			c.Advance(DefaultTTL / 2)
		}
	}
}

func TestColdStartOvershootIsHarmonic(t *testing.T) {
	a := NewAllocator(nil, 0, newClock().Now)
	sum := 0.0
	for i := 0; i < 5; i++ {
		sum += a.Grant(graphKey, fmt.Sprintf("gov-%d", i)).RPS
	}
	h5 := 1 + 1.0/2 + 1.0/3 + 1.0/4 + 1.0/5
	if !near(sum, 40*h5) {
		t.Errorf("cold-start sum %.3f, want the documented bound %.3f", sum, 40*h5)
	}
}

func TestSilentHolderIsDropped(t *testing.T) {
	c := newClock()
	a := NewAllocator(nil, 10*time.Second, c.Now)
	a.Grant(graphKey, "a")
	a.Grant(graphKey, "b")
	a.Grant(graphKey, "c")

	// a and b keep refreshing; c goes away.
	for elapsed := time.Duration(0); elapsed <= 20*time.Second; elapsed += 5 * time.Second {
		if got := a.Grant(graphKey, "a").RPS; !near(got, 40.0/3) {
			t.Fatalf("at +%v: share %.3f, want %.3f while c may still be alive", elapsed, got, 40.0/3)
		}
		a.Grant(graphKey, "b")
		c.Advance(5 * time.Second)
	}
	// Now 25 s since c last asked: more than 2×TTL.
	if got := a.Grant(graphKey, "a").RPS; !near(got, 20) {
		t.Errorf("after c went silent: share %.3f, want 20", got)
	}
	snap := a.Snapshot()
	if len(snap) != 1 || len(snap[0].Holders) != 2 || snap[0].Holders[0] != "a" || !near(snap[0].ShareRPS, 20) || snap[0].TotalRPS != 40 {
		t.Errorf("snapshot = %+v", snap)
	}

	// Everybody goes away: the key disappears.
	c.Advance(time.Minute)
	if snap := a.Snapshot(); len(snap) != 0 {
		t.Errorf("snapshot after all holders left = %+v", snap)
	}
	if got := a.Grant(graphKey, "z").RPS; !near(got, 40) {
		t.Errorf("fresh holder share %.3f, want 40", got)
	}
}

func TestKeysAreIndependentAndTotalsConfigurable(t *testing.T) {
	a := NewAllocator(map[string]float64{ScopeGraph: 10, "bogus": -1}, 0, newClock().Now)
	a.Grant(graphKey, "a")
	if got := a.Grant(graphKey, "b").RPS; !near(got, 5) {
		t.Errorf("graph share %.3f, want 5", got)
	}
	if got := a.Grant(Key{"fabrikam", ScopeGraph}, "a").RPS; !near(got, 10) {
		t.Errorf("other tenant share %.3f, want 10", got)
	}
	if got := a.Grant(Key{"contoso", ScopeSharePoint}, "a").RPS; !near(got, 20) {
		t.Errorf("sharepoint share %.3f, want the default 20", got)
	}
	if got := a.Grant(Key{"contoso", ScopeSlack}, "a").RPS; !near(got, 1) {
		t.Errorf("slack share %.3f, want the default 1", got)
	}
}

func TestUnknownScopeGetsSmallDefault(t *testing.T) {
	a := NewAllocator(nil, 0, newClock().Now)
	k := Key{"contoso", "carrier-pigeon"}
	if got := a.Grant(k, "a").RPS; !near(got, UnknownScopeRPS) {
		t.Errorf("share %.3f, want %.3f", got, UnknownScopeRPS)
	}
	if got := a.Grant(k, "b").RPS; !near(got, UnknownScopeRPS/2) {
		t.Errorf("share %.3f, want %.3f", got, UnknownScopeRPS/2)
	}
	l, err := Static(nil).Lease(context.Background(), k, "a")
	if err != nil || !near(l.RPS, UnknownScopeRPS) {
		t.Errorf("Static unknown scope = %+v, %v", l, err)
	}
}

func TestStatic(t *testing.T) {
	s := Static(map[string]float64{ScopeGraph: 7})
	for _, holder := range []string{"a", "b"} {
		l, err := s.Lease(context.Background(), graphKey, holder)
		if err != nil || l.RPS != 7 || l.TTL <= 0 {
			t.Errorf("Lease = %+v, %v", l, err)
		}
	}
	if l, _ := s.Lease(context.Background(), Key{"x", ScopeSlack}, "a"); l.RPS != 1 {
		t.Errorf("slack default = %+v", l)
	}
}

func TestServeAndRemote(t *testing.T) {
	b := bus.NewLocal()
	src := Remote(b)
	ctx := context.Background()

	if _, err := src.Lease(ctx, graphKey, "a"); !errors.Is(err, bus.ErrNoResponder) {
		t.Fatalf("with no allocator: err = %v, want bus.ErrNoResponder", err)
	}

	a := NewAllocator(nil, 4*time.Second, newClock().Now)
	stop, err := a.Serve(b)
	if err != nil {
		t.Fatal(err)
	}
	l, err := src.Lease(ctx, graphKey, "a")
	if err != nil || !near(l.RPS, 40) || l.TTL != 4*time.Second {
		t.Fatalf("first lease = %+v, %v", l, err)
	}
	l, err = src.Lease(ctx, graphKey, "b")
	if err != nil || !near(l.RPS, 20) {
		t.Fatalf("second lease = %+v, %v", l, err)
	}
	if _, err := src.Lease(ctx, Key{}, "a"); err == nil || errors.Is(err, bus.ErrNoResponder) {
		t.Errorf("empty key: err = %v, want a refusal", err)
	}

	stop()
	if _, err := src.Lease(ctx, graphKey, "a"); !errors.Is(err, bus.ErrNoResponder) {
		t.Errorf("after stop: err = %v, want bus.ErrNoResponder", err)
	}
}

func TestRemoteRejectsGarbageReplies(t *testing.T) {
	for _, reply := range []string{`nope`, `{}`, `{"rps":-1,"ttlMs":5}`, `{"rps":3}`} {
		b := bus.NewLocal()
		_, _ = b.Serve(Subject, func([]byte) []byte { return []byte(reply) })
		if l, err := Remote(b).Lease(context.Background(), graphKey, "a"); err == nil {
			t.Errorf("reply %q accepted as %+v", reply, l)
		}
	}
}

func TestConcurrentGrants(t *testing.T) {
	c := newClock()
	a := NewAllocator(nil, 0, c.Now)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				l := a.Grant(Key{fmt.Sprintf("t%d", j%3), ScopeGraph}, fmt.Sprintf("gov-%d", i))
				if l.RPS <= 0 || l.RPS > 40 {
					t.Errorf("share %.3f out of range", l.RPS)
				}
				if j%50 == 0 {
					c.Advance(time.Second)
					a.Snapshot()
				}
			}
		}()
	}
	wg.Wait()
	for _, ks := range a.Snapshot() {
		if len(ks.Holders) > 16 {
			t.Errorf("%v has %d holders", ks.Key, len(ks.Holders))
		}
	}
}

func TestAllocatorStateIsBounded(t *testing.T) {
	c := newClock()
	a := NewAllocator(nil, 10*time.Second, c.Now)
	a.maxKeys = 8

	for i := 0; i < 8; i++ {
		if l := a.Grant(Key{fmt.Sprintf("tenant-%d", i), ScopeGraph}, "gov"); !near(l.RPS, 40) {
			t.Fatalf("key %d: share %.3f", i, l.RPS)
		}
	}
	// Beyond the cap: the minimum, and nothing recorded.
	for i := 0; i < 1000; i++ {
		if l := a.Grant(Key{fmt.Sprintf("invented-%d", i), ScopeGraph}, "gov"); !near(l.RPS, UnknownScopeRPS) || l.TTL != 10*time.Second {
			t.Fatalf("over the cap: %+v, want the %.1f rps minimum", l, UnknownScopeRPS)
		}
	}
	if n := len(a.holders); n != 8 {
		t.Fatalf("tracked keys = %d, want 8", n)
	}
	// Known keys are unaffected.
	if l := a.Grant(Key{"tenant-3", ScopeGraph}, "gov"); !near(l.RPS, 40) {
		t.Errorf("existing key at the cap: %.3f", l.RPS)
	}

	// Idle keys are swept as Grants arrive, without anyone asking for
	// them, which frees room under the cap.
	c.Advance(21 * time.Second)
	if l := a.Grant(Key{"newcomer", ScopeGraph}, "gov"); !near(l.RPS, 40) {
		t.Errorf("after the idle keys expired: %.3f, want 40", l.RPS)
	}
	if n := len(a.holders); n != 1 {
		t.Errorf("tracked keys after the sweep = %d, want 1", n)
	}

	// The sweep also runs when the cap is not reached.
	b := NewAllocator(nil, 10*time.Second, c.Now)
	for i := 0; i < 50; i++ {
		b.Grant(Key{fmt.Sprintf("t%d", i), ScopeGraph}, fmt.Sprintf("h%d", i))
	}
	c.Advance(21 * time.Second)
	b.Grant(Key{"alive", ScopeGraph}, "gov")
	if n := len(b.holders); n != 1 {
		t.Errorf("idle keys left behind: %d tracked", n)
	}
}

func TestOverlongNamesAreRefused(t *testing.T) {
	a := NewAllocator(nil, 0, newClock().Now)
	long := strings.Repeat("x", MaxNameLen+1)
	ok := strings.Repeat("x", MaxNameLen)
	for _, c := range []struct {
		key    Key
		holder string
	}{{Key{long, ScopeGraph}, "gov"}, {Key{"contoso", long}, "gov"}, {Key{"contoso", ScopeGraph}, long}} {
		if l := a.Grant(c.key, c.holder); !near(l.RPS, UnknownScopeRPS) {
			t.Errorf("overlong name granted %.3f", l.RPS)
		}
	}
	if len(a.holders) != 0 {
		t.Errorf("overlong names were recorded: %d keys", len(a.holders))
	}
	if l := a.Grant(Key{ok, ScopeGraph}, ok); !near(l.RPS, 40) {
		t.Errorf("a name of exactly %d bytes: %.3f", MaxNameLen, l.RPS)
	}

	b := bus.NewLocal()
	stop, _ := a.Serve(b)
	defer stop()
	if _, err := Remote(b).Lease(context.Background(), Key{long, ScopeGraph}, "gov"); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Errorf("overlong tenant over the bus: %v", err)
	}
}

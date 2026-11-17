package egressproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/bus"
	"github.com/bgfdev/taut/internal/sidecar/budget"
	"github.com/bgfdev/taut/internal/sidecar/wire"
)

func ok(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(http.StatusOK) }

// graphHarness is a harness whose upstream is classified as Graph.
func graphHarness(t *testing.T, rps float64, mutate func(*Config)) (*harness, *upstream) {
	t.Helper()
	up := newUpstream(t, ok)
	r := newHarness(t, mutate, "graph=http://"+up.Listener.Addr().String())
	r.leases.set(rps, nil)
	return r, up
}

func (r *harness) post(up *upstream, channel string, want int) {
	r.t.Helper()
	rec := r.do(call{method: "POST", origin: up.URL, target: "/v1.0/teams/T/channels/" + channel + "/messages",
		auth: entraAuth, body: strings.NewReader("{}")})
	if rec.Code != want {
		r.t.Fatalf("POST status = %d, want %d", rec.Code, want)
	}
}

// elapsed runs f and returns the virtual time it took.
func (r *harness) elapsed(f func()) time.Duration {
	start := r.clock.Now()
	f()
	return r.clock.Now().Sub(start)
}

func TestTenantRateAndBurst(t *testing.T) {
	r, up := graphHarness(t, 10, nil)
	// The bucket holds one second of rate: ten go at once…
	if d := r.elapsed(func() {
		for i := 0; i < 10; i++ {
			r.get(up.URL, 200)
		}
	}); d != 0 {
		t.Errorf("the burst of 10 took %v of virtual time, want none", d)
	}
	// …and the next 25 are paced at 10 per second.
	if d := r.elapsed(func() {
		for i := 0; i < 25; i++ {
			r.get(up.URL, 200)
		}
	}); !near(d, 2500*time.Millisecond) {
		t.Errorf("25 requests at 10 rps took %v, want 2.5s", d)
	}
	// Idle time refills the bucket, but never beyond one second's worth.
	r.clock.Advance(10 * time.Minute)
	if d := r.elapsed(func() {
		for i := 0; i < 15; i++ {
			r.get(up.URL, 200)
		}
	}); !near(d, 500*time.Millisecond) {
		t.Errorf("15 requests after a long idle took %v, want 0.5s (burst 10, then 5 paced)", d)
	}
	if up.count() != 50 {
		t.Errorf("upstream saw %d requests", up.count())
	}
	if ts := r.tenant("contoso", ScopeGraph); ts.RateRPS != 10 || !ts.Leased || ts.QueueDepth != 0 {
		t.Errorf("state = %+v", ts)
	}
}

func TestSlowRateBurstIsOne(t *testing.T) {
	r, up := graphHarness(t, 0.5, nil)
	if d := r.elapsed(func() {
		for i := 0; i < 4; i++ {
			r.get(up.URL, 200)
		}
	}); !near(d, 6*time.Second) {
		t.Errorf("4 requests at 0.5 rps took %v, want 6s", d)
	}
}

func TestChannelRate(t *testing.T) {
	r, up := graphHarness(t, 1000, nil)
	// One post per second per channel, starting with a single token.
	if d := r.elapsed(func() {
		for i := 0; i < 5; i++ {
			r.post(up, "C1", 200)
		}
	}); !near(d, 4*time.Second) {
		t.Errorf("5 posts to one channel took %v, want 4s", d)
	}
	// Another channel is independent, and so is non-message traffic.
	if d := r.elapsed(func() {
		r.post(up, "C2", 200)
		r.get(up.URL, 200)
		rec := r.do(call{method: "POST", origin: up.URL, target: "/v1.0/teams/T/channels/C1/messages/M/replies", auth: entraAuth})
		if rec.Code != 200 {
			t.Fatalf("reply: %d", rec.Code)
		}
	}); !near(d, time.Second) {
		// Only the reply waits: it shares C1's bucket with the posts above.
		t.Errorf("mixed traffic took %v, want 1s", d)
	}
	if n := r.g.State().Channels; n != 2 {
		t.Errorf("channel buckets = %d, want 2", n)
	}
}

func (r *harness) postChat(up *upstream, chat string) {
	r.t.Helper()
	rec := r.do(call{method: "POST", origin: up.URL, target: "/v1.0/chats/" + chat + "/messages", auth: entraAuth, body: strings.NewReader("{}")})
	if rec.Code != 200 {
		r.t.Fatalf("POST to chat %s: status %d", chat, rec.Code)
	}
}

// A chat is paced like a channel: one message a second, in its own
// bucket, which a throttle slows and success speeds up again.
func TestChatRate(t *testing.T) {
	r, up := graphHarness(t, 1000, nil)
	if d := r.elapsed(func() {
		for i := 0; i < 5; i++ {
			r.postChat(up, "19:a@unq.gbl.spaces")
		}
	}); !near(d, 4*time.Second) {
		t.Errorf("5 posts to one chat took %v, want 4s", d)
	}
	// Another chat, a channel and other chat traffic do not wait for it.
	if d := r.elapsed(func() {
		r.postChat(up, "19:b@thread.v2")
		r.post(up, "C1", 200)
		for _, target := range []string{"/v1.0/chats", "/v1.0/chats/19:a@unq.gbl.spaces/startMigration", "/v1.0/chats/19:a@unq.gbl.spaces/completeMigration"} {
			if rec := r.do(call{method: "POST", origin: up.URL, target: target, auth: entraAuth}); rec.Code != 200 {
				t.Fatalf("POST %s: %d", target, rec.Code)
			}
		}
	}); d != 0 {
		t.Errorf("traffic outside the busy chat waited %v", d)
	}
	if n := r.g.State().Channels; n != 3 {
		t.Errorf("conversation buckets = %d, want 3 (two chats and a channel)", n)
	}
}

// Message imports into chats may use only a share of the tenant's Graph
// rate: Microsoft allows an app fewer chat messages per tenant than
// channel messages.
func TestChatsShareOfTheTenantRate(t *testing.T) {
	r, up := graphHarness(t, 40, nil)
	// Forty different chats, so no per-chat bucket is in the way. Two fifths
	// of the tenant's rate is theirs: sixteen go at once, the rest at 16 a
	// second.
	if d := r.elapsed(func() {
		for i := 0; i < 40; i++ {
			r.postChat(up, fmt.Sprintf("chat-%d", i))
		}
	}); !near(d, 1500*time.Millisecond) {
		t.Errorf("40 posts to 40 chats at 40 rps took %v, want 1.5s (a 16 rps share)", d)
	}
	// Channels are not held to that share.
	r.clock.Advance(time.Minute)
	if d := r.elapsed(func() {
		for i := 0; i < 40; i++ {
			r.post(up, fmt.Sprintf("C%d", i), 200)
		}
	}); d != 0 {
		t.Errorf("40 posts to 40 channels at 40 rps waited %v", d)
	}

	// The share is configuration.
	r2, up2 := graphHarness(t, 40, func(c *Config) { c.ChatShare = 0.25 })
	if d := r2.elapsed(func() {
		for i := 0; i < 20; i++ {
			r2.postChat(up2, fmt.Sprintf("chat-%d", i))
		}
	}); !near(d, time.Second) {
		t.Errorf("20 posts to 20 chats with a quarter share of 40 rps took %v, want 1s", d)
	}
}

// A chat's rate is a ceiling, not a start: however long it goes well, it is
// not raised beyond what Microsoft documents, although a channel's is.
func TestChatRateIsNotRaised(t *testing.T) {
	r, up := graphHarness(t, 1000, nil)
	// Enough successes to raise a channel's rate several times over.
	for i := 0; i < 4*channelIncreaseEvery; i++ {
		r.postChat(up, "19:a@unq.gbl.spaces")
		r.post(up, "C1", 200)
	}
	r.clock.Advance(time.Minute)
	chat := r.elapsed(func() {
		for i := 0; i < 5; i++ {
			r.postChat(up, "19:a@unq.gbl.spaces")
		}
	})
	if !near(chat, 4*time.Second) {
		t.Errorf("5 posts to a chat after a long good run took %v, want 4s: still one a second", chat)
	}
	r.clock.Advance(time.Minute)
	channel := r.elapsed(func() {
		for i := 0; i < 5; i++ {
			r.post(up, "C1", 200)
		}
	})
	if channel >= chat {
		t.Errorf("a channel's rate was not raised (%v for 5 posts); the comparison proves nothing", channel)
	}
	// The ceiling for chats is configuration of its own.
	r2, up2 := graphHarness(t, 1000, func(c *Config) { c.ChatRPS = 4 })
	if d := r2.elapsed(func() {
		for i := 0; i < 9; i++ {
			r2.postChat(up2, "19:b@thread.v2")
		}
	}); !near(d, 1250*time.Millisecond) {
		t.Errorf("9 posts to a chat at 4 rps took %v, want 1.25s (a burst of 4, then 5 paced)", d)
	}
}

// Two spellings of one conversation are one conversation.
func TestConversationSpellingsShareABucket(t *testing.T) {
	r, up := graphHarness(t, 1000, nil)
	if d := r.elapsed(func() {
		for _, target := range []string{
			"/v1.0/chats/19:A@unq.gbl.spaces/messages", "/v1.0/chats/19:a@unq.gbl.spaces/messages/",
			"/v1.0/Chats/19:a@unq.gbl.spaces/Messages", "/v1.0//chats/19:a@unq.gbl.spaces//messages",
		} {
			if rec := r.do(call{method: "POST", origin: up.URL, target: target, auth: entraAuth, body: strings.NewReader("{}")}); rec.Code != 200 {
				t.Fatalf("POST %s: %d", target, rec.Code)
			}
		}
	}); !near(d, 3*time.Second) {
		t.Errorf("4 posts to one chat under 4 spellings took %v, want 3s", d)
	}
	if n := r.g.State().Channels; n != 1 {
		t.Errorf("%d conversation buckets for one chat", n)
	}
}

// The chat share is more than 0 and at most 1; anything else is refused at
// start, not replaced by a guess.
func TestChatShareMustBeAShare(t *testing.T) {
	for _, share := range []float64{-0.5, 1.01, 2, math.NaN(), math.Inf(1)} {
		if _, err := New(Config{ChatShare: share}); err == nil {
			t.Errorf("a chat share of %v was accepted", share)
		}
	}
	for _, share := range []float64{0, 0.01, 0.5, 1} {
		g, err := New(Config{ChatShare: share})
		if err != nil {
			t.Errorf("a chat share of %v was refused: %v", share, err)
			continue
		}
		g.Close()
	}
	// With a share of 1, chats may use all of the tenant's rate.
	r, up := graphHarness(t, 40, func(c *Config) { c.ChatShare = 1 })
	if d := r.elapsed(func() {
		for i := 0; i < 40; i++ {
			r.postChat(up, fmt.Sprintf("chat-%d", i))
		}
	}); d != 0 {
		t.Errorf("40 posts to 40 chats with the whole rate waited %v", d)
	}
}

func TestOtherScopeIsUnlimited(t *testing.T) {
	up := newUpstream(t, ok)
	r := newHarness(t, nil, "http://"+up.Listener.Addr().String())
	r.leases.set(1, nil)
	if d := r.elapsed(func() {
		for i := 0; i < 50; i++ {
			if rec := r.do(call{origin: up.URL, auth: noAuth}); rec.Code != 200 {
				t.Fatalf("status %d", rec.Code)
			}
		}
	}); d != 0 || r.leases.calls != 0 || len(r.g.State().Tenants) != 0 {
		t.Errorf("unclassified host: waited %v, %d lease calls, %d buckets", d, r.leases.calls, len(r.g.State().Tenants))
	}
}

func TestTenantKeys(t *testing.T) {
	up := newUpstream(t, ok)
	host := up.Listener.Addr().String()
	r := newHarness(t, nil, "graph=http://"+host)
	r.do(call{origin: up.URL, auth: entraAuth})
	r.do(call{origin: up.URL, auth: noAuth})
	// One tenant spelled three ways is one bucket.
	for _, spelling := range []string{"Fabrikam ", "FABRIKAM", " fabrikam"} {
		if rec := r.do(call{origin: up.URL, auth: entraAuth, tenant: spelling}); rec.Code != 200 {
			t.Fatalf("status %d", rec.Code)
		}
	}
	if n := len(r.g.State().Tenants); n != 3 {
		t.Errorf("%d buckets, want 3: %+v", n, r.g.State().Tenants)
	}
	r.tenant(strings.Split(host, ":")[0], ScopeGraph)
	r.tenant("contoso", ScopeGraph)
	r.tenant("fabrikam", ScopeGraph)
}

func TestCancellationWhileWaitingConsumesNothing(t *testing.T) {
	r, up := graphHarness(t, 1, nil)
	r.get(up.URL, 200) // takes the only token

	ctx, cancel := context.WithCancel(context.Background())
	realSleep := r.g.cfg.Sleep
	var depthWhileWaiting int
	r.g.cfg.Sleep = func(ctx context.Context, d time.Duration) error {
		depthWhileWaiting = r.tenant("contoso", ScopeGraph).QueueDepth
		r.clock.Advance(d / 2) // part of the wait passes, then the caller gives up
		cancel()
		return ctx.Err()
	}
	rec := r.do(call{origin: up.URL, auth: entraAuth, ctx: ctx})
	r.g.cfg.Sleep = realSleep
	if rec.Code != 499 || errorCode(rec) != CodeCanceled {
		t.Errorf("canceled request: %d %s", rec.Code, rec.Body)
	}
	if depthWhileWaiting != 1 {
		t.Errorf("queue depth while waiting = %d, want 1", depthWhileWaiting)
	}
	if up.count() != 1 {
		t.Fatalf("the canceled request was sent upstream")
	}
	if d := r.tenant("contoso", ScopeGraph).QueueDepth; d != 0 {
		t.Errorf("queue depth after cancel = %d", d)
	}
	// Half a second passed; the next request needs only the other half.
	// Had the canceled request taken a token, it would need 1.5 s.
	if d := r.elapsed(func() { r.get(up.URL, 200) }); !near(d, 500*time.Millisecond) {
		t.Errorf("next request waited %v, want 0.5s", d)
	}

	// A request that arrives already canceled does not go out either.
	done, cancel2 := context.WithCancel(context.Background())
	cancel2()
	r.clock.Advance(time.Minute)
	if rec := r.do(call{origin: up.URL, auth: entraAuth, ctx: done}); rec.Code != 499 || up.count() != 2 {
		t.Errorf("pre-canceled request: %d, upstream %d", rec.Code, up.count())
	}
}

func TestFallbackAndRecovery(t *testing.T) {
	r, up := graphHarness(t, 10, nil)
	r.g.refreshInline = false // exercise the background renewal
	r.leases.set(0, errBoom)

	// No lease: 2 rps, burst 2, never unlimited.
	if d := r.elapsed(func() {
		for i := 0; i < 6; i++ {
			r.get(up.URL, 200)
		}
	}); !near(d, 2*time.Second) {
		t.Errorf("6 requests without a lease took %v, want 2s at the fallback rate", d)
	}
	if ts := r.tenant("contoso", ScopeGraph); ts.Leased || ts.RateRPS != 2 {
		t.Errorf("state without a lease = %+v", ts)
	}
	if !strings.Contains(r.logs.String(), "rate lease unavailable") {
		t.Error("no log line about the fallback")
	}

	// The control plane comes back; the next refresh picks it up.
	r.leases.set(10, nil)
	r.clock.Advance(defaultRefresh)
	r.get(up.URL, 200)
	eventually(t, "the lease to recover", func() bool { return r.tenant("contoso", ScopeGraph).Leased })
	if ts := r.tenant("contoso", ScopeGraph); ts.RateRPS != 10 || ts.LeaseRPS != 10 {
		t.Errorf("state after recovery = %+v", ts)
	}
	r.clock.Advance(time.Second)
	if d := r.elapsed(func() {
		for i := 0; i < 20; i++ {
			r.get(up.URL, 200)
		}
	}); !near(d, time.Second) {
		t.Errorf("20 requests after recovery took %v, want 1s", d)
	}
	if n := strings.Count(r.logs.String(), "rate lease unavailable"); n != 1 {
		t.Errorf("fallback logged %d times, want once", n)
	}
}

func TestFallbackVariants(t *testing.T) {
	up := newUpstream(t, ok)
	addr := up.Listener.Addr().String()

	// No budget source at all.
	r := newHarness(t, func(c *Config) { c.Budgets = nil; c.FallbackRPS = 4 }, "graph=http://"+addr)
	r.get(up.URL, 200)
	if ts := r.tenant("contoso", ScopeGraph); ts.Leased || ts.RateRPS != 4 {
		t.Errorf("no source: %+v", ts)
	}

	// Slack's whole budget is 1 rps, so its fallback cannot be 2.
	r = newHarness(t, nil, "slack=http://"+addr)
	r.leases.set(0, errBoom)
	r.do(call{origin: up.URL, auth: slackAuth, target: "/api/x"})
	if ts := r.g.State().Tenants[0]; ts.RateRPS != 1 {
		t.Errorf("slack fallback: %+v", ts)
	}

	// A lease that was smaller than the fallback stays the ceiling.
	r = newHarness(t, nil, "graph=http://"+addr)
	r.leases.set(0.5, nil)
	r.leases.ttl = 10 * time.Second
	r.get(up.URL, 200)
	r.leases.set(0, errBoom)
	r.clock.Advance(6 * time.Second)
	r.get(up.URL, 200)
	eventually(t, "the failed refresh", func() bool { return !r.tenant("contoso", ScopeGraph).Leased })
	if ts := r.tenant("contoso", ScopeGraph); ts.RateRPS != 0.5 {
		t.Errorf("fallback above the last lease: %+v", ts)
	}

	// A nonsensical lease counts as a failure.
	r = newHarness(t, nil, "graph=http://"+addr)
	r.leases.set(math.Inf(1), nil)
	r.get(up.URL, 200)
	if ts := r.tenant("contoso", ScopeGraph); ts.Leased || ts.RateRPS != 2 {
		t.Errorf("infinite lease: %+v", ts)
	}
}

func TestLeaseRefreshAndExpiry(t *testing.T) {
	r, up := graphHarness(t, 10, nil)
	r.g.refreshInline = false // exercise the background renewal
	r.leases.ttl = 10 * time.Second
	r.get(up.URL, 200)
	if r.leases.calls != 1 {
		t.Fatalf("lease calls = %d", r.leases.calls)
	}
	// Before TTL/2 nothing is refreshed.
	r.clock.Advance(4 * time.Second)
	r.get(up.URL, 200)
	if r.leases.calls != 1 {
		t.Errorf("refreshed early: %d calls", r.leases.calls)
	}
	// At TTL/2 the share has changed (another egress proxy joined).
	r.leases.set(4, nil)
	r.clock.Advance(time.Second)
	r.get(up.URL, 200)
	eventually(t, "the refreshed lease", func() bool { return r.tenant("contoso", ScopeGraph).LeaseRPS == 4 })
	r.clock.Advance(time.Second) // fill the (now smaller) bucket
	if d := r.elapsed(func() {
		for i := 0; i < 8; i++ {
			r.get(up.URL, 200)
		}
	}); !near(d, time.Second) {
		t.Errorf("8 requests at the refreshed 4 rps took %v, want 1s", d)
	}
	if !strings.Contains(r.logs.String(), "rate lease changed") {
		t.Error("no log line about the lease change")
	}

	// A lease that has run out without a refresh is no longer a permission:
	// the state shows the fallback until the refresh lands.
	calls := r.leases.calls
	r.clock.Advance(time.Hour)
	if ts := r.tenant("contoso", ScopeGraph); ts.Leased || ts.RateRPS != 2 {
		t.Errorf("expired lease still in force: %+v", ts)
	}
	r.get(up.URL, 200)
	eventually(t, "the lease after expiry", func() bool { return r.tenant("contoso", ScopeGraph).Leased })
	if r.leases.calls != calls+1 {
		t.Errorf("lease calls after expiry = %d, want %d", r.leases.calls, calls+1)
	}
}

// --- AIMD ---------------------------------------------------------------------------

func throttleOnce(status int, retryAfter string) func(http.ResponseWriter, *http.Request, int) {
	fired := false
	return func(w http.ResponseWriter, _ *http.Request, _ int) {
		if !fired {
			fired = true
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.WriteHeader(status)
			return
		}
		w.WriteHeader(http.StatusOK)
	}
}

func TestTenantThrottle(t *testing.T) {
	r, up := graphHarness(t, 100, func(c *Config) { c.Jitter = func() float64 { return 0.5 } })
	up.setHandler(throttleOnce(429, "7"))

	rec := r.get(up.URL, 429) // passed back unchanged, not retried
	if rec.Header().Get("Retry-After") != "7" || up.count() != 1 {
		t.Errorf("429 not relayed as-is: %v, upstream %d", rec.Header(), up.count())
	}
	ts := r.tenant("contoso", ScopeGraph)
	wantUntil := r.clock.Now().Add(7*time.Second + 350*time.Millisecond) // +10% × jitter 0.5
	if ts.Multiplier != 0.5 || ts.RateRPS != 50 || ts.PausedUntil == nil || !ts.PausedUntil.Equal(wantUntil) {
		t.Errorf("after 429: %+v, want multiplier 0.5 paused until %v", ts, wantUntil)
	}

	// The next request waits out the pause.
	if d := r.elapsed(func() { r.get(up.URL, 200) }); !near(d, 7350*time.Millisecond) {
		t.Errorf("request after 429 waited %v, want 7.35s", d)
	}
	if n := strings.Count(r.logs.String(), "upstream throttled"); n != 1 {
		t.Errorf("throttle logged %d times, want once", n)
	}

	// Additive increase: 0.02 per second of successful traffic.
	for i := 0; i < 5; i++ {
		r.clock.Advance(time.Second)
		r.get(up.URL, 200)
	}
	if m := r.tenant("contoso", ScopeGraph).Multiplier; math.Abs(m-0.62) > 1e-9 {
		// 0.5, +0.02 for the first success after the pause, +0.02 × 5.
		t.Errorf("multiplier after 6 good seconds = %v, want 0.62", m)
	}
	// Idle time does not count as successful traffic.
	r.clock.Advance(10 * time.Minute)
	r.get(up.URL, 200)
	if m := r.tenant("contoso", ScopeGraph).Multiplier; math.Abs(m-0.64) > 1e-9 {
		t.Errorf("multiplier after an idle spell and one success = %v, want 0.64", m)
	}
	for i := 0; i < 30; i++ {
		r.clock.Advance(time.Second)
		r.get(up.URL, 200)
	}
	if ts := r.tenant("contoso", ScopeGraph); ts.Multiplier != 1 || ts.RateRPS != 100 {
		t.Errorf("multiplier did not recover to 1: %+v", ts)
	}
}

func TestMultiplierFloorAndOncePerEvent(t *testing.T) {
	r, up := graphHarness(t, 100, nil)
	up.setHandler(func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(429)
	})
	for i := 0; i < 10; i++ {
		r.get(up.URL, 429)
	}
	if ts := r.tenant("contoso", ScopeGraph); ts.Multiplier != minMultiplier || ts.RateRPS != 10 {
		t.Errorf("after ten separate 429s: %+v, want the floor 0.1", ts)
	}

	// Several requests that were already in flight when the first 429
	// arrived count as one congestion event.
	r2, _ := graphHarness(t, 100, nil)
	rt := &route{scope: ScopeGraph, key: budget.Key{Tenant: "contoso", Scope: ScopeGraph}}
	admitted := r2.clock.Now()
	r2.clock.Advance(100 * time.Millisecond)
	for i := 0; i < 5; i++ {
		r2.g.throttled(rt, admitted, 3*time.Second)
	}
	if ts := r2.tenant("contoso", ScopeGraph); ts.Multiplier != 0.5 {
		t.Errorf("five 429s from one burst: multiplier %v, want 0.5", ts.Multiplier)
	}
	// One admitted after the decrease does count.
	r2.g.throttled(rt, r2.clock.Now(), 3*time.Second)
	if ts := r2.tenant("contoso", ScopeGraph); ts.Multiplier != 0.25 {
		t.Errorf("a later 429: multiplier %v, want 0.25", ts.Multiplier)
	}
}

func TestChannelThrottle(t *testing.T) {
	r, up := graphHarness(t, 100, func(c *Config) { c.Jitter = func() float64 { return 0.5 } })
	up.setHandler(throttleOnce(429, "10"))
	r.post(up, "C1", 429)

	// The channel takes the full pause and half the rate; the tenant gets
	// a 2-second hedge and loses 10 %.
	ts := r.tenant("contoso", ScopeGraph)
	if ts.Multiplier != 0.9 || ts.PausedUntil == nil || !ts.PausedUntil.Equal(r.clock.Now().Add(2100*time.Millisecond)) {
		t.Errorf("tenant after channel 429: %+v", ts)
	}
	cs := r.g.channels["t/c1"]
	if cs.bucket.rate != 0.5 || !cs.pauseUntil.Equal(r.clock.Now().Add(10500*time.Millisecond)) {
		t.Errorf("channel after 429: rate %v, paused until %v", cs.bucket.rate, cs.pauseUntil)
	}

	start := r.clock.Now()
	r.get(up.URL, 200) // tenant traffic resumes after the hedge
	if d := r.clock.Now().Sub(start); !near(d, 2100*time.Millisecond) {
		t.Errorf("tenant traffic waited %v, want 2.1s", d)
	}
	r.post(up, "C2", 200) // another channel is not held back further
	if d := r.clock.Now().Sub(start); !near(d, 2100*time.Millisecond) {
		t.Errorf("another channel waited until +%v, want +2.1s", d)
	}
	r.post(up, "C1", 200) // the throttled channel waits out its own pause
	if d := r.clock.Now().Sub(start); !near(d, 10500*time.Millisecond) {
		t.Errorf("the throttled channel resumed at +%v, want +10.5s", d)
	}

	// At 0.5 rps the channel now takes 2 s per post…
	if d := r.elapsed(func() { r.post(up, "C1", 200); r.post(up, "C1", 200) }); !near(d, 4*time.Second) {
		t.Errorf("two posts at the halved rate took %v, want 4s", d)
	}
	// …and earns +0.5 rps per 30 successful posts, up to the cap.
	for i := 0; i < 27; i++ {
		r.post(up, "C1", 200)
	}
	if cs.bucket.rate != 1.0 {
		t.Errorf("channel rate after 30 successes = %v, want 1.0", cs.bucket.rate)
	}
	for i := 0; i < 30*10; i++ {
		r.post(up, "C1", 200)
	}
	if cs.bucket.rate != 5 {
		t.Errorf("channel rate = %v, want the 5 rps cap", cs.bucket.rate)
	}

	// Repeated throttling bottoms out at the floor.
	up.setHandler(func(w http.ResponseWriter, _ *http.Request, _ int) { w.WriteHeader(429) })
	for i := 0; i < 12; i++ {
		r.post(up, "C1", 429)
	}
	if cs.bucket.rate != channelFloorRPS {
		t.Errorf("channel rate after repeated 429s = %v, want %v", cs.bucket.rate, channelFloorRPS)
	}
}

func TestRetryAfterVariants(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		retryAfter string
		wantPause  time.Duration
		wantMult   float64
	}{
		{"429 without Retry-After", 429, "", 5 * time.Second, 0.5},
		{"429 with garbage Retry-After", 429, "soon", 5 * time.Second, 0.5},
		{"429 with zero", 429, "0", 0, 0.5},
		{"429 with fractional seconds", 429, "1.5", 1500 * time.Millisecond, 0.5},
		{"429 with an absurd value", 429, "999999999", maxRetryAfter, 0.5},
		{"503 with Retry-After", 503, "3", 3 * time.Second, 0.5},
		{"503 without Retry-After", 503, "", 0, 1},
		{"503 with garbage Retry-After", 503, "later", 0, 1},
		{"500", 500, "9", 0, 1},
		{"404", 404, "", 0, 1},
		{"302", 302, "", 0, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, up := graphHarness(t, 100, nil)
			up.setHandler(throttleOnce(c.status, c.retryAfter))
			r.get(up.URL, c.status)
			ts := r.tenant("contoso", ScopeGraph)
			if ts.Multiplier != c.wantMult {
				t.Errorf("multiplier = %v, want %v", ts.Multiplier, c.wantMult)
			}
			if d := r.elapsed(func() { r.get(up.URL, 200) }); !near(d, c.wantPause) {
				t.Errorf("next request waited %v, want %v", d, c.wantPause)
			}
		})
	}

	// HTTP-date form.
	now := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	h := http.Header{"Retry-After": {now.Add(42 * time.Second).Format(http.TimeFormat)}}
	if d, ok := retryAfter(h, now); !ok || d != 42*time.Second {
		t.Errorf("HTTP-date Retry-After = %v, %v", d, ok)
	}
	h.Set("Retry-After", now.Add(-time.Minute).Format(http.TimeFormat))
	if d, ok := retryAfter(h, now); !ok || d != 0 {
		t.Errorf("past HTTP-date Retry-After = %v, %v", d, ok)
	}
	h.Set("Retry-After", "-5")
	if _, ok := retryAfter(h, now); ok {
		t.Error("negative Retry-After accepted")
	}
}

// --- bus ----------------------------------------------------------------------------

func TestProxiesBackOffTogether(t *testing.T) {
	b := bus.NewLocal()
	up := newUpstream(t, ok)
	allow := "graph=http://" + up.Listener.Addr().String()
	a := newHarness(t, func(c *Config) { c.Bus = b; c.Holder = "gov-a" }, allow)
	other := newHarness(t, func(c *Config) { c.Bus = b; c.Holder = "gov-b" }, allow)
	events, stop := b.Listen(wire.SubjectThrottle)
	defer stop()

	// Both serve tenant contoso; b also serves fabrikam.
	a.get(up.URL, 200)
	other.get(up.URL, 200)
	other.do(call{origin: up.URL, auth: entraAuth, tenant: "fabrikam"})
	at := other.clock.Now().UTC().Format(time.RFC3339Nano)

	up.setHandler(throttleOnce(429, "30"))
	a.get(up.URL, 429)

	var ev wire.Throttle
	select {
	case data := <-events:
		if err := json.Unmarshal(data, &ev); err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no throttle event published")
	}
	if ev.Tenant != "contoso" || ev.Scope != ScopeGraph || ev.RetryAfterMs != 30000 || ev.Holder != "gov-a" {
		t.Errorf("event = %+v", ev)
	}
	if _, err := time.Parse(time.RFC3339Nano, ev.At); err != nil {
		t.Errorf("event time %q: %v", ev.At, err)
	}

	eventually(t, "gov-b to apply the pause", func() bool { return other.g.State().RemoteThrottles == 1 })
	tsB := other.tenant("contoso", ScopeGraph)
	if tsB.PausedUntil == nil || !tsB.PausedUntil.Equal(other.clock.Now().Add(30*time.Second)) {
		t.Errorf("gov-b pause = %v", tsB.PausedUntil)
	}
	if tsB.Multiplier != 1 {
		t.Errorf("gov-b multiplier = %v; a remote throttle only pauses", tsB.Multiplier)
	}
	if ts := other.tenant("fabrikam", ScopeGraph); ts.PausedUntil != nil {
		t.Errorf("another tenant was paused: %+v", ts)
	}
	if d := other.elapsed(func() { other.get(up.URL, 200) }); !near(d, 30*time.Second) {
		t.Errorf("gov-b's next request waited %v, want 30s", d)
	}

	// Events that must be ignored: for a tenant gov-b does not serve, for
	// another scope, malformed, and gov-a's own (which it also receives).
	for _, e := range []string{
		`{"tenant":"northwind","scope":"graph","retryAfterMs":60000,"holder":"gov-z","at":"` + at + `"}`,
		`{"tenant":"contoso","scope":"slack","retryAfterMs":60000,"holder":"gov-z","at":"` + at + `"}`,
		`{"tenant":"contoso","scope":"graph","retryAfterMs":-5,"holder":"gov-z","at":"` + at + `"}`,
		// Not current: no timestamp, a stale one, one from the future.
		`{"tenant":"fabrikam","scope":"graph","retryAfterMs":20000,"holder":"gov-z"}`,
		`{"tenant":"fabrikam","scope":"graph","retryAfterMs":20000,"holder":"gov-z","at":"yesterday"}`,
		`{"tenant":"fabrikam","scope":"graph","retryAfterMs":20000,"holder":"gov-z","at":"` + other.clock.Now().Add(-61*time.Second).UTC().Format(time.RFC3339Nano) + `"}`,
		`{"tenant":"fabrikam","scope":"graph","retryAfterMs":20000,"holder":"gov-z","at":"` + other.clock.Now().Add(61*time.Second).UTC().Format(time.RFC3339Nano) + `"}`,
		`not json`,
	} {
		_ = b.Publish(context.Background(), wire.SubjectThrottle, []byte(e))
	}
	// A marker event proves the ones above have been processed.
	_ = b.Publish(context.Background(), wire.SubjectThrottle, []byte(`{"tenant":"fabrikam","scope":"graph","retryAfterMs":1000,"holder":"gov-z","at":"`+at+`"}`))
	eventually(t, "the marker event", func() bool { return other.g.State().RemoteThrottles == 2 })
	if ts := other.tenant("fabrikam", ScopeGraph); !ts.PausedUntil.Equal(other.clock.Now().Add(time.Second)) {
		t.Errorf("a stale or undated event was applied: paused until %v", ts.PausedUntil)
	}
	if len(other.g.State().Tenants) != 2 {
		t.Errorf("an event created a bucket: %+v", other.g.State().Tenants)
	}
	if st := a.g.State(); st.RemoteThrottles != 0 || st.Throttles != 1 {
		t.Errorf("gov-a applied its own event: %+v", st)
	}
	tsA := a.tenant("contoso", ScopeGraph)
	if tsA.Multiplier != 0.5 || !tsA.PausedUntil.Equal(a.clock.Now().Add(30*time.Second)) {
		t.Errorf("gov-a state = %+v", tsA)
	}

	// A remote pause is capped at 30 s, however large the number: nothing
	// authenticates the sender. (A first-hand 429 may pause for longer.)
	_ = b.Publish(context.Background(), wire.SubjectThrottle, []byte(`{"tenant":"fabrikam","scope":"graph","retryAfterMs":99999999999,"holder":"gov-z","at":"`+at+`"}`))
	eventually(t, "the capped event", func() bool { return other.g.State().RemoteThrottles == 3 })
	if ts := other.tenant("fabrikam", ScopeGraph); !ts.PausedUntil.Equal(other.clock.Now().Add(30 * time.Second)) {
		t.Errorf("uncapped remote pause: %v", ts.PausedUntil)
	}
}

func TestChannelThrottlePublishesTheHedge(t *testing.T) {
	b := bus.NewLocal()
	events, stop := b.Listen(wire.SubjectThrottle)
	defer stop()
	up := newUpstream(t, throttleOnce(429, "30"))
	r := newHarness(t, func(c *Config) { c.Bus = b }, "graph=http://"+up.Listener.Addr().String())
	r.post(up, "C1", 429)
	var ev wire.Throttle
	_ = json.Unmarshal(<-events, &ev)
	if ev.RetryAfterMs != 2000 {
		t.Errorf("a channel 429 broadcast a %d ms tenant pause, want the 2000 ms hedge", ev.RetryAfterMs)
	}
}

// --- eviction -----------------------------------------------------------------------

func TestChannelBucketsAreBounded(t *testing.T) {
	r, up := graphHarness(t, 1e6, func(c *Config) { c.MaxChannels = 32 })
	for i := 0; i < 2000; i++ {
		r.post(up, fmt.Sprintf("C%d", i), 200)
	}
	if n := r.g.State().Channels; n != 32 || r.g.channelLRU.Len() != 32 {
		t.Errorf("channel buckets = %d (list %d), want 32", n, r.g.channelLRU.Len())
	}
	// The most recently used survive, the oldest are gone.
	if r.g.channels["t/c1999"] == nil || r.g.channels["t/c0"] != nil {
		t.Error("eviction is not least-recently-used")
	}

	// A paused channel is kept even when it is the oldest: forgetting it
	// would forget the pause.
	up.setHandler(throttleOnce(429, "600"))
	r.post(up, "hot", 429)
	for i := 0; i < 100; i++ {
		r.post(up, fmt.Sprintf("D%d", i), 200)
	}
	if r.g.channels["t/hot"] == nil {
		t.Error("a paused channel was evicted")
	}
	if n := r.g.State().Channels; n > 33 {
		t.Errorf("channel buckets = %d", n)
	}
}

package dashboard

import (
	"math"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/tui/theme"
)

func TestRatioPercentAndBarNeverMisreport(t *testing.T) {
	cases := []struct {
		done, total float64
		final       bool
		pct, cells  int // cells of a 10-wide bar
	}{
		{0, 0, true, 0, 0}, // nothing to do is not a division by zero
		{5, 0, true, 0, 0}, // nor is progress against an unknown total
		{0, 100, true, 0, 0},
		{1, 1000, true, 0, 1}, // any progress shows
		{50, 100, true, 50, 5},
		{999, 1000, true, 99, 9}, // rounds down: 100 means finished
		{1000, 1000, true, 100, 10},
		{1200, 1000, true, 100, 10}, // never past the end
		{1000, 1000, false, 99, 9},  // a provisional total never reads as finished
		{-5, 100, true, 0, 0},
	}
	for _, c := range cases {
		f := ratio(c.done, c.total)
		if f < 0 || f > 1 || math.IsNaN(f) {
			t.Fatalf("ratio(%v, %v) = %v", c.done, c.total, f)
		}
		if got := percent(f, c.final); got != c.pct {
			t.Errorf("percent %v/%v final=%v = %d, want %d", c.done, c.total, c.final, got, c.pct)
		}
		if got := barCells(f, 10, c.final); got != c.cells {
			t.Errorf("barCells %v/%v final=%v = %d, want %d", c.done, c.total, c.final, got, c.cells)
		}
	}
	if barCells(1, 0, true) != 0 || barCells(0.5, 1, true) != 0 || barCells(1, 1, true) != 1 {
		t.Error("degenerate bar widths")
	}
}

func progressWith(posts, settled, batches, batchesDone int) *store.Progress {
	return &store.Progress{Posts: posts, Posted: settled,
		Stages: []store.StageProgress{{Stage: store.StageTransform, Lanes: 10, Done: 0, Batches: batches, BatchesDone: batchesDone}}}
}

func TestPostsAmountWithAMovingDenominator(t *testing.T) {
	// Too little of transform is done to project from.
	a := postsAmount(progressWith(120, 100, 400, 4), false)
	if !a.counting || a.frac != 0 {
		t.Fatalf("early: %+v", a)
	}
	// A quarter of transform done: the total is projected.
	a = postsAmount(progressWith(10_000, 5_000, 400, 100), false)
	if a.counting || !a.estimated || a.total != 40_000 || a.frac != 0.125 {
		t.Fatalf("projected: %+v", a)
	}
	// Load has caught up with transform. The raw ratio would be 100%.
	a = postsAmount(progressWith(10_000, 10_000, 400, 100), false)
	if percent(a.frac, a.final) != 25 {
		t.Fatalf("caught up: %+v", a)
	}
	// Transform finished: the count is exact.
	a = postsAmount(progressWith(40_000, 40_000, 400, 400), true)
	if a.estimated || a.counting || percent(a.frac, a.final) != 100 {
		t.Fatalf("final: %+v", a)
	}
	// The projection never falls below what is already settled.
	a = postsAmount(&store.Progress{Posts: 100, Posted: 90, Failed: 20,
		Stages: []store.StageProgress{{Stage: store.StageTransform, Lanes: 1, Batches: 10, BatchesDone: 10}}}, false)
	if a.total < a.done || a.frac > 1 {
		t.Fatalf("over-settled: %+v", a)
	}
	// As transform proceeds at a steady density the percentage only grows.
	last := -1
	for done := 20; done <= 400; done += 20 {
		a := postsAmount(progressWith(done*100, done*50, 400, done), done == 400)
		if p := percent(a.frac, a.final); p < last {
			t.Fatalf("percentage fell from %d to %d at %d batches", last, p, done)
		} else {
			last = p
		}
	}
	for n, want := range map[int]int{0: 0, 99: 99, 48_649: 49_000, 1_234_567: 1_200_000} {
		if got := roundEstimate(n); got != want {
			t.Errorf("roundEstimate(%d) = %d, want %d", n, got, want)
		}
	}
}

func TestSeriesRates(t *testing.T) {
	var s series
	s.observe(1000, 0) // the first sight of a counter is not a rate
	if s.n != 0 || s.rate() != 0 {
		t.Fatalf("first observation produced a sample: %+v", s)
	}
	s.observe(1100, 1)
	if s.rate() != 100 {
		t.Fatalf("rate %v", s.rate())
	}
	s.observe(1100, 1) // a quiet second pulls the smoothed rate down, gently
	if r := s.rate(); r >= 100 || r < 80 {
		t.Fatalf("smoothed rate %v", r)
	}
	s.observe(900, 1) // a counter going backwards is not a negative rate
	if got := s.samples(1)[0]; got != 0 || s.rate() < 0 {
		t.Fatalf("negative delta: sample %v rate %v", got, s.rate())
	}
	s.observe(1400, 5) // a long gap is averaged over its length
	if got := s.samples(1)[0]; got != 100 {
		t.Fatalf("gap sample %v", got)
	}
	// Steady input converges on the true rate.
	for range 100 {
		s.observe(s.last+250, 1)
	}
	if math.Abs(s.rate()-250) > 0.01 {
		t.Fatalf("did not converge: %v", s.rate())
	}
	// The ring keeps the newest ringSize samples, oldest first.
	var r series
	r.observe(0, 0)
	for i := 1; i <= ringSize+15; i++ {
		r.observe(r.last+float64(i), 1)
	}
	got := r.samples(1000)
	if len(got) != ringSize || got[0] != 16 || got[ringSize-1] != ringSize+15 {
		t.Fatalf("ring: %d samples, first %v last %v", len(got), got[0], got[len(got)-1])
	}
	if tail := r.samples(3); len(tail) != 3 || tail[2] != ringSize+15 || tail[0] != ringSize+13 {
		t.Fatalf("tail %v", tail)
	}
}

func TestETA(t *testing.T) {
	cases := []struct {
		remaining, rate float64
		want            string
	}{
		{0, 0, "<1m"},
		{-10, 5, "<1m"}, // never negative
		{100, 0, "—"},   // nothing moving: no estimate
		{100, -3, "—"},
		{100, math.NaN(), "—"},
		{100, math.Inf(1), "—"},
		{30, 1, "<1m"},
		{61, 1, "2m"}, // rounds up
		{3600, 1, "1h 0m"},
		{5400, 1, "1h 30m"},
		{90_000, 1, "1d 1h"},
		{1e9, 1, "—"}, // more than 99 days is not an estimate
		{1, 1e-12, "—"},
	}
	for _, c := range cases {
		d, ok := eta(c.remaining, c.rate)
		if got := formatETA(d, ok, noValue); got != c.want {
			t.Errorf("eta(%v, %v) = %q, want %q", c.remaining, c.rate, got, c.want)
		}
	}
	if formatETA(-time.Second, true, "-") != "-" || formatETA(100*24*time.Hour, true, "-") != "-" {
		t.Error("formatETA must refuse nonsense")
	}
}

func TestSparkline(t *testing.T) {
	levels := theme.Default().Glyph.Spark
	if got := sparkline(levels, []float64{0, 1, 4, 8}, 6); got != "▁▂▅█  " {
		t.Errorf("sparkline %q", got)
	}
	if got := sparkline(levels, []float64{0, 0, 0}, 3); got != "▁▁▁" {
		t.Errorf("all zero %q", got)
	}
	if got := sparkline(levels, []float64{1, 2, 3, 4, 5}, 2); got != "▇█" {
		t.Errorf("clipped to the newest %q", got)
	}
	if sparkline(levels, nil, 0) != "" || sparkline(levels, nil, 3) != "   " {
		t.Error("empty sparkline")
	}
}

func TestTrackerUsesControllersClock(t *testing.T) {
	view := func(at time.Time, posted int) controller.DashboardView {
		return controller.DashboardView{Run: controller.RunView{ID: "r1", Progress: &store.Progress{Posted: posted}}, At: at}
	}
	var trk tracker
	trk.observe(view(t0, 1000))
	trk.observe(view(t0.Add(2*time.Second), 1400))
	if trk.posts.rate() != 200 {
		t.Fatalf("rate %v", trk.posts.rate())
	}
	// The same instant twice measures nothing and loses nothing.
	trk.observe(view(t0.Add(2*time.Second), 1500))
	trk.observe(view(t0.Add(3*time.Second), 1600))
	if got := trk.posts.samples(1)[0]; got != 200 {
		t.Fatalf("after a repeated instant: %v", got)
	}
	// A clock that goes backwards restarts measurement without a spike.
	n := trk.posts.n
	trk.observe(view(t0.Add(-time.Hour), 1700))
	if trk.posts.n != n {
		t.Fatal("a backwards clock produced a sample")
	}
	trk.observe(view(t0.Add(-time.Hour+time.Second), 1750))
	if got := trk.posts.samples(1)[0]; got != 50 {
		t.Fatalf("after the clock reset: %v", got)
	}
	// Another run starts from nothing.
	other := view(t0, 5)
	other.Run.ID = "r2"
	trk.observe(other)
	if trk.posts.n != 0 || trk.posts.rate() != 0 {
		t.Fatal("rates leaked from one run to another")
	}
}

func TestLeaseTracking(t *testing.T) {
	view := func(sec int, expires time.Time) controller.DashboardView {
		return controller.DashboardView{Run: controller.RunView{ID: "r"}, At: t0.Add(time.Duration(sec) * time.Second),
			Cluster: controller.ClusterView{Workers: []store.Holder{{Holder: "h", LeaseExpires: expires}}}}
	}
	var trk tracker
	trk.observe(view(0, t0.Add(30*time.Second)))
	if trk.leases["h"].renewed || trk.leaseExpiring("h", 30*time.Second) {
		t.Fatal("a lease seen once is neither renewed nor expiring")
	}
	trk.observe(view(10, t0.Add(40*time.Second)))
	if s := trk.leases["h"]; !s.renewed || !s.changedAt.Equal(t0.Add(10*time.Second)) || s.ttl != 30*time.Second {
		t.Fatalf("renewal not seen: %+v", s)
	}
	trk.observe(view(25, t0.Add(40*time.Second)))
	if trk.leaseExpiring("h", 15*time.Second) {
		t.Fatal("half a lease left is healthy")
	}
	if !trk.leaseExpiring("h", 9*time.Second) {
		t.Fatal("under a third of the lease left must warn")
	}
	trk.observe(controller.DashboardView{Run: controller.RunView{ID: "r"}, At: t0.Add(50 * time.Second)})
	if len(trk.leases) != 0 {
		t.Fatal("a worker that left must be forgotten")
	}
}

func TestSorting(t *testing.T) {
	c := func(name string, mut func(*store.ChannelProgress)) store.ChannelProgress {
		x := ch("T", name, 100, 10, 0, 0)
		if mut != nil {
			mut(&x)
		}
		return x
	}
	v := controller.DashboardView{Run: controller.RunView{Status: store.RunRunning, Progress: &store.Progress{
		Stages: []store.StageProgress{{Stage: store.StageTransform, Lanes: 1, Done: 1}},
		Channels: []store.ChannelProgress{
			c("zulu-done", func(x *store.ChannelProgress) { x.Posted = 100 }),
			c("yankee-pending", nil),
			c("xray-failed", func(x *store.ChannelProgress) { x.LaneFailed = true }),
			c("whiskey-active", func(x *store.ChannelProgress) { x.Active = true }),
			c("alpha-done", func(x *store.ChannelProgress) { x.State = store.ChannelCompleted }),
			c("bravo-active", func(x *store.ChannelProgress) { x.Active = true }),
			// A lane that failed outranks the fact that it is still held.
			c("charlie-failed", func(x *store.ChannelProgress) { x.LaneFailed, x.Active = true, true }),
			{SlackID: "Cother", SlackName: "other", Team: "A"},
		},
	}}}
	names := func(b board) []string {
		var out []string
		for _, tm := range b.teams {
			for _, r := range tm.rows {
				out = append(out, r.c.SlackName)
			}
		}
		return out
	}
	equal := func(got, want []string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	byState := []string{"other", "bravo-active", "whiskey-active", "charlie-failed", "xray-failed", "yankee-pending", "alpha-done", "zulu-done"}
	if got := names(buildBoard(v, false, "")); !equal(got, byState) {
		t.Errorf("by state: %v", got)
	}
	byName := []string{"other", "alpha-done", "bravo-active", "charlie-failed", "whiskey-active", "xray-failed", "yankee-pending", "zulu-done"}
	if got := names(buildBoard(v, true, "")); !equal(got, byName) {
		t.Errorf("by name: %v", got)
	}
	b := buildBoard(v, false, "ACTIVE")
	if got := names(b); !equal(got, []string{"bravo-active", "whiskey-active"}) || b.channels != 8 || b.shown != 2 || b.active != 2 {
		t.Errorf("filter: %v (%d of %d, %d active)", got, b.shown, b.channels, b.active)
	}

	// Problems: failed lanes first, then the lane that has retried most.
	v.Run.Progress.Problems = []store.LaneProblem{
		{LaneID: "load:b", Status: string(store.LaneOpen), Attempts: 1},
		{LaneID: "load:c", Status: string(store.LaneFailed), Attempts: 5},
		{LaneID: "load:a", Status: string(store.LaneOpen), Attempts: 4},
		{LaneID: "files:a", Status: string(store.LaneFailed), Attempts: 5},
	}
	var got []string
	for _, p := range buildBoard(v, false, "").problems {
		got = append(got, p.LaneID)
	}
	if !equal(got, []string{"files:a", "load:c", "load:a", "load:b"}) {
		t.Errorf("problems: %v", got)
	}
}

func TestChannelsAreNotDoneWhileTotalsCanGrow(t *testing.T) {
	c := ch("T", "full", 100, 100, 2, 2)
	if classify(c, false, false) != classPending {
		t.Error("a channel with every known post imported is not done while transform is still allocating")
	}
	if classify(c, true, false) != classDone {
		t.Error("once totals are final it is done")
	}
	empty := ch("T", "empty", 0, 0, 0, 0)
	if classify(empty, true, false) != classPending || classify(empty, true, true) != classDone {
		t.Error("an empty channel is done when the run is")
	}
}

func TestThrottleState(t *testing.T) {
	at := t0
	v := controller.DashboardView{At: at, Cluster: controller.ClusterView{Throttles: []controller.ThrottleView{
		{Scope: "graph", At: at.Add(-2 * time.Second), Until: at.Add(12 * time.Second)},
		{Scope: "graph", At: at.Add(-1 * time.Second), Until: at.Add(8 * time.Second)},
		{Scope: "sharepoint", At: at.Add(-4 * time.Minute), Until: at.Add(-3 * time.Minute)},
		{Scope: "slack", At: at.Add(-6 * time.Minute), Until: at.Add(-5 * time.Minute)},
	}}}
	th := throttles(v)
	if len(th.scopes) != 1 || th.scopes[0] != "graph" || th.left != 12*time.Second || th.recent != 3 || th.capped {
		t.Fatalf("%+v", th)
	}
	// The controller's clock decides, not the operator's: ten seconds on, it is clear.
	v.At = at.Add(13 * time.Second)
	if th := throttles(v); len(th.scopes) != 0 || th.left != 0 {
		t.Fatalf("%+v", th)
	}
	if th := throttles(controller.DashboardView{At: at}); th.recent != 0 || len(th.scopes) != 0 {
		t.Fatalf("%+v", th)
	}
}

func TestBackoff(t *testing.T) {
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second}
	for n, w := range want {
		if got := backoff(time.Second, n); got != w {
			t.Errorf("backoff(%d) = %v, want %v", n, got, w)
		}
	}
	if backoff(time.Second, 1000) != maxBackoff || backoff(time.Minute, 1) != maxBackoff {
		t.Error("backoff must be capped")
	}
}

func TestFormatting(t *testing.T) {
	for n, want := range map[int64]string{0: "0 B", 999: "999 B", 1500: "1.5 KB", 3_100_000_000: "3.1 GB", 250_000_000: "250 MB", 5e15: "5000 TB"} {
		if got := byteSize(n); got != want {
			t.Errorf("byteSize(%d) = %q, want %q", n, got, want)
		}
	}
	for r, want := range map[float64]string{0: "0/s", -1: "0/s", 3.24: "3.2/s", 412.6: "413/s", 12345: "12,345/s"} {
		if got := perSecond(r); got != want {
			t.Errorf("perSecond(%v) = %q, want %q", r, got, want)
		}
	}
	for d, want := range map[time.Duration]string{-time.Second: "0s", 3200 * time.Millisecond: "4s", 90 * time.Second: "1m 30s", 2 * time.Hour: "2h 0m"} {
		if got := seconds(d); got != want {
			t.Errorf("seconds(%v) = %q, want %q", d, got, want)
		}
	}
}

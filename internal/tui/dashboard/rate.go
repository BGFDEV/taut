package dashboard

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/store"
)

const (
	// ringSize is how many rate samples a sparkline can draw from: about a
	// minute at the default interval.
	ringSize = 60
	// rateTau is the time constant, in seconds, of the smoothed rate.
	rateTau = 8.0
	// maxETA is the longest estimate worth showing. Beyond it the rate is
	// too close to zero for the number to mean anything.
	maxETA = 99 * 24 * time.Hour
)

// series turns a counter that only grows into rate samples.
type series struct {
	buf    [ringSize]float64
	n      int // samples held
	head   int // where the next sample goes
	ewma   float64
	primed bool
	last   float64
	seen   bool
}

// observe records the counter's value dt seconds after the previous one.
// A counter that went backwards (a retry returned posts to the queue, or
// the run was replaced) contributes a zero sample rather than a negative
// rate.
func (s *series) observe(counter, dt float64) {
	if !s.seen {
		s.last, s.seen = counter, true
		return
	}
	d := max(counter-s.last, 0)
	s.last = counter
	if dt <= 0 {
		return
	}
	inst := d / dt
	s.buf[s.head] = inst
	s.head = (s.head + 1) % ringSize
	s.n = min(s.n+1, ringSize)
	if !s.primed {
		s.ewma, s.primed = inst, true
		return
	}
	s.ewma += (1 - math.Exp(-dt/rateTau)) * (inst - s.ewma)
}

// rebase forgets the last counter value without losing the history, for
// when the clock the samples are measured against jumped.
func (s *series) rebase() { s.seen = false }

// rate is the smoothed rate per second.
func (s *series) rate() float64 {
	if !s.primed || s.ewma < 1e-9 {
		return 0
	}
	return s.ewma
}

// samples returns up to n of the most recent samples, oldest first.
func (s *series) samples(n int) []float64 {
	n = min(n, s.n)
	out := make([]float64, n)
	for i := range n {
		out[i] = s.buf[(s.head-n+i+2*ringSize)%ringSize]
	}
	return out
}

// sparkline draws vals in exactly width cells, oldest first, scaled to the
// largest value shown. It fills from the left; once full, the newest
// sample is at the right-hand edge. Cells with no sample yet are blank.
func sparkline(levels []string, vals []float64, width int) string {
	if width <= 0 || len(levels) == 0 {
		return ""
	}
	if len(vals) > width {
		vals = vals[len(vals)-width:]
	}
	top := 0.0
	for _, v := range vals {
		top = max(top, v)
	}
	var b strings.Builder
	for _, v := range vals {
		i := 0
		if top > 0 && v > 0 {
			i = min(max(int(math.Ceil(v/top*float64(len(levels)-1))), 1), len(levels)-1)
		}
		b.WriteString(levels[i])
	}
	b.WriteString(strings.Repeat(" ", width-len(vals)))
	return b.String()
}

// eta is how long remaining units take at rate per second. ok is false when
// there is no honest answer: nothing is moving, or the answer is absurd.
func eta(remaining, rate float64) (time.Duration, bool) {
	if remaining <= 0 {
		return 0, true
	}
	if !(rate > 0) || math.IsInf(rate, 0) {
		return 0, false
	}
	secs := remaining / rate
	if math.IsNaN(secs) || secs > maxETA.Seconds() {
		return 0, false
	}
	return time.Duration(secs * float64(time.Second)), true
}

// noValue stands in for a number that cannot be given.
const noValue = "—"

// formatETA renders an estimate coarsely, rounding up so it never
// promises too little.
func formatETA(d time.Duration, ok bool, dash string) string {
	if !ok || d < 0 || d > maxETA {
		return dash
	}
	mins := int(math.Ceil(d.Minutes()))
	switch {
	case d < time.Minute:
		return "<1m"
	case mins < 60:
		return fmt.Sprintf("%dm", mins)
	case mins < 24*60:
		return fmt.Sprintf("%dh %dm", mins/60, mins%60)
	}
	return fmt.Sprintf("%dd %dh", mins/(24*60), mins/60%24)
}

// leaseSeen is what successive snapshots reveal about one worker's lease.
type leaseSeen struct {
	expires time.Time
	// changedAt is the controller's clock at the first snapshot that showed the
	// current expiry, which is when the lease was last renewed, to within
	// one poll.
	changedAt time.Time
	// renewed is false until a renewal has actually been observed.
	renewed bool
	// ttl is the longest remaining lifetime seen: the lease's length.
	ttl time.Duration
}

// tracker carries what one snapshot cannot say on its own: rates, and when
// each worker last renewed its lease.
type tracker struct {
	runID  string
	at     time.Time
	posts  series // posts imported
	bytes  series // file bytes uploaded
	files  series // files settled
	leases map[string]leaseSeen
}

func (t *tracker) observe(v controller.DashboardView) {
	if v.Run.ID != t.runID {
		*t = tracker{runID: v.Run.ID}
	}
	t.observeLeases(v)
	if v.At.IsZero() || v.Run.Progress == nil {
		return
	}
	p := v.Run.Progress
	dt := v.At.Sub(t.at).Seconds()
	switch {
	case t.at.IsZero():
		dt = 0
	case dt == 0:
		// The same instant twice: nothing to measure. Leave the counters
		// alone so the change is credited to the next real interval.
		return
	case dt < 0:
		// The controller's clock went backwards (a restart on another host, say).
		// Start measuring again from here.
		t.posts.rebase()
		t.bytes.rebase()
		t.files.rebase()
		dt = 0
	}
	t.at = v.At
	t.posts.observe(float64(p.Posted), dt)
	t.bytes.observe(float64(p.FileBytesDone), dt)
	t.files.observe(float64(filesSettled(p)), dt)
}

func (t *tracker) observeLeases(v controller.DashboardView) {
	next := make(map[string]leaseSeen, len(v.Cluster.Workers))
	for _, h := range v.Cluster.Workers {
		left := h.LeaseExpires.Sub(v.At)
		seen, ok := t.leases[h.Holder]
		switch {
		case !ok:
			seen = leaseSeen{expires: h.LeaseExpires, changedAt: v.At, ttl: left}
		case !h.LeaseExpires.Equal(seen.expires):
			seen.expires, seen.changedAt, seen.renewed = h.LeaseExpires, v.At, true
		}
		seen.ttl = max(seen.ttl, left)
		next[h.Holder] = seen
	}
	t.leases = next
}

// leaseWarnFloor is the remaining lifetime below which a lease is always
// called out, however short its full length.
const leaseWarnFloor = 5 * time.Second

// leaseExpiring reports whether a lease has gone unrenewed for long enough
// to worry about. Workers renew every third of the lease's length, so a
// healthy lease always has more than half of it left.
func (t *tracker) leaseExpiring(holder string, left time.Duration) bool {
	return left < max(t.leases[holder].ttl/3, leaseWarnFloor)
}

func filesSettled(p *store.Progress) int {
	return p.FilesUploaded + p.FilesSkipped + p.FilesFailed
}

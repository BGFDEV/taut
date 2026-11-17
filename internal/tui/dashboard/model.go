package dashboard

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/preflight"
	"github.com/bgfdev/taut/internal/store"
)

// Stopped reports whether a run has reached a state it will not leave on
// its own: the dashboard shows a verdict and stops animating.
func Stopped(s store.RunStatus) bool {
	return s == store.RunCompleted || s == store.RunFailed || s == store.RunCancelled || s == store.RunFinalized
}

// --- bar math ----------------------------------------------------------------

// ratio is done/total as a fraction in [0, 1]. A total of zero is zero
// progress, not a division.
func ratio(done, total float64) float64 {
	if !(total > 0) || !(done > 0) {
		return 0
	}
	return min(done/total, 1)
}

// percent renders a fraction as a whole percentage, rounded down so that
// 100 means finished. While the total is still provisional it stops at 99.
func percent(frac float64, final bool) int {
	p := int(math.Floor(min(max(frac, 0), 1)*100 + 1e-9))
	if !final || frac < 1 {
		p = min(p, 99)
	}
	return p
}

// barCells is how many of width cells to fill. Any progress at all shows
// one cell; the last cell is filled only when the work is finished and the
// total is final.
func barCells(frac float64, width int, final bool) int {
	if width <= 0 {
		return 0
	}
	frac = min(max(frac, 0), 1)
	n := int(math.Floor(frac*float64(width) + 1e-9))
	if frac > 0 && n == 0 {
		n = 1
	}
	if !final || frac < 1 {
		n = min(n, width-1)
	}
	return max(n, 0)
}

// amount is one overall measure: how much is done of how much.
type amount struct {
	done, total int
	// estimated is true when total is a projection, not a count.
	estimated bool
	// counting is true when the total is not known well enough even to
	// project.
	counting bool
	// final is true when total can no longer grow.
	final bool
	frac  float64
}

// estimateBasis is the share of transform batches that must be done before
// the final number of posts is projected from them.
const estimateBasis = 0.05

// postsAmount measures the run's posts. See the package comment for how a
// total that is still growing is handled.
func postsAmount(p *store.Progress, final bool) amount {
	done := p.Posted + p.Failed + p.Held
	a := amount{done: done, total: max(p.Posts, done), final: final}
	if !final {
		tr, ok := stage(p, store.StageTransform)
		switch {
		case !ok || tr.Batches <= 0 || tr.BatchesDone <= 0 ||
			float64(tr.BatchesDone) < estimateBasis*float64(tr.Batches):
			a.counting = true
		case tr.BatchesDone < tr.Batches:
			est := float64(p.Posts) * float64(tr.Batches) / float64(tr.BatchesDone)
			a.total, a.estimated = max(int(math.Round(est)), a.total), true
		}
	}
	if !a.counting {
		a.frac = ratio(float64(a.done), float64(a.total))
	}
	return a
}

func filesAmount(p *store.Progress, final bool) amount {
	done := filesSettled(p)
	a := amount{done: done, total: max(p.Files, done), final: final}
	a.frac = ratio(float64(a.done), float64(a.total))
	return a
}

// roundEstimate keeps two significant figures, so a projected total does
// not flicker in its low digits from one snapshot to the next.
func roundEstimate(n int) int {
	if n < 100 {
		return n
	}
	mag := math.Pow(10, math.Floor(math.Log10(float64(n)))-1)
	return int(math.Round(float64(n)/mag) * mag)
}

func stage(p *store.Progress, name string) (store.StageProgress, bool) {
	for _, s := range p.Stages {
		if s.Stage == name {
			return s, true
		}
	}
	return store.StageProgress{}, false
}

// --- channels ------------------------------------------------------------------

// class orders channels by how much attention they deserve.
type class int

const (
	classActive class = iota
	classFailed
	classPending
	classDone
)

type chanRow struct {
	c       store.ChannelProgress
	class   class
	settled int
	frac    float64
	// attention is true for a finished channel that left posts behind.
	attention bool
}

func classify(c store.ChannelProgress, final, runDone bool) class {
	settled := c.Posted + c.Failed + c.Held
	switch {
	case c.LaneFailed:
		return classFailed
	case c.Active:
		return classActive
	case c.State == store.ChannelCompleted, c.State == store.ChannelSkipped, runDone,
		final && c.Posts > 0 && settled >= c.Posts && c.FilesDone >= c.Files:
		return classDone
	}
	return classPending
}

type team struct {
	name string
	rows []chanRow
	done int
	// chats is true for the group of direct messages, which belong to no
	// team. It is listed last.
	chats bool
}

// maxChatName is the widest a direct message's label makes the name
// column; a longer one is cut.
const maxChatName = 36

// directMessages is the heading of the group of direct messages.
const directMessages = "Direct messages"

// display is the name a row is shown and sorted by: "#general" for a
// channel, the names of its participants for a direct message.
func display(c store.ChannelProgress) string {
	if store.IsChatKind(c.Kind) {
		return c.TargetName
	}
	return "#" + c.SlackName
}

// chanLine is one line of the channels pane: a team heading (row < 0) or a
// channel.
type chanLine struct {
	team, row int
}

// board is everything derived from one snapshot, ready to draw.
type board struct {
	v controller.DashboardView
	p store.Progress
	// final is true once no total can grow any more.
	final   bool
	stopped bool
	posts   amount
	files   amount
	stages  []store.StageProgress

	teams    []team
	lines    []chanLine
	channels int // before filtering
	shown    int
	active   int
	failed   int
	done     int
	// Column measurements, taken over every channel so filtering and
	// scrolling never move a column.
	nameW, postsW, filesW int
	// extrasW is the widest "12 failed · 3 held" any channel needs.
	extrasW int

	problems []store.LaneProblem
	names    map[string]string // Slack channel ID to name
}

// stageOrder is the pipeline order of the stages strip.
var stageOrder = []string{
	store.StagePlan, store.StageExtract, store.StageTransform, store.StageTeam,
	store.StageFiles, store.StageLoad, store.StageFinalize, store.StageMembers,
}

func buildBoard(v controller.DashboardView, byName bool, filter string) board {
	b := board{v: v, stopped: Stopped(v.Run.Status)}
	if v.Run.Progress != nil {
		b.p = *v.Run.Progress
	}
	p := &b.p
	runDone := v.Run.Status == store.RunCompleted || v.Run.Status == store.RunFinalized || v.Run.Status == store.RunFinalizing
	tr, ok := stage(p, store.StageTransform)
	b.final = runDone || (ok && tr.Lanes > 0 && tr.Done == tr.Lanes)
	b.posts, b.files = postsAmount(p, b.final), filesAmount(p, b.final)

	seen := map[string]bool{}
	for _, name := range stageOrder {
		if s, ok := stage(p, name); ok && s.Lanes > 0 {
			b.stages, seen[name] = append(b.stages, s), true
		}
	}
	for _, s := range p.Stages {
		if !seen[s.Stage] && s.Lanes > 0 {
			b.stages = append(b.stages, s)
		}
	}

	b.channels = len(p.Channels)
	b.names = make(map[string]string, len(p.Channels))
	needle := strings.ToLower(strings.TrimSpace(filter))
	index := map[string]int{}
	maxPosts, maxFiles := 0, 0
	for _, c := range p.Channels {
		chat := store.IsChatKind(c.Kind)
		if !chat {
			// A problem with a direct message's lane is named by the lane,
			// which carries the conversation's ID and nobody's name.
			b.names[c.SlackID] = c.SlackName
		}
		// The names of a large group's participants run long; they do not
		// get to push every other column off the pane.
		if w := len([]rune(display(c))); chat {
			b.nameW = max(b.nameW, min(w, maxChatName))
		} else {
			b.nameW = max(b.nameW, w)
		}
		maxPosts, maxFiles = max(maxPosts, c.Posts, c.Posted), max(maxFiles, c.Files, c.FilesDone)
		row := chanRow{c: c, class: classify(c, b.final, runDone), settled: c.Posted + c.Failed + c.Held}
		row.frac = ratio(float64(row.settled), float64(c.Posts))
		row.attention = row.class == classDone && (c.Failed+c.Held > 0 || c.State == store.ChannelSkipped)
		switch row.class {
		case classActive:
			b.active++
		case classFailed:
			b.failed++
		case classDone:
			b.done++
		}
		extra := 0
		if c.Failed > 0 {
			extra += len(comma(c.Failed)) + len(" failed")
		}
		if c.Held > 0 {
			if extra > 0 {
				extra += 3
			}
			extra += len(comma(c.Held)) + len(" held")
		}
		b.extrasW = max(b.extrasW, extra)
		if needle != "" && !strings.Contains(strings.ToLower(c.SlackName+"\n"+c.TargetName+"\n"+c.Team), needle) {
			continue
		}
		group, key := team{name: c.Team}, "team\x00"+c.Team
		if chat {
			group, key = team{name: directMessages, chats: true}, "chats"
		}
		i, ok := index[key]
		if !ok {
			i = len(b.teams)
			index[key] = i
			b.teams = append(b.teams, group)
		}
		b.teams[i].rows = append(b.teams[i].rows, row)
		if row.class == classDone {
			b.teams[i].done++
		}
		b.shown++
	}
	b.postsW = 2*len(comma(maxPosts)) + 1
	b.filesW = max(2*len(comma(maxFiles))+1, len("files"))

	sort.SliceStable(b.teams, func(i, j int) bool {
		if b.teams[i].chats != b.teams[j].chats {
			return b.teams[j].chats
		}
		return b.teams[i].name < b.teams[j].name
	})
	for ti := range b.teams {
		rows := b.teams[ti].rows
		sort.SliceStable(rows, func(i, j int) bool {
			if !byName && rows[i].class != rows[j].class {
				return rows[i].class < rows[j].class
			}
			if x, y := display(rows[i].c), display(rows[j].c); x != y {
				return x < y
			}
			return rows[i].c.SlackID < rows[j].c.SlackID
		})
		b.lines = append(b.lines, chanLine{ti, -1})
		for ri := range rows {
			b.lines = append(b.lines, chanLine{ti, ri})
		}
	}

	b.problems = append(b.problems, p.Problems...)
	sort.SliceStable(b.problems, func(i, j int) bool {
		x, y := b.problems[i], b.problems[j]
		if fx, fy := isFailed(x), isFailed(y); fx != fy {
			return fx
		}
		if x.Attempts != y.Attempts {
			return x.Attempts > y.Attempts
		}
		return x.LaneID < y.LaneID
	})
	return b
}

func isFailed(p store.LaneProblem) bool { return p.Status == string(store.LaneFailed) }

func (b *board) failedLanes() int {
	n := 0
	for _, p := range b.problems {
		if isFailed(p) {
			n++
		}
	}
	return n
}

// subject names a lane for people: its stage and, when the lane belongs to
// a channel, the channel's name.
func (b *board) subject(laneID string) string {
	stageName, key, ok := strings.Cut(laneID, ":")
	if !ok {
		return laneID
	}
	if name, ok := b.names[key]; ok && name != "" {
		return stageName + " #" + name
	}
	return laneID
}

// elapsed is how long the run has taken, on the controller's clock.
func (b *board) elapsed() time.Duration {
	end := b.v.At
	if b.stopped && b.v.Run.FinishedAt != nil {
		end = *b.v.Run.FinishedAt
	}
	if end.IsZero() || b.v.Run.CreatedAt.IsZero() {
		return 0
	}
	return max(end.Sub(b.v.Run.CreatedAt), 0)
}

// --- cluster -----------------------------------------------------------------------

// throttleWindow is how far back the throttle count looks.
const throttleWindow = 5 * time.Minute

// throttleState summarizes the egress proxies' throttle events as of the
// snapshot: which scopes are paused, for how much longer, and how many
// events there were in the window.
type throttleState struct {
	scopes []string
	left   time.Duration
	recent int
	// capped is true when the list the controller keeps was full, so recent is a
	// lower bound.
	capped bool
}

// maxThrottles is the most throttle events the controller reports.
const maxThrottles = 50

func throttles(v controller.DashboardView) throttleState {
	var s throttleState
	seen := map[string]bool{}
	for _, t := range v.Cluster.Throttles {
		if left := t.Until.Sub(v.At); left > 0 {
			s.left = max(s.left, left)
			if !seen[t.Scope] {
				seen[t.Scope] = true
				s.scopes = append(s.scopes, t.Scope)
			}
		}
		if age := v.At.Sub(t.At); age <= throttleWindow {
			s.recent++
		}
	}
	sort.Strings(s.scopes)
	s.capped = s.recent >= maxThrottles
	return s
}

// --- formatting ------------------------------------------------------------------

func comma(n int) string { return preflight.Comma(int64(n)) }

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return comma(n) + " " + many
}

// byteSize renders a size in decimal units, as the rest of Taut does.
func byteSize(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d B", n)
	}
	v := float64(n)
	for _, u := range []string{"KB", "MB", "GB", "TB"} {
		v /= 1000
		if v < 1000 || u == "TB" {
			if v >= 100 {
				return fmt.Sprintf("%.0f %s", v, u)
			}
			return fmt.Sprintf("%.1f %s", v, u)
		}
	}
	return ""
}

// perSecond renders a count rate.
func perSecond(r float64) string {
	switch {
	case r <= 0:
		return "0/s"
	case r < 10:
		return fmt.Sprintf("%.1f/s", r)
	}
	return comma(int(math.Round(r))) + "/s"
}

// seconds renders a short duration for "12s left" and "3s ago".
func seconds(d time.Duration) string {
	d = max(d, 0)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(math.Ceil(d.Seconds())))
	case d < time.Hour:
		return fmt.Sprintf("%dm %ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
}

func rps(x float64) string {
	if x >= 1e6 || x < 0 {
		// Not a rate anyone configured; keep it from filling the row.
		return fmt.Sprintf("%.1e", x)
	}
	if x == math.Trunc(x) {
		return fmt.Sprintf("%.0f", x)
	}
	return fmt.Sprintf("%.1f", x)
}

// shortID trims a long opaque identifier for display.
func shortID(id string) string {
	if len(id) > 12 {
		return ansi.Truncate(id, 8, "")
	}
	return id
}

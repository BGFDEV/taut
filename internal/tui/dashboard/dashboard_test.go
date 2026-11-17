package dashboard

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/testutil"
	"github.com/bgfdev/taut/internal/tui/theme"
)

func running(t *testing.T, w, h int) *harness {
	t.Helper()
	r := newHarness(t, &fakeSource{views: script(40, midRun)}, theme.Default(), w, h)
	r.poll(8)
	return r
}

func throttled(step int) controller.DashboardView {
	v := midRun(step)
	tenant := "9f1c2d3e-0000-4000-8000-acme"
	v.Cluster.Throttles = []controller.ThrottleView{
		{Tenant: tenant, Scope: "graph", Holder: "a", At: v.At.Add(-3 * time.Second), Until: t0.Add(20 * time.Second)},
		{Tenant: tenant, Scope: "graph", Holder: "b", At: v.At.Add(-90 * time.Second), Until: v.At.Add(-60 * time.Second)},
		{Tenant: tenant, Scope: "sharepoint", Holder: "a", At: v.At.Add(-4 * time.Minute), Until: v.At.Add(-3 * time.Minute)},
		{Tenant: tenant, Scope: "graph", Holder: "a", At: v.At.Add(-9 * time.Minute), Until: v.At.Add(-8 * time.Minute)},
	}
	return v
}

// leaseLapsing has one worker that renewed for a while and then stopped.
func leaseLapsing(step int) controller.DashboardView {
	v := midRun(step)
	renewedAt := min(step, 3)
	v.Cluster.Workers[1].LeaseExpires = t0.Add(time.Duration(renewedAt)*time.Second + 30*time.Second)
	return v
}

func completed(int) controller.DashboardView {
	v := midRun(0)
	p := v.Run.Progress
	for i := range p.Stages {
		p.Stages[i].Done, p.Stages[i].Failed, p.Stages[i].Leased = p.Stages[i].Lanes, 0, 0
		p.Stages[i].BatchesDone = p.Stages[i].Batches
	}
	p.Posted, p.Failed, p.Held = 0, 0, 0
	for i := range p.Channels {
		c := &p.Channels[i]
		c.Active, c.LaneFailed, c.LastError = false, false, ""
		c.Posted, c.FilesDone = c.Posts-c.Failed-c.Held, c.Files
		p.Posted, p.Failed, p.Held = p.Posted+c.Posted, p.Failed+c.Failed, p.Held+c.Held
	}
	p.FilesUploaded, p.FileBytesDone, p.Problems = p.Files, p.FileBytes, nil
	done := t0.Add(31 * time.Minute)
	v.Run.Status, v.Run.FinishedAt = store.RunCompleted, &done
	v.Cluster.Workers, v.Cluster.Budgets = nil, nil
	v.At = t0.Add(40 * time.Minute)
	return v
}

func planning(step int) controller.DashboardView {
	return controller.DashboardView{
		Run: controller.RunView{ID: "01JCM4Q8ZK7W3N5T2R6V9XYB0D", Name: "acme-migration", Status: store.RunQueued,
			CreatedAt: t0.Add(-3 * time.Second), Progress: &store.Progress{}},
		At: t0.Add(time.Duration(step) * time.Second),
	}
}

func logEntries() []store.LogEntry {
	lane := "load:Cincidents"
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	return []store.LogEntry{
		{Stream: lane, Seq: 1, At: at(-400), Op: "channel.create", SourceID: "Cincidents", TargetID: "19:8a1f@thread.tacv2", Outcome: "ok"},
		{Stream: lane, Seq: 2, At: at(-398), Op: "channel.start_migration", SourceID: "Cincidents", TargetID: "19:8a1f@thread.tacv2", Outcome: "ok"},
		{Stream: lane, Seq: 3, At: at(-120), Op: "message.import", SourceID: "Cincidents/1699000000.000100", TargetID: "1699000000000", Outcome: "ok"},
		{Stream: lane, Seq: 4, At: at(-119), Op: "message.import", SourceID: "Cincidents/1699000041.000200", TargetID: "1699000041000", Outcome: "ok"},
		{Stream: lane, Seq: 5, At: at(-118), Op: "message.fail", SourceID: "Cincidents/1699000050.000300", Outcome: "error",
			Detail: "413 Request Entity Too Large: the message body is 4.2 MB after its inline images were embedded; the limit is 4 MB"},
		{Stream: lane, Seq: 6, At: at(-60), Op: "message.import", SourceID: "Cincidents/1699000077.000400", Outcome: "error",
			Detail: "403 Forbidden: Teamwork.Migrate.All is not granted"},
		{Stream: lane, Seq: 7, At: at(-30), Op: "message.import", SourceID: "Cincidents/1699000077.000400", Outcome: "error",
			Detail: "403 Forbidden: Teamwork.Migrate.All is not granted"},
		{Stream: "load:Cgeneral", Seq: 1, At: at(-10), Op: "message.import", SourceID: "Cgeneral/1", Outcome: "ok"},
	}
}

func TestSnapshots(t *testing.T) {
	cases := []struct {
		name  string
		build func(t *testing.T) *harness
	}{
		{"running_wide", func(t *testing.T) *harness { return running(t, 140, 40) }},
		{"running_narrow", func(t *testing.T) *harness { return running(t, 80, 24) }},
		{"running_tiny", func(t *testing.T) *harness { return running(t, 60, 16) }},
		{"running_ascii", func(t *testing.T) *harness {
			r := newHarness(t, &fakeSource{views: script(40, midRun)}, theme.New(theme.Options{Dark: true, Unicode: false}), 100, 30)
			r.poll(8)
			return r
		}},
		{"throttled", func(t *testing.T) *harness {
			r := newHarness(t, &fakeSource{views: script(40, throttled)}, theme.Default(), 80, 24)
			r.poll(8)
			r.press("2")
			return r
		}},
		{"lease_expiring", func(t *testing.T) *harness {
			r := newHarness(t, &fakeSource{views: script(40, leaseLapsing)}, theme.Default(), 80, 24)
			r.poll(29)
			r.press("tab")
			return r
		}},
		{"problems_narrow", func(t *testing.T) *harness {
			r := running(t, 80, 24)
			r.press("3", "down")
			return r
		}},
		{"problem_detail", func(t *testing.T) *harness {
			r := running(t, 140, 40)
			r.src.entries = logEntries()
			r.press("3", "enter")
			return r
		}},
		{"problem_detail_narrow", func(t *testing.T) *harness {
			r := running(t, 80, 24)
			r.src.entries = logEntries()
			r.press("3", "enter", "pgdown")
			return r
		}},
		// Direct messages are a group of their own, after the teams, shown
		// by their participants.
		{"direct_messages", func(t *testing.T) *harness {
			r := newHarness(t, &fakeSource{views: script(40, withChats)}, theme.Default(), 150, 34)
			r.poll(8)
			return r
		}},
		{"problem_detail_audit_error", func(t *testing.T) *harness {
			r := running(t, 80, 24)
			r.src.logErr = errors.New("connection refused")
			r.press("3", "enter")
			return r
		}},
		{"retry_confirm", func(t *testing.T) *harness {
			r := running(t, 80, 24)
			r.press("3", "r")
			return r
		}},
		{"retry_done", func(t *testing.T) *harness {
			r := running(t, 80, 24)
			r.src.retryN = 1
			r.press("3", "r", "y")
			return r
		}},
		{"stale", func(t *testing.T) *harness {
			r := running(t, 80, 24)
			r.src.dashboardErr = errors.New("dial tcp 127.0.0.1:7070: connect: connection refused")
			r.poll(5)
			return r
		}},
		{"no_snapshot", func(t *testing.T) *harness {
			return newHarness(t, &fakeSource{dashboardErr: errors.New("dial tcp 127.0.0.1:7070: connect: connection refused")}, theme.Default(), 80, 24)
		}},
		{"completed", func(t *testing.T) *harness {
			r := newHarness(t, &fakeSource{views: script(1, completed)}, theme.Default(), 140, 40)
			return r
		}},
		{"completed_narrow", func(t *testing.T) *harness {
			return newHarness(t, &fakeSource{views: script(1, completed)}, theme.Default(), 80, 24)
		}},
		{"planning", func(t *testing.T) *harness {
			r := newHarness(t, &fakeSource{views: script(5, planning)}, theme.Default(), 80, 24)
			r.poll(3)
			return r
		}},
		{"channels_600_scrolled", func(t *testing.T) *harness {
			r := newHarness(t, &fakeSource{views: script(40, manyChannels(600))}, theme.Default(), 140, 40)
			r.poll(4)
			r.press("pgdown", "pgdown", "pgdown", "down")
			return r
		}},
		{"channels_600_bottom", func(t *testing.T) *harness {
			r := newHarness(t, &fakeSource{views: script(40, manyChannels(600))}, theme.Default(), 80, 24)
			r.poll(4)
			r.press("G")
			return r
		}},
		{"filter", func(t *testing.T) *harness {
			r := running(t, 80, 24)
			r.press("/", "i", "n")
			return r
		}},
		{"sort_by_name", func(t *testing.T) *harness {
			r := running(t, 80, 24)
			r.press("o")
			return r
		}},
		{"help", func(t *testing.T) *harness {
			r := running(t, 80, 24)
			r.press("?")
			return r
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := c.build(t)
			out := r.frame()
			testutil.Golden(t, c.name, out)
		})
	}
}

// Every frame, at every size and in every state, is exactly the size of
// the terminal.
func TestFramesFitTheTerminal(t *testing.T) {
	sizes := [][2]int{{60, 14}, {60, 16}, {72, 20}, {80, 24}, {109, 30}, {110, 30}, {140, 40}, {220, 60}}
	for _, sz := range sizes {
		r := newHarness(t, &fakeSource{views: script(40, manyChannels(120)), entries: logEntries()}, theme.Default(), sz[0], sz[1])
		check := func(what string) {
			t.Helper()
			lines := strings.Split(r.m.View().Content, "\n")
			if len(lines) != sz[1] {
				t.Errorf("%dx%d %s: %d rows", sz[0], sz[1], what, len(lines))
			}
			for i, l := range lines {
				if w := ansi.StringWidth(l); w > sz[0] {
					t.Errorf("%dx%d %s: row %d is %d wide: %q", sz[0], sz[1], what, i, w, ansi.Strip(l))
				}
			}
		}
		r.poll(3)
		check("channels")
		r.press("G")
		check("bottom")
		r.press("tab")
		check("cluster")
		r.press("tab", "r")
		check("confirm")
		r.press("n", "enter")
		check("detail")
		r.press("G")
		check("detail bottom")
	}
}

// Columns hold still from one snapshot to the next: only digits change.
func TestRowsDoNotShiftBetweenSnapshots(t *testing.T) {
	r := running(t, 140, 40)
	shape := func() []int {
		var cols []int
		for _, l := range strings.Split(r.frame(), "\n") {
			if strings.Contains(l, "#general") || strings.HasPrefix(strings.TrimSpace(l), "Posts") {
				cols = append(cols, strings.Index(l, "%"), strings.Index(l, "/"))
			}
		}
		return cols
	}
	before := shape()
	if len(before) != 4 {
		t.Fatalf("did not find the rows to compare: %v", before)
	}
	for range 12 {
		r.poll(1)
		after := shape()
		for i := range before {
			if before[i] != after[i] {
				t.Fatalf("columns moved: %v then %v", before, after)
			}
		}
	}
}

func TestPollingBacksOffAndRecovers(t *testing.T) {
	r := running(t, 80, 24)
	r.timers = nil
	r.src.dashboardErr = errors.New("boom")
	r.poll(7)
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second}
	if len(r.timers) != len(want) {
		t.Fatalf("timers %v", r.timers)
	}
	for i := range want {
		if r.timers[i] != want[i] {
			t.Fatalf("backoff %v, want %v", r.timers, want)
		}
	}
	if !strings.Contains(r.frame(), "#general") || !strings.Contains(r.frame(), "stale 7s") {
		t.Fatalf("a failed poll must keep the last snapshot and mark it stale:\n%s", r.frame())
	}
	r.src.dashboardErr = nil
	r.timers = nil
	r.poll(1)
	if strings.Contains(r.frame(), "stale") || r.timers[0] != time.Second {
		t.Fatalf("did not recover: timers %v\n%s", r.timers, r.frame())
	}
}

func TestStoppedRunPollsSlowlyAndStopsAnimating(t *testing.T) {
	r := newHarness(t, &fakeSource{views: script(1, completed)}, theme.Default(), 80, 24)
	if r.timers[len(r.timers)-1] != idleFactor*time.Second {
		t.Fatalf("timers %v", r.timers)
	}
	r.send(frameMsg{}) // the frame that was already scheduled
	if r.s.animating() || r.s.ticking {
		t.Fatal("a stopped run must not keep the spinner ticking")
	}
}

func TestRetryFlow(t *testing.T) {
	r := running(t, 80, 24)
	r.src.retryN = 1
	r.press("3", "r")
	if !r.s.CapturingInput() || r.src.retries != 0 {
		t.Fatal("r must ask before retrying")
	}
	r.press("n")
	if r.src.retries != 0 || r.s.confirm {
		t.Fatal("n must cancel")
	}
	r.press("r", "q")
	if r.src.retries != 0 {
		t.Fatal("any key but y or enter must cancel")
	}
	r.press("r", "enter")
	if r.src.retries != 1 || !strings.Contains(r.frame(), "Returned 1 lane to the queue") {
		t.Fatalf("retry not reported:\n%s", r.frame())
	}
	r.src.retryErr = errors.New("controller: 503")
	r.press("r", "y")
	if !strings.Contains(r.frame(), "Retry failed") {
		t.Fatalf("retry error not reported:\n%s", r.frame())
	}
	// With nothing failed there is nothing to confirm.
	r2 := newHarness(t, &fakeSource{views: script(1, completed)}, theme.Default(), 80, 24)
	r2.press("r")
	if r2.s.confirm || !strings.Contains(r2.frame(), "Nothing to retry") {
		t.Fatalf("expected a notice:\n%s", r2.frame())
	}
}

func TestDetailQueriesTheLaneErrorsFirst(t *testing.T) {
	r := running(t, 140, 40)
	r.src.entries = logEntries()
	r.press("3", "enter")
	if len(r.src.filters) != 2 {
		t.Fatalf("filters %+v", r.src.filters)
	}
	first, second := r.src.filters[0], r.src.filters[1]
	if first.Stream != "load:Cincidents" || first.Outcome != "error" {
		t.Fatalf("first query %+v", first)
	}
	if second.Stream != "load:Cincidents" || second.Outcome != "" || second.Since.IsZero() {
		t.Fatalf("second query %+v", second)
	}
	if got := r.s.detail.errs; len(got) != 3 || got[0].Seq != 7 {
		t.Fatalf("errors must be newest first: %+v", got)
	}
	for _, e := range r.s.detail.recent {
		if e.Outcome == "error" || e.Stream != "load:Cincidents" {
			t.Fatalf("recent activity holds %+v", e)
		}
	}
	r.press("esc")
	if r.s.detail != nil || !strings.Contains(r.frame(), "Channels") {
		t.Fatal("esc must return to the dashboard")
	}
}

func TestFilterAndFocusKeys(t *testing.T) {
	r := running(t, 80, 24)
	r.press("/", "w", "i")
	if !r.s.CapturingInput() || r.s.board.shown != 1 {
		t.Fatalf("filter %q shows %d", r.s.filter, r.s.board.shown)
	}
	r.press("q") // typed into the filter, not a quit
	if r.s.filter != "wiq" {
		t.Fatalf("filter %q", r.s.filter)
	}
	r.press("backspace", "enter")
	if r.s.CapturingInput() || r.s.filter != "wi" {
		t.Fatalf("enter must keep the filter: %q", r.s.filter)
	}
	r.press("esc")
	if r.s.filter != "" || r.s.board.shown != 8 {
		t.Fatal("esc must clear the filter")
	}
	for i, want := range []pane{paneCluster, paneProblems, paneChannels} {
		r.press("tab")
		if r.s.focus != want {
			t.Fatalf("tab %d: focus %d", i, r.s.focus)
		}
	}
	r.press("3")
	if r.s.focus != paneProblems {
		t.Fatal("3 must focus problems")
	}
}

func BenchmarkView600Channels(b *testing.B) {
	r := newHarness(&testing.T{}, &fakeSource{views: script(40, manyChannels(600))}, theme.Default(), 140, 40)
	r.poll(4)
	for b.Loop() {
		_ = r.m.View()
	}
}

func BenchmarkIngest600Channels(b *testing.B) {
	r := newHarness(&testing.T{}, &fakeSource{views: script(40, manyChannels(600))}, theme.Default(), 140, 40)
	v := manyChannels(600)(5)
	for b.Loop() {
		r.s.ingest(v)
	}
}

// withChats is midRun with two direct messages and a group DM: no team,
// and the names of the participants where a channel has its name.
func withChats(step int) controller.DashboardView {
	v := midRun(step)
	p := v.Run.Progress
	chat := func(id, kind, slackName, label string, posts, posted int) store.ChannelProgress {
		return store.ChannelProgress{SlackID: id, SlackName: slackName, Kind: kind, TargetName: label,
			State: store.ChannelMigrating, Posts: posts, Posted: posted}
	}
	one := chat("D0AB12CD3", store.KindDM, "", "Ada Abbott, Ben Banerjee", 820, 410+step*3)
	one.Active = true
	chats := []store.ChannelProgress{
		one,
		chat("D0EF45GH6", store.KindDM, "", "Dev Dubois, Елена 🌻", 64, 64),
		chat("G0IJ78KL9", store.KindMPIM, "mpdm-ada--ben--dev-1", "Ada Abbott, Ben Banerjee, Dev Dubois", 230, 0),
	}
	for _, c := range chats {
		p.Channels = append(p.Channels, c)
		p.Posts += c.Posts
		p.Posted += c.Posted
	}
	p.Problems = append(p.Problems, store.LaneProblem{LaneID: "load:G0IJ78KL9", Stage: store.StageLoad, Status: string(store.LaneOpen), Attempts: 1,
		NotBefore: v.At.Add(4 * time.Second), Error: "group direct message G0IJ78KL9 has not been created yet"})
	return v
}

func TestDirectMessagesAreAGroupOfTheirOwn(t *testing.T) {
	r := newHarness(t, &fakeSource{views: script(40, withChats)}, theme.Default(), 150, 34)
	r.poll(8)
	out := r.frame()
	teams, chats := strings.Index(out, "Sales"), strings.Index(out, "Direct messages")
	if chats < 0 || teams < 0 || chats < teams {
		t.Errorf("direct messages are not listed after the teams:\n%s", out)
	}
	for _, want := range []string{"Direct messages  3 conversations", "Ada Abbott, Ben Banerjee", "Dev Dubois, Елена"} {
		if !strings.Contains(out, want) {
			t.Errorf("the dashboard does not show %q:\n%s", want, out)
		}
	}
	// Never as a channel, never by the name Slack generated, and not under
	// a team that does not exist.
	for _, bad := range []string{"#Ada", "#Dev", "mpdm-", "No team"} {
		if strings.Contains(out, bad) {
			t.Errorf("the dashboard shows %q:\n%s", bad, out)
		}
	}
	// A problem with a direct message's lane is named by the lane, which
	// carries its ID and nobody's name.
	r.press("3")
	if out := r.frame(); !strings.Contains(out, "load:G0IJ78KL9") {
		t.Errorf("the problem is not named by its lane:\n%s", out)
	}
	// The filter finds one by a participant.
	r.press("1", "/")
	for _, k := range "banerjee" {
		r.press(string(k))
	}
	r.press("enter")
	if out := r.frame(); !strings.Contains(out, "Ada Abbott, Ben Banerjee") || strings.Contains(out, "#general") {
		t.Errorf("filtering by a participant:\n%s", out)
	}
}

// A conversation Microsoft refused a chat for is over, not waiting, and it
// is marked as something to look at.
func TestSkippedConversationIsShownAsSkipped(t *testing.T) {
	skipped := func(step int) controller.DashboardView {
		v := withChats(step)
		p := v.Run.Progress
		for i := range p.Channels {
			if p.Channels[i].SlackID == "G0IJ78KL9" {
				p.Channels[i].State, p.Channels[i].SkipCode = store.ChannelSkipped, "graph.chat_refused"
				p.Channels[i].Failed = p.Channels[i].Posts
				p.Failed += p.Channels[i].Posts
			}
		}
		return v
	}
	c := skipped(1).Run.Progress.Channels
	last := c[len(c)-1]
	if classify(last, false, false) != classDone {
		t.Error("a skipped conversation is still waiting")
	}
	r := newHarness(t, &fakeSource{views: script(40, skipped)}, theme.Default(), 150, 70)
	r.poll(8)
	out := r.frame()
	var row string
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "230") && strings.Contains(line, "Ada Abbott, Ben Banerje") {
			row = line
		}
	}
	if !strings.Contains(row, "skip") || !strings.Contains(row, "!") {
		t.Errorf("the row does not say the conversation was skipped: %q\n%s", row, out)
	}
}

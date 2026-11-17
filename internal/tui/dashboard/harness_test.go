package dashboard

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/sidecar/budget"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/testutil"
	"github.com/bgfdev/taut/internal/tui/shell"
	"github.com/bgfdev/taut/internal/tui/theme"
)

// t0 is the controller's clock at the first scripted snapshot.
var t0 = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

// fakeSource is a scripted control plane: Dashboard returns the snapshots in
// order and then repeats the last one.
type fakeSource struct {
	views        []controller.DashboardView
	next         int
	dashboardErr error
	entries      []store.LogEntry
	logErr       error
	filters      []store.LogFilter
	retryN       int
	retryErr     error
	retries      int
}

func (f *fakeSource) Dashboard(context.Context) (controller.DashboardView, error) {
	if f.dashboardErr != nil {
		return controller.DashboardView{}, f.dashboardErr
	}
	if len(f.views) == 0 {
		return controller.DashboardView{}, errors.New("no snapshot scripted")
	}
	v := f.views[min(f.next, len(f.views)-1)]
	f.next++
	return v, nil
}

func (f *fakeSource) AuditLog(_ context.Context, lf store.LogFilter) (controller.LogPage, error) {
	f.filters = append(f.filters, lf)
	if f.logErr != nil {
		return controller.LogPage{}, f.logErr
	}
	var page controller.LogPage
	for _, e := range f.entries {
		if (lf.Stream == "" || e.Stream == lf.Stream) && (lf.Outcome == "" || e.Outcome == lf.Outcome) &&
			(lf.Since.IsZero() || !e.At.Before(lf.Since)) {
			page.Entries = append(page.Entries, e)
		}
	}
	return page, nil
}

func (f *fakeSource) Retry(context.Context) (int, error) {
	f.retries++
	return f.retryN, f.retryErr
}

// harness drives a dashboard inside the shell without a terminal or a clock:
// timers are recorded instead of slept on, and every other command runs
// at once.
type harness struct {
	t      *testing.T
	s      *Screen
	m      tea.Model
	src    *fakeSource
	now    time.Time
	timers []time.Duration
}

func newHarness(t *testing.T, src *fakeSource, th theme.Theme, w, h int) *harness {
	t.Helper()
	r := &harness{t: t, src: src, now: time.Date(2026, 11, 14, 4, 0, 0, 0, time.UTC)}
	r.s = New(src, Options{Now: func() time.Time { return r.now }})
	r.s.after = func(d time.Duration, msg tea.Msg) tea.Cmd {
		if _, frame := msg.(frameMsg); !frame {
			r.timers = append(r.timers, d)
		}
		return nil
	}
	r.m = shell.New(r.s, shell.Options{Theme: th, Command: "dashboard", Context: "acme-migration", LockTheme: true})
	r.send(tea.WindowSizeMsg{Width: w, Height: h})
	r.run(r.m.Init())
	return r
}

func (r *harness) send(msg tea.Msg) {
	var cmd tea.Cmd
	r.m, cmd = r.m.Update(msg)
	r.run(cmd)
}

func (r *harness) run(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	switch msg := cmd().(type) {
	case nil:
	case tea.BatchMsg:
		for _, c := range msg {
			r.run(c)
		}
	default:
		r.send(msg)
	}
}

// poll fetches the next scripted snapshot, as the timer would. The
// operator's clock moves a second each time.
func (r *harness) poll(n int) {
	for range n {
		r.now = r.now.Add(time.Second)
		r.send(pollMsg{})
	}
}

func (r *harness) press(keys ...string) {
	for _, k := range keys {
		var msg tea.KeyPressMsg
		switch k {
		case "tab":
			msg = tea.KeyPressMsg{Code: tea.KeyTab}
		case "esc":
			msg = tea.KeyPressMsg{Code: tea.KeyEscape}
		case "enter":
			msg = tea.KeyPressMsg{Code: tea.KeyEnter}
		case "down":
			msg = tea.KeyPressMsg{Code: tea.KeyDown}
		case "backspace":
			msg = tea.KeyPressMsg{Code: tea.KeyBackspace}
		case "pgdown":
			msg = tea.KeyPressMsg{Code: tea.KeyPgDown}
		default:
			msg = tea.KeyPressMsg{Code: []rune(k)[0], Text: k}
		}
		r.send(msg)
	}
}

func (r *harness) frame() string { return testutil.Plain(r.m.View().Content) }

// --- scripted data -------------------------------------------------------------

func ch(team, name string, posts, posted, files, filesDone int) store.ChannelProgress {
	return store.ChannelProgress{SlackID: "C" + name, SlackName: name, TargetName: name, Team: team,
		State: store.ChannelMigrating, Posts: posts, Posted: posted, Files: files, FilesDone: filesDone}
}

// midRun is a run part-way through: transform is still allocating posts,
// two workers are at work, one lane has failed and one is retrying.
// step moves it forward in time; each step is one second on the controller's clock.
func midRun(step int) controller.DashboardView {
	at := t0.Add(time.Duration(step) * time.Second)
	general := ch("Engineering", "general", 18400, 9200+step*31+(step*step*37)%40, 240, min(180+step*2, 240))
	general.Active = true
	deploys := ch("Engineering", "deploys", 6100, 1500+step*9, 12, 12)
	deploys.Active = true
	incidents := ch("Engineering", "incidents", 2210, 1890, 64, 64)
	incidents.Failed, incidents.Held, incidents.LaneFailed = 4, 2, true
	incidents.LastError = "Graph refused the message: 403 Forbidden"
	random := ch("Engineering", "random", 3020, 3020, 88, 88)
	archive := ch("Engineering", "archive-2019", 0, 0, 0, 0)
	archive.State = store.ChannelPlanned
	leads := ch("Sales", "leads", 4800, 4800, 310, 310)
	leads.Failed = 0
	pipeline := ch("Sales", "pipeline-review", 1250, 0, 40, 12)
	pipeline.State = store.ChannelCreated
	wins := ch("Sales", "wins", 940, 931, 5, 5)
	wins.Failed = 9
	wins.Posted = 931

	chans := []store.ChannelProgress{general, deploys, incidents, random, archive, leads, pipeline, wins}
	p := &store.Progress{
		Stages: []store.StageProgress{
			{Stage: store.StagePlan, Lanes: 1, Done: 1, Batches: 1, BatchesDone: 1},
			{Stage: store.StageExtract, Lanes: 8, Done: 8, Batches: 400, BatchesDone: 400},
			{Stage: store.StageTransform, Lanes: 8, Done: 5, Leased: 2, Batches: 400, BatchesDone: 300},
			{Stage: store.StageTeam, Lanes: 2, Done: 2, Batches: 2, BatchesDone: 2},
			{Stage: store.StageFiles, Lanes: 8, Done: 6, Leased: 1, Batches: 40, BatchesDone: 31},
			{Stage: store.StageLoad, Lanes: 8, Done: 2, Failed: 1, Leased: 2, Batches: 400, BatchesDone: 190},
		},
		Channels: chans,
		Problems: []store.LaneProblem{
			{LaneID: "files:Cpipeline-review", Stage: store.StageFiles, Status: string(store.LaneOpen), Attempts: 2,
				NotBefore: at.Add(12 * time.Second), Error: "SharePoint upload session expired: 404 itemNotFound"},
			{LaneID: "load:Cincidents", Stage: store.StageLoad, Status: string(store.LaneFailed), Attempts: 5,
				Error: "Graph refused the message: 403 Forbidden. The app registration lacks Teamwork.Migrate.All for this team, or admin consent was withdrawn while the run was in progress."},
		},
		FileBytes: 4_800_000_000, FileBytesDone: 3_100_000_000 + int64(step)*2_400_000 + int64(step*step%7)*900_000,
	}
	for _, c := range chans {
		p.Posts += c.Posts
		p.Posted += c.Posted
		p.Failed += c.Failed
		p.Held += c.Held
		p.Files += c.Files
		p.FilesUploaded += c.FilesDone
	}
	return controller.DashboardView{
		Run: controller.RunView{ID: "01JCM4Q8ZK7W3N5T2R6V9XYB0D", Name: "acme-migration", Status: store.RunRunning,
			CreatedAt: t0.Add(-12*time.Minute - 4*time.Second), Progress: p},
		Cluster: controller.ClusterView{
			Workers: []store.Holder{
				{Holder: "worker-a1f3", Lanes: 3, Stages: []string{"files", "load"}, LeaseExpires: t0.Add(time.Duration(step/10*10+30) * time.Second)},
				{Holder: "worker-c9d2", Lanes: 2, Stages: []string{"transform"}, LeaseExpires: t0.Add(time.Duration((step+5)/10*10+25) * time.Second)},
			},
			Budgets: []budget.KeyStatus{
				{Key: budget.Key{Tenant: "9f1c2d3e-0000-4000-8000-acme", Scope: "graph"}, TotalRPS: 40, ShareRPS: 20, Holders: []string{"a", "b"}},
				{Key: budget.Key{Tenant: "9f1c2d3e-0000-4000-8000-acme", Scope: "sharepoint"}, TotalRPS: 10, ShareRPS: 10, Holders: []string{"a"}},
			},
			Throttles: []controller.ThrottleView{
				{Tenant: "9f1c2d3e-0000-4000-8000-acme", Scope: "graph", Holder: "a", At: t0.Add(-3 * time.Minute), Until: t0.Add(-170 * time.Second)},
			},
		},
		At: at,
	}
}

// script returns n successive snapshots of fn.
func script(n int, fn func(int) controller.DashboardView) []controller.DashboardView {
	out := make([]controller.DashboardView, n)
	for i := range out {
		out[i] = fn(i)
	}
	return out
}

func manyChannels(n int) func(int) controller.DashboardView {
	return func(step int) controller.DashboardView {
		v := midRun(step)
		p := v.Run.Progress
		p.Channels = nil
		for i := range n {
			c := ch(fmt.Sprintf("Team %02d", i/50), fmt.Sprintf("channel-%03d", i), 1000+i, min(i*4, 1000+i), i%7, i%7)
			c.Active = i%97 == 3
			p.Channels = append(p.Channels, c)
		}
		return v
	}
}

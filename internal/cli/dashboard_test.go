package cli_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/sidecar/budget"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/testutil"
)

const dashboardRunID = "run-2026-11-14-d3c4b5"

var dashboardAt = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

// dashboardView is a run part-way through its import, as the controller would report it.
func dashboardView() controller.DashboardView {
	p := &store.Progress{
		Stages: []store.StageProgress{
			{Stage: store.StagePlan, Lanes: 1, Done: 1, Batches: 1, BatchesDone: 1},
			{Stage: store.StageExtract, Lanes: 3, Done: 3, Batches: 90, BatchesDone: 90},
			{Stage: store.StageTransform, Lanes: 3, Done: 3, Batches: 90, BatchesDone: 90},
			{Stage: store.StageTeam, Lanes: 1, Done: 1, Batches: 1, BatchesDone: 1},
			{Stage: store.StageFiles, Lanes: 3, Done: 2, Leased: 1, Batches: 6, BatchesDone: 5},
			{Stage: store.StageLoad, Lanes: 3, Done: 1, Leased: 1, Failed: 1, Batches: 90, BatchesDone: 52},
		},
		Channels: []store.ChannelProgress{
			{SlackID: "C024BE91L", SlackName: "general", TargetName: "General", Kind: "public", Team: "Fixture Corp", State: store.ChannelMigrating,
				Posts: 4200, Posted: 2100, Files: 60, FilesDone: 44, Active: true},
			{SlackID: "C07QX3M2K", SlackName: "random", TargetName: "random", Kind: "public", Team: "Fixture Corp", State: store.ChannelMigrating,
				Posts: 900, Posted: 900, Files: 12, FilesDone: 12},
			{SlackID: "C09ZZ81AB", SlackName: "incidents", TargetName: "incidents", Kind: "public", Team: "Fixture Corp", State: store.ChannelMigrating,
				Posts: 640, Posted: 310, Failed: 3, Held: 1, LaneFailed: true, LastError: "403 Forbidden"},
		},
		Problems: []store.LaneProblem{
			{LaneID: "load:C09ZZ81AB", Stage: store.StageLoad, Status: string(store.LaneFailed), Attempts: 5,
				Error: "Graph refused the message: 403 Forbidden"},
		},
		Posts: 5740, Posted: 3310, Failed: 3, Held: 1,
		Files: 72, FilesUploaded: 56, FileBytes: 480_000_000, FileBytesDone: 350_000_000,
	}
	return controller.DashboardView{
		Run: controller.RunView{ID: dashboardRunID, Name: "fixture", Status: store.RunRunning, ConfigHash: "sha256:5f1d", Pass: 1,
			CreatedAt: dashboardAt.Add(-9 * time.Minute), UpdatedAt: dashboardAt.Add(-time.Second), Progress: p},
		Cluster: controller.ClusterView{
			Workers: []store.Holder{
				{Holder: "worker-a1f3", Lanes: 2, Stages: []string{"files", "load"}, LeaseExpires: dashboardAt.Add(24 * time.Second)},
			},
			Budgets: []budget.KeyStatus{
				{Key: budget.Key{Tenant: "9f1c2d3e", Scope: budget.ScopeGraph}, TotalRPS: 40, ShareRPS: 40, Holders: []string{"worker-a1f3"}},
			},
			Throttles: []controller.ThrottleView{
				{Tenant: "9f1c2d3e", Scope: budget.ScopeGraph, Holder: "worker-a1f3", At: dashboardAt.Add(-4 * time.Second), Until: dashboardAt.Add(11 * time.Second)},
			},
		},
		At: dashboardAt,
	}
}

// dashboardEnv is a config file pointing at a stand-in controller that serves one
// dashboard snapshot over the real HTTP API.
type dashboardEnv struct {
	*env
	view  controller.DashboardView
	noRun bool
	// accept makes the stand-in take a new run: it waits for the export,
	// keeps what is uploaded, and is complete by the next look.
	accept   bool
	created  int
	uploaded []byte
	asked    []string // run IDs the dashboard endpoint was asked for
}

func newDashboardEnv(t *testing.T) *dashboardEnv {
	t.Helper()
	e := &dashboardEnv{env: newEnv(t, "medium"), view: dashboardView()}
	e.init()
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	notFound := func(w http.ResponseWriter) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": fail.New("run.not_found", "No such run", "", "Run `taut status` to see the current run, or `taut migrate` to start one.")})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/runs", func(w http.ResponseWriter, r *http.Request) {
		if e.noRun || r.URL.Query().Get("name") != "fixture" {
			notFound(w)
			return
		}
		writeJSON(w, http.StatusOK, e.view.Run)
	})
	mux.HandleFunc("GET /v1/runs/{id}/dashboard", func(w http.ResponseWriter, r *http.Request) {
		e.asked = append(e.asked, r.PathValue("id"))
		if r.PathValue("id") != dashboardRunID {
			notFound(w)
			return
		}
		writeJSON(w, http.StatusOK, e.view)
	})
	started := func() controller.RunView {
		v := e.view.Run
		v.Status, v.Progress = store.RunCompleted, nil
		return v
	}
	mux.HandleFunc("POST /v1/runs", func(w http.ResponseWriter, _ *http.Request) {
		if !e.accept {
			notFound(w)
			return
		}
		e.created++
		v := started()
		v.Status = store.RunAwaitingExport
		writeJSON(w, http.StatusOK, v)
	})
	mux.HandleFunc("PUT /v1/runs/{id}/export", func(w http.ResponseWriter, r *http.Request) {
		e.uploaded, _ = io.ReadAll(r.Body)
		writeJSON(w, http.StatusOK, started())
	})
	mux.HandleFunc("PUT /v1/runs/{id}/overrides", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /v1/runs/{id}", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, started()) })
	mux.HandleFunc("GET /v1/runs/{id}/audit/verify", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	raw, err := os.ReadFile(e.config)
	if err != nil {
		t.Fatal(err)
	}
	const defaultURL = "http://localhost:7070"
	if !strings.Contains(string(raw), defaultURL) {
		t.Fatalf("the config file has no controller URL to redirect:\n%s", raw)
	}
	if err := os.WriteFile(e.config, []byte(strings.ReplaceAll(string(raw), defaultURL, srv.URL)), 0o600); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *dashboardEnv) ok(args ...string) string {
	e.t.Helper()
	out, errOut, code := e.run(args...)
	if code != 0 {
		e.t.Fatalf("taut %s exited %d:\n%s\n%s", strings.Join(args, " "), code, out, errOut)
	}
	noSecrets(e.t, out, errOut)
	return out
}

// Without a terminal dashboard prints one snapshot and exits, rather than
// failing or drawing a screen into a pipe.
func TestDashboardPlain(t *testing.T) {
	e := newDashboardEnv(t)
	testutil.Golden(t, "dashboard_plain", e.ok("dashboard"))

	// A run that stopped says so, and names the command that resumes it.
	e.view.Run.Status = store.RunFailed
	e.view.Cluster = controller.ClusterView{}
	testutil.Golden(t, "dashboard_plain_failed", e.ok("dashboard"))

	// A finished run points at the next step of the migration.
	e.view = dashboardView()
	p := e.view.Run.Progress
	for i := range p.Stages {
		p.Stages[i].Done, p.Stages[i].Leased, p.Stages[i].Failed = p.Stages[i].Lanes, 0, 0
		p.Stages[i].BatchesDone = p.Stages[i].Batches
	}
	for i := range p.Channels {
		p.Channels[i].Posted, p.Channels[i].Failed, p.Channels[i].Held = p.Channels[i].Posts, 0, 0
		p.Channels[i].Active, p.Channels[i].LaneFailed = false, false
	}
	p.Posted, p.Failed, p.Held, p.Problems = p.Posts, 0, 0, nil
	p.FilesUploaded, p.FileBytesDone = p.Files, p.FileBytes
	done := dashboardAt.Add(-time.Minute)
	e.view.Run.Status, e.view.Run.FinishedAt, e.view.Cluster = store.RunCompleted, &done, controller.ClusterView{}
	testutil.Golden(t, "dashboard_plain_completed", e.ok("dashboard"))
}

// --json is exactly one DashboardView: what the controller sent, nothing added.
func TestDashboardJSON(t *testing.T) {
	e := newDashboardEnv(t)
	out := e.ok("dashboard", "--json")
	testutil.Golden(t, "dashboard_json", out)
	var got controller.DashboardView
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not one JSON document: %v\n%s", err, out)
	}
	want := dashboardView()
	if got.Run.ID != want.Run.ID || !got.At.Equal(want.At) || got.Run.Progress == nil ||
		got.Run.Progress.Posted != want.Run.Progress.Posted || len(got.Cluster.Workers) != 1 || len(got.Cluster.Throttles) != 1 {
		t.Fatalf("snapshot did not survive the round trip:\n%s", out)
	}
}

func TestDashboardSelectsTheRun(t *testing.T) {
	e := newDashboardEnv(t)
	e.ok("dashboard", "--json")
	e.ok("dashboard", "--json", "--run", dashboardRunID)
	if len(e.asked) != 2 || e.asked[0] != dashboardRunID || e.asked[1] != dashboardRunID {
		t.Fatalf("asked for %v", e.asked)
	}

	_, errOut, code := e.run("dashboard", "--run", "run-that-never-was")
	if code == 0 || !strings.Contains(errOut, "No such run") {
		t.Fatalf("an unknown run must fail (exit %d):\n%s", code, errOut)
	}

	e.noRun = true
	_, errOut, code = e.run("dashboard")
	if code == 0 || !strings.Contains(errOut, "No migration has been started for fixture") || !strings.Contains(errOut, "taut migrate") {
		t.Fatalf("no run for the config file (exit %d):\n%s", code, errOut)
	}
}

func TestDashboardRejectsATinyInterval(t *testing.T) {
	e := newDashboardEnv(t)
	_, errOut, code := e.run("dashboard", "--interval", "10ms")
	if code != fail.ExitUsage || !strings.Contains(errOut, "--interval 10ms is too short") {
		t.Fatalf("exit %d:\n%s", code, errOut)
	}
	if len(e.asked) != 0 {
		t.Fatal("the control plane was asked before the flags were checked")
	}
}

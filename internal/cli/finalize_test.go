package cli_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/testutil"
)

// finalizeEnv is a config file pointing at a stand-in controller that holds one run and
// records what `taut finalize` asks of it.
type finalizeEnv struct {
	*env
	mu      sync.Mutex
	state   controller.RunView
	reqs    []controller.FinalizeRequest
	retries int
	// logOK is what the stand-in answers to an audit log verification.
	logOK bool
	// releaseFails makes the release of an abandoned run's chats fail.
	releaseFails bool
}

func finalizeRun(status store.RunStatus) controller.RunView {
	p := &store.Progress{
		Stages: []store.StageProgress{
			{Stage: store.StagePlan, Lanes: 1, Done: 1, Batches: 1, BatchesDone: 1},
			{Stage: store.StageLoad, Lanes: 3, Done: 3, Batches: 90, BatchesDone: 90},
		},
		Channels: []store.ChannelProgress{
			{SlackID: "C024BE91L", SlackName: "general", TargetName: "General", Kind: "public", Team: "Fixture Corp", State: store.ChannelMigrating, Posts: 4200, Posted: 4200},
			{SlackID: "C07QX3M2K", SlackName: "random", TargetName: "random", Kind: "public", Team: "Fixture Corp", State: store.ChannelMigrating, Posts: 900, Posted: 897, Failed: 2, Held: 1},
			{SlackID: "C09ZZ81AB", SlackName: "incidents", TargetName: "incidents", Kind: "public", Team: "Fixture Corp", State: store.ChannelMigrating, Posts: 640, Posted: 640},
		},
		Posts: 5740, Posted: 5737, Failed: 2, Held: 1,
	}
	at := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	return controller.RunView{ID: "run-2026-11-14-m00r", Name: "fixture", Status: status, CreatedAt: at, UpdatedAt: at, Progress: p}
}

// finish makes the stand-in's run look finalized.
func (e *finalizeEnv) finish() {
	now := time.Date(2026, 11, 14, 12, 5, 0, 0, time.UTC)
	e.state.Status, e.state.PurgedAt = store.RunFinalized, &now
	p := e.state.Progress
	for i := range p.Channels {
		p.Channels[i].State = store.ChannelCompleted
	}
	p.Stages = append(p.Stages[:2:2],
		store.StageProgress{Stage: store.StageFinalize, Lanes: 3, Done: 3, Batches: 3, BatchesDone: 3},
		store.StageProgress{Stage: store.StageMembers, Lanes: 1, Done: 1, Batches: 1, BatchesDone: 1})
}

func newFinalizeEnv(t *testing.T, status store.RunStatus) *finalizeEnv {
	t.Helper()
	e := &finalizeEnv{env: newEnv(t, "medium"), state: finalizeRun(status), logOK: true}
	e.init()
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	get := func(w http.ResponseWriter, _ *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		// A run that is finalizing finishes by the time it is looked at
		// again.
		if e.state.Status == store.RunFinalizing {
			defer e.finish()
		}
		// So does the release of an abandoned run's chats, unless the
		// test says it fails.
		for i, s := range e.state.Progress.Stages {
			if s.Stage != store.StageRelease || s.Done+s.Failed == s.Lanes {
				continue
			}
			i := i
			defer func() {
				p := e.state.Progress
				if e.releaseFails {
					p.Stages[i].Failed = p.Stages[i].Lanes
					p.Problems = append(p.Problems, store.LaneProblem{LaneID: "release:D024BE91L", Stage: store.StageRelease,
						Status: string(store.LaneFailed), Error: "Microsoft Graph refused the request (403)"})
					return
				}
				p.Stages[i].Done = p.Stages[i].Lanes
				for j := range p.Channels {
					if store.IsChatKind(p.Channels[j].Kind) && p.Channels[j].State == store.ChannelMigrating {
						p.Channels[j].State = store.ChannelCompleted
					}
				}
			}()
		}
		writeJSON(w, http.StatusOK, e.state)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/runs", get)
	mux.HandleFunc("GET /v1/runs/{id}", get)
	mux.HandleFunc("POST /v1/runs/{id}/finalize", func(w http.ResponseWriter, r *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		var req controller.FinalizeRequest
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &req)
		e.reqs = append(e.reqs, req)
		if e.state.Status == store.RunFailed || e.state.Status == store.RunCancelled {
			// Like the control plane: chats, and only chats, of a run that
			// will not finish can be taken out of migration mode.
			chat, n := map[string]bool{}, 0
			for _, c := range e.state.Progress.Channels {
				chat[c.SlackID] = store.IsChatKind(c.Kind)
			}
			for _, id := range req.Channels {
				if !chat[id] {
					n = -1
					break
				}
				n++
			}
			if n > 0 {
				e.state.Progress.Stages = append(e.state.Progress.Stages, store.StageProgress{Stage: store.StageRelease, Lanes: n, Batches: n})
				writeJSON(w, http.StatusOK, controller.FinalizeView{Run: e.state, Chats: n, Releasing: n})
				return
			}
		}
		if e.state.Status != store.RunCompleted && e.state.Status != store.RunFinalizing {
			writeJSON(w, http.StatusConflict, map[string]any{"error": fail.New("finalize.not_ready", "The migration is not ready to be finalized", "", "Wait.")})
			return
		}
		e.state.Status = store.RunFinalizing
		writeJSON(w, http.StatusOK, controller.FinalizeView{Run: e.state, Channels: 3, Teams: 1})
	})
	mux.HandleFunc("POST /v1/runs/{id}/retry", func(w http.ResponseWriter, _ *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.retries++
		e.state.Status = store.RunFinalizing
		writeJSON(w, http.StatusOK, map[string]int{"lanes": 1})
	})
	mux.HandleFunc("GET /v1/runs/{id}/audit/verify", func(w http.ResponseWriter, _ *http.Request) {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.logOK {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "problem": "stream load:C024BE91L: entry 7 does not match its hash"})
	})
	mux.HandleFunc("GET /v1/runs/{id}/audit", func(w http.ResponseWriter, _ *http.Request) {
		entries := make([]store.LogEntry, 29)
		for i := range entries {
			entries[i] = store.LogEntry{Stream: "members:0", Seq: int64(i + 1), Op: "member.add", Outcome: "ok"}
		}
		writeJSON(w, http.StatusOK, controller.LogPage{Entries: entries})
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

func (e *finalizeEnv) requests() (reqs []controller.FinalizeRequest, retries int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]controller.FinalizeRequest(nil), e.reqs...), e.retries
}

// Finalizing cannot be undone, so without a terminal it needs --yes, and
// refusing sends nothing.
func TestFinalizeNeedsConfirmation(t *testing.T) {
	e := newFinalizeEnv(t, store.RunCompleted)
	_, errOut, code := e.run("finalize")
	if code != fail.ExitBlockers {
		t.Fatalf("exit %d, want %d:\n%s", code, fail.ExitBlockers, errOut)
	}
	if !strings.Contains(errOut, "taut finalize --yes") {
		t.Errorf("the error does not name the next step:\n%s", errOut)
	}
	if reqs, _ := e.requests(); len(reqs) != 0 {
		t.Errorf("an unconfirmed finalize sent %d requests", len(reqs))
	}
}

func TestFinalizeFinalizes(t *testing.T) {
	e := newFinalizeEnv(t, store.RunCompleted)
	out, errOut, code := e.run("finalize", "--yes")
	if code != 0 {
		t.Fatalf("exit %d:\n%s\n%s", code, out, errOut)
	}
	noSecrets(t, out, errOut)
	testutil.Golden(t, "finalize_plain", testutil.Stable(out))
	reqs, _ := e.requests()
	if len(reqs) != 1 || len(reqs[0].Channels) != 0 || reqs[0].Retain {
		t.Errorf("requests = %+v, want one for every channel", reqs)
	}
}

func TestFinalizeSelectsChannelsAndRetains(t *testing.T) {
	e := newFinalizeEnv(t, store.RunCompleted)
	if out, errOut, code := e.run("finalize", "--yes", "--channel", "#random", "--channel", "C09ZZ81AB", "--retain", "--json"); code != 0 {
		t.Fatalf("exit %d:\n%s\n%s", code, out, errOut)
	}
	reqs, _ := e.requests()
	if len(reqs) != 1 || strings.Join(reqs[0].Channels, ",") != "C07QX3M2K,C09ZZ81AB" || !reqs[0].Retain {
		t.Errorf("requests = %+v", reqs)
	}

	_, errOut, code := e.run("finalize", "--yes", "--channel", "nope")
	if code != fail.ExitUsage {
		t.Errorf("unknown channel: exit %d, want %d:\n%s", code, fail.ExitUsage, errOut)
	}
}

// Reattaching to a run that is being finalized watches it. It must not ask
// for anything new: a plain `taut finalize` after `taut finalize --channel x` would
// otherwise open every channel, unasked and unconfirmed.
func TestFinalizeReattachSendsNoRequest(t *testing.T) {
	e := newFinalizeEnv(t, store.RunFinalizing)
	out, errOut, code := e.run("finalize")
	if code != 0 {
		t.Fatalf("exit %d:\n%s\n%s", code, out, errOut)
	}
	if reqs, _ := e.requests(); len(reqs) != 0 {
		t.Errorf("reattaching sent %d finalize requests: %+v", len(reqs), reqs)
	}
	if !strings.Contains(out, "Migration finalized") {
		t.Errorf("no final verdict:\n%s", out)
	}

	// Naming more channels while it runs is a new request, and is
	// confirmed like one.
	e = newFinalizeEnv(t, store.RunFinalizing)
	_, _, code = e.run("finalize", "--channel", "general")
	if code != fail.ExitBlockers {
		t.Errorf("adding a channel without --yes: exit %d, want %d", code, fail.ExitBlockers)
	}
	if reqs, _ := e.requests(); len(reqs) != 0 {
		t.Errorf("an unconfirmed addition sent %d requests", len(reqs))
	}
}

// Finalizing that stopped part-way is continued, not started over wider.
func TestFinalizeContinuesAfterFailure(t *testing.T) {
	e := newFinalizeEnv(t, store.RunFailed)
	e.state.Progress.Stages = append(e.state.Progress.Stages,
		store.StageProgress{Stage: store.StageFinalize, Lanes: 1, Failed: 1, Batches: 1})
	out, errOut, code := e.run("finalize")
	if code != 0 {
		t.Fatalf("exit %d:\n%s\n%s", code, out, errOut)
	}
	reqs, retries := e.requests()
	if retries != 1 || len(reqs) != 0 {
		t.Errorf("retries = %d, finalize requests = %+v; want one retry and no new request", retries, reqs)
	}
}

func TestFinalizeRefusals(t *testing.T) {
	// Still importing.
	e := newFinalizeEnv(t, store.RunRunning)
	if _, errOut, code := e.run("finalize", "--yes"); code != fail.ExitBlockers || !strings.Contains(errOut, "still running") {
		t.Errorf("running: exit %d\n%s", code, errOut)
	}
	// The import itself failed.
	e = newFinalizeEnv(t, store.RunFailed)
	if _, errOut, code := e.run("finalize", "--yes"); code != fail.ExitBlockers || !strings.Contains(errOut, "taut migrate") {
		t.Errorf("failed import: exit %d\n%s", code, errOut)
	}
	// A broken audit trail stops it before anything irreversible.
	e = newFinalizeEnv(t, store.RunCompleted)
	e.logOK = false
	if _, errOut, code := e.run("finalize", "--yes"); code != fail.ExitFailure || !strings.Contains(errOut, "audit log failed verification") {
		t.Errorf("corrupt audit log: exit %d\n%s", code, errOut)
	}
	if reqs, _ := e.requests(); len(reqs) != 0 {
		t.Errorf("finalize went ahead on a corrupt audit log: %+v", reqs)
	}
}

// A second migration run must not import a finished migration again.
func TestMigrateRefusesFinishedMigration(t *testing.T) {
	for _, status := range []store.RunStatus{store.RunCompleted, store.RunFinalizing, store.RunFinalized} {
		e := newFinalizeEnv(t, status)
		_, errOut, code := e.run("migrate", "--yes")
		if code != fail.ExitUsage {
			t.Errorf("%s: exit %d, want %d:\n%s", status, code, fail.ExitUsage, errOut)
		}
	}
}

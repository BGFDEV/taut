package cli_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/bus"
	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/devstack"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/fakegraph"
	"github.com/bgfdev/taut/internal/fixtures"
	"github.com/bgfdev/taut/internal/graph"
	"github.com/bgfdev/taut/internal/preflight"
	"github.com/bgfdev/taut/internal/secret"
	"github.com/bgfdev/taut/internal/staging"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/store/storetest"
	"github.com/bgfdev/taut/internal/testutil"
	"github.com/bgfdev/taut/internal/worker"
)

// passEnv is a whole migration stack behind the CLI: a real ledger, a real
// controller and a worker, with the fake tenant and the fake Slack. It is
// what the commands of an incremental pass are tested against, since what
// they report comes from the ledger.
type passEnv struct {
	*env
	store *store.PG
}

func newPassEnv(t *testing.T, base string) *passEnv {
	t.Helper()
	st := storetest.OpenPG(t) // skips when no test database is configured
	e := &passEnv{store: st}
	// The tenant and the file host know the later export's people and
	// files, which include the base's.
	later := fixtures.LaterOf(base)
	dir := t.TempDir()
	if _, err := fixtures.Write(base, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := fixtures.Write(later, dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "elsewhere"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := fixtures.Write(later, filepath.Join(dir, "elsewhere"), fixtures.OtherWorkspace()); err != nil {
		t.Fatal(err)
	}
	fake := httptest.NewServer(devstack.Handler(later))
	t.Cleanup(fake.Close)
	t.Setenv("TAUT_SLACK_TOKEN", devstack.SlackToken)
	t.Setenv("TAUT_ENTRA_CLIENT_SECRET", fakegraph.DefaultSecret)
	e.env = &env{t: t, dir: dir, config: filepath.Join(dir, "taut.yaml"), url: fake.URL}

	blobs, err := staging.NewFS(filepath.Join(dir, "staging"))
	if err != nil {
		t.Fatal(err)
	}
	k, err := staging.GenerateKEK()
	if err != nil {
		t.Fatal(err)
	}
	kek, err := staging.ParseKEK(secret.New(k))
	if err != nil {
		t.Fatal(err)
	}
	local := bus.NewLocal()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := controller.New(controller.Config{}, log, controller.Services{Store: st, Blobs: blobs, KEK: kek, Bus: local, AllowTestEndpoints: true})
	ctl := httptest.NewServer(api.Handler())
	t.Cleanup(ctl.Close)

	ctx, cancel := context.WithCancel(context.Background())
	d, err := worker.New(worker.Config{
		Holder: "cli-test",
		Stages: []string{store.StagePlan, store.StageExtract, store.StageTransform, store.StageTeam, store.StageFiles, store.StageLoad, store.StageFinalize, store.StageMembers, store.StageRelease},
		Store:  st, Blobs: blobs, KEK: kek, Bus: local,
		Graph: func(_ context.Context, m *config.Config) (worker.Graph, error) {
			cred, err := graph.NewClientSecret(fakegraph.DefaultSecret)
			if err != nil {
				return nil, err
			}
			en := m.Target.Entra
			ts, err := graph.NewTokenSource(graph.CredentialsOptions{
				TenantID: en.TenantID, ClientID: en.ClientID, Credential: cred, Authority: en.Authority, GraphBase: en.GraphBaseURL,
			})
			if err != nil {
				return nil, err
			}
			return graph.New(graph.Options{BaseURL: en.GraphBaseURL, TokenSource: ts})
		},
		Slack: worker.HTTPSlackFiles{Rewrite: func(_ *config.Config, raw string) (string, http.Header, error) {
			return strings.Replace(raw, "https://files.slack.com", fake.URL, 1), nil, nil
		}},
		Concurrency: 4, LeaseTTL: 5 * time.Second, Poll: 20 * time.Millisecond,
		ChannelRPS: 1e6, TenantRPS: 1e6, ScratchDir: filepath.Join(dir, "scratch"),
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = d.Run(ctx) }()
	// The controller's control loop, which a served controller runs itself.
	go func() {
		for ctx.Err() == nil {
			api.Reconcile(ctx)
			time.Sleep(30 * time.Millisecond)
		}
	}()
	t.Cleanup(func() { cancel(); <-done })

	e.init("--export", filepath.Join(dir, base+".zip"))
	raw, err := os.ReadFile(e.config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "http://localhost:7070") {
		t.Fatal("the config file init wrote does not name the default controller")
	}
	if err := os.WriteFile(e.config, []byte(strings.Replace(string(raw), "http://localhost:7070", ctl.URL, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	return e
}

// useExport points the config file at another export.
func (e *passEnv) useExport(path string) {
	e.t.Helper()
	raw, err := os.ReadFile(e.config)
	if err != nil {
		e.t.Fatal(err)
	}
	m, err := config.Parse(raw)
	if err != nil {
		e.t.Fatal(err)
	}
	was := m.Source.Slack.Export
	if !strings.Contains(string(raw), was) {
		e.t.Fatalf("the config file does not contain its export path %q", was)
	}
	if err := os.WriteFile(e.config, []byte(strings.Replace(string(raw), was, path, 1)), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// runIDPattern matches a run ID (store.NewRunID), whole or cut short by a
// table column.
var runIDPattern = regexp.MustCompile(`\d{8}T\d{6}\.\d{1,6}(-[a-z0-9]{1,10})?`)

// steady replaces what differs from one test run to the next: run IDs and
// local times.
func steady(s string) string {
	s = runIDPattern.ReplaceAllString(s, "RUN-ID")
	return regexp.MustCompile(`\d{1,2} [A-Z][a-z]{2} \d{4} \d{2}:\d{2}`).ReplaceAllString(s, "DATE TIME")
}

func TestIncrementalPassEndToEnd(t *testing.T) {
	e := newPassEnv(t, "small")
	facts := fixtures.LaterFacts("small-later")
	added := facts.NewMessages - facts.NewDMMessages

	// Nothing to follow yet.
	if _, _, code := e.run("preflight", "--incremental", "--yes"); code != fail.ExitUsage {
		t.Fatalf("preflight --incremental before any run: exit %d, want %d", code, fail.ExitUsage)
	}
	out, errOut, code := e.run("migrate", "--incremental", "--yes")
	if code != fail.ExitUsage || !strings.Contains(errOut, "no earlier pass") {
		t.Fatalf("migrate --incremental before any run: exit %d\n%s\n%s", code, out, errOut)
	}

	// The first import.
	out, errOut, code = e.run("migrate", "--yes", "--json")
	if code != 0 {
		t.Fatalf("migrate failed (%d):\n%s\n%s", code, out, errOut)
	}
	var first struct {
		Run controller.RunView `json:"run"`
	}
	if err := json.Unmarshal([]byte(out), &first); err != nil {
		t.Fatalf("migrate --json: %v\n%s", err, out)
	}
	if first.Run.Status != store.RunCompleted || first.Run.Pass != 1 || first.Run.BaseRunID != "" || len(first.Run.Passes) != 0 {
		t.Fatalf("first import = pass %d, base %q, status %s", first.Run.Pass, first.Run.BaseRunID, first.Run.Status)
	}
	imported := first.Run.Progress.Posted

	// Imported migrations are not imported again; the way on is named.
	_, errOut, code = e.run("migrate", "--yes")
	if code != fail.ExitUsage || !strings.Contains(errOut, "taut migrate --incremental") {
		t.Fatalf("a second plain migrate: exit %d\n%s", code, errOut)
	}

	// The same export again holds nothing new, and no pass is started.
	out, errOut, code = e.run("preflight", "--incremental", "--yes")
	if code != 0 || !strings.Contains(out, "Nothing new to import") {
		t.Fatalf("preflight --incremental on the same export (%d):\n%s\n%s", code, out, errOut)
	}
	out, _, code = e.run("migrate", "--incremental", "--yes", "--json")
	var nothing struct {
		Started bool   `json:"started"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(out), &nothing); err != nil || code != 0 || nothing.Started || nothing.Reason != "nothing_new" {
		t.Fatalf("migrate --incremental with nothing new (%d): %v\n%s", code, err, out)
	}

	// Another workspace's export is refused before anything is started.
	e.useExport(filepath.Join(e.dir, "elsewhere", "small-later.zip"))
	out, _, code = e.run("preflight", "--incremental", "--json")
	var other preflight.Report
	if err := json.Unmarshal([]byte(out), &other); err != nil {
		t.Fatalf("preflight --incremental --json: %v\n%s", err, out)
	}
	if code != fail.ExitBlockers || len(other.Blockers) != 1 || other.Blockers[0].Code != "incremental.workspace_mismatch" ||
		other.Incremental == nil || other.Incremental.SameWorkspace || other.Incremental.New.Messages != 0 {
		t.Fatalf("preflight of another workspace's export (%d): blockers %+v, incremental %+v", code, other.Blockers, other.Incremental)
	}
	out, errOut, code = e.run("migrate", "--incremental", "--yes")
	if code != fail.ExitBlockers || !strings.Contains(out, "not of the Slack workspace") {
		t.Fatalf("migrate --incremental with another workspace's export (%d):\n%s\n%s", code, out, errOut)
	}
	testutil.Golden(t, "migrate_incremental_other_workspace", testutil.Plain(steady(out)))

	// The newer export of the same workspace.
	e.useExport(filepath.Join(e.dir, "small-later.zip"))
	out, errOut, code = e.run("preflight", "--incremental", "--yes")
	if code != 0 {
		t.Fatalf("preflight --incremental (%d):\n%s\n%s", code, out, errOut)
	}
	testutil.Golden(t, "preflight_incremental_plain", testutil.Plain(steady(out)))
	out, _, code = e.run("preflight", "--incremental", "--json")
	var rep preflight.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil || code != 0 {
		t.Fatalf("preflight --incremental --json (%d): %v\n%s", code, err, out)
	}
	inc := rep.Incremental
	if inc == nil || inc.Pass != 2 || inc.BaseRun != first.Run.ID || inc.BaseStatus != "completed" || !inc.SameWorkspace {
		t.Fatalf("incremental report = %+v", inc)
	}
	if inc.New.Messages != added || inc.New.RepliesToMigrated != facts.RepliesToOld || inc.New.Channels != facts.NewChannels ||
		inc.New.People != facts.NewUsers || inc.Edited != facts.Edited || inc.Deleted != facts.Deleted || inc.Already.Messages != imported {
		t.Errorf("incremental report: new %+v, edited %d, deleted %d, already %+v; the fixture adds %d messages (%d to old threads), %d channels, %d people, edits %d and deletes %d",
			inc.New, inc.Edited, inc.Deleted, inc.Already, added, facts.RepliesToOld, facts.NewChannels, facts.NewUsers, facts.Edited, facts.Deleted)
	}
	if rep.Totals.Messages != added || rep.Estimate.Imports != added {
		t.Errorf("the report's totals count %d messages and %d imports; the pass adds %d", rep.Totals.Messages, rep.Estimate.Imports, added)
	}
	codes := map[string]bool{}
	for _, w := range rep.Warnings {
		codes[w.Code] = true
	}
	if !codes["incremental.edits_not_synced"] || codes["incremental.reopens"] {
		t.Errorf("warnings = %+v", rep.Warnings)
	}

	// The pass.
	out, errOut, code = e.run("migrate", "--incremental", "--yes", "--json")
	if code != 0 {
		t.Fatalf("migrate --incremental failed (%d):\n%s\n%s", code, out, errOut)
	}
	var second struct {
		Run      controller.RunView `json:"run"`
		Verified *bool              `json:"auditLogVerified"`
	}
	if err := json.Unmarshal([]byte(out), &second); err != nil {
		t.Fatalf("migrate --incremental --json: %v\n%s", err, out)
	}
	p := second.Run.Progress
	if second.Run.Status != store.RunCompleted || second.Run.Pass != 2 || second.Run.BaseRunID != first.Run.ID || len(second.Run.Passes) != 2 {
		t.Fatalf("the pass = pass %d of %d, base %q, status %s", second.Run.Pass, len(second.Run.Passes), second.Run.BaseRunID, second.Run.Status)
	}
	if p.Posted != added || p.Carried != imported || p.Failed != 0 || p.Held != 0 || second.Verified == nil || !*second.Verified {
		t.Errorf("the pass posted %d and carried %d (failed %d, held %d); want %d and %d, with a verified audit log", p.Posted, p.Carried, p.Failed, p.Held, added, imported)
	}

	// Status tells the passes apart.
	out, errOut, code = e.run("status", "--yes")
	if code != 0 {
		t.Fatalf("status (%d):\n%s\n%s", code, out, errOut)
	}
	testutil.Golden(t, "status_incremental_plain", testutil.Plain(steady(out)))
	out, _, _ = e.run("dashboard", "--yes")
	if !strings.Contains(out, "Pass") || !strings.Contains(out, "incremental; follows run "+first.Run.ID) {
		t.Errorf("dashboard does not say which pass it shows:\n%s", out)
	}

	// Each pass has its own audit log.
	count := func(args ...string) int {
		t.Helper()
		out, errOut, code := e.run(append([]string{"audit", "--op", "message.import", "--limit", "0", "--json"}, args...)...)
		if code != 0 {
			t.Fatalf("audit %v (%d):\n%s", args, code, errOut)
		}
		return strings.Count(out, "\n")
	}
	if n := count(); n != added {
		t.Errorf("audit of the latest pass lists %d imports, want %d", n, added)
	}
	if n := count("--pass", "2"); n != added {
		t.Errorf("audit --pass 2 lists %d imports, want %d", n, added)
	}
	if n := count("--pass", "1"); n != imported {
		t.Errorf("audit --pass 1 lists %d imports, want %d", n, imported)
	}
	if _, errOut, code := e.run("audit", "--pass", "3", "--yes"); code != fail.ExitUsage || !strings.Contains(errOut, "passes 1 to 2") {
		t.Errorf("audit --pass 3: exit %d\n%s", code, errOut)
	}
	if _, _, code := e.run("audit", "--pass", "1", "--run", first.Run.ID, "--yes"); code != fail.ExitUsage {
		t.Errorf("audit --pass with --run: exit %d", code)
	}
	if out, _, code := e.run("audit", "--pass", "1", "--verify", "--yes"); code != 0 || !strings.Contains(out, "verified") {
		t.Errorf("audit --pass 1 --verify (%d):\n%s", code, out)
	}

	// Finalize works on the latest pass, and the next pass reopens what it
	// must.
	if out, errOut, code := e.run("finalize", "--yes"); code != 0 {
		t.Fatalf("finalize (%d):\n%s\n%s", code, out, errOut)
	}
	out, errOut, code = e.run("migrate", "--incremental", "--yes")
	if code != 0 || !strings.Contains(out, "Nothing new to import") {
		t.Fatalf("migrate --incremental after the finalize, with nothing new (%d):\n%s\n%s", code, out, errOut)
	}
}

// After a finalize, the preflight says which channels the pass puts back
// into migration mode, and under the legacy strategy it refuses.
func TestIncrementalPreflightAfterFinalize(t *testing.T) {
	e := newPassEnv(t, "small")
	facts := fixtures.LaterFacts("small-later")
	if out, errOut, code := e.run("migrate", "--yes"); code != 0 {
		t.Fatalf("migrate failed (%d):\n%s\n%s", code, out, errOut)
	}
	if out, errOut, code := e.run("finalize", "--yes"); code != 0 {
		t.Fatalf("finalize failed (%d):\n%s\n%s", code, out, errOut)
	}
	e.useExport(filepath.Join(e.dir, "small-later.zip"))

	out, errOut, code := e.run("preflight", "--incremental", "--json")
	var rep preflight.Report
	if err := json.Unmarshal([]byte(out), &rep); err != nil || code != 0 {
		t.Fatalf("preflight --incremental --json (%d): %v\n%s\n%s", code, err, out, errOut)
	}
	inc := rep.Incremental
	if inc.BaseStatus != "finalized" || inc.Reopened == 0 {
		t.Fatalf("incremental report after a finalize = %+v", inc)
	}
	reopens := false
	for _, w := range rep.Warnings {
		reopens = reopens || w.Code == "incremental.reopens"
	}
	if !reopens {
		t.Errorf("no warning that finalized channels are reopened: %+v", rep.Warnings)
	}
	for _, c := range inc.Channels {
		if c.SlackName == facts.QuietChannel && (c.Reopens || c.New != 0) {
			t.Errorf("the quiet channel would be reopened: %+v", c)
		}
		if c.Continued && c.SlackName != facts.QuietChannel && !c.Reopens {
			t.Errorf("channel #%s has new messages and is not reopened: %+v", c.SlackName, c)
		}
	}
	out, _, _ = e.run("preflight", "--incremental", "--yes")
	testutil.Golden(t, "preflight_incremental_finalized_plain", testutil.Plain(steady(out)))

	// The legacy strategy has no way back into a finalized channel.
	raw, err := os.ReadFile(e.config)
	if err != nil {
		t.Fatal(err)
	}
	m, err := config.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	m.Load.Strategy = config.StrategyLegacy
	legacy, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.config, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, code = e.run("preflight", "--incremental", "--json")
	var blocked preflight.Report
	if err := json.Unmarshal([]byte(out), &blocked); err != nil {
		t.Fatalf("preflight --incremental --json (legacy): %v\n%s", err, out)
	}
	found := false
	for _, b := range blocked.Blockers {
		found = found || b.Code == "incremental.legacy_finalized"
	}
	if code != fail.ExitBlockers || !found {
		t.Fatalf("legacy preflight after a finalize (%d): blockers %+v", code, blocked.Blockers)
	}
	if _, _, code := e.run("migrate", "--incremental", "--yes"); code != fail.ExitBlockers {
		t.Fatalf("legacy migrate --incremental after a finalize: exit %d, want %d", code, fail.ExitBlockers)
	}
	latest, ok, err := e.store.LatestRun(context.Background(), "fixture")
	if err != nil || !ok || latest.Pass != 1 || latest.Status != store.RunFinalized {
		t.Fatalf("a refused pass was started: latest run %+v, %v", latest, err)
	}
}

package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/devstack"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/fakegraph"
	"github.com/bgfdev/taut/internal/fakeslack"
	"github.com/bgfdev/taut/internal/fixtures"
	"github.com/bgfdev/taut/internal/local"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/slack/export"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/transform"
)

// localEnv is a migration in local mode driven through the real binary: a
// config file written by `taut init --local`, the fake tenant and file host
// in this process, and taut itself as a child process, so that it can be
// killed the way an operator's machine kills things.
type localEnv struct {
	t      *testing.T
	bin    string
	dir    string
	config string
	fake   *fakegraph.Server
	export string
	env    []string
}

func newLocalEnv(t *testing.T, fixture string) *localEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("builds the binary and starts a PostgreSQL server")
	}
	download := os.Getenv("TAUT_TEST_LOCAL_DOWNLOAD") == "1"
	if _, err := local.FindPostgres(t.Context(), local.Search{Download: download}); err != nil {
		t.Skipf("no PostgreSQL for local mode on this machine (%s); install one or set TAUT_TEST_LOCAL_DOWNLOAD=1", fail.From(err).Code)
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "taut")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", bin, "../../cmd/taut")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building taut: %v\n%s", err, out)
	}
	path, err := fixtures.Write(fixture, dir)
	if err != nil {
		t.Fatal(err)
	}
	fake := fakegraph.New(devstack.Seed(fixture))
	gsrv := httptest.NewServer(fake)
	t.Cleanup(gsrv.Close)
	sizes, err := fakeslack.FilesFromExport(path, 64<<10)
	if err != nil {
		t.Fatal(err)
	}
	fsrv := httptest.NewServer(fakeslack.NewFileHost(sizes))
	t.Cleanup(fsrv.Close)

	e := &localEnv{t: t, bin: bin, dir: dir, config: filepath.Join(dir, "taut.yaml"), fake: fake, export: path}
	e.env = append(os.Environ(),
		"TERM=dumb", "TAUT_GLYPHS=unicode", "NO_COLOR=1",
		"TAUT_ENTRA_CLIENT_SECRET="+fakegraph.DefaultSecret,
		"TAUT_ALLOW_TEST_ENDPOINTS=1", "TAUT_SLACK_FILES_URL="+fsrv.URL,
		// Slow enough that a kill lands mid-import, fast enough to finish.
		"TAUT_GRAPH_CHANNEL_RPS=150", "TAUT_GRAPH_CHANNEL_MAX=150",
		"TAUT_BUDGET_GRAPH_RPS=1000", "TAUT_BUDGET_SHAREPOINT_RPS=1000", "TAUT_BUDGET_SLACK_RPS=1000",
	)
	args := []string{"init", "--yes", "--local", "--no-slack-api",
		"--name", "local-kill", "--export", path,
		"--tenant-id", fakegraph.DefaultTenantID, "--client-id", fakegraph.DefaultClientID,
		"--client-secret-env", "TAUT_ENTRA_CLIENT_SECRET",
		"--team", "Fixture Corp", "--owner", devstack.OwnerUPN, "--unmapped-account", devstack.FormerMemberUPN,
		"--authority", gsrv.URL, "--graph-url", gsrv.URL,
	}
	if download {
		args = append(args, "--download-postgres")
	}
	if out, code := e.run(args...); code != 0 {
		t.Fatalf("init --local failed (%d):\n%s", code, out)
	}
	return e
}

func (e *localEnv) command(args ...string) *exec.Cmd {
	cmd := exec.Command(e.bin, append(args, "--config", e.config)...) //nolint:gosec // the binary this test built
	cmd.Env = e.env
	cmd.Dir = e.dir
	return cmd
}

// run runs taut to the end and returns its combined output and exit code.
func (e *localEnv) run(args ...string) (string, int) {
	e.t.Helper()
	out, err := e.command(args...).CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	if ee, ok := err.(*exec.ExitError); ok { //nolint:errorlint // exec returns it unwrapped
		return string(out), ee.ExitCode()
	}
	e.t.Fatalf("running taut %v: %v", args, err)
	return "", -1
}

// expected counts the messages the tenant must hold, from the export alone.
func (e *localEnv) expected() int {
	e.t.Helper()
	ex, err := export.Open(e.export)
	if err != nil {
		e.t.Fatal(err)
	}
	defer ex.Close()
	chans, err := ex.Channels()
	if err != nil {
		e.t.Fatal(err)
	}
	total := 0
	for _, ch := range chans {
		if ch.Kind != slack.KindPublic {
			continue
		}
		for msg, err := range ex.Messages(ch) {
			if err == nil && transform.Classify(msg).Disposition != transform.Skip {
				total++
			}
		}
	}
	return total
}

func (e *localEnv) inTenant() int {
	n := 0
	for _, team := range e.fake.Teams() {
		for _, c := range e.fake.Channels(team.ID) {
			msgs := e.fake.Messages(team.ID, c.ID)
			n += len(msgs)
			for i := 1; i < len(msgs); i++ {
				if !msgs[i].CreatedAt.After(msgs[i-1].CreatedAt) {
					e.t.Errorf("channel %q: duplicate or out-of-order message at %d", c.DisplayName, i)
				}
			}
		}
	}
	return n
}

// The local-mode acceptance test. `taut migrate` is killed with SIGKILL
// twice in the middle of importing the medium fixture, which also orphans
// the PostgreSQL server it started. Each time, running it again picks up
// where the ledger says it was. The tenant ends with every message once.
func TestLocalModeKillAndResume(t *testing.T) {
	e := newLocalEnv(t, "medium")
	want := e.expected()
	state := filepath.Join(e.dir, ".taut")

	for round, after := range []int{want / 6, want / 2} {
		cmd := e.command("migrate", "--yes")
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(3 * time.Minute)
		for e.fake.Stats().ImportsAccepted < after {
			if time.Now().After(deadline) {
				_ = cmd.Process.Kill()
				t.Fatalf("round %d: only %d messages were imported in three minutes:\n%s", round, e.fake.Stats().ImportsAccepted, out.String())
			}
			time.Sleep(10 * time.Millisecond)
		}
		if round == 0 {
			// While it runs: a second migrate is refused, and status reads
			// the running process's ledger without disturbing it.
			if o, code := e.run("migrate", "--yes"); code != fail.ExitUsage || !strings.Contains(o, "state directory is in use") {
				t.Errorf("a second migrate on the same state directory (%d):\n%s", code, o)
			}
			if o, code := e.run("status", "--json"); code != 0 || !strings.Contains(o, `"status": "running"`) {
				t.Errorf("status beside a running migrate (%d):\n%s", code, o)
			}
		}
		if err := cmd.Process.Signal(syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait()
		if got := e.inTenant(); got >= want {
			t.Fatalf("round %d: the import had finished (%d messages) before the kill; nothing was tested", round, got)
		}
		// The killed process could not stop its PostgreSQL.
		if _, err := os.Stat(filepath.Join(state, "postgres", "data", "postmaster.pid")); err != nil {
			t.Logf("round %d: no PostgreSQL was left behind (%v)", round, err)
		}
	}

	out, code := e.run("migrate", "--yes", "--json")
	if code != 0 {
		t.Fatalf("migrate after two kills failed (%d):\n%s", code, out)
	}
	var res struct {
		Run      controller.RunView `json:"run"`
		Verified *bool              `json:"auditLogVerified"`
	}
	if err := json.Unmarshal([]byte(out[strings.Index(out, "{"):]), &res); err != nil {
		t.Fatalf("migrate --json: %v\n%s", err, out)
	}
	p := res.Run.Progress
	if res.Run.Status != store.RunCompleted || p.Posted != want || p.Failed != 0 || p.Held != 0 {
		t.Fatalf("after two kills: status %s, posted %d of %d, failed %d, held %d", res.Run.Status, p.Posted, want, p.Failed, p.Held)
	}
	if res.Verified == nil || !*res.Verified {
		t.Error("the audit log did not verify after two kills")
	}
	if got := e.inTenant(); got != want {
		t.Errorf("the tenant holds %d messages, want %d", got, want)
	}
	stats := e.fake.Stats()
	if stats.ImportsAccepted != want {
		t.Errorf("Graph accepted %d imports for %d messages: a message was written twice", stats.ImportsAccepted, want)
	}
	t.Logf("recovered after two SIGKILLs: %d conflicts (409), %d adopted", stats.Conflicts, p.Adopted)

	// The rest of the commands, against the state directory.
	if o, code := e.run("status", "--yes"); code != 0 || !strings.Contains(o, "Import complete") {
		t.Errorf("status (%d):\n%s", code, o)
	}
	if o, code := e.run("dashboard", "--yes"); code != 0 || !strings.Contains(o, "Imported into Teams") {
		t.Errorf("dashboard (%d):\n%s", code, o)
	}
	if o, code := e.run("finalize", "--yes", "--detach"); code != fail.ExitUsage || !strings.Contains(o, "does not apply in local mode") {
		t.Errorf("finalize --detach in local mode (%d):\n%s", code, o)
	}
	if o, code := e.run("finalize", "--yes"); code != 0 || !strings.Contains(o, "Migration finalized") {
		t.Fatalf("finalize (%d):\n%s", code, o)
	}
	if o, code := e.run("audit", "--verify", "--yes"); code != 0 || !strings.Contains(o, "Audit log verified") {
		t.Errorf("audit --verify (%d):\n%s", code, o)
	}
	if o, code := e.run("doctor"); code != 0 || !strings.Contains(o, "PostgreSQL") || !strings.Contains(o, "State directory") || strings.Contains(o, "Controller") {
		t.Errorf("doctor in local mode (%d):\n%s", code, o)
	}
	for _, team := range e.fake.Teams() {
		for _, c := range e.fake.Channels(team.ID) {
			if c.MigrationMode != "completed" {
				t.Errorf("channel %q is %q after the finalize", c.DisplayName, c.MigrationMode)
			}
		}
	}
	// Nothing of taut's is left running.
	if _, ok := local.ReadRuntime(state); ok {
		t.Error("a runtime file is left in the state directory")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := os.Stat(filepath.Join(state, "postgres", "data", "postmaster.pid")); err == nil {
		// Give a last shutdown a moment before calling it a leak.
		for ctx.Err() == nil {
			if _, err := os.Stat(filepath.Join(state, "postgres", "data", "postmaster.pid")); err != nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if ctx.Err() != nil {
			t.Error("a PostgreSQL server is still running in the state directory")
		}
	}
}

// `taut init --local` writes a config file with no bucket and no
// controller, says what local mode means, and the commands that cannot
// apply to it say so before starting anything.
func TestInitLocal(t *testing.T) {
	e := newEnv(t, "medium")
	e.init("--local")
	raw, err := os.ReadFile(e.config)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if !strings.Contains(text, "local:\n  enabled: true") || strings.Contains(text, "staging:") || strings.Contains(text, "controller:") {
		t.Fatalf("the config file init --local wrote:\n%s", text)
	}
	if strings.Contains(text, "download") {
		t.Errorf("a download was allowed without --download-postgres:\n%s", text)
	}

	out, _, code := e.run("init", "--yes", "--force", "--local", "--download-postgres",
		"--name", "fixture", "--export", filepath.Join(e.dir, "medium.zip"),
		"--tenant-id", fakegraph.DefaultTenantID, "--client-id", fakegraph.DefaultClientID,
		"--client-secret-env", "TAUT_ENTRA_CLIENT_SECRET",
		"--team", "Fixture Corp", "--owner", devstack.OwnerUPN, "--unmapped-account", devstack.FormerMemberUPN)
	if code != 0 || !strings.Contains(out, "Local mode") || !strings.Contains(out, "downloaded once from Maven Central") || !strings.Contains(out, "not separated from the worker") {
		t.Fatalf("init --local --download-postgres (%d):\n%s", code, out)
	}

	for _, cmd := range []string{"migrate", "finalize"} {
		_, errOut, code := e.run(cmd, "--yes", "--detach")
		if code != fail.ExitUsage || !strings.Contains(errOut, "does not apply in local mode") {
			t.Errorf("%s --detach in local mode: exit %d\n%s", cmd, code, errOut)
		}
	}
	// Nothing was started for a command that was refused.
	if _, err := os.Stat(filepath.Join(e.dir, ".taut")); err == nil {
		t.Error("a refused command created the state directory")
	}

	// On a machine with no PostgreSQL the check fails, with its reason; it
	// is the checks that are there that matter here.
	out, _, code = e.run("doctor")
	if !strings.Contains(out, "State directory") || !strings.Contains(out, "PostgreSQL") || !strings.Contains(out, "not needed in local mode") || strings.Contains(out, "Controller") {
		t.Errorf("doctor for a local-mode config file (%d):\n%s", code, out)
	}
}

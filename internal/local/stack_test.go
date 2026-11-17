package local_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/devstack"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/fakegraph"
	"github.com/bgfdev/taut/internal/fakeslack"
	"github.com/bgfdev/taut/internal/fixtures"
	"github.com/bgfdev/taut/internal/local"
	"github.com/bgfdev/taut/internal/secret"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/slack/export"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/transform"
)

// needPostgres skips a test on a machine with no PostgreSQL for local mode
// to run. Set TAUT_TEST_LOCAL_DOWNLOAD=1 to let the tests download one, as
// local.postgres.download does; scripts/test-all.sh sets it.
func needPostgres(t *testing.T) bool {
	t.Helper()
	download := os.Getenv("TAUT_TEST_LOCAL_DOWNLOAD") == "1"
	if _, err := local.FindPostgres(t.Context(), local.Search{Download: download}); err != nil {
		t.Skipf("no PostgreSQL for local mode on this machine (%s); install one or set TAUT_TEST_LOCAL_DOWNLOAD=1", fail.From(err).Code)
	}
	return download
}

// world is a fake tenant and a fake Slack file host, and a local-mode
// config file that points at them.
type world struct {
	t      *testing.T
	fake   *fakegraph.Server
	m      *config.Config
	export string
	env    map[string]string
}

func newWorld(t *testing.T, fixture string) *world {
	t.Helper()
	download := needPostgres(t)
	dir := t.TempDir()
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
	t.Setenv("TAUT_ENTRA_CLIENT_SECRET", fakegraph.DefaultSecret)

	m := config.Default()
	m.Metadata.Name = "local-" + fixture
	m.Source.Slack.Export = path
	m.Target.Entra.TenantID = fakegraph.DefaultTenantID
	m.Target.Entra.ClientID = fakegraph.DefaultClientID
	m.Target.Entra.Credential = config.Credential{ClientSecret: secret.Ref{Env: "TAUT_ENTRA_CLIENT_SECRET"}}
	m.Target.Entra.Authority, m.Target.Entra.GraphBaseURL = gsrv.URL, gsrv.URL
	team := config.DefaultTeam()
	team.Name, team.Owner = "Fixture Corp", devstack.OwnerUPN
	m.Target.Teams = []config.TeamSpec{team}
	m.Identity.Unmapped.Policy = config.PolicyFormerMember
	m.Identity.Unmapped.Account = devstack.FormerMemberUPN
	m.Local.Enabled = true
	m.Local.Postgres.Download = download
	m.SetBaseDir(dir)
	if issues := m.Validate(); issues.HasErrors() {
		t.Fatal(issues.Err())
	}
	return &world{t: t, fake: fake, m: m, export: path, env: map[string]string{
		"TAUT_ALLOW_TEST_ENDPOINTS": "1",
		"TAUT_SLACK_FILES_URL":      fsrv.URL,
		// Fast enough that the test is not about waiting; the proxy still
		// admits every request.
		"TAUT_GRAPH_CHANNEL_RPS": "400", "TAUT_GRAPH_CHANNEL_MAX": "800",
		"TAUT_BUDGET_GRAPH_RPS": "3000", "TAUT_BUDGET_SHAREPOINT_RPS": "3000", "TAUT_BUDGET_SLACK_RPS": "3000",
	}}
}

func (w *world) open(work bool) (*local.Stack, error) {
	return local.Open(w.t.Context(), local.Options{
		Config: w.m, Work: work, Getenv: func(k string) string { return w.env[k] },
	})
}

func (w *world) mustOpen(work bool) *local.Stack {
	w.t.Helper()
	s, err := w.open(work)
	if err != nil {
		w.t.Fatalf("opening the local stack: %v", err)
	}
	w.t.Cleanup(s.Close)
	return s
}

// start creates the run and uploads the export, as `taut migrate` does in
// local mode.
func (w *world) start(c *controller.Client) controller.RunView {
	w.t.Helper()
	yaml, err := local.Absolute(w.m).Marshal()
	if err != nil {
		w.t.Fatal(err)
	}
	run, err := c.CreateRun(w.t.Context(), yaml, false)
	if err != nil {
		w.t.Fatal(err)
	}
	if run.Status == store.RunAwaitingExport {
		f, err := os.Open(w.export)
		if err != nil {
			w.t.Fatal(err)
		}
		defer f.Close()
		if _, err := c.UploadExport(w.t.Context(), run.ID, f, ""); err != nil {
			w.t.Fatal(err)
		}
	}
	return run
}

func (w *world) await(c *controller.Client, id string, limit time.Duration, done func(controller.RunView) bool) controller.RunView {
	w.t.Helper()
	deadline := time.Now().Add(limit)
	for {
		v, err := c.Run(w.t.Context(), id)
		if err != nil {
			w.t.Fatal(err)
		}
		if done(v) {
			return v
		}
		if v.Status == store.RunFailed || time.Now().After(deadline) {
			w.t.Fatalf("run is %s after %s; problems %+v", v.Status, limit, v.Progress)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// expected counts, from the export alone, the messages of each public
// channel that must be in the tenant.
func (w *world) expected() (byChannel map[string]int, total int) {
	w.t.Helper()
	ex, err := export.Open(w.export)
	if err != nil {
		w.t.Fatal(err)
	}
	defer ex.Close()
	chans, err := ex.Channels()
	if err != nil {
		w.t.Fatal(err)
	}
	byChannel = map[string]int{}
	for _, ch := range chans {
		if ch.Kind != slack.KindPublic {
			continue
		}
		name := ch.Name
		if ch.IsGeneral {
			name = "General"
		}
		byChannel[name] = 0
		for msg, err := range ex.Messages(ch) {
			if err == nil && transform.Classify(msg).Disposition != transform.Skip {
				byChannel[name]++
				total++
			}
		}
	}
	return byChannel, total
}

// verify asserts the tenant holds exactly the export's messages, once each.
func (w *world) verify(mode string) {
	t := w.t
	t.Helper()
	want, total := w.expected()
	teams := w.fake.Teams()
	if len(teams) != 1 {
		t.Fatalf("the tenant has %d teams, want 1", len(teams))
	}
	got := 0
	for _, c := range w.fake.Channels(teams[0].ID) {
		msgs := w.fake.Messages(teams[0].ID, c.ID)
		got += len(msgs)
		if n, ok := want[c.DisplayName]; !ok || n != len(msgs) {
			t.Errorf("channel %q holds %d messages, want %d", c.DisplayName, len(msgs), n)
		}
		if c.MigrationMode != mode {
			t.Errorf("channel %q is %q, want %q", c.DisplayName, c.MigrationMode, mode)
		}
		for i := 1; i < len(msgs); i++ {
			if !msgs[i].CreatedAt.After(msgs[i-1].CreatedAt) {
				t.Errorf("channel %q: duplicate or out-of-order message at %d", c.DisplayName, i)
			}
		}
	}
	if got != total {
		t.Errorf("the tenant holds %d messages, want %d", got, total)
	}
}

// The medium fixture, end to end in one process: import, finalize, audit.
// Every request to the fake tenant went through the egress proxy with a
// token from the broker, because the worker has no other way out.
func TestLocalMigrateFinalizeAudit(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a PostgreSQL server")
	}
	w := newWorld(t, "medium")
	s := w.mustOpen(true)
	if !s.Owner || s.URL == "" || s.Token == "" {
		t.Fatalf("stack = %+v", s)
	}
	c := s.Client()

	run := w.start(c)
	v := w.await(c, run.ID, 4*time.Minute, func(v controller.RunView) bool { return v.Status == store.RunCompleted })
	_, total := w.expected()
	if v.Progress.Posted != total || v.Progress.Failed != 0 || v.Progress.Held != 0 {
		t.Fatalf("posted %d (failed %d, held %d), want %d", v.Progress.Posted, v.Progress.Failed, v.Progress.Held, total)
	}
	if v.Progress.FilesUploaded == 0 || v.Progress.FilesFailed != 0 {
		t.Errorf("files: %d uploaded, %d failed", v.Progress.FilesUploaded, v.Progress.FilesFailed)
	}
	w.verify("inProgress")

	// The worker reached Microsoft only as the application, with tokens
	// the broker issued: nothing arrived without a bearer token, and the
	// process holds the secret only inside the broker's allow list.
	for _, r := range w.fake.Requests() {
		if strings.HasPrefix(r.Path, "/v1.0/") && r.AppID != fakegraph.DefaultClientID {
			t.Fatalf("a Graph request arrived as application %q: %s %s", r.AppID, r.Method, r.Path)
		}
	}

	// The controller's API needs the stack's token.
	if _, err := controller.NewClient(s.URL, "wrong").Run(t.Context(), run.ID); fail.ExitCode(err) != fail.ExitPermission {
		t.Errorf("the local controller answered a request with a wrong token: %v", err)
	}

	if _, err := c.Finalize(t.Context(), run.ID, controller.FinalizeRequest{}); err != nil {
		t.Fatal(err)
	}
	v = w.await(c, run.ID, 2*time.Minute, func(v controller.RunView) bool { return v.Status == store.RunFinalized && v.PurgedAt != nil })
	w.verify("completed")
	if ok, problem, err := c.VerifyAuditLog(t.Context(), run.ID); err != nil || !ok {
		t.Fatalf("audit log: ok=%v %s %v", ok, problem, err)
	}
	// Staging was purged with the finalize; the key stays for later runs.
	left := 0
	_ = filepath.WalkDir(filepath.Join(s.Dir, "staging"), func(_ string, d os.DirEntry, _ error) error {
		if d != nil && !d.IsDir() {
			left++
		}
		return nil
	})
	if left != 0 {
		t.Errorf("%d staged objects are left after the finalize", left)
	}

	// State is private.
	for _, name := range []string{"", "staging.key", "runtime.json", "postgres/password", "taut.log"} {
		st, err := os.Stat(filepath.Join(s.Dir, name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if st.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s has mode %04o; the state directory holds a key and a ledger", name, st.Mode().Perm())
		}
	}

	// A second run of the stack finds the same ledger.
	s.Close()
	if _, ok := local.ReadRuntime(s.Dir); ok {
		t.Error("a stopped stack left its runtime file behind")
	}
	again := w.mustOpen(false)
	latest, err := again.Client().LatestRun(t.Context(), w.m.Metadata.Name)
	if err != nil || latest.ID != run.ID || latest.Status != store.RunFinalized {
		t.Fatalf("after a restart the latest run is %+v, %v", latest, err)
	}
}

// One state directory, one owner. A second process that wants to work is
// refused; one that only reads attaches to the first's controller.
func TestLocalStateDirectoryHasOneOwner(t *testing.T) {
	if testing.Short() {
		t.Skip("starts a PostgreSQL server")
	}
	w := newWorld(t, "small")
	owner := w.mustOpen(true)

	_, err := w.open(true)
	if fe := fail.From(err); err == nil || fe.Code != "local.in_use" || fail.ExitCode(err) != fail.ExitUsage || fe.Next == "" {
		t.Fatalf("a second working stack on one state directory: %v", err)
	}
	reader, err := w.open(false)
	if err != nil {
		t.Fatalf("attaching to a running stack: %v", err)
	}
	if reader.Owner || reader.URL != owner.URL || reader.PID != os.Getpid() {
		t.Errorf("attached stack = %+v, owner at %s", reader, owner.URL)
	}
	if resp, err := http.Get(reader.URL + "/healthz"); err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("the owner's controller does not answer the attached process: %v", err)
	}
	// Closing an attached stack does not stop the owner's.
	reader.Close()
	if _, err := owner.Client().LatestRun(t.Context(), "nothing"); fail.From(err).Code != "run.not_found" {
		t.Errorf("the owner's stack after an attached one closed: %v", err)
	}

	// Once the owner is gone the directory is free again.
	owner.Close()
	next := w.mustOpen(true)
	if !next.Owner {
		t.Error("the state directory was not released")
	}
}

func TestFindPostgres(t *testing.T) {
	ctx := context.Background()
	nowhere := func(string) (string, error) { return "", os.ErrNotExist }

	// A directory that is not an installation.
	_, err := local.FindPostgres(ctx, local.Search{Binaries: t.TempDir(), LookPath: nowhere})
	if fe := fail.From(err); fe.Code != "local.postgres_not_found" || fe.Next == "" {
		t.Errorf("local.postgres.binaries naming an empty directory: %v", err)
	}

	pg, err := local.FindPostgres(ctx, local.Search{})
	if err != nil {
		// Nothing installed here: the error must say what to do, and
		// allowing a download must turn it into a plan to download.
		if fe := fail.From(err); fe.Code != "local.postgres_missing" || !strings.Contains(fe.Next, "local.postgres.download") {
			t.Fatalf("no PostgreSQL: %v", err)
		}
		pg, err = local.FindPostgres(ctx, local.Search{Download: true})
		if err != nil || !pg.NeedsDownload() {
			t.Fatalf("with a download allowed: %+v, %v", pg, err)
		}
		return
	}
	if pg.NeedsDownload() || pg.Major < local.MinPostgres || pg.Dir == "" {
		t.Fatalf("found %+v", pg)
	}
	// Naming it in the config file finds the same one, with or without
	// the bin directory.
	for _, dir := range []string{pg.Dir, filepath.Join(pg.Dir, "bin")} {
		named, err := local.FindPostgres(ctx, local.Search{Binaries: dir, LookPath: nowhere})
		if err != nil || named.Dir != pg.Dir || named.Origin != "config" || named.Major != pg.Major {
			t.Errorf("local.postgres.binaries = %s: %+v, %v", dir, named, err)
		}
	}
	// A ledger made by another version prefers an installation of that
	// version; with none, the first found is still named, and starting it
	// is what refuses.
	if same, err := local.FindPostgres(ctx, local.Search{Major: pg.Major}); err != nil || same.Major != pg.Major {
		t.Errorf("looking for the ledger's own version: %+v, %v", same, err)
	}
	// An installation is never passed over for a download.
	if again, err := local.FindPostgres(ctx, local.Search{Download: true}); err != nil || again.NeedsDownload() {
		t.Errorf("with a download allowed and PostgreSQL installed: %+v, %v", again, err)
	}
}

func TestAbsoluteSecretPaths(t *testing.T) {
	m := config.Default()
	m.SetBaseDir("/srv/migration")
	m.Target.Entra.Credential.Certificate = secret.Ref{File: "./app.pem"}
	m.Source.Slack.API.Token = secret.Ref{Env: "TAUT_SLACK_TOKEN"}
	abs := local.Absolute(m)
	if got := abs.Target.Entra.Credential.Certificate.File; got != "/srv/migration/app.pem" {
		t.Errorf("certificate path = %q", got)
	}
	if abs.Source.Slack.API.Token.Env != "TAUT_SLACK_TOKEN" || abs.Source.Slack.API.Token.File != "" {
		t.Errorf("an environment reference was changed: %+v", abs.Source.Slack.API.Token)
	}
	if m.Target.Entra.Credential.Certificate.File != "./app.pem" {
		t.Error("the config file itself was changed")
	}
}

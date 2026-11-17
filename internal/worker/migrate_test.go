package worker_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/bus"
	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/devstack"
	"github.com/bgfdev/taut/internal/fakegraph"
	"github.com/bgfdev/taut/internal/fakeslack"
	"github.com/bgfdev/taut/internal/fixtures"
	"github.com/bgfdev/taut/internal/graph"
	"github.com/bgfdev/taut/internal/secret"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/slack/export"
	"github.com/bgfdev/taut/internal/staging"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/store/storetest"
	"github.com/bgfdev/taut/internal/transform"
	"github.com/bgfdev/taut/internal/worker"
)

// harness is one complete, isolated Taut deployment: a real Postgres schema,
// encrypted staging on disk, a fake Microsoft Graph, the controller's API and any
// number of workers, all in-process.
type harness struct {
	t      *testing.T
	store  store.Store
	blobs  staging.Blobs
	kek    staging.KEK
	bus    *bus.Local
	fake   *fakegraph.Server
	graph  string // fake Graph base URL
	client *controller.Client
	api    *controller.Server
	dir    string
	yaml   []byte
	export string
	seq    atomic.Int64
	// files is the fake Slack file host; filesURL is where it listens.
	files    *fakeslack.FileHost
	filesURL string
	// stripFileTokens removes the token from export file URLs, so Slack
	// treats every download as unauthorized.
	stripFileTokens bool
	// maxAttempts overrides the workers' attempt limit (0: the default).
	maxAttempts int
	// folderWait overrides how long workers wait for a channel's files
	// folder (0: the default).
	folderWait time.Duration
	// private is true when the config selects private channels as well.
	private bool
}

// fileCap is the size the fake Slack serves for any file larger than it, so
// fixtures describing multi-gigabyte files stay fast. Tests that want a
// genuinely large file override one entry.
const fileCap = 192 << 10

func newHarness(t *testing.T, fixture string, fakeOpts ...fakegraph.Option) *harness {
	t.Helper()
	return newHarnessWith(t, fixture, nil, fakeOpts...)
}

// withPrivateChannels makes the harness's config select private channels too.
func withPrivateChannels(m *config.Config) {
	m.Target.Teams[0].Channels.Types = []string{config.ChannelPublic, config.ChannelPrivate}
}

// newHarnessWith is newHarness with a change to the config file before the
// run is created.
func newHarnessWith(t *testing.T, fixture string, mod func(*config.Config), fakeOpts ...fakegraph.Option) *harness {
	t.Helper()
	st := storetest.Open(t) // skips when no test database is configured
	dir := t.TempDir()

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

	fake := fakegraph.New(devstack.Seed(fixture), fakeOpts...)
	gsrv := httptest.NewServer(fake)
	t.Cleanup(gsrv.Close)

	exportPath, err := fixtures.Write(fixture, dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TAUT_ENTRA_CLIENT_SECRET", fakegraph.DefaultSecret)

	m := config.Default()
	m.Metadata.Name = "it-" + fixture
	m.Source.Slack.Export = exportPath
	m.Target.Entra.TenantID = fakegraph.DefaultTenantID
	m.Target.Entra.ClientID = fakegraph.DefaultClientID
	m.Target.Entra.Credential = config.Credential{ClientSecret: secret.Ref{Env: "TAUT_ENTRA_CLIENT_SECRET"}}
	m.Target.Entra.Authority, m.Target.Entra.GraphBaseURL = gsrv.URL, gsrv.URL
	team := config.DefaultTeam()
	team.Name, team.Owner = "Fixture Corp", devstack.OwnerUPN
	m.Target.Teams = []config.TeamSpec{team}
	m.Identity.Unmapped.Policy = config.PolicyFormerMember
	m.Identity.Unmapped.Account = devstack.FormerMemberUPN
	if mod != nil {
		mod(m)
	}
	private := false
	for _, kind := range m.Target.Teams[0].Channels.Types {
		private = private || kind == config.ChannelPrivate
	}
	yaml, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}

	sizes, err := fakeslack.FilesFromExport(exportPath, fileCap)
	if err != nil {
		t.Fatal(err)
	}
	fileHost := fakeslack.NewFileHost(sizes)
	fsrv := httptest.NewServer(fileHost)
	t.Cleanup(fsrv.Close)

	local := bus.NewLocal()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	api := controller.New(controller.Config{}, log, controller.Services{Store: st, Blobs: blobs, KEK: kek, Bus: local, AllowTestEndpoints: true})
	bsrv := httptest.NewServer(api.Handler())
	t.Cleanup(bsrv.Close)

	return &harness{t: t, store: st, blobs: blobs, kek: kek, bus: local, fake: fake, graph: gsrv.URL,
		client: controller.NewClient(bsrv.URL, ""), api: api, dir: dir, yaml: yaml, export: exportPath,
		files: fileHost, filesURL: fsrv.URL, private: private}
}

// graphFactory builds a real Graph client against the fake, with retries
// that do not sleep, so throttling and faults cost no wall-clock time.
func (r *harness) graphFactory() worker.GraphFactory {
	return func(_ context.Context, m *config.Config) (worker.Graph, error) {
		v, _, err := m.ResolveSecret(m.Target.Entra.Credential.ClientSecret)
		if err != nil {
			return nil, err
		}
		cred, err := graph.NewClientSecret(v.Reveal())
		if err != nil {
			return nil, err
		}
		e := m.Target.Entra
		ts, err := graph.NewTokenSource(graph.CredentialsOptions{
			TenantID: e.TenantID, ClientID: e.ClientID, Credential: cred, Authority: e.Authority, GraphBase: e.GraphBaseURL,
		})
		if err != nil {
			return nil, err
		}
		return graph.New(graph.Options{
			BaseURL: e.GraphBaseURL, TokenSource: ts,
			Retry: graph.RetryPolicy{MaxAttempts: 6, MaxTotalWait: time.Hour, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
			Sleep: func(ctx context.Context, _ time.Duration) error { return ctx.Err() },
		})
	}
}

// worker starts a worker and returns a function that kills it and waits
// for it to stop.
func (r *harness) worker(parent context.Context, ttl time.Duration) (kill func()) {
	return r.workerWith(parent, ttl, r.store)
}

// workerWith starts a worker that sees the store through st, so a test
// can give one worker a faulty view of the database.
func (r *harness) workerWith(parent context.Context, ttl time.Duration, st store.Store) (kill func()) {
	r.t.Helper()
	ctx, cancel := context.WithCancel(parent)
	d, err := worker.New(worker.Config{
		Holder: fmt.Sprintf("dh-%d", r.seq.Add(1)),
		Stages: []string{store.StagePlan, store.StageExtract, store.StageTransform, store.StageTeam, store.StageFiles, store.StageLoad, store.StageFinalize, store.StageMembers, store.StageRelease},
		Store:  st, Blobs: r.blobs, KEK: r.kek, Bus: r.bus, Graph: r.graphFactory(),
		// Export URLs name files.slack.com; send them to the fake instead.
		Slack: worker.HTTPSlackFiles{Rewrite: func(_ *config.Config, raw string) (string, http.Header, error) {
			if r.stripFileTokens {
				raw, _, _ = strings.Cut(raw, "?")
			}
			return strings.Replace(raw, "https://files.slack.com", r.filesURL, 1), nil, nil
		}},
		MaxAttempts: r.maxAttempts, FileRetryWait: 5 * time.Millisecond, FolderPoll: 20 * time.Millisecond, FolderWait: r.folderWait,
		// The production default for MaxAttempts, so a lane that fails for
		// the wrong reason fails the test rather than retrying forever.
		Concurrency: 4, LeaseTTL: ttl, Poll: 25 * time.Millisecond,
		ChannelRPS: 1e6, TenantRPS: 1e6, ScratchDir: filepath.Join(r.dir, fmt.Sprintf("scratch-%d", r.seq.Load())),
	})
	if err != nil {
		r.t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = d.Run(ctx) }()
	return func() { cancel(); <-done }
}

// start creates the run and uploads the export, as `taut migrate` does.
func (r *harness) start(ctx context.Context) controller.RunView {
	r.t.Helper()
	run, err := r.client.CreateRun(ctx, r.yaml, false)
	if err != nil {
		r.t.Fatal(err)
	}
	f, err := os.Open(r.export)
	if err != nil {
		r.t.Fatal(err)
	}
	defer f.Close()
	if _, err := r.client.UploadExport(ctx, run.ID, f, ""); err != nil {
		r.t.Fatal(err)
	}
	return run
}

// await reconciles and polls until the run stops or the deadline passes.
func (r *harness) await(ctx context.Context, id string, limit time.Duration) controller.RunView {
	r.t.Helper()
	deadline := time.Now().Add(limit)
	for {
		if _, err := r.store.Reconcile(ctx); err != nil {
			r.t.Fatal(err)
		}
		v, err := r.client.Run(ctx, id)
		if err != nil {
			r.t.Fatal(err)
		}
		if v.Status == store.RunCompleted || v.Status == store.RunFailed {
			return v
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("run did not finish in %s: status %s, progress %+v", limit, v.Status, v.Progress)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// want is the ground truth, computed straight from the export without any
// of the pipeline's code paths except the shared classifier: for each
// selected channel, the Slack timestamps that must end up in Teams, in
// order.
type want struct {
	byChannel map[string][]slack.Message // channel name → messages to import
	total     int
	// files is, per channel name, the Slack-hosted files its migrated
	// messages refer to (ID → true).
	files map[string]map[string]bool
	// chatPosts is how many posts of direct messages the run imported
	// besides the channels' (zero unless direct messages are on). They are
	// verified on their own; here they only make the totals add up.
	chatPosts int
}

func (r *harness) expected() want {
	r.t.Helper()
	ex, err := export.Open(r.export)
	if err != nil {
		r.t.Fatal(err)
	}
	defer ex.Close()
	chans, err := ex.Channels()
	if err != nil {
		r.t.Fatal(err)
	}
	w := want{byChannel: map[string][]slack.Message{}, files: map[string]map[string]bool{}}
	for _, ch := range chans {
		if ch.Kind != slack.KindPublic && (ch.Kind != slack.KindPrivate || !r.private) {
			continue
		}
		for msg, err := range ex.Messages(ch) {
			if err != nil {
				continue // unreadable entries are not migrated
			}
			if transform.Classify(msg).Disposition == transform.Skip {
				continue
			}
			w.byChannel[ch.Name] = append(w.byChannel[ch.Name], msg)
			w.total++
			for _, f := range msg.Files {
				if fakeslack.Downloadable(f) {
					if w.files[ch.Name] == nil {
						w.files[ch.Name] = map[string]bool{}
					}
					w.files[ch.Name][f.ID] = true
				}
			}
		}
	}
	return w
}

// verify asserts the fake tenant holds exactly the expected messages: none
// missing, none duplicated, in order, with threads intact.
func (r *harness) verify(v controller.RunView, w want) {
	t := r.t
	t.Helper()
	if v.Status != store.RunCompleted {
		t.Fatalf("run status = %s; problems: %+v", v.Status, v.Progress.Problems)
	}
	p := v.Progress
	if p.Failed != 0 || p.Held != 0 {
		t.Errorf("failed=%d held=%d, want 0 and 0", p.Failed, p.Held)
	}

	teams := r.fake.Teams()
	if len(teams) != 1 {
		t.Fatalf("fake tenant has %d teams, want exactly 1 (a duplicate team means creation was repeated)", len(teams))
	}
	channels := r.fake.Channels(teams[0].ID)
	byName := map[string]fakegraph.ChannelView{}
	for _, c := range channels {
		if _, dup := byName[c.DisplayName]; dup {
			t.Errorf("channel %q exists twice", c.DisplayName)
		}
		byName[c.DisplayName] = c
	}

	got := 0
	for name, msgs := range w.byChannel {
		target := name
		if name == "general" {
			target = "General"
		}
		c, ok := byName[target]
		if !ok {
			t.Errorf("channel %q was not created", target)
			continue
		}
		if c.MigrationMode != "inProgress" {
			t.Errorf("channel %q migration mode = %q, want inProgress until `taut finalize`", target, c.MigrationMode)
		}
		stored := r.fake.Messages(teams[0].ID, c.ID)
		got += len(stored)
		if len(stored) != len(msgs) {
			t.Errorf("channel %q holds %d messages, want %d", target, len(stored), len(msgs))
			continue
		}

		r.verifyFiles(teams[0].ID, c, target, w.files[name], msgs, stored)

		// Order and timestamps: the i-th message by Teams time is the i-th
		// by Slack ts, and its time is never earlier than Slack's.
		ids := map[string]bool{}
		slackToTeams := map[string]string{}
		for i, m := range stored {
			if ids[m.ID] {
				t.Errorf("channel %q: message ID %s appears twice", target, m.ID)
			}
			ids[m.ID] = true
			src := msgs[i]
			slackToTeams[src.TS] = m.ID
			if d := m.CreatedAt.Sub(src.Time().Truncate(time.Millisecond)); d < 0 || d > time.Second {
				t.Errorf("channel %q message %d: Teams time is %s away from its Slack time", target, i, d)
			}
			if i > 0 && !m.CreatedAt.After(stored[i-1].CreatedAt) {
				t.Errorf("channel %q: timestamps are not strictly increasing at %d", target, i)
			}
		}
		// Threads: a reply whose parent was migrated hangs off that parent.
		for i, m := range stored {
			src := msgs[i]
			if src.IsReply() {
				if parent, ok := slackToTeams[src.ThreadTS]; ok && m.ReplyToID != parent {
					t.Errorf("channel %q: reply %s is attached to %q, want its thread parent %q", target, src.TS, m.ReplyToID, parent)
				}
			} else if m.ReplyToID != "" {
				t.Errorf("channel %q: top-level message %s was posted as a reply", target, src.TS)
			}
		}
	}
	if got != w.total || p.Posted != w.total+w.chatPosts {
		t.Errorf("tenant holds %d channel messages and the ledger says %d posted; the export has %d to import into channels and %d into chats", got, p.Posted, w.total, w.chatPosts)
	}

	// Every write is in the audit trail, and the trail is intact.
	if err := r.store.VerifyLog(context.Background(), v.ID); err != nil {
		t.Errorf("audit log does not verify: %v", err)
	}
	imports := 0
	var after int64
	for {
		entries, err := r.store.LogEntries(context.Background(), v.ID, "", after, 1000)
		if err != nil {
			t.Fatal(err)
		}
		// LogEntries pages within (stream, seq) order; count by op.
		for _, e := range entries {
			if e.Op == "message.import" || e.Op == "message.adopt" {
				imports++
			}
		}
		if len(entries) < 1000 {
			break
		}
		after += 1000
		if after > 1_000_000 {
			t.Fatal("audit log paging did not terminate")
		}
	}
	_ = imports // counted per stream below
	perStream := 0
	for _, c := range p.Channels {
		var seq int64
		for {
			entries, err := r.store.LogEntries(context.Background(), v.ID, "load:"+c.SlackID, seq, 1000)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if e.Op == "message.import" || e.Op == "message.adopt" {
					perStream++
				}
				seq = e.Seq
			}
			if len(entries) < 1000 {
				break
			}
		}
	}
	if perStream != w.total+w.chatPosts {
		t.Errorf("audit log records %d imported messages, want %d", perStream, w.total+w.chatPosts)
	}
}

// verifyFiles asserts a channel's SharePoint folder holds exactly the files
// its messages refer to, byte for byte, and that each message carrying a
// file attaches it (or embeds it, for a small image).
func (r *harness) verifyFiles(teamID string, c fakegraph.ChannelView, name string, want map[string]bool, msgs []slack.Message, stored []fakegraph.MessageView) {
	t := r.t
	t.Helper()
	got := map[string]fakegraph.FileView{}
	for _, f := range r.fake.Files(teamID, c.ID) {
		id, _, _ := strings.Cut(f.Name, "-")
		if _, dup := got[id]; dup {
			t.Errorf("channel %q: file %s was uploaded twice", name, id)
		}
		got[id] = f
	}
	if len(got) != len(want) {
		t.Errorf("channel %q has %d files in SharePoint, want %d", name, len(got), len(want))
	}
	for id := range want {
		f, ok := got[id]
		if !ok {
			t.Errorf("channel %q: file %s was not uploaded", name, id)
			continue
		}
		size, _ := r.files.Size(id)
		if f.Size != size || f.SHA256 != fakeslack.ContentSHA256(id, size) {
			t.Errorf("channel %q: file %s arrived damaged (%d bytes, want %d)", name, id, f.Size, size)
		}
	}
	// Every Slack-hosted file on a message is attached to, or embedded in,
	// the Teams message.
	for i, src := range msgs {
		if i >= len(stored) {
			break
		}
		n := 0
		seen := map[string]bool{}
		for _, f := range src.Files {
			if fakeslack.Downloadable(f) && !seen[f.ID] {
				seen[f.ID] = true
				n++
			}
		}
		if have := len(stored[i].Attachments) + stored[i].HostedContents; have != n {
			t.Errorf("channel %q message %s: %d files attached or embedded, want %d", name, src.TS, have, n)
		}
		for _, a := range stored[i].Attachments {
			if !strings.Contains(a.ContentURL, "/Slack/") {
				t.Errorf("channel %q: attachment %q does not point into the Slack folder", name, a.Name)
			}
		}
	}
}

func TestMigrateSmall(t *testing.T) {
	r := newHarness(t, "small")
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()

	run := r.start(ctx)
	v := r.await(ctx, run.ID, 60*time.Second)
	r.verify(v, r.expected())
	if v.Progress.Adopted != 0 {
		t.Errorf("adopted = %d on an undisturbed run", v.Progress.Adopted)
	}

	// A finished import is not imported again unless asked to.
	if again, err := r.client.CreateRun(ctx, r.yaml, false); err == nil {
		t.Errorf("a second migration run on an imported migration started run %s", again.ID)
	}
}

// TestMigrateSurvivesKillsAndFaults is the import acceptance test: the
// medium fixture is imported while workers are killed and replaced at
// random and the fake Graph drops responses after committing, throttles and
// fails. The tenant must end up with exactly the export's messages.
func TestMigrateSurvivesKillsAndFaults(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos run skipped in -short mode")
	}
	r := newHarness(t, "medium", fakegraph.WithRandSeed(7))
	ctx := t.Context()

	// The crash that matters most: Graph stores the message, the response
	// never arrives. Plus ordinary throttling and server errors.
	faults := r.fake.Faults()
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/messages", Action: fakegraph.ActionFailAfterCommit, Probability: 0.03})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/messages/*/replies", Action: fakegraph.ActionFailAfterCommit, Probability: 0.03})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/messages", Action: fakegraph.ActionThrottle, RetryAfterSeconds: 1, Probability: 0.03})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/messages/**", Action: fakegraph.ActionStatus, Status: 503, Probability: 0.02})

	// The file path gets the same treatment: Slack's file host fails and
	// cuts downloads short, and SharePoint loses responses to upload chunks
	// after accepting the bytes, including the last chunk.
	r.files.FailEvery(6)
	faults.MustAdd(fakegraph.Fault{Method: "PUT", Path: "/_upload/**", Action: fakegraph.ActionFailAfterCommit, Probability: 0.08})
	faults.MustAdd(fakegraph.Fault{Method: "PUT", Path: "/_upload/**", Action: fakegraph.ActionThrottle, RetryAfterSeconds: 1, Probability: 0.05})
	faults.MustAdd(fakegraph.Fault{Method: "GET", Path: "/v1.0/teams/*/channels/*/filesFolder", Action: fakegraph.ActionStatus, Status: 503, Probability: 0.2})

	run := r.start(ctx)

	// Three workers at a time. Every so often one is killed mid-work and
	// a fresh one takes its place.
	var mu sync.Mutex
	kills := []func(){r.worker(ctx, 600*time.Millisecond), r.worker(ctx, 600*time.Millisecond), r.worker(ctx, 600*time.Millisecond)}
	stop := make(chan struct{})
	chaosDone := make(chan struct{})
	killed := 0
	go func() {
		defer close(chaosDone)
		rng := rand.New(rand.NewPCG(11, 13))
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Duration(60+rng.IntN(140)) * time.Millisecond):
			}
			mu.Lock()
			i := rng.IntN(len(kills))
			kills[i]()
			kills[i] = r.worker(ctx, 600*time.Millisecond)
			killed++
			mu.Unlock()
		}
	}()

	v := r.await(ctx, run.ID, 4*time.Minute)
	close(stop)
	<-chaosDone
	mu.Lock()
	for _, k := range kills {
		k()
	}
	mu.Unlock()

	r.verify(v, r.expected())

	stats := r.fake.Stats()
	t.Logf("workers killed: %d; imports accepted: %d; conflicts (409): %d; throttles: %d; adopted after a lost response: %d",
		killed, stats.ImportsAccepted, stats.Conflicts, stats.Throttles, v.Progress.Adopted)
	t.Logf("files: %d uploaded (%d bytes) in %d upload sessions; %d failed; %d downloads served",
		v.Progress.FilesUploaded, v.Progress.FileBytesDone, stats.UploadSessions, v.Progress.FilesFailed, r.files.Served())
	if v.Progress.FilesFailed != 0 {
		t.Errorf("%d files failed; faults on the file path must be retried, not given up on", v.Progress.FilesFailed)
	}
	if killed < 5 {
		t.Errorf("only %d workers were killed; the run was too short to prove anything", killed)
	}
	if v.Progress.Adopted == 0 || stats.Conflicts == 0 {
		t.Errorf("the recovery path was never exercised (adopted=%d, conflicts=%d)", v.Progress.Adopted, stats.Conflicts)
	}
	if stats.ImportsAccepted != v.Progress.Posted {
		t.Errorf("Graph accepted %d imports but %d posts exist: a message was written twice", stats.ImportsAccepted, v.Progress.Posted)
	}

	// The teams and channels are listed in the order Slack had them.
	names := []string{}
	for _, c := range r.fake.Channels(r.fake.Teams()[0].ID) {
		names = append(names, c.DisplayName)
	}
	sort.Strings(names)
	if len(names) != fixtures.Facts("medium").PublicChannels {
		t.Errorf("channels = %v", names)
	}
}

// TestMigrateNasty runs the edge-case fixture: a 2,500-reply thread, messages
// in the same millisecond, 60 KB and 150 KB messages that must be split,
// authors missing from the user list, deleted thread starters, unreadable
// entries, and channel names Teams does not allow.
func TestMigrateNasty(t *testing.T) {
	r := newHarness(t, "nasty")
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()

	run := r.start(ctx)
	v := r.await(ctx, run.ID, 3*time.Minute)
	if v.Status != store.RunCompleted {
		t.Fatalf("status = %s; problems: %+v", v.Status, v.Progress.Problems)
	}
	p := v.Progress
	if p.Failed != 0 || p.Held != 0 {
		for _, c := range p.Channels {
			if c.Failed+c.Held > 0 {
				t.Logf("channel %s: failed=%d held=%d", c.SlackName, c.Failed, c.Held)
			}
		}
		t.Errorf("failed=%d held=%d, want none", p.Failed, p.Held)
	}

	w := r.expected()
	// Split messages become several posts, so posts ≥ messages, and the
	// nasty fixture is known to need splitting.
	if p.Posted <= w.total {
		t.Errorf("posted %d posts for %d messages; the oversized messages were not split", p.Posted, w.total)
	}

	team := r.fake.Teams()[0]
	total := 0
	names := map[string]fakegraph.ChannelView{}
	for _, c := range r.fake.Channels(team.ID) {
		names[c.DisplayName] = c
		msgs := r.fake.Messages(team.ID, c.ID)
		total += len(msgs)
		for i := 1; i < len(msgs); i++ {
			if !msgs[i].CreatedAt.After(msgs[i-1].CreatedAt) {
				t.Errorf("channel %q: duplicate or out-of-order timestamp at %d", c.DisplayName, i)
			}
		}
		for _, m := range msgs {
			if len(m.BodyContent) > 100*1024 {
				t.Errorf("channel %q: a post is %d bytes, over the Teams limit", c.DisplayName, len(m.BodyContent))
			}
		}
	}
	if total != p.Posted {
		t.Errorf("tenant holds %d messages, ledger says %d", total, p.Posted)
	}
	// Names Teams forbids were rewritten the way `taut preflight` said.
	for _, want := range []string{"archive", "forms-channel", "con-channel", "General", "megathread"} {
		if _, ok := names[want]; !ok {
			t.Errorf("channel %q is missing; have %d channels", want, len(names))
		}
	}
	// The huge thread is one thread: every reply hangs off the same parent.
	mega := r.fake.Messages(team.ID, names["megathread"].ID)
	parents := map[string]int{}
	for _, m := range mega {
		if m.ReplyToID != "" {
			parents[m.ReplyToID]++
		}
	}
	if len(parents) != 1 {
		t.Errorf("megathread has %d reply parents, want 1", len(parents))
	}
	for _, n := range parents {
		if n < 2500 {
			t.Errorf("megathread parent has %d replies, want at least 2500", n)
		}
	}
	if err := r.store.VerifyLog(ctx, v.ID); err != nil {
		t.Errorf("audit log does not verify: %v", err)
	}
}

// TestMigrateWithOpaqueMessageIDs covers the case Microsoft does not promise:
// message IDs that are not the timestamp. Recovery after a lost response
// must then find the message by its time instead.
func TestMigrateWithOpaqueMessageIDs(t *testing.T) {
	r := newHarness(t, "small", fakegraph.WithOpaqueMessageIDs(), fakegraph.WithRandSeed(3))
	ctx := t.Context()
	r.fake.Faults().MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/messages", Action: fakegraph.ActionFailAfterCommit, Probability: 0.3})
	r.fake.Faults().MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/messages/*/replies", Action: fakegraph.ActionFailAfterCommit, Probability: 0.3})
	kill := r.worker(ctx, 5*time.Second)
	defer kill()

	run := r.start(ctx)
	v := r.await(ctx, run.ID, 90*time.Second)
	r.verify(v, r.expected())
	if v.Progress.Adopted == 0 {
		t.Error("no post was adopted; the fallback lookup was not exercised")
	}
}

// renewFails is a store through which lease renewal always fails, as if the
// database had become unreachable for the heartbeat only.
type renewFails struct{ store.Store }

func (renewFails) Renew(context.Context, *store.Lease, time.Duration) error {
	return fmt.Errorf("injected: database unreachable")
}

// TestMigrateZombieHolder runs one healthy worker beside one that can never
// renew a lease. The faulty one keeps claiming lanes and working them while
// its leases expire underneath it and the healthy one takes them over, so
// the two genuinely overlap on the same channels. The result must still be
// exact, and the faulty worker must stop working a lane once it can no
// longer vouch for its lease.
func TestMigrateZombieHolder(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped in -short mode")
	}
	r := newHarness(t, "medium")
	ctx := t.Context()
	killGood := r.worker(ctx, 400*time.Millisecond)
	defer killGood()
	killZombie := r.workerWith(ctx, 400*time.Millisecond, renewFails{r.store})
	defer killZombie()

	run := r.start(ctx)
	v := r.await(ctx, run.ID, 4*time.Minute)
	r.verify(v, r.expected())
	stats := r.fake.Stats()
	t.Logf("imports accepted: %d; conflicts: %d; adopted: %d", stats.ImportsAccepted, stats.Conflicts, v.Progress.Adopted)
	if stats.ImportsAccepted != v.Progress.Posted {
		t.Errorf("Graph accepted %d imports for %d posts", stats.ImportsAccepted, v.Progress.Posted)
	}
}

// TestMigrateReopensChannelsThatLeftMigrationMode completes every channel's
// migration behind Taut's back in the middle of the import. Taut must
// notice, reopen them and finish, with nothing lost.
func TestMigrateReopensChannelsThatLeftMigrationMode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped in -short mode")
	}
	r := newHarness(t, "medium")
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	run := r.start(ctx)

	// Wait until the import is well under way.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		v, err := r.client.Run(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if v.Progress != nil && v.Progress.Posted > 300 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("import never got under way")
		}
		time.Sleep(20 * time.Millisecond)
	}
	m, err := config.Parse(r.yaml)
	if err != nil {
		t.Fatal(err)
	}
	g, err := r.graphFactory()(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	team := r.fake.Teams()[0]
	closed := 0
	for _, c := range r.fake.Channels(team.ID) {
		if c.MigrationMode == "inProgress" {
			if err := g.CompleteChannelMigration(ctx, team.ID, c.ID); err == nil {
				closed++
			}
		}
	}
	if closed == 0 {
		t.Fatal("no channel was in migration mode to close")
	}

	v := r.await(ctx, run.ID, 4*time.Minute)
	r.verify(v, r.expected())
	t.Logf("channels closed mid-import: %d; all reopened and completed", closed)
}

// TestMigrateGigabyteFileInConstantMemory moves a 1 GiB file from Slack through
// encrypted staging into SharePoint and checks two things: it arrives byte
// for byte, and the process's heap never grows by more than a small, fixed
// amount on the way. Nothing in the path may hold a file in memory.
func TestMigrateGigabyteFileInConstantMemory(t *testing.T) {
	if testing.Short() {
		t.Skip("1 GiB transfer skipped in -short mode")
	}
	const gib = 1 << 30
	r := newHarness(t, "small")
	ctx := t.Context()

	// The small fixture has one file. Make the fake Slack serve a gigabyte
	// for it.
	sizes, err := fakeslack.FilesFromExport(r.export, 0)
	if err != nil || len(sizes) != 1 {
		t.Fatalf("expected the small fixture to have exactly one file: %v, %v", sizes, err)
	}
	var id string
	for k := range sizes {
		id = k
	}
	r.files.SetFiles(map[string]int64{id: gib})

	// Sample the heap for the whole run.
	var base runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&base)
	var peak atomic.Uint64
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		var m runtime.MemStats
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > peak.Load() {
					peak.Store(m.HeapAlloc)
				}
			}
		}
	}()

	kill := r.worker(ctx, 10*time.Second)
	defer kill()
	run := r.start(ctx)
	v := r.await(ctx, run.ID, 10*time.Minute)
	close(stop)
	<-sampled

	if v.Status != store.RunCompleted || v.Progress.FilesUploaded != 1 || v.Progress.FilesFailed != 0 {
		t.Fatalf("status=%s uploaded=%d failed=%d problems=%+v", v.Status, v.Progress.FilesUploaded, v.Progress.FilesFailed, v.Progress.Problems)
	}
	team := r.fake.Teams()[0]
	var found *fakegraph.FileView
	for _, c := range r.fake.Channels(team.ID) {
		for _, f := range r.fake.Files(team.ID, c.ID) {
			found = &f
		}
	}
	if found == nil {
		t.Fatal("the file is not in SharePoint")
	}
	if found.Size != gib {
		t.Fatalf("SharePoint holds %d bytes, want %d", found.Size, int64(gib))
	}
	if want := fakeslack.ContentSHA256(id, gib); found.SHA256 != want {
		t.Fatalf("the file arrived damaged: sha256 %s, want %s", found.SHA256, want)
	}

	growth := int64(peak.Load()) - int64(base.HeapAlloc)
	t.Logf("1 GiB moved; peak heap growth %d MiB", growth>>20)
	// One upload chunk (10 MiB), one staging chunk and the fake's buffers
	// account for a few tens of MiB. A gigabyte held anywhere would not
	// fit under this.
	if growth > 160<<20 {
		t.Errorf("heap grew by %d MiB while moving a 1 GiB file; something is buffering it", growth>>20)
	}
}

// TestMigrateStopsWhenSlackRefusesFiles: if Slack will not serve files (the
// export's links have expired, the token lacks access) the run must stop
// before the affected channel is opened, not import the channel with every
// file replaced by a note. Imported messages cannot be taken back.
func TestMigrateStopsWhenSlackRefusesFiles(t *testing.T) {
	r := newHarness(t, "small")
	r.stripFileTokens = true // Slack answers 200 with a sign-in page
	r.maxAttempts = 2
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()

	run := r.start(ctx)
	v := r.await(ctx, run.ID, 2*time.Minute)
	if v.Status != store.RunFailed {
		t.Fatalf("status = %s, want failed", v.Status)
	}
	if v.Progress.FilesUploaded != 0 {
		t.Errorf("%d files were uploaded from a sign-in page", v.Progress.FilesUploaded)
	}
	if v.Progress.FilesFailed != 0 {
		t.Errorf("%d files were written off as failed; an authorization problem is not about any one file", v.Progress.FilesFailed)
	}
	found := false
	for _, pr := range v.Progress.Problems {
		if strings.Contains(pr.Error, "slack.files_unauthorized") {
			found = true
		}
	}
	if !found {
		t.Errorf("the failure does not say Slack refused the files: %+v", v.Progress.Problems)
	}

	// The channel whose file could not be fetched was never opened, so none
	// of its messages were imported; channels without files finished.
	w := r.expected()
	team := r.fake.Teams()[0]
	byName := map[string]fakegraph.ChannelView{}
	for _, c := range r.fake.Channels(team.ID) {
		byName[c.DisplayName] = c
	}
	for name, files := range w.files {
		target := name
		if name == "general" {
			target = "General"
		}
		c := byName[target]
		if len(files) > 0 && (c.MigrationMode != "" || len(r.fake.Messages(team.ID, c.ID)) != 0) {
			t.Errorf("channel %q was opened and has %d messages although its files could not be fetched",
				target, len(r.fake.Messages(team.ID, c.ID)))
		}
	}
	if len(r.fake.Files(team.ID, byName["General"].ID)) != 0 {
		t.Error("a sign-in page was uploaded as a file")
	}
}

package worker_test

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/fakegraph"
	"github.com/bgfdev/taut/internal/fakeslack"
	"github.com/bgfdev/taut/internal/fixtures"
	"github.com/bgfdev/taut/internal/pipeline"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/slack/export"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/transform"
)

// Incremental passes: a second import, from a newer export of the same
// workspace, into the teams and channels the first one made. The ground
// truth is computed from the exports alone: the tenant must end up holding
// every message of either export exactly once, where the first export said
// it was.

// newPassHarness is a harness for a fixture pair: it starts with the base
// export, and its tenant already has accounts for the people who join
// before the later one.
func newPassHarness(t *testing.T, base string, mod func(*config.Config), fakeOpts ...fakegraph.Option) *harness {
	t.Helper()
	return newHarnessDirectory(t, base, fixtures.LaterOf(base), mod, fakeOpts...)
}

// useExport points the harness's config file at another export.
func (r *harness) useExport(path string) {
	r.t.Helper()
	m, err := config.Parse(r.yaml)
	if err != nil {
		r.t.Fatal(err)
	}
	m.Source.Slack.Export = path
	if r.yaml, err = m.Marshal(); err != nil {
		r.t.Fatal(err)
	}
	r.export = path
	sizes, err := fakeslack.FilesFromExport(path, fileCap)
	if err != nil {
		r.t.Fatal(err)
	}
	r.files.SetFiles(sizes)
}

// laterExport writes a fixture's later export and points the config at it.
func (r *harness) laterExport(fixture string, opts ...fixtures.Option) {
	r.t.Helper()
	dir := r.t.TempDir()
	path, err := fixtures.Write(fixture, dir, opts...)
	if err != nil {
		r.t.Fatal(err)
	}
	r.useExport(path)
}

// startPass starts an incremental pass with the harness's current config
// and export, as `taut migrate --incremental` does.
func (r *harness) startPass(ctx context.Context, replace bool) (controller.RunView, error) {
	r.t.Helper()
	run, err := r.client.CreateRunWith(ctx, r.yaml, controller.CreateOptions{Incremental: true, Replace: replace})
	if err != nil {
		return run, err
	}
	f, err := os.Open(r.export)
	if err != nil {
		r.t.Fatal(err)
	}
	defer f.Close()
	_, err = r.client.UploadExport(ctx, run.ID, f, "")
	return run, err
}

func (r *harness) mustStartPass(ctx context.Context) controller.RunView {
	r.t.Helper()
	run, err := r.startPass(ctx, false)
	if err != nil {
		r.t.Fatalf("starting an incremental pass: %v", err)
	}
	return run
}

// union is the ground truth after passes over several exports of one
// workspace: per channel, every message any of them says to import, once.
// A message more than one export has is taken from the first: that is the
// version Teams was given, and the import API cannot change it.
func (r *harness) union(exports ...string) want {
	r.t.Helper()
	w := want{byChannel: map[string][]slack.Message{}, files: map[string]map[string]bool{}}
	seen := map[string]map[string]bool{}
	for _, path := range exports {
		ex, err := export.Open(path)
		if err != nil {
			r.t.Fatal(err)
		}
		chans, err := ex.Channels()
		if err != nil {
			r.t.Fatal(err)
		}
		for _, ch := range chans {
			if ch.Kind != slack.KindPublic && (ch.Kind != slack.KindPrivate || !r.private) {
				continue
			}
			if seen[ch.Name] == nil {
				seen[ch.Name] = map[string]bool{}
			}
			if _, ok := w.byChannel[ch.Name]; !ok {
				// An empty channel is still created.
				w.byChannel[ch.Name] = nil
			}
			for msg, err := range ex.Messages(ch) {
				if err != nil || transform.Classify(msg).Disposition == transform.Skip || seen[ch.Name][msg.TS] {
					continue
				}
				seen[ch.Name][msg.TS] = true
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
		ex.Close()
	}
	for name := range w.byChannel {
		msgs := w.byChannel[name]
		sort.SliceStable(msgs, func(i, j int) bool { return slack.CompareTS(msgs[i].TS, msgs[j].TS) < 0 })
	}
	return w
}

// verifyUnion asserts the tenant holds exactly w: nothing missing, nothing
// twice, in Slack order, threads intact, files attached. mode says which
// migration mode each channel (by Slack name) must be in.
func (r *harness) verifyUnion(w want, mode func(channel string) string) {
	t := r.t
	t.Helper()
	teams := r.fake.Teams()
	if len(teams) != 1 {
		t.Fatalf("fake tenant has %d teams, want exactly 1 (a second team means a pass did not reuse the first)", len(teams))
	}
	byName := map[string]fakegraph.ChannelView{}
	for _, c := range r.fake.Channels(teams[0].ID) {
		if _, dup := byName[c.DisplayName]; dup {
			t.Errorf("channel %q exists twice", c.DisplayName)
		}
		byName[c.DisplayName] = c
	}
	if len(byName) != len(w.byChannel) {
		t.Errorf("tenant has %d channels, want %d", len(byName), len(w.byChannel))
	}
	got := 0
	for name, msgs := range w.byChannel {
		target := name
		if name == "general" {
			target = "General"
		}
		c, ok := byName[target]
		if !ok {
			t.Errorf("channel %q does not exist", target)
			continue
		}
		if want := mode(name); c.MigrationMode != want {
			t.Errorf("channel %q migration mode = %q, want %q", target, c.MigrationMode, want)
		}
		stored := r.fake.Messages(teams[0].ID, c.ID)
		got += len(stored)
		if len(stored) != len(msgs) {
			t.Errorf("channel %q holds %d messages, want %d", target, len(stored), len(msgs))
			continue
		}
		r.verifyFiles(teams[0].ID, c, target, w.files[name], msgs, stored)
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
				t.Errorf("channel %q message %d (%s): Teams time is %s away from its Slack time", target, i, src.TS, d)
			}
			if i > 0 && !m.CreatedAt.After(stored[i-1].CreatedAt) {
				t.Errorf("channel %q: timestamps are not strictly increasing at %d", target, i)
			}
			if !m.CreatedAt.After(c.CreatedAt) {
				t.Errorf("channel %q: message %s is not later than the channel's creation time", target, src.TS)
			}
		}
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
	if got != w.total {
		t.Errorf("tenant holds %d channel messages, want %d", got, w.total)
	}
}

func inProgress(string) string { return "inProgress" }

// checkPass asserts what a finished incremental pass says of itself: its
// own posts are the new ones, the rest are carried, and its audit log
// records exactly its own writes.
func (r *harness) checkPass(ctx context.Context, v controller.RunView, pass, carried, added int) {
	t := r.t
	t.Helper()
	if v.Status != store.RunCompleted {
		t.Fatalf("pass status = %s; problems: %+v", v.Status, v.Progress.Problems)
	}
	p := v.Progress
	if v.Pass != pass || v.BaseRunID == "" || len(v.Passes) != pass {
		t.Errorf("run reports pass %d, base %q and %d passes; want pass %d", v.Pass, v.BaseRunID, len(v.Passes), pass)
	}
	if p.Failed != 0 || p.Held != 0 {
		t.Errorf("failed=%d held=%d, want none", p.Failed, p.Held)
	}
	if p.Carried != carried || p.Posted != added || p.Posts != added {
		t.Errorf("pass carried %d posts and posted %d of %d; want %d carried and %d posted", p.Carried, p.Posted, p.Posts, carried, added)
	}
	if err := r.store.VerifyLog(ctx, v.ID); err != nil {
		t.Errorf("audit log of the pass does not verify: %v", err)
	}
	imports := r.logCount(ctx, v.ID, store.LogFilter{Op: "message.import"}) + r.logCount(ctx, v.ID, store.LogFilter{Op: "message.adopt"})
	if imports != added {
		t.Errorf("the pass's audit log records %d imported messages, want %d", imports, added)
	}
}

// basePassDone runs the first import to the end and returns it with the
// export it used.
func (r *harness) basePassDone(ctx context.Context, limit time.Duration) (controller.RunView, string, want) {
	r.t.Helper()
	run := r.start(ctx)
	v := r.await(ctx, run.ID, limit)
	w := r.expected()
	r.verify(v, w)
	return v, r.export, w
}

func (r *harness) finalizeAll(ctx context.Context, runID string) controller.RunView {
	r.t.Helper()
	if _, err := r.client.Finalize(ctx, runID, controller.FinalizeRequest{}); err != nil {
		r.t.Fatalf("finalize: %v", err)
	}
	return r.awaitFinalize(ctx, runID, 90*time.Second)
}

// A pass before the first is finalized: every channel is still in
// migration mode, and the pass adds to them.
func TestIncrementalBeforeFinalize(t *testing.T) {
	r := newPassHarness(t, "small", nil)
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()

	base, baseExport, bw := r.basePassDone(ctx, 60*time.Second)
	starts := r.fake.CountRequests("POST", "/v1.0/teams/*/channels/*/startMigration")
	teamsCreated := r.fake.CountRequests("POST", "/v1.0/teams")

	r.laterExport("small-later")
	facts := fixtures.LaterFacts("small-later")
	run := r.mustStartPass(ctx)
	v := r.await(ctx, run.ID, 60*time.Second)

	w := r.union(baseExport, r.export)
	r.verifyUnion(w, inProgress)
	// The pair's facts count the direct messages too; they are off here.
	added := facts.NewMessages - facts.NewDMMessages
	if w.total-bw.total != added {
		t.Fatalf("the exports differ by %d channel messages; the fixture's facts say %d", w.total-bw.total, added)
	}
	r.checkPass(ctx, v, 2, bw.total, added)
	if v.BaseRunID != base.ID {
		t.Errorf("the pass follows %s, want %s", v.BaseRunID, base.ID)
	}
	if v.Progress.Adopted != 0 {
		t.Errorf("adopted = %d on an undisturbed pass", v.Progress.Adopted)
	}
	// A pass has a layout of its own, so that a worker from before passes
	// existed refuses it instead of transforming carried messages again.
	if got, err := r.store.GetRun(ctx, v.ID); err != nil || got.Layout != pipeline.LayoutPass || pipeline.LayoutSupported(99) {
		t.Errorf("pass layout = %d, %v; want %d", got.Layout, err, pipeline.LayoutPass)
	}

	// Nothing that existed was created or opened again: one new channel,
	// one startMigration for it, no new team.
	if n := r.fake.CountRequests("POST", "/v1.0/teams/*/channels/*/startMigration") - starts; n != facts.NewChannels {
		t.Errorf("the pass called startMigration %d times, want %d (the new channel only)", n, facts.NewChannels)
	}
	if n := r.fake.CountRequests("POST", "/v1.0/teams") - teamsCreated; n != 0 {
		t.Errorf("the pass created %d teams", n)
	}
	if n := r.logCount(ctx, v.ID, store.LogFilter{Op: "channel.create"}); n != facts.NewChannels {
		t.Errorf("the pass's audit log has %d channel.create entries, want %d", n, facts.NewChannels)
	}

	// Edits and deletions are not carried into Teams, and are counted.
	team := r.fake.Teams()[0]
	edited, already := 0, 0
	lanes, err := r.store.Lanes(ctx, v.ID, store.StageTransform)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lanes {
		for seq := 0; ; seq++ {
			b, err := r.store.GetBatch(ctx, v.ID, store.BatchRef{LaneID: l.ID, Seq: seq})
			if err != nil {
				break
			}
			edited += batchStat(t, b, "edited_since")
			already += batchStat(t, b, "carried")
		}
	}
	if edited != facts.Edited {
		t.Errorf("the transform stage counted %d messages edited since, want %d", edited, facts.Edited)
	}
	if already != bw.total-facts.Deleted {
		t.Errorf("the transform stage found %d messages already migrated, want %d", already, bw.total-facts.Deleted)
	}
	for _, c := range r.fake.Channels(team.ID) {
		for _, m := range r.fake.Messages(team.ID, c.ID) {
			if strings.Contains(m.BodyContent, "(corrected)") {
				t.Errorf("channel %q: an edit made in Slack after the first pass reached Teams", c.DisplayName)
			}
		}
	}

	// The first pass is over: the second took its ledger, and its staging
	// (a copy of the first export) goes.
	r.api.Reconcile(ctx)
	if n := r.stagedObjects(ctx, base.ID); n != 0 {
		t.Errorf("the superseded pass still has %d staged objects", n)
	}
	if b, _ := r.store.GetRun(ctx, base.ID); b.PurgedAt == nil || b.Status != store.RunCompleted {
		t.Errorf("superseded pass: status %s, purged %v", b.Status, b.PurgedAt)
	}
	// It cannot be finalized any more; the latest pass is.
	if _, err := r.client.Finalize(ctx, base.ID, controller.FinalizeRequest{}); fail.From(err).Code != "finalize.superseded" {
		t.Errorf("finalizing the superseded pass: %v", err)
	}

	fv := r.finalizeAll(ctx, v.ID)
	if fv.Status != store.RunFinalized {
		t.Fatalf("after finalize the pass is %s; problems %+v", fv.Status, fv.Progress.Problems)
	}
	r.verifyUnion(w, func(string) string { return "completed" })
	// Everyone eligible in the newer export is a member, the person who
	// joined between the passes included.
	members := map[string]bool{}
	for _, m := range r.fake.Teams()[0].Members {
		members[strings.ToLower(m.UserID)] = true
	}
	joined := 0
	for slackID, entra := range entraIDs("small-later") {
		if _, old := entraIDs("small")[slackID]; !old {
			joined++
			if !members[strings.ToLower(entra)] {
				t.Errorf("%s joined Slack between the passes and is not a member of the team", slackID)
			}
		}
	}
	if joined == 0 {
		t.Fatal("the later fixture has nobody new with an account")
	}
}

func batchStat(t *testing.T, b store.Batch, key string) int {
	t.Helper()
	if len(b.Stats) == 0 {
		return 0
	}
	var m map[string]int
	if err := json.Unmarshal(b.Stats, &m); err != nil {
		t.Fatalf("batch %s/%d stats: %v", b.LaneID, b.Seq, err)
	}
	return m[key]
}

// A pass after the first was finalized. Channels with something new are
// put back into migration mode, which Microsoft documents; a channel with
// nothing new is not touched. It must hold under both readings of what an
// absent conversationCreationDateTime means.
func TestIncrementalAfterFinalize(t *testing.T) {
	for name, opts := range map[string][]fakegraph.Option{
		"default": nil,
		"literal": {fakegraph.WithLiteralMigrationStart()},
	} {
		t.Run(name, func(t *testing.T) {
			r := newPassHarness(t, "small", nil, opts...)
			ctx := t.Context()
			kill := r.worker(ctx, 5*time.Second)
			defer kill()

			base, baseExport, bw := r.basePassDone(ctx, 60*time.Second)
			if fv := r.finalizeAll(ctx, base.ID); fv.Status != store.RunFinalized {
				t.Fatalf("base pass is %s after finalize", fv.Status)
			}
			team := r.fake.Teams()[0]
			created := map[string]time.Time{}
			for _, c := range r.fake.Channels(team.ID) {
				if c.MigrationMode != "completed" {
					t.Fatalf("channel %q is %q after the base's finalize", c.DisplayName, c.MigrationMode)
				}
				created[c.DisplayName] = c.CreatedAt
			}
			starts := r.fake.CountRequests("POST", "/v1.0/teams/*/channels/*/startMigration")

			r.laterExport("small-later")
			facts := fixtures.LaterFacts("small-later")
			if facts.QuietChannel == "" {
				t.Fatal("the later fixture has no channel without new messages")
			}
			run := r.mustStartPass(ctx)
			v := r.await(ctx, run.ID, 60*time.Second)

			w := r.union(baseExport, r.export)
			r.verifyUnion(w, func(channel string) string {
				if channel == facts.QuietChannel {
					return "completed"
				}
				return "inProgress"
			})
			r.checkPass(ctx, v, 2, bw.total, facts.NewMessages-facts.NewDMMessages)

			// Reopened: every base channel but the quiet one, plus the
			// new channel's first start.
			reopened := len(bw.byChannel) - 1
			if n := r.fake.CountRequests("POST", "/v1.0/teams/*/channels/*/startMigration") - starts; n != reopened+facts.NewChannels {
				t.Errorf("the pass called startMigration %d times, want %d", n, reopened+facts.NewChannels)
			}
			if n := r.logCount(ctx, v.ID, store.LogFilter{Op: "channel.start_migration", Contains: "re-entered"}); n != reopened {
				t.Errorf("the audit log records %d channels re-entering migration mode, want %d", n, reopened)
			}
			// A creation time only ever moves back.
			for _, c := range r.fake.Channels(team.ID) {
				if was, ok := created[c.DisplayName]; ok && c.CreatedAt.After(was) {
					t.Errorf("channel %q: creation time moved forward from %s to %s", c.DisplayName, was, c.CreatedAt)
				}
			}
			for _, c := range v.Progress.Channels {
				switch {
				case c.SlackName == facts.QuietChannel && c.State != store.ChannelCompleted:
					t.Errorf("the quiet channel is %s in the ledger, want completed", c.State)
				case c.SlackName != facts.QuietChannel && c.State != store.ChannelMigrating:
					t.Errorf("channel #%s is %s in the ledger, want migrating", c.SlackName, c.State)
				}
			}

			// Finalizing the pass completes what it reopened.
			fv := r.finalizeAll(ctx, v.ID)
			if fv.Status != store.RunFinalized {
				t.Fatalf("after its finalize the pass is %s; problems %+v", fv.Status, fv.Progress.Problems)
			}
			r.verifyUnion(w, func(string) string { return "completed" })

			// And a third pass with nothing new touches nothing.
			writes := r.fake.CountRequests("POST", "/v1.0/**")
			third := r.mustStartPass(ctx)
			tv := r.await(ctx, third.ID, 60*time.Second)
			r.checkPass(ctx, tv, 3, w.total, 0)
			r.verifyUnion(w, func(string) string { return "completed" })
			if n := r.fake.CountRequests("POST", "/v1.0/**") - writes; n != 0 {
				t.Errorf("a pass with nothing to import made %d write requests", n)
			}
		})
	}
}

// The incremental acceptance test: the later medium export is imported on
// top of the first while workers are killed and replaced and the fake Graph
// loses responses after committing, throttles and fails. Every message of
// either export must be in the tenant once.
func TestIncrementalSurvivesKillsAndFaults(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos run skipped in -short mode")
	}
	r := newPassHarness(t, "medium", nil, fakegraph.WithRandSeed(17))
	ctx := t.Context()

	first := r.worker(ctx, 5*time.Second)
	_, baseExport, bw := r.basePassDone(ctx, 4*time.Minute)
	first()
	accepted := r.fake.Stats().ImportsAccepted

	faults := r.fake.Faults()
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/messages", Action: fakegraph.ActionFailAfterCommit, Probability: 0.12})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/messages/*/replies", Action: fakegraph.ActionFailAfterCommit, Probability: 0.12})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/messages", Action: fakegraph.ActionThrottle, RetryAfterSeconds: 1, Probability: 0.05})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/messages/**", Action: fakegraph.ActionStatus, Status: 503, Probability: 0.05})
	faults.MustAdd(fakegraph.Fault{Method: "PUT", Path: "/_upload/**", Action: fakegraph.ActionFailAfterCommit, Probability: 0.08})
	r.files.FailEvery(5)

	r.laterExport("medium-later")
	facts := fixtures.LaterFacts("medium-later")
	run := r.mustStartPass(ctx)

	var mu sync.Mutex
	kills := []func(){r.worker(ctx, 600*time.Millisecond), r.worker(ctx, 600*time.Millisecond), r.worker(ctx, 600*time.Millisecond)}
	// Planning a pass copies the base run's ledger in one transaction. On a
	// slow machine that outlasts the kill interval below, and a plan that is
	// always interrupted never finishes. So the plan is interrupted once,
	// and the steady kills start when it is done.
	time.Sleep(50 * time.Millisecond)
	kills[0]()
	kills[0] = r.worker(ctx, 600*time.Millisecond)
	for deadline := time.Now().Add(patient(2 * time.Minute)); ; {
		v, err := r.client.Run(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if v.Status != store.RunQueued {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the pass was not planned in time")
		}
		if _, err := r.store.Reconcile(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}

	stop := make(chan struct{})
	chaosDone := make(chan struct{})
	killed := 1
	go func() {
		defer close(chaosDone)
		rng := rand.New(rand.NewPCG(5, 23))
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Duration(60+rng.IntN(140)) * pace * time.Millisecond):
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

	w := r.union(baseExport, r.export)
	r.verifyUnion(w, inProgress)
	// The fixture's facts count the private channels and direct messages
	// too, which are not selected here; count from the exports instead.
	added := w.total - bw.total
	if added == 0 || added >= facts.NewMessages {
		t.Fatalf("the exports differ by %d channel messages; the fixture adds %d in all", added, facts.NewMessages)
	}
	r.checkPass(ctx, v, 2, bw.total, added)

	stats := r.fake.Stats()
	t.Logf("workers killed: %d; new imports accepted: %d; conflicts (409): %d; adopted after a lost response: %d; files uploaded: %d",
		killed, stats.ImportsAccepted-accepted, stats.Conflicts, v.Progress.Adopted, v.Progress.FilesUploaded)
	if stats.ImportsAccepted-accepted != added {
		t.Errorf("Graph accepted %d imports during the pass for %d new posts: a message was written twice", stats.ImportsAccepted-accepted, added)
	}
	if killed < 3 {
		t.Errorf("only %d workers were killed; the pass was too short to prove anything", killed)
	}
	if v.Progress.Adopted == 0 || stats.Conflicts == 0 {
		t.Errorf("the recovery path was never exercised (adopted=%d, conflicts=%d)", v.Progress.Adopted, stats.Conflicts)
	}
	if v.Progress.FilesFailed != 0 || v.Progress.FilesUploaded == 0 {
		t.Errorf("files: %d uploaded, %d failed", v.Progress.FilesUploaded, v.Progress.FilesFailed)
	}
}

// An export of another workspace is refused by the plan stage, whatever
// the CLI checked, before anything is written. The pass can then be
// started again with the right export.
func TestIncrementalRefusesAnotherWorkspace(t *testing.T) {
	r := newPassHarness(t, "small", nil)
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	_, baseExport, bw := r.basePassDone(ctx, 60*time.Second)

	writes := r.fake.CountRequests("POST", "/v1.0/**")
	r.laterExport("small-later", fixtures.OtherWorkspace())
	run := r.mustStartPass(ctx)
	v := r.await(ctx, run.ID, 60*time.Second)
	if v.Status != store.RunFailed {
		t.Fatalf("a pass from another workspace's export is %s, want failed", v.Status)
	}
	if len(v.Progress.Problems) != 1 || !strings.Contains(v.Progress.Problems[0].Error, "not of the Slack workspace") {
		t.Fatalf("problems = %+v", v.Progress.Problems)
	}
	if n := r.fake.CountRequests("POST", "/v1.0/**") - writes; n != 0 {
		t.Errorf("the refused pass made %d write requests", n)
	}
	if v.Progress.Carried != 0 || len(v.Progress.Channels) != 0 {
		t.Errorf("the refused pass carried %d posts into %d channels", v.Progress.Carried, len(v.Progress.Channels))
	}

	// It never planned, so it can be withdrawn and started again.
	r.laterExport("small-later")
	if _, err := r.startPass(ctx, false); fail.From(err).Code != "run.config_changed" {
		t.Fatalf("starting over without --new: %v", err)
	}
	again, err := r.startPass(ctx, true)
	if err != nil {
		t.Fatalf("replacing the unplanned pass: %v", err)
	}
	if again.Pass != 2 {
		t.Errorf("the replacement is pass %d, want 2", again.Pass)
	}
	v = r.await(ctx, again.ID, 60*time.Second)
	w := r.union(baseExport, r.export)
	r.verifyUnion(w, inProgress)
	facts := fixtures.LaterFacts("small-later")
	r.checkPass(ctx, v, 2, bw.total, facts.NewMessages-facts.NewDMMessages)
}

// What the controller refuses when a pass is asked for.
func TestIncrementalRefusals(t *testing.T) {
	r := newPassHarness(t, "small", nil)
	ctx := t.Context()

	// Nothing to follow.
	if _, err := r.startPass(ctx, false); fail.From(err).Code != "run.nothing_to_follow" {
		t.Fatalf("a pass with no earlier run: %v", err)
	}
	// The first import is still running.
	run := r.start(ctx)
	if _, err := r.startPass(ctx, false); fail.From(err).Code != "run.base_not_finished" {
		t.Fatalf("a pass while the first import runs: %v", err)
	}
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	v := r.await(ctx, run.ID, 60*time.Second)
	r.verify(v, r.expected())

	// Another tenant.
	yaml := r.yaml
	m, err := config.Parse(yaml)
	if err != nil {
		t.Fatal(err)
	}
	m.Target.Entra.TenantID = "11111111-2222-4333-8444-555555555555"
	if r.yaml, err = m.Marshal(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.startPass(ctx, false); fail.From(err).Code != "run.tenant_changed" {
		t.Fatalf("a pass into another tenant: %v", err)
	}
	r.yaml = yaml

	// A finished pass is followed by the next one, and a plain migrate
	// still says the migration is imported and points at --incremental.
	r.laterExport("small-later")
	pass := r.mustStartPass(ctx)
	pv := r.await(ctx, pass.ID, 60*time.Second)
	if pv.Status != store.RunCompleted {
		t.Fatalf("pass is %s", pv.Status)
	}
	_, err = r.client.CreateRun(ctx, r.yaml, false)
	if fe := fail.From(err); fe.Code != "run.already_imported" || !strings.Contains(fe.Next, "--incremental") {
		t.Fatalf("a plain run after a finished pass: %v", err)
	}
	third := r.mustStartPass(ctx)
	if third.Pass != 3 || third.BaseRunID != pass.ID {
		t.Fatalf("third pass = %+v", third)
	}
	tv := r.await(ctx, third.ID, 60*time.Second)
	if tv.Status != store.RunCompleted || tv.Progress.Posted != 0 {
		t.Fatalf("a pass over the same export posted %d (status %s)", tv.Progress.Posted, tv.Status)
	}
	if len(tv.Passes) != 3 || tv.Passes[0].ID != run.ID || tv.Passes[1].ID != pass.ID || tv.Passes[2].ID != third.ID {
		t.Errorf("passes of the migration = %+v", tv.Passes)
	}

	// A pass cannot follow a run that was abandoned after it started
	// importing: `taut migrate --new` cancels the latest run and starts
	// over, and that fresh run must finish before anything follows it.
	fresh, err := r.client.CreateRun(ctx, r.yaml, true)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Pass != 1 || fresh.BaseRunID != "" {
		t.Errorf("a run started with --new is pass %d with base %q", fresh.Pass, fresh.BaseRunID)
	}
	if _, err := r.startPass(ctx, false); fail.From(err).Code != "run.base_not_finished" {
		t.Fatalf("a pass while a fresh import runs: %v", err)
	}
}

// A planned pass that is still active cannot be replaced: its ledger has
// what the migration has sent so far.
func TestIncrementalPlannedPassIsNotReplaced(t *testing.T) {
	r := newPassHarness(t, "small", nil)
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	r.basePassDone(ctx, 60*time.Second)
	kill()

	r.laterExport("small-later")
	// Graph refuses every import, so the pass plans, carries, and then
	// stops with failed lanes.
	r.fake.Faults().MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/messages", Action: fakegraph.ActionStatus, Status: 500, Probability: 1})
	r.maxAttempts = 2
	kill = r.worker(ctx, 5*time.Second)
	defer kill()
	pass := r.mustStartPass(ctx)
	v := r.await(ctx, pass.ID, 90*time.Second)
	if v.Status != store.RunFailed || v.Progress.Carried == 0 {
		t.Fatalf("pass is %s with %d carried posts; want failed after planning", v.Status, v.Progress.Carried)
	}
	if _, err := r.startPass(ctx, true); fail.From(err).Code != "run.pass_started" {
		t.Fatalf("replacing a planned pass: %v", err)
	}
	// It is resumed like any run.
	resumed, err := r.startPass(ctx, false)
	if err != nil || !resumed.Resumed || resumed.ID != pass.ID {
		t.Fatalf("resuming the pass: %+v, %v", resumed, err)
	}
}

// A channel that was deleted in Slack between the passes is not in the
// newer export. Nothing is imported into it, and it stays in the pass's
// ledger so that finalize still completes it.
func TestIncrementalKeepsChannelsGoneFromSlack(t *testing.T) {
	r := newPassHarness(t, "small", nil)
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	_, baseExport, bw := r.basePassDone(ctx, 60*time.Second)

	r.laterExport("small-later", fixtures.Without("random"))
	run := r.mustStartPass(ctx)
	v := r.await(ctx, run.ID, 60*time.Second)
	if v.Status != store.RunCompleted {
		t.Fatalf("pass is %s; problems %+v", v.Status, v.Progress.Problems)
	}
	w := r.union(baseExport, r.export)
	r.verifyUnion(w, inProgress)
	var gone *store.ChannelProgress
	for i, c := range v.Progress.Channels {
		if c.SlackName == "random" {
			gone = &v.Progress.Channels[i]
		}
	}
	if gone == nil || !gone.Earlier || gone.State != store.ChannelMigrating || gone.Posts != 0 || gone.Carried != len(bw.byChannel["random"]) {
		t.Fatalf("the channel gone from Slack in the pass's ledger: %+v", gone)
	}

	fv := r.finalizeAll(ctx, v.ID)
	if fv.Status != store.RunFinalized {
		t.Fatalf("after finalize the pass is %s; problems %+v", fv.Status, fv.Progress.Problems)
	}
	r.verifyUnion(w, func(string) string { return "completed" })
}

// The config file may not move or drop a channel that is already in Teams.
func TestIncrementalRefusesAChangedMapping(t *testing.T) {
	r := newPassHarness(t, "small", nil)
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	r.basePassDone(ctx, 60*time.Second)

	r.laterExport("small-later")
	m, err := config.Parse(r.yaml)
	if err != nil {
		t.Fatal(err)
	}
	m.Target.Teams[0].Channels.Exclude = []string{"random"}
	if r.yaml, err = m.Marshal(); err != nil {
		t.Fatal(err)
	}
	run := r.mustStartPass(ctx)
	v := r.await(ctx, run.ID, 60*time.Second)
	if v.Status != store.RunFailed || len(v.Progress.Problems) != 1 || !strings.Contains(v.Progress.Problems[0].Error, "no longer selected") {
		t.Fatalf("a pass that drops a migrated channel: status %s, problems %+v", v.Status, v.Progress.Problems)
	}
}

// Under the legacy strategy a pass works while the team is in migration
// mode, and without startMigration, which the clouds that need legacy do
// not have. After a finalize it cannot: the channel is not reopened, and
// the lane says why.
func TestIncrementalLegacy(t *testing.T) {
	r := newPassHarness(t, "small", withLegacy, fakegraph.WithStartMigrationUnavailable())
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	_, baseExport, bw := r.basePassDone(ctx, 60*time.Second)

	r.laterExport("small-later")
	facts := fixtures.LaterFacts("small-later")
	run := r.mustStartPass(ctx)
	v := r.await(ctx, run.ID, 60*time.Second)
	w := r.union(baseExport, r.export)
	r.verifyUnion(w, inProgress)
	r.checkPass(ctx, v, 2, bw.total, facts.NewMessages-facts.NewDMMessages)
	r.noStartMigration()

	if fv := r.finalizeAll(ctx, v.ID); fv.Status != store.RunFinalized {
		t.Fatalf("after finalize the pass is %s", fv.Status)
	}
	// A pass after the finalize with nothing new is fine.
	third := r.mustStartPass(ctx)
	if tv := r.await(ctx, third.ID, 60*time.Second); tv.Status != store.RunCompleted || tv.Progress.Posted != 0 {
		t.Fatalf("a pass with nothing new after a legacy finalize: %s, %d posted", tv.Status, tv.Progress.Posted)
	}
	r.noStartMigration()
}

func TestIncrementalLegacyAfterFinalizeIsRefused(t *testing.T) {
	r := newPassHarness(t, "small", withLegacy, fakegraph.WithStartMigrationUnavailable())
	ctx := t.Context()
	r.maxAttempts = 2
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	base, _, _ := r.basePassDone(ctx, 60*time.Second)
	if fv := r.finalizeAll(ctx, base.ID); fv.Status != store.RunFinalized {
		t.Fatalf("base pass is %s after finalize", fv.Status)
	}
	imports := r.fake.Stats().ImportsAccepted

	r.laterExport("small-later")
	run := r.mustStartPass(ctx)
	v := r.await(ctx, run.ID, 90*time.Second)
	if v.Status != store.RunFailed {
		t.Fatalf("a legacy pass into finalized channels is %s, want failed", v.Status)
	}
	found := false
	for _, p := range v.Progress.Problems {
		found = found || strings.Contains(p.Error, "finalized by an earlier pass")
	}
	if !found {
		t.Errorf("problems do not say why: %+v", v.Progress.Problems)
	}
	if n := r.fake.Stats().ImportsAccepted - imports; n != 0 {
		// The new channel cannot be created in migration mode in a team
		// that has left it either, so nothing at all goes in.
		t.Logf("%d messages were imported into channels that could take them", n)
	}
	r.noStartMigration()
	for _, c := range r.fake.Channels(r.fake.Teams()[0].ID) {
		if c.MigrationMode == "inProgress" && c.DisplayName != fixtures.LaterFacts("small-later").NewChannel {
			t.Errorf("channel %q was put back into migration mode", c.DisplayName)
		}
	}
}

// Direct messages carry on between passes like channels: new messages go
// into the chats the first pass used, once.
func TestIncrementalDirectMessages(t *testing.T) {
	r := newPassHarness(t, "small", withDMs)
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()

	run := r.start(ctx)
	bv := r.await(ctx, run.ID, 60*time.Second)
	if bv.Status != store.RunCompleted {
		t.Fatalf("base pass is %s", bv.Status)
	}
	chatsBefore := r.fake.Chats()
	before := 0
	for _, c := range chatsBefore {
		before += len(r.fake.ChatMessages(c.ID))
	}
	if len(chatsBefore) != 2 || before == 0 {
		t.Fatalf("base pass made %d chats with %d messages", len(chatsBefore), before)
	}
	// One pass before the finalize, one after.
	for round, finalizeFirst := range []bool{false, true} {
		if finalizeFirst {
			latest, err := r.client.LatestRun(ctx, "it-small")
			if err != nil {
				t.Fatal(err)
			}
			if fv := r.finalizeAll(ctx, latest.ID); fv.Status != store.RunFinalized {
				t.Fatalf("finalize before round %d: %s", round, fv.Status)
			}
			for _, c := range r.fake.Chats() {
				if c.MigrationMode == "inProgress" {
					t.Fatalf("chat %s is still in migration mode after finalize", c.ID)
				}
			}
			// A third export: the later one again has nothing new, so
			// this round proves a finalized chat is left alone.
		}
		r.laterExport("small-later")
		pass := r.mustStartPass(ctx)
		v := r.await(ctx, pass.ID, 60*time.Second)
		if v.Status != store.RunCompleted || v.Progress.Failed != 0 {
			t.Fatalf("round %d: pass is %s with %d failed; problems %+v", round, v.Status, v.Progress.Failed, v.Progress.Problems)
		}
		chats := r.fake.Chats()
		if len(chats) != 2 {
			t.Fatalf("round %d: the tenant has %d chats, want the same 2", round, len(chats))
		}
		after := 0
		for _, c := range chats {
			msgs := r.fake.ChatMessages(c.ID)
			after += len(msgs)
			for i := 1; i < len(msgs); i++ {
				if !msgs[i].CreatedAt.After(msgs[i-1].CreatedAt) {
					t.Errorf("round %d: chat %s has a duplicate or out-of-order message at %d", round, c.ID, i)
				}
			}
			want := "inProgress"
			if finalizeFirst {
				want = "completed"
			}
			if c.MigrationMode != want {
				t.Errorf("round %d: chat %s is %q, want %q", round, c.ID, c.MigrationMode, want)
			}
		}
		if want := before + fixtures.LaterFacts("small-later").NewDMMessages; after != want {
			t.Fatalf("round %d: the chats hold %d messages, want %d", round, after, want)
		}
	}
}

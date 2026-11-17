package worker_test

import (
	"context"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/fakegraph"
	"github.com/bgfdev/taut/internal/graph"
	"github.com/bgfdev/taut/internal/store"
)

// withLegacy selects the creation-mode import path.
func withLegacy(m *config.Config) { m.Load.Strategy = config.StrategyLegacy }

// noStartMigration asserts the legacy path never called the API that the
// national clouds do not have.
func (r *harness) noStartMigration() {
	r.t.Helper()
	if n := r.fake.CountRequests("POST", "/v1.0/teams/*/channels/*/startMigration"); n != 0 {
		r.t.Errorf("%d startMigration requests under the legacy strategy; that API does not exist where legacy is needed", n)
	}
}

// checkAuthors asserts every imported message is attributed to the account
// its Slack author maps to.
func (r *harness) checkAuthors(ctx context.Context, runID string, w want) {
	t := r.t
	t.Helper()
	mapped, err := r.store.Users(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	entraOf := map[string]string{}
	for _, u := range mapped {
		entraOf[u.SlackID] = u.EntraID
	}
	team := r.fake.Teams()[0]
	checked := 0
	for _, c := range r.fake.Channels(team.ID) {
		name := c.DisplayName
		if name == "General" {
			name = "general"
		}
		msgs, stored := w.byChannel[name], r.fake.Messages(team.ID, c.ID)
		if len(msgs) != len(stored) {
			continue // verify has reported it
		}
		for i, m := range stored {
			if m.FromUserID == "" {
				t.Errorf("channel %q message %d has no author", c.DisplayName, i)
			}
			if id := entraOf[msgs[i].User]; id != "" && m.FromUserID != id {
				t.Errorf("channel %q message %s is attributed to %s, want its author %s", c.DisplayName, msgs[i].TS, m.FromUserID, id)
			} else if id != "" {
				checked++
			}
		}
	}
	if checked == 0 {
		t.Error("no message had a mapped author to check")
	}
}

// checkLegacyBeforeFinalize: the team exists in migration mode with nobody
// in it, not even its owner.
func (r *harness) checkLegacyBeforeFinalize() {
	r.t.Helper()
	team := r.fake.Teams()[0]
	if team.MigrationMode != "inProgress" {
		r.t.Errorf("team migration mode = %q before finalize, want inProgress", team.MigrationMode)
	}
	if len(team.Members) != 0 {
		r.t.Errorf("the team has %d members before finalize; a team in migration mode has none", len(team.Members))
	}
}

// checkLegacyFinalized: the team's migration is completed, once; the owner
// is its owner; General is completed.
func (r *harness) checkLegacyFinalized(ctx context.Context, v controller.RunView) {
	t := r.t
	t.Helper()
	teams, err := r.store.Teams(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	team := r.fake.Teams()[0]
	if team.MigrationMode != "completed" {
		t.Errorf("team migration mode = %q after finalize, want completed", team.MigrationMode)
	}
	ownerIsOwner := false
	for _, m := range team.Members {
		if m.UserID == teams[0].OwnerID {
			ownerIsOwner = m.Owner
		}
	}
	if !ownerIsOwner {
		t.Error("the configured owner is not an owner of the team after finalize")
	}
	for _, c := range r.fake.Channels(team.ID) {
		if c.DisplayName == "General" && c.MigrationMode != "completed" {
			t.Errorf("General migration mode = %q after finalize", c.MigrationMode)
		}
	}
	if n := r.logCount(ctx, v.ID, store.LogFilter{Op: "team.complete_migration"}); n != 1 {
		t.Errorf("audit log has %d team.complete_migration entries, want 1", n)
	}
	if n := r.logCount(ctx, v.ID, store.LogFilter{Op: "member.add_owner"}); n != 1 {
		t.Errorf("audit log has %d member.add_owner entries, want 1", n)
	}
}

// The legacy strategy end to end, against a tenant with no startMigration:
// every message once with its original time and author, files uploaded while
// the channels are in migration mode, nobody in the team until finalize,
// and then the team completed, its owner and members in.
func TestLegacyMigrateAndFinalize(t *testing.T) {
	for _, fixture := range []string{"small", "medium"} {
		t.Run(fixture, func(t *testing.T) {
			r := newHarnessWith(t, fixture, withLegacy, fakegraph.WithStartMigrationUnavailable())
			ctx := t.Context()
			kill := r.worker(ctx, 5*time.Second)
			defer kill()

			run := r.start(ctx)
			v := r.await(ctx, run.ID, 3*time.Minute)
			w := r.expected()
			r.verify(v, w)
			r.checkAuthors(ctx, run.ID, w)
			r.checkLegacyBeforeFinalize()
			r.noStartMigration()
			if v.Progress.FilesFailed != 0 || v.Progress.FilesUploaded == 0 {
				t.Errorf("files: %d uploaded, %d failed; with the folder available in migration mode all must be uploaded", v.Progress.FilesUploaded, v.Progress.FilesFailed)
			}
			// Every channel was created old enough for its history, and
			// the team older still.
			team := r.fake.Teams()[0]
			for _, c := range r.fake.Channels(team.ID) {
				if msgs := r.fake.Messages(team.ID, c.ID); len(msgs) > 0 && !c.CreatedAt.Before(msgs[0].CreatedAt) {
					t.Errorf("channel %q was created at %s, not before its first message", c.DisplayName, c.CreatedAt)
				}
			}

			mv, err := r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{})
			if err != nil {
				t.Fatal(err)
			}
			if mv.Teams != 1 {
				t.Errorf("finalize selection = %+v", mv)
			}
			v = r.awaitFinalize(ctx, run.ID, 90*time.Second)
			r.checkFinalized(ctx, v)
			r.checkLegacyFinalized(ctx, v)
			r.noStartMigration()
			if single := r.fake.CountRequests("POST", "/v1.0/teams/*/members"); single != 1 {
				t.Errorf("%d single member requests, want one: the owner", single)
			}
		})
	}
}

// A partial finalize under legacy completes the chosen channels and nothing
// else: the team stays in migration mode, with no members, until the rest
// are done.
func TestLegacyPartialFinalizeLeavesTheTeamClosed(t *testing.T) {
	r := newHarnessWith(t, "small", withLegacy, fakegraph.WithStartMigrationUnavailable())
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	run := r.start(ctx)
	v := r.await(ctx, run.ID, 60*time.Second)
	r.verify(v, r.expected())

	first := v.Progress.Channels[0].SlackID
	mv, err := r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{Channels: []string{first}})
	if err != nil {
		t.Fatal(err)
	}
	if mv.Channels != 1 || mv.Teams != 0 {
		t.Errorf("partial selection = %+v", mv)
	}
	v = r.awaitFinalize(ctx, run.ID, 60*time.Second)
	if v.Status != store.RunCompleted {
		t.Fatalf("after a partial finalize the run is %s, want completed", v.Status)
	}
	r.checkLegacyBeforeFinalize()
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "team.complete_migration"}); n != 0 {
		t.Errorf("the team was completed by a partial finalize")
	}

	if _, err := r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{}); err != nil {
		t.Fatal(err)
	}
	v = r.awaitFinalize(ctx, run.ID, 60*time.Second)
	r.checkFinalized(ctx, v)
	r.checkLegacyFinalized(ctx, v)
}

// When no Slack channel is imported into the team's General channel, it is
// still in migration mode and still has to be completed before the team can
// be.
func TestLegacyCompletesGeneralNobodyImportedInto(t *testing.T) {
	r := newHarnessWith(t, "small", func(m *config.Config) {
		withLegacy(m)
		m.Target.Teams[0].Channels.Exclude = []string{"general"}
	}, fakegraph.WithStartMigrationUnavailable())
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	run := r.start(ctx)
	v := r.await(ctx, run.ID, 60*time.Second)
	if v.Status != store.RunCompleted {
		t.Fatalf("status = %s; problems %+v", v.Status, v.Progress.Problems)
	}
	team := r.fake.Teams()[0]
	var general fakegraph.ChannelView
	for _, c := range r.fake.Channels(team.ID) {
		if c.DisplayName == "General" {
			general = c
		}
	}
	if general.MigrationMode != "inProgress" || general.MessageCount != 0 {
		t.Fatalf("General = %+v, want in migration mode and empty", general)
	}

	if _, err := r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{Retain: true}); err != nil {
		t.Fatal(err)
	}
	v = r.awaitFinalize(ctx, run.ID, 60*time.Second)
	if v.Status != store.RunFinalized {
		t.Fatalf("status = %s; problems %+v", v.Status, v.Progress.Problems)
	}
	r.checkLegacyFinalized(ctx, v)
	for _, c := range r.fake.Channels(team.ID) {
		if c.MigrationMode != "completed" {
			t.Errorf("channel %q migration mode = %q", c.DisplayName, c.MigrationMode)
		}
	}
	page, err := r.client.AuditLog(ctx, run.ID, store.LogFilter{Op: "channel.complete_migration", Stream: "members:0"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 || page.Entries[0].TargetID != general.ID {
		t.Errorf("General's completion is recorded %d times in the members lane, want once: %+v", len(page.Entries), page.Entries)
	}
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "member.add", Outcome: "ok"}); n == 0 {
		t.Error("no members were added")
	}
	if err := r.store.VerifyLog(ctx, run.ID); err != nil {
		t.Errorf("audit log: %v", err)
	}
}

// The legacy import and its finalize with workers dying and Graph losing
// responses, including the ones legacy alone depends on: creating the team
// and the channels in migration mode, and completing the channels and the
// team.
func TestLegacySurvivesKillsAndFaults(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos run skipped in -short mode")
	}
	r := newHarnessWith(t, "medium", withLegacy, fakegraph.WithStartMigrationUnavailable(), fakegraph.WithRandSeed(9))
	ctx := t.Context()

	faults := r.fake.Faults()
	// The team is created and the response never arrives.
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams", Action: fakegraph.ActionFailAfterCommit, Count: 1})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels", Action: fakegraph.ActionFailAfterCommit, Probability: 0.3})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/messages", Action: fakegraph.ActionFailAfterCommit, Probability: 0.03})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/messages/*/replies", Action: fakegraph.ActionFailAfterCommit, Probability: 0.03})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/messages", Action: fakegraph.ActionThrottle, RetryAfterSeconds: 1, Probability: 0.03})
	faults.MustAdd(fakegraph.Fault{Method: "PUT", Path: "/_upload/**", Action: fakegraph.ActionFailAfterCommit, Probability: 0.05})
	faults.MustAdd(fakegraph.Fault{Method: "GET", Path: "/v1.0/teams/*/channels/*/filesFolder", Action: fakegraph.ActionStatus, Status: 503, Probability: 0.2})

	run := r.start(ctx)
	var mu sync.Mutex
	kills := []func(){r.worker(ctx, 600*time.Millisecond), r.worker(ctx, 600*time.Millisecond), r.worker(ctx, 600*time.Millisecond)}
	chaos := func(every, spread int) (stop func() int) {
		quit, done := make(chan struct{}), make(chan struct{})
		killed := 0
		go func() {
			defer close(done)
			rng := rand.New(rand.NewPCG(17, 19))
			for {
				select {
				case <-quit:
					return
				case <-time.After(time.Duration(every+rng.IntN(spread)) * pace * time.Millisecond):
				}
				mu.Lock()
				i := rng.IntN(len(kills))
				kills[i]()
				kills[i] = r.worker(ctx, 600*time.Millisecond)
				killed++
				mu.Unlock()
			}
		}()
		return func() int { close(quit); <-done; return killed }
	}

	stop := chaos(60, 140)
	v := r.await(ctx, run.ID, 8*time.Minute)
	killedImport := stop()
	w := r.expected()
	r.verify(v, w)
	r.checkAuthors(ctx, run.ID, w)
	r.checkLegacyBeforeFinalize()
	r.noStartMigration()
	stats := r.fake.Stats()
	if stats.ImportsAccepted != v.Progress.Posted {
		t.Errorf("Graph accepted %d imports but %d posts exist: a message was written twice", stats.ImportsAccepted, v.Progress.Posted)
	}
	if v.Progress.FilesFailed != 0 {
		t.Errorf("%d files failed", v.Progress.FilesFailed)
	}
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "team.adopt"}); n != 1 {
		t.Errorf("%d team.adopt entries; the team whose creation response was lost should have been found and adopted once", n)
	}

	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/completeMigration", Action: fakegraph.ActionFailAfterCommit, Probability: 0.4})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/completeMigration", Action: fakegraph.ActionFailAfterCommit, Count: 2})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/completeMigration", Action: fakegraph.ActionStatus, Status: 503, Probability: 0.3})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/members", Action: fakegraph.ActionFailAfterCommit, Count: 1})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/members/add", Action: fakegraph.ActionFailAfterCommit, Count: 1})
	if _, err := r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{}); err != nil {
		t.Fatal(err)
	}
	stop = chaos(25, 35)
	v = r.awaitFinalize(ctx, run.ID, 4*time.Minute)
	killedFinalize := stop()
	mu.Lock()
	for _, k := range kills {
		k()
	}
	mu.Unlock()

	r.checkFinalized(ctx, v)
	r.checkLegacyFinalized(ctx, v)
	r.noStartMigration()
	t.Logf("workers killed: %d importing, %d finalizing; imports accepted: %d; conflicts: %d; adopted: %d",
		killedImport, killedFinalize, stats.ImportsAccepted, stats.Conflicts, v.Progress.Adopted)
	if killedImport < 5 {
		t.Errorf("only %d workers were killed during the import", killedImport)
	}
}

// A team with the configured name already exists and is not in migration
// mode. Legacy cannot import into it, and must say so rather than try.
func TestLegacyRefusesAnExistingTeam(t *testing.T) {
	r := newHarnessWith(t, "small", withLegacy, fakegraph.WithStartMigrationUnavailable())
	ctx := t.Context()
	m, err := config.Parse(r.yaml)
	if err != nil {
		t.Fatal(err)
	}
	g, err := r.graphFactory()(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	var owner string
	for u, err := range g.Users(ctx, graph.UserQuery{PageSize: 999}) {
		if err != nil {
			t.Fatal(err)
		}
		if owner == "" {
			owner = u.ID
		}
	}
	existing, err := g.CreateTeam(ctx, graph.TeamSpec{DisplayName: "Fixture Corp", OwnerID: owner})
	if err != nil {
		t.Fatal(err)
	}

	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	run := r.start(ctx)
	v := r.await(ctx, run.ID, 60*time.Second)
	if v.Status != store.RunFailed {
		t.Fatalf("status = %s, want failed", v.Status)
	}
	found := false
	for _, pr := range v.Progress.Problems {
		if strings.Contains(pr.Error, "team.exists_legacy") && strings.Contains(pr.Error, "cannot import into an existing team") {
			found = true
		}
	}
	if !found {
		t.Errorf("the failure does not say why: %+v", v.Progress.Problems)
	}
	teams := r.fake.Teams()
	if len(teams) != 1 || teams[0].ID != existing {
		t.Fatalf("tenant has %d teams, want only the one that was there", len(teams))
	}
	channels := r.fake.Channels(existing)
	if len(channels) != 1 || channels[0].MessageCount != 0 || channels[0].MigrationMode != "" {
		t.Errorf("the existing team was written to: %+v", channels)
	}
	if v.Progress.Posted != 0 {
		t.Errorf("%d posts imported", v.Progress.Posted)
	}
	r.noStartMigration()
}

// Something completes the channels' migration mid-import. The modern
// strategy reopens them; legacy cannot, and must stop with a message that
// says so instead of trying an API that does not exist.
func TestLegacyChannelThatLeftMigrationModeFails(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped in -short mode")
	}
	r := newHarnessWith(t, "medium", withLegacy, fakegraph.WithStartMigrationUnavailable())
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	run := r.start(ctx)

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
	closed := map[string]bool{}
	for _, c := range r.fake.Channels(team.ID) {
		if c.MigrationMode == "inProgress" {
			if err := g.CompleteChannelMigration(ctx, team.ID, c.ID); err == nil {
				closed[c.ID] = true
			}
		}
	}
	if len(closed) == 0 {
		t.Fatal("no channel was in migration mode to close")
	}

	v := r.await(ctx, run.ID, 3*time.Minute)
	if v.Status != store.RunFailed {
		t.Fatalf("status = %s, want failed: a legacy channel cannot be reopened", v.Status)
	}
	said := 0
	for _, pr := range v.Progress.Problems {
		if strings.Contains(pr.Error, "load.left_migration_legacy") && strings.Contains(pr.Error, "cannot reopen it") {
			said++
		}
	}
	if said == 0 {
		t.Errorf("no lane says the channel left migration mode and cannot be reopened: %+v", v.Progress.Problems)
	}
	r.noStartMigration()
	// Nothing was imported twice, and nothing after the channels closed.
	stats := r.fake.Stats()
	if stats.ImportsAccepted != v.Progress.Posted {
		t.Errorf("Graph accepted %d imports, the ledger records %d", stats.ImportsAccepted, v.Progress.Posted)
	}
	for _, c := range r.fake.Channels(team.ID) {
		if closed[c.ID] && c.MigrationMode == "inProgress" {
			t.Errorf("channel %q is in migration mode again", c.DisplayName)
		}
	}
	t.Logf("channels closed mid-import: %d; lanes that stopped saying so: %d", len(closed), said)
}

// Whether a channel's files folder can be used during migration mode is not
// documented, and under legacy files are uploaded during it. If the folder
// never appears, the channel is imported with a note in place of each file
// rather than held up for good.
func TestLegacyFilesFolderUnavailableInMigrationMode(t *testing.T) {
	r := newHarnessWith(t, "small", withLegacy, fakegraph.WithStartMigrationUnavailable(), fakegraph.WithFilesFolderBlockedInMigration())
	r.folderWait = 300 * time.Millisecond
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()

	started := time.Now()
	run := r.start(ctx)
	v := r.await(ctx, run.ID, 60*time.Second)
	if v.Status != store.RunCompleted {
		t.Fatalf("status = %s; problems %+v", v.Status, v.Progress.Problems)
	}
	w := r.expected()
	files := 0
	for _, ids := range w.files {
		files += len(ids)
	}
	if files == 0 {
		t.Fatal("the fixture has no files; this test would prove nothing")
	}
	p := v.Progress
	if p.FilesUploaded != 0 || p.FilesFailed != files {
		t.Errorf("files: %d uploaded, %d failed; want none uploaded and all %d settled as not moved", p.FilesUploaded, p.FilesFailed, files)
	}
	if p.Posted != w.total || p.Failed != 0 || p.Held != 0 {
		t.Errorf("posted=%d failed=%d held=%d, want all %d messages imported", p.Posted, p.Failed, p.Held, w.total)
	}
	team := r.fake.Teams()[0]
	notes, attached := 0, 0
	for _, c := range r.fake.Channels(team.ID) {
		if n := len(r.fake.Files(team.ID, c.ID)); n != 0 {
			t.Errorf("channel %q has %d files in SharePoint", c.DisplayName, n)
		}
		for _, m := range r.fake.Messages(team.ID, c.ID) {
			notes += strings.Count(m.BodyContent, "(not migrated)")
			attached += len(m.Attachments) + m.HostedContents
		}
	}
	if notes < files || attached != 0 {
		t.Errorf("%d notes and %d attachments for %d files; each file's message must carry a note and nothing else", notes, attached, files)
	}
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "file.fail", Contains: "folder_unavailable"}); n != files {
		t.Errorf("audit log has %d file.fail entries for the missing folder, want %d", n, files)
	}
	if took := time.Since(started); took > 30*time.Second {
		t.Errorf("the run took %s; the wait for the folder must be bounded", took)
	}
	r.noStartMigration()
	if err := r.store.VerifyLog(ctx, run.ID); err != nil {
		t.Errorf("audit log: %v", err)
	}
}

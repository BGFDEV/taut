package worker_test

import (
	"context"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/fakegraph"
	"github.com/bgfdev/taut/internal/slack/export"
	"github.com/bgfdev/taut/internal/staging"
	"github.com/bgfdev/taut/internal/store"
)

// awaitFinalize runs the control loop until the run leaves "finalizing".
func (r *harness) awaitFinalize(ctx context.Context, id string, limit time.Duration) controller.RunView {
	r.t.Helper()
	deadline := time.Now().Add(limit)
	for {
		r.api.Reconcile(ctx)
		v, err := r.client.Run(ctx, id)
		if err != nil {
			r.t.Fatal(err)
		}
		if v.Status != store.RunFinalizing && (v.Status != store.RunFinalized || v.Retain || v.PurgedAt != nil) {
			return v
		}
		if time.Now().After(deadline) {
			r.t.Fatalf("finalize did not finish in %s: status %s, progress %+v", limit, v.Status, v.Progress)
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func (r *harness) logCount(ctx context.Context, runID string, f store.LogFilter) int {
	r.t.Helper()
	n := 0
	seen := map[store.LogCursor]bool{}
	for {
		page, err := r.client.AuditLog(ctx, runID, f)
		if err != nil {
			r.t.Fatal(err)
		}
		for _, e := range page.Entries {
			k := store.LogCursor{Stream: e.Stream, Seq: e.Seq}
			if seen[k] {
				r.t.Fatalf("audit log entry %v returned twice", k)
			}
			seen[k] = true
		}
		n += len(page.Entries)
		if page.Next == nil {
			return n
		}
		f.After = *page.Next
	}
}

func (r *harness) stagedObjects(ctx context.Context, runID string) int {
	r.t.Helper()
	n := 0
	for _, err := range r.blobs.List(ctx, staging.RunPrefix(runID)) {
		if err != nil {
			r.t.Fatal(err)
		}
		n++
	}
	return n
}

// checkFinalized asserts the end state of a finalized run in the fake tenant
// and the ledger.
func (r *harness) checkFinalized(ctx context.Context, v controller.RunView) {
	r.t.Helper()
	if v.Status != store.RunFinalized {
		r.t.Fatalf("status = %s, want finalized; problems %+v", v.Status, v.Progress.Problems)
	}
	channels := 0
	for _, team := range r.fake.Teams() {
		for _, c := range r.fake.Channels(team.ID) {
			if c.MigrationMode == "" {
				continue // never in migration mode: not ours
			}
			channels++
			if c.MigrationMode != "completed" {
				r.t.Errorf("channel %q migration mode = %q, want completed", c.DisplayName, c.MigrationMode)
			}
		}
	}
	if channels != len(v.Progress.Channels) || channels == 0 {
		r.t.Errorf("%d channels completed in Teams, run has %d", channels, len(v.Progress.Channels))
	}
	for _, c := range v.Progress.Channels {
		if c.State != store.ChannelCompleted {
			r.t.Errorf("channel %s state = %s", c.SlackName, c.State)
		}
	}

	added := r.logCount(ctx, v.ID, store.LogFilter{Op: "member.add", Outcome: "ok"})
	if added == 0 {
		r.t.Error("no members were added")
	}
	for _, team := range r.fake.Teams() {
		if team.CreatedBy == "" {
			continue
		}
		owners, members := 0, 0
		seen := map[string]bool{}
		for _, m := range team.Members {
			if seen[m.UserID] {
				r.t.Errorf("user %s is in team %q twice", m.UserID, team.DisplayName)
			}
			seen[m.UserID] = true
			if m.Owner {
				owners++
			} else {
				members++
			}
		}
		if owners != 1 {
			r.t.Errorf("team %q has %d owners, want the 1 it was created with", team.DisplayName, owners)
		}
		if members != added {
			r.t.Errorf("team %q has %d members; the audit log records %d added", team.DisplayName, members, added)
		}
	}
	// Membership is access: nobody who was a guest, a bot, deactivated or
	// from another organization in Slack may have been given it.
	ex, err := export.Open(r.export)
	if err != nil {
		r.t.Fatal(err)
	}
	defer ex.Close()
	exUsers, err := ex.Users()
	if err != nil {
		r.t.Fatal(err)
	}
	mapped, err := r.store.Users(ctx, v.ID)
	if err != nil {
		r.t.Fatal(err)
	}
	entraOf := map[string]string{}
	for _, u := range mapped {
		entraOf[u.SlackID] = u.EntraID
	}
	barred, eligible := map[string]string{}, map[string]bool{}
	for _, u := range exUsers {
		id := entraOf[u.ID]
		if id == "" {
			continue
		}
		if u.Deleted || u.IsBot || u.IsAppUser || u.IsStranger || u.IsGuest() {
			barred[id] = u.Name
		} else {
			eligible[id] = true
		}
	}
	for _, team := range r.fake.Teams() {
		if team.CreatedBy == "" {
			continue
		}
		for _, m := range team.Members {
			if name, bad := barred[m.UserID]; bad && !eligible[m.UserID] && !m.Owner {
				r.t.Errorf("%s (guest, bot, deactivated or external in Slack) was added to team %q", name, team.DisplayName)
			}
		}
	}
	r.t.Logf("fixture has %d mapped users who must not become members", len(barred))
	if got := r.logCount(ctx, v.ID, store.LogFilter{Op: "channel.complete_migration"}); got != channels {
		r.t.Errorf("audit log has %d channel.complete_migration entries, want %d", got, channels)
	}
	if err := r.store.VerifyLog(ctx, v.ID); err != nil {
		r.t.Errorf("audit log: %v", err)
	}
}

func TestFinalizeFinalizesAndPurges(t *testing.T) {
	r := newHarness(t, "small")
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()

	run := r.start(ctx)

	// Not before the import is done.
	if _, err := r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{}); err == nil {
		t.Fatal("finalize was accepted while the run was still importing")
	}
	v := r.await(ctx, run.ID, 60*time.Second)
	r.verify(v, r.expected())
	if r.stagedObjects(ctx, run.ID) == 0 {
		t.Fatal("nothing in staging after the import; the purge check would prove nothing")
	}
	if _, err := r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{Channels: []string{"C-NOPE"}}); err == nil {
		t.Fatal("finalize accepted a channel that is not in the run")
	}
	imports := r.fake.Stats().ImportsAccepted

	mv, err := r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if mv.Channels != len(v.Progress.Channels) || mv.Already != 0 || mv.Teams != 1 {
		t.Errorf("finalize selection = %+v", mv)
	}
	// Asking twice changes nothing.
	if _, err := r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{}); err != nil {
		t.Fatalf("second finalize request: %v", err)
	}
	v = r.awaitFinalize(ctx, run.ID, 60*time.Second)
	r.checkFinalized(ctx, v)

	if v.PurgedAt == nil {
		t.Error("staging was not purged")
	}
	if n := r.stagedObjects(ctx, run.ID); n != 0 {
		t.Errorf("%d objects left in staging after the purge", n)
	}
	got, err := r.store.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.WrappedKey) != 0 {
		t.Error("the run's data key survived the purge")
	}
	if r.fake.Stats().ImportsAccepted != imports {
		t.Error("finalizing imported messages")
	}

	// A finished migration answers a further finalize as already done.
	mv, err = r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{})
	if err != nil || mv.Already != mv.Channels || mv.Run.Status != store.RunFinalized {
		t.Errorf("finalize on a finalized run = %+v, %v", mv, err)
	}
}

func TestFinalizeRetainKeepsStaging(t *testing.T) {
	r := newHarness(t, "nasty")
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	run := r.start(ctx)
	r.await(ctx, run.ID, 60*time.Second)
	if _, err := r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{Retain: true}); err != nil {
		t.Fatal(err)
	}
	v := r.awaitFinalize(ctx, run.ID, 60*time.Second)
	r.checkFinalized(ctx, v)
	r.api.Reconcile(ctx)
	if v.PurgedAt != nil || r.stagedObjects(ctx, run.ID) == 0 {
		t.Error("--retain did not keep staging")
	}
}

// A partial finalize opens the chosen channels, leaves the rest importable and
// holds the members back until the whole team is open.
func TestFinalizePartialThenRest(t *testing.T) {
	r := newHarness(t, "small")
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	run := r.start(ctx)
	v := r.await(ctx, run.ID, 60*time.Second)
	if len(v.Progress.Channels) < 2 {
		t.Skip("fixture has a single channel")
	}
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
	done := 0
	for _, c := range v.Progress.Channels {
		if c.State == store.ChannelCompleted {
			done++
			if c.SlackID != first {
				t.Errorf("channel %s was completed but not selected", c.SlackName)
			}
		}
	}
	if done != 1 {
		t.Errorf("%d channels completed, want 1", done)
	}
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "member.add"}); n != 0 {
		t.Errorf("%d members added while channels are still locked", n)
	}
	if r.stagedObjects(ctx, run.ID) == 0 {
		t.Error("staging was purged by a partial finalize")
	}

	mv, err = r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if mv.Already != 1 || mv.Teams != 1 {
		t.Errorf("second selection = %+v", mv)
	}
	v = r.awaitFinalize(ctx, run.ID, 60*time.Second)
	r.checkFinalized(ctx, v)
}

// Workers die and Graph loses responses while finalizing; every channel
// is still completed exactly once and every member added once.
func TestFinalizeSurvivesKillsAndFaults(t *testing.T) {
	r := newHarness(t, "medium")
	ctx := t.Context()
	kill := r.worker(ctx, 2*time.Second)
	run := r.start(ctx)
	r.await(ctx, run.ID, 120*time.Second)

	faults := r.fake.Faults()
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/completeMigration", Action: fakegraph.ActionFailAfterCommit, Probability: 0.4})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/members", Action: fakegraph.ActionFailAfterCommit, Probability: 0.1})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/members", Action: fakegraph.ActionThrottle, RetryAfterSeconds: 1, Probability: 0.05})
	// Members are added in bulk now; the same faults on that request. A
	// team's members fit in one request, so the lost response is certain
	// the first two times rather than likely.
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/members/add", Action: fakegraph.ActionFailAfterCommit, Count: 2})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/members/add", Action: fakegraph.ActionThrottle, RetryAfterSeconds: 1, Probability: 0.3})

	if _, err := r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{}); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(150 * time.Millisecond):
				kill()
				kill = r.worker(ctx, 2*time.Second)
			}
		}
	}()
	v := r.awaitFinalize(ctx, run.ID, 180*time.Second)
	close(stop)
	<-done
	kill()
	r.checkFinalized(ctx, v)
}

// Two partial finalize requests that overlap: the second arrives before the first has
// finished. Between them they open the whole team, so it must get its
// members before the run is finalized and staging is purged.
func TestFinalizeOverlappingPartialRequests(t *testing.T) {
	r := newHarness(t, "small")
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	run := r.start(ctx)
	v := r.await(ctx, run.ID, 60*time.Second)
	if len(v.Progress.Channels) < 2 {
		t.Skip("fixture has a single channel")
	}
	// No worker is running, so the first request cannot finish first.
	kill()
	var rest []string
	for _, c := range v.Progress.Channels[1:] {
		rest = append(rest, c.SlackID)
	}
	if _, err := r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{Channels: []string{v.Progress.Channels[0].SlackID}}); err != nil {
		t.Fatal(err)
	}
	mv, err := r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{Channels: rest})
	if err != nil {
		t.Fatal(err)
	}
	if mv.Teams != 1 {
		t.Errorf("the request that completes the team planned members for %d teams, want 1", mv.Teams)
	}
	kill = r.worker(ctx, 5*time.Second)
	defer kill()
	v = r.awaitFinalize(ctx, run.ID, 60*time.Second)
	r.checkFinalized(ctx, v)
}

// People who were in the team before Taut are left as they are: an owner is
// not demoted, and nobody already present is added or logged again.
func TestFinalizeLeavesExistingMembersAlone(t *testing.T) {
	r := newHarness(t, "small")
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	run := r.start(ctx)
	r.await(ctx, run.ID, 60*time.Second)

	// Make someone the export will want as a member a second owner first.
	mf, err := config.Parse(r.yaml)
	if err != nil {
		t.Fatal(err)
	}
	g, err := r.graphFactory()(ctx, mf)
	if err != nil {
		t.Fatal(err)
	}
	var teamID string
	for _, team := range r.fake.Teams() {
		if team.CreatedBy != "" {
			teamID = team.ID
		}
	}
	users, err := r.store.Users(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	teams, err := r.store.Teams(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var second string
	for _, u := range users {
		if u.EntraID != "" && u.EntraID != teams[0].OwnerID && u.Method != "unmapped" {
			second = u.EntraID
			break
		}
	}
	if second == "" {
		t.Skip("no second mapped user in the fixture")
	}
	if err := g.AddTeamMember(ctx, teamID, second, true); err != nil {
		t.Fatal(err)
	}

	if _, err := r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{Retain: true}); err != nil {
		t.Fatal(err)
	}
	v := r.awaitFinalize(ctx, run.ID, 60*time.Second)
	if v.Status != store.RunFinalized {
		t.Fatalf("status %s; problems %+v", v.Status, v.Progress.Problems)
	}
	owners := 0
	for _, team := range r.fake.Teams() {
		if team.ID != teamID {
			continue
		}
		for _, m := range team.Members {
			if m.Owner {
				owners++
			}
		}
	}
	if owners != 2 {
		t.Errorf("team has %d owners after finalize, want the 2 it had", owners)
	}
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "member.add", Contains: second}); n != 0 {
		t.Errorf("an existing owner was recorded as added %d times", n)
	}
}

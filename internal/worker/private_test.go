package worker_test

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/devstack"
	"github.com/bgfdev/taut/internal/fakegraph"
	"github.com/bgfdev/taut/internal/slack"
	"github.com/bgfdev/taut/internal/slack/export"
	"github.com/bgfdev/taut/internal/store"
)

// membership is who the export says must end up where: Entra IDs of the
// eligible, mapped Slack members, for the team and for each private channel
// by its Slack name. It is computed straight from the export and the run's
// identity mapping, not through the members stage.
type membership struct {
	team    map[string]bool
	private map[string]map[string]bool
	// slackIDs is the private channels' Slack IDs by name.
	slackIDs map[string]string
}

func (r *harness) expectedMembers(ctx context.Context, runID string) membership {
	r.t.Helper()
	ex, err := export.Open(r.export)
	if err != nil {
		r.t.Fatal(err)
	}
	defer ex.Close()
	exUsers, err := ex.Users()
	if err != nil {
		r.t.Fatal(err)
	}
	eligible := map[string]bool{}
	for _, u := range exUsers {
		eligible[u.ID] = !u.Deleted && !u.IsBot && !u.IsAppUser && !u.IsStranger && !u.IsGuest()
	}
	mapped, err := r.store.Users(ctx, runID)
	if err != nil {
		r.t.Fatal(err)
	}
	entraOf := map[string]string{}
	for _, u := range mapped {
		entraOf[u.SlackID] = u.EntraID
	}
	chans, err := ex.Channels()
	if err != nil {
		r.t.Fatal(err)
	}
	m := membership{team: map[string]bool{}, private: map[string]map[string]bool{}, slackIDs: map[string]string{}}
	for _, ch := range chans {
		if ch.Kind != slack.KindPublic && (ch.Kind != slack.KindPrivate || !r.private) {
			continue
		}
		var own map[string]bool
		if ch.Kind == slack.KindPrivate {
			own = map[string]bool{}
			m.private[ch.Name] = own
			m.slackIDs[ch.Name] = ch.ID
		}
		for _, id := range ch.Members {
			if !eligible[id] || entraOf[id] == "" {
				continue
			}
			m.team[entraOf[id]] = true
			if own != nil {
				own[entraOf[id]] = true
			}
		}
	}
	return m
}

// checkPrivateMembers asserts that every private channel holds exactly its
// own eligible Slack members beside the owner it was created with, that
// they are all members of the team, and that the audit log records each
// channel membership once.
func (r *harness) checkPrivateMembers(ctx context.Context, v controller.RunView) membership {
	t := r.t
	t.Helper()
	want := r.expectedMembers(ctx, v.ID)
	if len(want.private) == 0 {
		t.Fatal("the fixture has no private channels; this test would prove nothing")
	}
	teams, err := r.store.Teams(ctx, v.ID)
	if err != nil {
		t.Fatal(err)
	}
	owner := teams[0].OwnerID
	team := r.fake.Teams()[0]
	inTeam := map[string]bool{}
	for _, m := range team.Members {
		inTeam[m.UserID] = true
		if m.UserID != owner && !want.team[m.UserID] {
			t.Errorf("%s is in the team but was not an eligible member of any migrated channel", m.UserID)
		}
	}
	for id := range want.team {
		if !inTeam[id] {
			t.Errorf("%s was a member of a migrated channel but is not in the team", id)
		}
	}

	byName := map[string]fakegraph.ChannelView{}
	for _, c := range r.fake.Channels(team.ID) {
		byName[c.DisplayName] = c
	}
	channelAdds, outsiders := 0, 0
	for name, members := range want.private {
		c, ok := byName[name]
		if !ok || c.MembershipType != "private" {
			t.Errorf("private channel %q: created as %q (found %v)", name, c.MembershipType, ok)
			continue
		}
		got := map[string]bool{}
		owners := 0
		for _, m := range c.Members {
			if got[m.UserID] {
				t.Errorf("private channel %q: %s is a member twice", name, m.UserID)
			}
			got[m.UserID] = true
			if m.Owner {
				owners++
				if m.UserID != owner {
					t.Errorf("private channel %q: %s is an owner; only the team's owner should be", name, m.UserID)
				}
			}
			if !inTeam[m.UserID] {
				t.Errorf("private channel %q: member %s is not in the team", name, m.UserID)
			}
			if m.UserID != owner && !members[m.UserID] {
				t.Errorf("private channel %q: %s was added but was not an eligible member of it in Slack", name, m.UserID)
			}
		}
		if owners != 1 || !got[owner] {
			t.Errorf("private channel %q has %d owners (the team's owner present: %v), want exactly the owner it was created with", name, owners, got[owner])
		}
		for id := range members {
			if !got[id] {
				t.Errorf("private channel %q: Slack member %s is missing", name, id)
			}
		}
		for id := range inTeam {
			if !got[id] {
				outsiders++
			}
		}

		// The audit log names the channel on both sides of each entry,
		// once per member.
		slackID := want.slackIDs[name]
		page, err := r.client.AuditLog(ctx, v.ID, store.LogFilter{Op: "channel_member.add", Contains: c.ID, Limit: 5000})
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, e := range page.Entries {
			if seen[e.TargetID] {
				t.Errorf("private channel %q: %s is recorded as added twice", name, e.TargetID)
			}
			seen[e.TargetID] = true
			user := strings.TrimPrefix(e.TargetID, c.ID+"/")
			if user == e.TargetID || !members[user] || !strings.HasPrefix(e.SourceID, slackID+"/") || e.Stream != "members:0" || e.Outcome != "ok" {
				t.Errorf("private channel %q: unexpected audit entry %+v", name, e)
			}
		}
		if len(seen) != len(members) {
			t.Errorf("private channel %q: the audit log records %d members added, want %d", name, len(seen), len(members))
		}
		channelAdds += len(members)
	}
	if outsiders == 0 {
		t.Error("every team member is in every private channel; the test cannot tell channel membership from team membership")
	}
	if got := r.logCount(ctx, v.ID, store.LogFilter{Op: "channel_member.add"}); got != channelAdds {
		t.Errorf("audit log has %d channel_member.add entries, want %d", got, channelAdds)
	}
	if got := r.logCount(ctx, v.ID, store.LogFilter{Op: "channel_member.skip"}); got != 0 {
		t.Errorf("%d channel members were skipped", got)
	}
	return want
}

// Private channels end to end: created with an owner, imported with their
// files from a SharePoint site of their own that takes a while to appear,
// and opened at finalize to exactly their Slack members, who join the team
// as well. Team members are added in bulk.
func TestPrivateChannelsEndToEnd(t *testing.T) {
	r := newHarnessWith(t, "medium", withPrivateChannels, fakegraph.WithPrivateChannelSiteLag(3))
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()

	run := r.start(ctx)
	v := r.await(ctx, run.ID, 3*time.Minute)
	w := r.expected()
	r.verify(v, w)

	teams, err := r.store.Teams(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	owner := teams[0].OwnerID
	team := r.fake.Teams()[0]
	private, withFiles := 0, 0
	for _, c := range r.fake.Channels(team.ID) {
		if c.MembershipType != "private" {
			continue
		}
		private++
		if len(w.byChannel[c.DisplayName]) == 0 {
			t.Errorf("private channel %q has no messages to import", c.DisplayName)
		}
		if len(w.files[c.DisplayName]) > 0 {
			withFiles++
		}
		// Until finalize, the channel is visible to its owner alone.
		if len(c.Members) != 1 || c.Members[0].UserID != owner || !c.Members[0].Owner {
			t.Errorf("private channel %q members before finalize = %+v, want the team's owner as its only owner", c.DisplayName, c.Members)
		}
	}
	if private == 0 || withFiles == 0 {
		t.Fatalf("%d private channels, %d with files; the fixture must have both", private, withFiles)
	}
	if got := r.fake.CountRequests("GET", "/v1.0/teams/*/channels/*/filesFolder"); got < 4*private {
		t.Errorf("%d filesFolder requests; each private channel's site should have taken four to appear", got)
	}

	if _, err := r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{}); err != nil {
		t.Fatal(err)
	}
	v = r.awaitFinalize(ctx, run.ID, 90*time.Second)
	r.checkFinalized(ctx, v)
	want := r.checkPrivateMembers(ctx, v)

	// Bulk, not one request per person.
	added := r.logCount(ctx, run.ID, store.LogFilter{Op: "member.add", Outcome: "ok"})
	if added != len(want.team) {
		t.Errorf("audit log records %d team members added, want %d", added, len(want.team))
	}
	bulk := r.fake.CountRequests("POST", "/v1.0/teams/*/members/add")
	if most := (added + 199) / 200; bulk == 0 || bulk > most {
		t.Errorf("%d members/add requests for %d members, want at most %d", bulk, added, most)
	}
	if single := r.fake.CountRequests("POST", "/v1.0/teams/*/members"); single != 0 {
		t.Errorf("%d single member requests; team members must be added in bulk", single)
	}
	t.Logf("%d team members in %d request(s); %d private channels", added, bulk, private)
}

// The same with workers dying and Graph losing the responses to the bulk
// add and to channel member requests: everyone still ends up in the team and
// their channels exactly once, in Graph and in the audit log.
func TestPrivateChannelMembersSurviveKillsAndFaults(t *testing.T) {
	if testing.Short() {
		t.Skip("chaos run skipped in -short mode")
	}
	r := newHarnessWith(t, "medium", withPrivateChannels, fakegraph.WithRandSeed(5))
	ctx := t.Context()
	kill := r.worker(ctx, 2*time.Second)
	run := r.start(ctx)
	v := r.await(ctx, run.ID, 3*time.Minute)
	r.verify(v, r.expected())

	faults := r.fake.Faults()
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/completeMigration", Action: fakegraph.ActionFailAfterCommit, Probability: 0.3})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/members/add", Action: fakegraph.ActionFailAfterCommit, Count: 2})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/members/add", Action: fakegraph.ActionStatus, Status: 503, Probability: 0.3})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/members", Action: fakegraph.ActionFailAfterCommit, Probability: 0.25})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/members", Action: fakegraph.ActionThrottle, RetryAfterSeconds: 1, Probability: 0.1})
	faults.MustAdd(fakegraph.Fault{Method: "GET", Path: "/v1.0/teams/*/channels/*/members", Action: fakegraph.ActionStatus, Status: 503, Probability: 0.1})

	if _, err := r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{}); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	done := make(chan struct{})
	killed := 0
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			case <-time.After(120 * time.Millisecond):
				kill()
				kill = r.worker(ctx, 2*time.Second)
				killed++
			}
		}
	}()
	v = r.awaitFinalize(ctx, run.ID, 4*time.Minute)
	close(stop)
	<-done
	kill()

	r.checkFinalized(ctx, v)
	want := r.checkPrivateMembers(ctx, v)
	if added := r.logCount(ctx, run.ID, store.LogFilter{Op: "member.add", Outcome: "ok"}); added != len(want.team) {
		t.Errorf("audit log records %d team members added, want %d", added, len(want.team))
	}
	// Each team member is recorded once.
	page, err := r.client.AuditLog(ctx, run.ID, store.LogFilter{Op: "member.add", Limit: 5000})
	if err != nil {
		t.Fatal(err)
	}
	var targets []string
	for _, e := range page.Entries {
		targets = append(targets, e.TargetID)
	}
	sort.Strings(targets)
	for i := 1; i < len(targets); i++ {
		if targets[i] == targets[i-1] {
			t.Errorf("team member %s is recorded as added twice", targets[i])
		}
	}
	recovered := r.logCount(ctx, run.ID, store.LogFilter{Contains: "added by an interrupted attempt"})
	t.Logf("workers killed: %d; members recorded after an interrupted attempt: %d; members/add requests: %d; channel member requests: %d",
		killed, recovered, r.fake.CountRequests("POST", "/v1.0/teams/*/members/add"), r.fake.CountRequests("POST", "/v1.0/teams/*/channels/*/members"))
	if recovered == 0 {
		t.Error("no member was recorded after an interrupted attempt; the recovery path was never exercised")
	}
}

// A person who cannot be added to the team (the account is gone, or
// Microsoft Teams refuses them) is skipped for their private channels too,
// and both are recorded; a failure that may pass stops the lane with
// everyone else added, and is tried again.
func TestPrivateChannelMemberWhoCannotJoinTheTeam(t *testing.T) {
	r := newHarnessWith(t, "medium", withPrivateChannels)
	r.maxAttempts = 2
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	run := r.start(ctx)
	v := r.await(ctx, run.ID, 3*time.Minute)
	r.verify(v, r.expected())

	// Pick someone who is in a private channel.
	want := r.expectedMembers(ctx, run.ID)
	var gone, blocked string
	for _, members := range want.private {
		ids := make([]string, 0, len(members))
		for id := range members {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		if len(ids) >= 2 && gone == "" {
			gone, blocked = ids[0], ids[1]
		}
	}
	if gone == "" {
		t.Fatal("no private channel with two eligible members in the fixture")
	}
	// A third person, in the team but not one of the two, for whom Graph
	// is merely busy.
	var busy string
	for id := range want.team {
		if id != gone && id != blocked && (busy == "" || id < busy) {
			busy = id
		}
	}
	r.fake.FailMemberAdd(gone, "NotFound", "The user does not exist.")
	r.fake.FailMemberAdd(blocked, "Forbidden", "The user's licence does not include Teams.")
	r.fake.FailMemberAdd(busy, "ServiceUnavailable", "Try again later.")

	if _, err := r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{Retain: true}); err != nil {
		t.Fatal(err)
	}
	v = r.awaitFinalize(ctx, run.ID, 90*time.Second)
	if v.Status != store.RunFailed {
		t.Fatalf("status = %s, want failed: one member's add failed for a reason that may pass", v.Status)
	}
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "member.skip", Contains: gone}); n != 1 {
		t.Errorf("the deleted account has %d member.skip entries, want 1", n)
	}
	// Graph's verdict on one person is recorded and does not hold the team
	// closed; it is what Graph said, for someone to act on.
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "member.skip", Contains: "licence does not include Teams"}); n != 1 {
		t.Errorf("the refused member has %d member.skip entries saying why, want 1", n)
	}
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "member.add", Outcome: "ok"}); n != len(want.team)-3 {
		t.Errorf("%d members recorded as added, want everyone but the three who were not (%d)", n, len(want.team)-3)
	}
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "channel_member.add"}); n != 0 {
		t.Errorf("%d channel members were added although the team's members were not all settled", n)
	}

	// Graph is no longer busy; finalizing continues and finishes, although
	// the refusal stands.
	r.fake.ClearMemberAddFailures()
	r.fake.FailMemberAdd(gone, "NotFound", "The user does not exist.")
	r.fake.FailMemberAdd(blocked, "Forbidden", "The user's licence does not include Teams.")
	if _, err := r.client.Retry(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	v = r.awaitFinalize(ctx, run.ID, 90*time.Second)
	if v.Status != store.RunFinalized {
		t.Fatalf("status = %s; problems %+v", v.Status, v.Progress.Problems)
	}
	team := r.fake.Teams()[0]
	for _, m := range team.Members {
		if m.UserID == gone {
			t.Error("the account that no longer exists is in the team")
		}
	}
	skips := r.logCount(ctx, run.ID, store.LogFilter{Op: "channel_member.skip", Contains: gone})
	channels := 0
	for name, members := range want.private {
		if !members[gone] {
			continue
		}
		channels++
		for _, c := range r.fake.Channels(team.ID) {
			if c.DisplayName != name {
				continue
			}
			for _, m := range c.Members {
				if m.UserID == gone {
					t.Errorf("the account that no longer exists is in private channel %q", name)
				}
			}
		}
	}
	if skips != channels || channels == 0 {
		t.Errorf("%d channel_member.skip entries for %d private channels the person was in", skips, channels)
	}
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "member.skip"}); n != 2 {
		t.Errorf("%d member.skip entries after the retry, want still 2", n)
	}
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "member.add", Contains: busy}); n != 1 {
		t.Errorf("the member Graph was too busy for at first has %d member.add entries, want 1", n)
	}
	for _, m := range team.Members {
		if m.UserID == blocked {
			t.Error("the member Microsoft Teams refused is in the team")
		}
	}
	// Refused for the team, so skipped for their private channels, and
	// recorded there too.
	if n := r.logCount(ctx, run.ID, store.LogFilter{Op: "channel_member.skip", Contains: blocked}); n == 0 {
		t.Error("the refused member has no channel_member.skip entry")
	}
	if err := r.store.VerifyLog(ctx, run.ID); err != nil {
		t.Errorf("audit log: %v", err)
	}
}

// The team's owner is a member of a private channel in Slack, but not (any
// more) of the Teams channel. Being the owner is no reason to leave them
// out of it.
func TestPrivateChannelOwnerWhoIsNotInTheChannel(t *testing.T) {
	// The owner is someone from the export who is in a private channel.
	probe := newHarnessWith(t, "medium", withPrivateChannels)
	ex := probe.expectedOwnerCandidate()
	r := newHarnessWith(t, "medium", func(m *config.Config) {
		withPrivateChannels(m)
		m.Target.Teams[0].Owner = ex.upn
	})
	ctx := t.Context()
	kill := r.worker(ctx, 5*time.Second)
	defer kill()
	run := r.start(ctx)
	v := r.await(ctx, run.ID, 3*time.Minute)
	r.verify(v, r.expected())

	team := r.fake.Teams()[0]
	var channelID string
	for _, c := range r.fake.Channels(team.ID) {
		if c.DisplayName == ex.channel {
			channelID = c.ID
		}
	}
	// Someone takes the owner out of the channel before finalize.
	if channelID == "" || !r.fake.RemoveChannelMember(team.ID, channelID, ex.entraID) {
		t.Fatalf("the owner was not a member of private channel %q to begin with", ex.channel)
	}
	if _, err := r.client.Finalize(ctx, run.ID, controller.FinalizeRequest{Retain: true}); err != nil {
		t.Fatal(err)
	}
	v = r.awaitFinalize(ctx, run.ID, 2*time.Minute)
	if v.Status != store.RunFinalized {
		t.Fatalf("status = %s; problems %+v", v.Status, v.Progress.Problems)
	}
	in := false
	for _, c := range r.fake.Channels(team.ID) {
		if c.ID != channelID {
			continue
		}
		for _, m := range c.Members {
			in = in || m.UserID == ex.entraID
		}
	}
	if !in {
		t.Errorf("the team's owner, a member of #%s in Slack, was left out of the Teams channel", ex.channel)
	}
	owners := 0
	for _, m := range r.fake.Teams()[0].Members {
		if m.Owner {
			owners++
		}
	}
	if owners != 1 {
		t.Errorf("the team has %d owners", owners)
	}
}

type ownerCandidate struct{ upn, entraID, channel string }

// expectedOwnerCandidate picks a person of the export who has an account
// and is an eligible member of a private channel.
func (r *harness) expectedOwnerCandidate() ownerCandidate {
	r.t.Helper()
	ex, err := export.Open(r.export)
	if err != nil {
		r.t.Fatal(err)
	}
	defer ex.Close()
	users, err := ex.Users()
	if err != nil {
		r.t.Fatal(err)
	}
	chans, err := ex.Channels()
	if err != nil {
		r.t.Fatal(err)
	}
	entra := entraIDs("medium")
	upn := map[string]string{}
	for _, u := range devstack.Seed("medium").Users {
		upn[u.ID] = u.UserPrincipalName
	}
	eligible := map[string]bool{}
	for _, u := range users {
		eligible[u.ID] = !u.Deleted && !u.IsBot && !u.IsAppUser && !u.IsStranger && !u.IsGuest() && entra[u.ID] != ""
	}
	for _, ch := range chans {
		if ch.Kind != slack.KindPrivate {
			continue
		}
		for _, id := range ch.Members {
			if eligible[id] {
				return ownerCandidate{upn: upn[entra[id]], entraID: entra[id], channel: ch.Name}
			}
		}
	}
	r.t.Fatal("no eligible member of a private channel in the fixture")
	return ownerCandidate{}
}

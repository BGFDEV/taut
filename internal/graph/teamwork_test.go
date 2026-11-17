package graph_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/fakegraph"
	"github.com/bgfdev/taut/internal/graph"
)

const (
	adaID      = "a0000000-0000-4000-8000-000000000001"
	graceID    = "a0000000-0000-4000-8000-000000000002"
	engTeam    = "b0000000-0000-4000-8000-000000000001"
	engGen     = "19:general-eng@thread.tacv2"
	engRel     = "19:releases-eng@thread.tacv2"
	bodyCanary = "CUSTOMER-BODY-CANARY-7731"
)

func ts(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

func msgAt(when string) graph.ImportMessage {
	return graph.ImportMessage{
		CreatedAt: ts(when), From: graph.Identity{UserID: adaID, DisplayName: "Ada Lovelace"},
		HTML: "<p>" + bodyCanary + " " + when + "</p>",
	}
}

// clientFor builds another client against the same fake, optionally for a
// different app and with modified options.
func (e *env) clientFor(clientID, secret string, mod func(*graph.Options)) *graph.Client {
	e.t.Helper()
	tokens, err := graph.NewTokenSource(graph.CredentialsOptions{
		TenantID: fakegraph.DefaultTenantID, ClientID: clientID, Credential: mustSecret(e.t, secret),
		Authority: e.http.URL, GraphBase: e.http.URL, HTTPClient: e.http.Client(), Now: e.clock.Now,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	opts := graph.Options{
		BaseURL: e.http.URL, HTTPClient: e.http.Client(), TokenSource: tokens,
		Sleep: e.sleep.Sleep, Now: e.clock.Now, Jitter: func() float64 { return 0.5 },
	}
	if mod != nil {
		mod(&opts)
	}
	c, err := graph.New(opts)
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

// migrating returns an env whose seeded Engineering/General channel is in
// migration mode with its creation time moved back to 2015.
func migrating(t *testing.T, opts ...envOpt) (*env, graph.ImportTarget) {
	t.Helper()
	e := newEnv(t, opts...)
	back := ts("2015-01-01T00:00:00Z")
	if err := e.client.StartMigration(context.Background(), engTeam, engGen, &back); err != nil {
		t.Fatalf("StartMigration: %v", err)
	}
	return e, graph.ImportTarget{TeamID: engTeam, ChannelID: engGen}
}

func noBody(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		assertNoSecrets(t, err, bodyCanary)
	}
}

func TestWritePathHappyPath(t *testing.T) {
	e := newEnv(t, withServer(fakegraph.WithTeamProvisioningPolls(2), fakegraph.WithPageSize(2)))
	ctx := context.Background()
	c := e.client

	// A team, with provisioning that takes three status checks.
	teamID, err := c.CreateTeam(ctx, graph.TeamSpec{DisplayName: "Support's Dashboard", Description: "from Slack", OwnerID: adaID})
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if waits := e.sleep.Waits(); len(waits) != 2 || waits[0] != time.Second || waits[1] != 2*time.Second {
		t.Errorf("provisioning waits = %v", waits)
	}
	if got := e.fake.CountRequests("GET", "/v1.0/teams/*/operations/*"); got != 3 {
		t.Errorf("operation polls = %d, want 3", got)
	}
	team, err := c.GetTeam(ctx, teamID)
	if err != nil || team.DisplayName != "Support's Dashboard" || team.Visibility != "private" || team.Description != "from Slack" {
		t.Fatalf("GetTeam = %+v, %v", team, err)
	}
	// Duplicate names are allowed, and FindTeams sees both (quote escaped).
	second, err := c.CreateTeam(ctx, graph.TeamSpec{DisplayName: "Support's Dashboard", OwnerID: adaID, Visibility: "public"})
	if err != nil || second == teamID {
		t.Fatalf("second CreateTeam = %q, %v", second, err)
	}
	found, err := c.FindTeams(ctx, "support's dashboard")
	if err != nil || len(found) != 2 || found[0].ID != teamID || found[1].ID != second {
		t.Fatalf("FindTeams = %+v, %v", found, err)
	}
	if none, err := c.FindTeams(ctx, "Support"); err != nil || len(none) != 0 {
		t.Errorf("FindTeams(prefix) = %+v, %v", none, err)
	}
	views := e.fake.Teams()
	if v := views[2]; v.ID != teamID || v.Provisioning != "succeeded" || v.CreatedBy != fakegraph.DefaultClientID ||
		len(v.Members) != 1 || v.Members[0].UserID != adaID || !v.Members[0].Owner {
		t.Errorf("team view = %+v", v)
	}

	// The new team has a General channel; more are created.
	general, ok, err := c.FindChannel(ctx, teamID, "general")
	if err != nil || !ok || general.DisplayName != "General" || general.MembershipType != "standard" || general.MigrationMode != "" {
		t.Fatalf("FindChannel(general) = %+v, %v, %v", general, ok, err)
	}
	if _, ok, err := c.FindChannel(ctx, teamID, "nope"); ok || err != nil {
		t.Errorf("FindChannel(nope) = %v, %v", ok, err)
	}
	ch, err := c.CreateChannel(ctx, teamID, graph.ChannelSpec{DisplayName: "announcements", Description: "news"})
	if err != nil || ch.ID == "" || ch.MembershipType != "standard" || !ch.CreatedDateTime.Equal(e.clock.Now()) {
		t.Fatalf("CreateChannel = %+v, %v", ch, err)
	}
	// A private channel is created with one owner, who must be in the team.
	wantFail(t, second2(c.CreateChannel(ctx, teamID, graph.ChannelSpec{DisplayName: "leads", MembershipType: "private"})), "graph.bad_request")
	if err := second2(c.CreateChannel(ctx, teamID, graph.ChannelSpec{DisplayName: "leads", MembershipType: "private", OwnerID: graceID})); !graph.IsNotTeamMember(err) {
		t.Fatalf("private channel owned by someone outside the team: %v", err)
	}
	priv, err := c.CreateChannel(ctx, teamID, graph.ChannelSpec{DisplayName: "leads", MembershipType: "private", OwnerID: adaID})
	if err != nil || priv.MembershipType != "private" {
		t.Fatalf("CreateChannel(private) = %+v, %v", priv, err)
	}
	if got := e.fake.Channels(teamID); len(got) != 3 || got[1].DisplayName != "announcements" || got[1].TeamID != teamID {
		t.Errorf("channel views = %+v", got)
	}

	// Migration mode with a backdated creation time.
	created := ch.CreatedDateTime
	back := ts("2018-01-01T00:00:00Z")
	if err := c.StartMigration(ctx, teamID, ch.ID, &back); err != nil {
		t.Fatalf("StartMigration: %v", err)
	}
	got, err := c.GetChannel(ctx, teamID, ch.ID)
	if err != nil || got.MigrationMode != graph.MigrationInProgress || !got.CreatedDateTime.Equal(back) ||
		got.OriginalCreatedDateTime == nil || !got.OriginalCreatedDateTime.Equal(created) {
		t.Fatalf("GetChannel during migration = %+v, %v", got, err)
	}

	// Roots and replies, deliberately not in chronological order.
	target := graph.ImportTarget{TeamID: teamID, ChannelID: ch.ID}
	rootB, err := c.ImportMessage(ctx, target, msgAt("2019-03-01T10:00:00.250Z"))
	if err != nil {
		t.Fatalf("ImportMessage: %v", err)
	}
	if rootB.ID != "1551434400250" || rootB.ReplyToID != "" || !rootB.CreatedAt.Equal(ts("2019-03-01T10:00:00.250Z")) {
		t.Errorf("imported root = %+v", rootB)
	}
	withMention := msgAt("2019-01-01T08:00:00Z")
	withMention.HTML = `<p>hi <at id="0">Grace Hopper</at> ` + bodyCanary + `</p>`
	withMention.Mentions = []graph.Mention{{ID: 0, Text: "Grace Hopper", UserID: graceID}}
	rootA, err := c.ImportMessage(ctx, target, withMention)
	if err != nil {
		t.Fatalf("ImportMessage with mention: %v", err)
	}
	replyTarget := target
	replyTarget.ReplyToID = rootB.ID
	r2, err := c.ImportMessage(ctx, replyTarget, msgAt("2019-03-02T00:00:00Z"))
	if err != nil || r2.ReplyToID != rootB.ID {
		t.Fatalf("reply = %+v, %v", r2, err)
	}
	r1, err := c.ImportMessage(ctx, replyTarget, msgAt("2019-03-01T10:00:01Z"))
	if err != nil {
		t.Fatalf("second reply: %v", err)
	}
	rootC, err := c.ImportMessage(ctx, target, msgAt("2019-02-01T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}

	// The fake holds exactly these, in timestamp order.
	stored := e.fake.Messages(teamID, ch.ID)
	var order []string
	for _, m := range stored {
		order = append(order, m.ID)
	}
	if want := []string{rootA.ID, rootC.ID, rootB.ID, r1.ID, r2.ID}; fmt.Sprint(order) != fmt.Sprint(want) {
		t.Fatalf("stored order = %v, want %v", order, want)
	}
	if m := stored[0]; m.FromUserID != adaID || m.FromDisplayName != "Ada Lovelace" || m.ImportedBy != fakegraph.DefaultClientID ||
		m.BodyContent != withMention.HTML || len(m.Mentions) != 1 || m.Mentions[0].UserID != graceID || m.Mentions[0].Text != "Grace Hopper" {
		t.Errorf("stored message = %+v", m)
	}
	if st := e.fake.Stats(); st.ImportsAccepted != 5 || st.Conflicts != 0 {
		t.Errorf("stats = %+v", st)
	}

	// Reads.
	m, err := c.GetMessage(ctx, teamID, ch.ID, rootA.ID)
	if err != nil || m.FromUserID != adaID || m.Content != withMention.HTML || m.ContentType != "html" || !m.CreatedAt.Equal(withMention.CreatedAt) {
		t.Errorf("GetMessage = %+v, %v", m, err)
	}
	rp, err := c.GetReply(ctx, teamID, ch.ID, rootB.ID, r1.ID)
	if err != nil || rp.ReplyToID != rootB.ID || rp.ID != r1.ID {
		t.Errorf("GetReply = %+v, %v", rp, err)
	}
	for name, err := range map[string]error{
		"missing message":       second2(c.GetMessage(ctx, teamID, ch.ID, "1")),
		"reply as message":      second2(c.GetMessage(ctx, teamID, ch.ID, r1.ID)),
		"reply of wrong parent": second2(c.GetReply(ctx, teamID, ch.ID, rootA.ID, r1.ID)),
		"missing channel":       second2(c.GetChannel(ctx, teamID, "19:nope@thread.tacv2")),
	} {
		if !graph.IsNotFound(err) {
			t.Errorf("%s: want not found, got %v", name, err)
		}
	}
	roots, err := collect(c.Messages(ctx, teamID, ch.ID, 0)) // server page size 2: two pages
	if err != nil || len(roots) != 3 || roots[0].ID != rootB.ID || roots[1].ID != rootC.ID || roots[2].ID != rootA.ID {
		t.Errorf("Messages = %+v, %v", roots, err)
	}
	if got := e.fake.CountRequests("GET", "/v1.0/teams/*/channels/*/messages"); got != 2 {
		t.Errorf("message pages = %d, want 2", got)
	}
	replies, err := collect(c.Replies(ctx, teamID, ch.ID, rootB.ID, 1))
	if err != nil || len(replies) != 2 || replies[0].ID != r2.ID || replies[1].ID != r1.ID {
		t.Errorf("Replies = %+v, %v", replies, err)
	}

	// Completing ends imports; migration mode can be re-entered.
	if err := c.CompleteChannelMigration(ctx, teamID, ch.ID); err != nil {
		t.Fatalf("CompleteChannelMigration: %v", err)
	}
	if got, err := c.GetChannel(ctx, teamID, ch.ID); err != nil || got.MigrationMode != graph.MigrationCompleted {
		t.Fatalf("after completion = %+v, %v", got, err)
	}
	_, err = c.ImportMessage(ctx, target, msgAt("2019-05-01T00:00:00Z"))
	wantFail(t, err, "graph.not_in_migration")
	if !graph.IsNotInMigration(err) {
		t.Errorf("IsNotInMigration false for %v", err)
	}
	noBody(t, err)
	err = c.CompleteChannelMigration(ctx, teamID, ch.ID)
	wantFail(t, err, "graph.not_in_migration")
	if !graph.IsNotInMigration(err) {
		t.Error("second completion should be IsNotInMigration")
	}
	if err := c.StartMigration(ctx, teamID, ch.ID, nil); err != nil {
		t.Fatalf("re-entering migration: %v", err)
	}
	if _, err := c.ImportMessage(ctx, target, msgAt("2019-05-01T00:00:00Z")); err != nil {
		t.Fatalf("import after re-entry: %v", err)
	}
	if got, _ := c.GetChannel(ctx, teamID, ch.ID); !got.CreatedDateTime.Equal(back) || !got.OriginalCreatedDateTime.Equal(created) {
		t.Errorf("re-entry without a time changed creation times: %+v", got)
	}

	// Members.
	if err := c.AddTeamMember(ctx, teamID, graceID, false); err != nil {
		t.Fatalf("AddTeamMember: %v", err)
	}
	if err := c.AddTeamMember(ctx, teamID, graceID, true); err != nil {
		t.Fatalf("AddTeamMember (promote): %v", err)
	}
	if mem := e.fake.Teams()[2].Members; len(mem) != 2 || mem[1].UserID != graceID || !mem[1].Owner {
		t.Errorf("members = %+v", mem)
	}
	wantFail(t, c.AddTeamMember(ctx, teamID, "no-such-user", false), "graph.not_found")
}

func second2[T any](_ T, err error) error { return err }

func TestCreateTeamFailures(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, withServer(fakegraph.WithTeamProvisioningFailure("Doomed"), fakegraph.WithTeamProvisioningDelay(time.Hour)))

	// Provisioning that outlasts the wait budget.
	impatient := e.clientFor(fakegraph.DefaultClientID, fakegraph.DefaultSecret, func(o *graph.Options) {
		o.Provisioning = graph.ProvisioningPolicy{PollInterval: time.Second, MaxWait: 10 * time.Second}
	})
	id, err := impatient.CreateTeam(ctx, graph.TeamSpec{DisplayName: "Slow", OwnerID: adaID})
	wantFail(t, err, "graph.team_provisioning_timeout")
	if id == "" {
		t.Error("the team ID should be returned with a provisioning timeout")
	}
	if _, err := e.client.GetTeam(ctx, id); !graph.IsNotFound(err) {
		t.Errorf("team visible before provisioning finished: %v", err)
	}
	if found, err := e.client.FindTeams(ctx, "Slow"); err != nil || len(found) != 0 {
		t.Errorf("FindTeams before provisioning = %v, %v", found, err)
	}
	_, err = e.client.CreateChannel(ctx, id, graph.ChannelSpec{DisplayName: "x"})
	wantFail(t, err, "graph.not_found")
	e.clock.Advance(time.Hour)
	if team, err := e.client.GetTeam(ctx, id); err != nil || team.DisplayName != "Slow" {
		t.Errorf("team after the delay = %+v, %v", team, err)
	}

	// Provisioning that fails (the delay has to pass first: 1+2+4+8+10… s).
	patient := e.clientFor(fakegraph.DefaultClientID, fakegraph.DefaultSecret, func(o *graph.Options) {
		o.Provisioning = graph.ProvisioningPolicy{MaxWait: 2 * time.Hour}
	})
	id, err = patient.CreateTeam(ctx, graph.TeamSpec{DisplayName: "Doomed", OwnerID: adaID})
	wantFail(t, err, "graph.team_provisioning_failed")
	if _, err := e.client.GetTeam(ctx, id); id == "" || !graph.IsNotFound(err) {
		t.Errorf("failed team %q should stay invisible: %v", id, err)
	}
	if v := e.fake.Teams(); v[len(v)-1].Provisioning != "failed" {
		t.Errorf("team view = %+v", v[len(v)-1])
	}

	// Rejected outright.
	_, err = e.client.CreateTeam(ctx, graph.TeamSpec{DisplayName: "No owner", OwnerID: "ghost"})
	wantFail(t, err, "graph.bad_request")
	_, err = e.client.CreateTeam(ctx, graph.TeamSpec{DisplayName: " ", OwnerID: adaID})
	wantFail(t, err, "graph.bad_request")
	_, err = e.client.CreateTeam(ctx, graph.TeamSpec{DisplayName: "x", OwnerID: adaID, Visibility: "secret"})
	wantFail(t, err, "graph.bad_request")

	// A lost response is not retried: the team exists, the caller finds it.
	e.fake.Faults().MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams", Action: fakegraph.ActionFailAfterCommit, Count: 1})
	e2 := e.clientFor(fakegraph.DefaultClientID, fakegraph.DefaultSecret, nil)
	_, err = e2.CreateTeam(ctx, graph.TeamSpec{DisplayName: "Lost", OwnerID: adaID})
	wantFail(t, err, "graph.network")
	if got := e.fake.CountRequests("POST", "/v1.0/teams"); got != 5 {
		t.Errorf("POST /teams sent %d times in total, want 5 (no retry)", got)
	}
	e.clock.Advance(time.Hour)
	if found, err := e.client.FindTeams(ctx, "Lost"); err != nil || len(found) != 1 {
		t.Errorf("FindTeams after a lost response = %v, %v", found, err)
	}

	// Without Team.Create the error names the permission.
	limited := newEnv(t, withRoles("Team.ReadBasic.All"))
	_, err = limited.client.CreateTeam(ctx, graph.TeamSpec{DisplayName: "x", OwnerID: adaID})
	if fe := wantFail(t, err, "graph.forbidden"); !strings.Contains(fe.Next, "Team.Create") {
		t.Errorf("next = %q", fe.Next)
	}
}

func TestChannelNameRules(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	for _, name := range []string{
		"", strings.Repeat("x", 51), "a#b", "a~b", "a%b", "a&b", "a*b", "a{b", "a}b", "a+b", "a/b", `a\b`, "a:b", "a<b", "a>b",
		"a?b", "a|b", "a'b", `a"b`, "a,b", "a..b", "_x", ".x", "x.", "CON", "conin$", "CONOUT$", "prn", "Aux", "NUL",
		"com5", "LPT9", "Desktop.ini", "forms", "_vti_", "x_vti_y",
	} {
		_, err := e.client.CreateChannel(ctx, engTeam, graph.ChannelSpec{DisplayName: name})
		if err == nil || fail.From(err).Code != "graph.channel_name_invalid" {
			t.Errorf("CreateChannel(%q) = %v, want graph.channel_name_invalid", name, err)
		}
	}
	for _, name := range []string{strings.Repeat("é", 50), "a.b", "x_", "COM10", "formsy", "ünïcode ok", "a-b (c) [d] @ ! = ; $"} {
		if _, err := e.client.CreateChannel(ctx, engTeam, graph.ChannelSpec{DisplayName: name}); err != nil {
			t.Errorf("CreateChannel(%q): %v", name, err)
		}
	}
	_, err := e.client.CreateChannel(ctx, engTeam, graph.ChannelSpec{DisplayName: "RELEASES"})
	wantFail(t, err, "graph.channel_exists")
	var api *graph.APIError
	if !graph.IsChannelExists(err) || !errors.As(err, &api) || api.Status != 400 || api.Code != "BadRequest" {
		t.Errorf("channel exists error = %v (%+v)", err, api)
	}
	_, err = e.client.CreateChannel(ctx, engTeam, graph.ChannelSpec{DisplayName: "ok", MembershipType: "secret"})
	wantFail(t, err, "graph.bad_request")
	_, err = e.client.CreateChannel(ctx, "no-team", graph.ChannelSpec{DisplayName: "ok"})
	wantFail(t, err, "graph.not_found")
	if got := len(e.fake.Channels(engTeam)); got != 3+7 {
		t.Errorf("channels = %d, want 10", got)
	}

	limited := newEnv(t, withRoles("Channel.ReadBasic.All"))
	_, err = limited.client.CreateChannel(ctx, engTeam, graph.ChannelSpec{DisplayName: "ok"})
	if fe := wantFail(t, err, "graph.forbidden"); !strings.Contains(fe.Next, "Channel.Create") {
		t.Errorf("next = %q", fe.Next)
	}
}

func TestMigrationModeRules(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.client

	// The creation time must move backwards.
	same := fakegraph.SeedChannelCreated
	wantFail(t, c.StartMigration(ctx, engTeam, engGen, &same), "graph.bad_request")
	later := same.Add(time.Hour)
	wantFail(t, c.StartMigration(ctx, engTeam, engGen, &later), "graph.bad_request")
	if ch, _ := c.GetChannel(ctx, engTeam, engGen); ch.MigrationMode != "" || ch.OriginalCreatedDateTime != nil {
		t.Fatalf("rejected startMigration changed the channel: %+v", ch)
	}
	wantFail(t, c.CompleteChannelMigration(ctx, engTeam, engGen), "graph.not_in_migration")
	wantFail(t, c.StartMigration(ctx, engTeam, "19:nope@thread.tacv2", nil), "graph.not_found")

	// Without a time nothing about the creation time changes.
	if err := c.StartMigration(ctx, engTeam, engGen, nil); err != nil {
		t.Fatal(err)
	}
	if ch, _ := c.GetChannel(ctx, engTeam, engGen); ch.MigrationMode != graph.MigrationInProgress || !ch.CreatedDateTime.Equal(same) {
		t.Errorf("channel = %+v", ch)
	}
	// Already migrating, with or without a time.
	back := ts("2015-01-01T00:00:00Z")
	for _, when := range []*time.Time{nil, &back} {
		err := c.StartMigration(ctx, engTeam, engGen, when)
		wantFail(t, err, "graph.already_migrating")
		if !graph.IsAlreadyMigrating(err) || graph.IsNotInMigration(err) || graph.IsConflict(err) {
			t.Errorf("IsAlreadyMigrating misclassified %v", err)
		}
	}

	// A startMigration whose response is lost is retried and reports
	// "already migrating", which resuming callers treat as success.
	e.fake.Faults().MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/startMigration", Action: fakegraph.ActionFailAfterCommit, Count: 1})
	err := c.StartMigration(ctx, engTeam, engRel, &back)
	if !graph.IsAlreadyMigrating(err) {
		t.Fatalf("lost startMigration response: %v", err)
	}
	if ch, _ := c.GetChannel(ctx, engTeam, engRel); ch.MigrationMode != graph.MigrationInProgress || !ch.CreatedDateTime.Equal(back) {
		t.Errorf("channel after lost response = %+v", ch)
	}
	// Likewise for completeMigration, here with a 503 after the commit.
	e.fake.Faults().MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/completeMigration", Action: fakegraph.ActionFailAfterCommit, Status: 503, Count: 1})
	if err := c.CompleteChannelMigration(ctx, engTeam, engRel); !graph.IsNotInMigration(err) {
		t.Fatalf("lost completeMigration response: %v", err)
	}
	if ch, _ := c.GetChannel(ctx, engTeam, engRel); ch.MigrationMode != graph.MigrationCompleted {
		t.Errorf("channel = %+v", ch)
	}

	limited := newEnv(t, withRoles("Channel.ReadBasic.All", "Channel.Create"))
	err = limited.client.StartMigration(ctx, engTeam, engGen, nil)
	if fe := wantFail(t, err, "graph.forbidden"); !strings.Contains(fe.Next, "Teamwork.Migrate.All") {
		t.Errorf("next = %q", fe.Next)
	}
}

func TestImportRules(t *testing.T) {
	e, target := migrating(t, withServer(fakegraph.WithMaxMessageBytes(64*1024)))
	ctx := context.Background()
	c := e.client
	root, err := c.ImportMessage(ctx, target, msgAt("2019-01-01T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	reply, err := c.ImportMessage(ctx, graph.ImportTarget{TeamID: engTeam, ChannelID: engGen, ReplyToID: root.ID}, msgAt("2019-01-01T00:00:01Z"))
	if err != nil {
		t.Fatal(err)
	}

	mod := func(when string, f func(*graph.ImportMessage)) graph.ImportMessage {
		m := msgAt(when)
		f(&m)
		return m
	}
	for name, tc := range map[string]struct {
		target graph.ImportTarget
		msg    graph.ImportMessage
		code   string
	}{
		"unknown author":          {target, mod("2019-02-01T00:00:00Z", func(m *graph.ImportMessage) { m.From.UserID = "not-a-user" }), "graph.invalid_author"},
		"in the future":           {target, msgAt("2026-11-13T09:00:00.001Z"), "graph.bad_request"},
		"at channel creation":     {target, msgAt("2015-01-01T00:00:00Z"), "graph.bad_request"},
		"before channel creation": {target, msgAt("2014-12-31T23:59:59.999Z"), "graph.bad_request"},
		"too large":               {target, mod("2019-02-01T00:00:00Z", func(m *graph.ImportMessage) { m.HTML = bodyCanary + strings.Repeat("é", 32*1024) }), "graph.payload_too_large"},
		"empty body":              {target, mod("2019-02-01T00:00:00Z", func(m *graph.ImportMessage) { m.HTML = "" }), "graph.bad_request"},
		"at tag without mention":  {target, mod("2019-02-01T00:00:00Z", func(m *graph.ImportMessage) { m.HTML = bodyCanary + `<at id="3">x</at>` }), "graph.bad_request"},
		"mention without at tag": {target, mod("2019-02-01T00:00:00Z", func(m *graph.ImportMessage) {
			m.Mentions = []graph.Mention{{ID: 1, Text: "Grace", UserID: graceID}}
		}), "graph.bad_request"},
		"mention id mismatch": {target, mod("2019-02-01T00:00:00Z", func(m *graph.ImportMessage) {
			m.HTML = bodyCanary + `<at id="0">Grace</at>`
			m.Mentions = []graph.Mention{{ID: 1, Text: "Grace", UserID: graceID}}
		}), "graph.bad_request"},
		"parent missing":    {graph.ImportTarget{TeamID: engTeam, ChannelID: engGen, ReplyToID: "12345"}, msgAt("2019-02-01T00:00:00Z"), "graph.not_found"},
		"parent is a reply": {graph.ImportTarget{TeamID: engTeam, ChannelID: engGen, ReplyToID: reply.ID}, msgAt("2019-02-01T00:00:00Z"), "graph.bad_request"},
		"channel missing":   {graph.ImportTarget{TeamID: engTeam, ChannelID: "19:nope@thread.tacv2"}, msgAt("2019-02-01T00:00:00Z"), "graph.not_found"},
		"not migrating":     {graph.ImportTarget{TeamID: engTeam, ChannelID: engRel}, msgAt("2021-02-01T00:00:00Z"), "graph.not_in_migration"},
		"incomplete":        {target, graph.ImportMessage{HTML: bodyCanary}, "graph.bad_request"},
	} {
		_, err := c.ImportMessage(ctx, tc.target, tc.msg)
		if err == nil || fail.From(err).Code != tc.code {
			t.Errorf("%s: got %v, want %s", name, err, tc.code)
			continue
		}
		wantFail(t, err, tc.code)
		noBody(t, err)
	}
	// Exactly at the limit is fine; "now" is not the future.
	big := mod("2019-02-01T00:00:00Z", func(m *graph.ImportMessage) { m.HTML = strings.Repeat("é", 32*1024) })
	if _, err := c.ImportMessage(ctx, target, big); err != nil {
		t.Errorf("message at the size limit: %v", err)
	}
	if _, err := c.ImportMessage(ctx, target, msgAt("2026-11-13T09:00:00Z")); err != nil {
		t.Errorf("message created now: %v", err)
	}
	// None of the rejected messages was stored; rejections are not retried.
	if got := len(e.fake.Messages(engTeam, engGen)); got != 4 {
		t.Errorf("stored = %d, want 4", got)
	}
	if got := e.fake.CountRequests("POST", "/v1.0/teams/*/channels/*/messages"); got != 14 {
		t.Errorf("top-level message POSTs = %d, want 14 (one per call)", got)
	}
	if len(e.sleep.Waits()) != 0 {
		t.Errorf("4xx was retried: %v", e.sleep.Waits())
	}

	// With the lenient option an unknown author is accepted.
	lenient, target := migrating(t, withServer(fakegraph.WithLenientFrom()))
	ghost := mod("2019-02-01T00:00:00Z", func(m *graph.ImportMessage) { m.From = graph.Identity{UserID: "ghost", DisplayName: "Ghost"} })
	if _, err := lenient.client.ImportMessage(ctx, target, ghost); err != nil {
		t.Errorf("lenient from: %v", err)
	}
	if m := lenient.fake.Messages(engTeam, engGen); len(m) != 1 || m[0].FromUserID != "ghost" {
		t.Errorf("lenient stored = %+v", m)
	}

	// Without Teamwork.Migrate.All.
	limited := newEnv(t, withRoles("ChannelMessage.Read.All"))
	_, err = limited.client.ImportMessage(ctx, target, msgAt("2019-02-01T00:00:00Z"))
	if fe := wantFail(t, err, "graph.forbidden"); !strings.Contains(fe.Next, "Teamwork.Migrate.All") {
		t.Errorf("next = %q", fe.Next)
	}
	noBody(t, err)
	if _, err := collect(limited.client.Messages(ctx, engTeam, engGen, 10)); err != nil {
		t.Errorf("ChannelMessage.Read.All should allow reads: %v", err)
	}
	if _, err := collect(newEnv(t, withRoles("User.Read.All")).client.Messages(ctx, engTeam, engGen, 10)); fail.From(err).Code != "graph.forbidden" {
		t.Errorf("reads without a message role: %v", err)
	}
}

func TestImportConflicts(t *testing.T) {
	e, target := migrating(t)
	ctx := context.Background()
	c := e.client
	back := ts("2015-01-01T00:00:00Z")
	if err := c.StartMigration(ctx, engTeam, engRel, &back); err != nil {
		t.Fatal(err)
	}
	other := graph.ImportTarget{TeamID: engTeam, ChannelID: engRel}

	rootA, err := c.ImportMessage(ctx, target, msgAt("2019-01-01T00:00:00.100Z"))
	if err != nil {
		t.Fatal(err)
	}
	rootB, err := c.ImportMessage(ctx, target, msgAt("2019-01-01T00:00:00.101Z")) // 1 ms later: fine
	if err != nil {
		t.Fatalf("1 ms apart: %v", err)
	}
	inA := graph.ImportTarget{TeamID: engTeam, ChannelID: engGen, ReplyToID: rootA.ID}
	inB := graph.ImportTarget{TeamID: engTeam, ChannelID: engGen, ReplyToID: rootB.ID}
	if _, err := c.ImportMessage(ctx, inA, msgAt("2019-01-01T00:00:05Z")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ImportMessage(ctx, other, msgAt("2019-01-01T00:00:00.100Z")); err != nil {
		t.Fatalf("same millisecond in another channel: %v", err)
	}

	for name, tc := range map[string]struct {
		target graph.ImportTarget
		when   string
	}{
		"same ms as a root":                       {target, "2019-01-01T00:00:00.100Z"},
		"sub-millisecond digits truncated":        {target, "2019-01-01T00:00:00.100999Z"},
		"reply at a root's ms":                    {inB, "2019-01-01T00:00:00.101Z"},
		"root at a reply's ms":                    {target, "2019-01-01T00:00:05Z"},
		"reply at a reply's ms in another thread": {inB, "2019-01-01T00:00:05.0004Z"},
	} {
		_, err := c.ImportMessage(ctx, tc.target, msgAt(tc.when))
		if err == nil || fail.From(err).Code != "graph.conflict" {
			t.Errorf("%s: got %v, want graph.conflict", name, err)
			continue
		}
		wantFail(t, err, "graph.conflict")
		var api *graph.APIError
		if !graph.IsConflict(err) || !errors.As(err, &api) || api.Status != 409 || api.Code != "Conflict" || api.Attempts != 1 {
			t.Errorf("%s: conflict error = %v (%+v)", name, err, api)
		}
		noBody(t, err)
	}
	if st := e.fake.Stats(); st.Conflicts != 5 || st.ImportsAccepted != 4 {
		t.Errorf("stats = %+v", st)
	}
	if got := len(e.fake.Messages(engTeam, engGen)); got != 3 {
		t.Errorf("stored = %d, want 3", got)
	}
	if graph.IsConflict(nil) || graph.IsConflict(errors.New("x")) {
		t.Error("IsConflict false positives")
	}
}

func TestLostImportResponseIsRecoverable(t *testing.T) {
	const when = "2019-06-01T12:00:00.123Z"
	msgPath := "/v1.0/teams/*/channels/*/messages"
	for name, tc := range map[string]struct {
		fault  fakegraph.Fault
		opaque bool
	}{
		"dropped":        {fakegraph.Fault{Method: "POST", Path: msgPath, Action: fakegraph.ActionFailAfterCommit, Count: 1}, false},
		"503":            {fakegraph.Fault{Method: "POST", Path: msgPath, Action: fakegraph.ActionFailAfterCommit, Status: 503, Count: 1}, false},
		"dropped opaque": {fakegraph.Fault{Method: "POST", Path: msgPath, Action: fakegraph.ActionFailAfterCommit, Count: 1}, true},
	} {
		t.Run(name, func(t *testing.T) {
			var opts []envOpt
			if tc.opaque {
				opts = append(opts, withServer(fakegraph.WithOpaqueMessageIDs()))
			}
			e, target := migrating(t, opts...)
			ctx := context.Background()
			// An older and a newer neighbor, so the fallback has to search.
			for _, w := range []string{"2019-05-01T00:00:00Z", "2019-07-01T00:00:00Z"} {
				if _, err := e.client.ImportMessage(ctx, target, msgAt(w)); err != nil {
					t.Fatal(err)
				}
			}
			e.fake.Faults().MustAdd(tc.fault)
			_, err := e.client.ImportMessage(ctx, target, msgAt(when))
			wantFail(t, err, "graph.conflict")
			var api *graph.APIError
			if !graph.IsConflict(err) || !errors.As(err, &api) || api.Attempts != 2 {
				t.Fatalf("want a conflict on the second attempt, got %v", err)
			}
			noBody(t, err)
			if len(e.sleep.Waits()) != 1 {
				t.Errorf("waits = %v, want one backoff", e.sleep.Waits())
			}
			stored := e.fake.Messages(engTeam, engGen)
			if len(stored) != 3 || !stored[1].CreatedAt.Equal(ts(when)) || stored[1].BodyContent != msgAt(when).HTML {
				t.Fatalf("stored = %+v", stored)
			}

			epochID := graph.MessageIDForTime(ts(when))
			byID, err := e.client.GetMessage(ctx, engTeam, engGen, epochID)
			if tc.opaque {
				if !graph.IsNotFound(err) || stored[1].ID == epochID {
					t.Fatalf("opaque IDs: GetMessage(%s) = %+v, %v", epochID, byID, err)
				}
				// The listing fallback, by hand.
				var hit graph.ChatMessage
				for m, err := range e.client.Messages(ctx, engTeam, engGen, 1) {
					if err != nil {
						t.Fatal(err)
					}
					if m.CreatedAt.Equal(ts(when)) {
						hit = m
					}
				}
				if hit.ID != stored[1].ID {
					t.Fatalf("listing fallback found %+v", hit)
				}
			} else if err != nil || byID.ID != epochID || byID.Content != msgAt(when).HTML {
				t.Fatalf("GetMessage(%s) = %+v, %v", epochID, byID, err)
			}
			found, ok, err := e.client.FindMessageAt(ctx, target, ts(when))
			if err != nil || !ok || found.ID != stored[1].ID || found.Content != msgAt(when).HTML || found.FromUserID != adaID {
				t.Fatalf("FindMessageAt = %+v, %v, %v", found, ok, err)
			}
			if _, ok, err := e.client.FindMessageAt(ctx, target, ts("2019-06-01T12:00:00.124Z")); ok || err != nil {
				t.Errorf("FindMessageAt for an absent time = %v, %v", ok, err)
			}

			// The same for a reply.
			parent := graph.ImportTarget{TeamID: engTeam, ChannelID: engGen, ReplyToID: stored[0].ID}
			e.fake.Faults().MustAdd(fakegraph.Fault{Method: "POST", Path: msgPath + "/*/replies", Action: fakegraph.ActionFailAfterCommit, Count: 1})
			_, err = e.client.ImportMessage(ctx, parent, msgAt("2019-06-02T00:00:00Z"))
			if !graph.IsConflict(err) {
				t.Fatalf("reply: want conflict, got %v", err)
			}
			r, ok, err := e.client.FindMessageAt(ctx, parent, ts("2019-06-02T00:00:00Z"))
			if err != nil || !ok || r.ReplyToID != stored[0].ID {
				t.Fatalf("FindMessageAt(reply) = %+v, %v, %v", r, ok, err)
			}
			if got := len(e.fake.Messages(engTeam, engGen)); got != 4 {
				t.Errorf("stored = %d, want 4", got)
			}
		})
	}
}

func TestImportRetriesFailuresBeforeCommit(t *testing.T) {
	msgPath := "/v1.0/teams/*/channels/*/messages"
	for name, fault := range map[string]fakegraph.Fault{
		"drop": {Method: "POST", Path: msgPath, Action: fakegraph.ActionDrop, Count: 2},
		"500":  {Method: "POST", Path: msgPath, Action: fakegraph.ActionStatus, Status: 500, Count: 2},
		"502":  {Method: "POST", Path: msgPath, Action: fakegraph.ActionStatus, Status: 502, Count: 2},
		"503":  {Method: "POST", Path: msgPath, Action: fakegraph.ActionStatus, Status: 503, Count: 2},
		"429":  {Method: "POST", Path: msgPath, Action: fakegraph.ActionThrottle, RetryAfterSeconds: 3, Count: 2},
	} {
		e, target := migrating(t)
		e.fake.Faults().MustAdd(fault)
		m, err := e.client.ImportMessage(context.Background(), target, msgAt("2019-06-01T00:00:00Z"))
		if err != nil || m.ID == "" {
			t.Errorf("%s: %v", name, err)
		}
		if got := e.fake.Messages(engTeam, engGen); len(got) != 1 {
			t.Errorf("%s: stored %d messages, want 1", name, len(got))
		}
		if got := e.fake.CountRequests("POST", msgPath); got != 3 {
			t.Errorf("%s: %d POSTs, want 3", name, got)
		}
	}

	// A persistent outage ends in a retryable error, with nothing stored.
	e, target := migrating(t, withRetry(graph.RetryPolicy{MaxAttempts: 3}))
	e.fake.Faults().MustAdd(fakegraph.Fault{Method: "POST", Path: msgPath, Action: fakegraph.ActionDrop})
	_, err := e.client.ImportMessage(context.Background(), target, msgAt("2019-06-01T00:00:00Z"))
	wantFail(t, err, "graph.network")
	noBody(t, err)
	if got := e.fake.Messages(engTeam, engGen); len(got) != 0 {
		t.Errorf("stored %d messages during an outage", len(got))
	}
	// Other POSTs still are not repeated after an ambiguous failure.
	e.fake.Faults().Clear()
	e.fake.Faults().MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels", Action: fakegraph.ActionStatus, Status: 503})
	_, err = e.client.CreateChannel(context.Background(), engTeam, graph.ChannelSpec{DisplayName: "once"})
	wantFail(t, err, "graph.unavailable")
	if got := e.fake.CountRequests("POST", "/v1.0/teams/*/channels"); got != 1 {
		t.Errorf("CreateChannel sent %d times, want 1", got)
	}
}

func TestPerChannelThrottling(t *testing.T) {
	e, target := migrating(t, withServer(fakegraph.WithChannelRateLimit(1, 2)))
	ctx := context.Background()
	const n = 8
	for i := range n {
		if _, err := e.client.ImportMessage(ctx, target, msgAt(fmt.Sprintf("2019-06-01T00:00:%02dZ", i))); err != nil {
			t.Fatalf("message %d: %v", i, err)
		}
	}
	st := e.fake.Stats()
	if st.Throttles != n-2 || st.ImportsAccepted != n {
		t.Errorf("stats = %+v, want %d throttles and %d imports", st, n-2, n)
	}
	waits := e.sleep.Waits()
	if len(waits) != n-2 {
		t.Fatalf("waits = %v", waits)
	}
	for _, w := range waits {
		if w < time.Second || w > 2*time.Second {
			t.Errorf("wait %v does not honor Retry-After: 1", w)
		}
	}
	stored := e.fake.Messages(engTeam, engGen)
	if len(stored) != n {
		t.Fatalf("stored = %d, want %d", len(stored), n)
	}
	for i, m := range stored {
		if !m.CreatedAt.Equal(ts(fmt.Sprintf("2019-06-01T00:00:%02dZ", i))) {
			t.Errorf("message %d = %v", i, m.CreatedAt)
		}
	}
	// The bucket is per channel: another channel is unaffected, and reads
	// and other writes do not draw from it.
	back := ts("2015-01-01T00:00:00Z")
	if err := e.client.StartMigration(ctx, engTeam, engRel, &back); err != nil {
		t.Fatal(err)
	}
	before := len(e.sleep.Waits())
	for i := range 2 {
		if _, err := e.client.ImportMessage(ctx, graph.ImportTarget{TeamID: engTeam, ChannelID: engRel}, msgAt(fmt.Sprintf("2019-06-01T00:00:%02dZ", i))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := collect(e.client.Messages(ctx, engTeam, engGen, 50)); err != nil || len(e.sleep.Waits()) != before {
		t.Errorf("another channel or a read was throttled: %v, waits %v", err, e.sleep.Waits()[before:])
	}
}

func TestMigrationSessionOwnership(t *testing.T) {
	seed := fakegraph.DefaultSeed()
	seed.Apps = append(seed.Apps, fakegraph.App{ClientID: "second-app", Secrets: []string{"second-secret"}, Roles: fakegraph.DefaultRoles()})
	e, target := migrating(t, withSeed(seed))
	ctx := context.Background()
	intruder := e.clientFor("second-app", "second-secret", nil)

	_, err := intruder.ImportMessage(ctx, target, msgAt("2019-06-01T00:00:00Z"))
	fe := wantFail(t, err, "graph.migration_not_owner")
	var api *graph.APIError
	if !errors.As(err, &api) || api.Status != 403 || fe.Exit != fail.ExitPermission {
		t.Errorf("import by another app = %v", err)
	}
	noBody(t, err)
	wantFail(t, intruder.CompleteChannelMigration(ctx, engTeam, engGen), "graph.migration_not_owner")
	if err := intruder.StartMigration(ctx, engTeam, engGen, nil); !graph.IsAlreadyMigrating(err) {
		t.Errorf("startMigration by another app = %v", err)
	}
	if ch := e.fake.Channels(engTeam)[0]; ch.MigrationMode != "inProgress" || ch.MigrationOwner != fakegraph.DefaultClientID || ch.MessageCount != 0 {
		t.Errorf("channel = %+v", ch)
	}
	// The owner is unaffected; after completion the other app may start its own session.
	if _, err := e.client.ImportMessage(ctx, target, msgAt("2019-06-01T00:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if err := e.client.CompleteChannelMigration(ctx, engTeam, engGen); err != nil {
		t.Fatal(err)
	}
	if err := intruder.StartMigration(ctx, engTeam, engGen, nil); err != nil {
		t.Fatal(err)
	}
	m, err := intruder.ImportMessage(ctx, target, msgAt("2019-06-02T00:00:00Z"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.client.ImportMessage(ctx, target, msgAt("2019-06-03T00:00:00Z"))
	wantFail(t, err, "graph.migration_not_owner")
	if got := e.fake.Messages(engTeam, engGen); len(got) != 2 || got[1].ID != m.ID || got[1].ImportedBy != "second-app" {
		t.Errorf("stored = %+v", got)
	}
}

func TestConcurrentImports(t *testing.T) {
	seed := fakegraph.DefaultSeed()
	var channels []string
	for i := range 4 {
		id := fmt.Sprintf("19:lane-%d@thread.tacv2", i)
		channels = append(channels, id)
		seed.Teams[0].Channels = append(seed.Teams[0].Channels, fakegraph.Channel{ID: id, DisplayName: fmt.Sprintf("lane-%d", i)})
	}
	e := newEnv(t, withSeed(seed))
	ctx := context.Background()
	back := ts("2015-01-01T00:00:00Z")
	for _, id := range channels {
		if err := e.client.StartMigration(ctx, engTeam, id, &back); err != nil {
			t.Fatal(err)
		}
	}
	base := ts("2019-01-01T00:00:00Z")

	// Distinct timestamps: 8 workers, 4 channels, 50 messages each.
	const workers, perWorker = 8, 50
	var wg sync.WaitGroup
	errs := make(chan error, workers*perWorker)
	for w := range workers {
		wg.Go(func() {
			target := graph.ImportTarget{TeamID: engTeam, ChannelID: channels[w%4]}
			for i := range perWorker {
				m := graph.ImportMessage{
					CreatedAt: base.Add(time.Duration(w*1000+i) * time.Millisecond),
					From:      graph.Identity{UserID: adaID, DisplayName: "Ada"}, HTML: fmt.Sprintf("w%d-%d", w, i),
				}
				if _, err := e.client.ImportMessage(ctx, target, m); err != nil {
					errs <- err
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent import: %v", err)
	}
	for _, id := range channels {
		stored := e.fake.Messages(engTeam, id)
		if len(stored) != 2*perWorker {
			t.Errorf("%s: %d messages, want %d", id, len(stored), 2*perWorker)
		}
		seen := map[string]bool{}
		for i, m := range stored {
			if seen[m.BodyContent] || (i > 0 && !m.CreatedAt.After(stored[i-1].CreatedAt)) {
				t.Errorf("%s: duplicate or out-of-order message at %d", id, i)
			}
			seen[m.BodyContent] = true
		}
	}
	if st := e.fake.Stats(); st.ImportsAccepted != workers*perWorker || st.Conflicts != 0 {
		t.Errorf("stats = %+v", st)
	}

	// The same timestamp: exactly one winner.
	racer := graph.ImportTarget{TeamID: engTeam, ChannelID: engGen}
	if err := e.client.StartMigration(ctx, engTeam, engGen, &back); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var created, conflicts int
	for w := range workers {
		wg.Go(func() {
			m := msgAt("2019-01-01T00:00:00.500Z")
			m.HTML = fmt.Sprintf("racer-%d", w)
			_, err := e.client.ImportMessage(ctx, racer, m)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				created++
			case graph.IsConflict(err):
				conflicts++
			default:
				t.Errorf("racer %d: %v", w, err)
			}
		})
	}
	wg.Wait()
	if created != 1 || conflicts != workers-1 || len(e.fake.Messages(engTeam, engGen)) != 1 {
		t.Errorf("race: %d created, %d conflicts, %d stored", created, conflicts, len(e.fake.Messages(engTeam, engGen)))
	}
	if st := e.fake.Stats(); st.Conflicts != workers-1 {
		t.Errorf("stats = %+v", st)
	}
}

func TestWirePrecisionAndFilterEscaping(t *testing.T) {
	if got := graph.MessageTime(time.Date(2019, 2, 4, 20, 58, 15, 511_999_999, time.FixedZone("CET", 3600))); got != "2019-02-04T19:58:15.511Z" {
		t.Errorf("MessageTime = %q", got)
	}
	if got := graph.MessageTime(time.Date(2019, 2, 4, 19, 58, 15, 0, time.UTC)); got != "2019-02-04T19:58:15.000Z" {
		t.Errorf("MessageTime = %q", got)
	}
	if got := graph.MessageIDForTime(ts("2019-02-04T19:58:15.511Z")); got != "1549310295511" {
		t.Errorf("MessageIDForTime = %q", got)
	}
	if got := graph.FilterEq("displayName", "O'Brien's 'team'"); got != "displayName eq 'O''Brien''s ''team'''" {
		t.Errorf("FilterEq = %q", got)
	}
	// A local-zone, sub-millisecond timestamp arrives in UTC milliseconds.
	e, target := migrating(t)
	m := msgAt("2019-02-04T19:58:15Z")
	m.CreatedAt = time.Date(2019, 2, 4, 20, 58, 15, 511_700_000, time.FixedZone("CET", 3600))
	got, err := e.client.ImportMessage(context.Background(), target, m)
	if err != nil || got.ID != "1549310295511" || !e.fake.Messages(engTeam, engGen)[0].CreatedAt.Equal(ts("2019-02-04T19:58:15.511Z")) {
		t.Errorf("import = %+v, %v", got, err)
	}
}

// TestBulkMemberAdd covers POST /teams/{id}/members/add: every user in one
// request, 200 when all were added, 207 with per-user errors otherwise.
func TestBulkMemberAdd(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.client
	teamID, err := c.CreateTeam(ctx, graph.TeamSpec{DisplayName: "Bulk", OwnerID: adaID})
	if err != nil {
		t.Fatal(err)
	}
	const alanID, katherineID = "a0000000-0000-4000-8000-000000000003", "a0000000-0000-4000-8000-000000000004"

	// Nothing to add costs no request.
	if res, err := c.AddTeamMembers(ctx, teamID, nil); err != nil || res != nil {
		t.Errorf("AddTeamMembers(nil) = %v, %v", res, err)
	}
	res, err := c.AddTeamMembers(ctx, teamID, []string{graceID, alanID})
	if err != nil || len(res) != 2 || !res[0].Added() || !res[1].Added() || res[0].UserID != graceID || res[1].UserID != alanID {
		t.Fatalf("AddTeamMembers = %+v, %v", res, err)
	}
	if got := e.fake.CountRequests("POST", "/v1.0/teams/*/members/add"); got != 1 {
		t.Errorf("%d members/add requests for two users, want 1", got)
	}
	if got := e.fake.CountRequests("POST", "/v1.0/teams/*/members"); got != 0 {
		t.Errorf("%d single member POSTs, want none", got)
	}

	// 207: one user is unknown, one is refused for another reason, one is
	// added. Each result says which.
	e.fake.FailMemberAdd(katherineID, "Forbidden", "The user is blocked from joining teams.")
	res, err = c.AddTeamMembers(ctx, teamID, []string{"no-such-user", katherineID, "a0000000-0000-4000-8000-000000000007"})
	if err != nil || len(res) != 3 {
		t.Fatalf("partial AddTeamMembers = %+v, %v", res, err)
	}
	if r := res[0]; r.Added() || !r.Gone() || r.UserID != "no-such-user" || r.Err() == nil {
		t.Errorf("unknown user result = %+v", r)
	}
	if r := res[1]; r.Added() || r.Gone() || r.Code != "Forbidden" || r.Err() == nil {
		t.Errorf("refused user result = %+v", r)
	}
	if r := res[2]; !r.Added() || r.Gone() || r.Err() != nil {
		t.Errorf("added user result = %+v", r)
	}
	members := map[string]bool{}
	for m, err := range c.TeamMembers(ctx, teamID) {
		if err != nil {
			t.Fatal(err)
		}
		members[m.UserID] = m.IsOwner()
	}
	if len(members) != 4 || !members[adaID] || members[graceID] || members[katherineID] {
		t.Errorf("members after the adds = %v", members)
	}

	// The request limit is the caller's to respect; the client refuses to
	// send more than Graph accepts, and Graph refuses it too.
	many := make([]string, graph.MaxMembersPerAdd+1)
	for i := range many {
		many[i] = fmt.Sprintf("a0000000-0000-4000-8000-%012d", 100+i)
	}
	wantFail(t, second2(c.AddTeamMembers(ctx, teamID, many)), "graph.bad_request")
	wantFail(t, second2(c.AddTeamMembers(ctx, "no-such-team", []string{graceID})), "graph.not_found")

	// A lost response is not retried blindly: the caller reads back.
	e.fake.ClearMemberAddFailures()
	e.fake.Faults().MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/members/add", Action: fakegraph.ActionFailAfterCommit, Count: 1})
	before := e.fake.CountRequests("POST", "/v1.0/teams/*/members/add")
	if _, err := c.AddTeamMembers(ctx, teamID, []string{katherineID}); err == nil {
		t.Fatal("a lost response was reported as success")
	}
	if got := e.fake.CountRequests("POST", "/v1.0/teams/*/members/add") - before; got != 1 {
		t.Errorf("members/add was sent %d times after a lost response, want 1", got)
	}
	found := false
	for m, err := range c.TeamMembers(ctx, teamID) {
		if err != nil {
			t.Fatal(err)
		}
		found = found || m.UserID == katherineID
	}
	if !found {
		t.Error("the member whose response was lost is not in the team")
	}
}

// TestMemberResultClassification pins how per-user errors of a 207 are read.
// The codes the real service uses are not documented.
func TestMemberResultClassification(t *testing.T) {
	for _, tc := range []struct {
		r           graph.MemberResult
		added, gone bool
	}{
		{graph.MemberResult{UserID: "u"}, true, false},
		{graph.MemberResult{UserID: "u", Code: "NotFound", Message: "x"}, false, true},
		{graph.MemberResult{UserID: "u", Code: "Request_ResourceNotFound"}, false, true},
		{graph.MemberResult{UserID: "u", Code: "BadRequest", Message: "User does not exist."}, false, true},
		{graph.MemberResult{UserID: "u", Code: "BadRequest", Message: "Team does not exist."}, false, false},
		{graph.MemberResult{UserID: "u", Code: "Forbidden", Message: "Blocked."}, false, false},
		{graph.MemberResult{UserID: "u", Code: "TooManyRequests"}, false, false},
		{graph.MemberResult{UserID: "u", Message: "only a message"}, false, false},
	} {
		if tc.r.Added() != tc.added || tc.r.Gone() != tc.gone || (tc.r.Err() == nil) != tc.added {
			t.Errorf("%+v: Added=%v Gone=%v Err=%v", tc.r, tc.r.Added(), tc.r.Gone(), tc.r.Err())
		}
	}
}

// TestPrivateChannelMembers covers the members of a private channel: only
// team members can be added, a standard channel has none of its own, and
// the owner the channel was created with stays its owner.
func TestPrivateChannelMembers(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	c := e.client
	teamID, err := c.CreateTeam(ctx, graph.TeamSpec{DisplayName: "Roster", OwnerID: adaID})
	if err != nil {
		t.Fatal(err)
	}
	std, err := c.CreateChannel(ctx, teamID, graph.ChannelSpec{DisplayName: "open"})
	if err != nil {
		t.Fatal(err)
	}
	priv, err := c.CreateChannel(ctx, teamID, graph.ChannelSpec{DisplayName: "leads", MembershipType: "private", OwnerID: adaID})
	if err != nil {
		t.Fatal(err)
	}
	roster := func(channelID string) map[string]bool {
		t.Helper()
		out := map[string]bool{}
		for m, err := range c.ChannelMembers(ctx, teamID, channelID) {
			if err != nil {
				t.Fatal(err)
			}
			out[m.UserID] = m.IsOwner()
		}
		return out
	}
	if got := roster(priv.ID); len(got) != 1 || !got[adaID] {
		t.Fatalf("new private channel members = %v, want its owner only", got)
	}

	// Not in the team: refused, and recognisably so.
	err = c.AddChannelMember(ctx, teamID, priv.ID, graceID, false)
	if !graph.IsNotTeamMember(err) {
		t.Fatalf("adding someone outside the team: %v", err)
	}
	wantFail(t, err, "graph.not_team_member")
	if res, err := c.AddTeamMembers(ctx, teamID, []string{graceID}); err != nil || !res[0].Added() {
		t.Fatal(res, err)
	}
	if err := c.AddChannelMember(ctx, teamID, priv.ID, graceID, false); err != nil {
		t.Fatalf("AddChannelMember: %v", err)
	}
	if got := roster(priv.ID); len(got) != 2 || !got[adaID] || got[graceID] {
		t.Errorf("private channel members = %v", got)
	}
	// A deleted or disabled account answers 404.
	if err := c.AddChannelMember(ctx, teamID, priv.ID, "no-such-user", false); !graph.IsNotFound(err) {
		t.Errorf("unknown user: %v", err)
	}
	// A standard channel takes no members of its own and lists the team's.
	wantFail(t, c.AddChannelMember(ctx, teamID, std.ID, graceID, false), "graph.bad_request")
	if got := roster(std.ID); len(got) != 2 {
		t.Errorf("standard channel roster = %v, want the team's two members", got)
	}
	views := e.fake.Channels(teamID)
	if v := views[2]; len(v.Members) != 2 || !v.Members[0].Owner || v.Members[1].Owner {
		t.Errorf("channel view members = %+v", v.Members)
	}
}

// TestLegacyMigrationMode covers the creation-mode import path, the only one
// in the national clouds: a team and its channels are created in migration
// mode, messages are imported without startMigration, and the team takes
// members only after every channel and then the team itself are completed.
func TestLegacyMigrationMode(t *testing.T) {
	e := newEnv(t, withServer(fakegraph.WithStartMigrationUnavailable()))
	ctx := context.Background()
	c := e.client
	created := ts("2019-06-01T00:00:00Z")

	// A creation time in the future is refused.
	future := e.clock.Now().Add(time.Hour)
	wantFail(t, second2(c.CreateTeam(ctx, graph.TeamSpec{DisplayName: "Legacy", MigrationCreatedAt: &future})), "graph.bad_request")

	teamID, err := c.CreateTeam(ctx, graph.TeamSpec{DisplayName: "Legacy", OwnerID: adaID, MigrationCreatedAt: &created})
	if err != nil {
		t.Fatalf("CreateTeam in migration mode: %v", err)
	}
	view := e.fake.Teams()[2]
	if view.ID != teamID || view.MigrationMode != "inProgress" || len(view.Members) != 0 {
		t.Fatalf("team view = %+v, want migration mode and no members", view)
	}
	// Nobody can be added, by either request, until the team is completed.
	wantFail(t, c.AddTeamMember(ctx, teamID, adaID, true), "graph.bad_request")
	wantFail(t, second2(c.AddTeamMembers(ctx, teamID, []string{graceID})), "graph.bad_request")
	n := 0
	for _, err := range c.TeamMembers(ctx, teamID) {
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 0 {
		t.Errorf("a team in migration mode lists %d members", n)
	}

	// General exists, in migration mode, as old as the team.
	general, ok, err := c.FindChannel(ctx, teamID, "General")
	if err != nil || !ok || general.MigrationMode != graph.MigrationInProgress || !general.CreatedDateTime.Equal(created) {
		t.Fatalf("General = %+v, %v, %v", general, ok, err)
	}
	// Other channels are created in migration mode; standard only.
	chCreated := ts("2019-07-01T00:00:00Z")
	wantFail(t, second2(c.CreateChannel(ctx, teamID, graph.ChannelSpec{DisplayName: "late", MigrationCreatedAt: &future})), "graph.bad_request")
	wantFail(t, second2(c.CreateChannel(ctx, teamID, graph.ChannelSpec{DisplayName: "secret", MembershipType: "private", OwnerID: adaID, MigrationCreatedAt: &chCreated})), "graph.bad_request")
	ch, err := c.CreateChannel(ctx, teamID, graph.ChannelSpec{DisplayName: "history", MigrationCreatedAt: &chCreated})
	if err != nil || ch.MigrationMode != graph.MigrationInProgress || !ch.CreatedDateTime.Equal(chCreated) {
		t.Fatalf("CreateChannel in migration mode = %+v, %v", ch, err)
	}

	// Importing works at once; startMigration does not exist here.
	if _, err := c.ImportMessage(ctx, graph.ImportTarget{TeamID: teamID, ChannelID: ch.ID}, msgAt("2019-08-01T10:00:00Z")); err != nil {
		t.Fatalf("import into a channel created in migration mode: %v", err)
	}
	if _, err := c.ImportMessage(ctx, graph.ImportTarget{TeamID: teamID, ChannelID: general.ID}, msgAt("2019-08-01T11:00:00Z")); err != nil {
		t.Fatalf("import into General: %v", err)
	}
	if err := c.StartMigration(ctx, teamID, ch.ID, nil); err == nil || graph.IsAlreadyMigrating(err) {
		t.Errorf("startMigration where it is unavailable: %v", err)
	}

	// The team cannot be completed while a channel is in migration mode.
	err = c.CompleteTeamMigration(ctx, teamID)
	if !graph.IsChannelsMigrating(err) {
		t.Fatalf("CompleteTeamMigration with open channels: %v", err)
	}
	wantFail(t, err, "graph.channels_still_migrating")
	if err := c.CompleteChannelMigration(ctx, teamID, ch.ID); err != nil {
		t.Fatal(err)
	}
	if err := c.CompleteTeamMigration(ctx, teamID); !graph.IsChannelsMigrating(err) {
		t.Fatalf("CompleteTeamMigration with General still open: %v", err)
	}
	if err := c.CompleteChannelMigration(ctx, teamID, general.ID); err != nil {
		t.Fatal(err)
	}
	// A lost response is retried, and the repeat reads as already done.
	e.fake.Faults().MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/completeMigration", Action: fakegraph.ActionFailAfterCommit, Count: 1})
	err = c.CompleteTeamMigration(ctx, teamID)
	if !graph.IsNotInMigration(err) {
		t.Fatalf("CompleteTeamMigration after a lost response: %v", err)
	}
	wantFail(t, err, "graph.not_in_migration")
	if got := e.fake.Teams()[2].MigrationMode; got != "completed" {
		t.Errorf("team migration mode = %q", got)
	}
	// A team that never was in migration mode answers the same.
	if err := c.CompleteTeamMigration(ctx, engTeam); !graph.IsNotInMigration(err) {
		t.Errorf("CompleteTeamMigration on an ordinary team: %v", err)
	}

	// Now it takes its owner and members.
	if err := c.AddTeamMember(ctx, teamID, adaID, true); err != nil {
		t.Fatalf("adding the owner after completion: %v", err)
	}
	if res, err := c.AddTeamMembers(ctx, teamID, []string{graceID}); err != nil || !res[0].Added() {
		t.Fatalf("adding members after completion: %+v, %v", res, err)
	}
	if mem := e.fake.Teams()[2].Members; len(mem) != 2 || !mem[0].Owner || mem[1].Owner {
		t.Errorf("members = %+v", mem)
	}

	if got := e.fake.CountRequests("POST", "/v1.0/teams/*/channels/*/startMigration"); got != 1 {
		t.Errorf("%d startMigration requests, want only the one this test made on purpose", got)
	}
}

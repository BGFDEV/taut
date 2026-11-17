package store_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/store/storetest"
)

// chatPlan is testPlan with a direct message and a group DM added: each a
// conversation with no team, with its participants, and with an extract, a
// transform and a load lane.
func chatPlan() store.Plan {
	p := testPlan([]string{"C1"}, 1)
	p.Users = append(p.Users,
		store.UserMapping{SlackID: "U3", Name: "Cy", Method: "email", EntraID: "e-3"},
		store.UserMapping{SlackID: "U4", Name: "Di", Method: "email", EntraID: "e-4"})
	p.Channels = append(p.Channels,
		store.ChannelMapping{SlackID: "D1", Kind: store.KindDM, TeamIndex: store.NoTeam, TargetName: "Ada, Cy", Members: []string{"U1", "U3"}},
		store.ChannelMapping{SlackID: "G1", SlackName: "mpdm-ada--cy--di-1", Kind: store.KindMPIM, TeamIndex: store.NoTeam,
			TargetName: "Ada, Cy, Di", Members: []string{"U1", "U3", "U4"}, Topic: "Launch crew"})
	for _, c := range []string{"D1", "G1"} {
		p.Lanes = append(p.Lanes,
			store.Lane{ID: "extract:" + c, Stage: store.StageExtract, Key: c},
			store.Lane{ID: "transform:" + c, Stage: store.StageTransform, Key: c},
			store.Lane{ID: "load:" + c, Stage: store.StageLoad, Key: c})
		p.Batches = append(p.Batches,
			store.Batch{LaneID: "extract:" + c, Seq: 0, Key: "2024-03-01"},
			store.Batch{LaneID: "transform:" + c, Seq: 0, Key: "2024-03-01", DependsOn: []store.BatchRef{{LaneID: "extract:" + c, Seq: 0}}},
			store.Batch{LaneID: "load:" + c, Seq: 0, Key: "create"},
			store.Batch{LaneID: "load:" + c, Seq: 1, Key: "2024-03-01", DependsOn: []store.BatchRef{{LaneID: "transform:" + c, Seq: 0}}})
	}
	return p
}

func TestPlanWithChats(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := newRun(t, s, "acme")
	applyPlan(t, s, id, chatPlan())

	dm, err := s.Channel(ctx, id, "D1")
	if err != nil {
		t.Fatal(err)
	}
	if !dm.IsChat() || dm.TeamIndex != store.NoTeam || dm.TargetName != "Ada, Cy" || dm.Topic != "" ||
		!reflect.DeepEqual(dm.Members, []string{"U1", "U3"}) || dm.State != store.ChannelPlanned {
		t.Fatalf("direct message = %+v", dm)
	}
	group, err := s.Channel(ctx, id, "G1")
	if err != nil || group.Topic != "Launch crew" || !reflect.DeepEqual(group.Members, []string{"U1", "U3", "U4"}) || group.TeamIndex != store.NoTeam {
		t.Fatalf("group DM = %+v, %v", group, err)
	}
	ch, err := s.Channel(ctx, id, "C1")
	if err != nil || ch.IsChat() || ch.TeamIndex != 0 || ch.Members != nil || ch.Topic != "" {
		t.Fatalf("channel = %+v, %v", ch, err)
	}
	all, err := s.Channels(ctx, id)
	if err != nil || len(all) != 3 {
		t.Fatalf("Channels = %+v, %v", all, err)
	}

	// Progress lists the chats after the channels, with no team.
	p, err := s.Progress(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range p.Channels {
		got = append(got, c.SlackID+"/"+c.Kind+"/"+c.Team)
	}
	if want := []string{"C1/public/Engineering", "D1/dm/", "G1/mpim/"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("progress channels = %v, want %v", got, want)
	}

	// A chat is recorded and completed like a channel.
	l := claimLane(t, s, id, store.StageLoad, "load:D1", "loader")
	start := time.Date(2021, 5, 6, 7, 8, 9, 0, time.UTC)
	if err := s.Fenced(ctx, l, func(tx store.Tx) error {
		return tx.SetChannel(ctx, "D1", "19:abc_def@unq.gbl.spaces", store.ChannelMigrating, &start)
	}); err != nil {
		t.Fatal(err)
	}
	dm, err = s.Channel(ctx, id, "D1")
	if err != nil || dm.TeamsID != "19:abc_def@unq.gbl.spaces" || dm.State != store.ChannelMigrating || dm.MigrationStart == nil || !dm.MigrationStart.Equal(start) {
		t.Fatalf("after SetChannel: %+v, %v", dm, err)
	}
}

func TestPlanWithChatsValidation(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := newRun(t, s, "acme")
	l := mustClaim(t, s, id, "planner")
	for name, c := range map[string]struct {
		mutate func(p *store.Plan)
		want   string
	}{
		"a chat in a team":         {func(p *store.Plan) { p.Channels[1].TeamIndex = 0 }, "goes into a chat"},
		"a chat with one person":   {func(p *store.Plan) { p.Channels[1].Members = []string{"U1"} }, "at least two"},
		"a chat with nobody":       {func(p *store.Plan) { p.Channels[2].Members = nil }, "at least two"},
		"a channel with no team":   {func(p *store.Plan) { p.Channels[0].TeamIndex = store.NoTeam }, "not in the plan"},
		"a channel with members":   {func(p *store.Plan) { p.Channels[0].Members = []string{"U1", "U2"} }, "chat participants"},
		"a channel with a topic":   {func(p *store.Plan) { p.Channels[0].Topic = "x" }, "chat topic"},
		"a chat listed a 2nd time": {func(p *store.Plan) { p.Channels = append(p.Channels, p.Channels[1]) }, "appears twice"},
	} {
		p := chatPlan()
		c.mutate(&p)
		err := s.Fenced(ctx, l, func(tx store.Tx) error { return tx.ApplyPlan(ctx, p) })
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one containing %q", name, err, c.want)
		}
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM map_channels`); n != 0 {
		t.Fatalf("%d channel rows after rejected plans", n)
	}
	// The database refuses what the validation would: a chat with a team,
	// a channel without.
	release(t, s, l)
	applyPlan(t, s, id, chatPlan())
	if _, err := s.Pool().Exec(ctx, `UPDATE map_channels SET team_index = 0 WHERE slack_id = 'D1'`); err == nil {
		t.Error("the database let a chat have a team")
	}
	if _, err := s.Pool().Exec(ctx, `UPDATE map_channels SET team_index = NULL WHERE slack_id = 'C1'`); err == nil {
		t.Error("the database let a channel have no team")
	}
}

func TestShiftMessages(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := newRun(t, s, "acme")
	applyPlan(t, s, id, chatPlan())
	l := claimLane(t, s, id, store.StageLoad, "load:D1", "loader")
	in := func(fn func(tx store.Tx) error) error { return s.Fenced(ctx, l, fn) }
	shift := func(from int64) (int, error) {
		var n int
		err := in(func(tx store.Tx) (err error) { n, err = tx.ShiftMessages(ctx, "D1", from, 3); return err })
		return n, err
	}
	at := func(ts string) store.Message {
		t.Helper()
		m, err := s.GetMessage(ctx, id, store.MessageKey{ChannelID: "D1", SlackTS: ts})
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	if ms, err := s.LastTargetMS(ctx, id, "D1"); err != nil || ms != 0 {
		t.Fatalf("LastTargetMS of an empty conversation = %d, %v", ms, err)
	}
	// A split message (two parts a millisecond apart), a message right
	// behind it, a gap, and one more.
	if err := in(func(tx store.Tx) error {
		return tx.PutMessages(ctx, []store.Message{
			msg("D1", "1.1", 0, 1000, 0), msg("D1", "1.1", 1, 1001, 0),
			msg("D1", "1.2", 0, 1002, 0),
			msg("D1", "1.9", 0, 1009, 0),
			msg("G1", "1.1", 0, 1003, 0),
		})
	}); err != nil {
		t.Fatal(err)
	}
	if ms, err := s.LastTargetMS(ctx, id, "D1"); err != nil || ms != 1009 {
		t.Fatalf("LastTargetMS = %d, %v", ms, err)
	}

	// The whole run moves together; what is behind a gap, and the other
	// conversation, stay.
	n, err := shift(1000)
	if err != nil || n != 3 {
		t.Fatalf("shift = %d, %v; want the 3 posts without a gap between them", n, err)
	}
	for ts, want := range map[string]int64{"1.1": 1001, "1.2": 1003, "1.9": 1009} {
		if m := at(ts); m.TargetMS != want || m.Hash != "h-"+ts || m.State != store.MessagePending {
			t.Errorf("%s: %+v, want it at %d with its hash untouched", ts, m, want)
		}
	}
	if m, _ := s.GetMessage(ctx, id, store.MessageKey{ChannelID: "D1", SlackTS: "1.1", Part: 1}); m.TargetMS != 1002 || m.ShiftMS != 1 {
		t.Errorf("part 1 = %+v", m)
	}
	if m, _ := s.GetMessage(ctx, id, store.MessageKey{ChannelID: "G1", SlackTS: "1.1"}); m.TargetMS != 1003 || m.ShiftMS != 0 {
		t.Errorf("the other conversation moved: %+v", m)
	}
	// From the middle of a run: only what is behind moves.
	if n, err := shift(1003); err != nil || n != 1 || at("1.2").TargetMS != 1004 || at("1.2").ShiftMS != 2 || at("1.1").TargetMS != 1001 {
		t.Fatalf("shift from the last of the run = %d, %v", n, err)
	}

	// Nothing there: not found, nothing changed.
	_, err = shift(5000)
	wantErr(t, err, store.ErrNotFound)

	// The limit: a post moved as far as it may be stops its whole run, and
	// nothing moves.
	if _, err := shift(1004); err != nil { // 1.2 is now moved 3 ms
		t.Fatal(err)
	}
	_, err = shift(1001) // 1.1/0, 1.1/1 at 1001, 1002; 1.2 is at 1005, behind a gap
	if err != nil {
		t.Fatalf("a run that ends before the limited post: %v", err)
	}
	_, err = shift(1005)
	wantErr(t, err, store.ErrShiftLimit)
	if at("1.2").TargetMS != 1005 || at("1.2").ShiftMS != 3 {
		t.Errorf("a refused shift moved the post: %+v", at("1.2"))
	}

	// A post that is already in Teams cannot move, so nothing in front of
	// it can be pushed onto it: the head of the run stays where it is.
	if err := in(func(tx store.Tx) error {
		return tx.MarkPosted(ctx, store.MessageKey{ChannelID: "D1", SlackTS: "1.1", Part: 1}, "1003", false)
	}); err != nil {
		t.Fatal(err)
	}
	before := at("1.1")
	_, err = shift(before.TargetMS)
	wantErr(t, err, store.ErrTimestampTaken)
	if got := at("1.1"); got.TargetMS != before.TargetMS || got.ShiftMS != before.ShiftMS {
		t.Errorf("a blocked shift moved the head: %+v, was %+v", got, before)
	}
	// And the posted one itself is never moved.
	if _, err := shift(1003); err == nil {
		t.Error("a posted message was moved")
	}
}

// A database at schema version 3 with rows is migrated to version 4:
// channels keep their team, and every post has moved by nothing.
func TestMigrateFromV3WithRows(t *testing.T) {
	t.Parallel()
	url, schema := storetest.NewSchema(t)
	s, err := store.Open(t.Context(), url, store.WithSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ctx := t.Context()
	if err := store.MigrateTo(ctx, s, 3); err != nil {
		t.Fatal(err)
	}
	exec(t, s, `INSERT INTO runs (id, name, config, config_hash, status, planned_at) VALUES ('r1', 'acme', '', 'h', 'running', now())`)
	exec(t, s, `INSERT INTO map_teams (run_id, idx, name) VALUES ('r1', 0, 'T')`)
	exec(t, s, `INSERT INTO map_channels (run_id, slack_id, slack_name, kind, team_index, state) VALUES ('r1', 'C1', 'general', 'public', 0, 'migrating')`)
	exec(t, s, `INSERT INTO map_messages (run_id, channel_id, slack_ts, part, target_ms, hash) VALUES ('r1', 'C1', '1.1', 0, 1000, 'h')`)
	if _, err := s.Pool().Exec(ctx, `INSERT INTO map_channels (run_id, slack_id, kind, team_index) VALUES ('r1', 'D1', 'dm', NULL)`); err == nil {
		t.Fatal("version 3 accepted a conversation with no team")
	}

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM schema_migrations`); n != schemaVersion {
		t.Fatalf("schema_migrations has %d rows, want %d", n, schemaVersion)
	}
	ch, err := s.Channel(ctx, "r1", "C1")
	if err != nil || ch.TeamIndex != 0 || ch.Members != nil || ch.Topic != "" || ch.State != store.ChannelMigrating {
		t.Fatalf("channel after the migration = %+v, %v", ch, err)
	}
	m, err := s.GetMessage(ctx, "r1", store.MessageKey{ChannelID: "C1", SlackTS: "1.1"})
	if err != nil || m.ShiftMS != 0 || m.TargetMS != 1000 {
		t.Fatalf("message after the migration = %+v, %v", m, err)
	}
	exec(t, s, `INSERT INTO map_channels (run_id, slack_id, kind, team_index, members) VALUES ('r1', 'D1', 'dm', NULL, '{U1,U2}')`)
	p, err := s.Progress(ctx, "r1")
	if err != nil || len(p.Channels) != 2 || p.Channels[0].Team != "T" || p.Channels[1].Team != "" || p.Channels[1].Kind != "dm" {
		t.Fatalf("progress after the migration = %+v, %v", p.Channels, err)
	}
}

// chatStates puts the plan's two chats where a run would have them.
func chatStates(t *testing.T, s *store.PG, id string, dm, group store.ChannelState) {
	t.Helper()
	for slackID, state := range map[string]store.ChannelState{"D1": dm, "G1": group} {
		if state == store.ChannelPlanned {
			continue
		}
		exec(t, s, `UPDATE map_channels SET teams_id = '19:' || slack_id, state = $3 WHERE run_id = $1 AND slack_id = $2`, id, slackID, string(state))
	}
}

// Cancelling a run queues the release of every chat it may have put into
// migration mode, in the same transaction, and release lanes are claimable
// although the run is cancelled. Until they are done the run is not purged.
func TestCancelQueuesChatRelease(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := newRun(t, s, "acme")
	applyPlan(t, s, id, chatPlan())
	chatStates(t, s, id, store.ChannelMigrating, store.ChannelCreated)

	if err := s.CancelRun(ctx, id); err != nil {
		t.Fatal(err)
	}
	lanes, err := s.Lanes(ctx, id, store.StageRelease)
	if err != nil || len(lanes) != 2 || lanes[0].ID != "release:D1" || lanes[1].ID != "release:G1" {
		t.Fatalf("release lanes = %+v, %v", lanes, err)
	}
	// Nothing else of a cancelled run can be claimed.
	noClaim(t, s, id, store.StageLoad, store.StageExtract, store.StageTransform, store.StageFinalize)
	if runs, err := s.Purgeable(ctx); err != nil || len(runs) != 0 {
		t.Fatalf("a run with chats still in migration mode is purgeable: %+v, %v", runs, err)
	}
	wantErr(t, s.MarkPurged(ctx, id), store.ErrChatsMigrating)

	for range 2 {
		l := mustClaim(t, s, id, "releaser", store.StageRelease)
		if err := s.Fenced(ctx, l, func(tx store.Tx) error {
			if err := tx.SetChannel(ctx, l.Key, "", store.ChannelCompleted, nil); err != nil {
				return err
			}
			return tx.CompleteBatch(ctx, 0, "", 1, nil)
		}); err != nil {
			t.Fatal(err)
		}
		release(t, s, l)
	}
	noClaim(t, s, id, store.StageRelease)
	if runs, err := s.Purgeable(ctx); err != nil || len(runs) != 1 {
		t.Fatalf("purgeable after the release = %+v, %v", runs, err)
	}
	// Cancelling again changes nothing.
	if err := s.CancelRun(ctx, id); err != nil {
		t.Fatal(err)
	}
	if lanes, _ := s.Lanes(ctx, id, store.StageRelease); len(lanes) != 2 {
		t.Errorf("a second cancel added release lanes: %+v", lanes)
	}

	// Purging removes who was in each conversation, and keeps the record.
	exec(t, s, `INSERT INTO map_files (run_id, channel_id, slack_id, name, state) VALUES ($1, 'D1', 'F1', 'salary review.pdf', 'skipped'), ($1, 'C1', 'F2', 'roadmap.pdf', 'skipped')`, id)
	if err := s.MarkPurged(ctx, id); err != nil {
		t.Fatal(err)
	}
	for _, slackID := range []string{"D1", "G1"} {
		c, err := s.Channel(ctx, id, slackID)
		if err != nil || c.TargetName != "" || c.Members != nil || c.Topic != "" || c.SlackName != "" || c.TeamsID == "" || c.State != store.ChannelCompleted {
			t.Errorf("%s after the purge = %+v, %v", slackID, c, err)
		}
	}
	if f, err := s.GetFile(ctx, id, "D1", "F1"); err != nil || f.Name != "" {
		t.Errorf("a direct message's file after the purge = %+v, %v", f, err)
	}
	if f, err := s.GetFile(ctx, id, "C1", "F2"); err != nil || f.Name != "roadmap.pdf" {
		t.Errorf("a channel's file after the purge = %+v, %v", f, err)
	}
	if c, _ := s.Channel(ctx, id, "C1"); c.SlackName == "" || c.TargetName == "" {
		t.Errorf("a channel was scrubbed: %+v", c)
	}
	wantErr(t, func() error { _, err := s.ReleaseChats(ctx, id, nil); return err }(), store.ErrPurged)
}

// A run with no chat in Teams queues nothing, and one whose chats are
// already out of migration mode neither.
func TestCancelWithoutChatsToRelease(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := newRun(t, s, "acme")
	applyPlan(t, s, id, chatPlan())
	chatStates(t, s, id, store.ChannelCompleted, store.ChannelPlanned)
	if err := s.CancelRun(ctx, id); err != nil {
		t.Fatal(err)
	}
	if lanes, err := s.Lanes(ctx, id, store.StageRelease); err != nil || len(lanes) != 0 {
		t.Fatalf("release lanes = %+v, %v", lanes, err)
	}
	if runs, err := s.Purgeable(ctx); err != nil || len(runs) != 1 {
		t.Fatalf("purgeable = %+v, %v", runs, err)
	}
}

// ReleaseChats works on a run in any state, for the chats named, and
// retries release work that failed.
func TestReleaseChats(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := newRun(t, s, "acme")
	applyPlan(t, s, id, chatPlan())
	chatStates(t, s, id, store.ChannelMigrating, store.ChannelMigrating)
	exec(t, s, `UPDATE runs SET status = 'failed' WHERE id = $1`, id)

	n, err := s.ReleaseChats(ctx, id, []string{"D1"})
	if err != nil || n != 1 {
		t.Fatalf("ReleaseChats(D1) = %d, %v", n, err)
	}
	// Claimable on a failed run; fail it once.
	l := mustClaim(t, s, id, "releaser", store.StageRelease)
	if l.LaneID != "release:D1" {
		t.Fatalf("claimed %s", l.LaneID)
	}
	if err := s.Fenced(ctx, l, func(tx store.Tx) error { return tx.FailBatch(ctx, 0, "403") }); err != nil {
		t.Fatal(err)
	}
	release(t, s, l)
	noClaim(t, s, id, store.StageRelease)
	// Asking again, for everything, reopens it and adds the other chat.
	if n, err := s.ReleaseChats(ctx, id, nil); err != nil || n != 2 {
		t.Fatalf("ReleaseChats(all) = %d, %v", n, err)
	}
	seen := map[string]bool{}
	for range 2 {
		l := mustClaim(t, s, id, "releaser", store.StageRelease)
		seen[l.LaneID] = true
		complete(t, s, l, 0)
		release(t, s, l)
	}
	if !seen["release:D1"] || !seen["release:G1"] {
		t.Errorf("claimed %v", seen)
	}
	// The run is still failed: releasing chats does not change what it is.
	if st := runStatus(t, s, id); st != store.RunFailed {
		t.Errorf("status = %s", st)
	}
	if _, err := s.ReleaseChats(ctx, "no-such-run", nil); err == nil {
		t.Error("an unknown run was accepted")
	}
}

func TestSkipChannelAndFinalizedWithSkipped(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := newRun(t, s, "acme")
	applyPlan(t, s, id, chatPlan())
	l := claimLane(t, s, id, store.StageLoad, "load:D1", "loader")
	if err := s.Fenced(ctx, l, func(tx store.Tx) error {
		return tx.SkipChannel(ctx, "D1", "graph.chat_refused", "one of its participants has no Teams licence")
	}); err != nil {
		t.Fatal(err)
	}
	c, err := s.Channel(ctx, id, "D1")
	if err != nil || c.State != store.ChannelSkipped || c.SkipCode != "graph.chat_refused" || c.SkipReason == "" {
		t.Fatalf("skipped conversation = %+v, %v", c, err)
	}
	p, err := s.Progress(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, cp := range p.Channels {
		if (cp.SlackID == "D1") != (cp.SkipCode != "") || (cp.SlackID == "D1" && cp.State != store.ChannelSkipped) {
			t.Errorf("progress = %+v", cp)
		}
	}
	err = s.Fenced(ctx, l, func(tx store.Tx) error { return tx.SkipChannel(ctx, "D9", "x", "y") })
	wantErr(t, err, store.ErrNotFound)
	release(t, s, l)

	// A run whose every conversation is completed or skipped is finalized.
	exec(t, s, `UPDATE lanes SET status = 'done' WHERE run_id = $1`, id)
	exec(t, s, `UPDATE batches SET status = 'done' WHERE run_id = $1`, id)
	exec(t, s, `UPDATE map_channels SET state = 'completed' WHERE run_id = $1 AND slack_id <> 'D1'`, id)
	exec(t, s, `UPDATE runs SET status = 'finalizing' WHERE id = $1`, id)
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if st := runStatus(t, s, id); st != store.RunFinalized {
		t.Errorf("status = %s, want finalized: a skipped conversation has nothing left to finalize", st)
	}
}

func TestEarlierChatsAndRunLayout(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	first := newRun(t, s, "acme")
	applyPlan(t, s, first, chatPlan())
	start := time.Date(2020, 2, 3, 4, 5, 6, 0, time.UTC)
	exec(t, s, `UPDATE map_channels SET teams_id = '19:group', state = 'migrating', migration_start = $2 WHERE run_id = $1 AND slack_id = 'G1'`, first, start)
	if err := s.CancelRun(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := store.NewRunID()
	if err := s.CreateRun(ctx, store.Run{ID: second, Name: "acme", Config: []byte("name: acme"), ConfigHash: "mh", Layout: 4}); err != nil {
		t.Fatal(err)
	}
	got, err := s.EarlierChats(ctx, second, "G1")
	if err != nil || len(got) != 1 || got[0].RunID != first || got[0].TeamsID != "19:group" || got[0].MigrationStart == nil ||
		!got[0].MigrationStart.Equal(start) || string(got[0].Config) != "name: acme" {
		t.Fatalf("EarlierChats = %+v, %v", got, err)
	}
	// Not the run's own, and not a conversation that never got a chat.
	if got, err := s.EarlierChats(ctx, first, "G1"); err != nil || len(got) != 0 {
		t.Errorf("a run's own chat is earlier than itself: %+v, %v", got, err)
	}
	if got, err := s.EarlierChats(ctx, second, "D1"); err != nil || len(got) != 0 {
		t.Errorf("EarlierChats(D1) = %+v, %v", got, err)
	}

	// The layout a run needs is kept from its creation; the default is 3.
	if r, err := s.GetRun(ctx, second); err != nil || r.Layout != 4 {
		t.Errorf("layout = %d, %v", r.Layout, err)
	}
	if r, err := s.GetRun(ctx, first); err != nil || r.Layout != 3 {
		t.Errorf("default layout = %d, %v", r.Layout, err)
	}

	// Deleting a run takes its conversations with it, chats included.
	exec(t, s, `DELETE FROM audit_log_heads WHERE run_id = $1`, first)
	exec(t, s, `DELETE FROM runs WHERE id = $1`, first)
	if n := scalar[int](t, s, `SELECT count(*) FROM map_channels WHERE run_id = $1`, first); n != 0 {
		t.Errorf("%d conversations outlived their run", n)
	}
}

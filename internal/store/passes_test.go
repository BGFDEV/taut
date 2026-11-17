package store_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/store/storetest"
)

// basePass is a finished first pass with two channels. In C1 it has four
// Slack messages: 1.1 posted, 2.2 posted in two parts, 3.3 failed, and a
// reply 4.4 held; an uploaded file, a failed file, and a thread anchor. C2
// has one posted message. Both channels are in migration mode.
func basePass(t *testing.T, s *store.PG, name string) string {
	t.Helper()
	ctx := t.Context()
	id := plannedRun(t, s, name, []string{"C1", "C2"}, 1)
	drain(t, s, id, store.StageTeam, store.StageExtract, store.StageTransform)

	start := time.Date(2019, 12, 31, 0, 0, 0, 0, time.UTC)
	edited := "1700000000.000100"
	for _, c := range []string{"C1", "C2"} {
		l := claimLane(t, s, id, store.StageLoad, "load:"+c, "loader")
		err := s.Fenced(ctx, l, func(tx store.Tx) error {
			if err := tx.SetTeam(ctx, 0, "team-1", false); err != nil {
				return err
			}
			if err := tx.SetChannel(ctx, c, "19:"+c+"@thread.tacv2", store.ChannelMigrating, &start); err != nil {
				return err
			}
			if c == "C2" {
				if err := tx.PutMessages(ctx, []store.Message{{MessageKey: store.MessageKey{ChannelID: c, SlackTS: "9.9"}, TargetMS: 9900, Hash: "h9"}}); err != nil {
					return err
				}
				return tx.MarkPosted(ctx, store.MessageKey{ChannelID: c, SlackTS: "9.9"}, "9900", false)
			}
			if err := tx.SetChannelFolder(ctx, c, "drive-1", "folder-1"); err != nil {
				return err
			}
			msgs := []store.Message{
				{MessageKey: store.MessageKey{ChannelID: c, SlackTS: "1.1"}, TargetMS: 1100, Hash: "h1", Edited: &edited},
				{MessageKey: store.MessageKey{ChannelID: c, SlackTS: "2.2"}, TargetMS: 2200, Hash: "h2a"},
				{MessageKey: store.MessageKey{ChannelID: c, SlackTS: "2.2", Part: 1}, TargetMS: 2201, Hash: "h2b", AnchorTS: "2.2"},
				{MessageKey: store.MessageKey{ChannelID: c, SlackTS: "3.3"}, TargetMS: 3300, Hash: "h3"},
				{MessageKey: store.MessageKey{ChannelID: c, SlackTS: "4.4"}, TargetMS: 4400, Hash: "h4", AnchorTS: "3.3"},
			}
			if err := tx.PutMessages(ctx, msgs); err != nil {
				return err
			}
			for _, k := range []store.MessageKey{{ChannelID: c, SlackTS: "1.1"}, {ChannelID: c, SlackTS: "2.2"}, {ChannelID: c, SlackTS: "2.2", Part: 1}} {
				if err := tx.MarkPosted(ctx, k, "T"+k.SlackTS+"/"+string(rune('0'+k.Part)), k.Part == 1); err != nil {
					return err
				}
			}
			if err := tx.MarkFailed(ctx, store.MessageKey{ChannelID: c, SlackTS: "3.3"}, store.MessageFailed, "graph.invalid_author", "refused"); err != nil {
				return err
			}
			if err := tx.MarkFailed(ctx, store.MessageKey{ChannelID: c, SlackTS: "4.4"}, store.MessageHeld, "anchor_missing", "held"); err != nil {
				return err
			}
			if err := tx.SetAnchor(ctx, c, "0.5", "1.1"); err != nil {
				return err
			}
			if err := tx.PutFiles(ctx, []store.FileMapping{
				{ChannelID: c, SlackID: "F1", Name: "a.pdf", Size: 10},
				{ChannelID: c, SlackID: "F2", Name: "b.pdf", Size: 20},
			}); err != nil {
				return err
			}
			if err := tx.MarkFileStaged(ctx, c, "F1", "runs/"+id+"/files/C1/F1", "sha", 10); err != nil {
				return err
			}
			if err := tx.MarkFileUploaded(ctx, c, "F1", "drive-1", "item-1", "https://sp.example/a.pdf", "etag-1"); err != nil {
				return err
			}
			return tx.MarkFileSettled(ctx, c, "F2", store.FileFailed, "gone", "gone from Slack")
		})
		if err != nil {
			t.Fatalf("recording the base pass: %v", err)
		}
		finishLane(t, s, l)
		release(t, s, l)
	}
	drain(t, s, id)
	if _, err := s.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if st := runStatus(t, s, id); st != store.RunCompleted {
		t.Fatalf("base pass is %s, want completed", st)
	}
	return id
}

// newPass creates the pass that follows base and records its export.
func newPass(t *testing.T, s *store.PG, name, base string) string {
	t.Helper()
	ctx := t.Context()
	id := store.NewRunID()
	if err := s.CreateRun(ctx, store.Run{ID: id, Name: name, Config: []byte("name: " + name), ConfigHash: "mh2", BaseRunID: base}); err != nil {
		t.Fatalf("CreateRun (pass): %v", err)
	}
	if err := s.SetExport(ctx, id, "exports/"+id+".zip", "sha-"+id, 99); err != nil {
		t.Fatal(err)
	}
	return id
}

// carriedPlan is testPlan for a pass that continues C1 (carried, with what
// the base recorded) and adds a channel C3.
func carriedPlan(base string) store.Plan {
	start := time.Date(2019, 12, 31, 0, 0, 0, 0, time.UTC)
	p := testPlan([]string{"C1", "C3"}, 1)
	p.CarryFrom = base
	p.WorkspaceID = "T0WORK"
	p.Teams[0].TeamsID = "team-1"
	c := &p.Channels[0]
	c.Carried, c.TeamsID, c.State, c.MigrationStart = true, "19:C1@thread.tacv2", store.ChannelMigrating, &start
	c.FilesDriveID, c.FilesFolderID = "drive-1", "folder-1"
	return p
}

func TestCreatePass(t *testing.T) {
	s := open(t)
	ctx := t.Context()

	// A pass cannot follow a run that is still importing.
	running := plannedRun(t, s, "acme", []string{"C1"}, 1)
	err := s.CreateRun(ctx, store.Run{ID: store.NewRunID(), Name: "acme", BaseRunID: running})
	wantErr(t, err, store.ErrBaseNotFinished)
	if err := s.CancelRun(ctx, running); err != nil {
		t.Fatal(err)
	}
	wantErr(t, s.CreateRun(ctx, store.Run{ID: store.NewRunID(), Name: "acme", BaseRunID: running}), store.ErrBaseNotFinished)

	base := basePass(t, s, "acme")
	// Not under another name, and not from a run that does not exist.
	wantErr(t, s.CreateRun(ctx, store.Run{ID: store.NewRunID(), Name: "other", BaseRunID: base}), store.ErrNotFound)
	wantErr(t, s.CreateRun(ctx, store.Run{ID: store.NewRunID(), Name: "acme", BaseRunID: "nope"}), store.ErrNotFound)
	// A later pass without a base is not a pass.
	if err := s.CreateRun(ctx, store.Run{ID: store.NewRunID(), Name: "acme", Pass: 2}); err == nil {
		t.Fatal("a run with pass 2 and no base was accepted")
	}

	second := newPass(t, s, "acme", base)
	r, err := s.GetRun(ctx, second)
	if err != nil || r.Pass != 2 || r.BaseRunID != base || !r.Incremental() || r.Status != store.RunQueued {
		t.Fatalf("second pass = %+v, %v", r, err)
	}
	if b, _ := s.GetRun(ctx, base); b.Pass != 1 || b.Incremental() {
		t.Fatalf("base pass = %+v", b)
	}
	// One base, one follower: while the second pass is alive nothing else
	// starts from the base.
	err = s.CreateRun(ctx, store.Run{ID: store.NewRunID(), Name: "acme", BaseRunID: base})
	if err == nil || (!strings.Contains(err.Error(), "active run") && !strings.Contains(err.Error(), "later pass")) {
		t.Fatalf("a second follower: %v", err)
	}
	if f, ok, err := s.Follower(ctx, base); err != nil || !ok || f.ID != second {
		t.Fatalf("Follower = %+v, %v, %v", f, ok, err)
	}

	// A follower that was withdrawn frees the base again.
	if err := s.CancelRun(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Follower(ctx, base); ok {
		t.Fatal("a cancelled pass still follows its base")
	}
	third := newPass(t, s, "acme", base)
	if r, _ := s.GetRun(ctx, third); r.Pass != 2 {
		t.Fatalf("the replacement pass has number %d, want 2", r.Pass)
	}

	chain, err := s.Passes(ctx, third)
	if err != nil || len(chain) != 2 || chain[0].ID != base || chain[1].ID != third {
		t.Fatalf("Passes = %+v, %v", chain, err)
	}
	if _, err := s.Passes(ctx, "nope"); err == nil {
		t.Fatal("Passes of an unknown run succeeded")
	}
}

func TestCarryForward(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	base := basePass(t, s, "acme")
	pass := newPass(t, s, "acme", base)

	// Nothing of the base is purgeable or superseded before the pass plans.
	if runs, err := s.Purgeable(ctx); err != nil || len(runs) != 0 {
		t.Fatalf("Purgeable before the plan = %+v, %v", runs, err)
	}
	applyPlan(t, s, pass, carriedPlan(base))

	r, err := s.GetRun(ctx, pass)
	if err != nil || r.WorkspaceID != "T0WORK" || r.Status != store.RunRunning {
		t.Fatalf("pass after its plan = %+v, %v", r, err)
	}

	// Only what is in Teams came along: the posted posts with their
	// timestamps and IDs, the anchor, the uploaded file.
	rows, err := s.MessagesByTS(ctx, pass, "C1", []string{"1.1", "2.2", "3.3", "4.4", "none"})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, m := range rows {
		if !m.Carried || m.State != store.MessagePosted || m.BatchSeq != store.CarriedBatch {
			t.Errorf("carried row %+v", m)
		}
		got = append(got, m.SlackTS+"/"+string(rune('0'+m.Part))+"@"+m.TeamsID)
	}
	if want := []string{"1.1/0@T1.1/0", "2.2/0@T2.2/0", "2.2/1@T2.2/1"}; !slices.Equal(got, want) {
		t.Fatalf("carried posts = %v, want %v", got, want)
	}
	if rows[0].TargetMS != 1100 || rows[0].Edited == nil || *rows[0].Edited != "1700000000.000100" {
		t.Errorf("first carried post = %+v", rows[0])
	}
	if !rows[2].Adopted || rows[2].AnchorTS != "2.2" {
		t.Errorf("carried second part = %+v", rows[2])
	}
	if a, ok, err := s.Anchor(ctx, pass, "C1", "0.5"); err != nil || !ok || a != "1.1" {
		t.Errorf("carried anchor = %q, %v, %v", a, ok, err)
	}
	f, err := s.GetFile(ctx, pass, "C1", "F1")
	if err != nil || !f.Carried || f.State != store.FileUploaded || f.ItemID != "item-1" || f.ETag != "etag-1" || f.BatchSeq != store.CarriedBatch {
		t.Fatalf("carried file = %+v, %v", f, err)
	}
	if f.StagingKey != "" || f.StagedBytes != 0 {
		t.Errorf("a carried file kept the base's staging object: %+v", f)
	}
	if _, err := s.GetFile(ctx, pass, "C1", "F2"); err == nil {
		t.Error("a file the base failed to move was carried")
	}
	// C2 is not in the pass's plan and C3 is new: neither has carried rows.
	if n := scalar[int](t, s, `SELECT count(*) FROM map_messages WHERE run_id = $1 AND channel_id <> 'C1'`, pass); n != 0 {
		t.Errorf("%d rows carried for channels the plan does not carry", n)
	}
	ch, err := s.Channel(ctx, pass, "C1")
	if err != nil || !ch.Carried || ch.TeamsID != "19:C1@thread.tacv2" || ch.State != store.ChannelMigrating || ch.FilesFolderID != "folder-1" {
		t.Fatalf("carried channel = %+v, %v", ch, err)
	}

	// Carried rows belong to no batch of the pass.
	for seq := range 2 {
		if ms, err := s.MessagesInBatch(ctx, pass, "C1", seq); err != nil || len(ms) != 0 {
			t.Errorf("MessagesInBatch(%d) = %d rows, %v", seq, len(ms), err)
		}
		if fs, err := s.FilesInBatch(ctx, pass, "C1", seq); err != nil || len(fs) != 0 {
			t.Errorf("FilesInBatch(%d) = %d rows, %v", seq, len(fs), err)
		}
	}

	// They hold their milliseconds.
	taken, err := s.TakenTargets(ctx, pass, "C1", 1000, 2200)
	if err != nil || !slices.Equal(taken, []int64{1100, 2200}) {
		t.Fatalf("TakenTargets = %v, %v", taken, err)
	}
	drain(t, s, pass, store.StageTeam, store.StageExtract)
	tl := claimLane(t, s, pass, store.StageTransform, "transform:C1", "transformer")
	err = s.Fenced(ctx, tl, func(tx store.Tx) error {
		return tx.PutMessages(ctx, []store.Message{{MessageKey: store.MessageKey{ChannelID: "C1", SlackTS: "5.5"}, TargetMS: 2201, Hash: "x"}})
	})
	wantErr(t, err, store.ErrTimestampTaken)
	// A message the base did not get into Teams is new to the pass, and a
	// carried one is left exactly as it is.
	err = s.Fenced(ctx, tl, func(tx store.Tx) error {
		return tx.PutMessages(ctx, []store.Message{
			{MessageKey: store.MessageKey{ChannelID: "C1", SlackTS: "3.3"}, TargetMS: 3300, Hash: "h3"},
			{MessageKey: store.MessageKey{ChannelID: "C1", SlackTS: "1.1"}, TargetMS: 7777, Hash: "other"},
		})
	})
	if err != nil {
		t.Fatalf("PutMessages in the pass: %v", err)
	}
	if m, _ := s.GetMessage(ctx, pass, store.MessageKey{ChannelID: "C1", SlackTS: "1.1"}); m.TargetMS != 1100 || m.Hash != "h1" || !m.Carried {
		t.Errorf("a carried post was overwritten: %+v", m)
	}
	if m, _ := s.GetMessage(ctx, pass, store.MessageKey{ChannelID: "C1", SlackTS: "3.3"}); m.Carried || m.State != store.MessagePending || m.Edited == nil {
		t.Errorf("the retried post = %+v", m)
	}
	release(t, s, tl)

	// Progress is about the pass's own work.
	p, err := s.Progress(ctx, pass)
	if err != nil {
		t.Fatal(err)
	}
	if p.Posts != 1 || p.Posted != 0 || p.Carried != 3 || p.Files != 0 || p.CarriedFiles != 1 || p.Adopted != 0 {
		t.Errorf("progress: posts=%d posted=%d carried=%d files=%d carriedFiles=%d adopted=%d", p.Posts, p.Posted, p.Carried, p.Files, p.CarriedFiles, p.Adopted)
	}
	for _, c := range p.Channels {
		if c.Earlier != (c.SlackID == "C1") {
			t.Errorf("channel %s: earlier = %v", c.SlackID, c.Earlier)
		}
	}

	// The base's ledger is what the next preflight reads.
	l, err := s.Ledger(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if l.Pass != 1 || l.Status != store.RunCompleted || !l.Planned || l.Posted != 3 || l.Files != 1 || len(l.Channels) != 2 || len(l.Users) != 2 || len(l.Teams) != 1 {
		t.Fatalf("base ledger = %+v", l)
	}
	if c := l.Channels[0]; c.SlackID != "C1" || c.Team != "Engineering" || !c.InTeams || c.Posted != 2 || c.Files != 1 || c.State != store.ChannelMigrating {
		t.Errorf("base ledger channel = %+v", c)
	}
	var keys []string
	err = s.PostedKeys(ctx, pass, "C1", func(ts string, edited *string) error {
		k := ts
		if edited != nil {
			k += "=" + *edited
		}
		keys = append(keys, k)
		return nil
	})
	if err != nil || !slices.Equal(keys, []string{"1.1=1700000000.000100", "2.2="}) {
		t.Fatalf("PostedKeys = %v, %v", keys, err)
	}

	// Once the pass has planned, the base is over: its staging can go, its
	// chats (it has none here) are the pass's to complete.
	runs, err := s.Purgeable(ctx)
	if err != nil || len(runs) != 1 || runs[0].ID != base {
		t.Fatalf("Purgeable after the plan = %+v, %v", runs, err)
	}
	if err := s.MarkPurged(ctx, base); err != nil {
		t.Fatalf("MarkPurged(base): %v", err)
	}
	if runs, _ := s.Purgeable(ctx); len(runs) != 0 {
		t.Fatalf("Purgeable after the purge = %+v", runs)
	}
	// The pass itself is not purgeable: it is running.
	wantErr(t, s.MarkPurged(ctx, pass), store.ErrNotFinalized)
}

func TestCarryForwardRefusals(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	base := basePass(t, s, "acme")

	plan := func(runID string, p store.Plan) error {
		l := mustClaim(t, s, runID, "planner", store.StagePlan)
		defer release(t, s, l)
		return s.Fenced(ctx, l, func(tx store.Tx) error { return tx.ApplyPlan(ctx, p) })
	}

	// A first pass cannot carry from anything.
	first := newRun(t, s, "other")
	p := carriedPlan(base)
	wantErr(t, plan(first, p), store.ErrInvalidPlan)

	pass := newPass(t, s, "acme", base)
	// The plan must carry from the run's own base, and must say so.
	p = carriedPlan("another-run")
	wantErr(t, plan(pass, p), store.ErrInvalidPlan)
	p = carriedPlan(base)
	p.CarryFrom = ""
	wantErr(t, plan(pass, p), store.ErrInvalidPlan)
	// A carried conversation is one that exists.
	p = carriedPlan(base)
	p.Channels[0].TeamsID = ""
	wantErr(t, plan(pass, p), store.ErrInvalidPlan)

	// Only a base that finished importing is carried from. (While a pass
	// is active its base cannot become active again: one active run per
	// name. A base that is no longer finished can only have been
	// cancelled.)
	exec(t, s, `UPDATE runs SET status = 'cancelled' WHERE id = $1`, base)
	wantErr(t, plan(pass, carriedPlan(base)), store.ErrBaseNotFinished)
	if n := scalar[int](t, s, `SELECT count(*) FROM map_channels WHERE run_id = $1`, pass); n != 0 {
		t.Fatalf("a refused plan left %d channels behind", n)
	}
	exec(t, s, `UPDATE runs SET status = 'finalized' WHERE id = $1`, base)
	if err := plan(pass, carriedPlan(base)); err != nil {
		t.Fatalf("plan from a finalized base: %v", err)
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM map_messages WHERE run_id = $1 AND carried`, pass); n != 3 {
		t.Fatalf("carried %d posts from a finalized base, want 3", n)
	}
}

// A database migrated from the schema before passes keeps working: every
// run is pass 1, nothing is carried, and rows written before the edit
// marker existed say that it is not known.
func TestPassesMigration(t *testing.T) {
	t.Parallel()
	url, schema := storetest.NewSchema(t)
	s, err := store.Open(t.Context(), url, store.WithSchema(schema))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ctx := t.Context()
	if err := store.MigrateTo(ctx, s, 4); err != nil {
		t.Fatal(err)
	}
	exec(t, s, `INSERT INTO runs (id, name, config, config_hash, status, planned_at) VALUES ('r1', 'acme', '', 'h', 'completed', now())`)
	exec(t, s, `INSERT INTO map_teams (run_id, idx, name) VALUES ('r1', 0, 'T')`)
	exec(t, s, `INSERT INTO map_channels (run_id, slack_id, slack_name, kind, team_index, state, teams_id) VALUES ('r1', 'C1', 'general', 'public', 0, 'migrating', 'ch-1')`)
	exec(t, s, `INSERT INTO map_messages (run_id, channel_id, slack_ts, part, target_ms, hash, state, teams_id) VALUES ('r1', 'C1', '1.1', 0, 1000, 'h', 'posted', '1000')`)

	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM schema_migrations`); n != schemaVersion {
		t.Fatalf("schema_migrations has %d rows, want %d", n, schemaVersion)
	}
	r, err := s.GetRun(ctx, "r1")
	if err != nil || r.Pass != 1 || r.Incremental() || r.WorkspaceID != "" {
		t.Fatalf("run after the migration = %+v, %v", r, err)
	}
	m, err := s.GetMessage(ctx, "r1", store.MessageKey{ChannelID: "C1", SlackTS: "1.1"})
	if err != nil || m.Carried || m.Edited != nil {
		t.Fatalf("message after the migration = %+v, %v", m, err)
	}
	p, err := s.Progress(ctx, "r1")
	if err != nil || p.Posts != 1 || p.Posted != 1 || p.Carried != 0 {
		t.Fatalf("progress after the migration = %+v, %v", p, err)
	}
	// It can be the base of a pass.
	id := store.NewRunID()
	if err := s.CreateRun(ctx, store.Run{ID: id, Name: "acme", BaseRunID: "r1"}); err != nil {
		t.Fatalf("a pass after a migrated run: %v", err)
	}
}

package store_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/store"
)

func TestApplyPlanValidation(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := newRun(t, s, "acme")
	l := mustClaim(t, s, id, "planner")

	cases := []struct {
		name   string
		mutate func(p *store.Plan)
		want   string
	}{
		{"seq gap", func(p *store.Plan) {
			for i := range p.Batches {
				if p.Batches[i].LaneID == "extract:C1" && p.Batches[i].Seq == 1 {
					p.Batches[i].Seq = 5
				}
			}
		}, "no gaps"},
		{"seq does not start at 0", func(p *store.Plan) {
			for i := range p.Batches {
				if p.Batches[i].LaneID == "team:0" {
					p.Batches[i].Seq = 1
				}
			}
		}, "no gaps"},
		{"duplicate batch", func(p *store.Plan) {
			p.Batches = append(p.Batches, store.Batch{LaneID: "extract:C1", Seq: 0})
		}, "appears twice"},
		{"negative seq", func(p *store.Plan) {
			p.Batches = append(p.Batches, store.Batch{LaneID: "extract:C1", Seq: -1})
		}, "negative"},
		{"batch for unknown lane", func(p *store.Plan) {
			p.Batches = append(p.Batches, store.Batch{LaneID: "extract:NOPE", Seq: 0})
		}, "not in the plan"},
		{"dependency on unknown batch", func(p *store.Plan) {
			p.Batches[0].DependsOn = []store.BatchRef{{LaneID: "extract:C1", Seq: 99}}
		}, "depends on extract:C1/99"},
		{"dependency on unknown lane", func(p *store.Plan) {
			p.Batches[0].DependsOn = []store.BatchRef{{LaneID: "ghost", Seq: 0}}
		}, "depends on ghost/0"},
		{"self dependency", func(p *store.Plan) {
			p.Batches[0].DependsOn = []store.BatchRef{{LaneID: p.Batches[0].LaneID, Seq: p.Batches[0].Seq}}
		}, "own lane"},
		{"duplicate lane", func(p *store.Plan) {
			p.Lanes = append(p.Lanes, store.Lane{ID: "team:0", Stage: store.StageTeam})
		}, "lane team:0 appears twice"},
		{"redefines plan lane", func(p *store.Plan) {
			p.Lanes = append(p.Lanes, store.Lane{ID: "plan", Stage: store.StagePlan})
		}, "plan lane"},
		{"unknown stage", func(p *store.Plan) { p.Lanes[0].Stage = "polish" }, "unknown stage"},
		{"lane without ID", func(p *store.Plan) { p.Lanes[0].ID = "" }, "no ID"},
		{"lane not open", func(p *store.Plan) { p.Lanes[0].Status = store.LaneDone }, "must be open"},
		{"batch not pending", func(p *store.Plan) { p.Batches[0].Status = store.BatchDone }, "must be pending"},
		{"lane of another run", func(p *store.Plan) { p.Lanes[0].RunID = "other" }, "belongs to run"},
		{"duplicate user", func(p *store.Plan) { p.Users = append(p.Users, p.Users[0]) }, "user U1 appears twice"},
		{"duplicate team", func(p *store.Plan) { p.Teams = append(p.Teams, p.Teams[0]) }, "team index 0 appears twice"},
		{"duplicate channel", func(p *store.Plan) { p.Channels = append(p.Channels, p.Channels[0]) }, "channel C1 appears twice"},
		{"channel in unknown team", func(p *store.Plan) { p.Channels[0].TeamIndex = 7 }, "team index 7"},
		{"bad totals", func(p *store.Plan) { p.Totals = []byte("{nope") }, "not valid JSON"},
		{"bad cursor", func(p *store.Plan) { p.Lanes[0].Cursor = []byte("[") }, "not valid JSON"},
	}
	for _, tc := range cases {
		p := testPlan([]string{"C1", "C2"}, 3)
		tc.mutate(&p)
		err := s.Fenced(ctx, l, func(tx store.Tx) error { return tx.ApplyPlan(ctx, p) })
		if !errors.Is(err, store.ErrInvalidPlan) {
			t.Errorf("%s: error = %v, want ErrInvalidPlan", tc.name, err)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not mention %q", tc.name, err, tc.want)
		}
	}

	// Nothing was written by any of the rejected plans.
	for _, table := range []string{"map_users", "map_teams", "map_channels", "batch_deps"} {
		if n := scalar[int](t, s, `SELECT count(*) FROM `+table); n != 0 {
			t.Errorf("%s has %d rows after rejected plans", table, n)
		}
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM lanes`); n != 1 {
		t.Errorf("%d lanes after rejected plans, want only the plan lane", n)
	}
	if st := runStatus(t, s, id); st != store.RunQueued {
		t.Errorf("status = %s, want queued", st)
	}

	// The lease is still good and a valid plan applies.
	if err := s.Fenced(ctx, l, func(tx store.Tx) error { return tx.ApplyPlan(ctx, testPlan([]string{"C1", "C2"}, 3)) }); err != nil {
		t.Fatalf("valid plan: %v", err)
	}
}

func TestApplyPlanStoresEverythingAndIsIdempotent(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := newRun(t, s, "acme")
	p := testPlan([]string{"C1", "C2"}, 2)
	p.Lanes = append(p.Lanes, store.Lane{ID: "extract:EMPTY", Stage: store.StageExtract, Key: "EMPTY", Cursor: []byte(`{"a": 1}`)})
	// A duplicate dependency edge is tolerated.
	p.Batches[1].DependsOn = append(p.Batches[1].DependsOn, p.Batches[1].DependsOn...)
	ms := time.Date(2019, 5, 6, 7, 8, 9, 123456000, time.FixedZone("x", 3600))
	p.Channels[1].MigrationStart = &ms
	p.Channels[1].Created = time.Time{}

	l := mustClaim(t, s, id, "planner")
	if err := s.Fenced(ctx, l, func(tx store.Tx) error { return tx.ApplyPlan(ctx, p) }); err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	r, _ := s.GetRun(ctx, id)
	if r.Status != store.RunRunning {
		t.Fatalf("status = %s", r.Status)
	}

	users, err := s.Users(ctx, id)
	if err != nil || len(users) != 2 || users[0] != p.Users[0] || users[1] != p.Users[1] {
		t.Fatalf("Users = %+v, %v", users, err)
	}
	teams, err := s.Teams(ctx, id)
	if err != nil || len(teams) != 1 || teams[0] != p.Teams[0] {
		t.Fatalf("Teams = %+v, %v", teams, err)
	}
	chans, err := s.Channels(ctx, id)
	if err != nil || len(chans) != 2 {
		t.Fatalf("Channels = %+v, %v", chans, err)
	}
	c1 := chans[0]
	if c1.SlackID != "C1" || c1.SlackName != "chan-C1" || c1.Kind != "public" || c1.TargetName != "Chan C1" || !c1.IsGeneral ||
		c1.State != store.ChannelPlanned || c1.TeamsID != "" || c1.MigrationStart != nil ||
		!c1.Created.Equal(time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)) || c1.Created.Location() != time.UTC {
		t.Fatalf("channel C1 = %+v", c1)
	}
	c2, err := s.Channel(ctx, id, "C2")
	if err != nil || !c2.Created.IsZero() || c2.MigrationStart == nil || !c2.MigrationStart.Equal(ms) || c2.MigrationStart.Location() != time.UTC {
		t.Fatalf("channel C2 = %+v, %v", c2, err)
	}
	_, err = s.Channel(ctx, id, "nope")
	wantErr(t, err, store.ErrNotFound)

	// 1 plan + 1 team + 2*3 + the empty lane.
	if n := scalar[int](t, s, `SELECT count(*) FROM lanes WHERE run_id = $1`, id); n != 9 {
		t.Fatalf("%d lanes, want 9", n)
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM batches WHERE run_id = $1`, id); n != 1+1+2*(2+2+3) {
		t.Fatalf("%d batches", n)
	}
	if st := scalar[string](t, s, `SELECT status FROM lanes WHERE run_id = $1 AND id = 'extract:EMPTY'`, id); st != "done" {
		t.Fatalf("a lane without batches is %s, want done", st)
	}
	if cur := scalar[string](t, s, `SELECT cursor::text FROM lanes WHERE run_id = $1 AND id = 'extract:EMPTY'`, id); cur != `{"a": 1}` {
		t.Fatalf("cursor = %s", cur)
	}
	b, err := s.GetBatch(ctx, id, store.BatchRef{LaneID: "load:C1", Seq: 2})
	if err != nil {
		t.Fatal(err)
	}
	if b.Key != "2024-03-02" || len(b.DependsOn) != 1 || b.DependsOn[0] != (store.BatchRef{LaneID: "transform:C1", Seq: 1}) {
		t.Fatalf("load:C1/2 = %+v", b)
	}

	pr, err := s.Progress(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if string(pr.Totals) != `{"channels": 2}` {
		t.Fatalf("Totals = %s", pr.Totals)
	}

	// A second ApplyPlan, even with a different plan, changes nothing.
	other := testPlan([]string{"X9"}, 1)
	if err := s.Fenced(ctx, l, func(tx store.Tx) error { return tx.ApplyPlan(ctx, other) }); err != nil {
		t.Fatalf("second ApplyPlan: %v", err)
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM lanes WHERE run_id = $1`, id); n != 9 {
		t.Fatalf("%d lanes after second ApplyPlan, want 9", n)
	}
	if chans, _ := s.Channels(ctx, id); len(chans) != 2 {
		t.Fatalf("channels changed by second ApplyPlan: %+v", chans)
	}

	// ApplyPlan is refused outside the plan lane.
	complete(t, s, l, 0)
	release(t, s, l)
	el := mustClaim(t, s, id, "h", store.StageExtract)
	err = s.Fenced(ctx, el, func(tx store.Tx) error { return tx.ApplyPlan(ctx, other) })
	if err == nil || !strings.Contains(err.Error(), "plan lane") {
		t.Fatalf("ApplyPlan from an extract lane: %v", err)
	}
}

// A failed ApplyPlan transaction (fn returns an error after ApplyPlan
// succeeded) leaves nothing behind, and the plan can be applied afterwards.
func TestApplyPlanRollsBackWithTransaction(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := newRun(t, s, "acme")
	l := mustClaim(t, s, id, "planner")
	boom := errors.New("boom")
	err := s.Fenced(ctx, l, func(tx store.Tx) error {
		if err := tx.ApplyPlan(ctx, testPlan([]string{"C1"}, 2)); err != nil {
			return err
		}
		return boom
	})
	if err != boom {
		t.Fatalf("Fenced = %v, want the function's error", err)
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM lanes`); n != 1 {
		t.Fatalf("%d lanes after rollback", n)
	}
	if st := runStatus(t, s, id); st != store.RunQueued {
		t.Fatalf("status = %s after rollback", st)
	}
	release(t, s, l)
	applyPlan(t, s, id, testPlan([]string{"C1"}, 2))
}

func TestApplyLargePlan(t *testing.T) {
	s := open(t)
	ctx := t.Context()
	id := newRun(t, s, "big")

	// 300 lanes x 200 batches; every transform batch depends on its
	// extract batch.
	const pairs, per = 150, 200
	p := store.Plan{Teams: []store.TeamMapping{{Index: 0, Name: "T"}}}
	for c := range pairs {
		ch := fmt.Sprintf("C%04d", c)
		p.Channels = append(p.Channels, store.ChannelMapping{SlackID: ch, SlackName: ch, TeamIndex: 0})
		p.Lanes = append(p.Lanes,
			store.Lane{ID: "extract:" + ch, Stage: store.StageExtract, Key: ch},
			store.Lane{ID: "transform:" + ch, Stage: store.StageTransform, Key: ch})
		for i := range per {
			p.Batches = append(p.Batches,
				store.Batch{LaneID: "extract:" + ch, Seq: i, Key: fmt.Sprint(i)},
				store.Batch{LaneID: "transform:" + ch, Seq: i, Key: fmt.Sprint(i), DependsOn: []store.BatchRef{{LaneID: "extract:" + ch, Seq: i}}})
		}
	}
	l := mustClaim(t, s, id, "planner")
	start := time.Now()
	err := s.Fenced(ctx, l, func(tx store.Tx) error {
		if err := tx.ApplyPlan(ctx, p); err != nil {
			return err
		}
		return tx.CompleteBatch(ctx, 0, "", 0, nil)
	})
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}
	took := time.Since(start)
	t.Logf("applied %d lanes, %d batches, %d dependencies in %v", len(p.Lanes), len(p.Batches), pairs*per, took)
	if took > 20*time.Second {
		t.Errorf("ApplyPlan took %v", took)
	}
	release(t, s, l)
	if n := scalar[int](t, s, `SELECT count(*) FROM batches WHERE run_id = $1`, id); n != 2*pairs*per+1 {
		t.Fatalf("%d batches", n)
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM batch_deps WHERE run_id = $1`, id); n != pairs*per {
		t.Fatalf("%d deps", n)
	}

	// Claim stays quick with the full ledger: here all 150 transform
	// lanes are open but blocked, the worst case for the scan.
	exec(t, s, `ANALYZE lanes`)
	exec(t, s, `ANALYZE batches`)
	exec(t, s, `ANALYZE batch_deps`)
	start = time.Now()
	const claims = 50
	for range claims {
		noClaim(t, s, id, store.StageTransform, store.StageLoad)
	}
	blocked := time.Since(start) / claims
	start = time.Now()
	var leases []*store.Lease
	for range claims {
		leases = append(leases, mustClaim(t, s, id, "h", store.StageExtract, store.StageTransform))
	}
	claimed := time.Since(start) / claims
	t.Logf("Claim: %v when everything is blocked, %v when claiming", blocked, claimed)
	if blocked > 250*time.Millisecond || claimed > 250*time.Millisecond {
		t.Errorf("Claim is slow: blocked %v, claiming %v", blocked, claimed)
	}
	for _, l := range leases {
		if l.Stage != store.StageExtract {
			t.Fatalf("claimed %s while its dependencies are pending", l.LaneID)
		}
	}
	start = time.Now()
	if _, err := s.Progress(ctx, id); err != nil {
		t.Fatal(err)
	}
	t.Logf("Progress: %v", time.Since(start))
}

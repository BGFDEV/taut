package worker_test

import (
	"context"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/fakegraph"
	"github.com/bgfdev/taut/internal/fixtures"
	"github.com/bgfdev/taut/internal/store"
)

// A small load fixture goes through the whole pipeline against a fake that
// answers with some latency: every message of the export is in the tenant
// exactly once, counted the way `make load-test` counts it. It keeps the
// generator, the fake's load modes and the summary a load test verifies by
// from going stale between load tests.
func TestLoadFixtureSmoke(t *testing.T) {
	const fixture = "load:channels=8,messages=900,users=40,days=30,threads=0.25,files=0.02,private=0.25,dms=5,dmmessages=90"
	r := newHarnessWith(t, fixture, func(m *config.Config) {
		m.Metadata.Name = "it-load"
		withPrivateChannels(m)
		withDMs(m)
	}, fakegraph.WithLatency(200*time.Microsecond, 300*time.Microsecond), fakegraph.WithOpaqueMessageIDs())
	// The export's files are served by ID, as the local stack serves them.
	facts := fixtures.Facts(fixture)
	if facts.Messages != 990 || facts.DMs != 5 || facts.MPIMs != 1 || facts.PrivateChannels == 0 || facts.Files == 0 {
		t.Fatalf("facts = %+v", facts)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := r.worker(ctx, 5*time.Second)
	defer stop()
	stop2 := r.worker(ctx, 5*time.Second)
	defer stop2()
	run := r.start(ctx)
	v := r.await(ctx, run.ID, 2*time.Minute)
	if v.Status != store.RunCompleted {
		t.Fatalf("run %s: %+v", v.Status, v.Progress.Problems)
	}
	p := v.Progress
	if p.Posts != p.Posted+p.Failed+p.Held || p.Posted == 0 {
		t.Fatalf("posts %d, posted %d, failed %d, held %d", p.Posts, p.Posted, p.Failed, p.Held)
	}

	// What the ledger says was imported is what the tenant holds, per
	// conversation and in all, with one message per millisecond.
	sum := r.fake.Summary()
	if sum.Stats.ImportsAccepted != p.Posted {
		t.Errorf("the tenant accepted %d imports, the ledger records %d", sum.Stats.ImportsAccepted, p.Posted)
	}
	inTenant := map[string]fakegraph.ConversationSummary{}
	for _, c := range append(sum.Channels, sum.Chats...) {
		if c.DistinctMillis != c.Messages {
			t.Errorf("%s%s holds %d messages on %d milliseconds", c.ChannelID, c.ChatID, c.Messages, c.DistinctMillis)
		}
		inTenant[c.ChannelID+c.ChatID] = c
	}
	chans, err := r.store.Channels(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]store.ChannelMapping{}
	for _, c := range chans {
		byID[c.SlackID] = c
	}
	imported, chats := 0, 0
	for _, c := range p.Channels {
		m := byID[c.SlackID]
		got, ok := inTenant[m.TeamsID]
		if !ok || got.Imported != c.Posted {
			t.Errorf("%s: the ledger records %d posts, the tenant holds %d (%v)", c.SlackID, c.Posted, got.Imported, ok)
		}
		imported += c.Posted
		if store.IsChatKind(c.Kind) {
			chats++
		}
		// Nothing of the export is missing: every message became at least
		// one post, and none failed.
		if want := facts.PerChannel[c.SlackID].Messages; c.Posts < want || c.Failed+c.Held != 0 {
			t.Errorf("%s: %d posts for %d messages, %d failed, %d held", c.SlackID, c.Posts, want, c.Failed, c.Held)
		}
	}
	if imported != p.Posted || len(p.Channels) != facts.Channels || chats != 6 {
		t.Errorf("%d conversations (%d chats) with %d posts; facts %d channels, ledger %d posted", len(p.Channels), chats, imported, facts.Channels, p.Posted)
	}
	if p.FilesUploaded == 0 || p.FilesFailed != 0 {
		t.Errorf("files: %d uploaded, %d failed", p.FilesUploaded, p.FilesFailed)
	}
}

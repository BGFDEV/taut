package worker_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bgfdev/taut/internal/fakegraph"
	"github.com/bgfdev/taut/internal/graph"
	"github.com/bgfdev/taut/internal/sidecar/budget"
	"github.com/bgfdev/taut/internal/sidecar/egressproxy"
	"github.com/bgfdev/taut/internal/sidecar/tokenbroker"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/worker"
)

// pod is a worker with its two sidecars, as it runs in production: the
// worker has no credentials and reaches Microsoft and Slack only through
// its egress proxy, which gets tokens from the token broker over a Unix socket.
type pod struct {
	r        *harness
	name     string
	socket   string
	govAddr  string
	mu       sync.Mutex
	stopGov  func()
	stopHand func()
}

// newPod starts a token broker, an egress proxy and a worker.
func (r *harness) newPod(ctx context.Context, name string, ttl time.Duration) *pod {
	r.t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Token broker: the only component that can read the client secret.
	// Unix socket paths are short, so use a short temp dir of our own.
	sockDir, err := os.MkdirTemp("", "tt")
	if err != nil {
		r.t.Fatal(err)
	}
	r.t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	socket := filepath.Join(sockDir, "s", "token.sock")
	srv, err := tokenbroker.Listen(tokenbroker.Config{
		Socket: socket, AllowEndpoints: []string{r.graph}, Log: log,
		// The broker serves only the tenant it was told to.
		AllowTenants: []string{fakegraph.DefaultTenantID}, AllowClients: []string{fakegraph.DefaultClientID},
	})
	if err != nil {
		r.t.Fatal(err)
	}
	go func() { _ = srv.Serve(ctx) }()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		r.t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	p := &pod{r: r, name: name, socket: socket, govAddr: addr}
	p.startProxy(ctx)
	p.startWorker(ctx, ttl)
	return p
}

func (p *pod) startProxy(ctx context.Context) {
	p.r.t.Helper()
	g, err := egressproxy.New(egressproxy.Config{
		Holder:  p.name + "-egress proxy",
		Tokens:  tokenbroker.NewClient(p.socket),
		Budgets: budget.Remote(p.r.bus),
		Bus:     p.r.bus,
		// The fakes stand in for Graph, SharePoint and Slack's file host.
		AllowHosts: []string{"graph=" + p.r.graph, p.r.filesURL},
		// Fast enough that the test is not about waiting, slow enough that
		// admission control is really exercised.
		ChannelStartRPS: 400, ChannelMaxRPS: 800,
		// Leases above the egress proxy's own ceiling are clamped; raise it to
		// match the budgets this test's control plane hands out.
		MaxRPS: map[string]float64{"graph": 3000, "sharepoint": 3000, "slack": 3000},
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		p.r.t.Fatal(err)
	}
	var ln net.Listener
	for i := 0; ; i++ {
		ln, err = net.Listen("tcp", p.govAddr)
		if err == nil {
			break
		}
		if i > 200 {
			p.r.t.Fatalf("egress proxy could not rebind %s: %v", p.govAddr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	gctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = g.ServeListener(gctx, ln) }()
	p.stopGov = func() { cancel(); <-done; g.Close() }
}

func (p *pod) startWorker(ctx context.Context, ttl time.Duration) {
	p.r.t.Helper()
	route := worker.Proxied{
		URL: "http://" + p.govAddr, SlackFilesBase: p.r.filesURL,
		Retry: graph.RetryPolicy{MaxAttempts: 6, MaxTotalWait: time.Hour, BaseDelay: time.Millisecond, MaxDelay: 5 * time.Millisecond},
		Sleep: func(ctx context.Context, _ time.Duration) error {
			// Give a restarting egress proxy a moment instead of spinning.
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Millisecond):
				return nil
			}
		},
	}
	dctx, cancel := context.WithCancel(ctx)
	d, err := worker.New(worker.Config{
		Holder: fmt.Sprintf("%s-%d", p.name, p.r.seq.Add(1)),
		Stages: []string{store.StagePlan, store.StageExtract, store.StageTransform, store.StageTeam, store.StageFiles, store.StageLoad, store.StageFinalize, store.StageMembers, store.StageRelease},
		Store:  p.r.store, Blobs: p.r.blobs, KEK: p.r.kek, Bus: p.r.bus,
		Graph: route.GraphFactory(), Slack: route.SlackFiles(),
		Concurrency: 4, LeaseTTL: ttl, Poll: 25 * time.Millisecond,
		// The egress proxy sets the pace; the worker's own limits step aside.
		ChannelRPS: 1e6, TenantRPS: 1e6,
		ScratchDir: filepath.Join(p.r.dir, fmt.Sprintf("scratch-%s-%d", p.name, p.r.seq.Load())),
	})
	if err != nil {
		p.r.t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); _ = d.Run(dctx) }()
	p.stopHand = func() { cancel(); <-done }
}

func (p *pod) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopHand()
	p.stopGov()
}

// TestMigrateThroughSidecars imports the medium fixture with every worker
// behind its egress proxy and token broker, while workers AND egress proxies are
// killed and restarted at random and the fake Graph misbehaves. The worker
// processes hold no credential; the result must still be exact, files
// included.
func TestMigrateThroughSidecars(t *testing.T) {
	if testing.Short() {
		t.Skip("skipped in -short mode")
	}
	r := newHarness(t, "medium", fakegraph.WithRandSeed(5))
	ctx := t.Context()

	// The control plane hands out the tenant-wide budget; egress proxies lease
	// their share of it over the bus.
	totals := budget.DefaultTotals()
	totals["graph"], totals["sharepoint"], totals["slack"] = 3000, 3000, 3000
	alloc := budget.NewAllocator(totals, 2*time.Second, time.Now)
	// A lease lasts two seconds, so a snapshot taken after the run can be
	// empty on a slow machine. Watch for one while the run is going.
	var leased atomic.Bool
	watchCtx, stopWatch := context.WithCancel(context.Background())
	defer stopWatch()
	go func() {
		for watchCtx.Err() == nil {
			if len(alloc.Snapshot()) > 0 {
				leased.Store(true)
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
	stopAlloc, err := alloc.Serve(r.bus)
	if err != nil {
		t.Fatal(err)
	}
	defer stopAlloc()

	faults := r.fake.Faults()
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/messages", Action: fakegraph.ActionFailAfterCommit, Probability: 0.02})
	faults.MustAdd(fakegraph.Fault{Method: "POST", Path: "/v1.0/teams/*/channels/*/messages/*/replies", Action: fakegraph.ActionFailAfterCommit, Probability: 0.02})
	// No injected 429s here: the egress proxy answers each one by slowing the
	// channel down for real, which is tested in its own package and would
	// only make this test about waiting.
	faults.MustAdd(fakegraph.Fault{Method: "PUT", Path: "/_upload/**", Action: fakegraph.ActionFailAfterCommit, Probability: 0.05})
	r.files.FailEvery(9)

	pods := []*pod{r.newPod(ctx, "pod-a", time.Second), r.newPod(ctx, "pod-b", time.Second)}
	run := r.start(ctx)

	stop := make(chan struct{})
	chaosDone := make(chan struct{})
	handKills, govKills := 0, 0
	go func() {
		defer close(chaosDone)
		rng := rand.New(rand.NewPCG(21, 34))
		for {
			select {
			case <-stop:
				return
			case <-time.After(time.Duration(150+rng.IntN(250)) * pace * time.Millisecond):
			}
			p := pods[rng.IntN(len(pods))]
			p.mu.Lock()
			if rng.IntN(3) == 0 {
				// The egress proxy dies. Its worker's requests fail until it
				// is back; nothing may be lost or repeated because of it.
				p.stopGov()
				time.Sleep(time.Duration(rng.IntN(80)) * time.Millisecond)
				p.startProxy(ctx)
				govKills++
			} else {
				p.stopHand()
				p.startWorker(ctx, time.Second)
				handKills++
			}
			p.mu.Unlock()
		}
	}()

	v := r.await(ctx, run.ID, 6*time.Minute)
	close(stop)
	<-chaosDone
	for _, p := range pods {
		p.stop()
	}

	r.verify(v, r.expected())
	stats := r.fake.Stats()
	t.Logf("workers killed: %d; egress proxies killed: %d; imports accepted: %d; adopted: %d; files uploaded: %d",
		handKills, govKills, stats.ImportsAccepted, v.Progress.Adopted, v.Progress.FilesUploaded)
	if handKills < 3 || govKills < 2 {
		t.Errorf("too little chaos to prove anything: %d worker and %d egress proxy kills", handKills, govKills)
	}
	if stats.ImportsAccepted != v.Progress.Posted {
		t.Errorf("Graph accepted %d imports for %d posts", stats.ImportsAccepted, v.Progress.Posted)
	}
	if v.Progress.FilesFailed != 0 {
		t.Errorf("%d files failed", v.Progress.FilesFailed)
	}
	// Every request the fake Graph served arrived with a valid token, yet no
	// worker was ever given one: they can only have come from the token
	// broker by way of an egress proxy.
	if !leased.Load() {
		t.Error("no egress proxy ever leased a budget from the control plane")
	}
}

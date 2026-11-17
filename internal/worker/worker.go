// Package worker is Taut's worker runtime.
//
// A worker claims lanes from the store and works through their batches.
// It is stateless: everything it knows about a run comes from the store and
// from staging, so any number of workers can run, and any of them can be
// killed at any moment. The guarantees that make that safe are in the store
// (leases and fencing) and in the load stage (Graph's timestamp uniqueness
// used as an idempotency key); this file is the loop that drives them.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bgfdev/taut/internal/bus"
	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/graph"
	"github.com/bgfdev/taut/internal/pipeline"
	"github.com/bgfdev/taut/internal/slack/export"
	"github.com/bgfdev/taut/internal/staging"
	"github.com/bgfdev/taut/internal/store"
)

// Graph is the part of Microsoft Graph a worker uses. *graph.Client
// implements it.
type Graph interface {
	Users(ctx context.Context, q graph.UserQuery) iter.Seq2[graph.User, error]
	FindTeams(ctx context.Context, displayName string) ([]graph.Team, error)
	CreateTeam(ctx context.Context, spec graph.TeamSpec) (string, error)
	GetTeam(ctx context.Context, teamID string) (graph.Team, error)
	FindChannel(ctx context.Context, teamID, displayName string) (graph.Channel, bool, error)
	CreateChannel(ctx context.Context, teamID string, spec graph.ChannelSpec) (graph.Channel, error)
	GetChannel(ctx context.Context, teamID, channelID string) (graph.Channel, error)
	StartMigration(ctx context.Context, teamID, channelID string, conversationCreation *time.Time) error
	CompleteChannelMigration(ctx context.Context, teamID, channelID string) error
	CompleteTeamMigration(ctx context.Context, teamID string) error
	ImportMessage(ctx context.Context, target graph.ImportTarget, msg graph.ImportMessage) (graph.ChatMessage, error)
	GetMessage(ctx context.Context, teamID, channelID, messageID string) (graph.ChatMessage, error)
	GetReply(ctx context.Context, teamID, channelID, parentID, replyID string) (graph.ChatMessage, error)
	FindMessageAt(ctx context.Context, target graph.ImportTarget, createdAt time.Time) (graph.ChatMessage, bool, error)
	AddTeamMember(ctx context.Context, teamID, userID string, owner bool) error
	AddTeamMembers(ctx context.Context, teamID string, userIDs []string) ([]graph.MemberResult, error)
	TeamMembers(ctx context.Context, teamID string) iter.Seq2[graph.TeamMember, error]
	ChannelMembers(ctx context.Context, teamID, channelID string) iter.Seq2[graph.ChannelMember, error]
	AddChannelMember(ctx context.Context, teamID, channelID, userID string, owner bool) error
	FilesFolder(ctx context.Context, teamID, channelID string) (graph.DriveItem, error)
	EnsureFolder(ctx context.Context, driveID, parentID, name string) (graph.DriveItem, error)
	Upload(ctx context.Context, spec graph.UploadSpec) (graph.DriveItem, error)

	// Chats, for direct messages and group DMs.
	CreateChat(ctx context.Context, spec graph.ChatSpec) (graph.Chat, error)
	GetChat(ctx context.Context, chatID string) (graph.Chat, error)
	FindGroupChats(ctx context.Context, userID string, memberIDs []string, since time.Time) ([]graph.Chat, error)
	StartChatMigration(ctx context.Context, chatID string, conversationCreation *time.Time) error
	CompleteChatMigration(ctx context.Context, chatID string) error
	GetChatMessage(ctx context.Context, chatID, messageID string) (graph.ChatMessage, error)
	ChatMembers(ctx context.Context, chatID string) ([]graph.ChatMember, error)
	ChatIsEmpty(ctx context.Context, chatID string) (bool, error)
}

// GraphFactory builds a Graph client for a run's config, resolving the
// config file's secret references in the worker's own environment.
type GraphFactory func(ctx context.Context, m *config.Config) (Graph, error)

// Config configures a worker.
type Config struct {
	// Holder identifies this worker in leases and the audit log. It must be
	// unique among running workers.
	Holder string
	Stages []string
	Store  store.Store
	// Blobs is the raw staging backend; KEK unwraps each run's data key.
	Blobs staging.Blobs
	KEK   staging.KEK
	Bus   bus.Bus
	Graph GraphFactory
	// Slack fetches file content. Nil means files cannot be moved; file
	// batches then fail and are retried until a worker that can is
	// available.
	Slack SlackFiles
	Log   *slog.Logger
	// Observer receives telemetry. Nil means none.
	Observer Observer

	// Concurrency is how many lanes are worked at once. Default 4.
	Concurrency int
	// LeaseTTL is how long a lease lasts without renewal. Default 30s. A
	// killed worker's lanes become claimable again after this long.
	LeaseTTL time.Duration
	// Poll is how often an idle lane loop looks for work when no wake-up
	// arrives. Default 2s.
	Poll time.Duration
	// MaxAttempts is how many consecutive failed attempts a batch gets
	// before it is failed for an operator to look at. Default 8.
	MaxAttempts int
	// ChannelRPS and TenantRPS bound Graph message imports per channel and
	// across this process. Defaults 1 and 40. The tenant limit here is
	// per process; the egress proxy enforces the shared one.
	ChannelRPS float64
	TenantRPS  float64
	// FileIdleTimeout abandons a file download that delivers nothing for
	// this long. Default 90s.
	FileIdleTimeout time.Duration
	// FileRetryWait, when set, replaces the pause between in-place retries
	// of a download (tests).
	FileRetryWait time.Duration
	// FolderWait is how long to wait for a new channel's files folder to be
	// provisioned, polling every FolderPoll. Defaults 10m and 10s.
	FolderWait time.Duration
	FolderPoll time.Duration
	// ScratchDir holds a decrypted copy of each run's export while this
	// process works on it. Default: a private directory under os.TempDir.
	ScratchDir string
	// RunID restricts the worker to one run; empty means any.
	RunID string
}

func (c *Config) defaults() error {
	if c.Holder == "" || len(c.Stages) == 0 || c.Store == nil || c.Blobs == nil || c.KEK.IsZero() {
		return errors.New("worker: Holder, Stages, Store, Blobs and KEK are required")
	}
	if c.Bus == nil {
		c.Bus = bus.NewLocal()
	}
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
	if c.Observer == nil {
		c.Observer = noObserver{}
	}
	if c.Concurrency <= 0 {
		c.Concurrency = 4
	}
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = 30 * time.Second
	}
	if c.Poll <= 0 {
		c.Poll = 2 * time.Second
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = 8
	}
	if c.ChannelRPS <= 0 {
		c.ChannelRPS = 1
	}
	if c.TenantRPS <= 0 {
		c.TenantRPS = 40
	}
	if c.FileIdleTimeout <= 0 {
		c.FileIdleTimeout = 90 * time.Second
	}
	if c.FolderWait <= 0 {
		c.FolderWait = 10 * time.Minute
	}
	if c.FolderPoll <= 0 {
		c.FolderPoll = 10 * time.Second
	}
	return nil
}

// Worker is a running worker.
type Worker struct {
	cfg    Config
	tenant *limiter

	mu       sync.Mutex
	runs     map[string]*runState
	limiters map[string]*limiter // per load lane
	// refusals counts, per load lane, the posts in a row whose reactions
	// Graph refused (see refusedReactionsRun).
	refusals map[string]int
	// ownScratch is true when the worker created ScratchDir and should
	// remove it on exit.
	ownScratch bool
}

// New builds a worker.
func New(cfg Config) (*Worker, error) {
	if err := cfg.defaults(); err != nil {
		return nil, err
	}
	d := &Worker{cfg: cfg, runs: map[string]*runState{}, tenant: newLimiter(cfg.TenantRPS)}
	if d.cfg.ScratchDir == "" {
		dir, err := os.MkdirTemp("", "taut-worker-*")
		if err != nil {
			return nil, fmt.Errorf("worker: creating scratch directory: %w", err)
		}
		d.cfg.ScratchDir, d.ownScratch = dir, true
	}
	if err := os.MkdirAll(d.cfg.ScratchDir, 0o700); err != nil {
		return nil, fmt.Errorf("worker: creating scratch directory: %w", err)
	}
	return d, nil
}

// Run works until ctx is cancelled. On cancellation, lanes in hand are
// released so another worker can take them immediately rather than after
// the lease expires.
func (d *Worker) Run(ctx context.Context) error {
	defer d.cleanup()
	wake, stop := d.cfg.Bus.Subscribe()
	defer stop()

	// Each lane loop gets its own wake channel fed from the one subscription,
	// so a single signal can rouse all idle loops.
	kick := make([]chan struct{}, d.cfg.Concurrency)
	for i := range kick {
		kick[i] = make(chan struct{}, 1)
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-wake:
				for _, k := range kick {
					select {
					case k <- struct{}{}:
					default:
					}
				}
			}
		}
	}()

	// Forget runs that are over, and with them the decrypted copy of
	// their export on this worker's disk. The first pass also clears
	// what a previous process left behind when it was killed.
	d.sweep(ctx)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				d.sweep(ctx)
			}
		}
	}()

	var wg sync.WaitGroup
	for i := range d.cfg.Concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d.laneLoop(ctx, kick[i])
		}()
	}
	wg.Wait()
	return nil
}

func (d *Worker) laneLoop(ctx context.Context, kick <-chan struct{}) {
	for ctx.Err() == nil {
		lease, err := d.cfg.Store.Claim(ctx, store.ClaimFilter{Stages: d.cfg.Stages, RunID: d.cfg.RunID}, d.cfg.Holder, d.cfg.LeaseTTL)
		if err != nil {
			if ctx.Err() == nil {
				d.cfg.Log.Warn("claim failed", "error", err)
			}
			lease = nil
		}
		if lease == nil {
			// Nothing to do. Sleep until woken or the poll interval
			// passes; jitter keeps idle loops from polling in step.
			wait := d.cfg.Poll + time.Duration(rand.Int64N(int64(d.cfg.Poll/4)+1))
			select {
			case <-ctx.Done():
				return
			case <-kick:
			case <-time.After(wait):
			}
			continue
		}
		d.work(ctx, lease)
	}
}

// permanentError marks a failure that retrying cannot fix.
type permanentError struct{ err error }

func (e *permanentError) Error() string { return e.err.Error() }
func (e *permanentError) Unwrap() error { return e.err }

// permanent wraps err so the batch is failed for an operator rather than
// retried.
func permanent(err error) error { return &permanentError{err} }

func isPermanent(err error) bool {
	var p *permanentError
	return errors.As(err, &p)
}

// notMineError marks a failure that is about this worker (it is the wrong
// version for the run, or lacks something it needs), not about the batch.
// The lane is handed back without the batch being charged an attempt, so a
// misconfigured worker cannot fail work that others could do.
type notMineError struct{ err error }

func (e *notMineError) Error() string { return e.err.Error() }
func (e *notMineError) Unwrap() error { return e.err }

func notMine(err error) error { return &notMineError{err} }

func isNotMine(err error) bool {
	var n *notMineError
	return errors.As(err, &n)
}

// progressError marks an attempt that failed after settling some of its
// items. Its work is recorded, so it is not charged as a failed attempt:
// otherwise a large batch would run out of attempts at an ordinary error
// rate even though every attempt moved it forward.
type progressError struct{ err error }

func (e *progressError) Error() string { return e.err.Error() }
func (e *progressError) Unwrap() error { return e.err }

// withProgress wraps err as a progressError if the attempt settled anything.
func withProgress(err error, settled int) error {
	if err == nil || settled == 0 || isPermanent(err) || isNotMine(err) || errors.Is(err, store.ErrFenced) {
		return err
	}
	return &progressError{err}
}

func madeProgress(err error) bool {
	var p *progressError
	return errors.As(err, &p)
}

// backoff is the delay before attempt n+1: exponential from two seconds,
// capped at two minutes, with jitter so retries do not synchronize.
func backoff(attempts int) time.Duration {
	d := 2 * time.Second << min(attempts, 6)
	if d > 2*time.Minute {
		d = 2 * time.Minute
	}
	return d + time.Duration(rand.Int64N(int64(d/4)+1))
}

// work processes a leased lane until it runs out of runnable batches, the
// lease is lost, or a batch fails.
func (d *Worker) work(parent context.Context, lease *store.Lease) {
	log := d.cfg.Log.With("run", lease.RunID, "lane", lease.LaneID, "fence", lease.Fence)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	// Heartbeat: renew the lease well inside its lifetime. If renewal
	// reports the lease lost, stop working at once. If renewal cannot be
	// confirmed for a whole lease lifetime (the database is unreachable,
	// say), stop as well: by then another worker may hold the lane, and
	// while the database writes of a stale holder are fenced, its calls to
	// Microsoft are not.
	//
	// The heartbeat renews through its own copy of the lease: Renew updates
	// the expiry in place, and the lane loop reads the lease concurrently.
	hb := *lease
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		t := time.NewTicker(d.cfg.LeaseTTL / 3)
		defer t.Stop()
		lastOK := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				rctx, rcancel := context.WithTimeout(ctx, d.cfg.LeaseTTL/3)
				err := d.cfg.Store.Renew(rctx, &hb, d.cfg.LeaseTTL)
				rcancel()
				switch {
				case err == nil:
					lastOK = time.Now()
				case errors.Is(err, store.ErrFenced):
					log.Warn("lease lost; another worker has the lane")
					cancel()
					return
				case ctx.Err() != nil:
					return
				case time.Since(lastOK) >= d.cfg.LeaseTTL:
					log.Warn("lease could not be renewed for its whole lifetime; stopping work on the lane", "error", err)
					cancel()
					return
				default:
					log.Warn("lease renewal failed", "error", err)
				}
			}
		}
	}()
	defer func() { cancel(); <-hbDone }()

	// release hands the lane back even when the worker is shutting down,
	// so it uses a context that outlives cancellation.
	release := func(retry time.Duration, reason string) {
		rctx, rcancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
		defer rcancel()
		if err := d.cfg.Store.Release(rctx, lease, retry, clip(reason)); err != nil {
			log.Warn("release failed; the lease will expire on its own", "error", err)
		}
	}

	for {
		if ctx.Err() != nil {
			release(0, "")
			return
		}
		batch, err := d.cfg.Store.NextBatch(ctx, lease)
		if err != nil {
			if !errors.Is(err, store.ErrFenced) && ctx.Err() == nil {
				log.Warn("reading next batch failed", "error", err)
				release(backoff(lease.Attempts), err.Error())
			} else if ctx.Err() != nil {
				release(0, "")
			}
			return
		}
		if batch == nil {
			release(0, "")
			return
		}

		start := time.Now()
		octx, observed := d.cfg.Observer.Batch(ctx, BatchInfo{
			RunID: lease.RunID, Lane: lease.LaneID, Stage: lease.Stage, Seq: batch.Seq,
			Holder: d.cfg.Holder, Attempt: lease.Attempts + 1,
		})
		err = d.safely(octx, lease, batch)
		observed(err)
		switch {
		case err == nil, parent.Err() != nil:
		case errors.Is(err, store.ErrFenced):
			d.cfg.Observer.Count(ctx, lease.Stage, EventLeaseLost, 1)
		case !isNotMine(err) && !madeProgress(err):
			d.cfg.Observer.Count(ctx, lease.Stage, EventBatchRetried, 1)
		}
		switch {
		case err == nil:
			log.Debug("batch done", "seq", batch.Seq, "key", batch.Key, "took", time.Since(start).Round(time.Millisecond))
			// Progress clears the lane's failure count in the store; keep
			// the lease's copy in step, or an old count would be charged
			// against the next batch.
			lease.Attempts = 0
			// Something downstream may now be runnable.
			_ = d.cfg.Bus.Wake(ctx)
		case errors.Is(err, store.ErrFenced):
			log.Warn("lease lost mid-batch; nothing was recorded past the fence", "seq", batch.Seq)
			return
		case parent.Err() != nil:
			// Shutting down: not a failure of the batch.
			release(0, "")
			return
		case isNotMine(err):
			log.Error("this worker cannot work the lane; handing it back", "seq", batch.Seq, "error", describe(err))
			release(0, "")
			// Give a worker that can take it a head start before this
			// one looks for work again.
			select {
			case <-parent.Done():
			case <-time.After(10 * d.cfg.Poll):
			}
			return
		case madeProgress(err):
			log.Warn("batch attempt stopped part-way; its progress is recorded and it will continue", "seq", batch.Seq, "key", batch.Key, "error", describe(err))
			release(0, "")
			select {
			case <-parent.Done():
			case <-time.After(d.cfg.Poll):
			}
			return
		case isPermanent(err) || lease.Attempts+1 >= d.cfg.MaxAttempts:
			reason := describe(err)
			if !isPermanent(err) {
				reason = fmt.Sprintf("gave up after %d attempts: %s", lease.Attempts+1, reason)
			}
			log.Error("batch failed", "seq", batch.Seq, "key", batch.Key, "error", reason)
			ferr := d.cfg.Store.Fenced(ctx, lease, func(tx store.Tx) error { return tx.FailBatch(ctx, batch.Seq, clip(reason)) })
			if ferr != nil && !errors.Is(ferr, store.ErrFenced) {
				log.Warn("recording the failure failed", "error", ferr)
				release(backoff(lease.Attempts), reason)
				return
			}
			release(0, "")
			return
		default:
			wait := backoff(lease.Attempts)
			log.Warn("batch attempt failed; will retry", "seq", batch.Seq, "key", batch.Key, "retryIn", wait.Round(time.Second), "error", describe(err))
			release(wait, describe(err))
			return
		}
	}
}

// safely runs process, turning a panic into an ordinary batch failure so
// that one bad batch is counted and retried like any other error instead of
// taking the whole worker down and leaving the lane to expire uncounted.
func (d *Worker) safely(ctx context.Context, lease *store.Lease, b *store.Batch) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic while processing %s batch %d: %v", lease.LaneID, b.Seq, r)
		}
	}()
	return d.process(ctx, lease, b)
}

// clip bounds an error message kept in the ledger and removes bytes
// Postgres text cannot hold.
func clip(s string) string {
	s = strings.ToValidUTF8(strings.ReplaceAll(s, "\x00", ""), "?")
	if len(s) > 1000 {
		s = s[:1000]
	}
	return s
}

func (d *Worker) process(ctx context.Context, lease *store.Lease, b *store.Batch) error {
	rs, err := d.run(ctx, lease.RunID)
	if err != nil {
		return err
	}
	if lease.Stage != store.StagePlan {
		if err := d.checkLayout(ctx, rs); err != nil {
			return err
		}
	} else if layout := runLayout(rs.run); !pipeline.LayoutSupported(layout) {
		// The run says, from its creation, which layout it needs. A
		// worker that does not know that layout must not plan it: it
		// would leave out what it does not know about.
		return notMine(fmt.Errorf("run %s needs ledger layout %d, which this worker does not know; run workers of the version that created the run",
			rs.run.ID, layout))
	}
	switch lease.Stage {
	case store.StagePlan:
		return d.plan(ctx, rs, lease, b)
	case store.StageExtract:
		return d.extract(ctx, rs, lease, b)
	case store.StageTransform:
		return d.transform(ctx, rs, lease, b)
	case store.StageTeam:
		return d.team(ctx, rs, lease, b)
	case store.StageFiles:
		return d.files(ctx, rs, lease, b)
	case store.StageFinalize:
		return d.finalize(ctx, rs, lease, b)
	case store.StageMembers:
		return d.members(ctx, rs, lease, b)
	case store.StageRelease:
		return d.release(ctx, rs, lease, b)
	case store.StageLoad:
		// A channel's load lane begins with two provisioning batches:
		// create the channel, then (once its files are uploaded) put it in
		// migration mode. The day batches follow. Under the legacy
		// strategy the first creates the channel in migration mode and
		// the second does nothing.
		switch b.Seq {
		case loadSeqCreate:
			return d.createChannel(ctx, rs, lease, b)
		case loadSeqOpen:
			return d.openChannel(ctx, rs, lease, b)
		}
		return d.load(ctx, rs, lease, b)
	}
	return permanent(fmt.Errorf("unknown stage %q", lease.Stage))
}

// runLayout is the ledger layout a run was created to need.
func runLayout(run store.Run) int {
	if run.Layout == 0 {
		return pipeline.LayoutVersion
	}
	return run.Layout
}

// runState caches what a worker needs to know about one run.
type runState struct {
	run   store.Run
	m     *config.Config
	blobs staging.Blobs // encrypted view for this run

	mu       sync.Mutex
	graph    Graph
	export   *export.Export
	scratch  string
	users    map[string]store.UserMapping
	channels map[string]store.ChannelMapping
	layoutOK bool
	// noFolder holds the channels whose files folder never appeared
	// (legacy strategy only); their files are not waited for again.
	noFolder map[string]bool
}

// run returns the cached state for a run, loading it on first use.
func (d *Worker) run(ctx context.Context, id string) (*runState, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if rs, ok := d.runs[id]; ok {
		return rs, nil
	}
	run, err := d.cfg.Store.GetRun(ctx, id)
	if err != nil {
		return nil, err
	}
	m, err := config.Parse(run.Config)
	if err != nil {
		return nil, permanent(fmt.Errorf("the run's config no longer parses: %w", err))
	}
	dek, err := d.cfg.KEK.Unwrap(run.WrappedKey)
	if err != nil {
		// This worker has a different key-encryption key from the one
		// the run was started with. That is this worker's problem, not
		// the batch's: hand the lane back so a correctly configured
		// worker can take it.
		return nil, notMine(err)
	}
	rs := &runState{
		run: run, m: m, blobs: staging.Encrypted(d.cfg.Blobs, dek),
		scratch: filepath.Join(d.cfg.ScratchDir, id),
	}
	d.runs[id] = rs
	return rs, nil
}

// checkLayout verifies, once per run, that the run was planned under the
// ledger layout this worker understands. A mismatch is this worker's
// problem, not the batch's: the error is not permanent, so the lane goes
// back for a worker of the right version.
func (d *Worker) checkLayout(ctx context.Context, rs *runState) error {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.layoutOK {
		return nil
	}
	p, err := d.cfg.Store.Progress(ctx, rs.run.ID)
	if err != nil {
		return err
	}
	var totals pipeline.Totals
	if len(p.Totals) > 0 {
		if err := json.Unmarshal(p.Totals, &totals); err != nil {
			return fmt.Errorf("the run's plan cannot be read: %w", err)
		}
	}
	if !pipeline.LayoutSupported(totals.Layout) {
		return notMine(fmt.Errorf("this worker works ledger layouts up to %d but run %s was planned under layout %d; "+
			"run workers of one version only, and start a new run with `taut migrate --new` if the run predates an upgrade",
			pipeline.LayoutPassWithChats, rs.run.ID, totals.Layout))
	}
	rs.layoutOK = true
	return nil
}

// client returns the run's Graph client, building it on first use.
func (d *Worker) client(ctx context.Context, rs *runState) (Graph, error) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.graph != nil {
		return rs.graph, nil
	}
	if d.cfg.Graph == nil {
		return nil, notMine(errors.New("this worker has no Microsoft Graph credentials configured"))
	}
	g, err := d.cfg.Graph(ctx, rs.m)
	if err != nil {
		return nil, err
	}
	rs.graph = g
	return g, nil
}

// openExport returns the run's export, downloading and decrypting it to
// scratch on first use. The plaintext copy lives only as long as this
// process and is removed on exit.
func (d *Worker) openExport(ctx context.Context, rs *runState) (*export.Export, error) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.export != nil {
		return rs.export, nil
	}
	if rs.run.ExportKey == "" {
		return nil, permanent(errors.New("the run has no export uploaded"))
	}
	if err := os.MkdirAll(rs.scratch, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(rs.scratch, "export.zip")
	tmp := path + ".part"
	src, err := rs.blobs.Get(ctx, rs.run.ExportKey)
	if err != nil {
		return nil, err
	}
	defer src.Close()
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := copyCtx(ctx, f, src); err != nil {
		f.Close()
		os.Remove(tmp)
		return nil, err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return nil, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, err
	}
	ex, err := export.Open(path)
	if err != nil {
		return nil, permanent(err)
	}
	rs.export = ex
	return ex, nil
}

// mappings returns the run's user and channel maps, loaded once. They are
// fixed when the plan is applied, so caching them is safe.
func (d *Worker) mappings(ctx context.Context, rs *runState) (map[string]store.UserMapping, map[string]store.ChannelMapping, error) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.users != nil {
		return rs.users, rs.channels, nil
	}
	us, err := d.cfg.Store.Users(ctx, rs.run.ID)
	if err != nil {
		return nil, nil, err
	}
	cs, err := d.cfg.Store.Channels(ctx, rs.run.ID)
	if err != nil {
		return nil, nil, err
	}
	users := make(map[string]store.UserMapping, len(us))
	for _, u := range us {
		users[u.SlackID] = u
	}
	chans := make(map[string]store.ChannelMapping, len(cs))
	for _, c := range cs {
		chans[c.SlackID] = c
	}
	rs.users, rs.channels = users, chans
	return users, chans, nil
}

// sweep drops the cached state of runs that have been finalized: every lane is
// done, so no worker is using it, and staging is purged after a finalize, so
// the scratch copy of the export must not outlive it. (A cancelled run may
// still have a batch in flight; its scratch copy goes when the worker
// exits.)
func (d *Worker) sweep(ctx context.Context) {
	d.mu.Lock()
	ids := make([]string, 0, len(d.runs))
	for id := range d.runs {
		ids = append(ids, id)
	}
	d.mu.Unlock()
	for _, id := range ids {
		run, err := d.cfg.Store.GetRun(ctx, id)
		if err != nil || run.Status != store.RunFinalized {
			continue
		}
		d.mu.Lock()
		rs := d.runs[id]
		delete(d.runs, id)
		d.mu.Unlock()
		if rs == nil {
			continue
		}
		rs.mu.Lock()
		if rs.export != nil {
			_ = rs.export.Close()
			rs.export = nil
		}
		rs.mu.Unlock()
		_ = os.RemoveAll(rs.scratch)
		d.cfg.Log.Info("run is over; removed its scratch copy", "run", id, "status", string(run.Status))
	}

	// Scratch directories this process does not know: left by a worker
	// that was killed. Each is named after its run. Remove those whose run
	// is over or gone; a run still in progress may belong to a live
	// worker sharing the directory.
	entries, err := os.ReadDir(d.cfg.ScratchDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		d.mu.Lock()
		_, known := d.runs[name]
		d.mu.Unlock()
		if known || !e.IsDir() || !filepath.IsLocal(name) {
			continue
		}
		run, err := d.cfg.Store.GetRun(ctx, name)
		over := errors.Is(err, store.ErrNotFound) ||
			(err == nil && (run.Status == store.RunFinalized || run.Status == store.RunCancelled || run.PurgedAt != nil))
		if !over {
			continue
		}
		if err := os.RemoveAll(filepath.Join(d.cfg.ScratchDir, name)); err == nil {
			d.cfg.Log.Info("removed a scratch copy left by an earlier process", "run", name)
		}
	}
}

func (d *Worker) cleanup() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, rs := range d.runs {
		if rs.export != nil {
			_ = rs.export.Close()
		}
		_ = os.RemoveAll(rs.scratch)
	}
	if d.ownScratch {
		_ = os.RemoveAll(d.cfg.ScratchDir)
	}
}

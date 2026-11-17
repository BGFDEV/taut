// Package local runs a whole migration stack inside one process: the
// controller, a worker, the token broker and the egress proxy, with the
// ledger in a PostgreSQL server the process starts and stops and the staged
// data in an encrypted directory. It is what `local.enabled: true` in a
// config file selects, for workspaces too small to deserve a cluster.
//
// Nothing here is a second implementation. The ledger is the Postgres
// store, the bus is the in-process bus the tests use, staging is the
// filesystem backend under the same envelope encryption, and the worker
// still gets tokens only from the token broker and reaches Microsoft and
// Slack only through the egress proxy, at the rates the proxy leases from
// the allocator. What local mode does not have is the separation of those
// parts into processes: they share one address space and one user account.
package local

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bgfdev/taut/internal/bus"
	"github.com/bgfdev/taut/internal/config"
	"github.com/bgfdev/taut/internal/controller"
	"github.com/bgfdev/taut/internal/fail"
	"github.com/bgfdev/taut/internal/secret"
	"github.com/bgfdev/taut/internal/sidecar/budget"
	"github.com/bgfdev/taut/internal/sidecar/egressproxy"
	"github.com/bgfdev/taut/internal/sidecar/tokenbroker"
	"github.com/bgfdev/taut/internal/staging"
	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/worker"
)

// Options configure a local stack.
type Options struct {
	// Config is the migration's config file, loaded from disk: its
	// directory is what the state directory and relative secret paths
	// resolve against.
	Config *config.Config
	// Getenv reads the process environment. Required.
	Getenv func(string) string
	// Tell reports to the operator what is being started, and anything
	// that is downloaded. Nil is silent.
	Tell func(format string, args ...any)
	// Work starts the worker and its sidecars. Commands that only read
	// (status, dashboard, audit) leave it off: they need the ledger and
	// the controller's API, and must not import anything.
	Work bool
	// LookPath finds a program on PATH; nil means exec.LookPath.
	LookPath func(string) (string, error)
}

// Runtime is what a running stack writes into its state directory so that
// another taut process can find its controller.
type Runtime struct {
	PID     int       `json:"pid"`
	URL     string    `json:"url"`
	Token   string    `json:"token"`
	Work    bool      `json:"work"`
	Started time.Time `json:"started"`
}

// Stack is a running local stack, or a connection to one that another
// process runs.
type Stack struct {
	// URL and Token reach the stack's controller.
	URL   string
	Token string
	// Owner is false when the stack belongs to another process and this
	// one is only attached to it.
	Owner bool
	// PID is the process that runs the stack.
	PID int
	// Dir is the state directory.
	Dir string

	mu     sync.Mutex
	closed bool
	stops  []func()
}

// Client returns a client for the stack's controller.
func (s *Stack) Client() *controller.Client { return controller.NewClient(s.URL, s.Token) }

// Close stops everything the stack started, in reverse order, and releases
// the state directory. It is safe to call twice, and does nothing for a
// stack this process is only attached to.
func (s *Stack) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	for i := len(s.stops) - 1; i >= 0; i-- {
		s.stops[i]()
	}
}

func (s *Stack) onClose(fn func()) { s.stops = append(s.stops, fn) }

const (
	lockFile    = "lock"
	runtimeFile = "runtime.json"
	keyFile     = "staging.key"
)

// ReadRuntime returns what the stack running in a state directory wrote
// about itself, if one is.
func ReadRuntime(dir string) (Runtime, bool) {
	raw, err := os.ReadFile(filepath.Join(dir, runtimeFile)) //nolint:gosec // inside the state directory
	if err != nil {
		return Runtime{}, false
	}
	var rt Runtime
	if json.Unmarshal(raw, &rt) != nil || rt.URL == "" {
		return Runtime{}, false
	}
	return rt, true
}

// inUse is the refusal of a command that needs the state directory to
// itself while another process has it.
func inUse(dir string, rt Runtime, known bool) error {
	who := "Another taut process"
	if known {
		who = fmt.Sprintf("Another taut process (pid %d)", rt.PID)
	}
	return fail.New("local.in_use", "This migration's state directory is in use",
		who+" is running from "+dir+". Two processes working one ledger and one staging directory would step on each other.",
		"Watch it with `taut status` or `taut dashboard`, or stop it (ctrl+c in its terminal) and run this command again.").WithExit(fail.ExitUsage)
}

// Open starts the local stack for a config file, or attaches to the one
// another taut process is already running from the same state directory.
//
// A state directory has one owner at a time, held by a lock the operating
// system releases when the owner exits, however it exits. A command that
// does work (o.Work) must be the owner and is refused otherwise. A command
// that only reads attaches to the owner's controller when there is one,
// and otherwise becomes the owner of a stack with no worker.
func Open(ctx context.Context, o Options) (*Stack, error) {
	m := o.Config
	if o.Tell == nil {
		o.Tell = func(string, ...any) {}
	}
	dir := m.StateDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fail.Wrap(err, "local.state_dir", "The state directory could not be created", err.Error(),
			"Check that "+filepath.Dir(dir)+" is writable, or set local.stateDir in the config file.").WithExit(fail.ExitUsage)
	}
	lock, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // inside the state directory
	if err != nil {
		return nil, fail.Wrap(err, "local.state_dir", "The state directory could not be opened", err.Error(),
			"Check the permissions of "+dir+".").WithExit(fail.ExitUsage)
	}
	held, err := tryLock(lock)
	if err != nil {
		_ = lock.Close()
		return nil, fail.Wrap(err, "local.state_dir", "The state directory could not be locked", err.Error(),
			"Put the state directory on a local filesystem (local.stateDir); network filesystems often cannot lock.").WithExit(fail.ExitUsage)
	}
	if !held {
		_ = lock.Close()
		rt, known := ReadRuntime(dir)
		if o.Work || !known {
			return nil, inUse(dir, rt, known)
		}
		return &Stack{URL: rt.URL, Token: rt.Token, PID: rt.PID, Dir: dir, closed: true}, nil
	}

	s := &Stack{Owner: true, PID: os.Getpid(), Dir: dir}
	s.onClose(func() {
		_ = os.Remove(filepath.Join(dir, runtimeFile))
		_ = unlock(lock)
		_ = lock.Close()
	})
	if err := s.start(ctx, o); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// kek returns the state directory's key-encryption key, creating it on
// first use. It wraps the data key of every run staged here, so it lives
// and dies with the directory.
func kek(dir string) (staging.KEK, error) {
	path := filepath.Join(dir, keyFile)
	raw, err := os.ReadFile(path) //nolint:gosec // inside the state directory
	if errors.Is(err, os.ErrNotExist) {
		k, gerr := staging.GenerateKEK()
		if gerr != nil {
			return staging.KEK{}, gerr
		}
		if werr := os.WriteFile(path, []byte(k+"\n"), 0o600); werr != nil {
			return staging.KEK{}, werr
		}
		raw = []byte(k)
	} else if err != nil {
		return staging.KEK{}, err
	}
	return staging.ParseKEK(secret.New(strings.TrimSpace(string(raw))))
}

func randomToken() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// envFloat reads a positive number from the environment, or zero.
func envFloat(getenv func(string) string, name string) float64 {
	if v, err := strconv.ParseFloat(getenv(name), 64); err == nil && v > 0 {
		return v
	}
	return 0
}

// budgetTotals are the tenant-wide rate budgets: the defaults a cluster
// uses, overridden by the same environment variables.
func budgetTotals(getenv func(string) string) map[string]float64 {
	totals := budget.DefaultTotals()
	for scope, name := range map[string]string{
		"graph": "TAUT_BUDGET_GRAPH_RPS", "sharepoint": "TAUT_BUDGET_SHAREPOINT_RPS", "slack": "TAUT_BUDGET_SLACK_RPS",
	} {
		if v, err := strconv.ParseFloat(getenv(name), 64); err == nil && v > 0 {
			totals[scope] = v
		}
	}
	return totals
}

func (s *Stack) start(ctx context.Context, o Options) error {
	m, dir := o.Config, s.Dir
	logFile, err := os.OpenFile(filepath.Join(dir, "taut.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // inside the state directory
	if err != nil {
		return err
	}
	s.onClose(func() { _ = logFile.Close() })
	log := slog.New(slog.NewJSONHandler(logFile, nil))

	// --- the ledger ---------------------------------------------------------
	pg, err := FindPostgres(ctx, Search{
		Binaries: m.ResolvePath(m.Local.Postgres.Binaries), Download: m.Local.Postgres.Download,
		Major: DataMajor(dir), LookPath: o.LookPath,
	})
	if err != nil {
		return err
	}
	o.Tell("Starting the local ledger (PostgreSQL in %s)…", filepath.Join(dir, "postgres"))
	srv, err := startPostgres(ctx, pg, filepath.Join(dir, "postgres"), o.Tell)
	if err != nil {
		return err
	}
	s.onClose(func() {
		if err := srv.stop(); err != nil {
			log.Warn("the local PostgreSQL did not stop cleanly; the next run stops it", "error", err.Error())
		}
	})
	st, err := store.Open(ctx, srv.url, store.WithMaxConns(16))
	if err != nil {
		return err
	}
	s.onClose(st.Close)
	if err := st.Migrate(ctx); err != nil {
		return err
	}

	// --- staging, the bus, the budgets ----------------------------------------
	key, err := kek(dir)
	if err != nil {
		return fail.Wrap(err, "local.staging_key", "The staging key of this state directory could not be read", err.Error(),
			"Check "+filepath.Join(dir, keyFile)+". It is created on first use and must not be edited; without it, staged data cannot be read.")
	}
	s.onClose(key.Destroy)
	blobs, err := staging.NewFS(filepath.Join(dir, "staging"))
	if err != nil {
		return err
	}
	local := bus.NewLocal()
	totals := budgetTotals(o.Getenv)
	alloc := budget.NewAllocator(totals, 0, nil)
	stopAlloc, err := alloc.Serve(local)
	if err != nil {
		return err
	}
	s.onClose(stopAlloc)

	// --- the controller -------------------------------------------------------
	testEndpoints := o.Getenv("TAUT_ALLOW_TEST_ENDPOINTS") == "1"
	if s.Token, err = randomToken(); err != nil {
		return err
	}
	api := controller.New(controller.Config{}, log.With("part", "controller"), controller.Services{
		Store: st, Blobs: blobs, KEK: key, Bus: local, Token: s.Token,
		AllowTestEndpoints: testEndpoints, Budgets: alloc.Snapshot,
	})
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	s.URL = "http://" + ln.Addr().String()
	run, cancel := context.WithCancel(context.WithoutCancel(ctx))
	var wg sync.WaitGroup
	s.onClose(func() { cancel(); wg.Wait() })
	wg.Go(func() { _ = api.ServeListener(run, ln) })

	if o.Work {
		if err := s.startWork(run, &wg, o, log, st, blobs, key, local, totals, testEndpoints); err != nil {
			return err
		}
	}

	rt := Runtime{PID: s.PID, URL: s.URL, Token: s.Token, Work: o.Work, Started: time.Now().UTC()}
	raw, _ := json.Marshal(rt)
	if err := os.WriteFile(filepath.Join(dir, runtimeFile), raw, 0o600); err != nil {
		return err
	}
	return nil
}

// secretRefs are the credential references of a config file: what the
// token broker will be asked to read.
func secretRefs(m *config.Config) []secret.Ref {
	var refs []secret.Ref
	for _, r := range []secret.Ref{m.Target.Entra.Credential.Certificate, m.Target.Entra.Credential.ClientSecret, m.Source.Slack.API.Token} {
		if !r.IsZero() {
			refs = append(refs, r)
		}
	}
	return refs
}

// Absolute returns a copy of the config file in which every secret file
// reference is an absolute path. The token broker reads only absolute
// paths, and the config file a run is created with is all it sees: it has
// no notion of "relative to the config file".
func Absolute(m *config.Config) *config.Config {
	c := *m
	abs := func(r *secret.Ref) {
		if r.File != "" {
			r.File = m.ResolvePath(r.File)
			if a, err := filepath.Abs(r.File); err == nil {
				r.File = a
			}
		}
	}
	abs(&c.Target.Entra.Credential.Certificate)
	abs(&c.Target.Entra.Credential.ClientSecret)
	abs(&c.Source.Slack.API.Token)
	return &c
}

// startWork starts the parts that import: the token broker, the egress
// proxy and the worker, wired exactly as in a worker pod. The worker is
// given no credential and no direct route; its only way out is the proxy.
func (s *Stack) startWork(ctx context.Context, wg *sync.WaitGroup, o Options, log *slog.Logger, st store.Store,
	blobs staging.Blobs, key staging.KEK, local *bus.Local, totals map[string]float64, testEndpoints bool) error {
	m := Absolute(o.Config)

	// The broker serves this migration and nothing else: its tenant, its
	// application, its cloud, and the secrets its config file references.
	bc := tokenbroker.Config{
		AllowTenants: []string{m.Target.Entra.TenantID},
		AllowClients: []string{m.Target.Entra.ClientID},
		Log:          log.With("part", "token-broker"),
	}
	if m.Target.Entra.Cloud != "" {
		bc.AllowClouds = []string{m.Target.Entra.Cloud}
	}
	for _, r := range secretRefs(m) {
		if r.Env != "" {
			bc.EnvAllow = append(bc.EnvAllow, r.Env)
		} else {
			bc.SecretDirs = append(bc.SecretDirs, filepath.Dir(r.File))
		}
	}
	if len(bc.EnvAllow) == 0 {
		// An empty list means the broker's defaults; say "none" instead.
		bc.EnvAllow = []string{"TAUT_NO_SUCH_VARIABLE"}
	}
	if len(bc.SecretDirs) == 0 {
		bc.SecretDirs = []string{filepath.Join(s.Dir, "no-secrets")}
	}
	var allowHosts []string
	route := worker.Proxied{}
	if testEndpoints {
		// Fake servers, for the demo and the tests. The config file names
		// them and the controller was told to accept that.
		for _, ep := range []string{m.Target.Entra.Authority, m.Target.Entra.GraphBaseURL} {
			if ep != "" {
				bc.AllowEndpoints = append(bc.AllowEndpoints, ep)
			}
		}
		if g := m.Target.Entra.GraphBaseURL; g != "" {
			allowHosts = append(allowHosts, "graph="+g)
		}
		if files := o.Getenv("TAUT_SLACK_FILES_URL"); files != "" {
			route.SlackFilesBase = files
			if fu, err := url.Parse(files); err == nil {
				if gu, gerr := url.Parse(m.Target.Entra.GraphBaseURL); gerr != nil || gu.Host != fu.Host {
					allowHosts = append(allowHosts, "slackfiles="+files)
				}
			}
		}
	}

	// A Unix socket path is limited to about a hundred bytes, which a state
	// directory deep in a home directory can exceed; the socket goes into a
	// short private directory of its own.
	sockDir, err := os.MkdirTemp("", "taut-")
	if err != nil {
		return err
	}
	s.onClose(func() { _ = os.RemoveAll(sockDir) })
	bc.Socket = filepath.Join(sockDir, "s", "token.sock")
	broker, err := tokenbroker.Listen(bc)
	if err != nil {
		return err
	}
	wg.Go(func() { _ = broker.Serve(ctx) })

	holder := fmt.Sprintf("local-%d", os.Getpid())
	proxy, err := egressproxy.New(egressproxy.Config{
		Holder:     holder,
		Tokens:     tokenbroker.NewClient(bc.Socket),
		Budgets:    budget.Remote(local),
		Bus:        local,
		AllowHosts: allowHosts,
		MaxRPS:     totals,
		// The per-channel rates, by the variables the egress proxy of a
		// cluster reads. Zero keeps its defaults: start at 1, up to 5.
		ChannelStartRPS: envFloat(o.Getenv, "TAUT_GRAPH_CHANNEL_RPS"),
		ChannelMaxRPS:   envFloat(o.Getenv, "TAUT_GRAPH_CHANNEL_MAX"),
		ChatRPS:         envFloat(o.Getenv, "TAUT_GRAPH_CHAT_RPS"),
		Log:             log.With("part", "egress-proxy"),
	})
	if err != nil {
		return err
	}
	s.onClose(proxy.Close)
	pln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	wg.Go(func() { _ = proxy.ServeListener(ctx, pln) })

	route.URL = "http://" + pln.Addr().String()
	route.Client = &http.Client{}
	concurrency := 4
	if v, err := strconv.Atoi(o.Getenv("TAUT_WORKER_CONCURRENCY")); err == nil && v > 0 {
		concurrency = v
	}
	w, err := worker.New(worker.Config{
		Holder: holder,
		Stages: []string{store.StagePlan, store.StageExtract, store.StageTransform, store.StageTeam, store.StageFiles,
			store.StageLoad, store.StageFinalize, store.StageMembers, store.StageRelease},
		Store: st, Blobs: blobs, KEK: key, Bus: local,
		Graph: route.GraphFactory(), Slack: route.SlackFiles(),
		Log:         log.With("part", "worker"),
		Concurrency: concurrency,
		// The egress proxy sets the pace, as in a cluster.
		ChannelRPS: 1e6, TenantRPS: 1e6,
		ScratchDir: filepath.Join(s.Dir, "scratch"),
	})
	if err != nil {
		return err
	}
	wg.Go(func() { _ = w.Run(ctx) })
	return nil
}

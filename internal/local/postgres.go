package local

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"

	"github.com/bgfdev/taut/internal/fail"
)

// The ledger of a local migration is the same Postgres ledger a cluster
// uses, in a PostgreSQL server this process starts in the state directory
// and stops when it is done. See docs/design-notes.md for why it is a real
// PostgreSQL and not a second engine.

// MinPostgres is the oldest PostgreSQL major version the ledger's schema
// is tested against.
const MinPostgres = 14

// downloadVersion is the PostgreSQL that is downloaded when none is
// installed and the config file allows it.
const downloadVersion = embeddedpostgres.V17

// downloadHost is where it comes from: Maven Central, which hosts the
// PostgreSQL builds of the zonkyio/embedded-postgres-binaries project. The
// archive is compared with the SHA-256 published beside it before it is
// unpacked. That catches a damaged download; coming from the same host, it
// does not authenticate the build. TLS to Maven Central is what does.
const downloadHost = "https://repo1.maven.org/maven2"

// Postgres describes the PostgreSQL installation local mode would use.
type Postgres struct {
	// Dir is the installation: the directory that holds bin/postgres.
	Dir string
	// Major is its major version; zero until it is installed.
	Major int
	// Origin says where it was found: "config" (local.postgres.binaries),
	// "path", "system" (a usual installation directory), "cache" (a
	// download made earlier) or "download" (not there yet).
	Origin string
}

// NeedsDownload reports whether using it means downloading it first.
func (p Postgres) NeedsDownload() bool { return p.Origin == "download" }

func exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// complete reports whether dir holds the three programs local mode runs.
func complete(dir string) bool {
	for _, name := range []string{"postgres", "initdb", "pg_ctl"} {
		if st, err := os.Stat(filepath.Join(dir, "bin", exe(name))); err != nil || st.IsDir() {
			return false
		}
	}
	return true
}

var versionRE = regexp.MustCompile(`\(PostgreSQL\) (\d+)`)

// majorOf asks an installation for its major version.
func majorOf(ctx context.Context, dir string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, filepath.Join(dir, "bin", exe("postgres")), "--version").Output() //nolint:gosec // a PostgreSQL installation the operator has or named
	if err != nil {
		return 0, err
	}
	m := versionRE.FindSubmatch(out)
	if m == nil {
		return 0, fmt.Errorf("unexpected version output %q", strings.TrimSpace(string(out)))
	}
	return strconv.Atoi(string(m[1]))
}

// installDirs are where PostgreSQL usually lives when it is not on PATH:
// Debian and Ubuntu, Red Hat, Homebrew on both architectures, Postgres.app.
var installDirs = []string{
	"/usr/lib/postgresql/*",
	"/usr/pgsql-*",
	"/opt/homebrew/opt/postgresql@*",
	"/usr/local/opt/postgresql@*",
	"/Applications/Postgres.app/Contents/Versions/*",
}

// CacheDir is where a downloaded PostgreSQL is kept between runs, shared by
// every migration of this user: the user cache directory, under "taut".
func CacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "taut", "postgres"), nil
}

func downloadDir(cache string) string {
	return filepath.Join(cache, fmt.Sprintf("postgresql-%s-%s-%s", downloadVersion, runtime.GOOS, runtime.GOARCH))
}

// Search says what to look for.
type Search struct {
	// Binaries is local.postgres.binaries, resolved against the config
	// file; Download is local.postgres.download.
	Binaries string
	Download bool
	// Major, when not zero, is the PostgreSQL version that created the
	// ledger in the state directory. An installation of that version is
	// preferred over a newer one that could not open it.
	Major int
	// LookPath finds a program on PATH; nil means exec.LookPath.
	LookPath func(string) (string, error)
}

// DataMajor returns the PostgreSQL major version that created the ledger of
// a state directory, or zero when it has none yet.
func DataMajor(stateDir string) int {
	return dataMajor(filepath.Join(stateDir, "postgres", "data"))
}

// FindPostgres decides which PostgreSQL local mode uses, without starting
// or downloading anything.
//
// The order is: the installation the config file names; one on PATH; one in
// a usual installation directory (the newest); one downloaded earlier; and
// last, if the config file allows it, a download. An installation that is
// found is used as it is; nothing is fetched while one exists. When the
// state directory already has a ledger, an installation of the version that
// made it comes first, wherever in that order it was found.
func FindPostgres(ctx context.Context, q Search) (Postgres, error) {
	binaries, download, lookPath := q.Binaries, q.Download, q.LookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	usable := func(dir, origin string) (Postgres, bool) {
		if !complete(dir) {
			return Postgres{}, false
		}
		major, err := majorOf(ctx, dir)
		if err != nil || major < MinPostgres {
			return Postgres{}, false
		}
		return Postgres{Dir: dir, Major: major, Origin: origin}, true
	}

	if binaries != "" {
		dir := strings.TrimSuffix(filepath.Clean(binaries), string(filepath.Separator)+"bin")
		if !complete(dir) {
			return Postgres{}, fail.New("local.postgres_not_found", "local.postgres.binaries does not name a PostgreSQL installation",
				binaries+" has no bin/postgres, bin/initdb and bin/pg_ctl.",
				"Set local.postgres.binaries to the directory that contains bin/postgres, or remove it to let taut look for PostgreSQL itself.").WithExit(fail.ExitUsage)
		}
		major, err := majorOf(ctx, dir)
		switch {
		case err != nil:
			return Postgres{}, fail.Wrap(err, "local.postgres_not_found", "The PostgreSQL in local.postgres.binaries could not be run", err.Error(),
				"Check that "+filepath.Join(dir, "bin", "postgres")+" runs on this machine.").WithExit(fail.ExitUsage)
		case major < MinPostgres:
			return Postgres{}, tooOld(dir, major)
		}
		return Postgres{Dir: dir, Major: major, Origin: "config"}, nil
	}

	// Every installation there is, in order of preference.
	var (
		old   *fail.Error
		found []Postgres
	)
	if p, err := lookPath("pg_ctl"); err == nil {
		if real, err := filepath.EvalSymlinks(p); err == nil {
			p = real
		}
		dir := filepath.Dir(filepath.Dir(p))
		if pg, ok := usable(dir, "path"); ok {
			found = append(found, pg)
		} else if major, err := majorOf(ctx, dir); err == nil && major < MinPostgres {
			old = tooOld(dir, major)
		}
	}
	var system []Postgres
	for _, pattern := range installDirs {
		dirs, _ := filepath.Glob(pattern)
		for _, dir := range dirs {
			if pg, ok := usable(dir, "system"); ok {
				system = append(system, pg)
			}
		}
	}
	sort.SliceStable(system, func(i, j int) bool { return system[i].Major > system[j].Major })
	found = append(found, system...)
	cache, cacheErr := CacheDir()
	if cacheErr == nil {
		if pg, ok := usable(downloadDir(cache), "cache"); ok {
			found = append(found, pg)
		}
	}
	for _, pg := range found {
		if q.Major != 0 && pg.Major == q.Major {
			return pg, nil
		}
	}
	if len(found) > 0 {
		// None of the ledger's version, if there is a ledger: the first is
		// returned, and starting it says which version is needed.
		return found[0], nil
	}
	if cacheErr == nil && download {
		return Postgres{Dir: downloadDir(cache), Origin: "download"}, nil
	}
	if old != nil {
		return Postgres{}, old
	}
	return Postgres{}, fail.New("local.postgres_missing", "Local mode needs PostgreSQL, and none is installed",
		fmt.Sprintf("taut keeps the migration's ledger in a PostgreSQL server (version %d or newer) that it starts and stops itself. It found none on PATH or in the usual installation directories, and local.postgres.download is not set, so nothing was downloaded.", MinPostgres),
		"Install PostgreSQL (`brew install postgresql@17`, `apt install postgresql`, or from postgresql.org), or set local.postgres.download: true in the config file to let taut download it from Maven Central once. `taut doctor` shows what it finds.").WithExit(fail.ExitUsage)
}

func tooOld(dir string, major int) *fail.Error {
	return fail.New("local.postgres_too_old", fmt.Sprintf("The PostgreSQL at %s is version %d", dir, major),
		fmt.Sprintf("Local mode needs version %d or newer.", MinPostgres),
		"Install a newer PostgreSQL and point local.postgres.binaries at it, or set local.postgres.download: true to let taut download one.").WithExit(fail.ExitUsage)
}

// server is the running PostgreSQL of a local stack.
type server struct {
	ep   *embeddedpostgres.EmbeddedPostgres
	url  string
	logs io.Closer
}

// freePort asks the system for a loopback port nothing is using.
func freePort(ctx context.Context) (uint32, error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return uint32(ln.Addr().(*net.TCPAddr).Port), nil //nolint:gosec // a TCP port
}

// password returns the database password of a state directory, creating it
// on first use. PostgreSQL listens on loopback only, where other users of
// the machine can reach it; the password is what keeps them out.
func password(dir string) (string, error) {
	path := filepath.Join(dir, "password")
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) >= 32 { //nolint:gosec // inside the state directory
		return strings.TrimSpace(string(b)), nil
	}
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	pw := hex.EncodeToString(raw[:])
	if err := os.WriteFile(path, []byte(pw+"\n"), 0o600); err != nil {
		return "", err
	}
	return pw, nil
}

// dataMajor reads the PostgreSQL version a data directory was created by,
// or zero when there is none yet.
func dataMajor(data string) int {
	b, err := os.ReadFile(filepath.Join(data, "PG_VERSION")) //nolint:gosec // inside the state directory
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return n
}

// stopStale stops a PostgreSQL that an earlier taut process left running in
// this data directory: one that was killed before it could stop it. The
// state directory's lock is held, so nothing else can be using it.
func stopStale(ctx context.Context, pg Postgres, data string) error {
	if _, err := os.Stat(filepath.Join(data, "postmaster.pid")); err != nil {
		return nil //nolint:nilerr // no pid file: nothing is running here
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, filepath.Join(pg.Dir, "bin", exe("pg_ctl")), "stop", "-D", data, "-m", "fast", "-w", "-t", "60").CombinedOutput() //nolint:gosec // the installation chosen by FindPostgres
	if err != nil {
		if _, serr := os.Stat(filepath.Join(data, "postmaster.pid")); serr == nil && !strings.Contains(string(out), "No such process") && !strings.Contains(string(out), "Is server running") {
			return fail.New("local.postgres_stale", "A PostgreSQL left over from an earlier run could not be stopped",
				strings.TrimSpace(string(out)),
				"Stop it yourself with `"+filepath.Join(pg.Dir, "bin", "pg_ctl")+" stop -D "+data+" -m fast`, then run the command again.")
		}
		// The pid file was stale: the server it names is gone. PostgreSQL
		// removes it on the next start.
	}
	return nil
}

// startPostgres starts the state directory's PostgreSQL, creating its data
// directory on first use and downloading the server first if pg says so.
// tell reports what is happening to the operator.
func startPostgres(ctx context.Context, pg Postgres, dir string, tell func(format string, args ...any)) (*server, error) {
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		return nil, fail.New("local.root", "Local mode cannot run as root",
			"PostgreSQL refuses to start as the superuser.",
			"Run taut as an ordinary user.").WithExit(fail.ExitUsage)
	}
	data, run := filepath.Join(dir, "data"), filepath.Join(dir, "run")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	version := downloadVersion
	if pg.NeedsDownload() {
		cache := filepath.Dir(pg.Dir)
		if err := os.MkdirAll(cache, 0o700); err != nil {
			return nil, err
		}
		// Said before anything is fetched, every time something is.
		tell("Downloading PostgreSQL %s for %s/%s from %s into %s (once; local.postgres.download is set)…",
			version, runtime.GOOS, runtime.GOARCH, strings.TrimPrefix(downloadHost, "https://"), cache)
	} else {
		version = embeddedpostgres.PostgresVersion(fmt.Sprintf("%d.0.0", pg.Major))
		if was := dataMajor(data); was != 0 && was != pg.Major {
			// Never let a different major version near the ledger: it
			// cannot read the files, and re-creating them would erase it.
			return nil, fail.New("local.postgres_version", "The ledger was created by a different PostgreSQL version",
				fmt.Sprintf("The data in %s was created by PostgreSQL %d; the installation at %s is version %d.", data, was, pg.Dir, pg.Major),
				fmt.Sprintf("Point local.postgres.binaries at a PostgreSQL %d installation. The ledger is kept as it is.", was)).WithExit(fail.ExitUsage)
		}
		if err := stopStale(ctx, pg, data); err != nil {
			return nil, err
		}
	}
	if was := dataMajor(data); was != 0 && pg.NeedsDownload() {
		// A ledger exists but its PostgreSQL is gone (the cache was
		// cleared). The download is the version that made it only if the
		// majors agree.
		if want, _ := strconv.Atoi(strings.SplitN(string(version), ".", 2)[0]); was != want {
			return nil, fail.New("local.postgres_version", "The ledger was created by a different PostgreSQL version",
				fmt.Sprintf("The data in %s was created by PostgreSQL %d; taut downloads version %d.", data, was, want),
				fmt.Sprintf("Install PostgreSQL %d and point local.postgres.binaries at it. The ledger is kept as it is.", was)).WithExit(fail.ExitUsage)
		}
	}

	pw, err := password(dir)
	if err != nil {
		return nil, err
	}
	port, err := freePort(ctx)
	if err != nil {
		return nil, err
	}
	logs, err := os.OpenFile(filepath.Join(dir, "postgres.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // inside the state directory
	if err != nil {
		return nil, err
	}
	cfg := embeddedpostgres.DefaultConfig().
		Version(version).
		Port(port).
		Database("taut").Username("taut").Password(pw).
		BinariesPath(pg.Dir).CachePath(filepath.Dir(pg.Dir)).
		RuntimePath(run).DataPath(data).
		BinaryRepositoryURL(downloadHost).
		// The ledger orders text with an explicit "C" collation wherever
		// order matters, so the cluster's own locale is "C" too: the same
		// on every machine, whatever the operator's locale is.
		Locale("C").Encoding("UTF8").
		Logger(logs).
		StartTimeout(90 * time.Second).
		StartParameters(map[string]string{
			// Loopback only, and no socket file in a shared directory.
			"listen_addresses":        "127.0.0.1",
			"unix_socket_directories": "",
			"max_connections":         "64",
		})
	// PostgreSQL on macOS refuses to start when the environment names no
	// usable locale ("postmaster became multithreaded during startup"). It
	// inherits this process's environment, so give it one when there is
	// none. Nothing else in taut reads these variables.
	if os.Getenv("LC_ALL") == "" && os.Getenv("LANG") == "" {
		_ = os.Setenv("LC_ALL", "C")
	}
	fresh := dataMajor(data) == 0
	ep := embeddedpostgres.NewDatabase(cfg)
	if err := ep.Start(); err != nil {
		_ = logs.Close()
		if fresh {
			// A data directory that was being created is not a ledger yet.
			// Left half-made, the next start would take it for one.
			_ = os.RemoveAll(data)
		}
		why := lastLines(err.Error(), 6)
		next := "The server's own log is " + filepath.Join(dir, "postgres.log") + ". `taut doctor` checks the installation."
		if pg.NeedsDownload() {
			next = "Check the network path to " + strings.TrimPrefix(downloadHost, "https://") + ", or install PostgreSQL yourself and remove local.postgres.download. " + next
		}
		return nil, fail.Wrap(errors.New(why), "local.postgres_start", "The local PostgreSQL could not be started", why, next)
	}
	u := url.URL{Scheme: "postgres", User: url.UserPassword("taut", pw), Host: fmt.Sprintf("127.0.0.1:%d", port), Path: "/taut", RawQuery: "sslmode=disable"}
	return &server{ep: ep, url: u.String(), logs: logs}, nil
}

// lastLines keeps the end of a long message: PostgreSQL's tools say what
// went wrong last, after everything that went right.
func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return strings.Join(lines, "\n")
}

func (s *server) stop() error {
	err := s.ep.Stop()
	_ = s.logs.Close()
	return err
}

package local

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The download path, which a machine with PostgreSQL installed never takes:
// fetch the server, say so, create a ledger database with it, stop it, and
// start it again without fetching anything. It needs the network and is run
// only where downloads are allowed (TAUT_TEST_LOCAL_DOWNLOAD=1).
func TestDownloadedPostgres(t *testing.T) {
	if os.Getenv("TAUT_TEST_LOCAL_DOWNLOAD") != "1" || testing.Short() {
		t.Skip("set TAUT_TEST_LOCAL_DOWNLOAD=1 to let this test download PostgreSQL from Maven Central")
	}
	ctx := context.Background()
	cache, err := CacheDir()
	if err != nil {
		t.Fatal(err)
	}
	pg := Postgres{Dir: downloadDir(cache), Origin: "download"}
	state := filepath.Join(t.TempDir(), "postgres")

	var told []string
	tell := func(format string, args ...any) { told = append(told, fmt.Sprintf(format, args...)) }
	srv, err := startPostgres(ctx, pg, state, tell)
	if err != nil {
		t.Fatalf("starting a downloaded PostgreSQL: %v", err)
	}
	// Whether or not the archive was already in the cache, the operator is
	// told a download may happen, from where and into where.
	if len(told) != 1 || !strings.Contains(told[0], "repo1.maven.org") || !strings.Contains(told[0], cache) {
		t.Errorf("the operator was told: %q", told)
	}
	conn, err := pgx.Connect(ctx, srv.url)
	if err != nil {
		t.Fatalf("connecting to it: %v", err)
	}
	var version, collate string
	if err := conn.QueryRow(ctx, `SELECT current_setting('server_version'), datcollate FROM pg_database WHERE datname = current_database()`).Scan(&version, &collate); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close(ctx)
	if !strings.HasPrefix(version, "17.") || collate != "C" {
		t.Errorf("server version %s, collation %q; want 17 and C", version, collate)
	}
	if err := srv.stop(); err != nil {
		t.Fatalf("stopping it: %v", err)
	}
	if _, err := os.Stat(filepath.Join(state, "data", "postmaster.pid")); err == nil {
		t.Error("the server is still running after stop")
	}

	// Now it is in the cache, and found there like an installation.
	if !complete(pg.Dir) {
		t.Fatalf("%s is not a complete installation after the download", pg.Dir)
	}
	major, err := majorOf(ctx, pg.Dir)
	if err != nil || major != 17 {
		t.Fatalf("the cached PostgreSQL reports version %d, %v", major, err)
	}
	// A ledger made by 17 is not handed to another major version.
	_, err = startPostgres(ctx, Postgres{Dir: pg.Dir, Major: 16, Origin: "config"}, state, tell)
	if err == nil || !strings.Contains(err.Error(), "different PostgreSQL version") {
		t.Fatalf("a version-16 installation on a version-17 ledger: %v", err)
	}
	if dataMajor(filepath.Join(state, "data")) != 17 {
		t.Error("the refused start touched the ledger")
	}
	again, err := startPostgres(ctx, Postgres{Dir: pg.Dir, Major: 17, Origin: "cache"}, state, tell)
	if err != nil {
		t.Fatalf("starting it again from the cache: %v", err)
	}
	if err := again.stop(); err != nil {
		t.Fatal(err)
	}
}

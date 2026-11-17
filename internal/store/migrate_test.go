package store_test

import (
	"context"
	"sync"
	"testing"

	"github.com/bgfdev/taut/internal/store"
	"github.com/bgfdev/taut/internal/store/storetest"
)

func TestMigrateIdempotent(t *testing.T) {
	s := open(t) // already migrated once
	for range 3 {
		if err := s.Migrate(t.Context()); err != nil {
			t.Fatalf("Migrate again: %v", err)
		}
	}
	if n := scalar[int](t, s, `SELECT count(*) FROM schema_migrations`); n != schemaVersion {
		t.Fatalf("schema_migrations has %d rows, want %d", n, schemaVersion)
	}
	// The store works after repeated migration.
	newRun(t, s, "after-migrate")
}

// Eight separate pools stand in for eight processes starting at once on an
// empty schema.
func TestMigrateConcurrent(t *testing.T) {
	t.Parallel()
	url, schema := storetest.NewSchema(t)
	const n = 8
	stores := make([]*store.PG, n)
	for i := range stores {
		s, err := store.Open(t.Context(), url, store.WithSchema(schema), store.WithMaxConns(2))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(s.Close)
		stores[i] = s
	}
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i, s := range stores {
		wg.Go(func() {
			<-start
			errs[i] = s.Migrate(context.WithoutCancel(t.Context()))
		})
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("Migrate #%d: %v", i, err)
		}
	}
	if n := scalar[int](t, stores[0], `SELECT count(*) FROM schema_migrations`); n != schemaVersion {
		t.Fatalf("schema_migrations has %d rows, want %d", n, schemaVersion)
	}
	for _, table := range []string{"runs", "lanes", "batches", "batch_deps", "map_users", "map_teams",
		"map_channels", "map_messages", "map_files", "thread_anchors", "audit_log", "audit_log_heads"} {
		if n := scalar[int](t, stores[0], `SELECT count(*) FROM information_schema.tables WHERE table_schema = $1 AND table_name = $2`, schema, table); n != 1 {
			t.Errorf("table %s: found %d, want 1", table, n)
		}
	}
}

func TestOpenErrors(t *testing.T) {
	t.Parallel()
	if _, err := store.Open(t.Context(), "postgres://user:secret@127.0.0.1:1/nope?sslmode=disable&connect_timeout=2"); err == nil {
		t.Fatal("Open to a closed port succeeded")
	}
	_, err := store.Open(t.Context(), "postgres://user:hunter2@host:notaport/db")
	if err == nil {
		t.Fatal("Open with a malformed URL succeeded")
	}
	if got := err.Error(); containsAny(got, "hunter2") {
		t.Fatalf("error leaks the password: %s", got)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}

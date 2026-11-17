// Package storetest gives tests a real, isolated Postgres-backed store.
//
// Tests need a Postgres to talk to. Its URL is read from the environment
// variable TAUT_TEST_DATABASE_URL; when that is unset the test is skipped.
// Every call creates its own uniquely named schema, points the store's
// connections at it with search_path, migrates it, and drops it when the
// test ends, so tests can run in parallel and never see each other's rows.
package storetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bgfdev/taut/internal/store"
)

// EnvURL is the environment variable holding the test database's URL.
const EnvURL = "TAUT_TEST_DATABASE_URL"

// Open returns a migrated store in a fresh schema. It skips the test when
// TAUT_TEST_DATABASE_URL is unset. The store is closed and the schema
// dropped when the test ends.
func Open(t testing.TB) store.Store {
	t.Helper()
	return OpenPG(t)
}

// OpenPG is Open for tests that need the concrete type (for example its
// Pool, to inspect or arrange rows directly).
func OpenPG(t testing.TB) *store.PG {
	t.Helper()
	url, schema := NewSchema(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := store.Open(ctx, url, store.WithSchema(schema))
	if err != nil {
		t.Fatalf("storetest: open store: %v", err)
	}
	// Registered after NewSchema's cleanup, so it runs first: the pool is
	// closed before the schema is dropped.
	t.Cleanup(s.Close)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("storetest: migrate: %v", err)
	}
	return s
}

// NewSchema creates an empty, uniquely named schema and returns the
// database URL and the schema's name, for tests that open the store
// themselves (store.Open(ctx, url, store.WithSchema(schema))). It skips the
// test when TAUT_TEST_DATABASE_URL is unset. The schema is dropped when the
// test ends; close any store using it before then (a t.Cleanup registered
// after this call runs earlier).
func NewSchema(t testing.TB) (url, schema string) {
	t.Helper()
	url = os.Getenv(EnvURL)
	if url == "" {
		t.Skipf("%s is not set; skipping tests that need Postgres (for example %s=postgres://taut:taut-test@127.0.0.1:55432/taut_test?sslmode=disable)", EnvURL, EnvURL)
	}
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("storetest: %v", err)
	}
	schema = "taut_test_" + hex.EncodeToString(b[:])

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("storetest: connect to %s database: %v", EnvURL, err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{schema}.Sanitize()); err != nil {
		t.Fatalf("storetest: create schema: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, url)
		if err != nil {
			t.Logf("storetest: drop schema %s: %v", schema, err)
			return
		}
		defer conn.Close(ctx)
		if _, err := conn.Exec(ctx, "DROP SCHEMA IF EXISTS "+pgx.Identifier{schema}.Sanitize()+" CASCADE"); err != nil {
			t.Logf("storetest: drop schema %s: %v", schema, err)
		}
	})
	return url, schema
}

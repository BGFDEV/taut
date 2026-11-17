package store

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/bgfdev/taut/internal/fail"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

type migration struct {
	version  int
	name     string
	sql      string
	checksum string // hex SHA-256 of the file
}

var migrationFile = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

// loadMigrations reads the embedded migrations in version order and checks
// that versions are 1..n with no gaps or duplicates.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, err
	}
	var ms []migration
	for _, e := range entries {
		m := migrationFile.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migration file %q is not named NNNN_name.sql", e.Name())
		}
		v, _ := strconv.Atoi(m[1])
		body, err := fs.ReadFile(migrationFS, "migrations/"+e.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		ms = append(ms, migration{version: v, name: m[2], sql: string(body), checksum: hex.EncodeToString(sum[:])})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].version < ms[j].version })
	for i, m := range ms {
		if m.version != i+1 {
			return nil, fmt.Errorf("migrations must be numbered 0001, 0002, … without gaps; found %04d at position %d", m.version, i+1)
		}
	}
	return ms, nil
}

// migrateLockClass is the first key of the advisory lock Migrate takes. The
// second key is a hash of the current schema, so separate schemas in one
// database (tests) do not queue behind each other.
const migrateLockClass = 0x74617574 // "taut"

// Migrate brings the schema up to date.
//
// It takes a session-level Postgres advisory lock on a dedicated connection,
// so any number of processes may call it at once: one applies, the others
// wait and then find nothing to do. Each migration runs in its own
// transaction together with its schema_migrations row, so a migration is
// either fully applied and recorded or not applied at all. If the process
// dies mid-way the lock is released with its connection.
func (s *PG) Migrate(ctx context.Context) error {
	if err := s.migrate(ctx, 0); err != nil {
		return fail.Wrap(err, "store.migrate",
			"Could not bring the database schema up to date", "",
			"Check that the database user may create tables in its schema, then retry. If the message mentions a checksum or an unknown version, this binary and the database were built from different releases.")
	}
	return nil
}

// migrate applies pending migrations up to and including version upTo; zero
// means all of them. Stopping early exists for tests that need a database
// at an older schema version.
func (s *PG) migrate(ctx context.Context, upTo int) error {
	migs, err := loadMigrations()
	if err != nil {
		return err
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1, hashtext(current_schema()))`, migrateLockClass); err != nil {
		return fmt.Errorf("take migration lock: %w", err)
	}
	defer func() {
		// Unlock even if ctx is done; if this fails the connection is
		// broken and the server has dropped the lock with it.
		uctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(uctx, `SELECT pg_advisory_unlock($1, hashtext(current_schema()))`, migrateLockClass); err != nil {
			_ = conn.Conn().Close(uctx)
		}
	}()

	if _, err := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    integer PRIMARY KEY,
			name       text NOT NULL,
			checksum   text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[int]string{}
	rows, err := conn.Query(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var v int
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			rows.Close()
			return err
		}
		applied[v] = sum
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for v := range applied {
		if v > len(migs) {
			return fmt.Errorf("the database is at schema version %d but this binary only knows up to %d; use a newer taut", v, len(migs))
		}
	}

	for _, m := range migs {
		if upTo > 0 && m.version > upTo {
			break
		}
		if sum, ok := applied[m.version]; ok {
			if sum != m.checksum {
				return fmt.Errorf("migration %04d_%s was applied with a different checksum (database %s, binary %s)", m.version, m.name, sum, m.checksum)
			}
			continue
		}
		err := pgx.BeginTxFunc(ctx, conn, readCommitted, func(tx pgx.Tx) error {
			// The simple protocol lets one Exec carry the whole
			// multi-statement file.
			if _, err := tx.Exec(ctx, m.sql, pgx.QueryExecModeSimpleProtocol); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
				m.version, m.name, m.checksum)
			return err
		})
		if err != nil {
			return fmt.Errorf("apply migration %04d_%s: %w", m.version, m.name, err)
		}
	}
	return nil
}

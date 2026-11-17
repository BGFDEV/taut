package store

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bgfdev/taut/internal/fail"
)

// Errors returned by the Postgres store in addition to those in store.go.
var (
	// ErrTimestampTaken means PutMessages tried to insert a post whose
	// TargetMS is already allocated to a different post in the same
	// channel. The transaction is aborted; nothing from it is kept.
	ErrTimestampTaken = errors.New("store: target timestamp already allocated to another post in this channel")
	// ErrTargetConflict means a write would replace a Teams ID that is
	// already recorded with a different one, or would take a posted message
	// out of the posted state. Recorded Teams IDs are never overwritten.
	ErrTargetConflict = errors.New("store: a different Teams ID is already recorded")
	// ErrShiftLimit: a post has been moved from its allocated time as far
	// as it may be.
	ErrShiftLimit = errors.New("store: post cannot be moved further from its allocated time")
	// ErrOutOfOrder means CompleteBatch was called for a batch that is not
	// the lane's next runnable batch: an earlier batch of the lane is not
	// done, or one of its dependencies is not done.
	ErrOutOfOrder = errors.New("store: batch is not the lane's next runnable batch")
	// ErrInvalidPlan wraps every plan validation failure from ApplyPlan.
	ErrInvalidPlan = errors.New("store: invalid plan")
	// ErrLogCorrupt is wrapped by the *LogCorruptError VerifyLog returns.
	ErrLogCorrupt = errors.New("store: audit log does not verify")
)

// PG is the Postgres implementation of Store.
//
// # Clock
//
// All lease timing (expiry, takeover, retry backoff) is computed and compared
// with the database clock, now(), never with the caller's clock. Workers
// with skewed clocks therefore cannot disagree about whether a lease has
// expired: there is exactly one clock that decides. Lease.Expires is the
// database's value, reported for information; a holder should renew well
// before it rather than compare it with its own clock precisely. For the
// same reason there is no injectable clock.
//
// # Isolation
//
// Every transaction runs at READ COMMITTED, set explicitly so the store does
// not depend on the server's default_transaction_isolation. Correctness
// rests on row locks (see Claim and Fenced), not on snapshot isolation.
type PG struct {
	pool *pgxpool.Pool
}

var _ Store = (*PG)(nil)

type options struct {
	maxConns int32
	schema   string
}

// Option configures Open.
type Option func(*options)

// WithMaxConns sets the connection pool's size. Zero or negative keeps the
// default (pgxpool's: the larger of 4 and the number of CPUs, or pool_max_conns
// from the URL).
func WithMaxConns(n int) Option {
	return func(o *options) { o.maxConns = int32(n) }
}

// WithSchema makes every connection use schema as its search_path, so all
// tables live in, and are read from, that schema. The schema must exist.
// Empty keeps the server default (normally "public").
func WithSchema(schema string) Option {
	return func(o *options) { o.schema = schema }
}

// Open connects to Postgres and verifies the connection. It does not touch
// the schema; call Migrate.
func Open(ctx context.Context, databaseURL string, opts ...Option) (*PG, error) {
	var o options
	for _, opt := range opts {
		opt(&o)
	}
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		// The parse error can echo the URL, password included; do not
		// pass it on.
		return nil, fail.New("store.config",
			"The database URL is not valid",
			"it could not be parsed as a Postgres connection string",
			"Set it to something like postgres://user:password@host:5432/taut?sslmode=require.")
	}
	if o.maxConns > 0 {
		cfg.MaxConns = o.maxConns
	}
	if o.schema != "" {
		cfg.ConnConfig.RuntimeParams["search_path"] = pgx.Identifier{o.schema}.Sanitize()
	}
	if _, ok := cfg.ConnConfig.RuntimeParams["application_name"]; !ok {
		cfg.ConnConfig.RuntimeParams["application_name"] = "taut"
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, unreachable(err, cfg)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, unreachable(err, cfg)
	}
	return &PG{pool: pool}, nil
}

func unreachable(err error, cfg *pgxpool.Config) error {
	return fail.Wrap(err, "store.unreachable",
		fmt.Sprintf("Cannot connect to Postgres at %s:%d (database %q)",
			cfg.ConnConfig.Host, cfg.ConnConfig.Port, cfg.ConnConfig.Database),
		"",
		"Check that Postgres is running and that the database URL, user and password are right.")
}

// Close releases the pool.
func (s *PG) Close() { s.pool.Close() }

// Pool exposes the underlying pool for health checks, diagnostics and tests.
// Writing to Taut's tables through it bypasses fencing; don't.
func (s *PG) Pool() *pgxpool.Pool { return s.pool }

// NewRunID returns a new run ID: the UTC creation time to the microsecond
// followed by a random suffix, e.g. "20261113T235142.031544-k3v9q0d2mz".
// IDs sort lexicographically by creation time and contain only characters
// that are safe in URLs and file names.
func NewRunID() string {
	const alphabet = "0123456789abcdefghijklmnopqrstuv" // 32 symbols, 5 bits each
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("store: crypto/rand failed: " + err.Error())
	}
	for i := range b {
		b[i] = alphabet[b[i]&31]
	}
	return time.Now().UTC().Format("20060102T150405.000000") + "-" + string(b[:])
}

// --- helpers ---

var readCommitted = pgx.TxOptions{IsoLevel: pgx.ReadCommitted}

// repeatableRead is for multi-statement reads that must see one snapshot.
var repeatableRead = pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}

// inTx runs fn in a READ COMMITTED transaction.
func (s *PG) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return pgx.BeginTxFunc(ctx, s.pool, readCommitted, fn)
}

// querier is satisfied by both the pool and a transaction.
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// micros converts a duration to whole microseconds, the resolution of
// Postgres timestamps. SQL adds it as $n::bigint * interval '1 microsecond'.
func micros(d time.Duration) int64 { return d.Microseconds() }

// jsonArg converts opaque JSON to a query argument: NULL when empty,
// otherwise the text exactly as given (the columns are json, not jsonb, so
// the bytes are preserved).
func jsonArg(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

func checkJSON(what string, raw json.RawMessage) error {
	if len(raw) != 0 && !json.Valid(raw) {
		return fmt.Errorf("store: %s is not valid JSON", what)
	}
	return nil
}

func utc(t time.Time) time.Time { return t.UTC() }

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// pgCode returns the SQLSTATE and constraint name of a Postgres error.
func pgCode(err error) (code, constraint string) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code, pe.ConstraintName
	}
	return "", ""
}

const (
	sqlUniqueViolation     = "23505"
	sqlForeignKeyViolation = "23503"
)

package store

import "context"

// MigrateTo applies migrations up to and including version, so tests can
// build a database at an older schema version.
func MigrateTo(ctx context.Context, s *PG, version int) error { return s.migrate(ctx, version) }

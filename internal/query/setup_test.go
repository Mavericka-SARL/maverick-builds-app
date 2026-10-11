package query_test

import (
	"embed"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/testdb"
)

//go:embed testdata/*.sql
var testMigrations embed.FS

// setupDB returns a pool on a fresh database of the shared test container,
// migrated with this package's reduced schema (testdata/). The pool closes
// with the test; the returned func is kept for the callers' defer.
func setupDB(t *testing.T) (*pgxpool.Pool, func()) {
	t.Helper()
	return testdb.New(t, testMigrations, "testdata"), func() {}
}

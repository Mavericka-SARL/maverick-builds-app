// Package testdb hands every test a private, fully-migrated database while
// starting exactly one Postgres container per test binary.
//
// Each test fixture used to start its own container and run the full migration
// set into it. internal/gateway alone did that seventeen times in a single test
// binary, and the suite as a whole did it thirty-six times: minutes of the
// runtime were container startup and schema creation rather than testing.
//
// It also caused failures of its own. Under rootless Docker every container
// start races RootlessKit's port manager for a host port against the ephemeral
// ports the test processes' own connections are consuming, and enough of them
// starting at once meant some lost, with "bind: address already in use" on a
// line that had nothing to do with what was being tested.
//
// Isolation is unchanged. Migrations run once into a template database, and
// each caller gets a brand-new database cloned from it — a fresh schema with
// no rows from anyone else, which is exactly what a private container gave
// them, without the container.
package testdb

import (
	"context"
	"embed"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mavericks-engine/mavericks/pkg/db"
	"github.com/mavericks-engine/mavericks/pkg/migrate"
)

type shared struct {
	adminDSN string // DSN for the container's own bootstrap database
	err      error
}

var (
	once sync.Once
	inst shared

	mu sync.Mutex
	// One template per migration set, built on first use. A package can
	// legitimately need more than one — internal/gateway's trigger catalogue
	// test runs against a trimmed testdata schema while every other test in
	// that binary uses the real migrations — and they can still share the
	// container, which is where the cost is.
	templates = map[string]string{}
	seq       int
)

// New returns a pool to a private, fully-migrated database.
//
// fsys/dir name the migration set. They are read once per test binary, to build
// the template every later call clones — so every caller in one package must
// ask for the same set, and asking for a second one is a mistake this reports
// rather than silently ignores.
func New(t *testing.T, fsys embed.FS, dir string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	once.Do(func() { inst = start(ctx) })
	if inst.err != nil {
		t.Fatalf("shared test database: %v", inst.err)
	}

	template, err := templateFor(ctx, fsys, dir)
	if err != nil {
		t.Fatalf("build template for %q: %v", dir, err)
	}

	mu.Lock()
	seq++
	name := fmt.Sprintf("mavericks_t%d", seq)
	mu.Unlock()

	// CREATE DATABASE cannot run inside a transaction and needs a connection
	// to something other than the database being created, so this borrows one
	// against the container's own bootstrap database and closes it again.
	admin, err := db.Connect(ctx, inst.adminDSN)
	if err != nil {
		t.Fatalf("connect to create %s: %v", name, err)
	}
	_, err = admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s TEMPLATE %s`, name, template))
	admin.Close()
	if err != nil {
		t.Fatalf("create database %s: %v", name, err)
	}

	pool, err := db.Connect(ctx, dsnFor(inst.adminDSN, name))
	if err != nil {
		t.Fatalf("connect to %s: %v", name, err)
	}
	// Only the pool is closed. Dropping the database would need yet another
	// connection and buys nothing: the container is thrown away when the
	// binary exits.
	t.Cleanup(pool.Close)
	return pool
}

// AdminDSN is the shared container's bootstrap connection string: a database
// with CREATEDB rights on the same server every New database lives on. It is
// what a test of database provisioning (pkg/tenantdb) needs — creating and
// dropping sibling databases on the server the control plane runs on.
func AdminDSN(t *testing.T) string {
	t.Helper()
	once.Do(func() { inst = start(context.Background()) })
	if inst.err != nil {
		t.Fatalf("shared test database: %v", inst.err)
	}
	return inst.adminDSN
}

func start(ctx context.Context) shared {
	pgc, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("mavericks"),
		tcpostgres.WithUsername("mavericks"),
		tcpostgres.WithPassword("mavericks"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
		),
	)
	if err != nil {
		return shared{err: fmt.Errorf("start postgres: %w", err)}
	}
	// Deliberately not terminated: there is no process-wide teardown hook here,
	// and Ryuk (or the daemon's own cleanup) reaps it when the run ends.

	adminDSN, err := pgc.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return shared{err: fmt.Errorf("connection string: %w", err)}
	}

	return shared{adminDSN: adminDSN}
}

// templateFor builds (once per migration set) a database holding that set's
// schema, which every later caller clones. Migrating once and copying is what
// removes the per-test schema build, not just the per-test container.
func templateFor(ctx context.Context, fsys embed.FS, dir string) (string, error) {
	mu.Lock()
	defer mu.Unlock()
	if name, ok := templates[dir]; ok {
		return name, nil
	}

	name := fmt.Sprintf("mavericks_tmpl%d", len(templates)+1)
	admin, err := db.Connect(ctx, inst.adminDSN)
	if err != nil {
		return "", err
	}
	_, err = admin.Exec(ctx, `CREATE DATABASE `+name)
	admin.Close()
	if err != nil {
		return "", fmt.Errorf("create template database: %w", err)
	}

	// The pool is closed straight after migrating: CREATE DATABASE ... TEMPLATE
	// refuses to run while anything else is connected to the template.
	tmpl, err := db.Connect(ctx, dsnFor(inst.adminDSN, name))
	if err != nil {
		return "", fmt.Errorf("connect to template: %w", err)
	}
	err = migrate.Run(ctx, tmpl, fsys, dir)
	tmpl.Close()
	if err != nil {
		return "", fmt.Errorf("migrate template: %w", err)
	}

	templates[dir] = name
	return name, nil
}

// dsnFor swaps the database name in a DSN, keeping host, credentials and
// query string intact.
func dsnFor(dsn, database string) string {
	rest := dsn
	var query string
	if i := strings.Index(rest, "?"); i >= 0 {
		query, rest = rest[i:], rest[:i]
	}
	if i := strings.LastIndex(rest, "/"); i >= 0 {
		rest = rest[:i+1] + database
	}
	return rest + query
}

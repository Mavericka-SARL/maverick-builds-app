package migrate_test

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/db"
	"github.com/mavericks-engine/mavericks/pkg/migrate"
)

func startPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	pgc, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("mavericks"),
		tcpostgres.WithUsername("mavericks"),
		tcpostgres.WithPassword("mavericks"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = pgc.Terminate(ctx) })

	dsn, err := pgc.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestRun_ConcurrentCallsAreSerializedByAdvisoryLock is a regression test
// for the exact race this batch fixes: without the advisory lock, multiple
// processes calling Run() concurrently against a fresh (unmigrated)
// database — e.g. two gateway pods booting mid-rolling-update — could both
// see the same migration as unapplied and both execute it, racing on the
// _migrations INSERT. This reuses the real production migration set
// (migrationfs.FS) rather than synthetic testdata, so it exercises the
// real-world race window across every real migration file.
func TestRun_ConcurrentCallsAreSerializedByAdvisoryLock(t *testing.T) {
	pool := startPostgres(t)

	const n = 3
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = migrate.Run(context.Background(), pool, migrationfs.FS, ".")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: Run returned error: %v", i, err)
		}
	}

	// Sanity: every real migration file landed exactly once — no
	// duplicates from a race, no partial application from an error.
	entries, err := migrationfs.FS.ReadDir(".")
	if err != nil {
		t.Fatalf("read real migrations dir: %v", err)
	}
	wantCount := 0
	for _, e := range entries {
		if !e.IsDir() {
			wantCount++
		}
	}

	var gotCount int
	if err := pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM _migrations`).Scan(&gotCount); err != nil {
		t.Fatalf("count _migrations: %v", err)
	}
	if gotCount != wantCount {
		t.Errorf("_migrations row count = %d, want %d (the real migrations/ file count)", gotCount, wantCount)
	}
}

// TestRun_SecondCallIsANoOp is a sequential-idempotency sanity check
// (pre-existing behavior, unaffected by the lock): calling Run() twice in a
// row against an already-migrated database must succeed both times.
func TestRun_SecondCallIsANoOp(t *testing.T) {
	pool := startPostgres(t)
	ctx := context.Background()

	if err := migrate.Run(ctx, pool, migrationfs.FS, "."); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	if err := migrate.Run(ctx, pool, migrationfs.FS, "."); err != nil {
		t.Fatalf("second Run: %v", err)
	}
}

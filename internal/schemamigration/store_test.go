package schemamigration

import (
	"context"
	"embed"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mavericks-engine/mavericks/pkg/db"
	"github.com/mavericks-engine/mavericks/pkg/migrate"
)

//go:embed testdata/*.sql
var testMigrations embed.FS

func setupDB(t *testing.T) (*Store, func()) {
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

	dsn, err := pgc.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		_ = pgc.Terminate(ctx)
		t.Fatalf("connection string: %v", err)
	}

	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		_ = pgc.Terminate(ctx)
		t.Fatalf("connect: %v", err)
	}

	if err := migrate.Run(ctx, pool, testMigrations, "testdata"); err != nil {
		pool.Close()
		_ = pgc.Terminate(ctx)
		t.Fatalf("migrate: %v", err)
	}

	return NewStore(pool), func() {
		pool.Close()
		_ = pgc.Terminate(ctx)
	}
}

// insertModel creates the minimal core hierarchy and returns a model ID.
func insertModel(t *testing.T, store *Store) string {
	t.Helper()
	ctx := context.Background()

	var modelID string
	err := store.pool.QueryRow(ctx, `
		WITH
		  c AS (INSERT INTO core.customer (name) VALUES ('test-customer') RETURNING id),
		  w AS (INSERT INTO core.workspace (customer_id, name) SELECT id, 'test-ws' FROM c RETURNING id),
		  a AS (INSERT INTO core.application (workspace_id, name) SELECT id, 'test-app' FROM w RETURNING id)
		INSERT INTO core.model (application_id, name) SELECT id, 'test-model' FROM a RETURNING id::text
	`).Scan(&modelID)
	if err != nil {
		t.Fatalf("insert model: %v", err)
	}
	return modelID
}

// sampleFiles returns two trivial migration files with valid PostgreSQL.
func sampleFiles() []migrationFile {
	return []migrationFile{
		{
			Filename:    "0001_test_up.sql",
			SQL:         "CREATE TABLE IF NOT EXISTS _sm_test_apply (id serial PRIMARY KEY)",
			RollbackSQL: "DROP TABLE IF EXISTS _sm_test_apply",
			Checksum:    "deadbeef01",
		},
	}
}

func TestNextVersion_Empty(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	modelID := insertModel(t, store)

	ver, err := store.NextVersion(context.Background(), modelID)
	if err != nil {
		t.Fatalf("NextVersion: %v", err)
	}
	if ver != 1 {
		t.Errorf("NextVersion = %d, want 1", ver)
	}
}

func TestCreateAndGet(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := insertModel(t, store)

	files := sampleFiles()
	rec, err := store.Create(ctx, modelID, "", 1, files)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rec.Status != "pending" {
		t.Errorf("status = %q, want pending", rec.Status)
	}
	if rec.VersionNumber != 1 {
		t.Errorf("version_number = %d, want 1", rec.VersionNumber)
	}

	got, err := store.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != rec.ID {
		t.Errorf("ID mismatch: got %s, want %s", got.ID, rec.ID)
	}
	if len(got.Files) != 1 {
		t.Errorf("files = %d, want 1", len(got.Files))
	}
}

func TestNextVersion_Increments(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := insertModel(t, store)

	if _, err := store.Create(ctx, modelID, "", 1, sampleFiles()); err != nil {
		t.Fatalf("Create v1: %v", err)
	}

	ver, err := store.NextVersion(ctx, modelID)
	if err != nil {
		t.Fatalf("NextVersion: %v", err)
	}
	if ver != 2 {
		t.Errorf("NextVersion = %d, want 2", ver)
	}
}

func TestList(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := insertModel(t, store)

	for i := int32(1); i <= 3; i++ {
		if _, err := store.Create(ctx, modelID, "", i, sampleFiles()); err != nil {
			t.Fatalf("Create v%d: %v", i, err)
		}
	}

	records, err := store.List(ctx, modelID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) != 3 {
		t.Errorf("len(records) = %d, want 3", len(records))
	}
	// Should be ordered by version_number ASC
	for i, r := range records {
		if r.VersionNumber != int32(i+1) {
			t.Errorf("records[%d].VersionNumber = %d, want %d", i, r.VersionNumber, i+1)
		}
	}
}

func TestApply(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := insertModel(t, store)

	rec, err := store.Create(ctx, modelID, "", 1, sampleFiles())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := store.Apply(ctx, rec.ID); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got, err := store.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Get after apply: %v", err)
	}
	if got.Status != "applied" {
		t.Errorf("status = %q, want applied", got.Status)
	}
	if got.AppliedAt == nil {
		t.Error("applied_at is nil after apply")
	}
}

func TestApply_NonPendingFails(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := insertModel(t, store)

	rec, err := store.Create(ctx, modelID, "", 1, sampleFiles())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Apply(ctx, rec.ID); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Second Apply on already-applied record must fail
	if err := store.Apply(ctx, rec.ID); err == nil {
		t.Error("expected error applying non-pending migration, got nil")
	}
}

func TestRollback(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	modelID := insertModel(t, store)

	rec, err := store.Create(ctx, modelID, "", 1, sampleFiles())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.Apply(ctx, rec.ID); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := store.Rollback(ctx, rec.ID); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	got, err := store.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Get after rollback: %v", err)
	}
	if got.Status != "rolled_back" {
		t.Errorf("status = %q, want rolled_back", got.Status)
	}
}

func TestGet_NotFound(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()

	_, err := store.Get(context.Background(), "00000000-0000-0000-0000-000000000000")
	if err == nil {
		t.Error("expected error for unknown migration ID, got nil")
	}
}

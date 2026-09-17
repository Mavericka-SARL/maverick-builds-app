package model_test

import (
	"context"
	"embed"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/model"
	"github.com/mavericks-engine/mavericks/internal/testdb"
)

//go:embed testdata/*.sql
var testMigrations embed.FS

func setupDB(t *testing.T) *model.Store {
	t.Helper()

	pool := testdb.New(t, testMigrations, "testdata")
	return model.NewStore(pool)
}

func TestCreateAndListDimensions(t *testing.T) {
	store := setupDB(t)
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-000000000001"

	d, err := store.CreateDimension(ctx, modelID, "Department", nil)
	if err != nil {
		t.Fatalf("create dimension: %v", err)
	}
	if d.Name != "Department" {
		t.Errorf("name = %q, want Department", d.Name)
	}

	dims, err := store.ListDimensions(ctx, modelID, 10, 0)
	if err != nil {
		t.Fatalf("list dimensions: %v", err)
	}
	if len(dims) != 1 {
		t.Errorf("len(dims) = %d, want 1", len(dims))
	}
}

func TestDuplicateDimensionRejected(t *testing.T) {
	store := setupDB(t)
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-000000000002"

	if _, err := store.CreateDimension(ctx, modelID, "Region", nil); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, err := store.CreateDimension(ctx, modelID, "Region", nil)
	if err == nil {
		t.Fatal("expected duplicate error, got nil")
	}
}

func TestCreateMetricAndDependencies(t *testing.T) {
	store := setupDB(t)
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-000000000003"

	rev, err := store.CreateMetric(ctx, modelID, "revenue", "", "oltp", true)
	if err != nil {
		t.Fatalf("create revenue: %v", err)
	}
	cogs, err := store.CreateMetric(ctx, modelID, "cogs", "", "oltp", true)
	if err != nil {
		t.Fatalf("create cogs: %v", err)
	}
	gross, err := store.CreateMetric(ctx, modelID, "gross_profit", "{revenue} - {cogs}", "oltp", false)
	if err != nil {
		t.Fatalf("create gross_profit: %v", err)
	}

	if err := store.UpsertDependencies(ctx, gross.Id, []string{rev.Id, cogs.Id}); err != nil {
		t.Fatalf("upsert dependencies: %v", err)
	}

	graph, names, err := store.LoadDependencyGraph(ctx, modelID)
	if err != nil {
		t.Fatalf("load dependency graph: %v", err)
	}
	cycles := model.DetectCycles(graph, names)
	if len(cycles) != 0 {
		t.Errorf("unexpected cycles: %v", cycles)
	}
}

func TestCycleDetectionViaStore(t *testing.T) {
	store := setupDB(t)
	ctx := context.Background()
	modelID := "00000000-0000-0000-0000-000000000004"

	a, err := store.CreateMetric(ctx, modelID, "a", "{b}", "oltp", false)
	if err != nil {
		t.Fatalf("create metric a: %v", err)
	}
	b, err := store.CreateMetric(ctx, modelID, "b", "{a}", "oltp", false)
	if err != nil {
		t.Fatalf("create metric b: %v", err)
	}

	_ = store.UpsertDependencies(ctx, a.Id, []string{b.Id})
	_ = store.UpsertDependencies(ctx, b.Id, []string{a.Id})

	graph, names, err := store.LoadDependencyGraph(ctx, modelID)
	if err != nil {
		t.Fatalf("load graph: %v", err)
	}
	cycles := model.DetectCycles(graph, names)
	if len(cycles) == 0 {
		t.Error("expected cycle to be detected")
	}
}

func TestPublishRevision(t *testing.T) {
	store := setupDB(t)
	ctx := context.Background()

	// Need a core.model row first
	var modelID string
	err := store.Pool().QueryRow(ctx,
		`INSERT INTO core.model (application_id, name) VALUES (gen_random_uuid(), 'OPEX') RETURNING id::text`,
	).Scan(&modelID)
	if err != nil {
		t.Fatalf("insert model: %v", err)
	}

	// Add a metric so the model is non-empty
	if _, err := store.CreateMetric(ctx, modelID, "headcount", "", "oltp", true); err != nil {
		t.Fatalf("create metric: %v", err)
	}

	rev, err := store.PublishRevision(ctx, modelID)
	if err != nil {
		t.Fatalf("publish revision: %v", err)
	}
	if rev.VersionNumber != 1 {
		t.Errorf("version_number = %d, want 1", rev.VersionNumber)
	}
	if rev.SchemaHash == "" {
		t.Error("schema_hash should not be empty")
	}

	// Publishing again should increment the version
	rev2, err := store.PublishRevision(ctx, modelID)
	if err != nil {
		t.Fatalf("second publish: %v", err)
	}
	if rev2.VersionNumber != 2 {
		t.Errorf("version_number = %d, want 2", rev2.VersionNumber)
	}

	// GetRevision should return the first revision
	got, err := store.GetRevision(ctx, rev.Id)
	if err != nil {
		t.Fatalf("get revision: %v", err)
	}
	if got.SchemaHash != rev.SchemaHash {
		t.Errorf("schema_hash mismatch: got %q want %q", got.SchemaHash, rev.SchemaHash)
	}
}

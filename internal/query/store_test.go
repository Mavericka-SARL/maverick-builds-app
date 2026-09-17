package query_test

import (
	"context"
	"embed"
	"errors"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	queryv1 "github.com/mavericks-engine/mavericks/gen/go/query/v1"
	"github.com/mavericks-engine/mavericks/internal/query"
	"github.com/mavericks-engine/mavericks/pkg/db"
	"github.com/mavericks-engine/mavericks/pkg/migrate"
)

//go:embed testdata/*.sql
var testMigrations embed.FS

func setupDB(t *testing.T) (*query.Store, func()) {
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

	return query.NewStore(pool), func() {
		pool.Close()
		_ = pgc.Terminate(ctx)
	}
}

func insertMetric(t *testing.T, store *query.Store, modelID, name string, isInput bool) string {
	t.Helper()
	var id string
	err := store.Pool().QueryRow(context.Background(), `
		INSERT INTO model.metric_def (model_id, name, is_input)
		VALUES ($1::uuid, $2, $3) RETURNING id::text
	`, modelID, name, isInput).Scan(&id)
	if err != nil {
		t.Fatalf("insert metric %s: %v", name, err)
	}
	return id
}

// insertRevision seeds a (non-system-managed) model.revision row with the
// given id — Store.Writeback now runs writeguard.CheckWrite, which requires
// the revision to actually exist.
func insertRevision(t *testing.T, store *query.Store, revisionID, modelID string) {
	t.Helper()
	if _, err := store.Pool().Exec(context.Background(), `
		INSERT INTO model.revision (id, model_id, system_managed) VALUES ($1::uuid, $2::uuid, false)
	`, revisionID, modelID); err != nil {
		t.Fatalf("insert revision: %v", err)
	}
}

func TestWriteback(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	modelID := "00000000-0000-0000-0000-000000000001"
	revisionID := "00000000-0000-0000-0001-000000000001"
	userID := "00000000-0000-0000-0000-000000000099"
	metricID := insertMetric(t, store, modelID, "headcount", true)
	insertRevision(t, store, revisionID, modelID)

	updates := []*queryv1.WritebackUpdate{
		{DimMembers: map[string]string{"cost_center": "CC-001"}, MetricId: metricID, Value: 42},
		{DimMembers: map[string]string{"cost_center": "CC-002"}, MetricId: metricID, Value: 18},
	}

	result, err := store.Writeback(ctx, modelID, revisionID, userID, updates)
	if err != nil {
		t.Fatalf("Writeback: %v", err)
	}
	if result.CellsWritten != 2 {
		t.Errorf("CellsWritten = %d, want 2", result.CellsWritten)
	}
	if len(result.MetricIDs) != 1 || result.MetricIDs[0] != metricID {
		t.Errorf("MetricIDs = %v, want [%s]", result.MetricIDs, metricID)
	}
}

// TestWritebackRejectsHiddenMetric is a regression test: Writeback used to
// only check writeguard.CheckWrite (dimension-member access), never
// writeguard.MetricAccess — a user hidden/read-restricted from a metric
// could still write it via gRPC Writeback even though the identical HTTP
// /api/cells write is rejected.
func TestWritebackRejectsHiddenMetric(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	modelID := "00000000-0000-0000-0000-000000000010"
	revisionID := "00000000-0000-0000-0010-000000000010"
	userID := "00000000-0000-0000-0000-000000000098"
	metricID := insertMetric(t, store, modelID, "bonus_pool", true)
	insertRevision(t, store, revisionID, modelID)

	if _, err := store.Pool().Exec(ctx, `
		INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'metric', $2::uuid, 'hidden')
	`, userID, metricID); err != nil {
		t.Fatalf("seed hidden metric rule: %v", err)
	}

	_, err := store.Writeback(ctx, modelID, revisionID, userID, []*queryv1.WritebackUpdate{
		{DimMembers: map[string]string{}, MetricId: metricID, Value: 42},
	})
	if err == nil {
		t.Fatal("Writeback: want an error (metric is hidden for this user), got nil")
	}
	if !errors.Is(err, query.ErrWriteDenied) {
		t.Errorf("Writeback error = %v, want it to wrap query.ErrWriteDenied", err)
	}

	cells, cErr := store.QueryCells(ctx, modelID, revisionID, []string{metricID}, nil)
	if cErr != nil {
		t.Fatalf("QueryCells: %v", cErr)
	}
	if len(cells) != 0 {
		t.Errorf("cells after rejected writeback = %v, want none written", cells)
	}
}

func TestQueryCells_InputMetrics(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	modelID := "00000000-0000-0000-0000-000000000002"
	revisionID := "00000000-0000-0000-0002-000000000002"
	userID := "00000000-0000-0000-0000-000000000099"
	revenueID := insertMetric(t, store, modelID, "revenue", true)
	insertRevision(t, store, revisionID, modelID)

	_, err := store.Writeback(ctx, modelID, revisionID, userID, []*queryv1.WritebackUpdate{
		{DimMembers: map[string]string{}, MetricId: revenueID, Value: 1000},
	})
	if err != nil {
		t.Fatalf("Writeback: %v", err)
	}

	cells, err := store.QueryCells(ctx, modelID, revisionID, []string{revenueID}, nil)
	if err != nil {
		t.Fatalf("QueryCells: %v", err)
	}
	if len(cells) != 1 {
		t.Fatalf("expected 1 cell, got %d", len(cells))
	}
	if cells[0].Value != 1000 {
		t.Errorf("value = %v, want 1000", cells[0].Value)
	}
	if !cells[0].IsInput {
		t.Error("expected IsInput=true for writeback cell")
	}
}

func TestQueryCells_CalcResults(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	modelID := "00000000-0000-0000-0000-000000000003"
	revisionID := "00000000-0000-0000-0003-000000000003"
	grossProfitID := insertMetric(t, store, modelID, "gross_profit", false)

	_, err := store.Pool().Exec(ctx, `
		INSERT INTO runtime.calc_result (model_id, revision_id, metric_id, dim_members, value, partition_key)
		VALUES ($1::uuid, $2::uuid, $3::uuid, '{}', 400, 'pk-test')
	`, modelID, revisionID, grossProfitID)
	if err != nil {
		t.Fatalf("insert calc_result: %v", err)
	}

	cells, err := store.QueryCells(ctx, modelID, revisionID, []string{grossProfitID}, nil)
	if err != nil {
		t.Fatalf("QueryCells: %v", err)
	}
	if len(cells) != 1 {
		t.Fatalf("expected 1 cell, got %d", len(cells))
	}
	if cells[0].Value != 400 {
		t.Errorf("value = %v, want 400", cells[0].Value)
	}
	if cells[0].IsInput {
		t.Error("expected IsInput=false for calc result")
	}
}

func TestGetCell_CalcResult(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	modelID := "00000000-0000-0000-0000-000000000004"
	revisionID := "00000000-0000-0000-0004-000000000004"
	metricID := insertMetric(t, store, modelID, "total_opex", false)

	_, err := store.Pool().Exec(ctx, `
		INSERT INTO runtime.calc_result (model_id, revision_id, metric_id, dim_members, value, partition_key)
		VALUES ($1::uuid, $2::uuid, $3::uuid, '{"dept":"eng"}', 99.5, 'pk-test')
	`, modelID, revisionID, metricID)
	if err != nil {
		t.Fatalf("insert calc_result: %v", err)
	}

	cell, err := store.GetCell(ctx, modelID, revisionID, metricID, map[string]string{"dept": "eng"})
	if err != nil {
		t.Fatalf("GetCell: %v", err)
	}
	if cell.Value != 99.5 {
		t.Errorf("value = %v, want 99.5", cell.Value)
	}
}

func TestGetCell_FallbackToFactInput(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	modelID := "00000000-0000-0000-0000-000000000005"
	revisionID := "00000000-0000-0000-0005-000000000005"
	userID := "00000000-0000-0000-0000-000000000099"
	metricID := insertMetric(t, store, modelID, "salary", true)
	insertRevision(t, store, revisionID, modelID)

	_, err := store.Writeback(ctx, modelID, revisionID, userID, []*queryv1.WritebackUpdate{
		{DimMembers: map[string]string{"dept": "eng"}, MetricId: metricID, Value: 150000},
	})
	if err != nil {
		t.Fatalf("Writeback: %v", err)
	}

	cell, err := store.GetCell(ctx, modelID, revisionID, metricID, map[string]string{"dept": "eng"})
	if err != nil {
		t.Fatalf("GetCell: %v", err)
	}
	if cell.Value != 150000 {
		t.Errorf("value = %v, want 150000", cell.Value)
	}
	if !cell.IsInput {
		t.Error("expected IsInput=true when falling back to fact_input")
	}
}

func TestQueryCells_DimMemberFiltering(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	modelID := "00000000-0000-0000-0000-000000000006"
	revisionID := "00000000-0000-0000-0006-000000000006"
	userID := "00000000-0000-0000-0000-000000000099"
	metricID := insertMetric(t, store, modelID, "spend", true)
	insertRevision(t, store, revisionID, modelID)

	_, err := store.Writeback(ctx, modelID, revisionID, userID, []*queryv1.WritebackUpdate{
		{DimMembers: map[string]string{"dept": "eng"}, MetricId: metricID, Value: 100},
		{DimMembers: map[string]string{"dept": "mkt"}, MetricId: metricID, Value: 200},
	})
	if err != nil {
		t.Fatalf("Writeback: %v", err)
	}

	cells, err := store.QueryCells(ctx, modelID, revisionID, []string{metricID}, []string{"eng"})
	if err != nil {
		t.Fatalf("QueryCells with filter: %v", err)
	}
	if len(cells) != 1 {
		t.Fatalf("expected 1 cell after dim filter, got %d", len(cells))
	}
	if cells[0].Value != 100 {
		t.Errorf("value = %v, want 100", cells[0].Value)
	}
}

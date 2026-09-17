package importpkg_test

import (
	"context"
	"embed"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	importpkgv1 "github.com/mavericks-engine/mavericks/gen/go/importpkg/v1"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
	"github.com/mavericks-engine/mavericks/pkg/db"
	"github.com/mavericks-engine/mavericks/pkg/migrate"
)

//go:embed testdata/*.sql
var testMigrations embed.FS

func setupDB(t *testing.T) (*importpkg.Store, func()) {
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

	return importpkg.NewStore(pool), func() {
		pool.Close()
		_ = pgc.Terminate(ctx)
	}
}

// insertFixtures creates the minimum identity + model rows needed by the import tests.
func insertFixtures(t *testing.T, store *importpkg.Store) (userID, modelID, metricID string) {
	t.Helper()
	ctx := context.Background()

	if err := store.Pool().QueryRow(ctx, `
		INSERT INTO identity.user (email) VALUES ('importer@example.com') RETURNING id::text
	`).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	var custID, wsID, appID string
	store.Pool().QueryRow(ctx, `INSERT INTO core.customer (name) VALUES ('C1') RETURNING id::text`).Scan(&custID)                                 //nolint:errcheck
	store.Pool().QueryRow(ctx, `INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'W1') RETURNING id::text`, custID).Scan(&wsID)   //nolint:errcheck
	store.Pool().QueryRow(ctx, `INSERT INTO core.application (workspace_id, name) VALUES ($1::uuid, 'A1') RETURNING id::text`, wsID).Scan(&appID) //nolint:errcheck

	if err := store.Pool().QueryRow(ctx, `
		INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M1') RETURNING id::text
	`, appID).Scan(&modelID); err != nil {
		t.Fatalf("insert model: %v", err)
	}

	if err := store.Pool().QueryRow(ctx, `
		INSERT INTO model.metric_def (model_id, name, is_input) VALUES ($1::uuid, 'revenue', true) RETURNING id::text
	`, modelID).Scan(&metricID); err != nil {
		t.Fatalf("insert metric: %v", err)
	}

	return userID, modelID, metricID
}

func TestCreateAndGetImportJob(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	userID, modelID, _ := insertFixtures(t, store)

	job, err := store.CreateImportJob(ctx, modelID, "", "file://test.csv", userID, nil)
	if err != nil {
		t.Fatalf("CreateImportJob: %v", err)
	}
	if job.Id == "" {
		t.Fatal("expected non-empty job ID")
	}
	if job.Status != importpkgv1.ImportStatus_IMPORT_STATUS_VALIDATING {
		t.Errorf("status = %v, want VALIDATING", job.Status)
	}

	fetched, err := store.GetImportJob(ctx, job.Id)
	if err != nil {
		t.Fatalf("GetImportJob: %v", err)
	}
	if fetched.ModelId != modelID {
		t.Errorf("model_id = %q, want %q", fetched.ModelId, modelID)
	}
}

func TestStageRowsAndCommit(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	userID, modelID, metricID := insertFixtures(t, store)

	job, _ := store.CreateImportJob(ctx, modelID, "", "file://test.csv", userID, nil)

	rows := []importpkg.StagingRow{
		{MetricID: metricID, DimMembers: map[string]string{"region": "EMEA"}, Value: 1000.0, RowNumber: 1},
		{MetricID: metricID, DimMembers: map[string]string{"region": "APAC"}, Value: 2000.0, RowNumber: 2},
	}

	if err := store.StageRows(ctx, job.Id, rows, nil); err != nil {
		t.Fatalf("StageRows: %v", err)
	}

	// Verify job status is staged
	staged, _ := store.GetImportJob(ctx, job.Id)
	if staged.Status != importpkgv1.ImportStatus_IMPORT_STATUS_STAGED {
		t.Errorf("after staging: status = %v, want STAGED", staged.Status)
	}
	if staged.ValidRows != 2 {
		t.Errorf("valid_rows = %d, want 2", staged.ValidRows)
	}

	// Commit: flush staging → fact_input
	if _, err := store.CommitImport(ctx, job.Id, modelID, "", userID, importpkg.ModeIncremental); err != nil {
		t.Fatalf("CommitImport: %v", err)
	}

	committed, _ := store.GetImportJob(ctx, job.Id)
	if committed.Status != importpkgv1.ImportStatus_IMPORT_STATUS_COMMITTED {
		t.Errorf("after commit: status = %v, want COMMITTED", committed.Status)
	}

	// Verify rows landed in fact_input
	var count int
	store.Pool().QueryRow(ctx, `SELECT COUNT(*) FROM runtime.fact_input WHERE model_id = $1::uuid`, modelID).Scan(&count) //nolint:errcheck
	if count != 2 {
		t.Errorf("fact_input rows = %d, want 2", count)
	}
}

// TestCommitImportRejectsHiddenMetric is a regression test: CommitImport
// used to only run writeguard.CheckWrite (dimension-member access), never
// writeguard.MetricAccess — a user hidden/read-restricted from a metric
// could still write it by importing a CSV, even though the identical
// direct cell write (writeguard.MetricAccess, used by /api/cells) is
// rejected.
func TestCommitImportRejectsHiddenMetric(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	userID, modelID, metricID := insertFixtures(t, store)

	if _, err := store.Pool().Exec(ctx, `
		INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'metric', $2::uuid, 'hidden')
	`, userID, metricID); err != nil {
		t.Fatalf("seed hidden metric rule: %v", err)
	}

	job, _ := store.CreateImportJob(ctx, modelID, "", "file://test.csv", userID, nil)
	rows := []importpkg.StagingRow{
		{MetricID: metricID, DimMembers: map[string]string{"region": "EMEA"}, Value: 1000.0, RowNumber: 1},
	}
	if err := store.StageRows(ctx, job.Id, rows, nil); err != nil {
		t.Fatalf("StageRows: %v", err)
	}

	if _, err := store.CommitImport(ctx, job.Id, modelID, "", userID, importpkg.ModeIncremental); err == nil {
		t.Fatal("CommitImport: want an error (metric is hidden for this user), got nil")
	} else if !errors.Is(err, importpkg.ErrWriteDenied) {
		t.Errorf("CommitImport error = %v, want it to wrap importpkg.ErrWriteDenied", err)
	}

	var count int
	store.Pool().QueryRow(ctx, `SELECT COUNT(*) FROM runtime.fact_input WHERE model_id = $1::uuid`, modelID).Scan(&count) //nolint:errcheck
	if count != 0 {
		t.Errorf("fact_input rows after rejected commit = %d, want 0", count)
	}
}

func TestStageRowsWithErrors(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	userID, modelID, metricID := insertFixtures(t, store)

	job, _ := store.CreateImportJob(ctx, modelID, "", "file://test.csv", userID, nil)

	rows := []importpkg.StagingRow{
		{MetricID: metricID, DimMembers: nil, Value: 500.0, RowNumber: 1},
	}
	errs := []*importpkgv1.ImportError{
		{RowNumber: 2, Column: "value", ErrorCode: "INVALID_NUMBER", Message: "cannot parse 'abc'", RawValue: "abc"},
	}

	if err := store.StageRows(ctx, job.Id, rows, errs); err != nil {
		t.Fatalf("StageRows: %v", err)
	}

	staged, _ := store.GetImportJob(ctx, job.Id)
	if staged.ValidRows != 1 {
		t.Errorf("valid_rows = %d, want 1", staged.ValidRows)
	}
	if staged.ErrorRows != 1 {
		t.Errorf("error_rows = %d, want 1", staged.ErrorRows)
	}

	importErrs, err := store.GetImportErrors(ctx, job.Id, 10)
	if err != nil {
		t.Fatalf("GetImportErrors: %v", err)
	}
	if len(importErrs) != 1 {
		t.Fatalf("expected 1 error, got %d", len(importErrs))
	}
	if importErrs[0].ErrorCode != "INVALID_NUMBER" {
		t.Errorf("error_code = %q, want INVALID_NUMBER", importErrs[0].ErrorCode)
	}
}

func TestNoValidRowsResultsInFailed(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	userID, modelID, _ := insertFixtures(t, store)
	job, _ := store.CreateImportJob(ctx, modelID, "", "file://bad.csv", userID, nil)

	errs := []*importpkgv1.ImportError{
		{RowNumber: 1, Column: "value", ErrorCode: "MISSING_VALUE", Message: "value required"},
	}
	if err := store.StageRows(ctx, job.Id, nil, errs); err != nil {
		t.Fatalf("StageRows: %v", err)
	}

	failed, _ := store.GetImportJob(ctx, job.Id)
	if failed.Status != importpkgv1.ImportStatus_IMPORT_STATUS_FAILED {
		t.Errorf("status = %v, want FAILED", failed.Status)
	}
}

func TestValidateImportServerCSVParsing(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	userID, modelID, metricID := insertFixtures(t, store)
	job, _ := store.CreateImportJob(ctx, modelID, "", "inline", userID, nil)

	csvContent := strings.Join([]string{
		"metric_id,value,region",
		metricID + ",1500.00,EMEA",
		metricID + ",bad-num,APAC",
		metricID + ",3000.00,AMER",
	}, "\n")

	srv := importpkg.NewServer(zerolog.Nop(), store, nil)
	resp, err := srv.ValidateImport(ctx, &importpkgv1.ValidateImportRequest{
		JobId:      job.Id,
		CsvContent: []byte(csvContent),
	})
	if err != nil {
		t.Fatalf("ValidateImport: %v", err)
	}

	if resp.ValidRows != 2 {
		t.Errorf("valid_rows = %d, want 2", resp.ValidRows)
	}
	if resp.ErrorRows != 1 {
		t.Errorf("error_rows = %d, want 1", resp.ErrorRows)
	}
	if resp.Status != importpkgv1.ImportStatus_IMPORT_STATUS_STAGED {
		t.Errorf("status = %v, want STAGED", resp.Status)
	}
}

// TestFullReloadOnlyDeletesWhatTheActorMayWrite covers the destructive-import
// finding: full_reload issued `DELETE FROM runtime.fact_input WHERE model_id
// = $1 AND revision_id = $2` — the entire model/revision — while
// authorization (CheckWrite + MetricAccess) only ever covered the STAGED
// rows. A user scoped to one department could therefore destroy every other
// department's data, plus hidden members' data and form-posted facts, by
// uploading a single row. The route is open to any authenticated user.
func TestFullReloadOnlyDeletesWhatTheActorMayWrite(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()

	userID, modelID, metricID := insertFixtures(t, store)

	// A second metric the importer is read-restricted from, and a dimension
	// with one visible and one hidden member.
	var otherMetricID, dimID, visibleMemberID, hiddenMemberID string
	if err := store.Pool().QueryRow(ctx,
		`INSERT INTO model.metric_def (model_id, name, is_input) VALUES ($1::uuid, 'headcount', true) RETURNING id::text`,
		modelID).Scan(&otherMetricID); err != nil {
		t.Fatalf("insert other metric: %v", err)
	}
	if err := store.Pool().QueryRow(ctx,
		`INSERT INTO model.dimension_def (model_id, name) VALUES ($1::uuid, 'region') RETURNING id::text`,
		modelID).Scan(&dimID); err != nil {
		t.Fatalf("insert dimension: %v", err)
	}
	if err := store.Pool().QueryRow(ctx,
		`INSERT INTO model.dimension_member (dimension_id, code) VALUES ($1::uuid, 'EMEA') RETURNING id::text`,
		dimID).Scan(&visibleMemberID); err != nil {
		t.Fatalf("insert visible member: %v", err)
	}
	if err := store.Pool().QueryRow(ctx,
		`INSERT INTO model.dimension_member (dimension_id, code) VALUES ($1::uuid, 'APAC') RETURNING id::text`,
		dimID).Scan(&hiddenMemberID); err != nil {
		t.Fatalf("insert hidden member: %v", err)
	}
	for _, rule := range []struct{ ruleType, refID, access string }{
		{"metric", otherMetricID, "read"},
		{"dimension_member", hiddenMemberID, "hidden"},
	} {
		if _, err := store.Pool().Exec(ctx,
			`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, $2, $3, $4)`,
			userID, rule.ruleType, rule.refID, rule.access); err != nil {
			t.Fatalf("insert access rule: %v", err)
		}
	}

	// Pre-existing facts: one the importer may write, three they may not.
	seed := func(metric, code string, value float64, sourceRef *string) {
		t.Helper()
		if _, err := store.Pool().Exec(ctx, `
			INSERT INTO runtime.fact_input (model_id, metric_id, dim_members, value, entered_by, source_ref)
			VALUES ($1::uuid, $2::uuid, jsonb_build_object($3::text, $4::text), $5, $6::uuid, $7::uuid)
		`, modelID, metric, dimID, code, value, userID, sourceRef); err != nil {
			t.Fatalf("seed fact: %v", err)
		}
	}
	mappingRef := "33333333-3333-3333-3333-333333333333"
	seed(metricID, "EMEA", 10, nil)         // writable, direct  → may be replaced
	seed(metricID, "APAC", 20, nil)         // hidden member     → must survive
	seed(otherMetricID, "EMEA", 30, nil)    // read-only metric  → must survive
	seed(metricID, "EMEA", 40, &mappingRef) // form-posted       → must survive

	job, err := store.CreateImportJob(ctx, modelID, "", "file://reload.csv", userID, nil)
	if err != nil {
		t.Fatalf("CreateImportJob: %v", err)
	}
	if err := store.StageRows(ctx, job.Id, []importpkg.StagingRow{
		{MetricID: metricID, DimMembers: map[string]string{dimID: "EMEA"}, Value: 99, RowNumber: 1},
	}, nil); err != nil {
		t.Fatalf("StageRows: %v", err)
	}
	if _, err := store.CommitImport(ctx, job.Id, modelID, "", userID, importpkg.ModeFullReload); err != nil {
		t.Fatalf("CommitImport(full_reload): %v", err)
	}

	surviving := func(metric, code string, sourceRefSet bool) int {
		t.Helper()
		var n int
		q := `SELECT COUNT(*) FROM runtime.fact_input
		      WHERE model_id=$1::uuid AND metric_id=$2::uuid AND dim_members->>$3 = $4
		        AND source_ref IS ` + map[bool]string{true: "NOT NULL", false: "NULL"}[sourceRefSet]
		if err := store.Pool().QueryRow(ctx, q, modelID, metric, dimID, code).Scan(&n); err != nil {
			t.Fatalf("count facts: %v", err)
		}
		return n
	}

	if n := surviving(metricID, "APAC", false); n != 1 {
		t.Errorf("hidden member's fact: %d rows survived, want 1 — full_reload deleted data the importer cannot see", n)
	}
	if n := surviving(otherMetricID, "EMEA", false); n != 1 {
		t.Errorf("read-only metric's fact: %d rows survived, want 1 — full_reload deleted a metric the importer may not write", n)
	}
	if n := surviving(metricID, "EMEA", true); n != 1 {
		t.Errorf("form-posted fact: %d rows survived, want 1 — full_reload must not orphan form records", n)
	}
	// The writable direct-entry cell was genuinely reloaded: the old value is
	// gone and only the staged one remains.
	var values []float64
	rows, err := store.Pool().Query(ctx,
		`SELECT value FROM runtime.fact_input WHERE model_id=$1::uuid AND metric_id=$2::uuid AND dim_members->>$3 = 'EMEA' AND source_ref IS NULL`,
		modelID, metricID, dimID)
	if err != nil {
		t.Fatalf("read reloaded facts: %v", err)
	}
	for rows.Next() {
		var v float64
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			t.Fatalf("scan: %v", err)
		}
		values = append(values, v)
	}
	rows.Close()
	if len(values) != 1 || values[0] != 99 {
		t.Errorf("writable cell after full_reload = %v, want exactly [99]", values)
	}
}

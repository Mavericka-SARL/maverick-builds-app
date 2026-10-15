package importpkg_test

import (
	"context"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
)

// commit stages rows as one file and commits it in mode.
func commit(t *testing.T, store *importpkg.Store, modelID, userID string, mode importpkg.ImportMode, rows []importpkg.StagingRow) {
	t.Helper()
	ctx := context.Background()
	job, err := store.CreateImportJob(ctx, modelID, "", "file://rows.csv", userID, nil)
	if err != nil {
		t.Fatalf("create job: %v", err)
	}
	if err := store.StageRows(ctx, job.Id, rows, nil); err != nil {
		t.Fatalf("stage: %v", err)
	}
	if _, err := store.CommitImport(ctx, job.Id, modelID, "", userID, mode); err != nil {
		t.Fatalf("commit %s: %v", mode, err)
	}
}

// latest is what the reads show for a cell: its latest row, ties broken by
// id as every read path breaks them.
func latest(t *testing.T, store *importpkg.Store, metricID, region string) (value float64, text *string, rows int) {
	t.Helper()
	ctx := context.Background()
	if err := store.Pool().QueryRow(ctx, `
		SELECT value::float8, text_value, (SELECT count(*) FROM runtime.fact_input WHERE metric_id = $1::uuid AND dim_members = jsonb_build_object('region', $2::text))
		FROM runtime.fact_input WHERE metric_id = $1::uuid AND dim_members = jsonb_build_object('region', $2::text)
		ORDER BY entered_at DESC, id DESC LIMIT 1`, metricID, region).Scan(&value, &text, &rows); err != nil {
		t.Fatalf("read %s: %v", region, err)
	}
	return value, text, rows
}

func row(metricID, region string, value float64, n int) importpkg.StagingRow {
	return importpkg.StagingRow{MetricID: metricID, DimMembers: map[string]string{"region": region}, Value: value, RowNumber: n}
}

// A file is its rows entered top to bottom. Rows naming one cell used to be
// inserted with the same entered_at, and which one the reads took was the
// random id's tie-break: an incremental file adding 5 and 7 to a cell holding
// 10 left 15 or 17, never 22.
func TestCommitCombinesRowsOfOneCellInOrder(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	userID, modelID, metricID := insertFixtures(t, store)

	commit(t, store, modelID, userID, importpkg.ModeReplace, []importpkg.StagingRow{row(metricID, "EMEA", 10, 1)})
	commit(t, store, modelID, userID, importpkg.ModeIncremental, []importpkg.StagingRow{
		row(metricID, "EMEA", 5, 1), row(metricID, "APAC", 3, 2), row(metricID, "EMEA", 7, 3),
	})
	if v, _, n := latest(t, store, metricID, "EMEA"); v != 22 || n != 2 {
		t.Errorf("EMEA after adding 5 and 7 to 10: %v in %d rows, want 22 in 2 (one row per import)", v, n)
	}
	if v, _, _ := latest(t, store, metricID, "APAC"); v != 3 {
		t.Errorf("APAC = %v, want 3", v)
	}

	// Replace: the file's last row for a cell is its value.
	commit(t, store, modelID, userID, importpkg.ModeReplace, []importpkg.StagingRow{
		row(metricID, "EMEA", 1, 1), row(metricID, "EMEA", 2, 2), row(metricID, "EMEA", 4, 3),
	})
	if v, _, n := latest(t, store, metricID, "EMEA"); v != 4 || n != 3 {
		t.Errorf("EMEA after replacing with 1, 2, 4: %v in %d rows, want 4 in 3", v, n)
	}

	// Sent again in incremental, the same file adds again: what an
	// Idempotency-Key is for.
	commit(t, store, modelID, userID, importpkg.ModeIncremental, []importpkg.StagingRow{row(metricID, "EMEA", 4, 1)})
	if v, _, _ := latest(t, store, metricID, "EMEA"); v != 8 {
		t.Errorf("EMEA after adding 4 to 4: %v, want 8", v)
	}
}

// An incremental import into a pick-list or text cell takes the file's
// value: a pick-list's stored member key used to be added to the new one,
// leaving a number that names no member.
func TestIncrementalReplacesPicklistAndTextCells(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	userID, modelID, _ := insertFixtures(t, store)
	ctx := context.Background()
	var dimID, statusID, noteID string
	if err := store.Pool().QueryRow(ctx, `INSERT INTO model.dimension_def (model_id, name) VALUES ($1::uuid, 'stage') RETURNING id::text`, modelID).Scan(&dimID); err != nil {
		t.Fatalf("dimension: %v", err)
	}
	if err := store.Pool().QueryRow(ctx, `INSERT INTO model.metric_def (model_id, name, is_input, picklist_dimension_id) VALUES ($1::uuid, 'stage_pick', true, $2::uuid) RETURNING id::text`, modelID, dimID).Scan(&statusID); err != nil {
		t.Fatalf("pick-list metric: %v", err)
	}
	if err := store.Pool().QueryRow(ctx, `INSERT INTO model.metric_def (model_id, name, is_input, format) VALUES ($1::uuid, 'note', true, 'text') RETURNING id::text`, modelID).Scan(&noteID); err != nil {
		t.Fatalf("text metric: %v", err)
	}
	text := func(s string) *string { return &s }
	open, won := formula.PicklistKey("OPEN"), formula.PicklistKey("WON")

	commit(t, store, modelID, userID, importpkg.ModeReplace, []importpkg.StagingRow{
		row(statusID, "EMEA", open, 1),
		{MetricID: noteID, DimMembers: map[string]string{"region": "EMEA"}, Text: text("first"), RowNumber: 1},
	})
	commit(t, store, modelID, userID, importpkg.ModeIncremental, []importpkg.StagingRow{
		row(statusID, "EMEA", won, 1),
		{MetricID: noteID, DimMembers: map[string]string{"region": "EMEA"}, Text: text("second"), RowNumber: 1},
		{MetricID: noteID, DimMembers: map[string]string{"region": "EMEA"}, Text: text("third"), RowNumber: 2},
	})
	if v, _, _ := latest(t, store, statusID, "EMEA"); v != won {
		t.Errorf("pick-list cell = %v, want WON's key %v (OPEN's was %v)", v, won, open)
	}
	if _, got, _ := latest(t, store, noteID, "EMEA"); got == nil || *got != "third" {
		t.Errorf("text cell = %v, want the last row's \"third\"", got)
	}
}

// One person's import sent again under the same Idempotency-Key finds the
// first; two at once run once; one cut off before its commit frees its key.
func TestImportJobKeys(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	userID, modelID, metricID := insertFixtures(t, store)
	ctx := context.Background()

	if j, err := store.JobByKey(ctx, modelID, userID, "k1"); err != nil || j != nil {
		t.Fatalf("an unused key found %v (%v)", j, err)
	}
	first, _ := store.CreateImportJob(ctx, modelID, "", "upload", userID, nil)
	if err := store.SetJobKey(ctx, first.Id, "k1"); err != nil {
		t.Fatalf("set key: %v", err)
	}
	second, _ := store.CreateImportJob(ctx, modelID, "", "upload", userID, nil)
	if err := store.SetJobKey(ctx, second.Id, "k1"); err != importpkg.ErrImportKeyInUse {
		t.Errorf("a second job under the same key: %v, want ErrImportKeyInUse", err)
	}
	if err := store.StageRows(ctx, first.Id, []importpkg.StagingRow{row(metricID, "EMEA", 1, 1)}, nil); err != nil {
		t.Fatal(err)
	}
	j, err := store.JobByKey(ctx, modelID, userID, "k1")
	if err != nil || j == nil || j.ID != first.Id || j.Status != "staged" {
		t.Fatalf("the key's job = %+v (%v), want the first, staged", j, err)
	}

	// Cut off before its commit: the key is free and the job is aborted.
	if err := store.ReleaseKey(ctx, first.Id); err != nil {
		t.Fatal(err)
	}
	if j, _ := store.JobByKey(ctx, modelID, userID, "k1"); j != nil {
		t.Errorf("a released key still finds %+v", j)
	}
	if err := store.SetJobKey(ctx, second.Id, "k1"); err != nil {
		t.Errorf("the released key could not be used again: %v", err)
	}
	if _, err := store.CommitImport(ctx, first.Id, modelID, "", userID, importpkg.ModeIncremental); err == nil {
		t.Error("an aborted job committed")
	}
}

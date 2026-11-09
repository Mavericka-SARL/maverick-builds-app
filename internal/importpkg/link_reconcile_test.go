package importpkg_test

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/mavericks-engine/mavericks/internal/importpkg"
)

// A model link's replace run clears the cells whose latest typed value the
// link wrote and the run no longer holds — its source emptied them — and
// keeps a cell typed over since, a cell another link wrote and one nobody's
// link wrote. A run holding nothing at all clears all the link wrote.
func TestLinkRunClearsOnlyWhatTheLinkWrote(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	userID, modelID, metricID := insertFixtures(t, store)
	revID := "" // the test schema's write guard reads no revision
	link, other := uuid.NewString(), uuid.NewString()
	run := func(linkRef string, rows ...importpkg.StagingRow) int {
		t.Helper()
		job, err := store.CreateImportJob(ctx, modelID, revID, "model_link", userID, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 0 {
			err = store.StageNothing(ctx, job.Id)
		} else {
			err = store.StageRows(ctx, job.Id, rows, nil)
		}
		if err != nil {
			t.Fatal(err)
		}
		_, cleared, err := store.CommitImportWith(ctx, job.Id, modelID, revID, userID, importpkg.ModeReplace,
			importpkg.CommitOptions{LinkRef: linkRef, Reconcile: true})
		if err != nil {
			t.Fatalf("commit: %v", err)
		}
		return cleared
	}
	cell := func(region string) (float64, bool) {
		t.Helper()
		var v float64
		err := store.Pool().QueryRow(ctx, `SELECT value::float8 FROM runtime.fact_input WHERE metric_id=$1::uuid
			AND dim_members = jsonb_build_object('region', $2::text) ORDER BY entered_at DESC, id DESC LIMIT 1`, metricID, region).Scan(&v)
		return v, err == nil
	}

	run(link, row(metricID, "EMEA", 1, 1), row(metricID, "APAC", 2, 2), row(metricID, "LATAM", 3, 3))
	run(other, row(metricID, "NA", 4, 1))
	// Typed over since the link wrote it, and typed where no link wrote.
	if _, err := store.Pool().Exec(ctx, `INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value, entered_by)
		VALUES ($1::uuid, NULL, $2::uuid, '{"region":"APAC"}', 20, $3::uuid), ($1::uuid, NULL, $2::uuid, '{"region":"MEA"}', 5, $3::uuid)`, modelID, metricID, userID); err != nil {
		t.Fatal(err)
	}

	if n := run(link, row(metricID, "EMEA", 1, 1)); n != 1 {
		t.Errorf("cleared %d cells; want 1, LATAM", n)
	}
	if _, ok := cell("LATAM"); ok {
		t.Error("LATAM, emptied in the source, still holds a value")
	}
	for region, want := range map[string]float64{"EMEA": 1, "APAC": 20, "NA": 4, "MEA": 5} {
		if v, ok := cell(region); !ok || v != want {
			t.Errorf("%s = %v (%v); want %v kept", region, v, ok, want)
		}
	}

	// The source empty: everything the link still holds goes, nothing else.
	if n := run(link); n != 1 {
		t.Errorf("an empty run cleared %d cells; want 1, EMEA", n)
	}
	if _, ok := cell("EMEA"); ok {
		t.Error("EMEA still holds a value after the source emptied everything")
	}
	for region, want := range map[string]float64{"APAC": 20, "NA": 4, "MEA": 5} {
		if v, ok := cell(region); !ok || v != want {
			t.Errorf("after the empty run %s = %v (%v); want %v kept", region, v, ok, want)
		}
	}
}

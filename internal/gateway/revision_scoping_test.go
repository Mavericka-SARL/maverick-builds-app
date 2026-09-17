package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// TestForeignRevisionIDRejected covers the revision-scoping finding.
//
// resolveRevisionCtx used to resolve an explicit ?revision_id= with
// `WHERE id=$1` alone, passing modelID only to its fallback paths. Any
// authenticated caller could therefore name another tenant's revision:
//
//   - Reads came back empty rather than leaking, but only because the cell
//     queries happen to filter on model_id and revision_id together. That is a
//     property of those queries, not an access check, so it protected nothing
//     on the handlers whose queries are shaped differently.
//   - Writes stored the value. POST /api/developer/folders inserted a
//     dashboard_folder whose model_id was the caller's own and whose
//     revision_id pointed into another tenant's model — verified live against
//     the dev database before this fix, then reverted.
//   - The two failure modes were distinguishable: a revision belonging to
//     someone else returned 200, one that existed nowhere returned 500. That
//     is an existence oracle for revision UUIDs.
//
// Every case below therefore asserts the SAME status for a foreign revision and
// a nonexistent one, and each write additionally asserts no row survived.
func TestForeignRevisionIDRejected(t *testing.T) {
	f := setupRollupFixture(t)
	ft := seedForeignTenant(t, f)
	ctx := context.Background()

	const dev = "rollup-test-approver" // developer + business_admin in tenant A

	// A UUID that names no revision at all. Before the fix this produced a 500
	// while ft.revisionID produced a 200 — the difference is the oracle.
	const absentRev = "00000000-0000-0000-0000-0000000000ff"

	// ── reads ────────────────────────────────────────────────────────────────
	// The positive control matters more than the rejections here: if these
	// paths were simply broken, every rejection below would pass for the wrong
	// reason. Each one is re-issued with tenant A's own revision and must work.
	reads := []struct{ name, path string }{
		{"grid", "/api/grid?revision_id=%s"},
		{"grid export", "/api/grid/export?format=csv&grid_def_id=" + f.gridStaffID + "&revision_id=%s"},
		{"metrics", "/api/metrics?revision_id=%s"},
		{"developer folders", "/api/developer/folders?revision_id=%s"},
		{"business folders", "/api/folders?revision_id=%s"},
		{"forms", "/api/forms?revision_id=%s"},
		{"dimensions", "/api/dimensions?revision_id=%s"},
		{"developer integrations", "/api/developer/integrations?revision_id=%s"},
		{"debug facts", "/api/developer/debug/facts?revision_id=%s"},
	}
	for _, rd := range reads {
		t.Run("read/"+rd.name, func(t *testing.T) {
			foreignStatus, body := doAs(t, f, "GET", fmt.Sprintf(rd.path, ft.revisionID), dev, f.appID, nil)
			if foreignStatus != http.StatusNotFound {
				t.Errorf("foreign revision returned %d, want 404\nbody: %s", foreignStatus, body)
			}
			absentStatus, _ := doAs(t, f, "GET", fmt.Sprintf(rd.path, absentRev), dev, f.appID, nil)
			if absentStatus != foreignStatus {
				t.Errorf("existence oracle: a revision belonging to another tenant returned %d but a nonexistent one returned %d — they must be indistinguishable",
					foreignStatus, absentStatus)
			}
			// Positive control.
			ownStatus, ownBody := doAs(t, f, "GET", fmt.Sprintf(rd.path, f.workingRevID), dev, f.appID, nil)
			if ownStatus < 200 || ownStatus >= 300 {
				t.Errorf("own revision returned %d, want 2xx — the rejections above would be vacuous\nbody: %s", ownStatus, ownBody)
			}
		})
	}

	// ── writes ───────────────────────────────────────────────────────────────

	t.Run("write/developer folders", func(t *testing.T) {
		status, body := doAs(t, f, "POST", "/api/developer/folders?revision_id="+ft.revisionID, dev, f.appID,
			map[string]any{"name": "cross-tenant-folder"})
		if status != http.StatusNotFound {
			t.Errorf("POST folders with a foreign revision returned %d, want 404\nbody: %s", status, body)
		}
		assertNoRowReferencing(t, f, ctx,
			`SELECT count(*) FROM model.dashboard_folder WHERE model_id=$1::uuid AND revision_id=$2::uuid`,
			f.modelID, ft.revisionID, "dashboard_folder")

		// Positive control: the same call against tenant A's own revision
		// must still create the folder, or the rejection proves nothing.
		okStatus, okBody := doAs(t, f, "POST", "/api/developer/folders?revision_id="+f.workingRevID, dev, f.appID,
			map[string]any{"name": "own-revision-folder"})
		if okStatus < 200 || okStatus >= 300 {
			t.Fatalf("POST folders with own revision returned %d, want 2xx\nbody: %s", okStatus, okBody)
		}
		var n int
		if err := f.pool.QueryRow(ctx,
			`SELECT count(*) FROM model.dashboard_folder WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='own-revision-folder'`,
			f.modelID, f.workingRevID).Scan(&n); err != nil {
			t.Fatalf("count own folder: %v", err)
		}
		if n != 1 {
			t.Fatalf("own-revision folder count = %d, want 1", n)
		}
	})

	t.Run("write/developer grids", func(t *testing.T) {
		status, body := doAs(t, f, "POST", "/api/developer/grids", dev, f.appID,
			map[string]any{"name": "cross-tenant-grid", "revision_id": ft.revisionID})
		if status != http.StatusNotFound {
			t.Errorf("POST grids with a foreign revision returned %d, want 404\nbody: %s", status, body)
		}
		assertNoRowReferencing(t, f, ctx,
			`SELECT count(*) FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$2::uuid`,
			f.modelID, ft.revisionID, "grid_def")
	})

	// Import is the weakest case of the three, and deliberately recorded as
	// such: with the guard reverted this returns 400 rather than committing a
	// row, because column classification runs against the named revision and
	// the foreign one has no matching metric_defs. So the fact_input assertion
	// below is not load-bearing — it is the status that moves (400 -> 404).
	// The guard still belongs here: "the wrong revision happens to have no
	// matching column names" is not an access check, and a foreign revision
	// that did share metric names would reach the insert.
	t.Run("write/import upload", func(t *testing.T) {
		csv := "department,amount\nDept A,100\n"
		status, body := doAs(t, f, "POST", "/api/import/upload", dev, f.appID,
			map[string]any{"csv": csv, "revision_id": ft.revisionID})
		if status != http.StatusNotFound {
			t.Errorf("POST import/upload with a foreign revision returned %d, want 404\nbody: %s", status, body)
		}
		assertNoRowReferencing(t, f, ctx,
			`SELECT count(*) FROM runtime.fact_input WHERE model_id=$1::uuid AND revision_id=$2::uuid`,
			f.modelID, ft.revisionID, "fact_input")
	})

	// The foreign tenant's own rows are untouched by any of the above.
	var foreignFolders int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM model.dashboard_folder WHERE model_id=$1::uuid`, ft.modelID).Scan(&foreignFolders); err != nil {
		t.Fatalf("count foreign folders: %v", err)
	}
	if foreignFolders != 1 {
		t.Errorf("foreign tenant folder count = %d, want the 1 it was seeded with", foreignFolders)
	}
}

// assertNoRowReferencing fails when a rejected write nevertheless left a row in
// the caller's own model carrying another tenant's revision_id. A status-code
// assertion alone would not catch a handler that refuses the response but
// commits the row first.
func assertNoRowReferencing(t *testing.T, f *rollupFixture, ctx context.Context, sql, modelID, revisionID, label string) {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(ctx, sql, modelID, revisionID).Scan(&n); err != nil {
		// A missing table means the fixture can't observe the write at all,
		// which would make the assertion vacuous.
		if strings.Contains(err.Error(), "does not exist") {
			t.Fatalf("cannot verify %s: %v", label, err)
		}
		t.Fatalf("count %s: %v", label, err)
	}
	if n != 0 {
		t.Errorf("%d %s row(s) in this tenant's model carry another tenant's revision_id — the write was committed despite the rejection", n, label)
	}
}

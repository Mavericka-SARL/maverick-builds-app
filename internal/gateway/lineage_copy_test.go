package gateway

// Lineage (migration 099) is the identity a dimension, member or metric
// shares with its copies in every revision — what access rules resolve by
// (writeguard.RulesForRevision). A cross-entity column must be
// carried by revision duplication and model export/import. These tests pin
// both over HTTP.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// revisionLineages maps every dimension ("d:<name>"), member
// ("m:<dim>/<code>") and metric ("x:<name>") of revision rev to its
// lineage_id, and to its row id.
func revisionLineages(t *testing.T, pool *pgxpool.Pool, rev string) (lineage, ids map[string]string) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT 'd:' || d.name, d.lineage_id::text, d.id::text FROM model.dimension_def d WHERE d.revision_id = $1::uuid
		UNION ALL
		SELECT 'm:' || d.name || '/' || m.code, m.lineage_id::text, m.id::text
		FROM model.dimension_member m JOIN model.dimension_def d ON d.id = m.dimension_id WHERE d.revision_id = $1::uuid
		UNION ALL
		SELECT 'x:' || md.name, md.lineage_id::text, md.id::text FROM model.metric_def md WHERE md.revision_id = $1::uuid`, rev)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	lineage, ids = map[string]string{}, map[string]string{}
	for rows.Next() {
		var k, l, id string
		if err := rows.Scan(&k, &l, &id); err != nil {
			t.Fatal(err)
		}
		lineage[k], ids[k] = l, id
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return lineage, ids
}

// TestRevisionDuplicationCopiesLineage: the developer's revision create
// (source_revision_id) copies every dimension's, member's and metric's
// lineage_id while minting new row ids; a member added to the copy gets a
// lineage of its own.
func TestRevisionDuplicationCopiesLineage(t *testing.T) {
	f, _, _ := setupOldRevisionFixture(t)
	revA := f.revID
	revB := f.dev_("POST", "/api/developer/revisions", map[string]any{"name": "Next", "source_revision_id": revA})

	linA, idsA := revisionLineages(t, f.pool, revA)
	linB, idsB := revisionLineages(t, f.pool, revB)
	if len(linA) < 5 {
		t.Fatalf("fixture precondition: revision A has only %v", linA)
	}
	if len(linB) != len(linA) {
		t.Errorf("copy has %d rows, want %d: %v", len(linB), len(linA), linB)
	}
	seen := map[string]bool{}
	for k, l := range linA {
		if linB[k] != l {
			t.Errorf("%s: copied lineage %q, want the source's %q", k, linB[k], l)
		}
		if idsB[k] == idsA[k] {
			t.Errorf("%s: copy reuses the source row id", k)
		}
		if seen[l] {
			t.Errorf("%s: lineage %s shared with another row of the revision", k, l)
		}
		seen[l] = true
	}

	var dimB string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT id::text FROM model.dimension_def WHERE revision_id=$1::uuid`, revB).Scan(&dimB); err != nil {
		t.Fatal(err)
	}
	f.dev_("POST", "/api/developer/dimensions/"+dimB+"/members", map[string]any{"code": "FR", "label": "FR"})
	linB, _ = revisionLineages(t, f.pool, revB)
	for k, l := range linB {
		if _, ok := linA[k]; !ok && (l == "" || seen[l]) {
			t.Errorf("added %s: lineage %q, want a fresh one", k, l)
		}
	}
}

// TestModelExportImportLineage: model export writes each row's lineage
// into the package, and import keeps it — unless it is missing or already
// taken in this database (re-importing into the source's own database),
// where it mints fresh ones, one per package lineage.
func TestModelExportImportLineage(t *testing.T) {
	f := setupRoundTripFixture(t)
	srcLin, _ := revisionLineages(t, f.pool, f.workingRevID)

	status, pkg := f.do(t, "GET", fmt.Sprintf("/api/admin/models/%s/export?revision_id=%s", f.modelID, f.workingRevID), f.taPersona, nil)
	if status != http.StatusOK {
		t.Fatalf("export status = %d, body = %v", status, pkg)
	}
	// The package carries the source rows' lineage.
	exported := 0
	eachLineage(pkg, func(key string, row map[string]any) {
		exported++
		if got, _ := row["lineage_id"].(string); got != srcLin[key] {
			t.Errorf("package %s lineage_id = %q, want the source's %q", key, got, srcLin[key])
		}
	})
	if exported != len(srcLin) {
		t.Errorf("package carries %d rows, source revision has %d", exported, len(srcLin))
	}

	importAs := func(name string, p map[string]any) map[string]string {
		t.Helper()
		status, res := f.do(t, "POST", "/api/admin/models/import", f.taPersona,
			map[string]any{"application_id": f.appID, "model_name": name, "package": p})
		if status != http.StatusOK {
			t.Fatalf("import %s: status = %d, body = %v", name, status, res)
		}
		lin, _ := revisionLineages(t, f.pool, res["revision_id"].(string))
		if len(lin) != len(srcLin) {
			t.Fatalf("import %s: %d rows, want %d", name, len(lin), len(srcLin))
		}
		return lin
	}
	distinctFresh := func(label string, lin map[string]string) {
		t.Helper()
		seen := map[string]bool{}
		for _, l := range srcLin {
			seen[l] = true
		}
		for k, l := range lin {
			if seen[l] {
				t.Errorf("%s: %s lineage %s is the source's or another row's; want a fresh one", label, k, l)
			}
			seen[l] = true
		}
	}

	// Same database: every lineage is taken by the source model, so all are
	// re-minted.
	distinctFresh("re-import", importAs("Reimported", pkg))

	// A package from another database: its lineages are kept.
	foreign := clonePackage(t, pkg)
	want := map[string]string{}
	eachLineage(foreign, func(key string, row map[string]any) {
		row["lineage_id"] = uuid.NewString()
		want[key] = row["lineage_id"].(string)
	})
	got := importAs("Foreign", foreign)
	for k, l := range want {
		if got[k] != l {
			t.Errorf("foreign import: %s lineage %q, want the package's %q", k, got[k], l)
		}
	}

	// A package from before lineage existed: fresh lineages.
	legacy := clonePackage(t, pkg)
	eachLineage(legacy, func(_ string, row map[string]any) { delete(row, "lineage_id") })
	distinctFresh("legacy import", importAs("Legacy", legacy))
}

// eachLineage calls fn for every dimension, member and metric of an
// exported package, keyed as revisionLineages keys them.
func eachLineage(pkg map[string]any, fn func(key string, row map[string]any)) {
	dims, _ := pkg["dimensions"].([]any)
	for _, d := range dims {
		dm, _ := d.(map[string]any)
		name, _ := dm["name"].(string)
		fn("d:"+name, dm)
		members, _ := dm["members"].([]any)
		for _, m := range members {
			mm, _ := m.(map[string]any)
			code, _ := mm["code"].(string)
			fn("m:"+name+"/"+code, mm)
		}
	}
	metrics, _ := pkg["metrics"].([]any)
	for _, m := range metrics {
		mm, _ := m.(map[string]any)
		name, _ := mm["name"].(string)
		fn("x:"+name, mm)
	}
}

func clonePackage(t *testing.T, pkg map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(pkg)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

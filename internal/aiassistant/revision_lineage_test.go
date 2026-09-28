package aiassistant_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// lineagesIn maps every dimension ("d:<name>"), member ("m:<dim>/<code>")
// and metric ("x:<name>") of revision rev to its lineage_id, and returns
// the row ids too, so a copy can be checked to share lineage but not ids.
func lineagesIn(t *testing.T, pool *pgxpool.Pool, rev string) (lineage map[string]string, ids map[string]string) {
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

// TestCreateRevision_CopiesLineage: the AI's create_revision carries every
// dimension's, member's and metric's lineage_id into the copy (migration
// 099) — the identity access rules resolve by in every revision — while
// minting new row ids. A row added to the copy afterwards gets a lineage
// of its own.
func TestCreateRevision_CopiesLineage(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	srcRev := seedRevision(t, pool, modelID, "Source")
	exec := aiassistant.NewWriteExecutor(pool, modelID, srcRev)
	ctx := context.Background()

	if _, _, err := exec.Execute(ctx, "create_dimension", mustJSON(t, map[string]any{
		"name": "Region", "revision_id": srcRev,
		"members": []map[string]any{{"code": "US", "label": "US"}, {"code": "DE", "label": "DE"}},
	})); err != nil {
		t.Fatalf("create Region: %v", err)
	}
	if _, _, err := exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{
		"name": "revenue", "is_input": true, "revision_id": srcRev,
	})); err != nil {
		t.Fatalf("create metric: %v", err)
	}
	_, newRev, err := exec.Execute(ctx, "create_revision", mustJSON(t, map[string]any{
		"name": "Copy", "source_revision_id": srcRev,
	}))
	if err != nil {
		t.Fatalf("create_revision: %v", err)
	}

	srcLin, srcIDs := lineagesIn(t, pool, srcRev)
	newLin, newIDs := lineagesIn(t, pool, newRev)
	if len(srcLin) != 4 {
		t.Fatalf("source revision rows = %v, want Region, US, DE and revenue", srcLin)
	}
	if len(newLin) != len(srcLin) {
		t.Errorf("copy has %d rows, want %d: %v", len(newLin), len(srcLin), newLin)
	}
	seen := map[string]bool{}
	for k, l := range srcLin {
		if newLin[k] != l {
			t.Errorf("%s: copy lineage %q, want the source's %q", k, newLin[k], l)
		}
		if newIDs[k] == srcIDs[k] {
			t.Errorf("%s: copy reuses the source row id %s", k, srcIDs[k])
		}
		if seen[l] {
			t.Errorf("%s: lineage %s shared with another source row", k, l)
		}
		seen[l] = true
	}

	// A member added to the copy is a new lineage.
	var dimB string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_def WHERE revision_id=$1::uuid AND name='Region'`, newRev).Scan(&dimB); err != nil {
		t.Fatal(err)
	}
	execB := aiassistant.NewWriteExecutor(pool, modelID, newRev)
	if _, _, err := execB.Execute(ctx, "add_dimension_member", mustJSON(t, map[string]any{
		"dimension_id": dimB, "code": "UK", "label": "UK",
	})); err != nil {
		t.Fatalf("add member: %v", err)
	}
	newLin, _ = lineagesIn(t, pool, newRev)
	if l := newLin["m:Region/UK"]; l == "" || seen[l] {
		t.Errorf("added member UK lineage %q, want a fresh one", l)
	}
}

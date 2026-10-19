package gateway

// A dimension's display name (migration 146): the developer sets it with the
// dimension PATCH, the grid answers it beside the name for selectors and
// headers, and a revision copy and a model package carry it. The name stays
// what formulas read.

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestDimensionLabel_Route(t *testing.T) {
	f := setupMMFixture(t)
	ctx := context.Background()
	const dev = "mm-dev"
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	dimID := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'plan_case') RETURNING id::text`, f.modelA, f.revA)
	q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'BASE', 'Base') RETURNING id::text`, dimID)
	grid := q(`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Cases') RETURNING id::text`, f.modelA, f.revA)
	q(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid) RETURNING id::text`, grid, dimID)

	patch := func(sub string, body any) (int, string) {
		t.Helper()
		code, raw := f.do(t, "PATCH", "/api/developer/dimensions/"+dimID, sub, f.app1, f.modelA, body)
		return code, string(raw)
	}
	gridLabel := func() (name, label string) {
		t.Helper()
		code, raw := f.do(t, "GET", "/api/grid?grid_def_id="+grid, dev, f.app1, f.modelA, nil)
		if code != http.StatusOK {
			t.Fatalf("grid: %d %s", code, raw)
		}
		var g struct {
			Dimensions []struct {
				ID, Name, Label string
			} `json:"dimensions"`
		}
		_ = json.Unmarshal(raw, &g)
		for _, d := range g.Dimensions {
			if d.ID == dimID {
				return d.Name, d.Label
			}
		}
		t.Fatalf("dimension not on the grid: %s", raw)
		return "", ""
	}

	if code, raw := patch(dev, map[string]any{"label": "  Plan case "}); code != http.StatusOK {
		t.Fatalf("set label: %d %s", code, raw)
	}
	if name, label := gridLabel(); name != "plan_case" || label != "Plan case" {
		t.Errorf("grid dimension %q shown as %q, want plan_case shown as \"Plan case\"", name, label)
	}
	// A rename leaves the label; another field leaves it too.
	if code, raw := patch(dev, map[string]any{"tags": []string{"fpa"}}); code != http.StatusOK {
		t.Fatalf("set tags: %d %s", code, raw)
	}
	if got := q(`SELECT COALESCE(label,'') FROM model.dimension_def WHERE id=$1::uuid`, dimID); got != "Plan case" {
		t.Errorf("label after a tags change: %q", got)
	}
	if code, raw := patch(dev, map[string]any{"label": string(make([]byte, 81))}); code != http.StatusBadRequest {
		t.Errorf("an 81-character label: %d %s, want 400", code, raw)
	}
	for _, sub := range []string{"mm-ba1", "mm-user"} {
		if code, raw := patch(sub, map[string]any{"label": "Theirs"}); code != http.StatusForbidden {
			t.Errorf("%s: %d %s, want 403", sub, code, raw)
		}
	}

	// ── a duplicated revision carries it ─────────────────────────────────
	code, raw := f.do(t, "POST", "/api/developer/revisions", dev, f.app1, f.modelA, map[string]any{"name": "Copy", "source_revision_id": f.revA})
	if code != http.StatusOK {
		t.Fatalf("duplicate the revision: %d %s", code, raw)
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &created)
	if got := q(`SELECT COALESCE(label,'') FROM model.dimension_def WHERE revision_id=$1::uuid AND name='plan_case'`, created.ID); got != "Plan case" {
		t.Errorf("duplicated revision's label %q", got)
	}

	// ── model export and import carry it ─────────────────────────────────
	code, raw = f.do(t, "GET", "/api/admin/models/"+f.modelA+"/export?revision_id="+f.revA, "mm-tenant-admin", f.app1, "", nil)
	if code != http.StatusOK {
		t.Fatalf("export: %d %.300s", code, raw)
	}
	code, raw = f.do(t, "POST", "/api/admin/models/import", "mm-tenant-admin", f.app2, "",
		map[string]any{"application_id": f.app2, "model_name": "Imported", "package": json.RawMessage(raw)})
	if code != http.StatusOK {
		t.Fatalf("import: %d %s", code, raw)
	}
	var imported struct {
		RevisionID string `json:"revision_id"`
	}
	_ = json.Unmarshal(raw, &imported)
	if got := q(`SELECT COALESCE(label,'') FROM model.dimension_def WHERE revision_id=$1::uuid AND name='plan_case'`, imported.RevisionID); got != "Plan case" {
		t.Errorf("imported label %q", got)
	}

	// Empty shows the name again.
	if code, raw := patch(dev, map[string]any{"label": ""}); code != http.StatusOK {
		t.Fatalf("clear label: %d %s", code, raw)
	}
	if _, label := gridLabel(); label != "" {
		t.Errorf("label after clearing: %q", label)
	}
}

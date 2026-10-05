package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
)

// TestGridAndDashboardTags drives grid and dashboard tags through the
// developer API the console uses, then through revision duplication and model
// export/import, which must carry grid tags along (dashboard tags already
// travelled).
//
// It also pins both PATCHes as partial updates. The dashboard PATCH used to
// require a name and overwrite the tags with whatever was sent, so a request
// carrying only a name cleared them; the grid PATCH could only rename.
func TestGridAndDashboardTags(t *testing.T) {
	f := setupRoundTripFixture(t)
	ctx := context.Background()
	dev := "rollup-test-approver"

	call := func(method, path string, body any) map[string]any {
		t.Helper()
		status, raw := doAs(t, f.rollupFixture, method, path, dev, f.appID, body)
		if status != http.StatusOK {
			t.Fatalf("%s %s: %d %s", method, path, status, raw)
		}
		var out map[string]any
		_ = json.Unmarshal([]byte(raw), &out)
		return out
	}
	refuse := func(path string, body any) {
		t.Helper()
		if status, raw := doAs(t, f.rollupFixture, "PATCH", path, dev, f.appID, body); status != http.StatusBadRequest {
			t.Errorf("PATCH %s %v: %d %s, want 400", path, body, status, raw)
		}
	}
	row := func(table, id string) (name string, tags []string) {
		t.Helper()
		if err := f.pool.QueryRow(ctx, `SELECT name, tags FROM `+table+` WHERE id=$1::uuid`, id).Scan(&name, &tags); err != nil {
			t.Fatalf("read %s %s: %v", table, id, err)
		}
		return
	}

	// ── Grids ──
	created := call("POST", "/api/developer/grids", map[string]any{
		"name": "Tagged grid", "revision_id": f.workingRevID, "tags": []string{" Cost Centre ", "", "cost-centre", "opex"}})
	gridID := created["id"].(string)
	if _, tags := row("model.grid_def", gridID); !slices.Equal(tags, []string{"cost-centre", "opex"}) {
		t.Errorf("created grid tags = %v, want cleaned [cost-centre opex]", tags)
	}

	status, raw := doAs(t, f.rollupFixture, "GET", "/api/developer/grids?revision_id="+f.workingRevID, dev, f.appID, nil)
	if status != http.StatusOK {
		t.Fatalf("list grids: %d %s", status, raw)
	}
	var listed []struct {
		ID   string   `json:"id"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(raw), &listed); err != nil {
		t.Fatalf("decode grids: %v", err)
	}
	found := false
	for _, g := range listed {
		if g.Tags == nil {
			t.Errorf("grid %s listed with tags null, want []", g.ID)
		}
		if g.ID == gridID {
			found = true
			if !slices.Equal(g.Tags, []string{"cost-centre", "opex"}) {
				t.Errorf("listed grid tags = %v", g.Tags)
			}
		}
	}
	if !found {
		t.Errorf("grid %s not listed", gridID)
	}

	call("PATCH", "/api/developer/grids/"+gridID, map[string]any{"name": "Renamed grid"})
	if name, tags := row("model.grid_def", gridID); name != "Renamed grid" || !slices.Equal(tags, []string{"cost-centre", "opex"}) {
		t.Errorf("after rename: %s %v, want tags kept", name, tags)
	}
	call("PATCH", "/api/developer/grids/"+gridID, map[string]any{"tags": []string{"opex", "Board Pack"}})
	if name, tags := row("model.grid_def", gridID); name != "Renamed grid" || !slices.Equal(tags, []string{"opex", "board-pack"}) {
		t.Errorf("after retag: %s %v, want name kept", name, tags)
	}
	refuse("/api/developer/grids/"+gridID, map[string]any{})
	refuse("/api/developer/grids/"+gridID, map[string]any{"name": "  "})

	// ── Dashboards ──
	created = call("POST", "/api/developer/dashboards", map[string]any{
		"name": "Tagged dashboard", "revision_id": f.workingRevID, "tags": []string{"Monthly Close", "monthly-close"}})
	dashID := created["id"].(string)
	if _, tags := row("model.dashboard_def", dashID); !slices.Equal(tags, []string{"monthly-close"}) {
		t.Errorf("created dashboard tags = %v, want cleaned [monthly-close]", tags)
	}
	// Adding a tag sends only the tags: the name stays.
	call("PATCH", "/api/developer/dashboards/"+dashID, map[string]any{"tags": []string{"monthly-close", "board"}})
	if name, tags := row("model.dashboard_def", dashID); name != "Tagged dashboard" || !slices.Equal(tags, []string{"monthly-close", "board"}) {
		t.Errorf("after adding a tag: %s %v", name, tags)
	}
	// A rename alone used to clear them.
	call("PATCH", "/api/developer/dashboards/"+dashID, map[string]any{"name": "Close pack"})
	if name, tags := row("model.dashboard_def", dashID); name != "Close pack" || !slices.Equal(tags, []string{"monthly-close", "board"}) {
		t.Errorf("after rename: %s %v, want tags kept", name, tags)
	}
	// A folder move alone keeps both.
	call("PATCH", "/api/developer/dashboards/"+dashID, map[string]any{"folder_id": nil})
	if name, tags := row("model.dashboard_def", dashID); name != "Close pack" || !slices.Equal(tags, []string{"monthly-close", "board"}) {
		t.Errorf("after folder move: %s %v", name, tags)
	}
	refuse("/api/developer/dashboards/"+dashID, map[string]any{})
	refuse("/api/developer/dashboards/"+dashID, map[string]any{"name": ""})
	call("PATCH", "/api/developer/dashboards/"+dashID, map[string]any{"tags": []string{}})
	if _, tags := row("model.dashboard_def", dashID); len(tags) != 0 {
		t.Errorf("after clearing: %v, want none", tags)
	}

	// ── Revision duplication and export/import carry grid tags ──
	assertCopied := func(where, modelID, revID string) {
		t.Helper()
		var tags []string
		if err := f.pool.QueryRow(ctx, `SELECT tags FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='Renamed grid'`, modelID, revID).Scan(&tags); err != nil {
			t.Fatalf("%s: grid: %v", where, err)
		}
		if !slices.Equal(tags, []string{"opex", "board-pack"}) {
			t.Errorf("%s: grid tags %v", where, tags)
		}
	}
	rev := call("POST", "/api/developer/revisions", map[string]any{"name": "Tagged grid copy", "source_revision_id": f.workingRevID})
	assertCopied("duplicated revision", f.modelID, rev["id"].(string))

	status, pkg := f.do(t, "GET", fmt.Sprintf("/api/admin/models/%s/export?revision_id=%s", f.modelID, f.workingRevID), f.taPersona, nil)
	if status != http.StatusOK {
		t.Fatalf("export: %d %v", status, pkg)
	}
	status, res := f.do(t, "POST", "/api/admin/models/import", f.taPersona,
		map[string]any{"application_id": f.appID, "model_name": "Imported grid tags", "package": pkg})
	if status != http.StatusOK {
		t.Fatalf("import: %d %v", status, res)
	}
	assertCopied("imported model", res["model_id"].(string), res["revision_id"].(string))
}

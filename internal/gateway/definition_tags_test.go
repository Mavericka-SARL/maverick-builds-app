package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"
)

// TestDefinitionTagsAndPartialDimensionPatch drives dimension and metric tags
// through the developer API the console uses, then through revision
// duplication and model export/import, which must carry them along.
//
// It also pins the dimension PATCH as a partial update. The console's rename
// form sends only {name}; the handler used to write agg_rule="sum" and
// parent_dimension_id=NULL for whatever was left out, so renaming a child
// dimension silently detached it from its parent.
func TestDefinitionTagsAndPartialDimensionPatch(t *testing.T) {
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
	dimRow := func(id string) (name, aggRule string, parent *string, tags []string) {
		t.Helper()
		if err := f.pool.QueryRow(ctx, `SELECT name, agg_rule, parent_dimension_id::text, tags FROM model.dimension_def WHERE id=$1::uuid`, id).
			Scan(&name, &aggRule, &parent, &tags); err != nil {
			t.Fatalf("read dimension %s: %v", id, err)
		}
		return
	}

	// ── Dimensions ──
	created := call("POST", "/api/developer/dimensions", map[string]any{
		"name": "channel", "revision_id": f.workingRevID, "tags": []string{" sales ", "", "sales", "core"}})
	channelID := created["id"].(string)
	if _, _, _, tags := dimRow(channelID); !slices.Equal(tags, []string{"sales", "core"}) {
		t.Errorf("created tags = %v, want trimmed and de-duplicated [sales core]", tags)
	}

	status, raw := doAs(t, f.rollupFixture, "GET", "/api/developer/dimensions?revision_id="+f.workingRevID, dev, f.appID, nil)
	if status != http.StatusOK {
		t.Fatalf("list dimensions: %d %s", status, raw)
	}
	var listed []struct {
		ID   string   `json:"id"`
		Tags []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(raw), &listed); err != nil {
		t.Fatalf("decode dimensions: %v", err)
	}
	found := false
	for _, d := range listed {
		if d.ID == channelID {
			found = true
			if !slices.Equal(d.Tags, []string{"sales", "core"}) {
				t.Errorf("listed tags = %v", d.Tags)
			}
		}
	}
	if !found {
		t.Errorf("dimension %s not listed", channelID)
	}

	// staff is a declared child of departments. A rename alone must keep
	// the parent, the rollup rule and the tags.
	if _, err := f.pool.Exec(ctx, `UPDATE model.dimension_def SET agg_rule='average', tags='{people}' WHERE id=$1::uuid`, f.staffDimID); err != nil {
		t.Fatal(err)
	}
	call("PATCH", "/api/developer/dimensions/"+f.staffDimID, map[string]any{"name": "staff_members"})
	name, agg, parent, tags := dimRow(f.staffDimID)
	if name != "staff_members" || agg != "average" || parent == nil || *parent != f.deptsDimID || !slices.Equal(tags, []string{"people"}) {
		t.Errorf("after rename: name=%s agg=%s parent=%v tags=%v; want staff_members/average/%s/[people]", name, agg, parent, tags, f.deptsDimID)
	}
	// Tags alone change only the tags.
	call("PATCH", "/api/developer/dimensions/"+f.staffDimID, map[string]any{"tags": []string{"people", "hr"}})
	name, agg, parent, tags = dimRow(f.staffDimID)
	if name != "staff_members" || agg != "average" || parent == nil || !slices.Equal(tags, []string{"people", "hr"}) {
		t.Errorf("after tag edit: name=%s agg=%s parent=%v tags=%v", name, agg, parent, tags)
	}

	// An explicit null is the way to detach.
	call("PATCH", "/api/developer/dimensions/"+f.staffDimID, map[string]any{"parent_dimension_id": nil})
	if _, _, parent, _ := dimRow(f.staffDimID); parent != nil {
		t.Errorf("parent after explicit null = %v, want none", *parent)
	}

	// ── Metrics ──
	created = call("POST", "/api/developer/metrics", map[string]any{
		"name": "units", "is_input": true, "revision_id": f.workingRevID, "tags": []string{"volume"}})
	unitsID := created["id"].(string)
	metricTags := func() []string {
		t.Helper()
		model := call("GET", "/api/developer/model?revision_id="+f.workingRevID, nil)
		metrics, _ := model["metrics"].([]any)
		for _, m := range metrics {
			if mm := m.(map[string]any); mm["id"] == unitsID {
				var out []string
				for _, v := range mm["tags"].([]any) {
					out = append(out, v.(string))
				}
				return out
			}
		}
		t.Fatalf("metric %s not listed", unitsID)
		return nil
	}
	if got := metricTags(); !slices.Equal(got, []string{"volume"}) {
		t.Errorf("metric tags after create = %v", got)
	}
	patch := map[string]any{"name": "units", "formula": "", "agg_rule": "sum", "format": "number", "format_currency": "$", "time_summary": "sum"}
	call("PATCH", "/api/developer/metrics/"+unitsID, patch)
	if got := metricTags(); !slices.Equal(got, []string{"volume"}) {
		t.Errorf("metric tags after a PATCH without tags = %v, want kept", got)
	}
	patch["tags"] = []string{"volume", "ops"}
	call("PATCH", "/api/developer/metrics/"+unitsID, patch)
	if got := metricTags(); !slices.Equal(got, []string{"volume", "ops"}) {
		t.Errorf("metric tags after PATCH = %v", got)
	}

	// ── Revision duplication and export/import carry the tags ──
	assertCopied := func(where, modelID, revID string) {
		t.Helper()
		var dimTags, metTags []string
		if err := f.pool.QueryRow(ctx, `SELECT tags FROM model.dimension_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='channel'`, modelID, revID).Scan(&dimTags); err != nil {
			t.Fatalf("%s: dimension: %v", where, err)
		}
		if err := f.pool.QueryRow(ctx, `SELECT tags FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='units'`, modelID, revID).Scan(&metTags); err != nil {
			t.Fatalf("%s: metric: %v", where, err)
		}
		if !slices.Equal(dimTags, []string{"sales", "core"}) || !slices.Equal(metTags, []string{"volume", "ops"}) {
			t.Errorf("%s: dimension tags %v, metric tags %v", where, dimTags, metTags)
		}
	}
	rev := call("POST", "/api/developer/revisions", map[string]any{"name": "Tagged copy", "source_revision_id": f.workingRevID})
	assertCopied("duplicated revision", f.modelID, rev["id"].(string))

	status, pkg := f.do(t, "GET", fmt.Sprintf("/api/admin/models/%s/export?revision_id=%s", f.modelID, f.workingRevID), f.taPersona, nil)
	if status != http.StatusOK {
		t.Fatalf("export: %d %v", status, pkg)
	}
	status, res := f.do(t, "POST", "/api/admin/models/import", f.taPersona,
		map[string]any{"application_id": f.appID, "model_name": "Imported tags", "package": pkg})
	if status != http.StatusOK {
		t.Fatalf("import: %d %v", status, res)
	}
	assertCopied("imported model", res["model_id"].(string), res["revision_id"].(string))

	// Last, because it adds a model to the app: a PATCH naming a parent
	// dimension from another model is refused, as it is on create.
	var otherModel, otherDim string
	if err := f.pool.QueryRow(ctx, `INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Other') RETURNING id::text`, f.appID).Scan(&otherModel); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO model.dimension_def (model_id, name) VALUES ($1::uuid, 'foreign') RETURNING id::text`, otherModel).Scan(&otherDim); err != nil {
		t.Fatal(err)
	}
	if status, raw := doAs(t, f.rollupFixture, "PATCH", "/api/developer/dimensions/"+channelID, dev, f.appID,
		map[string]any{"name": "channel", "parent_dimension_id": otherDim}); status != http.StatusBadRequest {
		t.Errorf("parent from another model: %d %s, want 400", status, raw)
	}
}

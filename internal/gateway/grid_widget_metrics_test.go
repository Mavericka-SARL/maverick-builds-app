package gateway

// A grid widget picks which of its grid's metrics it shows, in what order
// (widget_props.metric_ids): a P&L's region table shows RF, LY, Var and Var %
// of a grid holding more. The list is checked against the grid on save,
// follows the metrics through revision duplication and model export/import,
// and loses a metric that is deleted — one left with none shows the whole
// grid again.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestGridWidgetChosenMetrics(t *testing.T) {
	f := setupRoundTripFixture(t)
	ctx := context.Background()
	dev := "rollup-test-approver"

	call := func(method, path string, body any) (int, string) {
		t.Helper()
		return doAs(t, f.rollupFixture, method, path, dev, f.appID, body)
	}
	must := func(method, path string, body any) string {
		t.Helper()
		status, raw := call(method, path, body)
		if status != http.StatusOK && status != http.StatusCreated {
			t.Fatalf("%s %s: %d %s", method, path, status, raw)
		}
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(raw), &out)
		return out.ID
	}
	metric := func(name string, onGrid bool) string {
		t.Helper()
		id := must("POST", "/api/developer/metrics", map[string]any{"name": name, "is_input": true, "revision_id": f.workingRevID})
		if onGrid {
			must("POST", "/api/developer/grids/"+f.gridStaffID+"/metrics/"+id, nil)
		}
		return id
	}
	price, qty := metric("price", true), metric("qty", true)
	stray := metric("stray", false)

	dashID := must("POST", "/api/developer/dashboards", map[string]any{"name": "Chosen metrics", "revision_id": f.workingRevID})
	widgets := "/api/developer/dashboards/" + dashID + "/widgets"
	grid := func(ids ...string) map[string]any {
		return map[string]any{"widget_type": "grid", "ref_id": f.gridStaffID, "pos_x": 0, "pos_y": 0, "size_w": 600, "size_h": 300,
			"widget_props": map[string]any{"metric_ids": ids}}
	}

	// ── Checked against the grid on save ──
	if status, raw := call("POST", widgets, grid(price, stray)); status != http.StatusBadRequest || !strings.Contains(raw, "stray") {
		t.Errorf("a metric the grid does not hold: %d %s, want 400 naming stray", status, raw)
	}
	pair := must("POST", widgets, grid(price, f.amountMetricID))
	only := must("POST", widgets, grid(qty))
	if status, raw := call("PATCH", widgets+"/"+pair, map[string]any{"widget_props": map[string]any{"metric_ids": []string{price, price}}}); status != http.StatusBadRequest {
		t.Errorf("a metric named twice: %d %s, want 400", status, raw)
	}

	chosen := func(where, widgetID string) []string {
		t.Helper()
		var raw []byte
		if err := f.pool.QueryRow(ctx, `SELECT COALESCE(widget_props->'metric_ids','null'::jsonb) FROM model.dashboard_widget WHERE id=$1::uuid`, widgetID).Scan(&raw); err != nil {
			t.Fatalf("%s: %v", where, err)
		}
		var ids []string
		_ = json.Unmarshal(raw, &ids)
		return ids
	}
	if got := chosen("saved", pair); !slices.Equal(got, []string{price, f.amountMetricID}) {
		t.Errorf("saved metric_ids = %v", got)
	}

	// ── Copies point at their own metrics, in the same order ──
	assertCopied := func(where, modelID, revID string) {
		t.Helper()
		var widgetID, wantPrice, wantAmount string
		if err := f.pool.QueryRow(ctx, `
			SELECT w.id::text FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id = w.dashboard_id
			WHERE d.model_id=$1::uuid AND d.revision_id=$2::uuid AND d.name='Chosen metrics' AND jsonb_array_length(w.widget_props->'metric_ids') = 2`,
			modelID, revID).Scan(&widgetID); err != nil {
			t.Fatalf("%s: widget: %v", where, err)
		}
		for name, dst := range map[string]*string{"price": &wantPrice, "amount": &wantAmount} {
			if err := f.pool.QueryRow(ctx, `SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name=$3`,
				modelID, revID, name).Scan(dst); err != nil {
				t.Fatalf("%s: metric %s: %v", where, name, err)
			}
		}
		if got := chosen(where, widgetID); !slices.Equal(got, []string{wantPrice, wantAmount}) {
			t.Errorf("%s: metric_ids = %v, want the copy's [price amount] = [%s %s]", where, got, wantPrice, wantAmount)
		}
	}
	revID := must("POST", "/api/developer/revisions", map[string]any{"name": "Chosen copy", "source_revision_id": f.workingRevID})
	assertCopied("duplicated revision", f.modelID, revID)

	status, pkg := f.do(t, "GET", fmt.Sprintf("/api/admin/models/%s/export?revision_id=%s", f.modelID, f.workingRevID), f.taPersona, nil)
	if status != http.StatusOK {
		t.Fatalf("export: %d %v", status, pkg)
	}
	status, res := f.do(t, "POST", "/api/admin/models/import", f.taPersona,
		map[string]any{"application_id": f.appID, "model_name": "Imported chosen", "package": pkg})
	if status != http.StatusOK {
		t.Fatalf("import: %d %v", status, res)
	}
	assertCopied("imported model", res["model_id"].(string), res["revision_id"].(string))

	// ── A deleted metric leaves the list; an emptied list shows the grid ──
	must("DELETE", "/api/developer/metrics/"+price, nil)
	if got := chosen("after deleting price", pair); !slices.Equal(got, []string{f.amountMetricID}) {
		t.Errorf("after deleting price: metric_ids = %v, want [amount]", got)
	}
	must("DELETE", "/api/developer/metrics/"+qty, nil)
	var hasKey bool
	if err := f.pool.QueryRow(ctx, `SELECT widget_props ? 'metric_ids' FROM model.dashboard_widget WHERE id=$1::uuid`, only).Scan(&hasKey); err != nil {
		t.Fatalf("widget left after deleting its only metric: %v", err)
	}
	if hasKey {
		t.Errorf("a grid widget whose only chosen metric was deleted kept metric_ids; it should show the whole grid")
	}
}

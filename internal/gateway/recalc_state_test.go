package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A cell written with "recalc": "background" answers once stored; the grid
// reports recalc_pending until the pass lands, then the new calculated value.
// Without the option the write still answers after the recalculation.
func TestBackgroundRecalcAfterACellWrite(t *testing.T) {
	f := setupRoundTripFixture(t)
	ctx := context.Background()
	dev := "rollup-test-approver"
	rev := f.workingRevID
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
	amount := must("POST", "/api/developer/metrics", map[string]any{"name": "bg_amount", "is_input": true, "revision_id": rev})
	double := must("POST", "/api/developer/metrics", map[string]any{"name": "bg_double", "formula": "bg_amount * 2", "revision_id": rev})
	grid := must("POST", "/api/developer/grids", map[string]any{"name": "Background", "revision_id": rev})
	must("POST", "/api/developer/grids/"+grid+"/metrics/"+amount, nil)
	must("POST", "/api/developer/grids/"+grid+"/metrics/"+double, nil)

	read := func() (float64, bool) {
		t.Helper()
		_, raw := call("GET", "/api/grid?grid_def_id="+grid+"&model_id="+f.modelID+"&revision_id="+rev, nil)
		var g struct {
			Totals        map[string]float64 `json:"totals"`
			RecalcPending bool               `json:"recalc_pending"`
		}
		_ = json.Unmarshal([]byte(raw), &g)
		return g.Totals[double], g.RecalcPending
	}

	status, raw := call("POST", "/api/cells", map[string]any{"model_id": f.modelID, "revision_id": rev, "metric_id": amount, "value": 21, "recalc": "background"})
	if status != http.StatusOK || !strings.Contains(raw, `"recalculating":true`) {
		t.Fatalf("background write: %d %s", status, raw)
	}
	var requested int64
	if err := f.pool.QueryRow(ctx, `SELECT requested FROM runtime.revision_recalc_state WHERE revision_id=$1::uuid`, rev).Scan(&requested); err != nil || requested < 1 {
		t.Fatalf("the write recorded no pending recalculation (requested %d, err %v)", requested, err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		v, pending := read()
		if !pending && v == 42 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("after the background pass: bg_double = %v, recalc_pending = %v; want 42, false", v, pending)
		}
		time.Sleep(200 * time.Millisecond)
	}

	// Without the option the write answers after the recalculation.
	if status, raw := call("POST", "/api/cells", map[string]any{"model_id": f.modelID, "revision_id": rev, "metric_id": amount, "value": 5}); status != http.StatusOK || strings.Contains(raw, "recalculating") {
		t.Fatalf("plain write: %d %s", status, raw)
	}
	if v, pending := read(); v != 10 || pending {
		t.Errorf("right after a plain write: bg_double = %v, recalc_pending = %v; want 10, false", v, pending)
	}
}

// TestATagOnAMetricRecalculatesNothing: a metric's tags, label or highlight
// rules move no value, so saving them leaves the calculated results as they
// are; a formula still recalculates (2026-11-08: each tag a developer gave a
// metric recalculated the whole model).
func TestATagOnAMetricRecalculatesNothing(t *testing.T) {
	f := setupRoundTripFixture(t)
	ctx := context.Background()
	dev := "rollup-test-approver"
	rev := f.workingRevID
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
		return raw
	}
	id := func(raw string) string {
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(raw), &out)
		return out.ID
	}
	amount := id(must("POST", "/api/developer/metrics", map[string]any{"name": "tg_amount", "is_input": true, "revision_id": rev}))
	double := id(must("POST", "/api/developer/metrics", map[string]any{"name": "tg_double", "formula": "tg_amount * 2", "revision_id": rev}))
	grid := id(must("POST", "/api/developer/grids", map[string]any{"name": "Tagged", "revision_id": rev}))
	must("POST", "/api/developer/grids/"+grid+"/metrics/"+amount, nil)
	must("POST", "/api/developer/grids/"+grid+"/metrics/"+double, nil)
	must("POST", "/api/cells", map[string]any{"model_id": f.modelID, "revision_id": rev, "metric_id": amount, "value": 21})
	calculated := func() (time.Time, float64) {
		t.Helper()
		var at time.Time
		var v float64
		if err := f.pool.QueryRow(ctx, `SELECT calc_at, value::float8 FROM runtime.calc_result
			WHERE metric_id=$1::uuid AND revision_id=$2::uuid AND dim_members='{}' ORDER BY calc_at DESC LIMIT 1`, double, rev).Scan(&at, &v); err != nil {
			t.Fatalf("read tg_double: %v", err)
		}
		return at, v
	}
	at, v := calculated()
	if v != 42 {
		t.Fatalf("tg_double = %v, want 42", v)
	}

	raw := must("PATCH", "/api/developer/metrics/"+double, map[string]any{"tags": []string{"demand"}, "label": "Double"})
	if strings.Contains(raw, `"revision_id"`) {
		t.Errorf("a tag and a label recalculated: %s", raw)
	}
	if again, _ := calculated(); !again.Equal(at) {
		t.Errorf("a tag and a label recalculated tg_double (%v, then %v)", at, again)
	}
	var tags []string
	_ = f.pool.QueryRow(ctx, `SELECT tags FROM model.metric_def WHERE id=$1::uuid`, double).Scan(&tags)
	if len(tags) != 1 || tags[0] != "demand" {
		t.Errorf("tags = %v, want [demand]", tags)
	}

	must("PATCH", "/api/developer/metrics/"+double, map[string]any{"formula": "tg_amount * 3"})
	if _, v := calculated(); v != 63 {
		t.Errorf("after a formula change tg_double = %v, want 63", v)
	}
}

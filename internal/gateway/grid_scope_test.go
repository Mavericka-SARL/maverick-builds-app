package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A grid's read answers for the metrics it shows: every other metric's
// cells rode along, so one read of a grid carried the whole model.
func TestGridReadAnswersForItsOwnMetrics(t *testing.T) {
	f := setupDevAuthoringFixture(t)
	ctx := context.Background()
	mk := func(path string, body map[string]any) string {
		t.Helper()
		status, out := f.do(t, http.MethodPost, path, body)
		if status != http.StatusOK {
			t.Fatalf("POST %s: %d %v", path, status, out)
		}
		id, _ := out["id"].(string)
		return id
	}
	other := mk("/api/developer/metrics", map[string]any{"name": "OtherCost", "is_input": true, "format": "number", "revision_id": f.revID})
	for _, m := range []struct{ name, code string }{{"North", "N"}, {"South", "S"}} {
		mk(fmt.Sprintf("/api/developer/dimensions/%s/members", f.dimID), map[string]any{"code": m.code, "label": m.name})
	}
	grid := mk("/api/developer/grids", map[string]any{"name": "Revenue only", "revision_id": f.revID})
	for _, p := range []string{"/dimensions/" + f.dimID, "/metrics/" + f.metricID} {
		if status, out := f.do(t, http.MethodPost, "/api/developer/grids/"+grid+p, nil); status != http.StatusOK {
			t.Fatalf("assign %s: %d %v", p, status, out)
		}
	}
	otherGrid := mk("/api/developer/grids", map[string]any{"name": "Cost", "revision_id": f.revID})
	for _, p := range []string{"/dimensions/" + f.dimID, "/metrics/" + other} {
		if status, out := f.do(t, http.MethodPost, "/api/developer/grids/"+otherGrid+p, nil); status != http.StatusOK {
			t.Fatalf("assign %s: %d %v", p, status, out)
		}
	}
	if status, out := f.do(t, http.MethodPost, "/api/import/upload", map[string]string{
		"csv": "FixtureRegion,FixtureRevenue,OtherCost\nN,10,1\nS,20,2\n", "revision_id": f.revID, "import_mode": "replace",
	}); status != http.StatusOK {
		t.Fatalf("import: %d %v", status, out)
	}

	status, out := f.do(t, http.MethodGet, "/api/grid?revision_id="+f.revID+"&grid_def_id="+grid, nil)
	if status != http.StatusOK {
		t.Fatalf("grid read: %d %v", status, out)
	}
	cells, _ := out["cells"].(map[string]any)
	if cells[f.metricID+":N"] != 10.0 || cells[f.metricID+":S"] != 20.0 {
		t.Errorf("the grid's own cells: %v", cells)
	}
	for k := range cells {
		if strings.HasPrefix(k, other) {
			t.Errorf("the grid answered another grid's metric cell %s", k)
		}
	}
	if totals, _ := out["totals"].(map[string]any); totals[other] != nil || totals[f.metricID] != 30.0 {
		t.Errorf("totals = %v, want the grid's metric (30) alone", totals)
	}
	// all_metrics still lists the whole model: the console labels with it.
	if all, _ := out["all_metrics"].([]any); len(all) < 2 {
		t.Errorf("all_metrics lists %d metrics, want the model's", len(all))
	}

	// A whole-model read still answers for every metric.
	status, out = f.do(t, http.MethodGet, "/api/grid?revision_id="+f.revID, nil)
	if status != http.StatusOK {
		t.Fatalf("whole-model read: %d %v", status, out)
	}
	raw, _ := json.Marshal(out["cells"])
	if !strings.Contains(string(raw), other) {
		t.Errorf("a whole-model read lost another metric's cells: %s", raw)
	}
	_ = ctx
}

// Full grid reads beyond gridReadsAtOnce wait their turn instead of all
// holding a grid in memory at once; a caller that goes away while waiting
// stops waiting.
func TestGridReadsTakeTurns(t *testing.T) {
	n := gridReadsAtOnce()
	var releases []func()
	for i := 0; i < n; i++ {
		release, err := gridReadSlot(context.Background())
		if err != nil {
			t.Fatalf("slot %d: %v", i, err)
		}
		releases = append(releases, release)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := gridReadSlot(ctx); err == nil {
		t.Fatalf("a read beyond %d at once got a turn", n)
	}
	got := make(chan error, 1)
	go func() {
		release, err := gridReadSlot(context.Background())
		if err == nil {
			release()
		}
		got <- err
	}()
	releases[0]()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("the waiting read: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("the waiting read never got the freed turn")
	}
	for _, release := range releases[1:] {
		release()
	}
}

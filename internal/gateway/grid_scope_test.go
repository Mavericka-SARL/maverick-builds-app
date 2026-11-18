package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/fairshare"
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

// One tenant's heavy work never holds back another's (2026-11-18): with
// another tenant holding its whole share of turns, a grid read is answered
// at once; with the reader's own tenant at its share, the read waits behind
// its own tenant's work — its caller giving up first — and is answered once
// a turn of its tenant's frees. The dev user has no tenant of its own, so its
// work is keyed by the user.
func TestGridReadsWaitBehindTheirOwnTenantOnly(t *testing.T) {
	f := setupDevAuthoringFixture(t)
	ctx := context.Background()
	var userID string
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM identity.user WHERE keycloak_sub = 'dev-audit-pa'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	lim := fairshare.Default()
	_, share := lim.Limits()
	hold := func(key string) func() {
		var releases []func()
		for range share {
			_, release, err := lim.Acquire(ctx, key)
			if err != nil {
				t.Fatal(err)
			}
			releases = append(releases, release)
		}
		return func() {
			for _, r := range releases {
				r()
			}
		}
	}
	path := "/api/grid?revision_id=" + f.revID

	releaseOther := hold("another-tenant")
	if status, out := f.do(t, http.MethodGet, path, nil); status != http.StatusOK {
		t.Fatalf("a read while another tenant holds its share: %d %v", status, out)
	}
	releaseOther()

	releaseOwn := hold("user:" + userID)
	waitCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(waitCtx, http.MethodGet, f.srv.URL+path, nil)
	req.Header.Set("X-Dev-User", "dev-audit-pa")
	req.Header.Set("X-App-Id", f.appID)
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatalf("a read with its own tenant at its share was answered %d without waiting", resp.StatusCode)
	}
	releaseOwn()
	if status, out := f.do(t, http.MethodGet, path, nil); status != http.StatusOK {
		t.Fatalf("a read once its tenant's turns freed: %d %v", status, out)
	}
	// Metadata reads are light and take no turn.
	releaseOwn = hold("user:" + userID)
	defer releaseOwn()
	if status, out := f.do(t, http.MethodGet, path+"&meta_only=1", nil); status != http.StatusOK {
		t.Fatalf("a metadata read with its tenant at its share: %d %v", status, out)
	}
}

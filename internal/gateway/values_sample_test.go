package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

// A developer fills a grid with sample values on the server: across its
// leaf combinations or under chosen members, a fixed value or a range per
// metric, repeatable by seed, only empty cells if asked, previewed first.
func TestDeveloperFillsSampleValues(t *testing.T) {
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
	for _, m := range []struct{ code, parent string }{{"EU", ""}, {"DE", "EU"}, {"FR", "EU"}, {"US", ""}} {
		body := map[string]any{"code": m.code, "label": m.code}
		if m.parent != "" {
			var pid string
			_ = f.pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`, f.dimID, m.parent).Scan(&pid)
			body["parent_member_id"] = pid
		}
		mk(fmt.Sprintf("/api/developer/dimensions/%s/members", f.dimID), body)
	}
	cost := mk("/api/developer/metrics", map[string]any{"name": "Cost", "is_input": true, "format": "number", "revision_id": f.revID})
	double := mk("/api/developer/metrics", map[string]any{"name": "DoubleRevenue", "is_input": false, "formula": "FixtureRevenue * 2", "format": "number", "revision_id": f.revID})
	grid := mk("/api/developer/grids", map[string]any{"name": "Plan", "revision_id": f.revID})
	for _, p := range []string{"/dimensions/" + f.dimID, "/metrics/" + f.metricID, "/metrics/" + cost, "/metrics/" + double} {
		if status, out := f.do(t, http.MethodPost, "/api/developer/grids/"+grid+p, nil); status != http.StatusOK {
			t.Fatalf("assign %s: %d %v", p, status, out)
		}
	}
	path := "/api/developer/grids/" + grid + "/sample-values"
	values := func(metric string) map[string]float64 {
		t.Helper()
		rows, err := f.pool.Query(ctx, `SELECT DISTINCT ON (dim_members) dim_members->>$2, value::float8 FROM runtime.fact_input
			WHERE metric_id = $1::uuid ORDER BY dim_members, entered_at DESC, id DESC`, metric, f.dimID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]float64{}
		for rows.Next() {
			var c string
			var v float64
			_ = rows.Scan(&c, &v)
			out[c] = v
		}
		return out
	}
	spec := map[string]any{"FixtureRevenue": map[string]any{"value": 5}, "Cost": map[string]any{"min": 1, "max": 2, "decimals": 1}}

	// Previewed: three leaves (DE, FR, US; EU has members) × two metrics.
	status, out := f.do(t, http.MethodPost, path, map[string]any{"metrics": spec, "dry_run": true})
	if status != http.StatusOK || out["cells"] != 6.0 {
		t.Fatalf("dry run: %d %v, want 6 cells", status, out)
	}
	if len(values(f.metricID)) != 0 {
		t.Fatal("a dry run wrote values")
	}
	buf, _ := json.Marshal(map[string]any{"metrics": spec, "dry_run": true, "format": "csv"})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.srv.URL+path, bytes.NewReader(buf))
	req.Header.Set("X-Dev-User", "dev-audit-pa")
	req.Header.Set("X-App-Id", f.appID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	csvBody, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	lines := strings.Split(strings.TrimSpace(string(csvBody)), "\n")
	if resp.Header.Get("Content-Type") != "text/csv; charset=utf-8" || len(lines) != 7 || lines[0] != "FixtureRegion,metric,value" {
		t.Fatalf("the preview CSV: %s %q", resp.Header.Get("Content-Type"), lines)
	}

	// Filled, and the same seed draws the same.
	if status, out := f.do(t, http.MethodPost, path, map[string]any{"metrics": spec, "seed": 7}); status != http.StatusOK || out["cells"] != 6.0 {
		t.Fatalf("fill: %d %v", status, out)
	}
	rev := values(f.metricID)
	if rev["DE"] != 5 || rev["FR"] != 5 || rev["US"] != 5 {
		t.Errorf("revenue = %v, want 5 everywhere", rev)
	}
	first := values(cost)
	for c, v := range first {
		if v < 1 || v > 2 || v*10 != float64(int(v*10)) {
			t.Errorf("cost at %s = %v, want 1 to 2 with one decimal", c, v)
		}
	}
	if status, _ := f.do(t, http.MethodPost, path, map[string]any{"metrics": spec, "seed": 7}); status != http.StatusOK {
		t.Fatal("refill")
	}
	if again := values(cost); fmt.Sprint(again) != fmt.Sprint(first) {
		t.Errorf("the same seed drew %v, then %v", first, again)
	}

	// Only empty cells: US cleared, then filled alone.
	if status, out := f.do(t, http.MethodPost, "/api/developer/grids/"+grid+"/clear-values", map[string]any{"members": map[string][]string{f.dimID: {"US"}}}); status != http.StatusOK || out["cells"] != 2.0 {
		t.Fatalf("clear US: %d %v", status, out)
	}
	if status, out := f.do(t, http.MethodPost, path, map[string]any{"metrics": map[string]any{"FixtureRevenue": map[string]any{"value": 9}, "Cost": map[string]any{"value": 9}}, "only_empty": true}); status != http.StatusOK || out["cells"] != 2.0 {
		t.Fatalf("fill only empty: %d %v, want US's 2 cells", status, out)
	}
	if rev := values(f.metricID); rev["US"] != 9 || rev["DE"] != 5 {
		t.Errorf("after filling only empty cells revenue = %v, want US 9 and DE kept at 5", rev)
	}

	// Under EU only; a calculated metric is refused.
	if status, out := f.do(t, http.MethodPost, path, map[string]any{"metrics": spec, "members": map[string][]string{f.dimID: {"EU"}}, "dry_run": true}); status != http.StatusOK || out["cells"] != 4.0 {
		t.Errorf("under EU: %d %v, want 4 cells", status, out)
	}
	if status, _ := f.do(t, http.MethodPost, path, map[string]any{"metrics": map[string]any{"DoubleRevenue": map[string]any{"value": 1}}}); status != http.StatusBadRequest {
		t.Errorf("filling a calculated metric: %d, want 400", status)
	}
	if row := f.latestAuditEvent(t, "values.generated"); row.resourceID != grid {
		t.Errorf("values.generated resource_id = %s, want the grid", row.resourceID)
	}
}

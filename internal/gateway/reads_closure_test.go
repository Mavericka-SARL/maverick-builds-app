package gateway

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/salesdemo"
)

// TestScopedReadWorksOutOnlyWhatItShows: a scoped grid read works out its
// own metrics and what they read, and a KPI tile's read (metric_ids) its
// metric and what it reads — never the whole model (2026-11-22: a six-metric
// grid of a 60-metric OPEX budget worked out all 60 on every poll). Every
// value they answer is the value the whole-model read, which still works out
// every metric, answers at the same coordinates, under scopes pinning two
// dimensions (no precomputed slice serves them), parents and leaves.
func TestScopedReadWorksOutOnlyWhatItShows(t *testing.T) {
	d := setupSalesDemo(t)
	caller := func(method, path string, body any) (map[string]any, error) {
		status, raw := d.req(method, path, d.dev, body)
		if status < 200 || status >= 300 {
			return nil, fmt.Errorf("%s %s: status %d: %s", method, path, status, raw)
		}
		var parsed map[string]any
		_ = json.Unmarshal(raw, &parsed)
		return parsed, nil
	}
	if err := salesdemo.BuildForecast(caller, d.revID, d.model); err != nil {
		t.Fatalf("build forecast: %v", err)
	}
	d.seedFacts()
	d.awaitCalc("margin", map[string]string{}, 760_000)

	// A grid showing two calculated metrics that read others (margin_pct
	// reads margin, which reads revenue and cost; weighted_target reads
	// quarter_weight and target).
	shown := []string{"margin_pct", "weighted_target"}
	g, err := caller("POST", "/api/developer/grids", map[string]any{"name": "Margin view", "revision_id": d.revID})
	if err != nil {
		t.Fatal(err)
	}
	gridID, _ := g["id"].(string)
	for _, dimID := range []string{d.geoDim, d.prodDim, d.periodDim} {
		if _, err := caller("POST", "/api/developer/grids/"+gridID+"/dimensions/"+dimID, nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range shown {
		// A metric belongs to one grid: it leaves the Sales Plan for this one.
		if _, err := caller("DELETE", "/api/developer/grids/"+d.gridID+"/metrics/"+d.metric[name], nil); err != nil {
			t.Fatal(err)
		}
		if _, err := caller("POST", "/api/developer/grids/"+gridID+"/metrics/"+d.metric[name], nil); err != nil {
			t.Fatal(err)
		}
	}

	type answer struct {
		Cells  map[string]float64 `json:"cells"`
		Totals map[string]float64 `json:"totals"`
	}
	read := func(label, query string) answer {
		t.Helper()
		status, raw := d.req("GET", "/api/grid?revision_id="+d.revID+query, d.dev, nil)
		if status != 200 {
			t.Fatalf("%s: status %d: %s", label, status, raw)
		}
		var a answer
		if err := json.Unmarshal(raw, &a); err != nil {
			t.Fatalf("%s: %v\n%s", label, err, raw)
		}
		return a
	}

	scopes := []map[string]string{
		{d.geoDim: "UK", d.periodDim: "Q1"},
		{d.geoDim: "AMER", d.prodDim: "HARDWARE"},
		{d.geoDim: "EMEA", d.prodDim: "LICENSE", d.periodDim: "Q2"},
	}
	answered, cells := 0, 0
	for _, sc := range scopes {
		raw, _ := json.Marshal(sc)
		scope := "&scope=" + url.QueryEscape(string(raw))
		whole := read("whole model "+string(raw), scope)
		grid := read("grid "+string(raw), "&grid_def_id="+gridID+scope)

		for _, name := range shown {
			id := d.metric[name]
			// Absent (no value at the slice) must stay absent: a read that
			// skipped a dependency would answer a 0 or nothing for a value.
			want, wantOK := whole.Totals[id]
			if wantOK {
				answered++
			}
			if got, ok := grid.Totals[id]; ok != wantOK || (ok && !nearly(got, want)) {
				t.Errorf("%s: grid total of %s = %v (present %v), whole-model read %v (present %v)", raw, name, got, ok, want, wantOK)
			}
			for k := range grid.Cells {
				if _, ok := whole.Cells[k]; strings.HasPrefix(k, id+":") && !ok {
					t.Errorf("%s: grid cell %s of %s has a value the whole-model read has not", raw, k, name)
				}
			}
			for k, want := range whole.Cells {
				if !strings.HasPrefix(k, id+":") {
					continue
				}
				cells++
				if got, ok := grid.Cells[k]; !ok || !nearly(got, want) {
					t.Errorf("%s: grid cell %s of %s = %v (present %v), whole-model read %v", raw, k, name, got, ok, want)
				}
			}

			kpi := read("kpi "+name+" "+string(raw), "&totals_only=1&fields=totals&metric_ids="+id+scope)
			if got, ok := kpi.Totals[id]; ok != wantOK || (ok && !nearly(got, want)) {
				t.Errorf("%s: KPI total of %s = %v (present %v), whole-model read %v (present %v)", raw, name, got, ok, want, wantOK)
			}
			if len(kpi.Totals) > 1 {
				t.Errorf("%s: a KPI read of %s answered %d totals, want its own alone", raw, name, len(kpi.Totals))
			}
		}
	}
	// The comparison compared something: totals and cells with values.
	if answered < 3 || cells < 10 {
		t.Fatalf("only %d totals and %d cells had values to compare", answered, cells)
	}
}

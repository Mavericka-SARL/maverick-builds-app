package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// A formula that reads a dimension at the cell its metric's grid does not
// carry is blank at every cell, so the developer API refuses it wherever
// the metric's dimensions become known: a formula save on a placed metric,
// placing it on a grid, removing the dimension from its grid and activating
// a revision that already holds one (DIMENSION_NOT_ON_METRIC).
func TestDimensionNotOnMetricOverHTTP(t *testing.T) {
	f := setupDimFormulaFixture(t)
	product := f.call("POST", "/api/developer/dimensions", map[string]any{"name": "product", "revision_id": f.revID, "dimension_type": "standard"})
	f.call("POST", "/api/developer/dimensions/"+product+"/members", map[string]any{"code": "P1", "label": "P1"})

	// Not placed yet: its dimensions are unknown, so it saves.
	f.metric["pflag"] = f.call("POST", "/api/developer/metrics", f.metricBody("pflag", `IF(product = "P1", 1, 0)`))
	f.refused("placing it on [region]", "POST", "/api/developer/grids/"+f.gridID+"/metrics/"+f.metric["pflag"], nil, formula.CodeDimensionNotOnMetric)

	f.metric["rflag"] = f.call("POST", "/api/developer/metrics", f.metricBody("rflag", `IF(region = "EMEA", 1, 0)`))
	f.call("POST", "/api/developer/grids/"+f.gridID+"/metrics/"+f.metric["rflag"], nil)
	f.refused("a placed metric's formula", "PATCH", "/api/developer/metrics/"+f.metric["rflag"],
		map[string]any{"name": "rflag", "formula": `IF(product = "P1", 1, 0)`, "agg_rule": "sum", "format": "number"}, formula.CodeDimensionNotOnMetric)
	f.refused("removing region from the grid", "DELETE", "/api/developer/grids/"+f.gridID+"/dimensions/"+f.region, nil, formula.CodeDimensionNotOnMetric)

	// A revision that already holds one (saved before this check) is not
	// activated until it is fixed; the message names the metric and the fix.
	if _, err := f.pool.Exec(context.Background(), `UPDATE model.metric_def SET formula=$2 WHERE id=$1::uuid`, f.metric["rflag"], `IF(product = "P1", 1, 0)`); err != nil {
		t.Fatal(err)
	}
	status, raw := f.req("PUT", "/api/developer/revisions/"+f.revID+"/activate", nil)
	if status != http.StatusBadRequest || !strings.Contains(string(raw), formula.CodeDimensionNotOnMetric) ||
		!strings.Contains(string(raw), "rflag") || !strings.Contains(string(raw), "put product on the grid") {
		t.Errorf("activate: status %d body %s; want 400 %s naming rflag and the fix", status, raw, formula.CodeDimensionNotOnMetric)
	}
}

package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/metricformula"
)

// TestPropertyRenameRewritesFormulasAndDeleteInUseIsRefused: renaming a
// property rewrites every formula that reads it (only the property token —
// a string literal that happens to spell it stays), the metric keeps
// computing under the new name, and deleting a property a formula still
// reads is refused with 409 naming the metric instead of leaving it to
// serve its last values.
func TestPropertyRenameRewritesFormulasAndDeleteInUseIsRefused(t *testing.T) {
	f := setupDimFormulaFixture(t)
	props := "/api/developer/dimensions/" + f.region + "/properties"
	factID := f.call("POST", props, map[string]any{"name": "fact", "data_type": "number"})
	tierID := f.call("POST", props, map[string]any{"name": "tier", "data_type": "text"})
	f.setMember("EMEA", map[string]string{"fact": "2", "tier": "A"})
	f.setMember("US", map[string]string{"fact": "3", "tier": "B"})

	f.metric["scaled"] = f.call("POST", "/api/developer/metrics",
		f.metricBody("scaled", `revenue * Region.FACT + IF(region = "region.fact", 1, 0)`))
	f.call("POST", "/api/developer/grids/"+f.gridID+"/metrics/"+f.metric["scaled"], nil)
	f.writeCell("revenue", "EMEA", 100)
	f.writeCell("revenue", "US", 50)
	f.await("scaled", "EMEA", 200)
	f.await("scaled", "US", 150)

	f.call("PATCH", props+"/"+factID, map[string]any{"name": "factor"})
	var text string
	if err := f.pool.QueryRow(context.Background(), `SELECT formula FROM model.metric_def WHERE id=$1::uuid`, f.metric["scaled"]).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if want := `revenue * Region.factor + IF(region = "region.fact", 1, 0)`; text != want {
		t.Errorf("formula after the rename: %q, want %q", text, want)
	}
	// Still computing under the new name: a member edit recalculates.
	f.setMember("US", map[string]string{"factor": "4"})
	f.await("scaled", "US", 200)

	status, raw := f.req("DELETE", props+"/"+factID, nil)
	if status != http.StatusConflict || !strings.Contains(string(raw), metricformula.CodePropertyInUse) || !strings.Contains(string(raw), "scaled") {
		t.Errorf("deleting a property a formula reads: status %d body %s; want 409 %s naming scaled", status, raw, metricformula.CodePropertyInUse)
	}
	var still int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM model.dimension_property WHERE id=$1::uuid`, factID).Scan(&still); err != nil || still != 1 {
		t.Errorf("the refused delete removed the property (count %d, err %v)", still, err)
	}

	// A property no formula reads deletes as before.
	if status, raw := f.req("DELETE", props+"/"+tierID, nil); status != http.StatusOK {
		t.Errorf("deleting an unused property: status %d body %s", status, raw)
	}
}

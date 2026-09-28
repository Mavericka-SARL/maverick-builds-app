package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
)

// Regressions from the final review of the dimensional-references change.

// TestLegacyDimensionPropertyRenameSparesShadowingRevision: renaming a
// property of a revision-less dimension rewrites only the formulas in which
// the dimension's name resolves to it — never those of a revision owning a
// dimension of the same name, whose formulas read that one's property.
// Deleting the revision-less property is not refused over those formulas
// either.
func TestLegacyDimensionPropertyRenameSparesShadowingRevision(t *testing.T) {
	f := setupDimFormulaFixture(t)
	f.call("POST", "/api/developer/dimensions/"+f.region+"/properties", map[string]any{"name": "fact", "data_type": "number"})
	legacy := f.call("POST", "/api/developer/dimensions", map[string]any{"name": "region", "dimension_type": "standard"})
	legacyProps := "/api/developer/dimensions/" + legacy + "/properties"
	legacyFact := f.call("POST", legacyProps, map[string]any{"name": "fact", "data_type": "number"})
	f.metric["scaled"] = f.call("POST", "/api/developer/metrics", f.metricBody("scaled", "revenue * region.fact"))

	f.call("PATCH", legacyProps+"/"+legacyFact, map[string]any{"name": "factor"})
	var text string
	if err := f.pool.QueryRow(context.Background(), `SELECT formula FROM model.metric_def WHERE id=$1::uuid`, f.metric["scaled"]).Scan(&text); err != nil {
		t.Fatal(err)
	}
	if text != "revenue * region.fact" {
		t.Errorf("the working revision's formula was rewritten by the revision-less dimension's rename: %q", text)
	}
	if status, raw := f.req("DELETE", legacyProps+"/"+legacyFact, nil); status != http.StatusOK {
		t.Errorf("deleting the revision-less property no formula resolves to: %d %s", status, raw)
	}
}

// TestFormulaGuardsOnMembersDimensionsAndNames:
//   - deleting a member a literal LOOKUP names is refused (MEMBER_IN_USE),
//     and activation re-checks literal LOOKUP members (UNKNOWN_MEMBER) — a
//     member renamed after the formula was saved;
//   - a bare dimension name matches case-insensitively, as PARENT and
//     dim.property do, and computes;
//   - a dimension a formula names cannot be deleted (409 DIMENSION_IN_USE),
//     as a property cannot (PROPERTY_IN_USE);
//   - a PATCH carrying only the formula names the stored metric in its
//     errors, never a UUID or an empty name.
func TestFormulaGuardsOnMembersDimensionsAndNames(t *testing.T) {
	f := setupDimFormulaFixture(t)
	f.metric["upper"] = f.call("POST", "/api/developer/metrics", f.metricBody("upper", `IF(REGION = "EMEA", revenue, 0)`))
	f.call("POST", "/api/developer/grids/"+f.gridID+"/metrics/"+f.metric["upper"], nil)
	f.metric["twice"] = f.call("POST", "/api/developer/metrics", f.metricBody("twice", "upper * 2"))
	f.call("POST", "/api/developer/grids/"+f.gridID+"/metrics/"+f.metric["twice"], nil)
	f.writeCell("revenue", "EMEA", 100)
	f.writeCell("revenue", "US", 50)
	f.await("upper", "EMEA", 100)
	f.await("upper", "US", 0)

	f.metric["lk"] = f.call("POST", "/api/developer/metrics", f.metricBody("lk", `LOOKUP(revenue, region, "US")`))
	f.call("POST", "/api/developer/grids/"+f.gridID+"/metrics/"+f.metric["lk"], nil)
	// The delete is refused while lk names US (MEMBER_IN_USE); a rename is
	// not, and does not rewrite the formula, so activation still meets it.
	if status, raw := f.req("DELETE", "/api/developer/dimensions/"+f.region+"/members/"+f.members["US"], nil); status != http.StatusConflict ||
		!strings.Contains(string(raw), metricformula.CodeMemberInUse) {
		t.Fatalf("delete member US while lk names it: %d %s; want 409 %s", status, raw, metricformula.CodeMemberInUse)
	}
	f.call("PATCH", "/api/developer/dimensions/"+f.region+"/members/"+f.members["US"], map[string]any{"code": "USA", "label": "USA"})
	status, raw := f.req("PUT", "/api/developer/revisions/"+f.revID+"/activate", nil)
	if status != http.StatusBadRequest || !strings.Contains(string(raw), formula.CodeUnknownMember) || !strings.Contains(string(raw), "lk") {
		t.Errorf("activating with a LOOKUP of a renamed-away member: %d %s; want 400 %s naming lk", status, raw, formula.CodeUnknownMember)
	}

	// Only the formula in the body: the stored name is named in the cycle.
	status, raw = f.req("PATCH", "/api/developer/metrics/"+f.metric["upper"], map[string]any{"formula": "twice + 1"})
	if status != http.StatusBadRequest || strings.Contains(string(raw), f.metric["upper"]) || !strings.Contains(string(raw), "upper") {
		t.Errorf("PATCH with only a formula: %d %s; want 400 naming upper, not its id", status, raw)
	}

	status, raw = f.req("DELETE", "/api/developer/dimensions/"+f.region, nil)
	if status != http.StatusConflict || !strings.Contains(string(raw), metricformula.CodeDimensionInUse) || !strings.Contains(string(raw), "upper") {
		t.Errorf("deleting a dimension formulas name: %d %s; want 409 %s naming upper", status, raw, metricformula.CodeDimensionInUse)
	}
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM model.dimension_def WHERE id=$1::uuid`, f.region).Scan(&n); err != nil || n != 1 {
		t.Errorf("the refused delete removed the dimension (count %d, err %v)", n, err)
	}
}

// TestDebugViewsHonourDeveloperHiddenMembers: a developer with a hidden
// member gets no persisted calculation rows (they include values derived
// from hidden members), and the facts listing — which spans every
// revision — filters each fact by its own revision's rules.
func TestDebugViewsHonourDeveloperHiddenMembers(t *testing.T) {
	f := setupRestrictedFixture(t)
	var devID string
	if err := f.pool.QueryRow(context.Background(), `SELECT id::text FROM identity.user WHERE keycloak_sub=$1`, f.dev).Scan(&devID); err != nil {
		t.Fatal(err)
	}
	f.call("PUT", "/api/business-admin/users/"+devID+"/access-rules", f.admin, map[string]any{"rules": []map[string]string{
		{"rule_type": "dimension_member", "ref_id": f.members["US"], "access": "hidden"}}})

	combo := url.QueryEscape(fmt.Sprintf(`{%q:"UK",%q:"2026-02"}`, f.region, f.period))
	status, raw := f.req("GET", "/api/developer/debug/calc?dim_members="+combo, f.dev, nil)
	if status != http.StatusForbidden {
		t.Errorf("debug calc rows for a developer with a hidden member: %d %s; want 403", status, raw)
	}

	newRevisionAndActivate(f)
	status, raw = f.req("GET", "/api/developer/debug/facts", f.dev, nil)
	if status != http.StatusOK {
		t.Fatalf("debug facts: %d %s", status, raw)
	}
	var facts []struct {
		DimMembers string  `json:"dim_members"`
		MetricName string  `json:"metric_name"`
		Value      float64 `json:"value"`
	}
	if err := json.Unmarshal(raw, &facts); err != nil {
		t.Fatal(err)
	}
	if len(facts) == 0 {
		t.Fatal("debug facts: nothing listed")
	}
	for _, fc := range facts {
		if strings.Contains(fc.DimMembers, `"US"`) {
			t.Errorf("debug facts lists a hidden member's fact: %s %s = %v", fc.MetricName, fc.DimMembers, fc.Value)
		}
	}
}

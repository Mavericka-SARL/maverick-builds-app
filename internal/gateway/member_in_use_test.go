package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/metricformula"
)

// Deleting a member a formula names is refused with 409 MEMBER_IN_USE
// naming the metric, and the member is kept; a member no formula names
// deletes, and so does the named one once the formula stops naming it.
func TestMemberDeleteRefusedWhileFormulaNamesIt(t *testing.T) {
	f := setupDimFormulaFixture(t)
	ctx := context.Background()
	f.metric["lk_emea"] = f.call("POST", "/api/developer/metrics", f.metricBody("lk_emea", `LOOKUP(revenue, region, "EMEA")`))
	f.metric["us_only"] = f.call("POST", "/api/developer/metrics", f.metricBody("us_only", `SUMIFS(revenue, region, "<>US")`))

	emea := "/api/developer/dimensions/" + f.region + "/members/" + f.members["EMEA"]
	status, raw := f.req("DELETE", emea, nil)
	if status != http.StatusConflict || !strings.Contains(string(raw), metricformula.CodeMemberInUse) || !strings.Contains(string(raw), "lk_emea") {
		t.Fatalf("DELETE EMEA while lk_emea names it: %d %s; want 409 %s naming lk_emea", status, raw, metricformula.CodeMemberInUse)
	}
	var kept int
	if err := f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM model.dimension_member WHERE id=$1::uuid`, f.members["EMEA"]).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("EMEA after the refused delete: count %d err %v; want kept", kept, err)
	}

	// "<>US" can match other members: it does not name US.
	status, raw = f.req("DELETE", "/api/developer/dimensions/"+f.region+"/members/"+f.members["US"], nil)
	if status != http.StatusOK {
		t.Fatalf("DELETE US (named by no formula): %d %s; want 200", status, raw)
	}

	// Changing only the formula keeps the rest of the metric and frees EMEA.
	f.call("PATCH", "/api/developer/metrics/"+f.metric["lk_emea"], map[string]any{"formula": "revenue * 2"})
	status, raw = f.req("DELETE", emea, nil)
	if status != http.StatusOK {
		t.Fatalf("DELETE EMEA once no formula names it: %d %s; want 200", status, raw)
	}
}

// A name already taken in the revision is a 409 with a stable code, never a
// 500 carrying the SQL error; and PATCH with only a formula is a partial
// update that keeps the name and every other setting.
func TestDefinitionNameTakenAndPartialMetricPatch(t *testing.T) {
	f := setupDimFormulaFixture(t)
	ctx := context.Background()

	status, raw := f.req("POST", "/api/developer/dimensions", map[string]any{"name": "region", "revision_id": f.revID})
	if status != http.StatusConflict || !strings.Contains(string(raw), "DIMENSION_NAME_TAKEN") || strings.Contains(string(raw), "SQLSTATE") {
		t.Errorf("POST a second dimension region: %d %s; want 409 DIMENSION_NAME_TAKEN", status, raw)
	}
	status, raw = f.req("POST", "/api/developer/metrics", f.metricBody("revenue", ""))
	if status != http.StatusConflict || !strings.Contains(string(raw), "METRIC_NAME_TAKEN") || strings.Contains(string(raw), "SQLSTATE") {
		t.Errorf("POST a second metric revenue: %d %s; want 409 METRIC_NAME_TAKEN", status, raw)
	}

	body := f.metricBody("double", "revenue * 2")
	body["agg_rule"] = "average"
	body["format"] = "percent"
	body["format_decimals"] = 2
	body["time_summary"] = "last"
	f.metric["double"] = f.call("POST", "/api/developer/metrics", body)
	f.metric["triple"] = f.call("POST", "/api/developer/metrics", f.metricBody("triple", "revenue * 3"))

	// Two formula-only PATCHes in one revision: each keeps its name.
	for name, text := range map[string]string{"double": "revenue * 2 + 1", "triple": "revenue * 3 + 1"} {
		status, raw := f.req("PATCH", "/api/developer/metrics/"+f.metric[name], map[string]any{"formula": text})
		if status != http.StatusOK {
			t.Fatalf("PATCH %s with only a formula: %d %s; want 200", name, status, raw)
		}
	}
	var name, formulaText, agg, format, summary string
	var decimals int
	if err := f.pool.QueryRow(ctx, `SELECT name, formula, agg_rule, format, format_decimals, time_summary FROM model.metric_def WHERE id=$1::uuid`,
		f.metric["double"]).Scan(&name, &formulaText, &agg, &format, &decimals, &summary); err != nil {
		t.Fatal(err)
	}
	if name != "double" || formulaText != "revenue * 2 + 1" || agg != "average" || format != "percent" || decimals != 2 || summary != "last" {
		t.Errorf("after a formula-only PATCH: name=%q formula=%q agg=%q format=%q decimals=%d summary=%q; want the formula changed and the rest kept",
			name, formulaText, agg, format, decimals, summary)
	}

	// A name-only PATCH keeps the formula.
	f.call("PATCH", "/api/developer/metrics/"+f.metric["triple"], map[string]any{"name": "thrice"})
	if err := f.pool.QueryRow(ctx, `SELECT name, formula FROM model.metric_def WHERE id=$1::uuid`, f.metric["triple"]).Scan(&name, &formulaText); err != nil {
		t.Fatal(err)
	}
	if name != "thrice" || formulaText != "revenue * 3 + 1" {
		t.Errorf("after a name-only PATCH: name=%q formula=%q; want thrice, revenue * 3 + 1", name, formulaText)
	}

	// Renaming onto a taken name is the same 409.
	status, raw = f.req("PATCH", "/api/developer/metrics/"+f.metric["triple"], map[string]any{"name": "double"})
	if status != http.StatusConflict || !strings.Contains(string(raw), "METRIC_NAME_TAKEN") {
		t.Errorf("PATCH rename onto double: %d %s; want 409 METRIC_NAME_TAKEN", status, raw)
	}
}

// SWITCH on the dimension names each literal match value the way dim = "X"
// does, so deleting such a member is refused too; its trailing default is a
// result and names nothing.
func TestMemberDeleteRefusedWhileSwitchNamesIt(t *testing.T) {
	f := setupDimFormulaFixture(t)
	f.members["APAC"] = f.call("POST", "/api/developer/dimensions/"+f.region+"/members", map[string]any{"code": "APAC", "label": "APAC"})
	f.metric["sw"] = f.call("POST", "/api/developer/metrics", f.metricBody("sw", `SWITCH(region, "APAC", 1, 0) * revenue`))

	status, raw := f.req("DELETE", "/api/developer/dimensions/"+f.region+"/members/"+f.members["APAC"], nil)
	if status != http.StatusConflict || !strings.Contains(string(raw), metricformula.CodeMemberInUse) || !strings.Contains(string(raw), "sw") {
		t.Fatalf("DELETE APAC while SWITCH(region, \"APAC\", ...) names it: %d %s; want 409 %s naming sw", status, raw, metricformula.CodeMemberInUse)
	}
	f.call("PATCH", "/api/developer/metrics/"+f.metric["sw"], map[string]any{"formula": `SWITCH(region, "EMEA", 1, "APAC")`})
	status, raw = f.req("DELETE", "/api/developer/dimensions/"+f.region+"/members/"+f.members["APAC"], nil)
	if status != http.StatusOK {
		t.Fatalf("DELETE APAC when it is only SWITCH's default result: %d %s; want 200", status, raw)
	}
}

// A member code already used in its dimension is a 409 MEMBER_CODE_TAKEN on
// create and on a recode, for a standard and a time dimension, never a 500
// carrying the SQL error.
func TestMemberCodeTakenIsConflict(t *testing.T) {
	f := setupDimFormulaFixture(t)
	members := "/api/developer/dimensions/" + f.region + "/members"
	want := func(what string, status int, raw []byte) {
		t.Helper()
		if status != http.StatusConflict || !strings.Contains(string(raw), metricformula.CodeMemberCodeTaken) || strings.Contains(string(raw), "SQLSTATE") {
			t.Errorf("%s: %d %s; want 409 %s", what, status, raw, metricformula.CodeMemberCodeTaken)
		}
	}
	status, raw := f.req("POST", members, map[string]any{"code": "EMEA", "label": "again"})
	want("POST a second member EMEA", status, raw)
	status, raw = f.req("PATCH", members+"/"+f.members["EMEA"], map[string]any{"code": "US", "label": "EMEA"})
	want("PATCH EMEA's code to US", status, raw)

	month := f.call("POST", "/api/developer/dimensions", map[string]any{"name": "month", "revision_id": f.revID,
		"dimension_type": "time", "time_granularity": "month", "fiscal_year_start_month": 1})
	monthMembers := "/api/developer/dimensions/" + month + "/members"
	jan := f.call("POST", monthMembers, map[string]any{"code": "2026-01", "label": "Jan", "period_start": "2026-01-01", "period_end": "2026-01-31"})
	f.call("POST", monthMembers, map[string]any{"code": "2026-02", "label": "Feb", "period_start": "2026-02-01", "period_end": "2026-02-28"})
	status, raw = f.req("POST", monthMembers, map[string]any{"code": "2026-02", "label": "Feb again"})
	want("POST a second time member 2026-02", status, raw)
	status, raw = f.req("PATCH", monthMembers+"/"+jan, map[string]any{"code": "2026-02", "label": "Jan", "period_start": "2026-01-01", "period_end": "2026-01-31"})
	want("PATCH 2026-01's code to 2026-02", status, raw)
}

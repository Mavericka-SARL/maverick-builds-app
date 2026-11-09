package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// gridValues helpers over the dimensional-formula fixture: a second
// dimension, scenario (Base, Alt), and reads of a metric's stored values.

func (f *dimFormulaFixture) addScenario() string {
	f.t.Helper()
	scenario := f.call("POST", "/api/developer/dimensions", map[string]any{"name": "scenario", "revision_id": f.revID, "dimension_type": "standard"})
	for _, code := range []string{"Base", "Alt"} {
		f.call("POST", "/api/developer/dimensions/"+scenario+"/members", map[string]any{"code": code, "label": code})
	}
	return scenario
}

// typedValues answers a metric's latest typed value per coordinate, keyed by
// the coordinate's JSON (keys sorted).
func (f *dimFormulaFixture) typedValues(metric string) map[string]float64 {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(), `
		SELECT DISTINCT ON (dim_members) dim_members::text, value::float8 FROM runtime.fact_input
		WHERE metric_id=$1::uuid AND source_ref IS NULL ORDER BY dim_members, entered_at DESC, id DESC`, f.metric[metric])
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var k string
		var v float64
		if err := rows.Scan(&k, &v); err != nil {
			f.t.Fatal(err)
		}
		out[k] = v
	}
	return out
}

func coord(pairs ...string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(pairs); i += 2 {
		m[pairs[i]] = pairs[i+1]
	}
	raw, _ := json.Marshal(m)
	// jsonb's text form puts a space after ':' and ','.
	return strings.NewReplacer(`":"`, `": "`, `","`, `", "`).Replace(string(raw))
}

// A grid gaining a dimension asks which member its metrics' values belong
// to, and files them there (OBSERVATIONS "A grid gaining a dimension hides
// the values its metrics already hold"). The add used to answer 200 and leave
// every value keyed without the new dimension: the grid showed none of them
// and formulas read nothing.
func TestGridGainingADimensionKeepsItsValues(t *testing.T) {
	f := setupDimFormulaFixture(t)
	f.writeCell("revenue", "EMEA", 100)
	f.writeCell("revenue", "US", 50)
	f.metric["double"] = f.call("POST", "/api/developer/metrics", f.metricBody("double", "revenue*2"))
	f.call("POST", "/api/developer/grids/"+f.gridID+"/metrics/"+f.metric["double"], nil)
	scenario := f.addScenario()
	add := "/api/developer/grids/" + f.gridID + "/dimensions/" + scenario

	status, raw := f.req("POST", add, nil)
	if status != http.StatusConflict || !strings.Contains(string(raw), "VALUES_NEED_MEMBER") {
		t.Fatalf("add without a member: %d %s, want 409 VALUES_NEED_MEMBER", status, raw)
	}
	var onGrid bool
	if err := f.pool.QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM model.grid_dimension WHERE grid_id=$1::uuid AND dimension_id=$2::uuid)`,
		f.gridID, scenario).Scan(&onGrid); err != nil || onGrid {
		t.Fatalf("a refused add left the dimension on the grid (%v)", err)
	}
	if status, raw = f.req("POST", add, map[string]any{"member": "Nope"}); status != http.StatusBadRequest || !strings.Contains(string(raw), "UNKNOWN_MEMBER") {
		t.Errorf("add with an unknown member: %d %s, want 400 UNKNOWN_MEMBER", status, raw)
	}
	status, raw = f.req("POST", add, map[string]any{"member": "Base"})
	if status != http.StatusOK {
		t.Fatalf("add with a member: %d %s", status, raw)
	}
	var out struct {
		Moved int `json:"moved"`
	}
	_ = json.Unmarshal(raw, &out)
	if out.Moved != 2 {
		t.Errorf("moved = %d (%s), want 2", out.Moved, raw)
	}
	got := f.typedValues("revenue")
	if got[coord(f.region, "EMEA", scenario, "Base")] != 100 || got[coord(f.region, "US", scenario, "Base")] != 50 || len(got) != 2 {
		t.Errorf("revenue's values after the add = %v, want EMEA:Base 100 and US:Base 50", got)
	}
	f.awaitCalc(f.metric["double"], map[string]string{f.region: "EMEA", scenario: "Base"}, 200)

	// The grid shows them.
	status, raw = f.req("GET", fmt.Sprintf("/api/grid?revision_id=%s&grid_def_id=%s", f.revID, f.gridID), nil)
	if status != http.StatusOK || !strings.Contains(string(raw), `:EMEA:Base`) && !strings.Contains(string(raw), `:Base:EMEA`) {
		t.Errorf("grid after the add: %d, no EMEA/Base cell in %.300s", status, raw)
	}

	// A grid whose metrics hold no value takes a dimension without a member.
	empty := f.call("POST", "/api/developer/grids", map[string]any{"name": "Empty", "revision_id": f.revID})
	f.call("POST", "/api/developer/grids/"+empty+"/dimensions/"+scenario, nil)
}

// A posting into the grid's metrics that maps no member of the new dimension
// refuses the add: its values would land in no cell.
func TestGridGainingADimensionRefusedWhileAPostingDoesNotMapIt(t *testing.T) {
	f := setupDimFormulaFixture(t)
	ctx := context.Background()
	scenario := f.addScenario()
	var formID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO model.form_def (model_id, revision_id, name, label, fields)
		VALUES ($1::uuid, $2::uuid, 'sales', 'Sales', jsonb_build_array(
			jsonb_build_object('name','region','label','Region','type','dimension','dimension_id',$3::text),
			jsonb_build_object('name','amt','label','Amt','type','metric','metric_id',$4::text)
		)) RETURNING id::text`, f.modelID, f.revID, f.region, f.metric["revenue"]).Scan(&formID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `
		INSERT INTO model.form_metric_mapping (model_id, revision_id, form_id, grid_id, name, source_field, target_metric_id, dimension_mappings)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4::uuid, 'post sales', 'amt', $5::uuid, jsonb_build_object($6::text, 'region'))`,
		f.modelID, f.revID, formID, f.gridID, f.metric["revenue"], f.region); err != nil {
		t.Fatal(err)
	}
	status, raw := f.req("POST", "/api/developer/grids/"+f.gridID+"/dimensions/"+scenario, map[string]any{"member": "Base"})
	if status != http.StatusConflict || !strings.Contains(string(raw), "post sales") || !strings.Contains(string(raw), "POSTINGS_DO_NOT_FIT") {
		t.Errorf("add while a posting maps region only: %d %s, want 409 naming the posting", status, raw)
	}
}

// A metric moved onto a grid with a dimension its values lack is given the
// member they belong to; one moved onto a grid without a dimension its values
// carry has them combined along it by its own rule (owner decision
// 2026-11-09: add them up; an average is averaged, a rule that cannot combine
// stored values refuses).
func TestMetricMovedBetweenGridsKeepsItsValues(t *testing.T) {
	f := setupDimFormulaFixture(t)
	ctx := context.Background()
	scenario := f.addScenario()
	f.writeCell("revenue", "EMEA", 100)
	wide := f.call("POST", "/api/developer/grids", map[string]any{"name": "Wide", "revision_id": f.revID})
	f.call("POST", "/api/developer/grids/"+wide+"/dimensions/"+f.region, nil)
	f.call("POST", "/api/developer/grids/"+wide+"/dimensions/"+scenario, nil)

	// Onto a wider grid: the member is asked for.
	f.call("DELETE", "/api/developer/grids/"+f.gridID+"/metrics/"+f.metric["revenue"], nil)
	attach := "/api/developer/grids/" + wide + "/metrics/" + f.metric["revenue"]
	if status, raw := f.req("POST", attach, nil); status != http.StatusConflict || !strings.Contains(string(raw), "VALUES_NEED_MEMBER") || !strings.Contains(string(raw), scenario) {
		t.Fatalf("attach without members: %d %s, want 409 VALUES_NEED_MEMBER naming scenario", status, raw)
	}
	if status, raw := f.req("POST", attach, map[string]any{"members": map[string]string{scenario: "Alt"}}); status != http.StatusOK {
		t.Fatalf("attach with members: %d %s", status, raw)
	}
	if got := f.typedValues("revenue"); got[coord(f.region, "EMEA", scenario, "Alt")] != 100 || len(got) != 1 {
		t.Errorf("revenue after the move = %v, want EMEA:Alt 100", got)
	}

	// Onto a narrower grid: summed along scenario.
	write := func(metric string, dims map[string]string, v float64) {
		t.Helper()
		f.call("POST", "/api/cells", map[string]any{"model_id": f.modelID, "metric_id": f.metric[metric], "revision_id": f.revID, "dim_codes": dims, "value": v})
	}
	write("revenue", map[string]string{f.region: "EMEA", scenario: "Base"}, 20)
	write("revenue", map[string]string{f.region: "US", scenario: "Base"}, 7)
	f.call("DELETE", "/api/developer/grids/"+wide+"/metrics/"+f.metric["revenue"], nil)
	status, raw := f.req("POST", "/api/developer/grids/"+f.gridID+"/metrics/"+f.metric["revenue"], nil)
	if status != http.StatusOK {
		t.Fatalf("attach to the region grid: %d %s", status, raw)
	}
	var out struct {
		Combined int `json:"combined"`
	}
	_ = json.Unmarshal(raw, &out)
	if out.Combined != 3 {
		t.Errorf("combined = %d (%s), want 3", out.Combined, raw)
	}
	if got := f.typedValues("revenue"); got[coord(f.region, "EMEA")] != 120 || got[coord(f.region, "US")] != 7 || len(got) != 2 {
		t.Errorf("revenue summed along scenario = %v, want EMEA 120, US 7", got)
	}
	var reason string
	if err := f.pool.QueryRow(ctx, `SELECT delete_reason FROM runtime.fact_input_history WHERE metric_id=$1::uuid AND delete_reason <> '' LIMIT 1`,
		f.metric["revenue"]).Scan(&reason); err != nil || !strings.Contains(reason, "combined along scenario") {
		t.Errorf("cell history reason = %q (%v), want it to say the values were combined along scenario", reason, err)
	}

	// An average is averaged, not summed.
	avg := f.metricBody("price", "")
	avg["agg_rule"] = "average"
	f.metric["price"] = f.call("POST", "/api/developer/metrics", avg)
	f.call("POST", "/api/developer/grids/"+wide+"/metrics/"+f.metric["price"], nil)
	write("price", map[string]string{f.region: "EMEA", scenario: "Base"}, 10)
	write("price", map[string]string{f.region: "EMEA", scenario: "Alt"}, 20)
	f.call("DELETE", "/api/developer/grids/"+wide+"/metrics/"+f.metric["price"], nil)
	f.call("POST", "/api/developer/grids/"+f.gridID+"/metrics/"+f.metric["price"], nil)
	if got := f.typedValues("price"); got[coord(f.region, "EMEA")] != 15 || len(got) != 1 {
		t.Errorf("price averaged along scenario = %v, want EMEA 15", got)
	}

	// A rule that gives no total refuses, naming the metric and the dimension.
	none := f.metricBody("rank", "")
	none["agg_rule"] = "none"
	f.metric["rank"] = f.call("POST", "/api/developer/metrics", none)
	f.call("POST", "/api/developer/grids/"+wide+"/metrics/"+f.metric["rank"], nil)
	write("rank", map[string]string{f.region: "US", scenario: "Base"}, 1)
	f.call("DELETE", "/api/developer/grids/"+wide+"/metrics/"+f.metric["rank"], nil)
	if status, raw := f.req("POST", "/api/developer/grids/"+f.gridID+"/metrics/"+f.metric["rank"], nil); status != http.StatusConflict ||
		!strings.Contains(string(raw), "VALUES_CANNOT_COMBINE") || !strings.Contains(string(raw), "rank") || !strings.Contains(string(raw), "scenario") {
		t.Errorf("attach a none-rule metric to a narrower grid: %d %s, want 409 naming rank and scenario", status, raw)
	}
	if got := f.typedValues("rank"); got[coord(f.region, "US", scenario, "Base")] != 1 {
		t.Errorf("a refused move changed rank's values: %v", got)
	}
}

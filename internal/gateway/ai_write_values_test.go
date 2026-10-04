package gateway

import (
	"strings"
	"testing"
)

// The AI Developer writes input values as a developer types them into a
// grid: a setting on a grid with no dimensions (Actual Through Month = 9 had
// to be imported from a sheet of mixed text, which took models several turns
// or failed), and values by member. A total member, a missing dimension and a
// calculated metric are refused by the plan check.
func TestAIDeveloperWritesInputValues(t *testing.T) {
	b := runAIBuild(t, "Set Actual Through Month to 9 and the prices", []map[string]any{
		proposeStep("create_dimension", "Region", map[string]any{"name": "region", "members": []map[string]any{
			{"code": "ALL", "label": "All"}, {"code": "EU", "label": "Europe", "parent_code": "ALL"}, {"code": "US", "label": "US", "parent_code": "ALL"}}}),
		proposeStep("create_metric", "Setting", map[string]any{"name": "actual_through_month", "is_input": true}),
		proposeStep("create_metric", "Price", map[string]any{"name": "price", "is_input": true, "format": "currency"}),
		proposeStep("create_metric", "Double price", map[string]any{"name": "double_price", "formula": "price * 2"}),
		proposeStep("create_grid", "Setup", map[string]any{"name": "Setup", "metrics": []string{"actual_through_month"}}),
		proposeStep("create_grid", "Prices", map[string]any{"name": "Prices", "metrics": []string{"price", "double_price"}, "dimensions": []string{"region"}}),
		proposeStep("write_input_values", "Actual Through Month = 9", map[string]any{"metric_id": "actual_through_month", "values": []map[string]any{{"value": 9}}}),
		proposeStep("write_input_values", "Prices", map[string]any{"metric_id": "price", "values": []map[string]any{
			{"members": map[string]string{"region": "EU"}, "value": 10}, {"members": map[string]string{"Region": "US"}, "value": 12.5}}}),
	})
	latest := func(metric, code string) string {
		t.Helper()
		return b.q(`
			SELECT f.value::text FROM runtime.fact_input f JOIN model.metric_def m ON m.id = f.metric_id
			WHERE f.revision_id=$1::uuid AND m.name=$2 AND COALESCE(f.dim_members->>(SELECT d.id::text FROM model.dimension_def d WHERE d.revision_id=$1::uuid AND d.name='region'), '')=$3
			ORDER BY f.entered_at DESC LIMIT 1`, b.draft, metric, code)
	}
	if v := latest("actual_through_month", ""); v != "9" {
		t.Errorf("actual_through_month = %s, want 9", v)
	}
	if eu, us := latest("price", "EU"), latest("price", "US"); eu != "10" || us != "12.5" {
		t.Errorf("price EU/US = %s/%s, want 10/12.5", eu, us)
	}

	for what, tc := range map[string]struct {
		params map[string]any
		want   string
	}{
		"a total member":      {map[string]any{"metric_id": "price", "values": []map[string]any{{"members": map[string]string{"region": "ALL"}, "value": 1}}}, "has members under it"},
		"a missing member":    {map[string]any{"metric_id": "price", "values": []map[string]any{{"value": 1}}}, "names no member of region"},
		"a calculated metric": {map[string]any{"metric_id": "double_price", "values": []map[string]any{{"members": map[string]string{"region": "EU"}, "value": 1}}}, "is calculated"},
	} {
		got := b.proposeRefused("write "+what, []map[string]any{proposeStep("write_input_values", what, tc.params)})
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: the check said %q, want %q", what, got, tc.want)
		}
	}
}

package aiassistant_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// list_metrics reads one grid's metrics, the named ones, or those matching a
// text: an AI build re-read a 250-metric model whole 38 times (2026-11-07).
func TestListMetricsFilters(t *testing.T) {
	ctx := context.Background()
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	for _, p := range []map[string]any{
		{"name": "net_revenue", "is_input": true},
		{"name": "cost_of_sales", "is_input": true},
		{"name": "gross_margin", "formula": "net_revenue - cost_of_sales"},
	} {
		if _, _, err := exec.Execute(ctx, "create_metric", mustJSON(t, p)); err != nil {
			t.Fatalf("create %v: %v", p["name"], err)
		}
	}
	if _, _, err := exec.Execute(ctx, "create_grid", mustJSON(t, map[string]any{"name": "Revenue", "metrics": []string{"net_revenue"}})); err != nil {
		t.Fatalf("create grid: %v", err)
	}
	read := aiassistant.NewToolExecutor(pool, modelID, revID)
	list := func(args map[string]any) string {
		t.Helper()
		raw, _ := json.Marshal(args)
		out, err := read.Execute(ctx, "list_metrics", raw)
		if err != nil {
			t.Fatalf("list_metrics %v: %v", args, err)
		}
		return out
	}
	for _, tc := range []struct {
		args       map[string]any
		has, hasnt []string
	}{
		{map[string]any{"grid": "Revenue"}, []string{"net_revenue"}, []string{"cost_of_sales", "gross_margin"}},
		{map[string]any{"names": []string{"COST_OF_SALES"}}, []string{"cost_of_sales"}, []string{"net_revenue", "gross_margin"}},
		{map[string]any{"search": "revenue -"}, []string{"gross_margin"}, []string{"[INPUT]"}},
		{map[string]any{"search": "nothing like it"}, []string{"(none match)"}, []string{"net_revenue"}},
		{map[string]any{}, []string{"net_revenue", "cost_of_sales", "gross_margin"}, nil},
	} {
		out := list(tc.args)
		for _, h := range tc.has {
			if !strings.Contains(out, h) {
				t.Errorf("list_metrics %v lacks %q:\n%s", tc.args, h, out)
			}
		}
		for _, h := range tc.hasnt {
			if strings.Contains(out, h) {
				t.Errorf("list_metrics %v has %q:\n%s", tc.args, h, out)
			}
		}
	}
}

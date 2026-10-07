package aiassistant_test

import (
	"context"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// create_metric left without format_decimals shows two decimal places for
// a number, currency or percentage — not none, which showed a USD 0.3m
// increment as 0 and a 5.6% rate as 6% in every model the assistant built —
// and none for a date or text; a step that sets them keeps its choice.
func TestAIMetricDecimalsDefault(t *testing.T) {
	ctx := context.Background()
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)

	for _, tc := range []struct {
		params map[string]any
		want   int
	}{
		{map[string]any{"name": "units", "is_input": true}, 2},
		{map[string]any{"name": "increment", "is_input": true, "format": "currency"}, 2},
		{map[string]any{"name": "rate", "is_input": true, "format": "percentage", "agg_rule": "average", "time_summary": "average"}, 2},
		{map[string]any{"name": "note", "is_input": true, "format": "text"}, 0},
		{map[string]any{"name": "hire_date", "is_input": true, "format": "date"}, 0},
		{map[string]any{"name": "cutoff", "is_input": true, "format_decimals": 0}, 0},
		{map[string]any{"name": "price", "is_input": true, "format": "currency", "format_decimals": 1}, 1},
	} {
		_, id, err := exec.Execute(ctx, "create_metric", mustJSON(t, tc.params))
		if err != nil {
			t.Fatalf("create_metric %v: %v", tc.params["name"], err)
		}
		var got int
		if err := pool.QueryRow(ctx, `SELECT format_decimals FROM model.metric_def WHERE id=$1::uuid`, id).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != tc.want {
			t.Errorf("%v: format_decimals = %d, want %d", tc.params["name"], got, tc.want)
		}
	}
}

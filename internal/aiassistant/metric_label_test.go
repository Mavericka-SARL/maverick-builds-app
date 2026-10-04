package aiassistant_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// A metric's display label through the AI Developer's tools, as the console
// sets it: create_metric takes one, update_metric changes or clears it (and
// keeps it when the step says nothing), and list_metrics shows it.
func TestAIMetricLabel(t *testing.T) {
	ctx := context.Background()
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)

	labelOf := func(id string) string {
		t.Helper()
		var l *string
		if err := pool.QueryRow(ctx, `SELECT label FROM model.metric_def WHERE id=$1::uuid`, id).Scan(&l); err != nil {
			t.Fatalf("read label: %v", err)
		}
		if l == nil {
			return "<null>"
		}
		return *l
	}
	run := func(tool string, params map[string]any) string {
		t.Helper()
		_, id, err := exec.Execute(ctx, tool, mustJSON(t, params))
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		return id
	}

	id := run("create_metric", map[string]any{"name": "r_and_d", "label": " R&D ", "is_input": true})
	if got := labelOf(id); got != "R&D" {
		t.Errorf("create_metric label = %q", got)
	}
	out, err := aiassistant.NewToolExecutor(pool, modelID, revID).Execute(ctx, "list_metrics", nil)
	if err != nil || !strings.Contains(out, `"R&D"`) {
		t.Errorf("list_metrics does not show the label: %v\n%s", err, out)
	}
	run("update_metric", map[string]any{"metric_id": id, "format": "currency"})
	if got := labelOf(id); got != "R&D" {
		t.Errorf("update_metric without a label changed it to %q", got)
	}
	run("update_metric", map[string]any{"metric_id": id, "label": "Research & Development"})
	if got := labelOf(id); got != "Research & Development" {
		t.Errorf("update_metric label = %q", got)
	}
	run("update_metric", map[string]any{"metric_id": id, "label": ""})
	if got := labelOf(id); got != "<null>" {
		t.Errorf("update_metric \"\" left %q, want it cleared", got)
	}
}

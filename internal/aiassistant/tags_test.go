package aiassistant_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// Tags through the AI Developer's tools: the developer can tag a metric,
// dimension or dashboard when creating it and change the tags afterwards,
// so the AI can too — and the listings it reads show them.

func TestAITags(t *testing.T) {
	ctx := context.Background()
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)

	tagsOf := func(table, id string) []string {
		t.Helper()
		var out []string
		if err := pool.QueryRow(ctx, `SELECT tags FROM `+table+` WHERE id=$1::uuid`, id).Scan(&out); err != nil {
			t.Fatalf("read %s tags: %v", table, err)
		}
		return out
	}
	run := func(tool string, params map[string]any) (string, string) {
		t.Helper()
		result, id, err := exec.Execute(ctx, tool, mustJSON(t, params))
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		return result, id
	}

	// Created with tags, stored the way the console stores them.
	_, metricID := run("create_metric", map[string]any{"name": "units", "is_input": true, "tags": []string{"Volume", " Unit Count ", "volume"}})
	if got := tagsOf("model.metric_def", metricID); !slices.Equal(got, []string{"volume", "unit-count"}) {
		t.Errorf("create_metric tags = %v", got)
	}
	_, dimID := run("create_dimension", map[string]any{"name": "channel", "tags": []string{"sales"},
		"members": []map[string]any{{"code": "WEB", "label": "Web"}}})
	if got := tagsOf("model.dimension_def", dimID); !slices.Equal(got, []string{"sales"}) {
		t.Errorf("create_dimension tags = %v", got)
	}
	_, dashID := run("create_dashboard", map[string]any{"name": "Overview", "tags": []string{"Board Pack"}})
	if got := tagsOf("model.dashboard_def", dashID); !slices.Equal(got, []string{"board-pack"}) {
		t.Errorf("create_dashboard tags = %v", got)
	}

	// update_metric keeps the tags when it says nothing about them, replaces
	// them when it does.
	run("update_metric", map[string]any{"metric_id": metricID, "name": "units", "format": "number"})
	if got := tagsOf("model.metric_def", metricID); !slices.Equal(got, []string{"volume", "unit-count"}) {
		t.Errorf("update_metric without tags changed them: %v", got)
	}
	run("update_metric", map[string]any{"metric_id": metricID, "name": "units", "format": "number", "tags": []string{"ops"}})
	if got := tagsOf("model.metric_def", metricID); !slices.Equal(got, []string{"ops"}) {
		t.Errorf("update_metric tags = %v", got)
	}

	// set_tags on each kind, by id or by exact name; [] clears.
	run("set_tags", map[string]any{"kind": "dimension", "id": "channel", "tags": []string{"Go To Market", "sales"}})
	if got := tagsOf("model.dimension_def", dimID); !slices.Equal(got, []string{"go-to-market", "sales"}) {
		t.Errorf("set_tags dimension by name = %v", got)
	}
	run("set_tags", map[string]any{"kind": "dashboard", "id": dashID, "tags": []string{"exec"}})
	if got := tagsOf("model.dashboard_def", dashID); !slices.Equal(got, []string{"exec"}) {
		t.Errorf("set_tags dashboard by id = %v", got)
	}
	result, _ := run("set_tags", map[string]any{"kind": "metric", "id": "units", "tags": []string{}})
	if got := tagsOf("model.metric_def", metricID); len(got) != 0 || !strings.Contains(result, "cleared") {
		t.Errorf("set_tags [] = %v (%q)", got, result)
	}

	// Refusals: unknown kind, no id, tags left out, something outside the model.
	otherModel := seedModel(t, pool)
	otherRev := seedRevision(t, pool, otherModel, "Other")
	var foreignMetric string
	if err := pool.QueryRow(ctx, `INSERT INTO model.metric_def (model_id, revision_id, name, is_input) VALUES ($1::uuid, $2::uuid, 'foreign', true) RETURNING id::text`,
		otherModel, otherRev).Scan(&foreignMetric); err != nil {
		t.Fatal(err)
	}
	for name, params := range map[string]map[string]any{
		"unknown kind":  {"kind": "grid", "id": "x", "tags": []string{"a"}},
		"no id":         {"kind": "metric", "tags": []string{"a"}},
		"no tags":       {"kind": "metric", "id": metricID},
		"foreign model": {"kind": "metric", "id": foreignMetric, "tags": []string{"a"}},
	} {
		if _, _, err := exec.Execute(ctx, "set_tags", mustJSON(t, params)); err == nil {
			t.Errorf("set_tags with %s: want an error", name)
		}
	}
	var foreignTags []string
	_ = pool.QueryRow(ctx, `SELECT tags FROM model.metric_def WHERE id=$1::uuid`, foreignMetric).Scan(&foreignTags)
	if len(foreignTags) != 0 {
		t.Errorf("another model's metric was tagged: %v", foreignTags)
	}

	// The listings the AI reads show the tags.
	run("set_tags", map[string]any{"kind": "metric", "id": "units", "tags": []string{"ops"}})
	reader := aiassistant.NewToolExecutor(pool, modelID, revID)
	for tool, want := range map[string]string{
		"list_metrics":    "tags: ops",
		"list_dimensions": "tags: go-to-market, sales",
		"list_dashboards": "tags: exec",
	} {
		out, err := reader.Execute(ctx, tool, nil)
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		if !strings.Contains(out, want) {
			t.Errorf("%s does not show %q:\n%s", tool, want, out)
		}
	}
}

// TestListDimensionsIsTheWorkingRevisions: list_dimensions read every
// revision of the model at once, so after a revision copy each member was
// listed once per revision, and a dimension with no members yet was not
// listed at all.
func TestListDimensionsIsTheWorkingRevisions(t *testing.T) {
	ctx := context.Background()
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revA := seedRevision(t, pool, modelID, "Rev A")
	revB := seedRevision(t, pool, modelID, "Rev B")
	for _, rev := range []string{revA, revB} {
		var dimID string
		if err := pool.QueryRow(ctx, `INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'region') RETURNING id::text`,
			modelID, rev).Scan(&dimID); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'EU', 'Europe')`, dimID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'empty_one')`, modelID, revA); err != nil {
		t.Fatal(err)
	}

	out, err := aiassistant.NewToolExecutor(pool, modelID, revA).Execute(ctx, "list_dimensions", nil)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out, "EU (Europe)"); n != 1 {
		t.Errorf("member EU listed %d times, want once:\n%s", n, out)
	}
	if !strings.Contains(out, "Dimension: empty_one") {
		t.Errorf("a dimension with no members is missing:\n%s", out)
	}
}

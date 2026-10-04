package aiassistant_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
)

// The AI's create_metric / create_dimension / update_metric refuse a taken
// name with the developer endpoint's codes, and update_metric is a partial
// update: a step carrying only the formula keeps the name and settings.
func TestAINameTakenAndPartialUpdateMetric(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()
	codeOf := func(err error) string {
		var ve *metricformula.ValidationError
		if err != nil && errors.As(err, &ve) {
			return ve.Code
		}
		return ""
	}

	if _, _, err := exec.Execute(ctx, "create_dimension", mustJSON(t, map[string]any{"name": "region"})); err != nil {
		t.Fatal(err)
	}
	_, _, err := exec.Execute(ctx, "create_dimension", mustJSON(t, map[string]any{"name": "region"}))
	if codeOf(err) != metricformula.CodeDimensionNameTaken {
		t.Errorf("second create_dimension region: %v; want %s", err, metricformula.CodeDimensionNameTaken)
	}
	for _, m := range []map[string]any{
		{"name": "revenue", "is_input": true},
		{"name": "double", "formula": "revenue * 2", "agg_rule": "average", "format": "percentage", "format_decimals": 2},
		{"name": "triple", "formula": "revenue * 3"},
	} {
		if _, _, err := exec.Execute(ctx, "create_metric", mustJSON(t, m)); err != nil {
			t.Fatalf("create_metric %v: %v", m["name"], err)
		}
	}
	_, _, err = exec.Execute(ctx, "create_metric", mustJSON(t, map[string]any{"name": "revenue", "is_input": true}))
	if codeOf(err) != metricformula.CodeMetricNameTaken {
		t.Errorf("second create_metric revenue: %v; want %s", err, metricformula.CodeMetricNameTaken)
	}

	id := func(name string) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, `SELECT id::text FROM model.metric_def WHERE revision_id=$1::uuid AND name=$2`, revID, name).Scan(&id); err != nil {
			t.Fatalf("metric %s: %v", name, err)
		}
		return id
	}
	double, triple := id("double"), id("triple")
	for mid, text := range map[string]string{double: "revenue * 2 + 1", triple: "revenue * 3 + 1"} {
		if _, _, err := exec.Execute(ctx, "update_metric", mustJSON(t, map[string]any{"metric_id": mid, "formula": text})); err != nil {
			t.Fatalf("formula-only update_metric: %v", err)
		}
	}
	var name, text, agg, format string
	var decimals int
	if err := pool.QueryRow(ctx, `SELECT name, formula, agg_rule, format, format_decimals FROM model.metric_def WHERE id=$1::uuid`, double).
		Scan(&name, &text, &agg, &format, &decimals); err != nil {
		t.Fatal(err)
	}
	if name != "double" || text != "revenue * 2 + 1" || agg != "average" || format != "percentage" || decimals != 2 {
		t.Errorf("after a formula-only update_metric: %q %q %q %q %d; want the formula changed and the rest kept", name, text, agg, format, decimals)
	}
	if _, _, err := exec.Execute(ctx, "update_metric", mustJSON(t, map[string]any{"metric_id": triple, "name": "thrice"})); err != nil {
		t.Fatalf("name-only update_metric: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT name, formula FROM model.metric_def WHERE id=$1::uuid`, triple).Scan(&name, &text); err != nil {
		t.Fatal(err)
	}
	if name != "thrice" || text != "revenue * 3 + 1" {
		t.Errorf("after a name-only update_metric: %q %q; want thrice, revenue * 3 + 1", name, text)
	}
	_, _, err = exec.Execute(ctx, "update_metric", mustJSON(t, map[string]any{"metric_id": triple, "name": "double"}))
	if codeOf(err) != metricformula.CodeMetricNameTaken {
		t.Errorf("update_metric rename onto double: %v; want %s", err, metricformula.CodeMetricNameTaken)
	}
}

// add_dimension_member refuses a code the dimension already has with the
// developer endpoint's MEMBER_CODE_TAKEN, for a standard and a time
// dimension, instead of a wrapped SQL error.
func TestAIAddDimensionMemberCodeTaken(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	revID := seedRevision(t, pool, modelID, "Rev A")
	exec := aiassistant.NewWriteExecutor(pool, modelID, revID)
	ctx := context.Background()
	codeOf := func(err error) string {
		var ve *metricformula.ValidationError
		if err != nil && errors.As(err, &ve) {
			return ve.Code
		}
		return ""
	}

	_, regionID, err := exec.Execute(ctx, "create_dimension", mustJSON(t, map[string]any{"name": "region"}))
	if err != nil {
		t.Fatal(err)
	}
	add := map[string]any{"dimension_id": regionID, "code": "EMEA", "label": "EMEA"}
	if _, _, err := exec.Execute(ctx, "add_dimension_member", mustJSON(t, add)); err != nil {
		t.Fatal(err)
	}
	_, _, err = exec.Execute(ctx, "add_dimension_member", mustJSON(t, add))
	if codeOf(err) != metricformula.CodeMemberCodeTaken {
		t.Errorf("second add_dimension_member EMEA: %v; want %s", err, metricformula.CodeMemberCodeTaken)
	}

	_, monthID, err := exec.Execute(ctx, "create_dimension", mustJSON(t, map[string]any{"name": "month", "dimension_type": "time",
		"time_granularity": "month", "fiscal_year_start_month": 1}))
	if err != nil {
		t.Fatal(err)
	}
	jan := map[string]any{"dimension_id": monthID, "code": "2026-01", "label": "Jan 2026",
		"period_start": "2026-01-01", "period_end": "2026-01-31"}
	if _, _, err := exec.Execute(ctx, "add_dimension_member", mustJSON(t, jan)); err != nil {
		t.Fatal(err)
	}
	_, _, err = exec.Execute(ctx, "add_dimension_member", mustJSON(t, jan))
	if codeOf(err) != metricformula.CodeMemberCodeTaken {
		t.Errorf("second add_dimension_member 2026-01: %v; want %s", err, metricformula.CodeMemberCodeTaken)
	}
}

package gateway

// A table-to-metric mapping names only its own model's table, grid, input
// metric and dimensions, in its own revision, and fields its table has. The
// developer API used to store whatever ids a body named: a developer of one
// tenant could map another tenant's table and backfill its records into
// their own metric. One stored before the check posts nothing.

import (
	"context"
	"net/http"
	"testing"
	"time"
)

func TestMappingNamesOnlyItsOwnModel(t *testing.T) {
	f := setupRecordFixture(t)
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	// Tenant B's model, with a table that holds a record, an input metric, a
	// grid and a dimension.
	modelB := q(`SELECT m.id::text FROM core.model m JOIN core.application a ON a.id = m.application_id WHERE a.name = 'B app'`)
	formB := q(`INSERT INTO model.form_def (model_id, name, label, fields) VALUES ($1::uuid, 'salaries', 'Salaries',
		'[{"name":"amount","label":"Amount","type":"number"}]') RETURNING id::text`, modelB)
	q(`INSERT INTO runtime.form_record (form_id, data) VALUES ($1::uuid, '{"amount": 90000}') RETURNING id::text`, formB)
	metricB := q(`INSERT INTO model.metric_def (model_id, name, is_input, agg_rule) VALUES ($1::uuid, 'pay', true, 'sum') RETURNING id::text`, modelB)
	gridB := q(`INSERT INTO model.grid_def (model_id, name) VALUES ($1::uuid, 'Pay') RETURNING id::text`, modelB)
	dimB := q(`INSERT INTO model.dimension_def (model_id, name) VALUES ($1::uuid, 'person') RETURNING id::text`, modelB)

	// Tenant A's own input metric.
	metricA := idOf(decodeObj(t, f.expect(http.StatusOK, http.MethodPost, "/api/developer/metrics", prDev, map[string]any{
		"name": "expense_total", "is_input": true, "agg_rule": "sum", "format": "number", "revision_id": f.revID,
	})))
	path := "/api/developer/form-integrations?revision_id=" + f.revID
	mapping := func(over map[string]any) map[string]any {
		m := map[string]any{"form_id": f.formID, "name": "expenses", "source_field": "amount", "target_metric_id": metricA,
			"aggregation": "sum", "posting_statuses": []string{"approved"}, "live_posting": true}
		for k, v := range over {
			m[k] = v
		}
		return m
	}
	for name, over := range map[string]map[string]any{
		"another tenant's table":     {"form_id": formB},
		"another tenant's metric":    {"target_metric_id": metricB},
		"another tenant's grid":      {"grid_id": gridB},
		"another tenant's dimension": {"dimension_mappings": map[string]string{dimB: "amount"}},
		"a field the table lacks":    {"source_field": "salary"},
		"a malformed id":             {"form_id": "not-an-id"},
	} {
		if status, raw := f.do(http.MethodPost, path, prDev, mapping(over)); status != http.StatusBadRequest {
			t.Errorf("a mapping naming %s: %d %s, want 400", name, status, raw)
		}
	}
	own := idOf(decodeObj(t, f.expect(http.StatusOK, http.MethodPost, path, prDev, mapping(nil))))
	if status, raw := f.do(http.MethodPatch, "/api/developer/form-integrations/"+own, prDev, mapping(map[string]any{"target_metric_id": metricB})); status != http.StatusBadRequest {
		t.Errorf("retargeting a mapping to another tenant's metric: %d %s, want 400", status, raw)
	}

	// A mapping stored before the check, naming tenant B's table: a backfill
	// reads none of B's records into A's metric.
	stale := q(`INSERT INTO model.form_metric_mapping (model_id, revision_id, form_id, name, source_field, target_metric_id, posting_statuses)
		VALUES ($1::uuid, $2::uuid, $3::uuid, 'stale', 'amount', $4::uuid, '{approved}') RETURNING id::text`, f.modelID, f.revID, formB, metricA)
	raw := f.expect(http.StatusOK, http.MethodPost, "/api/developer/form-integrations/"+stale+"/backfill", prDev, nil)
	if got := decodeObj(t, raw)["records_processed"]; got != 0.0 {
		t.Errorf("backfill of a mapping naming another tenant's table processed %v record(s), want 0", got)
	}
	time.Sleep(300 * time.Millisecond)
	var facts int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.fact_input WHERE metric_id = $1::uuid`, metricA).Scan(&facts)
	if facts != 0 {
		t.Errorf("tenant A's metric holds %d fact(s) from tenant B's records", facts)
	}
}

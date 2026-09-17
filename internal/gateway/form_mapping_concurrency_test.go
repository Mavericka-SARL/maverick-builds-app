package gateway

import (
	"context"
	"sync"
	"testing"
	"time"
)

// TestFormMappingConcurrentPostingsConverge is a regression test for a race
// in recomputeFactInput: it used to read runtime.form_record_posting,
// DELETE the mapping's existing runtime.fact_input row, then INSERT the
// freshly computed aggregate — all as three separate unguarded statements.
// applyFormMappings runs this from an unguarded `go func()` per record
// status-transition (see recordAction / the /api/forms/{id}/records POST
// handler), so approving several records against the same mapping in quick
// succession schedules multiple concurrent calls. Without serialization,
// two overlapping delete-then-insert passes can interleave (both DELETE,
// then both INSERT) and leave more than one snapshot behind — and since
// grid()'s read path SUMS every source_ref-tagged fact_input row for a
// cell (form postings are additive by design, unlike direct-entry rows,
// which are latest-wins), a leftover stale snapshot gets silently counted
// again on top of the correct one, inflating the metric's total.
//
// Found via cmd/qa-engine-test/form_agg.go: posting 3 records (10, 20, 30)
// into a sum-aggregation mapping intermittently produced a total of 190
// instead of 160 (100 direct + 60 posted) because of a leftover stale
// snapshot row. Fixed by wrapping the read-delete-insert sequence in a
// transaction holding a Postgres advisory lock keyed on the mapping ID, so
// concurrent recomputes for the SAME mapping serialize instead of
// interleaving (different mappings still run fully in parallel).
func TestFormMappingConcurrentPostingsConverge(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	// A scalar (no dimension) input metric — keeps this test focused on
	// the posting/aggregation race, not dimension resolution.
	targetMetricID := q(`
		INSERT INTO model.metric_def (model_id, name, is_input, agg_rule, revision_id)
		VALUES ($1::uuid, 'concurrency_target', true, 'sum', $2::uuid)
		RETURNING id::text`, f.modelID, f.workingRevID)

	formID := q(`
		INSERT INTO model.form_def (model_id, revision_id, name, label, fields)
		VALUES ($1::uuid, $2::uuid, 'concurrency_form', 'Concurrency Form',
			jsonb_build_array(jsonb_build_object('name','amount','label','Amount','type','number')))
		RETURNING id::text`, f.modelID, f.workingRevID)

	mappingID := q(`
		INSERT INTO model.form_metric_mapping
		  (model_id, revision_id, form_id, grid_id, name, source_field, target_metric_id,
		   aggregation, posting_statuses, dimension_mappings, live_posting)
		VALUES ($1::uuid, $2::uuid, $3::uuid, NULL, 'concurrency mapping', 'amount', $4::uuid,
		        'sum', ARRAY['approved'], '{}'::jsonb, true)
		RETURNING id::text`, f.modelID, f.workingRevID, formID, targetMetricID)

	devPersonas["rollup-test-manager-concurrency"] = "test-manager"
	t.Cleanup(func() { delete(devPersonas, "rollup-test-manager-concurrency") })

	// Create N records as draft first (sequential — draft status never
	// matches posting_statuses=["approved"], so this can't itself trigger
	// a recompute), then transition ALL of them to "approved" CONCURRENTLY
	// — this is what actually races multiple recomputeFactInput calls
	// against the same mapping_id.
	const n = 8
	amounts := []float64{10, 20, 30, 40, 50, 60, 70, 80} // sum = 360
	recordIDs := make([]string, n)
	for i := 0; i < n; i++ {
		status, body := f.do(t, "POST", "/api/forms/"+formID+"/records", "rollup-test-manager-concurrency", map[string]any{
			"data": map[string]any{"amount": amounts[i]},
		})
		if status != 200 {
			t.Fatalf("create record %d: status=%d body=%v", i, status, body)
		}
		recID, _ := body["id"].(string)
		if recID == "" {
			t.Fatalf("create record %d: no id in response %v", i, body)
		}
		recordIDs[i] = recID
	}

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(recID string, amount float64) {
			defer wg.Done()
			status, body := f.do(t, "PUT", "/api/records/"+recID, "rollup-test-manager-concurrency", map[string]any{
				"data":   map[string]any{"amount": amount},
				"status": "approved",
			})
			if status != 200 {
				t.Errorf("approve record %s: status=%d body=%v", recID, status, body)
			}
		}(recordIDs[i], amounts[i])
	}
	wg.Wait()

	// applyFormMappings is fired from its own `go func()` per request, so
	// wait for the fact_input row to converge to the correct sum rather
	// than asserting immediately.
	const expected = 360.0
	deadline := time.Now().Add(10 * time.Second)
	var rowCount int
	var value float64
	for {
		if err := f.pool.QueryRow(ctx, `
			SELECT count(*), COALESCE(SUM(value), 0) FROM runtime.fact_input
			WHERE source_ref=$1::uuid AND metric_id=$2::uuid
		`, mappingID, targetMetricID).Scan(&rowCount, &value); err != nil {
			t.Fatalf("query fact_input: %v", err)
		}
		if rowCount == 1 && value == expected {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("did not converge: rowCount=%d value=%v (want rowCount=1 value=%v) — %d concurrent approvals left stale/duplicate snapshot rows behind",
				rowCount, value, expected, n)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Give any straggler goroutine a moment, then assert it stays settled —
	// exactly one row, exactly the right value, no further drift.
	time.Sleep(300 * time.Millisecond)
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(SUM(value), 0) FROM runtime.fact_input
		WHERE source_ref=$1::uuid AND metric_id=$2::uuid
	`, mappingID, targetMetricID).Scan(&rowCount, &value); err != nil {
		t.Fatalf("query fact_input (final): %v", err)
	}
	if rowCount != 1 {
		t.Errorf("expected exactly 1 fact_input row for this mapping after settling, got %d (stale/duplicate snapshots from concurrent recomputes)", rowCount)
	}
	if value != expected {
		t.Errorf("expected fact_input value %v after settling, got %v", expected, value)
	}
}

// TestFormMappingRecalculatesItsOwnRevisionNotTheModelsActiveOne is a
// regression test: applyFormMappings' final recalc used to re-query the
// model's active_revision_id and recalc THAT, discarding the per-mapping
// revisionID actually used for the fact_input/posting write inside the
// loop — silently recalculating the wrong revision whenever a mapping's own
// revision differs from the model's active one (a revision-scoped mapping,
// or the common pre-promote case where active_revision_id is unset). This
// fixture's model has active_revision_id=workingRevID; this mapping is
// deliberately scoped to a separate, non-system-managed revision instead
// (f.annualRevID is system_managed=true — the on-approve-copy target
// revision used by other tests in this fixture, deliberately read-only to
// direct writes, so unsuitable here), so the bug (recalc always targeting
// workingRevID) would mean target_double's calc_result in the mapping's
// own revision never appears at all.
func TestFormMappingRecalculatesItsOwnRevisionNotTheModelsActiveOne(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	otherRevID := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Q2') RETURNING id::text`, f.modelID)

	targetMetricID := q(`
		INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule)
		VALUES ($1::uuid, $2::uuid, 'annual_target', true, 'sum')
		RETURNING id::text`, f.modelID, otherRevID)
	doubleMetricID := q(`
		INSERT INTO model.metric_def (model_id, revision_id, name, is_input, formula)
		VALUES ($1::uuid, $2::uuid, 'annual_target_double', false, '=annual_target*2')
		RETURNING id::text`, f.modelID, otherRevID)
	exec(`INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id) VALUES ($1::uuid, $2::uuid)`,
		doubleMetricID, targetMetricID)

	formID := q(`
		INSERT INTO model.form_def (model_id, revision_id, name, label, fields)
		VALUES ($1::uuid, $2::uuid, 'annual_form', 'Annual Form',
			jsonb_build_array(jsonb_build_object('name','amount','label','Amount','type','number')))
		RETURNING id::text`, f.modelID, otherRevID)
	mappingID := q(`
		INSERT INTO model.form_metric_mapping
		  (model_id, revision_id, form_id, grid_id, name, source_field, target_metric_id,
		   aggregation, posting_statuses, dimension_mappings, live_posting)
		VALUES ($1::uuid, $2::uuid, $3::uuid, NULL, 'annual mapping', 'amount', $4::uuid,
		        'sum', ARRAY['approved'], '{}'::jsonb, true)
		RETURNING id::text`, f.modelID, otherRevID, formID, targetMetricID)

	devPersonas["rollup-test-manager-annual"] = "test-manager"
	t.Cleanup(func() { delete(devPersonas, "rollup-test-manager-annual") })

	status, body := f.do(t, "POST", "/api/forms/"+formID+"/records", "rollup-test-manager-annual", map[string]any{
		"data": map[string]any{"amount": 25.0},
	})
	if status != 200 {
		t.Fatalf("create record: status=%d body=%v", status, body)
	}
	recordID, _ := body["id"].(string)
	if recordID == "" {
		t.Fatalf("create record: no id in response %v", body)
	}
	if status, body := f.do(t, "PUT", "/api/records/"+recordID, "rollup-test-manager-annual", map[string]any{
		"data": map[string]any{"amount": 25.0}, "status": "approved",
	}); status != 200 {
		t.Fatalf("approve record: status=%d body=%v", status, body)
	}

	// Every record status-transition fires its own detached
	// applyFormMappings call (see TestFormMappingConcurrentPostingsConverge's
	// own comment above) — including this record's own earlier
	// creation-as-draft call, which (via this fix's retraction-branch
	// tracking) triggers its own harmless recalc computing 0 (no fact
	// posted yet). That call races the approval call's recalc with no
	// ordering guarantee, so polling for "any row present" can catch the
	// stale intermediate 0 before the real 50 lands. Poll for the actual
	// expected value instead, matching the sibling concurrency test's own
	// convergence-polling style, with a generous timeout for CI.
	waitForValue := func(query string, want float64, args ...any) bool {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for {
			var v float64
			if err := f.pool.QueryRow(ctx, query, args...).Scan(&v); err == nil && v == want {
				return true
			}
			if time.Now().After(deadline) {
				return false
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	calcQuery := `
		SELECT value::float8 FROM runtime.calc_result
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid AND dim_members='{}'
		ORDER BY calc_at DESC LIMIT 1
	`
	if !waitForValue(calcQuery, 50.0, f.modelID, otherRevID, doubleMetricID) {
		var got float64
		_ = f.pool.QueryRow(ctx, calcQuery, f.modelID, otherRevID, doubleMetricID).Scan(&got)
		t.Fatalf("annual_target_double never converged to 50 (25*2) in the mapping's own revision, last seen %v — recalc targeted the wrong revision (or never ran), matching the pre-fix bug (recalc always re-derived the model's active_revision_id, discarding the mapping's own)", got)
	}

	// Retraction branch: moving the record OUT of an eligible status must
	// also recalc against the mapping's own revision — this branch never
	// tracked itself for recalc at all before the fix, old or new.
	if status, body := f.do(t, "PUT", "/api/records/"+recordID, "rollup-test-manager-annual", map[string]any{
		"data": map[string]any{"amount": 25.0}, "status": "rejected",
	}); status != 200 {
		t.Fatalf("reject record: status=%d body=%v", status, body)
	}

	deadline := time.Now().Add(20 * time.Second)
	var factCount int
	for time.Now().Before(deadline) {
		_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.fact_input WHERE source_ref=$1::uuid AND metric_id=$2::uuid`,
			mappingID, targetMetricID).Scan(&factCount)
		if factCount == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if factCount != 0 {
		t.Fatalf("expected the retracted record's fact_input row to be removed, %d remain", factCount)
	}

	if !waitForValue(calcQuery, 0.0, f.modelID, otherRevID, doubleMetricID) {
		var got2 float64
		_ = f.pool.QueryRow(ctx, calcQuery, f.modelID, otherRevID, doubleMetricID).Scan(&got2)
		t.Fatalf("annual_target_double never converged to 0 after the retraction, last seen %v — the retraction branch's recalc never ran (or never settled)", got2)
	}
}

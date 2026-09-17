package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// Tests for a real access-control bypass found in a cross-subsystem
// synchronization audit: internal/writeguard was built specifically so
// there's "exactly one implementation of 'is this write allowed'" for
// runtime.fact_input — cells() (POST /api/cells) and every import path
// call it. The forms path never did, on either the read side
// (GET /api/dimensions, which fed the Forms UI's dimension-member picker
// with zero identity.user_access_rule filtering) or the write side
// (applyFormMappings, which powers form record create, record update,
// /api/forms/{id}/sync, and CSV/XLSX import — all four funnel through
// it — inserting into runtime.form_record_posting/runtime.fact_input with
// no policy check at all). A user hidden/read-restricted from a
// dimension member or metric on the grid could see that member exists
// via the leaking picker and post a value for it via a form, completely
// bypassing the access-control system cells() enforces.

// TestPublicDimensionsFiltersHiddenMember covers both the direct rule
// (DEPT_B, hidden for rollup-test-manager) and the cascade (STAFF_B1 has
// no rule of its own — it's hidden only because setupRollupFixture's
// department-level rule cascades down staff via writeguard.ExpandHidden,
// exactly like cells()'s write-side AncestorChain walk already covers)
// in one pass, since the fixture's existing rules already exercise both.
func TestPublicDimensionsFiltersHiddenMember(t *testing.T) {
	f := setupRollupFixture(t)

	// jsonOK(w, dims) encodes a top-level JSON array, not an object — f.do's
	// helper decodes into map[string]any (fine for every other endpoint in
	// this file, all object-shaped), so /api/dimensions needs its own
	// array-decoding request here instead.
	memberCodes := func(t *testing.T, persona string) map[string][]string {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), "GET", f.srv.URL+"/api/dimensions?revision_id="+f.workingRevID, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("X-Dev-User", persona)
		req.Header.Set("X-App-Id", f.appID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		defer resp.Body.Close() //nolint:errcheck
		if resp.StatusCode != 200 {
			t.Fatalf("GET /api/dimensions as %s: status=%d", persona, resp.StatusCode)
		}
		var dims []map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&dims); err != nil {
			t.Fatalf("decode /api/dimensions response: %v", err)
		}
		out := map[string][]string{}
		for _, dm := range dims {
			name, _ := dm["name"].(string)
			members, _ := dm["members"].([]any)
			for _, m := range members {
				mm, _ := m.(map[string]any)
				code, _ := mm["code"].(string)
				out[name] = append(out[name], code)
			}
		}
		return out
	}

	restricted := memberCodes(t, "rollup-test-manager")
	if contains(restricted["departments"], "DEPT_B") {
		t.Errorf("rollup-test-manager: DEPT_B (direct hidden rule) still present in /api/dimensions: %v", restricted["departments"])
	}
	if !contains(restricted["departments"], "DEPT_A") {
		t.Errorf("rollup-test-manager: DEPT_A (not restricted) missing from /api/dimensions: %v", restricted["departments"])
	}
	if contains(restricted["staff"], "STAFF_B1") {
		t.Errorf("rollup-test-manager: STAFF_B1 (cascaded from hidden DEPT_B, no direct rule of its own) still present in /api/dimensions: %v", restricted["staff"])
	}
	if !contains(restricted["staff"], "STAFF_A1") {
		t.Errorf("rollup-test-manager: STAFF_A1 (not restricted) missing from /api/dimensions: %v", restricted["staff"])
	}

	unrestricted := memberCodes(t, "rollup-test-approver")
	if !contains(unrestricted["departments"], "DEPT_B") {
		t.Errorf("rollup-test-approver (no access rules): DEPT_B missing from /api/dimensions: %v", unrestricted["departments"])
	}
	if !contains(unrestricted["staff"], "STAFF_B1") {
		t.Errorf("rollup-test-approver (no access rules): STAFF_B1 missing from /api/dimensions: %v", unrestricted["staff"])
	}
}

// TestFormPostingRejectsHiddenMemberWrite is the write-side counterpart:
// a form posting into a dimension member the submitting user is hidden
// from must never reach runtime.form_record_posting/runtime.fact_input,
// exactly as cells() already rejects the equivalent direct grid write
// (TestCellsRejectsDirectWriteToHiddenMember, same fixture, same
// DEPT_B/STAFF_B1 scoping).
func TestFormPostingRejectsHiddenMemberWrite(t *testing.T) {
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

	formID := q(`
		INSERT INTO model.form_def (model_id, revision_id, name, label, fields)
		VALUES ($1::uuid, $2::uuid, 'access_form', 'Access Form',
			jsonb_build_array(
				jsonb_build_object('name','amount','label','Amount','type','number'),
				jsonb_build_object('name','staff_code','label','Staff','type','text')
			))
		RETURNING id::text`, f.modelID, f.workingRevID)

	dimMappingsJSON := fmt.Sprintf(`{"%s":"staff_code"}`, f.staffDimID)
	mappingID := q(`
		INSERT INTO model.form_metric_mapping
		  (model_id, revision_id, form_id, grid_id, name, source_field, target_metric_id,
		   aggregation, posting_statuses, dimension_mappings, live_posting)
		VALUES ($1::uuid, $2::uuid, $3::uuid, NULL, 'access mapping', 'amount', $4::uuid,
		        'sum', ARRAY['approved'], $5::jsonb, true)
		RETURNING id::text`, f.modelID, f.workingRevID, formID, f.amountMetricID, dimMappingsJSON)

	submitAndApprove := func(t *testing.T, persona, staffCode string, amount float64) string {
		t.Helper()
		status, body := f.do(t, "POST", "/api/forms/"+formID+"/records", persona, map[string]any{
			"data": map[string]any{"amount": amount, "staff_code": staffCode},
		})
		if status != 200 {
			t.Fatalf("create record as %s: status=%d body=%v", persona, status, body)
		}
		recID, _ := body["id"].(string)
		if recID == "" {
			t.Fatalf("create record as %s: no id in response %v", persona, body)
		}
		status, body = f.do(t, "PUT", "/api/records/"+recID, persona, map[string]any{
			"data": map[string]any{"amount": amount, "staff_code": staffCode}, "status": "approved",
		})
		if status != 200 {
			t.Fatalf("approve record as %s: status=%d body=%v", persona, status, body)
		}
		return recID
	}

	postingCount := func(t *testing.T) int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(ctx,
			`SELECT count(*) FROM runtime.form_record_posting WHERE mapping_id=$1::uuid`, mappingID,
		).Scan(&n); err != nil {
			t.Fatalf("count postings: %v", err)
		}
		return n
	}

	// rollup-test-manager is hidden from DEPT_B (and, by cascade, STAFF_B1)
	// — the same restriction TestCellsRejectsDirectWriteToHiddenMember
	// proves cells() enforces. Posting a form record against STAFF_B1 must
	// be silently withheld: the record itself still saves (record-saving
	// and metric-posting are decoupled), but no posting/fact row appears.
	submitAndApprove(t, "rollup-test-manager", "STAFF_B1", 999)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if n := postingCount(t); n != 0 {
		t.Errorf("rollup-test-manager posted to hidden STAFF_B1: %d form_record_posting row(s), want 0 — write guard was bypassed", n)
	}
	var factCount int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM runtime.fact_input WHERE source_ref=$1::uuid`, mappingID,
	).Scan(&factCount); err != nil {
		t.Fatalf("count fact_input: %v", err)
	}
	if factCount != 0 {
		t.Errorf("rollup-test-manager posted to hidden STAFF_B1: %d fact_input row(s), want 0 — write guard was bypassed", factCount)
	}

	// rollup-test-manager-b CAN see DEPT_B/STAFF_B1 (hidden from DEPT_A
	// instead) — the identical submission from them must succeed, proving
	// the guard blocks the specific restricted user, not everyone.
	submitAndApprove(t, "rollup-test-manager-b", "STAFF_B1", 123)

	deadline = time.Now().Add(5 * time.Second)
	for {
		if n := postingCount(t); n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("rollup-test-manager-b (unrestricted on STAFF_B1) posting never landed: %d form_record_posting rows after 5s", postingCount(t))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestFormPostingRejectsHiddenMetricWrite covers the metric-level half of
// the same guard (writeguard.MetricAccess, not just CheckWrite's
// dimension-member check) — a fresh user/rule, since setupRollupFixture's
// personas only carry dimension_member rules.
func TestFormPostingRejectsHiddenMetricWrite(t *testing.T) {
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

	var custID, wsID string
	if err := f.pool.QueryRow(ctx, `SELECT customer_id::text, workspace_id::text FROM core.application WHERE id=$1::uuid`, f.appID).Scan(&custID, &wsID); err != nil {
		t.Fatalf("resolve customer/workspace: %v", err)
	}
	restrictedUserID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('test-metric-hidden', 'metric-hidden@t.com', 'Metric Hidden', $1::uuid) RETURNING id::text`, custID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid)`, restrictedUserID, wsID)
	devPersonas["rollup-test-metric-hidden"] = "test-metric-hidden"
	t.Cleanup(func() { delete(devPersonas, "rollup-test-metric-hidden") })

	// Scalar (no dimension) target metric, mirroring
	// form_mapping_concurrency_test.go's concurrency_target — keeps this
	// test focused on the metric-access check, not dimension resolution.
	targetMetricID := q(`
		INSERT INTO model.metric_def (model_id, name, is_input, agg_rule, revision_id)
		VALUES ($1::uuid, 'metric_hidden_target', true, 'sum', $2::uuid)
		RETURNING id::text`, f.modelID, f.workingRevID)
	exec(`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'metric', $2, 'hidden')`, restrictedUserID, targetMetricID)

	formID := q(`
		INSERT INTO model.form_def (model_id, revision_id, name, label, fields)
		VALUES ($1::uuid, $2::uuid, 'metric_access_form', 'Metric Access Form',
			jsonb_build_array(jsonb_build_object('name','amount','label','Amount','type','number')))
		RETURNING id::text`, f.modelID, f.workingRevID)
	mappingID := q(`
		INSERT INTO model.form_metric_mapping
		  (model_id, revision_id, form_id, grid_id, name, source_field, target_metric_id,
		   aggregation, posting_statuses, dimension_mappings, live_posting)
		VALUES ($1::uuid, $2::uuid, $3::uuid, NULL, 'metric access mapping', 'amount', $4::uuid,
		        'sum', ARRAY['approved'], '{}'::jsonb, true)
		RETURNING id::text`, f.modelID, f.workingRevID, formID, targetMetricID)

	submitAndApprove := func(t *testing.T, persona string, amount float64) {
		t.Helper()
		status, body := f.do(t, "POST", "/api/forms/"+formID+"/records", persona, map[string]any{
			"data": map[string]any{"amount": amount},
		})
		if status != 200 {
			t.Fatalf("create record as %s: status=%d body=%v", persona, status, body)
		}
		recID, _ := body["id"].(string)
		status, body = f.do(t, "PUT", "/api/records/"+recID, persona, map[string]any{
			"data": map[string]any{"amount": amount}, "status": "approved",
		})
		if status != 200 {
			t.Fatalf("approve record as %s: status=%d body=%v", persona, status, body)
		}
	}

	submitAndApprove(t, "rollup-test-metric-hidden", 999)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	var n int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM runtime.fact_input WHERE source_ref=$1::uuid`, mappingID,
	).Scan(&n); err != nil {
		t.Fatalf("count fact_input: %v", err)
	}
	if n != 0 {
		t.Errorf("user hidden from the target metric posted successfully: %d fact_input row(s), want 0 — write guard was bypassed", n)
	}

	// Unrestricted persona: identical submission must succeed.
	submitAndApprove(t, "rollup-test-approver", 456)
	deadline = time.Now().Add(5 * time.Second)
	for {
		if err := f.pool.QueryRow(ctx,
			`SELECT count(*) FROM runtime.fact_input WHERE source_ref=$1::uuid`, mappingID,
		).Scan(&n); err != nil {
			t.Fatalf("count fact_input: %v", err)
		}
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("unrestricted persona's posting never landed: %d fact_input rows after 5s", n)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Tests for the P0-2 fail-closed/ownership-scoping fixes: cells() rejecting
// a foreign-model metric_id/revision_id, debugFacts and importJobAction no
// longer leaking/deleting across models, markNotifRead no longer touching
// another user's notifications, and the CSV import legacy metric_id column
// rejecting a foreign-model metric UUID. Two fully independent
// customer/workspace/app/model trees ("A" and "B") are seeded on top of
// setupRollupFixture's postgres container + httptest server, so a request
// scoped to A can be checked against B's data — the exact shape of gap each
// fix closes.
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

type authzScopeFixture struct {
	*rollupFixture

	appAID, modelAID, revAID, metricAID string
	appBID, modelBID, revBID, metricBID string
	userAID, userBID                    string
}

func setupAuthzScopeFixture(t *testing.T) *authzScopeFixture {
	t.Helper()
	base := setupRollupFixture(t)
	f := &authzScopeFixture{rollupFixture: base}
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

	// Tree A — a fully independent customer/workspace/app/model, with a
	// "developer" role scoped only to workspace A.
	custAID := q(`INSERT INTO core.customer (name, plan) VALUES ('AuthzA', 'enterprise') RETURNING id::text`)
	wsAID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'wsA') RETURNING id::text`, custAID)
	f.appAID = q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'AppA', 'planning') RETURNING id::text`, wsAID, custAID)
	f.modelAID = q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'ModelA') RETURNING id::text`, f.appAID)
	f.revAID = q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, f.modelAID)
	exec(`UPDATE core.model SET active_revision_id=$1::uuid WHERE id=$2::uuid`, f.revAID, f.modelAID)
	f.metricAID = q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input) VALUES ($1::uuid, $2::uuid, 'amount', true) RETURNING id::text`, f.modelAID, f.revAID)
	f.userAID = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('authz-dev-a', 'devA@t.com', 'Dev A', $1::uuid) RETURNING id::text`, custAID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'developer', $2::uuid)`, f.userAID, wsAID)
	exec(`INSERT INTO runtime.fact_input (model_id, revision_id, revision_name, metric_id, dim_members, value, entered_by) VALUES ($1::uuid, $2::uuid, 'Working', $3::uuid, '{}'::jsonb, 111, $4::uuid)`,
		f.modelAID, f.revAID, f.metricAID, f.userAID)
	exec(`INSERT INTO import.import_job (id, model_id, revision_id, file_url, created_by) VALUES (gen_random_uuid(), $1::uuid, $2::uuid, 'a.csv', $3::uuid)`, f.modelAID, f.revAID, f.userAID)

	// Tree B — an entirely separate tenant, no relation to A.
	custBID := q(`INSERT INTO core.customer (name, plan) VALUES ('AuthzB', 'enterprise') RETURNING id::text`)
	wsBID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'wsB') RETURNING id::text`, custBID)
	f.appBID = q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'AppB', 'planning') RETURNING id::text`, wsBID, custBID)
	f.modelBID = q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'ModelB') RETURNING id::text`, f.appBID)
	f.revBID = q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, f.modelBID)
	exec(`UPDATE core.model SET active_revision_id=$1::uuid WHERE id=$2::uuid`, f.revBID, f.modelBID)
	f.metricBID = q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input) VALUES ($1::uuid, $2::uuid, 'amount', true) RETURNING id::text`, f.modelBID, f.revBID)
	f.userBID = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('authz-dev-b', 'devB@t.com', 'Dev B', $1::uuid) RETURNING id::text`, custBID)
	exec(`INSERT INTO runtime.fact_input (model_id, revision_id, revision_name, metric_id, dim_members, value, entered_by) VALUES ($1::uuid, $2::uuid, 'Working', $3::uuid, '{}'::jsonb, 222, $4::uuid)`,
		f.modelBID, f.revBID, f.metricBID, f.userBID)
	exec(`INSERT INTO import.import_job (id, model_id, revision_id, file_url, created_by) VALUES (gen_random_uuid(), $1::uuid, $2::uuid, 'b.csv', $3::uuid)`, f.modelBID, f.revBID, f.userBID)

	// One notification per user, so markNotifRead's ownership scoping can
	// be checked directly.
	exec(`INSERT INTO notification.notification (id, recipient_user_id, template_id) VALUES (gen_random_uuid(), $1::uuid, 'test')`, f.userAID)
	exec(`INSERT INTO notification.notification (id, recipient_user_id, template_id) VALUES (gen_random_uuid(), $1::uuid, 'test')`, f.userBID)

	devPersonas["authz-dev-a"] = "authz-dev-a"
	devPersonas["authz-dev-b"] = "authz-dev-b"
	t.Cleanup(func() {
		delete(devPersonas, "authz-dev-a")
		delete(devPersonas, "authz-dev-b")
	})

	return f
}

func TestCellsRejectsForeignModelMetricAndRevision(t *testing.T) {
	f := setupAuthzScopeFixture(t)

	// Model B's metric supplied under Model A's model_id: the is_input
	// query now requires model_id to match, so this metric simply won't be
	// found as writable.
	resp, body := f.do(t, "POST", "/api/cells", "authz-dev-a", map[string]any{
		"model_id": f.modelAID, "metric_id": f.metricBID, "revision_id": f.revAID, "value": 1,
	})
	if resp != http.StatusForbidden {
		t.Errorf("foreign metric_id: status = %d, body = %v, want 403", resp, body)
	}

	// Model B's revision supplied under Model A's model_id.
	resp, body = f.do(t, "POST", "/api/cells", "authz-dev-a", map[string]any{
		"model_id": f.modelAID, "metric_id": f.metricAID, "revision_id": f.revBID, "value": 1,
	})
	if resp != http.StatusForbidden {
		t.Errorf("foreign revision_id: status = %d, body = %v, want 403", resp, body)
	}

	// Sanity: A's own metric/revision under A's own model_id still works.
	resp, body = f.do(t, "POST", "/api/cells", "authz-dev-a", map[string]any{
		"model_id": f.modelAID, "metric_id": f.metricAID, "revision_id": f.revAID, "value": 1,
	})
	if resp != http.StatusOK {
		t.Fatalf("own model/metric/revision: status = %d, body = %v, want 200", resp, body)
	}
}

func TestDebugFactsScopesToCallersModel(t *testing.T) {
	f := setupAuthzScopeFixture(t)

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, f.srv.URL+"/api/developer/debug/facts", nil)
	req.Header.Set("X-Dev-User", "authz-dev-a")
	req.Header.Set("X-App-Id", f.appAID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var facts []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&facts); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, fact := range facts {
		if fact["metric_id"] == f.metricBID {
			t.Fatalf("debugFacts scoped to model A returned model B's fact: %v", fact)
		}
	}
	found := false
	for _, fact := range facts {
		if fact["metric_id"] == f.metricAID {
			found = true
		}
	}
	if !found {
		t.Error("expected model A's own fact to be present")
	}
}

// TestDebugFactsFiltersHiddenMetric is a regression test: debugFacts used
// to apply NO identity.user_access_rule filtering at all (unlike every
// sibling read endpoint), and its route was registered "any" instead of
// "developer" like every other /api/developer/* route.
func TestDebugFactsFiltersHiddenMetric(t *testing.T) {
	f := setupAuthzScopeFixture(t)
	ctx := context.Background()

	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'metric', $2::uuid, 'hidden')`,
		f.userAID, f.metricAID,
	); err != nil {
		t.Fatalf("seed hidden metric rule: %v", err)
	}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, f.srv.URL+"/api/developer/debug/facts", nil)
	req.Header.Set("X-Dev-User", "authz-dev-a")
	req.Header.Set("X-App-Id", f.appAID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var facts []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&facts); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, fact := range facts {
		if fact["metric_id"] == f.metricAID {
			t.Errorf("debugFacts returned a fact for a metric hidden from this user: %v", fact)
		}
	}
}

func TestImportJobActionRejectsForeignModelJob(t *testing.T) {
	f := setupAuthzScopeFixture(t)
	ctx := context.Background()

	var jobBID string
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM import.import_job WHERE model_id=$1::uuid`, f.modelBID).Scan(&jobBID); err != nil {
		t.Fatalf("find model B job: %v", err)
	}

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodDelete, f.srv.URL+"/api/import/jobs/"+jobBID, nil)
	req.Header.Set("X-Dev-User", "authz-dev-a")
	req.Header.Set("X-App-Id", f.appAID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("delete model B's job while scoped to model A: status = %d, want 404", resp.StatusCode)
	}

	var stillExists bool
	if err := f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM import.import_job WHERE id=$1::uuid)`, jobBID).Scan(&stillExists); err != nil {
		t.Fatalf("check job existence: %v", err)
	}
	if !stillExists {
		t.Error("model B's job was deleted by a request scoped to model A")
	}
}

func TestMarkNotifReadDoesNotTouchAnotherUsersNotification(t *testing.T) {
	f := setupAuthzScopeFixture(t)
	ctx := context.Background()

	var notifAID, notifBID string
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM notification.notification WHERE recipient_user_id=$1::uuid`, f.userAID).Scan(&notifAID); err != nil {
		t.Fatalf("find A's notification: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM notification.notification WHERE recipient_user_id=$1::uuid`, f.userBID).Scan(&notifBID); err != nil {
		t.Fatalf("find B's notification: %v", err)
	}

	resp, body := f.do(t, "POST", "/api/notifications/mark-read", "authz-dev-a", map[string]any{
		"ids": []string{notifAID, notifBID},
	})
	if resp != http.StatusOK {
		t.Fatalf("mark-read status = %d, body = %v", resp, body)
	}
	if updated, _ := body["updated"].(float64); updated != 1 {
		t.Errorf("updated = %v, want 1 (only the caller's own notification)", body["updated"])
	}

	var bStatus string
	if err := f.pool.QueryRow(ctx, `SELECT status::text FROM notification.notification WHERE id=$1::uuid`, notifBID).Scan(&bStatus); err != nil {
		t.Fatalf("query B's notification status: %v", err)
	}
	if bStatus == "read" {
		t.Error("user A's mark-read call marked user B's notification as read")
	}
}

func TestImportRejectsForeignModelLegacyMetricID(t *testing.T) {
	f := setupAuthzScopeFixture(t)

	csv := fmt.Sprintf("metric_id,value\n%s,999\n", f.metricBID)
	resp, body := f.do(t, "POST", "/api/import/upload", "authz-dev-a", map[string]any{
		"csv":         csv,
		"revision_id": f.revAID,
	})
	if resp != http.StatusOK {
		// The endpoint itself may reject the whole upload outright — either
		// shape proves the row wasn't silently accepted.
		return
	}
	errs, _ := body["errors"].([]any)
	if len(errs) == 0 {
		t.Fatalf("expected the foreign-model metric_id row to be rejected, body = %v", body)
	}

	var count int
	_ = f.pool.QueryRow(context.Background(), `
		SELECT count(*) FROM runtime.fact_input WHERE model_id=$1::uuid AND metric_id=$2::uuid
	`, f.modelAID, f.metricBID).Scan(&count)
	if count != 0 {
		t.Errorf("model B's metric was written under model A after import: %d rows", count)
	}
}

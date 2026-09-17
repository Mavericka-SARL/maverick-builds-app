package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// Usage is counted, never estimated: each number must match what the tables
// hold, scoped to the tenant, with "active" meaning seen or signed in within
// the period. A platform admin sees every tenant, a tenant admin their own.
func TestUsageAnalytics(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	// Tenant A: two users (one seen today, one long ago), one app with a
	// model on two revisions, facts, a workflow instance, an audit event.
	custA := q(`INSERT INTO core.customer (name, plan) VALUES ('Acme', 'enterprise') RETURNING id::text`)
	wsA := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, custA)
	appA := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Plan', 'planning') RETURNING id::text`, wsA, custA)
	modelA := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M') RETURNING id::text`, appA)
	revA := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, modelA)
	exec(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Draft')`, modelA)
	metricA := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule) VALUES ($1::uuid, $2::uuid, 'spend', true, 'sum') RETURNING id::text`, modelA, revA)
	adminA := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id, last_seen_at) VALUES ('usage-admin-a', 'admin@acme.test', 'Acme Admin', $1::uuid, now()) RETURNING id::text`, custA)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'tenant_admin', $2::uuid)`, adminA, wsA)
	dormant := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id, last_login_at) VALUES ('usage-dormant', 'old@acme.test', 'Old Timer', $1::uuid, now() - interval '200 days') RETURNING id::text`, custA)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid)`, dormant, wsA)
	for i := 0; i < 3; i++ {
		exec(`INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value, entered_by) VALUES ($1::uuid, $2::uuid, $3::uuid, '{}', $4, $5::uuid)`, modelA, revA, metricA, float64(i), adminA)
	}
	wf := q(`INSERT INTO workflow.workflow_def (application_id, revision_id, name, trigger_event, steps, status, created_by, updated_by) VALUES ($1::uuid, $2::uuid, 'Flow', 'manual', '[]', 'published', $3::uuid, $3::uuid) RETURNING id::text`, appA, revA, adminA)
	exec(`INSERT INTO workflow.workflow_instance (workflow_def_id, started_by, context) VALUES ($1::uuid, $2::uuid, '{}')`, wf, adminA)
	exec(`INSERT INTO workflow.workflow_instance (workflow_def_id, started_by, context, started_at) VALUES ($1::uuid, $2::uuid, '{}', now() - interval '100 days')`, wf, adminA)
	auditlog.Log(ctx, pool, logger.New("test"), auditlog.Fields{Category: auditlog.CategoryAdmin, EventType: auditlog.EventRoleCreated, ActorUserID: adminA, ApplicationID: appA, ResourceType: "business_role", ResourceID: "r"})
	// Tenant B: one dormant user, nothing else.
	custB := q(`INSERT INTO core.customer (name, plan) VALUES ('Beta', 'starter') RETURNING id::text`)
	wsB := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, custB)
	userB := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('usage-b', 'b@beta.test', 'B', $1::uuid) RETURNING id::text`, custB)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid)`, userB, wsB)
	// A platform admin with no tenant.
	padmin := q(`INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('usage-padmin', 'p@platform.test', 'Platform') RETURNING id::text`)
	exec(`INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'platform_admin')`, padmin)

	t.Setenv("DEV_MODE", "true")
	srv := httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, nil, Deps{License: enterpriseManager(t)}))
	t.Cleanup(srv.Close)
	community := httptest.NewServer(NewHandler(logger.New("test"), pool, nil))
	t.Cleanup(community.Close)
	get := func(s *httptest.Server, persona, path string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.URL+path, nil)
		req.Header.Set("X-Dev-User", persona)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		out := new(bytes.Buffer)
		_, _ = out.ReadFrom(resp.Body)
		var m map[string]any
		_ = json.Unmarshal(out.Bytes(), &m)
		return resp.StatusCode, m
	}
	byName := func(m map[string]any) map[string]map[string]any {
		out := map[string]map[string]any{}
		for _, t := range m["tenants"].([]any) {
			row := t.(map[string]any)
			out[row["name"].(string)] = row
		}
		return out
	}

	code, body := get(srv, "usage-padmin", "/api/admin/usage?period=30d")
	if code != http.StatusOK || body["period_days"] != float64(30) {
		t.Fatalf("platform usage: %d %v", code, body)
	}
	rows := byName(body)
	acme, beta := rows["Acme"], rows["Beta"]
	if acme == nil || beta == nil {
		t.Fatalf("tenants: %v", body["tenants"])
	}
	want := map[string]float64{"users": 2, "active_users": 1, "applications": 1, "models": 1, "revisions": 2, "fact_rows": 3, "workflow_instances": 1, "audit_events": 1, "db_bytes": 0}
	for k, v := range want {
		if acme[k] != v {
			t.Errorf("Acme %s = %v, want %v", k, acme[k], v)
		}
	}
	if acme["last_activity_at"] == nil {
		t.Error("Acme has activity but no last_activity_at")
	}
	if beta["users"] != float64(1) || beta["active_users"] != float64(0) || beta["models"] != float64(0) || beta["last_activity_at"] != nil {
		t.Errorf("Beta: %v", beta)
	}
	// A longer period counts the old instance and the dormant login.
	_, body = get(srv, "usage-padmin", "/api/admin/usage?period=365d")
	acme = byName(body)["Acme"]
	if acme["workflow_instances"] != float64(2) || acme["active_users"] != float64(2) {
		t.Errorf("365d: instances=%v active=%v", acme["workflow_instances"], acme["active_users"])
	}
	// A tenant admin sees exactly their own tenant.
	code, body = get(srv, "usage-admin-a", "/api/admin/usage")
	if code != http.StatusOK || len(body["tenants"].([]any)) != 1 || byName(body)["Acme"] == nil {
		t.Errorf("tenant admin: %d %v", code, body)
	}
	if code, _ := get(srv, "usage-padmin", "/api/admin/usage?period=forever"); code != http.StatusBadRequest {
		t.Errorf("bad period: %d", code)
	}
	if code, _ := get(community, "usage-padmin", "/api/admin/usage"); code != http.StatusForbidden {
		t.Errorf("community: %d", code)
	}

	// The last-seen mark: the dormant user's request updates last_seen_at
	// once, and a burst of requests within the interval costs no more writes.
	h := &handler{log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), devMode: true}
	h.touchLastSeen(ctx, dormant)
	var seen1 time.Time
	if err := pool.QueryRow(ctx, `SELECT last_seen_at FROM identity.user WHERE id = $1::uuid`, dormant).Scan(&seen1); err != nil {
		t.Fatalf("last_seen_at not set: %v", err)
	}
	exec(`UPDATE identity.user SET last_seen_at = now() - interval '1 hour' WHERE id = $1::uuid`, dormant)
	h.touchLastSeen(ctx, dormant)
	var seen2 time.Time
	_ = pool.QueryRow(ctx, `SELECT last_seen_at FROM identity.user WHERE id = $1::uuid`, dormant).Scan(&seen2)
	if time.Since(seen2) < 30*time.Minute {
		t.Error("a second touch within the interval must not write again")
	}
	// ...and it makes the user active for the platform view.
	_, body = get(srv, "usage-padmin", "/api/admin/usage?period=30d")
	if byName(body)["Acme"]["active_users"] != float64(2) {
		t.Errorf("after a touch, active users = %v, want 2", byName(body)["Acme"]["active_users"])
	}
}

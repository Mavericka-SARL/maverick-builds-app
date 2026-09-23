package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// Removing a person never removes what they did for the tenant. Before
// migration 094 a tenant administrator got a 500 for anyone who had ever
// entered a fact — in production, every real user.
func TestUserDeleteKeepsTenantData(t *testing.T) {
	t.Setenv("DEV_MODE", "true")
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	custID := q(`INSERT INTO core.customer (name) VALUES ('Leavers Ltd') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Default') RETURNING id::text`, custID)
	appID := q(`INSERT INTO core.application (customer_id, workspace_id, name, mode) VALUES ($1::uuid, $2::uuid, 'App', 'planning') RETURNING id::text`, custID, wsID)
	modelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M') RETURNING id::text`, appID)
	revID := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, modelID)
	metricID := q(`INSERT INTO model.metric_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Cost') RETURNING id::text`, modelID, revID)
	formID := q(`INSERT INTO model.form_def (model_id, revision_id, name, label) VALUES ($1::uuid, $2::uuid, 'expense', 'Expense') RETURNING id::text`, modelID, revID)
	wdID := q(`INSERT INTO workflow.workflow_def (application_id, revision_id, name, trigger_event) VALUES ($1::uuid, $2::uuid, 'Approve', 'manual') RETURNING id::text`, appID, revID)
	mk := func(sub, role string) string {
		id := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, $1 || '@leavers.test', $1, $2::uuid) RETURNING id::text`, sub, custID)
		q(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, $2, $3::uuid) RETURNING id::text`, id, role, wsID)
		return id
	}
	adminID := mk("leavers-admin", "tenant_admin")
	workerID := mk("leavers-worker", "business_user")
	_ = adminID

	// A working life: facts, a form record, a workflow they started and a
	// step pinned to them, an import, an assistant session.
	q(`INSERT INTO runtime.fact_input (model_id, revision_id, dim_members, metric_id, value, entered_by) VALUES ($1::uuid, $2::uuid, '{}', $3::uuid, 42, $4::uuid) RETURNING id::text`, modelID, revID, metricID, workerID)
	q(`INSERT INTO runtime.form_record (form_id, data, created_by) VALUES ($1::uuid, '{}', $2::uuid) RETURNING id::text`, formID, workerID)
	wiID := q(`INSERT INTO workflow.workflow_instance (workflow_def_id, started_by) VALUES ($1::uuid, $2::uuid) RETURNING id::text`, wdID, workerID)
	q(`INSERT INTO workflow.workflow_step (instance_id, step_def_id, assignee_user_id) VALUES ($1::uuid, 'review', $2::uuid) RETURNING id::text`, wiID, workerID)
	q(`INSERT INTO import.import_job (model_id, revision_id, file_url, created_by) VALUES ($1::uuid, $2::uuid, 'memory://x.csv', $3::uuid) RETURNING id::text`, modelID, revID, workerID)
	q(`INSERT INTO ai_assistant.session (application_id, user_id) VALUES ($1::uuid, $2::uuid) RETURNING id::text`, appID, workerID)

	h := &handler{log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), devMode: true}
	mux := http.NewServeMux()
	h.registerRoutes(mux, nil)
	srv := httptest.NewServer(appIDMiddleware(mux))
	t.Cleanup(srv.Close)
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, srv.URL+"/api/admin/users/"+workerID, nil)
	req.Header.Set("X-Dev-User", "leavers-admin")
	req.Header.Set("X-App-Id", appID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete user: %d", resp.StatusCode)
	}

	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	if n := count(`SELECT count(*) FROM identity.user WHERE id = $1::uuid`, workerID); n != 0 {
		t.Fatal("the user row remains")
	}
	for _, c := range []struct{ what, sql string }{
		{"facts", `SELECT count(*) FROM runtime.fact_input WHERE model_id = $1::uuid AND entered_by IS NULL`},
		{"form records", `SELECT count(*) FROM runtime.form_record r JOIN model.form_def f ON f.id = r.form_id WHERE f.model_id = $1::uuid AND r.created_by IS NULL`},
		{"import jobs", `SELECT count(*) FROM import.import_job WHERE model_id = $1::uuid AND created_by IS NULL`},
	} {
		if n := count(c.sql, modelID); n != 1 {
			t.Fatalf("%s after the author left: %d rows authored by a former user, want 1", c.what, n)
		}
	}
	if n := count(`SELECT count(*) FROM workflow.workflow_instance WHERE id = $1::uuid AND started_by IS NULL`, wiID); n != 1 {
		t.Fatal("the workflow instance did not survive its starter")
	}
	if n := count(`SELECT count(*) FROM workflow.workflow_step WHERE instance_id = $1::uuid AND assignee_user_id IS NULL`, wiID); n != 1 {
		t.Fatal("the pinned step was not released")
	}
	if n := count(`SELECT count(*) FROM ai_assistant.session WHERE application_id = $1::uuid`, appID); n != 0 {
		t.Fatal("the assistant session, which is personal, remains")
	}
	// And the tenant's reads still work with a NULL author.
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/workflow/instances/"+wiID, nil)
	req.Header.Set("X-Dev-User", "leavers-admin")
	req.Header.Set("X-App-Id", appID)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode >= 500 {
		t.Fatalf("instance read after its starter left: %v %d", err, resp.StatusCode)
	} else {
		resp.Body.Close() //nolint:errcheck
	}
}

// Deleting a tenant takes its people with it — identity rows and Keycloak
// accounts, the way deleting them one by one does. Users the tenant does not
// own (no customer link, or a platform administrator) stay.
func TestTenantDeleteRemovesItsUsers(t *testing.T) {
	t.Setenv("DEV_MODE", "true")
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	custID := q(`INSERT INTO core.customer (name) VALUES ('Gone Inc') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Default') RETURNING id::text`, custID)
	broker := newFakeBroker(t)
	mk := func(sub, role string, owned bool) string {
		var cust any
		if owned {
			cust = custID
		}
		id := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, $1 || '@gone.test', $1, $2::uuid) RETURNING id::text`, sub, cust)
		if role == "platform_admin" {
			q(`INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, $2) RETURNING id::text`, id, role)
		} else {
			q(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, $2, $3::uuid) RETURNING id::text`, id, role, wsID)
		}
		broker.users[sub] = &fakeKCUser{Email: sub + "@gone.test", Enabled: true}
		return id
	}
	platformID := mk("gone-platform", "platform_admin", false)
	ownerID := mk("gone-owner", "tenant_admin", true)
	memberID := mk("gone-member", "business_user", true)
	legacyID := mk("gone-legacy", "developer", false)

	h := &handler{log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), devMode: true, kc: broker.client()}
	mux := http.NewServeMux()
	h.registerRoutes(mux, nil)
	srv := httptest.NewServer(appIDMiddleware(mux))
	t.Cleanup(srv.Close)
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, srv.URL+"/api/admin/tenants/"+custID, nil)
	req.Header.Set("X-Dev-User", "gone-platform")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete tenant: %d", resp.StatusCode)
	}

	var remaining []string
	rows, _ := pool.Query(ctx, `SELECT keycloak_sub FROM identity.user ORDER BY 1`)
	for rows.Next() {
		var s string
		_ = rows.Scan(&s)
		remaining = append(remaining, s)
	}
	rows.Close()
	if len(remaining) != 2 || remaining[0] != "gone-legacy" || remaining[1] != "gone-platform" {
		t.Fatalf("users after the tenant is gone: %v, want only the legacy user and the platform admin", remaining)
	}
	_ = platformID
	_ = legacyID
	deleted := map[string]bool{}
	for _, s := range broker.deleted {
		deleted[s] = true
	}
	if !deleted["gone-owner"] || !deleted["gone-member"] || deleted["gone-platform"] || deleted["gone-legacy"] {
		t.Fatalf("Keycloak accounts removed: %v", broker.deleted)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit.audit_event WHERE event_type = 'user.deleted' AND resource_id::text IN ($1, $2)`, ownerID, memberID).Scan(&n)
	if n != 2 {
		t.Fatalf("audit rows for the removed users: %d, want 2", n)
	}
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit.audit_event WHERE event_type = 'tenant.deleted' AND metadata->>'users_removed' = '2'`).Scan(&n)
	if n != 1 {
		t.Fatal("the tenant's audit row does not say how many people left with it")
	}
}

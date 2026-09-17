package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

// Tests for the remaining real access-control gaps found by the
// dashboards/dimensions/metrics/forms/workflows sync audit and the
// functionality/user-roles-model audit, beyond the two most severe ones
// (forms bypassing writeguard; automation rules with zero role/ownership
// check) already covered by forms_access_control_test.go and
// automation_rules_access_control_test.go. Reuses setupWTFixture from
// workflow_trigger_business_test.go (same package).

// baAdmin creates a real business_admin user in f's own workspace/customer
// (not foreign — these tests are about a legitimately-scoped admin abusing
// another workspace's IDs via the URL, not about cross-tenant access, which
// is already covered by foreignDeveloper-style tests elsewhere).
func baAdmin(t *testing.T, f *wtFixture, sub string) string {
	t.Helper()
	ctx := context.Background()
	var userID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, $1||'@wt.com', $1, $2::uuid) RETURNING id::text`,
		sub, f.custID).Scan(&userID); err != nil {
		t.Fatalf("insert business_admin user: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_admin', $2::uuid)`,
		userID, f.wsID); err != nil {
		t.Fatalf("assign business_admin role: %v", err)
	}
	return sub
}

// unassignedBusinessUser creates a business_user in f's workspace with NO
// identity.business_role_member row anywhere — the specific edge case that
// distinguishes the correct "workspace has no business roles at all"
// fallback (businessDashboards) from the weaker "caller has no
// business_role_member rows anywhere" fallback (the old dashboardWidgetAction
// bug): f's workspace DOES define business roles (Owners/Approvers/Viewers),
// so a correctly-scoped caller with no membership must be denied.
func unassignedBusinessUser(t *testing.T, f *wtFixture, sub string) {
	t.Helper()
	ctx := context.Background()
	var userID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, $1||'@wt.com', $1, $2::uuid) RETURNING id::text`,
		sub, f.custID).Scan(&userID); err != nil {
		t.Fatalf("insert unassigned user: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid)`,
		userID, f.wsID); err != nil {
		t.Fatalf("assign business_user role: %v", err)
	}
}

// ── A1: dashboardWidgetAction fallback consistency ──────────────────────────

func TestDashboardChartDataMatchesDashboardListFallback(t *testing.T) {
	f := setupWTFixture(t)
	ctx := context.Background()

	// A chart widget on f.dashID, which is granted only to "Budget Owners".
	// widget_props is minimal — the access check runs BEFORE chart-config
	// validation, so a 403 (access denied) is cleanly distinguishable from
	// a 400 (access wrongly granted, then rejected for an empty config).
	var widgetID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, widget_props)
		VALUES ($1::uuid, 'chart', $1, '{"chart":{}}'::jsonb) RETURNING id::text
	`, f.dashID).Scan(&widgetID); err != nil {
		t.Fatalf("insert chart widget: %v", err)
	}

	unassignedSub := "wt-chart-unassigned"
	unassignedBusinessUser(t, f, unassignedSub)

	// Owner-decided semantics change (2026-08-30): a user in NO business
	// role is allowed by default — access must be GRANTED here, so the
	// request proceeds to config validation (400 for the bare config).
	// A 403 would mean the old members-only rule regressed back in.
	status, body := f.do(t, "POST", "/api/dashboard-widgets/"+widgetID+"/chart-data", unassignedSub, map[string]any{})
	if status == http.StatusForbidden {
		t.Errorf("unassigned business_user wrongly denied chart-data: status=%d, body=%s (unassigned users see dashboards by default)", status, body)
	}

	// Sanity check: the dashboard's real owner must still get past the
	// access check (a 400 here is fine — empty chart config — a 403 is not).
	status, body = f.do(t, "POST", "/api/dashboard-widgets/"+widgetID+"/chart-data", f.ownerSub, map[string]any{})
	if status == http.StatusForbidden {
		t.Errorf("dashboard owner wrongly denied chart-data: status=%d, body=%s", status, body)
	}
}

// ── A2: businessFolders filtering ───────────────────────────────────────────

func TestBusinessFoldersFiltersByDashboardAccess(t *testing.T) {
	f := setupWTFixture(t)
	ctx := context.Background()

	var restrictedFolderID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO model.dashboard_folder (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Budget Folder') RETURNING id::text`,
		f.modelID, f.revID).Scan(&restrictedFolderID); err != nil {
		t.Fatalf("insert restricted folder: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`UPDATE model.dashboard_def SET folder_id=$1::uuid WHERE id=$2::uuid`, restrictedFolderID, f.dashID,
	); err != nil {
		t.Fatalf("assign dashboard to folder: %v", err)
	}

	var emptyFolderID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO model.dashboard_folder (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Empty Folder') RETURNING id::text`,
		f.modelID, f.revID).Scan(&emptyFolderID); err != nil {
		t.Fatalf("insert empty folder: %v", err)
	}

	unassignedSub := "wt-folder-unassigned"
	unassignedBusinessUser(t, f, unassignedSub)

	status, body := f.do(t, "GET", "/api/folders", unassignedSub, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/folders: status=%d, body=%s", status, body)
	}
	var folders []dashboardFolderItem
	if err := json.Unmarshal(body, &folders); err != nil {
		t.Fatalf("decode folders: %v, body=%s", err, body)
	}
	ids := map[string]bool{}
	for _, f := range folders {
		ids[f.ID] = true
	}
	// Owner-decided semantics change (2026-08-30): a user who is a member
	// of NO business role sees everything by default — role membership is
	// what opts a user into restriction. The unassigned user therefore
	// sees the restricted folder too now.
	if !ids[restrictedFolderID] {
		t.Errorf("unassigned business_user did not see folder %q — unassigned users see all dashboards by default", restrictedFolderID)
	}
	if !ids[emptyFolderID] {
		t.Errorf("unassigned business_user did not see empty folder %q, want visible (no dashboards to restrict on)", emptyFolderID)
	}

	status, body = f.do(t, "GET", "/api/folders", f.ownerSub, nil)
	if status != http.StatusOK {
		t.Fatalf("GET /api/folders (owner): status=%d, body=%s", status, body)
	}
	folders = nil
	if err := json.Unmarshal(body, &folders); err != nil {
		t.Fatalf("decode folders (owner): %v, body=%s", err, body)
	}
	ids = map[string]bool{}
	for _, f := range folders {
		ids[f.ID] = true
	}
	if !ids[restrictedFolderID] {
		t.Errorf("dashboard owner did not see restricted folder %q, want visible", restrictedFolderID)
	}
}

// ── A3 / A3b: business-admin workspace scoping ──────────────────────────────

func TestBusinessAdminRoleActionRejectsForeignWorkspace(t *testing.T) {
	f := setupWTFixture(t)
	ctx := context.Background()

	foreignWsID := ""
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'foreign-ws') RETURNING id::text`,
		f.custID).Scan(&foreignWsID); err != nil {
		t.Fatalf("insert foreign workspace: %v", err)
	}
	var foreignRoleID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Foreign Role') RETURNING id::text`,
		foreignWsID).Scan(&foreignRoleID); err != nil {
		t.Fatalf("insert foreign business role: %v", err)
	}

	admin := baAdmin(t, f, "wt-badmin-role")

	if status, body := f.do(t, "PATCH", "/api/business-admin/roles/"+foreignRoleID, admin, map[string]any{"name": "Hijacked"}); status != http.StatusForbidden {
		t.Errorf("PATCH foreign role: status=%d, want 403, body=%s", status, body)
	}
	if status, body := f.do(t, "PUT", "/api/business-admin/roles/"+foreignRoleID+"/dashboards", admin, map[string]any{"dashboard_ids": []string{f.dashID}}); status != http.StatusForbidden {
		t.Errorf("PUT foreign role dashboards: status=%d, want 403, body=%s", status, body)
	}
	if status, body := f.do(t, "DELETE", "/api/business-admin/roles/"+foreignRoleID, admin, nil); status != http.StatusForbidden {
		t.Errorf("DELETE foreign role: status=%d, want 403, body=%s", status, body)
	}

	var name string
	if err := f.pool.QueryRow(ctx, `SELECT name FROM identity.business_role WHERE id=$1::uuid`, foreignRoleID).Scan(&name); err != nil {
		t.Fatalf("foreign role missing after rejected DELETE: %v", err)
	}
	if name != "Foreign Role" {
		t.Errorf("foreign role name = %q, want unchanged", name)
	}
}

func TestBusinessAdminUserActionRejectsForeignWorkspace(t *testing.T) {
	f := setupWTFixture(t)
	ctx := context.Background()

	foreignWsID := ""
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'foreign-ws-2') RETURNING id::text`,
		f.custID).Scan(&foreignWsID); err != nil {
		t.Fatalf("insert foreign workspace: %v", err)
	}
	var foreignUserID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('wt-foreign-target', 'ft@wt.com', 'Foreign Target', $1::uuid) RETURNING id::text`,
		f.custID).Scan(&foreignUserID); err != nil {
		t.Fatalf("insert foreign target user: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid)`,
		foreignUserID, foreignWsID); err != nil {
		t.Fatalf("assign foreign target role: %v", err)
	}

	admin := baAdmin(t, f, "wt-badmin-user")

	if status, body := f.do(t, "GET", "/api/business-admin/users/"+foreignUserID+"/access-rules", admin, nil); status != http.StatusForbidden {
		t.Errorf("GET foreign user access-rules: status=%d, want 403, body=%s", status, body)
	}
	if status, body := f.do(t, "PUT", "/api/business-admin/users/"+foreignUserID+"/access-rules", admin,
		map[string]any{"rules": []map[string]any{{"rule_type": "dimension_member", "ref_id": "x", "access": "hide"}}},
	); status != http.StatusForbidden {
		t.Errorf("PUT foreign user access-rules: status=%d, want 403, body=%s", status, body)
	}

	var n int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM identity.user_access_rule WHERE user_id=$1::uuid`, foreignUserID).Scan(&n); err != nil {
		t.Fatalf("count access rules: %v", err)
	}
	if n != 0 {
		t.Errorf("foreign user gained %d access rule(s) from rejected PUT, want 0", n)
	}
}

// ── A4: form-schema CRUD role gate ──────────────────────────────────────────

func TestFormSchemaCRUDRequiresDeveloperRole(t *testing.T) {
	f := setupWTFixture(t)
	ctx := context.Background()

	create := map[string]any{"name": "New Form", "label": "New Form"}
	if status, body := f.do(t, "POST", "/api/forms", f.bystanderSub, create); status != http.StatusForbidden {
		t.Errorf("business_user POST /api/forms: status=%d, want 403, body=%s", status, body)
	}

	devSub := "wt-form-dev"
	var devID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'formdev@wt.com', 'Form Dev', $2::uuid) RETURNING id::text`,
		devSub, f.custID).Scan(&devID); err != nil {
		t.Fatalf("insert developer user: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'developer', $2::uuid)`,
		devID, f.wsID); err != nil {
		t.Fatalf("assign developer role: %v", err)
	}

	status, body := f.do(t, "POST", "/api/forms", devSub, create)
	if status != http.StatusOK {
		t.Fatalf("developer POST /api/forms: status=%d, want 200, body=%s", status, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.ID == "" {
		t.Fatalf("decode created form: err=%v, body=%s", err, body)
	}

	if status, body := f.do(t, "PATCH", "/api/forms/"+created.ID, f.bystanderSub, map[string]any{"name": "Hijacked", "label": "Hijacked"}); status != http.StatusForbidden {
		t.Errorf("business_user PATCH /api/forms/{id}: status=%d, want 403, body=%s", status, body)
	}
	if status, body := f.do(t, "DELETE", "/api/forms/"+created.ID, f.bystanderSub, nil); status != http.StatusForbidden {
		t.Errorf("business_user DELETE /api/forms/{id}: status=%d, want 403, body=%s", status, body)
	}
}

// ── A5: AI session IDOR ─────────────────────────────────────────────────────

func TestAISessionOwnershipEnforced(t *testing.T) {
	f := setupWTFixture(t)
	ctx := context.Background()

	makeDev := func(sub, email string) {
		var id string
		if err := f.pool.QueryRow(ctx,
			`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, $2, $1, $3::uuid) RETURNING id::text`,
			sub, email, f.custID).Scan(&id); err != nil {
			t.Fatalf("insert developer user %s: %v", sub, err)
		}
		if _, err := f.pool.Exec(ctx,
			`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'developer', $2::uuid)`,
			id, f.wsID); err != nil {
			t.Fatalf("assign developer role %s: %v", sub, err)
		}
	}
	makeDev("wt-ai-owner", "aiowner@wt.com")
	makeDev("wt-ai-other", "aiother@wt.com")

	status, body := f.do(t, "POST", "/api/ai/sessions", "wt-ai-owner", nil)
	if status != http.StatusOK {
		t.Fatalf("create session: status=%d, body=%s", status, body)
	}
	var sess struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &sess); err != nil || sess.ID == "" {
		t.Fatalf("decode session: err=%v, body=%s", err, body)
	}

	if status, body := f.do(t, "GET", "/api/ai/sessions/"+sess.ID, "wt-ai-other", nil); status != http.StatusNotFound {
		t.Errorf("foreign developer GET session: status=%d, want 404, body=%s", status, body)
	}
	if status, body := f.do(t, "POST", "/api/ai/sessions/"+sess.ID+"/messages", "wt-ai-other", map[string]any{"content": "hijacked"}); status != http.StatusNotFound {
		t.Errorf("foreign developer POST message: status=%d, want 404, body=%s", status, body)
	}

	// Owner can still read their own session.
	if status, body := f.do(t, "GET", "/api/ai/sessions/"+sess.ID, "wt-ai-owner", nil); status != http.StatusOK {
		t.Errorf("owner GET own session: status=%d, want 200, body=%s", status, body)
	}

	var n int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM ai_assistant.message WHERE session_id=$1::uuid`, sess.ID).Scan(&n); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	if n != 0 {
		t.Errorf("foreign developer's rejected message still landed, count=%d, want 0", n)
	}
}

// ── A6: integrationRun auth ─────────────────────────────────────────────────

func TestIntegrationRunRejectsUnauthenticatedAndForeignApp(t *testing.T) {
	f := setupWTFixture(t)
	ctx := context.Background()

	var formID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO model.form_def (model_id, name, label, fields, revision_id) VALUES ($1::uuid, 'Import Target', 'Import Target', '[]'::jsonb, $2::uuid) RETURNING id::text`,
		f.modelID, f.revID).Scan(&formID); err != nil {
		t.Fatalf("insert form: %v", err)
	}
	var intID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO model.integration_def (model_id, name, type, target_type, target_id, config, revision_id)
		 VALUES ($1::uuid, 'CSV Import', 'csv_import', 'form', $2::uuid, '{}'::jsonb, $3::uuid) RETURNING id::text`,
		f.modelID, formID, f.revID).Scan(&intID); err != nil {
		t.Fatalf("insert integration: %v", err)
	}

	// A persona that doesn't resolve to any real user — resolveActor
	// should fail closed (401), not silently fall back to a zero-UUID
	// actor and still run the import.
	status, body := f.do(t, "POST", "/api/integrations/"+intID+"/run", "wt-nonexistent-user", map[string]any{"csv": "col\nval"})
	if status != http.StatusUnauthorized {
		t.Errorf("unauthenticated integration run: status=%d, want 401, body=%s", status, body)
	}

	foreignSub := foreignDeveloper(t, f)
	status, body = f.do(t, "POST", "/api/integrations/"+intID+"/run", foreignSub, map[string]any{"csv": "col\nval"})
	if status != http.StatusForbidden && status != http.StatusNotFound {
		t.Errorf("foreign developer integration run: status=%d, want 403 or 404, body=%s", status, body)
	}

	var n int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.form_record WHERE form_id=$1::uuid`, formID).Scan(&n); err != nil {
		t.Fatalf("count form records: %v", err)
	}
	if n != 0 {
		t.Errorf("rejected integration runs still created %d form record(s), want 0", n)
	}
}

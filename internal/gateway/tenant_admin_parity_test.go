package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// The role model: a tenant administrator has what a platform administrator
// has, for their own tenant only; the platform administrator has it for
// every tenant, plus the platform's own screens (plans, licence,
// infrastructure, creating and deleting tenants). This drives every admin
// route as both, against the tenant admin's own tenant and a foreign one,
// and reports the actual verdicts — the parity audit of 2026-09-20.
func TestTenantAdminParity(t *testing.T) {
	f := setupRollupFixture(t)
	ft := seedForeignTenant(t, f)
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	custA := q(`SELECT customer_id::text FROM core.application WHERE id=$1::uuid`, f.appID)
	// The console-created shape: the tenant is the user's own, the role unscoped.
	taID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('par-ta', 'ta@a.test', 'TA', $1::uuid) RETURNING id::text`, custA)
	exec(`INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'tenant_admin')`, taID)
	paID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('par-pa', 'pa@platform.test', 'PA') RETURNING id::text`)
	exec(`INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'platform_admin')`, paID)
	ubID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('par-ub', 'ub@b.test', 'UB', $1::uuid) RETURNING id::text`, ft.custID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid)`, ubID, ft.wsID)
	t.Setenv("DEV_MODE", "true")

	// Enterprise licence, so the feature-gated tabs answer on their merits.
	srv := httptest.NewServer(NewHandlerWithDeps(logger.New("test"), f.pool, nil, Deps{License: enterpriseManager(t)}))
	t.Cleanup(srv.Close)
	const ta, pa = "par-ta", "par-pa"

	call := func(persona, method, path string, body any, headers ...string) (int, any) {
		t.Helper()
		var buf bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&buf).Encode(body)
		}
		req, _ := http.NewRequestWithContext(ctx, method, srv.URL+path, &buf)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Dev-User", persona)
		for i := 0; i+1 < len(headers); i += 2 {
			req.Header.Set(headers[i], headers[i+1])
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		var out any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	reachable := func(code int) bool {
		return code != http.StatusForbidden && code != http.StatusUnauthorized && code != http.StatusNotFound
	}
	refused := func(code int) bool { return code == http.StatusForbidden || code == http.StatusNotFound }

	t.Run("listings are scoped: the tenant admin sees only their tenant, the platform admin all", func(t *testing.T) {
		code, out := call(ta, "GET", "/api/admin/tenants", nil)
		ids := idsOf(out)
		if code != 200 || !ids[custA] || ids[ft.custID] {
			t.Fatalf("tenant admin tenants: %d %v", code, ids)
		}
		code, out = call(pa, "GET", "/api/admin/tenants", nil)
		ids = idsOf(out)
		if code != 200 || !ids[custA] || !ids[ft.custID] {
			t.Fatalf("platform admin tenants: %d %v", code, ids)
		}
		code, out = call(ta, "GET", "/api/admin/applications", nil)
		ids = idsOf(out)
		if code != 200 || !ids[f.appID] || ids[ft.appID] {
			t.Fatalf("tenant admin applications: %d %v", code, ids)
		}
		code, out = call(ta, "GET", "/api/admin/users", nil)
		ids = idsOf(out)
		if code != 200 || !ids[taID] || ids[ubID] {
			t.Fatalf("tenant admin users: %d own=%v foreign=%v", code, ids[taID], ids[ubID])
		}
		code, out = call(pa, "GET", "/api/admin/users", nil)
		ids = idsOf(out)
		if code != 200 || !ids[taID] || !ids[ubID] {
			t.Fatalf("platform admin users: %d", code)
		}
	})

	t.Run("mutations on the own tenant work for both; on a foreign tenant only for the platform admin", func(t *testing.T) {
		type row struct {
			name    string
			method  string
			own     string
			foreign string
			body    func(cust string) any
		}
		rows := []row{
			{"rename tenant", "PATCH", "/api/admin/tenants/" + custA, "/api/admin/tenants/" + ft.custID, func(string) any { return map[string]any{"name": "Renamed"} }},
			{"create application", "POST", "/api/admin/applications", "/api/admin/applications", func(c string) any { return map[string]any{"customer_id": c, "name": "New app", "mode": "planning"} }},
			{"rename application", "PATCH", "/api/admin/applications/" + f.appID, "/api/admin/applications/" + ft.appID, func(string) any { return map[string]any{"name": "Renamed app"} }},
			{"create model", "POST", "/api/admin/models", "/api/admin/models", func(c string) any {
				if c == custA {
					return map[string]any{"application_id": f.appID, "name": "New model"}
				}
				return map[string]any{"application_id": ft.appID, "name": "New model"}
			}},
			{"create revision", "POST", "/api/admin/revisions", "/api/admin/revisions", func(c string) any {
				if c == custA {
					return map[string]any{"model_id": f.modelID, "name": "Draft"}
				}
				return map[string]any{"model_id": ft.modelID, "name": "Draft"}
			}},
		}
		for _, r := range rows {
			if code, out := call(ta, r.method, r.own, r.body(custA), tenantHeader, custA); !reachable(code) {
				t.Errorf("%s: tenant admin on own tenant refused: %d %v", r.name, code, out)
			}
			if code, out := call(ta, r.method, r.foreign, r.body(ft.custID), tenantHeader, ft.custID); !refused(code) {
				t.Errorf("%s: tenant admin on FOREIGN tenant not refused: %d %v", r.name, code, out)
			}
			if code, out := call(pa, r.method, r.foreign, r.body(ft.custID), tenantHeader, ft.custID); !reachable(code) {
				t.Errorf("%s: platform admin refused: %d %v", r.name, code, out)
			}
		}
	})

	t.Run("per-tenant settings tabs answer the tenant admin", func(t *testing.T) {
		for _, p := range []string{
			"/api/admin/audit", "/api/admin/usage", "/api/notifications/settings", "/api/admin/audit/settings",
			"/api/admin/branding", "/api/admin/sso", "/api/admin/scim/tokens", "/api/admin/ai-settings",
			"/api/admin/plans", "/api/admin/workspaces", "/api/admin/me",
		} {
			if code, out := call(ta, "GET", p, nil); !reachable(code) {
				t.Errorf("GET %s: tenant admin refused: %d %v", p, code, out)
			}
			if code, out := call(pa, "GET", p, nil, tenantHeader, custA); !reachable(code) {
				t.Errorf("GET %s: platform admin refused: %d %v", p, code, out)
			}
		}
		if code, _ := call(ta, "POST", "/api/notifications/settings/test", nil); code != http.StatusConflict {
			t.Errorf("test mail without a relay: %d", code) // reachable; refused only for want of a relay
		}
	})

	t.Run("the platform's own screens stay with the platform admin", func(t *testing.T) {
		if code, _ := call(ta, "PUT", "/api/admin/plans/starter", map[string]any{"name": "Starter"}); code != 403 {
			t.Errorf("tenant admin editing the plan catalog: %d", code)
		}
		if code, _ := call(ta, "PATCH", "/api/admin/tenants/"+custA, map[string]any{"plan": "enterprise"}); code != 403 {
			t.Errorf("tenant admin changing own plan: %d", code)
		}
		if code, _ := call(ta, "POST", "/api/admin/tenants", map[string]any{"name": "Mine too", "plan": "starter"}); code != 403 {
			t.Errorf("tenant admin creating a tenant: %d", code)
		}
		if code, _ := call(ta, "DELETE", "/api/admin/tenants/"+custA, nil); code != 403 {
			t.Errorf("tenant admin deleting own tenant: %d", code)
		}
		if code, _ := call(ta, "GET", "/api/admin/infra/nodes", nil); code != 403 {
			t.Errorf("tenant admin reading cluster node stats: %d (platform data)", code)
		}
	})

	t.Run("model export: own tenant for the tenant admin, every tenant for the platform admin", func(t *testing.T) {
		// tenantAdm() admits both since 2026-09-20 — the platform admin is
		// nowhere narrower than a tenant admin — and never a developer.
		if code, out := call(ta, "GET", "/api/admin/models/"+f.modelID+"/export", nil); !reachable(code) {
			t.Errorf("tenant admin exporting own model: %d %v", code, out)
		}
		if code, _ := call(ta, "GET", "/api/admin/models/"+ft.modelID+"/export", nil); !refused(code) {
			t.Errorf("tenant admin exporting a FOREIGN model: %d", code)
		}
		for _, m := range []string{f.modelID, ft.modelID} {
			if code, out := call(pa, "GET", "/api/admin/models/"+m+"/export", nil); !reachable(code) {
				t.Errorf("platform admin exporting %s: %d %v", m, code, out)
			}
		}
		if code, _ := call("rollup-test-approver", "GET", "/api/admin/models/"+f.modelID+"/export", nil); code != http.StatusForbidden {
			t.Errorf("developer exporting: %d", code)
		}
	})
}

// idsOf collects the "id" of every object in a JSON list.
func idsOf(v any) map[string]bool {
	out := map[string]bool{}
	list, _ := v.([]any)
	for _, it := range list {
		if m, ok := it.(map[string]any); ok {
			if id, ok := m["id"].(string); ok {
				out[id] = true
			}
		}
	}
	return out
}

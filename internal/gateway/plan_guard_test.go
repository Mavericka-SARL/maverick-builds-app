package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/plan"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// What a plan does to requests: a tenant the sweep found over its limits is
// read-only (its own admin included, the platform admin excluded) until the
// platform admin moves it; a limit refuses the one creation that would cross it;
// the sweep's verdict makes a tenant read-only except for deleting; and the
// catalog is the platform admin's to edit, everyone else's to read.
func TestPlanGuardAndLimits(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	t.Setenv("DEV_MODE", "true")
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	// Tenant A: the last sweep found it over its plan. Tenant B: starter, unlimited.
	custA := q(`INSERT INTO core.customer (name, plan, limit_state, limit_reason) VALUES ('Over Co', 'test', 'over', 'storage 120 MB of 100') RETURNING id::text`)
	wsA := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Default') RETURNING id::text`, custA)
	adminA := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('guard-a', 'a@ended.test', 'A', $1::uuid) RETURNING id::text`, custA)
	exec(`INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'tenant_admin'), ($1::uuid, 'developer')`, adminA)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_admin', $2::uuid)`, adminA, wsA)
	custB := q(`INSERT INTO core.customer (name, plan) VALUES ('Beta Co', 'starter') RETURNING id::text`)
	wsB := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Default') RETURNING id::text`, custB)
	adminB := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('guard-b', 'b@beta.test', 'B', $1::uuid) RETURNING id::text`, custB)
	exec(`INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'tenant_admin'), ($1::uuid, 'developer')`, adminB)
	appB := q(`INSERT INTO core.application (customer_id, workspace_id, name, mode) VALUES ($1::uuid, $2::uuid, 'B App', 'planning') RETURNING id::text`, custB, wsB)
	padmin := q(`INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('guard-padmin', 'p@platform.test', 'Platform') RETURNING id::text`)
	exec(`INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'platform_admin')`, padmin)

	// The enforcer the sweep uses is the gateway's own, as in cmd/gateway:
	// its verdict must not wait for a cache to expire.
	enforcer := plan.NewEnforcer(pool)
	srv := httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, nil, Deps{Plans: enforcer, Signup: SignupConfig{ContactURL: "https://example.test/pricing"}}))
	t.Cleanup(srv.Close)

	t.Run("over its limits: reads work, writes are refused with the reason, platform admin is exempt", func(t *testing.T) {
		if code, _ := callJSONList(t, srv, "guard-a", "/api/admin/tenants"); code != 200 {
			t.Fatalf("read: %d", code)
		}
		code, me := callJSON(t, srv, "guard-a", http.MethodGet, "/api/me", nil)
		st, _ := me["plan"].(map[string]any)
		if code != 200 || st["read_only"] != true || st["code"] != plan.CodeOverLimit {
			t.Fatalf("me: %d %v", code, me)
		}
		if _, has := st["trial"]; has {
			t.Fatalf("plan state still speaks of a trial: %v", st)
		}
		code, body := callJSON(t, srv, "guard-a", http.MethodPost, "/api/admin/applications", map[string]any{"customer_id": custA, "name": "Nope", "mode": "planning"})
		if code != http.StatusPaymentRequired || body["code"] != plan.CodeOverLimit || body["contact_url"] != "https://example.test/pricing" || !strings.Contains(body["error"].(string), "storage 120 MB of 100") {
			t.Fatalf("write while over: %d %v", code, body)
		}
		if code, body := callJSON(t, srv, "guard-padmin", http.MethodPost, "/api/admin/applications", map[string]any{"customer_id": custA, "name": "By platform", "mode": "planning"}, tenantHeader, custA); code != 200 {
			t.Fatalf("platform admin write: %d %v", code, body)
		}
	})

	t.Run("only the platform admin changes a plan; the next sweep judges the tenant by it", func(t *testing.T) {
		if code, _ := callJSON(t, srv, "guard-a", http.MethodPatch, "/api/admin/tenants/"+custA, map[string]any{"plan": "standard"}); code != 402 {
			// The guard refuses before the handler's own 403: the tenant is read-only.
			t.Fatalf("tenant admin lifting own limits: %d", code)
		}
		if code, body := callJSON(t, srv, "guard-padmin", http.MethodPatch, "/api/admin/tenants/"+custA, map[string]any{"plan": "standard"}, tenantHeader, custA); code != 200 {
			t.Fatalf("plan change: %d %v", code, body)
		}
		_, me := callJSON(t, srv, "guard-a", http.MethodGet, "/api/me", nil)
		st, _ := me["plan"].(map[string]any)
		if st["plan"].(map[string]any)["key"] != "standard" {
			t.Fatalf("me after plan change: %v", st)
		}
		// The unlimited plan has nothing to be over: the sweep clears the
		// verdict, and the tenant's admin can write again.
		if tt, err := enforcer.Sweep(ctx, pool, custA); err != nil || tt.LimitState != "ok" {
			t.Fatalf("sweep after plan change: %+v %v", tt, err)
		}
		if code, body := callJSON(t, srv, "guard-a", http.MethodPost, "/api/admin/applications", map[string]any{"customer_id": custA, "name": "Now fine", "mode": "planning"}); code != 200 {
			t.Fatalf("write after plan change: %d %v", code, body)
		}
		if code, body := callJSON(t, srv, "guard-padmin", http.MethodPatch, "/api/admin/tenants/"+custA, map[string]any{"plan": "no-such-plan"}, tenantHeader, custA); code != 400 || !strings.Contains(body["error"].(string), "unknown plan") {
			t.Fatalf("unknown plan: %d %v", code, body)
		}
	})

	t.Run("the catalog: read by admins, written by the platform admin, validated", func(t *testing.T) {
		code, plans := callJSONList(t, srv, "guard-b", "/api/admin/plans")
		if code != 200 || len(plans) < 4 || plans[0]["key"] != "test" {
			t.Fatalf("list: %d %v", code, plans)
		}
		if code, _ := callJSON(t, srv, "guard-b", http.MethodPut, "/api/admin/plans/starter", map[string]any{"name": "Starter"}); code != 403 {
			t.Fatalf("tenant admin editing a plan: %d", code)
		}
		if code, body := callJSON(t, srv, "guard-padmin", http.MethodPut, "/api/admin/plans/Bad%20Key", map[string]any{"name": "x"}); code != 400 || !strings.Contains(body["error"].(string), "plan key") {
			t.Fatalf("bad key: %d %v", code, body)
		}
		code, saved := callJSON(t, srv, "guard-padmin", http.MethodPut, "/api/admin/plans/starter",
			map[string]any{"name": "Starter", "description": "One model", "limits": map[string]int{"max_models": 1}, "sort_order": 20})
		if code != 200 || saved["key"] != "starter" || saved["limits"].(map[string]any)["max_models"] != float64(1) {
			t.Fatalf("save: %d %v", code, saved)
		}
	})

	t.Run("a limit refuses the creation that would cross it, naming plan and numbers", func(t *testing.T) {
		if code, body := callJSON(t, srv, "guard-b", http.MethodPost, "/api/admin/models", map[string]any{"application_id": appB, "name": "First"}); code != 200 {
			t.Fatalf("first model: %d %v", code, body)
		}
		code, body := callJSON(t, srv, "guard-b", http.MethodPost, "/api/admin/models", map[string]any{"application_id": appB, "name": "Second"})
		if code != http.StatusPaymentRequired || body["code"] != "plan_limit" || body["limit"] != "max_models" || body["max"] != float64(1) || body["current"] != float64(1) ||
			!strings.Contains(body["error"].(string), "The Starter plan allows 1 model; this tenant has 1") {
			t.Fatalf("second model: %d %v", code, body)
		}
	})

	t.Run("the sweep makes an over-limit tenant read-only except for deleting", func(t *testing.T) {
		extra := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Slipped in') RETURNING id::text`, appB)
		if tt, err := enforcer.Sweep(ctx, pool, custB); err != nil || tt.LimitState != "over" {
			t.Fatalf("sweep: %+v %v", tt, err)
		}
		code, body := callJSON(t, srv, "guard-b", http.MethodPost, "/api/admin/applications", map[string]any{"customer_id": custB, "name": "More", "mode": "planning"})
		if code != http.StatusPaymentRequired || body["code"] != plan.CodeOverLimit || !strings.Contains(body["error"].(string), "this tenant has 2") {
			t.Fatalf("write while over: %d %v", code, body)
		}
		if code, body := callJSON(t, srv, "guard-b", http.MethodDelete, "/api/admin/models/"+extra, nil); code != 200 {
			t.Fatalf("delete while over: %d %v", code, body)
		}
	})
}

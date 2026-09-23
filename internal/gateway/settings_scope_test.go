package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/ee/aikeys"
	"github.com/mavericks-engine/mavericks/ee/auditexport"
	"github.com/mavericks-engine/mavericks/internal/notification"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/license"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// Per-tenant settings in ONE shared database (migration 091): tenant A's
// webhook never receives tenant B's notifications, B's audit events outlive
// A's retention, and the deployment's own row — an enterprise capability —
// is what a tenant inherits until it sets its own. Before 091 every one of
// these was a single row for the whole database.
func TestSettingsAreScopedPerTenant(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	t.Setenv("DEV_MODE", "true")
	t.Setenv("AI_KEY_ENCRYPTION_SECRET", "scope-test-secret")
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	custA := q(`INSERT INTO core.customer (name, plan) VALUES ('A Co', 'enterprise') RETURNING id::text`)
	custB := q(`INSERT INTO core.customer (name, plan) VALUES ('B Co', 'enterprise') RETURNING id::text`)
	mk := func(sub, role, cust string) string {
		id := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, $1 || '@scope.test', $1, NULLIF($2, '')::uuid) RETURNING id::text`, sub, cust)
		exec(`INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, $2)`, id, role)
		return id
	}
	mk("sc-ta-a", "tenant_admin", custA)
	mk("sc-ta-b", "tenant_admin", custB)
	mk("sc-pa", "platform_admin", "")
	userA := mk("sc-user-a", "business_user", custA)
	userB := mk("sc-user-b", "business_user", custB)

	// The dispatcher's view of the deployment row, as cmd/gateway wires it:
	// gated by the licence the server under test runs with.
	var lic *license.Manager
	notification.DeploymentDefaults = func(ctx context.Context) (notification.Settings, bool) {
		if lic == nil || !lic.Has(license.FeatureDeploymentSettings) {
			return notification.Settings{}, false
		}
		s, found, err := notification.NewStore(pool).GetSettings(ctx, notification.DeploymentScope)
		return s, err == nil && found
	}
	auditexport.Defaults = func(ctx context.Context) (auditexport.Settings, bool) {
		if lic == nil || !lic.Has(license.FeatureDeploymentSettings) {
			return auditexport.Settings{}, false
		}
		s, found, err := auditexport.GetSettings(ctx, pool, auditexport.DeploymentScope)
		return s, err == nil && found
	}
	aikeys.Defaults = func(ctx context.Context) (string, string, string, bool, bool) {
		if lic == nil || !lic.Has(license.FeatureDeploymentSettings) {
			return "", "", "", false, false
		}
		p, m, k, e, err := aikeys.NewStore(pool).Resolve(ctx, aikeys.DeploymentScope)
		return p, m, k, e, err == nil
	}
	t.Cleanup(func() { notification.DeploymentDefaults, auditexport.Defaults, aikeys.Defaults = nil, nil, nil })

	serve := func(l *license.Manager) *httptest.Server {
		lic = l
		srv := httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, nil, Deps{License: l}))
		t.Cleanup(srv.Close)
		return srv
	}
	outbound := func(userID string) int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM notification.notification WHERE recipient_user_id = $1::uuid AND channel = 'webhook'`, userID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	notify := func(userID string) {
		if _, err := notification.NewStore(pool).Notify(ctx, userID, "t", map[string]string{"subject": "s"}, "", ""); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("community: each tenant has only its own row, nothing is inherited", func(t *testing.T) {
		srv := serve(nil)
		if code, body := callJSON(t, srv, "sc-ta-a", http.MethodPut, "/api/notifications/settings", map[string]any{"webhook_enabled": true, "webhook_url": "https://a.example.test/hook"}); code != 200 || body["scope"].(map[string]any)["customer_id"] != custA {
			t.Fatalf("A saves: %d %v", code, body)
		}
		code, body := callJSON(t, srv, "sc-ta-b", http.MethodGet, "/api/notifications/settings", nil)
		if code != 200 || body["webhook_enabled"] != false || body["scope"].(map[string]any)["inherited"] != false {
			t.Fatalf("B must not see A's webhook: %d %v", code, body)
		}
		notify(userA)
		notify(userB)
		if outbound(userA) != 1 || outbound(userB) != 0 {
			t.Fatalf("webhook rows: A=%d B=%d — B's notification must not go to A's webhook", outbound(userA), outbound(userB))
		}
		// The platform admin edits a tenant by naming it; without naming one
		// there is no deployment row on this edition.
		if code, _ := callJSON(t, srv, "sc-pa", http.MethodGet, "/api/notifications/settings", nil); code != http.StatusForbidden {
			t.Fatalf("deployment row without the licence: %d", code)
		}
		if code, body := callJSON(t, srv, "sc-pa", http.MethodGet, "/api/notifications/settings", nil, tenantHeader, custA); code != 200 || body["webhook_url"] != "https://a.example.test/hook" {
			t.Fatalf("platform admin reading A's: %d %v", code, body)
		}
		if code, _ := callJSON(t, srv, "sc-ta-a", http.MethodGet, "/api/notifications/settings", nil, tenantHeader, custB); code != http.StatusForbidden {
			t.Fatalf("A naming B: %d", code)
		}
	})

	t.Run("enterprise: the deployment row is inherited until a tenant sets its own", func(t *testing.T) {
		srv := serve(enterpriseManager(t))
		if code, body := callJSON(t, srv, "sc-pa", http.MethodPut, "/api/notifications/settings", map[string]any{"webhook_enabled": true, "webhook_url": "https://deployment.example.test/hook"}); code != 200 || body["scope"].(map[string]any)["deployment"] != true {
			t.Fatalf("deployment save: %d %v", code, body)
		}
		code, body := callJSON(t, srv, "sc-ta-b", http.MethodGet, "/api/notifications/settings", nil)
		if code != 200 || body["webhook_url"] != "https://deployment.example.test/hook" || body["scope"].(map[string]any)["inherited"] != true {
			t.Fatalf("B inherits: %d %v", code, body)
		}
		notify(userB)
		if outbound(userB) != 1 {
			t.Fatalf("B's notification should now go out through the inherited webhook: %d", outbound(userB))
		}
		// A keeps its own; B sets its own, then drops it and inherits again.
		if _, body := callJSON(t, srv, "sc-ta-a", http.MethodGet, "/api/notifications/settings", nil); body["webhook_url"] != "https://a.example.test/hook" || body["scope"].(map[string]any)["inherited"] != false {
			t.Fatalf("A's own must win: %v", body)
		}
		if code, body := callJSON(t, srv, "sc-ta-b", http.MethodPut, "/api/notifications/settings", map[string]any{"webhook_enabled": false}); code != 200 || body["scope"].(map[string]any)["inherited"] != false {
			t.Fatalf("B's own: %d %v", code, body)
		}
		if code, body := callJSON(t, srv, "sc-ta-b", http.MethodDelete, "/api/notifications/settings", nil); code != 200 || body["scope"].(map[string]any)["inherited"] != true || body["webhook_enabled"] != true {
			t.Fatalf("B back to inheriting: %d %v", code, body)
		}
	})

	t.Run("audit retention sweeps each tenant by its own days", func(t *testing.T) {
		exec(`INSERT INTO audit.audit_event (event_type, category, actor_user_id, occurred_at) VALUES ('x.old', 'admin', $1::uuid, now() - interval '100 days'), ('x.old', 'admin', $2::uuid, now() - interval '100 days')`, userA, userB)
		if _, err := auditexport.UpdateSettings(ctx, pool, custA, 30); err != nil {
			t.Fatal(err)
		}
		lic = nil
		n, err := auditexport.Sweep(ctx, pool)
		if err != nil || n != 1 {
			t.Fatalf("sweep removed %d (%v): only A's event is past A's retention", n, err)
		}
		var left string
		if err := pool.QueryRow(ctx, `SELECT actor_user_id::text FROM audit.audit_event WHERE event_type = 'x.old'`).Scan(&left); err != nil || left != userB {
			t.Fatalf("B's event must survive: %q %v", left, err)
		}
	})

	t.Run("the AI key resolves to the tenant's own, else the deployment's on enterprise", func(t *testing.T) {
		store := aikeys.NewStore(pool)
		if _, err := store.Update(ctx, custA, aikeys.Settings{Provider: "anthropic", APIKey: "sk-a"}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Update(ctx, aikeys.DeploymentScope, aikeys.Settings{Provider: "openai", APIKey: "sk-deployment"}); err != nil {
			t.Fatal(err)
		}
		lic = nil
		if _, _, k, _, err := store.Resolve(ctx, custA); err != nil || k != "sk-a" {
			t.Fatalf("A's own: %q %v", k, err)
		}
		if _, _, _, _, err := store.Resolve(ctx, custB); err == nil {
			t.Fatal("B has no key and the community edition inherits none")
		}
		lic = enterpriseManager(t)
		if _, _, k, _, err := store.Resolve(ctx, custB); err != nil || k != "sk-deployment" {
			t.Fatalf("B inherits on enterprise: %q %v", k, err)
		}
		if _, _, k, _, err := store.Resolve(ctx, custA); err != nil || k != "sk-a" {
			t.Fatalf("A's own still wins: %q %v", k, err)
		}
	})

	t.Run("SCIM tokens are listed and revoked per tenant", func(t *testing.T) {
		srv := serve(enterpriseManager(t))
		code, tok := callJSON(t, srv, "sc-ta-a", http.MethodPost, "/api/admin/scim/tokens", map[string]any{"name": "A directory", "default_role": "business_user"})
		if code != http.StatusCreated {
			t.Fatalf("issue: %d %v", code, tok)
		}
		if code, list := callJSONList(t, srv, "sc-ta-b", "/api/admin/scim/tokens"); code != 200 || len(list) != 0 {
			t.Fatalf("B sees A's tokens: %d %v", code, list)
		}
		if code, _ := callJSON(t, srv, "sc-ta-b", http.MethodDelete, "/api/admin/scim/tokens/"+tok["id"].(string), nil); code != http.StatusNotFound {
			t.Fatalf("B revoking A's token: %d", code)
		}
		if code, list := callJSONList(t, srv, "sc-ta-a", "/api/admin/scim/tokens"); code != 200 || len(list) != 1 || !strings.HasPrefix(list[0]["name"].(string), "A directory") {
			t.Fatalf("A's own list: %d %v", code, list)
		}
	})
}

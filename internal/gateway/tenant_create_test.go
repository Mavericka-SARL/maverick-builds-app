package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/starter"
	"github.com/mavericks-engine/mavericks/internal/startersync"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// A self-hosted deployment's first administrator is a platform admin with
// no tenant (scripts/bootstrap-platform-admin.sh). Creating their
// organisation's tenant with join leaves them where a Playground sign-up
// arrives: every tenant role beside platform_admin, the "Getting started"
// application with every starter, opening on the tour, and the starters
// recorded so internal/startersync keeps them current. join is offered
// once, and a tenant created for someone else gets the application too
// unless asked not to.
func TestCreateTenantGettingStartedAndJoin(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	t.Setenv("DEV_MODE", "true")
	var padmin string
	if err := pool.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('first-admin', 'first@self.test', 'First Admin') RETURNING id::text`).Scan(&padmin); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'platform_admin')`, padmin); err != nil {
		t.Fatal(err)
	}
	// The identity provider mirrors the roles taken, as for a sign-up.
	broker := newFakeBroker(t)
	srv := httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, nil, Deps{Keycloak: broker.client()}))
	t.Cleanup(srv.Close)

	// Before: platform_admin only, and the console is told join is offered.
	code, me := callJSON(t, srv, "first-admin", http.MethodGet, "/api/me", nil)
	if code != 200 || me["join_new_tenant"] != true || me["customer_id"] != nil {
		t.Fatalf("me before: %d %v", code, me)
	}

	code, out := callJSON(t, srv, "first-admin", http.MethodPost, "/api/admin/tenants", map[string]any{"name": "Self Hosted Co", "join": true})
	if code != 200 || out["id"] == nil || out["application_id"] == nil || out["model_id"] == nil {
		t.Fatalf("create with join: %d %v", code, out)
	}
	tenantID, appID, landingID := out["id"].(string), out["application_id"].(string), out["model_id"].(string)

	code, me = callJSON(t, srv, "first-admin", http.MethodGet, "/api/me", nil)
	if code != 200 || me["customer_id"] != tenantID || me["join_new_tenant"] != nil {
		t.Fatalf("me after: %d %v", code, me)
	}
	held := map[string]bool{}
	for _, r := range me["roles"].([]any) {
		held[r.(string)] = true
	}
	for _, want := range append([]string{"platform_admin"}, everyTenantRole...) {
		if !held[want] {
			t.Fatalf("roles = %v, missing %s", me["roles"], want)
		}
	}
	broker.mu.Lock()
	mirrored := 0
	for _, sub := range broker.realmRoles {
		if sub == "first-admin" {
			mirrored++
		}
	}
	broker.mu.Unlock()
	if mirrored != len(everyTenantRole) {
		t.Fatalf("realm roles mirrored %d times, want %d", mirrored, len(everyTenantRole))
	}
	// The business roles are the tenant's default workspace's.
	var wsRoles int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM identity.role_assignment ra JOIN core.workspace w ON w.id = ra.workspace_id
		WHERE ra.user_id = $1::uuid AND w.customer_id = $2::uuid AND ra.role IN ('business_admin', 'business_user')`, padmin, tenantID).Scan(&wsRoles); err != nil || wsRoles != 2 {
		t.Fatalf("workspace roles = %d (%v), want 2", wsRoles, err)
	}

	// The application a sign-up gets, opening on the tour.
	code, apps := callJSONList(t, srv, "first-admin", "/api/apps")
	if code != 200 || len(apps) != 1 || apps[0]["name"] != startersync.AppName || apps[0]["id"] != appID {
		t.Fatalf("apps: %d %v", code, apps)
	}
	var defaultModel, landingName string
	if err := pool.QueryRow(ctx, `SELECT a.default_model_id::text, m.name FROM core.application a JOIN core.model m ON m.id = a.default_model_id WHERE a.id = $1::uuid`, appID).Scan(&defaultModel, &landingName); err != nil {
		t.Fatal(err)
	}
	var tourName string
	for _, s := range starter.Starters() {
		if s.Key == starter.LandingKey {
			tourName = s.Package.ModelName
		}
	}
	if defaultModel != landingID || landingName != tourName {
		t.Fatalf("default model %s %q, want %s %q", defaultModel, landingName, landingID, tourName)
	}
	var models, recorded int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM core.model WHERE application_id = $1::uuid),
		(SELECT count(*) FROM core.starter_model WHERE customer_id = $2::uuid AND model_id IS NOT NULL)`, appID, tenantID).Scan(&models, &recorded); err != nil {
		t.Fatal(err)
	}
	if n := len(starter.Starters()); models != n || recorded != n {
		t.Fatalf("models %d, recorded %d, want %d each", models, recorded, n)
	}
	// The developer role works there: the tour's dashboards answer.
	if code, _ := callJSON(t, srv, "first-admin", http.MethodGet, "/api/developer/dashboards", nil, "X-App-Id", appID, "X-Model-Id", landingID); code != 200 {
		t.Fatalf("developer dashboards: %d", code)
	}

	// Offered once: the account now has a tenant of its own.
	code, out = callJSON(t, srv, "first-admin", http.MethodPost, "/api/admin/tenants", map[string]any{"name": "Second Co", "join": true})
	if code != 400 || !strings.Contains(out["error"].(string), "already belongs to a tenant") {
		t.Fatalf("second join: %d %v", code, out)
	}

	// A tenant for someone else: the guides by default, none when asked.
	code, out = callJSON(t, srv, "first-admin", http.MethodPost, "/api/admin/tenants", map[string]any{"name": "Customer Co"})
	if code != 200 || out["application_id"] == nil {
		t.Fatalf("create for a customer: %d %v", code, out)
	}
	var owner string
	if err := pool.QueryRow(ctx, `SELECT customer_id::text FROM core.application WHERE id = $1::uuid`, out["application_id"]).Scan(&owner); err != nil || owner != out["id"] {
		t.Fatalf("application of %s belongs to %s (%v)", out["id"], owner, err)
	}
	code, out = callJSON(t, srv, "first-admin", http.MethodPost, "/api/admin/tenants", map[string]any{"name": "Empty Co", "getting_started": false})
	if code != 200 || out["application_id"] != nil {
		t.Fatalf("create without the guides: %d %v", code, out)
	}
	var apps2 int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM core.application WHERE customer_id = $1::uuid`, out["id"]).Scan(&apps2); err != nil || apps2 != 0 {
		t.Fatalf("Empty Co has %d applications (%v)", apps2, err)
	}
	// The creator's own tenant is unchanged by tenants made for others.
	var home string
	if err := pool.QueryRow(ctx, `SELECT customer_id::text FROM identity.user WHERE id = $1::uuid`, padmin).Scan(&home); err != nil || home != tenantID {
		t.Fatalf("home = %s (%v), want %s", home, err, tenantID)
	}
	// The console's first load, before an application is chosen, opens the
	// creator's own tour as a sign-up's does — not the deployment's largest
	// model, though Customer Co's guides are as large as theirs.
	code, demo := callJSON(t, srv, "first-admin", http.MethodGet, "/api/demo", nil)
	if code != 200 || demo["model_id"] != landingID || demo["app_id"] != appID {
		t.Fatalf("first load: %d model %v app %v, want %s in %s", code, demo["model_id"], demo["app_id"], landingID, appID)
	}
}

// With a database per tenant, a platform admin's roles come from the control
// plane in every tenant, so join is neither offered nor accepted; the
// guides are installed in the tenant's own database, imported by the
// admin's stand-in.
func TestCreateDedicatedTenantGettingStarted(t *testing.T) {
	f := newDedicatedFixture(t, Deps{})
	code, body := do(t, f.srv, f.pa, http.MethodGet, "/api/me", nil)
	var me map[string]any
	if code != 200 || json.Unmarshal(body, &me) != nil || me["join_new_tenant"] != nil {
		t.Fatalf("me: %d %s", code, body)
	}
	code, body = do(t, f.srv, f.pa, http.MethodPost, "/api/admin/tenants", map[string]any{"name": "Joined Co", "join": true})
	if code != 400 || !strings.Contains(string(body), "database per tenant") {
		t.Fatalf("join in dedicated mode: %d %s", code, body)
	}
	code, body = do(t, f.srv, f.pa, http.MethodPost, "/api/admin/tenants", map[string]any{"name": "Guided Co"})
	var out map[string]string
	if code != 200 || json.Unmarshal(body, &out) != nil || out["application_id"] == "" {
		t.Fatalf("create: %d %s", code, body)
	}
	p, err := f.router.Pool(f.ctx, out["id"])
	if err != nil {
		t.Fatal(err)
	}
	var models int
	var name string
	if err := p.QueryRow(f.ctx, `SELECT a.name, (SELECT count(*) FROM core.model m WHERE m.application_id = a.id) FROM core.application a WHERE a.id = $1::uuid`, out["application_id"]).Scan(&name, &models); err != nil {
		t.Fatal(err)
	}
	if name != startersync.AppName || models != len(starter.Starters()) {
		t.Fatalf("tenant database holds %q with %d models", name, models)
	}
	// Listed as any application is: the platform console shows it.
	if code, body := do(t, f.srv, f.pa, http.MethodGet, "/api/admin/tenants", nil); code != 200 || !strings.Contains(string(body), out["application_id"]) {
		t.Fatalf("tenants listing: %d %s", code, body)
	}
}

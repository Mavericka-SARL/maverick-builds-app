package gateway

// A list shows what the gate opens, on the list's own route, and nothing
// else. The console's lists of applications and models — the business
// Models tab and model switcher (GET /api/apps, which the chat connector
// reads too), Developer › Developer Models (GET /api/developer/applications)
// and Tenant admin › Applications (GET /api/admin/tenants) — used to restate
// the gate in SQL of their own. On the Playground a tenant admin who is also
// a developer had narrowed its own account to a few models of its tenant
// (identity.user_model_access): the gate still opened every application of
// its tenant, as a tenant admin's grants never narrow its admin scope, so
// the business console opened the application — but Developer Models and
// Tenant admin › Applications left it out, and the model switcher left out
// its model (2026-10-31).
//
// Rather than one assertion per list and role, every list is compared with
// the gate (actorCanAccessApp, actorCanAccessModel) for a matrix of accounts:
// each role, narrowed by application and model grants and not, in its own
// tenant and across two. Built on setupWSFixture: tenant 1 has ws1a (app1,
// models A — the default — and B), ws1b (app2, model C) and appT (tenant
// level, no workspace, no model); tenant 2 has ws2 (app3, model D).

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sort"
	"testing"

	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

type gateAgreementFixture struct {
	*wsFixture
	h        *handler
	modelC   string
	accounts []string
}

func setupGateAgreementFixture(t *testing.T) *gateAgreementFixture {
	t.Helper()
	f := &gateAgreementFixture{wsFixture: setupWSFixture(t)}
	ctx := context.Background()
	f.h = &handler{log: logger.New("test"), db: tenantdb.NewHandle(f.pool, nil), devMode: true}
	f.modelC = f.value(t, `SELECT id::text FROM core.model WHERE application_id=$1::uuid`, f.app2)

	type grant struct{ role, ws string }
	account := func(sub, cust string, roles []grant, apps, models []string) {
		t.Helper()
		id := f.value(t, `INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, $1||'@ga.test', $1, $2::uuid) RETURNING id::text`, sub, cust)
		for _, g := range roles {
			if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, $2::identity.user_role, NULLIF($3,'')::uuid)`, id, g.role, g.ws); err != nil {
				t.Fatalf("grant %s to %s: %v", g.role, sub, err)
			}
		}
		for _, app := range apps {
			if _, err := f.pool.Exec(ctx, `INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid)`, id, app); err != nil {
				t.Fatalf("app grant to %s: %v", sub, err)
			}
		}
		for _, m := range models {
			if _, err := f.pool.Exec(ctx, `INSERT INTO identity.user_model_access (user_id, model_id) VALUES ($1::uuid, $2::uuid)`, id, m); err != nil {
				t.Fatalf("model grant to %s: %v", sub, err)
			}
		}
		f.accounts = append(f.accounts, sub)
	}
	// The Playground's case: the sign-up owner (tenant admin, developer and
	// user of its tenant) granted itself both its applications but the
	// models of only one.
	account("ga-owner", f.cust1, []grant{{"tenant_admin", ""}, {"developer", ""}, {"business_user", f.ws1a}},
		[]string{f.app1, f.app2}, []string{f.modelA})
	account("ga-owner-open", f.cust1, []grant{{"tenant_admin", ""}, {"developer", ""}, {"business_user", f.ws1a}}, nil, nil)
	account("ga-admin-narrow", f.cust1, []grant{{"tenant_admin", ""}}, nil, []string{f.modelB})
	account("ga-dev-narrow", f.cust1, []grant{{"developer", ""}}, []string{f.app1}, []string{f.modelA})
	account("ga-devws-narrow", f.cust1, []grant{{"developer", f.ws1a}}, nil, []string{f.modelB})
	account("ga-user-narrow", f.cust1, []grant{{"business_user", f.ws1a}}, nil, []string{f.modelA})
	account("ga-ba-both", f.cust1, []grant{{"business_admin", f.ws1a}, {"business_admin", f.ws1b}}, []string{f.app2}, nil)
	// A developer of tenant 1 who administers tenant 2's workspace, narrowed
	// in tenant 1.
	account("ga-cross", f.cust1, []grant{{"developer", ""}, {"tenant_admin", f.ws2}}, nil, []string{f.modelA, f.modelD})
	// The fixture's own accounts: a tenant admin, a workspace developer, a
	// user, a business admin narrowed to app2, an inert unscoped business
	// admin and a platform admin.
	f.accounts = append(f.accounts, "mm-tenant-admin", "mm-dev", "mm-user", "mm-ba-narrow", "mm-ba-unscoped", "mm-platform")
	return f
}

// value is the single value sql returns.
func (f *gateAgreementFixture) value(t *testing.T, sql string, args ...any) string {
	t.Helper()
	var s string
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return s
}

// gateRoute is a list's route as the gate reads it from the context.
type gateRoute struct {
	name, path string
	ctx        func(context.Context) context.Context
}

var gateRoutes = []gateRoute{
	{"business", "/api/apps", func(c context.Context) context.Context { return c }},
	{"developer-or-administrator", "/api/developer/applications", withBuilderRoute},
	{"administrator", "/api/admin/tenants", func(c context.Context) context.Context {
		return context.WithValue(withBuilderRoute(c), adminRouteKey{}, true)
	}},
}

// gateOpens is every application and every model the gate opens sub on route.
func (f *gateAgreementFixture) gateOpens(t *testing.T, sub string, route gateRoute) (apps, models []string) {
	t.Helper()
	ctx := route.ctx(context.Background())
	act, err := f.h.actorByKeycloakSub(context.Background(), sub)
	if err != nil {
		t.Fatalf("actor %s: %v", sub, err)
	}
	for _, id := range f.column(t, `SELECT id::text FROM core.application`) {
		ok, err := f.h.actorCanAccessApp(ctx, act, id)
		if err != nil {
			t.Fatalf("gate %s app %s: %v", sub, id, err)
		}
		if ok {
			apps = append(apps, id)
		}
	}
	for _, id := range f.column(t, `SELECT id::text FROM core.model`) {
		ok, err := f.h.actorCanAccessModel(ctx, act, id)
		if err != nil {
			t.Fatalf("gate %s model %s: %v", sub, id, err)
		}
		if ok {
			models = append(models, id)
		}
	}
	sort.Strings(apps)
	sort.Strings(models)
	return apps, models
}

// listed is the applications and models route's list shows sub, ok false
// when the route refuses sub altogether.
func (f *gateAgreementFixture) listed(t *testing.T, sub string, route gateRoute) (apps, models []string, ok bool) {
	t.Helper()
	code, raw := f.do(t, http.MethodGet, route.path, sub, "", "", nil)
	if code == http.StatusForbidden {
		return nil, nil, false
	}
	if code != http.StatusOK {
		t.Fatalf("%s as %s: %d %s", route.path, sub, code, raw)
	}
	type model struct {
		ID string `json:"id"`
	}
	type app struct {
		ID     string  `json:"id"`
		Models []model `json:"models"`
	}
	var list []app
	if route.path == "/api/apps" {
		if err := json.Unmarshal(raw, &list); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
	} else {
		var tenants []struct {
			Applications []app `json:"applications"`
		}
		if err := json.Unmarshal(raw, &tenants); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		for _, tn := range tenants {
			list = append(list, tn.Applications...)
		}
	}
	for _, a := range list {
		apps = append(apps, a.ID)
		for _, m := range a.Models {
			models = append(models, m.ID)
		}
	}
	sort.Strings(apps)
	sort.Strings(models)
	return apps, models, true
}

// TestListsShowWhatTheGateOpens: on every list route, for every account, the
// models listed are exactly the models the gate opens; the applications
// listed are ones the gate opens, and every application with a model the
// gate opens is listed. On the business list an application with no model
// is shown only to whoever's scope opens it (the platform admin, a tenant
// admin of its tenant): there is nothing in it to switch to.
func TestListsShowWhatTheGateOpens(t *testing.T) {
	f := setupGateAgreementFixture(t)
	for _, route := range gateRoutes {
		for _, sub := range f.accounts {
			t.Run(route.name+"/"+sub, func(t *testing.T) {
				apps, models, ok := f.listed(t, sub, route)
				if !ok {
					return
				}
				gateApps, gateModels := f.gateOpens(t, sub, route)
				if !slices.Equal(models, gateModels) {
					t.Errorf("models listed %v, the gate opens %v", f.names(t, models), f.names(t, gateModels))
				}
				for _, id := range apps {
					if !slices.Contains(gateApps, id) {
						t.Errorf("application %s is listed but the gate refuses it", f.names(t, []string{id}))
					}
				}
				withModel := f.column(t, `SELECT DISTINCT application_id::text FROM core.model WHERE id::text = ANY($1)`, gateModels)
				for _, id := range withModel {
					if !slices.Contains(apps, id) {
						t.Errorf("application %s has a model the gate opens but is not listed", f.names(t, []string{id}))
					}
				}
				if route.path != "/api/apps" {
					// The builders' lists show every application the gate
					// opens, a model-less one too: building it is what those
					// screens are for.
					for _, id := range gateApps {
						if !slices.Contains(apps, id) {
							t.Errorf("application %s is opened by the gate but not listed", f.names(t, []string{id}))
						}
					}
				}
			})
		}
	}
}

// TestNarrowedTenantAdminSeesItsWholeTenant is the Playground's case by
// name: every list shows the owner app2 and its model C, which its own
// grants leave out, and the model opens from the switcher.
func TestNarrowedTenantAdminSeesItsWholeTenant(t *testing.T) {
	f := setupGateAgreementFixture(t)
	for _, route := range gateRoutes {
		apps, models, ok := f.listed(t, "ga-owner", route)
		if !ok {
			t.Fatalf("%s refused the tenant admin", route.path)
		}
		if !slices.Contains(apps, f.app2) || !slices.Contains(models, f.modelC) {
			t.Errorf("%s: app2 or its model C missing: apps %v, models %v", route.path, f.names(t, apps), f.names(t, models))
		}
		if !slices.Contains(models, f.modelB) {
			t.Errorf("%s: model B of app1 missing: %v", route.path, f.names(t, models))
		}
	}
	// The switcher's choice of an ungranted model of its own tenant opens.
	code, raw := f.do(t, http.MethodGet, "/api/demo", "ga-owner", f.app2, f.modelC, nil)
	if code != http.StatusOK {
		t.Fatalf("/api/demo app2 model C: %d %s", code, raw)
	}
	var demo struct {
		ModelID string `json:"model_id"`
	}
	if err := json.Unmarshal(raw, &demo); err != nil || demo.ModelID != f.modelC {
		t.Errorf("/api/demo opened %q, want model C (%v)", demo.ModelID, err)
	}
	// A narrowed user of the same tenant still gets exactly its grant.
	_, models, _ := f.listed(t, "ga-user-narrow", gateRoutes[0])
	if !slices.Equal(models, []string{f.modelA}) {
		t.Errorf("narrowed user lists %v, want only model A", f.names(t, models))
	}
	if code, _ := f.do(t, http.MethodGet, "/api/demo", "ga-user-narrow", f.app1, f.modelB, nil); code == http.StatusOK {
		t.Errorf("narrowed user opened model B from the switcher")
	}
}

// names is ids as the fixture's application and model names, for messages.
func (f *gateAgreementFixture) names(t *testing.T, ids []string) []string {
	t.Helper()
	if len(ids) == 0 {
		return nil
	}
	return f.column(t, `
		SELECT name FROM (SELECT id::text, name FROM core.application UNION ALL SELECT id::text, name FROM core.model) x
		WHERE id = ANY($1) ORDER BY name`, ids)
}

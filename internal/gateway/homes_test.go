package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// sharedTenant makes a tenant in the control plane, as shared mode does: a
// customer, a workspace, and an application with one model. It returns the
// tenant, workspace and application ids.
func (f *dedicatedFixture) sharedTenant(t *testing.T, name string) (id, workspace, app string) {
	t.Helper()
	q := func(sql string, args ...any) string {
		t.Helper()
		var out string
		if err := f.control.QueryRow(f.ctx, sql, args...).Scan(&out); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return out
	}
	id = q(`INSERT INTO core.customer (name, plan) VALUES ($1, 'enterprise') RETURNING id::text`, name)
	workspace = q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Default') RETURNING id::text`, id)
	app = q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, $3, 'planning') RETURNING id::text`,
		workspace, id, name+" Planning")
	q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Budget') RETURNING id::text`, app)
	return id, workspace, app
}

// appIn makes an application with one model in a dedicated tenant's
// database, and records it in the application directory.
func (f *dedicatedFixture) appIn(t *testing.T, tctx context.Context, workspace, name string) (app, model string) {
	t.Helper()
	if err := f.reader.db.QueryRow(tctx, `INSERT INTO core.application (workspace_id, customer_id, name, mode)
		VALUES ($1::uuid, $2::uuid, $3, 'planning') RETURNING id::text`, workspace, tenantdb.TenantFrom(tctx), name).Scan(&app); err != nil {
		t.Fatal(err)
	}
	if err := f.reader.db.QueryRow(tctx, `INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Budget') RETURNING id::text`, app).Scan(&model); err != nil {
		t.Fatal(err)
	}
	if err := f.router.Catalog().AddApplication(f.ctx, app, tenantdb.TenantFrom(tctx)); err != nil {
		t.Fatal(err)
	}
	return app, model
}

// get is a GET as c, decoded into out; it fails the test unless it answers 200.
func (f *dedicatedFixture) get(t *testing.T, c call, path string, out any) {
	t.Helper()
	code, body := do(t, f.srv, c, http.MethodGet, path, nil)
	if code != http.StatusOK || json.Unmarshal(body, out) != nil {
		t.Fatalf("GET %s as %s: %d %s", path, c.persona, code, body)
	}
}

type listed struct {
	ID       string `json:"id"`
	Email    string `json:"email"`
	Name     string `json:"name"`
	TenantID string `json:"tenant_id"`
	Status   string `json:"status"`
	// /api/admin/usage and /api/admin/tenants
	CustomerID string `json:"customer_id"`
}

func tenantsOf(rows []listed) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.TenantID)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// The platform admin sees every user, workspace, tenant, application and
// audit event, and every tenant's usage, in dedicated mode as in shared mode,
// whatever application was opened last (2026-10-01): the users, workspaces
// and audit views read one database, usage left out the control plane's
// tenants, and an application's X-App-Id took the tenants list into one
// tenant's database.
func TestPlatformAdminSeesEveryDatabase(t *testing.T) {
	f := newDedicatedFixture(t, Deps{License: enterpriseManager(t)})
	shared, sharedWS, sharedApp := f.sharedTenant(t, "SharedCo")
	var samID string
	if err := f.control.QueryRow(f.ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('kc-sam', 'sam@shared.test', 'Sam', $1::uuid) RETURNING id::text`, shared).Scan(&samID); err != nil {
		t.Fatal(err)
	}
	acme, acmeWS, actx := f.tenant(t, "Acme")
	globex, globexWS, gctx := f.tenant(t, "Globex")
	f.seed(t, actx, "kc-ann", "ann@acme.test", "Ann", "tenant_admin", acmeWS)
	f.seed(t, gctx, "kc-gil", "gil@globex.test", "Gil", "business_user", globexWS)
	acmeApp, _ := f.appIn(t, actx, acmeWS, "Acme Planning")
	globexApp, _ := f.appIn(t, gctx, globexWS, "Globex Planning")

	// Users: every database's, each naming where it lives; none a stand-in.
	var users []listed
	f.get(t, f.pa, "/api/admin/users", &users)
	where := map[string]string{}
	for _, u := range users {
		where[u.Email] = u.TenantID
		if strings.HasSuffix(u.Email, standInDomain) {
			t.Errorf("the users list shows a stand-in: %s", u.Email)
		}
	}
	for email, want := range map[string]string{"sam@shared.test": controlPlaneAddress, "pa@held.test": controlPlaneAddress,
		"ann@acme.test": acme, "gil@globex.test": globex} {
		if where[email] != want {
			t.Errorf("%s listed in %q; want %q (all: %v)", email, where[email], want, where)
		}
	}
	var acmeOnly []listed
	f.get(t, call{persona: "held-pa", tenant: acme}, "/api/admin/users", &acmeOnly)
	if got := tenantsOf(acmeOnly); !slices.Equal(got, []string{acme}) {
		t.Errorf("X-Tenant-Id: Acme lists users of %v", got)
	}

	// Workspaces, each with its database.
	var workspaces []listed
	f.get(t, f.pa, "/api/admin/workspaces", &workspaces)
	wsWhere := map[string]string{}
	for _, w := range workspaces {
		wsWhere[w.ID] = w.TenantID
	}
	if wsWhere[sharedWS] != controlPlaneAddress || wsWhere[acmeWS] != acme || wsWhere[globexWS] != globex {
		t.Errorf("workspaces: %v", wsWhere)
	}

	// Tenants and applications, the same whatever application was opened.
	for _, c := range []call{f.pa, {persona: "held-pa", app: acmeApp}} {
		var tenants []listed
		f.get(t, c, "/api/admin/tenants", &tenants)
		var ids []string
		for _, tn := range tenants {
			ids = append(ids, tn.ID)
		}
		for _, want := range []string{shared, acme, globex} {
			if !slices.Contains(ids, want) {
				t.Errorf("tenants as the platform admin (app %q) miss %s: %v", c.app, want, ids)
			}
		}
		var apps []listed
		f.get(t, c, "/api/apps", &apps)
		var appIDs []string
		for _, a := range apps {
			appIDs = append(appIDs, a.ID)
		}
		for _, want := range []string{sharedApp, acmeApp, globexApp} {
			if !slices.Contains(appIDs, want) {
				t.Errorf("applications as the platform admin (app %q) miss %s: %v", c.app, want, appIDs)
			}
		}
	}

	// The audit log spans databases: the control plane's tenant creations,
	// and Acme's own events.
	var events []listed
	f.get(t, f.pa, "/api/admin/audit", &events)
	if got := tenantsOf(events); !slices.Contains(got, controlPlaneAddress) {
		t.Errorf("the audit log has no control-plane events: %v", got)
	}
	if _, err := f.reader.db.Exec(actx, `INSERT INTO audit.audit_event (category, event_type, actor_role, resource_type, resource_id)
		VALUES ('admin', 'acme.marker', 'system', 'tenant', $1)`, acme); err != nil {
		t.Fatal(err)
	}
	f.get(t, f.pa, "/api/admin/audit", &events)
	if got := tenantsOf(events); !slices.Contains(got, acme) {
		t.Errorf("the audit log has no Acme events: %v", got)
	}

	// Usage: every tenant, and one whose database cannot be read is listed
	// with why, as on the tenants list.
	if err := f.router.Catalog().SetStatus(f.ctx, globex, tenantdb.StatusFailed, "disk full"); err != nil {
		t.Fatal(err)
	}
	f.router.Forget(globex)
	var usage struct {
		Tenants []listed `json:"tenants"`
	}
	f.get(t, f.pa, "/api/admin/usage", &usage)
	counted := map[string]string{}
	for _, u := range usage.Tenants {
		counted[u.CustomerID] = u.Status
	}
	if _, ok := counted[shared]; !ok || counted[acme] != "" || counted[globex] != tenantdb.StatusFailed {
		t.Errorf("usage: %v; want SharedCo and Acme counted, Globex failed", counted)
	}
	var tenants []listed
	f.get(t, f.pa, "/api/admin/tenants", &tenants)
	for _, tn := range tenants {
		if tn.ID == globex && tn.Status != tenantdb.StatusFailed {
			t.Errorf("the tenants list shows Globex as %q; want failed", tn.Status)
		}
	}

	// The platform admin's preferences are theirs wherever they work.
	if code, body := do(t, f.srv, call{persona: "held-pa", app: acmeApp}, http.MethodPatch, "/api/me/preferences",
		map[string]any{"theme": "dark"}); code != http.StatusOK {
		t.Fatalf("preferences from Acme: %d %s", code, body)
	}
	var stored string
	if err := f.control.QueryRow(f.ctx, `SELECT preferences->>'theme' FROM identity.user WHERE id = $1::uuid`, f.paID).Scan(&stored); err != nil || stored != "dark" {
		t.Errorf("the platform admin's theme saved from Acme is %q in the control plane (%v); want dark", stored, err)
	}
}

// A person the control plane and dedicated tenants hold reaches each of them
// (2026-10-01): their applications and notifications are listed from every
// home, each by their own account there; the control plane is addressed by
// one of its applications or as "control-plane". Every home lists by what
// the person holds in it: a tenant admin of Acme who is a business user of
// Globex sees none of Globex's applications on the administration list.
func TestAPersonReachesEveryHome(t *testing.T) {
	f := newDedicatedFixture(t, Deps{})
	shared, sharedWS, sharedApp := f.sharedTenant(t, "SharedCo")
	_ = shared
	acme, acmeWS, actx := f.tenant(t, "Acme")
	globex, globexWS, gctx := f.tenant(t, "Globex")
	f.seed(t, actx, "kc-ann", "ann@acme.test", "Ann", "tenant_admin", acmeWS)
	f.seed(t, gctx, "kc-gail", "gail@globex.test", "Gail", "tenant_admin", globexWS)
	f.seed(t, gctx, "kc-gil", "gil@globex.test", "Gil", "business_user", globexWS)
	acmeApp, _ := f.appIn(t, actx, acmeWS, "Acme Planning")
	globexApp, _ := f.appIn(t, gctx, globexWS, "Globex Planning")

	// Carl: the control plane's, a business user of SharedCo, and a member
	// of Acme.
	var carlControl string
	if err := f.control.QueryRow(f.ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('kc-carl', 'carl@shared.test', 'Carl', $1::uuid) RETURNING id::text`, shared).Scan(&carlControl); err != nil {
		t.Fatal(err)
	}
	if _, err := f.control.Exec(f.ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		VALUES ($1::uuid, 'business_user', $2::uuid)`, carlControl, sharedWS); err != nil {
		t.Fatal(err)
	}
	f.broker.mu.Lock()
	f.broker.users["kc-carl"] = &fakeKCUser{Email: "carl@shared.test", First: "Carl", Last: "Shared", Enabled: true}
	f.broker.mu.Unlock()
	ann := call{persona: "kc-ann"}
	f.invite(t, ann, "carl@shared.test", "Carl", "business_user", acmeWS, http.StatusOK)
	carlAcme, ok := f.row(actx, "carl@shared.test")
	if !ok || carlAcme.customer != "" {
		t.Fatalf("Carl in Acme: %+v (ok %v); want a member", carlAcme, ok)
	}

	// Both homes' applications, each with its tenant.
	carl := call{persona: "kc-carl"}
	var apps []struct {
		ID         string `json:"id"`
		TenantName string `json:"tenant_name"`
	}
	f.get(t, carl, "/api/apps", &apps)
	var appIDs []string
	for _, a := range apps {
		appIDs = append(appIDs, a.ID)
	}
	if !slices.Contains(appIDs, sharedApp) || !slices.Contains(appIDs, acmeApp) || slices.Contains(appIDs, globexApp) {
		t.Errorf("Carl's applications: %v; want SharedCo's and Acme's only", apps)
	}

	// The control plane is addressed through its application, or by name.
	me := func(c call) string {
		t.Helper()
		var out struct {
			UserID string `json:"user_id"`
		}
		f.get(t, c, "/api/me", &out)
		return out.UserID
	}
	if got := me(carl); got != carlAcme.id {
		t.Errorf("Carl with nothing addressed is %s; want his Acme account %s", got, carlAcme.id)
	}
	if got := me(call{persona: "kc-carl", app: sharedApp}); got != carlControl {
		t.Errorf("Carl opening SharedCo's application is %s; want his control-plane account %s", got, carlControl)
	}
	if got := me(call{persona: "kc-carl", tenant: controlPlaneAddress}); got != carlControl {
		t.Errorf("Carl addressing the control plane is %s; want %s", got, carlControl)
	}
	// Someone the control plane does not hold is never routed there.
	gil, _ := f.row(gctx, "gil@globex.test")
	if got := me(call{persona: "kc-gil", tenant: controlPlaneAddress}); got != gil.id {
		t.Errorf("Gil addressing the control plane is %s; want his Globex account %s", got, gil.id)
	}
	if got := me(call{persona: "kc-gil", app: sharedApp}); got != gil.id {
		t.Errorf("Gil opening SharedCo's application is %s; want his Globex account %s", got, gil.id)
	}

	// Notifications from every home, and marked read in their own.
	var notifID string
	if err := f.control.QueryRow(f.ctx, `INSERT INTO notification.notification (recipient_user_id, template_id)
		VALUES ($1::uuid, 'control.marker') RETURNING id::text`, carlControl).Scan(&notifID); err != nil {
		t.Fatal(err)
	}
	var notes []struct {
		ID         string `json:"id"`
		TemplateID string `json:"template_id"`
	}
	f.get(t, carl, "/api/notifications", &notes)
	var templates []string
	for _, n := range notes {
		templates = append(templates, n.TemplateID)
	}
	if !slices.Contains(templates, "control.marker") || !slices.Contains(templates, accessGrantedTemplate) {
		t.Errorf("Carl's notifications: %v; want the control plane's and Acme's", templates)
	}
	code, body := do(t, f.srv, carl, http.MethodPost, "/api/notifications/mark-read", map[string]any{"ids": []string{notifID}})
	if code != http.StatusOK || !strings.Contains(string(body), `"updated":1`) {
		t.Errorf("Carl marking the control plane's notification read: %d %s", code, body)
	}

	// Ann: Acme's tenant admin, added to Globex as a business user. Her
	// administration lists Acme only.
	f.invite(t, call{persona: "kc-gail"}, "ann@acme.test", "Ann", "business_user", globexWS, http.StatusOK)
	var tenants []struct {
		ID           string            `json:"id"`
		Applications []json.RawMessage `json:"applications"`
	}
	f.get(t, ann, "/api/admin/tenants", &tenants)
	for _, tn := range tenants {
		if tn.ID == globex {
			t.Errorf("Acme's admin, a business user of Globex, lists Globex with %d applications", len(tn.Applications))
		}
	}
	if len(tenants) != 1 || tenants[0].ID != acme {
		t.Errorf("Acme's admin lists %d tenants; want Acme only", len(tenants))
	}
}

// A platform-wide builder — a developer the control plane holds with no
// tenant and no narrowing grants — builds in every tenant, dedicated ones
// included (2026-10-01): it was looked up in a dedicated tenant's database,
// held no account there, and was refused.
func TestAPlatformWideBuilderReachesDedicatedTenants(t *testing.T) {
	f := newDedicatedFixture(t, Deps{License: enterpriseManager(t), PublicURL: "https://console.test"})
	acme, acmeWS, actx := f.tenant(t, "Acme")
	acmeApp, acmeModel := f.appIn(t, actx, acmeWS, "Acme Planning")
	var beaID string
	if err := f.control.QueryRow(f.ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name)
		VALUES ('kc-bea', 'bea@builder.test', 'Bea') RETURNING id::text`).Scan(&beaID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.control.Exec(f.ctx, `INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'developer')`, beaID); err != nil {
		t.Fatal(err)
	}
	bea := call{persona: "kc-bea"}

	var tenants []struct {
		ID string `json:"id"`
	}
	f.get(t, bea, "/api/developer/applications", &tenants)
	listedAcme := false
	for _, tn := range tenants {
		listedAcme = listedAcme || tn.ID == acme
	}
	if !listedAcme {
		t.Errorf("the platform-wide builder's applications leave out Acme: %v", tenants)
	}
	inAcme := call{persona: "kc-bea", app: acmeApp}
	if code, body := do(t, f.srv, inAcme, http.MethodGet, "/api/developer/revisions?model_id="+acmeModel, nil); code != http.StatusOK {
		t.Errorf("the platform-wide builder reading Acme's revisions: %d %s", code, body)
	}
	// Only a builder there: its tenant_admin of a shared tenant is that
	// tenant's, and Acme's administration refuses it.
	_, sharedWS, _ := f.sharedTenant(t, "SharedCo")
	if _, err := f.control.Exec(f.ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		VALUES ($1::uuid, 'tenant_admin', $2::uuid)`, beaID, sharedWS); err != nil {
		t.Fatal(err)
	}
	asAcme := call{persona: "kc-bea", tenant: acme}
	for _, try := range []struct{ method, path string }{
		{http.MethodPost, "/api/admin/scim/tokens"},
		{http.MethodGet, "/api/admin/sso"},
		{http.MethodGet, "/api/admin/usage"},
	} {
		if code, body := do(t, f.srv, asAcme, try.method, try.path, map[string]string{"name": "Mine"}); code == http.StatusOK || code == http.StatusCreated {
			t.Errorf("the builder, tenant_admin of a shared tenant, %s %s in Acme: %d %s; want refused", try.method, try.path, code, body)
		}
	}
	if code, body := do(t, f.srv, inAcme, http.MethodGet, "/api/developer/revisions?model_id="+acmeModel, nil); code != http.StatusOK {
		t.Errorf("the builder, also a shared tenant's admin, reading Acme's revisions: %d %s", code, body)
	}
	// Its stand-in in Acme holds nothing.
	var standIn bool
	if err := f.reader.db.QueryRow(actx, `SELECT stand_in FROM identity.user WHERE id = $1::uuid`, beaID).Scan(&standIn); err != nil || !standIn {
		t.Errorf("the builder's row in Acme is no stand-in (%v)", err)
	}
	// Narrowed to an application in the control plane, it is no longer
	// platform-wide, and Acme refuses it.
	if _, err := f.control.Exec(f.ctx, `INSERT INTO core.customer (id, name, plan) VALUES (gen_random_uuid(), 'X', 'enterprise')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.control.Exec(f.ctx, `
		WITH c AS (SELECT id FROM core.customer WHERE name = 'X'),
		     w AS (INSERT INTO core.workspace (customer_id, name) SELECT id, 'W' FROM c RETURNING id),
		     a AS (INSERT INTO core.application (workspace_id, name, mode) SELECT id, 'X app', 'planning' FROM w RETURNING id)
		INSERT INTO identity.user_app_access (user_id, application_id) SELECT $1::uuid, id FROM a`, beaID); err != nil {
		t.Fatal(err)
	}
	if code, body := do(t, f.srv, inAcme, http.MethodGet, "/api/developer/revisions?model_id="+acmeModel, nil); code == http.StatusOK {
		t.Errorf("a narrowed builder still reads Acme's revisions: %d %s", code, body)
	}
}

// Routing decides by membership and by platform reach in the control plane,
// never by having no membership (2026-10-01): a first sign-in through one
// tenant's identity provider, aimed at another tenant, was routed there. A
// home whose account is deactivated is no default.
func TestRoutingDecidesByMembershipAndReach(t *testing.T) {
	f := newDedicatedFixture(t, Deps{})
	acme, acmeWS, actx := f.tenant(t, "Acme")
	globex, globexWS, gctx := f.tenant(t, "Globex")
	f.seed(t, actx, "kc-ann", "ann@acme.test", "Ann", "tenant_admin", acmeWS)
	annGlobex := f.seed(t, gctx, "kc-ann", "ann@acme.test", "Ann", "business_user", globexWS)
	_ = annGlobex
	h := &handler{log: f.reader.log, db: f.reader.db, devMode: true}
	route := func(persona, tenant string) (string, bool) {
		t.Helper()
		var got string
		var marked bool
		next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			got = tenantdb.TenantFrom(r.Context())
			marked, _ = r.Context().Value(actorOnControlKey{}).(bool)
		})
		req, _ := http.NewRequestWithContext(f.ctx, http.MethodGet, "/api/me", nil)
		req.Header.Set("X-Dev-User", persona)
		if tenant != "" {
			req.Header.Set("X-Tenant-Id", tenant)
		}
		h.tenantRouting(next).ServeHTTP(httptest.NewRecorder(), req)
		return got, marked
	}
	if got, marked := route("kc-nobody", acme); got != "" || marked {
		t.Errorf("a subject held nowhere, aiming at Acme, was routed to %q (marked %v); want the control plane", got, marked)
	}
	if got, marked := route("held-pa", acme); got != acme || !marked {
		t.Errorf("the platform admin aiming at Acme was routed to %q (marked %v)", got, marked)
	}
	// Ann's directory lists Acme then Globex; aiming at a tenant she is not
	// in leaves her in her first.
	if err := f.router.Catalog().AddUser(f.ctx, "kc-ann", globex, "ann@acme.test"); err != nil {
		t.Fatal(err)
	}
	if got, _ := route("kc-ann", "00000000-0000-0000-0000-000000000001"); got != acme {
		t.Errorf("Ann aiming at a tenant she is not in was routed to %q; want Acme", got)
	}
	// Deactivated in Acme, Ann's default is Globex.
	if _, err := f.reader.db.Exec(actx, `UPDATE identity.user SET disabled_at = now() WHERE keycloak_sub = 'kc-ann'`); err != nil {
		t.Fatal(err)
	}
	h.active = nil // what the routing cache held for activeHomeTTL
	if got, _ := route("kc-ann", ""); got != globex {
		t.Errorf("Ann, deactivated in Acme, was routed to %q; want Globex", got)
	}
}

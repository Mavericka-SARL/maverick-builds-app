package gateway

// A platform-wide builder (isGlobalBuilder: developer, an unscoped developer
// grant, no tenant of its own, no application or model grant) builds in every
// tenant, and adminScopeCustomerIDs makes it administrator of every tenant.
// Two gaps, both confirmed live on 2026-09-29:
//
//  1. Anyone allowed to grant developer could make one. A tenant admin
//     granted unscoped developer to a member with no customer_id — and a
//     tenant_admin with no customer_id granted it to itself — and the
//     account then edited tenant 2's metrics and applications.
//     platformWideGrantErr now keeps that grant, on every path, to platform
//     admins; anyone else names a workspace.
//  2. An account narrowed to some applications or models by user_app_access
//     or user_model_access was still platform-wide: isGlobalBuilder answered
//     before the grants were read. Now its grants are its scope. Removing the
//     last of them — directly or by deleting what it grants — was then kept
//     to platform admins; since 2026-09-30 anyone else's removal goes ahead
//     and takes the account's unscoped developer grant with it.
//
// These reuse setupDevRouteFixture (developer_route_scope_test.go): tenant 1
// has ws1a (app1, models A and B), ws1b (app2) and appT; tenant 2 has ws2
// (app3, model D). mm-tenant-admin is tenant 1's admin, mm-platform a
// platform admin.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/mavericks-engine/mavericks/ee/sso"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	"github.com/mavericks-engine/mavericks/internal/workflow"
	"github.com/mavericks-engine/mavericks/internal/workflow/assignee/assigneetest"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// gbUser creates an account of tenant cust ("" = none of its own) with
// grants role → workspace ("" = unscoped), and returns its id.
func (f *devRouteFixture) gbUser(t *testing.T, sub, cust string, grants ...[2]string) string {
	t.Helper()
	id := f.one(t, `INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ($1, $1||'@gb.test', $1, NULLIF($2,'')::uuid) RETURNING id::text`, sub, cust)
	for _, g := range grants {
		if _, err := f.pool.Exec(context.Background(), `INSERT INTO identity.role_assignment (user_id, role, workspace_id)
			VALUES ($1::uuid, $2::identity.user_role, NULLIF($3,'')::uuid)`, id, g[0], g[1]); err != nil {
			t.Fatalf("grant %s@%q to %s: %v", g[0], g[1], sub, err)
		}
	}
	return id
}

// expect sends a request and fails unless the status is one of want.
func (f *devRouteFixture) expect(t *testing.T, sub, method, path, app string, body any, want ...int) []byte {
	t.Helper()
	code, raw := f.do(t, method, path, sub, app, "", body)
	if !slices.Contains(want, code) {
		t.Errorf("%s %s (app %q) as %s: status %d %s, want %v", method, path, app, sub, code, raw, want)
	}
	return raw
}

// renameMetric sends a metric PATCH that keeps its name: a write that proves
// reach without changing anything another assertion reads.
func (f *devRouteFixture) renameMetric(t *testing.T, sub, metricID string, want ...int) {
	t.Helper()
	name := f.one(t, `SELECT name FROM model.metric_def WHERE id=$1::uuid`, metricID)
	f.expect(t, sub, "PATCH", "/api/developer/metrics/"+metricID, "", map[string]string{"name": name}, want...)
}

// The two live cases, and what a tenant admin can still grant.
func TestTenantAdminCannotMakePlatformWideBuilder(t *testing.T) {
	f := setupDevRouteFixture(t)
	const ta = "mm-tenant-admin"
	taID := f.one(t, `SELECT id::text FROM identity.user WHERE keycloak_sub=$1`, ta)
	appRename := map[string]string{"name": "Hijacked application"}

	// Live case 1: a member of tenant 1 (business_user in ws1a) with no
	// customer_id of its own.
	member := f.gbUser(t, "gb-member", "", [2]string{"business_user", f.ws1a})
	raw := f.expect(t, ta, "POST", "/api/admin/users/"+member+"/roles", "", map[string]string{"role": "developer"}, http.StatusForbidden)
	if !strings.Contains(string(raw), "workspace") {
		t.Errorf("refusal does not tell the admin to name a workspace: %s", raw)
	}
	if n := f.one(t, `SELECT count(*)::text FROM identity.role_assignment WHERE user_id=$1::uuid AND role='developer'`, member); n != "0" {
		t.Errorf("refused grant left %s developer rows", n)
	}
	f.renameMetric(t, "gb-member", f.metricD, http.StatusForbidden)
	f.expect(t, "gb-member", "PATCH", "/api/admin/applications/"+f.app3, "", appRename, http.StatusForbidden)

	// A workspace-scoped developer grant to the same member is the tenant
	// admin's to make, and builds in tenant 1 only.
	f.expect(t, ta, "POST", "/api/admin/users/"+member+"/roles", "", map[string]string{"role": "developer", "workspace_id": f.ws1a}, http.StatusOK)
	f.renameMetric(t, "gb-member", f.metricA, http.StatusOK)
	f.renameMetric(t, "gb-member", f.metricD, http.StatusForbidden, http.StatusNotFound)
	if by := f.one(t, `SELECT COALESCE(assigned_by::text,'') FROM identity.role_assignment
		WHERE user_id=$1::uuid AND role='developer' AND workspace_id=$2::uuid`, member, f.ws1a); by != taID {
		t.Errorf("grant assigned_by = %q, want the tenant admin %s", by, taID)
	}

	// A member WITH a customer_id may still get unscoped developer: it is
	// then the developer of its own tenant, not of tenant 2.
	f.expect(t, ta, "POST", "/api/admin/users/"+f.user1b+"/roles", "", map[string]string{"role": "developer"}, http.StatusOK)
	f.renameMetric(t, "ws-bu1b", f.metricC, http.StatusOK)
	f.renameMetric(t, "ws-bu1b", f.metricD, http.StatusForbidden, http.StatusNotFound)

	// Live case 2: a tenant_admin of ws1a with no customer_id, granting
	// itself. It could not grant this to another account, so not to itself.
	self := f.gbUser(t, "gb-ta-self", "", [2]string{"tenant_admin", f.ws1a})
	f.expect(t, "gb-ta-self", "PATCH", "/api/admin/applications/"+f.app3, "", appRename, http.StatusForbidden)
	f.expect(t, "gb-ta-self", "POST", "/api/admin/users/"+self+"/roles", "", map[string]string{"role": "developer"}, http.StatusForbidden)
	f.renameMetric(t, "gb-ta-self", f.metricD, http.StatusForbidden)
	f.expect(t, "gb-ta-self", "PATCH", "/api/admin/applications/"+f.app3, "", appRename, http.StatusForbidden)
	// What it may grant anyone, it may grant itself: developer inside its
	// own workspace, which adds nothing in tenant 2.
	f.expect(t, "gb-ta-self", "POST", "/api/admin/users/"+self+"/roles", "", map[string]string{"role": "developer", "workspace_id": f.ws1a}, http.StatusOK)
	f.renameMetric(t, "gb-ta-self", f.metricD, http.StatusForbidden, http.StatusNotFound)
	f.expect(t, "gb-ta-self", "PATCH", "/api/admin/applications/"+f.app3, "", appRename, http.StatusForbidden)
	if got := f.one(t, `SELECT name FROM core.application WHERE id=$1::uuid`, f.app3); got == appRename["name"] {
		t.Errorf("tenant 2's application was renamed")
	}

	// Creating the account is not a way around it: a platform-wide builder
	// that also holds tenant_admin without a workspace may not create a
	// platform-wide developer. Nor one inside a workspace: its tenant_admin
	// grant, on an account with no tenant, is held in the tenants it holds a
	// workspace role in — none — and its developer grant makes it a developer
	// of tenant 1, which grants no developer (roles count per tenant,
	// decided 2026-09-30).
	f.gbUser(t, "gb-global-ta", "", [2]string{"developer", ""}, [2]string{"tenant_admin", ""})
	create := func(sub, email, ws string, want int) {
		t.Helper()
		body := map[string]string{"email": email, "first_name": "Gee", "last_name": "Bee", "role": "developer"}
		if ws != "" {
			body["workspace_id"] = ws
		}
		f.expect(t, sub, "POST", "/api/admin/users/", "", body, want)
	}
	create("gb-global-ta", "minted@gb.test", "", http.StatusBadRequest)
	if n := f.one(t, `SELECT count(*)::text FROM identity.user WHERE email='minted@gb.test'`); n != "0" {
		t.Errorf("refused creation left %s accounts", n)
	}
	create("gb-global-ta", "scoped@gb.test", f.ws1a, http.StatusForbidden)

	// A platform admin still makes a platform-wide developer, which then
	// builds in every tenant.
	create("mm-platform", "platform-dev@gb.test", "", http.StatusOK)
	f.renameMetric(t, "admin-created-platform-dev@gb.test", f.metricD, http.StatusOK)
	f.renameMetric(t, "admin-created-platform-dev@gb.test", f.metricA, http.StatusOK)
	platformID := f.one(t, `SELECT id::text FROM identity.user WHERE keycloak_sub='mm-platform'`)
	if by := f.one(t, `SELECT COALESCE(ra.assigned_by::text,'') FROM identity.role_assignment ra
		JOIN identity.user u ON u.id = ra.user_id WHERE u.email='platform-dev@gb.test' AND ra.role='developer'`); by != platformID {
		t.Errorf("created grant assigned_by = %q, want the platform admin %s", by, platformID)
	}
	// ...and grants one to an existing account with no tenant.
	other := f.gbUser(t, "gb-member2", "", [2]string{"business_user", f.ws1a})
	f.expect(t, "mm-platform", "POST", "/api/admin/users/"+other+"/roles", "", map[string]string{"role": "developer"}, http.StatusOK)
	f.renameMetric(t, "gb-member2", f.metricD, http.StatusOK)
}

// A developer with no tenant narrowed by application or model grants builds
// only there; one with no grant keeps its reach everywhere.
func TestNarrowedCustomerlessDeveloperIsNotPlatformWide(t *testing.T) {
	f := setupDevRouteFixture(t)
	ctx := context.Background()

	// Existing platform-wide developers are unchanged.
	f.gbUser(t, "gb-global", "", [2]string{"developer", ""})
	f.renameMetric(t, "gb-global", f.metricD, http.StatusOK)
	f.renameMetric(t, "gb-global", f.metricA, http.StatusOK)
	f.expect(t, "gb-global", "GET", "/api/developer/model", f.app3, nil, http.StatusOK)

	narrow := f.gbUser(t, "gb-narrow", "", [2]string{"developer", ""})
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid)`, narrow, f.app1); err != nil {
		t.Fatal(err)
	}
	f.renameMetric(t, "gb-narrow", f.metricD, http.StatusForbidden, http.StatusNotFound)
	f.renameMetric(t, "gb-narrow", f.metricA, http.StatusOK)
	f.renameMetric(t, "gb-narrow", f.metricC, http.StatusForbidden, http.StatusNotFound) // tenant 1, but not app1
	f.expect(t, "gb-narrow", "GET", "/api/developer/model", f.app3, nil, http.StatusForbidden, http.StatusNotFound)
	f.expect(t, "gb-narrow", "PATCH", "/api/admin/applications/"+f.app3, "", map[string]string{"name": "Hijacked"}, http.StatusForbidden)

	// With no application named it opens its own.
	var model struct {
		ModelID string `json:"model_id"`
	}
	if err := json.Unmarshal(f.expect(t, "gb-narrow", "GET", "/api/developer/model", "", nil, http.StatusOK), &model); err != nil || model.ModelID != f.modelA {
		t.Errorf("model with no application named: %q (%v), want model A %s", model.ModelID, err, f.modelA)
	}
	var apps []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(f.expect(t, "gb-narrow", "GET", "/api/apps", "", nil, http.StatusOK), &apps); err != nil || len(apps) != 1 || apps[0].ID != f.app1 {
		t.Errorf("/api/apps: %+v (%v), want only app1 %s", apps, err, f.app1)
	}
	var tenants []struct {
		ID           string `json:"id"`
		Applications []struct {
			ID string `json:"id"`
		} `json:"applications"`
	}
	if err := json.Unmarshal(f.expect(t, "gb-narrow", "GET", "/api/developer/applications", "", nil, http.StatusOK), &tenants); err != nil ||
		len(tenants) != 1 || tenants[0].ID != f.cust1 || len(tenants[0].Applications) != 1 || tenants[0].Applications[0].ID != f.app1 {
		t.Errorf("/api/developer/applications: %+v (%v), want tenant 1 with app1 only", tenants, err)
	}
	// A developer administers the business roles of the tenants it builds
	// in (canAdministerWorkspace), and no others.
	f.expect(t, "gb-narrow", "GET", "/api/business-admin/roles", f.app1, nil, http.StatusOK)
	f.expect(t, "gb-narrow", "GET", "/api/business-admin/roles", f.app3, nil, http.StatusForbidden, http.StatusNotFound)

	// Narrowed to a model: that model only.
	byModel := f.gbUser(t, "gb-narrow-model", "", [2]string{"developer", ""})
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.user_model_access (user_id, model_id) VALUES ($1::uuid, $2::uuid)`, byModel, f.modelB); err != nil {
		t.Fatal(err)
	}
	f.renameMetric(t, "gb-narrow-model", f.metricB, http.StatusOK)
	f.renameMetric(t, "gb-narrow-model", f.metricA, http.StatusForbidden, http.StatusNotFound)
	f.renameMetric(t, "gb-narrow-model", f.metricD, http.StatusForbidden, http.StatusNotFound)

	// Removing the last grant would make it platform-wide again. A tenant
	// admin's removal goes ahead and takes the unscoped developer grant with
	// it, audited as its own event: the account ends with no builder reach,
	// never platform-wide (decided 2026-09-30). Directly, by the grant:
	const ta = "mm-tenant-admin"
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid)`, narrow, f.app2); err != nil {
		t.Fatal(err)
	}
	// A grant that is not the last one goes as before, and takes nothing else.
	raw := f.expect(t, ta, "DELETE", "/api/admin/users/"+narrow+"/access/apps/"+f.app2, "", nil, http.StatusOK)
	if strings.Contains(string(raw), "revoked") {
		t.Errorf("removing a grant that is not the last revoked something: %s", raw)
	}
	f.renameMetric(t, "gb-narrow", f.metricA, http.StatusOK)
	// The last one, in any spelling of the ids.
	up := strings.ToUpper
	raw = f.expect(t, ta, "DELETE", "/api/admin/users/"+up(narrow)+"/access/apps/"+up(f.app1), "", nil, http.StatusOK)
	if !strings.Contains(string(raw), `"revoked"`) || !strings.Contains(string(raw), "gb-narrow@gb.test") {
		t.Errorf("last grant removal does not report the revoked developer grant: %s", raw)
	}
	f.assertNoBuilderReach(t, "gb-narrow", narrow, "app_access_revoked")

	// By deleting the model it was narrowed to.
	raw = f.expect(t, ta, "DELETE", "/api/admin/models/"+up(f.modelB), "", nil, http.StatusOK)
	if !strings.Contains(string(raw), "gb-narrow-model@gb.test") {
		t.Errorf("model delete does not report the revoked developer grant: %s", raw)
	}
	f.assertNoBuilderReach(t, "gb-narrow-model", byModel, "model_deleted")

	// By deleting the application, for every account it was the last grant
	// of, directly or through one of its models.
	byApp := f.gbUser(t, "gb-narrow-app", "", [2]string{"developer", ""})
	byAppModel := f.gbUser(t, "gb-narrow-appmodel", "", [2]string{"developer", ""})
	kept := f.gbUser(t, "gb-narrow-kept", "", [2]string{"developer", ""})
	for _, g := range []struct{ sql, user, id string }{
		{`INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid)`, byApp, f.app1},
		{`INSERT INTO identity.user_model_access (user_id, model_id) VALUES ($1::uuid, $2::uuid)`, byAppModel, f.modelA},
		{`INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid)`, kept, f.app1},
		{`INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid)`, kept, f.app2},
	} {
		if _, err := f.pool.Exec(ctx, g.sql, g.user, g.id); err != nil {
			t.Fatal(err)
		}
	}
	f.expect(t, ta, "DELETE", "/api/admin/applications/"+f.app1, "", nil, http.StatusOK)
	f.assertNoBuilderReach(t, "gb-narrow-app", byApp, "application_deleted")
	f.assertNoBuilderReach(t, "gb-narrow-appmodel", byAppModel, "application_deleted")
	// An account the application was not the last grant of keeps its role.
	if n := f.one(t, `SELECT count(*)::text FROM identity.role_assignment WHERE user_id=$1::uuid AND role='developer'`, kept); n != "1" {
		t.Errorf("an account still narrowed to app2 lost its developer grant (%s rows)", n)
	}
	f.renameMetric(t, "gb-narrow-kept", f.metricC, http.StatusOK)
	f.renameMetric(t, "gb-narrow-kept", f.metricD, http.StatusForbidden, http.StatusNotFound)

	// A platform admin's removal is as it was: it may mean to make the
	// account platform-wide.
	pw := f.gbUser(t, "gb-platform-widened", "", [2]string{"developer", ""})
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid)`, pw, f.app2); err != nil {
		t.Fatal(err)
	}
	f.expect(t, "mm-platform", "DELETE", "/api/admin/users/"+pw+"/access/apps/"+f.app2, "", nil, http.StatusOK)
	f.renameMetric(t, "gb-platform-widened", f.metricD, http.StatusOK)
}

// assertNoBuilderReach fails unless the account — a developer with no tenant
// whose last grant a tenant admin just removed — was left with no unscoped
// developer grant, builds nowhere, and its revocation was audited with cause.
func (f *devRouteFixture) assertNoBuilderReach(t *testing.T, sub, id, cause string) {
	t.Helper()
	if n := f.one(t, `SELECT count(*)::text FROM identity.role_assignment WHERE user_id=$1::uuid AND role='developer' AND workspace_id IS NULL`, id); n != "0" {
		t.Errorf("%s keeps %s unscoped developer grants after its last grant went", sub, n)
	}
	if pw := f.one(t, `SELECT `+platformWideBuilderSQL("$1::uuid")+`::text`, id); pw != "false" {
		t.Errorf("%s is platform-wide", sub)
	}
	f.renameMetric(t, sub, f.metricD, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound)
	f.renameMetric(t, sub, f.metricC, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound)
	if n := f.one(t, `SELECT count(*)::text FROM audit.audit_event
		WHERE event_type='user.role_revoked' AND resource_id=$1 AND metadata->>'role'='developer'
		  AND metadata->>'cause'=$2 AND metadata->>'reason' LIKE 'last_grant_removed%'`, id, cause); n != "1" {
		t.Errorf("%s: %s audited revocations with cause %s, want 1", sub, n, cause)
	}
}

// Two removals of an account's last two grants, sent together, never leave
// it platform-wide: the removal and the revocation it may bring run in one
// transaction that locks the account. Checked apart, both saw the other
// grant, and the account was left with none and its developer grant.
func TestConcurrentGrantRemovalsKeepTheLastGrant(t *testing.T) {
	f := setupDevRouteFixture(t)
	ctx := context.Background()
	ids := make([]string, 20)
	for i := range ids {
		ids[i] = f.gbUser(t, fmt.Sprintf("gb-race-%02d", i), "", [2]string{"developer", ""})
		for _, app := range []string{f.app1, f.app2} {
			if _, err := f.pool.Exec(ctx, `INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid)`, ids[i], app); err != nil {
				t.Fatal(err)
			}
		}
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		for _, app := range []string{f.app1, f.app2} {
			wg.Add(1)
			go func(path string) {
				defer wg.Done()
				req, err := http.NewRequestWithContext(ctx, http.MethodDelete, f.srv.URL+path, nil)
				if err != nil {
					return
				}
				req.Header.Set("X-Dev-User", "mm-tenant-admin")
				if resp, err := http.DefaultClient.Do(req); err == nil {
					_ = resp.Body.Close()
				}
			}("/api/admin/users/" + id + "/access/apps/" + app)
		}
	}
	wg.Wait()
	// Both removals go ahead (decided 2026-09-30); whichever runs second
	// sees the first's, and takes the developer grant with the last one.
	if n := f.one(t, `SELECT count(*)::text FROM unnest($1::uuid[]) AS acc(id) WHERE `+platformWideBuilderSQL("acc.id"), ids); n != "0" {
		t.Errorf("%s of %d accounts were left platform-wide by two concurrent removals", n, len(ids))
	}
	if n := f.one(t, `SELECT count(*)::text FROM unnest($1::uuid[]) AS acc(id)
		WHERE EXISTS (SELECT 1 FROM identity.user_app_access ua WHERE ua.user_id = acc.id)
		   OR EXISTS (SELECT 1 FROM identity.role_assignment ra WHERE ra.user_id = acc.id)`, ids); n != "0" {
		t.Errorf("%s of %d accounts kept a grant or a role after both removals", n, len(ids))
	}
}

// The grant door across tenants, and the accounts a tenant admin may not
// take over — another tenant's member, a platform-wide builder, a platform
// admin — by granting, by creating a user over the address, or by narrowing.
func TestTenantAdminCannotReachBeyondItsTenant(t *testing.T) {
	f := setupDevRouteFixture(t)
	ctx := context.Background()
	const ta = "mm-tenant-admin"

	// An account of tenant 2 that is a member of tenant 1 through a
	// workspace role: unscoped developer would make it a builder of tenant
	// 2, whose administrators granted nothing.
	xt := f.gbUser(t, "gb-xt", f.cust2, [2]string{"business_user", f.ws1a})
	raw := f.expect(t, ta, "POST", "/api/admin/users/"+xt+"/roles", "", map[string]string{"role": "developer"}, http.StatusForbidden)
	if !strings.Contains(string(raw), "another tenant") {
		t.Errorf("refusal does not say the account belongs to another tenant: %s", raw)
	}
	f.renameMetric(t, "gb-xt", f.metricD, http.StatusForbidden, http.StatusNotFound)
	// Developer inside a workspace of tenant 1 is the tenant admin's to give.
	f.expect(t, ta, "POST", "/api/admin/users/"+xt+"/roles", "", map[string]string{"role": "developer", "workspace_id": f.ws1a}, http.StatusOK)
	f.renameMetric(t, "gb-xt", f.metricD, http.StatusForbidden, http.StatusNotFound)

	// Creating a user over an existing address used to adopt the account,
	// and was then refused (409). Now it adds the role to the account inside
	// a workspace of the caller's tenant, and changes nothing else — with the
	// answer a new invitation gets; a platform admin's or platform-wide
	// builder's account gets nothing (decided 2026-09-30).
	pw := f.gbUser(t, "gb-pw", "", [2]string{"developer", ""}, [2]string{"business_user", f.ws1a})
	pa := f.gbUser(t, "gb-pa", "", [2]string{"platform_admin", ""}, [2]string{"business_user", f.ws1a})
	for _, c := range []struct {
		email, role, ws string
		want            int
	}{
		// An invitation names a workspace, or is refused before the address
		// is looked at (decided 2026-09-30): nothing granted.
		{"gb-xt@gb.test", "developer", "", http.StatusBadRequest},
		{"GB-XT@gb.test", "business_user", f.ws1b, http.StatusOK},
		{"gb-pw@gb.test", "business_user", f.ws1b, http.StatusOK},
		{"gb-pa@gb.test", "business_user", f.ws1b, http.StatusOK},
	} {
		body := map[string]string{"email": c.email, "first_name": "Taken", "last_name": "Over", "role": c.role}
		if c.ws != "" {
			body["workspace_id"] = c.ws
		}
		f.expect(t, ta, "POST", "/api/admin/users/", "", body, c.want)
	}
	for id, want := range map[string]string{
		xt: "gb-xt|" + f.cust2 + "|business_user@" + f.ws1a + ",business_user@" + f.ws1b + ",developer@" + f.ws1a,
		pw: "gb-pw|-|business_user@" + f.ws1a + ",developer@",
		pa: "gb-pa|-|business_user@" + f.ws1a + ",platform_admin@",
	} {
		got := f.one(t, `SELECT u.display_name||'|'||COALESCE(u.customer_id::text,'-')||'|'||
			(SELECT string_agg(ra.role::text||'@'||COALESCE(ra.workspace_id::text,''), ',' ORDER BY ra.role::text, ra.workspace_id = $2::uuid)
			 FROM identity.role_assignment ra WHERE ra.user_id = u.id)
			FROM identity.user u WHERE u.id = $1::uuid`, id, f.ws1b)
		if got != want {
			t.Errorf("account %s after the invitations: %q, want %q", id, got, want)
		}
	}
	f.renameMetric(t, "gb-xt", f.metricD, http.StatusForbidden, http.StatusNotFound)
	// The identity-provider subject this creation resolves to (in dev mode
	// the synthetic admin-created-<email>) names an account of tenant 2 that
	// has no role in tenant 1 at all: adopted, it became builder of tenant 2.
	victim := f.one(t, `INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ('admin-created-victim@t2.test', 'victim@t2.test', 'Vic Tim', $1::uuid) RETURNING id::text`, f.cust2)
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid)`, victim, f.ws2); err != nil {
		t.Fatal(err)
	}
	f.expect(t, ta, "POST", "/api/admin/users/", "", map[string]string{"email": "victim@t2.test", "first_name": "Taken", "last_name": "Over", "role": "developer"}, http.StatusBadRequest)
	f.expect(t, ta, "POST", "/api/admin/users/", "", map[string]string{"email": "victim@t2.test", "first_name": "Taken", "last_name": "Over",
		"role": "developer", "workspace_id": f.ws1a}, http.StatusOK)
	f.renameMetric(t, "admin-created-victim@t2.test", f.metricD, http.StatusForbidden, http.StatusNotFound)
	if got := f.one(t, `SELECT display_name||'|'||customer_id::text||'|'||(SELECT string_agg(role::text||'@'||workspace_id::text, ',' ORDER BY role::text)
		FROM identity.role_assignment WHERE user_id=$1::uuid) FROM identity.user WHERE id=$1::uuid`, victim); got != "Vic Tim|"+f.cust2+"|business_user@"+f.ws2+",developer@"+f.ws1a {
		t.Errorf("tenant 2's account after the invitation: %q", got)
	}

	// A platform-wide builder is a platform admin's to modify, as a
	// platform admin is: not renamed, deleted or narrowed by a tenant admin
	// whose member it also is.
	f.expect(t, ta, "PATCH", "/api/admin/users/"+pw, "", map[string]string{"display_name": "Renamed"}, http.StatusForbidden)
	f.expect(t, ta, "POST", "/api/admin/users/"+pw+"/access/apps/"+f.app1, "", nil, http.StatusForbidden)
	f.expect(t, ta, "POST", "/api/admin/users/"+pw+"/access/models/"+f.modelA, "", nil, http.StatusForbidden)
	f.expect(t, ta, "DELETE", "/api/admin/users/"+pw, "", nil, http.StatusForbidden)
	f.renameMetric(t, "gb-pw", f.metricD, http.StatusOK)
	f.expect(t, "mm-platform", "POST", "/api/admin/users/"+pw+"/access/apps/"+f.app1, "", nil, http.StatusOK)
	f.renameMetric(t, "gb-pw", f.metricD, http.StatusForbidden, http.StatusNotFound)

	// A developer with no tenant narrowed to app1 (tenant 1) that holds a
	// plain business_user role in tenant 2 administers neither: its grant is
	// its reach, exactly app1, and no administration of tenant 1's people
	// (decided 2026-09-30); it used to administer tenant 1.
	nx := f.gbUser(t, "gb-nx", "", [2]string{"developer", ""}, [2]string{"business_user", f.ws2})
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid)`, nx, f.app1); err != nil {
		t.Fatal(err)
	}
	var listed []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(f.expect(t, "gb-nx", "GET", "/api/admin/users", "", nil, http.StatusOK), &listed); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(listed, func(u struct {
		ID string `json:"id"`
	}) bool {
		return u.ID == f.ba2
	}) {
		t.Errorf("the users list shows tenant 2's business admin")
	}
	f.expect(t, "gb-nx", "POST", "/api/admin/users/"+nx+"/roles", "", map[string]string{"role": "business_admin", "workspace_id": f.ws2}, http.StatusForbidden)
	f.expect(t, "gb-nx", "POST", "/api/admin/users/"+f.ba2+"/roles", "", map[string]string{"role": "business_user", "workspace_id": f.ws2}, http.StatusForbidden)
	f.expect(t, "gb-nx", "POST", "/api/admin/users/", "", map[string]string{"email": "nx-made@gb.test", "first_name": "Nx", "last_name": "Made",
		"role": "business_admin", "workspace_id": f.ws2}, http.StatusForbidden)
	f.expect(t, "gb-nx", "DELETE", "/api/admin/users/"+f.ba2, "", nil, http.StatusForbidden)
	if n := f.one(t, `SELECT count(*)::text FROM identity.user WHERE id=$1::uuid`, f.ba2); n != "1" {
		t.Errorf("tenant 2's business admin was deleted")
	}
	f.expect(t, "gb-nx", "POST", "/api/admin/users/"+nx+"/roles", "", map[string]string{"role": "business_admin", "workspace_id": f.ws1a}, http.StatusForbidden)
	if len(listed) != 0 {
		t.Errorf("the users list of a developer narrowed to app1 lists %d accounts, want none", len(listed))
	}
	f.renameMetric(t, "gb-nx", f.metricA, http.StatusOK)
}

// Workflow assignment draws the same line: a narrowed developer with no
// tenant is an admin-role assignee in the tenant of its grants only.
func TestNarrowedCustomerlessDeveloperWorkflowScope(t *testing.T) {
	f := assigneetest.New(t)
	ctx := context.Background()
	var uid string
	if err := f.Pool.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name)
		VALUES ('asg-dev_narrow', 'dev_narrow@example.test', 'dev_narrow') RETURNING id::text`).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	for _, st := range []struct {
		sql string
		arg string
	}{
		{`INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'developer')`, ""},
		{`INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid)`, f.App1},
		// a plain role in App3's tenant is no admin scope there
		{`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid)`, f.WS2},
	} {
		args := []any{uid}
		if st.arg != "" {
			args = append(args, st.arg)
		}
		if _, err := f.Pool.Exec(ctx, st.sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	store := workflow.NewStore(f.Pool)
	for app, want := range map[string]bool{f.App1: true, f.App3: false} {
		ok, err := store.IsAssigneeEligible(ctx, f.Step(t, app, "developer"), uid)
		if err != nil {
			t.Fatal(err)
		}
		if ok != want {
			t.Errorf("assignee of a developer step of %s: %v, want %v", app, ok, want)
		}
	}
}

// First sign-in through a tenant's own identity provider adopts an account
// that already exists under the address: re-points it to the new login and
// gives it the default role. Only the tenant's own accounts are its to take.
// An account of another tenant, or of none, kept its customer_id, so with no
// workspace to put the role in, unscoped developer made it builder of that
// tenant — or, with none, of every tenant — and the whole account went to
// whoever the tenant's identity provider said owned the address.
func TestSSOFirstLoginMakesNoPlatformWideGrant(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	cust := q(`INSERT INTO core.customer (name, plan) VALUES ('No Workspace Co', 'enterprise') RETURNING id::text`)
	other := q(`INSERT INTO core.customer (name, plan) VALUES ('Other Co', 'enterprise') RETURNING id::text`)
	q(`INSERT INTO identity.sso_provider (customer_id, alias, protocol, display_name, metadata_url, client_id, allowed_domains, jit_provisioning, default_role, enabled)
	   VALUES ($1::uuid, $2, 'oidc', 'IdP', 'https://idp.test/x', 'mvx', ARRAY['nows.test'], TRUE, 'developer', TRUE) RETURNING alias`, cust, sso.Alias(cust))
	account := func(email, customer string, roles ...string) string {
		t.Helper()
		id := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('old-'||$1, $1, $1, NULLIF($2,'')::uuid) RETURNING id::text`, email, customer)
		for _, r := range roles {
			q(`INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, $2::identity.user_role) RETURNING role::text`, id, r)
		}
		return id
	}
	refused := map[string]string{
		"held@nows.test":  account("held@nows.test", ""),                      // no tenant
		"b@nows.test":     account("b@nows.test", other),                      // another tenant
		"admin@nows.test": account("admin@nows.test", cust, "platform_admin"), // a platform admin
		"pwb@nows.test":   account("pwb@nows.test", "", "developer"),          // a platform-wide builder
	}
	state := func(id string) string {
		t.Helper()
		return q(`SELECT keycloak_sub||'|'||COALESCE(customer_id::text,'-')||'|'||COALESCE((SELECT string_agg(role::text||'@'||COALESCE(workspace_id::text,''), ','
			ORDER BY role::text) FROM identity.role_assignment WHERE user_id = u.id), '') FROM identity.user u WHERE id=$1::uuid`, id)
	}
	before := map[string]string{}
	for _, id := range refused {
		before[id] = state(id)
	}
	check := func(when string) {
		t.Helper()
		for email, id := range refused {
			_, err := sso.ProvisionFirstLogin(ctx, pool, cust, "brokered-"+email, email, "Held")
			var np *sso.ErrNotProvisionable
			if !errors.As(err, &np) {
				t.Errorf("%s: first login onto %s: %v, want refused", when, email, err)
			}
			if got := state(id); got != before[id] {
				t.Errorf("%s: refused login changed %s from %q to %q", when, email, before[id], got)
			}
		}
	}
	check("no workspace")

	// The tenant's own account is adopted: re-pointed, and given the role —
	// with no workspace, the tenant's developer.
	own := account("own@nows.test", cust)
	if id, err := sso.ProvisionFirstLogin(ctx, pool, cust, "brokered-own", "own@nows.test", "Own"); err != nil || id != own {
		t.Fatalf("first login onto the tenant's own account: %q, %v", id, err)
	}
	if got := q(`SELECT keycloak_sub||'|'||(SELECT string_agg(role::text||'@'||COALESCE(workspace_id::text,''), ',') FROM identity.role_assignment WHERE user_id = u.id)
		FROM identity.user u WHERE id=$1::uuid`, own); got != "brokered-own|developer@" {
		t.Errorf("adopted own account: %q", got)
	}

	// A new address becomes the tenant's own developer: customer_id set.
	id, err := sso.ProvisionFirstLogin(ctx, pool, cust, "new-sub", "new@nows.test", "New")
	if err != nil {
		t.Fatalf("first login of a new address: %v", err)
	}
	if got := q(`SELECT COALESCE(customer_id::text,'') FROM identity.user WHERE id=$1::uuid`, id); got != cust {
		t.Errorf("new account's customer_id = %q, want %s", got, cust)
	}

	// A workspace changes where the role goes, not whose accounts these are.
	ws := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Default') RETURNING id::text`, cust)
	check("with a workspace")
	later, err := sso.ProvisionFirstLogin(ctx, pool, cust, "later-sub", "later@nows.test", "Later")
	if err != nil {
		t.Fatalf("first login with a workspace: %v", err)
	}
	if got := q(`SELECT string_agg(role::text||'@'||COALESCE(workspace_id::text,''), ',') FROM identity.role_assignment WHERE user_id=$1::uuid`, later); got != "developer@"+ws {
		t.Errorf("new account's grants = %q, want developer@%s", got, ws)
	}
}

// SCIM provisioning never makes an unscoped grant: the token's workspace (or
// the tenant's first) takes the default role, and the account belongs to
// the token's tenant. Pinned, since a developer default role is allowed.
func TestSCIMProvisionsNoPlatformWideBuilder(t *testing.T) {
	f := setupSsoFixture(t)
	ctx := context.Background()
	code, body := f.do(t, f.srv, http.MethodPost, "/api/admin/scim/tokens", map[string]any{"name": "IdP", "default_role": "developer"}, nil)
	if code != http.StatusCreated {
		t.Fatalf("issue: %d %s", code, body)
	}
	bearer := map[string]string{"Authorization": "Bearer " + decode(t, body)["token"].(string), "X-Dev-User": ""}
	code, body = f.do(t, f.srv, http.MethodPost, "/api/scim/v2/Users", map[string]any{
		"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:User"}, "userName": "dev@acme.test",
		"name": map[string]string{"givenName": "Dev", "familyName": "Eloper"}, "active": true,
	}, bearer)
	if code != http.StatusCreated {
		t.Fatalf("provision: %d %s", code, body)
	}
	var cust, unscoped string
	if err := f.pool.QueryRow(ctx, `
		SELECT COALESCE(u.customer_id::text,''),
		       (SELECT count(*) FROM identity.role_assignment ra WHERE ra.user_id = u.id AND ra.workspace_id IS NULL)::text
		FROM identity.user u WHERE u.email = 'dev@acme.test'`).Scan(&cust, &unscoped); err != nil {
		t.Fatal(err)
	}
	if cust != f.custID || unscoped != "0" {
		t.Errorf("provisioned account: customer %q, %s unscoped grants; want %s and 0", cust, unscoped, f.custID)
	}
}

// A tenant's SCIM token lists everyone holding a role in its workspaces, but
// changes and deletes only the tenant's own accounts, never a platform
// admin. It rewrote a platform-wide builder's and a platform admin's e-mail
// (a set-password mail then went to the new address) and deleted a
// platform admin's sign-in account.
func TestSCIMChangesOnlyTheTenantsOwnAccounts(t *testing.T) {
	f := setupSsoFixture(t)
	ctx := context.Background()
	code, body := f.do(t, f.srv, http.MethodPost, "/api/admin/scim/tokens", map[string]any{"name": "IdP"}, nil)
	if code != http.StatusCreated {
		t.Fatalf("issue: %d %s", code, body)
	}
	bearer := map[string]string{"Authorization": "Bearer " + decode(t, body)["token"].(string), "X-Dev-User": ""}
	other := ""
	if err := f.pool.QueryRow(ctx, `INSERT INTO core.customer (name, plan) VALUES ('Other', 'enterprise') RETURNING id::text`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	member := func(sub, customer string, roles ...string) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
			VALUES ($1, $1||'@acme.test', 'Held', NULLIF($2,'')::uuid) RETURNING id::text`, sub, customer).Scan(&id); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid)`, id, f.wsID); err != nil {
			t.Fatal(err)
		}
		for _, r := range roles {
			if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, $2::identity.user_role)`, id, r); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	put := func(sub, userName, display string, active bool) map[string]any {
		return map[string]any{"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:User"}, "userName": userName, "displayName": display, "active": active}
	}
	for sub, id := range map[string]string{
		"kc-pw":    member("kc-pw", "", "developer"),            // platform-wide builder
		"kc-pa":    member("kc-pa", f.custID, "platform_admin"), // platform admin, even of this tenant
		"kc-other": member("kc-other", other),                   // another tenant's
		"kc-none":  member("kc-none", ""),                       // no tenant
	} {
		email := sub + "@acme.test"
		if code, body := f.do(t, f.srv, http.MethodGet, "/api/scim/v2/Users/"+id, nil, bearer); code != http.StatusOK {
			t.Errorf("GET %s: %d %s, want it listed", sub, code, body)
		}
		// What changes nothing is answered as it stands.
		if code, body := f.do(t, f.srv, http.MethodPut, "/api/scim/v2/Users/"+id, put(sub, email, "Held", true), bearer); code != http.StatusOK {
			t.Errorf("unchanged PUT %s: %d %s", sub, code, body)
		}
		for what, in := range map[string]map[string]any{
			"e-mail": put(sub, "attacker-"+sub+"@evil.test", "Held", true),
			"name":   put(sub, email, "Renamed", true),
			"active": put(sub, email, "Held", false),
		} {
			if code, body := f.do(t, f.srv, http.MethodPut, "/api/scim/v2/Users/"+id, in, bearer); code != http.StatusForbidden {
				t.Errorf("PUT %s of %s: %d %s, want 403", what, sub, code, body)
			}
		}
		if code, body := f.do(t, f.srv, http.MethodPatch, "/api/scim/v2/Users/"+id, map[string]any{
			"schemas":    []string{"urn:ietf:params:scim:api:messages:2.0:PatchOp"},
			"Operations": []map[string]any{{"op": "replace", "path": "userName", "value": "attacker-" + sub + "@evil.test"}},
		}, bearer); code != http.StatusForbidden {
			t.Errorf("PATCH e-mail of %s: %d %s, want 403", sub, code, body)
		}
		// A delete takes a member's place in the tenant (Remove from this
		// tenant) and leaves the account; a platform account is refused.
		wantDelete := http.StatusNoContent
		if sub == "kc-pw" || sub == "kc-pa" {
			wantDelete = http.StatusForbidden
		}
		if code, body := f.do(t, f.srv, http.MethodDelete, "/api/scim/v2/Users/"+id, nil, bearer); code != wantDelete {
			t.Errorf("DELETE %s: %d %s, want %d", sub, code, body, wantDelete)
		}
		var roles int
		_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM identity.role_assignment WHERE user_id=$1::uuid AND workspace_id=$2::uuid`, id, f.wsID).Scan(&roles)
		if wantDelete == http.StatusNoContent && roles != 0 {
			t.Errorf("%s keeps %d role(s) in the tenant after the delete", sub, roles)
		}
		if wantDelete == http.StatusForbidden && roles != 1 {
			t.Errorf("%s lost its role in the tenant to a refused delete", sub)
		}
		var got string
		if err := f.pool.QueryRow(ctx, `SELECT email||'|'||display_name||'|'||(disabled_at IS NULL)::text FROM identity.user WHERE id=$1::uuid`, id).Scan(&got); err != nil {
			t.Errorf("%s: %v", sub, err)
		} else if got != email+"|Held|true" {
			t.Errorf("%s after the refused writes: %q", sub, got)
		}
	}

	// The tenant's own account is the directory's to change.
	code, body = f.do(t, f.srv, http.MethodPost, "/api/scim/v2/Users", put("", "own@acme.test", "Own", true), bearer)
	if code != http.StatusCreated {
		t.Fatalf("provision: %d %s", code, body)
	}
	own := decode(t, body)["id"].(string)
	if code, body := f.do(t, f.srv, http.MethodPut, "/api/scim/v2/Users/"+own, put("", "own2@acme.test", "Own Two", true), bearer); code != http.StatusOK {
		t.Errorf("PUT own account: %d %s", code, body)
	}
	if code, body := f.do(t, f.srv, http.MethodDelete, "/api/scim/v2/Users/"+own, nil, bearer); code != http.StatusNoContent {
		t.Errorf("DELETE own account: %d %s", code, body)
	}
}

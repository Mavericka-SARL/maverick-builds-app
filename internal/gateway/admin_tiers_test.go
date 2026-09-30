package gateway

// Administration counts per tenant (decided 2026-09-30, admin_tiers.go): the
// caller's tier in a tenant is decided by the roles it holds there; an
// application or model grant opens exactly that application or model and
// administers nothing; an invitation names a role and a workspace; and an
// account a platform admin invites into a workspace belongs to that
// workspace's tenant.
//
// These reuse setupDevRouteFixture: tenant 1 has ws1a (app1, models A and
// B), ws1b (app2) and appT; tenant 2 has ws2 (app3, model D).
// mm-tenant-admin is tenant 1's admin, mm-platform a platform admin,
// dr-devws-ta2 an account of tenant 1 that is a developer of ws1a and
// tenant_admin of ws2, ws-bu1b a business user of ws1b.

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// settingsStatus is GET /api/notifications/settings as sub for the tenant
// X-Tenant-Id names.
func (f *devRouteFixture) settingsStatus(t *testing.T, sub, customerID string) int {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, f.srv.URL+"/api/notifications/settings", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Dev-User", sub)
	req.Header.Set(tenantHeader, customerID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

// listedIDs is the "id" of each object of a JSON array.
func listedIDs(t *testing.T, raw []byte) []string {
	t.Helper()
	var items []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	out := []string{}
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

// The live case: tenant 1's admin adds tenant 2's administrator to tenant 1
// as a business user and grants it app1. That made it an administrator of
// tenant 1: it listed tenant 1's members and renamed one (2026-09-30), and
// with tenant_admin as its tier granted developer there and deleted a
// developer.
func TestApplicationGrantGivesNoAdministration(t *testing.T) {
	f := setupDevRouteFixture(t)
	const ta, t2 = "mm-tenant-admin", "ab-t2-ta"
	t2ID := f.gbUser(t, t2, f.cust2, [2]string{"tenant_admin", ""}, [2]string{"business_user", f.ws2})
	dev1 := f.gbUser(t, "at-dev1", f.cust1, [2]string{"developer", f.ws1b})
	f.expect(t, ta, "POST", "/api/admin/users", "", map[string]string{"email": t2 + "@gb.test", "first_name": "T", "last_name": "Two",
		"role": "business_user", "workspace_id": f.ws1b}, http.StatusOK)
	f.expect(t, ta, "POST", "/api/admin/users/"+t2ID+"/access/apps/"+f.app1, "", nil, http.StatusOK)

	before := f.accountRow(t, f.user1b)
	if _, listed := f.listUsers(t, t2)[f.user1b]; listed {
		t.Errorf("tenant 2's admin lists tenant 1's business user")
	}
	f.expect(t, t2, "PATCH", "/api/admin/users/"+f.user1b, "", map[string]string{"display_name": "Renamed from tenant 2"}, http.StatusForbidden)
	f.expect(t, t2, "POST", "/api/admin/users/"+f.user1b+"/roles", "", map[string]string{"role": "developer", "workspace_id": f.ws1b}, http.StatusForbidden)
	f.expect(t, t2, "DELETE", "/api/admin/users/"+dev1, "", nil, http.StatusForbidden)
	f.expect(t, t2, "POST", "/api/admin/users/"+f.user1b+"/access/apps/"+f.app2, "", nil, http.StatusForbidden)
	if code := f.settingsStatus(t, t2, f.cust1); code != http.StatusForbidden {
		t.Errorf("tenant 2's admin reads tenant 1's notification settings: %d", code)
	}
	if got := f.accountRow(t, f.user1b); got != before {
		t.Errorf("tenant 1's business user changed: %q → %q", before, got)
	}
	if n := f.one(t, `SELECT count(*)::text FROM identity.role_assignment WHERE user_id=$1::uuid`, f.user1b); n != "1" {
		t.Errorf("tenant 1's business user holds %s roles, want its 1", n)
	}
	if n := f.one(t, `SELECT count(*)::text FROM identity.user WHERE id=$1::uuid`, dev1); n != "1" {
		t.Errorf("tenant 1's developer was deleted")
	}

	// Its own tenant it still administers.
	f.expect(t, t2, "PATCH", "/api/admin/users/"+f.ba2, "", map[string]string{"display_name": "Business Admin Two"}, http.StatusOK)
	if code := f.settingsStatus(t, t2, f.cust2); code != http.StatusOK {
		t.Errorf("tenant 2's admin reads its own notification settings: %d", code)
	}
}

// An account with no tenant holding tenant_admin — a platform admin's
// creation — narrowed to app1: its grant is its reach, exactly app1, and no
// administration of tenant 1's people. It used to administer all of tenant 1.
func TestCustomerlessAdminGrantIsReachNotAdministration(t *testing.T) {
	f := setupDevRouteFixture(t)
	const sub = "at-cless-ta"
	id := f.gbUser(t, sub, "", [2]string{"tenant_admin", ""})
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid)`, id, f.app1); err != nil {
		t.Fatal(err)
	}

	users := f.listUsers(t, sub)
	if _, listed := users[f.user1b]; listed {
		t.Errorf("it lists tenant 1's business user")
	}
	f.expect(t, sub, "PATCH", "/api/admin/users/"+f.user1b, "", map[string]string{"display_name": "Renamed"}, http.StatusForbidden)
	f.expect(t, sub, "POST", "/api/admin/users/"+f.user1b+"/access/apps/"+f.app1, "", nil, http.StatusForbidden)
	if code := f.settingsStatus(t, sub, f.cust1); code != http.StatusForbidden {
		t.Errorf("it reads tenant 1's notification settings: %d", code)
	}

	// Reach: app1, and nothing else of tenant 1.
	name := func(app string) map[string]string {
		return map[string]string{"name": f.one(t, `SELECT name FROM core.application WHERE id=$1::uuid`, app)}
	}
	f.expect(t, sub, "PATCH", "/api/admin/applications/"+f.app1, "", name(f.app1), http.StatusOK)
	f.expect(t, sub, "PATCH", "/api/admin/applications/"+f.app2, "", name(f.app2), http.StatusForbidden)
	f.expect(t, sub, "PATCH", "/api/admin/applications/"+f.appT, "", name(f.appT), http.StatusForbidden)
	for _, path := range []string{"/api/admin/applications", "/api/apps"} {
		got := listedIDs(t, f.expect(t, sub, "GET", path, "", nil, http.StatusOK))
		if !slices.Contains(got, f.app1) || slices.Contains(got, f.app2) || slices.Contains(got, f.appT) || slices.Contains(got, f.app3) {
			t.Errorf("GET %s lists %v, want app1 %s and no other", path, got, f.app1)
		}
	}
	// The administrator's tree: tenant 1, with app1 alone.
	var tree []struct {
		ID           string `json:"id"`
		Applications []struct {
			ID string `json:"id"`
		} `json:"applications"`
	}
	if err := json.Unmarshal(f.expect(t, sub, "GET", "/api/admin/tenants", "", nil, http.StatusOK), &tree); err != nil {
		t.Fatal(err)
	}
	if len(tree) != 1 || tree[0].ID != f.cust1 || len(tree[0].Applications) != 1 || tree[0].Applications[0].ID != f.app1 {
		t.Errorf("GET /api/admin/tenants: %+v, want tenant 1 with app1 %s alone", tree, f.app1)
	}
}

// dr-devws-ta2 is a developer in tenant 1 and tenant_admin in tenant 2. Its
// tier in each is its own: in tenant 1 it did what tenant 1's administrator
// does — renamed its applications, granted application access and
// developer, renamed and deleted its developers, read its settings and audit.
// And tenant 1's administrator, given developer in a workspace of tenant 2,
// did the same there.
func TestRolesCountPerTenant(t *testing.T) {
	f := setupDevRouteFixture(t)
	ctx := context.Background()
	const cross, ta = "dr-devws-ta2", "mm-tenant-admin"
	bu1 := f.gbUser(t, "at-bu1", f.cust1, [2]string{"business_user", f.ws1a})
	dev1 := f.gbUser(t, "at-dev1", f.cust1, [2]string{"developer", f.ws1b})
	bu2 := f.gbUser(t, "at-bu2", f.cust2, [2]string{"business_user", f.ws2})
	appName := func(app string) map[string]string {
		return map[string]string{"name": f.one(t, `SELECT name FROM core.application WHERE id=$1::uuid`, app)}
	}

	// In tenant 1 it is a developer.
	f.expect(t, cross, "PATCH", "/api/admin/applications/"+f.app1, "", appName(f.app1), http.StatusForbidden)
	f.expect(t, cross, "POST", "/api/admin/users/"+bu1+"/access/apps/"+f.app1, "", nil, http.StatusForbidden)
	f.expect(t, cross, "POST", "/api/admin/users/"+bu1+"/access/models/"+f.modelA, "", nil, http.StatusForbidden)
	f.expect(t, cross, "POST", "/api/admin/users/"+bu1+"/roles", "", map[string]string{"role": "developer", "workspace_id": f.ws1b}, http.StatusForbidden)
	f.expect(t, cross, "PATCH", "/api/admin/users/"+dev1, "", map[string]string{"display_name": "Renamed"}, http.StatusForbidden)
	f.expect(t, cross, "DELETE", "/api/admin/users/"+dev1, "", nil, http.StatusForbidden)
	if code := f.settingsStatus(t, cross, f.cust1); code != http.StatusForbidden {
		t.Errorf("tenant 1's notification settings as its developer: %d", code)
	}
	if got := listedIDs(t, f.expect(t, cross, "GET", "/api/admin/applications", "", nil, http.StatusOK)); slices.Contains(got, f.app1) {
		t.Errorf("the administrator's applications list of a developer of tenant 1 shows app1: %v", got)
	}
	if n := f.one(t, `SELECT count(*)::text FROM identity.user_app_access WHERE user_id=$1::uuid`, bu1); n != "0" {
		t.Errorf("%s application grants given in tenant 1 by its developer", n)
	}
	// What a developer does there it still does.
	f.expect(t, cross, "PATCH", "/api/admin/users/"+bu1, "", map[string]string{"display_name": "Renamed by a developer"}, http.StatusOK)
	f.expect(t, cross, "POST", "/api/admin/users/"+bu1+"/roles", "", map[string]string{"role": "business_admin", "workspace_id": f.ws1b}, http.StatusOK)
	f.renameMetric(t, cross, f.metricA, http.StatusOK)
	// Tenant 1's events are its administrator's to read.
	if auditedFor(t, f, cross, bu1) {
		t.Errorf("a developer of tenant 1 reads tenant 1's audit")
	}
	if !auditedFor(t, f, ta, bu1) {
		t.Errorf("tenant 1's administrator does not read its audit")
	}

	// In tenant 2 it is tenant admin.
	f.expect(t, cross, "POST", "/api/admin/users/"+bu2+"/roles", "", map[string]string{"role": "developer", "workspace_id": f.ws2}, http.StatusOK)
	f.expect(t, cross, "POST", "/api/admin/users/"+bu2+"/access/apps/"+f.app3, "", nil, http.StatusOK)
	f.expect(t, cross, "PATCH", "/api/admin/applications/"+f.app3, "", appName(f.app3), http.StatusOK)
	if code := f.settingsStatus(t, cross, f.cust2); code != http.StatusOK {
		t.Errorf("tenant 2's notification settings as its admin: %d", code)
	}

	// The other way round: tenant 1's administrator, a developer of ws2.
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		SELECT id, 'developer', $1::uuid FROM identity.user WHERE keycloak_sub = $2`, f.ws2, ta); err != nil {
		t.Fatal(err)
	}
	bu2b := f.gbUser(t, "at-bu2b", f.cust2, [2]string{"business_user", f.ws2})
	dev2 := f.gbUser(t, "at-dev2", f.cust2, [2]string{"developer", f.ws2})
	f.expect(t, ta, "POST", "/api/admin/users/"+bu2b+"/access/apps/"+f.app3, "", nil, http.StatusForbidden)
	f.expect(t, ta, "POST", "/api/admin/users/"+bu2b+"/roles", "", map[string]string{"role": "developer", "workspace_id": f.ws2}, http.StatusForbidden)
	f.expect(t, ta, "PATCH", "/api/admin/users/"+dev2, "", map[string]string{"display_name": "Renamed"}, http.StatusForbidden)
	f.expect(t, ta, "PATCH", "/api/admin/applications/"+f.app3, "", appName(f.app3), http.StatusForbidden)
	if code := f.settingsStatus(t, ta, f.cust2); code != http.StatusForbidden {
		t.Errorf("tenant 2's notification settings as tenant 1's admin and tenant 2's developer: %d", code)
	}
	// As a developer of tenant 2 it renames a business user there, and in
	// its own tenant it is administrator still.
	f.expect(t, ta, "PATCH", "/api/admin/users/"+bu2b, "", map[string]string{"display_name": "Renamed by a developer"}, http.StatusOK)
	f.expect(t, ta, "POST", "/api/admin/users/"+bu1+"/roles", "", map[string]string{"role": "developer", "workspace_id": f.ws1b}, http.StatusOK)

	// An account of tenant 2 added to tenant 1: tenant 1's administrator
	// removes it from tenant 1, though it is a developer of tenant 2 — and
	// what it holds in tenant 2 stays.
	visitor := f.gbUser(t, "at-visitor", f.cust2, [2]string{"business_user", f.ws2}, [2]string{"business_user", f.ws1a})
	if got := f.listUsers(t, ta)[visitor]; !got.Permissions.RemoveFromTenant {
		t.Errorf("tenant 1's admin is not offered to remove tenant 2's visitor from tenant 1: %+v", got)
	}
	f.expect(t, ta, "DELETE", "/api/admin/users/"+visitor+"/tenant-access", "", nil, http.StatusOK)
	if got := f.one(t, `SELECT string_agg(workspace_id::text, ',') FROM identity.role_assignment WHERE user_id=$1::uuid`, visitor); got != f.ws2 {
		t.Errorf("the visitor's roles after its removal from tenant 1: %s, want its ws2 role", got)
	}
	// Tenant 2's own account it is not offered to remove from tenant 1:
	// it holds nothing there.
	if got := f.listUsers(t, ta)[bu2b]; got.Permissions.RemoveFromTenant {
		t.Errorf("offered to remove a tenant-2 account that holds nothing in tenant 1: %+v", got)
	}
}

// Anyone but a platform admin invites with a role inside a workspace; the
// refusal comes before the address is looked at, so it is the same for an
// existing account and a new address. It used to be answered as done, and
// grant nothing to an existing account.
func TestInviteNeedsRoleAndWorkspace(t *testing.T) {
	f := setupDevRouteFixture(t)
	existing := f.one(t, `SELECT email FROM identity.user WHERE id=$1::uuid`, f.user1b)
	for _, sub := range []string{"mm-tenant-admin", "ws-dev-ba"} {
		for i, c := range [][2]string{{"business_user", ""}, {"", f.ws1b}, {"", ""}} {
			var answers []string
			for _, email := range []string{existing, strings.ToUpper(existing), "at-new-" + sub + "-" + string(rune('a'+i)) + "@gb.test"} {
				body := map[string]string{"email": email, "first_name": "Some", "last_name": "One", "role": c[0], "workspace_id": c[1]}
				answers = append(answers, string(f.expect(t, sub, "POST", "/api/admin/users", "", body, http.StatusBadRequest)))
			}
			if answers[0] != answers[2] || answers[1] != answers[2] {
				t.Errorf("%s, role %q, workspace %q: existing address answered %q, a new one %q", sub, c[0], c[1], answers[0], answers[2])
			}
		}
	}
	if n := f.one(t, `SELECT count(*)::text FROM identity.user WHERE email LIKE 'at-new-%'`); n != "0" {
		t.Errorf("%s accounts created by refused invitations", n)
	}
	if n := f.one(t, `SELECT count(*)::text FROM audit.audit_event WHERE event_type='user.role_granted' AND resource_id=$1`, f.user1b); n != "0" {
		t.Errorf("%s grant events about the existing account", n)
	}
	// A platform admin still creates an account with no role and no workspace.
	f.expect(t, "mm-platform", "POST", "/api/admin/users", "", map[string]string{"email": "at-platform-bare@gb.test", "first_name": "Bare", "last_name": "One"}, http.StatusOK)
}

// An account a platform admin invites into a workspace belongs to that
// workspace's tenant; one invited without a workspace to none. It used to
// belong to none either way, so its tenant's administrators could not rename
// it, and its tenant's single sign-on refused it.
func TestPlatformAdminInviteBelongsToTheWorkspacesTenant(t *testing.T) {
	f := setupDevRouteFixture(t)
	create := func(email, role, ws string) string {
		t.Helper()
		body := map[string]string{"email": email, "first_name": "Made", "last_name": "ByPlatform", "role": role}
		if ws != "" {
			body["workspace_id"] = ws
		}
		var out map[string]any
		if err := json.Unmarshal(f.expect(t, "mm-platform", "POST", "/api/admin/users", "", body, http.StatusOK), &out); err != nil {
			t.Fatal(err)
		}
		id, _ := out["id"].(string)
		return id
	}
	inWS := create("at-pa-ws2@gb.test", "business_user", f.ws2)
	bare := create("at-pa-bare@gb.test", "", "")
	if got := f.one(t, `SELECT COALESCE(customer_id::text, '-') FROM identity.user WHERE id=$1::uuid`, inWS); got != f.cust2 {
		t.Errorf("invited into ws2: tenant %s, want tenant 2's %s", got, f.cust2)
	}
	if got := f.one(t, `SELECT COALESCE(customer_id::text, '-') FROM identity.user WHERE id=$1::uuid`, bare); got != "-" {
		t.Errorf("invited with no workspace: tenant %s, want none", got)
	}
	if got := f.one(t, `SELECT COALESCE(string_agg(metadata->>'customer_id', ','), '-') FROM audit.audit_event
		WHERE event_type='user.created' AND resource_id=$1`, inWS); got != f.cust2 {
		t.Errorf("the creation's audit event records tenant %q, want %s", got, f.cust2)
	}
	// Tenant 2's administrator now administers it; tenant 1's does not.
	f.gbUser(t, "at-t2-admin", f.cust2, [2]string{"tenant_admin", ""})
	f.expect(t, "at-t2-admin", "PATCH", "/api/admin/users/"+inWS, "", map[string]string{"display_name": "Renamed by its tenant"}, http.StatusOK)
	f.expect(t, "mm-tenant-admin", "PATCH", "/api/admin/users/"+inWS, "", map[string]string{"display_name": "Renamed elsewhere"}, http.StatusForbidden)
	// The platform admin giving a workspace it does not know is told so.
	f.expect(t, "mm-platform", "POST", "/api/admin/users", "", map[string]string{"email": "at-pa-nows@gb.test", "first_name": "No", "last_name": "Ws",
		"role": "business_user", "workspace_id": "00000000-0000-0000-0000-000000000000"}, http.StatusBadRequest)
}

// Renaming a tenant is its administrator's: by the roles held in it. An
// account of tenant 2 holding tenant_admin only in a workspace of tenant 1
// renamed tenant 2, and dr-devws-ta2 — an account of tenant 1 that is only a
// developer there — renamed tenant 1 (2026-09-30): the check was "holds
// tenant_admin anywhere, and belongs to the tenant".
func TestTenantRenameCountsRolesPerTenant(t *testing.T) {
	f := setupDevRouteFixture(t)
	name := func(cust string) string {
		return f.one(t, `SELECT name FROM core.customer WHERE id=$1::uuid`, cust)
	}
	name1, name2 := name(f.cust1), name(f.cust2)
	f.gbUser(t, "at-t2-ta1ws", f.cust2, [2]string{"tenant_admin", f.ws1a})
	f.gbUser(t, "at-t2-ta", f.cust2, [2]string{"tenant_admin", ""})

	f.expect(t, "at-t2-ta1ws", "PATCH", "/api/admin/tenants/"+f.cust2, "", map[string]string{"name": "Renamed by tenant 1's admin"}, http.StatusForbidden)
	f.expect(t, "dr-devws-ta2", "PATCH", "/api/admin/tenants/"+f.cust1, "", map[string]string{"name": "Renamed by a developer"}, http.StatusForbidden)
	if got := name(f.cust1); got != name1 {
		t.Errorf("tenant 1 renamed to %q", got)
	}
	if got := name(f.cust2); got != name2 {
		t.Errorf("tenant 2 renamed to %q", got)
	}
	// Where it is tenant admin, it renames.
	f.expect(t, "at-t2-ta", "PATCH", "/api/admin/tenants/"+f.cust2, "", map[string]string{"name": name2}, http.StatusOK)
	f.expect(t, "at-t2-ta1ws", "PATCH", "/api/admin/tenants/"+f.cust1, "", map[string]string{"name": name1}, http.StatusOK)
	f.expect(t, "dr-devws-ta2", "PATCH", "/api/admin/tenants/"+f.cust2, "", map[string]string{"name": name2}, http.StatusOK)
	f.expect(t, "mm-tenant-admin", "PATCH", "/api/admin/tenants/"+f.cust1, "", map[string]string{"name": name1}, http.StatusOK)
}

// An account with no tenant that holds tenant_admin only inside a workspace
// of tenant 2 — tenant 2's administrator — added to tenant 1 by its
// administrator and granted app1: the grant is no reach on the
// administrator's routes. It renamed app1 and exported model A with its data
// (2026-09-30): customerlessAdminGrantTenantSQL counted a tenant_admin grant
// held anywhere.
func TestWorkspaceTenantAdminGrantIsNoAdministration(t *testing.T) {
	f := setupDevRouteFixture(t)
	const sub = "at-nt-ta-ws2"
	id := f.gbUser(t, sub, "", [2]string{"tenant_admin", f.ws2})
	f.expect(t, "mm-tenant-admin", "POST", "/api/admin/users", "", map[string]string{"email": sub + "@gb.test", "first_name": "Other", "last_name": "Admin",
		"role": "business_user", "workspace_id": f.ws1b}, http.StatusOK)
	f.expect(t, "mm-tenant-admin", "POST", "/api/admin/users/"+id+"/access/apps/"+f.app1, "", nil, http.StatusOK)

	appName := f.one(t, `SELECT name FROM core.application WHERE id=$1::uuid`, f.app1)
	f.expect(t, sub, "PATCH", "/api/admin/applications/"+f.app1, "", map[string]string{"name": "Renamed from tenant 2"}, http.StatusForbidden)
	f.expect(t, sub, "GET", "/api/admin/models/"+f.modelA+"/export", "", nil, http.StatusForbidden)
	f.expect(t, sub, "DELETE", "/api/admin/applications/"+f.app1, "", nil, http.StatusForbidden)
	if got := f.one(t, `SELECT COALESCE((SELECT name FROM core.application WHERE id=$1::uuid), '-')`, f.app1); got != appName {
		t.Errorf("app1 is now %q, want %q", got, appName)
	}
	// Tenant 2 it still administers.
	f.expect(t, sub, "PATCH", "/api/admin/applications/"+f.app3, "", map[string]string{"name": f.one(t, `SELECT name FROM core.application WHERE id=$1::uuid`, f.app3)}, http.StatusOK)
}

// A platform admin inviting an address that already has an account with no
// tenant adopts it as it is: the account does not take the workspace's
// tenant, which only a new account does. It did (2026-09-30): a platform-wide
// builder became tenant 2's developer, a builder narrowed to app1 lost it, an
// administrator with no tenant lost tenant 1, and an account with roles in
// two tenants was handed to one of them.
func TestPlatformAdminInviteKeepsAnExistingAccountsTenant(t *testing.T) {
	f := setupDevRouteFixture(t)
	ctx := context.Background()
	// In dev mode an invitation's subject is admin-created-<address>.
	existing := func(email string, grants ...[2]string) string {
		return f.gbUser(t, "admin-created-"+email, "", grants...)
	}
	pw := existing("at-pw@gb.test", [2]string{"developer", ""})
	nd := existing("at-nd@gb.test", [2]string{"developer", ""})
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid)`, nd, f.app1); err != nil {
		t.Fatal(err)
	}
	cta := existing("at-cta@gb.test", [2]string{"tenant_admin", ""}, [2]string{"business_user", f.ws1a})
	two := existing("at-two@gb.test", [2]string{"business_user", f.ws1a})

	for _, email := range []string{"at-pw@gb.test", "at-nd@gb.test", "at-cta@gb.test", "at-two@gb.test"} {
		f.expect(t, "mm-platform", "POST", "/api/admin/users", "", map[string]string{"email": email, "first_name": "Already", "last_name": "There",
			"role": "business_user", "workspace_id": f.ws2}, http.StatusOK)
	}
	for name, id := range map[string]string{"platform-wide builder": pw, "narrowed builder": nd, "administrator with no tenant": cta, "account of two tenants": two} {
		if got := f.one(t, `SELECT COALESCE(customer_id::text, '-') FROM identity.user WHERE id=$1::uuid`, id); got != "-" {
			t.Errorf("%s: tenant %s after the invitation, want none", name, got)
		}
		if n := f.one(t, `SELECT count(*)::text FROM identity.role_assignment WHERE user_id=$1::uuid AND workspace_id=$2::uuid AND role='business_user'`, id, f.ws2); n != "1" {
			t.Errorf("%s: %s business_user roles in ws2, want the one invited to", name, n)
		}
	}
	if got := f.one(t, `SELECT (`+platformWideBuilderSQL("$1::uuid")+`)::text`, pw); got != "true" {
		t.Errorf("the platform-wide builder is not platform-wide after the invitation")
	}
	if got := f.one(t, `SELECT (`+customerlessGrantTenantSQL("$1::uuid", "$2::uuid")+`)::text`, nd, f.cust1); got != "true" {
		t.Errorf("the builder narrowed to app1 no longer reaches tenant 1")
	}
	// The administrator with no tenant still administers tenant 1.
	f.expect(t, "admin-created-at-cta@gb.test", "PATCH", "/api/admin/users/"+f.user1b, "", map[string]string{"display_name": "Renamed by its administrator"}, http.StatusOK)
	// Tenant 2's administrator is given none of them.
	f.gbUser(t, "at-t2-admin", f.cust2, [2]string{"tenant_admin", ""})
	for _, id := range []string{pw, nd, cta, two} {
		f.expect(t, "at-t2-admin", "DELETE", "/api/admin/users/"+id, "", nil, http.StatusForbidden)
	}
}

// The users screen is told what the caller may do per tenant: the roles it
// may grant in each workspace and whether it manages access to the
// workspace's tenant's applications and models, and the roles it may grant
// each account with no workspace. It worked this out from the caller's roles
// merged across tenants, so tenant 1's administrator, a developer of ws2,
// was offered developer and application access in tenant 2, and each was
// refused (2026-09-30).
func TestUsersScreenIsToldTheTierPerTenant(t *testing.T) {
	f := setupDevRouteFixture(t)
	const ta = "mm-tenant-admin"
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		SELECT id, 'developer', $1::uuid FROM identity.user WHERE keycloak_sub = $2`, f.ws2, ta); err != nil {
		t.Fatal(err)
	}
	bu1 := f.gbUser(t, "at-g-bu1", f.cust1, [2]string{"business_user", f.ws1a})
	bu2 := f.gbUser(t, "at-g-bu2", f.cust2, [2]string{"business_user", f.ws2})

	var wss []struct {
		ID             string   `json:"id"`
		GrantableRoles []string `json:"grantable_roles"`
		ManageAccess   *bool    `json:"manage_access"`
	}
	if err := json.Unmarshal(f.expect(t, ta, "GET", "/api/admin/workspaces", "", nil, http.StatusOK), &wss); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		f.ws1a: {"developer", "business_admin", "business_user"},
		f.ws2:  {"business_admin", "business_user"},
	}
	wantAccess := map[string]bool{f.ws1a: true, f.ws2: false}
	seen := 0
	for _, ws := range wss {
		roles, ok := want[ws.ID]
		if !ok {
			continue
		}
		seen++
		if !slices.Equal(ws.GrantableRoles, roles) {
			t.Errorf("workspace %s: grantable_roles %v, want %v", ws.ID, ws.GrantableRoles, roles)
		}
		if ws.ManageAccess == nil || *ws.ManageAccess != wantAccess[ws.ID] {
			t.Errorf("workspace %s: manage_access %v, want %v", ws.ID, ws.ManageAccess, wantAccess[ws.ID])
		}
	}
	if seen != len(want) {
		t.Errorf("GET /api/admin/workspaces lists %d of ws1a and ws2, want both", seen)
	}
	// What it is told it may do, it may; what it is not, it may not.
	f.expect(t, ta, "POST", "/api/admin/users/"+bu1+"/roles", "", map[string]string{"role": "developer", "workspace_id": f.ws1a}, http.StatusOK)
	f.expect(t, ta, "POST", "/api/admin/users/"+bu2+"/roles", "", map[string]string{"role": "developer", "workspace_id": f.ws2}, http.StatusForbidden)
	f.expect(t, ta, "POST", "/api/admin/users/"+bu2+"/roles", "", map[string]string{"role": "business_admin", "workspace_id": f.ws2}, http.StatusOK)
	f.expect(t, ta, "POST", "/api/admin/users/"+bu2+"/access/apps/"+f.app3, "", nil, http.StatusForbidden)

	users := f.grantableRoles(t, ta)
	if got := users[bu1]; !slices.Equal(got, []string{"developer", "business_admin", "business_user"}) {
		t.Errorf("an account of tenant 1: grantable_roles %v, want developer and the business roles", got)
	}
	// In tenant 2 it is a developer: the business roles, which the grant
	// endpoint accepts, and not developer, which it refuses.
	if got := users[bu2]; !slices.Equal(got, []string{"business_admin", "business_user"}) {
		t.Errorf("an account of tenant 2: grantable_roles %v, want the business roles", got)
	}
	f.expect(t, ta, "POST", "/api/admin/users/"+bu2+"/roles", "", map[string]string{"role": "developer"}, http.StatusForbidden)
	f.expect(t, ta, "POST", "/api/admin/users/"+bu2+"/roles", "", map[string]string{"role": "business_user"}, http.StatusOK)
	// An account whose tenant the caller holds nothing in: none, and
	// present.
	f.gbUser(t, "at-g-ta2only", f.cust2, [2]string{"tenant_admin", ""})
	visitor := f.gbUser(t, "at-g-visitor", f.cust1, [2]string{"business_user", f.ws2})
	if got, ok := f.grantableRoles(t, "at-g-ta2only")[visitor]; !ok || got == nil || len(got) != 0 {
		t.Errorf("an account of tenant 1 in ws2, to tenant 2's admin: grantable_roles %v (listed %v), want none (and present)", got, ok)
	}
	// A platform admin may grant every role, to an account with no tenant too.
	nt := f.gbUser(t, "at-g-nt", "", [2]string{"business_user", f.ws1a})
	if got := f.grantableRoles(t, "mm-platform")[nt]; !slices.Equal(got, []string{"platform_admin", "tenant_admin", "developer", "business_admin", "business_user"}) {
		t.Errorf("a platform admin, an account with no tenant: grantable_roles %v, want every role", got)
	}
}

// grantableRoles is each listed account's grantable_roles in GET
// /api/admin/users as sub, by id.
func (f *devRouteFixture) grantableRoles(t *testing.T, sub string) map[string][]string {
	t.Helper()
	var users []struct {
		ID             string   `json:"id"`
		GrantableRoles []string `json:"grantable_roles"`
	}
	if err := json.Unmarshal(f.expect(t, sub, "GET", "/api/admin/users", "", nil, http.StatusOK), &users); err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, u := range users {
		out[u.ID] = u.GrantableRoles
	}
	return out
}

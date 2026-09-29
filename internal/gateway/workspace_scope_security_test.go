package gateway

// Business roles are workspace-scoped: a business admin administers the
// workspace it holds business_admin in, and a business user works in the
// applications of its own workspace. Two gaps let roles reach further:
//
//  1. actorCanAccessApp matched a role held in ANY workspace of the
//     application's tenant, and baWorkspaceModel then adopted the
//     application's workspace — so a business admin of ws1a, sending the
//     X-App-Id of ws1b's application, listed and renamed ws1b's business
//     roles, users and access rules. /api/tasks drew the same tenant-wide
//     line for inbox steps (and matched an unscoped role everywhere).
//  2. adminScopeCustomerIDs read an unscoped tenant_admin/developer grant as
//     "admin of every tenant where I hold any workspace role", so a grant in
//     tenant 1 plus a plain business_user role in tenant 2 made its holder
//     admin of tenant 2.
//
// Closing them surfaced three more. The resource checks behind the admin and
// developer routes asked whether ANY role opens the application, so a
// business_user role in tenant 2 let a tenant admin or developer of tenant 1
// rename, delete and build there; those routes now only count the admin
// scope and developer grants (builderRouteKey). A developer role held in
// tenant 2 made an account of tenant 1 a builder of tenant 1 too. And a
// workflow step naming no assignee role was in every signed-in user's inbox,
// in every tenant.
//
// These tests pin both, plus what must keep working: a developer's
// tenant-wide reach, the sign-up owner's reach over its whole tenant, and a
// tenant with a single workspace. They reuse setupMMFixture
// (multimodel_scope_test.go): tenant 1 has ws1a (app1), ws1b (app2) and the
// tenant-level appT; tenant 2 has ws2 (app3).

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"testing"
)

type wsFixture struct {
	*mmFixture
	cust1, cust2    string
	ws1a, ws1b, ws2 string
	role1b          string // business role in ws1b
	user1b          string // business_user in ws1b (access-rules target)
	ba2             string // business_admin of ws2 (tenant 2)
	step1, step2    string // in-progress steps assigned to business_admin: app1, app2
	stepT, step3    string // … appT, app3
	named1, named2  string // steps assigned to the business role "Approvers": app1, app2
	approvers1a     string // business role "Approvers" in ws1a
	metricC         string // a metric of ws1b's model (app2)
	metricD         string // a metric of tenant 2's model (app3)
}

func setupWSFixture(t *testing.T) *wsFixture {
	t.Helper()
	f := &wsFixture{mmFixture: setupMMFixture(t)}
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	appWS := func(appID string) (ws, cust string) {
		t.Helper()
		if err := f.pool.QueryRow(ctx, `SELECT workspace_id::text, customer_id::text FROM core.application WHERE id=$1::uuid`, appID).Scan(&ws, &cust); err != nil {
			t.Fatalf("app %s: %v", appID, err)
		}
		return ws, cust
	}
	f.ws1a, f.cust1 = appWS(f.app1)
	f.ws1b, _ = appWS(f.app2)
	f.ws2, f.cust2 = appWS(f.app3)

	// user creates an account of tenant cust ("" = none of its own) with
	// roles: role → workspace ("" = unscoped). Several grants of one role
	// are separate calls.
	user := func(sub, cust string, grants ...[2]string) string {
		t.Helper()
		id := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, $1||'@ws.test', $1, NULLIF($2,'')::uuid) RETURNING id::text`, sub, cust)
		for _, g := range grants {
			if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, $2::identity.user_role, NULLIF($3,'')::uuid)`, id, g[0], g[1]); err != nil {
				t.Fatalf("grant %s@%q to %s: %v", g[0], g[1], sub, err)
			}
		}
		return id
	}
	g := func(role, ws string) [2]string { return [2]string{role, ws} }

	// The tenant-level application gets a model: an application with none
	// opens for nobody but admins (actorCanAccessApp).
	modelT := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'T') RETURNING id::text`, f.appT)
	revT := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Live') RETURNING id::text`, modelT)
	if _, err := f.pool.Exec(ctx, `UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Live' WHERE id=$2::uuid`, revT, modelT); err != nil {
		t.Fatalf("activate model T: %v", err)
	}

	f.role1b = q(`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Reviewers 1b') RETURNING id::text`, f.ws1b)
	f.user1b = user("ws-bu1b", f.cust1, g("business_user", f.ws1b))
	f.ba2 = q(`SELECT id::text FROM identity.user WHERE keycloak_sub='mm-ba2'`)
	user("ws-ba1a-bu1b", f.cust1, g("business_admin", f.ws1a), g("business_user", f.ws1b))
	user("ws-dev-ba", f.cust1, g("developer", f.ws1a), g("business_admin", f.ws1a))
	// The sign-up owner's shape (signup.go): tenant_admin and developer
	// unscoped, business_admin in the tenant's first workspace.
	user("ws-owner", f.cust1, g("tenant_admin", ""), g("developer", ""), g("business_admin", f.ws1a))
	// Gap 2: an unscoped grant of tenant 1 plus a plain business_user role
	// in tenant 2.
	user("ws-ta-cross", f.cust1, g("tenant_admin", ""), g("business_admin", f.ws1a), g("business_user", f.ws2))
	user("ws-dev-cross", f.cust1, g("developer", ""), g("business_user", f.ws2))
	// A developer role held in tenant 2 by an account of tenant 1, where it
	// is only a business user: admin of tenant 2, not of tenant 1.
	user("ws-dev-elsewhere", f.cust1, g("business_user", f.ws1a), g("developer", f.ws2))
	user("ws-bu2", f.cust2, g("business_user", f.ws2))
	// A tenant admin scoped to ws1a, and the sign-up owner's shape, each
	// with a plain business_user role in tenant 2: the business role opens
	// tenant 2's applications on the business routes, never admin or
	// developer reach there.
	user("ws-ta-ws", f.cust1, g("tenant_admin", f.ws1a), g("business_user", f.ws2))
	user("ws-owner-cross", f.cust1, g("tenant_admin", ""), g("developer", ""), g("business_admin", f.ws1a), g("business_user", f.ws2))
	f.metricC = q(`SELECT md.id::text FROM model.metric_def md JOIN core.model m ON m.id = md.model_id WHERE m.application_id = $1::uuid LIMIT 1`, f.app2)
	f.metricD = q(`SELECT id::text FROM model.metric_def WHERE model_id = $1::uuid LIMIT 1`, f.modelD)

	// Workflow steps in progress, one per application, assigned to the
	// platform role business_admin; and two assigned to the business role
	// "Approvers", which exists in ws1a AND in ws1b under the same name.
	stepFor := func(appID, assignee string) string {
		t.Helper()
		def := q(`INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps, status, published_at)
		          VALUES ($1::uuid, 'Sign-off by '||$2, 'manual',
		                  jsonb_build_array(jsonb_build_object('id','s1','name','Sign off','type','approval','assignee_roles',jsonb_build_array($2::text))),
		                  'published', now()) RETURNING id::text`, appID, assignee)
		inst := q(`INSERT INTO workflow.workflow_instance (workflow_def_id, started_by, test_run) VALUES ($1::uuid, $2::uuid, false) RETURNING id::text`, def, f.businessUserID)
		return q(`INSERT INTO workflow.workflow_step (instance_id, step_def_id, status) VALUES ($1::uuid, 's1', 'in_progress') RETURNING id::text`, inst)
	}
	f.step1, f.step2 = stepFor(f.app1, "business_admin"), stepFor(f.app2, "business_admin")
	f.stepT, f.step3 = stepFor(f.appT, "business_admin"), stepFor(f.app3, "business_admin")
	f.named1, f.named2 = stepFor(f.app1, "Approvers"), stepFor(f.app2, "Approvers")
	f.approvers1a = q(`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Approvers') RETURNING id::text`, f.ws1a)
	q(`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Approvers') RETURNING id::text`, f.ws1b)
	// mm-user (business_user in ws1a) is in ws1a's "Approvers"; ws-bu1b is
	// in neither.
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid, $2::uuid)`, f.approvers1a, f.businessUserID); err != nil {
		t.Fatalf("approvers member: %v", err)
	}
	return f
}

func (f *wsFixture) status(t *testing.T, method, path, sub, appID string, body any) int {
	t.Helper()
	code, _ := f.do(t, method, path, sub, appID, "", body)
	return code
}

// tenantInstances is every workflow instance (not a test run) of cust's
// applications — what its admin's workflow history lists.
func (f *wsFixture) tenantInstances(t *testing.T, cust string) []string {
	t.Helper()
	return f.column(t, `
		SELECT wi.id::text FROM workflow.workflow_instance wi
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		JOIN core.application app ON app.id = wd.application_id
		WHERE app.customer_id = $1::uuid AND NOT wi.test_run`, cust)
}

// wsIDsOf decodes a JSON array and returns the sorted values of key.
func wsIDsOf(t *testing.T, raw []byte, key string) []string {
	t.Helper()
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	out := []string{}
	for _, it := range items {
		if s, ok := it[key].(string); ok {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func wsContains(list []string, id string) bool {
	for _, s := range list {
		if s == id {
			return true
		}
	}
	return false
}

// Gap 1, business-admin surface: roles, users, access rules and pickers of
// another workspace of the same tenant are out of a business admin's reach.
func TestBusinessAdminCannotAdministerAnotherWorkspace(t *testing.T) {
	f := setupWSFixture(t)

	for _, sub := range []string{"mm-ba1", "ws-ba1a-bu1b"} {
		for _, tc := range []struct{ method, path string }{
			{"GET", "/api/business-admin/roles"},
			{"GET", "/api/business-admin/roles/" + f.role1b + "/members"},
			{"GET", "/api/business-admin/users"},
			{"GET", "/api/business-admin/users/" + f.user1b + "/access-rules"},
			{"GET", "/api/business-admin/available?type=dashboards"},
		} {
			if code := f.status(t, tc.method, tc.path, sub, f.app2, nil); code != http.StatusForbidden {
				t.Errorf("%s %s as %s with ws1b's application: status %d, want 403", tc.method, tc.path, sub, code)
			}
		}
		if code := f.status(t, "PATCH", "/api/business-admin/roles/"+f.role1b, sub, f.app2, map[string]string{"name": "Hijacked"}); code != http.StatusForbidden {
			t.Errorf("rename ws1b's role as %s: status %d, want 403", sub, code)
		}
		if code := f.status(t, "PUT", "/api/business-admin/users/"+f.user1b+"/access-rules", sub, f.app2, map[string]any{"rules": []any{}}); code != http.StatusForbidden {
			t.Errorf("replace a ws1b user's access rules as %s: status %d, want 403", sub, code)
		}
		if code := f.status(t, "POST", "/api/business-admin/roles", sub, f.app2, map[string]string{"name": "Planted"}); code != http.StatusForbidden {
			t.Errorf("create a role in ws1b as %s: status %d, want 403", sub, code)
		}
	}
	var name string
	if err := f.pool.QueryRow(context.Background(), `SELECT name FROM identity.business_role WHERE id=$1::uuid`, f.role1b).Scan(&name); err != nil || name != "Reviewers 1b" {
		t.Errorf("ws1b's role after refused renames: name %q (err %v), want unchanged", name, err)
	}
	if n := f.column(t, `SELECT id::text FROM identity.business_role WHERE name='Planted'`); len(n) != 0 {
		t.Errorf("refused creates left roles behind: %v", n)
	}

	// Its own workspace, and the tenant-level application — which belongs
	// to every workspace of its tenant, so it administers its own
	// workspace's roles there.
	for _, app := range []string{f.app1, f.appT, ""} {
		code, raw := f.do(t, "GET", "/api/business-admin/roles", "mm-ba1", app, "", nil)
		if code != http.StatusOK {
			t.Fatalf("own roles (app %q): status %d %s", app, code, raw)
		}
		if got, want := mmIDs(t, raw), mmSorted(f.roleID, f.approvers1a); !mmEqual(got, want) {
			t.Errorf("own roles (app %q) = %v, want ws1a's %v", app, got, want)
		}
	}
	// A business admin of tenant 2 has no workspace in tenant 1 to
	// administer through its tenant-level application.
	if code := f.status(t, "GET", "/api/business-admin/roles", "mm-ba2", f.appT, nil); code != http.StatusForbidden {
		t.Errorf("tenant 2's business admin on tenant 1's tenant-level application: status %d, want 403", code)
	}
	// An unscoped business_admin grant is inert: without an application the
	// fallback used to land on the workspace where it is a business user.
	if code := f.status(t, "GET", "/api/business-admin/roles", "mm-ba-unscoped", "", nil); code != http.StatusForbidden {
		t.Errorf("unscoped business_admin with a business_user role in ws2: status %d, want 403", code)
	}
}

// Gap 1, business console: a business role opens its own workspace's
// applications and its tenant's tenant-level ones, not the rest of the tenant.
func TestBusinessRolesOpenOnlyTheirWorkspacesApplications(t *testing.T) {
	f := setupWSFixture(t)

	code, raw := f.do(t, "GET", "/api/apps", "mm-user", "", "", nil)
	if code != http.StatusOK {
		t.Fatalf("apps as business user: status %d %s", code, raw)
	}
	if got, want := wsIDsOf(t, raw, "id"), mmSorted(f.app1, f.appT); !mmEqual(got, want) {
		t.Errorf("apps listed to ws1a's business user = %v, want %v", got, want)
	}
	for _, tc := range []struct {
		sub, app string
		want     int
	}{
		{"mm-user", f.app2, http.StatusForbidden},
		{"mm-ba1", f.app2, http.StatusForbidden},
		{"mm-user", f.app1, http.StatusOK},
		{"mm-user", f.appT, http.StatusOK},
		{"mm-ba1b", f.app2, http.StatusOK},
	} {
		if code := f.status(t, "GET", "/api/automation/rules", tc.sub, tc.app, nil); code != tc.want {
			t.Errorf("automation rules as %s in app %s: status %d, want %d", tc.sub, tc.app, code, tc.want)
		}
	}
}

// Gap 1, inbox: a step assigned to a role shows to — and is decided by —
// the holders of that role in the step's application's workspace.
func TestTaskInboxStaysInTheRolesWorkspace(t *testing.T) {
	f := setupWSFixture(t)

	inbox := func(sub string) []string {
		t.Helper()
		code, raw := f.do(t, "GET", "/api/tasks", sub, "", "", nil)
		if code != http.StatusOK {
			t.Fatalf("tasks as %s: status %d %s", sub, code, raw)
		}
		return mmIDs(t, raw)
	}
	for _, tc := range []struct {
		sub  string
		want []string
	}{
		{"mm-ba1", mmSorted(f.step1, f.stepT)},
		{"mm-ba1b", mmSorted(f.step2, f.stepT)},
		{"mm-ba2", mmSorted(f.step3)},
		{"ws-ba1a-bu1b", mmSorted(f.step1, f.stepT)},
		// An unscoped business_admin grant is inert, here as everywhere.
		{"mm-ba-unscoped", []string{}},
		// Named business roles: ws1a's "Approvers" is not ws1b's.
		{"mm-user", mmSorted(f.named1)},
		{"ws-bu1b", []string{}},
		// The platform admin holds business_admin in ws1a; platform_admin is
		// not an assignee of these steps.
		{"mm-platform", mmSorted(f.step1, f.stepT)},
	} {
		if got := inbox(tc.sub); !mmEqual(got, tc.want) {
			t.Errorf("inbox of %s = %v, want %v", tc.sub, got, tc.want)
		}
	}

	stepStatus := func(id string) string {
		t.Helper()
		var s string
		if err := f.pool.QueryRow(context.Background(), `SELECT status::text FROM workflow.workflow_step WHERE id=$1::uuid`, id).Scan(&s); err != nil {
			t.Fatalf("step %s: %v", id, err)
		}
		return s
	}
	for _, tc := range []struct{ sub, step, name string }{
		{"mm-ba1", f.step2, "ws1b's step as ws1a's business admin"},
		{"mm-ba1", f.step3, "tenant 2's step as tenant 1's business admin"},
		{"mm-ba-unscoped", f.step1, "a step through an inert unscoped grant"},
		{"ws-bu1b", f.named1, "ws1a's Approvers step as a member of no Approvers role"},
	} {
		if code := f.status(t, "POST", "/api/tasks/"+tc.step+"/complete", tc.sub, "", map[string]string{"decision": "approve"}); code != http.StatusForbidden {
			t.Errorf("deciding %s: status %d, want 403", tc.name, code)
		}
		if got := stepStatus(tc.step); got != "in_progress" {
			t.Errorf("deciding %s was refused but left the step %q", tc.name, got)
		}
	}
	if code, raw := f.do(t, "POST", "/api/tasks/"+f.step2+"/complete", "mm-ba1b", "", "", map[string]string{"decision": "approve"}); code != http.StatusOK {
		t.Errorf("ws1b's business admin deciding its own step: status %d %s", code, raw)
	}
	if code, raw := f.do(t, "POST", "/api/tasks/"+f.named1+"/complete", "mm-user", "", "", map[string]string{"decision": "approve"}); code != http.StatusOK {
		t.Errorf("an Approvers member deciding its workspace's step: status %d %s", code, raw)
	}
}

// Gap 2: an unscoped tenant_admin/developer grant is admin scope over the
// holder's own tenant, not over every tenant where it holds a workspace role.
func TestUnscopedAdminGrantStaysInItsOwnTenant(t *testing.T) {
	f := setupWSFixture(t)

	tenants := func(sub string) []string {
		t.Helper()
		code, raw := f.do(t, "GET", "/api/admin/tenants", sub, "", "", nil)
		if code != http.StatusOK {
			t.Fatalf("tenants as %s: status %d %s", sub, code, raw)
		}
		return wsIDsOf(t, raw, "id")
	}
	if got := tenants("ws-ta-cross"); !mmEqual(got, []string{f.cust1}) {
		t.Errorf("tenants of tenant 1's admin with a business_user role in tenant 2 = %v, want [%s]", got, f.cust1)
	}
	users := func(sub string) []string {
		t.Helper()
		code, raw := f.do(t, "GET", "/api/admin/users", sub, "", "", nil)
		if code != http.StatusOK {
			t.Fatalf("users as %s: status %d %s", sub, code, raw)
		}
		return wsIDsOf(t, raw, "id")
	}
	for _, sub := range []string{"ws-ta-cross", "ws-dev-cross"} {
		got := users(sub)
		if wsContains(got, f.ba2) {
			t.Errorf("users listed to %s include tenant 2's business admin %s", sub, f.ba2)
		}
		if !wsContains(got, f.user1b) {
			t.Errorf("users listed to %s leave out its own tenant's user %s", sub, f.user1b)
		}
	}
	// A developer role held in tenant 2 is admin scope over tenant 2 only:
	// in tenant 1 this account is a business user.
	if got := users("ws-dev-elsewhere"); wsContains(got, f.user1b) || !wsContains(got, f.ba2) {
		t.Errorf("users listed to a tenant 1 account developing in tenant 2 = %v, want tenant 2's (incl. %s) and not %s", got, f.ba2, f.user1b)
	}

	// workflowAdminScope reuses the admin scope: tenant 2's instance stays out.
	code, raw := f.do(t, "GET", "/api/workflow/history", "ws-ta-cross", "", "", nil)
	if code != http.StatusOK {
		t.Fatalf("history: status %d %s", code, raw)
	}
	if got, want := mmIDs(t, raw), f.tenantInstances(t, f.cust1); !mmEqual(got, want) || wsContains(got, f.inst3) {
		t.Errorf("history of tenant 1's admin with a business_user role in tenant 2 = %v, want tenant 1's %v", got, want)
	}
	if code := f.status(t, "PATCH", "/api/workflow/instances/"+f.inst3, "ws-ta-cross", "", map[string]string{"status": "cancelled"}); code != http.StatusNotFound {
		t.Errorf("override of tenant 2's instance: status %d, want 404", code)
	}

	// What the business_user role in tenant 2 opens, it still opens.
	code, raw = f.do(t, "GET", "/api/apps", "ws-ta-cross", "", "", nil)
	if code != http.StatusOK {
		t.Fatalf("apps: status %d %s", code, raw)
	}
	if got, want := wsIDsOf(t, raw, "id"), mmSorted(f.app1, f.app2, f.appT, f.app3); !mmEqual(got, want) {
		t.Errorf("apps listed to tenant 1's admin with a business_user role in tenant 2 = %v, want %v", got, want)
	}
	if code := f.status(t, "GET", "/api/automation/rules", "ws-ta-cross", f.app3, nil); code != http.StatusOK {
		t.Errorf("automation rules of tenant 2's app as its business user: status %d, want 200", code)
	}
}

// What must keep working: developers are tenant-wide, the sign-up owner and
// a tenant admin reach every workspace of their tenant, and a tenant with
// one workspace is unaffected.
func TestWorkspaceScopeKeepsLegitimateReach(t *testing.T) {
	f := setupWSFixture(t)

	for _, tc := range []struct {
		sub, method, path, app string
		body                   any
	}{
		// A developer manages the business roles its workflow steps need,
		// in every workspace of its tenant (baOrDev).
		{"mm-dev", "GET", "/api/business-admin/roles", f.app2, nil},
		{"mm-dev", "GET", "/api/business-admin/roles/" + f.role1b + "/members", f.app2, nil},
		{"mm-dev", "GET", "/api/business-admin/roles", f.appT, nil},
		{"ws-dev-ba", "GET", "/api/business-admin/roles", f.app2, nil},
		{"mm-dev", "GET", "/api/automation/rules", f.app2, nil},
		// The sign-up owner and a tenant admin: every workspace.
		{"ws-owner", "GET", "/api/business-admin/roles", f.app2, nil},
		{"ws-owner", "GET", "/api/business-admin/users", f.app2, nil},
		{"ws-owner", "GET", "/api/business-admin/users/" + f.user1b + "/access-rules", f.app2, nil},
		{"ws-owner", "GET", "/api/business-admin/available?type=dashboards", f.app2, nil},
		{"ws-owner", "GET", "/api/business-admin/users", f.appT, nil},
		{"ws-owner", "GET", "/api/automation/rules", f.app2, nil},
		{"mm-tenant-admin", "GET", "/api/business-admin/users", f.app2, nil},
		// A single-workspace tenant: its business admin, with or without
		// the application named, and its business user.
		{"mm-ba2", "GET", "/api/business-admin/roles", f.app3, nil},
		{"mm-ba2", "GET", "/api/business-admin/users", f.app3, nil},
		{"mm-ba2", "GET", "/api/business-admin/users", "", nil},
		{"mm-ba2", "GET", "/api/business-admin/available?type=dashboards", f.app3, nil},
		{"ws-bu2", "GET", "/api/automation/rules", f.app3, nil},
	} {
		if code, raw := f.do(t, tc.method, tc.path, tc.sub, tc.app, "", tc.body); code != http.StatusOK {
			t.Errorf("%s %s as %s (app %q): status %d %s, want 200", tc.method, tc.path, tc.sub, tc.app, code, raw)
		}
	}
	if code, raw := f.do(t, "PATCH", "/api/business-admin/roles/"+f.role1b, "mm-dev", f.app2, "", map[string]string{"name": "Reviewers ws1b"}); code != http.StatusOK {
		t.Errorf("developer renaming ws1b's role: status %d %s", code, raw)
	}
	// Business-admin-only routes stay business-admin-only: holding
	// developer does not stand in for business_admin in another workspace.
	if code := f.status(t, "GET", "/api/business-admin/users", "ws-dev-ba", f.app2, nil); code != http.StatusForbidden {
		t.Errorf("developer + ws1a business admin listing ws1b's users: status %d, want 403", code)
	}

	apps := func(sub string) []string {
		t.Helper()
		code, raw := f.do(t, "GET", "/api/apps", sub, "", "", nil)
		if code != http.StatusOK {
			t.Fatalf("apps as %s: status %d %s", sub, code, raw)
		}
		return wsIDsOf(t, raw, "id")
	}
	for _, tc := range []struct {
		sub  string
		want []string
	}{
		{"mm-dev", mmSorted(f.app1, f.app2, f.appT)},
		{"ws-owner", mmSorted(f.app1, f.app2, f.appT)},
		{"mm-ba2", mmSorted(f.app3)},
		{"ws-bu2", mmSorted(f.app3)},
	} {
		if got := apps(tc.sub); !mmEqual(got, tc.want) {
			t.Errorf("apps listed to %s = %v, want %v", tc.sub, got, tc.want)
		}
	}

	code, raw := f.do(t, "GET", "/api/workflow/history", "ws-owner", "", "", nil)
	if code != http.StatusOK {
		t.Fatalf("owner history: status %d %s", code, raw)
	}
	if got, want := mmIDs(t, raw), f.tenantInstances(t, f.cust1); !mmEqual(got, want) || !wsContains(got, f.inst2) {
		t.Errorf("owner's history = %v, want every workspace's: %v", got, want)
	}
	code, raw = f.do(t, "GET", "/api/admin/tenants", "ws-owner", "", "", nil)
	if code != http.StatusOK {
		t.Fatalf("owner tenants: status %d %s", code, raw)
	}
	if got := wsIDsOf(t, raw, "id"); !mmEqual(got, []string{f.cust1}) {
		t.Errorf("owner's tenants = %v, want [%s]", got, f.cust1)
	}
	if code, raw := f.do(t, "GET", "/api/tasks", "ws-owner", "", "", nil); code != http.StatusOK {
		t.Errorf("owner inbox: status %d %s", code, raw)
	} else if got, want := mmIDs(t, raw), mmSorted(f.step1, f.stepT); !mmEqual(got, want) {
		t.Errorf("owner's inbox (business_admin in ws1a) = %v, want %v", got, want)
	}
}

// Gap 2 on the admin and developer routes: a business role held in another
// tenant opens that tenant's applications on the business routes only. The
// resource checks behind the admin and developer routes (requireResourceAccess,
// resolveDemoModelID, googleConnectionScope) are builder checks — admin scope
// or a developer grant — so a tenant admin or developer of tenant 1 with a
// business_user role in tenant 2 administers and builds nothing there.
func TestAdminAndDeveloperRoutesIgnoreBusinessRolesElsewhere(t *testing.T) {
	type call struct {
		method, path, app string
		body              any
	}
	// Each account shape gets a fresh fixture: on a handler that lets one
	// through, its deletes would otherwise hide what the next shape may do.
	// Destructive calls come last for the same reason.
	admin := func(f *wsFixture) []call {
		return []call{
			{"PATCH", "/api/admin/applications/" + f.app3, "", map[string]string{"name": "Hijacked"}},
			{"POST", "/api/admin/models", "", map[string]string{"application_id": f.app3, "name": "Planted model"}},
			{"POST", "/api/admin/revisions", "", map[string]string{"model_id": f.modelD, "name": "Planted revision"}},
			{"PATCH", "/api/admin/revisions/" + f.revD, "", map[string]string{"name": "Hijacked revision"}},
			{"PUT", "/api/admin/models/" + f.modelD + "/active-revision", "", map[string]string{"revision_name": "Live"}},
			{"GET", "/api/developer/integrations/google-service-account", f.app3, nil},
			{"DELETE", "/api/developer/integrations/google-service-account", f.app3, nil},
			{"DELETE", "/api/admin/revisions/" + f.revD, "", nil},
			{"DELETE", "/api/admin/models/" + f.modelD, "", nil},
			{"DELETE", "/api/admin/applications/" + f.app3, "", nil},
		}
	}
	developer := func(f *wsFixture) []call {
		return []call{
			{"PATCH", "/api/developer/metrics/" + f.metricD, "", map[string]string{"name": "Hijacked metric"}},
			{"POST", "/api/developer/metrics", f.app3, map[string]any{"name": "Planted metric", "is_input": true, "agg_rule": "sum", "revision_id": f.revD}},
		}
	}
	both := func(f *wsFixture) []call { return append(developer(f), admin(f)...) }
	for _, tc := range []struct {
		sub   string
		calls func(*wsFixture) []call
	}{
		{"ws-ta-cross", admin},      // unscoped tenant_admin of tenant 1
		{"ws-ta-ws", admin},         // tenant_admin scoped to ws1a
		{"ws-owner-cross", both},    // the sign-up owner's shape
		{"ws-dev-cross", developer}, // unscoped developer of tenant 1
	} {
		t.Run(tc.sub, func(t *testing.T) {
			f := setupWSFixture(t)
			for _, c := range tc.calls(f) {
				if code, raw := f.do(t, c.method, c.path, tc.sub, c.app, "", c.body); code != http.StatusForbidden {
					t.Errorf("%s %s as %s (business_user in tenant 2): status %d %s, want 403", c.method, c.path, tc.sub, code, raw)
				}
			}
			one := func(sql string, args ...any) string {
				t.Helper()
				var s string
				if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
					t.Fatalf("%s: %v", sql, err)
				}
				return s
			}
			if got := one(`SELECT COALESCE((SELECT name FROM core.application WHERE id=$1::uuid), '<deleted>')`, f.app3); got != "App Three" {
				t.Errorf("tenant 2's application after refused calls: %q, want \"App Three\"", got)
			}
			if got := one(`SELECT COALESCE((SELECT name FROM model.revision WHERE id=$1::uuid), '<deleted>')`, f.revD); got != "Live" {
				t.Errorf("tenant 2's live revision after refused calls: %q, want \"Live\"", got)
			}
			if got := one(`SELECT COALESCE((SELECT name FROM model.metric_def WHERE id=$1::uuid), '<deleted>')`, f.metricD); got != "Sales D" {
				t.Errorf("tenant 2's metric after refused calls: %q, want \"Sales D\"", got)
			}
			if got := one(`SELECT count(*)::text FROM (
				SELECT id FROM core.model WHERE name='Planted model'
				UNION ALL SELECT id FROM model.revision WHERE name='Planted revision'
				UNION ALL SELECT id FROM model.metric_def WHERE name='Planted metric') p`); got != "0" {
				t.Errorf("refused creates left %s rows behind in tenant 2", got)
			}
		})
	}

	f := setupWSFixture(t)

	// Their own tenant stays theirs, and the business role in tenant 2
	// still opens tenant 2's application on the business routes.
	for _, tc := range []struct {
		sub, method, path, app string
		body                   any
	}{
		{"ws-ta-cross", "PATCH", "/api/admin/applications/" + f.app1, "", map[string]string{"name": "App One"}},
		{"ws-ta-ws", "PATCH", "/api/admin/applications/" + f.app2, "", map[string]string{"name": "App Two"}},
		{"ws-owner-cross", "PATCH", "/api/admin/applications/" + f.app1, "", map[string]string{"name": "App One"}},
		{"ws-owner-cross", "PATCH", "/api/developer/metrics/" + f.metricC, "", map[string]string{"name": "Sales C"}},
		{"ws-dev-cross", "PATCH", "/api/developer/metrics/" + f.metricA, "", map[string]string{"name": "Sales A"}},
		{"ws-owner-cross", "GET", "/api/developer/integrations/google-service-account", f.app1, nil},
		// ws-dev-elsewhere is a developer in tenant 2's workspace.
		{"ws-dev-elsewhere", "PATCH", "/api/developer/metrics/" + f.metricD, "", map[string]string{"name": "Sales D"}},
		{"ws-ta-cross", "GET", "/api/automation/rules", f.app3, nil},
		{"ws-ta-ws", "GET", "/api/automation/rules", f.app3, nil},
		{"ws-owner-cross", "GET", "/api/automation/rules", f.app3, nil},
		{"ws-dev-cross", "GET", "/api/automation/rules", f.app3, nil},
	} {
		if code, raw := f.do(t, tc.method, tc.path, tc.sub, tc.app, "", tc.body); code != http.StatusOK {
			t.Errorf("%s %s as %s (app %q): status %d %s, want 200", tc.method, tc.path, tc.sub, tc.app, code, raw)
		}
	}
	code, raw := f.do(t, "GET", "/api/apps", "ws-ta-ws", "", "", nil)
	if code != http.StatusOK {
		t.Fatalf("apps as ws-ta-ws: status %d %s", code, raw)
	}
	if got, want := wsIDsOf(t, raw, "id"), mmSorted(f.app1, f.app2, f.appT, f.app3); !mmEqual(got, want) {
		t.Errorf("apps listed to a ws1a tenant admin with a business_user role in tenant 2 = %v, want %v", got, want)
	}
}

// Gap 1 for a developer role held in another tenant: it makes its holder a
// builder of that tenant, not of its own, where the account is only a
// business user of ws1a. The developer arms of roleReachesAppSQL and
// canAdministerWorkspace take an unscoped developer grant only, as
// adminScopeCustomerIDs does.
func TestDeveloperGrantElsewhereIsNoBuilderAtHome(t *testing.T) {
	f := setupWSFixture(t)
	const sub = "ws-dev-elsewhere"

	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/business-admin/roles"},
		{"GET", "/api/business-admin/roles/" + f.role1b + "/members"},
		{"GET", "/api/automation/rules"},
	} {
		if code := f.status(t, tc.method, tc.path, sub, f.app2, nil); code != http.StatusForbidden {
			t.Errorf("%s %s with ws1b's application: status %d, want 403", tc.method, tc.path, code)
		}
	}
	if code := f.status(t, "PATCH", "/api/business-admin/roles/"+f.role1b, sub, f.app2, map[string]string{"name": "Hijacked"}); code != http.StatusForbidden {
		t.Errorf("rename ws1b's role: status %d, want 403", code)
	}
	if code := f.status(t, "PATCH", "/api/developer/metrics/"+f.metricC, sub, "", map[string]string{"name": "Hijacked"}); code != http.StatusForbidden {
		t.Errorf("edit ws1b's metric: status %d, want 403", code)
	}
	var roleName, metricName string
	if err := f.pool.QueryRow(context.Background(), `SELECT (SELECT name FROM identity.business_role WHERE id=$1::uuid), (SELECT name FROM model.metric_def WHERE id=$2::uuid)`, f.role1b, f.metricC).Scan(&roleName, &metricName); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if roleName != "Reviewers 1b" || metricName != "Sales C" {
		t.Errorf("after refused edits: role %q, metric %q; want unchanged", roleName, metricName)
	}

	code, raw := f.do(t, "GET", "/api/apps", sub, "", "", nil)
	if code != http.StatusOK {
		t.Fatalf("apps: status %d %s", code, raw)
	}
	// ws1a's application and the tenant-level one as a business user; tenant
	// 2's as its developer; not ws1b's.
	if got, want := wsIDsOf(t, raw, "id"), mmSorted(f.app1, f.appT, f.app3); !mmEqual(got, want) {
		t.Errorf("apps listed = %v, want %v", got, want)
	}
	// As tenant 2's developer it manages tenant 2's business roles.
	if code, raw := f.do(t, "GET", "/api/business-admin/roles", sub, f.app3, "", nil); code != http.StatusOK {
		t.Errorf("tenant 2's roles as its developer: status %d %s", code, raw)
	}
}

// Gap 1 for steps that name no assignee role: they are open to everyone who
// reaches the step's application, not to every signed-in user of every
// tenant — who saw the instance's context and could decide the step.
func TestOpenTaskStaysInItsApplication(t *testing.T) {
	f := setupWSFixture(t)
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	openStep := func(appID, startedBy string) string {
		t.Helper()
		def := q(`INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps, status, published_at)
		          VALUES ($1::uuid, 'Open step '||gen_random_uuid()::text, 'manual',
		                  jsonb_build_array(jsonb_build_object('id','s1','name','Open','type','approval')),
		                  'published', now()) RETURNING id::text`, appID)
		inst := q(`INSERT INTO workflow.workflow_instance (workflow_def_id, started_by, test_run, context)
		           VALUES ($1::uuid, $2::uuid, false, '{"secret":"value"}'::jsonb) RETURNING id::text`, def, startedBy)
		return q(`INSERT INTO workflow.workflow_step (instance_id, step_def_id, status) VALUES ($1::uuid, 's1', 'in_progress') RETURNING id::text`, inst)
	}
	open1a, open3 := openStep(f.app1, f.businessUserID), openStep(f.app3, f.ba2)
	open3b := openStep(f.app3, f.ba2) // decided by its own tenant below

	inbox := func(sub string) []string {
		t.Helper()
		code, raw := f.do(t, "GET", "/api/tasks", sub, "", "", nil)
		if code != http.StatusOK {
			t.Fatalf("tasks as %s: status %d %s", sub, code, raw)
		}
		return mmIDs(t, raw)
	}
	for _, tc := range []struct {
		sub          string
		sees, hidden []string
	}{
		{"mm-user", []string{open1a}, []string{open3}},         // business user of ws1a
		{"ws-bu1b", nil, []string{open1a, open3}},              // business user of ws1b
		{"ws-bu2", []string{open3}, []string{open1a}},          // business user of ws2
		{"mm-dev", []string{open1a}, []string{open3}},          // developer of tenant 1
		{"mm-tenant-admin", []string{open1a}, []string{open3}}, // tenant 1's admin
		{"ws-ta-cross", []string{open1a, open3}, nil},          // … with business_user in ws2
		{"mm-platform", []string{open1a, open3}, nil},          // platform admin
		{"mm-ba-unscoped", []string{open3}, []string{open1a}},  // inert grant; business_user in ws2
	} {
		got := inbox(tc.sub)
		for _, id := range tc.sees {
			if !wsContains(got, id) {
				t.Errorf("inbox of %s leaves out open step %s", tc.sub, id)
			}
		}
		for _, id := range tc.hidden {
			if wsContains(got, id) {
				t.Errorf("inbox of %s lists open step %s of an application it does not reach", tc.sub, id)
			}
		}
	}

	status := func(id string) string {
		t.Helper()
		return q(`SELECT status::text FROM workflow.workflow_step WHERE id=$1::uuid`, id)
	}
	for _, tc := range []struct{ sub, step string }{
		{"ws-bu1b", open3},
		{"ws-bu1b", open1a},
		{"mm-ba2", open1a},
	} {
		if code := f.status(t, "POST", "/api/tasks/"+tc.step+"/complete", tc.sub, "", map[string]string{"decision": "approve"}); code != http.StatusForbidden {
			t.Errorf("%s deciding open step %s: status %d, want 403", tc.sub, tc.step, code)
		}
		if got := status(tc.step); got != "in_progress" {
			t.Errorf("refused decision on %s left it %q", tc.step, got)
		}
	}
	if code, raw := f.do(t, "POST", "/api/tasks/"+open3b+"/complete", "ws-bu2", "", "", map[string]string{"decision": "approve"}); code != http.StatusOK {
		t.Errorf("tenant 2's business user deciding its open step: status %d %s", code, raw)
	}
}

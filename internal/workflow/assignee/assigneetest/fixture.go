// Package assigneetest seeds, on the real schema, one user of every role
// shape the workflow assignment boundary (package assignee) tells apart, so
// the tests of every surface that draws it — deciding a step, notification
// step recipients, SLA reminders — check the same people.
package assigneetest

import (
	"context"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// Fixture is two tenants. Tenant 1 has workspaces ws1a (App1) and ws1b
// (App2) and the tenant-level AppT (workspace_id NULL); tenant 2 has ws2
// (App3).
//
// Users, by name:
//
//	ba_1a, ba_1b, ba_2   business_admin held in ws1a, ws1b, ws2
//	ba_unscoped          business_admin with no workspace (inert), tenant 1
//	bu_1a                business_user in ws1a
//	dev_ws1b, dev_ws2    developer held in ws1b (tenant 1), ws2 (tenant 2)
//	dev_home1, dev_home2 unscoped developer of an account of tenant 1, 2
//	dev_global           unscoped developer of an account with no tenant
//	ta_1, ta_2           unscoped tenant_admin of an account of tenant 1, 2
//	ta_grant2            unscoped tenant_admin, no tenant, granted App3
//	pa                   platform_admin
//	appr_1a, appr_1b,    business_user in ws1a, ws1b, ws2 and member of
//	appr_2               that workspace's business role "Approvers"
//	norole_1             an account of tenant 1 holding no role
//	ba_1a_off            business_admin in ws1a, account disabled
//	imp_1a               business_user in ws1a and member of ws1a's business
//	                     roles named business_admin, developer, tenant_admin
//	                     and platform_admin (a business admin can name one so)
//	ta_ws1b              tenant_admin held in ws1b (tenant 1)
//	pa_ws1a              platform_admin held in ws1a, account of tenant 1
//	ta_nt_1b             unscoped tenant_admin, no tenant, business_user in
//	                     ws1b (admin scope: the tenants of its workspaces)
//	ta_model2            unscoped tenant_admin, no tenant, granted a model of
//	                     App3
//	owner_1              the sign-up owner of tenant 1: unscoped tenant_admin
//	                     and developer, and business_admin in ws1a
//	ba_dev_global        business_admin in ws1a and unscoped developer of an
//	                     account with no tenant (a global builder)
//	ta_home1_grant3      unscoped tenant_admin of an account of tenant 1,
//	                     granted App3 (tenant 2): its reach there, no tier
//	ta_home1_dev2        unscoped tenant_admin of an account of tenant 1 and
//	                     developer held in ws2: a developer of tenant 2 only
//	ta_ws2_nt_grant1     no tenant, tenant_admin held in ws2, business_user in
//	                     ws1b and granted App1: tenant 2's administrator, and
//	                     in tenant 1 only what ws1b opens
type Fixture struct {
	Pool                   *pgxpool.Pool
	Cust1, Cust2           string
	WS1a, WS1b, WS2        string
	App1, App2, AppT, App3 string
	// Users maps a name above to its user id.
	Users map[string]string
}

// New seeds the fixture into a fresh database built from the real
// migrations.
func New(t *testing.T) *Fixture {
	t.Helper()
	return Seed(t, testdb.New(t, migrationfs.FS, "."))
}

// Seed seeds the fixture into pool, which holds the real schema.
func Seed(t *testing.T, pool *pgxpool.Pool) *Fixture {
	t.Helper()
	ctx := context.Background()
	one := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	ex := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	f := &Fixture{Pool: pool, Users: map[string]string{}}
	f.Cust1 = one(`INSERT INTO core.customer (name, plan) VALUES ('Tenant One', 'enterprise') RETURNING id::text`)
	f.Cust2 = one(`INSERT INTO core.customer (name, plan) VALUES ('Tenant Two', 'enterprise') RETURNING id::text`)
	ws := func(cust, name string) string {
		return one(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, $2) RETURNING id::text`, cust, name)
	}
	f.WS1a, f.WS1b, f.WS2 = ws(f.Cust1, "ws1a"), ws(f.Cust1, "ws1b"), ws(f.Cust2, "ws2")
	app := func(ws any, cust, name string) string {
		return one(`INSERT INTO core.application (workspace_id, customer_id, name, mode)
		            VALUES ($1::uuid, $2::uuid, $3, 'planning') RETURNING id::text`, ws, cust, name)
	}
	f.App1, f.App2, f.App3 = app(f.WS1a, f.Cust1, "App1"), app(f.WS1b, f.Cust1, "App2"), app(f.WS2, f.Cust2, "App3")
	f.AppT = app(nil, f.Cust1, "AppT")

	user := func(name string, cust any) string {
		id := one(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		           VALUES ($1, $1 || '@example.test', $1, $2::uuid) RETURNING id::text`, "asg-"+name, cust)
		f.Users[name] = id
		return id
	}
	role := func(userID, role string, ws any) {
		ex(`INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		    VALUES ($1::uuid, $2::identity.user_role, $3::uuid)`, userID, role, ws)
	}
	approvers := map[string]string{}
	for _, w := range []string{f.WS1a, f.WS1b, f.WS2} {
		approvers[w] = one(`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Approvers') RETURNING id::text`, w)
	}
	member := func(userID, ws string) {
		ex(`INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid, $2::uuid)`, approvers[ws], userID)
	}

	role(user("ba_1a", f.Cust1), "business_admin", f.WS1a)
	role(user("ba_1b", f.Cust1), "business_admin", f.WS1b)
	role(user("ba_2", f.Cust2), "business_admin", f.WS2)
	role(user("ba_unscoped", f.Cust1), "business_admin", nil)
	role(user("bu_1a", f.Cust1), "business_user", f.WS1a)
	role(user("dev_ws1b", f.Cust1), "developer", f.WS1b)
	role(user("dev_ws2", f.Cust2), "developer", f.WS2)
	role(user("dev_home1", f.Cust1), "developer", nil)
	role(user("dev_home2", f.Cust2), "developer", nil)
	role(user("dev_global", nil), "developer", nil)
	role(user("ta_1", f.Cust1), "tenant_admin", nil)
	role(user("ta_2", f.Cust2), "tenant_admin", nil)
	role(user("ta_grant2", nil), "tenant_admin", nil)
	ex(`INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid)`, f.Users["ta_grant2"], f.App3)
	role(user("pa", nil), "platform_admin", nil)
	for name, w := range map[string]string{"appr_1a": f.WS1a, "appr_1b": f.WS1b, "appr_2": f.WS2} {
		cust := f.Cust1
		if w == f.WS2 {
			cust = f.Cust2
		}
		id := user(name, cust)
		role(id, "business_user", w)
		member(id, w)
	}
	user("norole_1", f.Cust1)

	role(user("ba_1a_off", f.Cust1), "business_admin", f.WS1a)
	ex(`UPDATE identity.user SET disabled_at = now() WHERE id = $1::uuid`, f.Users["ba_1a_off"])
	imp := user("imp_1a", f.Cust1)
	role(imp, "business_user", f.WS1a)
	for _, name := range []string{"business_admin", "developer", "tenant_admin", "platform_admin"} {
		br := one(`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, $2) RETURNING id::text`, f.WS1a, name)
		ex(`INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid, $2::uuid)`, br, imp)
	}
	role(user("ta_ws1b", f.Cust1), "tenant_admin", f.WS1b)
	role(user("pa_ws1a", f.Cust1), "platform_admin", f.WS1a)
	ntb := user("ta_nt_1b", nil)
	role(ntb, "tenant_admin", nil)
	role(ntb, "business_user", f.WS1b)
	role(user("ta_model2", nil), "tenant_admin", nil)
	model := one(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Plan') RETURNING id::text`, f.App3)
	ex(`INSERT INTO identity.user_model_access (user_id, model_id) VALUES ($1::uuid, $2::uuid)`, f.Users["ta_model2"], model)
	owner := user("owner_1", f.Cust1)
	role(owner, "tenant_admin", nil)
	role(owner, "developer", nil)
	role(owner, "business_admin", f.WS1a)
	bdg := user("ba_dev_global", nil)
	role(bdg, "business_admin", f.WS1a)
	role(bdg, "developer", nil)
	// Roles count per tenant (2026-09-30): what these hold in one tenant
	// makes them nothing in another.
	grant3 := user("ta_home1_grant3", f.Cust1)
	role(grant3, "tenant_admin", nil)
	ex(`INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid)`, grant3, f.App3)
	dev2 := user("ta_home1_dev2", f.Cust1)
	role(dev2, "tenant_admin", nil)
	role(dev2, "developer", f.WS2)
	nt := user("ta_ws2_nt_grant1", nil)
	role(nt, "tenant_admin", f.WS2)
	role(nt, "business_user", f.WS1b)
	ex(`INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid)`, nt, f.App1)
	return f
}

// Step starts an instance of a one-step approval workflow of application
// appID whose step names roles (none when empty), and returns the step's id,
// in progress. Its due time is past, so a reminder is due as well.
func (f *Fixture) Step(t *testing.T, appID string, roles ...string) string {
	t.Helper()
	ctx := context.Background()
	if roles == nil {
		roles = []string{}
	}
	var stepID string
	err := f.Pool.QueryRow(ctx, `
		WITH def AS (
		    INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps)
		    VALUES ($1::uuid, 'Budget Approval ' || gen_random_uuid()::text, 'manual',
		            jsonb_build_array(jsonb_build_object('id', 's1', 'name', 'Review', 'type', 'approval',
		                                                 'assignee_roles', to_jsonb($2::text[]))))
		    RETURNING id
		), inst AS (
		    INSERT INTO workflow.workflow_instance (workflow_def_id, started_by)
		    SELECT id, $3::uuid FROM def RETURNING id
		)
		INSERT INTO workflow.workflow_step (instance_id, step_def_id, status, due_at)
		SELECT id, 's1', 'in_progress', now() - interval '2 hours' FROM inst
		RETURNING id::text
	`, appID, roles, f.Users["norole_1"]).Scan(&stepID)
	if err != nil {
		t.Fatalf("start a step of %s naming %v: %v", appID, roles, err)
	}
	return stepID
}

// Names returns the fixture names of ids, sorted; an id outside the
// fixture shows as itself.
func (f *Fixture) Names(ids []string) []string {
	byID := make(map[string]string, len(f.Users))
	for name, id := range f.Users {
		byID[id] = name
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if name, ok := byID[id]; ok {
			out = append(out, name)
		} else {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// Case is one step shape and the fixture users it is assigned to.
type Case struct {
	Name  string
	App   func(*Fixture) string
	Roles []string
	Want  []string // sorted fixture names
}

// Cases are the boundary's cases, shared by every surface's test. No case
// has ba_1a_off (disabled) or, for a named role, imp_1a (a business role
// named like a platform role is not that role).
var Cases = []Case{
	{"named business_admin, workspace app", func(f *Fixture) string { return f.App1 }, []string{"business_admin"},
		[]string{"ba_1a", "ba_dev_global", "owner_1"}},
	{"named business_admin, tenant-level app", func(f *Fixture) string { return f.AppT }, []string{"business_admin"},
		[]string{"ba_1a", "ba_1b", "ba_dev_global", "owner_1"}},
	{"named business role, workspace app", func(f *Fixture) string { return f.App1 }, []string{"Approvers"},
		[]string{"appr_1a"}},
	{"named business role, tenant-level app", func(f *Fixture) string { return f.AppT }, []string{"Approvers"},
		[]string{"appr_1a", "appr_1b"}},
	{"named developer, tenant 1", func(f *Fixture) string { return f.App1 }, []string{"developer"},
		[]string{"ba_dev_global", "dev_global", "dev_home1", "dev_ws1b", "owner_1"}},
	{"named developer, tenant 2", func(f *Fixture) string { return f.App3 }, []string{"developer"},
		[]string{"ba_dev_global", "dev_global", "dev_home2", "dev_ws2", "ta_home1_dev2"}},
	{"named tenant_admin, tenant 1", func(f *Fixture) string { return f.App1 }, []string{"tenant_admin"},
		[]string{"owner_1", "ta_1", "ta_home1_dev2", "ta_home1_grant3", "ta_nt_1b", "ta_ws1b"}},
	{"named tenant_admin, tenant-level app", func(f *Fixture) string { return f.AppT }, []string{"tenant_admin"},
		[]string{"owner_1", "ta_1", "ta_home1_dev2", "ta_home1_grant3", "ta_nt_1b", "ta_ws1b"}},
	// An application or model grant is no tier (ta_grant2, ta_model2,
	// ta_home1_grant3), and a developer role in tenant 2 makes an
	// administrator of tenant 1 no administrator here (ta_home1_dev2).
	{"named tenant_admin, tenant 2", func(f *Fixture) string { return f.App3 }, []string{"tenant_admin"},
		[]string{"ta_2", "ta_ws2_nt_grant1"}},
	{"named platform_admin", func(f *Fixture) string { return f.App3 }, []string{"platform_admin"},
		[]string{"pa", "pa_ws1a"}},
	{"named platform_admin, workspace app", func(f *Fixture) string { return f.App1 }, []string{"platform_admin"},
		[]string{"pa", "pa_ws1a"}},
	// ta_ws2_nt_grant1's App1 grant is no reach: it holds tenant_admin only
	// inside a workspace of tenant 2.
	{"no role named, workspace app", func(f *Fixture) string { return f.App1 }, nil,
		[]string{"appr_1a", "ba_1a", "ba_dev_global", "bu_1a", "dev_global", "dev_home1", "dev_ws1b", "imp_1a",
			"owner_1", "pa", "pa_ws1a", "ta_1", "ta_home1_dev2", "ta_home1_grant3", "ta_nt_1b", "ta_ws1b"}},
	{"no role named, tenant-level app", func(f *Fixture) string { return f.AppT }, nil,
		[]string{"appr_1a", "appr_1b", "ba_1a", "ba_1b", "ba_dev_global", "bu_1a", "dev_global", "dev_home1", "dev_ws1b",
			"imp_1a", "owner_1", "pa", "pa_ws1a", "ta_1", "ta_home1_dev2", "ta_home1_grant3", "ta_nt_1b", "ta_ws1b",
			"ta_ws2_nt_grant1"}},
	// ta_grant2 and ta_model2 reach App3 through their grants (they have no
	// tenant); ta_home1_grant3, which has one, does not: its grant narrows,
	// it opens nothing.
	{"no role named, tenant 2", func(f *Fixture) string { return f.App3 }, nil,
		[]string{"appr_2", "ba_2", "ba_dev_global", "dev_global", "dev_home2", "dev_ws2", "pa", "pa_ws1a", "ta_2",
			"ta_grant2", "ta_home1_dev2", "ta_model2", "ta_ws2_nt_grant1"}},
}

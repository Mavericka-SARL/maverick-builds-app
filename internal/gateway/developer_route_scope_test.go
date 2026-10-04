package gateway

// A developer-only route (dev, developerRouteKey) builds only where the
// caller holds a developer grant. The dev guard asks whether the account
// holds developer in SOME scope, and the resource checks behind it
// (requireResourceAccess, the member reorder, resolveDemoModelID,
// resolveDemoAppID) used to take the tenant_admin arm first — the admin
// scope — so an account that is a developer in tenant 1 and tenant_admin of
// a tenant-2 workspace reordered, renamed and created in tenant 2's model,
// where it holds no developer role. The developer-or-administrator routes
// (devOrAdm) and the administrator routes keep the admin scope.
//
// These reuse setupWSFixture (workspace_scope_security_test.go): tenant 1
// has ws1a (app1, models A and B), ws1b (app2) and the tenant-level appT;
// tenant 2 has ws2 (app3, model D).

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

type devRouteFixture struct {
	*wsFixture
	dimA, dimD   string // the Region dimension of model A (tenant 1) and D (tenant 2)
	memberD2     string // a second member of dimD, after memberD
	memberD      string
	connD        string // an integration connection of app3
	crossSubs    []string
	crossOwnSubs map[string]string // account → a tenant-1 metric its developer grant reaches
}

func setupDevRouteFixture(t *testing.T) *devRouteFixture {
	t.Helper()
	f := &devRouteFixture{wsFixture: setupWSFixture(t)}
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return s
	}
	f.dimA = q(`SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid`, f.modelA)
	f.dimD = q(`SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid`, f.modelD)
	f.memberD = q(`UPDATE model.dimension_member SET sort_order=1 WHERE dimension_id=$1::uuid RETURNING id::text`, f.dimD)
	f.memberD2 = q(`INSERT INTO model.dimension_member (dimension_id, code, label, sort_order) VALUES ($1::uuid, 'MD2', 'Member D2', 2) RETURNING id::text`, f.dimD)
	f.connD = q(`INSERT INTO model.integration_connection (application_id, name, auth_type) VALUES ($1::uuid, 'Tenant two connection', 'none') RETURNING id::text`, f.app3)
	// Make tenant 2's model the first pick of every "no application named"
	// ordering over both tenants (the application's default, then the most
	// metrics): an admin-scope resolution then lands in tenant 2.
	q(`UPDATE core.application SET default_model_id=$1::uuid WHERE id=$2::uuid RETURNING id::text`, f.modelD, f.app3)
	q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule) VALUES ($1::uuid, $2::uuid, 'Cost D', true, 'sum') RETURNING id::text`, f.modelD, f.revD)

	user := func(sub, cust string, grants ...[2]string) {
		t.Helper()
		id := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, $1||'@dr.test', $1, $2::uuid) RETURNING id::text`, sub, cust)
		for _, g := range grants {
			if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, $2::identity.user_role, NULLIF($3,'')::uuid)`, id, g[0], g[1]); err != nil {
				t.Fatalf("grant %s@%q to %s: %v", g[0], g[1], sub, err)
			}
		}
	}
	// Accounts of tenant 1, developers there, and tenant_admin of tenant 2's
	// workspace: with an unscoped developer grant (a developer of the
	// account's own tenant) and with one scoped to ws1a.
	user("dr-dev1-ta2", f.cust1, [2]string{"developer", ""}, [2]string{"tenant_admin", f.ws2})
	user("dr-devws-ta2", f.cust1, [2]string{"developer", f.ws1a}, [2]string{"tenant_admin", f.ws2})
	f.crossSubs = []string{"dr-dev1-ta2", "dr-devws-ta2"}
	// A developer is tenant-wide: each reaches ws1b's metric too.
	f.crossOwnSubs = map[string]string{"dr-dev1-ta2": f.metricA, "dr-devws-ta2": f.metricC}
	return f
}

func (f *devRouteFixture) one(t *testing.T, sql string, args ...any) string {
	t.Helper()
	var s string
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&s); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return s
}

// The live case: a tenant_admin grant in tenant 2 gives a developer of
// tenant 1 no builder reach in tenant 2 — the reorder, a dimension rename, a
// metric edit, creates in tenant 2 as the current model or application, and
// the first model or application picked when none is named.
func TestDeveloperRoutesIgnoreAdminScopeElsewhere(t *testing.T) {
	for _, sub := range []string{"dr-dev1-ta2", "dr-devws-ta2"} {
		t.Run(sub, func(t *testing.T) {
			f := setupDevRouteFixture(t)
			for _, c := range []struct {
				method, path, app string
				body              any
				want              int
			}{
				// The reorder answers 404 for a dimension outside the
				// caller's scope, as for an unknown one.
				{"PUT", "/api/developer/dimensions/" + f.dimD + "/members/order", "", map[string]any{"member_ids": []string{f.memberD2, f.memberD}}, http.StatusNotFound},
				{"PATCH", "/api/developer/dimensions/" + f.dimD, "", map[string]string{"name": "Hijacked dimension"}, http.StatusForbidden},
				{"PATCH", "/api/developer/metrics/" + f.metricD, "", map[string]string{"name": "Hijacked metric"}, http.StatusForbidden},
				{"POST", "/api/developer/metrics", f.app3, map[string]any{"name": "Planted metric", "is_input": true, "agg_rule": "sum", "revision_id": f.revD}, http.StatusForbidden},
				{"POST", "/api/developer/dimensions", f.app3, map[string]any{"name": "Planted dimension", "revision_id": f.revD}, http.StatusForbidden},
				{"GET", "/api/developer/model", f.app3, nil, http.StatusForbidden},
				{"GET", "/api/developer/integration-connections", f.app3, nil, http.StatusForbidden},
				{"POST", "/api/developer/integration-connections", f.app3, map[string]any{"name": "Planted connection", "auth_type": "none"}, http.StatusForbidden},
			} {
				if code, raw := f.do(t, c.method, c.path, sub, c.app, "", c.body); code != c.want {
					t.Errorf("%s %s (app %q) as %s: status %d %s, want %d", c.method, c.path, c.app, sub, code, raw, c.want)
				}
			}

			if got := f.one(t, `SELECT string_agg(code, ',' ORDER BY sort_order, code) FROM model.dimension_member WHERE dimension_id=$1::uuid`, f.dimD); got != "MD,MD2" {
				t.Errorf("tenant 2's member order after a refused reorder: %s, want MD,MD2", got)
			}
			if got := f.one(t, `SELECT name FROM model.dimension_def WHERE id=$1::uuid`, f.dimD); got != "Region D" {
				t.Errorf("tenant 2's dimension after a refused rename: %q, want \"Region D\"", got)
			}
			if got := f.one(t, `SELECT name FROM model.metric_def WHERE id=$1::uuid`, f.metricD); got != "Sales D" {
				t.Errorf("tenant 2's metric after a refused edit: %q, want \"Sales D\"", got)
			}
			if got := f.one(t, `SELECT count(*)::text FROM (
				SELECT id FROM model.metric_def WHERE name='Planted metric'
				UNION ALL SELECT id FROM model.dimension_def WHERE name='Planted dimension'
				UNION ALL SELECT id FROM model.integration_connection WHERE name='Planted connection') p`); got != "0" {
				t.Errorf("refused creates left %s rows behind in tenant 2", got)
			}

			// With no application named, the model and the application
			// resolved are the developer's, in tenant 1 — not tenant 2's,
			// which every ordering over both tenants would pick first.
			code, raw := f.do(t, "GET", "/api/developer/model", sub, "", "", nil)
			if code != http.StatusOK {
				t.Fatalf("model with no application named: status %d %s", code, raw)
			}
			var model struct {
				ModelID string `json:"model_id"`
			}
			if err := json.Unmarshal(raw, &model); err != nil {
				t.Fatalf("decode model: %v", err)
			}
			if cust := f.one(t, `SELECT app.customer_id::text FROM core.model m JOIN core.application app ON app.id=m.application_id WHERE m.id=$1::uuid`, model.ModelID); cust != f.cust1 {
				t.Errorf("model resolved with no application named is %s, of tenant %s; want a tenant-1 model", model.ModelID, cust)
			}
			code, raw = f.do(t, "GET", "/api/developer/integration-connections", sub, "", "", nil)
			if code != http.StatusOK {
				t.Fatalf("connections with no application named: status %d %s", code, raw)
			}
			if strings.Contains(string(raw), f.connD) {
				t.Errorf("connections with no application named list tenant 2's connection: %s", raw)
			}
		})
	}
}

// What must keep working: each cross account still builds in its own tenant
// and keeps its admin reach in tenant 2 on the developer-or-administrator
// and administrator routes; the sign-up owner (tenant_admin and developer
// unscoped) builds across its tenant; a plain developer is tenant-wide; a
// tenant admin keeps the devOrAdm routes.
func TestDeveloperRoutesKeepLegitimateReach(t *testing.T) {
	f := setupDevRouteFixture(t)
	type call struct {
		sub, method, path, app string
		body                   any
	}
	calls := []call{
		// The sign-up owner's shape (ws-owner): tenant_admin and developer
		// unscoped, business_admin in ws1a.
		{"ws-owner", "PUT", "/api/developer/dimensions/" + f.dimA + "/members/order", "", map[string]any{"member_ids": []string{f.memberA}}},
		{"ws-owner", "PATCH", "/api/developer/dimensions/" + f.dimA, "", map[string]string{"name": "Region A"}},
		{"ws-owner", "PATCH", "/api/developer/metrics/" + f.metricC, "", map[string]string{"name": "Sales C"}},
		{"ws-owner", "POST", "/api/developer/metrics", f.app1, map[string]any{"name": "owner_metric", "is_input": true, "agg_rule": "sum", "revision_id": f.revA}},
		{"ws-owner", "GET", "/api/developer/integration-connections", f.app2, nil},
		{"ws-owner", "GET", "/api/developer/integrations/google-service-account", f.app1, nil},
		// A plain developer of ws1a reaches every workspace of its tenant.
		{"mm-dev", "PUT", "/api/developer/dimensions/" + f.dimA + "/members/order", "", map[string]any{"member_ids": []string{f.memberA}}},
		{"mm-dev", "PATCH", "/api/developer/metrics/" + f.metricC, "", map[string]string{"name": "Sales C"}},
		{"mm-dev", "POST", "/api/developer/dimensions", f.app1, map[string]any{"name": "Dev dimension", "revision_id": f.revA}},
		{"mm-dev", "GET", "/api/developer/integration-connections", f.appT, nil},
		// A tenant admin (no developer grant) on a devOrAdm route.
		{"mm-tenant-admin", "GET", "/api/developer/integrations/google-service-account", f.app2, nil},
		{"mm-tenant-admin", "GET", "/api/developer/applications", "", nil},
	}
	for _, sub := range f.crossSubs {
		calls = append(calls,
			// Its developer grant builds in tenant 1.
			call{sub, "PUT", "/api/developer/dimensions/" + f.dimA + "/members/order", "", map[string]any{"member_ids": []string{f.memberA}}},
			call{sub, "PATCH", "/api/developer/metrics/" + f.crossOwnSubs[sub], "", map[string]string{"name": f.one(t, `SELECT name FROM model.metric_def WHERE id=$1::uuid`, f.crossOwnSubs[sub])}},
			call{sub, "POST", "/api/developer/metrics", f.app1, map[string]any{"name": "cross_metric_" + strings.ReplaceAll(sub, "-", "_"), "is_input": true, "agg_rule": "sum", "revision_id": f.revA}},
			// Its tenant_admin grant still administers tenant 2 on the
			// developer-or-administrator and administrator routes.
			call{sub, "GET", "/api/developer/integrations/google-service-account", f.app3, nil},
			call{sub, "PATCH", "/api/admin/applications/" + f.app3, "", map[string]string{"name": "App Three"}},
		)
	}
	for _, c := range calls {
		if code, raw := f.do(t, c.method, c.path, c.sub, c.app, "", c.body); code != http.StatusOK {
			t.Errorf("%s %s (app %q) as %s: status %d %s, want 200", c.method, c.path, c.app, c.sub, code, raw)
		}
	}
	for _, sub := range f.crossSubs {
		code, raw := f.do(t, "GET", "/api/developer/applications", sub, "", "", nil)
		if code != http.StatusOK {
			t.Fatalf("applications as %s: status %d %s", sub, code, raw)
		}
		if !strings.Contains(string(raw), f.app3) || !strings.Contains(string(raw), f.app1) {
			t.Errorf("applications listed to %s (devOrAdm) leave out tenant 1's or tenant 2's: %s", sub, raw)
		}
	}
	// The sign-up owner with no application named opens its own tenant.
	code, raw := f.do(t, "GET", "/api/developer/model", "ws-owner", "", "", nil)
	if code != http.StatusOK {
		t.Fatalf("model as the sign-up owner: status %d %s", code, raw)
	}
	var model struct {
		ModelID string `json:"model_id"`
	}
	if err := json.Unmarshal(raw, &model); err != nil {
		t.Fatalf("decode model: %v", err)
	}
	if model.ModelID != f.modelA {
		t.Errorf("sign-up owner's model with no application named: %s, want model A %s", model.ModelID, f.modelA)
	}
}

// A tenant admin that is a developer of the same tenant (the sign-up owner)
// keeps, on the developer-only routes, what its admin grant opens there
// (builderAdminScope): an application with no model — an execution-mode
// application never gets one — and applications outside its user_app_access
// grants, which narrow the developer arms and never narrowed the admin one.
// A tenant_admin grant in a tenant where the account is no developer still
// opens no application to build, model-less ones included.
func TestDeveloperRoutesKeepOwnerAdminReach(t *testing.T) {
	f := setupDevRouteFixture(t)
	appX := f.one(t, `INSERT INTO core.application (customer_id, name, mode) VALUES ($1::uuid, 'Execution only', 'execution') RETURNING id::text`, f.cust1)
	appX2 := f.one(t, `INSERT INTO core.application (customer_id, name, mode) VALUES ($1::uuid, 'Execution only two', 'execution') RETURNING id::text`, f.cust2)
	type call struct {
		sub, method, path, app string
		body                   any
		want                   int
	}
	calls := []call{
		{"ws-owner", "GET", "/api/developer/workflows?application_id=" + appX, "", nil, http.StatusOK},
		{"ws-owner", "POST", "/api/developer/workflows?application_id=" + appX, "", map[string]string{"name": "Owner workflow"}, http.StatusOK},
		{"ws-owner", "GET", "/api/developer/workflow-roles?application_id=" + appX, "", nil, http.StatusOK},
		{"ws-owner", "GET", "/api/developer/workflow-trigger-events?application_id=" + appX, "", nil, http.StatusOK},
		{"ws-owner", "POST", "/api/automation/rules", appX, map[string]string{"name": "Owner rule", "trigger_type": "manual"}, http.StatusOK},
	}
	for _, sub := range f.crossSubs {
		calls = append(calls,
			call{sub, "GET", "/api/developer/workflows?application_id=" + appX2, "", nil, http.StatusForbidden},
			call{sub, "POST", "/api/developer/workflows?application_id=" + appX2, "", map[string]string{"name": "Planted workflow"}, http.StatusForbidden},
			call{sub, "GET", "/api/developer/workflow-roles?application_id=" + appX2, "", nil, http.StatusForbidden},
			call{sub, "GET", "/api/developer/workflow-trigger-events?application_id=" + appX2, "", nil, http.StatusForbidden},
			call{sub, "POST", "/api/automation/rules", appX2, map[string]string{"name": "Planted rule", "trigger_type": "manual"}, http.StatusForbidden},
		)
	}
	for _, c := range calls {
		if code, raw := f.do(t, c.method, c.path, c.sub, c.app, "", c.body); code != c.want {
			t.Errorf("%s %s (app %q) as %s: status %d %s, want %d", c.method, c.path, c.app, c.sub, code, raw, c.want)
		}
	}
	if got := f.one(t, `SELECT count(*)::text FROM (
		SELECT id FROM workflow.workflow_def WHERE application_id=$1::uuid
		UNION ALL SELECT id FROM workflow.automation_rule WHERE application_id=$1::uuid) p`, appX2); got != "0" {
		t.Errorf("refused creates left %s rows behind in tenant 2's model-less application", got)
	}

	// An app grant narrows the owner's developer arms, not its admin reach
	// in its own tenant: it still builds in ws1b's application (app2), which
	// it administers.
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO identity.user_app_access (user_id, application_id)
		SELECT id, $1::uuid FROM identity.user WHERE keycloak_sub='ws-owner'`, f.app1); err != nil {
		t.Fatalf("grant app1 to ws-owner: %v", err)
	}
	for _, c := range []call{
		{"ws-owner", "PATCH", "/api/developer/metrics/" + f.metricC, "", map[string]string{"name": "Sales C"}, http.StatusOK},
		{"ws-owner", "PATCH", "/api/admin/applications/" + f.app2, "", map[string]string{"name": f.one(t, `SELECT name FROM core.application WHERE id=$1::uuid`, f.app2)}, http.StatusOK},
	} {
		if code, raw := f.do(t, c.method, c.path, c.sub, c.app, "", c.body); code != c.want {
			t.Errorf("%s %s as %s with an app1 grant: status %d %s, want %d", c.method, c.path, c.sub, code, raw, c.want)
		}
	}
}

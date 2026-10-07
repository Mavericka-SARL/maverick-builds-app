package gateway

// A tenant's application holds several models (sign-up gives "Getting
// started" four), and the console names the one it works in: X-Model-Id for
// the model picked under Business Admin/User › Models, ?revision_id= for the
// developer's working revision. These tests pin the endpoints that used to
// act on an arbitrary or default model instead — and the workflow history
// and status override, which acted on every tenant's instances.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// mmFixture: tenant 1 has workspaces ws1a (application app1, models A — the
// default — and B) and ws1b (app2), plus appT, a tenant-level application
// with no workspace (the shape POST /api/admin/applications creates); tenant
// 2 has ws2 (app3, model D).
type mmFixture struct {
	pool *pgxpool.Pool
	srv  *httptest.Server

	app1, app2, app3 string
	appT             string // tenant 1, workspace_id NULL
	modelA, modelB   string
	modelD           string
	revA, revB, revD string
	memberA, memberB string
	metricA, metricB string
	dashA, dashB     string
	roleID           string // business role in ws1a
	businessUserID   string // business_user in ws1a

	// Workflow instances: one per application, plus a test run in app1.
	inst1, inst1Test, inst2, inst3, instT string
}

func setupMMFixture(t *testing.T) *mmFixture {
	t.Helper()
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	f := &mmFixture{pool: pool}
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	cust1 := q(`INSERT INTO core.customer (name, plan) VALUES ('MM One', 'enterprise') RETURNING id::text`)
	cust2 := q(`INSERT INTO core.customer (name, plan) VALUES ('MM Two', 'enterprise') RETURNING id::text`)
	ws1a := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws1a') RETURNING id::text`, cust1)
	ws1b := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws1b') RETURNING id::text`, cust1)
	ws2 := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws2') RETURNING id::text`, cust2)
	app := func(ws, cust, name string) string {
		return q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, $3, 'planning') RETURNING id::text`, ws, cust, name)
	}
	f.app1, f.app2, f.app3 = app(ws1a, cust1, "App One"), app(ws1b, cust1, "App Two"), app(ws2, cust2, "App Three")
	f.appT = q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES (NULL, $1::uuid, 'App Tenant', 'planning') RETURNING id::text`, cust1)

	type built struct{ model, rev, member, metric, dash string }
	model := func(appID, name string) built {
		var b built
		b.model = q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, $2) RETURNING id::text`, appID, name)
		b.rev = q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Live') RETURNING id::text`, b.model)
		exec(`UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Live' WHERE id=$2::uuid`, b.rev, b.model)
		dim := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Region '||$3) RETURNING id::text`, b.model, b.rev, name)
		b.member = q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'M'||$2, 'Member '||$2) RETURNING id::text`, dim, name)
		b.metric = q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule) VALUES ($1::uuid, $2::uuid, 'Sales '||$3, true, 'sum') RETURNING id::text`, b.model, b.rev, name)
		b.dash = q(`INSERT INTO model.dashboard_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Dashboard '||$3) RETURNING id::text`, b.model, b.rev, name)
		return b
	}
	a, b, d := model(f.app1, "A"), model(f.app1, "B"), model(f.app3, "D")
	model(f.app2, "C")
	exec(`UPDATE core.application SET default_model_id=$1::uuid WHERE id=$2::uuid`, a.model, f.app1)
	// B is the newer model: a resolver that ignored the default and took the
	// newest would land on it, so "no header means A" is a real assertion.
	exec(`UPDATE core.model SET created_at = now() + interval '1 minute' WHERE id=$1::uuid`, b.model)
	f.modelA, f.revA, f.memberA, f.metricA, f.dashA = a.model, a.rev, a.member, a.metric, a.dash
	f.modelB, f.revB, f.memberB, f.metricB, f.dashB = b.model, b.rev, b.member, b.metric, b.dash
	f.modelD, f.revD = d.model, d.rev

	user := func(sub, cust string, roles map[string]string) string {
		id := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, $1||'@mm.test', $1, $2::uuid) RETURNING id::text`, sub, cust)
		for role, ws := range roles {
			exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, $2::identity.user_role, NULLIF($3,'')::uuid)`, id, role, ws)
		}
		return id
	}
	ba1 := user("mm-ba1", cust1, map[string]string{"business_admin": ws1a})
	user("mm-ba1b", cust1, map[string]string{"business_admin": ws1b})
	ba2 := user("mm-ba2", cust2, map[string]string{"business_admin": ws2})
	user("mm-tenant-admin", cust1, map[string]string{"tenant_admin": "", "business_admin": ws1a})
	user("mm-dev", cust1, map[string]string{"developer": ws1a})
	f.businessUserID = user("mm-user", cust1, map[string]string{"business_user": ws1a})
	// Business admin of both tenant-1 workspaces, narrowed by user_app_access
	// to app2.
	narrow := user("mm-ba-narrow", cust1, map[string]string{"business_admin": ws1a})
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_admin', $2::uuid)`, narrow, ws1b)
	exec(`INSERT INTO identity.user_app_access (user_id, application_id) VALUES ($1::uuid, $2::uuid)`, narrow, f.app2)
	// A business_admin grant with no workspace — which a tenant 1 admin can
	// make — plus a plain business_user role in tenant 2. The unscoped grant
	// is inert: it must not reach tenant 1's nor tenant 2's workflows.
	user("mm-ba-unscoped", cust1, map[string]string{"business_admin": "", "business_user": ws2})
	user("mm-platform", cust1, map[string]string{"platform_admin": "", "business_admin": ws1a})
	f.roleID = q(`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Reviewers') RETURNING id::text`, ws1a)

	wfDef := func(appID string) string {
		return q(`INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps, status, published_at)
		          VALUES ($1::uuid, 'Approval', 'manual', '[]'::jsonb, 'published', now()) RETURNING id::text`, appID)
	}
	instance := func(defID, startedBy string, testRun bool) string {
		return q(`INSERT INTO workflow.workflow_instance (workflow_def_id, started_by, test_run) VALUES ($1::uuid, $2::uuid, $3) RETURNING id::text`, defID, startedBy, testRun)
	}
	def1, def2, def3, defT := wfDef(f.app1), wfDef(f.app2), wfDef(f.app3), wfDef(f.appT)
	f.inst1, f.inst1Test = instance(def1, ba1, false), instance(def1, ba1, true)
	f.inst2 = instance(def2, ba1, false)
	f.inst3 = instance(def3, ba2, false)
	f.instT = instance(defT, ba1, false)

	t.Setenv("DEV_MODE", "true")
	f.srv = httptest.NewServer(NewHandler(logger.New("test"), pool, nil))
	t.Cleanup(f.srv.Close)
	return f
}

// do sends a request as dev persona sub, naming appID and modelID in the
// headers when they are not empty.
func (f *mmFixture) do(t *testing.T, method, path, sub, appID, modelID string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader = strings.NewReader("")
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode body: %v", err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, f.srv.URL+path, rd)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Dev-User", sub)
	req.Header.Set("Content-Type", "application/json")
	if appID != "" {
		req.Header.Set("X-App-Id", appID)
	}
	if modelID != "" {
		req.Header.Set("X-Model-Id", modelID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, raw
}

// ids decodes a JSON array of objects and returns their sorted "id"s.
func mmIDs(t *testing.T, raw []byte) []string {
	t.Helper()
	var items []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.ID)
	}
	sort.Strings(out)
	return out
}

func mmSorted(ids ...string) []string {
	out := append([]string{}, ids...)
	sort.Strings(out)
	return out
}

func mmEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (f *mmFixture) column(t *testing.T, sql string, args ...any) []string {
	t.Helper()
	rows, err := f.pool.Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, s)
	}
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out
}

func TestBusinessAdminWorksInTheSelectedModel(t *testing.T) {
	f := setupMMFixture(t)
	const ba = "mm-ba1"

	t.Run("pickers list the selected model, else the default", func(t *testing.T) {
		// Another tenant's model is refused (404 MODEL_NOT_OPEN): it used to
		// be replaced by the default without a word.
		if status, raw := f.do(t, "GET", "/api/business-admin/available?type=dashboards", ba, f.app1, f.modelD, nil); status != http.StatusNotFound || !strings.Contains(string(raw), "MODEL_NOT_OPEN") {
			t.Errorf("dashboards with another tenant's X-Model-Id: status %d %s, want 404 MODEL_NOT_OPEN", status, raw)
		}
		for _, tc := range []struct {
			kind, model string
			want        string
		}{
			{"dashboards", "", f.dashA},
			{"dashboards", f.modelB, f.dashB},
			{"dimension_members", "", f.memberA},
			{"dimension_members", f.modelB, f.memberB},
			{"metrics", f.modelB, f.metricB},
		} {
			status, raw := f.do(t, "GET", "/api/business-admin/available?type="+tc.kind, ba, f.app1, tc.model, nil)
			if status != http.StatusOK {
				t.Fatalf("%s with X-Model-Id %q: status %d %s", tc.kind, tc.model, status, raw)
			}
			if got := mmIDs(t, raw); !mmEqual(got, []string{tc.want}) {
				t.Errorf("%s with X-Model-Id %q = %v, want [%s]", tc.kind, tc.model, got, tc.want)
			}
		}
	})

	t.Run("saving one model's dashboard grants keeps the other model's", func(t *testing.T) {
		path := "/api/business-admin/roles/" + f.roleID + "/dashboards"
		grants := func() []string {
			return f.column(t, `SELECT dashboard_id::text FROM identity.business_role_dashboard WHERE role_id=$1::uuid`, f.roleID)
		}
		steps := []struct {
			model string
			ids   []string
			want  []string
		}{
			{"", []string{f.dashA}, mmSorted(f.dashA)},
			{f.modelB, []string{f.dashB}, mmSorted(f.dashA, f.dashB)},
			{f.modelB, []string{}, mmSorted(f.dashA)},
			{f.modelB, []string{f.dashA, f.dashB}, mmSorted(f.dashA, f.dashB)}, // the console sends the whole list back
			{"", []string{f.dashB}, mmSorted(f.dashB)},
			{"", []string{}, mmSorted(f.dashB)},
			{f.modelB, []string{}, []string{}},
		}
		for i, s := range steps {
			if status, raw := f.do(t, "PUT", path, ba, f.app1, s.model, map[string]any{"dashboard_ids": s.ids}); status != http.StatusOK {
				t.Fatalf("step %d: status %d %s", i, status, raw)
			}
			if got := grants(); !mmEqual(got, s.want) {
				t.Errorf("step %d (X-Model-Id %q, ids %v): grants = %v, want %v", i, s.model, s.ids, got, s.want)
			}
		}
	})

	t.Run("saving one model's access rules keeps the other model's", func(t *testing.T) {
		path := "/api/business-admin/users/" + f.businessUserID + "/access-rules"
		rules := func() []string {
			return f.column(t, `SELECT rule_type||':'||ref_id||':'||access FROM identity.user_access_rule WHERE user_id=$1::uuid`, f.businessUserID)
		}
		rule := func(kind, ref, access string) map[string]string {
			return map[string]string{"rule_type": kind, "ref_id": ref, "access": access}
		}
		ruleA := "dimension_member:" + f.memberA + ":hidden"
		ruleB := "dimension_member:" + f.memberB + ":read"
		metricB := "metric:" + f.metricB + ":hidden"
		steps := []struct {
			model string
			rules []map[string]string
			want  []string
		}{
			{"", []map[string]string{rule("dimension_member", f.memberA, "hidden")}, mmSorted(ruleA)},
			{f.modelB, []map[string]string{rule("dimension_member", f.memberB, "read"), rule("metric", f.metricB, "hidden")}, mmSorted(ruleA, ruleB, metricB)},
			{f.modelB, []map[string]string{rule("metric", f.metricB, "hidden")}, mmSorted(ruleA, metricB)},
			{f.modelB, []map[string]string{}, mmSorted(ruleA)},
			{"", []map[string]string{}, []string{}},
		}
		for i, s := range steps {
			if status, raw := f.do(t, "PUT", path, ba, f.app1, s.model, map[string]any{"rules": s.rules}); status != http.StatusOK {
				t.Fatalf("step %d: status %d %s", i, status, raw)
			}
			if got := rules(); !mmEqual(got, s.want) {
				t.Errorf("step %d (X-Model-Id %q): rules = %v, want %v", i, s.model, got, s.want)
			}
		}

		// A rule stored against an older revision's row of model B belongs
		// to B by lineage even once that row is gone: saving model A's
		// rules keeps it.
		ctx := context.Background()
		oldRev := f.column(t, `INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Old B') RETURNING id::text`, f.modelB)[0]
		oldMetric := f.column(t, `
			INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule, lineage_id)
			SELECT model_id, $2::uuid, name, is_input, agg_rule, lineage_id FROM model.metric_def WHERE id=$1::uuid
			RETURNING id::text`, f.metricB, oldRev)[0]
		if status, raw := f.do(t, "PUT", path, ba, f.app1, f.modelB, map[string]any{"rules": []map[string]string{rule("metric", oldMetric, "hidden")}}); status != http.StatusOK {
			t.Fatalf("save old-revision rule under B: status %d %s", status, raw)
		}
		if _, err := f.pool.Exec(ctx, `DELETE FROM model.metric_def WHERE id=$1::uuid`, oldMetric); err != nil {
			t.Fatalf("delete old-revision metric: %v", err)
		}
		oldRule := "metric:" + oldMetric + ":hidden"
		if status, raw := f.do(t, "PUT", path, ba, f.app1, "", map[string]any{"rules": []map[string]string{rule("dimension_member", f.memberA, "hidden")}}); status != http.StatusOK {
			t.Fatalf("save A's rules: status %d %s", status, raw)
		}
		if got, want := rules(), mmSorted(ruleA, oldRule); !mmEqual(got, want) {
			t.Errorf("after saving A's rules: rules = %v, want %v (B's rule kept by lineage)", got, want)
		}
		if status, raw := f.do(t, "PUT", path, ba, f.app1, f.modelB, map[string]any{"rules": []map[string]string{}}); status != http.StatusOK {
			t.Fatalf("clear B's rules: status %d %s", status, raw)
		}
		if got, want := rules(), mmSorted(ruleA); !mmEqual(got, want) {
			t.Errorf("after clearing B's rules: rules = %v, want %v", got, want)
		}
	})
}

// The developer's Roles tab edits the grants of its working revision's
// dashboards: unticking one there removes the grant (it used to be kept,
// silently, because the save only looked at the active revision's
// dashboards), and leaves the active revision's grants alone.
func TestRoleDashboardGrantsFollowTheEditedRevision(t *testing.T) {
	f := setupMMFixture(t)
	const dev = "mm-dev"
	draft := f.column(t, `INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Draft A') RETURNING id::text`, f.modelA)[0]
	dashDraft := f.column(t, `INSERT INTO model.dashboard_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Dashboard A draft') RETURNING id::text`, f.modelA, draft)[0]
	for _, d := range []string{f.dashA, f.dashB, dashDraft} {
		if _, err := f.pool.Exec(context.Background(), `INSERT INTO identity.business_role_dashboard (role_id, dashboard_id) VALUES ($1::uuid, $2::uuid)`, f.roleID, d); err != nil {
			t.Fatalf("grant %s: %v", d, err)
		}
	}
	grants := func() []string {
		return f.column(t, `SELECT dashboard_id::text FROM identity.business_role_dashboard WHERE role_id=$1::uuid`, f.roleID)
	}
	path := "/api/business-admin/roles/" + f.roleID + "/dashboards"
	for i, s := range []struct {
		rev  string
		ids  []string
		code int
		want []string
	}{
		{f.revD, []string{}, http.StatusNotFound, mmSorted(f.dashA, f.dashB, dashDraft)},           // another tenant's revision
		{"not-a-revision", []string{}, http.StatusNotFound, mmSorted(f.dashA, f.dashB, dashDraft)}, // unknown
		{draft, []string{f.dashA, f.dashB}, http.StatusOK, mmSorted(f.dashA, f.dashB)},             // unticked in the draft
		{draft, []string{dashDraft}, http.StatusOK, mmSorted(f.dashA, f.dashB, dashDraft)},         // ticked again; A's live grant kept
		{f.revB, []string{}, http.StatusOK, mmSorted(f.dashA, dashDraft)},                          // B's revision pins model B
		{"", []string{}, http.StatusOK, mmSorted(dashDraft)},                                       // no revision: the default model's active one
	} {
		code, raw := f.do(t, "PUT", path+"?revision_id="+s.rev, dev, f.app1, "", map[string]any{"dashboard_ids": s.ids})
		if code != s.code {
			t.Fatalf("step %d (revision %q): status %d %s, want %d", i, s.rev, code, raw, s.code)
		}
		if got := grants(); !mmEqual(got, s.want) {
			t.Errorf("step %d (revision %q, ids %v): grants = %v, want %v", i, s.rev, s.ids, got, s.want)
		}
	}
}

func TestWorkflowHistoryAndOverrideStayInScope(t *testing.T) {
	f := setupMMFixture(t)

	history := func(sub, appID string) []string {
		t.Helper()
		status, raw := f.do(t, "GET", "/api/workflow/history", sub, appID, "", nil)
		if status != http.StatusOK {
			t.Fatalf("history as %s (app %q): status %d %s", sub, appID, status, raw)
		}
		return mmIDs(t, raw)
	}
	for _, tc := range []struct {
		sub, app string
		want     []string
	}{
		{"mm-ba1", f.app1, mmSorted(f.inst1)},
		{"mm-ba1", "", mmSorted(f.inst1, f.instT)}, // tenant-level app: any workspace of its tenant
		{"mm-ba1", f.appT, mmSorted(f.instT)},
		{"mm-ba1", f.app2, []string{}}, // same tenant, another workspace
		{"mm-ba1", f.app3, []string{}}, // another tenant
		{"mm-ba1b", f.app2, mmSorted(f.inst2)},
		{"mm-ba1b", "", mmSorted(f.inst2, f.instT)},
		{"mm-ba2", "", mmSorted(f.inst3)}, // not tenant 1's tenant-level app
		{"mm-ba2", f.appT, []string{}},
		{"mm-tenant-admin", "", mmSorted(f.inst1, f.inst2, f.instT)},
		{"mm-ba-narrow", "", mmSorted(f.inst2)}, // user_app_access: app2 only
		{"mm-ba-unscoped", "", []string{}},      // an unscoped business_admin grant is inert
		{"mm-platform", "", mmSorted(f.inst1, f.inst2, f.inst3, f.instT)},
	} {
		if got := history(tc.sub, tc.app); !mmEqual(got, tc.want) {
			t.Errorf("history as %s (app %q) = %v, want %v", tc.sub, tc.app, got, tc.want)
		}
	}

	status := func(id string) string {
		t.Helper()
		var s string
		if err := f.pool.QueryRow(context.Background(), `SELECT status::text FROM workflow.workflow_instance WHERE id=$1::uuid`, id).Scan(&s); err != nil {
			t.Fatalf("instance %s: %v", id, err)
		}
		return s
	}
	for _, tc := range []struct{ name, id string }{
		{"another workspace's instance", f.inst2},
		{"another tenant's instance", f.inst3},
		{"a developer's test run", f.inst1Test},
	} {
		code, raw := f.do(t, "PATCH", "/api/workflow/instances/"+tc.id, "mm-ba1", f.app1, "", map[string]string{"status": "cancelled"})
		if code != http.StatusNotFound {
			t.Errorf("override on %s: status %d %s, want 404", tc.name, code, raw)
		}
		if got := status(tc.id); got != "running" {
			t.Errorf("override on %s was refused but changed its status to %q", tc.name, got)
		}
	}
	for _, tc := range []struct{ sub, id, name string }{
		{"mm-ba-unscoped", f.inst3, "the tenant where it is only a business user"},
		{"mm-ba-unscoped", f.inst1, "its own tenant"},
		{"mm-ba-unscoped", f.instT, "its own tenant's tenant-level app"},
		{"mm-ba-narrow", f.inst1, "an application user_app_access leaves out"},
		{"mm-ba2", f.instT, "another tenant's tenant-level app"},
	} {
		code, raw := f.do(t, "PATCH", "/api/workflow/instances/"+tc.id, tc.sub, "", "", map[string]string{"status": "cancelled"})
		if code != http.StatusNotFound {
			t.Errorf("override as %s in %s: status %d %s, want 404", tc.sub, tc.name, code, raw)
		}
		if got := status(tc.id); got != "running" {
			t.Errorf("override as %s in %s was refused but changed its status to %q", tc.sub, tc.name, got)
		}
	}
	for _, id := range []string{"00000000-0000-0000-0000-000000000000", "not-a-uuid"} {
		if code, raw := f.do(t, "PATCH", "/api/workflow/instances/"+id, "mm-ba1", f.app1, "", map[string]string{"status": "cancelled"}); code != http.StatusNotFound {
			t.Errorf("override on unknown instance %q: status %d %s, want 404", id, code, raw)
		}
	}
	if code, raw := f.do(t, "PATCH", "/api/workflow/instances/"+f.inst1, "mm-ba1", f.app1, "", map[string]string{"status": "cancelled"}); code != http.StatusOK {
		t.Fatalf("override on own instance: status %d %s", code, raw)
	}
	if got := status(f.inst1); got != "cancelled" {
		t.Errorf("own instance status after override = %q, want cancelled", got)
	}
	// The tenant admin administers both of its workspaces.
	if code, raw := f.do(t, "PATCH", "/api/workflow/instances/"+f.inst2, "mm-tenant-admin", "", "", map[string]string{"status": "cancelled"}); code != http.StatusOK {
		t.Errorf("tenant admin override in its second workspace: status %d %s", code, raw)
	}
	if code, raw := f.do(t, "PATCH", "/api/workflow/instances/"+f.inst3, "mm-tenant-admin", "", "", map[string]string{"status": "cancelled"}); code != http.StatusNotFound {
		t.Errorf("tenant admin override in another tenant: status %d %s, want 404", code, raw)
	}
	if code, raw := f.do(t, "PATCH", "/api/workflow/instances/"+f.instT, "mm-ba1b", "", "", map[string]string{"status": "cancelled"}); code != http.StatusOK {
		t.Errorf("override on its tenant's tenant-level app: status %d %s", code, raw)
	}
	if code, raw := f.do(t, "PATCH", "/api/workflow/instances/"+f.inst3, "mm-platform", "", "", map[string]string{"status": "cancelled"}); code != http.StatusOK {
		t.Errorf("platform admin override in any tenant: status %d %s", code, raw)
	}
	if got := status(f.inst3); got != "cancelled" {
		t.Errorf("platform admin override left status %q, want cancelled", got)
	}
}

func TestNewRevisionIsMadeInTheSourceRevisionsModel(t *testing.T) {
	f := setupMMFixture(t)
	const dev = "mm-dev"
	create := func(query, model string, body map[string]any) (int, string, []byte) {
		t.Helper()
		code, raw := f.do(t, "POST", "/api/developer/revisions"+query, dev, f.app1, model, body)
		var res struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &res)
		return code, res.ID, raw
	}
	modelOf := func(revID string) string {
		t.Helper()
		var m string
		if err := f.pool.QueryRow(context.Background(), `SELECT model_id::text FROM model.revision WHERE id=$1::uuid`, revID).Scan(&m); err != nil {
			t.Fatalf("revision %s: %v", revID, err)
		}
		return m
	}
	metricsOf := func(revID string) []string {
		return f.column(t, `SELECT name FROM model.metric_def WHERE revision_id=$1::uuid`, revID)
	}
	revisions := func() int {
		var n int
		_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM model.revision`).Scan(&n)
		return n
	}

	// A copy of B's revision, with no model named: made in B, from B.
	code, id, raw := create("", "", map[string]any{"name": "B copy", "source_revision_id": f.revB})
	if code != http.StatusOK {
		t.Fatalf("copy of B's revision: status %d %s", code, raw)
	}
	if got := modelOf(id); got != f.modelB {
		t.Errorf("copy of B's revision was made in model %s, want B %s", got, f.modelB)
	}
	if got := metricsOf(id); !mmEqual(got, []string{"Sales B"}) {
		t.Errorf("copy of B's revision holds metrics %v, want [Sales B]", got)
	}

	// Named model and source agree.
	if code, id, raw := create("?model_id="+f.modelB, "", map[string]any{"name": "B copy 2", "source_revision_id": f.revB}); code != http.StatusOK || modelOf(id) != f.modelB {
		t.Errorf("copy of B's revision with model_id=B: status %d %s", code, raw)
	}

	// Named model and source disagree: refused, nothing created.
	before := revisions()
	if code, _, raw := create("?model_id="+f.modelA, "", map[string]any{"name": "Mixed", "source_revision_id": f.revB}); code != http.StatusBadRequest {
		t.Errorf("model_id=A with B's revision as source: status %d %s, want 400", code, raw)
	}
	// Another tenant's revision, and one that does not exist: not found.
	for _, src := range []string{f.revD, "00000000-0000-0000-0000-000000000000"} {
		if code, _, raw := create("", "", map[string]any{"name": "Foreign", "source_revision_id": src}); code != http.StatusNotFound {
			t.Errorf("source %s outside the caller's application: status %d %s, want 404", src, code, raw)
		}
	}
	if after := revisions(); after != before {
		t.Errorf("refused requests created %d revision(s)", after-before)
	}

	// Neither named: the selected model (X-Model-Id), else the default.
	if code, id, raw := create("", "", map[string]any{"name": "Default copy"}); code != http.StatusOK || modelOf(id) != f.modelA {
		t.Errorf("no model, no source: status %d %s, want a revision of the default model A", code, raw)
	} else if got := metricsOf(id); !mmEqual(got, []string{"Sales A"}) {
		t.Errorf("no model, no source: copy holds %v, want A's live revision [Sales A]", got)
	}
	if code, id, raw := create("", f.modelB, map[string]any{"name": "Selected copy"}); code != http.StatusOK || modelOf(id) != f.modelB {
		t.Errorf("X-Model-Id B, no source: status %d %s, want a revision of B", code, raw)
	}
}

// The Triggers tab lists and creates rules against the developer's working
// revision (?revision_id=); every endpoint it uses must read and write that
// revision's model, not the application's default.
func TestTriggersTabEndpointsHonourTheWorkingRevision(t *testing.T) {
	f := setupMMFixture(t)
	const dev = "mm-dev"
	onB := "?revision_id=" + f.revB
	call := func(method, path string, body any) []byte {
		t.Helper()
		code, raw := f.do(t, method, path, dev, f.app1, "", body)
		if code != http.StatusOK {
			t.Fatalf("%s %s: status %d %s", method, path, code, raw)
		}
		return raw
	}
	idOf := func(raw []byte) string {
		var res struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &res)
		if res.ID == "" {
			t.Fatalf("no id in %s", raw)
		}
		return res.ID
	}
	scopeOf := func(table, id string) string {
		t.Helper()
		var model, rev string
		var sql string
		switch table {
		case "workflow.automation_rule", "workflow.workflow_def":
			sql = `SELECT '', COALESCE(revision_id::text,'') FROM ` + table + ` WHERE id=$1::uuid`
		default:
			sql = `SELECT model_id::text, COALESCE(revision_id::text,'') FROM ` + table + ` WHERE id=$1::uuid`
		}
		if err := f.pool.QueryRow(context.Background(), sql, id).Scan(&model, &rev); err != nil {
			t.Fatalf("%s %s: %v", table, id, err)
		}
		return model + "/" + rev
	}
	contains := func(raw []byte, id string) bool { return bytes.Contains(raw, []byte(id)) }

	gridID := idOf(call("POST", "/api/developer/grids"+onB, map[string]any{"name": "Plan B"}))
	formID := idOf(call("POST", "/api/forms"+onB, map[string]any{"name": "request_b", "label": "Request B", "fields": []any{}}))
	integrationID := idOf(call("POST", "/api/developer/integrations"+onB, map[string]any{"name": "Feed B", "type": "csv_import"}))
	workflowID := idOf(call("POST", "/api/developer/workflows?application_id="+f.app1+"&revision_id="+f.revB, map[string]any{"name": "Flow B"}))
	ruleID := idOf(call("POST", "/api/automation/rules"+onB, map[string]any{"name": "Rule B", "trigger_type": "manual"}))

	for _, tc := range []struct{ table, id, want string }{
		{"model.grid_def", gridID, f.modelB + "/" + f.revB},
		{"model.form_def", formID, f.modelB + "/" + f.revB},
		{"model.integration_def", integrationID, f.modelB + "/" + f.revB},
		{"workflow.workflow_def", workflowID, "/" + f.revB},
		{"workflow.automation_rule", ruleID, "/" + f.revB},
	} {
		if got := scopeOf(tc.table, tc.id); got != tc.want {
			t.Errorf("%s created with ?revision_id=B is in %s, want %s", tc.table, got, tc.want)
		}
	}

	onA := "?revision_id=" + f.revA
	for _, tc := range []struct{ path, id string }{
		{"/api/developer/grids", gridID},
		{"/api/forms", formID},
		{"/api/integrations", integrationID},
		{"/api/automation/rules", ruleID},
	} {
		if raw := call("GET", tc.path+onB, nil); !contains(raw, tc.id) {
			t.Errorf("GET %s?revision_id=B does not list what was created there: %s", tc.path, raw)
		}
		if raw := call("GET", tc.path+onA, nil); contains(raw, tc.id) {
			t.Errorf("GET %s?revision_id=A lists B's row: %s", tc.path, raw)
		}
	}
	wfPath := "/api/developer/workflows?application_id=" + f.app1 + "&revision_id="
	if raw := call("GET", wfPath+f.revB, nil); !contains(raw, workflowID) {
		t.Errorf("workflow list for B misses B's workflow: %s", raw)
	}
	if raw := call("GET", wfPath+f.revA, nil); contains(raw, workflowID) {
		t.Errorf("workflow list for A lists B's workflow: %s", raw)
	}

	// A revision outside the application is refused, not taken verbatim.
	before := f.column(t, `SELECT id::text FROM model.grid_def`)
	for _, rev := range []string{f.revD, "not-a-uuid"} {
		for _, method := range []string{"GET", "POST"} {
			var body any
			if method == "POST" {
				body = map[string]any{"name": "Stray"}
			}
			if code, raw := f.do(t, method, "/api/developer/grids?revision_id="+rev, dev, f.app1, "", body); code != http.StatusNotFound {
				t.Errorf("%s /api/developer/grids?revision_id=%s: status %d %s, want 404", method, rev, code, raw)
			}
		}
	}
	if after := f.column(t, `SELECT id::text FROM model.grid_def`); !mmEqual(before, after) {
		t.Errorf("a refused grid create stored a grid: %v -> %v", before, after)
	}

	// The same for every other endpoint of the tab, a malformed id included
	// (it used to fall through to "no revision": a revision-global form).
	counts := func() string {
		var s string
		_ = f.pool.QueryRow(context.Background(), `SELECT
			(SELECT count(*) FROM model.form_def)::text || '/' ||
			(SELECT count(*) FROM model.integration_def)::text || '/' ||
			(SELECT count(*) FROM workflow.workflow_def)::text || '/' ||
			(SELECT count(*) FROM workflow.automation_rule)::text`).Scan(&s)
		return s
	}
	beforeCounts := counts()
	for _, rev := range []string{f.revD, "not-a-uuid"} {
		for _, tc := range []struct {
			method, path string
			body         any
		}{
			{"GET", "/api/forms?revision_id=" + rev, nil},
			{"POST", "/api/forms?revision_id=" + rev, map[string]any{"name": "stray", "label": "Stray", "fields": []any{}}},
			{"GET", "/api/integrations?revision_id=" + rev, nil},
			{"POST", "/api/developer/integrations?revision_id=" + rev, map[string]any{"name": "Stray", "type": "csv_import"}},
			{"GET", "/api/automation/rules?revision_id=" + rev, nil},
			{"POST", "/api/automation/rules?revision_id=" + rev, map[string]any{"name": "Stray", "trigger_type": "manual"}},
			{"GET", "/api/developer/workflows?application_id=" + f.app1 + "&revision_id=" + rev, nil},
			{"POST", "/api/developer/workflows?application_id=" + f.app1 + "&revision_id=" + rev, map[string]any{"name": "Stray"}},
		} {
			if code, raw := f.do(t, tc.method, tc.path, dev, f.app1, "", tc.body); code != http.StatusNotFound {
				t.Errorf("%s %s: status %d %s, want 404", tc.method, tc.path, code, raw)
			}
		}
	}
	if after := counts(); after != beforeCounts {
		t.Errorf("refused creates stored rows: forms/integrations/workflows/rules %s -> %s", beforeCounts, after)
	}
}

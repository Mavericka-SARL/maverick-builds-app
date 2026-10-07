package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/modeltransfer"
	"github.com/mavericks-engine/mavericks/internal/starter"
	"github.com/mavericks-engine/mavericks/internal/startersync"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// callJSON performs one JSON request as a persona and decodes the body.
func callJSON(t *testing.T, srv *httptest.Server, persona, method, path string, body any, headers ...string) (int, map[string]any) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, _ := http.NewRequestWithContext(context.Background(), method, srv.URL+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if persona != "" {
		req.Header.Set("X-Dev-User", persona)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// Sign-up on the dev stack (no identity provider): the whole tenant comes
// out of one request — plan, roles, application, starter model with
// its numbers computed — and the account is immediately usable as a dev
// persona. Then the guards around it: validation, an address already
// registered, the per-address throttle, and the switch being off.
func TestSignupCreatesAUsableTenant(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	t.Setenv("DEV_MODE", "true")
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	var padmin string
	if err := pool.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('signup-padmin', 'p@platform.test', 'Platform') RETURNING id::text`).Scan(&padmin); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'platform_admin')`, padmin); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, nil, Deps{Signup: SignupConfig{Enabled: true, ContactURL: "https://example.test/pricing"}}))
	t.Cleanup(srv.Close)

	code, opts := callJSON(t, srv, "", http.MethodGet, "/api/signup/options", nil)
	if code != 200 || opts["enabled"] != true || opts["contact_url"] != "https://example.test/pricing" {
		t.Fatalf("options: %d %v", code, opts)
	}
	// What sign-up offers is the basic workspace: no trial, 100 MB of storage.
	if p, _ := opts["plan"].(map[string]any); p["key"] != "community" || p["limits"].(map[string]any)["max_storage_mb"] != float64(100) {
		t.Fatalf("options plan: %v", opts["plan"])
	}

	// 1st attempt: refused input costs a throttle token but creates nothing.
	if code, body := callJSON(t, srv, "", http.MethodPost, "/api/signup", map[string]any{"company": "A", "first_name": "Ann", "last_name": "Lee", "email": "ann@acme.test"}); code != 400 || !strings.Contains(body["error"].(string), "company name") {
		t.Fatalf("short company: %d %v", code, body)
	}
	// 2nd: success.
	code, out := callJSON(t, srv, "", http.MethodPost, "/api/signup", map[string]any{"company": "Acme Test", "first_name": "Ann", "last_name": "Lee", "email": "Ann@Acme.test"})
	if code != 200 || out["status"] != "created" || out["plan"] != "community" || out["dev_persona"] != "signup-ann@acme.test" {
		t.Fatalf("signup: %d %v", code, out)
	}
	if _, has := out["trial_ends_at"]; has {
		t.Fatalf("a basic workspace has no trial end: %v", out)
	}
	// model_id is the model the tenant lands on: the starter.LandingKey one.
	tenantID, landingID := out["tenant_id"].(string), out["model_id"].(string)

	// The new person sees their plan, their roles and their application.
	code, me := callJSON(t, srv, "signup-ann@acme.test", http.MethodGet, "/api/me", nil)
	if code != 200 || me["email"] != "ann@acme.test" || me["customer_id"] != tenantID || me["contact_url"] != "https://example.test/pricing" {
		t.Fatalf("me: %d %v", code, me)
	}
	// Every role sign-up gives: the user role (business_user) too, which
	// the console's User group comes from.
	roles, _ := me["roles"].([]any)
	held := map[string]bool{}
	for _, r := range roles {
		held[r.(string)] = true
	}
	if len(roles) != 4 || !held["tenant_admin"] || !held["developer"] || !held["business_admin"] || !held["business_user"] {
		t.Fatalf("roles = %v, want tenant_admin, developer, business_admin and business_user", roles)
	}
	st, _ := me["plan"].(map[string]any)
	if _, has := st["trial"]; has || st["read_only"] != false || st["plan"].(map[string]any)["key"] != "community" {
		t.Fatalf("me.plan = %v", st)
	}
	code, apps := callJSONList(t, srv, "signup-ann@acme.test", "/api/apps")
	if code != 200 || len(apps) != 1 || apps[0]["name"] != signupAppName {
		t.Fatalf("apps: %d %v", code, apps)
	}
	// Every starter package is a model of that one application. The one a
	// new tenant lands on (starter.LandingKey, the tour) is the
	// application's default, so the model switcher lists it first and the
	// application card names it. Expectations come from starter.Starters()
	// itself, so the guides' contents can change without this test knowing
	// them.
	pkgs := starter.Packages()
	var landingPkg, tourPkg modeltransfer.Package
	for _, st := range starter.Starters() {
		if st.Key == starter.LandingKey {
			landingPkg = st.Package
		}
		if st.Key == starter.TourKey {
			tourPkg = st.Package
		}
	}
	appModels, _ := apps[0]["models"].([]any)
	if len(appModels) != len(pkgs) || apps[0]["model_name"] != landingPkg.ModelName {
		t.Fatalf("app models: %d of %d, card names %v: %v", len(appModels), len(pkgs), apps[0]["model_name"], appModels)
	}
	modelIDByName := make(map[string]string, len(appModels))
	for i, raw := range appModels {
		m, _ := raw.(map[string]any)
		name, _ := m["name"].(string)
		id, _ := m["id"].(string)
		modelIDByName[name] = id
		if wantDefault := i == 0; m["is_default"] != wantDefault {
			t.Fatalf("model %d (%s) is_default = %v, want %v", i, name, m["is_default"], wantDefault)
		}
	}
	if first, _ := appModels[0].(map[string]any); first["id"] != landingID || first["name"] != landingPkg.ModelName {
		t.Fatalf("first model = %v, want the landing model %s (%s)", first, landingPkg.ModelName, landingID)
	}
	// guideIDs: every other starter, in starter.Starters order (the tour
	// among them), as the sign-up's audit record lists them.
	guideIDs := make([]string, 0, len(pkgs)-1)
	for _, p := range pkgs {
		id := modelIDByName[p.ModelName]
		if id == "" {
			t.Fatalf("package %q has no model (models: %v)", p.ModelName, modelIDByName)
		}
		if p.ModelName != landingPkg.ModelName {
			guideIDs = append(guideIDs, id)
		}
	}
	tourID := modelIDByName[tourPkg.ModelName]
	var defaultModel string
	_ = pool.QueryRow(ctx, `SELECT COALESCE(default_model_id::text,'') FROM core.application WHERE id=$1::uuid`, appID(apps)).Scan(&defaultModel)
	if defaultModel != landingID {
		t.Fatalf("application default model = %q, want the landing model %s", defaultModel, landingID)
	}
	// The account is its tenant's developer, not a platform-level one: with
	// another tenant's larger model in the database, the console's first
	// screen (no application chosen yet) must still land on its own model,
	// and the tenant list must hold its tenant only.
	otherCust := q(`INSERT INTO core.customer (name, plan) VALUES ('Other Co', 'starter') RETURNING id::text`)
	otherWs := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, otherCust)
	otherApp := q(`INSERT INTO core.application (customer_id, workspace_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Other App', 'planning') RETURNING id::text`, otherCust, otherWs)
	otherModel := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Bigger') RETURNING id::text`, otherApp)
	otherRev := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, otherModel)
	for i := 0; i < 10; i++ {
		q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule) VALUES ($1::uuid, $2::uuid, $3, true, 'sum') RETURNING id::text`, otherModel, otherRev, "m"+strconv.Itoa(i))
	}
	// With no application chosen yet the resolution picks among the
	// tenant's own models only, never the other tenant's larger one. The
	// console then chooses the application (AppPicker) and every request
	// carries it: that is where the default decides, and it is the landing
	// model.
	code, demo := callJSON(t, srv, "signup-ann@acme.test", http.MethodGet, "/api/demo", nil)
	resolved, _ := demo["model_id"].(string)
	if ownModel := resolved == landingID || slices.Contains(guideIDs, resolved); code != 200 || !ownModel || demo["revision"] != starter.RevisionName {
		t.Fatalf("first screen resolved model %v (%v), want one of the tenant's own models", demo["model_id"], demo["revision"])
	}
	code, demo = callJSON(t, srv, "signup-ann@acme.test", http.MethodGet, "/api/demo", nil, "X-App-Id", appID(apps))
	if code != 200 || demo["model_id"] != landingID || demo["revision"] != starter.RevisionName {
		t.Fatalf("application's model resolved %v (%v), want the landing model %s", demo["model_id"], demo["revision"], landingID)
	}
	code, tenantsSeen := callJSONList(t, srv, "signup-ann@acme.test", "/api/admin/tenants")
	if code != 200 || len(tenantsSeen) != 1 || tenantsSeen[0]["id"] != tenantID {
		t.Fatalf("tenant list for the new account: %d %v", code, tenantsSeen)
	}
	code, dashboards := callJSONList(t, srv, "signup-ann@acme.test", "/api/developer/dashboards", "X-App-Id", appID(apps))
	if code != 200 || len(dashboards) != len(landingPkg.Dashboards) || dashboards[0]["name"] != landingPkg.Dashboards[0].Name {
		names := make([]any, 0, len(dashboards))
		for _, d := range dashboards {
			names = append(names, d["name"])
		}
		t.Fatalf("developer dashboards: %d %v", code, names)
	}
	// Each starter is what the developer and the business consoles show when
	// the model switcher names it: all of its dashboards, and only its own.
	for _, p := range pkgs {
		want := make([]string, 0, len(p.Dashboards))
		for _, d := range p.Dashboards {
			want = append(want, d.Name)
		}
		slices.Sort(want)
		for _, path := range []string{"/api/developer/dashboards", "/api/dashboards"} {
			code, got := callJSONList(t, srv, "signup-ann@acme.test", path, "X-App-Id", appID(apps), "X-Model-Id", modelIDByName[p.ModelName])
			names := make([]string, 0, len(got))
			for _, d := range got {
				name, _ := d["name"].(string)
				names = append(names, name)
			}
			slices.Sort(names)
			if code != 200 || !slices.Equal(names, want) {
				t.Fatalf("%s for %q: %d %v, want %v", path, p.ModelName, code, names, want)
			}
		}
	}
	// Every calculated metric of every model has its numbers worked out at
	// sign-up, including one that reads no input metric (a guide showing a
	// property or a COUNTIFS): a first look must not find an empty cell.
	for _, p := range pkgs {
		for _, m := range p.Metrics {
			if m.IsInput || m.Formula == nil || strings.TrimSpace(*m.Formula) == "" {
				continue
			}
			var rows int
			var errs string
			_ = pool.QueryRow(ctx, `SELECT count(*) FROM runtime.calc_result cr JOIN model.metric_def md ON md.id = cr.metric_id
				WHERE cr.model_id=$1::uuid AND md.name=$2`, modelIDByName[p.ModelName], m.Name).Scan(&rows)
			_ = pool.QueryRow(ctx, `SELECT COALESCE(string_agg(ps.error, '; '), '') FROM runtime.metric_partition_state ps
				JOIN model.metric_def md ON md.id::text = ps.metric_id::text
				WHERE ps.model_id::text=$1 AND md.name=$2 AND ps.status='error'`, modelIDByName[p.ModelName], m.Name).Scan(&errs)
			if rows == 0 {
				t.Errorf("%q: calculated metric %q has no calc_result rows (calculation errors: %q)", p.ModelName, m.Name, errs)
			}
		}
	}
	var metrics, members, facts, calc, audit int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM model.metric_def WHERE model_id=$1::uuid`, tourID).Scan(&metrics)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM model.dimension_member m JOIN model.dimension_def d ON d.id=m.dimension_id WHERE d.model_id=$1::uuid`, tourID).Scan(&members)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM runtime.fact_input WHERE model_id=$1::uuid`, tourID).Scan(&facts)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM runtime.calc_result WHERE model_id=$1::uuid`, tourID).Scan(&calc)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit.audit_event WHERE event_type='tenant.signed_up' AND resource_id=$1`, tenantID).Scan(&audit)
	if metrics != 3 || members != 8 || facts != 16 || calc == 0 || audit != 1 {
		t.Fatalf("starter model: metrics=%d members=%d facts=%d calc=%d audit=%d", metrics, members, facts, calc, audit)
	}
	// The sign-up's audit record names the landing model as its model and
	// lists the other starters beside it.
	var auditModel, auditGuides string
	if err := pool.QueryRow(ctx, `SELECT metadata->>'model_id', COALESCE(metadata->>'guide_model_ids','') FROM audit.audit_event
		WHERE event_type='tenant.signed_up' AND resource_id=$1`, tenantID).Scan(&auditModel, &auditGuides); err != nil {
		t.Fatal(err)
	}
	if auditModel != landingID || auditGuides != strings.Join(guideIDs, ",") {
		t.Fatalf("audit metadata: model_id=%q guide_model_ids=%q, want %q and %q", auditModel, auditGuides, landingID, strings.Join(guideIDs, ","))
	}
	// Each starter is recorded with the content it was imported with, so a
	// later change reaches this tenant as a new revision (startersync).
	for _, st := range starter.Starters() {
		var recModel, recHash string
		if err := pool.QueryRow(ctx, `SELECT model_id::text, content_hash FROM core.starter_model WHERE customer_id=$1::uuid AND starter_key=$2`,
			tenantID, st.Key).Scan(&recModel, &recHash); err != nil {
			t.Fatalf("starter record %s: %v", st.Key, err)
		}
		if want := modelIDByName[st.Package.ModelName]; recModel != want || recHash != startersync.Hash(st.Package) {
			t.Errorf("starter record %s: model %s hash %s…, want model %s and the current content", st.Key, recModel, recHash[:8], want)
		}
	}
	// The tour's calculated metric is worked out from its own figures, not
	// left blank: the first screen has to show a real number.
	var cost float64
	if err := pool.QueryRow(ctx, `SELECT cr.value FROM runtime.calc_result cr JOIN model.metric_def m ON m.id=cr.metric_id
		WHERE cr.model_id=$1::uuid AND m.name='cost' AND cr.dim_members->>(SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND name='team')='SALES'
		  AND cr.dim_members->>(SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND name='quarter')='Q1' LIMIT 1`, tourID).Scan(&cost); err != nil {
		t.Fatalf("cost cell: %v", err)
	}
	if cost != 48000 { // 4 people at 12,000
		t.Fatalf("cost Q1/SALES = %v, want 48000", cost)
	}
	// The tour's two cards show what their titles say, read the way a card
	// reads them (MetricKpiWidget: GET /api/grid?totals_only=1, plus the
	// card's pinned member as scope). The cards and their pins come from the
	// package; the figures are the tour's own. Total cost for the year is
	// Sales 4+4+5+5 at 12,000 plus Engineering 6+6+7+8 at 15,000 = 621,000;
	// headcount at the end of Q4 is 5 + 8 = 13.
	tour := tourPkg
	tourRevID := q(`SELECT active_revision_id::text FROM core.model WHERE id=$1::uuid`, tourID)
	// The package names things by placeholder id; the database by its own.
	tourIDs, byName := map[string]string{}, map[string]string{}
	metricNames := map[string]string{}
	for _, m := range tour.Metrics {
		tourIDs[m.ID] = q(`SELECT id::text FROM model.metric_def WHERE revision_id=$1::uuid AND name=$2`, tourRevID, m.Name)
		byName[m.Name], metricNames[m.ID] = tourIDs[m.ID], m.Name
	}
	for _, d := range tour.Dimensions {
		tourIDs[d.ID] = q(`SELECT id::text FROM model.dimension_def WHERE revision_id=$1::uuid AND name=$2`, tourRevID, d.Name)
		byName[d.Name] = tourIDs[d.ID]
	}
	tourTotals := func(scope map[string]string) map[string]any {
		t.Helper()
		path := "/api/grid?totals_only=1&revision_id=" + tourRevID
		if len(scope) > 0 {
			b, _ := json.Marshal(scope)
			path += "&scope=" + url.QueryEscape(string(b))
		}
		code, body := callJSON(t, srv, "signup-ann@acme.test", http.MethodGet, path, nil, "X-App-Id", appID(apps))
		totals, _ := body["totals"].(map[string]any)
		if code != 200 || totals == nil {
			t.Fatalf("tour totals %v: %d %v", scope, code, body)
		}
		return totals
	}
	wantCard := map[string]float64{"cost": 621000, "headcount": 13}
	cards := 0
	for _, d := range tour.Dashboards {
		for _, w := range d.Widgets {
			if w.WidgetType != "metric_kpi" || w.RefID == nil {
				continue
			}
			var props struct {
				Mode  string `json:"kpi_context_mode"`
				Scope *struct {
					DimensionID string `json:"dimension_id"`
					MemberCode  string `json:"member_code"`
				} `json:"kpi_scope"`
			}
			if err := json.Unmarshal(w.Props, &props); err != nil {
				t.Fatalf("card props on %q: %v", d.Name, err)
			}
			var scope map[string]string
			if props.Mode == "pin" && props.Scope != nil {
				scope = map[string]string{tourIDs[props.Scope.DimensionID]: props.Scope.MemberCode}
			}
			name := metricNames[*w.RefID]
			want, ok := wantCard[name]
			if !ok {
				t.Errorf("tour card on %q (%s) has no expected figure", name, d.Name)
				continue
			}
			cards++
			if got := tourTotals(scope)[tourIDs[*w.RefID]]; got != want {
				t.Errorf("tour card %q (%s, scope %v) = %v, want %v", name, props.Mode, scope, got, want)
			}
		}
	}
	if cards != len(wantCard) {
		t.Errorf("the tour has %d cards with an expected figure, want %d", cards, len(wantCard))
	}
	// The totals the tour's prose explains: over the year, headcount shows
	// its last quarter (13), not the sum of four; cost per head is averaged
	// (12,000 and 15,000 give 13,500). And the pin is applied at all: Q1's
	// headcount is 4 + 6 = 10.
	year := tourTotals(nil)
	if got := year[byName["headcount"]]; got != float64(13) {
		t.Errorf("headcount for the year = %v, want 13 (its last quarter)", got)
	}
	if got := year[byName["cost_per_head"]]; got != float64(13500) {
		t.Errorf("cost per head for the year = %v, want 13500 (averaged)", got)
	}
	if got := tourTotals(map[string]string{byName["quarter"]: "Q1"})[byName["headcount"]]; got != float64(10) {
		t.Errorf("headcount pinned to Q1 = %v, want 10", got)
	}

	// The platform admin's tenant list carries the plan state.
	code, tenants := callJSONList(t, srv, "signup-padmin", "/api/admin/tenants")
	if code != 200 {
		t.Fatalf("tenants: %d", code)
	}
	var found bool
	for _, tn := range tenants {
		if tn["id"] == tenantID {
			found = true
			ps, _ := tn["plan_state"].(map[string]any)
			if tn["plan"] != "community" || ps["limit_state"] != "ok" {
				t.Fatalf("tenant listing: %v", tn)
			}
		}
	}
	if !found {
		t.Fatal("new tenant not listed")
	}

	// Application-scoped screens work on the model the console opens. A form
	// built on the tour (the Developer guide has one built) is offered as a
	// workflow trigger: the catalog reads the model of the revision it is
	// handed rather than picking one of the four again.
	const persona = "signup-ann@acme.test"
	tourApp := appID(apps)
	code, form := callJSON(t, srv, persona, http.MethodPost, "/api/forms", map[string]any{
		"name": "requests", "label": "Requests",
		"fields": []map[string]any{{"name": "amount", "label": "Amount", "type": "number"}},
	}, "X-App-Id", tourApp)
	formID, _ := form["id"].(string)
	if code != 200 || formID == "" {
		t.Fatalf("create a form on the tour: %d %v", code, form)
	}
	code, events := callJSONList(t, srv, persona, "/api/developer/workflow-trigger-events?application_id="+tourApp)
	if code != 200 || !slices.ContainsFunc(events, func(e map[string]any) bool { return e["source_id"] == formID }) {
		t.Fatalf("trigger events: %d, no event for the tour's form %s in %v", code, formID, events)
	}
	// Business Admin › Access Rules offers the dashboards of the model the
	// admin has open: the switcher's choice, else the default.
	for _, p := range pkgs {
		headers := []string{"X-App-Id", tourApp}
		if p.ModelName != landingPkg.ModelName {
			headers = append(headers, "X-Model-Id", modelIDByName[p.ModelName])
		}
		want := make([]string, 0, len(p.Dashboards))
		for _, d := range p.Dashboards {
			want = append(want, d.Name)
		}
		code, got := callJSONList(t, srv, persona, "/api/business-admin/available?type=dashboards", headers...)
		names := make([]string, 0, len(got))
		for _, d := range got {
			name, _ := d["name"].(string)
			names = append(names, name)
		}
		slices.Sort(want)
		slices.Sort(names)
		if code != 200 || !slices.Equal(names, want) {
			t.Fatalf("access-rule dashboards for %q: %d %v, want %v", p.ModelName, code, names, want)
		}
	}
	// With no application chosen yet, the application and the model resolve
	// alike. A second application of the tenant, larger than any starter
	// model, loses to the landing model for both, so an automation-rules
	// request that names its revision is not refused as another
	// application's.
	tourWs := q(`SELECT workspace_id::text FROM core.application WHERE id=$1::uuid`, tourApp)
	landingRev := q(`SELECT active_revision_id::text FROM core.model WHERE id=$1::uuid`, landingID)
	bigApp := q(`INSERT INTO core.application (customer_id, workspace_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Larger', 'planning') RETURNING id::text`, tenantID, tourWs)
	bigModel := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Larger') RETURNING id::text`, bigApp)
	bigRev := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, bigModel)
	for i := 0; i < 12; i++ {
		q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule) VALUES ($1::uuid, $2::uuid, $3, true, 'sum') RETURNING id::text`, bigModel, bigRev, "m"+strconv.Itoa(i))
	}
	if code, demo := callJSON(t, srv, persona, http.MethodGet, "/api/demo", nil); code != 200 || demo["model_id"] != landingID {
		t.Fatalf("no application chosen, beside a larger one: resolved %v, want the landing model %s", demo["model_id"], landingID)
	}
	if code, _ := callJSONList(t, srv, persona, "/api/automation/rules?revision_id="+landingRev); code != 200 {
		t.Fatalf("automation rules on the landing model's revision, no application chosen: %d", code)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM core.application WHERE id=$1::uuid`, bigApp); err != nil {
		t.Fatal(err)
	}

	// The default stays an ordinary setting: Developer › Models › "Set as
	// business default" moves it to another starter (the same statement
	// sign-up ran), and back.
	if len(guideIDs) > 0 {
		for _, target := range []string{guideIDs[0], landingID} {
			if code, body := callJSON(t, srv, "signup-ann@acme.test", http.MethodPost, "/api/developer/models/"+target+"/set-default", nil); code != 200 {
				t.Fatalf("set default to %s: %d %v", target, code, body)
			}
			_, apps := callJSONList(t, srv, "signup-ann@acme.test", "/api/apps")
			listed, _ := apps[0]["models"].([]any)
			first, _ := listed[0].(map[string]any)
			_, demo := callJSON(t, srv, "signup-ann@acme.test", http.MethodGet, "/api/demo", nil, "X-App-Id", appID(apps))
			if first["id"] != target || first["is_default"] != true || demo["model_id"] != target {
				t.Fatalf("after setting the default to %s: first listed %v, resolved %v", target, first, demo["model_id"])
			}
		}
	}

	// Each guide ends by saying it can be deleted. Deleting the default one
	// clears the default, and models created in one transaction tie on
	// created_at: the listing and the resolution must still come out the
	// same every time — by name — and no model may drop out of the switcher.
	if code, body := callJSON(t, srv, "signup-ann@acme.test", http.MethodDelete, "/api/admin/models/"+landingID, nil); code != 200 {
		t.Fatalf("delete the landing model: %d %v", code, body)
	}
	guideNames := make([]string, 0, len(pkgs)-1)
	for _, p := range pkgs {
		if p.ModelName != landingPkg.ModelName {
			guideNames = append(guideNames, p.ModelName)
		}
	}
	slices.Sort(guideNames)
	for attempt := 0; attempt < 3; attempt++ {
		code, apps := callJSONList(t, srv, "signup-ann@acme.test", "/api/apps")
		if code != 200 || len(apps) != 1 {
			t.Fatalf("apps after deleting the landing model: %d %v", code, apps)
		}
		listed, _ := apps[0]["models"].([]any)
		names := make([]string, 0, len(listed))
		for _, raw := range listed {
			m, _ := raw.(map[string]any)
			name, _ := m["name"].(string)
			names = append(names, name)
			if m["is_default"] != false {
				t.Fatalf("with no default set, %q reports is_default=%v", name, m["is_default"])
			}
		}
		if !slices.Equal(names, guideNames) {
			t.Fatalf("models after deleting the landing model: %v, want %v", names, guideNames)
		}
		if len(guideNames) == 0 {
			break
		}
		code, demo := callJSON(t, srv, "signup-ann@acme.test", http.MethodGet, "/api/demo", nil, "X-App-Id", appID(apps))
		if code != 200 || demo["model_id"] != modelIDByName[guideNames[0]] {
			t.Fatalf("with no default, the application resolved %v, want %q (%s)", demo["model_id"], guideNames[0], modelIDByName[guideNames[0]])
		}
	}

	// 3rd: the same address again is refused. 4th: the throttle closes.
	if code, body := callJSON(t, srv, "", http.MethodPost, "/api/signup", map[string]any{"company": "Acme Again", "first_name": "Ann", "last_name": "Lee", "email": "ann@acme.test"}); code != 409 || !strings.Contains(body["error"].(string), "already exists") {
		t.Fatalf("duplicate: %d %v", code, body)
	}
	if code, _ := callJSON(t, srv, "", http.MethodPost, "/api/signup", map[string]any{"company": "Fourth Co", "first_name": "F", "last_name": "G", "email": "f@fourth.test"}); code != 429 {
		t.Fatalf("throttle: %d", code)
	}
	var fourth int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM core.customer WHERE name='Fourth Co'`).Scan(&fourth)
	if fourth != 0 {
		t.Fatal("a throttled attempt created a tenant")
	}

	// Switched off: options say so and the endpoint refuses.
	off := httptest.NewServer(NewHandler(logger.New("test"), pool, nil))
	t.Cleanup(off.Close)
	if code, opts := callJSON(t, off, "", http.MethodGet, "/api/signup/options", nil); code != 200 || opts["enabled"] != false || !strings.Contains(opts["reason"].(string), "not enabled") {
		t.Fatalf("options off: %d %v", code, opts)
	}
	if code, _ := callJSON(t, off, "", http.MethodPost, "/api/signup", map[string]any{"company": "Off Co", "first_name": "O", "last_name": "F", "email": "o@off.test"}); code != 503 {
		t.Fatalf("signup off: %d", code)
	}
}

func callJSONList(t *testing.T, srv *httptest.Server, persona, path string, headers ...string) (int, []map[string]any) {
	t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+path, nil)
	req.Header.Set("X-Dev-User", persona)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var out []map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func appID(apps []map[string]any) string {
	if len(apps) == 0 {
		return ""
	}
	id, _ := apps[0]["id"].(string)
	return id
}

// With an identity provider: the account is created there with its roles
// and invited; when the invitation cannot be sent, everything — the
// account, the tenant, the user — is undone, because a sign-up nobody can
// finish is not a sign-up.
func TestSignupWithIdentityProviderAndRollback(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	t.Setenv("DEV_MODE", "true")
	broker := newFakeBroker(t)
	srv := httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, nil, Deps{Keycloak: broker.client(), Signup: SignupConfig{Enabled: true}}))
	t.Cleanup(srv.Close)

	code, out := callJSON(t, srv, "", http.MethodPost, "/api/signup", map[string]any{"company": "Brokered Co", "first_name": "Bo", "last_name": "Kim", "email": "bo@brokered.test"})
	if code != 200 || out["status"] != "invited" || out["invited"] != true || out["dev_persona"] != nil {
		t.Fatalf("signup: %d %v", code, out)
	}
	countModels := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM core.model`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := countModels(); n != len(starter.Packages()) {
		t.Fatalf("models after one sign-up: %d, want %d", n, len(starter.Packages()))
	}
	var sub string
	if err := pool.QueryRow(ctx, `SELECT keycloak_sub FROM identity.user WHERE email='bo@brokered.test'`).Scan(&sub); err != nil {
		t.Fatal(err)
	}
	broker.mu.Lock()
	u, ok := broker.users[sub]
	invited := len(broker.invited) == 1 && broker.invited[0] == sub
	broker.mu.Unlock()
	if !ok || u.First != "Bo" || !invited {
		t.Fatalf("identity provider: user=%v invited=%v", u, invited)
	}
	// Known to the provider now: a second sign-up with that address is a conflict.
	if code, _ := callJSON(t, srv, "", http.MethodPost, "/api/signup", map[string]any{"company": "Other", "first_name": "Bo", "last_name": "Kim", "email": "bo@brokered.test"}); code != 409 {
		t.Fatalf("duplicate via provider: %d", code)
	}

	broker.mu.Lock()
	broker.failInvite = true
	broker.mu.Unlock()
	code, body := callJSON(t, srv, "", http.MethodPost, "/api/signup", map[string]any{"company": "Doomed Co", "first_name": "Do", "last_name": "Om", "email": "do@doomed.test"})
	if code != 502 || !strings.Contains(body["error"].(string), "nothing was created") {
		t.Fatalf("invite failure: %d %v", code, body)
	}
	var customers, users int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM core.customer WHERE name='Doomed Co'`).Scan(&customers)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM identity.user WHERE email='do@doomed.test'`).Scan(&users)
	broker.mu.Lock()
	deleted := len(broker.deleted)
	_, stillThere := broker.users["kc-sub-2"]
	broker.mu.Unlock()
	if customers != 0 || users != 0 || deleted != 1 || stillThere {
		t.Fatalf("rollback: customers=%d users=%d deletedAtProvider=%d stillThere=%v", customers, users, deleted, stillThere)
	}
	// Every one of the undone sign-up's models went with it: only the first
	// sign-up's remain.
	if n := countModels(); n != len(starter.Packages()) {
		t.Fatalf("models after the rollback: %d, want %d (the first sign-up's only)", n, len(starter.Packages()))
	}
}

// Sign-up imports its starter packages and then recalculates each one with
// recalcRevisionCalculated rather than recalcRevisionFromInputs. This pins
// why, on a package the guides could plausibly contain: a metric that reads
// only a dimension (COUNTIFS) and a metric that reads it, with no input
// metric anywhere. The input-driven pass leaves both empty; the one sign-up
// uses computes both. It also checks the import side of the same path: a
// grid widget's saved layout (default_view) names dimensions by id, and
// has to name the imported model's dimension, not the package's.
func TestSignupRecalcReachesMetricsWithoutInputs(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	h := &handler{log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), devMode: true}

	var custID, wsID, appID, userID string
	for _, step := range []struct {
		sql  string
		args []any
		out  *string
	}{
		{`INSERT INTO core.customer (name, plan) VALUES ('Counts Co', 'starter') RETURNING id::text`, nil, &custID},
		{`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Default') RETURNING id::text`, []any{&custID}, &wsID},
		{`INSERT INTO core.application (customer_id, workspace_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Counts', 'planning') RETURNING id::text`, []any{&custID, &wsID}, &appID},
		{`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('counts', 'c@counts.test', 'C', $1::uuid) RETURNING id::text`, []any{&custID}, &userID},
	} {
		args := make([]any, len(step.args))
		for i, a := range step.args {
			args[i] = *(a.(*string))
		}
		if err := pool.QueryRow(ctx, step.sql, args...).Scan(step.out); err != nil {
			t.Fatalf("%s: %v", step.sql, err)
		}
	}

	formula := func(s string) *string { return &s }
	at := func(n int) *int { return &n }
	region := modeltransfer.Dimension{ID: "dim-region", Name: "region", AggRule: "sum", Members: []modeltransfer.Member{
		{ID: "r-emea", Code: "EMEA", Label: "EMEA", SortOrder: 0},
		{ID: "r-us", Code: "US", Label: "US", SortOrder: 1},
	}}
	layout, _ := json.Marshal(map[string]any{"default_view": map[string]any{
		"rows": []string{"__metrics__"}, "cols": []string{"dim-region"}, "context": []string{},
		"filter_sel": map[string]string{"dim-region": "US"},
	}})
	grid := "grid-counts"
	pkg := modeltransfer.Package{
		Format: modeltransfer.PackageFormat, Version: modeltransfer.PackageVersion,
		ModelName: "Counts", StorageType: "oltp", RevisionName: starter.RevisionName, IncludeData: true,
		Dimensions: []modeltransfer.Dimension{region},
		Metrics: []modeltransfer.Metric{
			{ID: "m-n", Name: "n_regions", Formula: formula(`COUNTIFS(region, "*")`), StorageType: "oltp", AggRule: "sum", Format: "number"},
			{ID: "m-2n", Name: "doubled", Formula: formula(`n_regions * 2`), StorageType: "oltp", AggRule: "sum", Format: "number"},
		},
		Dependencies: []modeltransfer.Dependency{{MetricID: "m-2n", DependsOn: "m-n"}},
		Grids: []modeltransfer.Grid{{ID: grid, Name: "Counts", Metrics: []modeltransfer.GridMetric{{MetricID: "m-n"}, {MetricID: "m-2n", SortOrder: 1}},
			Dimensions: []modeltransfer.GridDimension{{DimensionID: "dim-region"}}}},
		// Tags and positions set: modeltransfer.Import writes a nil dashboard
		// Tags or widget position as NULL, which the tables refuse — not what
		// this test is about. Keep this fixture focused on recalculation.
		Dashboards: []modeltransfer.Dashboard{{ID: "dash-counts", Name: "Counts", Tags: []string{"guide"}, Widgets: []modeltransfer.Widget{
			{WidgetType: "grid", RefID: &grid, PosX: at(0), PosY: at(0), SizeW: at(600), SizeH: at(300), Props: layout},
		}}},
	}
	var modelID, revisionID string
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		modelID, revisionID, err = modeltransfer.Import(ctx, tx, modeltransfer.ImportRequest{ApplicationID: appID, Package: pkg}, userID)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	var regionID, props string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND name='region'`, modelID).Scan(&regionID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT w.widget_props::text FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id=w.dashboard_id
		WHERE d.model_id=$1::uuid AND w.widget_type='grid'`, modelID).Scan(&props); err != nil {
		t.Fatal(err)
	}
	var saved struct {
		DefaultView struct {
			Rows      []string          `json:"rows"`
			Cols      []string          `json:"cols"`
			FilterSel map[string]string `json:"filter_sel"`
		} `json:"default_view"`
	}
	if err := json.Unmarshal([]byte(props), &saved); err != nil {
		t.Fatal(err)
	}
	if dv := saved.DefaultView; !slices.Equal(dv.Rows, []string{"__metrics__"}) || !slices.Equal(dv.Cols, []string{regionID}) || dv.FilterSel[regionID] != "US" || len(dv.FilterSel) != 1 {
		t.Fatalf("imported grid layout %s, want its columns and filter on the imported region %s", props, regionID)
	}

	rowsFor := func(name string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM runtime.calc_result cr JOIN model.metric_def m ON m.id=cr.metric_id
			WHERE cr.model_id=$1::uuid AND m.name=$2`, modelID, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if err := h.recalcRevisionFromInputs(ctx, modelID, revisionID); err != nil {
		t.Fatal(err)
	}
	if n, d := rowsFor("n_regions"), rowsFor("doubled"); n != 0 || d != 0 {
		t.Fatalf("the input-driven pass computed n_regions=%d doubled=%d rows; this test assumes it reaches neither", n, d)
	}
	h.recalcRevisionCalculated(ctx, modelID, revisionID)
	for name, want := range map[string]float64{"n_regions": 2, "doubled": 4} {
		var v float64
		if err := pool.QueryRow(ctx, `SELECT cr.value FROM runtime.calc_result cr JOIN model.metric_def m ON m.id=cr.metric_id
			WHERE cr.model_id=$1::uuid AND m.name=$2 AND cr.dim_members->>$3='US' LIMIT 1`, modelID, name, regionID).Scan(&v); err != nil {
			t.Fatalf("%s after the sign-up recalculation: %v", name, err)
		}
		if v != want {
			t.Errorf("%s = %v, want %v", name, v, want)
		}
	}
}

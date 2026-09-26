package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/starter"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
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
	if p, _ := opts["plan"].(map[string]any); p["key"] != "test" || p["limits"].(map[string]any)["max_storage_mb"] != float64(100) {
		t.Fatalf("options plan: %v", opts["plan"])
	}

	// 1st attempt: refused input costs a throttle token but creates nothing.
	if code, body := callJSON(t, srv, "", http.MethodPost, "/api/signup", map[string]any{"company": "A", "first_name": "Ann", "last_name": "Lee", "email": "ann@acme.test"}); code != 400 || !strings.Contains(body["error"].(string), "company name") {
		t.Fatalf("short company: %d %v", code, body)
	}
	// 2nd: success.
	code, out := callJSON(t, srv, "", http.MethodPost, "/api/signup", map[string]any{"company": "Acme Test", "first_name": "Ann", "last_name": "Lee", "email": "Ann@Acme.test"})
	if code != 200 || out["status"] != "created" || out["plan"] != "test" || out["dev_persona"] != "signup-ann@acme.test" {
		t.Fatalf("signup: %d %v", code, out)
	}
	if _, has := out["trial_ends_at"]; has {
		t.Fatalf("a basic workspace has no trial end: %v", out)
	}
	tenantID, modelID := out["tenant_id"].(string), out["model_id"].(string)

	// The new person sees their plan, their roles and their application.
	code, me := callJSON(t, srv, "signup-ann@acme.test", http.MethodGet, "/api/me", nil)
	if code != 200 || me["email"] != "ann@acme.test" || me["customer_id"] != tenantID || me["contact_url"] != "https://example.test/pricing" {
		t.Fatalf("me: %d %v", code, me)
	}
	roles, _ := me["roles"].([]any)
	if len(roles) != 3 || !strings.Contains(strings.Join([]string{roles[0].(string), roles[1].(string), roles[2].(string)}, ","), "tenant_admin") {
		t.Fatalf("roles = %v", roles)
	}
	st, _ := me["plan"].(map[string]any)
	if _, has := st["trial"]; has || st["read_only"] != false || st["plan"].(map[string]any)["key"] != "test" {
		t.Fatalf("me.plan = %v", st)
	}
	code, apps := callJSONList(t, srv, "signup-ann@acme.test", "/api/apps")
	if code != 200 || len(apps) != 1 || apps[0]["name"] != signupAppName {
		t.Fatalf("apps: %d %v", code, apps)
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
	code, demo := callJSON(t, srv, "signup-ann@acme.test", http.MethodGet, "/api/demo", nil)
	if code != 200 || demo["model_id"] != modelID || demo["revision"] != starter.RevisionName {
		t.Fatalf("first screen resolved model %v (%v), want the starter model %s", demo["model_id"], demo["revision"], modelID)
	}
	code, tenantsSeen := callJSONList(t, srv, "signup-ann@acme.test", "/api/admin/tenants")
	if code != 200 || len(tenantsSeen) != 1 || tenantsSeen[0]["id"] != tenantID {
		t.Fatalf("tenant list for the new account: %d %v", code, tenantsSeen)
	}
	code, dashboards := callJSONList(t, srv, "signup-ann@acme.test", "/api/developer/dashboards", "X-App-Id", appID(apps))
	if code != 200 || len(dashboards) != len(starter.Package().Dashboards) || dashboards[0]["name"] != starter.DashboardName {
		names := make([]any, 0, len(dashboards))
		for _, d := range dashboards {
			names = append(names, d["name"])
		}
		t.Fatalf("developer dashboards: %d %v", code, names)
	}
	var metrics, members, facts, calc, audit int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM model.metric_def WHERE model_id=$1::uuid`, modelID).Scan(&metrics)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM model.dimension_member m JOIN model.dimension_def d ON d.id=m.dimension_id WHERE d.model_id=$1::uuid`, modelID).Scan(&members)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM runtime.fact_input WHERE model_id=$1::uuid`, modelID).Scan(&facts)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM runtime.calc_result WHERE model_id=$1::uuid`, modelID).Scan(&calc)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM audit.audit_event WHERE event_type='tenant.signed_up' AND resource_id=$1`, tenantID).Scan(&audit)
	if metrics != 3 || members != 8 || facts != 16 || calc == 0 || audit != 1 {
		t.Fatalf("starter model: metrics=%d members=%d facts=%d calc=%d audit=%d", metrics, members, facts, calc, audit)
	}
	// The tour's calculated metric is worked out from its own figures, not
	// left blank: the first screen has to show a real number.
	var cost float64
	if err := pool.QueryRow(ctx, `SELECT cr.value FROM runtime.calc_result cr JOIN model.metric_def m ON m.id=cr.metric_id
		WHERE cr.model_id=$1::uuid AND m.name='cost' AND cr.dim_members->>(SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND name='team')='SALES'
		  AND cr.dim_members->>(SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND name='quarter')='Q1' LIMIT 1`, modelID).Scan(&cost); err != nil {
		t.Fatalf("cost cell: %v", err)
	}
	if cost != 48000 { // 4 people at 12,000
		t.Fatalf("cost Q1/SALES = %v, want 48000", cost)
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
			if tn["plan"] != "test" || ps["limit_state"] != "ok" {
				t.Fatalf("tenant listing: %v", tn)
			}
		}
	}
	if !found {
		t.Fatal("new tenant not listed")
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
}

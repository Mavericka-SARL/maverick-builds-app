package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/ee/sso"
	"github.com/mavericks-engine/mavericks/internal/identity"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/auth"
	"github.com/mavericks-engine/mavericks/pkg/keycloak"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// ── a Keycloak that remembers what it was told ────────────────────────────

type fakeKCUser struct {
	Email, First, Last string
	Enabled            bool
}

type fakeBroker struct {
	srv       *httptest.Server
	mu        sync.Mutex
	users     map[string]*fakeKCUser
	idps      map[string]map[string]any
	federated map[string][]map[string]string
	invited   []string
	deleted   []string
	nextID    int
	// failInvite makes the invitation mail fail, as a realm without SMTP does.
	failInvite bool
}

func newFakeBroker(t *testing.T) *fakeBroker {
	t.Helper()
	f := &fakeBroker{users: map[string]*fakeKCUser{}, idps: map[string]map[string]any{}, federated: map[string][]map[string]string{}}
	mux := http.NewServeMux()
	const base = "/admin/realms/mavericks"
	mux.HandleFunc("/realms/mavericks/protocol/openid-connect/token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": 300})
	})
	mux.HandleFunc(base+"/users", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			email := r.URL.Query().Get("email")
			out := []map[string]any{}
			for sub, u := range f.users {
				if strings.EqualFold(u.Email, email) {
					out = append(out, map[string]any{"id": sub, "email": u.Email})
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		case http.MethodPost:
			var u map[string]any
			_ = json.NewDecoder(r.Body).Decode(&u)
			f.nextID++
			sub := fmt.Sprintf("kc-sub-%d", f.nextID)
			f.users[sub] = &fakeKCUser{Email: fmt.Sprint(u["email"]), First: fmt.Sprint(u["firstName"]), Last: fmt.Sprint(u["lastName"]), Enabled: true}
			w.Header().Set("Location", base+"/users/"+sub)
			w.WriteHeader(http.StatusCreated)
		}
	})
	mux.HandleFunc(base+"/roles/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, base+"/roles/")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "r-" + name, "name": name})
	})
	mux.HandleFunc(base+"/users/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		rest := strings.TrimPrefix(r.URL.Path, base+"/users/")
		sub, tail, _ := strings.Cut(rest, "/")
		switch {
		case tail == "role-mappings/realm":
			w.WriteHeader(http.StatusNoContent)
		case tail == "execute-actions-email":
			if f.failInvite {
				http.Error(w, `{"errorMessage":"Failed to send execute actions email"}`, http.StatusInternalServerError)
				return
			}
			f.invited = append(f.invited, sub)
			w.WriteHeader(http.StatusNoContent)
		case tail == "federated-identity":
			links := f.federated[sub]
			if links == nil {
				links = []map[string]string{}
			}
			_ = json.NewEncoder(w).Encode(links)
		case tail == "" && r.Method == http.MethodGet:
			u, ok := f.users[sub]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": sub, "email": u.Email, "firstName": u.First, "lastName": u.Last, "enabled": u.Enabled})
		case tail == "" && r.Method == http.MethodPut:
			u, ok := f.users[sub]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if v, ok := body["enabled"].(bool); ok {
				u.Enabled = v
			}
			if v, ok := body["firstName"].(string); ok {
				u.First = v
			}
			if v, ok := body["lastName"].(string); ok {
				u.Last = v
			}
			if v, ok := body["email"].(string); ok {
				u.Email = v
			}
			w.WriteHeader(http.StatusNoContent)
		case tail == "" && r.Method == http.MethodDelete:
			f.deleted = append(f.deleted, sub)
			delete(f.users, sub)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	mux.HandleFunc(base+"/identity-provider/import-config", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.Contains(body["fromUrl"], "unreachable") {
			http.Error(w, `{"errorMessage":"Could not read document"}`, http.StatusBadRequest)
			return
		}
		if body["providerId"] == "saml" {
			_ = json.NewEncoder(w).Encode(map[string]string{"singleSignOnServiceUrl": "https://idp.acme.test/saml", "idpEntityId": "https://idp.acme.test", "signingCertificate": "MIIC..."})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"issuer": "https://idp.acme.test", "authorizationUrl": "https://idp.acme.test/auth", "tokenUrl": "https://idp.acme.test/token", "jwksUrl": "https://idp.acme.test/jwks"})
	})
	mux.HandleFunc(base+"/identity-provider/instances", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		var idp map[string]any
		_ = json.NewDecoder(r.Body).Decode(&idp)
		f.idps[fmt.Sprint(idp["alias"])] = idp
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc(base+"/identity-provider/instances/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		alias := strings.TrimPrefix(r.URL.Path, base+"/identity-provider/instances/")
		switch r.Method {
		case http.MethodGet:
			idp, ok := f.idps[alias]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			// Keycloak masks the secret on read.
			masked := map[string]any{}
			for k, v := range idp {
				masked[k] = v
			}
			if cfg, ok := idp["config"].(map[string]any); ok {
				mc := map[string]any{}
				for k, v := range cfg {
					mc[k] = v
				}
				if _, has := mc["clientSecret"]; has {
					mc["clientSecret"] = "**********"
				}
				masked["config"] = mc
			}
			_ = json.NewEncoder(w).Encode(masked)
		case http.MethodPut:
			var idp map[string]any
			_ = json.NewDecoder(r.Body).Decode(&idp)
			// Keycloak: the masked value on update means "keep the stored secret".
			if cfg, ok := idp["config"].(map[string]any); ok && cfg["clientSecret"] == keycloak.MaskedSecret {
				if old, ok := f.idps[alias]["config"].(map[string]any); ok {
					cfg["clientSecret"] = old["clientSecret"]
				}
			}
			f.idps[alias] = idp
			w.WriteHeader(http.StatusNoContent)
		case http.MethodDelete:
			delete(f.idps, alias)
			w.WriteHeader(http.StatusNoContent)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBroker) client() *keycloak.Client {
	return keycloak.New(f.srv.URL, "mavericks", "mavericks-admin", "secret", "https://console.test")
}

func (f *fakeBroker) idpConfig(alias, key string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	idp, ok := f.idps[alias]
	if !ok {
		return "<no idp>"
	}
	cfg, _ := idp["config"].(map[string]any)
	return fmt.Sprint(cfg[key])
}

// ── fixture ───────────────────────────────────────────────────────────────

type ssoFixture struct {
	pool              *pgxpool.Pool
	custID, wsID      string
	adminSub, adminID string
	broker            *fakeBroker
	srv               *httptest.Server
	communitySrv      *httptest.Server
	h                 *handler
}

func setupSsoFixture(t *testing.T) *ssoFixture {
	t.Helper()
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	f := &ssoFixture{pool: pool, broker: newFakeBroker(t), adminSub: "sso-admin"}
	f.custID = q(`INSERT INTO core.customer (name, plan) VALUES ('Acme', 'enterprise') RETURNING id::text`)
	f.wsID = q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Default') RETURNING id::text`, f.custID)
	f.adminID = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'admin@acme.test', 'Acme Admin', $2::uuid) RETURNING id::text`, f.adminSub, f.custID)
	if _, err := pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'tenant_admin', $2::uuid)`, f.adminID, f.wsID); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEV_MODE", "true")
	deps := Deps{Keycloak: f.broker.client(), License: enterpriseManager(t), KeycloakPublicURL: "https://auth.test", KeycloakRealm: "mavericks", PublicURL: "https://console.test"}
	f.srv = httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, nil, deps))
	t.Cleanup(f.srv.Close)
	f.communitySrv = httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, nil, Deps{Keycloak: f.broker.client()}))
	t.Cleanup(f.communitySrv.Close)
	f.h = &handler{log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), kc: f.broker.client(), lic: enterpriseManager(t), devMode: true}
	return f
}

func (f *ssoFixture) do(t *testing.T, srv *httptest.Server, method, path string, body any, headers map[string]string) (int, []byte) {
	t.Helper()
	var buf []byte
	if body != nil {
		switch b := body.(type) {
		case string:
			buf = []byte(b)
		default:
			buf, _ = json.Marshal(body)
		}
	}
	req, err := http.NewRequestWithContext(context.Background(), method, srv.URL+path, bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Dev-User", f.adminSub)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	out := new(bytes.Buffer)
	_, _ = out.ReadFrom(resp.Body)
	return resp.StatusCode, out.Bytes()
}

func decode(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not JSON: %s", b)
	}
	return m
}

// ── tests ─────────────────────────────────────────────────────────────────

func TestSsoAndScimGatedByEdition(t *testing.T) {
	f := setupSsoFixture(t)
	code, body := f.do(t, f.communitySrv, http.MethodGet, "/api/admin/sso", nil, nil)
	if code != http.StatusForbidden || !strings.Contains(string(body), "Single sign-on requires the enterprise edition") {
		t.Fatalf("community sso: %d %s", code, body)
	}
	code, body = f.do(t, f.communitySrv, http.MethodGet, "/api/admin/scim/tokens", nil, nil)
	if code != http.StatusForbidden || !strings.Contains(string(body), "SCIM provisioning requires") {
		t.Fatalf("community scim tokens: %d %s", code, body)
	}
	code, body = f.do(t, f.communitySrv, http.MethodGet, "/api/scim/v2/Users", nil, map[string]string{"Authorization": "Bearer mvx_scim_x"})
	if code != http.StatusForbidden || !strings.Contains(string(body), "scim:api:messages:2.0:Error") {
		t.Fatalf("community scim endpoint: %d %s", code, body)
	}
	// Discovery is public and edition-independent: it only says "no".
	code, body = f.do(t, f.communitySrv, http.MethodGet, "/api/sso/discover?email=a@acme.test", nil, nil)
	if code != http.StatusOK || decode(t, body)["sso"] != false {
		t.Fatalf("community discover: %d %s", code, body)
	}
}

func TestSsoRegisterDiscoverAndFirstLogin(t *testing.T) {
	f := setupSsoFixture(t)
	ctx := context.Background()
	alias := sso.Alias(f.custID)

	code, body := f.do(t, f.srv, http.MethodGet, "/api/admin/sso", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("GET: %d %s", code, body)
	}
	initial := decode(t, body)
	if initial["configured"] != false || initial["registry_configured"] != true || !strings.HasSuffix(fmt.Sprint(initial["broker_endpoint"]), "/realms/mavericks/broker/"+alias+"/endpoint") {
		t.Fatalf("initial: %+v", initial)
	}

	// What the developer's screen would refuse, the API refuses.
	for name, in := range map[string]map[string]any{
		"no domains":   {"protocol": "oidc", "metadata_url": "https://idp.acme.test/.well-known/openid-configuration", "client_id": "mvx", "client_secret": "s", "allowed_domains": []string{}},
		"bad protocol": {"protocol": "ldap", "metadata_url": "https://x", "allowed_domains": []string{"acme.test"}},
		"no client id": {"protocol": "oidc", "metadata_url": "https://idp.acme.test/x", "client_secret": "s", "allowed_domains": []string{"acme.test"}},
		"no secret":    {"protocol": "oidc", "metadata_url": "https://idp.acme.test/x", "client_id": "mvx", "allowed_domains": []string{"acme.test"}},
		"unreadable":   {"protocol": "oidc", "metadata_url": "https://unreachable.test/x", "client_id": "mvx", "client_secret": "s", "allowed_domains": []string{"acme.test"}},
	} {
		if code, body := f.do(t, f.srv, http.MethodPut, "/api/admin/sso", in, nil); code == http.StatusOK {
			t.Errorf("%s: accepted: %s", name, body)
		}
	}

	code, body = f.do(t, f.srv, http.MethodPut, "/api/admin/sso", map[string]any{
		"protocol": "oidc", "display_name": "Acme SSO", "metadata_url": "https://idp.acme.test/.well-known/openid-configuration",
		"client_id": "mvx", "client_secret": "s3cret", "allowed_domains": []string{"acme.test", " @Acme.TEST ", ""},
		"jit_provisioning": true, "default_role": "business_user", "enabled": true,
	}, nil)
	if code != http.StatusOK {
		t.Fatalf("register: %d %s", code, body)
	}
	saved := decode(t, body)
	if saved["configured"] != true || saved["alias"] != alias || saved["enabled"] != true {
		t.Fatalf("saved: %+v", saved)
	}
	if doms := fmt.Sprint(saved["allowed_domains"]); doms != "[acme.test]" {
		t.Errorf("domains normalised to %s", doms)
	}
	if _, leaked := saved["client_secret"]; leaked {
		t.Errorf("client_secret returned: %+v", saved)
	}
	if got := f.broker.idpConfig(alias, "clientSecret"); got != "s3cret" {
		t.Errorf("secret in the registry = %q", got)
	}
	if got := f.broker.idpConfig(alias, "issuer"); got != "https://idp.acme.test" {
		t.Errorf("issuer imported = %q", got)
	}
	if hide := f.broker.idpConfig(alias, "hideOnLoginPage"); hide != "true" {
		t.Errorf("tenant provider must be hidden from the shared login page, got %q", hide)
	}

	// Saving again without a secret keeps the stored one.
	code, _ = f.do(t, f.srv, http.MethodPut, "/api/admin/sso", map[string]any{
		"protocol": "oidc", "display_name": "Acme SSO", "metadata_url": "https://idp.acme.test/.well-known/openid-configuration",
		"client_id": "mvx", "allowed_domains": []string{"acme.test"}, "jit_provisioning": true, "default_role": "business_user", "enabled": true,
	}, nil)
	if code != http.StatusOK || f.broker.idpConfig(alias, "clientSecret") != "s3cret" {
		t.Fatalf("re-save: %d secret=%q", code, f.broker.idpConfig(alias, "clientSecret"))
	}

	// Discovery: who has SSO, and where an address goes.
	for _, c := range []struct {
		email     string
		wantAlias string
	}{
		{"", ""}, {"Someone@ACME.test", alias}, {"x@other.test", ""}, {"garbage", ""},
	} {
		code, body := f.do(t, f.srv, http.MethodGet, "/api/sso/discover?email="+c.email, nil, map[string]string{"X-Forwarded-For": "10.0.0." + fmt.Sprint(len(c.email))})
		got := decode(t, body)
		if code != http.StatusOK || got["sso"] != true || fmt.Sprint(got["alias"]) != firstOr(c.wantAlias, "<nil>") {
			t.Errorf("discover %q: %d %s", c.email, code, body)
		}
		if c.wantAlias != "" && got["display_name"] != "Acme SSO" {
			t.Errorf("discover %q: display_name %v", c.email, got["display_name"])
		}
	}
	// The same database under a deployment whose licence lacks SSO: the
	// registered domain is not advertised.
	code, body = f.do(t, f.communitySrv, http.MethodGet, "/api/sso/discover?email=someone@acme.test", nil, map[string]string{"X-Forwarded-For": "10.0.1.1"})
	if got := decode(t, body); code != http.StatusOK || got["sso"] != false || got["alias"] != nil {
		t.Errorf("discover without an SSO licence: %d %s", code, body)
	}
	// ...and it is throttled per address.
	throttled := false
	for i := 0; i < 15; i++ {
		if code, _ := f.do(t, f.srv, http.MethodGet, "/api/sso/discover", nil, map[string]string{"X-Forwarded-For": "203.0.113.9"}); code == http.StatusTooManyRequests {
			throttled = true
			break
		}
	}
	if !throttled {
		t.Error("15 discovery calls from one address were never throttled")
	}

	// First login through the provider: the token's subject is unknown; the
	// federated link names the provider, the alias names the tenant.
	f.broker.mu.Lock()
	f.broker.federated["kc-new"] = []map[string]string{{"identityProvider": alias, "userId": "ext-9", "userName": "new@acme.test"}}
	f.broker.federated["kc-stranger"] = []map[string]string{{"identityProvider": alias, "userId": "ext-10", "userName": "new@other.test"}}
	f.broker.mu.Unlock()
	claims := func(sub, email, name string) *identity.Claims {
		return &identity.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: sub}, Email: email, Name: name}
	}
	a, err := f.h.jitProvision(ctx, claims("kc-new", "New@acme.test", "New Person"))
	if err != nil {
		t.Fatalf("first login: %v", err)
	}
	if a.Email != "new@acme.test" || !a.hasRole("business_user") {
		t.Fatalf("provisioned actor: %+v", a)
	}
	var cust, ws string
	if err := f.pool.QueryRow(ctx, `
		SELECT u.customer_id::text, COALESCE(ra.workspace_id::text,'') FROM identity.user u
		LEFT JOIN identity.role_assignment ra ON ra.user_id = u.id WHERE u.keycloak_sub = 'kc-new'`).Scan(&cust, &ws); err != nil {
		t.Fatal(err)
	}
	if cust != f.custID || ws != f.wsID {
		t.Errorf("account landed in customer %s workspace %s, want %s / %s", cust, ws, f.custID, f.wsID)
	}
	again, err := f.h.jitProvision(ctx, claims("kc-new", "new@acme.test", "New Person"))
	if err != nil || again.UserID != a.UserID {
		t.Fatalf("second login: %v %+v", err, again)
	}

	// Refusals carry a reason a person can read.
	var np *errNotProvisionable
	if _, err := f.h.jitProvision(ctx, claims("kc-stranger", "new@other.test", "Stranger")); !errors.As(err, &np) || !strings.Contains(np.reason, "not an address") {
		t.Errorf("foreign domain: %v", err)
	}
	if _, err := f.h.jitProvision(ctx, claims("kc-unlinked", "x@acme.test", "X")); err == nil || errors.As(err, &np) {
		t.Errorf("a realm user with no provider link must be refused plainly, got %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE identity.sso_provider SET jit_provisioning = FALSE`); err != nil {
		t.Fatal(err)
	}
	f.broker.mu.Lock()
	f.broker.federated["kc-late"] = []map[string]string{{"identityProvider": alias}}
	f.broker.mu.Unlock()
	if _, err := f.h.jitProvision(ctx, claims("kc-late", "late@acme.test", "Late")); !errors.As(err, &np) || !strings.Contains(np.reason, "has not been created") {
		t.Errorf("jit off: %v", err)
	}

	// A deactivated account is refused by every resolver, token or not.
	if _, err := f.pool.Exec(ctx, `UPDATE identity.user SET disabled_at = now() WHERE keycloak_sub = 'kc-new'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.h.actorByKeycloakSub(ctx, "kc-new"); err == nil {
		t.Error("gateway resolver accepted a disabled user")
	}
	if _, err := auth.ResolveActorByKeycloakSub(ctx, f.pool, "kc-new"); err == nil {
		t.Error("gRPC resolver accepted a disabled user")
	}

	// Removing the provider removes it from the registry and from discovery.
	code, body = f.do(t, f.srv, http.MethodDelete, "/api/admin/sso", nil, nil)
	if code != http.StatusOK || decode(t, body)["configured"] != false {
		t.Fatalf("remove: %d %s", code, body)
	}
	if _, still := f.broker.idps[alias]; still {
		t.Error("identity provider still registered after removal")
	}
	code, body = f.do(t, f.srv, http.MethodGet, "/api/sso/discover", nil, map[string]string{"X-Forwarded-For": "10.9.9.9"})
	if code != http.StatusOK || decode(t, body)["sso"] != false {
		t.Fatalf("discover after removal: %d %s", code, body)
	}
}

func firstOr(s, alt string) string {
	if s == "" {
		return alt
	}
	return s
}

func TestScimProvisioningLifecycle(t *testing.T) {
	f := setupSsoFixture(t)
	ctx := context.Background()

	code, body := f.do(t, f.srv, http.MethodPost, "/api/admin/scim/tokens", map[string]any{"name": "Entra ID"}, nil)
	if code != http.StatusCreated {
		t.Fatalf("issue: %d %s", code, body)
	}
	issued := decode(t, body)
	token := fmt.Sprint(issued["token"])
	if !strings.HasPrefix(token, "mvx_scim_") || issued["base_url"] != "https://console.test/api/scim/v2" {
		t.Fatalf("issued: %+v", issued)
	}
	code, body = f.do(t, f.srv, http.MethodGet, "/api/admin/scim/tokens", nil, nil)
	if code != http.StatusOK || strings.Contains(string(body), token) || !strings.Contains(string(body), `"name":"Entra ID"`) {
		t.Fatalf("list: %d %s", code, body)
	}
	bearer := map[string]string{"Authorization": "Bearer " + token, "X-Dev-User": ""}
	scimCall := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		code, raw := f.do(t, f.srv, method, "/api/scim/v2"+path, body, bearer)
		if len(raw) == 0 {
			return code, nil
		}
		return code, decode(t, raw)
	}

	if code, _ := f.do(t, f.srv, http.MethodGet, "/api/scim/v2/Users", nil, map[string]string{"X-Dev-User": ""}); code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := f.do(t, f.srv, http.MethodGet, "/api/scim/v2/Users", nil, map[string]string{"Authorization": "Bearer mvx_scim_00000000000000000000000000000000_" + strings.Repeat("x", 43)}); code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", code)
	}
	if code, cfg := scimCall(http.MethodGet, "/ServiceProviderConfig", nil); code != http.StatusOK || cfg["patch"].(map[string]any)["supported"] != true {
		t.Fatalf("ServiceProviderConfig: %d %+v", code, cfg)
	}

	// Create — the console's own sequence, visible in the fake registry.
	code, jane := scimCall(http.MethodPost, "/Users", map[string]any{
		"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:User"}, "userName": "Jane@Acme.test", "externalId": "ext-1",
		"name": map[string]string{"givenName": "Jane", "familyName": "Doe"}, "emails": []map[string]any{{"value": "Jane@Acme.test", "primary": true}}, "active": true,
	})
	if code != http.StatusCreated {
		t.Fatalf("create: %d %+v", code, jane)
	}
	janeID := fmt.Sprint(jane["id"])
	if jane["userName"] != "jane@acme.test" || jane["active"] != true || jane["externalId"] != "ext-1" {
		t.Fatalf("created resource: %+v", jane)
	}
	var sub, cust, role, ws string
	var scimManaged bool
	if err := f.pool.QueryRow(ctx, `
		SELECT u.keycloak_sub, u.customer_id::text, u.scim_managed, ra.role::text, ra.workspace_id::text
		FROM identity.user u JOIN identity.role_assignment ra ON ra.user_id = u.id WHERE u.id = $1::uuid`, janeID).Scan(&sub, &cust, &scimManaged, &role, &ws); err != nil {
		t.Fatal(err)
	}
	if cust != f.custID || !scimManaged || role != "business_user" || ws != f.wsID || !strings.HasPrefix(sub, "kc-sub-") {
		t.Fatalf("row: sub=%s cust=%s managed=%v role=%s ws=%s", sub, cust, scimManaged, role, ws)
	}
	if u := f.broker.users[sub]; u == nil || u.First != "Jane" || u.Last != "Doe" || !u.Enabled {
		t.Fatalf("registry account: %+v", f.broker.users[sub])
	}
	if len(f.broker.invited) != 1 {
		t.Errorf("no SSO on this tenant, so the invitation must be sent: invited=%v", f.broker.invited)
	}
	if code, _ := scimCall(http.MethodPost, "/Users", map[string]any{"userName": "jane@acme.test"}); code != http.StatusConflict {
		t.Errorf("duplicate create: %d", code)
	}

	// Filters the directories actually send.
	for filter, want := range map[string]int{
		`userName eq "jane@acme.test"`: 1, `externalId eq "ext-1"`: 1, `userName eq "nobody@acme.test"`: 0,
		// The tenant admin is an active user of the tenant too.
		`emails[type eq "work"].value eq "JANE@acme.test"`: 1, `active eq true`: 2,
	} {
		code, res := scimCall(http.MethodGet, "/Users?filter="+strings.ReplaceAll(filter, " ", "%20"), nil)
		if code != http.StatusOK || int(res["totalResults"].(float64)) != want {
			t.Errorf("filter %q: %d %+v", filter, code, res)
		}
	}
	if code, res := scimCall(http.MethodGet, "/Users?filter=userName%20co%20%22j%22", nil); code != http.StatusBadRequest || res["scimType"] != "invalidFilter" {
		t.Errorf("unsupported filter: %d %+v", code, res)
	}

	// Deactivate, Entra style: capitalised op, string boolean, no path.
	code, patched := scimCall(http.MethodPatch, "/Users/"+janeID, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":[{"op":"Replace","value":{"active":"False"}}]}`)
	if code != http.StatusOK || patched["active"] != false {
		t.Fatalf("deactivate: %d %+v", code, patched)
	}
	if u := f.broker.users[sub]; u.Enabled {
		t.Error("registry account still enabled after deactivation")
	}
	if _, err := f.h.actorByKeycloakSub(ctx, sub); err == nil {
		t.Error("a deactivated user still resolves")
	}
	// Reactivate and rename, Okta style (PUT).
	code, put := scimCall(http.MethodPut, "/Users/"+janeID, map[string]any{
		"userName": "jane@acme.test", "name": map[string]string{"givenName": "Jane", "familyName": "Smith"}, "active": true,
	})
	if code != http.StatusOK || put["active"] != true || put["displayName"] != "Jane Smith" {
		t.Fatalf("reactivate: %d %+v", code, put)
	}
	if u := f.broker.users[sub]; !u.Enabled || u.Last != "Smith" {
		t.Errorf("registry after PUT: %+v", u)
	}
	if _, err := f.h.actorByKeycloakSub(ctx, sub); err != nil {
		t.Errorf("reactivated user does not resolve: %v", err)
	}

	// Groups are business roles.
	code, grp := scimCall(http.MethodPost, "/Groups", map[string]any{"displayName": "Finance Review", "externalId": "g-1", "members": []map[string]string{{"value": janeID}}})
	if code != http.StatusCreated {
		t.Fatalf("create group: %d %+v", code, grp)
	}
	gid := fmt.Sprint(grp["id"])
	var roleWS, roleName string
	if err := f.pool.QueryRow(ctx, `SELECT workspace_id::text, name FROM identity.business_role WHERE id = $1::uuid`, gid).Scan(&roleWS, &roleName); err != nil {
		t.Fatal(err)
	}
	if roleWS != f.wsID || roleName != "Finance Review" {
		t.Errorf("business role: ws=%s name=%s", roleWS, roleName)
	}
	if code, res := scimCall(http.MethodGet, "/Groups?filter=displayName%20eq%20%22Finance%20Review%22", nil); code != http.StatusOK || int(res["totalResults"].(float64)) != 1 {
		t.Errorf("group filter: %d %+v", code, res)
	}
	code, _ = scimCall(http.MethodPatch, "/Groups/"+gid, `{"Operations":[{"op":"remove","path":"members[value eq \"`+janeID+`\"]"}]}`)
	if code != http.StatusOK {
		t.Fatalf("remove member: %d", code)
	}
	if _, g := scimCall(http.MethodGet, "/Groups/"+gid, nil); len(g["members"].([]any)) != 0 {
		t.Errorf("member not removed: %+v", g["members"])
	}
	code, _ = scimCall(http.MethodPatch, "/Groups/"+gid, `{"Operations":[{"op":"Add","path":"members","value":[{"value":"`+janeID+`"}]}]}`)
	if code != http.StatusOK {
		t.Fatalf("add member: %d", code)
	}
	if _, u := scimCall(http.MethodGet, "/Users/"+janeID, nil); len(u["groups"].([]any)) != 1 {
		t.Errorf("user does not show the group: %+v", u["groups"])
	}
	if code, res := scimCall(http.MethodPatch, "/Groups/"+gid, `{"Operations":[{"op":"add","path":"members","value":[{"value":"00000000-0000-0000-0000-000000000000"}]}]}`); code != http.StatusBadRequest || res["scimType"] != "invalidValue" {
		t.Errorf("unknown member: %d %+v", code, res)
	}

	// Delete: the row, the membership, the registry account.
	if code, _ := scimCall(http.MethodDelete, "/Users/"+janeID, nil); code != http.StatusNoContent {
		t.Fatalf("delete user: %d", code)
	}
	var n int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM identity.user WHERE id = $1::uuid`, janeID).Scan(&n)
	if n != 0 || len(f.broker.deleted) != 1 {
		t.Errorf("after delete: rows=%d registry deleted=%v", n, f.broker.deleted)
	}
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM identity.business_role_member WHERE role_id = $1::uuid`, gid).Scan(&n)
	if n != 0 {
		t.Errorf("membership survived the user: %d", n)
	}
	if code, _ := scimCall(http.MethodDelete, "/Groups/"+gid, nil); code != http.StatusNoContent {
		t.Fatalf("delete group: %d", code)
	}

	// Revocation is immediate.
	if code, _ := f.do(t, f.srv, http.MethodDelete, "/api/admin/scim/tokens/"+fmt.Sprint(issued["id"]), nil, nil); code != http.StatusOK {
		t.Fatalf("revoke: %d", code)
	}
	if code, _ := scimCall(http.MethodGet, "/Users", nil); code != http.StatusUnauthorized {
		t.Errorf("revoked token still works: %d", code)
	}
}

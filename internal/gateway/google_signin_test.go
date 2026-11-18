package gateway

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/mavericks-engine/mavericks/internal/identity"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// signGoogleToken mints a console token for sub with the claims Google's
// first login leaves on the realm account: address and names.
func signGoogleToken(t *testing.T, key *rsa.PrivateKey, kid, issuer, sub, email, given, family string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, identity.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: issuer, Subject: sub,
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
		AuthorizedParty: "mavericks-web",
		Email:           email, GivenName: given, FamilyName: family,
	})
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

// A first sign-in with Google is a self-service sign-up (owner, 2026-11-18:
// "also new sign-ups"): the realm account Google's first login made gets a
// tenant of its own, with every tenant role, once — however many of the
// console's first requests arrive together. Where sign-up is closed it is
// refused with a reason the console shows, and the realm account goes, so a
// later invitation to the address is not blocked by it.
func TestGoogleFirstSignInSignsUp(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	broker := newFakeBroker(t)
	jwksSrv, key, kid := newTestJWKS(t)
	validator, err := newValidatorFor(ctx, jwksSrv.URL)
	if err != nil {
		t.Fatalf("jwks validator: %v", err)
	}
	issuer := jwksSrv.URL + "/realms/mavericks"
	t.Setenv("DEV_MODE", "false")
	open := httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, validator, Deps{Keycloak: broker.client(), Signup: SignupConfig{Enabled: true}}))
	t.Cleanup(open.Close)
	closed := httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, validator, Deps{Keycloak: broker.client()}))
	t.Cleanup(closed.Close)

	get := func(srv *httptest.Server, path, token string) (int, map[string]any) {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close() //nolint:errcheck
		body, _ := io.ReadAll(res.Body)
		out := map[string]any{}
		_ = json.Unmarshal(body, &out)
		return res.StatusCode, out
	}

	// No provider yet: the sign-up page offers no Google button.
	if _, opts := get(open, "/api/signup/options", ""); opts["google"] != false {
		t.Fatalf("options without the provider: %v", opts)
	}
	broker.mu.Lock()
	broker.idps[googleIdPAlias] = map[string]any{"alias": googleIdPAlias, "providerId": "google", "enabled": true}
	link := func(sub, email string) {
		broker.users[sub] = &fakeKCUser{Email: email, Enabled: true}
		broker.federated[sub] = []map[string]string{{"identityProvider": googleIdPAlias, "userId": "g-" + sub, "userName": email}}
	}
	link("kc-ana", "ana@acme-co.test")
	// Any public mail service of the gateway's own list: the source names
	// none, as the public snapshot keeps personal-looking addresses out.
	var publicDomain string
	for d := range publicMailDomains {
		publicDomain = d
		break
	}
	bo := "bo@" + publicDomain
	link("kc-bo", bo)
	link("kc-cy", "cy@closed.test")
	broker.mu.Unlock()
	// The answer is remembered a minute: a fresh server asks again.
	fresh := httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, validator, Deps{Keycloak: broker.client(), Signup: SignupConfig{Enabled: true}}))
	t.Cleanup(fresh.Close)
	if _, opts := get(fresh, "/api/signup/options", ""); opts["google"] != true {
		t.Fatalf("options with the provider: %v", opts)
	}

	// The console's first requests, together: one tenant.
	ana := signGoogleToken(t, key, kid, issuer, "kc-ana", "Ana@Acme-Co.test", "Ana", "Lind")
	var wg sync.WaitGroup
	codes := make([]int, 5)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = get(open, "/api/me", ana)
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("request %d of the first sign-in: %d", i, c)
		}
	}
	var tenants, users int
	var tenantName, roles string
	if err := pool.QueryRow(ctx, `
		SELECT count(DISTINCT c.id), count(DISTINCT u.id), min(c.name),
		       (SELECT string_agg(ra.role::text, ',' ORDER BY ra.role::text) FROM identity.role_assignment ra WHERE ra.user_id = min(u.id::text)::uuid)
		FROM identity.user u JOIN core.customer c ON c.id = u.customer_id
		WHERE u.keycloak_sub = 'kc-ana'`).Scan(&tenants, &users, &tenantName, &roles); err != nil {
		t.Fatal(err)
	}
	if tenants != 1 || users != 1 || tenantName != "acme-co.test" {
		t.Errorf("Ana: %d tenants, %d accounts, tenant %q; want one each, named after her company's domain", tenants, users, tenantName)
	}
	if roles != "business_admin,business_user,developer,tenant_admin" {
		t.Errorf("Ana's roles: %q, want every tenant role", roles)
	}
	if _, me := get(open, "/api/me", ana); me["email"] != "ana@acme-co.test" {
		t.Errorf("Ana's /api/me: %v", me)
	}
	broker.mu.Lock()
	assigned := strings.Count(strings.Join(broker.realmRoles, ","), "kc-ana")
	broker.mu.Unlock()
	if assigned != len(everyTenantRole) {
		t.Errorf("realm roles assigned to Ana %d times, want %d (once each)", assigned, len(everyTenantRole))
	}

	// An address at a public mail service names the tenant after the person.
	if code, _ := get(open, "/api/me", signGoogleToken(t, key, kid, issuer, "kc-bo", bo, "Bo", "Kim")); code != http.StatusOK {
		t.Fatalf("Bo's first sign-in: %d", code)
	}
	if err := pool.QueryRow(ctx, `SELECT c.name FROM identity.user u JOIN core.customer c ON c.id = u.customer_id WHERE u.keycloak_sub = 'kc-bo'`).Scan(&tenantName); err != nil || tenantName != "Bo Kim" {
		t.Errorf("Bo's tenant: %q %v, want \"Bo Kim\"", tenantName, err)
	}

	// Sign-up closed: refused with the reason, and the realm account removed.
	code, out := get(closed, "/api/me", signGoogleToken(t, key, kid, issuer, "kc-cy", "cy@closed.test", "Cy", "Ng"))
	if code != http.StatusForbidden || !strings.Contains(out["error"].(string), "Ask your administrator to invite you") {
		t.Errorf("closed sign-up: %d %v, want 403 with the reason", code, out)
	}
	broker.mu.Lock()
	deleted := strings.Join(broker.deleted, ",")
	broker.mu.Unlock()
	if !strings.Contains(deleted, "kc-cy") {
		t.Errorf("the refused realm account was not removed (deleted: %q)", deleted)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM identity.user WHERE keycloak_sub = 'kc-cy'`).Scan(&n)
	if n != 0 {
		t.Errorf("a refused sign-in left %d accounts", n)
	}

	// A realm account not from Google stays unknown, as before.
	if code, _ := get(open, "/api/me", signGoogleToken(t, key, kid, issuer, "kc-plain", "plain@acme-co.test", "P", "L")); code != http.StatusUnauthorized {
		t.Errorf("an unknown subject without a Google link: %d, want 401", code)
	}
}

// Tests that creating a user in the console provisions a real identity
// provider account, and that a half-finished provisioning never survives.
//
// The bug this closes: POST /api/admin/users wrote an identity.user row whose
// keycloak_sub was the synthetic string "admin-created-<email>". In dev that
// doubles as an X-Dev-User persona, so the flow looked correct. Under JWKS
// validation the subject in a real token is a UUID minted by Keycloak, so it
// matched nothing — every console-created user got a bare 401, and the console
// reported success. It reached production and was found only when someone
// asked why an invitation had never arrived.
package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mavericks-engine/mavericks/internal/identity"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/db"
	"github.com/mavericks-engine/mavericks/pkg/keycloak"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/migrate"
)

// ── fake identity provider ────────────────────────────────────────────────

type fakeIDP struct {
	srv        *httptest.Server
	users      map[string]string // sub -> email
	roles      map[string][]string
	invited    []string
	deleted    []string
	failInvite bool
	nextID     int
}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	f := &fakeIDP{users: map[string]string{}, roles: map[string][]string{}}
	mux := http.NewServeMux()

	mux.HandleFunc("/realms/mavericks/protocol/openid-connect/token", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "t", "expires_in": 300})
	})
	mux.HandleFunc("/admin/realms/mavericks/users", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			email := r.URL.Query().Get("email")
			out := []map[string]string{}
			for sub, e := range f.users {
				if e == email {
					out = append(out, map[string]string{"id": sub, "email": e})
				}
			}
			_ = json.NewEncoder(w).Encode(out)
		case http.MethodPost:
			var u map[string]any
			_ = json.NewDecoder(r.Body).Decode(&u)
			f.nextID++
			sub := fmt.Sprintf("idp-sub-%d", f.nextID)
			f.users[sub] = fmt.Sprint(u["email"])
			w.Header().Set("Location", "/admin/realms/mavericks/users/"+sub)
			w.WriteHeader(http.StatusCreated)
		}
	})
	mux.HandleFunc("/admin/realms/mavericks/roles/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/admin/realms/mavericks/roles/")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "r-" + name, "name": name})
	})
	mux.HandleFunc("/admin/realms/mavericks/users/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/admin/realms/mavericks/users/")
		switch {
		case strings.HasSuffix(rest, "/role-mappings/realm"):
			sub := strings.TrimSuffix(rest, "/role-mappings/realm")
			var rs []map[string]string
			_ = json.NewDecoder(r.Body).Decode(&rs)
			for _, role := range rs {
				f.roles[sub] = append(f.roles[sub], role["name"])
			}
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(rest, "/execute-actions-email"):
			if f.failInvite {
				http.Error(w, `{"errorMessage":"Failed to send email"}`, http.StatusInternalServerError)
				return
			}
			f.invited = append(f.invited, strings.TrimSuffix(rest, "/execute-actions-email"))
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete:
			f.deleted = append(f.deleted, rest)
			delete(f.users, rest)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIDP) client() *keycloak.Client {
	return keycloak.New(f.srv.URL, "mavericks", "mavericks-admin", "secret", "https://console.test")
}

// ── fixture ───────────────────────────────────────────────────────────────

type provFixture struct {
	pool     *pgxpool.Pool
	srv      *httptest.Server
	idp      *fakeIDP
	adminSub string
}

func setupProvFixture(t *testing.T, idp *fakeIDP) *provFixture {
	t.Helper()
	ctx := context.Background()

	pool := testdb.New(t, migrationfs.FS, ".")

	f := &provFixture{pool: pool, idp: idp, adminSub: "prov-platform-admin"}
	var adminID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ($1,'pa@prov.test','PA') RETURNING id::text`,
		f.adminSub).Scan(&adminID); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid,'platform_admin')`, adminID); err != nil {
		t.Fatalf("seed admin role: %v", err)
	}

	t.Setenv("DEV_MODE", "true")
	deps := Deps{}
	if idp != nil {
		deps.Keycloak = idp.client()
	}
	f.srv = httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, nil, deps))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *provFixture) do(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf []byte
	if body != nil {
		buf, _ = json.Marshal(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, f.srv.URL+path, bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Dev-User", f.adminSub)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var parsed map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&parsed)
	return resp.StatusCode, parsed
}

func (f *provFixture) subFor(t *testing.T, email string) string {
	t.Helper()
	var sub string
	err := f.pool.QueryRow(context.Background(),
		`SELECT keycloak_sub FROM identity.user WHERE email=$1`, email).Scan(&sub)
	if err != nil {
		return ""
	}
	return sub
}

// ── tests ─────────────────────────────────────────────────────────────────

// The core fix: the stored subject must be the one Keycloak minted, because
// that is what arrives in the `sub` claim of a real token.
func TestCreateUserStoresRealIdentityProviderSubject(t *testing.T) {
	idp := newFakeIDP(t)
	f := setupProvFixture(t, idp)

	status, body := f.do(t, "POST", "/api/admin/users", map[string]any{
		"email": "new.dev@prov.test", "first_name": "New", "last_name": "Dev", "role": "developer",
	})
	if status != 200 {
		t.Fatalf("create user = %d, body %v", status, body)
	}
	if body["invited"] != true {
		t.Errorf("invited = %v, want true", body["invited"])
	}

	sub := f.subFor(t, "new.dev@prov.test")
	if strings.HasPrefix(sub, "admin-created-") {
		t.Fatalf("keycloak_sub = %q — the synthetic subject is back; this account cannot sign in", sub)
	}
	if idp.users[sub] != "new.dev@prov.test" {
		t.Errorf("stored sub %q does not correspond to an identity provider account", sub)
	}
	if got := idp.roles[sub]; len(got) != 1 || got[0] != "developer" {
		t.Errorf("identity provider roles = %v, want [developer]", got)
	}
	if len(idp.invited) != 1 || idp.invited[0] != sub {
		t.Errorf("invited = %v, want exactly [%s]", idp.invited, sub)
	}
}

// An invitation that cannot be sent means an account nobody can reach: no
// password is set at creation, so the link is the only way in. Reporting
// success there produces a user who looks provisioned and is not.
func TestCreateUserRollsBackWhenInvitationCannotBeSent(t *testing.T) {
	idp := newFakeIDP(t)
	idp.failInvite = true
	f := setupProvFixture(t, idp)

	status, body := f.do(t, "POST", "/api/admin/users", map[string]any{
		"email": "doomed@prov.test", "first_name": "Doomed", "last_name": "User", "role": "developer",
	})
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body %v", status, body)
	}
	if sub := f.subFor(t, "doomed@prov.test"); sub != "" {
		t.Errorf("an application user row survived a failed invitation (sub %q)", sub)
	}
	if len(idp.deleted) != 1 {
		t.Errorf("identity provider accounts deleted = %v, want the orphan cleaned up", idp.deleted)
	}
	if len(idp.users) != 0 {
		t.Errorf("identity provider still holds %v", idp.users)
	}
}

// Re-running a creation that previously failed after the account was made must
// adopt it. It must NOT then delete that pre-existing account if this attempt
// fails, since this request did not create it.
func TestCreateUserAdoptsExistingIdentityProviderAccount(t *testing.T) {
	idp := newFakeIDP(t)
	f := setupProvFixture(t, idp)
	ctx := context.Background()

	pre, err := idp.client().CreateUser(ctx, "orphan@prov.test", "Orphan", "User")
	if err != nil {
		t.Fatalf("seed orphan: %v", err)
	}

	status, body := f.do(t, "POST", "/api/admin/users", map[string]any{
		"email": "orphan@prov.test", "first_name": "Orphan", "last_name": "User", "role": "developer",
	})
	if status != 200 {
		t.Fatalf("create = %d, body %v", status, body)
	}
	if got := f.subFor(t, "orphan@prov.test"); got != pre {
		t.Errorf("stored sub = %q, want the adopted account %q", got, pre)
	}
	if len(idp.users) != 1 {
		t.Errorf("a duplicate account was created: %v", idp.users)
	}
}

func TestCreateUserDoesNotDeleteAdoptedAccountOnFailure(t *testing.T) {
	idp := newFakeIDP(t)
	f := setupProvFixture(t, idp)
	ctx := context.Background()

	pre, err := idp.client().CreateUser(ctx, "keepme@prov.test", "Keep", "Me")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	idp.failInvite = true

	status, _ := f.do(t, "POST", "/api/admin/users", map[string]any{
		"email": "keepme@prov.test", "first_name": "Keep", "last_name": "Me", "role": "developer",
	})
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", status)
	}
	if len(idp.deleted) != 0 {
		t.Errorf("deleted %v — this request did not create that account and must not remove it", idp.deleted)
	}
	if _, still := idp.users[pre]; !still {
		t.Error("the pre-existing identity provider account was removed")
	}
}

// Deleting a user must not leave the identity provider account behind: it
// keeps the address occupied, so re-creating the same person later silently
// adopts the old account rather than provisioning a fresh one.
func TestDeleteUserRemovesIdentityProviderAccount(t *testing.T) {
	idp := newFakeIDP(t)
	f := setupProvFixture(t, idp)

	_, body := f.do(t, "POST", "/api/admin/users", map[string]any{
		"email": "temp@prov.test", "first_name": "Temp", "last_name": "User", "role": "developer",
	})
	userID, _ := body["id"].(string)
	sub := f.subFor(t, "temp@prov.test")

	status, _ := f.do(t, "DELETE", "/api/admin/users/"+userID, nil)
	if status != 200 {
		t.Fatalf("delete = %d", status)
	}
	if len(idp.deleted) != 1 || idp.deleted[0] != sub {
		t.Errorf("deleted = %v, want [%s]", idp.deleted, sub)
	}
}

// Users predating provisioning carry a synthetic subject and no account.
// Deleting them must not attempt an identity provider call with a subject it
// has never seen.
func TestDeleteLegacyUserSkipsIdentityProvider(t *testing.T) {
	idp := newFakeIDP(t)
	f := setupProvFixture(t, idp)

	var legacyID string
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO identity.user (keycloak_sub, email, display_name)
		 VALUES ('admin-created-legacy@prov.test','legacy@prov.test','Legacy') RETURNING id::text`,
	).Scan(&legacyID); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}

	if status, _ := f.do(t, "DELETE", "/api/admin/users/"+legacyID, nil); status != 200 {
		t.Fatalf("delete = %d", status)
	}
	if len(idp.deleted) != 0 {
		t.Errorf("called the identity provider with a synthetic subject: %v", idp.deleted)
	}
}

func TestResendInvitation(t *testing.T) {
	idp := newFakeIDP(t)
	f := setupProvFixture(t, idp)

	_, body := f.do(t, "POST", "/api/admin/users", map[string]any{
		"email": "resend@prov.test", "first_name": "Resend", "last_name": "User", "role": "developer",
	})
	userID, _ := body["id"].(string)

	status, out := f.do(t, "POST", "/api/admin/users/"+userID+"/invite", nil)
	if status != 200 {
		t.Fatalf("resend = %d, body %v", status, out)
	}
	if len(idp.invited) != 2 {
		t.Errorf("invitations sent = %d, want 2 (creation + resend)", len(idp.invited))
	}
}

func TestResendInvitationRejectsLegacyUser(t *testing.T) {
	idp := newFakeIDP(t)
	f := setupProvFixture(t, idp)

	var legacyID string
	if err := f.pool.QueryRow(context.Background(),
		`INSERT INTO identity.user (keycloak_sub, email, display_name)
		 VALUES ('admin-created-old@prov.test','old@prov.test','Old') RETURNING id::text`,
	).Scan(&legacyID); err != nil {
		t.Fatalf("seed: %v", err)
	}

	status, out := f.do(t, "POST", "/api/admin/users/"+legacyID+"/invite", nil)
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %v", status, out)
	}
	if len(idp.invited) != 0 {
		t.Errorf("attempted to invite a subject the provider has never seen: %v", idp.invited)
	}
}

// The dev stack has no identity provider wired, and X-Dev-User personas stand
// in for real accounts. That path must keep working exactly as before.
func TestDevModeWithoutIdentityProviderKeepsSyntheticSubject(t *testing.T) {
	f := setupProvFixture(t, nil)

	status, body := f.do(t, "POST", "/api/admin/users", map[string]any{
		"email": "devpersona@prov.test", "display_name": "Dev Persona", "role": "developer",
	})
	if status != 200 {
		t.Fatalf("create = %d, body %v", status, body)
	}
	if body["invited"] != false {
		t.Errorf("invited = %v, want false with no provider configured", body["invited"])
	}
	if got := f.subFor(t, "devpersona@prov.test"); got != "admin-created-devpersona@prov.test" {
		t.Errorf("keycloak_sub = %q, want the dev persona form", got)
	}
}

func TestCreateUserRequiresEmail(t *testing.T) {
	f := setupProvFixture(t, newFakeIDP(t))
	status, _ := f.do(t, "POST", "/api/admin/users", map[string]any{"display_name": "No Email"})
	if status != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", status)
	}
}

// ── production mode: no provider configured ───────────────────────────────

// Outside dev mode there is no persona fallback, so writing a synthetic
// subject would recreate the original bug. Creation must refuse instead.
func TestProductionModeRefusesUserCreationWithoutIdentityProvider(t *testing.T) {
	ctx := context.Background()

	pgc, err := tcpostgres.Run(ctx, "postgres:16-alpine",
		tcpostgres.WithDatabase("mavericks"),
		tcpostgres.WithUsername("mavericks"),
		tcpostgres.WithPassword("mavericks"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
		),
	)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = pgc.Terminate(ctx) })

	dsn, _ := pgc.ConnectionString(ctx, "sslmode=disable")
	pool, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := migrate.Run(ctx, pool, migrationfs.FS, "."); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	jwksSrv, key, kid := newTestJWKS(t)
	validator, err := newValidatorFor(ctx, jwksSrv.URL)
	if err != nil {
		t.Fatalf("jwks validator: %v", err)
	}

	const adminSub = "prod-admin-sub"
	var adminID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ($1,'pa@prod.test','PA') RETURNING id::text`,
		adminSub).Scan(&adminID); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid,'platform_admin')`, adminID); err != nil {
		t.Fatalf("seed role: %v", err)
	}

	t.Setenv("DEV_MODE", "false")
	srv := httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, validator, Deps{}))
	t.Cleanup(srv.Close)

	token := signTestToken(t, key, kid, jwksSrv.URL+"/realms/mavericks", adminSub)
	// Both names given: this test is about refusing when no identity provider is
	// configured, not about name validation, and a payload missing a surname
	// would now be rejected before it reached that check.
	buf, _ := json.Marshal(map[string]any{"email": "nope@prod.test", "first_name": "No", "last_name": "Pe", "role": "developer"})
	req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/api/admin/users", bytes.NewReader(buf))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 — creation must refuse rather than write an unusable account", resp.StatusCode)
	}
	var n int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM identity.user WHERE email='nope@prod.test'`).Scan(&n)
	if n != 0 {
		t.Errorf("a user row was written despite the refusal (%d rows)", n)
	}
}

// ── small JWKS fixture, local to this file ────────────────────────────────

func newTestJWKS(t *testing.T) (*httptest.Server, *rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	const kid = "prov-kid"
	doc := map[string]any{"keys": []map[string]any{{
		"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
		"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
	}}}
	mux := http.NewServeMux()
	mux.HandleFunc("/realms/mavericks/protocol/openid-connect/certs",
		func(w http.ResponseWriter, _ *http.Request) { _ = json.NewEncoder(w).Encode(doc) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, key, kid
}

// The fixture is reached directly, so the fetch URL and the issuer are the
// same and the public-issuer argument stays empty.
func newValidatorFor(ctx context.Context, jwksBaseURL string) (*identity.JWKSValidator, error) {
	return identity.NewJWKSValidator(ctx, jwksBaseURL, "mavericks", "")
}

func signTestToken(t *testing.T, key *rsa.PrivateKey, kid, issuer, sub string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer: issuer, Subject: sub,
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		IssuedAt:  jwt.NewNumericDate(time.Now()),
	})
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

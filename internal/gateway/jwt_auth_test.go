// Tests for the P0-1 fix: outside DEV_MODE, resolveActor must validate a
// real Bearer token against JWKS and must never fall back to the
// X-Dev-User dev-persona path. internal/identity.JWKSValidator's own
// Validate logic (issuer + expiry + signature) already existed before this
// fix but had no test anywhere in the repo; this exercises it end to end
// through the real HTTP handler, without needing a running Keycloak — a
// local httptest server serves a real JWK Set built from a freshly
// generated RSA key, and github.com/golang-jwt/jwt/v5 signs tokens against
// the matching private key.
package gateway_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MicahParks/jwkset"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/gateway"
	"github.com/mavericks-engine/mavericks/internal/identity"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

const jwtTestRealm = "test-realm"

func setupJWTAuthDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	pool := testdb.New(t, migrationfs.FS, ".")
	return pool
}

// jwksFixture serves a real JWK Set — backed by a freshly generated RSA
// key — at the same path shape identity.NewJWKSValidator expects
// (/realms/{realm}/protocol/openid-connect/certs), so JWKSValidator can be
// exercised without a running Keycloak instance.
type jwksFixture struct {
	srv        *httptest.Server
	privateKey *rsa.PrivateKey
	issuer     string
}

func setupJWKSFixture(t *testing.T) *jwksFixture {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}

	jwk, err := jwkset.NewJWKFromKey(privateKey, jwkset.JWKOptions{
		Metadata: jwkset.JWKMetadataOptions{KID: "test-kid", ALG: jwkset.AlgRS256, USE: jwkset.UseSig},
	})
	if err != nil {
		t.Fatalf("build jwk: %v", err)
	}
	store := jwkset.NewMemoryStorage()
	if err := store.KeyWrite(context.Background(), jwk); err != nil {
		t.Fatalf("write jwk to store: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/realms/"+jwtTestRealm+"/protocol/openid-connect/certs", func(w http.ResponseWriter, r *http.Request) {
		body, err := store.JSONPublic(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})

	f := &jwksFixture{privateKey: privateKey, srv: httptest.NewServer(mux)}
	f.issuer = f.srv.URL + "/realms/" + jwtTestRealm
	return f
}

// sign mints a token for sub/issuer/expiresAt, signed with the fixture's
// private key and a "kid" header matching the JWK served above.
func (f *jwksFixture) sign(t *testing.T, sub, issuer string, expiresAt time.Time) string {
	t.Helper()
	claims := identity.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   sub,
			Issuer:    issuer,
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-time.Minute)),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "test-kid"
	signed, err := token.SignedString(f.privateKey)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

func TestResolveActorValidatesRealJWT(t *testing.T) {
	ctx := context.Background()
	pool := setupJWTAuthDB(t)
	jf := setupJWKSFixture(t)
	defer jf.srv.Close()

	var userID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('test-sub-001', 'jwt@t.com', 'JWT User') RETURNING id::text
	`).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	// Empty public issuer: this fixture reaches Keycloak directly, so the
	// fetch URL and the issuer really are the same. The split between them is
	// covered in internal/identity/jwks_test.go.
	jwks, err := identity.NewJWKSValidator(ctx, jf.srv.URL, jwtTestRealm, "")
	if err != nil {
		t.Fatalf("new jwks validator: %v", err)
	}

	// No t.Setenv("DEV_MODE", "true") anywhere in this test — this is
	// exactly the production-mode path the fix closes.
	srv := httptest.NewServer(gateway.NewHandler(logger.New("test"), pool, jwks))
	defer srv.Close()

	get := func(t *testing.T, headers map[string]string) (int, map[string]any) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/me", nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		defer resp.Body.Close() //nolint:errcheck
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}

	t.Run("valid token resolves the actor", func(t *testing.T) {
		token := jf.sign(t, "test-sub-001", jf.issuer, time.Now().Add(time.Hour))
		status, body := get(t, map[string]string{"Authorization": "Bearer " + token})
		if status != http.StatusOK {
			t.Fatalf("status = %d, body = %v, want 200", status, body)
		}
		if body["user_id"] != userID {
			t.Errorf("user_id = %v, want %v", body["user_id"], userID)
		}
	})

	t.Run("expired token is rejected", func(t *testing.T) {
		token := jf.sign(t, "test-sub-001", jf.issuer, time.Now().Add(-time.Hour))
		status, body := get(t, map[string]string{"Authorization": "Bearer " + token})
		if status != http.StatusUnauthorized {
			t.Errorf("status = %d, body = %v, want 401", status, body)
		}
	})

	t.Run("wrong issuer is rejected", func(t *testing.T) {
		token := jf.sign(t, "test-sub-001", "https://not-the-real-issuer/realms/other", time.Now().Add(time.Hour))
		status, body := get(t, map[string]string{"Authorization": "Bearer " + token})
		if status != http.StatusUnauthorized {
			t.Errorf("status = %d, body = %v, want 401", status, body)
		}
	})

	t.Run("missing token is rejected", func(t *testing.T) {
		status, body := get(t, nil)
		if status != http.StatusUnauthorized {
			t.Errorf("status = %d, body = %v, want 401", status, body)
		}
	})

	t.Run("X-Dev-User alone has no effect outside dev mode", func(t *testing.T) {
		status, body := get(t, map[string]string{"X-Dev-User": "dept_head"})
		if status != http.StatusUnauthorized {
			t.Errorf("status = %d, body = %v, want 401 — dev-persona header must not bypass JWT auth outside DEV_MODE", status, body)
		}
	})
}

func TestNewHandlerNilJWKSOutsideDevModeRejectsEveryRequest(t *testing.T) {
	pool := setupJWTAuthDB(t)

	// A nil validator with DEV_MODE unset (the exact startup-safety-net
	// scenario cmd/gateway's own fail-closed check is meant to prevent from
	// ever being reachable in a real deployment) must reject every request
	// rather than silently falling back to dev-persona auth.
	srv := httptest.NewServer(gateway.NewHandler(logger.New("test"), pool, nil))
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/api/me", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Dev-User", "dept_head")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

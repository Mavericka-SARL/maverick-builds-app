package identity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// These tests cover the split between the address JWKS is FETCHED from and the
// issuer tokens are VALIDATED against — the two are the same only when
// Keycloak is reached directly, and differ in every deployment that puts it
// behind an ingress.
//
// Deriving the issuer from the fetch URL (the original behaviour) rejects
// every genuine token in that shape. It reached production before anything
// caught it, because no test here exercised a token whose `iss` was not the
// in-cluster Service name.

// newJWKSServer serves a JWKS document for a freshly generated RSA key, and
// returns the key for signing test tokens.
func newJWKSServer(t *testing.T) (*httptest.Server, *rsa.PrivateKey, string) {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	const kid = "test-key-1"

	doc := map[string]any{
		"keys": []map[string]any{{
			"kty": "RSA",
			"kid": kid,
			"use": "sig",
			"alg": "RS256",
			"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/realms/mavericks/protocol/openid-connect/certs",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(doc)
		})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, key, kid
}

func signToken(t *testing.T, key *rsa.PrivateKey, kid, issuer, sub string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer:    issuer,
		Subject:   sub,
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		IssuedAt:  jwt.NewNumericDate(time.Now()),
	})
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

// The production shape: JWKS fetched in-cluster, tokens minted with the public
// origin. This is the case that was broken.
func TestValidateAcceptsPublicIssuerWhenFetchURLDiffers(t *testing.T) {
	srv, key, kid := newJWKSServer(t)
	const publicIssuer = "https://auth.example.com"

	v, err := NewJWKSValidator(context.Background(), srv.URL, "mavericks", publicIssuer)
	if err != nil {
		t.Fatalf("NewJWKSValidator: %v", err)
	}

	token := signToken(t, key, kid, publicIssuer+"/realms/mavericks", "user-sub-123")

	claims, err := v.Validate(token)
	if err != nil {
		t.Fatalf("Validate rejected a token minted by the very Keycloak it fetched keys from: %v", err)
	}
	if claims.Subject != "user-sub-123" {
		t.Errorf("Subject = %q, want %q", claims.Subject, "user-sub-123")
	}
}

// The regression guard: without the public issuer configured, the validator
// falls back to deriving it from the fetch URL, and that token no longer
// verifies. Reverting NewJWKSValidator to its original single-URL behaviour
// makes the test above fail exactly this way.
func TestValidateRejectsPublicIssuerWhenNotConfigured(t *testing.T) {
	srv, key, kid := newJWKSServer(t)

	v, err := NewJWKSValidator(context.Background(), srv.URL, "mavericks", "")
	if err != nil {
		t.Fatalf("NewJWKSValidator: %v", err)
	}

	token := signToken(t, key, kid, "https://auth.example.com/realms/mavericks", "user-sub-123")

	if _, err := v.Validate(token); err == nil {
		t.Fatal("Validate accepted a token whose issuer was neither configured nor derivable; " +
			"issuer validation is not actually running")
	}
}

// The dev-stack shape: Keycloak reached directly, so the empty publicIssuer
// must keep deriving the issuer from the fetch URL.
func TestValidateEmptyIssuerFallsBackToFetchURL(t *testing.T) {
	srv, key, kid := newJWKSServer(t)

	v, err := NewJWKSValidator(context.Background(), srv.URL, "mavericks", "")
	if err != nil {
		t.Fatalf("NewJWKSValidator: %v", err)
	}

	token := signToken(t, key, kid, srv.URL+"/realms/mavericks", "dev-sub")

	if _, err := v.Validate(token); err != nil {
		t.Fatalf("Validate rejected a token issued by the fetch URL itself: %v", err)
	}
}

// A trailing slash on KEYCLOAK_ISSUER is an easy thing to paste into a
// ConfigMap, and would otherwise produce "https://host//realms/..." — an
// issuer that matches nothing, failing the same silent-401 way.
func TestValidateToleratesTrailingSlashOnIssuer(t *testing.T) {
	srv, key, kid := newJWKSServer(t)

	v, err := NewJWKSValidator(context.Background(), srv.URL, "mavericks", "https://auth.example.com/")
	if err != nil {
		t.Fatalf("NewJWKSValidator: %v", err)
	}

	token := signToken(t, key, kid, "https://auth.example.com/realms/mavericks", "user-sub-123")

	if _, err := v.Validate(token); err != nil {
		t.Fatalf("Validate rejected a valid token because the configured issuer had a trailing slash: %v", err)
	}
}

// Expiry is enforced independently of the issuer change.
func TestValidateRejectsExpiredToken(t *testing.T) {
	srv, key, kid := newJWKSServer(t)
	const publicIssuer = "https://auth.example.com"

	v, err := NewJWKSValidator(context.Background(), srv.URL, "mavericks", publicIssuer)
	if err != nil {
		t.Fatalf("NewJWKSValidator: %v", err)
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer:    publicIssuer + "/realms/mavericks",
		Subject:   "user-sub-123",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Minute)),
		IssuedAt:  jwt.NewNumericDate(time.Now().Add(-time.Hour)),
	})
	tok.Header["kid"] = kid
	signed, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}

	_, err = v.Validate(signed)
	if err == nil {
		t.Fatal("Validate accepted an expired token")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("error = %v, want it to mention expiry", err)
	}
}

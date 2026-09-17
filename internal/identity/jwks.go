package identity

import (
	"context"
	"fmt"
	"strings"

	"github.com/MicahParks/keyfunc/v3"
	"github.com/golang-jwt/jwt/v5"
)

// JWKSValidator validates JWTs against Keycloak's JWKS endpoint.
type JWKSValidator struct {
	jwks   keyfunc.Keyfunc
	issuer string
}

type Claims struct {
	jwt.RegisteredClaims
	Email             string `json:"email"`
	PreferredUsername string `json:"preferred_username"`
	// Name claims, present when the realm maps them (it does by default);
	// first-login provisioning uses them for the display name.
	Name       string `json:"name"`
	GivenName  string `json:"given_name"`
	FamilyName string `json:"family_name"`
	RealmAccess       struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

// NewJWKSValidator builds a validator that fetches signing keys from
// keycloakURL and requires every token to carry the matching issuer.
//
// keycloakURL and publicIssuer are separate because they are genuinely
// different URLs whenever Keycloak sits behind a reverse proxy, which is the
// normal production shape. In-cluster callers reach it at a Service name
// (http://keycloak:8080) and should keep doing so — that is the fast path and
// it does not depend on DNS, the load balancer, or the internet being up. But
// Keycloak MINTS tokens with the public origin it was given in KC_HOSTNAME
// (https://auth.example.com), so `iss` never matches the Service name.
//
// Deriving the issuer from keycloakURL, as this did originally, therefore
// rejects every genuine token the moment those two differ — with a plain 401
// that looks like a Keycloak misconfiguration rather than a URL mismatch.
// That is exactly what happened on the first real deployment.
//
// publicIssuer may be empty, which restores the old derive-from-keycloakURL
// behaviour. That keeps the dev stack (where Keycloak is reached directly and
// the two URLs really are the same) working with no configuration at all.
func NewJWKSValidator(ctx context.Context, keycloakURL, realm, publicIssuer string) (*JWKSValidator, error) {
	jwksURL := fmt.Sprintf("%s/realms/%s/protocol/openid-connect/certs", keycloakURL, realm)

	jwks, err := keyfunc.NewDefaultCtx(ctx, []string{jwksURL})
	if err != nil {
		return nil, fmt.Errorf("init jwks from %s: %w", jwksURL, err)
	}

	issuer := publicIssuer
	if issuer == "" {
		issuer = keycloakURL
	}
	return &JWKSValidator{jwks: jwks, issuer: fmt.Sprintf("%s/realms/%s", strings.TrimSuffix(issuer, "/"), realm)}, nil
}

func (v *JWKSValidator) Validate(tokenStr string) (*Claims, error) {
	token, err := jwt.ParseWithClaims(tokenStr, &Claims{}, v.jwks.Keyfunc,
		jwt.WithIssuer(v.issuer),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("jwt parse: %w", err)
	}

	claims, ok := token.Claims.(*Claims)
	if !ok || !token.Valid {
		return nil, fmt.Errorf("invalid token claims")
	}

	return claims, nil
}

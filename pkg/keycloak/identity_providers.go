package keycloak

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Identity brokering and account state — what single sign-on (ee/sso) and
// SCIM provisioning (ee/scim) need beyond the user-creation calls in
// client.go. The service account gains two realm-management roles for this:
// view-identity-providers and manage-identity-providers (see
// scripts/keycloak-provisioning-setup.sh).

// MaskedSecret is what Keycloak returns in place of a stored secret, and
// what it accepts on update to mean "keep the stored one".
const MaskedSecret = "**********"

// IdentityProvider is the subset of Keycloak's IdentityProviderRepresentation
// the platform sets. Keycloak 24 (the deployed version) rejects a field it
// does not know with 400 — "hideOnLogin" became a top-level field only in
// Keycloak 26; here it is config["hideOnLoginPage"]. Found live, not by any
// unit test, which is the point of registering against a real Keycloak
// before shipping. Config keys are Keycloak's own: for "oidc" —
// clientId, clientSecret, authorizationUrl, tokenUrl, jwksUrl, issuer,
// defaultScope, validateSignature, useJwksUrl, syncMode; for "saml" —
// singleSignOnServiceUrl, idpEntityId, entityId, nameIDPolicyFormat,
// principalType, signingCertificate, validateSignature, wantAuthnRequestsSigned.
type IdentityProvider struct {
	Alias                     string            `json:"alias"`
	ProviderID                string            `json:"providerId"` // "oidc" | "saml"
	DisplayName               string            `json:"displayName,omitempty"`
	Enabled                   bool              `json:"enabled"`
	TrustEmail                bool              `json:"trustEmail"`
	StoreToken                bool              `json:"storeToken"`
	FirstBrokerLoginFlowAlias string            `json:"firstBrokerLoginFlowAlias,omitempty"`
	Config                    map[string]string `json:"config"`
}

// ImportIdentityProviderConfig asks Keycloak to read an OIDC discovery
// document or a SAML metadata document at fromURL and returns the provider
// config it derives (endpoints, issuer, certificate). Keycloak does the
// fetch, so the URL must be reachable from the Keycloak pod, not from here.
func (c *Client) ImportIdentityProviderConfig(ctx context.Context, providerID, fromURL string) (map[string]string, error) {
	var out map[string]string
	if _, err := c.do(ctx, http.MethodPost, "/identity-provider/import-config",
		map[string]string{"providerId": providerID, "fromUrl": fromURL}, &out); err != nil {
		return nil, fmt.Errorf("import identity provider configuration from %s: %w", fromURL, err)
	}
	return out, nil
}

// GetIdentityProvider returns the provider with alias, or nil when none exists.
func (c *Client) GetIdentityProvider(ctx context.Context, alias string) (*IdentityProvider, error) {
	var out IdentityProvider
	res, err := c.do(ctx, http.MethodGet, "/identity-provider/instances/"+url.PathEscape(alias), nil, &out)
	if err != nil {
		if res.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &out, nil
}

// UpsertIdentityProvider creates the provider or replaces its configuration.
// The secret in Config is write-only on Keycloak's side: a later Get
// returns "**********" for clientSecret, which callers must not write back.
func (c *Client) UpsertIdentityProvider(ctx context.Context, idp IdentityProvider) error {
	existing, err := c.GetIdentityProvider(ctx, idp.Alias)
	if err != nil {
		return err
	}
	if existing == nil {
		if _, err := c.do(ctx, http.MethodPost, "/identity-provider/instances", idp, nil); err != nil {
			return fmt.Errorf("create identity provider %s: %w", idp.Alias, err)
		}
		return nil
	}
	if secret, ok := idp.Config["clientSecret"]; ok && (secret == "" || strings.Trim(secret, "*") == "") {
		// Keep the stored secret when the caller did not supply a new one.
		// Keycloak masks a secret as "**********" on read and, on update,
		// treats exactly that value as "leave the stored secret alone"
		// (ComponentRepresentation.SECRET_VALUE). Sending an empty string
		// would clear it, so the sentinel is set deliberately here.
		idp.Config["clientSecret"] = MaskedSecret
		for k, v := range existing.Config {
			if _, set := idp.Config[k]; !set {
				idp.Config[k] = v
			}
		}
	}
	if _, err := c.do(ctx, http.MethodPut, "/identity-provider/instances/"+url.PathEscape(idp.Alias), idp, nil); err != nil {
		return fmt.Errorf("update identity provider %s: %w", idp.Alias, err)
	}
	return nil
}

// DeleteIdentityProvider removes the provider; a missing one is not an error.
func (c *Client) DeleteIdentityProvider(ctx context.Context, alias string) error {
	res, err := c.do(ctx, http.MethodDelete, "/identity-provider/instances/"+url.PathEscape(alias), nil, nil)
	if err != nil && res.StatusCode != http.StatusNotFound {
		return err
	}
	return nil
}

// FederatedIdentity is one link between a Keycloak user and the external
// identity provider that authenticated them.
type FederatedIdentity struct {
	IdentityProvider string `json:"identityProvider"`
	UserID           string `json:"userId"`
	UserName         string `json:"userName"`
}

// FederatedIdentities returns which brokered providers a user signed in
// through. This is how first-login provisioning learns the tenant: the
// token itself carries no such claim, and asking Keycloak needs only
// view-users, which the service account already holds.
func (c *Client) FederatedIdentities(ctx context.Context, sub string) ([]FederatedIdentity, error) {
	var out []FederatedIdentity
	if _, err := c.do(ctx, http.MethodGet, "/users/"+url.PathEscape(sub)+"/federated-identity", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetUser returns the account, or nil when it does not exist.
func (c *Client) GetUser(ctx context.Context, sub string) (*User, error) {
	var out User
	res, err := c.do(ctx, http.MethodGet, "/users/"+url.PathEscape(sub), nil, &out)
	if err != nil {
		if res.StatusCode == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &out, nil
}

// SetUserEnabled switches an account on or off. A disabled account cannot
// obtain a token, so a SCIM deactivation cuts sign-in immediately rather
// than only at the next application-side check.
func (c *Client) SetUserEnabled(ctx context.Context, sub string, enabled bool) error {
	if _, err := c.do(ctx, http.MethodPut, "/users/"+url.PathEscape(sub), map[string]any{"enabled": enabled}, nil); err != nil {
		return fmt.Errorf("set user %s enabled=%v: %w", sub, enabled, err)
	}
	return nil
}

// UpdateUserProfile changes the name and address Keycloak shows; the
// application keeps its own copy in identity.user.
func (c *Client) UpdateUserProfile(ctx context.Context, sub, email, firstName, lastName string) error {
	body := map[string]any{"firstName": firstName, "lastName": lastName}
	if email != "" {
		body["email"] = email
		body["username"] = email
	}
	if _, err := c.do(ctx, http.MethodPut, "/users/"+url.PathEscape(sub), body, nil); err != nil {
		return fmt.Errorf("update user %s profile: %w", sub, err)
	}
	return nil
}

// BrokerEndpoint is the URL the tenant registers at their IdP as the
// redirect URI (OIDC) or assertion consumer service (SAML): where Keycloak
// receives the provider's response. publicBase is Keycloak's public origin.
func BrokerEndpoint(publicBase, realm, alias string) string {
	return fmt.Sprintf("%s/realms/%s/broker/%s/endpoint", strings.TrimSuffix(publicBase, "/"), realm, alias)
}

// SAMLEntityID is the service-provider entity id Keycloak presents for a
// realm; the tenant's IdP needs it to trust assertions requests.
func SAMLEntityID(publicBase, realm string) string {
	return fmt.Sprintf("%s/realms/%s", strings.TrimSuffix(publicBase, "/"), realm)
}

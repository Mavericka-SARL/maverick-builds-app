// Package sso is the enterprise single sign-on feature: a tenant registers
// its own identity provider (OIDC or SAML) and its people sign in through
// it. Licensed under ee/LICENSE; gated by license.FeatureSSO.
//
// The mechanism is Keycloak identity brokering. The platform's realm stays
// the only issuer the gateway trusts; each tenant's provider becomes one
// Keycloak identity provider, aliased by tenant, hidden from the shared
// login page and reached through kc_idp_hint after the sign-in page has
// worked out which tenant an e-mail address belongs to. The provider's
// secret lives in Keycloak only.
//
// A person's first brokered login creates their account here when the
// tenant allows it (jit_provisioning), provided their address is in one of
// the tenant's allowed domains — an identity provider can assert any
// e-mail it likes, and the allow-list is what keeps it to its own people.
package sso

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/pkg/keycloak"
)

// Settings is the one row of identity.sso_provider as the console sees it.
// ClientSecret is write-only: accepted on save, forwarded to Keycloak, never
// stored or returned here.
type Settings struct {
	Alias           string   `json:"alias"`
	Protocol        string   `json:"protocol"`
	DisplayName     string   `json:"display_name"`
	MetadataURL     string   `json:"metadata_url"`
	ClientID        string   `json:"client_id"`
	ClientSecret    string   `json:"client_secret,omitempty"`
	AllowedDomains  []string `json:"allowed_domains"`
	JITProvisioning bool     `json:"jit_provisioning"`
	DefaultRole     string   `json:"default_role"`
	Enabled         bool     `json:"enabled"`
	// Configured says a provider is registered in Keycloak under Alias.
	Configured bool `json:"configured"`
}

// Alias is the Keycloak identity-provider alias for a tenant: stable,
// URL-safe, and reversible (see CustomerFromAlias) so a brokered login can
// be traced back to its tenant without a lookup.
func Alias(customerID string) string {
	return "mvx-" + strings.ReplaceAll(strings.ToLower(customerID), "-", "")
}

// CustomerFromAlias inverts Alias; ok is false for any other alias.
func CustomerFromAlias(alias string) (string, bool) {
	hex := strings.TrimPrefix(alias, "mvx-")
	if hex == alias || len(hex) != 32 {
		return "", false
	}
	return hex[0:8] + "-" + hex[8:12] + "-" + hex[12:16] + "-" + hex[16:20] + "-" + hex[20:32], true
}

// Store reads and writes the tenant's row.
type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// Get returns the tenant's row (migration 091: one per tenant); a tenant
// that has never registered a provider reports the defaults.
func (s *Store) Get(ctx context.Context, customerID string) (Settings, error) {
	var out Settings
	err := s.pool.QueryRow(ctx, `
		SELECT alias, protocol, display_name, metadata_url, client_id, allowed_domains,
		       jit_provisioning, default_role::text, enabled
		FROM identity.sso_provider WHERE customer_id = $1::uuid
	`, customerID).Scan(&out.Alias, &out.Protocol, &out.DisplayName, &out.MetadataURL, &out.ClientID, &out.AllowedDomains,
		&out.JITProvisioning, &out.DefaultRole, &out.Enabled)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Settings{Protocol: "oidc", DefaultRole: "business_user", JITProvisioning: true, AllowedDomains: []string{}}, nil
		}
		return Settings{}, fmt.Errorf("read sso settings: %w", err)
	}
	if out.AllowedDomains == nil {
		out.AllowedDomains = []string{}
	}
	out.Configured = out.Alias != ""
	return out, nil
}

func (s *Store) save(ctx context.Context, customerID string, in Settings) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO identity.sso_provider
		    (customer_id, alias, protocol, display_name, metadata_url, client_id, allowed_domains, jit_provisioning, default_role, enabled)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9::identity.user_role, $10)
		ON CONFLICT (customer_id) DO UPDATE SET
		    alias = EXCLUDED.alias, protocol = EXCLUDED.protocol, display_name = EXCLUDED.display_name,
		    metadata_url = EXCLUDED.metadata_url, client_id = EXCLUDED.client_id, allowed_domains = EXCLUDED.allowed_domains,
		    jit_provisioning = EXCLUDED.jit_provisioning, default_role = EXCLUDED.default_role,
		    enabled = EXCLUDED.enabled, updated_at = now()
	`, customerID, in.Alias, in.Protocol, in.DisplayName, in.MetadataURL, in.ClientID, in.AllowedDomains,
		in.JITProvisioning, in.DefaultRole, in.Enabled)
	if err != nil {
		return fmt.Errorf("save sso settings: %w", err)
	}
	return nil
}

// clear forgets the tenant's provider.
func (s *Store) clear(ctx context.Context, customerID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM identity.sso_provider WHERE customer_id = $1::uuid`, customerID)
	return err
}

// Domains is the control-plane index from e-mail domain to tenant, kept in
// platform.sso_domain, so the sign-in page can route a person before it
// knows who they are.
type Domains struct{ control *pgxpool.Pool }

func NewDomains(control *pgxpool.Pool) *Domains { return &Domains{control: control} }

// Sync replaces the tenant's domains. A domain another tenant already
// claims is refused: two tenants cannot both own acme.com.
func (d *Domains) Sync(ctx context.Context, customerID, alias string, domains []string) error {
	tx, err := d.control.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, dom := range domains {
		var owner string
		if err := tx.QueryRow(ctx, `SELECT customer_id::text FROM platform.sso_domain WHERE domain = $1`, dom).Scan(&owner); err == nil && owner != customerID {
			return fmt.Errorf("the domain %s is already registered by another tenant", dom)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM platform.sso_domain WHERE customer_id = $1::uuid`, customerID); err != nil {
		return err
	}
	for _, dom := range domains {
		if _, err := tx.Exec(ctx, `
			INSERT INTO platform.sso_domain (domain, customer_id, alias) VALUES ($1, $2::uuid, $3)
		`, dom, customerID, alias); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Remove drops every domain of a tenant.
func (d *Domains) Remove(ctx context.Context, customerID string) error {
	_, err := d.control.Exec(ctx, `DELETE FROM platform.sso_domain WHERE customer_id = $1::uuid`, customerID)
	return err
}

// Lookup finds the provider alias for an e-mail domain, or "" when none.
func (d *Domains) Lookup(ctx context.Context, domain string) (customerID, alias string, err error) {
	err = d.control.QueryRow(ctx, `SELECT customer_id::text, alias FROM platform.sso_domain WHERE domain = $1`,
		strings.ToLower(domain)).Scan(&customerID, &alias)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	return customerID, alias, err
}

// Any reports whether any tenant has a provider at all — what decides
// whether the sign-in page shows the company-account option.
func (d *Domains) Any(ctx context.Context) (bool, error) {
	var n int
	err := d.control.QueryRow(ctx, `SELECT count(*) FROM platform.sso_domain`).Scan(&n)
	return n > 0, err
}

// Broker is the Keycloak calls this package makes; *keycloak.Client
// satisfies it, and tests substitute a fake.
type Broker interface {
	ImportIdentityProviderConfig(ctx context.Context, providerID, fromURL string) (map[string]string, error)
	UpsertIdentityProvider(ctx context.Context, idp keycloak.IdentityProvider) error
	DeleteIdentityProvider(ctx context.Context, alias string) error
}

// NormalizeDomains lower-cases and de-duplicates, dropping blanks and any
// leading "@" people type out of habit.
func NormalizeDomains(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, d := range in {
		d = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(d), "@")))
		if d == "" || seen[d] || strings.ContainsAny(d, " /@") {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// Register validates the settings, has Keycloak read the provider's
// discovery or metadata document, creates or updates the identity provider
// under the tenant's alias, then saves the row and the domain index. The
// secret goes to Keycloak and nowhere else.
func Register(ctx context.Context, store *Store, domains *Domains, broker Broker, customerID string, in Settings) (Settings, error) {
	in.Protocol = strings.ToLower(strings.TrimSpace(in.Protocol))
	if in.Protocol != "oidc" && in.Protocol != "saml" {
		return Settings{}, fmt.Errorf("protocol must be oidc or saml")
	}
	in.MetadataURL = strings.TrimSpace(in.MetadataURL)
	if !strings.HasPrefix(in.MetadataURL, "https://") && !strings.HasPrefix(in.MetadataURL, "http://") {
		return Settings{}, fmt.Errorf("metadata_url must be an http(s) address: the OIDC discovery document or the SAML metadata")
	}
	in.AllowedDomains = NormalizeDomains(in.AllowedDomains)
	if len(in.AllowedDomains) == 0 {
		return Settings{}, fmt.Errorf("at least one allowed e-mail domain is required — it is what keeps the provider to its own people")
	}
	switch in.DefaultRole {
	case "", "business_user", "business_admin", "developer", "tenant_admin":
		if in.DefaultRole == "" {
			in.DefaultRole = "business_user"
		}
	default:
		return Settings{}, fmt.Errorf("default_role must be one of business_user, business_admin, developer, tenant_admin")
	}
	if in.DisplayName == "" {
		in.DisplayName = "Company account"
	}
	in.Alias = Alias(customerID)

	cur, err := store.Get(ctx, customerID)
	if err != nil {
		return Settings{}, err
	}
	if in.Protocol == "oidc" {
		in.ClientID = strings.TrimSpace(in.ClientID)
		if in.ClientID == "" {
			return Settings{}, fmt.Errorf("client_id is required for an OIDC provider")
		}
		if strings.TrimSpace(in.ClientSecret) == "" && !cur.Configured {
			return Settings{}, fmt.Errorf("client_secret is required the first time an OIDC provider is registered")
		}
	}

	if broker != nil {
		cfg, err := broker.ImportIdentityProviderConfig(ctx, in.Protocol, in.MetadataURL)
		if err != nil {
			return Settings{}, fmt.Errorf("the identity provider's document could not be read: %w", err)
		}
		if in.Protocol == "oidc" {
			cfg["clientId"] = in.ClientID
			if s := strings.TrimSpace(in.ClientSecret); s != "" {
				cfg["clientSecret"] = s
			} else {
				cfg["clientSecret"] = "" // keep what Keycloak has
			}
			cfg["clientAuthMethod"] = "client_secret_post"
			cfg["defaultScope"] = "openid email profile"
			cfg["validateSignature"] = "true"
			cfg["useJwksUrl"] = "true"
		} else {
			cfg["principalType"] = "SUBJECT"
			cfg["validateSignature"] = "true"
			if cfg["nameIDPolicyFormat"] == "" {
				cfg["nameIDPolicyFormat"] = "urn:oasis:names:tc:SAML:1.1:nameid-format:emailAddress"
			}
		}
		// IMPORT: the account is created on first login and not overwritten
		// afterwards, so a name changed in the console survives.
		cfg["syncMode"] = "IMPORT"
		// Hidden from the shared login page: a tenant's provider is reached
		// through kc_idp_hint from the sign-in page, never listed for
		// everyone. (Keycloak 24 keeps this in config; 26 lifted it to a
		// top-level field.)
		cfg["hideOnLoginPage"] = "true"
		if err := broker.UpsertIdentityProvider(ctx, keycloak.IdentityProvider{
			Alias:       in.Alias,
			ProviderID:  in.Protocol,
			DisplayName: in.DisplayName,
			Enabled:     in.Enabled,
			TrustEmail:  true,
			// The default first-broker-login flow asks the person to
			// confirm/link an account when the e-mail already exists in the
			// realm — right for an invited user who now arrives through SSO.
			FirstBrokerLoginFlowAlias: "first broker login",
			Config:                    cfg,
		}); err != nil {
			return Settings{}, err
		}
	}
	if err := store.save(ctx, customerID, in); err != nil {
		return Settings{}, err
	}
	if domains != nil {
		if err := domains.Sync(ctx, customerID, in.Alias, in.AllowedDomains); err != nil {
			return Settings{}, err
		}
	}
	return store.Get(ctx, customerID)
}

// Remove deletes the provider from Keycloak and the tenant's row and domains.
// Accounts already created through it keep working with a password reset;
// what stops is signing in through the provider.
func Remove(ctx context.Context, store *Store, domains *Domains, broker Broker, customerID string) (Settings, error) {
	if broker != nil {
		if err := broker.DeleteIdentityProvider(ctx, Alias(customerID)); err != nil {
			return Settings{}, err
		}
	}
	if domains != nil {
		if err := domains.Remove(ctx, customerID); err != nil {
			return Settings{}, err
		}
	}
	if err := store.clear(ctx, customerID); err != nil {
		return Settings{}, err
	}
	return store.Get(ctx, customerID)
}

// Probe reads the provider document without registering anything: the
// console's Test button. It reports what Keycloak found so the admin can
// see the issuer or entity id before committing.
func Probe(ctx context.Context, broker Broker, protocol, metadataURL string) (map[string]string, error) {
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	if protocol != "oidc" && protocol != "saml" {
		return nil, fmt.Errorf("protocol must be oidc or saml")
	}
	if broker == nil {
		return nil, fmt.Errorf("the identity provider registry is not configured on this deployment")
	}
	cfg, err := broker.ImportIdentityProviderConfig(ctx, protocol, strings.TrimSpace(metadataURL))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, k := range []string{"issuer", "authorizationUrl", "tokenUrl", "jwksUrl", "idpEntityId", "singleSignOnServiceUrl", "nameIDPolicyFormat"} {
		if v := cfg[k]; v != "" {
			out[k] = v
		}
	}
	if cfg["signingCertificate"] != "" {
		out["signingCertificate"] = "present"
	}
	return out, nil
}

// DomainAllowed reports whether an address belongs to one of the domains.
func DomainAllowed(email string, domains []string) bool {
	at := strings.LastIndex(email, "@")
	if at < 0 {
		return false
	}
	dom := strings.ToLower(email[at+1:])
	for _, d := range domains {
		if d == dom {
			return true
		}
	}
	return false
}

// ErrNotProvisionable explains why a first login was refused; the gateway
// answers 403 with it so the person sees a reason instead of a bare 401.
type ErrNotProvisionable struct{ Reason string }

func (e *ErrNotProvisionable) Error() string { return e.Reason }

// ProvisionFirstLogin creates the account a brokered login arrives with, in
// the tenant that owns the provider: an identity.user row with the tenant's
// default role in its first workspace. It returns the user id. Idempotent:
// an account that already exists is returned as is.
func ProvisionFirstLogin(ctx context.Context, pool *pgxpool.Pool, customerID, sub, email, displayName string) (string, error) {
	settings, err := NewStore(pool).Get(ctx, customerID)
	if err != nil {
		return "", err
	}
	if !settings.Enabled || !settings.Configured {
		return "", &ErrNotProvisionable{"single sign-on is not enabled for this tenant"}
	}
	if !settings.JITProvisioning {
		return "", &ErrNotProvisionable{"your account has not been created yet — ask your administrator to add you"}
	}
	if !DomainAllowed(email, settings.AllowedDomains) {
		return "", &ErrNotProvisionable{fmt.Sprintf("%s is not an address this tenant's single sign-on may create accounts for", email)}
	}
	var existing string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM identity.user WHERE keycloak_sub = $1`, sub).Scan(&existing); err == nil {
		return existing, nil
	}
	if displayName == "" {
		displayName = email
	}
	var userID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id, last_login_at)
		VALUES ($1, $2, $3, $4::uuid, $5)
		ON CONFLICT (email) DO UPDATE SET keycloak_sub = EXCLUDED.keycloak_sub, last_login_at = EXCLUDED.last_login_at
		RETURNING id::text
	`, sub, strings.ToLower(email), displayName, customerID, time.Now()).Scan(&userID); err != nil {
		return "", fmt.Errorf("create account: %w", err)
	}
	var wsID string
	_ = pool.QueryRow(ctx, `SELECT id::text FROM core.workspace WHERE customer_id = $1::uuid ORDER BY created_at LIMIT 1`, customerID).Scan(&wsID)
	if _, err := pool.Exec(ctx, `
		INSERT INTO identity.role_assignment (user_id, role, workspace_id)
		VALUES ($1::uuid, $2::identity.user_role, NULLIF($3,'')::uuid)
		ON CONFLICT DO NOTHING
	`, userID, settings.DefaultRole, wsID); err != nil {
		return "", fmt.Errorf("assign default role: %w", err)
	}
	return userID, nil
}

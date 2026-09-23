// Package scim is the enterprise SCIM 2.0 provisioning endpoint: an
// identity provider (Entra ID, Okta, OneLogin) creates, updates, deactivates
// and deletes this tenant's users, and keeps its groups in step with the
// tenant's business roles. Licensed under ee/LICENSE; gated by
// license.FeatureSCIM.
//
// A user the endpoint creates is exactly the user the console's Users screen
// would create — the same identity-provider account, the same rows, the
// same directory entry — so nothing downstream can tell them apart.
package scim

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// tokenPrefix marks a SCIM bearer token. The tenant id follows it in the
// clear so a request can be routed to the right database before the secret
// is checked; the secret after that is the only part that authenticates.
const tokenPrefix = "mvx_scim_"

// NewToken mints a token for a tenant and returns it with its hash. The
// plaintext is shown once and never stored.
func NewToken(customerID string) (plain, hash string, err error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", "", err
	}
	plain = tokenPrefix + strings.ReplaceAll(strings.ToLower(customerID), "-", "") + "_" + base64.RawURLEncoding.EncodeToString(secret)
	return plain, HashToken(plain), nil
}

// HashToken is what the database stores and compares.
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// ParseToken recovers the tenant a token belongs to; ok is false for
// anything that is not shaped like a SCIM token.
func ParseToken(plain string) (customerID string, ok bool) {
	rest := strings.TrimPrefix(plain, tokenPrefix)
	if rest == plain {
		return "", false
	}
	parts := strings.SplitN(rest, "_", 2)
	if len(parts) != 2 || len(parts[0]) != 32 || len(parts[1]) < 40 {
		return "", false
	}
	h := parts[0]
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32], true
}

// Token is one row of identity.scim_token, never including the hash.
type Token struct {
	ID          string     `json:"id"`
	Name        string     `json:"name"`
	DefaultRole string     `json:"default_role"`
	WorkspaceID string     `json:"workspace_id"`
	CreatedAt   time.Time  `json:"created_at"`
	LastUsedAt  *time.Time `json:"last_used_at,omitempty"`
	RevokedAt   *time.Time `json:"revoked_at,omitempty"`
}

// TokenStore manages a tenant's tokens in its own database.
type TokenStore struct{ pool *pgxpool.Pool }

func NewTokenStore(pool *pgxpool.Pool) *TokenStore { return &TokenStore{pool: pool} }

// ErrBadToken is returned by Verify for an unknown or revoked token.
var ErrBadToken = errors.New("invalid or revoked SCIM token")

// Issue creates a token and returns it with the one-time plaintext.
func (s *TokenStore) Issue(ctx context.Context, customerID, name, defaultRole, workspaceID, createdBy string) (Token, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return Token{}, "", fmt.Errorf("name is required — say which identity provider this token is for")
	}
	if defaultRole == "" {
		defaultRole = "business_user"
	}
	switch defaultRole {
	case "business_user", "business_admin", "developer":
	default:
		return Token{}, "", fmt.Errorf("default_role must be business_user, business_admin or developer")
	}
	plain, hash, err := NewToken(customerID)
	if err != nil {
		return Token{}, "", err
	}
	var t Token
	err = s.pool.QueryRow(ctx, `
		INSERT INTO identity.scim_token (customer_id, name, token_hash, default_role, workspace_id, created_by)
		VALUES ($6::uuid, $1, $2, $3::identity.user_role, NULLIF($4,'')::uuid, NULLIF($5,'')::uuid)
		RETURNING id::text, name, default_role::text, COALESCE(workspace_id::text,''), created_at
	`, name, hash, defaultRole, workspaceID, createdBy, customerID).Scan(&t.ID, &t.Name, &t.DefaultRole, &t.WorkspaceID, &t.CreatedAt)
	if err != nil {
		return Token{}, "", fmt.Errorf("issue scim token: %w", err)
	}
	return t, plain, nil
}

// List returns every token, revoked ones included, newest first.
// List is the tenant's tokens (migration 091: rows carry their tenant).
func (s *TokenStore) List(ctx context.Context, customerID string) ([]Token, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, name, default_role::text, COALESCE(workspace_id::text,''), created_at, last_used_at, revoked_at
		FROM identity.scim_token WHERE customer_id = $1::uuid ORDER BY created_at DESC`, customerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Token{}
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.ID, &t.Name, &t.DefaultRole, &t.WorkspaceID, &t.CreatedAt, &t.LastUsedAt, &t.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Revoke disables a token; the row stays so the audit trail keeps its name.
// Revoke ends one of the tenant's tokens; another tenant's is not found.
func (s *TokenStore) Revoke(ctx context.Context, customerID, id string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE identity.scim_token SET revoked_at = now() WHERE id = $1::uuid AND customer_id = $2::uuid AND revoked_at IS NULL`, id, customerID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("token not found or already revoked")
	}
	return nil
}

// Verify authenticates a presented token and records its use.
func (s *TokenStore) Verify(ctx context.Context, plain string) (Token, error) {
	var t Token
	err := s.pool.QueryRow(ctx, `
		UPDATE identity.scim_token SET last_used_at = now()
		WHERE token_hash = $1 AND revoked_at IS NULL
		  -- The token names its tenant in its prefix; the row must agree.
		  AND (customer_id IS NULL OR customer_id = $2::uuid)
		RETURNING id::text, name, default_role::text, COALESCE(workspace_id::text,''), created_at, last_used_at
	`, HashToken(plain), tokenCustomer(plain)).Scan(&t.ID, &t.Name, &t.DefaultRole, &t.WorkspaceID, &t.CreatedAt, &t.LastUsedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, ErrBadToken
	}
	return t, err
}

// tokenCustomer is the tenant a token's prefix names, as a uuid string, or
// the nil uuid for a malformed token (which then matches no row).
func tokenCustomer(plain string) string {
	if cid, ok := ParseToken(plain); ok {
		return cid
	}
	return "00000000-0000-0000-0000-000000000000"
}

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A tenant credential is what a tenant hands the platform to reach one of
// its own external systems (core.tenant_credential, migration 092): today
// a Google service account for private Sheets. One row per (tenant, kind).
// The secret half is sealed with the same mandatory encryption as a
// connector's credential, with the tenant and the kind as associated data,
// so a row copied onto another tenant fails to open rather than opening
// where it was never granted.

// KindGoogleServiceAccount is a Google Cloud service account's key: the
// address the tenant shares its private sheets with, and the private key
// that signs the JWT bearer grant.
const KindGoogleServiceAccount = "google_service_account"

// ErrNoTenantCredential is returned by Get when the tenant has none of
// that kind.
var ErrNoTenantCredential = errors.New("no such tenant credential")

// TenantCredential is one row, public half only.
type TenantCredential struct {
	Kind      string            `json:"kind"`
	Meta      map[string]string `json:"meta"`
	UpdatedAt time.Time         `json:"updated_at"`
}

// TenantCredentialStore reads and writes one database's rows.
type TenantCredentialStore struct{ pool *pgxpool.Pool }

func NewTenantCredentialStore(pool *pgxpool.Pool) *TenantCredentialStore {
	return &TenantCredentialStore{pool: pool}
}

// Get returns the public half; ErrNoTenantCredential when there is none.
func (s *TenantCredentialStore) Get(ctx context.Context, customerID, kind string) (TenantCredential, error) {
	var out TenantCredential
	var meta []byte
	err := s.pool.QueryRow(ctx, `SELECT kind, meta, updated_at FROM core.tenant_credential WHERE customer_id = $1::uuid AND kind = $2`,
		customerID, kind).Scan(&out.Kind, &meta, &out.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TenantCredential{}, ErrNoTenantCredential
	}
	if err != nil {
		return TenantCredential{}, fmt.Errorf("read tenant credential: %w", err)
	}
	if err := json.Unmarshal(meta, &out.Meta); err != nil {
		return TenantCredential{}, fmt.Errorf("tenant credential meta: %w", err)
	}
	return out, nil
}

// Put seals secret for (customerID, kind) and stores it with its public
// meta, replacing what was there. Refused without INTEGRATION_CRED_KEY.
func (s *TenantCredentialStore) Put(ctx context.Context, customerID, kind string, meta map[string]string, secret []byte, createdBy string) error {
	sealed, err := EncryptCredential(secret, customerID, kind)
	if err != nil {
		return err
	}
	metaJSON, _ := json.Marshal(meta)
	_, err = s.pool.Exec(ctx, `
		INSERT INTO core.tenant_credential (customer_id, kind, meta, secret_enc, created_by)
		VALUES ($1::uuid, $2, $3::jsonb, $4, NULLIF($5, '')::uuid)
		ON CONFLICT (customer_id, kind) DO UPDATE SET
		    meta = EXCLUDED.meta, secret_enc = EXCLUDED.secret_enc, created_by = EXCLUDED.created_by, updated_at = now()`,
		customerID, kind, metaJSON, sealed, createdBy)
	if err != nil {
		return fmt.Errorf("store tenant credential: %w", err)
	}
	return nil
}

// Secret opens the sealed half. ErrNoTenantCredential when there is none.
func (s *TenantCredentialStore) Secret(ctx context.Context, customerID, kind string) ([]byte, error) {
	var sealed string
	err := s.pool.QueryRow(ctx, `SELECT secret_enc FROM core.tenant_credential WHERE customer_id = $1::uuid AND kind = $2`,
		customerID, kind).Scan(&sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNoTenantCredential
	}
	if err != nil {
		return nil, fmt.Errorf("read tenant credential: %w", err)
	}
	return DecryptCredential(sealed, customerID, kind)
}

// Delete forgets the credential.
func (s *TenantCredentialStore) Delete(ctx context.Context, customerID, kind string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM core.tenant_credential WHERE customer_id = $1::uuid AND kind = $2`, customerID, kind)
	return err
}

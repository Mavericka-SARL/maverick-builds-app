// Package aikeys is the enterprise tenant-level AI provider key.
//
// A community or commercial deployment gives every developer their own key in
// ai_assistant.llm_settings; that path is untouched and keeps working here.
// An enterprise tenant can instead hold ONE key, managed by its tenant admin:
// developers never paste a personal key, the tenant's AI spend sits on one
// account, and — when the key is enforced — no model data can leave through
// an employee's private provider account.
//
// Licensed under ee/LICENSE, not the Sustainable Use License that covers the
// rest of the repository. Gated at runtime by license.FeatureTenantAIKeys.
package aikeys

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// Settings is one scope's row of ai_assistant.tenant_llm_settings: a
// tenant's, or — with no tenant — the deployment's own, which a tenant
// inherits until it sets its own on an edition that includes deployment
// settings (migration 091).
//
// The key itself is never returned over the API: HasKey says whether one is
// stored, and an empty APIKey on update keeps the stored one, so a console
// that never receives the key can still save the form.
type Settings struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// APIKey is write-only: accepted on update, never populated on read.
	APIKey   string `json:"api_key,omitempty"`
	HasKey   bool   `json:"has_key"`
	Enforced bool   `json:"enforced"`
}

// Store reads and writes the tenant keys held in one database. Construct it
// with the pool for the tenant in the current request, exactly like every
// other per-tenant store: aikeys.NewStore(h.db.For(ctx)).
type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// DeploymentScope is the customer id of the deployment's own row.
const DeploymentScope = ""

// Defaults supplies the deployment's row from the control plane, or reports
// that this edition has none. Set once by cmd/gateway.
var Defaults func(ctx context.Context) (provider, model, apiKey string, enforced, ok bool)

// ErrNoTenantKey is returned by Resolve when the tenant has no key stored.
var ErrNoTenantKey = errors.New("no tenant AI key configured")

// Get returns one scope's settings without the key. found is false — and
// the defaults come back — when the scope has never saved a row.
func (s *Store) Get(ctx context.Context, customerID string) (Settings, bool, error) {
	var out Settings
	var enc string
	err := s.pool.QueryRow(ctx, `
		SELECT provider, model, api_key_enc, enforced
		FROM ai_assistant.tenant_llm_settings WHERE customer_id IS NOT DISTINCT FROM NULLIF($1, '')::uuid
	`, customerID).Scan(&out.Provider, &out.Model, &enc, &out.Enforced)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Settings{Provider: "openai"}, false, nil
		}
		return Settings{}, false, fmt.Errorf("read tenant ai settings: %w", err)
	}
	out.HasKey = enc != ""
	return out, true, nil
}

// Effective is what applies to a tenant: its own row, else the deployment's
// when the edition includes one, else the empty defaults. inherited says
// which.
func (s *Store) Effective(ctx context.Context, customerID string) (out Settings, inherited bool, err error) {
	out, found, err := s.Get(ctx, customerID)
	if err != nil || found || customerID == DeploymentScope {
		return out, false, err
	}
	if Defaults != nil {
		if provider, model, apiKey, enforced, ok := Defaults(ctx); ok {
			return Settings{Provider: provider, Model: model, HasKey: apiKey != "", Enforced: enforced}, true, nil
		}
	}
	return out, false, nil
}

// Update writes one scope's row, creating it on first save, and returns it
// as Get would. Enforcing without a key is refused: it would disable every
// developer's assistant at once.
func (s *Store) Update(ctx context.Context, customerID string, in Settings) (Settings, error) {
	provider := strings.TrimSpace(in.Provider)
	if provider == "" {
		provider = "openai"
	}
	key := strings.TrimSpace(in.APIKey)
	if in.Enforced && key == "" {
		cur, _, err := s.Get(ctx, customerID)
		if err != nil {
			return Settings{}, err
		}
		if !cur.HasKey {
			return Settings{}, errors.New("set a key before enforcing it, or every developer loses the assistant")
		}
	}
	var enc string
	if key != "" {
		var err error
		if enc, err = aiassistant.EncryptAPIKey(key); err != nil {
			return Settings{}, fmt.Errorf("encrypt tenant ai key: %w", err)
		}
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO ai_assistant.tenant_llm_settings (customer_id, provider, model, api_key_enc, enforced)
		VALUES (NULLIF($1, '')::uuid, $2, $3, $4, $5)
		ON CONFLICT (customer_id) DO UPDATE SET
		    provider    = EXCLUDED.provider,
		    model       = EXCLUDED.model,
		    api_key_enc = CASE WHEN EXCLUDED.api_key_enc = '' THEN ai_assistant.tenant_llm_settings.api_key_enc ELSE EXCLUDED.api_key_enc END,
		    enforced    = EXCLUDED.enforced,
		    updated_at  = now()
	`, customerID, provider, strings.TrimSpace(in.Model), enc, in.Enforced)
	if err != nil {
		return Settings{}, fmt.Errorf("update tenant ai settings: %w", err)
	}
	out, _, err := s.Get(ctx, customerID)
	return out, err
}

// Clear forgets one scope's key (and lifts enforcement) but keeps the row.
func (s *Store) Clear(ctx context.Context, customerID string) (Settings, error) {
	_, err := s.pool.Exec(ctx, `
		UPDATE ai_assistant.tenant_llm_settings
		SET api_key_enc = '', enforced = FALSE, updated_at = now()
		WHERE customer_id IS NOT DISTINCT FROM NULLIF($1, '')::uuid
	`, customerID)
	if err != nil {
		return Settings{}, fmt.Errorf("clear tenant ai key: %w", err)
	}
	out, _, err := s.Get(ctx, customerID)
	return out, err
}

// Resolve is what an AI call uses: the tenant's own key, else the
// deployment's when the edition includes one. ErrNoTenantKey when neither
// has a key.
func (s *Store) Resolve(ctx context.Context, customerID string) (provider, model, apiKey string, enforced bool, err error) {
	provider, model, apiKey, enforced, err = s.resolveOwn(ctx, customerID)
	if err == nil || !errors.Is(err, ErrNoTenantKey) || customerID == DeploymentScope || Defaults == nil {
		return provider, model, apiKey, enforced, err
	}
	// No row, or a row without a key, of its own: the deployment's applies.
	if dp, dm, dk, de, ok := Defaults(ctx); ok && dk != "" {
		return dp, dm, dk, de, nil
	}
	return provider, model, apiKey, enforced, err
}

func (s *Store) resolveOwn(ctx context.Context, customerID string) (provider, model, apiKey string, enforced bool, err error) {
	var enc string
	err = s.pool.QueryRow(ctx, `
		SELECT provider, model, api_key_enc, enforced
		FROM ai_assistant.tenant_llm_settings WHERE customer_id IS NOT DISTINCT FROM NULLIF($1, '')::uuid
	`, customerID).Scan(&provider, &model, &enc, &enforced)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", "", "", false, ErrNoTenantKey
		}
		return "", "", "", false, err
	}
	if enc == "" {
		return provider, model, "", enforced, ErrNoTenantKey
	}
	if apiKey, err = aiassistant.DecryptAPIKey(enc); err != nil {
		return "", "", "", enforced, fmt.Errorf("decrypt tenant ai key: %w", err)
	}
	return provider, model, apiKey, enforced, nil
}

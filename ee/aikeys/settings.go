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

// Settings is the one row of ai_assistant.tenant_llm_settings.
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

// Store reads and writes the tenant key for one tenant database. Construct it
// with the pool for the tenant in the current request, exactly like every
// other per-tenant store: aikeys.NewStore(h.db.For(ctx)).
type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

// ErrNoTenantKey is returned by Resolve when the tenant has no key stored.
var ErrNoTenantKey = errors.New("no tenant AI key configured")

// Get returns the tenant's settings without the key.
func (s *Store) Get(ctx context.Context) (Settings, error) {
	var out Settings
	var enc string
	err := s.pool.QueryRow(ctx, `
		SELECT provider, model, api_key_enc, enforced
		FROM ai_assistant.tenant_llm_settings WHERE id = TRUE
	`).Scan(&out.Provider, &out.Model, &enc, &out.Enforced)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// A tenant database migrated before this feature existed: report
			// the same shape the migration's default row would have.
			return Settings{Provider: "openai"}, nil
		}
		return Settings{}, fmt.Errorf("read tenant ai settings: %w", err)
	}
	out.HasKey = enc != ""
	return out, nil
}

// Update writes the row and returns it as Get would. Enforcing without a key
// is refused: it would disable every developer's assistant at once.
func (s *Store) Update(ctx context.Context, in Settings) (Settings, error) {
	provider := strings.TrimSpace(in.Provider)
	if provider == "" {
		provider = "openai"
	}
	key := strings.TrimSpace(in.APIKey)
	if in.Enforced && key == "" {
		cur, err := s.Get(ctx)
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
		UPDATE ai_assistant.tenant_llm_settings SET
		    provider    = $1,
		    model       = $2,
		    api_key_enc = CASE WHEN $3 = '' THEN api_key_enc ELSE $3 END,
		    enforced    = $4,
		    updated_at  = now()
		WHERE id = TRUE
	`, provider, strings.TrimSpace(in.Model), enc, in.Enforced)
	if err != nil {
		return Settings{}, fmt.Errorf("update tenant ai settings: %w", err)
	}
	return s.Get(ctx)
}

// Clear removes the stored key and stops enforcing it, returning the tenant
// to per-user keys without losing the provider choice.
func (s *Store) Clear(ctx context.Context) (Settings, error) {
	_, err := s.pool.Exec(ctx, `
		UPDATE ai_assistant.tenant_llm_settings
		SET api_key_enc = '', enforced = FALSE, updated_at = now()
		WHERE id = TRUE
	`)
	if err != nil {
		return Settings{}, fmt.Errorf("clear tenant ai key: %w", err)
	}
	return s.Get(ctx)
}

// Resolve returns the tenant's provider, model and decrypted key, and whether
// the key is enforced. It is the only path that decrypts, and it is called on
// the request path, so it never logs the key.
func (s *Store) Resolve(ctx context.Context) (provider, model, apiKey string, enforced bool, err error) {
	var enc string
	err = s.pool.QueryRow(ctx, `
		SELECT provider, model, api_key_enc, enforced
		FROM ai_assistant.tenant_llm_settings WHERE id = TRUE
	`).Scan(&provider, &model, &enc, &enforced)
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

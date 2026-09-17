package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/mavericks-engine/mavericks/ee/aikeys"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
	"github.com/mavericks-engine/mavericks/pkg/license"
)

// Tenant-level AI provider key — the enterprise alternative to every
// developer holding their own key.
//
// Community and commercial deployments keep per-user keys and never see these
// routes: they sit behind requireFeature(license.FeatureTenantAIKeys), which
// answers 403 naming the edition. The store lives in ee/aikeys.
//
// Tenant admins only. The key is this tenant's money and, when enforced, the
// only route model data takes out of the deployment.

// tenantAISettings handles GET and PUT on /api/admin/ai-settings.
func (h *handler) tenantAISettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	act, ok := h.requireRole(w, r, "platform_admin", "tenant_admin")
	if !ok {
		return
	}
	store := aikeys.NewStore(h.db.For(ctx))

	switch r.Method {
	case http.MethodGet:
		s, err := store.Get(ctx)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		jsonOK(w, s)
	case http.MethodPut:
		var body aikeys.Settings
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
		// The provider catalog lives with buildProvider, so validate against
		// it here rather than duplicating the list in ee/aikeys.
		if body.Provider != "" {
			if _, err := buildProvider(body.Provider, "probe"); err != nil {
				jsonErr(w, err, http.StatusBadRequest)
				return
			}
		}
		s, err := store.Update(ctx, body)
		if err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: auditlog.EventTenantAISettingsUpdated,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ResourceType: "tenant_ai_settings", ResourceID: "settings",
			Metadata: map[string]string{
				"provider":        s.Provider,
				"model":           s.Model,
				"api_key_changed": strconv.FormatBool(strings.TrimSpace(body.APIKey) != ""),
				"enforced":        boolWord(s.Enforced),
			},
		})
		jsonOK(w, s)
	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// tenantAIKeyClear handles DELETE /api/admin/ai-settings/key: drop the stored
// key and stop enforcing it, returning the tenant to per-user keys.
func (h *handler) tenantAIKeyClear(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	act, ok := h.requireRole(w, r, "platform_admin", "tenant_admin")
	if !ok {
		return
	}
	s, err := aikeys.NewStore(h.db.For(ctx)).Clear(ctx)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryAdmin, EventType: auditlog.EventTenantAIKeyCleared,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ResourceType: "tenant_ai_settings", ResourceID: "settings",
	})
	jsonOK(w, s)
}

// tenantAITestSettings handles POST /api/admin/ai-settings/test: the same
// tool-calling probe the per-user screen uses, against the tenant key. It
// matters more here — enforcing a key that cannot tool-call would break every
// developer in the tenant at once — so the console tests before enforcing.
func (h *handler) tenantAITestSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	act, ok := h.requireRole(w, r, "platform_admin", "tenant_admin")
	if !ok {
		return
	}
	var req struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		APIKey   string `json:"api_key"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	stored, _ := aikeys.NewStore(h.db.For(ctx)).Get(ctx)
	provider := firstNonEmpty(req.Provider, stored.Provider, "openai")
	model := firstNonEmpty(req.Model, stored.Model, providerDefaultModels[provider])
	apiKey := strings.TrimSpace(req.APIKey)
	if apiKey == "" {
		// Nothing typed in the form: probe whatever is already stored.
		_, _, apiKey, _, _ = aikeys.NewStore(h.db.For(ctx)).Resolve(ctx)
	}

	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryAdmin, EventType: auditlog.EventAISettingsTested,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ResourceType: "tenant_ai_settings", ResourceID: "settings",
		Metadata: map[string]string{"provider": provider, "model": model},
	})
	if apiKey == "" {
		jsonOK(w, map[string]any{"ok": false, "provider": provider, "model": model,
			"error": fmt.Sprintf("no tenant key stored for %s — paste one to test it", provider)})
		return
	}
	jsonOK(w, probeProviderKey(ctx, provider, model, apiKey))
}

// tenantAIKey resolves this tenant's key for the request path. Any failure —
// no key, a database that predates migration 079, an undecryptable value — is
// reported as "no tenant key" rather than an error, so the assistant falls
// back to the per-user key instead of breaking.
type tenantAIKeyResult struct {
	Provider string
	Model    string
	APIKey   string
	Enforced bool
	OK       bool
}

func (h *handler) tenantAIKey(ctx context.Context) tenantAIKeyResult {
	if h.lic == nil || !h.lic.Has(license.FeatureTenantAIKeys) {
		return tenantAIKeyResult{}
	}
	provider, model, key, enforced, err := aikeys.NewStore(h.db.For(ctx)).Resolve(ctx)
	if err != nil {
		return tenantAIKeyResult{Enforced: false}
	}
	return tenantAIKeyResult{Provider: provider, Model: model, APIKey: key, Enforced: enforced, OK: true}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

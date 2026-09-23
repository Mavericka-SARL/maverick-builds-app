package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/ee/aikeys"
	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/license"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// The tenant key is an enterprise feature, so a community deployment must not
// expose the routes at all — and must say which edition would.
func TestTenantAISettingsGatedByEdition(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	var adminID string
	if err := pool.QueryRow(ctx,
		`WITH c AS (INSERT INTO core.customer (name, plan) VALUES ('TAI Co', 'enterprise') RETURNING id)
		 INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		 SELECT 'tai-admin', 'admin@tai.dev', 'Admin', c.id FROM c RETURNING id::text`,
	).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'tenant_admin')`, adminID); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEV_MODE", "true")
	t.Setenv("AI_KEY_ENCRYPTION_SECRET", "tenant-ai-test-secret")

	call := func(t *testing.T, srv *httptest.Server, method, path, body string) (int, map[string]any) {
		t.Helper()
		var rdr *strings.Reader = strings.NewReader(body)
		req, _ := http.NewRequestWithContext(ctx, method, srv.URL+path, rdr)
		req.Header.Set("X-Dev-User", "tai-admin")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	t.Run("community refuses and names the edition", func(t *testing.T) {
		srv := httptest.NewServer(NewHandler(logger.New("test"), pool, nil))
		defer srv.Close()
		code, body := call(t, srv, http.MethodGet, "/api/admin/ai-settings", "")
		if code != http.StatusForbidden {
			t.Fatalf("GET code=%d, want 403", code)
		}
		msg, _ := body["error"].(string)
		for _, want := range []string{"Tenant AI keys", "enterprise edition", "community edition"} {
			if !strings.Contains(msg, want) {
				t.Errorf("error %q lacks %q", msg, want)
			}
		}
		if code, _ := call(t, srv, http.MethodPut, "/api/admin/ai-settings", `{"provider":"openai"}`); code != http.StatusForbidden {
			t.Fatalf("PUT code=%d, want 403", code)
		}
	})

	t.Run("enterprise stores a key without ever returning it", func(t *testing.T) {
		srv := httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, nil, Deps{License: enterpriseManager(t)}))
		defer srv.Close()

		code, body := call(t, srv, http.MethodGet, "/api/admin/ai-settings", "")
		if code != http.StatusOK || body["has_key"] != false || body["provider"] != "openai" {
			t.Fatalf("initial GET code=%d body=%+v", code, body)
		}

		// Enforcing with nothing stored would silently break every developer.
		code, body = call(t, srv, http.MethodPut, "/api/admin/ai-settings", `{"provider":"anthropic","enforced":true}`)
		if code != http.StatusBadRequest || !strings.Contains(body["error"].(string), "set a key before enforcing") {
			t.Fatalf("enforce-without-key code=%d body=%+v", code, body)
		}

		code, body = call(t, srv, http.MethodPut, "/api/admin/ai-settings",
			`{"provider":"anthropic","model":"claude-opus-4-8","api_key":"sk-tenant-secret","enforced":true}`)
		if code != http.StatusOK || body["has_key"] != true || body["enforced"] != true {
			t.Fatalf("PUT code=%d body=%+v", code, body)
		}
		if _, leaked := body["api_key"]; leaked {
			t.Fatalf("api_key returned to the client: %+v", body)
		}
		raw, _ := json.Marshal(body)
		if strings.Contains(string(raw), "sk-tenant-secret") {
			t.Fatalf("plaintext key present in the response: %s", raw)
		}

		// An unknown provider is refused by the catalog the chat path uses.
		if code, _ := call(t, srv, http.MethodPut, "/api/admin/ai-settings", `{"provider":"not-a-provider"}`); code != http.StatusBadRequest {
			t.Fatalf("unknown provider code=%d, want 400", code)
		}

		// An empty api_key on update keeps the stored one.
		code, body = call(t, srv, http.MethodPut, "/api/admin/ai-settings", `{"provider":"anthropic","enforced":false}`)
		if code != http.StatusOK || body["has_key"] != true || body["enforced"] != false {
			t.Fatalf("keep-key PUT code=%d body=%+v", code, body)
		}

		// The stored value is encrypted at rest, not the plaintext key.
		var stored string
		if err := pool.QueryRow(ctx, `SELECT api_key_enc FROM ai_assistant.tenant_llm_settings WHERE customer_id IS NOT NULL`).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored == "sk-tenant-secret" || stored == "" {
			t.Fatalf("api_key_enc = %q, want an encrypted value", stored)
		}

		code, body = call(t, srv, http.MethodDelete, "/api/admin/ai-settings/key", "")
		if code != http.StatusOK || body["has_key"] != false || body["enforced"] != false {
			t.Fatalf("DELETE code=%d body=%+v", code, body)
		}
	})
}

// buildProviderForRequest is where the money is spent, so the order it picks a
// key in is the behaviour worth pinning: enforced tenant key, else personal
// key, else an unenforced tenant key, else the environment.
func TestAIKeyResolutionOrder(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	t.Setenv("AI_KEY_ENCRYPTION_SECRET", "resolution-test-secret")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")

	var userID, custID string
	if err := pool.QueryRow(ctx,
		`WITH c AS (INSERT INTO core.customer (name, plan) VALUES ('Res Co', 'enterprise') RETURNING id)
		 INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		 SELECT 'res-dev', 'dev@res.dev', 'Dev', c.id FROM c RETURNING id::text, customer_id::text`,
	).Scan(&userID, &custID); err != nil {
		t.Fatal(err)
	}

	newHandler := func(lic *license.Manager) *handler {
		return &handler{log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), devMode: true, lic: lic}
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/ai/settings", nil)

	// Personal key: openai. Tenant key: anthropic. The provider name the
	// resolver returns therefore says which key it chose.
	chat := aiassistant.NewChatStore(pool)
	tenantStore := aikeys.NewStore(pool)

	t.Run("no key anywhere is an actionable error", func(t *testing.T) {
		_, _, _, err := newHandler(enterpriseManager(t)).buildProviderForRequest(req, &actor{UserID: userID, CustomerID: custID})
		if err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("personal key is used when it is the only one", func(t *testing.T) {
		if err := chat.SaveSettings(ctx, userID, "openai", "gpt-4o-mini", "sk-personal"); err != nil {
			t.Fatal(err)
		}
		_, provider, model, err := newHandler(enterpriseManager(t)).buildProviderForRequest(req, &actor{UserID: userID, CustomerID: custID})
		if err != nil || provider != "openai" || model != "gpt-4o-mini" {
			t.Fatalf("provider=%q model=%q err=%v", provider, model, err)
		}
	})

	t.Run("an unenforced tenant key does not displace a personal key", func(t *testing.T) {
		if _, err := tenantStore.Update(ctx, custID, aikeys.Settings{Provider: "anthropic", Model: "claude-opus-4-8", APIKey: "sk-tenant"}); err != nil {
			t.Fatal(err)
		}
		_, provider, _, err := newHandler(enterpriseManager(t)).buildProviderForRequest(req, &actor{UserID: userID, CustomerID: custID})
		if err != nil || provider != "openai" {
			t.Fatalf("provider=%q err=%v — the personal key should still win", provider, err)
		}
	})

	t.Run("an enforced tenant key overrides the personal key", func(t *testing.T) {
		if _, err := tenantStore.Update(ctx, custID, aikeys.Settings{Provider: "anthropic", Model: "claude-opus-4-8", Enforced: true}); err != nil {
			t.Fatal(err)
		}
		_, provider, model, err := newHandler(enterpriseManager(t)).buildProviderForRequest(req, &actor{UserID: userID, CustomerID: custID})
		if err != nil || provider != "anthropic" || model != "claude-opus-4-8" {
			t.Fatalf("provider=%q model=%q err=%v", provider, model, err)
		}
	})

	t.Run("without the licence the tenant key is invisible even when enforced", func(t *testing.T) {
		_, provider, _, err := newHandler(nil).buildProviderForRequest(req, &actor{UserID: userID, CustomerID: custID})
		if err != nil || provider != "openai" {
			t.Fatalf("community provider=%q err=%v — the tenant key must not apply", provider, err)
		}
	})

	t.Run("a tenant key fills in for a developer with no personal key", func(t *testing.T) {
		var otherID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('res-dev2', 'dev2@res.dev', 'Dev2', $1::uuid) RETURNING id::text`, custID,
		).Scan(&otherID); err != nil {
			t.Fatal(err)
		}
		if _, err := tenantStore.Update(ctx, custID, aikeys.Settings{Provider: "anthropic", Model: "claude-opus-4-8", Enforced: false}); err != nil {
			t.Fatal(err)
		}
		_, provider, _, err := newHandler(enterpriseManager(t)).buildProviderForRequest(req, &actor{UserID: otherID, CustomerID: custID})
		if err != nil || provider != "anthropic" {
			t.Fatalf("provider=%q err=%v", provider, err)
		}
	})
}

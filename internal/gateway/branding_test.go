package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/license"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// White-labelling: the tenant admin sets it (commercial or enterprise), the
// public endpoint serves it to the tenant's own people and to anonymous
// visitors of the tenant's host, validation refuses what the console could
// not render, and a host belongs to one tenant.
func TestBranding(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	tenant := func(name string) (custID, adminSub, userSub string) {
		custID = q(`INSERT INTO core.customer (name, plan) VALUES ($1, 'commercial') RETURNING id::text`, name)
		wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, custID)
		adminSub = "brand-admin-" + strings.ToLower(name)
		adminID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, $2, $3, $4::uuid) RETURNING id::text`, adminSub, adminSub+"@test", name+" Admin", custID)
		exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'tenant_admin', $2::uuid)`, adminID, wsID)
		userSub = "brand-user-" + strings.ToLower(name)
		userID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ($1, $2, $3) RETURNING id::text`, userSub, userSub+"@test", name+" User")
		exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid)`, userID, wsID)
		return
	}
	_, adminA, userA := tenant("Acme")
	_, adminB, userB := tenant("Beta")

	t.Setenv("DEV_MODE", "true")
	commercial := license.Static(&license.Claims{Edition: license.EditionCommercial, Customer: "Acme", ExpiresAt: time.Now().Add(time.Hour)})
	srv := httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, nil, Deps{License: commercial}))
	t.Cleanup(srv.Close)
	community := httptest.NewServer(NewHandler(logger.New("test"), pool, nil))
	t.Cleanup(community.Close)
	do := func(s *httptest.Server, persona, method, path string, body any, headers map[string]string) (int, map[string]any) {
		t.Helper()
		var buf []byte
		if body != nil {
			buf, _ = json.Marshal(body)
		}
		req, _ := http.NewRequestWithContext(ctx, method, s.URL+path, bytes.NewReader(buf))
		if persona != "" {
			req.Header.Set("X-Dev-User", persona)
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			if k == "Host" {
				req.Host = v // Go's client sends req.Host, never a Host header
				continue
			}
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		var m map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	logo := "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(`<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"/>`))

	// Gated on edition; the public endpoint always answers, with the default look.
	if code, body := do(community, adminA, http.MethodGet, "/api/admin/branding", nil, nil); code != http.StatusForbidden || !strings.Contains(body["error"].(string), "commercial edition") {
		t.Fatalf("community admin: %d %v", code, body)
	}
	if code, body := do(community, userA, http.MethodGet, "/api/branding", nil, nil); code != http.StatusOK || body["source"] != "default" {
		t.Fatalf("community public: %d %v", code, body)
	}

	// Validation refuses what the console could not render.
	for name, in := range map[string]map[string]any{
		"colour":      {"product_name": "Acme", "brand_color": "teal"},
		"logo type":   {"logo_data_url": "data:text/html;base64," + base64.StdEncoding.EncodeToString([]byte("<script>"))},
		"logo size":   {"logo_data_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 300*1024))},
		"domain":      {"custom_domain": "not a host"},
		"name length": {"product_name": strings.Repeat("x", 61)},
	} {
		if code, body := do(srv, adminA, http.MethodPut, "/api/admin/branding", in, nil); code != http.StatusBadRequest {
			t.Errorf("%s accepted: %d %v", name, code, body)
		}
	}

	// Acme brands itself; its host is registered.
	code, saved := do(srv, adminA, http.MethodPut, "/api/admin/branding", map[string]any{
		"product_name": "Acme Planning", "tagline": "Numbers you can sign.", "brand_color": "#0F766E", "logo_data_url": logo,
		"email_from_name": "Acme Planning", "custom_domain": "https://Planning.Acme.test/login",
	}, nil)
	if code != http.StatusOK || saved["configured"] != true || saved["brand_color"] != "#0f766e" || saved["custom_domain"] != "planning.acme.test" {
		t.Fatalf("save: %d %v", code, saved)
	}
	// Beta cannot take Acme's host.
	if code, body := do(srv, adminB, http.MethodPut, "/api/admin/branding", map[string]any{"product_name": "Beta", "custom_domain": "planning.acme.test"}, nil); code != http.StatusBadRequest || !strings.Contains(body["error"].(string), "another tenant") {
		t.Errorf("host uniqueness: %d %v", code, body)
	}

	// Who sees it: Acme's people (by tenant), a visitor of Acme's host (by
	// host), nobody else.
	if code, body := do(srv, userA, http.MethodGet, "/api/branding", nil, nil); code != http.StatusOK || body["source"] != "tenant" || body["product_name"] != "Acme Planning" || body["logo_data_url"] != logo {
		t.Errorf("Acme user: %d %v", code, body)
	}
	if code, body := do(srv, userB, http.MethodGet, "/api/branding", nil, nil); code != http.StatusOK || body["source"] != "default" {
		t.Errorf("Beta user must not see Acme's brand: %d %v", code, body)
	}
	if code, body := do(srv, "", http.MethodGet, "/api/branding", nil, map[string]string{"Host": "planning.acme.test:443"}); code != http.StatusOK || body["source"] != "host" || body["product_name"] != "Acme Planning" {
		t.Errorf("visitor of Acme's host: %d %v", code, body)
	}
	if code, body := do(srv, "", http.MethodGet, "/api/branding", nil, map[string]string{"X-Forwarded-Host": "planning.acme.test"}); code != http.StatusOK || body["source"] != "host" {
		t.Errorf("visitor behind the ingress: %d %v", code, body)
	}
	if code, body := do(srv, "", http.MethodGet, "/api/branding", nil, map[string]string{"Host": "other.example.test"}); code != http.StatusOK || body["source"] != "default" {
		t.Errorf("unknown host: %d %v", code, body)
	}
	// The public view never carries the e-mail or domain settings.
	if _, body := do(srv, userA, http.MethodGet, "/api/branding", nil, nil); body["email_from_name"] != nil || body["custom_domain"] != nil {
		t.Errorf("public view leaks admin settings: %v", body)
	}

	// Removing returns the default look and frees the host.
	if code, body := do(srv, adminA, http.MethodDelete, "/api/admin/branding", nil, nil); code != http.StatusOK || body["configured"] != false {
		t.Fatalf("remove: %d %v", code, body)
	}
	if _, body := do(srv, userA, http.MethodGet, "/api/branding", nil, nil); body["source"] != "default" {
		t.Errorf("after removal: %v", body)
	}
	if code, _ := do(srv, adminB, http.MethodPut, "/api/admin/branding", map[string]any{"product_name": "Beta", "custom_domain": "planning.acme.test"}, nil); code != http.StatusOK {
		t.Errorf("host not freed after removal: %d", code)
	}
}

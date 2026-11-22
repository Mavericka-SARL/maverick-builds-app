package gateway

import (
	"bytes"
	"context"
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

// enterpriseRoutePrefixes are the HTTP surfaces of the enterprise features. Every route
// registered under one is driven below; an enterprise feature with a new surface
// adds its prefix here.
var enterpriseRoutePrefixes = []string{
	"/api/cells/history",
	"/api/admin/audit/export",
	"/api/admin/audit/settings",
	"/api/admin/usage",
	"/api/admin/branding",
	"/api/admin/ai-settings",
	"/api/admin/sso",
	"/api/admin/scim",
	"/api/scim/v2",
}

// TestPaidRoutesFollowTheLicence drives every paid route through the real
// router as a platform administrator under three licences: community
// refuses all of them with the licence message; so does an enterprise key
// the day after it expired — there is no transition period (owner,
// 2026-11-22); an active enterprise key refuses none of them on licence
// grounds. Frontend gates are not enforcement — this is.
func TestPaidRoutesFollowTheLicence(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	var adminID string
	if err := pool.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('paid-routes-admin', 'paid-routes-admin@test', 'Admin') RETURNING id::text`).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'platform_admin')`, adminID); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEV_MODE", "true")

	var routes []RouteInfo
	seen := map[string]bool{}
	for _, r := range BuildRoutes() {
		for _, p := range enterpriseRoutePrefixes {
			if r.Pattern == p || strings.HasPrefix(r.Pattern, p+"/") {
				routes = append(routes, r)
				seen[p] = true
			}
		}
	}
	if len(routes) < 30 {
		t.Fatalf("only %d paid routes found; the prefixes no longer match the router", len(routes))
	}
	for _, p := range enterpriseRoutePrefixes {
		if !seen[p] {
			t.Errorf("no route registered under paid prefix %s", p)
		}
	}

	claims := func(expires time.Time) *license.Claims {
		return &license.Claims{ID: "lic-routes", Edition: license.EditionEnterprise, Customer: "Acme Corp",
			IssuedAt: expires.Add(-365 * 24 * time.Hour), ExpiresAt: expires}
	}
	servers := map[string]*httptest.Server{
		"community":  httptest.NewServer(NewHandler(logger.New("test"), pool, nil)),
		"expired":    httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, nil, Deps{License: license.Static(claims(time.Now().Add(-24 * time.Hour)))})),
		"enterprise": httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, nil, Deps{License: license.Static(claims(time.Now().Add(365 * 24 * time.Hour)))})),
	}
	for _, s := range servers {
		t.Cleanup(s.Close)
	}
	call := func(s *httptest.Server, method, pattern string) (int, string) {
		t.Helper()
		path := strings.NewReplacer("{id}", "00000000-0000-0000-0000-000000000001", "{modelId}", "00000000-0000-0000-0000-000000000002").Replace(pattern)
		var body []byte
		if method != http.MethodGet && method != http.MethodHead && method != http.MethodDelete {
			body = []byte(`{}`)
		}
		req, _ := http.NewRequestWithContext(ctx, method, s.URL+path, bytes.NewReader(body))
		req.Header.Set("X-Dev-User", "paid-routes-admin")
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		msg, _ := out["error"].(string)
		if d, ok := out["detail"].(string); ok { // SCIM-shaped errors
			msg = d
		}
		return resp.StatusCode, msg
	}
	licenceRefusal := func(code int, msg string) bool {
		return code == http.StatusForbidden && strings.Contains(msg, "edition")
	}

	for _, r := range routes {
		name := r.Method + " " + r.Pattern
		if code, msg := call(servers["community"], r.Method, r.Pattern); code != http.StatusForbidden || !strings.Contains(msg, "edition") {
			t.Errorf("community %s = %d %q, want the licence refusal", name, code, msg)
		}
		if code, msg := call(servers["expired"], r.Method, r.Pattern); !licenceRefusal(code, msg) {
			t.Errorf("expired %s = %d %q, want the licence refusal, as for community", name, code, msg)
		}
		if code, msg := call(servers["enterprise"], r.Method, r.Pattern); licenceRefusal(code, msg) {
			t.Errorf("enterprise %s = %d %q, want no licence refusal", name, code, msg)
		}
	}
}

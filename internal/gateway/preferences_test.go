package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/plan"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// Per-person preferences (the console theme): every role sets its own, on
// its own row only, through PATCH /api/me/preferences, and reads them back
// in /api/me. Only registered keys and values are stored.
func TestMyPreferences(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	t.Setenv("DEV_MODE", "true")
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	// One healthy tenant, one that is read-only because it is over its plan.
	cust := q(`INSERT INTO core.customer (name, plan) VALUES ('Pref Co', 'commercial') RETURNING id::text`)
	ws := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Default') RETURNING id::text`, cust)
	over := q(`INSERT INTO core.customer (name, plan, limit_state, limit_reason) VALUES ('Full Co', 'community', 'over', 'storage 120 MB of 100') RETURNING id::text`)

	users := map[string]string{} // sub -> id
	addUser := func(sub, customer string, roles ...string) {
		var id string
		if customer == "" {
			id = q(`INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ($1, $1 || '@pref.test', $1) RETURNING id::text`, sub)
		} else {
			id = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, $1 || '@pref.test', $1, $2::uuid) RETURNING id::text`, sub, customer)
		}
		for _, r := range roles {
			if r == "business_user" || r == "business_admin" {
				exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, $2, $3::uuid)`, id, r, ws)
			} else {
				exec(`INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, $2)`, id, r)
			}
		}
		users[sub] = id
	}
	addUser("pref-dev", cust, "developer")
	addUser("pref-other", cust, "developer")
	addUser("pref-business", cust, "business_user")
	addUser("pref-badmin", cust, "business_admin")
	addUser("pref-tadmin", cust, "tenant_admin")
	addUser("pref-padmin", "", "platform_admin")
	addUser("pref-over", over, "tenant_admin", "developer")

	srv := httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, nil, Deps{Plans: plan.NewEnforcer(pool)}))
	t.Cleanup(srv.Close)

	patch := func(sub string, body any) (int, map[string]any) {
		t.Helper()
		return callJSON(t, srv, sub, http.MethodPatch, "/api/me/preferences", body)
	}
	themeOf := func(sub string) any {
		t.Helper()
		code, me := callJSON(t, srv, sub, http.MethodGet, "/api/me", nil)
		if code != 200 {
			t.Fatalf("/api/me as %s: %d %v", sub, code, me)
		}
		prefs, ok := me["preferences"].(map[string]any)
		if !ok {
			t.Fatalf("/api/me as %s has no preferences object: %v", sub, me)
		}
		return prefs["theme"]
	}

	t.Run("none until chosen, then stored on the caller's own account only", func(t *testing.T) {
		if got := themeOf("pref-dev"); got != nil {
			t.Fatalf("fresh account theme = %v, want none", got)
		}
		code, body := patch("pref-dev", map[string]any{"theme": "dark"})
		if code != 200 || body["preferences"].(map[string]any)["theme"] != "dark" {
			t.Fatalf("patch: %d %v", code, body)
		}
		if got := themeOf("pref-dev"); got != "dark" {
			t.Fatalf("after patch theme = %v, want dark", got)
		}
		if got := themeOf("pref-other"); got != nil {
			t.Fatalf("another account picked up the change: %v", got)
		}
	})

	t.Run("every role reaches its own preferences", func(t *testing.T) {
		for _, sub := range []string{"pref-business", "pref-badmin", "pref-tadmin", "pref-padmin"} {
			if code, body := patch(sub, map[string]any{"theme": "system"}); code != 200 {
				t.Fatalf("%s: %d %v", sub, code, body)
			}
			if got := themeOf(sub); got != "system" {
				t.Fatalf("%s: theme = %v, want system", sub, got)
			}
		}
	})

	t.Run("unknown keys, bad values and non-objects are refused and change nothing", func(t *testing.T) {
		cases := []struct {
			body any
			want string
		}{
			{map[string]any{"theme": "blue"}, "theme"},
			{map[string]any{"theme": 1}, "theme"},
			{map[string]any{"font": "large"}, "unknown preference"},
			{map[string]any{"theme": "light", "font": "large"}, "unknown preference"},
			{[]any{"theme"}, "JSON object"},
			{"dark", "JSON object"},
		}
		for _, c := range cases {
			code, body := patch("pref-dev", c.body)
			if code != http.StatusBadRequest || !strings.Contains(body["error"].(string), c.want) {
				t.Fatalf("%v: %d %v", c.body, code, body)
			}
		}
		if got := themeOf("pref-dev"); got != "dark" {
			t.Fatalf("a refused request changed the theme to %v", got)
		}
	})

	t.Run("null resets to the default; an empty object changes nothing", func(t *testing.T) {
		if code, body := patch("pref-dev", map[string]any{}); code != 200 || body["preferences"].(map[string]any)["theme"] != "dark" {
			t.Fatalf("empty patch: %d %v", code, body)
		}
		code, body := patch("pref-dev", map[string]any{"theme": nil})
		if code != 200 {
			t.Fatalf("reset: %d %v", code, body)
		}
		if _, has := body["preferences"].(map[string]any)["theme"]; has {
			t.Fatalf("reset left the theme: %v", body)
		}
		if got := themeOf("pref-dev"); got != nil {
			t.Fatalf("after reset theme = %v, want none", got)
		}
	})

	t.Run("a stored key or value the registry does not know is not served", func(t *testing.T) {
		exec(`UPDATE identity.user SET preferences = '{"theme":"light","retired":true}' WHERE id = $1::uuid`, users["pref-other"])
		code, me := callJSON(t, srv, "pref-other", http.MethodGet, "/api/me", nil)
		prefs := me["preferences"].(map[string]any)
		if code != 200 || prefs["theme"] != "light" || prefs["retired"] != nil {
			t.Fatalf("filtered read: %d %v", code, prefs)
		}
		exec(`UPDATE identity.user SET preferences = '{"theme":"blue"}' WHERE id = $1::uuid`, users["pref-other"])
		if got := themeOf("pref-other"); got != nil {
			t.Fatalf("invalid stored theme served: %v", got)
		}
	})

	t.Run("a read-only tenant can still change its own display preferences", func(t *testing.T) {
		// The guard still refuses the tenant's real writes...
		if code, _ := callJSON(t, srv, "pref-over", http.MethodPost, "/api/admin/applications", map[string]any{"customer_id": over, "name": "Nope", "mode": "planning"}); code != http.StatusPaymentRequired {
			t.Fatalf("tenant write while over: %d, want 402", code)
		}
		// ...but a colour scheme is not tenant data.
		if code, body := patch("pref-over", map[string]any{"theme": "dark"}); code != 200 {
			t.Fatalf("preferences while over: %d %v", code, body)
		}
		if got := themeOf("pref-over"); got != "dark" {
			t.Fatalf("theme = %v, want dark", got)
		}
	})

	t.Run("no signed-in person, no preferences", func(t *testing.T) {
		if code, _ := patch("no-such-person", map[string]any{"theme": "dark"}); code != http.StatusUnauthorized {
			t.Fatalf("anonymous patch: %d, want 401", code)
		}
	})
}

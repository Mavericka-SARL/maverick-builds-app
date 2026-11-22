package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/plan"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/license"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// A self-hosted deployment without a licence key runs the Community Edition,
// and so do its tenants: the console is offered Community only, a tenant
// created without naming an edition starts on it, and Commercial or
// Enterprise is refused — at creation and as a change (owner, 2026-11-21).
// A key adds its edition and new tenants start on it; the Playground keeps
// its whole catalog and starts new tenants on Community.
func TestEditionsADeploymentOffers(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	t.Setenv("DEV_MODE", "true")
	var padmin string
	if err := pool.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('ed-padmin', 'p@editions.test', 'Platform') RETURNING id::text`).Scan(&padmin); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'platform_admin')`, padmin); err != nil {
		t.Fatal(err)
	}
	server := func(deps Deps) *httptest.Server {
		deps.Plans = plan.NewEnforcer(pool)
		srv := httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, nil, deps))
		t.Cleanup(srv.Close)
		return srv
	}
	// offered is the list's keys, the one marked default starred.
	offered := func(srv *httptest.Server) string {
		t.Helper()
		code, plans := callJSONList(t, srv, "ed-padmin", "/api/admin/plans")
		if code != 200 {
			t.Fatalf("plans: %d", code)
		}
		keys := make([]string, 0, len(plans))
		for _, p := range plans {
			k := p["key"].(string)
			if p["default"] == true {
				k += "*"
			}
			keys = append(keys, k)
		}
		return strings.Join(keys, ",")
	}
	create := func(srv *httptest.Server, name, edition string) (int, map[string]any) {
		t.Helper()
		body := map[string]any{"name": name, "getting_started": false}
		if edition != "" {
			body["plan"] = edition
		}
		return callJSON(t, srv, "ed-padmin", http.MethodPost, "/api/admin/tenants", body)
	}
	editionOf := func(id string) string {
		t.Helper()
		var p string
		if err := pool.QueryRow(ctx, `SELECT plan FROM core.customer WHERE id = $1::uuid`, id).Scan(&p); err != nil {
			t.Fatal(err)
		}
		return p
	}

	t.Run("no licence key: Community only", func(t *testing.T) {
		srv := server(Deps{})
		if got := offered(srv); got != "community*" {
			t.Fatalf("offered %q, want community*", got)
		}
		for _, ed := range []string{"commercial", "enterprise"} {
			if code, body := create(srv, "Refused "+ed, ed); code != 400 || !strings.Contains(body["error"].(string), "Community Edition") {
				t.Fatalf("create on %s: %d %v", ed, code, body)
			}
		}
		code, body := create(srv, "Self Co", "")
		id, _ := body["id"].(string)
		if code != 200 || id == "" {
			t.Fatalf("create: %d %v", code, body)
		}
		if got := editionOf(id); got != "community" {
			t.Fatalf("a tenant created without naming an edition is on %q, want community", got)
		}
		// Without a key there is no edition change at all (owner, 2026-11-22).
		if code, body := callJSON(t, srv, "ed-padmin", http.MethodPatch, "/api/admin/tenants/"+id, map[string]any{"plan": "enterprise"}); code != 400 || !strings.Contains(body["error"].(string), "needs a license key") {
			t.Fatalf("change to enterprise: %d %v", code, body)
		}
		if got := editionOf(id); got != "community" {
			t.Fatalf("after a refused change the tenant is on %q", got)
		}
	})

	t.Run("a Commercial key adds Commercial, and new tenants start on it", func(t *testing.T) {
		srv := server(Deps{License: license.Static(&license.Claims{Edition: license.EditionCommercial,
			IssuedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(24 * time.Hour)})})
		if got := offered(srv); got != "community,commercial*" {
			t.Fatalf("offered %q", got)
		}
		if code, body := create(srv, "Too High", "enterprise"); code != 400 {
			t.Fatalf("create on enterprise: %d %v", code, body)
		}
		code, body := create(srv, "Licensed Co", "")
		if id, _ := body["id"].(string); code != 200 || editionOf(id) != "commercial" {
			t.Fatalf("create: %d %v", code, body)
		}
	})

	t.Run("an Enterprise key offers every edition, and new tenants start on Enterprise", func(t *testing.T) {
		srv := server(Deps{License: license.Static(&license.Claims{Edition: license.EditionEnterprise,
			IssuedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(24 * time.Hour)})})
		if got := offered(srv); got != "community,commercial,enterprise*" {
			t.Fatalf("offered %q", got)
		}
		code, body := create(srv, "Enterprise Co", "")
		if id, _ := body["id"].(string); code != 200 || editionOf(id) != "enterprise" {
			t.Fatalf("create: %d %v", code, body)
		}
	})

	t.Run("the Playground offers its whole catalog and starts new tenants on Community", func(t *testing.T) {
		srv := server(Deps{Signup: SignupConfig{Playground: true}})
		if got := offered(srv); got != "community*,commercial,enterprise" {
			t.Fatalf("offered %q", got)
		}
		code, body := create(srv, "Hosted Co", "")
		if id, _ := body["id"].(string); code != 200 || editionOf(id) != "community" {
			t.Fatalf("create: %d %v", code, body)
		}
		if code, body := create(srv, "Hosted Big", "enterprise"); code != 200 {
			t.Fatalf("create on enterprise: %d %v", code, body)
		}
	})
}

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// Dedicated tenant databases are a security boundary, so the test that
// matters goes through the real HTTP surface: two tenants created by the
// admin API, each getting its own database, and a developer of one who
// cannot reach the other even when they ask for it by id.
func setupIsolation(t *testing.T) (*httptest.Server, *tenantdb.Router) {
	t.Helper()
	ctx := context.Background()
	control := testdb.New(t, migrationfs.FS, ".")

	var adminID string
	if err := control.QueryRow(ctx,
		`INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('iso-pa', 'pa@iso.dev', 'PA') RETURNING id::text`,
	).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	if _, err := control.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'platform_admin')`, adminID); err != nil {
		t.Fatal(err)
	}
	router := tenantdb.New(control, tenantdb.Config{
		Migrations: migrationfs.FS, MigrationsDir: ".",
		AdminURL: testdb.AdminDSN(t), IdleTimeout: -1, Log: logger.New("test"),
	})
	t.Cleanup(router.Close)
	t.Setenv("DEV_MODE", "true")
	srv := httptest.NewServer(NewHandlerWithDeps(logger.New("test"), control, nil, Deps{Router: router}))
	t.Cleanup(srv.Close)
	return srv, router
}

type call struct {
	persona string
	tenant  string // X-Tenant-Id
	app     string // X-App-Id
}

func do(t *testing.T, srv *httptest.Server, c call, method, path string, body any) (int, []byte) {
	t.Helper()
	var buf []byte
	if body != nil {
		buf, _ = json.Marshal(body)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, srv.URL+path, bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Dev-User", c.persona)
	if c.tenant != "" {
		req.Header.Set("X-Tenant-Id", c.tenant)
	}
	if c.app != "" {
		req.Header.Set("X-App-Id", c.app)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func TestTenantsGetTheirOwnDatabase(t *testing.T) {
	ctx := context.Background()
	srv, router := setupIsolation(t)
	pa := call{persona: "iso-pa"}

	// Two tenants, created the way the console creates them.
	newTenant := func(name string) string {
		code, body := do(t, srv, pa, http.MethodPost, "/api/admin/tenants", map[string]string{"name": name, "plan": "enterprise"})
		if code != http.StatusOK {
			t.Fatalf("create %s: %d %s", name, code, body)
		}
		var out struct {
			ID          string `json:"id"`
			WorkspaceID string `json:"workspace_id"`
		}
		_ = json.Unmarshal(body, &out)
		if out.ID == "" || out.WorkspaceID == "" {
			t.Fatalf("create %s returned %s", name, body)
		}
		return out.ID
	}
	acme, globex := newTenant("Acme Corp"), newTenant("Globex")

	for _, id := range []string{acme, globex} {
		tn, err := router.Catalog().Get(ctx, id)
		if err != nil || tn.Status != tenantdb.StatusReady || tn.Database != tenantdb.DatabaseName(id) {
			t.Fatalf("tenant %s: %+v %v", id, tn, err)
		}
	}

	// An application in each. The admin is not a member of either, so the
	// tenant comes from the request body.
	newApp := func(customerID, name string) string {
		code, body := do(t, srv, pa, http.MethodPost, "/api/admin/applications",
			map[string]string{"customer_id": customerID, "name": name, "mode": "planning"})
		if code != http.StatusOK {
			t.Fatalf("create app %s: %d %s", name, code, body)
		}
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(body, &out)
		return out.ID
	}
	acmeApp, globexApp := newApp(acme, "Acme Planning"), newApp(globex, "Globex Planning")

	// Each application exists in exactly one database, and none in the
	// control plane.
	acmePool, err := router.Pool(ctx, acme)
	if err != nil {
		t.Fatal(err)
	}
	globexPool, err := router.Pool(ctx, globex)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := acmePool.QueryRow(ctx, `SELECT count(*) FROM core.application WHERE id=$1::uuid`, acmeApp).Scan(&n); err != nil || n != 1 {
		t.Fatalf("Acme's application is not in Acme's database (%d, %v)", n, err)
	}
	if err := globexPool.QueryRow(ctx, `SELECT count(*) FROM core.application WHERE id=$1::uuid`, acmeApp).Scan(&n); err != nil || n != 0 {
		t.Fatalf("Acme's application leaked into Globex's database (%d, %v)", n, err)
	}
	if err := router.Control().QueryRow(ctx, `SELECT count(*) FROM core.application`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("applications leaked into the control plane (%d, %v)", n, err)
	}

	// The platform admin still sees every tenant, with the applications read
	// from each tenant's own database.
	code, body := do(t, srv, pa, http.MethodGet, "/api/admin/tenants", nil)
	if code != http.StatusOK {
		t.Fatalf("list tenants: %d %s", code, body)
	}
	var listed []struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Applications []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"applications"`
	}
	if err := json.Unmarshal(body, &listed); err != nil {
		t.Fatal(err)
	}
	seen := map[string][]string{}
	for _, tn := range listed {
		for _, a := range tn.Applications {
			seen[tn.ID] = append(seen[tn.ID], a.Name)
		}
	}
	if len(seen[acme]) != 1 || seen[acme][0] != "Acme Planning" || len(seen[globex]) != 1 || seen[globex][0] != "Globex Planning" {
		t.Fatalf("admin listing did not read each tenant's own database: %v", seen)
	}

	// A developer who belongs to Acme.
	var devID string
	if err := acmePool.QueryRow(ctx,
		`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('iso-dev', 'dev@acme.iso', 'Dev', $1::uuid) RETURNING id::text`, acme,
	).Scan(&devID); err != nil {
		t.Fatal(err)
	}
	if _, err := acmePool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'developer')`, devID); err != nil {
		t.Fatal(err)
	}
	if err := router.Catalog().AddUser(ctx, "iso-dev", acme, "dev@acme.iso"); err != nil {
		t.Fatal(err)
	}

	dev := call{persona: "iso-dev"}
	code, body = do(t, srv, dev, http.MethodGet, "/api/developer/applications", nil)
	if code != http.StatusOK {
		t.Fatalf("developer listing: %d %s", code, body)
	}
	if !bytes.Contains(body, []byte("Acme Planning")) || bytes.Contains(body, []byte("Globex Planning")) {
		t.Fatalf("a developer of Acme saw the wrong applications: %s", body)
	}
	// A member is routed to their tenant's database, where the customer row
	// lives, AND appears in the catalog: listing both sources once showed the
	// same tenant twice.
	var devTenants []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &devTenants); err != nil {
		t.Fatal(err)
	}
	if len(devTenants) != 1 || devTenants[0].ID != acme {
		t.Fatalf("a developer of Acme should see exactly one tenant, got %v", devTenants)
	}

	// Asking for another tenant by id must not reach it: the header is
	// honoured only for members (and for tenant-less platform admins).
	code, body = do(t, srv, call{persona: "iso-dev", tenant: globex}, http.MethodGet, "/api/developer/applications", nil)
	if code == http.StatusOK && bytes.Contains(body, []byte("Globex Planning")) {
		t.Fatalf("X-Tenant-Id let a developer of Acme read Globex: %s", body)
	}
	// Same for an application id that belongs to the other tenant.
	code, body = do(t, srv, call{persona: "iso-dev", app: globexApp}, http.MethodGet, "/api/developer/applications", nil)
	if code == http.StatusOK && bytes.Contains(body, []byte("Globex Planning")) {
		t.Fatalf("X-App-Id let a developer of Acme read Globex: %s", body)
	}

	// Deleting a tenant drops its database.
	if code, body := do(t, srv, pa, http.MethodDelete, "/api/admin/tenants/"+globex, nil); code != http.StatusOK {
		t.Fatalf("delete tenant: %d %s", code, body)
	}
	var exists bool
	if err := router.Control().QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname=$1)`, tenantdb.DatabaseName(globex)).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("deleting a tenant left its database behind")
	}
}

// Shared mode must behave exactly as it did before dedicated databases
// existed: no router, no directory, everything in one database.
func TestSharedModeIsUnchanged(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	var adminID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('shared-pa', 'pa@shared.dev', 'PA') RETURNING id::text`,
	).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'platform_admin')`, adminID); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEV_MODE", "true")
	srv := httptest.NewServer(NewHandler(logger.New("test"), pool, nil))
	defer srv.Close()

	code, body := do(t, srv, call{persona: "shared-pa"}, http.MethodPost, "/api/admin/tenants", map[string]string{"name": "Shared Co", "plan": "standard"})
	if code != http.StatusOK {
		t.Fatalf("create tenant: %d %s", code, body)
	}
	var out struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &out)
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM core.customer WHERE id=$1::uuid`, out.ID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("shared mode should write the customer into the one database (%d, %v)", n, err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM platform.tenant_database`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("shared mode must not touch the tenant catalog (%d, %v)", n, err)
	}
}

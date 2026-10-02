package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/plan"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// dedicatedFixture is a gateway with a database per tenant, a fake identity
// provider, and a platform admin in the control plane: subject held-pa,
// pa@held.test.
type dedicatedFixture struct {
	ctx     context.Context
	control *pgxpool.Pool
	router  *tenantdb.Router
	broker  *fakeBroker
	srv     *httptest.Server
	// reader reads and writes the databases directly.
	reader *handler
	// plans is the gateway's plan enforcer, shared with any sweep a test
	// runs, as cmd/gateway shares it with the usage sweep.
	plans *plan.Enforcer
	pa    call
	paID  string
}

func newDedicatedFixture(t *testing.T, deps Deps) *dedicatedFixture {
	t.Helper()
	f := &dedicatedFixture{ctx: context.Background(), pa: call{persona: "held-pa"}}
	f.control = testdb.New(t, migrationfs.FS, ".")
	if err := f.control.QueryRow(f.ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name)
		VALUES ('held-pa', 'pa@held.test', 'Pat Admin') RETURNING id::text`).Scan(&f.paID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.control.Exec(f.ctx, `INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'platform_admin')`, f.paID); err != nil {
		t.Fatal(err)
	}
	f.router = tenantdb.New(f.control, tenantdb.Config{
		Migrations: migrationfs.FS, MigrationsDir: ".",
		AdminURL: testdb.AdminDSN(t), IdleTimeout: -1, Log: logger.New("test"),
	})
	t.Cleanup(f.router.Close)
	f.broker = newFakeBroker(t)
	f.broker.users["held-pa"] = &fakeKCUser{Email: "pa@held.test", First: "Pat", Last: "Admin", Enabled: true}
	t.Setenv("DEV_MODE", "true")
	if deps.Plans == nil {
		deps.Plans = plan.NewEnforcer(f.control)
	}
	f.plans = deps.Plans
	deps.Router, deps.Keycloak = f.router, f.broker.client()
	f.srv = httptest.NewServer(NewHandlerWithDeps(logger.New("test"), f.control, nil, deps))
	t.Cleanup(f.srv.Close)
	f.reader = &handler{log: logger.New("test"), db: tenantdb.NewHandle(f.control, f.router)}
	return f
}

// tenant creates a dedicated tenant as the platform admin does, and returns
// its id, its workspace and a context routed to its database.
func (f *dedicatedFixture) tenant(t *testing.T, name string) (id, workspace string, tctx context.Context) {
	t.Helper()
	code, body := do(t, f.srv, f.pa, http.MethodPost, "/api/admin/tenants", map[string]string{"name": name, "plan": "enterprise"})
	var out struct {
		ID          string `json:"id"`
		WorkspaceID string `json:"workspace_id"`
	}
	if code != http.StatusOK || json.Unmarshal(body, &out) != nil || out.WorkspaceID == "" {
		t.Fatalf("create %s: %d %s", name, code, body)
	}
	p, err := f.router.Pool(f.ctx, out.ID)
	if err != nil {
		t.Fatal(err)
	}
	return out.ID, out.WorkspaceID, tenantdb.WithScope(f.ctx, tenantdb.Scope{CustomerID: out.ID, Pool: p})
}

// seed puts a person in a tenant's database, the directory and the identity
// provider, with role in workspace ("" for none), and returns their row id.
func (f *dedicatedFixture) seed(t *testing.T, tctx context.Context, sub, email, first, role, workspace string) string {
	t.Helper()
	tenant := tenantdb.TenantFrom(tctx)
	f.broker.mu.Lock()
	f.broker.users[sub] = &fakeKCUser{Email: email, First: first, Last: "Real", Enabled: true}
	f.broker.mu.Unlock()
	var id string
	if err := f.reader.db.QueryRow(tctx, `INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		VALUES ($1, $2, $3, $4::uuid) RETURNING id::text`, sub, email, first+" Real", tenant).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if role != "" {
		if _, err := f.reader.db.Exec(tctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id)
			VALUES ($1::uuid, $2::identity.user_role, NULLIF($3, '')::uuid)`, id, role, workspace); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.router.Catalog().AddUser(f.ctx, sub, tenant, email); err != nil {
		t.Fatal(err)
	}
	return id
}

// fixtureRow is one identity.user row as the tests compare it.
type fixtureRow struct{ id, sub, email, name, customer string }

// row reads the row under email in the database tctx is routed to.
func (f *dedicatedFixture) row(tctx context.Context, email string) (fixtureRow, bool) {
	var u fixtureRow
	err := f.reader.db.QueryRow(tctx, `SELECT id::text, keycloak_sub, email, display_name, COALESCE(customer_id::text, '')
		FROM identity.user WHERE lower(email) = lower($1)`, email).Scan(&u.id, &u.sub, &u.email, &u.name, &u.customer)
	return u, err == nil
}

// memberships is the directory's tenants for sub, sorted.
func (f *dedicatedFixture) memberships(sub string) []string {
	member, _ := f.router.Catalog().TenantsForUser(f.ctx, sub)
	slices.Sort(member)
	return member
}

// sent counts sub in one of the broker's records (invited, realmRoles,
// deleted).
func (f *dedicatedFixture) sent(list *[]string, sub string) int {
	f.broker.mu.Lock()
	defer f.broker.mu.Unlock()
	n := 0
	for _, s := range *list {
		if s == sub {
			n++
		}
	}
	return n
}

// invite is POST /api/admin/users as c, expecting want.
func (f *dedicatedFixture) invite(t *testing.T, c call, email, first, role, workspace string, want int) []byte {
	t.Helper()
	code, body := do(t, f.srv, c, http.MethodPost, "/api/admin/users", map[string]string{
		"email": email, "first_name": first, "last_name": "Typed", "role": role, "workspace_id": workspace})
	if code != want {
		t.Fatalf("invite %s as %s: %d %s; want %d", email, c.persona, code, body, want)
	}
	return body
}

// users is GET /api/admin/users as c, by id.
func (f *dedicatedFixture) users(t *testing.T, c call) map[string]listedUser {
	t.Helper()
	code, body := do(t, f.srv, c, http.MethodGet, "/api/admin/users", nil)
	var list []listedUser
	if code != http.StatusOK || json.Unmarshal(body, &list) != nil {
		t.Fatalf("users as %s: %d %s", c.persona, code, body)
	}
	out := map[string]listedUser{}
	for _, u := range list {
		out[u.ID] = u
	}
	return out
}

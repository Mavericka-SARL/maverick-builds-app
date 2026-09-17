// Tests the tiered role-assignment model on /api/admin/users: which
// identity.user_role values each admin tier (platform_admin, tenant_admin,
// developer) may grant or revoke, at user-creation time and on existing
// users, both as a NULL-workspace ("platform") grant and a workspace-scoped
// grant.
//
// This closes a real gap found while adding the tiered model: the
// workspace-scoped grant path (POST .../roles with workspace_id set) never
// checked which role value was being granted — only that the actor could
// touch the workspace — so any tenant_admin or developer could hand out
// workspace-scoped 'tenant_admin' or 'developer' rows. Because resolveActor
// aggregates role_assignment rows by name only, ignoring workspace_id (see
// actorByKeycloakSub), a workspace-scoped grant of a high-tier role confers
// that role's full authority everywhere — an escalation, not a scoped grant.
// TestWorkspaceScopedGrantCannotEscalate below reproduces that exact path
// and asserts it is now refused.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

type rolesFixture struct {
	pool *pgxpool.Pool
	srv  *httptest.Server

	custID, wsID string

	platformAdminSub, tenantAdminSub, developerSub, targetSub string
	targetID                                                  string
}

func setupRolesFixture(t *testing.T) *rolesFixture {
	t.Helper()
	ctx := context.Background()

	pool := testdb.New(t, migrationfs.FS, ".")

	f := &rolesFixture{pool: pool}
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

	f.custID = q(`INSERT INTO core.customer (name, plan) VALUES ('RoleCo', 'enterprise') RETURNING id::text`)
	f.wsID = q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, f.custID)

	f.platformAdminSub, f.tenantAdminSub, f.developerSub, f.targetSub = "roles-platform-admin", "roles-tenant-admin", "roles-developer", "roles-target"

	platformAdminID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ($1, 'pa@roleco.com', 'PA') RETURNING id::text`, f.platformAdminSub)
	exec(`INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'platform_admin')`, platformAdminID)

	tenantAdminID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'ta@roleco.com', 'TA', $2::uuid) RETURNING id::text`, f.tenantAdminSub, f.custID)
	exec(`INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'tenant_admin')`, tenantAdminID)

	developerID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'dev@roleco.com', 'Dev', $2::uuid) RETURNING id::text`, f.developerSub, f.custID)
	exec(`INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'developer')`, developerID)

	f.targetID = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'target@roleco.com', 'Target', $2::uuid) RETURNING id::text`, f.targetSub, f.custID)

	t.Setenv("DEV_MODE", "true")
	f.srv = httptest.NewServer(NewHandler(logger.New("test"), pool, nil))
	t.Cleanup(f.srv.Close)

	return f
}

func (f *rolesFixture) do(t *testing.T, method, path, sub string, body any) (int, map[string]any) {
	t.Helper()
	var buf []byte
	if body != nil {
		var err error
		buf, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequestWithContext(context.Background(), method, f.srv.URL+path, bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Dev-User", sub)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var parsed map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&parsed)
	return resp.StatusCode, parsed
}

func hasRoleRow(t *testing.T, pool *pgxpool.Pool, userID, role, workspaceID string) bool {
	t.Helper()
	var n int
	var err error
	if workspaceID == "" {
		err = pool.QueryRow(context.Background(),
			`SELECT count(*) FROM identity.role_assignment WHERE user_id=$1::uuid AND role=$2::identity.user_role AND workspace_id IS NULL`,
			userID, role).Scan(&n)
	} else {
		err = pool.QueryRow(context.Background(),
			`SELECT count(*) FROM identity.role_assignment WHERE user_id=$1::uuid AND role=$2::identity.user_role AND workspace_id=$3::uuid`,
			userID, role, workspaceID).Scan(&n)
	}
	if err != nil {
		t.Fatalf("check role row: %v", err)
	}
	return n > 0
}

// ── create-user role gate ────────────────────────────────────────────────────

func TestCreateUserRoleGate(t *testing.T) {
	f := setupRolesFixture(t)

	cases := []struct {
		name       string
		actorSub   string
		email      string
		role       string
		wantStatus int
	}{
		{"platform_admin can create a platform_admin", f.platformAdminSub, "new-pa@roleco.com", "platform_admin", http.StatusOK},
		{"platform_admin can create a tenant_admin", f.platformAdminSub, "new-pa-ta@roleco.com", "tenant_admin", http.StatusOK},
		{"tenant_admin can create a developer", f.tenantAdminSub, "new-ta-dev@roleco.com", "developer", http.StatusOK},
		{"tenant_admin can create a business_admin", f.tenantAdminSub, "new-ta-ba@roleco.com", "business_admin", http.StatusOK},
		{"tenant_admin CANNOT create a tenant_admin", f.tenantAdminSub, "new-ta-ta@roleco.com", "tenant_admin", http.StatusForbidden},
		{"tenant_admin CANNOT create a platform_admin", f.tenantAdminSub, "new-ta-pa@roleco.com", "platform_admin", http.StatusForbidden},
		{"developer can create a business_admin", f.developerSub, "new-dev-ba@roleco.com", "business_admin", http.StatusOK},
		{"developer can create a business_user", f.developerSub, "new-dev-bu@roleco.com", "business_user", http.StatusOK},
		{"developer CANNOT create a developer", f.developerSub, "new-dev-dev@roleco.com", "developer", http.StatusForbidden},
		{"developer CANNOT create a tenant_admin", f.developerSub, "new-dev-ta@roleco.com", "tenant_admin", http.StatusForbidden},
		{"developer CANNOT create a platform_admin", f.developerSub, "new-dev-pa@roleco.com", "platform_admin", http.StatusForbidden},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body := f.do(t, "POST", "/api/admin/users/", c.actorSub, map[string]string{
				"email": c.email, "display_name": "New User", "role": c.role,
			})
			if status != c.wantStatus {
				t.Fatalf("status = %d, want %d (body %v)", status, c.wantStatus, body)
			}
			if c.wantStatus != http.StatusOK {
				return
			}
			userID, _ := body["id"].(string)
			if userID == "" {
				t.Fatalf("expected created user id in response, got %v", body)
			}
			if !hasRoleRow(t, f.pool, userID, c.role, "") {
				t.Errorf("expected role %q assigned to created user", c.role)
			}
		})
	}
}

// ── add-role gate: platform-level (NULL workspace) grants ──────────────────

func TestAddPlatformLevelRoleGate(t *testing.T) {
	f := setupRolesFixture(t)

	cases := []struct {
		name       string
		actorSub   string
		role       string
		wantStatus int
	}{
		{"platform_admin grants platform-level tenant_admin", f.platformAdminSub, "tenant_admin", http.StatusOK},
		{"tenant_admin grants platform-level developer", f.tenantAdminSub, "developer", http.StatusOK},
		{"tenant_admin CANNOT grant platform-level tenant_admin", f.tenantAdminSub, "tenant_admin", http.StatusForbidden},
		{"tenant_admin CANNOT grant platform-level platform_admin", f.tenantAdminSub, "platform_admin", http.StatusForbidden},
		{"developer CANNOT grant platform-level developer", f.developerSub, "developer", http.StatusForbidden},
		{"developer CANNOT grant platform-level tenant_admin", f.developerSub, "tenant_admin", http.StatusForbidden},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body := f.do(t, "POST", "/api/admin/users/"+f.targetID+"/roles", c.actorSub, map[string]string{"role": c.role})
			if status != c.wantStatus {
				t.Fatalf("status = %d, want %d (body %v)", status, c.wantStatus, body)
			}
			if c.wantStatus == http.StatusOK && !hasRoleRow(t, f.pool, f.targetID, c.role, "") {
				t.Errorf("expected platform-level role %q granted", c.role)
			}
		})
	}
}

// ── add-role gate: workspace-scoped grants (the closed escalation path) ────

func TestWorkspaceScopedGrantCannotEscalate(t *testing.T) {
	f := setupRolesFixture(t)

	// The exploit this closes: before the fix, POST .../roles only checked
	// workspace_id != "" to skip the platform_admin-only gate entirely —
	// the role VALUE was never validated. A developer could grant
	// workspace-scoped 'tenant_admin' (or a tenant_admin could grant
	// workspace-scoped 'tenant_admin' to a peer), and because actor role
	// checks (hasRole) ignore workspace_id, that grant conferred full
	// tenant_admin authority everywhere, not just in that workspace.
	escalations := []struct {
		name     string
		actorSub string
		role     string
	}{
		{"developer cannot grant workspace-scoped developer (self-replication)", f.developerSub, "developer"},
		{"developer cannot grant workspace-scoped tenant_admin (escalation)", f.developerSub, "tenant_admin"},
		{"developer cannot grant workspace-scoped platform_admin (escalation)", f.developerSub, "platform_admin"},
		{"tenant_admin cannot grant workspace-scoped tenant_admin (self-replication)", f.tenantAdminSub, "tenant_admin"},
		{"tenant_admin cannot grant workspace-scoped platform_admin (escalation)", f.tenantAdminSub, "platform_admin"},
	}
	for _, c := range escalations {
		t.Run(c.name, func(t *testing.T) {
			status, body := f.do(t, "POST", "/api/admin/users/"+f.targetID+"/roles", c.actorSub, map[string]string{
				"role": c.role, "workspace_id": f.wsID,
			})
			if status != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body %v)", status, body)
			}
			if hasRoleRow(t, f.pool, f.targetID, c.role, f.wsID) {
				t.Fatalf("role %q was persisted despite the 403 — escalation succeeded", c.role)
			}
		})
	}

	// The legitimate counterpart: workspace-scoped grants of roles BELOW the
	// actor's own tier still work — but only for actors who may manage
	// resource access at all.
	//
	// "developer can grant workspace-scoped business_user" used to be in this
	// list. A workspace-scoped grant decides which workspace someone reaches,
	// which is resource access rather than a platform role, and developers no
	// longer make that call — see canManageResourceAccess in handler.go and
	// TestDeveloperCannotManageResourceAccess below. Developers keep every
	// other part of user administration, including platform-role grants within
	// their tier.
	allowed := []struct {
		name     string
		actorSub string
		role     string
	}{
		{"tenant_admin can grant workspace-scoped developer", f.tenantAdminSub, "developer"},
		{"tenant_admin can grant workspace-scoped business_admin", f.tenantAdminSub, "business_admin"},
	}
	for _, c := range allowed {
		t.Run(c.name, func(t *testing.T) {
			status, body := f.do(t, "POST", "/api/admin/users/"+f.targetID+"/roles", c.actorSub, map[string]string{
				"role": c.role, "workspace_id": f.wsID,
			})
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %v)", status, body)
			}
			if !hasRoleRow(t, f.pool, f.targetID, c.role, f.wsID) {
				t.Errorf("expected workspace-scoped role %q granted", c.role)
			}
		})
	}
}

// ── remove-role gate mirrors the grant gate ─────────────────────────────────

func TestRemoveRoleGate(t *testing.T) {
	f := setupRolesFixture(t)
	ctx := context.Background()

	// Seed two removable rows directly: a platform-level 'developer' and a
	// workspace-scoped 'tenant_admin' on the target user.
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'developer')`, f.targetID); err != nil {
		t.Fatalf("seed developer role: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'tenant_admin', $2::uuid)`, f.targetID, f.wsID); err != nil {
		t.Fatalf("seed workspace tenant_admin role: %v", err)
	}

	// A developer may not strip a tenant_admin's role — even workspace-scoped.
	status, body := f.do(t, "DELETE", "/api/admin/users/"+f.targetID+"/roles/tenant_admin?workspace_id="+f.wsID, f.developerSub, nil)
	if status != http.StatusForbidden {
		t.Fatalf("developer removing tenant_admin: status = %d, want 403 (body %v)", status, body)
	}
	if !hasRoleRow(t, f.pool, f.targetID, "tenant_admin", f.wsID) {
		t.Fatal("tenant_admin role was removed by a developer despite the 403")
	}

	// A tenant_admin may remove a platform-level developer role it granted.
	status, body = f.do(t, "DELETE", "/api/admin/users/"+f.targetID+"/roles/developer", f.tenantAdminSub, nil)
	if status != http.StatusOK {
		t.Fatalf("tenant_admin removing developer: status = %d, want 200 (body %v)", status, body)
	}
	if hasRoleRow(t, f.pool, f.targetID, "developer", "") {
		t.Error("developer role still present after removal")
	}

	// A tenant_admin may not remove another tenant_admin's role.
	status, body = f.do(t, "DELETE", "/api/admin/users/"+f.targetID+"/roles/tenant_admin?workspace_id="+f.wsID, f.tenantAdminSub, nil)
	if status != http.StatusForbidden {
		t.Fatalf("tenant_admin removing tenant_admin: status = %d, want 403 (body %v)", status, body)
	}
}

// ── resource-access gate: who decides what a user can reach ────────────────

// A developer may administer users — create, invite, rename, grant the
// business roles below their tier — but not decide who reaches which
// application, model or workspace. Developers build within a model; deciding
// who may open it belongs to whoever owns the tenant.
//
// Enforced server-side, not just hidden in the console: the console omits
// these controls for a developer, but omitting a button is not a boundary.
func TestDeveloperCannotManageResourceAccess(t *testing.T) {
	f := setupRolesFixture(t)
	ctx := context.Background()

	var appID, modelID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO core.application (customer_id, name, mode) VALUES ($1::uuid, 'App', 'planning') RETURNING id::text`,
		f.custID).Scan(&appID); err != nil {
		t.Fatalf("seed application: %v", err)
	}
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Model') RETURNING id::text`,
		appID).Scan(&modelID); err != nil {
		t.Fatalf("seed model: %v", err)
	}

	countRows := func(table, col, id string) int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(ctx,
			"SELECT count(*) FROM identity."+table+" WHERE user_id=$1::uuid AND "+col+"=$2::uuid",
			f.targetID, id).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}

	refused := []struct {
		name, method, path string
		check              func() int
	}{
		{"grant app access", "POST", "/api/admin/users/" + f.targetID + "/access/apps/" + appID,
			func() int { return countRows("user_app_access", "application_id", appID) }},
		{"grant model access", "POST", "/api/admin/users/" + f.targetID + "/access/models/" + modelID,
			func() int { return countRows("user_model_access", "model_id", modelID) }},
	}
	for _, c := range refused {
		t.Run("developer cannot "+c.name, func(t *testing.T) {
			status, body := f.do(t, c.method, c.path, f.developerSub, nil)
			if status != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (body %v)", status, body)
			}
			if n := c.check(); n != 0 {
				t.Fatalf("%d access row(s) written despite the 403", n)
			}
		})
	}

	// Workspace-scoped BUSINESS roles were carved out of this rule: a developer
	// could assign business_admin/business_user but only unscoped, and an
	// unscoped business role grants no application access at all, so the
	// capability produced users who could not open anything. See
	// canGrantWorkspaceRole and TestDeveloperCanGrantBusinessRolesInReachableWorkspace.
	// What stays closed is granting a role above the business tier inside a
	// workspace — that would hand out authority, not place someone in a team.
	t.Run("developer cannot grant a workspace-scoped role above the business tier", func(t *testing.T) {
		status, body := f.do(t, "POST", "/api/admin/users/"+f.targetID+"/roles", f.developerSub,
			map[string]string{"role": "developer", "workspace_id": f.wsID})
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body %v)", status, body)
		}
		if hasRoleRow(t, f.pool, f.targetID, "developer", f.wsID) {
			t.Fatal("workspace-scoped role persisted despite the 403")
		}
	})

	// The capability a developer keeps: platform-level roles within their tier.
	t.Run("developer can still grant a platform role in their tier", func(t *testing.T) {
		status, body := f.do(t, "POST", "/api/admin/users/"+f.targetID+"/roles", f.developerSub,
			map[string]string{"role": "business_user"})
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %v)", status, body)
		}
		if !hasRoleRow(t, f.pool, f.targetID, "business_user", "") {
			t.Fatal("platform role was not granted")
		}
	})

	// tenant_admin is unaffected — the rule is about developers only.
	t.Run("tenant_admin can still grant app access", func(t *testing.T) {
		status, body := f.do(t, "POST", "/api/admin/users/"+f.targetID+"/access/apps/"+appID, f.tenantAdminSub, nil)
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %v)", status, body)
		}
		if n := countRows("user_app_access", "application_id", appID); n != 1 {
			t.Fatalf("app access rows = %d, want 1", n)
		}
	})
}

// Changing the address here would desynchronise it from the identity
// provider, where it is the sign-in identity: the account would keep signing
// in under the old address while the console displayed the new one.
func TestUserEmailIsImmutable(t *testing.T) {
	f := setupRolesFixture(t)
	ctx := context.Background()

	status, body := f.do(t, "PATCH", "/api/admin/users/"+f.targetID, f.platformAdminSub,
		map[string]string{"email": "someone.else@roleco.com", "display_name": "Renamed"})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body %v)", status, body)
	}

	var email, name string
	if err := f.pool.QueryRow(ctx,
		`SELECT email, display_name FROM identity.user WHERE id=$1::uuid`, f.targetID).Scan(&email, &name); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if email != "target@roleco.com" {
		t.Errorf("email = %q, want it unchanged", email)
	}
	// The rejected request must not have applied its display_name either.
	if name == "Renamed" {
		t.Error("display_name was applied even though the request was rejected")
	}

	// Renaming without touching the address still works.
	status, body = f.do(t, "PATCH", "/api/admin/users/"+f.targetID, f.platformAdminSub,
		map[string]string{"display_name": "New Name"})
	if status != http.StatusOK {
		t.Fatalf("rename: status = %d, want 200 (body %v)", status, body)
	}
	if err := f.pool.QueryRow(ctx,
		`SELECT email, display_name FROM identity.user WHERE id=$1::uuid`, f.targetID).Scan(&email, &name); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if name != "New Name" {
		t.Errorf("display_name = %q, want %q", name, "New Name")
	}
	// The regression this guards: the update also wrote email=$2, so a
	// display-name-only request blanked the address.
	if email != "target@roleco.com" {
		t.Errorf("email = %q — a display-name-only update wiped the address", email)
	}
}

// ── self-destruction guards ─────────────────────────────────────────────────
//
// An administrator can end their own access two ways from the Users screen:
// the delete button, and the ✕ on their own role chip. Neither is undoable
// from inside the product, and for the sole platform admin — the shape a
// fresh deployment starts in, and the shape this one was in — either one
// leaves nobody able to administer users at all.

// The fixture records subjects, not ids; a self-referential request needs the id.
func (f *rolesFixture) userID(t *testing.T, sub string) string {
	t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT id::text FROM identity.user WHERE keycloak_sub=$1`, sub).Scan(&id); err != nil {
		t.Fatalf("look up user %q: %v", sub, err)
	}
	return id
}

func TestCannotDeleteOwnAccount(t *testing.T) {
	f := setupRolesFixture(t)
	ctx := context.Background()
	selfID := f.userID(t, f.platformAdminSub)

	status, body := f.do(t, "DELETE", "/api/admin/users/"+selfID, f.platformAdminSub, nil)
	if status != http.StatusForbidden {
		t.Fatalf("self-delete: status = %d, want 403 (body %v)", status, body)
	}
	var alive int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM identity.user WHERE id=$1::uuid`, selfID).Scan(&alive); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if alive != 1 {
		t.Fatal("the account was deleted despite the request being refused")
	}

	// The ::uuid casts in this handler accept spellings that differ as text —
	// a plain string comparison would let this one through and then delete the
	// row it claimed not to match.
	status, body = f.do(t, "DELETE", "/api/admin/users/"+strings.ToUpper(selfID), f.platformAdminSub, nil)
	if status != http.StatusForbidden {
		t.Fatalf("self-delete via an uppercase id: status = %d, want 403 (body %v)", status, body)
	}
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM identity.user WHERE id=$1::uuid`, selfID).Scan(&alive); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if alive != 1 {
		t.Fatal("an uppercase id got past the guard and deleted the account")
	}

	// The rule is about deleting yourself, not about platform admins being
	// undeletable: a second platform admin can still remove the first.
	otherSub := "roles-platform-admin-2"
	var otherID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ($1,'pa2@roleco.com','PA2') RETURNING id::text`,
		otherSub).Scan(&otherID); err != nil {
		t.Fatalf("seed second platform admin: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid,'platform_admin')`, otherID); err != nil {
		t.Fatalf("seed second platform admin role: %v", err)
	}
	status, body = f.do(t, "DELETE", "/api/admin/users/"+selfID, otherSub, nil)
	if status != http.StatusOK {
		t.Fatalf("delete by another admin: status = %d, want 200 (body %v)", status, body)
	}
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM identity.user WHERE id=$1::uuid`, selfID).Scan(&alive); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if alive != 0 {
		t.Error("another admin's delete did not take effect")
	}
}

func TestCannotRemoveOwnLastAdminRole(t *testing.T) {
	f := setupRolesFixture(t)
	ctx := context.Background()
	selfID := f.userID(t, f.platformAdminSub)

	// The only shape that reaches this guard: roleIsAssignableBy already stops
	// a developer or tenant_admin from revoking their own tier, so it is the
	// platform admin — the one who can assign every role, including their own
	// — who could otherwise strip the grant that lets them administer users.
	status, body := f.do(t, "DELETE", "/api/admin/users/"+selfID+"/roles/platform_admin", f.platformAdminSub, nil)
	if status != http.StatusForbidden {
		t.Fatalf("self-revoke of the last admin role: status = %d, want 403 (body %v)", status, body)
	}
	if !hasRoleRow(t, f.pool, selfID, "platform_admin", "") {
		t.Fatal("the role was removed despite the request being refused")
	}

	// Not a blanket ban on editing your own roles: one that has nothing to do
	// with reaching this screen still comes off.
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid,'business_user')`, selfID); err != nil {
		t.Fatalf("seed own business_user role: %v", err)
	}
	status, body = f.do(t, "DELETE", "/api/admin/users/"+selfID+"/roles/business_user", f.platformAdminSub, nil)
	if status != http.StatusOK {
		t.Fatalf("self-revoke of a non-admin role: status = %d, want 200 (body %v)", status, body)
	}
	if hasRoleRow(t, f.pool, selfID, "business_user", "") {
		t.Error("business_user was not actually removed")
	}

	// Nor a ban on giving up platform_admin as such — only on giving up the
	// last grant that keeps user administration reachable.
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid,'tenant_admin')`, selfID); err != nil {
		t.Fatalf("seed own tenant_admin role: %v", err)
	}
	status, body = f.do(t, "DELETE", "/api/admin/users/"+selfID+"/roles/platform_admin", f.platformAdminSub, nil)
	if status != http.StatusOK {
		t.Fatalf("self-revoke with another admin role remaining: status = %d, want 200 (body %v)", status, body)
	}
	if hasRoleRow(t, f.pool, selfID, "platform_admin", "") {
		t.Error("platform_admin was not actually removed")
	}
}

// ── developers placing people in workspaces ─────────────────────────────────
//
// roleIsAssignableBy has always let a developer assign business_admin and
// business_user, but canManageResourceAccess refused every workspace-scoped
// grant — and an unscoped business role grants no application access, because
// actorCanAccessApp resolves access by joining role_assignment.workspace_id to
// the application's workspace. The capability was reachable and useless: a
// developer could create business users who could not open anything.
//
// Developers may now scope those roles, and only those, and only into
// workspaces they already reach.

func TestDeveloperCanGrantBusinessRolesInReachableWorkspace(t *testing.T) {
	f := setupRolesFixture(t)

	for _, role := range []string{"business_user", "business_admin"} {
		t.Run(role, func(t *testing.T) {
			status, body := f.do(t, "POST", "/api/admin/users/"+f.targetID+"/roles", f.developerSub,
				map[string]string{"role": role, "workspace_id": f.wsID})
			if status != http.StatusOK {
				t.Fatalf("developer granting %s in its own tenant's workspace: status=%d, want 200 (body %v)", role, status, body)
			}
			if !hasRoleRow(t, f.pool, f.targetID, role, f.wsID) {
				t.Errorf("%s was not actually granted in the workspace", role)
			}
			// Symmetric: whoever may grant it may take it back.
			status, body = f.do(t, "DELETE",
				"/api/admin/users/"+f.targetID+"/roles/"+role+"?workspace_id="+f.wsID, f.developerSub, nil)
			if status != http.StatusOK {
				t.Fatalf("developer revoking %s: status=%d, want 200 (body %v)", role, status, body)
			}
			if hasRoleRow(t, f.pool, f.targetID, role, f.wsID) {
				t.Errorf("%s was not actually revoked", role)
			}
		})
	}
}

func TestDeveloperWorkspaceGrantStaysNarrow(t *testing.T) {
	f := setupRolesFixture(t)
	ctx := context.Background()

	// A workspace in a different tenant entirely.
	var otherCust, otherWS string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO core.customer (name, plan) VALUES ('OtherCo', 'enterprise') RETURNING id::text`).Scan(&otherCust); err != nil {
		t.Fatalf("seed other customer: %v", err)
	}
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'other-ws') RETURNING id::text`, otherCust).Scan(&otherWS); err != nil {
		t.Fatalf("seed other workspace: %v", err)
	}

	cases := []struct {
		name        string
		role        string
		workspaceID string
		wantStatus  int
	}{
		// The tier gate is unchanged: business roles are the only ones a
		// developer may hand out, scoped or not.
		{"developer cannot grant developer in a workspace", "developer", f.wsID, http.StatusForbidden},
		{"developer cannot grant tenant_admin in a workspace", "tenant_admin", f.wsID, http.StatusForbidden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body := f.do(t, "POST", "/api/admin/users/"+f.targetID+"/roles", f.developerSub,
				map[string]string{"role": c.role, "workspace_id": c.workspaceID})
			if status != c.wantStatus {
				t.Fatalf("status=%d, want %d (body %v)", status, c.wantStatus, body)
			}
			if hasRoleRow(t, f.pool, f.targetID, c.role, c.workspaceID) {
				t.Errorf("a refused grant of %q was written anyway", c.role)
			}
		})
	}

	// Reachability is still enforced — but not against f.developerSub, whose
	// developer role is unscoped and therefore makes them a global builder
	// with platform-wide reach by design (see isGlobalBuilder). A
	// workspace-scoped developer is the one the check actually constrains.
	scopedSub := "roles-developer-scoped"
	var scopedID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		 VALUES ($1, 'dev.scoped@roleco.com', 'Scoped Dev', $2::uuid) RETURNING id::text`,
		scopedSub, f.custID).Scan(&scopedID); err != nil {
		t.Fatalf("seed scoped developer: %v", err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'developer', $2::uuid)`,
		scopedID, f.wsID); err != nil {
		t.Fatalf("seed scoped developer role: %v", err)
	}
	if status, body := f.do(t, "POST", "/api/admin/users/"+f.targetID+"/roles", scopedSub,
		map[string]string{"role": "business_user", "workspace_id": otherWS}); status != http.StatusForbidden {
		t.Errorf("scoped developer reaching another tenant's workspace: status=%d, want 403 (body %v)", status, body)
	}
	if hasRoleRow(t, f.pool, f.targetID, "business_user", otherWS) {
		t.Error("a refused cross-tenant grant was written anyway")
	}

	// The other half of the boundary is untouched: deciding which application
	// or model someone reaches is still not a developer's call.
	var appID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO core.application (customer_id, name, mode) VALUES ($1::uuid, 'ScopeApp', 'planning') RETURNING id::text`,
		f.custID).Scan(&appID); err != nil {
		t.Fatalf("seed application: %v", err)
	}
	status, body := f.do(t, "POST",
		"/api/admin/users/"+f.targetID+"/access/apps/"+appID, f.developerSub, nil)
	if status != http.StatusForbidden {
		t.Errorf("developer granting application access: status=%d, want 403 (body %v)", status, body)
	}
}

// Inviting is the path people actually use, and it must not be a way around
// either check.
func TestDeveloperInviteWithWorkspaceScopedRole(t *testing.T) {
	f := setupRolesFixture(t)
	ctx := context.Background()

	status, body := f.do(t, "POST", "/api/admin/users/", f.developerSub, map[string]string{
		"email": "scoped@roleco.com", "first_name": "Scoped", "last_name": "User", "role": "business_user", "workspace_id": f.wsID,
	})
	if status != http.StatusOK {
		t.Fatalf("invite with a workspace-scoped business role: status=%d, want 200 (body %v)", status, body)
	}
	newID, _ := body["id"].(string)
	if newID == "" {
		t.Fatal("no id returned")
	}
	// The point of the whole change: the assignment carries the workspace, so
	// actorCanAccessApp's join has something to match.
	if !hasRoleRow(t, f.pool, newID, "business_user", f.wsID) {
		var ws *string
		_ = f.pool.QueryRow(ctx,
			`SELECT workspace_id::text FROM identity.role_assignment WHERE user_id=$1::uuid`, newID).Scan(&ws)
		t.Fatalf("invited user's business_user role is not scoped to the workspace (workspace_id=%v)", ws)
	}

	// And the tier gate still applies on this path.
	status, body = f.do(t, "POST", "/api/admin/users/", f.developerSub, map[string]string{
		"email": "nope@roleco.com", "first_name": "Nope", "last_name": "User", "role": "developer", "workspace_id": f.wsID,
	})
	if status != http.StatusForbidden {
		t.Errorf("developer inviting another developer: status=%d, want 403 (body %v)", status, body)
	}
}

// Keycloak's realm marks first and last name mandatory. An account created
// without them is met with an "Update Account Information" form the moment the
// invitation is opened, asking the new user for something whoever invited them
// already knew — and blocking sign-in until they answer. The invite is where
// both names have to be collected, so a missing one is refused there instead.
func TestInviteRequiresBothNames(t *testing.T) {
	f := setupRolesFixture(t)
	ctx := context.Background()

	cases := []struct {
		name       string
		body       map[string]string
		wantStatus int
	}{
		{"both names given", map[string]string{"email": "a@roleco.com", "first_name": "Ada", "last_name": "Lovelace"}, http.StatusOK},
		{"last name missing", map[string]string{"email": "b@roleco.com", "first_name": "Ada"}, http.StatusBadRequest},
		{"first name missing", map[string]string{"email": "c@roleco.com", "last_name": "Lovelace"}, http.StatusBadRequest},
		{"neither given", map[string]string{"email": "d@roleco.com"}, http.StatusBadRequest},
		// A display name carrying both still works, for callers predating the
		// split — this is the shape the console itself used to send.
		{"display name with a surname", map[string]string{"email": "e@roleco.com", "display_name": "Ada Lovelace"}, http.StatusOK},
		// …but a one-word one does not, because that is exactly the input that
		// produced an account with no surname.
		{"display name with no surname", map[string]string{"email": "f@roleco.com", "display_name": "Ada"}, http.StatusBadRequest},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, body := f.do(t, "POST", "/api/admin/users/", f.platformAdminSub, c.body)
			if status != c.wantStatus {
				t.Fatalf("status=%d, want %d (body %v)", status, c.wantStatus, body)
			}
			var n int
			if err := f.pool.QueryRow(ctx,
				`SELECT count(*) FROM identity.user WHERE email=$1`, c.body["email"]).Scan(&n); err != nil {
				t.Fatalf("read back: %v", err)
			}
			want := 0
			if c.wantStatus == http.StatusOK {
				want = 1
			}
			if n != want {
				t.Errorf("%d user row(s) for %s, want %d", n, c.body["email"], want)
			}
		})
	}

	// The stored display name is assembled from the two, so the console keeps
	// showing a whole name without a separate field to keep in step.
	var display string
	if err := f.pool.QueryRow(ctx,
		`SELECT display_name FROM identity.user WHERE email='a@roleco.com'`).Scan(&display); err != nil {
		t.Fatalf("read display name: %v", err)
	}
	if display != "Ada Lovelace" {
		t.Errorf("display_name = %q, want %q", display, "Ada Lovelace")
	}
}

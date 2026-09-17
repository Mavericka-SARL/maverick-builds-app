package identity_test

import (
	"context"
	"embed"
	"testing"

	commonv1 "github.com/mavericks-engine/mavericks/gen/go/common/v1"
	"github.com/mavericks-engine/mavericks/internal/identity"
	"github.com/mavericks-engine/mavericks/internal/testdb"
)

//go:embed testdata/*.sql
var testMigrations embed.FS

func setupDB(t *testing.T) *identity.Store {
	t.Helper()

	pool := testdb.New(t, testMigrations, "testdata")
	return identity.NewStore(pool)
}

func TestUpsertAndGetUser(t *testing.T) {
	store := setupDB(t)
	ctx := context.Background()

	userID, err := store.UpsertUserFromClaims(ctx, "sub-001", "alice@example.com", "Alice")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	user, err := store.GetUser(ctx, userID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if user.Email != "alice@example.com" {
		t.Errorf("email = %q, want alice@example.com", user.Email)
	}
	if user.DisplayName != "Alice" {
		t.Errorf("display_name = %q, want Alice", user.DisplayName)
	}
}

func TestUpsertIdempotent(t *testing.T) {
	store := setupDB(t)
	ctx := context.Background()

	id1, err := store.UpsertUserFromClaims(ctx, "sub-idem", "bob@example.com", "Bob")
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	id2, err := store.UpsertUserFromClaims(ctx, "sub-idem", "bob@example.com", "Bobby")
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if id1 != id2 {
		t.Errorf("upsert changed ID: got %q want %q", id2, id1)
	}

	// display_name should be updated
	user, _ := store.GetUser(ctx, id1)
	if user.DisplayName != "Bobby" {
		t.Errorf("display_name = %q, want Bobby", user.DisplayName)
	}
}

func TestGetUserByKeycloakSub(t *testing.T) {
	store := setupDB(t)
	ctx := context.Background()

	_, err := store.UpsertUserFromClaims(ctx, "sub-xyz", "carol@example.com", "Carol")
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}

	user, err := store.GetUserByKeycloakSub(ctx, "sub-xyz")
	if err != nil {
		t.Fatalf("get by sub: %v", err)
	}
	if user.Email != "carol@example.com" {
		t.Errorf("email = %q, want carol@example.com", user.Email)
	}
}

func TestAssignAndListRoles(t *testing.T) {
	store := setupDB(t)
	ctx := context.Background()

	id, _ := store.UpsertUserFromClaims(ctx, "sub-admin", "admin@example.com", "Admin")

	if err := store.AssignRole(ctx, id, commonv1.Role_ROLE_PLATFORM_ADMIN, nil, ""); err != nil {
		t.Fatalf("assign role: %v", err)
	}

	user, err := store.GetUser(ctx, id)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if user.Role != commonv1.Role_ROLE_PLATFORM_ADMIN {
		t.Errorf("role = %v, want ROLE_PLATFORM_ADMIN", user.Role)
	}
}

// TestAssignRoleTenantAdminRoundTrips is a D1 regression test: tenant_admin
// (added to identity.user_role later than the original 4-value enum) had
// no commonv1.Role protobuf equivalent — roleFromString silently mapped it
// to ROLE_UNSPECIFIED, and roleToString's default then mapped that back to
// "business_user", a silent role downgrade through AssignRole. Confirms
// the new ROLE_TENANT_ADMIN value round-trips correctly.
func TestAssignRoleTenantAdminRoundTrips(t *testing.T) {
	store := setupDB(t)
	ctx := context.Background()

	id, _ := store.UpsertUserFromClaims(ctx, "sub-tenant-admin", "tadmin@example.com", "Tenant Admin")

	if err := store.AssignRole(ctx, id, commonv1.Role_ROLE_TENANT_ADMIN, nil, ""); err != nil {
		t.Fatalf("assign role: %v", err)
	}

	user, err := store.GetUser(ctx, id)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if user.Role != commonv1.Role_ROLE_TENANT_ADMIN {
		t.Errorf("role = %v, want ROLE_TENANT_ADMIN", user.Role)
	}
}

// TestAssignRoleRejectsUnmappedRole is the other half of the D1 fix:
// roleToString now returns "" for an unrecognized commonv1.Role (e.g. the
// zero-value ROLE_UNSPECIFIED passed by mistake) instead of silently
// defaulting to "business_user", and AssignRole must reject that rather
// than inserting a wrong-but-valid role string.
func TestAssignRoleRejectsUnmappedRole(t *testing.T) {
	store := setupDB(t)
	ctx := context.Background()

	id, _ := store.UpsertUserFromClaims(ctx, "sub-unmapped-role", "unmapped@example.com", "Unmapped")

	if err := store.AssignRole(ctx, id, commonv1.Role_ROLE_UNSPECIFIED, nil, ""); err == nil {
		t.Fatal("AssignRole with ROLE_UNSPECIFIED: want an error, got nil (silently stored as some real role)")
	}

	user, err := store.GetUser(ctx, id)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if user.Role != commonv1.Role_ROLE_UNSPECIFIED {
		t.Errorf("role = %v, want ROLE_UNSPECIFIED (no role assignment should have been inserted)", user.Role)
	}
}

func TestListUsers(t *testing.T) {
	store := setupDB(t)
	ctx := context.Background()

	for i, sub := range []string{"sub-u1", "sub-u2", "sub-u3"} {
		email := sub + "@example.com"
		id, err := store.UpsertUserFromClaims(ctx, sub, email, sub)
		if err != nil {
			t.Fatalf("upsert user %d: %v", i, err)
		}
		_ = store.AssignRole(ctx, id, commonv1.Role_ROLE_BUSINESS_USER, nil, "")
	}

	users, err := store.ListUsers(ctx, "", 100, 0)
	if err != nil {
		t.Fatalf("list users: %v", err)
	}
	if len(users) < 3 {
		t.Errorf("len(users) = %d, want >= 3", len(users))
	}
}

func TestCreateAndRevokeAPIKey(t *testing.T) {
	store := setupDB(t)
	ctx := context.Background()

	userID, _ := store.UpsertUserFromClaims(ctx, "sub-apikey", "key@example.com", "Key User")

	keyID, err := store.CreateAPIKey(ctx, userID, "test-key", "hashed-value", nil)
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	if keyID == "" {
		t.Error("api key ID should not be empty")
	}

	if err := store.RevokeAPIKey(ctx, keyID); err != nil {
		t.Fatalf("revoke api key: %v", err)
	}
}

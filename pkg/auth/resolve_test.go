package auth_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	commonv1 "github.com/mavericks-engine/mavericks/gen/go/common/v1"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/auth"
)

// setupDB mirrors internal/calculation/integration_test.go's setupDB /
// internal/gateway/jwt_auth_test.go's setupJWTAuthDB pattern: a real
// Postgres container with the full app schema applied.
func setupDB(t *testing.T) *pgxpool.Pool {
	t.Helper()

	pool := testdb.New(t, migrationfs.FS, ".")
	return pool
}

func insertUser(t *testing.T, pool *pgxpool.Pool, sub, email string, roles ...string) string {
	t.Helper()
	ctx := context.Background()

	var userID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ($1, $2, $1) RETURNING id::text
	`, sub, email).Scan(&userID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	for _, role := range roles {
		if _, err := pool.Exec(ctx, `
			INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, $2::identity.user_role)
		`, userID, role); err != nil {
			t.Fatalf("insert role_assignment %q: %v", role, err)
		}
	}
	return userID
}

func TestResolveActorByKeycloakSub(t *testing.T) {
	pool := setupDB(t)

	t.Run("single role", func(t *testing.T) {
		userID := insertUser(t, pool, "sub-single", "single@t.com", "business_user")
		a, err := auth.ResolveActorByKeycloakSub(context.Background(), pool, "sub-single")
		if err != nil {
			t.Fatalf("ResolveActorByKeycloakSub: %v", err)
		}
		if a.UserId != userID {
			t.Errorf("UserId = %q, want %q", a.UserId, userID)
		}
		if a.Role != commonv1.Role_ROLE_BUSINESS_USER {
			t.Errorf("Role = %v, want ROLE_BUSINESS_USER", a.Role)
		}
	})

	t.Run("multiple roles collapse to the highest-privilege one", func(t *testing.T) {
		insertUser(t, pool, "sub-multi", "multi@t.com", "business_user", "developer", "business_admin")
		a, err := auth.ResolveActorByKeycloakSub(context.Background(), pool, "sub-multi")
		if err != nil {
			t.Fatalf("ResolveActorByKeycloakSub: %v", err)
		}
		if a.Role != commonv1.Role_ROLE_DEVELOPER {
			t.Errorf("Role = %v, want ROLE_DEVELOPER (higher priority than business_user/business_admin)", a.Role)
		}
	})

	t.Run("no roles resolves UNSPECIFIED, not an error", func(t *testing.T) {
		insertUser(t, pool, "sub-none", "none@t.com")
		a, err := auth.ResolveActorByKeycloakSub(context.Background(), pool, "sub-none")
		if err != nil {
			t.Fatalf("ResolveActorByKeycloakSub: %v", err)
		}
		if a.Role != commonv1.Role_ROLE_UNSPECIFIED {
			t.Errorf("Role = %v, want ROLE_UNSPECIFIED", a.Role)
		}
	})

	t.Run("unknown sub is an error", func(t *testing.T) {
		if _, err := auth.ResolveActorByKeycloakSub(context.Background(), pool, "no-such-sub"); err == nil {
			t.Error("ResolveActorByKeycloakSub: got nil error, want one for an unknown sub")
		}
	})
}

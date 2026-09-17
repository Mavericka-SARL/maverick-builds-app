package auth

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	commonv1 "github.com/mavericks-engine/mavericks/gen/go/common/v1"
)

// ResolveActorByKeycloakSub looks up a real identity.user by keycloak_sub
// and collapses their identity.role_assignment rows into a single
// commonv1.Actor for gRPC use (WithActor/ActorFromContext). Mirrors, but
// does not replace, internal/gateway's own actorByKeycloakSub — that one
// returns gateway's private multi-role `actor` type for HTTP role-gate
// checks; this returns commonv1.Actor (a single Role enum field) because
// that's what this package's context plumbing is typed for.
//
// commonv1.Role is a single enum but a real user can hold multiple
// identity.role_assignment rows, so multiple roles collapse to the
// highest-privilege one via highestRole. This is a deliberate
// simplification: nothing that consumes ActorFromContext today reads
// .Role (only .UserId, e.g. internal/importpkg's callerUserID) — v1 scope
// is closing the verified UserId gap, not building gRPC-side multi-role
// authorization.
//
// Actor.WorkspaceId is left empty: nothing reads it from context yet, and
// scoping it correctly needs a per-RPC "which workspace is this call
// about" answer that doesn't exist today.
func ResolveActorByKeycloakSub(ctx context.Context, pool *pgxpool.Pool, sub string) (*commonv1.Actor, error) {
	var userID, rolesCSV string
	err := pool.QueryRow(ctx, `
		SELECT u.id::text, COALESCE(string_agg(DISTINCT ra.role::text, ','), '')
		FROM identity.user u
		LEFT JOIN identity.role_assignment ra ON ra.user_id = u.id
		WHERE u.keycloak_sub = $1 AND u.disabled_at IS NULL
		GROUP BY u.id
	`, sub).Scan(&userID, &rolesCSV)
	if err != nil {
		return nil, fmt.Errorf("resolve actor %q: %w", sub, err)
	}

	var roles []string
	if rolesCSV != "" {
		roles = strings.Split(rolesCSV, ",")
	}

	return &commonv1.Actor{UserId: userID, Role: highestRole(roles)}, nil
}

// rolePriority orders identity.role_assignment role names from highest to
// lowest privilege for collapsing a multi-role user onto commonv1.Role's
// single enum field.
var rolePriority = []struct {
	name string
	role commonv1.Role
}{
	{"platform_admin", commonv1.Role_ROLE_PLATFORM_ADMIN},
	{"developer", commonv1.Role_ROLE_DEVELOPER},
	{"business_admin", commonv1.Role_ROLE_BUSINESS_ADMIN},
	{"business_user", commonv1.Role_ROLE_BUSINESS_USER},
}

func highestRole(roles []string) commonv1.Role {
	held := make(map[string]bool, len(roles))
	for _, r := range roles {
		held[r] = true
	}
	for _, p := range rolePriority {
		if held[p.name] {
			return p.role
		}
	}
	return commonv1.Role_ROLE_UNSPECIFIED
}

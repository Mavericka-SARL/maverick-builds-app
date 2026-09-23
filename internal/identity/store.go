package identity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/mavericks-engine/mavericks/gen/go/common/v1"
	identityv1 "github.com/mavericks-engine/mavericks/gen/go/identity/v1"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

var ErrNotFound = errors.New("not found")

// UpsertUserFromClaims creates or updates a user record from validated JWT claims.
// Returns the user's internal ID and current role.
func (s *Store) UpsertUserFromClaims(ctx context.Context, sub, email, displayName string) (string, error) {
	var userID string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, last_login_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (keycloak_sub) DO UPDATE
			SET email = EXCLUDED.email,
			    display_name = EXCLUDED.display_name,
			    last_login_at = now()
		RETURNING id
	`, sub, email, displayName).Scan(&userID)
	if err != nil {
		return "", fmt.Errorf("upsert user: %w", err)
	}
	return userID, nil
}

func (s *Store) GetUser(ctx context.Context, userID string) (*identityv1.User, error) {
	var u identityv1.User
	var createdAt, lastLoginAt time.Time
	var roleStr *string

	err := s.pool.QueryRow(ctx, `
		SELECT u.id, u.email, u.display_name, u.created_at, u.last_login_at,
		       r.role
		FROM identity.user u
		LEFT JOIN identity.role_assignment r ON r.user_id = u.id
		WHERE u.id = $1
		LIMIT 1
	`, userID).Scan(&u.Id, &u.Email, &u.DisplayName, &createdAt, &lastLoginAt, &roleStr)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get user: %w", err)
	}

	u.CreatedAt = timestamppb.New(createdAt)
	u.LastLoginAt = timestamppb.New(lastLoginAt)
	if roleStr != nil {
		u.Role = roleFromString(*roleStr)
	}
	return &u, nil
}

func (s *Store) GetUserByKeycloakSub(ctx context.Context, sub string) (*identityv1.User, error) {
	var userID string
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM identity.user WHERE keycloak_sub = $1`, sub,
	).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.GetUser(ctx, userID)
}

func (s *Store) AssignRole(ctx context.Context, userID string, role commonv1.Role, workspaceID *string, assignedBy string) error {
	wsID := (*uuid.UUID)(nil)
	if workspaceID != nil && *workspaceID != "" {
		id, err := uuid.Parse(*workspaceID)
		if err != nil {
			return fmt.Errorf("invalid workspace_id: %w", err)
		}
		wsID = &id
	}

	var assignedByPtr *string
	if assignedBy != "" {
		assignedByPtr = &assignedBy
	}

	roleStr := roleToString(role)
	if roleStr == "" {
		return fmt.Errorf("invalid role: %v", role)
	}

	_, err := s.pool.Exec(ctx, `
		INSERT INTO identity.role_assignment (user_id, role, workspace_id, assigned_by)
		VALUES ($1, $2::identity.user_role, $3, $4)
		-- No conflict target: a platform-level grant (workspace_id NULL) is
		-- arbitrated by the partial unique index from migration 086, which
		-- naming the constraint's columns would exclude — turning a repeat
		-- grant into a unique violation instead of a no-op.
		ON CONFLICT DO NOTHING
	`, userID, roleStr, wsID, assignedByPtr)
	return err
}

func (s *Store) RevokeRole(ctx context.Context, userID, workspaceID string) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM identity.role_assignment
		WHERE user_id = $1 AND workspace_id = $2
	`, userID, workspaceID)
	return err
}

func (s *Store) CreateAPIKey(ctx context.Context, userID, name, keyHash string, expiresAt *time.Time) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO identity.api_key (user_id, name, key_hash, expires_at)
		VALUES ($1, $2, $3, $4)
		RETURNING id
	`, userID, name, keyHash, expiresAt).Scan(&id)
	return id, err
}

func (s *Store) RevokeAPIKey(ctx context.Context, apiKeyID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE identity.api_key SET revoked_at = now() WHERE id = $1
	`, apiKeyID)
	return err
}

// ListUsers returns all users, optionally filtered to a single customer.
func (s *Store) ListUsers(ctx context.Context, customerID string, limit, offset int) ([]*identityv1.User, error) {
	const qAll = `
		SELECT u.id, u.email, u.display_name, u.created_at, u.last_login_at,
		       COALESCE(u.customer_id::text, ''), r.role
		FROM identity.user u
		LEFT JOIN LATERAL (
			SELECT role FROM identity.role_assignment
			WHERE user_id = u.id
			ORDER BY CASE role
				WHEN 'platform_admin' THEN 1
				WHEN 'developer'      THEN 2
				WHEN 'business_admin' THEN 3
				ELSE 4 END
			LIMIT 1
		) r ON true
		ORDER BY u.created_at DESC
		LIMIT $1 OFFSET $2`

	const qByCustomer = `
		SELECT u.id, u.email, u.display_name, u.created_at, u.last_login_at,
		       COALESCE(u.customer_id::text, ''), r.role
		FROM identity.user u
		LEFT JOIN LATERAL (
			SELECT role FROM identity.role_assignment
			WHERE user_id = u.id
			ORDER BY CASE role
				WHEN 'platform_admin' THEN 1
				WHEN 'developer'      THEN 2
				WHEN 'business_admin' THEN 3
				ELSE 4 END
			LIMIT 1
		) r ON true
		WHERE u.customer_id = $1::uuid
		ORDER BY u.created_at DESC
		LIMIT $2 OFFSET $3`

	var (
		pgrows pgx.Rows
		err    error
	)
	if customerID != "" {
		pgrows, err = s.pool.Query(ctx, qByCustomer, customerID, limit, offset)
	} else {
		pgrows, err = s.pool.Query(ctx, qAll, limit, offset)
	}
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer pgrows.Close()

	var users []*identityv1.User
	for pgrows.Next() {
		var u identityv1.User
		var createdAt, lastLoginAt time.Time
		var roleStr *string
		if err := pgrows.Scan(&u.Id, &u.Email, &u.DisplayName, &createdAt, &lastLoginAt, &u.CustomerId, &roleStr); err != nil {
			return nil, err
		}
		u.CreatedAt = timestamppb.New(createdAt)
		u.LastLoginAt = timestamppb.New(lastLoginAt)
		if roleStr != nil {
			u.Role = roleFromString(*roleStr)
		}
		users = append(users, &u)
	}
	return users, pgrows.Err()
}

// CreateUser inserts a new user directly (for admin-created users without SSO).
func (s *Store) CreateUser(ctx context.Context, email, displayName, keycloakSub string) (string, error) {
	if keycloakSub == "" {
		keycloakSub = "admin-created-" + uuid.New().String()
	}
	var userID string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name)
		VALUES ($1, $2, $3) RETURNING id
	`, keycloakSub, email, displayName).Scan(&userID)
	if err != nil {
		return "", fmt.Errorf("create user: %w", err)
	}
	return userID, nil
}

// UpdateUser updates mutable fields for a user.
func (s *Store) UpdateUser(ctx context.Context, userID, email, displayName string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE identity.user SET email = $2, display_name = $3 WHERE id = $1
	`, userID, email, displayName)
	return err
}

// DeleteUser removes a user and all their role assignments.
func (s *Store) DeleteUser(ctx context.Context, userID string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM identity.user WHERE id = $1`, userID)
	return err
}

// UpdateUserRole replaces all role assignments for a user in a workspace.
func (s *Store) UpdateUserRole(ctx context.Context, userID, workspaceID string, role commonv1.Role) error {
	_, err := s.pool.Exec(ctx, `
		DELETE FROM identity.role_assignment WHERE user_id = $1 AND workspace_id = $2
	`, userID, workspaceID)
	if err != nil {
		return err
	}
	return s.AssignRole(ctx, userID, role, &workspaceID, "")
}

func roleFromString(s string) commonv1.Role {
	switch s {
	case "platform_admin":
		return commonv1.Role_ROLE_PLATFORM_ADMIN
	case "developer":
		return commonv1.Role_ROLE_DEVELOPER
	case "business_admin":
		return commonv1.Role_ROLE_BUSINESS_ADMIN
	case "business_user":
		return commonv1.Role_ROLE_BUSINESS_USER
	case "tenant_admin":
		return commonv1.Role_ROLE_TENANT_ADMIN
	default:
		return commonv1.Role_ROLE_UNSPECIFIED
	}
}

// roleToString returns "" for an unrecognized role rather than silently
// defaulting to "business_user" — the previous default masqueraded any
// unmapped role (e.g. ROLE_UNSPECIFIED) as a real, valid, lower-privilege
// role instead of failing. AssignRole below rejects an empty result rather
// than inserting it.
func roleToString(r commonv1.Role) string {
	switch r {
	case commonv1.Role_ROLE_PLATFORM_ADMIN:
		return "platform_admin"
	case commonv1.Role_ROLE_DEVELOPER:
		return "developer"
	case commonv1.Role_ROLE_BUSINESS_ADMIN:
		return "business_admin"
	case commonv1.Role_ROLE_BUSINESS_USER:
		return "business_user"
	case commonv1.Role_ROLE_TENANT_ADMIN:
		return "tenant_admin"
	default:
		return ""
	}
}

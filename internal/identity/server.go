package identity

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"time"

	"github.com/rs/zerolog"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/mavericks-engine/mavericks/gen/go/common/v1"
	identityv1 "github.com/mavericks-engine/mavericks/gen/go/identity/v1"
)

type Server struct {
	identityv1.UnimplementedIdentityServiceServer
	log      zerolog.Logger
	store    *Store
	jwks     *JWKSValidator
	sessions *SessionStore // nil in test/dev environments
}

func NewServer(log zerolog.Logger, store *Store, jwks *JWKSValidator) *Server {
	return &Server{log: log, store: store, jwks: jwks}
}

func (s *Server) WithSessionStore(ss *SessionStore) *Server {
	s.sessions = ss
	return s
}

func (s *Server) ValidateToken(ctx context.Context, req *identityv1.ValidateTokenRequest) (*identityv1.ValidateTokenResponse, error) {
	// Fast path: Redis session cache
	if s.sessions != nil {
		if cached, ok := s.sessions.Get(ctx, req.Token); ok {
			return cached, nil
		}
	}

	claims, err := s.jwks.Validate(req.Token)
	if err != nil {
		s.log.Debug().Err(err).Msg("token validation failed")
		return &identityv1.ValidateTokenResponse{Valid: false}, nil
	}

	user, err := s.store.GetUserByKeycloakSub(ctx, claims.Subject)
	if errors.Is(err, ErrNotFound) {
		// First-time login — provision the user record
		userID, err := s.store.UpsertUserFromClaims(ctx, claims.Subject, claims.Email, claims.PreferredUsername)
		if err != nil {
			s.log.Error().Err(err).Str("sub", claims.Subject).Msg("failed to upsert user")
			return nil, status.Error(codes.Internal, "user provisioning failed")
		}
		user, err = s.store.GetUser(ctx, userID)
		if err != nil {
			return nil, status.Error(codes.Internal, "failed to fetch user after provisioning")
		}
	} else if err != nil {
		return nil, status.Error(codes.Internal, "store error")
	}

	// Populate session cache for subsequent requests
	if s.sessions != nil {
		s.sessions.Set(ctx, req.Token, user)
	}

	return &identityv1.ValidateTokenResponse{
		Valid: true,
		Actor: &commonv1.Actor{UserId: user.Id, Role: user.Role},
	}, nil
}

func (s *Server) GetUser(ctx context.Context, req *identityv1.GetUserRequest) (*identityv1.GetUserResponse, error) {
	user, err := s.store.GetUser(ctx, req.UserId)
	if errors.Is(err, ErrNotFound) {
		return nil, status.Error(codes.NotFound, "user not found")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &identityv1.GetUserResponse{User: user}, nil
}

func (s *Server) ListUsers(ctx context.Context, req *identityv1.ListUsersRequest) (*identityv1.ListUsersResponse, error) {
	limit := 50
	if req.Page != nil && req.Page.PageSize > 0 && req.Page.PageSize <= 200 {
		limit = int(req.Page.PageSize)
	}
	users, err := s.store.ListUsers(ctx, req.CustomerId, limit, 0)
	if err != nil {
		s.log.Error().Err(err).Msg("list users")
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &identityv1.ListUsersResponse{Users: users}, nil
}

func (s *Server) AssignRole(ctx context.Context, req *identityv1.AssignRoleRequest) (*identityv1.AssignRoleResponse, error) {
	var wsID *string
	if req.WorkspaceId != "" {
		wsID = &req.WorkspaceId
	}

	if err := s.store.AssignRole(ctx, req.UserId, req.Role, wsID, ""); err != nil {
		s.log.Error().Err(err).Msg("assign role")
		return nil, status.Error(codes.Internal, err.Error())
	}

	// Invalidate any cached sessions so the new role takes effect immediately
	if s.sessions != nil {
		_ = s.sessions.RevokeAll(ctx, req.UserId)
	}

	return &identityv1.AssignRoleResponse{Success: true}, nil
}

func (s *Server) RevokeRole(ctx context.Context, req *identityv1.RevokeRoleRequest) (*identityv1.RevokeRoleResponse, error) {
	if err := s.store.RevokeRole(ctx, req.UserId, req.WorkspaceId); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &identityv1.RevokeRoleResponse{Success: true}, nil
}

func (s *Server) CreateAPIKey(ctx context.Context, req *identityv1.CreateAPIKeyRequest) (*identityv1.CreateAPIKeyResponse, error) {
	raw, err := generateAPIKey()
	if err != nil {
		return nil, status.Error(codes.Internal, "key generation failed")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(raw), bcrypt.DefaultCost)
	if err != nil {
		return nil, status.Error(codes.Internal, "key hashing failed")
	}

	var expiresAt *time.Time
	if req.ExpiresAt != nil {
		t := req.ExpiresAt.AsTime()
		expiresAt = &t
	}

	id, err := s.store.CreateAPIKey(ctx, req.UserId, req.Name, string(hash), expiresAt)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &identityv1.CreateAPIKeyResponse{ApiKeyId: id, Key: raw}, nil
}

func (s *Server) RevokeAPIKey(ctx context.Context, req *identityv1.RevokeAPIKeyRequest) (*identityv1.RevokeAPIKeyResponse, error) {
	if err := s.store.RevokeAPIKey(ctx, req.ApiKeyId); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &identityv1.RevokeAPIKeyResponse{Success: true}, nil
}

func generateAPIKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "mv_" + base64.RawURLEncoding.EncodeToString(b), nil
}

// Ensure timestamppb is used (avoid import pruning)
var _ = timestamppb.Now

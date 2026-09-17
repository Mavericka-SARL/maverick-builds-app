package identity

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"

	commonv1 "github.com/mavericks-engine/mavericks/gen/go/common/v1"
	identityv1 "github.com/mavericks-engine/mavericks/gen/go/identity/v1"
)

const sessionTTL = 15 * time.Minute

type sessionPayload struct {
	UserID string        `json:"user_id"`
	Email  string        `json:"email"`
	Role   commonv1.Role `json:"role"`
}

// SessionStore caches validated actors in Redis so repeated token validation
// hits Redis instead of going to JWKS + DB on every request.
// It also supports revocation: deleting all sessions for a user forces the next
// request to re-validate against JWKS and re-fetch from DB.
type SessionStore struct {
	rdb *redis.Client
}

func NewSessionStore(rdb *redis.Client) *SessionStore {
	return &SessionStore{rdb: rdb}
}

// Get returns the cached actor for a raw JWT token string, or (nil, false) on miss.
func (s *SessionStore) Get(ctx context.Context, rawToken string) (*identityv1.ValidateTokenResponse, bool) {
	key := s.tokenKey(rawToken)
	val, err := s.rdb.Get(ctx, key).Result()
	if err != nil {
		return nil, false
	}
	var p sessionPayload
	if err := json.Unmarshal([]byte(val), &p); err != nil {
		return nil, false
	}
	return &identityv1.ValidateTokenResponse{
		Valid: true,
		Actor: &commonv1.Actor{UserId: p.UserID, Role: p.Role},
	}, true
}

// Set caches a successful validation result, TTL-bounded so stale roles
// are eventually evicted even without explicit revocation.
func (s *SessionStore) Set(ctx context.Context, rawToken string, user *identityv1.User) {
	key := s.tokenKey(rawToken)
	userKey := s.userKey(user.Id)

	p := sessionPayload{UserID: user.Id, Email: user.Email, Role: user.Role}
	b, _ := json.Marshal(p)

	pipe := s.rdb.Pipeline()
	pipe.Set(ctx, key, b, sessionTTL)
	// Track token key under the user set so RevokeAll can delete them
	pipe.SAdd(ctx, userKey, key)
	pipe.Expire(ctx, userKey, sessionTTL+time.Minute)
	_, _ = pipe.Exec(ctx)
}

// RevokeAll deletes all cached sessions for a user, forcing re-validation.
// Called when a role is changed or a user is suspended.
func (s *SessionStore) RevokeAll(ctx context.Context, userID string) error {
	userKey := s.userKey(userID)
	keys, err := s.rdb.SMembers(ctx, userKey).Result()
	if err != nil {
		return err
	}
	if len(keys) > 0 {
		if err := s.rdb.Del(ctx, keys...).Err(); err != nil {
			return err
		}
	}
	return s.rdb.Del(ctx, userKey).Err()
}

func (s *SessionStore) tokenKey(rawToken string) string {
	h := sha256.Sum256([]byte(rawToken))
	return fmt.Sprintf("session:token:%x", h)
}

func (s *SessionStore) userKey(userID string) string {
	return fmt.Sprintf("session:user:%s", userID)
}

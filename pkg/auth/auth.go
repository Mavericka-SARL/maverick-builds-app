package auth

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc/metadata"

	commonv1 "github.com/mavericks-engine/mavericks/gen/go/common/v1"
)

type contextKey struct{}

var ErrMissingToken = errors.New("missing authorization token")
var ErrInvalidToken = errors.New("invalid authorization token")

// ActorFromContext retrieves the authenticated Actor injected by gateway middleware.
func ActorFromContext(ctx context.Context) (*commonv1.Actor, error) {
	actor, ok := ctx.Value(contextKey{}).(*commonv1.Actor)
	if !ok || actor == nil {
		return nil, ErrMissingToken
	}
	return actor, nil
}

// WithActor injects an Actor into a context (used by gateway after token validation).
func WithActor(ctx context.Context, actor *commonv1.Actor) context.Context {
	return context.WithValue(ctx, contextKey{}, actor)
}

// BearerToken extracts the Bearer token from gRPC incoming metadata.
func BearerToken(ctx context.Context) (string, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", ErrMissingToken
	}
	vals := md.Get("authorization")
	if len(vals) == 0 {
		return "", ErrMissingToken
	}
	token := strings.TrimPrefix(vals[0], "Bearer ")
	if token == "" {
		return "", ErrInvalidToken
	}
	return token, nil
}

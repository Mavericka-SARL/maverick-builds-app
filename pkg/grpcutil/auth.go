package grpcutil

import (
	"context"
	"strings"

	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	commonv1 "github.com/mavericks-engine/mavericks/gen/go/common/v1"
	"github.com/mavericks-engine/mavericks/internal/identity"
	"github.com/mavericks-engine/mavericks/pkg/auth"
)

// ActorResolver resolves a keycloak_sub to a commonv1.Actor — normally
// auth.ResolveActorByKeycloakSub bound to a real pool (see WithAuth's
// callers in each cmd/<service>/main.go), or a fake in tests, matching
// this package's existing fakeAuditClient pattern in audit_test.go.
type ActorResolver func(ctx context.Context, sub string) (*commonv1.Actor, error)

// exemptFromAuth matches RPCs that skip identity checks in every mode:
// the k8s grpc-health-probe liveness/readiness check sends no
// credentials at all, and grpcurl's reflection-based introspection (used
// throughout this codebase's own manual verification) would otherwise
// also start requiring a token.
func exemptFromAuth(fullMethod string) bool {
	return strings.HasPrefix(fullMethod, "/grpc.health.v1.Health/") ||
		strings.HasPrefix(fullMethod, "/grpc.reflection.")
}

// WithAuth returns the ServerOption that installs AuthInterceptor.
func WithAuth(resolve ActorResolver, jwks *identity.JWKSValidator, devMode bool, log zerolog.Logger) []grpc.ServerOption {
	return []grpc.ServerOption{grpc.ChainUnaryInterceptor(AuthInterceptor(resolve, jwks, devMode, log))}
}

// AuthInterceptor resolves the calling actor and injects it via
// auth.WithActor, so both the handler and (chained after this one)
// AuditInterceptor's existing auth.ActorFromContext(ctx) call finally get
// something real instead of always finding nothing.
//
// Dev mode: reads "x-dev-user" from incoming gRPC metadata (a
// keycloak_sub) and resolves it. A missing header means an anonymous
// call — a soft fallback, not a failure, so dev tooling that hasn't been
// updated to send one keeps working exactly as before.
//
// Production: requires and validates a real Bearer token via
// auth.BearerToken + jwks.Validate; a missing/invalid token or a nil jwks
// (misconfigured service) is a hard codes.Unauthenticated failure — there
// is no soft fallback in prod, mirroring internal/gateway's own
// resolveActor dev-vs-prod asymmetry.
func AuthInterceptor(resolve ActorResolver, jwks *identity.JWKSValidator, devMode bool, log zerolog.Logger) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if exemptFromAuth(info.FullMethod) {
			return handler(ctx, req)
		}

		if devMode {
			if md, ok := metadata.FromIncomingContext(ctx); ok {
				if vals := md.Get("x-dev-user"); len(vals) > 0 && vals[0] != "" {
					a, err := resolve(ctx, vals[0])
					if err != nil {
						log.Warn().Err(err).Str("method", info.FullMethod).Msg("dev-mode actor resolution failed")
					} else {
						ctx = auth.WithActor(ctx, a)
					}
				}
			}
			return handler(ctx, req)
		}

		if jwks == nil {
			return nil, status.Error(codes.Unauthenticated, "authentication not configured")
		}
		token, err := auth.BearerToken(ctx)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, err.Error())
		}
		claims, err := jwks.Validate(token)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "invalid token")
		}
		a, err := resolve(ctx, claims.Subject)
		if err != nil {
			return nil, status.Error(codes.Unauthenticated, "unknown actor")
		}

		return handler(auth.WithActor(ctx, a), req)
	}
}

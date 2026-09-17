package grpcutil

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/MicahParks/jwkset"
	"github.com/golang-jwt/jwt/v5"
	"github.com/rs/zerolog"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	commonv1 "github.com/mavericks-engine/mavericks/gen/go/common/v1"
	"github.com/mavericks-engine/mavericks/internal/identity"
	"github.com/mavericks-engine/mavericks/pkg/auth"
)

// fakeResolver resolves exactly the subs pre-loaded into it — no real DB
// needed, matching this package's existing fakeAuditClient pattern.
func fakeResolver(subToUserID map[string]string) ActorResolver {
	return func(_ context.Context, sub string) (*commonv1.Actor, error) {
		userID, ok := subToUserID[sub]
		if !ok {
			return nil, status.Error(codes.NotFound, "unknown sub")
		}
		return &commonv1.Actor{UserId: userID}, nil
	}
}

// actorCapturingHandler returns a grpc.UnaryHandler that records whatever
// actor (if any) auth.ActorFromContext finds, so tests can assert on what
// AuthInterceptor injected.
func actorCapturingHandler(got *[]*commonv1.Actor) grpc.UnaryHandler {
	return func(ctx context.Context, _ any) (any, error) {
		a, err := auth.ActorFromContext(ctx)
		if err == nil {
			*got = append(*got, a)
		} else {
			*got = append(*got, nil)
		}
		return "ok", nil
	}
}

const authTestRealm = "test-realm"

// jwksFixture serves a real JWK Set — backed by a freshly generated RSA
// key — at the path shape identity.NewJWKSValidator expects, mirroring
// internal/gateway/jwt_auth_test.go's fixture so AuthInterceptor's
// production path is exercised without a running Keycloak.
type jwksFixture struct {
	srv        *httptest.Server
	privateKey *rsa.PrivateKey
	issuer     string
}

func setupJWKSFixture(t *testing.T) *jwksFixture {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}

	jwk, err := jwkset.NewJWKFromKey(privateKey, jwkset.JWKOptions{
		Metadata: jwkset.JWKMetadataOptions{KID: "test-kid", ALG: jwkset.AlgRS256, USE: jwkset.UseSig},
	})
	if err != nil {
		t.Fatalf("build jwk: %v", err)
	}
	store := jwkset.NewMemoryStorage()
	if err := store.KeyWrite(context.Background(), jwk); err != nil {
		t.Fatalf("write jwk to store: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/realms/"+authTestRealm+"/protocol/openid-connect/certs", func(w http.ResponseWriter, r *http.Request) {
		body, err := store.JSONPublic(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})

	f := &jwksFixture{privateKey: privateKey, srv: httptest.NewServer(mux)}
	f.issuer = f.srv.URL + "/realms/" + authTestRealm
	t.Cleanup(f.srv.Close)
	return f
}

func (f *jwksFixture) sign(t *testing.T, sub string, expiresAt time.Time) string {
	t.Helper()
	claims := identity.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   sub,
			Issuer:    f.issuer,
			IssuedAt:  jwt.NewNumericDate(time.Now().Add(-time.Minute)),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "test-kid"
	signed, err := token.SignedString(f.privateKey)
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signed
}

func TestAuthInterceptor_DevMode(t *testing.T) {
	resolve := fakeResolver(map[string]string{"dev-sub-1": "user-1"})
	interceptor := AuthInterceptor(resolve, nil, true, zerolog.Nop())
	info := &grpc.UnaryServerInfo{FullMethod: "/tenant.v1.TenantService/CreateCustomer"}

	t.Run("x-dev-user metadata resolves the actor", func(t *testing.T) {
		var got []*commonv1.Actor
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("x-dev-user", "dev-sub-1"))
		if _, err := interceptor(ctx, "req", info, actorCapturingHandler(&got)); err != nil {
			t.Fatalf("interceptor: %v", err)
		}
		if len(got) != 1 || got[0] == nil || got[0].UserId != "user-1" {
			t.Fatalf("captured actor = %+v, want UserId=user-1", got)
		}
	})

	t.Run("no metadata is a soft fallback, not a failure", func(t *testing.T) {
		var got []*commonv1.Actor
		if _, err := interceptor(context.Background(), "req", info, actorCapturingHandler(&got)); err != nil {
			t.Fatalf("interceptor: %v", err)
		}
		if len(got) != 1 || got[0] != nil {
			t.Fatalf("captured actor = %+v, want [nil] (handler still runs, no actor)", got)
		}
	})
}

func TestAuthInterceptor_ProdMode(t *testing.T) {
	jf := setupJWKSFixture(t)
	// Empty public issuer: the fixture is reached directly, so the JWKS fetch
	// URL and the token issuer are the same. See internal/identity/jwks_test.go
	// for the reverse-proxy case where they differ.
	jwks, err := identity.NewJWKSValidator(context.Background(), jf.srv.URL, authTestRealm, "")
	if err != nil {
		t.Fatalf("new jwks validator: %v", err)
	}
	resolve := fakeResolver(map[string]string{"prod-sub-1": "user-2"})
	interceptor := AuthInterceptor(resolve, jwks, false, zerolog.Nop())
	info := &grpc.UnaryServerInfo{FullMethod: "/tenant.v1.TenantService/CreateCustomer"}
	noopHandler := func(ctx context.Context, _ any) (any, error) { return "ok", nil }

	t.Run("valid token resolves the actor via the fake resolver", func(t *testing.T) {
		token := jf.sign(t, "prod-sub-1", time.Now().Add(time.Hour))
		var got []*commonv1.Actor
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+token))
		if _, err := interceptor(ctx, "req", info, actorCapturingHandler(&got)); err != nil {
			t.Fatalf("interceptor: %v", err)
		}
		if len(got) != 1 || got[0] == nil || got[0].UserId != "user-2" {
			t.Fatalf("captured actor = %+v, want UserId=user-2", got)
		}
	})

	t.Run("missing token is rejected", func(t *testing.T) {
		_, err := interceptor(context.Background(), "req", info, noopHandler)
		if status.Code(err) != codes.Unauthenticated {
			t.Errorf("code = %v, want Unauthenticated", status.Code(err))
		}
	})

	t.Run("invalid token is rejected", func(t *testing.T) {
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer not-a-real-jwt"))
		_, err := interceptor(ctx, "req", info, noopHandler)
		if status.Code(err) != codes.Unauthenticated {
			t.Errorf("code = %v, want Unauthenticated", status.Code(err))
		}
	})

	t.Run("nil jwks (misconfigured service) rejects every request", func(t *testing.T) {
		misconfigured := AuthInterceptor(resolve, nil, false, zerolog.Nop())
		token := jf.sign(t, "prod-sub-1", time.Now().Add(time.Hour))
		ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+token))
		_, err := misconfigured(ctx, "req", info, noopHandler)
		if status.Code(err) != codes.Unauthenticated {
			t.Errorf("code = %v, want Unauthenticated", status.Code(err))
		}
	})
}

func TestAuthInterceptor_ExemptMethodsBypassAuth(t *testing.T) {
	// Production mode, nil jwks (would hard-fail any non-exempt call) — a
	// health check or reflection call must still succeed, since a real
	// grpc-health-probe/grpcurl never sends credentials.
	interceptor := AuthInterceptor(fakeResolver(nil), nil, false, zerolog.Nop())
	handler := func(ctx context.Context, _ any) (any, error) { return "ok", nil }

	for _, method := range []string{
		"/grpc.health.v1.Health/Check",
		"/grpc.reflection.v1alpha.ServerReflection/ServerReflectionInfo",
	} {
		t.Run(method, func(t *testing.T) {
			info := &grpc.UnaryServerInfo{FullMethod: method}
			if _, err := interceptor(context.Background(), "req", info, handler); err != nil {
				t.Errorf("interceptor: %v, want no error (exempt method)", err)
			}
		})
	}
}

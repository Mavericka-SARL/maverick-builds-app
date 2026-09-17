package main

import (
	"context"
	"os"

	"github.com/redis/go-redis/v9"

	auditv1 "github.com/mavericks-engine/mavericks/gen/go/audit/v1"
	commonv1 "github.com/mavericks-engine/mavericks/gen/go/common/v1"
	identityv1 "github.com/mavericks-engine/mavericks/gen/go/identity/v1"
	"github.com/mavericks-engine/mavericks/internal/identity"
	"github.com/mavericks-engine/mavericks/pkg/auth"
	"github.com/mavericks-engine/mavericks/pkg/clients"
	"github.com/mavericks-engine/mavericks/pkg/config"
	"github.com/mavericks-engine/mavericks/pkg/db"
	"github.com/mavericks-engine/mavericks/pkg/grpcutil"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

type cfg struct {
	config.BaseConfig `mapstructure:",squash"`
	AuditAddr         string `mapstructure:"AUDIT_ADDR"`
}

var auditPolicy = grpcutil.AuditPolicy{
	"AssignRole":   auditv1.EventCategory_EVENT_CATEGORY_ADMIN,
	"RevokeRole":   auditv1.EventCategory_EVENT_CATEGORY_ADMIN,
	"CreateAPIKey": auditv1.EventCategory_EVENT_CATEGORY_ADMIN,
	"RevokeAPIKey": auditv1.EventCategory_EVENT_CATEGORY_ADMIN,
}

var deliberatelyUnaudited = []string{"GetUser", "ListUsers", "ValidateToken"}

func main() {
	log := logger.New("identity")
	ctx := context.Background()

	var c cfg
	c.KeycloakURL = "http://localhost:8180"
	c.KeycloakRealm = "mavericks"
	c.AuditAddr = "audit:9090"
	if err := config.Load(&c); err != nil {
		log.Fatal().Err(err).Msg("failed to load config")
		os.Exit(1)
	}

	pool, err := db.Connect(ctx, c.DatabaseURL)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to connect to database")
		os.Exit(1)
	}
	defer pool.Close()

	// Unconditional (unlike the other 10 services' AuthInterceptor-only
	// jwks): identity.NewServer's own ValidateToken RPC needs a real
	// validator regardless of DEV_MODE. AuthInterceptor below reuses this
	// same instance for its production-mode path; in dev mode it simply
	// goes unused by the interceptor (x-dev-user metadata is used
	// instead), which is harmless.
	jwks, err := identity.NewJWKSValidator(ctx, c.KeycloakURL, c.KeycloakRealm, c.KeycloakIssuer)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to initialize JWKS validator")
		os.Exit(1)
	}

	rdb, err := redis.ParseURL(c.RedisURL)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to parse Redis URL")
		os.Exit(1)
	}
	redisClient := redis.NewClient(rdb)

	auditClient, auditConn, err := clients.NewAuditClient(c.AuditAddr)
	if err != nil {
		log.Warn().Err(err).Msg("audit service unavailable; RPC mutations will not be recorded")
	}
	if auditConn != nil {
		defer func() { _ = auditConn.Close() }()
	}

	store := identity.NewStore(pool)
	sessions := identity.NewSessionStore(redisClient)
	devMode := os.Getenv("DEV_MODE") == "true"
	resolveActor := func(ctx context.Context, sub string) (*commonv1.Actor, error) {
		return auth.ResolveActorByKeycloakSub(ctx, pool, sub)
	}

	srv := grpcutil.NewServer(append(
		grpcutil.WithAuth(resolveActor, jwks, devMode, log),
		grpcutil.WithAudit(auditClient, auditPolicy, log)...,
	)...)
	identityv1.RegisterIdentityServiceServer(srv,
		identity.NewServer(log, store, jwks).WithSessionStore(sessions))

	if err := grpcutil.Serve(ctx, c.GRPCPort, srv, log); err != nil {
		log.Fatal().Err(err).Msg("server exited with error")
	}
}

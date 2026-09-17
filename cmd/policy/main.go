package main

import (
	"context"
	"os"

	"github.com/redis/go-redis/v9"

	auditv1 "github.com/mavericks-engine/mavericks/gen/go/audit/v1"
	commonv1 "github.com/mavericks-engine/mavericks/gen/go/common/v1"
	policyv1 "github.com/mavericks-engine/mavericks/gen/go/policy/v1"
	"github.com/mavericks-engine/mavericks/internal/identity"
	"github.com/mavericks-engine/mavericks/internal/policy"
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
	"UpsertRACIRule":        auditv1.EventCategory_EVENT_CATEGORY_POLICY_CHANGE,
	"UpsertDimensionPolicy": auditv1.EventCategory_EVENT_CATEGORY_POLICY_CHANGE,
	"UpsertMetricPolicy":    auditv1.EventCategory_EVENT_CATEGORY_POLICY_CHANGE,
	"UpsertAttributeRule":   auditv1.EventCategory_EVENT_CATEGORY_POLICY_CHANGE,
	"UpsertCellPolicy":      auditv1.EventCategory_EVENT_CATEGORY_POLICY_CHANGE,
}

// InvalidatePolicyCache only invalidates a Redis cache entry — no persisted
// business data changes, so it's deliberately not audited (unlike the
// Upsert* RPCs above, which change the policy rules themselves).
var deliberatelyUnaudited = []string{
	"CheckPermission", "CheckRACIPermission", "GetEffectivePermissions",
	"CheckCellPermission", "InvalidatePolicyCache",
}

func main() {
	log := logger.New("policy")
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

	opt, err := redis.ParseURL(c.RedisURL)
	if err != nil {
		log.Fatal().Err(err).Msg("invalid redis url")
		os.Exit(1)
	}
	rdb := redis.NewClient(opt)
	defer func() { _ = rdb.Close() }()

	auditClient, auditConn, err := clients.NewAuditClient(c.AuditAddr)
	if err != nil {
		log.Warn().Err(err).Msg("audit service unavailable; RPC mutations will not be recorded")
	}
	if auditConn != nil {
		defer func() { _ = auditConn.Close() }()
	}

	store := policy.NewStore(pool)
	cache := policy.NewCache(rdb)
	devMode := os.Getenv("DEV_MODE") == "true"
	var jwks *identity.JWKSValidator
	if devMode {
		log.Warn().Msg("DEV_MODE=true: gRPC JWKS validation disabled, x-dev-user metadata is active")
	} else {
		jwks, err = identity.NewJWKSValidator(ctx, c.KeycloakURL, c.KeycloakRealm, c.KeycloakIssuer)
		if err != nil {
			log.Fatal().Err(err).Msg("failed to initialize JWKS validator")
			os.Exit(1)
		}
	}
	resolveActor := func(ctx context.Context, sub string) (*commonv1.Actor, error) {
		return auth.ResolveActorByKeycloakSub(ctx, pool, sub)
	}

	srv := grpcutil.NewServer(append(
		grpcutil.WithAuth(resolveActor, jwks, devMode, log),
		grpcutil.WithAudit(auditClient, auditPolicy, log)...,
	)...)
	policyv1.RegisterPolicyServiceServer(srv, policy.NewServer(log, store, cache))

	if err := grpcutil.Serve(ctx, c.GRPCPort, srv, log); err != nil {
		log.Fatal().Err(err).Msg("server exited with error")
	}
}

package main

import (
	"context"
	"os"

	"github.com/nats-io/nats.go"

	auditv1 "github.com/mavericks-engine/mavericks/gen/go/audit/v1"
	commonv1 "github.com/mavericks-engine/mavericks/gen/go/common/v1"
	queryv1 "github.com/mavericks-engine/mavericks/gen/go/query/v1"
	"github.com/mavericks-engine/mavericks/internal/identity"
	"github.com/mavericks-engine/mavericks/internal/query"
	"github.com/mavericks-engine/mavericks/pkg/auth"
	"github.com/mavericks-engine/mavericks/pkg/clients"
	"github.com/mavericks-engine/mavericks/pkg/config"
	"github.com/mavericks-engine/mavericks/pkg/db"
	"github.com/mavericks-engine/mavericks/pkg/grpcutil"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

type cfg struct {
	config.BaseConfig `mapstructure:",squash"`
	PolicyAddr        string `mapstructure:"POLICY_ADDR"`
	AuditAddr         string `mapstructure:"AUDIT_ADDR"`
}

var auditPolicy = grpcutil.AuditPolicy{
	"Writeback": auditv1.EventCategory_EVENT_CATEGORY_DATA_CHANGE,
}

var deliberatelyUnaudited = []string{"Query", "GetCell"}

func main() {
	log := logger.New("query")
	ctx := context.Background()

	var c cfg
	c.KeycloakURL = "http://localhost:8180"
	c.KeycloakRealm = "mavericks"
	c.PolicyAddr = "policy:9090"
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

	nc, err := nats.Connect(c.NATSUrl)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to connect to NATS")
		os.Exit(1)
	}
	defer nc.Close()

	pub, err := query.NewPublisher(nc)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to create NATS publisher")
		os.Exit(1)
	}

	policyClient, policyConn, err := clients.NewPolicyClient(c.PolicyAddr)
	if err != nil {
		log.Warn().Err(err).Msg("policy service unavailable; permission checks will be skipped")
	}
	if policyConn != nil {
		defer func() { _ = policyConn.Close() }()
	}

	auditClient, auditConn, err := clients.NewAuditClient(c.AuditAddr)
	if err != nil {
		log.Warn().Err(err).Msg("audit service unavailable; RPC mutations will not be recorded")
	}
	if auditConn != nil {
		defer func() { _ = auditConn.Close() }()
	}

	store := query.NewStore(pool)
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
	queryv1.RegisterQueryServiceServer(srv, query.NewServer(log, store, pub, policyClient))

	if err := grpcutil.Serve(ctx, c.GRPCPort, srv, log); err != nil {
		log.Fatal().Err(err).Msg("server exited with error")
	}
}

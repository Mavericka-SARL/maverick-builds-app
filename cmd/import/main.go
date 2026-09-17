package main

import (
	"context"
	"os"

	"github.com/nats-io/nats.go"

	auditv1 "github.com/mavericks-engine/mavericks/gen/go/audit/v1"
	commonv1 "github.com/mavericks-engine/mavericks/gen/go/common/v1"
	importpkgv1 "github.com/mavericks-engine/mavericks/gen/go/importpkg/v1"
	"github.com/mavericks-engine/mavericks/internal/identity"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
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
	"CreateImportJob": auditv1.EventCategory_EVENT_CATEGORY_DATA_CHANGE,
	"CommitImport":    auditv1.EventCategory_EVENT_CATEGORY_DATA_CHANGE,
	"AbortImport":     auditv1.EventCategory_EVENT_CATEGORY_DATA_CHANGE,
}

var deliberatelyUnaudited = []string{
	"ValidateImport", "GetImportJob", "ListImportJobs", "GetImportErrors",
}

func main() {
	log := logger.New("import")
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

	var pub *importpkg.Publisher
	nc, err := nats.Connect(c.NATSUrl)
	if err != nil {
		log.Warn().Err(err).Msg("NATS unavailable; facts.committed events will not be published")
	} else {
		defer nc.Close()
		if p, err := importpkg.NewPublisher(nc); err != nil {
			log.Warn().Err(err).Msg("could not create NATS publisher")
		} else {
			pub = p
		}
	}

	auditClient, auditConn, err := clients.NewAuditClient(c.AuditAddr)
	if err != nil {
		log.Warn().Err(err).Msg("audit service unavailable; RPC mutations will not be recorded")
	}
	if auditConn != nil {
		defer func() { _ = auditConn.Close() }()
	}

	store := importpkg.NewStore(pool)
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
	importpkgv1.RegisterImportServiceServer(srv, importpkg.NewServer(log, store, pub))

	if err := grpcutil.Serve(ctx, c.GRPCPort, srv, log); err != nil {
		log.Fatal().Err(err).Msg("server exited with error")
	}
}

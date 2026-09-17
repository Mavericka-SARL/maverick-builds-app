package main

import (
	"context"
	"os"

	auditv1 "github.com/mavericks-engine/mavericks/gen/go/audit/v1"
	commonv1 "github.com/mavericks-engine/mavericks/gen/go/common/v1"
	workflowv1 "github.com/mavericks-engine/mavericks/gen/go/workflow/v1"
	"github.com/mavericks-engine/mavericks/internal/identity"
	"github.com/mavericks-engine/mavericks/internal/workflow"
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

// auditPolicy deliberately does NOT use one category for the whole
// service (unlike every other cmd/*/main.go): CreateWorkflowDef is a real
// model/schema change (MODEL_CHANGE, matching model/schema-migration's
// equivalent Create* RPCs), but StartWorkflow/CompleteStep are workflow
// *execution* — matching pkg/auditlog's own HTTP-side categorization of
// the equivalent actions (EventWorkflowInstanceStarted/EventTaskCompleted,
// both CategoryDataChange). Do not "simplify" this back to one constant.
var auditPolicy = grpcutil.AuditPolicy{
	"CreateWorkflowDef": auditv1.EventCategory_EVENT_CATEGORY_MODEL_CHANGE,
	"StartWorkflow":     auditv1.EventCategory_EVENT_CATEGORY_DATA_CHANGE,
	"CompleteStep":      auditv1.EventCategory_EVENT_CATEGORY_DATA_CHANGE,
}

var deliberatelyUnaudited = []string{
	"GetWorkflowDef", "GetWorkflowInstance", "ListWorkflowInstances", "GetPendingTasks",
}

func main() {
	log := logger.New("workflow")
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

	auditClient, auditConn, err := clients.NewAuditClient(c.AuditAddr)
	if err != nil {
		log.Warn().Err(err).Msg("audit service unavailable; RPC mutations will not be recorded")
	}
	if auditConn != nil {
		defer func() { _ = auditConn.Close() }()
	}

	store := workflow.NewStore(pool)
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
	workflowv1.RegisterWorkflowServiceServer(srv, workflow.NewServer(log, store))

	if err := grpcutil.Serve(ctx, c.GRPCPort, srv, log); err != nil {
		log.Fatal().Err(err).Msg("server exited with error")
	}
}

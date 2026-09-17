package main

import (
	"context"
	"os"
	"time"

	auditv1 "github.com/mavericks-engine/mavericks/gen/go/audit/v1"
	"github.com/mavericks-engine/mavericks/internal/audit"
	"github.com/mavericks-engine/mavericks/pkg/config"
	"github.com/mavericks-engine/mavericks/pkg/db"
	"github.com/mavericks-engine/mavericks/pkg/grpcutil"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

type cfg struct {
	config.BaseConfig `mapstructure:",squash"`
}

func main() {
	log := logger.New("audit")
	ctx := context.Background()

	var c cfg
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

	store := audit.NewStore(pool)

	// Ensure partition exists for the current month on startup
	if err := store.EnsurePartition(ctx, time.Now()); err != nil {
		log.Warn().Err(err).Msg("partition creation on startup failed; default partition will be used")
	}

	srv := grpcutil.NewServer()
	auditv1.RegisterAuditServiceServer(srv, audit.NewServer(log, store))

	if err := grpcutil.Serve(ctx, c.GRPCPort, srv, log); err != nil {
		log.Fatal().Err(err).Msg("server exited with error")
	}
}

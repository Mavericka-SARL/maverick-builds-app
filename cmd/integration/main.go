// The integration worker: claims REST-connector runs from the durable
// PostgreSQL queue (FOR UPDATE SKIP LOCKED) and executes them through
// internal/integration's SSRF-hardened client — the ONLY process that makes
// connector requests; the gateway only validates, persists and enqueues.
//
// It also owns the schedule ticker: due schedules enqueue runs (idempotent
// per tick via the one-run-per-(integration, scheduled_for) index, so
// replicas never double-fire).
//
// Its NetworkPolicy allows DNS + Postgres + external 443 ONLY; everything
// in-cluster beyond the queue is refused at the network layer AND by the
// in-process destination rules (defense in depth).
package main

import (
	"context"
	"github.com/mavericks-engine/mavericks/internal/workflow"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/mavericks-engine/mavericks/internal/integration"
	"github.com/mavericks-engine/mavericks/pkg/config"
	"github.com/mavericks-engine/mavericks/pkg/db"
	"github.com/mavericks-engine/mavericks/pkg/grpcutil"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

type cfg struct {
	config.BaseConfig `mapstructure:",squash"`
	// Mirrors the gateway: "dedicated" runs one worker per tenant database.
	TenantDBMode     string `mapstructure:"TENANT_DB_MODE"`
	TenantDBMaxConns int    `mapstructure:"TENANT_DB_MAX_CONNS"`
}

const (
	claimInterval = 2 * time.Second
	// How often to look for tenants created since start-up.
	tenantWatchInterval = 30 * time.Second
	schedInterval       = 15 * time.Second
	leaseFor            = 2 * time.Minute
	concurrency         = 4
)

func main() {
	log := logger.New("integration")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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

	var router *tenantdb.Router
	if strings.EqualFold(c.TenantDBMode, "dedicated") {
		maxConns := c.TenantDBMaxConns
		if maxConns <= 0 {
			maxConns = 5
		}
		router = tenantdb.New(pool, tenantdb.Config{MaxConnsPerTenant: int32(maxConns), Log: log}) //nolint:gosec // bounded by config
		defer router.Close()
		log.Info().Msg("dedicated tenant databases: one worker per tenant")
	}

	allowInsecure := os.Getenv("INTEGRATION_ALLOW_INSECURE") == "1" || os.Getenv("DEV_MODE") == "true"
	if allowInsecure {
		log.Warn().Msg("INTEGRATION_ALLOW_INSECURE: plain-http/loopback destinations permitted (dev only)")
	}

	srv := grpcutil.NewServer()
	go func() {
		if err := grpcutil.Serve(ctx, 9090, srv, log); err != nil {
			log.Error().Err(err).Msg("health server exited")
		}
	}()

	base := "integration-" + uuid.NewString()[:8]
	if router == nil {
		runWorker(ctx, log, pool, base, allowInsecure)
		return
	}
	// One worker per tenant database, including tenants created while this
	// process runs.
	var wg sync.WaitGroup
	router.Watch(ctx, tenantWatchInterval, func(t tenantdb.Tenant, tpool *pgxpool.Pool) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runWorker(ctx, log.With().Str("tenant", t.CustomerID).Logger(), tpool, base+"-"+t.CustomerID[:8], allowInsecure)
		}()
	})
	<-ctx.Done()
	wg.Wait()
}

func heartbeat(ctx context.Context, store *integration.Store, runID, workerID string, done <-chan struct{}) {
	t := time.NewTicker(leaseFor / 3)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		case <-t.C:
			_ = store.Heartbeat(ctx, runID, workerID, leaseFor)
		}
	}
}

// fireDue enqueues runs for due schedules. Overlap policy "skip" refuses to
// enqueue while a run is queued/running; the unique tick index deduplicates
// racing replicas.
func fireDue(ctx context.Context, log zerolog.Logger, store *integration.Store, now time.Time) {
	due, err := store.DueSchedules(ctx, now, 50)
	if err != nil {
		return
	}
	for i := range due {
		sc := &due[i]
		fireAt := now.Truncate(time.Minute)
		if sc.OverlapPolicy == "skip" && store.HasActiveRun(ctx, sc.IntegrationID) {
			_ = store.AdvanceSchedule(ctx, sc, now)
			continue
		}
		if runID, err := store.Enqueue(ctx, sc.IntegrationID, "schedule", sc.EnabledBy, false, &fireAt); err == nil {
			if runID != "" {
				log.Info().Str("integration", sc.IntegrationID).Str("run", runID).Msg("schedule fired")
			}
			_ = store.AdvanceSchedule(ctx, sc, now)
		} else {
			log.Warn().Err(err).Str("integration", sc.IntegrationID).Msg("schedule enqueue")
		}
	}
}

// runWorker drives one database: the schedule ticker plus a pool of claimers.
// It returns when ctx ends and every in-flight run has finished. With
// dedicated tenant databases one of these runs per tenant, each with its own
// worker id, so a run is only ever claimed by a worker connected to the
// database that holds it.
func runWorker(ctx context.Context, log zerolog.Logger, pool *pgxpool.Pool, workerID string, allowInsecure bool) {
	store := integration.NewStore(pool)
	runner := &integration.Runner{
		Store:         store,
		Log:           log,
		AllowInsecure: allowInsecure,
		Committer:     &integration.DBCommitter{Pool: pool, Log: log},
		// A finished run fires the application's integration_completed /
		// integration_failed automation rules (scoped to this integration or
		// to any). The trigger catalog had advertised these events without
		// anything dispatching them.
		OnFinished: func(ctx context.Context, run *integration.Run, res integration.RunResult) {
			var triggerType string
			switch res.Status {
			case "success", "partial":
				triggerType = "integration_completed"
			case "failed":
				triggerType = "integration_failed"
			default:
				return // cancelled: nothing to react to
			}
			var appID, revisionID string
			if err := pool.QueryRow(ctx, `
				SELECT m.application_id::text, COALESCE(d.revision_id::text, '')
				FROM model.integration_def d JOIN core.model m ON m.id = d.model_id
				WHERE d.id = $1::uuid
			`, run.IntegrationID).Scan(&appID, &revisionID); err != nil {
				log.Warn().Err(err).Str("integration", run.IntegrationID).Msg("resolve application for integration event")
				return
			}
			payload := map[string]string{
				"integration_id": run.IntegrationID,
				"run_id":         run.ID,
				"record_count":   strconv.Itoa(res.RecordsWritten),
				"status":         res.Status,
			}
			if res.Status == "failed" {
				payload["error_message"] = res.Message
				if payload["error_message"] == "" {
					payload["error_message"] = res.ErrorCode
				}
			}
			workflow.NewStore(pool).DispatchEventRules(context.WithoutCancel(ctx), appID, revisionID, triggerType, run.IntegrationID, run.RunBy, payload)
		},
	}

	// Health endpoint for the standard grpc-health-probe manifests.

	// Schedule ticker.
	go func() {
		t := time.NewTicker(schedInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				fireDue(ctx, log, store, now)
			}
		}
	}()

	// Claim workers.
	var wg sync.WaitGroup
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				run, err := store.Claim(ctx, workerID, leaseFor)
				if err != nil {
					log.Error().Err(err).Msg("claim")
				}
				if run == nil {
					select {
					case <-ctx.Done():
						return
					case <-time.After(claimInterval):
					}
					continue
				}
				log.Info().Str("run", run.ID).Str("integration", run.IntegrationID).
					Str("trigger", run.TriggerType).Bool("dry_run", run.DryRun).Msg("run claimed")
				runCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
				hbDone := make(chan struct{})
				go heartbeat(runCtx, store, run.ID, workerID, hbDone)
				runner.Execute(runCtx, run)
				close(hbDone)
				cancel()
			}
		}()
	}

	log.Info().Str("worker", workerID).Int("concurrency", concurrency).Msg("integration worker started")
	<-ctx.Done()
	wg.Wait()
}

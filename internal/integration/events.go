package integration

import (
	"context"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	"github.com/mavericks-engine/mavericks/internal/workflow"
)

// DispatchRunEvents is a Runner's OnFinished for pool's database: a
// finished run fires the application's integration_completed /
// integration_failed automation rules (scoped to this integration or to
// any). The trigger catalog had advertised these events without anything
// dispatching them. Both runners use it: the worker's and the gateway's
// model-link runner.
func DispatchRunEvents(pool *pgxpool.Pool, log zerolog.Logger) func(ctx context.Context, run *Run, res RunResult) {
	return func(ctx context.Context, run *Run, res RunResult) {
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
	}
}

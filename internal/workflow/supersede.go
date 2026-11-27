package workflow

import (
	"context"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/internal/writeguard"
)

// supersedeEarlierApprovals marks the approved runs of instanceID's workflow
// about the same scope, started before it, as superseded by it — once it is
// approved itself (migration 201, owner decision 2026-11-27). Their locks
// end as its own begins: a project's second budget revision is approved
// over the first without HQ reopening the first, which unlocked the budget
// for everyone in between. A test run supersedes nothing.
func (s *Store) supersedeEarlierApprovals(ctx context.Context, instanceID string) {
	var approved, testRun bool
	var name, app string
	var started time.Time
	var ctxJSON, schemaJSON []byte
	if err := s.db.QueryRow(ctx, `
		SELECT `+writeguard.ReopenableSQL+`, wi.test_run, wd.name, wd.application_id::text, wi.started_at,
		       COALESCE(wi.context, '{}'::jsonb), COALESCE(wd.context_schema, '[]'::jsonb)
		FROM workflow.workflow_instance wi
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		`+writeguard.LastApprovalJoinSQL+`
		WHERE wi.id = $1::uuid`, instanceID).Scan(&approved, &testRun, &name, &app, &started, &ctxJSON, &schemaJSON); err != nil || !approved || testRun {
		return
	}
	key := writeguard.ApprovalScopeKeyJSON(ctxJSON, schemaJSON)
	rows, err := s.db.Query(ctx, `
		SELECT wi.id::text, COALESCE(wi.context, '{}'::jsonb), COALESCE(wd.context_schema, '[]'::jsonb)
		FROM workflow.workflow_instance wi
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		`+writeguard.LastApprovalJoinSQL+`
		WHERE wd.application_id = $1::uuid AND lower(wd.name) = lower($2)
		  AND wi.id <> $3::uuid AND wi.started_at < $4 AND NOT wi.test_run
		  AND `+writeguard.ReopenableSQL, app, name, instanceID, started)
	if err != nil {
		s.log.Warn().Err(err).Str("instance", instanceID).Msg("finding the approvals it supersedes failed")
		return
	}
	var earlier []string
	for rows.Next() {
		var id string
		var c, sc []byte
		if err := rows.Scan(&id, &c, &sc); err != nil {
			rows.Close()
			return
		}
		if writeguard.ApprovalScopeKeyJSON(c, sc) == key {
			earlier = append(earlier, id)
		}
	}
	rows.Close()
	if len(earlier) == 0 {
		return
	}
	if _, err := s.db.Exec(ctx, `
		UPDATE workflow.workflow_instance SET superseded_by = $1::uuid
		WHERE id = ANY($2::uuid[]) AND superseded_by IS NULL`, instanceID, earlier); err != nil {
		s.log.Warn().Err(err).Str("instance", instanceID).Msg("superseding earlier approvals failed")
		return
	}
	s.log.Info().Str("instance", instanceID).Str("workflow", name).Str("superseded", strings.Join(earlier, ",")).Msg("earlier approvals of the same scope superseded")
}

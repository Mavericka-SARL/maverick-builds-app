package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/notification"
	"github.com/mavericks-engine/mavericks/internal/workflow/assignee"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
)

// ErrInstanceNotRunning refuses to cancel an instance that already ended.
var ErrInstanceNotRunning = errors.New("this workflow run has already ended")

// ErrInstanceNotReopenable refuses to reopen a run that is not finished and
// approved, and the status override of one that is.
var ErrInstanceNotReopenable = errors.New("only a finished, approved workflow run can be reopened")

// CancelledTemplate is the notification a cancelled run sends.
const CancelledTemplate = "workflow_instance_cancelled"

// CancelResult is what CancelInstance did.
type CancelResult struct {
	ApplicationID string
	WorkflowName  string
	// Notified is how many people were told: the open steps' assignees and
	// the person who started the run, the canceller left out.
	Notified int
}

// CancelInstance stops a running instance before it finishes (owner
// request, 2026-10-10: a developer or business admin stops a run started
// by mistake). In one transaction its open steps are withdrawn — "skipped",
// so they leave every inbox — and the instance and the automation
// execution that started it become "cancelled". Steps already decided keep
// their effect: values an approval posted stay posted. A cancelled instance
// locks nothing (writeguard.WorkflowLockReason).
//
// Afterwards the people the open steps were assigned to and the person who
// started the run are notified, unless it was a test run, which pages
// nobody; userID, the canceller, is not told of their own action. reason is
// optional and goes into the message.
func (s *Store) CancelInstance(ctx context.Context, instanceID, userID, reason string) (*CancelResult, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck

	res := &CancelResult{}
	var status, startedBy string
	var testRun bool
	if err := tx.QueryRow(ctx, `
		SELECT wi.status::text, COALESCE(wi.started_by::text, ''), wi.test_run,
		       wd.application_id::text, wd.name
		FROM workflow.workflow_instance wi
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		WHERE wi.id = $1::uuid
		FOR UPDATE OF wi
	`, instanceID).Scan(&status, &startedBy, &testRun, &res.ApplicationID, &res.WorkflowName); err != nil {
		return nil, err
	}
	if status != "running" {
		return nil, ErrInstanceNotRunning
	}

	// Who holds the open steps, read before they are withdrawn.
	recipients, err := stepRecipients(ctx, tx, instanceID, `ws.status IN ('pending', 'in_progress')`)
	if err != nil {
		return nil, err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE workflow.workflow_step SET status = 'skipped'
		WHERE instance_id = $1::uuid AND status IN ('pending', 'in_progress')
	`, instanceID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workflow.workflow_instance SET status = 'cancelled', completed_at = now()
		WHERE id = $1::uuid
	`, instanceID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workflow.execution SET status = 'cancelled', completed_at = now()
		WHERE instance_id = $1::uuid AND status = 'running'
	`, instanceID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	// The records the run was about go back to their creators as drafts.
	s.driveRecords(ctx, instanceID, runCancelled, userID)

	if testRun {
		return res, nil
	}
	if startedBy != "" {
		recipients[startedBy] = true
	}
	by := s.actorName(ctx, userID)
	message := fmt.Sprintf("%q was cancelled by %s. Its open steps were withdrawn; nothing more is asked of anyone.", res.WorkflowName, by)
	if r := strings.TrimSpace(reason); r != "" {
		message = fmt.Sprintf("%q was cancelled by %s: %s. Its open steps were withdrawn; nothing more is asked of anyone.", res.WorkflowName, by, r)
	}
	res.Notified = s.notifyRun(ctx, instanceID, userID, recipients, "Cancelled: "+res.WorkflowName, message, res.WorkflowName, reason)
	return res, nil
}

// ReopenInstance takes back a finished run's approval (owner decision,
// 2026-11-07): a figure locked by mistake could only be corrected by
// rebuilding the model. The run becomes "cancelled", which is what releases
// its lock (writeguard), and everything it did stays: its steps and their
// decisions, the values it posted, the records it approved. A run that is
// not finished and approved is refused. reason is required: the numbers it
// frees were agreed, so the audit log and the message say why.
func (s *Store) ReopenInstance(ctx context.Context, instanceID, userID, reason string) (*CancelResult, error) {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck

	res := &CancelResult{}
	var reopenable, testRun bool
	var startedBy string
	if err := tx.QueryRow(ctx, `
		SELECT `+writeguard.ReopenableSQL+`, COALESCE(wi.started_by::text, ''), wi.test_run,
		       wd.application_id::text, wd.name
		FROM workflow.workflow_instance wi
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		`+writeguard.LastApprovalJoinSQL+`
		WHERE wi.id = $1::uuid
		FOR UPDATE OF wi
	`, instanceID).Scan(&reopenable, &startedBy, &testRun, &res.ApplicationID, &res.WorkflowName); err != nil {
		return nil, err
	}
	if !reopenable {
		return nil, ErrInstanceNotReopenable
	}
	recipients, err := stepRecipients(ctx, tx, instanceID, `ws.decision IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow.workflow_instance SET status = 'cancelled' WHERE id = $1::uuid`, instanceID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE workflow.execution SET status = 'cancelled' WHERE instance_id = $1::uuid`, instanceID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	if testRun {
		return res, nil
	}
	if startedBy != "" {
		recipients[startedBy] = true
	}
	message := fmt.Sprintf("%q was reopened by %s: %s. What it approved can be changed again.", res.WorkflowName, s.actorName(ctx, userID), strings.TrimSpace(reason))
	res.Notified = s.notifyRun(ctx, instanceID, userID, recipients, "Reopened: "+res.WorkflowName, message, res.WorkflowName, reason)
	return res, nil
}

// Reopenable reports whether a run is finished and stands approved, so that
// only ReopenInstance may take it out of "completed".
func (s *Store) Reopenable(ctx context.Context, instanceID string) (bool, error) {
	var ok bool
	err := s.db.QueryRow(ctx, `
		SELECT `+writeguard.ReopenableSQL+`
		FROM workflow.workflow_instance wi
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		`+writeguard.LastApprovalJoinSQL+`
		WHERE wi.id = $1::uuid
	`, instanceID).Scan(&ok)
	return ok, err
}

// stepRecipients resolves who a run's steps matching where were assigned to:
// the named assignee, else everyone the step's roles assign it to (as the
// inbox and the reminders resolve them).
func stepRecipients(ctx context.Context, tx pgx.Tx, instanceID, where string) (map[string]bool, error) {
	recipients := map[string]bool{}
	rows, err := tx.Query(ctx, `
		SELECT DISTINCT u.id::text
		FROM workflow.workflow_step ws
		JOIN workflow.workflow_instance wi ON wi.id = ws.instance_id
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		LEFT JOIN LATERAL (
		    SELECT elem FROM jsonb_array_elements(COALESCE(wi.steps_snapshot, wd.steps)) AS elem
		    WHERE elem->>'id' = ws.step_def_id LIMIT 1
		) step_def ON TRUE
		JOIN identity.user u ON u.id = ws.assignee_user_id
		    OR (ws.assignee_user_id IS NULL
		        AND `+assignee.SQL("wd.application_id", "COALESCE(step_def.elem->'assignee_roles', '[]'::jsonb)", "u.id")+`)
		WHERE ws.instance_id = $1::uuid AND `+where, instanceID)
	if err != nil {
		return nil, fmt.Errorf("resolve assignees: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		recipients[id] = true
	}
	return recipients, rows.Err()
}

// actorName is how a run's message names the person who acted.
func (s *Store) actorName(ctx context.Context, userID string) string {
	var by string
	if err := s.db.QueryRow(ctx, `SELECT COALESCE(NULLIF(display_name, ''), email) FROM identity.user WHERE id = $1::uuid`, userID).Scan(&by); err != nil {
		s.log.Warn().Err(err).Str("user", userID).Msg("run notification: loading the actor's name failed")
	}
	if by == "" {
		return "an administrator"
	}
	return by
}

// notifyRun tells recipients what became of a run, the person who acted left
// out, and answers how many were told.
func (s *Store) notifyRun(ctx context.Context, instanceID, userID string, recipients map[string]bool, subject, message, workflowName, reason string) int {
	delete(recipients, userID)
	vars := map[string]string{
		"subject":       subject,
		"message":       message,
		"workflow_name": workflowName,
		"reason":        strings.TrimSpace(reason),
	}
	notified := 0
	notifStore := notification.NewStoreOn(s.db)
	for id := range recipients {
		if _, err := notifStore.Notify(ctx, id, CancelledTemplate, vars, "workflow_instance", instanceID); err != nil {
			if !errors.Is(err, notification.ErrRecipientDisabled) {
				s.log.Warn().Err(err).Str("user", id).Str("instance", instanceID).Msg("run notification failed")
			}
			continue
		}
		notified++
	}
	return notified
}

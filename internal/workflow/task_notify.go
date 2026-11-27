package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/internal/notification"
)

// TaskAssignedTemplate is the notification a person gets when a task or an
// approval of a run becomes theirs.
const TaskAssignedTemplate = "workflow_task_assigned"

// NotifyNewTasks tells the people each newly active task or approval of a
// run is assigned to that it is theirs — in the console, and by e-mail or
// webhook where their tenant has those on (notification.Store.Notify). The
// people are the step's named assignee, else everyone its roles assign it to,
// as the inbox and the reminders draw them. Each step is told once
// (workflow_step.task_notified_at, migration 191); a step set back to
// in_progress has the stamp cleared and is told again.
//
// actorUserID — who started the run or decided the step before — is left
// out: the console already shows them what they just caused. A test run
// tells no one, and neither does a step naming no role — it is open to
// everyone who reaches the application, and the reminders leave it alone
// for the same reason. A reworked step goes back to the person who did it
// (assignee_user_id), so only they are told. It answers how many people
// were told.
//
// Until 2026-11-20 a step that became someone's task notified no one: with
// e-mail on, people heard of a run when it was cancelled, never when it had
// work for them.
func (s *Store) NotifyNewTasks(ctx context.Context, instanceID, actorUserID string) int {
	// A step naming its person from the run's data is theirs before anyone
	// is told of it (named_assignee.go).
	s.AssignNamedPeople(ctx, instanceID)
	type newTask struct {
		id, name, kind, workflow string
		due                      *time.Time
	}
	rows, err := s.db.Query(ctx, `
		SELECT ws.id::text, COALESCE(NULLIF(step_def.elem->>'name', ''), 'Task'),
		       lower(COALESCE(step_def.elem->>'type', '')), wd.name, ws.due_at
		FROM workflow.workflow_step ws
		JOIN workflow.workflow_instance wi ON wi.id = ws.instance_id
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		LEFT JOIN LATERAL (
		    SELECT elem FROM jsonb_array_elements(COALESCE(wi.steps_snapshot, wd.steps)) AS elem
		    WHERE elem->>'id' = ws.step_def_id LIMIT 1
		) step_def ON TRUE
		WHERE ws.instance_id = $1::uuid
		  AND ws.status = 'in_progress' AND ws.task_notified_at IS NULL
		  AND wi.status = 'running' AND NOT COALESCE(wi.test_run, FALSE)
		  AND lower(COALESCE(step_def.elem->>'type', '')) IN ('task', 'approval', 'step_type_task', 'step_type_approval', '1', '2')
		  AND (ws.assignee_user_id IS NOT NULL
		       OR (jsonb_typeof(step_def.elem->'assignee_roles') = 'array' AND step_def.elem->'assignee_roles' <> '[]'::jsonb))
		ORDER BY ws.created_at`, instanceID)
	if err != nil {
		s.log.Warn().Err(err).Str("instance", instanceID).Msg("finding new tasks to announce failed")
		return 0
	}
	var tasks []newTask
	for rows.Next() {
		var t newTask
		if err := rows.Scan(&t.id, &t.name, &t.kind, &t.workflow, &t.due); err != nil {
			rows.Close()
			s.log.Warn().Err(err).Str("instance", instanceID).Msg("reading new tasks failed")
			return 0
		}
		tasks = append(tasks, t)
	}
	rows.Close()

	told := 0
	notifStore := notification.NewStoreOn(s.db)
	for _, t := range tasks {
		// Claimed first: two requests finishing at once tell people once.
		ct, err := s.db.Exec(ctx, `UPDATE workflow.workflow_step SET task_notified_at = now() WHERE id = $1::uuid AND task_notified_at IS NULL`, t.id)
		if err != nil || ct.RowsAffected() == 0 {
			continue
		}
		recipients, err := stepRecipients(ctx, s.db, instanceID, `ws.id = $2::uuid`, t.id)
		if err != nil {
			s.log.Warn().Err(err).Str("step", t.id).Msg("resolving a new task's assignees failed")
			continue
		}
		delete(recipients, actorUserID)
		if len(recipients) == 0 {
			continue
		}
		subject, message := taskAssignedText(t.kind, t.name, t.workflow, t.due)
		vars := map[string]string{
			"subject":       subject,
			"message":       message,
			"step_name":     t.name,
			"workflow_name": t.workflow,
		}
		if t.due != nil {
			vars["due_at"] = t.due.Format(time.RFC3339)
		}
		for userID := range recipients {
			if _, err := notifStore.Notify(ctx, userID, TaskAssignedTemplate, vars, "workflow_instance", instanceID); err != nil {
				if !errors.Is(err, notification.ErrRecipientDisabled) {
					s.log.Warn().Err(err).Str("user", userID).Str("step", t.id).Msg("announcing a new task failed")
				}
				continue
			}
			told++
		}
	}
	return told
}

// taskAssignedText is a new task's subject and message: an approval asks for
// a decision, a task for the work.
func taskAssignedText(kind, step, workflow string, due *time.Time) (subject, message string) {
	approval := kind == "approval" || kind == "step_type_approval" || kind == "2"
	if approval {
		subject = "Approval needed: " + step
		message = fmt.Sprintf("%q in %q is waiting for your decision.", step, workflow)
	} else {
		subject = "New task: " + step
		message = fmt.Sprintf("%q in %q is yours to do.", step, workflow)
	}
	if due != nil {
		message = strings.TrimSuffix(message, ".") + fmt.Sprintf(", due %s.", due.Format("2 January 2006 15:04"))
	}
	return subject, message + " It is waiting on the Workflow page."
}

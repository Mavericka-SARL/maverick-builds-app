package notification

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rs/zerolog"
)

// Task reminders.
//
// A workflow step carries a due time when its designer set SLA hours. Nothing
// used it beyond colouring the inbox, so a task simply sat there. The
// reminder loop notifies everyone who could act on a step once it reaches its
// due time (or the configured lead time before it), through whatever channels
// the tenant has turned on, and records that it did so — one reminder per
// step, never a drip every pass.

// ReminderTemplate is the template id of a task reminder, so a console can
// tell reminders apart from the notification steps a designer wrote.
const ReminderTemplate = "workflow_task_reminder"

// Reminder sends those reminders for one database.
type Reminder struct {
	Store *Store
	Log   zerolog.Logger
	// Batch caps one pass (default 100 steps).
	Batch int
}

// Run reminds on every tick until ctx ends.
func (r *Reminder) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if _, err := r.RunOnce(ctx); err != nil && ctx.Err() == nil {
			r.Log.Warn().Err(err).Msg("task reminder pass failed")
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type dueStep struct {
	StepID     string
	InstanceID string
	CustomerID string
	StepName   string
	Workflow   string
	DueAt      time.Time
}

// RunOnce reminds for every step that has come due, and returns how many
// steps were reminded about.
func (r *Reminder) RunOnce(ctx context.Context) (int, error) {
	// Reminders are a per-tenant setting (migration 091): steps come due by
	// their own tenant's lead time, so the query casts the widest net any
	// tenant asks for and each step is then judged by its tenant's settings.
	maxLead, err := r.Store.maxReminderLead(ctx)
	if err != nil {
		return 0, err
	}
	batch := r.Batch
	if batch <= 0 {
		batch = 100
	}
	steps, err := r.dueSteps(ctx, maxLead, batch)
	if err != nil {
		return 0, err
	}
	perTenant := map[string]Settings{}
	reminded := 0
	for _, st := range steps {
		settings, ok := perTenant[st.CustomerID]
		if !ok {
			settings, _, err = r.Store.Effective(ctx, st.CustomerID, DeploymentDefaults)
			if err != nil {
				return reminded, err
			}
			perTenant[st.CustomerID] = settings
		}
		if !settings.RemindersEnabled || st.DueAt.Add(-time.Duration(settings.ReminderLeadHours)*time.Hour).After(time.Now()) {
			continue
		}
		recipients, rErr := r.assignees(ctx, st.StepID)
		if rErr != nil {
			r.Log.Warn().Err(rErr).Str("step", st.StepID).Msg("resolving a task's assignees failed")
			continue
		}
		// A step nobody can act on still gets marked: re-asking every pass
		// would not find anyone either, and the workflow's own validation is
		// what catches an unassigned step.
		vars := map[string]string{
			"subject": fmt.Sprintf("Reminder: %s", st.StepName),
			"message": fmt.Sprintf("%q in %q is due %s and is still waiting for a decision.",
				st.StepName, st.Workflow, st.DueAt.Format("2 January 2006 15:04")),
			"step_name":     st.StepName,
			"workflow_name": st.Workflow,
			"due_at":        st.DueAt.Format(time.RFC3339),
		}
		for _, userID := range recipients {
			if _, nErr := r.Store.Notify(ctx, userID, ReminderTemplate, vars, "workflow_instance", st.InstanceID); nErr != nil {
				r.Log.Warn().Err(nErr).Str("step", st.StepID).Str("user", userID).Msg("sending a reminder failed")
			}
		}
		if mErr := r.markReminded(ctx, st.StepID); mErr != nil {
			r.Log.Warn().Err(mErr).Str("step", st.StepID).Msg("marking a step reminded failed")
			continue
		}
		reminded++
	}
	return reminded, nil
}

// dueSteps finds in-progress steps whose due time has arrived (minus the
// configured lead) and that have not been reminded about, skipping test runs,
// which must never page real people.
func (r *Reminder) dueSteps(ctx context.Context, leadHours int32, limit int) ([]dueStep, error) {
	rows, err := r.Store.pool.Query(ctx, `
		SELECT ws.id::text, wi.id::text, COALESCE(app.customer_id::text, ''),
		       COALESCE(step_def.elem->>'name', 'Task'),
		       wd.name, ws.due_at
		FROM workflow.workflow_step ws
		JOIN workflow.workflow_instance wi ON wi.id = ws.instance_id
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		LEFT JOIN core.application app ON app.id = wd.application_id
		LEFT JOIN LATERAL (
		    SELECT elem FROM jsonb_array_elements(COALESCE(wi.steps_snapshot, wd.steps)) AS elem
		    WHERE elem->>'id' = ws.step_def_id LIMIT 1
		) step_def ON TRUE
		WHERE ws.status = 'in_progress'
		  AND ws.reminded_at IS NULL
		  AND ws.due_at IS NOT NULL
		  AND ws.due_at - make_interval(hours => $1::int) <= now()
		  AND COALESCE(wi.test_run, FALSE) = FALSE
		ORDER BY ws.due_at
		LIMIT $2
	`, leadHours, limit)
	if err != nil {
		return nil, fmt.Errorf("find due tasks: %w", err)
	}
	defer rows.Close()
	var out []dueStep
	for rows.Next() {
		var st dueStep
		if err := rows.Scan(&st.StepID, &st.InstanceID, &st.CustomerID, &st.StepName, &st.Workflow, &st.DueAt); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// assignees lists the users who may act on a step: the same platform-role and
// named-business-role matching the inbox and IsAssigneeEligible use, so a
// reminder reaches exactly the people who see the task.
func (r *Reminder) assignees(ctx context.Context, stepID string) ([]string, error) {
	var rolesJSON []byte
	var appID string
	err := r.Store.pool.QueryRow(ctx, `
		SELECT COALESCE(step_def.elem->'assignee_roles', '[]'::jsonb), wd.application_id::text
		FROM workflow.workflow_step ws
		JOIN workflow.workflow_instance wi ON wi.id = ws.instance_id
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		LEFT JOIN LATERAL (
		    SELECT elem FROM jsonb_array_elements(COALESCE(wi.steps_snapshot, wd.steps)) AS elem
		    WHERE elem->>'id' = ws.step_def_id LIMIT 1
		) step_def ON TRUE
		WHERE ws.id = $1::uuid
	`, stepID).Scan(&rolesJSON, &appID)
	if err != nil {
		return nil, err
	}
	var roles []string
	_ = json.Unmarshal(rolesJSON, &roles)
	if len(roles) == 0 {
		return nil, nil
	}
	rows, err := r.Store.pool.Query(ctx, `
		SELECT DISTINCT u.id::text
		FROM identity.user u
		WHERE EXISTS (
		        SELECT 1 FROM identity.role_assignment ra
		        WHERE ra.user_id = u.id AND ra.role::text = ANY($1)
		    )
		   OR EXISTS (
		        SELECT 1
		        FROM identity.business_role_member brm
		        JOIN identity.business_role br ON br.id = brm.role_id
		        JOIN core.workspace bws ON bws.id = br.workspace_id
		        JOIN core.application app ON app.id = $2::uuid
		             AND (app.workspace_id = bws.id OR app.customer_id = bws.customer_id)
		        WHERE brm.user_id = u.id AND br.name = ANY($1)
		    )
	`, roles, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (r *Reminder) markReminded(ctx context.Context, stepID string) error {
	_, err := r.Store.pool.Exec(ctx,
		`UPDATE workflow.workflow_step SET reminded_at = now() WHERE id = $1::uuid`, stepID)
	return err
}

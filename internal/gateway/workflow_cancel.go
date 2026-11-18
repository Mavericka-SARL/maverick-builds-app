package gateway

// Cancelling a running workflow instance (owner request, 2026-10-10): a
// developer or a business admin stops a run started by mistake — from
// Developer › Triggers' execution log, or Business Admin › Workflow (its History). Both
// doors run workflow.Store.CancelInstance, so the instance, its open steps
// and the execution that started it end the same way, and the people the
// run involved are told.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/workflow"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// workflowInstanceCancel handles POST /api/workflow/instances/{id}/cancel,
// body {"reason"} (optional).
func (h *handler) workflowInstanceCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ctx := r.Context()
	instanceID := r.PathValue("id")
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
	}
	body.Reason = strings.TrimSpace(body.Reason)
	if len(body.Reason) > 500 {
		jsonErr(w, fmt.Errorf("the reason is limited to 500 characters"), http.StatusBadRequest)
		return
	}

	ok, err := h.workflowInstanceCancellable(ctx, act, instanceID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !ok {
		// The same answer as an id that does not exist, so it does not
		// confirm another tenant's instance.
		jsonErr(w, fmt.Errorf("workflow instance not found"), http.StatusNotFound)
		return
	}
	res, err := h.workflowStore(ctx).CancelInstance(ctx, instanceID, act.UserID, body.Reason)
	if errors.Is(err, workflow.ErrInstanceNotRunning) {
		jsonErr(w, err, http.StatusConflict)
		return
	}
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	h.auditInstanceCancelled(ctx, act, res, instanceID, body.Reason)
	jsonOK(w, map[string]any{"status": "cancelled", "notified": res.Notified})
}

// workflowInstanceReopen handles POST /api/workflow/instances/{id}/reopen,
// body {"reason"} (required): the same people who may cancel a run take back
// a finished run's approval, which releases what it locks.
func (h *handler) workflowInstanceReopen(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ctx := r.Context()
	instanceID := r.PathValue("id")
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
		return
	}
	body.Reason = strings.TrimSpace(body.Reason)
	if body.Reason == "" {
		jsonErr(w, fmt.Errorf("say why this run is reopened: what it approved was agreed"), http.StatusBadRequest)
		return
	}
	if len(body.Reason) > 500 {
		jsonErr(w, fmt.Errorf("the reason is limited to 500 characters"), http.StatusBadRequest)
		return
	}
	ok, err := h.workflowInstanceReopenable(ctx, act, instanceID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !ok {
		// Someone who uses the application is told who reopens; anyone else
		// gets the 404 an unknown run gets.
		var appID string
		_ = h.db.QueryRow(ctx, `SELECT wd.application_id::text FROM workflow.workflow_instance wi
			JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id WHERE wi.id::text = $1`, instanceID).Scan(&appID)
		if reach, _ := h.actorCanAccessApp(ctx, act, appID); appID != "" && reach {
			jsonErr(w, fmt.Errorf("forbidden: an approved run is reopened by the application's business admins and developers"), http.StatusForbidden)
			return
		}
		jsonErr(w, fmt.Errorf("workflow instance not found"), http.StatusNotFound)
		return
	}
	if roles, err := h.workflowStore(ctx).ReopenRoles(ctx, instanceID); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	} else if len(roles) > 0 {
		holds, err := h.workflowStore(ctx).HoldsReopenRole(ctx, instanceID, act.UserID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if !holds {
			jsonErr(w, fmt.Errorf("forbidden: this workflow is reopened by %s", strings.Join(roles, " or ")), http.StatusForbidden)
			return
		}
	}
	res, err := h.workflowStore(ctx).ReopenInstance(ctx, instanceID, act.UserID, body.Reason)
	if errors.Is(err, workflow.ErrInstanceNotReopenable) {
		jsonErr(w, err, http.StatusConflict)
		return
	}
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryDataChange, EventType: auditlog.EventWorkflowInstanceReopened,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ApplicationID: res.ApplicationID, ResourceType: "workflow_instance", ResourceID: instanceID,
		Metadata: map[string]string{"workflow": res.WorkflowName, "reason": body.Reason, "notified": fmt.Sprint(res.Notified)},
	})
	jsonOK(w, map[string]any{"status": "reopened", "notified": res.Notified})
}

// workflowInstanceReopenable reports whether act reaches instanceID to
// reopen it: as for a cancel (workflowInstanceCancellable), or — when its
// definition names who may reopen it (reopen_roles) — anyone who reaches its
// application; the handler then asks for one of those roles. A CFO who
// approved the release is no business admin and reopens it by holding the
// role (owner decision 2026-11-13).
func (h *handler) workflowInstanceReopenable(ctx context.Context, act *actor, instanceID string) (bool, error) {
	roles, err := h.workflowStore(ctx).ReopenRoles(ctx, instanceID)
	if err != nil {
		return false, err
	}
	if len(roles) == 0 {
		return h.workflowInstanceCancellable(ctx, act, instanceID)
	}
	var appID string
	var testRun bool
	err = h.db.QueryRow(ctx, `
		SELECT wd.application_id::text, wi.test_run FROM workflow.workflow_instance wi
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id WHERE wi.id::text = $1`, instanceID).Scan(&appID, &testRun)
	if errors.Is(err, pgx.ErrNoRows) || testRun {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return h.actorCanAccessApp(ctx, act, appID)
}

// workflowInstanceCancellable reports whether act may cancel instanceID: a
// developer who builds its application (test runs included, which are a
// developer's own), or a business admin whose History lists it — the
// workflowAdminScopeSQL scope, test runs excluded. A narrow check of its
// own, not a wider shared guard. An unknown id is false.
func (h *handler) workflowInstanceCancellable(ctx context.Context, act *actor, instanceID string) (bool, error) {
	var appID string
	var testRun bool
	err := h.db.QueryRow(ctx, `
		SELECT wd.application_id::text, wi.test_run
		FROM workflow.workflow_instance wi
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		WHERE wi.id::text = $1`, instanceID).Scan(&appID, &testRun)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if act.hasRole("developer") {
		ok, err := h.actorCanAccessApp(withBuilderRoute(ctx), act, appID)
		if err != nil || ok {
			return ok, err
		}
	}
	if !act.hasRole("business_admin") || testRun {
		return false, nil
	}
	scope, err := h.resolveWorkflowAdminScope(ctx, act)
	if err != nil {
		return false, err
	}
	var inScope bool
	err = h.db.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM core.application app
			WHERE app.id::text = $4 AND `+workflowAdminScopeSQL+`)`, scope.args(appID)...).Scan(&inScope)
	return inScope, err
}

func (h *handler) auditInstanceCancelled(ctx context.Context, act *actor, res *workflow.CancelResult, instanceID, reason string) {
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryDataChange, EventType: auditlog.EventWorkflowInstanceCancelled,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ApplicationID: res.ApplicationID, ResourceType: "workflow_instance", ResourceID: instanceID,
		Metadata: map[string]string{"workflow": res.WorkflowName, "reason": reason, "notified": fmt.Sprint(res.Notified)},
	})
}

// workflowInstanceGet handles GET /api/workflow/instances/{id}: one run, for
// whoever may administer it (workflowInstanceCancellable) or took part in it —
// started it, acted on a step or was told of it, as My history lists runs.
// Anyone else gets 404. Its status reads "reopened" for a reopened run, with
// when, by whom and why (2026-11-13: one run could not be read by its id).
func (h *handler) workflowInstanceGet(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ctx := r.Context()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	instanceID := r.PathValue("id")
	if _, err := uuid.Parse(instanceID); err != nil {
		jsonErr(w, fmt.Errorf("workflow instance not found"), http.StatusNotFound)
		return
	}
	admin, err := h.workflowInstanceCancellable(ctx, act, instanceID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	type stepOut struct {
		StepID      string  `json:"step_id"`
		Status      string  `json:"status"`
		Decision    string  `json:"decision,omitempty"`
		Comment     string  `json:"comment,omitempty"`
		CompletedAt *string `json:"completed_at,omitempty"`
	}
	var out struct {
		ID            string          `json:"id"`
		Workflow      string          `json:"workflow"`
		ApplicationID string          `json:"application_id"`
		Status        string          `json:"status"`
		Context       json.RawMessage `json:"context"`
		StartedAt     string          `json:"started_at"`
		CompletedAt   *string         `json:"completed_at,omitempty"`
		ReopenedAt    *string         `json:"reopened_at,omitempty"`
		ReopenedBy    *string         `json:"reopened_by,omitempty"`
		ReopenReason  *string         `json:"reopen_reason,omitempty"`
		Steps         []stepOut       `json:"steps"`
	}
	err = h.db.QueryRow(ctx, `
		SELECT wi.id::text, wd.name, wd.application_id::text,
		       CASE WHEN wi.reopened_at IS NOT NULL THEN 'reopened' ELSE wi.status::text END,
		       COALESCE(wi.context, '{}'::jsonb), wi.started_at::text,
		       CASE WHEN wi.completed_at IS NOT NULL THEN wi.completed_at::text END,
		       CASE WHEN wi.reopened_at IS NOT NULL THEN wi.reopened_at::text END,
		       (SELECT COALESCE(NULLIF(u.display_name, ''), u.email) FROM identity.user u WHERE u.id = wi.reopened_by),
		       wi.reopen_reason
		FROM workflow.workflow_instance wi
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		WHERE wi.id = $2::uuid AND NOT wi.test_run
		  AND ($3::bool OR (`+myHistoryReachSQL+` AND (wi.started_by = $1::uuid
		       OR EXISTS (SELECT 1 FROM workflow.workflow_step ws WHERE ws.instance_id = wi.id AND ws.assignee_user_id = $1::uuid)
		       OR EXISTS (SELECT 1 FROM notification.notification n WHERE n.recipient_user_id = $1::uuid
		                  AND n.resource_type = 'workflow_instance' AND n.resource_id = wi.id::text))))`,
		act.UserID, instanceID, admin,
	).Scan(&out.ID, &out.Workflow, &out.ApplicationID, &out.Status, &out.Context, &out.StartedAt, &out.CompletedAt,
		&out.ReopenedAt, &out.ReopenedBy, &out.ReopenReason)
	if errors.Is(err, pgx.ErrNoRows) {
		jsonErr(w, fmt.Errorf("workflow instance not found"), http.StatusNotFound)
		return
	}
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	rows, err := h.db.Query(ctx, `
		SELECT step_def_id, status::text, COALESCE(decision, ''), COALESCE(comment, ''),
		       CASE WHEN completed_at IS NOT NULL THEN completed_at::text END
		FROM workflow.workflow_step WHERE instance_id = $1::uuid ORDER BY created_at`, instanceID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out.Steps = []stepOut{}
	for rows.Next() {
		var s stepOut
		if err := rows.Scan(&s.StepID, &s.Status, &s.Decision, &s.Comment, &s.CompletedAt); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		out.Steps = append(out.Steps, s)
	}
	jsonOK(w, out)
}

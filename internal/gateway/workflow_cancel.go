package gateway

// Cancelling a running workflow instance (owner request, 2026-10-10): a
// developer or a business admin stops a run started by mistake — from
// Developer › Triggers' execution log, or Business Admin › History. Both
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

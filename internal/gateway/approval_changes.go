package gateway

import (
	"context"
	"net/http"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/approvalsnap"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
)

// approvalChanges handles GET /api/workflow/approval-changes: the approved
// runs of the selected model's application whose approved figures changed
// since their approval (approvalsnap), for anyone who uses the model — the
// dashboards and status widgets say so (owner decision 2026-11-13). Each run
// says how many values changed; its examples leave out a metric or member
// hidden from the reader. ?instance_id= narrows it to one run.
func (h *handler) approvalChanges(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	var appID string
	if err := h.db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	runs, err := h.visibleApprovalChanges(ctx, act.UserID, appID, strings.TrimSpace(r.URL.Query().Get("instance_id")), 5)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	for i := range runs {
		runs[i].CanReopen, runs[i].ReopenRoles = h.mayReopen(ctx, act, runs[i].InstanceID)
	}
	jsonOK(w, map[string]any{"runs": runs})
}

// mayReopen says whether act may reopen the approved run instanceID — the
// checks POST /api/workflow/instances/{id}/reopen applies — and the roles the
// workflow names as its reopeners (none: the application's business admins
// and developers). A dashboard tells someone who may not whom to ask instead
// of asking them to reopen it (2026-11-19: Sales saw "reopen the run" above
// their drivers after a month-end).
func (h *handler) mayReopen(ctx context.Context, act *actor, instanceID string) (bool, []string) {
	roles, _ := h.workflowStore(ctx).ReopenRoles(ctx, instanceID)
	ok, err := h.workflowInstanceReopenable(ctx, act, instanceID)
	if err != nil || !ok {
		return false, roles
	}
	if len(roles) > 0 {
		holds, err := h.workflowStore(ctx).HoldsReopenRole(ctx, instanceID, act.UserID)
		return err == nil && holds, roles
	}
	return true, roles
}

// visibleApprovalChanges is approvalsnap.Changes with each run's examples
// limited to what userID sees: a metric or a member hidden from them is not
// shown (their count still counts it).
func (h *handler) visibleApprovalChanges(ctx context.Context, userID, appID, instanceID string, examples int) ([]approvalsnap.RunChanges, error) {
	runs, err := approvalsnap.Changes(ctx, h.db.For(ctx), appID, instanceID, examples*4)
	if err != nil {
		return nil, err
	}
	pool := h.db.For(ctx)
	for i := range runs {
		kept := runs[i].Examples[:0]
		for _, c := range runs[i].Examples {
			if len(kept) == examples {
				break
			}
			if a, err := writeguard.MetricAccess(ctx, pool, userID, c.MetricID); err != nil || a == "hidden" {
				continue
			}
			hidden := false
			for _, id := range c.MemberIDs() {
				if a, err := writeguard.HiddenAccess(ctx, pool, userID, id); err != nil || a == "hidden" {
					hidden = true
					break
				}
			}
			if !hidden {
				kept = append(kept, c)
			}
		}
		runs[i].Examples = kept
	}
	return runs, nil
}

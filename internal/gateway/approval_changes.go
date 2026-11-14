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
	jsonOK(w, map[string]any{"runs": runs})
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

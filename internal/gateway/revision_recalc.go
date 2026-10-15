package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// recalculateRevision serves POST /api/developer/revisions/{id}/recalculate:
// every calculated metric of the revision recalculated, after the request
// has answered, as a cell write's recalculation runs (the grid shows its
// calculated cells as pending until it lands).
//
// Passes start from writes and edits only, so a pass cut short — the
// gateway restarted or was killed mid-way — left its metrics stale until
// someone happened to change one of their inputs, and nobody could ask for
// one: six models of a tenant whose gateways were OOM-killed stayed so
// (2026-10-15).
func (h *handler) recalculateRevision(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !h.requireResourceAccess(w, r, "revision", id) {
		return
	}
	ctx := r.Context()
	modelID, metricIDs, err := h.calculatedMetricsOf(ctx, id)
	if err != nil {
		jsonErr(w, err, http.StatusNotFound)
		return
	}
	h.recalculateMetrics(ctx, modelID, id, metricIDs, "recalculation on request")
	act, _ := h.resolveActor(ctx, r)
	actorID, actorRole := "", ""
	if act != nil {
		actorID, actorRole = act.UserID, strings.Join(act.Roles, ",")
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryModelChange, EventType: auditlog.EventRevisionRecalculated,
		ActorUserID: actorID, ActorRole: actorRole,
		ResourceType: "revision", ResourceID: id, RevisionID: id,
		Metadata: map[string]string{"metrics": strconv.Itoa(len(metricIDs))},
	})
	jsonOK(w, map[string]any{"recalculating": len(metricIDs) > 0, "metrics": len(metricIDs)})
}

// calculatedMetricsOf is the model of revisionID and the ids of its
// calculated metrics (the revision's own and the model's revision-global
// ones).
func (h *handler) calculatedMetricsOf(ctx context.Context, revisionID string) (modelID string, metricIDs []string, err error) {
	if err := h.db.QueryRow(ctx, `SELECT model_id::text FROM model.revision WHERE id::text = $1`, revisionID).Scan(&modelID); err != nil {
		return "", nil, fmt.Errorf("no such revision")
	}
	rows, err := h.db.Query(ctx, `
		SELECT id::text FROM model.metric_def
		WHERE model_id = $1::uuid AND (revision_id = $2::uuid OR revision_id IS NULL) AND NOT is_input
		ORDER BY id`, modelID, revisionID)
	if err != nil {
		return "", nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var mid string
		if err := rows.Scan(&mid); err != nil {
			return "", nil, err
		}
		metricIDs = append(metricIDs, mid)
	}
	return modelID, metricIDs, rows.Err()
}

// recalculateMetrics recalculates metricIDs of revisionID in the
// background, recorded as pending for the grid.
func (h *handler) recalculateMetrics(ctx context.Context, modelID, revisionID string, metricIDs []string, what string) {
	if len(metricIDs) == 0 {
		return
	}
	sched := calculation.NewScheduler(h.log, calculation.NewStore(h.db.For(ctx)))
	h.recalcInBackground(ctx, revisionID, what, func(bg context.Context) {
		if err := sched.RecalcSpecific(bg, modelID, revisionID, metricIDs); err != nil {
			h.log.Warn().Err(err).Str("revision", revisionID).Msg("recalculation on request failed")
		}
	})
}

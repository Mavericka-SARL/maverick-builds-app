package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/modeledit"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// ── PUT /api/developer/dashboards/order ────────────────────────────────────

// reorderDashboards sets the order of a revision's dashboards: {dashboard_ids
// (every dashboard of one revision, in the wanted order)}. It is the order of
// the Dashboards page's tabs, and its first dashboard is where everyone
// lands. The edit is modeledit.ReorderDashboards, which the AI Developer's
// reorder_dashboards runs too.
//
// Developer only (the route's dev guard), in a model the caller may build
// in; a dashboard outside the caller's scope answers 404 like an unknown one.
func (h *handler) reorderDashboards(w http.ResponseWriter, r *http.Request) {
	ctx := withBuilderRoute(r.Context())
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	var body struct {
		DashboardIDs []string `json:"dashboard_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
		return
	}
	if len(body.DashboardIDs) == 0 {
		jsonErr(w, fmt.Errorf("dashboard_ids is required: every dashboard of the revision, in the wanted order"), http.StatusBadRequest)
		return
	}
	var modelID, revisionID string
	if err := h.db.QueryRow(ctx, `SELECT model_id::text, COALESCE(revision_id::text, '') FROM model.dashboard_def WHERE id=$1::uuid`,
		strings.TrimSpace(body.DashboardIDs[0])).Scan(&modelID, &revisionID); err != nil {
		jsonErr(w, fmt.Errorf("dashboard not found"), http.StatusNotFound)
		return
	}
	allowed, err := h.actorCanAccessModel(ctx, act, modelID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !allowed {
		jsonErr(w, fmt.Errorf("dashboard not found"), http.StatusNotFound)
		return
	}
	if revisionID == "" {
		jsonErr(w, fmt.Errorf("the dashboard belongs to no revision"), http.StatusBadRequest)
		return
	}
	names, err := modeledit.ReorderDashboards(ctx, h.db, modelID, revisionID, body.DashboardIDs)
	if err != nil {
		var re *modeledit.ReorderError
		if errors.As(err, &re) {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		jsonErr(w, fmt.Errorf("reorder dashboards: %w", err), http.StatusInternalServerError)
		return
	}
	_, appID := h.dashboardScope(ctx, strings.TrimSpace(body.DashboardIDs[0]))
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryModelChange, EventType: auditlog.EventDashboardsReordered,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ApplicationID: appID, ResourceType: "revision", ResourceID: revisionID, RevisionID: revisionID,
		Metadata: map[string]string{"order": strings.Join(names, " | ")},
	})
	jsonOK(w, map[string]string{"status": "ok"})
}

// ── POST /api/developer/dashboards/{id}/arrange ────────────────────────────
// arrangeDashboard lays a dashboard's widgets out: {rows (every widget once,
// row by row from the top), weights?}. The edit is modeledit.ArrangeDashboard,
// which the AI Developer's arrange_dashboard runs too. Developer only, in a
// model the caller may build in; a dashboard outside the caller's scope
// answers 404 like an unknown one.
func (h *handler) arrangeDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := withBuilderRoute(r.Context())
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	dashID := strings.TrimSpace(r.PathValue("id"))
	var body struct {
		Rows    [][]string  `json:"rows"`
		Weights [][]float64 `json:"weights"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Rows) == 0 {
		jsonErr(w, fmt.Errorf("rows is required: every widget of the dashboard, row by row"), http.StatusBadRequest)
		return
	}
	var modelID string
	if err := h.db.QueryRow(ctx, `SELECT model_id::text FROM model.dashboard_def WHERE id=$1::uuid`, dashID).Scan(&modelID); err != nil {
		jsonErr(w, fmt.Errorf("dashboard not found"), http.StatusNotFound)
		return
	}
	allowed, err := h.actorCanAccessModel(ctx, act, modelID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !allowed {
		jsonErr(w, fmt.Errorf("dashboard not found"), http.StatusNotFound)
		return
	}
	n, err := modeledit.ArrangeDashboard(ctx, h.db, dashID, body.Rows, body.Weights)
	if err != nil {
		var le *modeledit.LayoutError
		if errors.As(err, &le) {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		jsonErr(w, fmt.Errorf("arrange dashboard: %w", err), http.StatusInternalServerError)
		return
	}
	revisionID, appID := h.dashboardScope(ctx, dashID)
	rows := make([]string, len(body.Rows))
	for i, row := range body.Rows {
		rows[i] = strings.Join(row, ",")
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryModelChange, EventType: auditlog.EventDashboardArranged,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ApplicationID: appID, ResourceType: "dashboard", ResourceID: dashID, RevisionID: revisionID,
		Metadata: map[string]string{"rows": strings.Join(rows, " | ")},
	})
	jsonOK(w, map[string]any{"status": "ok", "widgets": n})
}

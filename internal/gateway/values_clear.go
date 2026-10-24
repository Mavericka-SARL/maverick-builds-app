package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/importpkg"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// Clearing typed values in bulk (importpkg.ClearValues): a grid's input
// metrics, or one metric, in the revision the grid or metric belongs to,
// optionally under chosen members. A developer's, from Grids › Clear values,
// Metrics › Clear values and the AI Developer's clear_input_values. The only
// bulk clear was a full reload, which empties the whole revision.

// nothingToClearCode marks the refusal of a grid or metric with no input
// metric: a clear of everything has nothing to do there (ai_values.go).
const nothingToClearCode = "nothing_to_clear"

type clearBody struct {
	// MetricIDs narrows a grid's clear to some of its input metrics.
	MetricIDs []string        `json:"metric_ids"`
	Members   json.RawMessage `json:"members"`
	DryRun    bool            `json:"dry_run"`
}

func readClearBody(w http.ResponseWriter, r *http.Request) (clearBody, bool) {
	var b clearBody
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil && !errors.Is(err, io.EOF) {
		jsonErr(w, fmt.Errorf("invalid JSON body"), http.StatusBadRequest)
		return b, false
	}
	return b, true
}

// clearGridValues serves POST /api/developer/grids/{id}/clear-values.
func (h *handler) clearGridValues(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !h.requireResourceAccess(w, r, "grid", id) {
		return
	}
	body, ok := readClearBody(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	var modelID, revisionID string
	if err := h.db.QueryRow(ctx, `SELECT model_id::text, COALESCE(revision_id::text, '') FROM model.grid_def WHERE id = $1::uuid`, id).Scan(&modelID, &revisionID); err != nil {
		jsonErr(w, fmt.Errorf("grid not found"), http.StatusNotFound)
		return
	}
	rows, err := h.db.Query(ctx, `
		SELECT m.id::text FROM model.grid_metric gm JOIN model.metric_def m ON m.id = gm.metric_id
		WHERE gm.grid_id = $1::uuid AND m.is_input ORDER BY gm.sort_order`, id)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	onGrid := map[string]bool{}
	var inputs []string
	for rows.Next() {
		var mid string
		if err := rows.Scan(&mid); err != nil {
			rows.Close()
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		onGrid[mid] = true
		inputs = append(inputs, mid)
	}
	rows.Close()
	if len(body.MetricIDs) > 0 {
		for _, mid := range body.MetricIDs {
			if !onGrid[mid] {
				jsonErr(w, fmt.Errorf("metric %s is not an input metric of this grid", mid), http.StatusBadRequest)
				return
			}
		}
		inputs = body.MetricIDs
	}
	if revisionID == "" {
		revisionID = h.activeRevisionOf(r, modelID)
	}
	h.clearValues(w, r, modelID, revisionID, inputs, body, "grid", id)
}

// clearMetricValues serves POST /api/developer/metrics/{id}/clear-values.
func (h *handler) clearMetricValues(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !h.requireResourceAccess(w, r, "metric", id) {
		return
	}
	body, ok := readClearBody(w, r)
	if !ok {
		return
	}
	var modelID, revisionID string
	if err := h.db.QueryRow(r.Context(), `SELECT model_id::text, COALESCE(revision_id::text, '') FROM model.metric_def WHERE id = $1::uuid`, id).Scan(&modelID, &revisionID); err != nil {
		jsonErr(w, fmt.Errorf("metric not found"), http.StatusNotFound)
		return
	}
	if revisionID == "" {
		revisionID = h.activeRevisionOf(r, modelID)
	}
	h.clearValues(w, r, modelID, revisionID, []string{id}, body, "metric", id)
}

// activeRevisionOf is a model's active revision, for a revision-global grid
// or metric.
func (h *handler) activeRevisionOf(r *http.Request, modelID string) string {
	var rev string
	_ = h.db.QueryRow(r.Context(), `SELECT COALESCE(active_revision_id::text, '') FROM core.model WHERE id = $1::uuid`, modelID).Scan(&rev)
	return rev
}

func (h *handler) clearValues(w http.ResponseWriter, r *http.Request, modelID, revisionID string, metricIDs []string, body clearBody, kind, id string) {
	ctx := r.Context()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}
	members, err := importpkg.ParseMembers(body.Members)
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	res, err := importpkg.ClearValues(ctx, h.db.For(ctx), importpkg.ClearRequest{
		ModelID: modelID, RevisionID: revisionID, UserID: act.UserID,
		MetricIDs: metricIDs, Members: members, DryRun: body.DryRun,
	})
	switch {
	case errors.Is(err, importpkg.ErrWriteDenied):
		jsonErr(w, err, http.StatusForbidden)
		return
	case errors.Is(err, importpkg.ErrNothingToClear):
		jsonCodeErr(w, http.StatusBadRequest, nothingToClearCode, fmt.Sprintf("this %s has no input metric: only typed values are cleared", kind))
		return
	case err != nil && (strings.Contains(err.Error(), "has no member") || strings.Contains(err.Error(), "name at least one") ||
		strings.Contains(err.Error(), "is calculated") || strings.Contains(err.Error(), "is not in this model")):
		jsonErr(w, err, http.StatusBadRequest)
		return
	case err != nil:
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !body.DryRun && res.Cells > 0 {
		h.remeasureStorage(ctx, h.customerOfModel(ctx, modelID))
		h.recalcAfterImport(ctx, modelID, revisionID, "clear", res.MetricIDs)
		var appID string
		_ = h.db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID)
		md := map[string]string{"cells": strconv.Itoa(res.Cells), "metrics": strconv.Itoa(len(metricIDs))}
		if len(members) > 0 {
			raw, _ := json.Marshal(members)
			md["members"] = string(raw)
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryDataChange, EventType: auditlog.EventValuesCleared,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ApplicationID: appID, ResourceType: kind, ResourceID: id, RevisionID: revisionID, Metadata: md,
		})
	}
	jsonOK(w, map[string]any{"cells": res.Cells, "withheld": res.Withheld, "posted": res.Posted, "dry_run": body.DryRun})
}

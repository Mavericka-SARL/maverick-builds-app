package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// Copying values from one member to another — the plan into the budget, one
// scenario into another — by a business admin, in the live revision (owner
// request, 2026-11-07: a budget was frozen by downloading the plan and
// uploading it again). Every value goes through the same checks as a typed
// one (prepareCell: access rules, workflow locks, closed and posted cells),
// all or nothing; then the "Lock the approved budget" style workflow freezes
// it, and Reopen takes it back.

// maxCopyCells bounds one copy: each cell is checked as a typed value is.
const maxCopyCells = 20000

// baCopyValues handles POST /api/business-admin/copy-values, body
// {"dimension_id", "from", "to", "metric_ids" (optional), "dry_run"}.
func (h *handler) baCopyValues(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	_, modelID, err := h.baWorkspaceModel(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve workspace model")
		return
	}
	var body struct {
		DimensionID string   `json:"dimension_id"`
		From        string   `json:"from"`
		To          string   `json:"to"`
		MetricIDs   []string `json:"metric_ids"`
		DryRun      bool     `json:"dry_run"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
		return
	}
	body.From, body.To = strings.TrimSpace(body.From), strings.TrimSpace(body.To)
	if body.From == "" || body.To == "" || body.From == body.To {
		jsonErr(w, fmt.Errorf("name two different members to copy from and to"), http.StatusBadRequest)
		return
	}
	var revisionID, dimName string
	err = h.db.QueryRow(ctx, `
		SELECT cm.active_revision_id::text, d.name
		FROM core.model cm JOIN model.dimension_def d ON d.model_id = cm.id AND d.revision_id = cm.active_revision_id
		WHERE cm.id = $1::uuid AND d.id::text = $2`, modelID, body.DimensionID).Scan(&revisionID, &dimName)
	if errors.Is(err, pgx.ErrNoRows) {
		jsonErr(w, fmt.Errorf("dimension not found"), http.StatusNotFound)
		return
	}
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	for _, code := range []string{body.From, body.To} {
		var leaf bool
		if err := h.db.QueryRow(ctx, `
			SELECT EXISTS (SELECT 1 FROM model.dimension_member m WHERE m.dimension_id = $1::uuid AND m.code = $2
			               AND NOT EXISTS (SELECT 1 FROM model.dimension_member c WHERE c.parent_member_id = m.id))`,
			body.DimensionID, code).Scan(&leaf); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if !leaf {
			jsonErr(w, fmt.Errorf("%q is no member of %s that holds values (a member without members under it)", code, dimName), http.StatusBadRequest)
			return
		}
	}

	// The latest value typed into each cell of from: a value a table of
	// records posts stays with its records.
	rows, err := h.db.Query(ctx, `
		SELECT DISTINCT ON (f.metric_id, f.dim_members) f.metric_id::text, f.dim_members::text, f.value, f.text_value, COALESCE(md.format, '')
		FROM runtime.fact_input f
		JOIN model.metric_def md ON md.id = f.metric_id AND md.is_input
		WHERE f.model_id = $1::uuid AND f.revision_id = $2::uuid AND f.source_ref IS NULL
		  AND f.dim_members->>$3 = $4
		  AND (COALESCE(cardinality($5::text[]), 0) = 0 OR f.metric_id::text = ANY($5::text[]))
		ORDER BY f.metric_id, f.dim_members, f.entered_at DESC`,
		modelID, revisionID, body.DimensionID, body.From, body.MetricIDs)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	type source struct {
		metricID, format string
		codes            map[string]string
		value            *float64
		text             *string
	}
	var sources []source
	for rows.Next() {
		var s source
		var dm string
		if err := rows.Scan(&s.metricID, &dm, &s.value, &s.text, &s.format); err != nil {
			rows.Close()
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if json.Unmarshal([]byte(dm), &s.codes) != nil {
			continue
		}
		s.codes[body.DimensionID] = body.To
		sources = append(sources, s)
		if len(sources) > maxCopyCells {
			break
		}
	}
	rows.Close()
	if len(sources) > maxCopyCells {
		jsonErr(w, fmt.Errorf("more than %d values to copy: choose some metrics", maxCopyCells), http.StatusBadRequest)
		return
	}
	if len(sources) == 0 {
		jsonOK(w, map[string]any{"cells": 0, "dry_run": body.DryRun})
		return
	}

	cells := make([]preparedCell, 0, len(sources))
	var refused []cellBatchRefusal
	for _, s := range sources {
		req := writebackReq{ModelID: modelID, RevisionID: revisionID, MetricID: s.metricID, DimCodes: s.codes}
		switch {
		case s.text != nil:
			req.Text = s.text
		case s.value != nil:
			req.Value = *s.value
		default:
			continue
		}
		c, ref := h.prepareCell(ctx, a, revisionID, req, s.format)
		if ref != nil {
			if len(refused) < 20 {
				refused = append(refused, cellBatchRefusal{MetricID: s.metricID, Status: ref.status, Code: ref.code, Error: ref.message})
			}
			if ref.status >= http.StatusInternalServerError {
				jsonErr(w, errors.New(ref.message), ref.status)
				return
			}
			continue
		}
		cells = append(cells, c)
	}
	if len(refused) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":   fmt.Sprintf("some values cannot be written to %s (a lock or an access rule); nothing was copied", body.To),
			"refused": refused,
		})
		return
	}
	if body.DryRun {
		jsonOK(w, map[string]any{"cells": len(cells), "dry_run": true})
		return
	}
	if err := h.checkCellLimits(ctx, modelID, cells); err != nil {
		h.jsonLimitErr(w, err)
		return
	}
	if err := h.storeCells(ctx, modelID, revisionID, a.UserID, cells); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	done := context.WithoutCancel(ctx)
	auditlog.Log(done, h.db.For(done), h.log, auditlog.Fields{
		Category: auditlog.CategoryDataChange, EventType: auditlog.EventCellsWritten,
		ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
		ResourceType: "revision", ResourceID: revisionID, RevisionID: revisionID,
		Metadata: map[string]string{"model_id": modelID, "cells": strconv.Itoa(len(cells)), "via": "copy_values",
			"dimension": dimName, "from": body.From, "to": body.To},
	})
	go func() { //nolint:contextcheck
		calcLog := h.log.With().Str("op", "copy_values").Logger()
		scheduler := calculation.NewScheduler(calcLog, calculation.NewStore(h.db.For(done)))
		if err := recalcWrittenCells(done, scheduler, modelID, revisionID, cells); err != nil {
			h.log.Warn().Err(err).Msg("recalc failed after copying values")
		}
	}()
	jsonOK(w, map[string]any{"cells": len(cells), "dry_run": false})
}

package gateway

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// Cell writes, one (POST /api/cells) or many at once (POST
// /api/cells/batch). Both run the same checks, in the same order, through
// the helpers below: the model and revision once, each metric once, each
// cell's members, write guard and value. A batch is all or nothing: every
// cell is checked before any is written, and they are written in one
// transaction with one recalculation, so a refused cell leaves the grid as
// it was.

// cellRefusal is why a cell write is refused: the status and message the
// route answers with, and a code when the refusal has one.
type cellRefusal struct {
	status  int
	code    string
	message string
}

func refuseCell(status int, format string, args ...any) *cellRefusal {
	return &cellRefusal{status: status, message: fmt.Sprintf(format, args...)}
}

func (r *cellRefusal) respond(w http.ResponseWriter) {
	if r.code != "" {
		jsonCodeErr(w, r.status, r.code, r.message)
		return
	}
	jsonErr(w, errors.New(r.message), r.status)
}

// cellRevision checks that a writes to modelID at all, and resolves the
// revision it writes: revisionID, or the model's active revision when it is
// empty. The revision must belong to the model, be one the caller may write
// (revision_access.go), and not be system-managed.
func (h *handler) cellRevision(ctx context.Context, a *actor, modelID, revisionID string) (string, *cellRefusal) {
	// The caller must have access to this model at all — a metric or
	// revision ID from a model the caller has no grant on must never reach
	// the checks below, regardless of whether those IDs happen to exist.
	if canAccess, err := h.actorCanAccessModel(ctx, a, modelID); err != nil {
		return "", refuseCell(http.StatusInternalServerError, "%v", err)
	} else if !canAccess {
		return "", refuseCell(http.StatusForbidden, "forbidden: model is outside your access scope")
	}
	if revisionID == "" {
		_ = h.db.QueryRow(ctx,
			`SELECT COALESCE(active_revision_id::text, (SELECT id::text FROM model.revision WHERE model_id=$1::uuid ORDER BY created_at DESC LIMIT 1)) FROM core.model WHERE id=$1::uuid`,
			modelID,
		).Scan(&revisionID)
	}
	// The revision must actually belong to this model — a client-supplied
	// revision_id from a different model is rejected, not silently accepted.
	// Checked even for the auto-resolved case above (redundant there, since
	// that path is already model-scoped by construction, but harmless).
	var belongs bool
	if err := h.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM model.revision WHERE id=$1::uuid AND model_id=$2::uuid)`,
		revisionID, modelID,
	).Scan(&belongs); err != nil {
		return "", refuseCell(http.StatusInternalServerError, "check revision ownership: %v", err)
	}
	if !belongs {
		return "", refuseCell(http.StatusForbidden, "revision is outside this model")
	}
	// Only the open revision is a user's to write (revision_access.go).
	if !h.revisionOpen(ctx, a, modelID, revisionID) {
		return "", refuseCell(http.StatusNotFound, "revision not found")
	}
	return revisionID, nil
}

// cellRevisionWritable refuses a system-managed (read-only) revision — as
// such, whatever the write names.
func (h *handler) cellRevisionWritable(ctx context.Context, revisionID string) *cellRefusal {
	if systemManaged, err := writeguard.SystemManaged(ctx, h.db.For(ctx), revisionID); err != nil {
		return refuseCell(http.StatusInternalServerError, "check system-managed: %v", err)
	} else if systemManaged {
		return refuseCell(http.StatusForbidden, "this revision is system-managed and read-only")
	}
	return nil
}

// cellMetric checks that metricID is an input metric of modelID the caller
// may write, and answers its format.
func (h *handler) cellMetric(ctx context.Context, a *actor, modelID, metricID string) (string, *cellRefusal) {
	// The model_id predicate matters: without it, an is_input metric from a
	// completely different tenant's model would otherwise pass.
	var isInput bool
	var format string
	if err := h.db.QueryRow(ctx,
		`SELECT is_input, COALESCE(format,'') FROM model.metric_def WHERE id=$1::uuid AND model_id=$2::uuid`, metricID, modelID,
	).Scan(&isInput, &format); err != nil || !isInput {
		return "", refuseCell(http.StatusForbidden, "metric is not writable")
	}
	// The caller must not be restricted to read-only or hidden access for it.
	access, err := writeguard.MetricAccess(ctx, h.db.For(ctx), a.UserID, metricID)
	if err != nil {
		return "", refuseCell(http.StatusInternalServerError, "check metric access: %v", err)
	}
	if access == "hidden" || access == "read" {
		return "", refuseCell(http.StatusForbidden, "access denied")
	}
	return format, nil
}

// preparedCell is one checked cell, ready to store.
type preparedCell struct {
	metricID   string
	dimMembers string // the cell's coordinate as stored: {dimId: memberCode}
	write      cellWrite
	req        writebackReq
}

// prepareCell checks one cell of a metric already checked (cellMetric):
// its members, the write guard, and the value it holds.
func (h *handler) prepareCell(ctx context.Context, a *actor, revisionID string, req writebackReq, format string) (preparedCell, *cellRefusal) {
	// A code no member of this revision's dimension has, a parent member and
	// a calculated one are refused: a value stored there is never read by a
	// grid (found live: "Snacks" for the member SNACKS).
	memberIDs, err := writeguard.ResolveWriteMembers(ctx, h.db.For(ctx), req.ModelID, revisionID, req.DimCodes)
	if me := (*writeguard.MemberError)(nil); errors.As(err, &me) {
		return preparedCell{}, &cellRefusal{status: http.StatusBadRequest, code: me.Code, message: me.Message}
	} else if err != nil {
		return preparedCell{}, refuseCell(http.StatusInternalServerError, "%v", err)
	}

	// Generic write guard: system-managed revision, hidden/read-only access
	// (cascading through the dimension hierarchy — e.g. a cost center hidden
	// from this user also blocks a write to one of its employees, even
	// without a direct rule on that employee — see writeguard.ExpandHidden's
	// read-side equivalent in grid()), and workflow-lock. The single
	// implementation shared with every import path (HTTP and gRPC) — see
	// writeguard.CheckWrite's package doc.
	if reason, err := writeguard.CheckWriteMetrics(ctx, h.db.For(ctx), req.ModelID, revisionID, a.UserID, memberIDs, []string{req.MetricID}); err != nil {
		return preparedCell{}, refuseCell(http.StatusInternalServerError, "write guard: %v", err)
	} else if reason != "" {
		return preparedCell{}, refuseCell(http.StatusForbidden, "%s", reason)
	}

	// A pick-list cell holds a member of its dimension (the value must be
	// one's key, or Member names one), a text metric's cell its text, and
	// a clear empties the cell.
	write, err := h.resolveCellWrite(ctx, req, format)
	if err != nil {
		return preparedCell{}, refuseCell(http.StatusBadRequest, "%v", err)
	}

	dimMembers := "{}"
	if len(req.DimCodes) > 0 {
		b, _ := json.Marshal(req.DimCodes)
		dimMembers = string(b)
	}
	return preparedCell{metricID: req.MetricID, dimMembers: dimMembers, write: write, req: req}, nil
}

// checkCellLimits refuses cells the workspace's plan has no room for.
func (h *handler) checkCellLimits(ctx context.Context, modelID string, cells []preparedCell) error {
	rows := 0
	for _, c := range cells {
		if !c.write.clear {
			rows++
		}
	}
	if rows == 0 || h.plans == nil {
		return nil
	}
	cid := h.customerOfModel(ctx, modelID)
	if cid == "" {
		return nil
	}
	return cmp.Or(h.plans.CheckFactRows(ctx, h.db.For(ctx), cid, modelID, rows), h.plans.CheckStorage(ctx, h.db.For(ctx), cid))
}

// storeCells writes checked cells in one transaction: a value or text is a
// new fact_input row entered by userID, a clear deletes the cell's directly
// entered rows (clearCellTx).
func (h *handler) storeCells(ctx context.Context, modelID, revisionID, userID string, cells []preparedCell) error {
	tx, err := h.db.For(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, c := range cells {
		if c.write.clear {
			err = clearCellTx(ctx, tx, modelID, revisionID, c.metricID, c.dimMembers)
		} else {
			_, err = tx.Exec(ctx, `
				INSERT INTO runtime.fact_input
				    (model_id, revision_id, metric_id, dim_members, value, entered_by, text_value)
				VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6::uuid, $7)
			`, modelID, revisionID, c.metricID, c.dimMembers, c.write.value, userID, c.write.text)
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ── POST /api/cells/batch ───────────────────────────────────────────────────

// maxCellBatch caps one batch: a pasted block or a chat's planned month,
// not a file — imports carry those.
const maxCellBatch = 500

type cellBatchReq struct {
	ModelID    string          `json:"model_id"`
	RevisionID string          `json:"revision_id"`
	Cells      []cellBatchCell `json:"cells"`
	// DryRun checks every cell and writes none.
	DryRun bool `json:"dry_run,omitempty"`
	// Recalc "background" answers once the cells are stored, as for one
	// cell (writebackReq.Recalc).
	Recalc string `json:"recalc,omitempty"`
}

// cellBatchCell is one cell of a batch: what a single write's body carries
// besides the model and revision.
type cellBatchCell struct {
	MetricID string            `json:"metric_id"`
	DimCodes map[string]string `json:"dim_codes"`
	Value    float64           `json:"value"`
	Member   *string           `json:"member,omitempty"`
	Text     *string           `json:"text,omitempty"`
	Clear    bool              `json:"clear,omitempty"`
}

// cellBatchRefusal is one refused cell of a batch, by its index.
type cellBatchRefusal struct {
	Index    int    `json:"index"`
	MetricID string `json:"metric_id"`
	Status   int    `json:"status"`
	Code     string `json:"code,omitempty"`
	Error    string `json:"error"`
}

func (h *handler) cellsBatch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	var req cellBatchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	if req.ModelID == "" {
		jsonErr(w, fmt.Errorf("model_id is required"), http.StatusBadRequest)
		return
	}
	if len(req.Cells) == 0 || len(req.Cells) > maxCellBatch {
		jsonErr(w, fmt.Errorf("a batch writes 1 to %d cells (this one has %d)", maxCellBatch, len(req.Cells)), http.StatusBadRequest)
		return
	}
	revisionID, ref := h.cellRevision(ctx, a, req.ModelID, req.RevisionID)
	// A chat connection's write, to a model the person may write at all,
	// stops here when the model's tenant turned chat writes off.
	via, delegated := delegatedWrite(ctx)
	if ref == nil && delegated {
		ref = h.chatWritesAllowed(ctx, req.ModelID)
	}
	if ref == nil {
		ref = h.cellRevisionWritable(ctx, revisionID)
	}
	if ref != nil {
		ref.respond(w)
		return
	}

	formats := map[string]string{}
	metricRefusals := map[string]*cellRefusal{}
	cells := make([]preparedCell, 0, len(req.Cells))
	var refused []cellBatchRefusal
	seen := map[string]int{}
	for i, c := range req.Cells {
		refuse := func(ref *cellRefusal) {
			refused = append(refused, cellBatchRefusal{Index: i, MetricID: c.MetricID, Status: ref.status, Code: ref.code, Error: ref.message})
		}
		if c.MetricID == "" {
			refuse(refuseCell(http.StatusBadRequest, "metric_id is required"))
			continue
		}
		format, known := formats[c.MetricID]
		if !known {
			if mref, done := metricRefusals[c.MetricID]; done {
				refuse(mref)
				continue
			}
			var mref *cellRefusal
			if format, mref = h.cellMetric(ctx, a, req.ModelID, c.MetricID); mref != nil {
				metricRefusals[c.MetricID] = mref
				refuse(mref)
				continue
			}
			formats[c.MetricID] = format
		}
		one := writebackReq{ModelID: req.ModelID, RevisionID: revisionID, MetricID: c.MetricID, DimCodes: c.DimCodes,
			Value: c.Value, Member: c.Member, Text: c.Text, Clear: c.Clear}
		p, cref := h.prepareCell(ctx, a, revisionID, one, format)
		if cref != nil {
			refuse(cref)
			continue
		}
		// Two writes to one cell would leave it holding whichever was
		// stored last: the batch says which it means instead.
		key := p.metricID + " " + p.dimMembers
		if first, dup := seen[key]; dup {
			refuse(refuseCell(http.StatusBadRequest, "the same cell as cell %d of this batch", first))
			continue
		}
		seen[key] = i
		cells = append(cells, p)
	}
	if len(refused) > 0 {
		// A refusal the server could not decide is the server's failure,
		// not the batch's.
		for _, rf := range refused {
			if rf.Status >= http.StatusInternalServerError {
				jsonErr(w, errors.New(rf.Error), rf.Status)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":   fmt.Sprintf("%d of %d cells were refused; nothing was written", len(refused), len(req.Cells)),
			"refused": refused,
		})
		return
	}
	if err := h.checkCellLimits(ctx, req.ModelID, cells); err != nil {
		h.jsonLimitErr(w, err)
		return
	}

	cleared := 0
	var metricIDs []string
	for _, c := range cells {
		if c.write.clear {
			cleared++
		}
		if !slices.Contains(metricIDs, c.metricID) {
			metricIDs = append(metricIDs, c.metricID)
		}
	}
	if req.DryRun {
		jsonOK(w, map[string]any{"status": "valid", "revision_id": revisionID, "cells": len(cells), "cleared": cleared})
		return
	}

	if err := h.storeCells(ctx, req.ModelID, revisionID, a.UserID, cells); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	// The cells are stored: what follows runs to its end even if the caller
	// stops waiting, so a write is never left without its recalculation.
	done := context.WithoutCancel(ctx)

	md := map[string]string{"model_id": req.ModelID, "revision_id": revisionID,
		"cells": strconv.Itoa(len(cells)), "cleared": strconv.Itoa(cleared), "metrics": strings.Join(metricIDs, ",")}
	if delegated {
		md["via"] = via
	}
	auditlog.Log(done, h.db.For(done), h.log, auditlog.Fields{
		Category: auditlog.CategoryDataChange, EventType: auditlog.EventCellsWritten,
		ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
		ResourceType: "revision", ResourceID: revisionID, RevisionID: revisionID,
		Metadata: md,
	})

	recalc := func(ctx context.Context) {
		calcLog := h.log.With().Str("op", "cells_batch").Logger()
		scheduler := calculation.NewScheduler(calcLog, calculation.NewStore(h.db.For(ctx)), nil)
		if err := scheduler.RecalcAffected(ctx, req.ModelID, revisionID, metricIDs); err != nil {
			h.log.Warn().Err(err).Msg("recalc failed after a batch write")
		}
	}
	// grid_change rules fire once per metric written, as they would for one
	// cell of it, after the recalculation (cells()).
	dispatch := func(ctx context.Context) {
		appID, _ := h.appIDFromModelID(ctx, req.ModelID)
		if appID == "" {
			return
		}
		for _, metricID := range metricIDs {
			var sourceGridID string
			_ = h.db.QueryRow(ctx, `SELECT grid_id::text FROM model.grid_metric WHERE metric_id=$1::uuid LIMIT 1`, metricID).Scan(&sourceGridID)
			h.workflowStore(ctx).DispatchEventRules(ctx, appID, revisionID, "grid_change", sourceGridID, a.UserID, map[string]string{
				"model_id": req.ModelID, "revision_id": revisionID, "metric_id": metricID, "grid_id": sourceGridID,
			})
		}
	}
	answer := map[string]any{"status": "ok", "revision_id": revisionID, "cells": len(cells), "cleared": cleared}
	if req.Recalc == "background" {
		h.recalcInBackground(done, revisionID, "recalculation after a batch of cell writes", func(bg context.Context) {
			recalc(bg)
			dispatch(bg)
		})
		answer["recalculating"] = true
		jsonOK(w, answer)
		return
	}
	recalc(done)
	go dispatch(done)
	jsonOK(w, answer)
}

// chatWritesAllowed refuses a chat connection's write to a model whose
// tenant has turned chat writes off (connector_settings.go).
func (h *handler) chatWritesAllowed(ctx context.Context, modelID string) *cellRefusal {
	cid := h.customerOfModel(ctx, modelID)
	if cid == "" {
		return nil
	}
	on, err := h.chatWritesOn(ctx, cid)
	if err != nil {
		return refuseCell(http.StatusInternalServerError, "check chat writes: %v", err)
	}
	if !on {
		return refuseCell(http.StatusForbidden, "your administrators have turned off changing data from chat connections")
	}
	return nil
}

// clearCellTx empties one cell inside tx: its directly entered rows are
// deleted, and the archive trigger keeps them in fact_input_history with
// the reason "cleared". Rows a form or an import posted (source_ref) stay —
// they are that source's, and its next posting would bring them back anyway.
func clearCellTx(ctx context.Context, tx pgx.Tx, modelID, revisionID, metricID, dimMembers string) error {
	if _, err := tx.Exec(ctx, `SET LOCAL mvx.delete_reason = 'cleared'`); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		DELETE FROM runtime.fact_input
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid
		  AND dim_members = $4::jsonb AND source_ref IS NULL`,
		modelID, revisionID, metricID, dimMembers)
	return err
}

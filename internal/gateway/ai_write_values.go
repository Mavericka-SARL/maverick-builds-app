package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// aiWriteValues writes a write_input_values step: its values as rows of the
// metric's grid, through the import pipeline's resolution and commit — the
// write guard, plan limits and recalculation a typed cell (POST /api/cells)
// or an imported file gets — into the AI draft.
func (h *handler) aiWriteValues(ctx context.Context, act *actor, req aiassistant.ValuesWriteRequest) (string, error) {
	modelID, err := h.modelOfRevision(ctx, req.RevisionID)
	if err != nil {
		return "", err
	}
	systemManaged, err := writeguard.SystemManaged(ctx, h.db.For(ctx), req.RevisionID)
	if err != nil {
		return "", fmt.Errorf("check system-managed: %w", err)
	}
	if systemManaged {
		return "", fmt.Errorf("this revision is system-managed and read-only")
	}
	clears, err := resolveClears(ctx, h.db.For(ctx), modelID, req)
	if err != nil {
		return "", err
	}
	var staged []importpkg.StagingRow
	if len(req.Rows) > 0 || len(clears) == 0 {
		if staged, err = resolveValueRows(ctx, h.db.For(ctx), modelID, req); err != nil {
			return "", err
		}
	}
	if len(clears) > 0 {
		if err := h.aiClearCells(ctx, act, modelID, req, clears); err != nil {
			return "", err
		}
	}
	if len(staged) > 0 {
		if _, err := h.commitGridRows(ctx, act, modelID, req.RevisionID, staged, importpkg.ModeReplace, "ai_values"); err != nil {
			return "", err
		}
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryDataChange, EventType: auditlog.EventCellWritten,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ResourceType: "metric", ResourceID: req.MetricID, RevisionID: req.RevisionID,
		Metadata: map[string]string{"model_id": modelID, "revision_id": req.RevisionID, "source": "ai_values",
			"values": strconv.Itoa(len(staged)), "cleared": strconv.Itoa(len(clears))},
	})
	return valuesSummary(len(staged), len(clears), req) + h.percentFractionWarning(ctx, h.db.For(ctx), staged), nil
}

// valuesSummary says what a write_input_values step wrote and emptied.
func valuesSummary(written, cleared int, req aiassistant.ValuesWriteRequest) string {
	metric := req.Header[len(req.Header)-1]
	switch {
	case cleared == 0:
		return fmt.Sprintf("Wrote %d value(s) into %s", written, metric)
	case written == 0:
		return fmt.Sprintf("Cleared %d cell(s) of %s", cleared, metric)
	}
	return fmt.Sprintf("Wrote %d value(s) into %s and cleared %d cell(s)", written, metric, cleared)
}

// clearTarget is one cell a write_input_values step empties: its
// dim_members as stored, and its members for the write guard.
type clearTarget struct {
	dimMembers string
	memberIDs  []string
}

// resolveClears resolves the cells a "value": null empties, with the cell
// write's own coordinate check.
func resolveClears(ctx context.Context, q writeguard.QueryRower, modelID string, req aiassistant.ValuesWriteRequest) ([]clearTarget, error) {
	out := make([]clearTarget, 0, len(req.Clears))
	for i, dims := range req.Clears {
		ids, err := writeguard.ResolveWriteMembers(ctx, q, modelID, req.RevisionID, dims)
		if err != nil {
			return nil, fmt.Errorf("clear %d: %w", i+1, err)
		}
		dm := "{}"
		if len(dims) > 0 {
			b, _ := json.Marshal(dims)
			dm = string(b)
		}
		out = append(out, clearTarget{dimMembers: dm, memberIDs: ids})
	}
	return out, nil
}

// aiClearCells empties cells as POST /api/cells' clear does: the write
// guard first, then clearCell, then the metric's dependents recalculated.
func (h *handler) aiClearCells(ctx context.Context, act *actor, modelID string, req aiassistant.ValuesWriteRequest, clears []clearTarget) error {
	var memberIDs []string
	for _, c := range clears {
		memberIDs = append(memberIDs, c.memberIDs...)
	}
	if reason, err := writeguard.CheckWriteMetrics(ctx, h.db.For(ctx), modelID, req.RevisionID, act.UserID, memberIDs, []string{req.MetricID}); err != nil {
		return fmt.Errorf("write guard: %w", err)
	} else if reason != "" {
		return fmt.Errorf("%s", reason)
	}
	for _, c := range clears {
		if err := h.clearCell(ctx, modelID, req.RevisionID, req.MetricID, c.dimMembers); err != nil {
			return fmt.Errorf("clear: %w", err)
		}
	}
	scheduler := calculation.NewScheduler(h.log, calculation.NewStore(h.db.For(ctx)), nil)
	if err := scheduler.RecalcAffected(ctx, modelID, req.RevisionID, []string{req.MetricID}); err != nil {
		h.log.Warn().Err(err).Msg("recalc failed after an AI clear")
	}
	return nil
}

// aiCheckWriteValues is aiWriteValues for the plan check: the same
// resolution on the check's transaction, nothing committed.
func (h *handler) aiCheckWriteValues(ctx context.Context, tx pgx.Tx, req aiassistant.ValuesWriteRequest) (string, error) {
	var modelID string
	if err := tx.QueryRow(ctx, `SELECT model_id::text FROM model.revision WHERE id=$1::uuid`, req.RevisionID).Scan(&modelID); err != nil {
		return "", fmt.Errorf("revision: %w", err)
	}
	clears, err := resolveClears(ctx, tx, modelID, req)
	if err != nil {
		return "", err
	}
	if len(req.Rows) == 0 && len(clears) > 0 {
		return fmt.Sprintf("Would clear %d cell(s) of %s", len(clears), req.Header[len(req.Header)-1]), nil
	}
	staged, err := resolveValueRows(ctx, tx, modelID, req)
	if err != nil {
		return "", err
	}
	// Refused as a file import's are: the assistant wrote 0.03 for a 3%
	// threshold, and the warning came only after the write.
	if w := h.percentFractionWarning(ctx, tx, staged); w != "" && !req.ValuesArePercentUnits {
		return "", fmt.Errorf("%s (if these really are percents under 1%%, pass \"values_are_percent_units\": true)", strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(w), "WARNING: ")))
	}
	return fmt.Sprintf("Would write %d value(s) into %s", len(staged), req.Header[len(req.Header)-1]), nil
}

// resolveValueRows checks the columns against the grid and resolves the rows
// as an import does; one row that does not resolve refuses the step.
func resolveValueRows(ctx context.Context, q importpkg.Querier, modelID string, req aiassistant.ValuesWriteRequest) ([]importpkg.StagingRow, error) {
	if err := importpkg.CheckGridColumns(ctx, q, req.GridID, req.Header); err != nil {
		return nil, err
	}
	staged, importErrs, err := importpkg.ResolveRows(ctx, q, modelID, req.RevisionID, req.Header, req.Rows)
	if err != nil {
		return nil, err
	}
	if len(importErrs) > 0 {
		rows := make([]map[string]any, 0, len(importErrs))
		for _, e := range importErrs {
			rows = append(rows, map[string]any{"row": e.RowNumber, "column": e.Column, "message": e.Message})
		}
		return nil, fmt.Errorf("values refused, nothing was written (row n is values[n-1]):\n%s", importErrorLines(rows, 10))
	}
	if len(staged) == 0 {
		return nil, fmt.Errorf("no values to write")
	}
	return staged, nil
}

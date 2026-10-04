package gateway

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
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
	staged, err := resolveValueRows(ctx, h.db.For(ctx), modelID, req)
	if err != nil {
		return "", err
	}
	if _, err := h.commitGridRows(ctx, act, modelID, req.RevisionID, staged, importpkg.ModeReplace, "ai_values"); err != nil {
		return "", err
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryDataChange, EventType: auditlog.EventCellWritten,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ResourceType: "metric", ResourceID: req.MetricID, RevisionID: req.RevisionID,
		Metadata: map[string]string{"model_id": modelID, "revision_id": req.RevisionID, "source": "ai_values", "values": strconv.Itoa(len(staged))},
	})
	return fmt.Sprintf("Wrote %d value(s) into %s", len(staged), req.Header[len(req.Header)-1]) + h.percentFractionWarning(ctx, h.db.For(ctx), staged), nil
}

// aiCheckWriteValues is aiWriteValues for the plan check: the same
// resolution on the check's transaction, nothing committed.
func (h *handler) aiCheckWriteValues(ctx context.Context, tx pgx.Tx, req aiassistant.ValuesWriteRequest) (string, error) {
	var modelID string
	if err := tx.QueryRow(ctx, `SELECT model_id::text FROM model.revision WHERE id=$1::uuid`, req.RevisionID).Scan(&modelID); err != nil {
		return "", fmt.Errorf("revision: %w", err)
	}
	staged, err := resolveValueRows(ctx, tx, modelID, req)
	if err != nil {
		return "", err
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

package gateway

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/metricformula"
)

// Text cells and cleared cells (migration 111). A text metric's cell holds
// free text — a planner's comment beside the number it explains, an owner —
// in fact_input.text_value, with value 0. Formulas do not read it
// (metricformula refuses the reference); grids, exports, imports, copies and
// the cell history carry it.

// cellWrite is what one POST /api/cells writes once its request is resolved:
// a number, a text, or nothing (a clear).
type cellWrite struct {
	value float64
	text  *string // set for a text metric
	clear bool
}

// resolveCellWrite turns a writeback request into the write it makes for a
// metric of format: a clear (clear, an empty text, an empty member), a text
// for a text metric, a number (a pick-list's member key) for any other.
func (h *handler) resolveCellWrite(ctx context.Context, req writebackReq, format string) (cellWrite, error) {
	if req.Clear {
		return cellWrite{clear: true}, nil
	}
	if format == metricformula.FormatText {
		if req.Text == nil {
			return cellWrite{}, fmt.Errorf("this is a text metric: send its text in \"text\" (\"\" clears the cell)")
		}
		t := strings.TrimRight(*req.Text, " \t\r\n")
		if t == "" {
			return cellWrite{clear: true}, nil
		}
		if len(t) > maxCellText {
			return cellWrite{}, fmt.Errorf("a text cell holds at most %d characters (this one has %d)", maxCellText, len(t))
		}
		return cellWrite{text: &t}, nil
	}
	if req.Text != nil {
		return cellWrite{}, fmt.Errorf("\"text\" is for a text metric; this metric's format is %q", cmp.Or(format, "number"))
	}
	if req.Member != nil && strings.TrimSpace(*req.Member) == "" {
		return cellWrite{clear: true}, nil
	}
	v, err := h.picklistValue(ctx, req.MetricID, req.Value, req.Member)
	if err != nil {
		return cellWrite{}, err
	}
	return cellWrite{value: v}, nil
}

// maxCellText caps one text cell: a comment, not a document.
const maxCellText = 4000

// clearCell empties one cell: its directly entered rows are deleted, and the
// archive trigger keeps them in fact_input_history with the reason
// "cleared". Rows a form or an import posted (source_ref) stay — they are
// that source's, and its next posting would bring them back anyway.
func (h *handler) clearCell(ctx context.Context, modelID, revisionID, metricID, dimMembers string) error {
	tx, err := h.db.For(ctx).Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL mvx.delete_reason = 'cleared'`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM runtime.fact_input
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid
		  AND dim_members = $4::jsonb AND source_ref IS NULL`,
		modelID, revisionID, metricID, dimMembers); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// loadTextCells reads the latest text of every cell of textMetrics, keyed
// as grid() keys cells ("metricId:code1[:code2...]" over the metric's own
// dimensions; a dimensionless metric by its bare ID). scopeSQL/scopeArgs are
// grid()'s fact filter ($3 on); hidden members and metrics are left out.
func (h *handler) loadTextCells(ctx context.Context, modelID, revisionID string, textMetrics []string,
	metricDims map[string][]string, hiddenByDim map[string]map[string]bool, metricRules map[string]string,
	scopeSQL string, scopeArgs []any) (map[string]string, error) {
	if len(textMetrics) == 0 {
		return nil, nil
	}
	args := append([]any{modelID, revisionID}, scopeArgs...)
	args = append(args, textMetrics)
	rows, err := h.db.Query(ctx, fmt.Sprintf(`
		SELECT DISTINCT ON (metric_id, dim_members) metric_id::text, dim_members::text, COALESCE(text_value, '')
		FROM runtime.fact_input
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND source_ref IS NULL
		  AND metric_id::text = ANY($%d::text[])%s
		ORDER BY metric_id, dim_members, entered_at DESC, id DESC`, len(args), scopeSQL), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var metricID, dmJSON, text string
		if err := rows.Scan(&metricID, &dmJSON, &text); err != nil {
			return nil, err
		}
		var dm map[string]string
		if json.Unmarshal([]byte(dmJSON), &dm) != nil || factRowHidden(dm, hiddenByDim) || metricRules[metricID] == "hidden" || text == "" {
			continue
		}
		if key, ok := textCellKey(metricID, metricDims[metricID], dm); ok {
			out[key] = text
		}
	}
	return out, rows.Err()
}

// textCellKey is a fact's cell key; false when the fact lacks one of the
// metric's dimensions.
func textCellKey(metricID string, ownDims []string, dm map[string]string) (string, bool) {
	if len(ownDims) == 0 {
		return metricID, true
	}
	codes := make([]string, 0, len(ownDims))
	for _, dimID := range ownDims {
		code, ok := dm[dimID]
		if !ok {
			return "", false
		}
		codes = append(codes, code)
	}
	return metricID + ":" + strings.Join(codes, ":"), true
}

// cellWriteAudit is the audit record's metadata for one cell write.
func cellWriteAudit(req writebackReq, write cellWrite) map[string]string {
	md := map[string]string{"model_id": req.ModelID, "revision_id": req.RevisionID}
	if write.clear {
		md["cleared"] = "true"
	}
	return md
}

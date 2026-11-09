package metricformula

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// RenameDimensionInFormulas carries a dimension's rename into what names it
// by text in its revision: every calculated metric's formula that reads it
// (formula.RenameDimension: a bare or braced name, a LOOKUP or criteria
// dimension, PARENT's argument, the dimension of dim.property — nothing
// else in the text changes), the highlight rules limited to its members
// ("dimension") and the data exports (an export's "dimensions",
// "pivot_dimension", "filters" keys and the "column_names" keys heading its
// column). Left on the old name, those formulas lost their rows or read 0
// with no error at the next recalculation, and an export dropped the
// filter (2026-09-27). Where a metric of the revision carries the old name
// too (a clash saved before such names were refused) a bare name is the
// metric's, so only dim.property references change there. Workflow
// conditions are left alone: the names they read are metrics and context
// variables. It runs after the dimension's own row is renamed, in the same
// transaction, and returns the IDs of the metrics whose formula it
// rewrote. Formula references to a dimension create no calc_dependency
// rows, and the rewritten formulas compute what they computed, so nothing
// needs recalculating.
func RenameDimensionInFormulas(ctx context.Context, q QueryExecer, dimensionID, oldName, newName string) ([]string, error) {
	oldName, newName = strings.TrimSpace(oldName), strings.TrimSpace(newName)
	if oldName == "" || newName == "" || strings.EqualFold(oldName, newName) {
		return nil, nil // names are matched without case
	}
	var modelID, revisionID string
	if err := q.QueryRow(ctx, `SELECT model_id::text, COALESCE(revision_id::text,'') FROM model.dimension_def WHERE id=$1::uuid`,
		dimensionID).Scan(&modelID, &revisionID); err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `
		SELECT m.id::text, m.formula,
		       EXISTS (SELECT 1 FROM model.metric_def o
		               WHERE o.model_id = m.model_id AND o.revision_id IS NOT DISTINCT FROM m.revision_id
		                 AND lower(o.name) = lower($3))
		FROM model.metric_def m
		WHERE m.model_id=$1::uuid AND m.is_input = false AND m.formula IS NOT NULL AND m.formula <> ''
		  AND ($2 = '' OR m.revision_id = NULLIF($2,'')::uuid)
		  AND `+resolvesToDimension+`
		FOR UPDATE OF m
	`, modelID, revisionID, oldName)
	if err != nil {
		return nil, err
	}
	type row struct {
		id, text string
		clash    bool
	}
	metrics, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.id, &x.text, &x.clash)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	var rewritten []string
	for _, m := range metrics {
		rename := formula.RenameDimension
		if m.clash {
			rename = formula.RenameDimensionProperties
		}
		next, changed := rename(m.text, oldName, newName)
		if !changed {
			continue
		}
		if _, err := q.Exec(ctx, `UPDATE model.metric_def SET formula=$2 WHERE id=$1::uuid`, m.id, next); err != nil {
			return rewritten, err
		}
		rewritten = append(rewritten, m.id)
	}
	if err := renameRuleDimension(ctx, q, modelID, revisionID, oldName, newName); err != nil {
		return rewritten, err
	}
	_, err = rewriteFileExports(ctx, q, modelID, revisionID, func(cfg map[string]any) bool {
		changed := renameInList(cfg, "dimensions", oldName, newName)
		changed = renameField(cfg, "pivot_dimension", oldName, newName) || changed
		changed = renameKey(cfg, "filters", oldName, newName) || changed
		changed = renameKey(cfg, "column_names", oldName, newName) || changed
		changed = renameKey(cfg, "column_names", oldName+" label", newName+" label") || changed
		return changed
	})
	return rewritten, err
}

// renameRuleDimension renames the dimension a highlight rule limits its
// members to, in the revision's metrics.
func renameRuleDimension(ctx context.Context, q QueryExecer, modelID, revisionID, oldName, newName string) error {
	rows, err := q.Query(ctx, `
		SELECT id::text, highlight_rules FROM model.metric_def
		WHERE model_id=$1::uuid AND revision_id IS NOT DISTINCT FROM NULLIF($2,'')::uuid AND highlight_rules <> '[]'::jsonb
		FOR UPDATE`, modelID, revisionID)
	if err != nil {
		return err
	}
	type row struct {
		id    string
		rules []byte
	}
	metrics, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.id, &x.rules)
		return x, err
	})
	if err != nil {
		return err
	}
	for _, m := range metrics {
		rules, ok := decodeObjects(m.rules)
		if !ok {
			continue
		}
		changed := false
		for _, r := range rules {
			changed = renameField(r, "dimension", oldName, newName) || changed
		}
		if !changed {
			continue
		}
		b, err := json.Marshal(rules)
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, `UPDATE model.metric_def SET highlight_rules=$2::jsonb WHERE id=$1::uuid`, m.id, string(b)); err != nil {
			return err
		}
	}
	return nil
}

// RenameMemberInFormulas carries a member code's rename into what names the
// code by text in its dimension's revision: every calculated metric's
// formula naming it (formula.RenameMemberCode — the literals the
// member-delete guard sees, a TIMESUM bound only for a metric whose grid
// carries this time dimension, plus "<>" criteria) and the data exports
// filtering by it or, pivoted on its dimension by code, heading a column
// with it. Left alone, a LOOKUP lost its row, a comparison or a criterion
// silently read 0 — or, for "<>", started counting the renamed member —
// and an export dropped the member (2026-09-27). It runs after the
// member's own row is re-keyed (modeledit.RekeyMemberCode) and returns the
// IDs of the metrics whose formula it rewrote; their values are what they
// were, under the new code.
func RenameMemberInFormulas(ctx context.Context, q QueryExecer, dimensionID, oldCode, newCode string) ([]string, error) {
	if oldCode == "" || newCode == "" || oldCode == newCode {
		return nil, nil
	}
	var modelID, revisionID, dimName, dimType string
	if err := q.QueryRow(ctx, `
		SELECT model_id::text, COALESCE(revision_id::text, ''), name, COALESCE(dimension_type, '')
		FROM model.dimension_def WHERE id=$1::uuid
	`, dimensionID).Scan(&modelID, &revisionID, &dimName, &dimType); err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `
		SELECT m.id::text, m.formula,
		       EXISTS (SELECT 1 FROM model.grid_metric gm
		               JOIN model.grid_dimension gd ON gd.grid_id = gm.grid_id
		               WHERE gm.metric_id = m.id AND gd.dimension_id = $4::uuid)
		FROM model.metric_def m
		WHERE m.model_id=$1::uuid AND m.is_input = false AND m.formula IS NOT NULL AND m.formula <> ''
		  AND ($2 = '' OR m.revision_id = NULLIF($2,'')::uuid)
		  AND `+resolvesToDimension+`
		FOR UPDATE OF m
	`, modelID, revisionID, dimName, dimensionID)
	if err != nil {
		return nil, err
	}
	type row struct {
		id, text string
		onGrid   bool
	}
	metrics, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.id, &x.text, &x.onGrid)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	var rewritten []string
	for _, m := range metrics {
		next, changed := formula.RenameMemberCode(m.text, dimName, oldCode, newCode, dimType == "time" && m.onGrid)
		if !changed {
			continue
		}
		if _, err := q.Exec(ctx, `UPDATE model.metric_def SET formula=$2 WHERE id=$1::uuid`, m.id, next); err != nil {
			return rewritten, err
		}
		rewritten = append(rewritten, m.id)
	}
	_, err = rewriteFileExports(ctx, q, modelID, revisionID, func(cfg map[string]any) bool {
		changed := false
		if filters, ok := cfg["filters"].(map[string]any); ok {
			for k, v := range filters {
				if !strings.EqualFold(strings.TrimSpace(k), dimName) {
					continue
				}
				if codes, ok := v.([]any); ok {
					for i, c := range codes {
						if s, ok := c.(string); ok && s == oldCode {
							codes[i] = newCode
							changed = true
						}
					}
				}
			}
		}
		// A pivot on the dimension heads a column with each member's code.
		pivot, _ := cfg["pivot_dimension"].(string)
		display, _ := cfg["member_display"].(string)
		layout, _ := cfg["layout"].(string)
		if strings.EqualFold(layout, "pivot") && strings.EqualFold(strings.TrimSpace(pivot), dimName) &&
			(display == "" || strings.EqualFold(display, "code")) {
			if names, ok := cfg["column_names"].(map[string]any); ok {
				if v, ok := names[oldCode]; ok {
					if _, taken := names[newCode]; !taken {
						delete(names, oldCode)
						names[newCode] = v
						changed = true
					}
				}
			}
		}
		return changed
	})
	return rewritten, err
}

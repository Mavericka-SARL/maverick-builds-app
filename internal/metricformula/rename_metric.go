package metricformula

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// RenameMetricInFormulas carries a metric's rename into the formulas that
// name it, in the metric's revision: every calculated metric's formula and
// every workflow condition (a condition step's {"formula": …}). Formulas
// name metrics by name, so a renamed metric left them on the old name: they
// still ran on their stored dependency edges until the next draft copy
// re-read them, and then every cell went blank with no error (the AI build of
// 2026-11-08; the developer's metric save did the same). Only name tokens
// change (formula.RenameIdent): string literals, functions of that name and
// everything else stay byte-for-byte, and calc_dependency rows are by id, so
// none changes. It runs after the metric's own row is renamed, in the same
// transaction where there is one, and returns how many formulas it rewrote.
func RenameMetricInFormulas(ctx context.Context, q QueryExecer, metricID, oldName, newName string) (int, error) {
	oldName, newName = strings.TrimSpace(oldName), strings.TrimSpace(newName)
	if oldName == "" || newName == "" || strings.EqualFold(oldName, newName) {
		return 0, nil // names are matched without case
	}
	var modelID, revisionID string
	if err := q.QueryRow(ctx, `SELECT model_id::text, COALESCE(revision_id::text,'') FROM model.metric_def WHERE id=$1::uuid`,
		metricID).Scan(&modelID, &revisionID); err != nil {
		return 0, err
	}
	rows, err := q.Query(ctx, `
		SELECT id::text, formula FROM model.metric_def
		WHERE model_id=$1::uuid AND revision_id IS NOT DISTINCT FROM NULLIF($2,'')::uuid
		  AND formula IS NOT NULL AND strpos(lower(formula), lower($3)) > 0`, modelID, revisionID, oldName)
	if err != nil {
		return 0, err
	}
	type row struct{ id, text string }
	metrics, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.id, &x.text)
		return x, err
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range metrics {
		next, changed := formula.RenameIdent(m.text, oldName, newName)
		if !changed {
			continue
		}
		if _, err := q.Exec(ctx, `UPDATE model.metric_def SET formula=$2 WHERE id=$1::uuid`, m.id, next); err != nil {
			return n, err
		}
		n++
	}
	if revisionID == "" {
		return n, nil
	}
	wrows, err := q.Query(ctx, `
		SELECT id::text, steps FROM workflow.workflow_def
		WHERE revision_id=$1::uuid AND strpos(lower(steps::text), lower($2)) > 0`, revisionID, oldName)
	if err != nil {
		return n, err
	}
	type wf struct {
		id    string
		steps []byte
	}
	defs, err := pgx.CollectRows(wrows, func(r pgx.CollectableRow) (wf, error) {
		var x wf
		err := r.Scan(&x.id, &x.steps)
		return x, err
	})
	if err != nil {
		return n, err
	}
	for _, d := range defs {
		var steps []map[string]any
		dec := json.NewDecoder(bytes.NewReader(d.steps))
		dec.UseNumber() // numbers in a step stay as written
		if dec.Decode(&steps) != nil {
			continue
		}
		changed := false
		for _, st := range steps {
			cond, ok := st["condition"].(map[string]any)
			if !ok {
				continue
			}
			text, ok := cond["formula"].(string)
			if !ok {
				continue
			}
			if next, c := formula.RenameIdent(text, oldName, newName); c {
				cond["formula"] = next
				changed = true
				n++
			}
		}
		if !changed {
			continue
		}
		b, err := json.Marshal(steps)
		if err != nil {
			return n, err
		}
		if _, err := q.Exec(ctx, `UPDATE workflow.workflow_def SET steps=$2::jsonb WHERE id=$1::uuid`, d.id, string(b)); err != nil {
			return n, err
		}
	}
	return n, nil
}

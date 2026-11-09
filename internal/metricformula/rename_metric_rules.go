package metricformula

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"
)

// RenameMetricInRules carries a metric's rename into what names it by text
// outside formulas, in the metric's revision: the highlight rules of every
// metric (a rule's "metric" and its "than"), the closed rules ("metric")
// and the data exports (an export's "metrics", and a "column_names" key
// heading the metric's column when the export heads columns by name).
// These name metrics as formulas do, so a revision copy or a model export
// carries them unchanged; a rename that left them on the old name made the
// grid silently stop tinting, the export silently drop the column, and a
// closed rule turned every write to its input into an error (2026-10-05).
// Formulas and workflow conditions are RenameMetricInFormulas's. Names match
// without case, as the grid and the write guard match them. It runs after
// the metric's own row is renamed, in the same transaction where there is
// one, and returns how many metrics and exports it rewrote.
func RenameMetricInRules(ctx context.Context, q QueryExecer, metricID, oldName, newName string) (int, error) {
	oldName, newName = strings.TrimSpace(oldName), strings.TrimSpace(newName)
	if oldName == "" || newName == "" || strings.EqualFold(oldName, newName) {
		return 0, nil
	}
	var modelID, revisionID string
	if err := q.QueryRow(ctx, `SELECT model_id::text, COALESCE(revision_id::text,'') FROM model.metric_def WHERE id=$1::uuid`,
		metricID).Scan(&modelID, &revisionID); err != nil {
		return 0, err
	}
	rows, err := q.Query(ctx, `
		SELECT id::text, highlight_rules, closed_when FROM model.metric_def
		WHERE model_id=$1::uuid AND revision_id IS NOT DISTINCT FROM NULLIF($2,'')::uuid
		  AND (highlight_rules <> '[]'::jsonb OR closed_when <> '[]'::jsonb)
		FOR UPDATE`, modelID, revisionID)
	if err != nil {
		return 0, err
	}
	type row struct {
		id                string
		highlight, closed []byte
	}
	metrics, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.id, &x.highlight, &x.closed)
		return x, err
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, m := range metrics {
		highlight, hOK := decodeObjects(m.highlight)
		closed, cOK := decodeObjects(m.closed)
		changed := false
		if hOK {
			for _, r := range highlight {
				changed = renameField(r, "metric", oldName, newName) || changed
				changed = renameField(r, "than", oldName, newName) || changed
			}
		}
		if cOK {
			for _, r := range closed {
				changed = renameField(r, "metric", oldName, newName) || changed
			}
		}
		if !changed {
			continue
		}
		hb, cb := m.highlight, m.closed
		if hOK {
			if hb, err = json.Marshal(highlight); err != nil {
				return n, err
			}
		}
		if cOK {
			if cb, err = json.Marshal(closed); err != nil {
				return n, err
			}
		}
		if _, err := q.Exec(ctx, `UPDATE model.metric_def SET highlight_rules=$2::jsonb, closed_when=$3::jsonb WHERE id=$1::uuid`,
			m.id, string(hb), string(cb)); err != nil {
			return n, err
		}
		n++
	}
	exports, err := rewriteFileExports(ctx, q, modelID, revisionID, func(cfg map[string]any) bool {
		changed := renameInList(cfg, "metrics", oldName, newName)
		if display, _ := cfg["metric_display"].(string); display == "" || strings.EqualFold(display, "name") {
			changed = renameKey(cfg, "column_names", oldName, newName) || changed
		}
		return changed
	})
	return n + exports, err
}

// decodeObjects decodes a JSON array of objects, numbers kept as written.
func decodeObjects(raw []byte) ([]map[string]any, bool) {
	var list []map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&list) != nil {
		return nil, false
	}
	return list, true
}

// renameField sets obj[key] to newName when it is a text equal to oldName
// without case.
func renameField(obj map[string]any, key, oldName, newName string) bool {
	if v, ok := obj[key].(string); ok && strings.EqualFold(strings.TrimSpace(v), oldName) {
		obj[key] = newName
		return true
	}
	return false
}

// renameInList replaces the texts of list obj[key] equal to oldName
// without case.
func renameInList(obj map[string]any, key, oldName, newName string) bool {
	list, ok := obj[key].([]any)
	if !ok {
		return false
	}
	changed := false
	for i, v := range list {
		if s, ok := v.(string); ok && strings.EqualFold(strings.TrimSpace(s), oldName) {
			list[i] = newName
			changed = true
		}
	}
	return changed
}

// renameKey re-keys the entries of map obj[key] whose key equals oldName
// without case.
func renameKey(obj map[string]any, key, oldName, newName string) bool {
	m, ok := obj[key].(map[string]any)
	if !ok {
		return false
	}
	changed := false
	for k, v := range m {
		if k != newName && strings.EqualFold(strings.TrimSpace(k), oldName) {
			delete(m, k)
			m[newName] = v
			changed = true
		}
	}
	return changed
}

// rewriteFileExports applies edit to the config of every data export
// ("file_export" integration) of the revision, and stores the configs it
// changed. It returns how many it changed.
func rewriteFileExports(ctx context.Context, q QueryExecer, modelID, revisionID string, edit func(cfg map[string]any) bool) (int, error) {
	rows, err := q.Query(ctx, `
		SELECT id::text, config FROM model.integration_def
		WHERE model_id=$1::uuid AND revision_id IS NOT DISTINCT FROM NULLIF($2,'')::uuid AND type='file_export'
		FOR UPDATE`, modelID, revisionID)
	if err != nil {
		return 0, err
	}
	type row struct {
		id  string
		cfg []byte
	}
	defs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.id, &x.cfg)
		return x, err
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, d := range defs {
		var cfg map[string]any
		dec := json.NewDecoder(bytes.NewReader(d.cfg))
		dec.UseNumber()
		if dec.Decode(&cfg) != nil || cfg == nil || !edit(cfg) {
			continue
		}
		b, err := json.Marshal(cfg)
		if err != nil {
			return n, err
		}
		if _, err := q.Exec(ctx, `UPDATE model.integration_def SET config=$2::jsonb WHERE id=$1::uuid`, d.id, string(b)); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

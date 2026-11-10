package metricformula

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
//
// It also carries the rename into what names the metric as a value
// (renameMetricValues): a grid loader's column map and a connector's mapping
// into a grid, a record's field of type metric, and the Metric fields of the
// revision's workflow runs. Left on the old name, a saved import failed, a
// record named a metric that no longer existed, and a run's condition stopped
// reading its metric (2026-11-09).
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
	if err != nil {
		return n + exports, err
	}
	values, err := renameMetricValues(ctx, q, modelID, revisionID, oldName, newName)
	return n + exports + values, err
}

// renameMetricValues carries a metric rename into what holds the metric's
// name as a value, in its model's revision, and returns how many rows it
// rewrote:
//   - a grid loader's column map ({source column: metric name}, csv_import
//     and google_sheets) and a connector's mapping into a grid (each field's
//     "target");
//   - each record's field of type metric;
//   - the Metric fields of the workflow runs of the revision's workflows,
//     and of the application's own workflows when the revision is the
//     model's active one. A run of another revision keeps its name: it
//     reads that revision. The write guard's lock follows a metric by its
//     lineage whichever name a run holds (writeguard.loadWorkflowLocks).
func renameMetricValues(ctx context.Context, q QueryExecer, modelID, revisionID, oldName, newName string) (int, error) {
	n, err := rewriteIntegrationConfigs(ctx, q, modelID, revisionID, []string{"csv_import", "google_sheets", "rest_api"},
		func(targetType string, cfg map[string]any) bool {
			changed := false
			if targetType == "grid" {
				changed = renameValues(cfg, "column_map", oldName, newName)
			}
			if t, _ := cfg["target_type"].(string); t == "grid" {
				if mapping, ok := cfg["mapping"].(map[string]any); ok {
					if fields, ok := mapping["fields"].([]any); ok {
						for _, f := range fields {
							if obj, ok := f.(map[string]any); ok {
								changed = renameField(obj, "target", oldName, newName) || changed
							}
						}
					}
				}
			}
			return changed
		})
	if err != nil {
		return n, err
	}
	// Records: a field of type metric holds the metric's name.
	tag, err := q.Exec(ctx, `
		UPDATE runtime.form_record r
		SET data = r.data || (
			SELECT jsonb_object_agg(fe->>'name', to_jsonb($4::text))
			FROM jsonb_array_elements(fd.fields) fe
			WHERE fe->>'type' = 'metric' AND lower(btrim(r.data->>(fe->>'name'))) = lower($3))
		FROM model.form_def fd
		WHERE fd.id = r.form_id AND fd.model_id = $1::uuid AND fd.revision_id IS NOT DISTINCT FROM NULLIF($2,'')::uuid
		  AND jsonb_typeof(fd.fields) = 'array'
		  AND EXISTS (SELECT 1 FROM jsonb_array_elements(fd.fields) fe
		              WHERE fe->>'type' = 'metric' AND lower(btrim(r.data->>(fe->>'name'))) = lower($3))`,
		modelID, revisionID, oldName, newName)
	if err != nil {
		return n, fmt.Errorf("rename the metric in records: %w", err)
	}
	n += int(tag.RowsAffected())
	runs, err := RenameWorkflowRunValues(ctx, q, modelID, revisionID, "Metric", "", oldName, newName)
	return n + runs, err
}

// RenameWorkflowRunValues sets the context fields of type dataType that hold
// oldValue — without case — to newValue, and returns how many runs it
// rewrote. The run's own snapshot of its fields decides their types. A
// field of dimension dimID ("Dimension member") names that dimension
// exactly, so every run holding it is rewritten; with no dimID (a "Metric"
// field holds a name) the runs are those of modelID's revision's workflows
// and, when the revision is the model's active one, of its application's own
// workflows.
func RenameWorkflowRunValues(ctx context.Context, q QueryExecer, modelID, revisionID, dataType, dimID, oldValue, newValue string) (int, error) {
	tag, err := q.Exec(ctx, `
		WITH scope AS (
			SELECT wd.id FROM workflow.workflow_def wd JOIN core.model cm ON cm.id = $1::uuid
			WHERE (wd.revision_id IS NOT NULL AND wd.revision_id = NULLIF($2,'')::uuid)
			   OR (wd.revision_id IS NULL AND wd.application_id = cm.application_id
			       AND cm.active_revision_id IS NOT DISTINCT FROM NULLIF($2,'')::uuid)
		), hit AS (
			SELECT wi.id, jsonb_object_agg(v->>'key', to_jsonb($6::text)) AS patch
			FROM workflow.workflow_instance wi
			JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
			CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(COALESCE(wi.context_schema_snapshot, wd.context_schema)) = 'array'
			                                             THEN COALESCE(wi.context_schema_snapshot, wd.context_schema) ELSE '[]'::jsonb END) v
			WHERE v->>'data_type' = $3
			  AND (($4 <> '' AND v->>'dimension_id' = $4) OR ($4 = '' AND wd.id IN (SELECT id FROM scope)))
			  AND jsonb_typeof(wi.context) = 'object'
			  AND lower(btrim(wi.context->>(v->>'key'))) = lower($5)
			GROUP BY wi.id
		)
		UPDATE workflow.workflow_instance wi SET context = wi.context || hit.patch
		FROM hit WHERE hit.id = wi.id`, modelID, revisionID, dataType, dimID, oldValue, newValue)
	if err != nil {
		return 0, fmt.Errorf("rename %s fields of workflow runs: %w", dataType, err)
	}
	return int(tag.RowsAffected()), nil
}

// renameValues replaces the values of map obj[key] equal to oldName without
// case.
func renameValues(obj map[string]any, key, oldName, newName string) bool {
	m, ok := obj[key].(map[string]any)
	if !ok {
		return false
	}
	changed := false
	for k, v := range m {
		if s, ok := v.(string); ok && strings.EqualFold(strings.TrimSpace(s), oldName) {
			m[k] = newName
			changed = true
		}
	}
	return changed
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
	return rewriteIntegrationConfigs(ctx, q, modelID, revisionID, []string{"file_export"},
		func(_ string, cfg map[string]any) bool { return edit(cfg) })
}

// rewriteIntegrationConfigs applies edit — given the row's target_type — to
// the config of every integration of the revision whose type is one of
// types, and stores the configs it changed. It returns how many it changed.
func rewriteIntegrationConfigs(ctx context.Context, q QueryExecer, modelID, revisionID string, types []string, edit func(targetType string, cfg map[string]any) bool) (int, error) {
	rows, err := q.Query(ctx, `
		SELECT id::text, COALESCE(target_type, ''), config FROM model.integration_def
		WHERE model_id=$1::uuid AND revision_id IS NOT DISTINCT FROM NULLIF($2,'')::uuid AND type = ANY($3) AND config IS NOT NULL
		FOR UPDATE`, modelID, revisionID, types)
	if err != nil {
		return 0, err
	}
	type row struct {
		id, targetType string
		cfg            []byte
	}
	defs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.id, &x.targetType, &x.cfg)
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
		if dec.Decode(&cfg) != nil || cfg == nil || !edit(d.targetType, cfg) {
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

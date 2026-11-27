package workflow

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/workflow/assignee"
)

// A step may name the person who decides it from the run's own data
// (assignee_field, owner decision 2026-11-27): a path from a context
// variable holding a member of a dimension a table keeps — or "record", the
// run's record — through the dimension fields of the tables keeping those
// dimensions, to a person field. "project.country.manager" is the manager
// of the country of the run's project: the Senegal plan goes to Senegal's
// country manager, not to every country manager.
//
// The person is resolved when the step becomes someone's task, and is its
// assignee only if they hold one of the step's roles (the boundary of
// package assignee): a person field anyone may edit then cannot route an
// approval to them. An empty or unknown path step, or a person without the
// role, leaves the step to its roles, as before. A named step is shown to,
// decided by and reminded to that person — and the application's
// administrators (assignee.NamedSQL).

// AssignNamedPeople resolves the named person of each step of instanceID
// that just became a task (in progress, not yet announced, no assignee).
func (s *Store) AssignNamedPeople(ctx context.Context, instanceID string) {
	rows, err := s.db.Query(ctx, `
		SELECT ws.id::text, step_def.elem->>'assignee_field',
		       COALESCE(`+assignee.StepRolesSQL("step_def.elem")+`, '[]'::jsonb)::text,
		       wd.application_id::text, COALESCE(wi.context, '{}'::jsonb)::text,
		       COALESCE(wi.context_schema_snapshot, wd.context_schema, '[]'::jsonb)::text
		FROM workflow.workflow_step ws
		JOIN workflow.workflow_instance wi ON wi.id = ws.instance_id
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		CROSS JOIN LATERAL (
		    SELECT elem FROM jsonb_array_elements(COALESCE(wi.steps_snapshot, wd.steps)) AS elem
		    WHERE elem->>'id' = ws.step_def_id LIMIT 1
		) step_def
		WHERE ws.instance_id = $1::uuid AND ws.status = 'in_progress'
		  AND ws.assignee_user_id IS NULL AND ws.task_notified_at IS NULL
		  AND COALESCE(step_def.elem->>'assignee_field', '') <> ''`, instanceID)
	if err != nil {
		s.log.Warn().Err(err).Str("instance", instanceID).Msg("finding named assignees failed")
		return
	}
	type named struct{ step, path, roles, app, vars, schema string }
	var steps []named
	for rows.Next() {
		var n named
		if err := rows.Scan(&n.step, &n.path, &n.roles, &n.app, &n.vars, &n.schema); err != nil {
			rows.Close()
			return
		}
		steps = append(steps, n)
	}
	rows.Close()
	for _, n := range steps {
		var vars map[string]any
		_ = json.Unmarshal([]byte(n.vars), &vars)
		var schema []startContextVarDef
		_ = json.Unmarshal([]byte(n.schema), &schema)
		user := s.resolvePersonPath(ctx, n.path, vars, schema)
		if user == "" {
			continue
		}
		var eligible bool
		if err := s.db.QueryRow(ctx, `SELECT `+assignee.SQL("$1::uuid", "$2::jsonb", "$3::uuid"), n.app, n.roles, user).Scan(&eligible); err != nil || !eligible {
			s.log.Info().Str("step", n.step).Str("assignee_field", n.path).Msg("the person named for the step holds none of its roles: the step stays with its roles")
			continue
		}
		if _, err := s.db.Exec(ctx, `UPDATE workflow.workflow_step SET assignee_user_id = $2::uuid WHERE id = $1::uuid AND assignee_user_id IS NULL`, n.step, user); err != nil {
			s.log.Warn().Err(err).Str("step", n.step).Msg("naming the step's assignee failed")
		}
	}
}

// resolvePersonPath follows path through the run's data to a person's user
// id; "" when any step of it finds nothing.
func (s *Store) resolvePersonPath(ctx context.Context, path string, vars map[string]any, schema []startContextVarDef) string {
	var segs []string
	for _, p := range strings.Split(path, ".") {
		if p = strings.TrimSpace(p); p != "" {
			segs = append(segs, p)
		}
	}
	if len(segs) == 1 {
		segs = append([]string{"record"}, segs...) // a field of the run's record
	}
	if len(segs) < 2 {
		return ""
	}
	var formID string
	var data map[string]any
	if strings.EqualFold(segs[0], "record") {
		recordID := varText(vars, "record_id")
		if recordID == "" {
			return ""
		}
		var raw []byte
		if err := s.db.QueryRow(ctx, `SELECT form_id::text, data FROM runtime.form_record WHERE id::text = $1`, recordID).Scan(&formID, &raw); err != nil {
			return ""
		}
		_ = json.Unmarshal(raw, &data)
	} else {
		dimID := ""
		for _, v := range schema {
			if strings.EqualFold(v.Key, segs[0]) {
				dimID = v.DimensionID
			}
		}
		code := varText(vars, segs[0])
		if dimID == "" || code == "" {
			return ""
		}
		formID, data = s.keptRecord(ctx, dimID, code)
	}
	for _, seg := range segs[1 : len(segs)-1] {
		f, ok := s.formField(ctx, formID, seg)
		if !ok || f.Type != "dimension" || f.DimensionID == "" {
			return ""
		}
		code, _ := data[f.Name].(string)
		if formID, data = s.keptRecord(ctx, f.DimensionID, code); formID == "" {
			return ""
		}
	}
	f, ok := s.formField(ctx, formID, segs[len(segs)-1])
	if !ok || f.Type != "person" {
		return ""
	}
	user, _ := data[f.Name].(string)
	return strings.TrimSpace(user)
}

// keptRecord is the record of the table keeping dimID whose code is code.
func (s *Store) keptRecord(ctx context.Context, dimID, code string) (string, map[string]any) {
	if dimID == "" || strings.TrimSpace(code) == "" {
		return "", nil
	}
	var formID string
	var raw []byte
	if err := s.db.QueryRow(ctx, `
		SELECT f.id::text, r.data
		FROM model.form_def f
		CROSS JOIN LATERAL (
		    SELECT COALESCE(
		        (SELECT e->>'name' FROM jsonb_array_elements(f.fields) e WHERE e->>'type' = 'autocode' LIMIT 1),
		        (SELECT e->>'name' FROM jsonb_array_elements(f.fields) e WHERE e->>'code' = 'true' LIMIT 1)) AS code_field
		) cf
		JOIN runtime.form_record r ON r.form_id = f.id AND r.data->>cf.code_field = $2
		WHERE f.keeps_dimension_id::text = $1
		LIMIT 1`, dimID, code).Scan(&formID, &raw); err != nil {
		return "", nil
	}
	var data map[string]any
	_ = json.Unmarshal(raw, &data)
	return formID, data
}

type pathField struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	DimensionID string `json:"dimension_id"`
}

// formField is formID's field named name (any case).
func (s *Store) formField(ctx context.Context, formID, name string) (pathField, bool) {
	var raw []byte
	if err := s.db.QueryRow(ctx, `SELECT COALESCE(fields, '[]'::jsonb) FROM model.form_def WHERE id::text = $1`, formID).Scan(&raw); err != nil {
		return pathField{}, false
	}
	var fields []pathField
	_ = json.Unmarshal(raw, &fields)
	for _, f := range fields {
		if strings.EqualFold(f.Name, name) {
			return f, true
		}
	}
	return pathField{}, false
}

func varText(vars map[string]any, key string) string {
	for k, v := range vars {
		if strings.EqualFold(k, key) {
			if s, ok := v.(string); ok {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

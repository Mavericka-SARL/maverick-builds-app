package assignee

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/mavericks-engine/mavericks/pkg/dbx"
)

// Renamed is what RenameReferences carried a business role's new name into.
type Renamed struct {
	Workflows  []string `json:"workflows"`  // workflow definitions, by name, once each
	Runs       int      `json:"runs"`       // workflow runs whose steps snapshot named it
	Tables     []string `json:"tables"`     // tables of records it may create in
	Dimensions []string `json:"dimensions"` // dimensions it may edit
}

// Empty reports whether nothing named the role.
func (r Renamed) Empty() bool {
	return len(r.Workflows) == 0 && r.Runs == 0 && len(r.Tables) == 0 && len(r.Dimensions) == 0
}

// Summary names what followed the rename, for a sentence:
// "workflows Budget Approval and Expense; 2 runs; table of records Deals".
func (r Renamed) Summary() string {
	var parts []string
	plural := func(n int, one, many string) string {
		if n == 1 {
			return one
		}
		return many
	}
	if len(r.Workflows) > 0 {
		parts = append(parts, plural(len(r.Workflows), "workflow ", "workflows ")+joinAnd(r.Workflows))
	}
	if r.Runs > 0 {
		parts = append(parts, strconv.Itoa(r.Runs)+plural(r.Runs, " run", " runs"))
	}
	if len(r.Tables) > 0 {
		parts = append(parts, plural(len(r.Tables), "table of records ", "tables of records ")+joinAnd(r.Tables))
	}
	if len(r.Dimensions) > 0 {
		parts = append(parts, plural(len(r.Dimensions), "dimension ", "dimensions ")+joinAnd(r.Dimensions))
	}
	return strings.Join(parts, "; ")
}

// RenameReferences carries the rename of a business role of workspace wsID
// from oldName to newName into everything that names it, in every revision:
// workflow definitions (step assignees, notification recipients, who may
// start and reopen), workflow runs' steps snapshots (so their waiting tasks
// keep reaching the role's members), tables of records' create_roles and
// dimensions' editor_roles. Roles are matched by name, so without this a
// renamed role silently stops being reached.
//
// The boundary is stepAssigneeSQL's: the applications of wsID, and the
// tenant-level applications of its tenant — unless another workspace of the
// tenant still holds a role named oldName, which those applications' names
// then go on to mean. Call it in the transaction that renames the role.
func RenameReferences(ctx context.Context, db dbx.DB, wsID, oldName, newName string) (Renamed, error) {
	out := Renamed{Workflows: []string{}, Tables: []string{}, Dimensions: []string{}}
	if oldName == newName {
		return out, nil
	}
	var shadowed bool
	if err := db.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM identity.business_role br
		    JOIN core.workspace w ON w.id = br.workspace_id
		    WHERE br.name = $2 AND br.workspace_id <> $1::uuid
		      AND w.customer_id = (SELECT customer_id FROM core.workspace WHERE id = $1::uuid))`,
		wsID, oldName).Scan(&shadowed); err != nil {
		return out, fmt.Errorf("rename role references: %w", err)
	}
	apps := `SELECT a.id FROM core.application a
		WHERE a.workspace_id = $1::uuid
		   OR (NOT $2::bool AND a.workspace_id IS NULL
		       AND a.customer_id = (SELECT customer_id FROM core.workspace WHERE id = $1::uuid))`
	models := `SELECT mo.id FROM core.model mo WHERE mo.application_id IN (` + apps + `)`

	// Workflow definitions.
	type defRow struct {
		id, name                  string
		steps, starters, reopener []byte
	}
	var defs []defRow
	rows, err := db.Query(ctx, `
		SELECT wd.id::text, wd.name, wd.steps, wd.start_roles, wd.reopen_roles
		FROM workflow.workflow_def wd
		WHERE wd.application_id IN (`+apps+`)
		  AND (COALESCE(wd.start_roles, '[]'::jsonb) ? $3
		       OR COALESCE(wd.reopen_roles, '[]'::jsonb) ? $3
		       OR `+stepsNameRoleSQL("wd.steps", "$3")+`)
		ORDER BY wd.name, wd.id
		FOR UPDATE OF wd`, wsID, shadowed, oldName)
	if err != nil {
		return out, fmt.Errorf("rename role references: workflows: %w", err)
	}
	for rows.Next() {
		var d defRow
		if err := rows.Scan(&d.id, &d.name, &d.steps, &d.starters, &d.reopener); err != nil {
			rows.Close()
			return out, fmt.Errorf("rename role references: workflows: %w", err)
		}
		defs = append(defs, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("rename role references: workflows: %w", err)
	}
	for _, d := range defs {
		steps, c1 := renameInSteps(d.steps, oldName, newName)
		starters, c2 := renameInList(d.starters, oldName, newName)
		reopener, c3 := renameInList(d.reopener, oldName, newName)
		if !c1 && !c2 && !c3 {
			continue
		}
		if _, err := db.Exec(ctx, `
			UPDATE workflow.workflow_def SET steps = $2::jsonb, start_roles = $3::jsonb, reopen_roles = $4::jsonb
			WHERE id = $1::uuid`, d.id, string(steps), string(starters), string(reopener)); err != nil {
			return out, fmt.Errorf("rename role references: workflow %s: %w", d.name, err)
		}
		out.Workflows = appendOnce(out.Workflows, d.name)
	}

	// Workflow runs: what their waiting tasks are matched against.
	type runRow struct {
		id    string
		steps []byte
	}
	var runs []runRow
	rows, err = db.Query(ctx, `
		SELECT wi.id::text, wi.steps_snapshot
		FROM workflow.workflow_instance wi
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		WHERE wd.application_id IN (`+apps+`)
		  AND wi.steps_snapshot IS NOT NULL
		  AND `+stepsNameRoleSQL("wi.steps_snapshot", "$3")+`
		FOR UPDATE OF wi`, wsID, shadowed, oldName)
	if err != nil {
		return out, fmt.Errorf("rename role references: runs: %w", err)
	}
	for rows.Next() {
		var r runRow
		if err := rows.Scan(&r.id, &r.steps); err != nil {
			rows.Close()
			return out, fmt.Errorf("rename role references: runs: %w", err)
		}
		runs = append(runs, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("rename role references: runs: %w", err)
	}
	for _, r := range runs {
		steps, changed := renameInSteps(r.steps, oldName, newName)
		if !changed {
			continue
		}
		if _, err := db.Exec(ctx, `UPDATE workflow.workflow_instance SET steps_snapshot = $2::jsonb WHERE id = $1::uuid`,
			r.id, string(steps)); err != nil {
			return out, fmt.Errorf("rename role references: run %s: %w", r.id, err)
		}
		out.Runs++
	}

	// Tables of records.
	// A table names roles in create_roles, delete_roles (migration 192),
	// and in its fields: a person field's roles and a transition's
	// (crudapp/field_rules.go).
	type listRow struct {
		id, name                 string
		list, deleteList, fields []byte
	}
	var tables []listRow
	rows, err = db.Query(ctx, `
		SELECT fd.id::text, fd.label, fd.create_roles, fd.delete_roles, COALESCE(fd.fields, '[]'::jsonb)
		FROM model.form_def fd
		WHERE fd.model_id IN (`+models+`)
		  AND (jsonb_typeof(fd.create_roles) = 'array' AND fd.create_roles ? $3
		    OR jsonb_typeof(fd.delete_roles) = 'array' AND fd.delete_roles ? $3
		    OR `+FieldsNameRole("fd.fields", "$3")+`)
		ORDER BY fd.label, fd.id
		FOR UPDATE OF fd`, wsID, shadowed, oldName)
	if err != nil {
		return out, fmt.Errorf("rename role references: tables: %w", err)
	}
	for rows.Next() {
		var t listRow
		if err := rows.Scan(&t.id, &t.name, &t.list, &t.deleteList, &t.fields); err != nil {
			rows.Close()
			return out, fmt.Errorf("rename role references: tables: %w", err)
		}
		tables = append(tables, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("rename role references: tables: %w", err)
	}
	for _, t := range tables {
		list, c1 := renameInList(t.list, oldName, newName)
		deleteList, c2 := renameInList(t.deleteList, oldName, newName)
		fields, c3 := renameInFields(t.fields, oldName, newName)
		if !c1 && !c2 && !c3 {
			continue
		}
		if _, err := db.Exec(ctx, `UPDATE model.form_def SET create_roles = $2::jsonb, delete_roles = $3::jsonb, fields = $4::jsonb WHERE id = $1::uuid`,
			t.id, string(list), string(deleteList), string(fields)); err != nil {
			return out, fmt.Errorf("rename role references: table %s: %w", t.name, err)
		}
		out.Tables = appendOnce(out.Tables, t.name)
	}

	// Dimension editors.
	type dimRow struct {
		id, name string
		editors  []string
	}
	var dims []dimRow
	rows, err = db.Query(ctx, `
		SELECT dd.id::text, dd.name, dd.editor_roles
		FROM model.dimension_def dd
		WHERE dd.model_id IN (`+models+`) AND $3 = ANY(dd.editor_roles)
		ORDER BY dd.name, dd.id
		FOR UPDATE OF dd`, wsID, shadowed, oldName)
	if err != nil {
		return out, fmt.Errorf("rename role references: dimensions: %w", err)
	}
	for rows.Next() {
		var d dimRow
		if err := rows.Scan(&d.id, &d.name, &d.editors); err != nil {
			rows.Close()
			return out, fmt.Errorf("rename role references: dimensions: %w", err)
		}
		dims = append(dims, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("rename role references: dimensions: %w", err)
	}
	for _, d := range dims {
		editors := make([]string, 0, len(d.editors))
		for _, e := range d.editors {
			if e == oldName {
				e = newName
			}
			editors = appendOnce(editors, e)
		}
		if _, err := db.Exec(ctx, `UPDATE model.dimension_def SET editor_roles = $2 WHERE id = $1::uuid`, d.id, editors); err != nil {
			return out, fmt.Errorf("rename role references: dimension %s: %w", d.name, err)
		}
		out.Dimensions = appendOnce(out.Dimensions, d.name)
	}
	return out, nil
}

// renameInList replaces oldName by newName in a JSON array of role names,
// leaving every other element as it is and newName in it once. Anything but
// an array comes back unchanged.
func renameInList(raw []byte, oldName, newName string) ([]byte, bool) {
	var items []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &items) != nil || items == nil {
		return raw, false
	}
	changed, haveNew := false, false
	out := make([]json.RawMessage, 0, len(items))
	for _, it := range items {
		var s string
		if json.Unmarshal(it, &s) == nil {
			if s == oldName {
				s, changed = newName, true
				it, _ = json.Marshal(s)
			}
			if s == newName {
				if haveNew {
					continue
				}
				haveNew = true
			}
		}
		out = append(out, it)
	}
	if !changed {
		return raw, false
	}
	b, err := json.Marshal(out)
	if err != nil {
		return raw, false
	}
	return b, true
}

// renameInSteps replaces oldName by newName in a steps array: each step's
// assignee_roles and its notification's recipient_role. Every other key of
// a step is kept as it is.
func renameInSteps(raw []byte, oldName, newName string) ([]byte, bool) {
	var steps []map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &steps) != nil {
		return raw, false
	}
	changed := false
	for _, st := range steps {
		if v, ok := st["assignee_roles"]; ok {
			if nv, ch := renameInList(v, oldName, newName); ch {
				st["assignee_roles"], changed = nv, true
			}
		}
		if v, ok := st["notification"]; ok {
			var n map[string]json.RawMessage
			if json.Unmarshal(v, &n) == nil && n != nil {
				var recipient string
				if json.Unmarshal(n["recipient_role"], &recipient) == nil && recipient == oldName {
					n["recipient_role"], _ = json.Marshal(newName)
					if b, err := json.Marshal(n); err == nil {
						st["notification"], changed = b, true
					}
				}
			}
		}
	}
	if !changed {
		return raw, false
	}
	b, err := json.Marshal(steps)
	if err != nil {
		return raw, false
	}
	return b, true
}

func appendOnce(list []string, s string) []string {
	for _, x := range list {
		if x == s {
			return list
		}
	}
	return append(list, s)
}

func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// FieldsNameRole is a predicate, true when the fields (a table's jsonb
// array, the SQL expression fields) name the role the SQL expression role
// yields: a person field's roles or a transition's.
func FieldsNameRole(fields, role string) string {
	return "(jsonb_path_exists(" + fields + ", '$[*].roles[*] ? (@ == $n)', jsonb_build_object('n', " + role + "))" +
		" OR jsonb_path_exists(" + fields + ", '$[*].transitions[*].roles[*] ? (@ == $n)', jsonb_build_object('n', " + role + ")))"
}

// renameInFields replaces oldName by newName in the role lists of a
// table's fields (a person field's roles, each transition's roles),
// reporting whether any held it. Fields it cannot read are left as they are.
func renameInFields(raw []byte, oldName, newName string) ([]byte, bool) {
	var fields []map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		return raw, false
	}
	changed := false
	swap := func(v any) any {
		list, ok := v.([]any)
		if !ok {
			return v
		}
		for i, r := range list {
			if s, ok := r.(string); ok && s == oldName {
				list[i] = newName
				changed = true
			}
		}
		return list
	}
	for _, f := range fields {
		if roles, ok := f["roles"]; ok {
			f["roles"] = swap(roles)
		}
		if ts, ok := f["transitions"].([]any); ok {
			for _, t := range ts {
				if tm, ok := t.(map[string]any); ok {
					if roles, ok := tm["roles"]; ok {
						tm["roles"] = swap(roles)
					}
				}
			}
		}
	}
	if !changed {
		return raw, false
	}
	out, err := json.Marshal(fields)
	if err != nil {
		return raw, false
	}
	return out, true
}

package assignee

import (
	"context"
	"fmt"
	"strings"

	"github.com/mavericks-engine/mavericks/pkg/dbx"
)

// ModelUse is a model that uses a business role.
type ModelUse struct {
	ModelID         string `json:"model_id"`
	ModelName       string `json:"model_name"`
	ApplicationName string `json:"application_name"`
}

// Label is how a person tells the model apart: its name, and its
// application's name when that differs.
func (m ModelUse) Label() string {
	if m.ApplicationName == "" || strings.EqualFold(m.ApplicationName, m.ModelName) {
		return m.ModelName
	}
	return m.ModelName + " (" + m.ApplicationName + ")"
}

// Usage returns, per business role of workspace wsID (only roleIDs when it
// names any), the models that use the role in any of their revisions:
//
//   - dashboards the role may open, and its access rules on a model's
//     dimension members and metrics (held by id);
//   - workflows whose steps name it as assignee or notification recipient,
//     or that it may start or reopen, and running workflows whose steps
//     snapshot names it (the definition may have changed since they
//     started); records tables it may create in; and dimensions it may edit
//     (held by name, within the boundary stepAssigneeSQL draws: an
//     application of this workspace, or a tenant-level application of its
//     tenant).
//
// Business roles belong to the workspace, so every model of its
// applications shares them; this is what tells a model's own roles from
// another model's. A role no model uses has no entry.
func Usage(ctx context.Context, db dbx.DB, wsID string, roleIDs ...string) (map[string][]ModelUse, error) {
	var ids any
	if len(roleIDs) > 0 {
		ids = roleIDs
	}
	rows, err := db.Query(ctx, strings.NewReplacer(
		"{def_steps_name_role}", stepsNameRoleSQL("wd.steps", "r.name"),
		"{run_steps_name_role}", stepsNameRoleSQL("wi.steps_snapshot", "r.name"),
	).Replace(`
		WITH roles AS (
		    SELECT br.id, br.name
		    FROM identity.business_role br
		    WHERE br.workspace_id = $1::uuid
		      AND ($2::uuid[] IS NULL OR br.id = ANY($2::uuid[]))
		),
		reach AS (
		    SELECT mo.id AS model_id, mo.application_id
		    FROM core.model mo
		    JOIN core.application a ON a.id = mo.application_id
		    WHERE a.workspace_id = $1::uuid
		       OR (a.workspace_id IS NULL
		           AND a.customer_id = (SELECT customer_id FROM core.workspace WHERE id = $1::uuid))
		),
		uses AS (
		    SELECT brd.role_id, dd.model_id
		    FROM identity.business_role_dashboard brd
		    JOIN model.dashboard_def dd ON dd.id = brd.dashboard_id
		    WHERE brd.role_id IN (SELECT id FROM roles)
		  UNION
		    SELECT rr.role_id, d.model_id
		    FROM identity.business_role_access_rule rr
		    JOIN model.dimension_member dm
		      ON dm.id = CASE WHEN rr.ref_id ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
		                      THEN rr.ref_id::uuid END
		    JOIN model.dimension_def d ON d.id = dm.dimension_id
		    WHERE rr.rule_type = 'dimension_member' AND rr.role_id IN (SELECT id FROM roles)
		  UNION
		    SELECT rr.role_id, md.model_id
		    FROM identity.business_role_access_rule rr
		    JOIN model.metric_def md
		      ON md.id = CASE WHEN rr.ref_id ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
		                      THEN rr.ref_id::uuid END
		    WHERE rr.rule_type = 'metric' AND rr.role_id IN (SELECT id FROM roles)
		  UNION
		    SELECT r.id, rc.model_id
		    FROM roles r
		    JOIN workflow.workflow_def wd ON TRUE
		    LEFT JOIN model.revision wrev ON wrev.id = wd.revision_id
		    JOIN reach rc ON rc.application_id = wd.application_id
		                 AND (wd.revision_id IS NULL OR rc.model_id = wrev.model_id)
		    WHERE COALESCE(wd.start_roles, '[]'::jsonb) ? r.name
		       OR COALESCE(wd.reopen_roles, '[]'::jsonb) ? r.name
		       OR {def_steps_name_role}
		  UNION
		    SELECT r.id, rc.model_id
		    FROM roles r
		    JOIN workflow.workflow_instance wi ON wi.status = 'running' AND wi.steps_snapshot IS NOT NULL
		    JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		    LEFT JOIN model.revision wrev ON wrev.id = wd.revision_id
		    JOIN reach rc ON rc.application_id = wd.application_id
		                 AND (wd.revision_id IS NULL OR rc.model_id = wrev.model_id)
		    WHERE {run_steps_name_role}
		  UNION
		    SELECT r.id, fd.model_id
		    FROM roles r
		    JOIN model.form_def fd ON (jsonb_typeof(fd.create_roles) = 'array' AND fd.create_roles ? r.name
		                            OR jsonb_typeof(fd.delete_roles) = 'array' AND fd.delete_roles ? r.name
		                            OR `+FieldsNameRole("fd.fields", "r.name")+`)
		    WHERE fd.model_id IN (SELECT model_id FROM reach)
		  UNION
		    SELECT r.id, dd.model_id
		    FROM roles r
		    JOIN model.dimension_def dd ON r.name = ANY(dd.editor_roles)
		    WHERE dd.model_id IN (SELECT model_id FROM reach)
		)
		SELECT u.role_id::text, mo.id::text, mo.name, COALESCE(a.name, '')
		FROM uses u
		JOIN core.model mo ON mo.id = u.model_id
		LEFT JOIN core.application a ON a.id = mo.application_id
		ORDER BY u.role_id, lower(mo.name), lower(COALESCE(a.name, '')), mo.id
	`), wsID, ids)
	if err != nil {
		return nil, fmt.Errorf("business role usage: %w", err)
	}
	defer rows.Close()
	out := map[string][]ModelUse{}
	for rows.Next() {
		var roleID string
		var m ModelUse
		if err := rows.Scan(&roleID, &m.ModelID, &m.ModelName, &m.ApplicationName); err != nil {
			return nil, fmt.Errorf("business role usage: %w", err)
		}
		out[roleID] = append(out[roleID], m)
	}
	return out, rows.Err()
}

// stepsNameRoleSQL is a condition: the steps array col has a step naming
// the role name as an assignee or as a notification's recipient.
func stepsNameRoleSQL(col, name string) string {
	return `EXISTS (
		            SELECT 1
		            FROM jsonb_array_elements(CASE WHEN jsonb_typeof(` + col + `) = 'array'
		                                           THEN ` + col + ` ELSE '[]'::jsonb END) s
		            WHERE (jsonb_typeof(s->'assignee_roles') = 'array' AND s->'assignee_roles' ? ` + name + `)
		               OR s->'notification'->>'recipient_role' = ` + name + `)`
}

// OtherModels is the models other than modelID that use role roleID of
// workspace wsID: the ones a rename or delete made from modelID would change
// without anyone looking at them.
func OtherModels(ctx context.Context, db dbx.DB, wsID, roleID, modelID string) ([]ModelUse, error) {
	use, err := Usage(ctx, db, wsID, roleID)
	if err != nil {
		return nil, err
	}
	var out []ModelUse
	for _, m := range use[roleID] {
		if m.ModelID != modelID {
			out = append(out, m)
		}
	}
	return out, nil
}

// Labels joins the models' labels for a sentence: "A", "A and B",
// "A, B and C".
func Labels(models []ModelUse) string {
	labels := make([]string, len(models))
	for i, m := range models {
		labels[i] = m.Label()
	}
	return joinAnd(labels)
}

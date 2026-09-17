package aiassistant

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// resolveWorkflowDefRef turns what the model handed us for a workflow — a
// UUID from list_workflows, or just the name — into the id of that
// definition INSIDE the working revision.
//
// Two facts make this more than a lookup. Workflow definitions are
// application-scoped and copied per revision, so an id the model read before
// the session's draft existed points at the active revision's row; the draft
// holds a same-named copy, and that copy is the only one this session may
// touch (update_workflow_def learned this live: adding a context variable to
// a published def failed with "not visible"). And models routinely pass the
// name where the schema says id, so a non-UUID is resolved by name.
func resolveWorkflowDefRef(ctx context.Context, pool *pgxpool.Pool, modelID, revID, ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("workflow_def_id is required")
	}
	var appID string
	if err := pool.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID); err != nil {
		return "", fmt.Errorf("resolve application: %w", err)
	}
	byName := func(name string) (string, error) {
		var id string
		err := pool.QueryRow(ctx, `
			SELECT id::text FROM workflow.workflow_def
			WHERE application_id = $1::uuid AND lower(name) = lower($2)
			  AND ($3 = '' OR revision_id IS NULL OR revision_id::text = $3)
			ORDER BY created_at DESC LIMIT 1
		`, appID, name, revID).Scan(&id)
		if err != nil {
			return "", fmt.Errorf("workflow %q not found in the working revision — call list_workflows to see what exists", name)
		}
		return id, nil
	}
	if !uuidShaped(ref) {
		return byName(ref)
	}
	var defAppID, defRevID, name string
	if err := pool.QueryRow(ctx, `
		SELECT application_id::text, COALESCE(revision_id::text,''), name
		FROM workflow.workflow_def WHERE id=$1::uuid
	`, ref).Scan(&defAppID, &defRevID, &name); err != nil {
		return "", fmt.Errorf("workflow %s not found", ref)
	}
	if defAppID != appID {
		return "", fmt.Errorf("workflow %s belongs to another application", ref)
	}
	if revID != "" && defRevID != "" && defRevID != revID {
		id, err := byName(name)
		if err != nil {
			return "", fmt.Errorf("workflow %s is not visible in the current working revision and has no same-named counterpart there", ref)
		}
		return id, nil
	}
	return ref, nil
}

// resolveFormDefRef is resolveWorkflowDefRef for forms, which are
// model-scoped rather than application-scoped but copied per revision in
// exactly the same way.
func resolveFormDefRef(ctx context.Context, pool *pgxpool.Pool, modelID, revID, ref string) (string, error) {
	if ref == "" {
		return "", fmt.Errorf("form_id is required")
	}
	byName := func(name string) (string, error) {
		var id string
		err := pool.QueryRow(ctx, `
			SELECT id::text FROM model.form_def
			WHERE model_id = $1::uuid AND lower(name) = lower($2)
			  AND ($3 = '' OR revision_id IS NULL OR revision_id::text = $3)
			ORDER BY created_at DESC LIMIT 1
		`, modelID, name, revID).Scan(&id)
		if err != nil {
			return "", fmt.Errorf("form %q not found in the working revision — call list_forms to see what exists", name)
		}
		return id, nil
	}
	if !uuidShaped(ref) {
		return byName(ref)
	}
	var formModelID, formRevID, name string
	if err := pool.QueryRow(ctx, `
		SELECT model_id::text, COALESCE(revision_id::text,''), name FROM model.form_def WHERE id=$1::uuid
	`, ref).Scan(&formModelID, &formRevID, &name); err != nil {
		return "", fmt.Errorf("form %s not found", ref)
	}
	if formModelID != modelID {
		return "", fmt.Errorf("form %s belongs to another model", ref)
	}
	if revID != "" && formRevID != "" && formRevID != revID {
		id, err := byName(name)
		if err != nil {
			return "", fmt.Errorf("form %s is not visible in the current working revision and has no same-named counterpart there", ref)
		}
		return id, nil
	}
	return ref, nil
}

// validTriggerTypes mirrors the workflow.trigger_type enum. A rule with any
// other value would fail at the column, so the tool refuses it with the
// list instead.
var validTriggerTypes = map[string]bool{
	"manual": true, "form_submit": true, "form_approval": true, "grid_change": true,
	"schedule": true, "api": true, "integration_completed": true, "integration_failed": true,
}

const triggerTypeList = "manual | form_submit | form_approval | grid_change | schedule | api | integration_completed | integration_failed"

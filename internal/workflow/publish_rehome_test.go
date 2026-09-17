// Publishing a workflow def must make it live for the business: the
// business-facing surfaces only show the ACTIVE revision's defs, but an
// AI-authored def is born inside the session's lazily-created draft — found
// live when "Country data approval" published into an orphan draft and the
// Planning workspace start dialog stayed empty. Publish now re-homes the
// def onto the active revision and remaps context_schema dimension
// references by NAME onto the active revision's copies.
package workflow_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/workflow"
)

func TestPublishReHomesDraftDefToActiveRevision(t *testing.T) {
	f := setupOnApproveFixture(t)
	ctx := context.Background()

	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}

	// An "AI draft" revision holding the def, and an ACTIVE revision with a
	// same-named geography under a different UUID — the shape every
	// session-draft authoring flow produces.
	draftRev := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'AI Draft X') RETURNING id::text`, f.modelID)
	activeRev := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Active') RETURNING id::text`, f.modelID)
	if _, err := f.pool.Exec(ctx, `UPDATE core.model SET active_revision_id=$2::uuid WHERE id=$1::uuid`, f.modelID, activeRev); err != nil {
		t.Fatalf("set active: %v", err)
	}
	draftGeo := q(`INSERT INTO model.dimension_def (model_id, name, revision_id) VALUES ($1::uuid, 'geography', $2::uuid) RETURNING id::text`, f.modelID, draftRev)
	activeGeo := q(`INSERT INTO model.dimension_def (model_id, name, revision_id) VALUES ($1::uuid, 'geography', $2::uuid) RETURNING id::text`, f.modelID, activeRev)

	schema := `[{"key":"country","label":"Country","data_type":"Dimension member","dimension_id":"` + draftGeo + `"},{"key":"note","data_type":"Text"}]`
	defID := q(`
		INSERT INTO workflow.workflow_def (application_id, name, trigger_event, status, steps, context_schema, revision_id, created_by)
		VALUES ($1::uuid, 'Rehome me', 'manual', 'draft', '[]'::jsonb, $2::jsonb, $3::uuid, $4::uuid)
		RETURNING id::text`, f.appID, schema, draftRev, f.userID)

	if _, err := workflow.NewStore(f.pool).PublishWorkflowDef(ctx, defID, f.userID); err != nil {
		t.Fatalf("publish: %v", err)
	}

	var gotRev string
	var gotSchema []byte
	if err := f.pool.QueryRow(ctx,
		`SELECT revision_id::text, context_schema FROM workflow.workflow_def WHERE id=$1::uuid`, defID,
	).Scan(&gotRev, &gotSchema); err != nil {
		t.Fatalf("reload def: %v", err)
	}
	if gotRev != activeRev {
		t.Fatalf("def revision = %s, want the active revision %s (publish must re-home)", gotRev, activeRev)
	}
	var vars []map[string]any
	if err := json.Unmarshal(gotSchema, &vars); err != nil {
		t.Fatalf("parse schema: %v", err)
	}
	if got := vars[0]["dimension_id"]; got != activeGeo {
		t.Fatalf("dimension_id = %v, want remapped to active revision's geography %s", got, activeGeo)
	}
	if _, has := vars[1]["dimension_id"]; has && vars[1]["dimension_id"] != nil {
		t.Fatalf("text variable must pass through untouched, got %v", vars[1])
	}
}

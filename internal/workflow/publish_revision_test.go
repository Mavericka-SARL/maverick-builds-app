// Publishing a workflow def keeps it in its own revision. Publish used to
// move a definition from any non-live revision onto the live one (meant for
// AI drafts that could not be promoted then), so a developer preparing
// Revision 2 who published there changed the live app at once and lost the
// workflow from Revision 2. Making a revision live is a separate decision.
package workflow_test

import (
	"context"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/workflow"
)

func TestPublishKeepsDefInItsOwnRevision(t *testing.T) {
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

	// A working revision holding the def beside a live revision that already
	// has a definition of the same name — the move used to fail on
	// workflow_def_rev_name_uq after the status was set.
	workingRev := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Revision 2') RETURNING id::text`, f.modelID)
	activeRev := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Revision 1') RETURNING id::text`, f.modelID)
	if _, err := f.pool.Exec(ctx, `UPDATE core.model SET active_revision_id=$2::uuid WHERE id=$1::uuid`, f.modelID, activeRev); err != nil {
		t.Fatalf("set active: %v", err)
	}
	workingGeo := q(`INSERT INTO model.dimension_def (model_id, name, revision_id) VALUES ($1::uuid, 'geography', $2::uuid) RETURNING id::text`, f.modelID, workingRev)
	q(`INSERT INTO model.dimension_def (model_id, name, revision_id) VALUES ($1::uuid, 'geography', $2::uuid) RETURNING id::text`, f.modelID, activeRev)

	schema := `[{"key":"country","label":"Country","data_type":"Dimension member","dimension_id":"` + workingGeo + `"}]`
	liveDef := q(`
		INSERT INTO workflow.workflow_def (application_id, name, trigger_event, status, steps, context_schema, revision_id, created_by)
		VALUES ($1::uuid, 'Approve plan', 'manual', 'published', '[]'::jsonb, '[]'::jsonb, $2::uuid, $3::uuid)
		RETURNING id::text`, f.appID, activeRev, f.userID)
	defID := q(`
		INSERT INTO workflow.workflow_def (application_id, name, trigger_event, status, steps, context_schema, revision_id, created_by)
		VALUES ($1::uuid, 'Approve plan', 'manual', 'draft', '[]'::jsonb, $2::jsonb, $3::uuid, $4::uuid)
		RETURNING id::text`, f.appID, schema, workingRev, f.userID)

	pub, err := workflow.NewStore(f.pool).PublishWorkflowDef(ctx, defID, f.userID)
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if pub.Status != "published" {
		t.Fatalf("status = %q, want published", pub.Status)
	}

	var gotRev, gotDim string
	if err := f.pool.QueryRow(ctx,
		`SELECT revision_id::text, context_schema->0->>'dimension_id' FROM workflow.workflow_def WHERE id=$1::uuid`, defID,
	).Scan(&gotRev, &gotDim); err != nil {
		t.Fatalf("reload def: %v", err)
	}
	if gotRev != workingRev {
		t.Fatalf("def revision = %s, want its own revision %s (publish must not move it into the live one)", gotRev, workingRev)
	}
	if gotDim != workingGeo {
		t.Fatalf("dimension_id = %s, want the working revision's geography %s", gotDim, workingGeo)
	}

	var liveCount int
	if err := f.pool.QueryRow(ctx,
		`SELECT count(*) FROM workflow.workflow_def WHERE revision_id=$1::uuid`, activeRev,
	).Scan(&liveCount); err != nil {
		t.Fatalf("count live defs: %v", err)
	}
	if liveCount != 1 {
		t.Fatalf("live revision holds %d definitions, want only its own %s", liveCount, liveDef)
	}
}

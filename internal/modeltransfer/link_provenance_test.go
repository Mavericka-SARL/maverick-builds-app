package modeltransfer

import (
	"context"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// A value a model link wrote names the link (fact_input.link_ref, migration
// 169), so the link's next run clears it once its source no longer holds
// it. Exported and imported, it names the imported link; a typed value
// names none.
func TestLinkWrittenValuesNameTheImportedLink(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	app, user := importTarget(t, ctx, pool)
	srcModel, srcRev, err := importInto(ctx, pool, app, user, decodePackage(t, referencePackage))
	if err != nil {
		t.Fatal(err)
	}
	var metricID, dimID, linkID string
	if err := pool.QueryRow(ctx, `SELECT m.id::text, d.id::text FROM model.metric_def m JOIN model.dimension_def d ON d.revision_id = m.revision_id
		WHERE m.revision_id=$1::uuid AND m.name='Revenue' AND d.name='Region'`, srcRev).Scan(&metricID, &dimID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO model.integration_def (model_id, revision_id, name, type, target_type, config, status)
		VALUES ($1::uuid, $2::uuid, 'Sales link', 'rest_api', 'grid', '{}'::jsonb, 'active') RETURNING link_id::text`, srcModel, srcRev).Scan(&linkID); err != nil {
		t.Fatal(err)
	}
	for code, link := range map[string]any{"N": linkID, "ALL": nil} {
		if _, err := pool.Exec(ctx, `INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value, link_ref)
			VALUES ($1::uuid, $2::uuid, $3::uuid, jsonb_build_object($4::text, $5::text), 5, $6::uuid)`, srcModel, srcRev, metricID, dimID, code, link); err != nil {
			t.Fatal(err)
		}
	}
	exported, err := CollectExport(ctx, pool, srcModel, srcRev, "Imported")
	if err != nil {
		t.Fatal(err)
	}
	otherApp, otherUser := tenantTarget(t, ctx, pool, "Other Co")
	_, rev, err := importInto(ctx, pool, otherApp, otherUser, *exported)
	if err != nil {
		t.Fatal(err)
	}
	var linked, unlinked int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE f.link_ref = i.link_id), count(*) FILTER (WHERE f.link_ref IS NULL)
		FROM runtime.fact_input f LEFT JOIN model.integration_def i ON i.revision_id = f.revision_id AND i.name = 'Sales link'
		WHERE f.revision_id = $1::uuid`, rev).Scan(&linked, &unlinked); err != nil {
		t.Fatal(err)
	}
	if linked != 1 || unlinked != 1 {
		t.Errorf("imported values naming the imported link = %d, naming none = %d; want 1 and 1", linked, unlinked)
	}
}

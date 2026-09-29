package aiassistant_test

import (
	"context"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// The AI's create_revision copies connectors as the developer console's
// duplicateRevision does: both target references — the target_id column and
// config.target_id, which a connector's runs read and write — name the
// copy's row of every kind, and the connector's settings travel with it. It
// used to copy the config verbatim, remap the column only for grids, forms
// and dashboards, and run before forms and dashboards were copied, so a form
// or dashboard target kept naming the source revision's row too.
func TestCreateRevisionPointsConnectorsAtTheCopy(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	modelID := seedModel(t, pool)
	srcRev := seedRevision(t, pool, modelID, "Source")
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	appID := q(`SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID)
	targets := map[string]string{
		"grid":      q(`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Plan') RETURNING id::text`, modelID, srcRev),
		"dimension": q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Region') RETURNING id::text`, modelID, srcRev),
		"form":      q(`INSERT INTO model.form_def (model_id, revision_id, name, label, fields) VALUES ($1::uuid, $2::uuid, 'intake', 'Intake', '[]'::jsonb) RETURNING id::text`, modelID, srcRev),
		"dashboard": q(`INSERT INTO model.dashboard_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Board') RETURNING id::text`, modelID, srcRev),
	}
	tables := map[string]string{"grid": "model.grid_def", "dimension": "model.dimension_def", "form": "model.form_def", "dashboard": "model.dashboard_def"}
	connID := q(`INSERT INTO model.integration_connection (application_id, name, auth_type) VALUES ($1::uuid, 'ERP', 'none') RETURNING id::text`, appID)
	for kind, id := range targets {
		q(`INSERT INTO model.integration_def (model_id, revision_id, name, type, target_type, target_id, config,
			                                   direction, status, tags, description, connection_id)
			VALUES ($1::uuid, $2::uuid, $3, 'rest_api', $3, $4::uuid,
			        jsonb_build_object('kind','rest_api/v1','direction','push','target_type',$3::text,'target_id',$4::text),
			        'push', 'draft', '{erp}', 'Nightly', $5::uuid)
			RETURNING id::text`, modelID, srcRev, kind, id, connID)
	}

	exec := aiassistant.NewWriteExecutor(pool, modelID, srcRev)
	_, newRev, err := exec.Execute(ctx, "create_revision", mustJSON(t, map[string]any{
		"name": "Copy", "source_revision_id": srcRev,
	}))
	if err != nil {
		t.Fatalf("create_revision: %v", err)
	}

	for kind, srcID := range targets {
		var column, config, settings string
		if err := pool.QueryRow(ctx, `
			SELECT COALESCE(target_id::text,''), COALESCE(config->>'target_id',''),
			       concat_ws('|', direction, status, array_to_string(tags, ','), description, COALESCE(connection_id::text,''))
			FROM model.integration_def WHERE revision_id=$1::uuid AND name=$2`, newRev, kind).Scan(&column, &config, &settings); err != nil {
			t.Fatalf("%s connector in the copy: %v", kind, err)
		}
		want := q(`SELECT n.id::text FROM `+tables[kind]+` o JOIN `+tables[kind]+` n ON n.name = o.name AND n.revision_id = $2::uuid WHERE o.id = $1::uuid`, srcID, newRev)
		if column != want {
			t.Errorf("%s connector: target_id column = %s, want the copy's %s (source %s)", kind, column, want, srcID)
		}
		if config != want {
			t.Errorf("%s connector: config target_id = %s, want the copy's %s (source %s)", kind, config, want, srcID)
		}
		if settings != "push|draft|erp|Nightly|"+connID {
			t.Errorf("%s connector settings in the copy = %s, want push|draft|erp|Nightly|%s", kind, settings, connID)
		}
	}
}

package gateway

import (
	"context"
	"testing"

	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// A connector copied into a new revision targets the new revision's rows. It
// names its target twice — the target_id column and config.target_id, which
// is what its runs read and write — and duplicateRevision used to copy the
// config verbatim and remap the column only for grids, forms and dashboards,
// so a connector in the copy wrote into the source revision. Every target
// kind goes through the duplication's maps now, a target left in another
// revision by an earlier copy included, and the connector's own settings
// travel with it.
func TestDuplicateRevisionPointsConnectorsAtTheCopy(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()
	h := &handler{db: tenantdb.NewHandle(f.pool, nil), log: logger.New("test")}
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	formID := q(`INSERT INTO model.form_def (model_id, revision_id, name, label, fields)
		VALUES ($1::uuid, $2::uuid, 'intake', 'Intake', '[]'::jsonb) RETURNING id::text`, f.modelID, f.workingRevID)
	dashID := q(`INSERT INTO model.dashboard_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Board') RETURNING id::text`,
		f.modelID, f.workingRevID)
	connID := q(`INSERT INTO model.integration_connection (application_id, name, auth_type) VALUES ($1::uuid, 'ERP', 'none') RETURNING id::text`, f.appID)
	// The same grid in another revision of the model: what a connector
	// copied before its config was remapped still names.
	siblingGridID := q(`INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid, 'Staff Grid', $2::uuid) RETURNING id::text`,
		f.modelID, f.annualRevID)
	const outsideID = "0b6f7c1e-9a51-4c34-8f3e-2d9d5e7a1c42" // names no row

	add := func(name, typ, targetType, columnTarget, config string) {
		t.Helper()
		q(`INSERT INTO model.integration_def (model_id, revision_id, name, type, target_type, target_id, config,
			                                   direction, status, tags, description, connection_id)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, NULLIF($6,'')::uuid, $7::jsonb, 'push', 'draft', '{erp}', 'Nightly', $8::uuid)
			RETURNING id::text`,
			f.modelID, f.workingRevID, name, typ, targetType, columnTarget, config, connID)
	}
	restConfig := func(targetType, targetID string) string {
		return `{"kind":"rest_api/v1","direction":"push","target_type":"` + targetType + `","target_id":"` + targetID +
			`","request":{"method":"POST","url":"https://erp.test","body":"` + f.gridStaffID + `"}}`
	}
	add("Feed", "rest_api", "grid", f.gridStaffID, restConfig("grid", f.gridStaffID))
	add("Depts", "rest_api", "dimension", f.deptsDimID, restConfig("dimension", f.deptsDimID))
	add("Intake", "rest_api", "form", formID, restConfig("form", formID))
	add("Legacy", "csv_import", "dashboard", dashID, `{"dashboard_id":"`+dashID+`"}`)
	add("Stale", "rest_api", "grid", f.gridStaffID, restConfig("grid", siblingGridID))
	add("Elsewhere", "rest_api", "grid", "", restConfig("grid", outsideID))

	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	copyRev, err := h.duplicateRevision(ctx, tx, f.modelID, "Copy", f.workingRevID, &f.workingRevID)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("duplicateRevision: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Each connector's column target and config target name the copy's row
	// of the source's name and kind.
	for _, c := range []struct{ connector, table, name, configKey string }{
		{"Feed", "model.grid_def", "Staff Grid", "target_id"},
		{"Depts", "model.dimension_def", q(`SELECT name FROM model.dimension_def WHERE id=$1::uuid`, f.deptsDimID), "target_id"},
		{"Intake", "model.form_def", "intake", "target_id"},
		{"Legacy", "model.dashboard_def", "Board", "dashboard_id"},
	} {
		var column, config, want string
		if err := f.pool.QueryRow(ctx,
			`SELECT COALESCE(target_id::text,''), COALESCE(config->>'`+c.configKey+`','') FROM model.integration_def WHERE revision_id=$1::uuid AND name=$2`,
			copyRev, c.connector).Scan(&column, &config); err != nil {
			t.Fatalf("%s in the copy: %v", c.connector, err)
		}
		want = q(`SELECT id::text FROM `+c.table+` WHERE revision_id=$1::uuid AND name=$2`, copyRev, c.name)
		if column != want {
			t.Errorf("%s: target_id column = %s, want the copy's %s %s", c.connector, column, c.name, want)
		}
		if config != want {
			t.Errorf("%s: config %s = %s, want the copy's %s %s", c.connector, c.configKey, config, c.name, want)
		}
	}
	copyGrid := q(`SELECT id::text FROM model.grid_def WHERE revision_id=$1::uuid AND name='Staff Grid'`, copyRev)
	if got := q(`SELECT config->>'target_id' FROM model.integration_def WHERE revision_id=$1::uuid AND name='Stale'`, copyRev); got != copyGrid {
		t.Errorf("Stale: config target_id = %s, want the copy's Staff Grid %s (it named another revision's)", got, copyGrid)
	}
	// An ID naming no row of the model is left as it is, and nothing nested
	// in the config — the external system's request — is rewritten.
	if got := q(`SELECT config->>'target_id' FROM model.integration_def WHERE revision_id=$1::uuid AND name='Elsewhere'`, copyRev); got != outsideID {
		t.Errorf("Elsewhere: config target_id = %s, want %s kept", got, outsideID)
	}
	if got := q(`SELECT config->'request'->>'body' FROM model.integration_def WHERE revision_id=$1::uuid AND name='Feed'`, copyRev); got != f.gridStaffID {
		t.Errorf("Feed: request body = %s, want %s as written", got, f.gridStaffID)
	}

	// The connector's own settings travel with it.
	if got := q(`SELECT concat_ws('|', direction, status, array_to_string(tags, ','), description, COALESCE(connection_id::text,''))
		FROM model.integration_def WHERE revision_id=$1::uuid AND name='Feed'`, copyRev); got != "push|draft|erp|Nightly|"+connID {
		t.Errorf("Feed settings in the copy = %s, want push|draft|erp|Nightly|%s", got, connID)
	}

	// The source revision's connectors are untouched.
	if got := q(`SELECT concat_ws('|', target_id::text, config->>'target_id') FROM model.integration_def WHERE revision_id=$1::uuid AND name='Feed'`,
		f.workingRevID); got != f.gridStaffID+"|"+f.gridStaffID {
		t.Errorf("source Feed targets = %s, want its own grid twice", got)
	}
}

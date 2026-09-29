package gateway

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/modeltransfer"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// A revision the engine made with duplicateRevision exports and imports
// back into the database it came from — as a copy in the same tenant and
// into another tenant — with every reference landing inside the imported
// revision. Such a revision is what real databases hold: the workflow shared
// by every revision names the source revision's dimension, a connector's
// config names its source's target, and a grid widget copied before its
// layout was remapped names the source's dimensions. The export points each
// at the copy's own row, so the import keeps them rather than dropping them.
func TestModelExportOfDuplicatedRevisionImportsBack(t *testing.T) {
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

	// Connectors as integration.Store writes them: the target in the column
	// and again in config.target_id — onto a grid and onto a dimension.
	for _, c := range []struct{ name, typ, target string }{
		{"Feed", "grid", f.gridStaffID},
		{"Depts", "dimension", f.deptsDimID},
	} {
		cfg := fmt.Sprintf(`{"kind":"rest_api/v1","direction":"pull","target_type":%q,"target_id":%q,"request":{"method":"GET","url":"https://x.test"}}`, c.typ, c.target)
		q(`INSERT INTO model.integration_def (model_id, revision_id, name, type, target_type, target_id, config)
			VALUES ($1::uuid,$2::uuid,$3,'rest_api',$4,$5::uuid,$6::jsonb) RETURNING id::text`,
			f.modelID, f.workingRevID, c.name, c.typ, c.target, cfg)
	}
	dash := q(`INSERT INTO model.dashboard_def (model_id, revision_id, name) VALUES ($1::uuid,$2::uuid,'Board') RETURNING id::text`, f.modelID, f.workingRevID)
	q(`INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, sort_order, widget_props)
		VALUES ($1::uuid, 'grid', $2, 0, jsonb_build_object('default_view', jsonb_build_object(
		  'rows', jsonb_build_array($3::text), 'cols', jsonb_build_array('__metrics__'), 'context', '[]'::jsonb,
		  'filter_sel', jsonb_build_object($4::text, 'DEPT_A')))) RETURNING id::text`,
		dash, f.gridStaffID, f.staffDimID, f.deptsDimID)
	// The documented static context of an automation button: a revision of
	// the source model, which no package holds.
	q(`INSERT INTO model.dashboard_widget (dashboard_id, widget_type, sort_order, widget_props)
		VALUES ($1::uuid, 'automation_button', 1, jsonb_build_object('context', jsonb_build_object('target_revision_id', $2::text))) RETURNING id::text`,
		dash, f.annualRevID)

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
	// As a copy made before duplicateRevision remapped a grid layout left it.
	if _, err := f.pool.Exec(ctx, `
		UPDATE model.dashboard_widget w SET widget_props = jsonb_build_object('default_view', jsonb_build_object(
		  'rows', jsonb_build_array($2::text), 'cols', jsonb_build_array('__metrics__'), 'context', '[]'::jsonb,
		  'filter_sel', jsonb_build_object($3::text, 'DEPT_A')))
		FROM model.dashboard_def d WHERE d.id = w.dashboard_id AND d.revision_id = $1::uuid AND w.widget_type = 'grid'`,
		copyRev, f.staffDimID, f.deptsDimID); err != nil {
		t.Fatal(err)
	}

	pkg, err := modeltransfer.CollectExport(ctx, f.pool, f.modelID, copyRev, "Copy")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	cust := q(`INSERT INTO core.customer (name, plan) VALUES ('Other Co', 'test') RETURNING id::text`)
	ws := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Default') RETURNING id::text`, cust)
	otherApp := q(`INSERT INTO core.application (customer_id, workspace_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Other', 'planning') RETURNING id::text`, cust, ws)
	otherUser := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('other-importer', 'other@import.test', 'Other', $1::uuid) RETURNING id::text`, cust)

	for _, target := range []struct{ name, app, user string }{
		{"copy in the same tenant", f.appID, f.managerID},
		{"another tenant", otherApp, otherUser},
	} {
		var rev string
		if err := pgx.BeginFunc(ctx, f.pool, func(tx pgx.Tx) error {
			var err error
			_, rev, err = modeltransfer.Import(ctx, tx, modeltransfer.ImportRequest{ApplicationID: target.app, Package: *pkg}, target.user)
			return err
		}); err != nil {
			t.Fatalf("%s: import of the engine's own duplicated revision: %v", target.name, err)
		}
		assertRevisionSelfContained(t, ctx, f, rev, target.name)
		wfName := q(`SELECT name FROM workflow.workflow_def WHERE id=$1::uuid`, f.wfDefID)
		for _, c := range []struct{ what, sql, want string }{
			{"shared workflow's dimension", `SELECT d.name FROM workflow.workflow_def w JOIN model.dimension_def d ON d.id::text = w.context_schema->0->>'dimension_id'
				WHERE w.revision_id=$1::uuid AND d.revision_id=$1::uuid AND w.name=` + quoteLiteral(wfName),
				q(`SELECT d.name FROM workflow.workflow_def w JOIN model.dimension_def d ON d.id::text = w.context_schema->0->>'dimension_id' WHERE w.id=$1::uuid`, f.wfDefID)},
			{"grid connector's config target", `SELECT g.name FROM model.integration_def i JOIN model.grid_def g ON g.id::text = i.config->>'target_id'
				WHERE i.name='Feed' AND i.revision_id=$1::uuid AND g.revision_id=$1::uuid AND g.id = i.target_id`, q(`SELECT name FROM model.grid_def WHERE id=$1::uuid`, f.gridStaffID)},
			{"dimension connector's target", `SELECT d.name FROM model.integration_def i JOIN model.dimension_def d ON d.id::text = i.config->>'target_id'
				WHERE i.name='Depts' AND i.revision_id=$1::uuid AND d.revision_id=$1::uuid AND d.id = i.target_id`, q(`SELECT name FROM model.dimension_def WHERE id=$1::uuid`, f.deptsDimID)},
			{"grid layout", `SELECT d.name FROM model.dashboard_widget w JOIN model.dashboard_def b ON b.id=w.dashboard_id
				JOIN model.dimension_def d ON d.id::text = w.widget_props->'default_view'->'rows'->>0
				WHERE w.widget_type='grid' AND b.revision_id=$1::uuid AND d.revision_id=$1::uuid`, q(`SELECT name FROM model.dimension_def WHERE id=$1::uuid`, f.staffDimID)},
			{"static target revision", `SELECT COALESCE(w.widget_props->'context'->>'target_revision_id', 'dropped') FROM model.dashboard_widget w
				JOIN model.dashboard_def b ON b.id=w.dashboard_id WHERE w.widget_type='automation_button' AND b.revision_id=$1::uuid`, "dropped"},
		} {
			var got []string
			rows, err := f.pool.Query(ctx, c.sql, rev)
			if err != nil {
				t.Fatalf("%s: %s: %v", target.name, c.what, err)
			}
			for rows.Next() {
				var s string
				if err := rows.Scan(&s); err != nil {
					t.Fatal(err)
				}
				got = append(got, s)
			}
			rows.Close()
			if len(got) != 1 || got[0] != c.want {
				t.Errorf("%s: %s = %v, want %s", target.name, c.what, got, c.want)
			}
		}
	}
}

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

var roundTripUUID = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// assertRevisionSelfContained fails unless every ID revision rev's rows
// hold — foreign keys, ref_id and targets, and every UUID in their JSON
// documents — names a row of rev.
func assertRevisionSelfContained(t *testing.T, ctx context.Context, f *rollupFixture, rev, what string) {
	t.Helper()
	list := func(sql string) []string {
		t.Helper()
		rows, err := f.pool.Query(ctx, sql, rev)
		if err != nil {
			t.Fatalf("%s: %s: %v", what, sql, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}
	own := map[string]bool{}
	for _, id := range list(`
		          SELECT id::text FROM model.dimension_def WHERE revision_id=$1::uuid
		UNION ALL SELECT m.id::text FROM model.dimension_member m JOIN model.dimension_def d ON d.id=m.dimension_id WHERE d.revision_id=$1::uuid
		UNION ALL SELECT id::text FROM model.metric_def WHERE revision_id=$1::uuid
		UNION ALL SELECT id::text FROM model.grid_def WHERE revision_id=$1::uuid
		UNION ALL SELECT id::text FROM model.form_def WHERE revision_id=$1::uuid
		UNION ALL SELECT id::text FROM model.form_metric_mapping WHERE revision_id=$1::uuid
		UNION ALL SELECT id::text FROM model.dashboard_folder WHERE revision_id=$1::uuid
		UNION ALL SELECT id::text FROM model.dashboard_def WHERE revision_id=$1::uuid
		UNION ALL SELECT id::text FROM model.integration_def WHERE revision_id=$1::uuid
		UNION ALL SELECT id::text FROM workflow.workflow_def WHERE revision_id=$1::uuid
		UNION ALL SELECT id::text FROM workflow.automation_rule WHERE revision_id=$1::uuid`) {
		own[id] = true
	}
	held := list(`
		          SELECT concat_ws(' ', 'dimension', name, parent_dimension_id, source_dimension_id) FROM model.dimension_def WHERE revision_id=$1::uuid
		UNION ALL SELECT concat_ws(' ', 'member', m.code, m.parent_member_id) FROM model.dimension_member m JOIN model.dimension_def d ON d.id=m.dimension_id WHERE d.revision_id=$1::uuid
		UNION ALL SELECT concat_ws(' ', 'grid', name, rollup_source_grid_id) FROM model.grid_def WHERE revision_id=$1::uuid
		UNION ALL SELECT concat_ws(' ', 'form', name, fields::text) FROM model.form_def WHERE revision_id=$1::uuid
		UNION ALL SELECT concat_ws(' ', 'mapping', name, form_id, grid_id, target_metric_id, dimension_mappings::text) FROM model.form_metric_mapping WHERE revision_id=$1::uuid
		UNION ALL SELECT concat_ws(' ', 'folder', name, parent_id) FROM model.dashboard_folder WHERE revision_id=$1::uuid
		UNION ALL SELECT concat_ws(' ', 'dashboard', name, folder_id) FROM model.dashboard_def WHERE revision_id=$1::uuid
		UNION ALL SELECT concat_ws(' ', 'widget', w.widget_type, w.ref_id, w.widget_props::text) FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id=w.dashboard_id WHERE d.revision_id=$1::uuid
		UNION ALL SELECT concat_ws(' ', 'integration', name, target_id, config::text) FROM model.integration_def WHERE revision_id=$1::uuid
		UNION ALL SELECT concat_ws(' ', 'workflow', name, subject_config::text, context_schema::text) FROM workflow.workflow_def WHERE revision_id=$1::uuid
		UNION ALL SELECT concat_ws(' ', 'rule', name, workflow_def_id, source_form_id, source_grid_id) FROM workflow.automation_rule WHERE revision_id=$1::uuid
		UNION ALL SELECT concat_ws(' ', 'fact', metric_id, source_ref, dim_members::text) FROM runtime.fact_input WHERE revision_id=$1::uuid`)
	sort.Strings(held)
	for _, line := range held {
		for _, id := range roundTripUUID.FindAllString(strings.ToLower(line), -1) {
			if !own[id] {
				t.Errorf("%s: %q names %s, which is not a row of the imported revision", what, line, id)
			}
		}
	}
}

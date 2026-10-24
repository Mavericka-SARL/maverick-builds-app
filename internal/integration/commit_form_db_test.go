package integration_test

import (
	"context"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/integration"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	"github.com/mavericks-engine/mavericks/pkg/logger"

	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// A connector pull into a table checks each row as a record saved by hand
// is checked — its values, and where it would post within the run's
// principal's access — gives it its code and posts it into the table's
// metrics: it used to create bare records nothing checked or posted. A row
// that fails is skipped and counted, as a grid target's is.
func TestCommitFormChecksAndPostsEachRecord(t *testing.T) {
	pool := testdb.New(t, migrationfs.FS, ".")
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
		return id
	}
	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('FormCo','enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid,'ws') RETURNING id::text`, custID)
	appID := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid,$2::uuid,'App','planning') RETURNING id::text`, wsID, custID)
	modelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid,'M') RETURNING id::text`, appID)
	revID := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid,'Working') RETURNING id::text`, modelID)
	q(`UPDATE core.model SET active_revision_id=$1::uuid WHERE id=$2::uuid RETURNING id::text`, revID, modelID)
	region := q(`INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type) VALUES ($1::uuid,$2::uuid,'region','standard') RETURNING id::text`, modelID, revID)
	eu := q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid,'EU','Europe') RETURNING id::text`, region)
	q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid,'US','United States') RETURNING id::text`, region)
	metric := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule) VALUES ($1::uuid,$2::uuid,'spend',true,'sum') RETURNING id::text`, modelID, revID)
	grid := q(`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid,$2::uuid,'G') RETURNING id::text`, modelID, revID)
	q(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid,$2::uuid) RETURNING grid_id::text`, grid, region)
	q(`INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid,$2::uuid) RETURNING grid_id::text`, grid, metric)
	form := q(`INSERT INTO model.form_def (model_id, revision_id, name, label, fields) VALUES ($1::uuid,$2::uuid,'spend','Spend',
		jsonb_build_array(
			jsonb_build_object('name','code','label','Code','type','autocode','code_prefix','S-'),
			jsonb_build_object('name','region','label','Region','type','dimension','dimension_id',$3::text),
			jsonb_build_object('name','amount','label','Amount','type','number'))) RETURNING id::text`, modelID, revID, region)
	mapping := q(`INSERT INTO model.form_metric_mapping (model_id, revision_id, form_id, grid_id, name, source_field, target_metric_id, dimension_mappings)
		VALUES ($1::uuid,$2::uuid,$3::uuid,$4::uuid,'spend','amount',$5::uuid,jsonb_build_object($6::text,'region')) RETURNING id::text`,
		modelID, revID, form, grid, metric, region)
	// The run's principal may only read Europe.
	dev := q(`INSERT INTO identity.user (keycloak_sub, email, customer_id) VALUES ('dev','dev@t.co',$1::uuid) RETURNING id::text`, custID)
	q(`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid,'dimension_member',$2,'read') RETURNING id::text`, dev, eu)

	c := &integration.DBCommitter{Pool: pool, Log: logger.New("test")}
	def := &integration.Definition{ModelID: modelID, RevisionID: revID,
		Config: &integration.Config{TargetType: integration.TargetForm, TargetID: form}}
	header := []string{"region", "amount", "code"}
	rows := [][]string{
		{"US", "10", "MINE"}, // posts; the code is the server's
		{"EU", "5", ""},      // read-only for the principal, and it would post there
		{"XX", "3", ""},      // no such member
		{"US", "abc", ""},    // not a number
	}

	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	written, skipped, err := c.CommitPull(ctx, def, header, rows, "", true, dev)
	if err != nil || written != 1 || skipped != 3 {
		t.Fatalf("dry run: written=%d skipped=%d err=%v, want 1 and 3", written, skipped, err)
	}
	if n := count(`SELECT count(*) FROM runtime.form_record WHERE form_id=$1::uuid`, form); n != 0 {
		t.Fatalf("a dry run created %d record(s)", n)
	}
	if n := count(`SELECT count(*) FROM import.import_job WHERE model_id=$1::uuid`, modelID); n != 0 {
		t.Fatalf("a dry run is listed in Recent Imports (%d job(s))", n)
	}

	written, skipped, err = c.CommitPull(ctx, def, header, rows, "", false, dev)
	if err != nil || written != 1 || skipped != 3 {
		t.Fatalf("run: written=%d skipped=%d err=%v, want 1 and 3", written, skipped, err)
	}
	// The sync is in Recent Imports, as a file of the table's records is: it
	// was only in the integration's run history (2026-10-23).
	if n := count(`SELECT count(*) FROM import.import_job WHERE model_id=$1::uuid AND revision_id=$2::uuid AND created_by=$3::uuid
		AND status='committed' AND file_url='rest_api' AND target_type='table' AND target_name='Spend' AND valid_rows=1 AND error_rows=3`,
		modelID, revID, dev); n != 1 {
		t.Errorf("%d Recent Imports line(s) for the sync, want 1", n)
	}
	var code string
	if err := pool.QueryRow(ctx, `SELECT data->>'code' FROM runtime.form_record WHERE form_id=$1::uuid`, form).Scan(&code); err != nil || code != "S-0001" {
		t.Errorf("the record's code = %q (%v), want S-0001", code, err)
	}
	var posted float64
	if err := pool.QueryRow(ctx, `SELECT COALESCE(sum(value), 0) FROM runtime.fact_input WHERE source_ref=$1::uuid`, mapping).Scan(&posted); err != nil || posted != 10 {
		t.Errorf("posted into spend = %v (%v), want 10 once the run returns", posted, err)
	}
}

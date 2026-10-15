package migrations_test

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	"github.com/mavericks-engine/mavericks/migrations"
)

// TestRecordTables139 applies migrations 001–138, then 139: every table
// that existed keeps its records' statuses through a Status field of its
// own ("record_status" where a field is already named "status"), a record
// may then have no status, and a table may show each person their own
// records.
func TestRecordTables139(t *testing.T) {
	ctx := context.Background()
	adminDSN := testdb.AdminDSN(t)
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("tables139_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	_ = admin.Close(ctx)
	cfg, err := pgx.ParseConfig(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = name
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	apply := func(f string) {
		t.Helper()
		sql, err := fs.ReadFile(migrations.FS, f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	for _, f := range files {
		if f >= "139" {
			break
		}
		apply(f)
	}

	var modelID string
	if err := conn.QueryRow(ctx, `
		WITH c AS (INSERT INTO core.customer (name) VALUES ('T') RETURNING id),
		     w AS (INSERT INTO core.workspace (customer_id, name) SELECT id, 'W' FROM c RETURNING id),
		     a AS (INSERT INTO core.application (workspace_id, name, mode) SELECT id, 'A', 'planning' FROM w RETURNING id)
		INSERT INTO core.model (application_id, name) SELECT id, 'M' FROM a RETURNING id::text`).Scan(&modelID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO model.form_def (model_id, name, label, fields) VALUES
		  ($1::uuid, 'plain', 'Plain', '[{"name":"amount","label":"Amount","type":"number"}]'),
		  ($1::uuid, 'taken', 'Taken', '[{"name":"status","label":"Status text","type":"text"}]'),
		  ($1::uuid, 'empty', 'Empty', '[]')`, modelID); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `
		WITH d AS (INSERT INTO model.dashboard_def (model_id, name) VALUES ($1::uuid, 'D') RETURNING id)
		INSERT INTO model.dashboard_widget (dashboard_id, widget_type, ref_id, widget_props)
		SELECT d.id, w.t, NULL, w.p FROM d, (VALUES ('form', NULL::jsonb), ('form', '{"background":"none"}'::jsonb), ('text', NULL::jsonb)) AS w(t, p)`, modelID); err != nil {
		t.Fatal(err)
	}
	// Mappings of the model: one of its own, one naming another model's
	// table (with a total it posted), one naming another model's grid.
	var otherModel, user0 string
	if err := conn.QueryRow(ctx, `
		WITH a AS (SELECT application_id FROM core.model WHERE id = $1::uuid)
		INSERT INTO core.model (application_id, name) SELECT application_id, 'Other' FROM a RETURNING id::text`, modelID).Scan(&otherModel); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email) VALUES ('u0', 'u0@t.co') RETURNING id::text`).Scan(&user0); err != nil {
		t.Fatal(err)
	}
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := conn.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	rev := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'R') RETURNING id::text`, modelID)
	metric := q(`INSERT INTO model.metric_def (model_id, name, is_input, agg_rule) VALUES ($1::uuid, 'm', true, 'sum') RETURNING id::text`, modelID)
	ownForm := q(`SELECT id::text FROM model.form_def WHERE name = 'plain'`)
	foreignForm := q(`INSERT INTO model.form_def (model_id, name, label) VALUES ($1::uuid, 'theirs', 'Theirs') RETURNING id::text`, otherModel)
	foreignGrid := q(`INSERT INTO model.grid_def (model_id, name) VALUES ($1::uuid, 'g') RETURNING id::text`, otherModel)
	ownMapping := q(`INSERT INTO model.form_metric_mapping (model_id, form_id, name, source_field, target_metric_id) VALUES ($1::uuid, $2::uuid, 'own', 'amount', $3::uuid) RETURNING id::text`, modelID, ownForm, metric)
	foreignMapping := q(`INSERT INTO model.form_metric_mapping (model_id, form_id, name, source_field, target_metric_id) VALUES ($1::uuid, $2::uuid, 'theirs', 'amount', $3::uuid) RETURNING id::text`, modelID, foreignForm, metric)
	gridMapping := q(`INSERT INTO model.form_metric_mapping (model_id, form_id, grid_id, name, source_field, target_metric_id) VALUES ($1::uuid, $2::uuid, $3::uuid, 'grid', 'amount', $4::uuid) RETURNING id::text`, modelID, ownForm, foreignGrid, metric)
	if _, err := conn.Exec(ctx, `INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value, entered_by, source_ref) VALUES ($1::uuid, $5::uuid, $2::uuid, '{}', 7, $3::uuid, $4::uuid)`,
		modelID, metric, user0, foreignMapping, rev); err != nil {
		t.Fatal(err)
	}

	apply("139_record_tables.sql")

	// A mapping naming another model's table is gone with what it posted;
	// another model's grid is cleared; the model's own mapping stays.
	var mappings, facts int
	var grid *string
	_ = conn.QueryRow(ctx, `SELECT count(*) FROM model.form_metric_mapping WHERE id = ANY($1::uuid[])`, []string{ownMapping, foreignMapping, gridMapping}).Scan(&mappings)
	_ = conn.QueryRow(ctx, `SELECT count(*) FROM runtime.fact_input WHERE source_ref = $1::uuid`, foreignMapping).Scan(&facts)
	_ = conn.QueryRow(ctx, `SELECT grid_id::text FROM model.form_metric_mapping WHERE id = $1::uuid`, gridMapping).Scan(&grid)
	if mappings != 2 || facts != 0 || grid != nil {
		t.Errorf("after 139: %d of the 3 mappings left (want 2), %d fact(s) of the foreign mapping (want 0), grid %v (want none)", mappings, facts, grid)
	}

	// The records widgets that exist keep their "+ New record" button.
	var withButton, records int
	if err := conn.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE (widget_props->>'new_record_button')::boolean), count(*) FILTER (WHERE widget_type = 'form')
		FROM model.dashboard_widget`).Scan(&withButton, &records); err != nil {
		t.Fatal(err)
	}
	if withButton != 2 || records != 2 {
		t.Errorf("records widgets with the button after 139: %d of %d, want both (and no other widget)", withButton, records)
	}

	rows, err := conn.Query(ctx, `
		SELECT f.name, (SELECT string_agg(e->>'name', ',') FROM jsonb_array_elements(f.fields) e WHERE e->>'type' = 'status'), f.own_records_only
		FROM model.form_def f ORDER BY f.name`)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for rows.Next() {
		var n, field string
		var own bool
		if err := rows.Scan(&n, &field, &own); err != nil {
			t.Fatal(err)
		}
		if own {
			t.Errorf("table %s shows each person their own records after the migration", n)
		}
		got[n] = field
	}
	rows.Close()
	if got["plain"] != "status" || got["taken"] != "record_status" || got["empty"] != "status" {
		t.Errorf("status fields after 139 = %v", got)
	}

	// A record may now have no status.
	var user string
	if err := conn.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email) VALUES ('a', 'a@t.co') RETURNING id::text`).Scan(&user); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO runtime.form_record (form_id, data, created_by)
		SELECT id, '{}', $1::uuid FROM model.form_def WHERE name = 'plain'`, user); err != nil {
		t.Fatalf("a record without a status: %v", err)
	}
	var status *string
	if err := conn.QueryRow(ctx, `SELECT status::text FROM runtime.form_record`).Scan(&status); err != nil || status != nil {
		t.Errorf("a new record's status = %v (%v), want none", status, err)
	}
}

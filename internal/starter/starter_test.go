package starter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/modeltransfer"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// The starter package must import cleanly through the platform's own import
// path and come out whole: every dimension member, metric, dependency, grid
// membership, widget (with its ids remapped) and fact.
func TestStarterImportsWhole(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return id
	}
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	cust := q(`INSERT INTO core.customer (name, plan) VALUES ('Starter Co', 'trial') RETURNING id::text`)
	ws := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Default') RETURNING id::text`, cust)
	app := q(`INSERT INTO core.application (customer_id, workspace_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Getting started', 'planning') RETURNING id::text`, cust, ws)
	user := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('starter-dev', 'dev@starter.test', 'Dev', $1::uuid) RETURNING id::text`, cust)

	pkg := Package()
	if pkg.Format != modeltransfer.PackageFormat || pkg.Version != modeltransfer.PackageVersion || !pkg.IncludeData {
		t.Fatalf("package header = %+v", pkg)
	}
	var modelID, revID string
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		var err error
		modelID, revID, err = modeltransfer.Import(ctx, tx, modeltransfer.ImportRequest{ApplicationID: app, Package: pkg}, user)
		return err
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}

	if n := count(`SELECT count(*) FROM model.dimension_def WHERE model_id=$1::uuid AND revision_id=$2::uuid`, modelID, revID); n != 2 {
		t.Fatalf("dimensions = %d", n)
	}
	if n := count(`SELECT count(*) FROM model.dimension_member m JOIN model.dimension_def d ON d.id=m.dimension_id WHERE d.model_id=$1::uuid`, modelID); n != 17+5 {
		t.Fatalf("members = %d, want 22", n)
	}
	// Three levels in the period dimension: months hang off quarters, which
	// hang off the year.
	if n := count(`SELECT count(*) FROM model.dimension_member m JOIN model.dimension_member p ON p.id=m.parent_member_id JOIN model.dimension_member g ON g.id=p.parent_member_id
		JOIN model.dimension_def d ON d.id=m.dimension_id WHERE d.model_id=$1::uuid AND d.name='period'`, modelID); n != 12 {
		t.Fatalf("months with a quarter and a year above = %d", n)
	}
	if n := count(`SELECT count(*) FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid`, modelID, revID); n != 4 {
		t.Fatalf("metrics = %d", n)
	}
	if n := count(`SELECT count(*) FROM model.calc_dependency cd JOIN model.metric_def m ON m.id=cd.metric_id WHERE m.model_id=$1::uuid`, modelID); n != 4 {
		t.Fatalf("dependencies = %d", n)
	}
	if n := count(`SELECT count(*) FROM model.grid_metric gm JOIN model.grid_def g ON g.id=gm.grid_id WHERE g.model_id=$1::uuid`, modelID); n != 4 {
		t.Fatalf("grid metrics = %d", n)
	}
	if n := count(`SELECT count(*) FROM runtime.fact_input WHERE model_id=$1::uuid`, modelID); n != 4*12*2 {
		t.Fatalf("facts = %d", n)
	}
	var active string
	_ = pool.QueryRow(ctx, `SELECT COALESCE(active_revision_id::text,'') FROM core.model WHERE id=$1::uuid`, modelID).Scan(&active)
	if active != revID {
		t.Fatalf("active revision = %q, want %q", active, revID)
	}

	// The dashboard's widgets point at the model as created, not at the
	// placeholder ids of the package.
	rows, err := pool.Query(ctx, `SELECT w.widget_type, COALESCE(w.ref_id::text,''), COALESCE(w.widget_props::text,'{}')
		FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id=w.dashboard_id WHERE d.model_id=$1::uuid ORDER BY w.sort_order`, modelID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var widgets int
	for rows.Next() {
		var typ, ref, props string
		if err := rows.Scan(&typ, &ref, &props); err != nil {
			t.Fatal(err)
		}
		widgets++
		if strings.Contains(ref, "m-") || strings.Contains(ref, "grid-") || strings.Contains(props, "m-budget") || strings.Contains(props, "dim-department") {
			t.Fatalf("widget %s still carries placeholder ids: ref=%q props=%s", typ, ref, props)
		}
		if typ == "chart" {
			var p struct {
				Chart struct {
					MetricIDs   []string `json:"metric_ids"`
					DimensionID string   `json:"dimension_id"`
				} `json:"chart"`
			}
			_ = json.Unmarshal([]byte(props), &p)
			if len(p.Chart.MetricIDs) != 2 || p.Chart.DimensionID == "" {
				t.Fatalf("chart props not remapped: %s", props)
			}
			if n := count(`SELECT count(*) FROM model.metric_def WHERE id = ANY($1::uuid[]) AND model_id=$2::uuid`, p.Chart.MetricIDs, modelID); n != 2 {
				t.Fatalf("chart metrics resolve to %d rows of this model", n)
			}
		}
		if (typ == "grid" || typ == "metric_kpi") && ref == "" {
			t.Fatalf("%s widget lost its reference", typ)
		}
	}
	if widgets != 6 {
		t.Fatalf("widgets = %d", widgets)
	}
}

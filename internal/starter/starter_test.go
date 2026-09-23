package starter

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/imagedata"
	"github.com/mavericks-engine/mavericks/internal/modeltransfer"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// The starter package must import cleanly through the platform's own import
// path and come out whole: every dimension member, metric, dependency, grid
// membership, widget (with its ids remapped) and fact. It is a guided tour,
// so the prose and the pictures are part of what must survive the trip.
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
	cust := q(`INSERT INTO core.customer (name, plan) VALUES ('Starter Co', 'test') RETURNING id::text`)
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
	if n := count(`SELECT count(*) FROM model.dimension_member m JOIN model.dimension_def d ON d.id=m.dimension_id WHERE d.model_id=$1::uuid`, modelID); n != 3+5 {
		t.Fatalf("members = %d, want 8: 3 teams plus 5 quarters", n)
	}
	// Two levels in each dimension: quarters hang off the year, teams off
	// the whole company, so the tour can show a total nobody typed.
	if n := count(`SELECT count(*) FROM model.dimension_member m JOIN model.dimension_member p ON p.id=m.parent_member_id
		JOIN model.dimension_def d ON d.id=m.dimension_id WHERE d.model_id=$1::uuid AND d.name='quarter'`, modelID); n != 4 {
		t.Fatalf("quarters under the year = %d", n)
	}
	if n := count(`SELECT count(*) FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid`, modelID, revID); n != 3 {
		t.Fatalf("metrics = %d", n)
	}
	if n := count(`SELECT count(*) FROM model.calc_dependency cd JOIN model.metric_def m ON m.id=cd.metric_id WHERE m.model_id=$1::uuid`, modelID); n != 2 {
		t.Fatalf("dependencies = %d", n)
	}
	if n := count(`SELECT count(*) FROM model.grid_metric gm JOIN model.grid_def g ON g.id=gm.grid_id WHERE g.model_id=$1::uuid`, modelID); n != 3 {
		t.Fatalf("grid metrics = %d", n)
	}
	if n := count(`SELECT count(*) FROM runtime.fact_input WHERE model_id=$1::uuid`, modelID); n != 2*4*2 {
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
		if strings.Contains(ref, "m-") || strings.Contains(ref, "grid-") || strings.Contains(props, "m-cost") || strings.Contains(props, "dim-quarter") {
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
			if len(p.Chart.MetricIDs) != 1 || p.Chart.DimensionID == "" {
				t.Fatalf("chart props not remapped: %s", props)
			}
			if n := count(`SELECT count(*) FROM model.metric_def WHERE id = ANY($1::uuid[]) AND model_id=$2::uuid`, p.Chart.MetricIDs, modelID); n != 1 {
				t.Fatalf("chart metrics resolve to %d rows of this model", n)
			}
		}
		if (typ == "grid" || typ == "metric_kpi") && ref == "" {
			t.Fatalf("%s widget lost its reference", typ)
		}
	}
	if widgets < 20 {
		t.Fatalf("widgets = %d, want the whole tour", widgets)
	}
}

// The tour teaches through prose and pictures, so both must arrive intact:
// four dashboards, text that is really Markdown, and diagrams stored as
// image data URLs the browser will display.
func TestStarterCarriesItsTour(t *testing.T) {
	pkg := Package()
	if len(pkg.Dashboards) != 4 {
		t.Fatalf("dashboards = %d, want 4", len(pkg.Dashboards))
	}
	var texts, images, live, prose int
	for _, d := range pkg.Dashboards {
		if len(d.Widgets) == 0 {
			t.Fatalf("dashboard %q is empty", d.Name)
		}
		for _, w := range d.Widgets {
			switch w.WidgetType {
			case "text":
				if w.Content == nil || strings.TrimSpace(*w.Content) == "" {
					t.Fatalf("dashboard %q has an empty text widget", d.Name)
				}
				texts++
				prose += len(*w.Content)
			case "image":
				if w.Content == nil || !strings.HasPrefix(*w.Content, "data:image/svg+xml;base64,") {
					t.Fatalf("dashboard %q has an image that is not an inline data URL", d.Name)
				}
				if err := imagedata.Validate("the image", *w.Content, imagedata.MaxWidgetBytes); err != nil {
					t.Fatalf("dashboard %q: %v", d.Name, err)
				}
				images++
			case "grid", "chart", "metric_kpi":
				live++
			}
		}
	}
	if texts < 12 || images < 4 || live < 4 || prose < 4000 {
		t.Fatalf("tour = %d texts (%d characters), %d pictures, %d live widgets", texts, prose, images, live)
	}
	// Markdown is what the text widget reads, so the tour should use it.
	joined := ""
	for _, d := range pkg.Dashboards {
		for _, w := range d.Widgets {
			if w.WidgetType == "text" && w.Content != nil {
				joined += *w.Content + "\n"
			}
		}
	}
	for _, want := range []string{"# ", "## ", "**", "- "} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the tour never uses %q", want)
		}
	}
	// A tour that teaches the vocabulary must use the platform's own.
	if strings.Contains(joined, "version") {
		t.Fatal("the tour says \"version\"; the unit of change is a revision")
	}
}

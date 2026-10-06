package starter

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/imagedata"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
	"github.com/mavericks-engine/mavericks/internal/modeltransfer"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// repoRoot is where docs/ and scripts/ live, seen from this package's
// directory (go test runs in it).
const repoRoot = "../.."

// metricsAxis is the planning grid's pseudo-dimension for "the metrics".
const metricsAxis = "__metrics__"

// Every package sign-up creates must import whole, the way sign-up imports
// them: all into ONE application, inside ONE transaction, each producing its
// own model with an active revision — and come out with every dimension,
// member, metric, dependency, grid, dashboard, widget and fact present, and
// no placeholder id left behind in a widget's ref or props. The expected
// counts are read from each Package, so the test does not depend on what a
// guide happens to contain.
func TestPackagesImportWhole(t *testing.T) {
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
	cust := q(`INSERT INTO core.customer (name, plan) VALUES ('Starter Co', 'community') RETURNING id::text`)
	ws := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Default') RETURNING id::text`, cust)
	app := q(`INSERT INTO core.application (customer_id, workspace_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Getting started', 'planning') RETURNING id::text`, cust, ws)
	user := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('starter-dev', 'dev@starter.test', 'Dev', $1::uuid) RETURNING id::text`, cust)

	type imported struct {
		pkg            modeltransfer.Package
		modelID, revID string
	}
	var models []imported
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		for _, pkg := range Packages() {
			modelID, revID, err := modeltransfer.Import(ctx, tx, modeltransfer.ImportRequest{ApplicationID: app, Package: pkg}, user)
			if err != nil {
				return fmt.Errorf("%q: %w", pkg.ModelName, err)
			}
			models = append(models, imported{pkg, modelID, revID})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM core.model WHERE application_id=$1::uuid`, app); n != len(Packages()) {
		t.Fatalf("models in the application = %d, want %d", n, len(Packages()))
	}
	for _, m := range models {
		t.Run(m.pkg.ModelName, func(t *testing.T) {
			checkImported(t, ctx, pool, m.pkg, m.modelID, m.revID)
		})
	}
}

func countRows(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

func checkImported(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pkg modeltransfer.Package, modelID, revID string) {
	count := func(sql string, args ...any) int { t.Helper(); return countRows(t, pool, sql, args...) }

	var name, active, revName string
	if err := pool.QueryRow(ctx, `SELECT m.name, COALESCE(m.active_revision_id::text,''), r.name
		FROM core.model m JOIN model.revision r ON r.id=$2::uuid WHERE m.id=$1::uuid`, modelID, revID).Scan(&name, &active, &revName); err != nil {
		t.Fatal(err)
	}
	if name != pkg.ModelName || active != revID || revName != pkg.RevisionName {
		t.Fatalf("model %q active=%q revision %q; want %q active=%q revision %q", name, active, revName, pkg.ModelName, revID, pkg.RevisionName)
	}

	var members, parented, dated, gridMetrics, gridDims, widgets int
	for _, d := range pkg.Dimensions {
		members += len(d.Members)
		for _, m := range d.Members {
			if m.ParentMemberID != nil {
				parented++
			}
			if m.PeriodStart != nil {
				dated++
			}
		}
	}
	for _, g := range pkg.Grids {
		gridMetrics += len(g.Metrics)
		gridDims += len(g.Dimensions)
	}
	for _, d := range pkg.Dashboards {
		widgets += len(d.Widgets)
	}
	formRecords := 0
	if pkg.IncludeData {
		formRecords = len(pkg.FormRecords)
	}
	const inModel = `d.model_id=$1::uuid AND d.revision_id=$2::uuid`
	for _, c := range []struct {
		what string
		sql  string
		want int
	}{
		{"dimensions", `SELECT count(*) FROM model.dimension_def d WHERE ` + inModel, len(pkg.Dimensions)},
		{"members", `SELECT count(*) FROM model.dimension_member m JOIN model.dimension_def d ON d.id=m.dimension_id WHERE ` + inModel, members},
		{"members under a parent", `SELECT count(*) FROM model.dimension_member m JOIN model.dimension_def d ON d.id=m.dimension_id WHERE m.parent_member_id IS NOT NULL AND ` + inModel, parented},
		{"dated time members", `SELECT count(*) FROM model.dimension_member m JOIN model.dimension_def d ON d.id=m.dimension_id WHERE m.period_start IS NOT NULL AND ` + inModel, dated},
		{"metrics", `SELECT count(*) FROM model.metric_def d WHERE ` + inModel, len(pkg.Metrics)},
		{"dependencies", `SELECT count(*) FROM model.calc_dependency cd JOIN model.metric_def d ON d.id=cd.metric_id WHERE ` + inModel, len(pkg.Dependencies)},
		{"grids", `SELECT count(*) FROM model.grid_def d WHERE ` + inModel, len(pkg.Grids)},
		{"grid metrics", `SELECT count(*) FROM model.grid_metric gm JOIN model.grid_def d ON d.id=gm.grid_id WHERE ` + inModel, gridMetrics},
		{"grid dimensions", `SELECT count(*) FROM model.grid_dimension gd JOIN model.grid_def d ON d.id=gd.grid_id WHERE ` + inModel, gridDims},
		{"folders", `SELECT count(*) FROM model.dashboard_folder d WHERE ` + inModel, len(pkg.Folders)},
		{"dashboards", `SELECT count(*) FROM model.dashboard_def d WHERE ` + inModel, len(pkg.Dashboards)},
		{"widgets", `SELECT count(*) FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id=w.dashboard_id WHERE ` + inModel, widgets},
		{"forms", `SELECT count(*) FROM model.form_def d WHERE ` + inModel, len(pkg.Forms)},
		{"form records", `SELECT count(*) FROM runtime.form_record r JOIN model.form_def d ON d.id=r.form_id WHERE ` + inModel, formRecords},
		{"form mappings", `SELECT count(*) FROM model.form_metric_mapping d WHERE ` + inModel, len(pkg.FormMappings)},
		{"integrations", `SELECT count(*) FROM model.integration_def d WHERE ` + inModel, len(pkg.Integrations)},
		{"workflows", `SELECT count(*) FROM workflow.workflow_def WHERE revision_id=$2::uuid AND $1::uuid IS NOT NULL`, len(pkg.Workflows)},
		{"automation rules", `SELECT count(*) FROM workflow.automation_rule WHERE revision_id=$2::uuid AND $1::uuid IS NOT NULL`, len(pkg.AutomationRules)},
		{"facts", `SELECT count(*) FROM runtime.fact_input WHERE model_id=$1::uuid AND revision_id=$2::uuid`, len(pkg.Facts)},
	} {
		if n := count(c.sql, modelID, revID); n != c.want {
			t.Errorf("%s = %d, want %d", c.what, n, c.want)
		}
	}

	checkImportedWidgets(t, ctx, pool, pkg, modelID, revID)
	checkImportedFormulas(t, ctx, pool, modelID, revID)
}

// placeholderIDs is every id a package uses for a thing Import creates, which
// must never survive into a widget of the created model. Member ids are left
// out: widgets name members by code, and a code may be spelled like an id.
func placeholderIDs(pkg modeltransfer.Package) map[string]bool {
	ids := map[string]bool{}
	add := func(id string) {
		if id != "" {
			ids[id] = true
		}
	}
	for _, d := range pkg.Dimensions {
		add(d.ID)
	}
	for _, m := range pkg.Metrics {
		add(m.ID)
	}
	for _, g := range pkg.Grids {
		add(g.ID)
	}
	for _, f := range pkg.Folders {
		add(f.ID)
	}
	for _, d := range pkg.Dashboards {
		add(d.ID)
	}
	for _, f := range pkg.Forms {
		add(f.ID)
	}
	for _, m := range pkg.FormMappings {
		add(m.ID)
	}
	for _, i := range pkg.Integrations {
		add(i.ID)
	}
	for _, w := range pkg.Workflows {
		add(w.ID)
	}
	for _, r := range pkg.AutomationRules {
		add(r.ID)
	}
	return ids
}

// findPlaceholder walks a JSON value and returns the first key or string
// that is a placeholder id.
func findPlaceholder(v any, ids map[string]bool) string {
	switch x := v.(type) {
	case string:
		if ids[x] {
			return x
		}
	case []any:
		for _, e := range x {
			if hit := findPlaceholder(e, ids); hit != "" {
				return hit
			}
		}
	case map[string]any:
		for k, e := range x {
			if ids[k] {
				return k
			}
			if hit := findPlaceholder(e, ids); hit != "" {
				return hit
			}
		}
	}
	return ""
}

// refTarget is where a widget's ref_id must point, by widget type.
var refTarget = map[string]string{
	"grid":               `SELECT count(*) FROM model.grid_def WHERE id=$1::uuid AND model_id=$2::uuid AND revision_id=$3::uuid`,
	"chart":              `SELECT count(*) FROM model.grid_def WHERE id=$1::uuid AND model_id=$2::uuid AND revision_id=$3::uuid`,
	"import":             `SELECT count(*) FROM model.grid_def WHERE id=$1::uuid AND model_id=$2::uuid AND revision_id=$3::uuid`,
	"metric_kpi":         `SELECT count(*) FROM model.metric_def WHERE id=$1::uuid AND model_id=$2::uuid AND revision_id=$3::uuid`,
	"form":               `SELECT count(*) FROM model.form_def WHERE id=$1::uuid AND model_id=$2::uuid AND revision_id=$3::uuid`,
	"integration_button": `SELECT count(*) FROM model.integration_def WHERE id=$1::uuid AND model_id=$2::uuid AND revision_id=$3::uuid`,
	"automation_button":  `SELECT count(*) FROM workflow.automation_rule WHERE id=$1::uuid AND revision_id=$3::uuid AND $2::uuid IS NOT NULL`,
}

// needsRef are the widget types that render nothing without a ref.
var needsRef = map[string]bool{"grid": true, "chart": true, "metric_kpi": true, "import": true, "form": true, "automation_button": true, "integration_button": true}

func checkImportedWidgets(t *testing.T, ctx context.Context, pool *pgxpool.Pool, pkg modeltransfer.Package, modelID, revID string) {
	count := func(sql string, args ...any) int { t.Helper(); return countRows(t, pool, sql, args...) }
	placeholders := placeholderIDs(pkg)
	metricOK := func(id string) bool {
		return count(`SELECT count(*) FROM model.metric_def WHERE id::text=$1 AND model_id=$2::uuid AND revision_id=$3::uuid`, id, modelID, revID) == 1
	}
	dimOK := func(id string) bool {
		return count(`SELECT count(*) FROM model.dimension_def WHERE id::text=$1 AND model_id=$2::uuid AND revision_id=$3::uuid`, id, modelID, revID) == 1
	}
	onGrid := func(table, col, gridID, id string) bool {
		return count(`SELECT count(*) FROM model.`+table+` WHERE grid_id::text=$1 AND `+col+`::text=$2`, gridID, id) == 1
	}

	rows, err := pool.Query(ctx, `SELECT d.name, w.widget_type, COALESCE(w.ref_id::text,''), COALESCE(w.widget_props::text,'{}')
		FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id=w.dashboard_id
		WHERE d.model_id=$1::uuid AND d.revision_id=$2::uuid ORDER BY d.name, w.sort_order`, modelID, revID)
	if err != nil {
		t.Fatal(err)
	}
	type widget struct{ dash, typ, ref, props string }
	var ws []widget
	for rows.Next() {
		var w widget
		if err := rows.Scan(&w.dash, &w.typ, &w.ref, &w.props); err != nil {
			t.Fatal(err)
		}
		ws = append(ws, w)
	}
	rows.Close()

	for _, w := range ws {
		where := fmt.Sprintf("%q %s widget", w.dash, w.typ)
		if needsRef[w.typ] && w.ref == "" {
			t.Errorf("%s has no ref, so it renders nothing", where)
		}
		if w.ref != "" {
			if placeholders[w.ref] {
				t.Errorf("%s still points at placeholder %q", where, w.ref)
			} else if sql, ok := refTarget[w.typ]; ok && count(sql, w.ref, modelID, revID) != 1 {
				t.Errorf("%s ref %s is not a %s of this model's revision", where, w.ref, w.typ)
			}
		}
		var p map[string]any
		if err := json.Unmarshal([]byte(w.props), &p); err != nil {
			t.Errorf("%s props are not JSON: %v", where, err)
			continue
		}
		if hit := findPlaceholder(p, placeholders); hit != "" {
			t.Errorf("%s props still carry placeholder %q: %s", where, hit, w.props)
		}
		if chart, ok := p["chart"].(map[string]any); ok {
			ids, _ := chart["metric_ids"].([]any)
			for _, raw := range ids {
				id, _ := raw.(string)
				if !metricOK(id) || !onGrid("grid_metric", "metric_id", w.ref, id) {
					t.Errorf("%s plots metric %q, which is not on its grid", where, id)
				}
			}
			for _, k := range []string{"x_metric_id", "y_metric_id"} {
				if id, ok := chart[k].(string); ok && id != "" && !metricOK(id) {
					t.Errorf("%s %s %q is not a metric of this model", where, k, id)
				}
			}
			if dim, _ := chart["dimension_id"].(string); !dimOK(dim) || !onGrid("grid_dimension", "dimension_id", w.ref, dim) {
				t.Errorf("%s plots dimension %q, which is not on its grid", where, dim)
			}
			if cd, ok := chart["context_defaults"].(map[string]any); ok {
				for dim := range cd {
					if !dimOK(dim) {
						t.Errorf("%s context default for unknown dimension %q", where, dim)
					}
				}
			}
		}
		if scope, ok := p["kpi_scope"].(map[string]any); ok {
			dim, _ := scope["dimension_id"].(string)
			code, _ := scope["member_code"].(string)
			if n := count(`SELECT count(*) FROM model.dimension_member m JOIN model.dimension_def d ON d.id=m.dimension_id
				WHERE d.id::text=$1 AND d.model_id=$2::uuid AND d.revision_id=$3::uuid AND m.code=$4`, dim, modelID, revID, code); n != 1 {
				t.Errorf("%s is pinned to %q in dimension %q, which this model does not have", where, code, dim)
			}
		}
		if view, ok := p["default_view"].(map[string]any); ok {
			for _, axis := range []string{"rows", "cols", "context"} {
				ids, _ := view[axis].([]any)
				for _, raw := range ids {
					id, _ := raw.(string)
					if id != metricsAxis && (!dimOK(id) || !onGrid("grid_dimension", "dimension_id", w.ref, id)) {
						t.Errorf("%s default view puts %q on its %s, which is not a dimension of its grid", where, id, axis)
					}
				}
			}
			if sel, ok := view["filter_sel"].(map[string]any); ok {
				for dim := range sel {
					if !dimOK(dim) {
						t.Errorf("%s default view selects in unknown dimension %q", where, dim)
					}
				}
			}
		}
	}
}

// Every calculated metric must pass the check the developer's save runs
// (metricformula.Validate, as an update so self-references and cycles are
// judged too), and the calc_dependency rows the package wrote must be exactly
// the edges that check resolves from the formula, offsets included — the
// scheduler reads those rows, not the formula. The revision must also pass
// the check that publishing it runs (metricformula.ValidateTime), since the
// import makes it live without asking.
func checkImportedFormulas(t *testing.T, ctx context.Context, pool *pgxpool.Pool, modelID, revID string) {
	rows, err := pool.Query(ctx, `SELECT id::text, name, formula FROM model.metric_def
		WHERE model_id=$1::uuid AND revision_id=$2::uuid AND COALESCE(formula,'') <> '' ORDER BY name`, modelID, revID)
	if err != nil {
		t.Fatal(err)
	}
	type metric struct{ id, name, formula string }
	var ms []metric
	for rows.Next() {
		var m metric
		if err := rows.Scan(&m.id, &m.name, &m.formula); err != nil {
			t.Fatal(err)
		}
		ms = append(ms, m)
	}
	rows.Close()

	edgeKey := func(to string, e metricformula.Edge) string {
		return fmt.Sprintf("%s [%d,%d] past=%t future=%t", to, e.MinTimeOffset, e.MaxTimeOffset, e.UnboundedPast, e.UnboundedFuture)
	}
	for _, m := range ms {
		res, err := metricformula.Validate(ctx, pool, metricformula.Request{ModelID: modelID, RevisionID: revID, MetricID: m.id, Name: m.name, Formula: m.formula})
		if err != nil {
			t.Errorf("metric %s: formula %q is refused by the save-time check: %v", m.name, m.formula, err)
			continue
		}
		var want []string
		for _, e := range res.Edges {
			to := e.To
			if to == metricformula.SelfReference {
				to = m.id
			}
			want = append(want, edgeKey(to, e))
		}
		drows, err := pool.Query(ctx, `SELECT depends_on_metric_id::text, min_time_offset, max_time_offset, unbounded_past, unbounded_future
			FROM model.calc_dependency WHERE metric_id=$1::uuid`, m.id)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for drows.Next() {
			var to string
			var e metricformula.Edge
			if err := drows.Scan(&to, &e.MinTimeOffset, &e.MaxTimeOffset, &e.UnboundedPast, &e.UnboundedFuture); err != nil {
				t.Fatal(err)
			}
			got = append(got, edgeKey(to, e))
		}
		drows.Close()
		sort.Strings(want)
		sort.Strings(got)
		if strings.Join(got, "; ") != strings.Join(want, "; ") {
			t.Errorf("metric %s (%s): package dependencies\n  %v\nbut the formula reads\n  %v", m.name, m.formula, got, want)
		}
	}
	if err := metricformula.ValidateTime(ctx, pool, modelID, revID); err != nil {
		t.Errorf("the revision could not be made live: %v", err)
	}
}

// ── What each package carries, checked without a database ─────────────────

// TestPackagesCarryTheirGuides holds every package to the same shape: a
// numbered, tagged sequence of pages that open with a title and hand on to
// the next page, widgets that point at what they draw, prose in the Markdown
// the text widget reads, diagrams the browser will display, the platform's
// own vocabulary, formulas with their dependencies, and links that resolve.
func TestPackagesCarryTheirGuides(t *testing.T) {
	names := map[string]bool{}
	for _, pkg := range Packages() {
		if names[pkg.ModelName] || pkg.ModelName == "" {
			t.Fatalf("model name %q is empty or used twice", pkg.ModelName)
		}
		names[pkg.ModelName] = true
		t.Run(pkg.ModelName, func(t *testing.T) {
			if pkg.Format != modeltransfer.PackageFormat || pkg.Version != modeltransfer.PackageVersion || !pkg.IncludeData || pkg.RevisionName != RevisionName {
				t.Errorf("package header = %s %d data=%t revision %q", pkg.Format, pkg.Version, pkg.IncludeData, pkg.RevisionName)
			}
			checkPages(t, pkg)
			checkWidgetRefs(t, pkg)
			checkProse(t, pkg)
			checkVocabulary(t, pkg)
			checkFormulas(t, pkg)
			checkLinks(t, pkg)
		})
	}
}

// textWidgets returns a dashboard's text widgets in reading order.
func textWidgets(d modeltransfer.Dashboard) []modeltransfer.Widget {
	var out []modeltransfer.Widget
	for _, w := range d.Widgets {
		if w.WidgetType == "text" {
			out = append(out, w)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return intOf(out[i].PosY) < intOf(out[j].PosY) })
	return out
}

func intOf(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

var pageName = regexp.MustCompile(`^(\d+) · \S`)

// checkPages: dashboards numbered "1 · …" in order, unique, tagged "guide"
// under "Getting started"; each opens with a "# " title and hands on to the
// next by its exact name; widgets placed on the page without overlapping.
func checkPages(t *testing.T, pkg modeltransfer.Package) {
	if len(pkg.Dashboards) == 0 {
		t.Errorf("no pages")
		return
	}
	seen := map[string]bool{}
	for i, d := range pkg.Dashboards {
		if seen[d.Name] {
			t.Errorf("page %q appears twice", d.Name)
		}
		seen[d.Name] = true
		m := pageName.FindStringSubmatch(d.Name)
		if m == nil || m[1] != strconv.Itoa(i+1) {
			t.Errorf("page %d is named %q, want %q", i+1, d.Name, strconv.Itoa(i+1)+" · …")
		}
		if !slices.Contains(d.Tags, "guide") || d.Category != "Getting started" {
			t.Errorf("page %q: tags %v, category %q; want tag guide, category Getting started", d.Name, d.Tags, d.Category)
		}
		if len(d.Widgets) == 0 {
			t.Errorf("page %q is empty", d.Name)
			continue
		}
		texts := textWidgets(d)
		if len(texts) == 0 || !strings.HasPrefix(content(texts[0]), "# ") {
			t.Errorf("page %q does not open with a \"# \" title", d.Name)
		}
		if len(texts) > 0 {
			last := content(texts[len(texts)-1])
			if i+1 < len(pkg.Dashboards) {
				if next := "Next: **" + pkg.Dashboards[i+1].Name + "**"; !strings.Contains(last, next) {
					t.Errorf("page %q does not end by naming the next page (%s)", d.Name, next)
				}
			} else if strings.Contains(last, "Next: **") {
				t.Errorf("the last page %q points at a next page", d.Name)
			}
		}
		checkLayout(t, d)
	}
}

// checkLayout: every widget has a place inside the reading measure, and no
// two widgets cover the same spot.
func checkLayout(t *testing.T, d modeltransfer.Dashboard) {
	type box struct{ x, y, w, h int }
	var boxes []box
	for _, w := range d.Widgets {
		if w.PosX == nil || w.PosY == nil || w.SizeW == nil || w.SizeH == nil {
			t.Errorf("page %q: a %s widget has no position or size", d.Name, w.WidgetType)
			continue
		}
		b := box{*w.PosX, *w.PosY, *w.SizeW, *w.SizeH}
		if b.x < 0 || b.y < 0 || b.w <= 0 || b.h <= 0 || b.x+b.w > pageWidth {
			t.Errorf("page %q: a %s widget sits at %+v, outside the %d px page", d.Name, w.WidgetType, b, pageWidth)
		}
		for _, o := range boxes {
			if b.x < o.x+o.w && o.x < b.x+b.w && b.y < o.y+o.h && o.y < b.y+b.h {
				t.Errorf("page %q: a %s widget at %+v overlaps one at %+v", d.Name, w.WidgetType, b, o)
			}
		}
		boxes = append(boxes, b)
	}
}

func content(w modeltransfer.Widget) string {
	if w.Content == nil {
		return ""
	}
	return *w.Content
}

// checkWidgetRefs: every widget that draws something points at it, with the
// ids its props carry belonging to what it points at.
func checkWidgetRefs(t *testing.T, pkg modeltransfer.Package) {
	grids := map[string]modeltransfer.Grid{}
	for _, g := range pkg.Grids {
		grids[g.ID] = g
	}
	metrics := map[string]bool{}
	for _, m := range pkg.Metrics {
		metrics[m.ID] = true
	}
	dims := map[string]modeltransfer.Dimension{}
	for _, d := range pkg.Dimensions {
		dims[d.ID] = d
	}
	ids := func(kind string) map[string]bool {
		out := map[string]bool{}
		switch kind {
		case "form":
			for _, f := range pkg.Forms {
				out[f.ID] = true
			}
		case "integration":
			for _, i := range pkg.Integrations {
				out[i.ID] = true
			}
		case "rule":
			for _, r := range pkg.AutomationRules {
				out[r.ID] = true
			}
		}
		return out
	}
	gridHasMetric := func(g modeltransfer.Grid, id string) bool {
		for _, m := range g.Metrics {
			if m.MetricID == id {
				return true
			}
		}
		return false
	}
	gridHasDim := func(g modeltransfer.Grid, id string) bool {
		for _, d := range g.Dimensions {
			if d.DimensionID == id {
				return true
			}
		}
		return false
	}
	hasMember := func(dimID, code string) bool {
		for _, m := range dims[dimID].Members {
			if m.Code == code {
				return true
			}
		}
		return false
	}

	for _, d := range pkg.Dashboards {
		for _, w := range d.Widgets {
			where := fmt.Sprintf("page %q %s widget", d.Name, w.WidgetType)
			ref := ""
			if w.RefID != nil {
				ref = *w.RefID
			}
			if needsRef[w.WidgetType] && ref == "" {
				t.Errorf("%s has no ref, so it renders nothing", where)
				continue
			}
			var p map[string]any
			if len(w.Props) > 0 {
				if err := json.Unmarshal(w.Props, &p); err != nil {
					t.Errorf("%s props are not JSON: %v", where, err)
					continue
				}
			}
			switch w.WidgetType {
			case "grid", "chart", "import":
				g, ok := grids[ref]
				if !ok {
					t.Errorf("%s points at %q, which is not a grid of the package", where, ref)
					continue
				}
				if w.WidgetType == "chart" {
					chart, _ := p["chart"].(map[string]any)
					if chart == nil {
						t.Errorf("%s has no chart settings", where)
						continue
					}
					if typ, _ := chart["chart_type"].(string); typ == "" {
						t.Errorf("%s has no chart type", where)
					}
					plotted, _ := chart["metric_ids"].([]any)
					if len(plotted) == 0 {
						if x, _ := chart["x_metric_id"].(string); x == "" {
							t.Errorf("%s plots no metric", where)
						}
					}
					for _, raw := range plotted {
						if id, _ := raw.(string); !gridHasMetric(g, id) {
							t.Errorf("%s plots %q, which is not on grid %q", where, id, g.Name)
						}
					}
					if dim, _ := chart["dimension_id"].(string); !gridHasDim(g, dim) {
						t.Errorf("%s plots over %q, which is not on grid %q", where, dim, g.Name)
					}
				}
				if w.WidgetType == "grid" {
					if view, ok := p["default_view"].(map[string]any); ok {
						placed := map[string]int{}
						for _, axis := range []string{"rows", "cols", "context"} {
							list, ok := view[axis].([]any)
							if !ok {
								// PlanningGrid reads all three lists; a missing one breaks the grid.
								t.Errorf("%s default view has no %s list", where, axis)
								continue
							}
							for _, raw := range list {
								id, _ := raw.(string)
								placed[id]++
								if id != metricsAxis && !gridHasDim(g, id) {
									t.Errorf("%s default view places %q, which is not on grid %q", where, id, g.Name)
								}
							}
						}
						for _, gd := range g.Dimensions {
							if placed[gd.DimensionID] != 1 {
								t.Errorf("%s default view places dimension %q %d times, want once", where, gd.DimensionID, placed[gd.DimensionID])
							}
						}
						if placed[metricsAxis] != 1 {
							t.Errorf("%s default view places the metrics %d times, want once", where, placed[metricsAxis])
						}
					}
				}
			case "metric_kpi":
				if !metrics[ref] {
					t.Errorf("%s points at %q, which is not a metric of the package", where, ref)
				}
				mode, _ := p["kpi_context_mode"].(string)
				switch mode {
				case "", "total", "sync":
				case "pin":
					scope, _ := p["kpi_scope"].(map[string]any)
					dim, _ := scope["dimension_id"].(string)
					code, _ := scope["member_code"].(string)
					if _, ok := dims[dim]; !ok || !hasMember(dim, code) {
						t.Errorf("%s is pinned to %q in %q, which the package does not have", where, code, dim)
					}
				default:
					t.Errorf("%s has context mode %q; the widget knows total, sync and pin", where, mode)
				}
			case "form":
				if !ids("form")[ref] {
					t.Errorf("%s points at %q, which is not a form of the package", where, ref)
				}
			case "integration_button":
				if !ids("integration")[ref] {
					t.Errorf("%s points at %q, which is not an integration of the package", where, ref)
				}
			case "automation_button":
				if !ids("rule")[ref] {
					t.Errorf("%s points at %q, which is not a trigger of the package", where, ref)
				}
			}
		}
	}
}

// The Markdown the text widget reads (web/src/ui/RichText.tsx): # ## ###
// headings, **bold**, *italic*, `code`, [label](target), "- " and "1. "
// lists, "> " quotes, "---" rules. Anything else shows as literal text.
var (
	mdLink       = regexp.MustCompile(`\[[^\]]+\]\([^)\s]+\)`)
	mdCode       = regexp.MustCompile("`[^`]+`")
	mdBadLine    = regexp.MustCompile(`^(#{4,}\s|\* |\+ |` + "```" + `|\||===|<[A-Za-z/!])`)
	mdHTMLInline = regexp.MustCompile(`<[A-Za-z/][^>]*>`)
)

// checkProse: text widgets hold Markdown the widget can render, image
// widgets hold an inline SVG data URL the browser will show.
func checkProse(t *testing.T, pkg modeltransfer.Package) {
	for _, d := range pkg.Dashboards {
		for _, w := range d.Widgets {
			switch w.WidgetType {
			case "text":
				body := content(w)
				if strings.TrimSpace(body) == "" {
					t.Errorf("page %q has an empty text widget", d.Name)
					continue
				}
				for para := range strings.SplitSeq(body, "\n\n") {
					plain := mdCode.ReplaceAllString(mdLink.ReplaceAllString(para, "L"), "C")
					for ln := range strings.SplitSeq(para, "\n") {
						if mdBadLine.MatchString(ln) {
							t.Errorf("page %q: %q is not Markdown the text widget reads", d.Name, ln)
						}
					}
					if mdHTMLInline.MatchString(plain) {
						t.Errorf("page %q: HTML in %q shows as literal text", d.Name, para)
					}
					if strings.Count(plain, "**")%2 != 0 {
						t.Errorf("page %q: unbalanced ** in %q", d.Name, para)
					}
					if strings.Count(strings.ReplaceAll(plain, "**", ""), "*")%2 != 0 {
						t.Errorf("page %q: unbalanced * in %q", d.Name, para)
					}
					if strings.Contains(plain, "](") {
						t.Errorf("page %q: a link in %q is not [label](target) with no spaces in the target", d.Name, para)
					}
				}
			case "image":
				c := content(w)
				if !strings.HasPrefix(c, "data:image/svg+xml;base64,") {
					t.Errorf("page %q has an image that is not an inline SVG data URL", d.Name)
					continue
				}
				if err := imagedata.Validate("the image", c, imagedata.MaxWidgetBytes); err != nil {
					t.Errorf("page %q: %v", d.Name, err)
				}
				var p map[string]any
				_ = json.Unmarshal(w.Props, &p)
				if alt, _ := p["alt"].(string); strings.TrimSpace(alt) == "" {
					t.Errorf("page %q has a picture with no alt text", d.Name)
				}
			}
		}
	}
}

// svgText is the words an inline SVG data URL shows, tags stripped.
func svgText(dataURL string) string {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(dataURL, "data:image/svg+xml;base64,"))
	if err != nil {
		return ""
	}
	return regexp.MustCompile(`<[^>]*>`).ReplaceAllString(string(raw), " ")
}

var versionWord = regexp.MustCompile(`(?i)\bversion`)

// checkVocabulary: the unit of change is a revision. No "version" in
// anything a reader sees — prose, titles, pictures, names — while
// "conversion" and the like stay fine.
func checkVocabulary(t *testing.T, pkg modeltransfer.Package) {
	seen := []string{pkg.ModelName, pkg.RevisionName}
	for _, d := range pkg.Dimensions {
		seen = append(seen, d.Name)
		for _, m := range d.Members {
			seen = append(seen, m.Label, m.Code)
		}
	}
	for _, m := range pkg.Metrics {
		seen = append(seen, m.Name)
	}
	for _, g := range pkg.Grids {
		seen = append(seen, g.Name)
	}
	for _, f := range pkg.Folders {
		seen = append(seen, f.Name)
	}
	for _, f := range pkg.Forms {
		seen = append(seen, f.Name, f.Label, string(f.Fields))
	}
	for _, w := range pkg.Workflows {
		seen = append(seen, w.Name, w.Description, string(w.Steps), string(w.ContextSchema))
	}
	for _, r := range pkg.AutomationRules {
		seen = append(seen, r.Name, r.Description)
	}
	for _, i := range pkg.Integrations {
		seen = append(seen, i.Name)
	}
	for _, d := range pkg.Dashboards {
		seen = append(seen, d.Name, d.Category, strings.Join(d.Tags, " "))
		for _, w := range d.Widgets {
			if w.Title != nil {
				seen = append(seen, *w.Title)
			}
			var p map[string]any
			_ = json.Unmarshal(w.Props, &p)
			if alt, ok := p["alt"].(string); ok {
				seen = append(seen, alt)
			}
			switch w.WidgetType {
			case "image":
				seen = append(seen, svgText(content(w)))
			default:
				seen = append(seen, content(w))
			}
		}
	}
	for _, s := range seen {
		if loc := versionWord.FindStringIndex(s); loc != nil {
			from, to := max(0, loc[0]-40), min(len(s), loc[1]+40)
			t.Errorf("says \"version\" (%q); the unit of change is a revision", s[from:to])
		}
	}
}

// checkFormulas: inputs have no formula, calculated metrics have one, and
// every dependency joins two metrics of the package, from a calculated one.
// Whether a formula passes the engine's own check, and whether the
// dependencies are exactly what it reads, needs the created model:
// TestPackagesImportWhole checks both.
func checkFormulas(t *testing.T, pkg modeltransfer.Package) {
	calculated := map[string]bool{}
	known := map[string]bool{}
	for _, m := range pkg.Metrics {
		known[m.ID] = true
		hasFormula := m.Formula != nil && strings.TrimSpace(*m.Formula) != ""
		if hasFormula == m.IsInput {
			t.Errorf("metric %s: input=%t but formula present=%t", m.Name, m.IsInput, hasFormula)
		}
		if hasFormula {
			calculated[m.ID] = true
		}
	}
	for _, dep := range pkg.Dependencies {
		if !calculated[dep.MetricID] || !known[dep.DependsOn] {
			t.Errorf("dependency %s → %s does not join a calculated metric to a metric of the package", dep.MetricID, dep.DependsOn)
		}
	}
}

// checkLinks: every link in the prose goes somewhere that answers — one of
// the two manuals the console serves (and whose file is in docs/), a
// document of the public repository (in docs/ and not held back from the
// public snapshot), the product's own site, or an e-mail address.
func checkLinks(t *testing.T, pkg modeltransfer.Package) {
	excluded := publicExclusions(t)
	target := regexp.MustCompile(`\[[^\]]+\]\(([^)\s]+)\)`)
	for _, d := range pkg.Dashboards {
		for _, w := range d.Widgets {
			if w.WidgetType != "text" {
				continue
			}
			for _, m := range target.FindAllStringSubmatch(content(w), -1) {
				link := m[1]
				bare, _, _ := strings.Cut(link, "#")
				switch {
				case strings.HasPrefix(link, "/"):
					if bare != formulasManual && bare != developerManual {
						t.Errorf("page %q links to %q; the console serves only %s and %s", d.Name, link, formulasManual, developerManual)
					} else if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(strings.TrimPrefix(bare, "/")))); err != nil {
						t.Errorf("page %q links to %q, which is not in the repository: %v", d.Name, link, err)
					}
				case strings.HasPrefix(link, publicDocs):
					doc := "docs/" + strings.TrimPrefix(bare, publicDocs)
					if _, err := os.Stat(filepath.Join(repoRoot, filepath.FromSlash(doc))); err != nil {
						t.Errorf("page %q links to %q, and %s does not exist", d.Name, link, doc)
					} else if pattern := excludedBy(doc, excluded); pattern != "" {
						t.Errorf("page %q links to %q, but %s stays private (scripts/public-exclude.txt: %s)", d.Name, link, doc, pattern)
					}
				case strings.HasPrefix(link, "https://"):
					u, err := url.Parse(link)
					if err != nil || (u.Hostname() != "maverickbuilds.app" && !strings.HasSuffix(u.Hostname(), ".maverickbuilds.app")) {
						t.Errorf("page %q links to %q; outside links go to maverickbuilds.app or the public docs only", d.Name, link)
					}
				case strings.HasPrefix(link, "mailto:"):
				default:
					t.Errorf("page %q has a link %q of a kind the guides do not use", d.Name, link)
				}
			}
		}
	}
}

// publicExclusions reads scripts/public-exclude.txt: one path or shell glob
// per line, # comments and blank lines ignored. The public repository is
// exported without that file (it lists itself), and without every path it
// names, so there it excludes nothing: a link to an excluded document already
// fails the check that the linked file exists.
func publicExclusions(t *testing.T) []string {
	f, err := os.Open(filepath.Join(repoRoot, "scripts", "public-exclude.txt"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("public exclusions: %v", err)
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, strings.TrimSuffix(line, "/"))
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("public exclusions: %v", err)
	}
	return out
}

func excludedBy(p string, patterns []string) string {
	for _, pat := range patterns {
		if ok, _ := path.Match(pat, p); ok || p == pat || strings.HasPrefix(p, pat+"/") {
			return pat
		}
	}
	return ""
}

// ── The tour itself ────────────────────────────────────────────────────────

// The tour is the page a new tenant lands on, and the sign-up test finds it
// by its first dashboard's name. Its example is small enough to count, and
// its live widgets must show what their titles and prose say.
func TestTourShape(t *testing.T) {
	pkg := Package()
	if pkg.ModelName != ModelName || len(pkg.Dashboards) != 4 || pkg.Dashboards[0].Name != DashboardName {
		t.Fatalf("tour = %q with %d pages, first %q", pkg.ModelName, len(pkg.Dashboards), pkg.Dashboards[0].Name)
	}
	var members, dated int
	for _, d := range pkg.Dimensions {
		members += len(d.Members)
		for _, m := range d.Members {
			if m.PeriodStart != nil {
				dated++
			}
		}
	}
	if len(pkg.Dimensions) != 2 || members != 3+5 || dated != 4 || len(pkg.Metrics) != 3 || len(pkg.Dependencies) != 2 || len(pkg.Facts) != 2*4*2 {
		t.Fatalf("example = %d dimensions, %d members (%d dated), %d metrics, %d dependencies, %d facts",
			len(pkg.Dimensions), members, dated, len(pkg.Metrics), len(pkg.Dependencies), len(pkg.Facts))
	}

	// Totals must mean something: quarter is a time dimension, headcount's
	// year is its last quarter, cost per head's the mean, cost's the sum.
	var quarter modeltransfer.Dimension
	for _, d := range pkg.Dimensions {
		if d.ID == dimQuarter {
			quarter = d
		}
	}
	if quarter.DimensionType != "time" || quarter.TimeGranularity == nil || *quarter.TimeGranularity != "quarter" {
		t.Fatalf("quarter = %+v, want a quarterly time dimension", quarter)
	}
	summary := map[string]string{}
	for _, m := range pkg.Metrics {
		summary[m.ID] = m.TimeSummary
	}
	if summary[metHeadcnt] != "last" || summary[metPerHead] != "average" || summary[metCost] != "sum" {
		t.Fatalf("time summaries = %v", summary)
	}

	var texts, images, live, prose int
	kpis := map[string]map[string]any{}
	for _, d := range pkg.Dashboards {
		for _, w := range d.Widgets {
			var p map[string]any
			_ = json.Unmarshal(w.Props, &p)
			switch w.WidgetType {
			case "text":
				texts++
				prose += len(content(w))
			case "image":
				images++
			case "grid":
				live++
				view, _ := p["default_view"].(map[string]any)
				if fmt.Sprint(view["rows"]) != fmt.Sprint([]any{dimTeam, metricsAxis}) || fmt.Sprint(view["cols"]) != fmt.Sprint([]any{dimQuarter}) {
					t.Errorf("grid layout = %v; the prose describes teams (with their metrics) on rows and quarters across", view)
				}
			case "chart":
				live++
				if w.RefID == nil || *w.RefID != gridMain {
					t.Errorf("the chart does not draw from the example grid")
				}
			case "metric_kpi":
				live++
				kpis[*w.RefID] = p
			}
		}
	}
	if texts < 12 || images < 4 || live < 4 || prose < 4000 {
		t.Fatalf("tour = %d texts (%d characters), %d pictures, %d live widgets", texts, prose, images, live)
	}
	// "Total cost, FY 2026" is the whole-model total; "Headcount at year end"
	// is pinned to Q4, since a headcount does not add up over the year.
	if kpis[metCost]["kpi_context_mode"] != "total" {
		t.Errorf("cost card = %v, want the whole-model total", kpis[metCost])
	}
	if scope, _ := kpis[metHeadcnt]["kpi_scope"].(map[string]any); kpis[metHeadcnt]["kpi_context_mode"] != "pin" || scope["dimension_id"] != dimQuarter || scope["member_code"] != "Q4" {
		t.Errorf("headcount card = %v, want it pinned to Q4", kpis[metHeadcnt])
	}
	// The close names the three guides beside it, by their model names.
	last := textWidgets(pkg.Dashboards[3])
	closing := content(last[len(last)-1])
	for _, name := range []string{DeveloperGuideName, BusinessAdminGuideName, TenantAdminGuideName} {
		if !strings.Contains(closing, "**"+name+"**") {
			t.Errorf("the tour's close does not name %q", name)
		}
	}
}

// The "one number" picture on page 2 names a team, a quarter and the cost
// there. It is drawn from the example's data, so it must show what the
// package's own facts give that cell — headcount × cost per head — and label
// its rows and columns with the members the grid shows.
func TestOneNumberFollowsTheData(t *testing.T) {
	for n, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 48000: "48,000", 621000: "621,000", -1234567: "-1,234,567"} {
		if got := thousands(n); got != want {
			t.Errorf("thousands(%d) = %q, want %q", n, got, want)
		}
	}

	pkg := Package()
	var head, perHead float64
	for _, f := range pkg.Facts {
		var at map[string]string
		if err := json.Unmarshal(f.DimMembers, &at); err != nil {
			t.Fatal(err)
		}
		if at[dimTeam] != teams[0].code || at[dimQuarter] != quarters[0].code {
			continue
		}
		switch f.MetricID {
		case metHeadcnt:
			head = f.Value
		case metPerHead:
			perHead = f.Value
		}
	}
	if head == 0 || perHead == 0 {
		t.Fatalf("no facts for %s/%s", teams[0].code, quarters[0].code)
	}

	var picture string
	for _, w := range pkg.Dashboards[1].Widgets {
		if w.WidgetType == "image" && w.Content != nil && strings.Contains(string(w.Props), "addresses one value") {
			picture = svgText(*w.Content)
		}
	}
	if picture == "" {
		t.Fatal("page 2 has no one-number picture")
	}
	words := strings.Fields(picture)
	if want := thousands(int(head * perHead)); !slices.Contains(words, want) {
		t.Errorf("the picture shows %q, want the cost the facts give, %s", picture, want)
	}
	for _, tm := range teams {
		if !strings.Contains(picture, tm.label) {
			t.Errorf("the picture has no row for %s", tm.label)
		}
	}
	for _, q := range quarters {
		if !slices.Contains(words, q.code) {
			t.Errorf("the picture has no column for %s", q.code)
		}
	}
}

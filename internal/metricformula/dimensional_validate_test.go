package metricformula

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// dimFixture is a revision with dimensions, members, property declarations,
// metrics and grids, seeded directly: this package's checks read the
// schema, and the HTTP paths that write it are exercised by the gateway
// tests.
//
//	region   World -> EMEA, US          properties: segment (text), factor (number)
//	segment  SMB, ENT                   grouped from region.segment (source_property)
//	product  P1, P2
//	channel  WEB                        (a metric named channel exists too)
//	month    2026-01 .. 2026-03         time dimension
//
//	grid G1 [region, month]: revenue, flow, opening, closing, selfy
//	grid G2 [product]:       (empty until a test places "other")
type dimFixture struct {
	pool                                          *pgxpool.Pool
	modelID, revID                                string
	region, segment, product, channel, month      string
	g1, g2                                        string
	revenue, flow, opening, closing, selfy, other string
	channelMetric                                 string
}

func setupDimFixture(t *testing.T) *dimFixture {
	t.Helper()
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	f := &dimFixture{pool: pool}
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}
	cust := q(`INSERT INTO core.customer (name, plan) VALUES ('Dim Co', 'enterprise') RETURNING id::text`)
	ws := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'W') RETURNING id::text`, cust)
	app := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'A', 'planning') RETURNING id::text`, ws, cust)
	f.modelID = q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M') RETURNING id::text`, app)
	f.revID = q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, f.modelID)

	dim := func(name string) string {
		return q(`INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type) VALUES ($1::uuid, $2::uuid, $3, 'standard') RETURNING id::text`,
			f.modelID, f.revID, name)
	}
	member := func(dimID, code, parent, props string) string {
		return q(`INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id, properties)
			VALUES ($1::uuid, $2, $2, NULLIF($3,'')::uuid, $4::jsonb) RETURNING id::text`, dimID, code, parent, props)
	}
	f.region = dim("region")
	world := member(f.region, "World", "", `{}`)
	member(f.region, "EMEA", world, `{"segment":"SMB","factor":"2"}`)
	member(f.region, "US", world, `{"segment":"ENT","factor":"3"}`)
	exec(`INSERT INTO model.dimension_property (dimension_id, name, data_type) VALUES ($1::uuid, 'segment', 'text'), ($1::uuid, 'factor', 'number')`, f.region)
	f.segment = dim("segment")
	exec(`UPDATE model.dimension_def SET source_dimension_id=$2::uuid, source_property='segment' WHERE id=$1::uuid`, f.segment, f.region)
	member(f.segment, "SMB", "", `{}`)
	member(f.segment, "ENT", "", `{}`)
	f.product = dim("product")
	member(f.product, "P1", "", `{}`)
	member(f.product, "P2", "", `{}`)
	f.channel = dim("channel")
	member(f.channel, "WEB", "", `{}`)

	f.month = q(`INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type, time_granularity, fiscal_year_start_month)
		VALUES ($1::uuid, $2::uuid, 'month', 'time', 'month', 1) RETURNING id::text`, f.modelID, f.revID)
	for i, code := range []string{"2026-01", "2026-02", "2026-03"} {
		exec(`INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end, time_index)
			VALUES ($1::uuid, $2, $2, $3::date, ($3::date + interval '1 month' - interval '1 day')::date, $4)`,
			f.month, code, code+"-01", i)
	}

	metric := func(name, formulaText string, input bool) string {
		return q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, formula) VALUES ($1::uuid, $2::uuid, $3, $4, NULLIF($5,'')) RETURNING id::text`,
			f.modelID, f.revID, name, input, formulaText)
	}
	f.revenue = metric("revenue", "", true)
	f.flow = metric("flow", "", true)
	f.other = metric("other", "", true)
	f.channelMetric = metric("channel", "", true)
	f.closing = metric("closing", "opening + flow", false)
	f.opening = metric("opening", "LAG(closing, 1, 0)", false)
	f.selfy = metric("selfy", "revenue", false)
	exec(`INSERT INTO model.calc_dependency (metric_id, depends_on_metric_id, min_time_offset, max_time_offset) VALUES
		($1::uuid, $2::uuid, 0, 0), ($1::uuid, $3::uuid, 0, 0), ($3::uuid, $1::uuid, -1, -1), ($4::uuid, $5::uuid, 0, 0)`,
		f.closing, f.opening, f.flow, f.selfy, f.revenue)
	// opening = LAG(closing): the $3 -> $1 edge above is opening -> closing.

	grid := func(name string, dims []string, metrics []string) string {
		g := q(`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, $3) RETURNING id::text`, f.modelID, f.revID, name)
		for _, d := range dims {
			exec(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid, $2::uuid)`, g, d)
		}
		for _, m := range metrics {
			exec(`INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid, $2::uuid)`, g, m)
		}
		return g
	}
	f.g1 = grid("G1", []string{f.region, f.month}, []string{f.revenue, f.flow, f.opening, f.closing, f.selfy})
	f.g2 = grid("G2", []string{f.product}, nil)
	return f
}

func (f *dimFixture) validate(t *testing.T, metricID, name, text string) (*Result, error) {
	t.Helper()
	return Validate(context.Background(), f.pool, Request{ModelID: f.modelID, RevisionID: f.revID, MetricID: metricID, Name: name, Formula: text})
}

func codeOf(err error) string {
	var ve *ValidationError
	if errors.As(err, &ve) {
		return ve.Code
	}
	return ""
}

func TestValidateDimensionalReferences(t *testing.T) {
	f := setupDimFixture(t)

	for _, tc := range []struct {
		name, formula, code string
	}{
		// C1: dim.property and PARENT.
		{"declared property", `revenue * region.factor`, ""},
		{"property names match case-insensitively", `IF(REGION.Segment = "SMB", 1, 0)`, ""},
		{"undeclared property", `region.colour`, formula.CodeUnknownProperty},
		{"property of an unknown dimension", `nowhere.segment`, formula.CodeDimensionArgRequired},
		{"property of a metric", `revenue.segment`, formula.CodeDimensionArgRequired},
		{"PARENT of a dimension", `IF(PARENT(region) = "World", 1, 0)`, ""},
		{"PARENT of a metric", `PARENT(revenue)`, formula.CodeDimensionArgRequired},
		{"PARENT of an unknown name", `PARENT(nowhere)`, formula.CodeDimensionArgRequired},
		// A dimension and a metric share the name "channel": a dimension
		// argument means the dimension.
		{"dimension argument shadowed by a metric", `COUNTIFS(channel, "WEB")`, ""},
		// C2: LOOKUP.
		{"LOOKUP a parent member", `LOOKUP(revenue, region, "World")`, ""},
		{"LOOKUP an unknown member", `LOOKUP(revenue, region, "Atlantis")`, formula.CodeUnknownMember},
		{"LOOKUP with a dimension as source", `LOOKUP(region, region, "EMEA")`, formula.CodeSourceMustBeMetric},
		{"LOOKUP with an expression as source", `LOOKUP(revenue + 1, region, "EMEA")`, formula.CodeSourceMustBeMetric},
		{"LOOKUP with a metric as dimension", `LOOKUP(flow, revenue, "EMEA")`, formula.CodeDimensionArgRequired},
		{"LOOKUP along a dimension not on the source", `LOOKUP(revenue, product, "P1")`, formula.CodeDimensionNotOnSource},
		{"LOOKUP along a property-related dimension", `LOOKUP(revenue, segment, "SMB")`, ""},
		{"LOOKUP with conflicting overrides", `LOOKUP(revenue, region, "EMEA", segment, "SMB")`, formula.CodeConflictingDimensions},
		{"LOOKUP along a time dimension", `LOOKUP(revenue, month, "2026-01")`, ""},
		{"LOOKUP an unknown period", `LOOKUP(revenue, month, "2031-01")`, formula.CodeUnknownMember},
		// C3: conditional aggregation.
		{"SUMIFS on a property range", `SUMIFS(revenue, region.segment, "SMB")`, ""},
		{"SUMIFS on an undeclared property", `SUMIFS(revenue, region.tier, "SMB")`, formula.CodeUnknownProperty},
		{"SUMIFS along a dimension not on the source", `SUMIFS(revenue, product, "P*")`, formula.CodeDimensionNotOnSource},
		{"SUMIFS with conflicting ranges", `SUMIFS(revenue, region, "E*", segment, "SMB")`, formula.CodeConflictingDimensions},
		{"COUNTIFS on any dimension", `COUNTIFS(product, "P*")`, ""},
		{"SUMIF with a dimension as source", `SUMIF(region, "E*", product)`, formula.CodeSourceMustBeMetric},
		// C5: *VALUE sources.
		{"YEARVALUE of a dimension", `YEARVALUE(region)`, formula.CodeSourceMustBeMetric},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.validate(t, "", "new_metric", tc.formula)
			if tc.code == "" {
				if err != nil {
					t.Fatalf("%s: want saved, got %v", tc.formula, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%s: want %s, got saved", tc.formula, tc.code)
			}
			if got := codeOf(err); got != tc.code {
				t.Fatalf("%s: code %q, want %s (%v)", tc.formula, got, tc.code, err)
			}
			if !IsValidationError(err) {
				t.Errorf("%s: must be a ValidationError (400), got %T", tc.formula, err)
			}
		})
	}
}

// A same-named metric never becomes a dependency edge through a dimension
// argument, and a LOOKUP / criteria range over the time dimension records
// its source unbounded past and future (C2, C3).
func TestValidateDimensionalEdges(t *testing.T) {
	f := setupDimFixture(t)
	edgeTo := func(res *Result, id string) (Edge, bool) {
		for _, e := range res.Edges {
			if e.To == id {
				return e, true
			}
		}
		return Edge{}, false
	}

	res, err := f.validate(t, "", "n1", `COUNTIFS(channel, "WEB")`)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := edgeTo(res, f.channelMetric); ok || len(res.Edges) != 0 {
		t.Errorf("a dimension argument must not become an edge: %+v", res.Edges)
	}

	for _, text := range []string{`LOOKUP(revenue, month, "2026-01")`, `SUMIFS(revenue, month, "2026-0*")`} {
		res, err := f.validate(t, "", "n2", text)
		if err != nil {
			t.Fatalf("%s: %v", text, err)
		}
		e, ok := edgeTo(res, f.revenue)
		if !ok || !e.UnboundedPast || !e.UnboundedFuture {
			t.Errorf("%s: the source edge must be unbounded past and future, got %+v (found %v)", text, e, ok)
		}
	}
	res, err = f.validate(t, "", "n3", `LOOKUP(revenue, region, "EMEA")`)
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := edgeTo(res, f.revenue); !ok || e.UnboundedPast || e.UnboundedFuture || e.MinTimeOffset != 0 || e.MaxTimeOffset != 0 {
		t.Errorf("a LOOKUP along a standard dimension reads the current period only: %+v", e)
	}
}

func TestValidateDimensionalSelfReference(t *testing.T) {
	f := setupDimFixture(t)
	for _, tc := range []struct{ id, name, text string }{
		{f.selfy, "selfy", `LOOKUP(selfy, region, "EMEA")`},
		{"", "brand_new", `SUMIFS(brand_new, region, "E*")`},
		{f.selfy, "selfy", `YEARVALUE(selfy)`},
	} {
		_, err := f.validate(t, tc.id, tc.name, tc.text)
		if err == nil {
			t.Fatalf("%s: a metric reading itself through a dimensional function must be refused", tc.text)
		}
		msg := err.Error()
		if strings.Contains(msg, "PREVIOUS") || strings.Contains(msg, "LAG") {
			t.Errorf("%s: message must not talk about PREVIOUS/LAG: %s", tc.text, msg)
		}
		if !strings.Contains(msg, tc.name) {
			t.Errorf("%s: message should name the metric: %s", tc.text, msg)
		}
	}
}

// C4: a recurrence member may not call LOOKUP or a conditional aggregation,
// whatever it reads — checked with the metric's own ID so its component is
// the real one.
func TestValidateRefusesDimensionalCallInRecurrence(t *testing.T) {
	f := setupDimFixture(t)
	if _, err := f.validate(t, f.closing, "closing", `opening + flow`); err != nil {
		t.Fatalf("the plain recurrence must stay valid: %v", err)
	}
	for fn, text := range map[string]string{
		"COUNTIFS": `opening + flow + COUNTIFS(product, "P*")`,
		"LOOKUP":   `opening + LOOKUP(flow, region, "EMEA")`,
	} {
		_, err := f.validate(t, f.closing, "closing", text)
		if codeOf(err) != formula.CodeTemporalCycleNotCausal || !strings.Contains(err.Error(), fn) {
			t.Errorf("%s in a recurrence: want TEMPORAL_CYCLE_NOT_CAUSAL naming %s, got %v", text, fn, err)
		}
	}
	// ValidateTime (activation) sees it too once the formula is stored
	// around the validator.
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE model.metric_def SET formula=$2 WHERE id=$1::uuid`, f.closing, `opening + flow + COUNTIFS(product, "P*")`); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTime(ctx, f.pool, f.modelID, f.revID); codeOf(err) != formula.CodeTemporalCycleNotCausal {
		t.Errorf("activation: want TEMPORAL_CYCLE_NOT_CAUSAL, got %v", err)
	}
}

// Placement: DIMENSION_NOT_ON_SOURCE is re-checked when a grid gains a
// metric, for dependents on OTHER grids too, and at activation; TIMESUM's
// literal period codes are checked against the metric's axis.
func TestPlacementChecksDimensionalCalls(t *testing.T) {
	f := setupDimFixture(t)
	ctx := context.Background()

	// "other" is unplaced: its dimensions are unknown, so the save passes.
	res, err := f.validate(t, "", "lk", `LOOKUP(other, region, "EMEA")`)
	if err != nil {
		t.Fatalf("an unplaced source cannot be checked at save: %v", err)
	}
	var lk string
	if err := f.pool.QueryRow(ctx, `INSERT INTO model.metric_def (model_id, revision_id, name, is_input, formula)
		VALUES ($1::uuid, $2::uuid, 'lk', false, $3) RETURNING id::text`, f.modelID, f.revID, `LOOKUP(other, region, "EMEA")`).Scan(&lk); err != nil {
		t.Fatal(err)
	}
	if err := WriteDependencies(ctx, f.pool, lk, res.Edges); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid, $2::uuid)`, f.g1, lk); err != nil {
		t.Fatal(err)
	}
	if err := ValidateGridTime(ctx, f.pool, f.modelID, f.revID, f.g1); err != nil {
		t.Fatalf("G1 with lk placed: %v", err)
	}
	// Placing "other" on G2 [product] fixes its dimensions: lk, on G1,
	// now reads it along a dimension it does not have.
	if _, err := f.pool.Exec(ctx, `INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid, $2::uuid)`, f.g2, f.other); err != nil {
		t.Fatal(err)
	}
	if err := ValidateGridTime(ctx, f.pool, f.modelID, f.revID, f.g2); codeOf(err) != formula.CodeDimensionNotOnSource {
		t.Errorf("placing the source on G2: want DIMENSION_NOT_ON_SOURCE for the dependent on G1, got %v", err)
	}
	if err := ValidateTime(ctx, f.pool, f.modelID, f.revID); codeOf(err) != formula.CodeDimensionNotOnSource {
		t.Errorf("activation: want DIMENSION_NOT_ON_SOURCE, got %v", err)
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM model.grid_metric WHERE metric_id=$1::uuid`, f.other); err != nil {
		t.Fatal(err)
	}

	// TIMESUM: literal codes against the placed metric's time axis.
	if _, err := f.validate(t, f.selfy, "selfy", `TIMESUM(revenue, "2026-01", "2026-03")`); err != nil {
		t.Errorf("known periods: %v", err)
	}
	if _, err := f.validate(t, f.selfy, "selfy", `TIMESUM(revenue, "2026-01", "2099-12")`); codeOf(err) != formula.CodeUnknownMember {
		t.Errorf("unknown period at save of a placed metric: want UNKNOWN_MEMBER, got %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE model.metric_def SET formula=$2 WHERE id=$1::uuid`, f.selfy, `TIMESUM(revenue, "2026-01", "2099-12")`); err != nil {
		t.Fatal(err)
	}
	if err := ValidateGridTime(ctx, f.pool, f.modelID, f.revID, f.g1); codeOf(err) != formula.CodeUnknownMember {
		t.Errorf("placement: want UNKNOWN_MEMBER for the TIMESUM period, got %v", err)
	}
}

func TestFormulaReferencesDimension(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{`IF(region = "EMEA", 1, 0)`, true},
		{`revenue * Region.factor`, true},
		{`IF(PARENT(REGION) = "World", 1, 0)`, true},
		{`LOOKUP(revenue, region, "EMEA")`, true},
		{`SUMIFS(revenue, region.segment, "SMB")`, true},
		{`COUNTIFS(region, "E*")`, true},
		{`revenue * 2`, false},
		{`LOOKUP(revenue, product, "P1")`, false},
		{`regional_total + 1`, false},
		{`=(`, false},
	} {
		if got := FormulaReferencesDimension(tc.text, "region"); got != tc.want {
			t.Errorf("FormulaReferencesDimension(%q, region) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestMetricsReferencingDimensionAndPropertyDeclarations(t *testing.T) {
	f := setupDimFixture(t)
	ctx := context.Background()
	var uses string
	if err := f.pool.QueryRow(ctx, `INSERT INTO model.metric_def (model_id, revision_id, name, is_input, formula)
		VALUES ($1::uuid, $2::uuid, 'uses_region', false, 'revenue * region.factor') RETURNING id::text`, f.modelID, f.revID).Scan(&uses); err != nil {
		t.Fatal(err)
	}
	byRev, err := MetricsReferencingDimension(ctx, f.pool, f.modelID, f.revID, "REGION")
	if err != nil {
		t.Fatal(err)
	}
	if got := byRev[f.revID]; len(got) != 1 || got[0] != uses {
		t.Errorf("metrics referencing region: %v, want [%s]", got, uses)
	}

	var segmentProp string
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_property WHERE dimension_id=$1::uuid AND name='segment'`, f.region).Scan(&segmentProp); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ propID, name, typ, code string }{
		{"", "tier", "text", ""},
		{"", "launch_date", "date", ""},
		{"", "Sales Segment", "text", CodeInvalidPropertyName},
		{"", "1st", "text", CodeInvalidPropertyName},
		{"", "a.b", "text", CodeInvalidPropertyName},
		{"", "", "text", CodeInvalidPropertyName},
		{"", "tier", "money", CodeInvalidPropertyType},
		{"", "SEGMENT", "text", CodePropertyNameTaken},
		{segmentProp, "Segment", "text", ""}, // renaming a property to itself in another case
		{segmentProp, "factor", "text", CodePropertyNameTaken},
	} {
		err := ValidatePropertyDeclaration(ctx, f.pool, f.region, tc.propID, tc.name, tc.typ)
		if got := codeOf(err); got != tc.code || (tc.code == "" && err != nil) {
			t.Errorf("declare %q %s: got %v, want code %q", tc.name, tc.typ, err, tc.code)
		}
	}

	// A rename carries the values and the grouping with it.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := RenamePropertyValues(ctx, tx, f.region, "segment", "tier"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var withOld, withNew int
	if err := f.pool.QueryRow(ctx, `SELECT COUNT(*) FILTER (WHERE properties ? 'segment'), COUNT(*) FILTER (WHERE properties->>'tier' IS NOT NULL)
		FROM model.dimension_member WHERE dimension_id=$1::uuid`, f.region).Scan(&withOld, &withNew); err != nil {
		t.Fatal(err)
	}
	if withOld != 0 || withNew != 2 {
		t.Errorf("after rename: %d members keep 'segment', %d have 'tier'; want 0 and 2", withOld, withNew)
	}
	var sourceProp string
	if err := f.pool.QueryRow(ctx, `SELECT source_property FROM model.dimension_def WHERE id=$1::uuid`, f.segment).Scan(&sourceProp); err != nil {
		t.Fatal(err)
	}
	if sourceProp != "tier" {
		t.Errorf("segment's source_property = %q, want tier", sourceProp)
	}
}

// Removing a dimension from a grid shrinks its metrics' dimensions: a
// LOOKUP on another grid that reads one of them along the removed
// dimension is then refused, as adding would have been.
func TestGridDimensionRemovalChecksDependents(t *testing.T) {
	f := setupDimFixture(t)
	ctx := context.Background()

	var lk string
	if err := f.pool.QueryRow(ctx, `INSERT INTO model.metric_def (model_id, revision_id, name, is_input, formula)
		VALUES ($1::uuid, $2::uuid, 'lk_rev', false, $3) RETURNING id::text`, f.modelID, f.revID, `LOOKUP(revenue, region, "EMEA")`).Scan(&lk); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid, $2::uuid)`, f.g2, lk); err != nil {
		t.Fatal(err)
	}
	if err := ValidateGridDimensional(ctx, f.pool, f.modelID, f.revID, f.g1); err != nil {
		t.Fatalf("revenue still has region: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM model.grid_dimension WHERE grid_id=$1::uuid AND dimension_id=$2::uuid`, f.g1, f.region); err != nil {
		t.Fatal(err)
	}
	if err := ValidateGridDimensional(ctx, f.pool, f.modelID, f.revID, f.g1); codeOf(err) != formula.CodeDimensionNotOnSource {
		t.Errorf("region removed from revenue's grid: want DIMENSION_NOT_ON_SOURCE for lk_rev on G2, got %v", err)
	}
}

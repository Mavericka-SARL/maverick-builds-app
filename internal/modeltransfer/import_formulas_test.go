package modeltransfer

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// formulaPackage is a small model whose calculated metrics are given per
// case: Region (one member, N), a Month time dimension, an input "rev",
// and the case's metrics, all on one grid over Region and Month.
func formulaPackage(metrics []Metric, deps []Dependency) Package {
	str := func(s string) *string { return &s }
	month := "month"
	fy := 1
	pkg := Package{
		ModelName: "Formulas",
		Dimensions: []Dimension{
			{ID: "dim-region", Name: "Region", Members: []Member{{ID: "mem-n", Code: "N", Label: "North"}}},
			{ID: "dim-month", Name: "Month", DimensionType: "time", TimeGranularity: &month, FiscalYearStart: &fy, Members: []Member{
				{ID: "mem-jan", Code: "2026-01", Label: "Jan", PeriodStart: str("2026-01-01"), PeriodEnd: str("2026-01-31")},
				{ID: "mem-feb", Code: "2026-02", Label: "Feb", PeriodStart: str("2026-02-01"), PeriodEnd: str("2026-02-28")}}},
		},
		Metrics:      append([]Metric{{ID: "met-rev", Name: "rev", IsInput: true}}, metrics...),
		Dependencies: deps,
	}
	grid := Grid{ID: "grid-plan", Name: "Plan", Dimensions: []GridDimension{{DimensionID: "dim-region"}, {DimensionID: "dim-month"}}}
	for _, m := range pkg.Metrics {
		grid.Metrics = append(grid.Metrics, GridMetric{MetricID: m.ID})
	}
	pkg.Grids = []Grid{grid}
	return pkg
}

// An import is held to the checks a formula's save applies (metricformula
// Validate, ValidateMemberFormula) and to the time checks publishing a
// revision applies (ValidateTime), since Import makes its revision the
// live one. Each of these imported before, and was refused only when
// someone next saved the formula, or computed wrong values until then.
func TestImportRefusesInvalidFormulas(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	app, user := importTarget(t, ctx, pool)
	str := func(s string) *string { return &s }
	calc := func(id, name, f string) Metric { return Metric{ID: id, Name: name, Formula: str(f)} }
	month := "month"
	fy := 1
	twoTimes := formulaPackage(nil, nil)
	twoTimes.Dimensions = append(twoTimes.Dimensions, Dimension{ID: "dim-week", Name: "Week", DimensionType: "time",
		TimeGranularity: &month, FiscalYearStart: &fy, Members: []Member{
			{ID: "mem-w1", Code: "2027-01", Label: "Jan 27", PeriodStart: str("2027-01-01"), PeriodEnd: str("2027-01-31")}}})
	twoTimes.Grids[0].Dimensions = append(twoTimes.Grids[0].Dimensions, GridDimension{DimensionID: "dim-week"})
	memberFormula := formulaPackage(nil, nil)
	memberFormula.Dimensions[0].Members = append(memberFormula.Dimensions[0].Members,
		Member{ID: "mem-var", Code: "VAR", Label: "Variance", Formula: "{N} - {NOPE}"})

	for _, c := range []struct {
		name string
		pkg  Package
		want string
	}{
		{"literal member that does not exist", formulaPackage([]Metric{calc("c", "c", `LOOKUP(rev, Region, "NOPE")`)}, nil), "UNKNOWN_MEMBER"},
		{"undeclared property", formulaPackage([]Metric{calc("c", "c", `IF(Region.colour = "x", 1, 0)`)}, nil), "UNKNOWN_PROPERTY"},
		{"formula that does not parse", formulaPackage([]Metric{calc("c", "c", `rev +* 2`)}, nil), "does not parse"},
		{"unknown function", formulaPackage([]Metric{calc("c", "c", `TOTAL(rev)`)}, nil), "unknown function"},
		{"unknown metric", formulaPackage([]Metric{calc("c", "c", `nosuch * 2`)}, nil), "unknown metric or dimension"},
		{"self reference", formulaPackage([]Metric{calc("c", "selfref", `selfref + 1`)}, nil), "references the metric it defines"},
		{"same-period cycle", formulaPackage([]Metric{calc("a", "a", `b + 1`), calc("b", "b", `a + 1`)}, nil), "TEMPORAL_CYCLE_NOT_CAUSAL"},
		{"member formula naming an unknown member", memberFormula, "INVALID_MEMBER_FORMULA"},
		{"grid with two time dimensions", twoTimes, "MULTIPLE_TIME_DIMENSIONS"},
	} {
		_, _, err := importInto(ctx, pool, app, user, c.pkg)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: import error = %v, want %q", c.name, err, c.want)
		}
	}
}

// A calculated metric's dependency edges are derived from its formula, as
// every save derives them, not copied from the package: the scheduler
// orders a recalculation and finds what an edit reaches by them, so an
// edge a hand edit left behind (c2 -> other, where c2 reads c1) computed
// c2 before c1 and left it 0.
func TestImportDerivesDependencyEdges(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	app, user := importTarget(t, ctx, pool)
	str := func(s string) *string { return &s }
	pkg := formulaPackage([]Metric{
		{ID: "met-other", Name: "other", IsInput: true},
		{ID: "met-c1", Name: "c1", Formula: str("rev + 1")},
		{ID: "met-c2", Name: "c2", Formula: str("c1 * 2")},
		{ID: "met-bal", Name: "bal", Formula: str("PREVIOUS(bal) + rev")},
	}, []Dependency{{MetricID: "met-c1", DependsOn: "met-rev"}, {MetricID: "met-c2", DependsOn: "met-other"}})
	pkg.Facts = []Fact{
		{MetricID: "met-rev", DimMembers: []byte(`{"dim-region": "N", "dim-month": "2026-01"}`), Value: 5},
		{MetricID: "met-other", DimMembers: []byte(`{"dim-region": "N", "dim-month": "2026-01"}`), Value: 1},
	}
	modelID, revID, err := importInto(ctx, pool, app, user, pkg)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	edges := texts(t, ctx, pool, `
		SELECT m.name || '->' || t.name || '[' || d.min_time_offset || ',' || d.max_time_offset || ']'
		FROM model.calc_dependency d
		JOIN model.metric_def m ON m.id = d.metric_id
		JOIN model.metric_def t ON t.id = d.depends_on_metric_id
		WHERE m.revision_id = $1::uuid`, revID)
	sort.Strings(edges)
	if got, want := strings.Join(edges, " "), "bal->bal[-1,-1] bal->rev[0,0] c1->rev[0,0] c2->c1[0,0]"; got != want {
		t.Errorf("calc_dependency = %s, want %s", got, want)
	}

	ids := map[string]string{}
	for _, row := range texts(t, ctx, pool, `SELECT name || '=' || id::text FROM model.metric_def WHERE revision_id = $1::uuid`, revID) {
		name, id, _ := strings.Cut(row, "=")
		ids[name] = id
	}
	var regionID, monthID string
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT id::text FROM model.dimension_def WHERE revision_id = $1::uuid AND name = 'Region'),
		       (SELECT id::text FROM model.dimension_def WHERE revision_id = $1::uuid AND name = 'Month')`, revID).Scan(&regionID, &monthID); err != nil {
		t.Fatalf("dimensions: %v", err)
	}
	c2 := func() float64 {
		t.Helper()
		var v float64
		if err := pool.QueryRow(ctx, `
			SELECT COALESCE(SUM(value), 0) FROM runtime.calc_result
			WHERE revision_id = $1::uuid AND metric_id = $2::uuid AND dim_members = jsonb_build_object($3::text, 'N', $4::text, '2026-01')`,
			revID, ids["c2"], regionID, monthID).Scan(&v); err != nil {
			t.Fatalf("c2: %v", err)
		}
		return v
	}
	sched := calculation.NewScheduler(zerolog.Nop(), calculation.NewStore(pool))
	if err := sched.RecalcFromAllInputs(ctx, modelID, revID, []string{ids["rev"], ids["other"]}); err != nil {
		t.Fatalf("recalc: %v", err)
	}
	if got := c2(); got != 12 {
		t.Errorf("c2 after import = %v, want 12 ((5+1)*2)", got)
	}
	if _, err := pool.Exec(ctx, `UPDATE runtime.fact_input SET value = 10 WHERE revision_id = $1::uuid AND metric_id = $2::uuid`, revID, ids["rev"]); err != nil {
		t.Fatalf("set rev: %v", err)
	}
	if err := sched.RecalcAffected(ctx, modelID, revID, []string{ids["rev"]}); err != nil {
		t.Fatalf("recalc affected: %v", err)
	}
	if got := c2(); got != 22 {
		t.Errorf("c2 after rev = 10 = %v, want 22 ((10+1)*2)", got)
	}
}

// Package starter is the model a self-service tenant starts with: a small
// budget-versus-actual planning model with a year of figures, a grid and a
// dashboard, so the first screen after sign-up shows numbers rather than an
// empty workspace.
//
// It is expressed as a modeltransfer.Package and created through
// modeltransfer.Import — the same path a tenant admin's package import
// takes — so the starter exercises code every customer relies on and needs
// no fixture file to regenerate when the schema moves.
package starter

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/mavericks-engine/mavericks/internal/modeltransfer"
)

// Names of what the package creates, for callers that want to find them.
const (
	ModelName     = "Budget vs Actual"
	RevisionName  = "FY2026 Plan"
	GridName      = "Budget vs Actual by department"
	DashboardName = "Budget overview"
)

// Placeholder ids: Import allocates real ones and remaps every reference.
const (
	dimPeriod     = "dim-period"
	dimDepartment = "dim-department"
	metBudget     = "m-budget"
	metActual     = "m-actual"
	metVariance   = "m-variance"
	metVariancePc = "m-variance-pct"
	gridMain      = "grid-main"
	dashMain      = "dash-main"
)

var months = []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}

// departments and their monthly budget; actuals drift around it so the
// variance columns have something to say. Values are in thousands.
var departments = []struct {
	code, label   string
	monthlyBudget float64
	drift         []float64 // actual = budget * (1 + drift[month])
}{
	{"SALES", "Sales", 120, []float64{-0.04, 0.02, 0.05, -0.01, 0.03, 0.08, 0.02, -0.03, 0.04, 0.06, 0.09, 0.12}},
	{"MKT", "Marketing", 60, []float64{0.10, 0.06, -0.05, 0.02, 0.15, 0.04, -0.08, -0.02, 0.05, 0.11, 0.18, 0.07}},
	{"ENG", "Engineering", 200, []float64{-0.02, -0.01, 0.01, 0.03, 0.02, 0.04, 0.05, 0.03, 0.06, 0.04, 0.02, 0.01}},
	{"OPS", "Operations", 80, []float64{0.01, -0.03, -0.02, 0.00, 0.02, -0.01, 0.03, 0.02, -0.04, 0.01, 0.00, 0.05}},
}

func str(s string) *string { return &s }
func num(n int) *int       { return &n }

// Package builds the starter model. Deterministic: the same package every
// time, so a test can count what it contains.
func Package() modeltransfer.Package {
	// Period: year → quarters → months. Three levels, because a rollup that
	// is right at the top and wrong in the middle is the classic bug and a
	// starter model should let a person see quarters.
	period := modeltransfer.Dimension{ID: dimPeriod, Name: "period", AggRule: "sum"}
	period.Members = append(period.Members, modeltransfer.Member{ID: "p-2026", Code: "FY2026", Label: "FY 2026", SortOrder: 0})
	for q := 0; q < 4; q++ {
		qid := fmt.Sprintf("p-q%d", q+1)
		period.Members = append(period.Members, modeltransfer.Member{ID: qid, Code: fmt.Sprintf("Q%d", q+1), Label: fmt.Sprintf("Q%d 2026", q+1),
			ParentMemberID: str("p-2026"), SortOrder: 1 + q*4})
		for m := q * 3; m < q*3+3; m++ {
			period.Members = append(period.Members, modeltransfer.Member{ID: "p-" + months[m], Code: months[m], Label: months[m] + " 2026",
				ParentMemberID: str(qid), SortOrder: 2 + q*4 + (m - q*3)})
		}
	}

	dept := modeltransfer.Dimension{ID: dimDepartment, Name: "department", AggRule: "sum"}
	dept.Members = append(dept.Members, modeltransfer.Member{ID: "d-all", Code: "COMPANY", Label: "Whole company", SortOrder: 0})
	for i, d := range departments {
		dept.Members = append(dept.Members, modeltransfer.Member{ID: "d-" + d.code, Code: d.code, Label: d.label, ParentMemberID: str("d-all"), SortOrder: i + 1})
	}

	// Formulas use the stored form: a leading "=" and metric names.
	metrics := []modeltransfer.Metric{
		{ID: metBudget, Name: "budget", IsInput: true, StorageType: "oltp", AggRule: "sum", Format: "currency", FormatCurrency: "$"},
		{ID: metActual, Name: "actual", IsInput: true, StorageType: "oltp", AggRule: "sum", Format: "currency", FormatCurrency: "$"},
		{ID: metVariance, Name: "variance", Formula: str("=actual-budget"), StorageType: "oltp", AggRule: "sum", Format: "currency", FormatCurrency: "$"},
		{ID: metVariancePc, Name: "variance_pct", Formula: str("=ROUND((actual-budget)/budget*100,1)"), StorageType: "oltp", AggRule: "formula", Format: "percentage", FormatDecimals: 1, FormatCurrency: "$"},
	}
	deps := []modeltransfer.Dependency{
		{MetricID: metVariance, DependsOn: metActual}, {MetricID: metVariance, DependsOn: metBudget},
		{MetricID: metVariancePc, DependsOn: metActual}, {MetricID: metVariancePc, DependsOn: metBudget},
	}

	grid := modeltransfer.Grid{
		ID: gridMain, Name: GridName,
		Metrics:    []modeltransfer.GridMetric{{MetricID: metBudget, SortOrder: 0}, {MetricID: metActual, SortOrder: 1}, {MetricID: metVariance, SortOrder: 2}, {MetricID: metVariancePc, SortOrder: 3}},
		Dimensions: []modeltransfer.GridDimension{{DimensionID: dimDepartment}, {DimensionID: dimPeriod}},
	}

	chartProps, _ := json.Marshal(map[string]any{
		"chart":        map[string]any{"chart_type": "bar", "metric_ids": []string{metBudget, metActual}, "dimension_id": dimDepartment},
		"sync_context": true,
	})
	gridProps, _ := json.Marshal(map[string]any{"sync_context": true})
	dashboard := modeltransfer.Dashboard{
		ID: dashMain, Name: DashboardName, Tags: []string{"finance"}, Category: "Finance",
		Widgets: []modeltransfer.Widget{
			{WidgetType: "text", Content: str("Budget versus actual spend by department for FY 2026. Edit the blue cells in the grid: variance and variance % recalculate as you type."),
				SortOrder: 0, ColStart: 1, ColSpan: 12, PosX: num(0), PosY: num(0), SizeW: num(1180), SizeH: num(60), Title: str("Welcome"), ShowTitle: true},
			{WidgetType: "metric_kpi", RefID: str(metBudget), SortOrder: 1, ColStart: 1, ColSpan: 4, PosX: num(0), PosY: num(80), SizeW: num(380), SizeH: num(120), Title: str("Budget"), ShowTitle: true, Props: json.RawMessage(`{}`)},
			{WidgetType: "metric_kpi", RefID: str(metActual), SortOrder: 2, ColStart: 5, ColSpan: 4, PosX: num(400), PosY: num(80), SizeW: num(380), SizeH: num(120), Title: str("Actual"), ShowTitle: true, Props: json.RawMessage(`{}`)},
			{WidgetType: "metric_kpi", RefID: str(metVariance), SortOrder: 3, ColStart: 9, ColSpan: 4, PosX: num(800), PosY: num(80), SizeW: num(380), SizeH: num(120), Title: str("Variance"), ShowTitle: true, Props: json.RawMessage(`{}`)},
			{WidgetType: "chart", SortOrder: 4, ColStart: 1, ColSpan: 12, PosX: num(0), PosY: num(220), SizeW: num(1180), SizeH: num(320), Title: str("Budget vs actual by department"), ShowTitle: true, Props: json.RawMessage(chartProps)},
			{WidgetType: "grid", RefID: str(gridMain), SortOrder: 5, ColStart: 1, ColSpan: 12, PosX: num(0), PosY: num(560), SizeW: num(1180), SizeH: num(420), Title: str(GridName), ShowTitle: true, Props: json.RawMessage(gridProps)},
		},
	}

	var facts []modeltransfer.Fact
	for _, d := range departments {
		for m, month := range months {
			members, _ := json.Marshal(map[string]string{dimDepartment: d.code, dimPeriod: month})
			budget := d.monthlyBudget * 1000
			actual := float64(int(budget*(1+d.drift[m])/100)) * 100 // round to hundreds
			facts = append(facts,
				modeltransfer.Fact{MetricID: metBudget, DimMembers: members, Value: budget},
				modeltransfer.Fact{MetricID: metActual, DimMembers: members, Value: actual},
			)
		}
	}

	return modeltransfer.Package{
		Format: modeltransfer.PackageFormat, Version: modeltransfer.PackageVersion, ExportedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ModelName: ModelName, StorageType: "oltp", RevisionName: RevisionName, IncludeData: true,
		Dimensions: []modeltransfer.Dimension{dept, period}, Metrics: metrics, Dependencies: deps,
		Grids: []modeltransfer.Grid{grid}, Dashboards: []modeltransfer.Dashboard{dashboard},
		Facts: facts,
	}
}

package formula_test

import (
	"math"
	"sort"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

func TestRoleDeveloperFormulaAuthor(t *testing.T) {
	metricNames := map[string]bool{
		"revenue":         true,
		"cogs":            true,
		"gross_profit":    true,
		"commission":      true,
		"commission_rate": true,
	}
	dimensionNames := map[string]bool{
		"department":       true,
		"month":            true,
		"region":           true,
		"product_category": true,
	}

	cases := []struct {
		name       string
		formulaStr string
		metrics    []string
		dimensions []string
	}{
		{
			name:       "same context metric formula",
			formulaStr: "=revenue - cogs",
			metrics:    []string{"revenue", "cogs"},
		},
		{
			name:       "dimension conditioned metric formula",
			formulaStr: `=IF(department = "sales", revenue * 0.08, revenue * 0.03)`,
			metrics:    []string{"revenue"},
			dimensions: []string{"department"},
		},
		{
			name:       "dimension driven rate formula",
			formulaStr: "=revenue * commission_rate",
			metrics:    []string{"revenue", "commission_rate"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			refs, err := formula.ExtractRefs(c.formulaStr)
			if err != nil {
				t.Fatalf("extract refs: %v", err)
			}
			gotMetrics, gotDimensions, unknown := classifyRefs(refs, metricNames, dimensionNames)
			assertSameStrings(t, gotMetrics, c.metrics, "metric refs")
			assertSameStrings(t, gotDimensions, c.dimensions, "dimension refs")
			if len(unknown) != 0 {
				t.Fatalf("unexpected unknown refs: %v", unknown)
			}
		})
	}

	ambiguousMetricNames := map[string]bool{"department": true}
	_, _, unknown := classifyRefs([]string{"department"}, ambiguousMetricNames, dimensionNames)
	if len(unknown) != 1 || !strings.Contains(unknown[0], "ambiguous") {
		t.Fatalf("expected ambiguous metric/dimension name to be rejected, got %v", unknown)
	}
}

func TestRoleDeptHeadGridMetricCalculations(t *testing.T) {
	eng := newEngine()
	eng.addMetric("revenue", "")
	eng.addMetric("cogs", "")
	eng.addMetric("gross_profit", "=revenue - cogs", "revenue", "cogs")
	eng.addMetric("commission", `=IF(department = "sales", revenue * 0.08, revenue * 0.03)`, "revenue")
	eng.addMetric("net_after_commission", "=gross_profit - commission", "gross_profit", "commission")

	rows := []struct {
		department string
		month      string
		revenue    float64
		cogs       float64
		commission float64
		net        float64
	}{
		{"sales", "jan", 1_000_000, 600_000, 80_000, 320_000},
		{"finance", "jan", 200_000, 120_000, 6_000, 74_000},
		{"sales", "feb", 1_200_000, 700_000, 96_000, 404_000},
	}

	contexts := make([]dimCtx, 0, len(rows))
	for _, row := range rows {
		ctx := dimCtx{"department": row.department, "month": row.month}
		eng.setInput("revenue", ctx, row.revenue)
		eng.setInput("cogs", ctx, row.cogs)
		contexts = append(contexts, ctx)
	}

	if err := eng.evaluate(contexts); err != nil {
		t.Fatalf("evaluate grid metrics: %v", err)
	}

	for _, row := range rows {
		ctx := dimCtx{"department": row.department, "month": row.month}
		assertFloat(t, eng.cell("gross_profit", ctx), row.revenue-row.cogs, "gross_profit")
		assertFloat(t, eng.cell("commission", ctx), row.commission, "commission")
		assertFloat(t, eng.cell("net_after_commission", ctx), row.net, "net_after_commission")
	}
}

func TestRoleFinanceFormApprovalPosting(t *testing.T) {
	type formRecord struct {
		status string
		data   map[string]formula.Value
	}
	fieldFormulas := map[string]string{
		"net_expense":     "=expense_amount + tax_amount",
		"approval_status": `=IF(net_expense > 10000, "Needs Approval", "Auto Approved")`,
	}

	records := []formRecord{
		{
			status: "draft",
			data: map[string]formula.Value{
				"expense_amount": formula.NumberVal(400),
				"tax_amount":     formula.NumberVal(40),
				"department":     formula.StringVal("sales"),
				"month":          formula.StringVal("jan"),
			},
		},
		{
			status: "approved",
			data: map[string]formula.Value{
				"expense_amount": formula.NumberVal(1000),
				"tax_amount":     formula.NumberVal(100),
				"department":     formula.StringVal("sales"),
				"month":          formula.StringVal("jan"),
			},
		},
		{
			status: "approved",
			data: map[string]formula.Value{
				"expense_amount": formula.NumberVal(500),
				"tax_amount":     formula.NumberVal(50),
				"department":     formula.StringVal("sales"),
				"month":          formula.StringVal("jan"),
			},
		},
	}

	var postedOpex float64
	for _, record := range records {
		evaluated := evaluateRoleFormFields(t, record.data, fieldFormulas)
		if evaluated["approval_status"].String() != "Auto Approved" {
			t.Fatalf("approval_status = %q, want Auto Approved", evaluated["approval_status"].String())
		}
		if record.status == "approved" {
			netExpense, ok := evaluated["net_expense"].Number()
			if !ok {
				t.Fatalf("net_expense is not numeric: %v", evaluated["net_expense"])
			}
			postedOpex += netExpense
		}
	}

	if postedOpex != 1650 {
		t.Fatalf("posted opex = %v, want 1650", postedOpex)
	}

	eng := newEngine()
	eng.addMetric("revenue", "")
	eng.addMetric("opex", "")
	eng.addMetric("gross_profit", "=revenue - opex", "revenue", "opex")

	ctx := dimCtx{"department": "sales", "month": "jan"}
	eng.setInput("revenue", ctx, 5_000)
	eng.setInput("opex", ctx, postedOpex)

	if err := eng.evaluate([]dimCtx{ctx}); err != nil {
		t.Fatalf("evaluate posted metric: %v", err)
	}
	assertFloat(t, eng.cell("gross_profit", ctx), 3350, "gross_profit after approved form postings")
}

func TestRolePlatformAdminFormRecordsIntegrationMapping(t *testing.T) {
	forms := map[string]roleFormDef{
		"expense_requests": {
			name: "expense_requests",
			fields: map[string]string{
				"department":      "text",
				"month":           "text",
				"region":          "text",
				"net_expense":     "number",
				"approval_status": "text",
			},
		},
	}
	inputMetrics := map[string]bool{"opex": true}
	dimensions := map[string]bool{"department": true, "month": true, "region": true}

	cfg := roleIntegrationConfig{
		formName:         "expense_requests",
		sourceField:      "net_expense",
		targetMetric:     "opex",
		aggregation:      "sum",
		postingStatuses:  []string{"approved"},
		dimensionMapping: map[string]string{"department": "department", "month": "month", "region": "region"},
	}
	if errs := validateRoleIntegration(cfg, forms, inputMetrics, dimensions); len(errs) != 0 {
		t.Fatalf("valid integration rejected: %v", errs)
	}

	invalid := cfg
	invalid.targetMetric = "gross_profit"
	errs := validateRoleIntegration(invalid, forms, inputMetrics, dimensions)
	if len(errs) == 0 || !strings.Contains(strings.Join(errs, ","), "target metric") {
		t.Fatalf("expected non-input target metric to be rejected, got %v", errs)
	}

	invalid = cfg
	invalid.dimensionMapping = map[string]string{"department": "department", "month": "month"}
	errs = validateRoleIntegration(invalid, forms, inputMetrics, dimensions)
	if len(errs) == 0 || !strings.Contains(strings.Join(errs, ","), "missing dimension mapping") {
		t.Fatalf("expected missing dimension mapping to be rejected, got %v", errs)
	}
}

func classifyRefs(refs []string, metrics, dimensions map[string]bool) ([]string, []string, []string) {
	var metricRefs, dimensionRefs, unknown []string
	for _, ref := range refs {
		name := strings.ToLower(ref)
		isMetric := metrics[name]
		isDimension := dimensions[name]
		switch {
		case isMetric && isDimension:
			unknown = append(unknown, "ambiguous reference: "+name)
		case isMetric:
			metricRefs = append(metricRefs, name)
		case isDimension:
			dimensionRefs = append(dimensionRefs, name)
		default:
			unknown = append(unknown, name)
		}
	}
	sort.Strings(metricRefs)
	sort.Strings(dimensionRefs)
	sort.Strings(unknown)
	return metricRefs, dimensionRefs, unknown
}

func evaluateRoleFormFields(t *testing.T, data map[string]formula.Value, fieldFormulas map[string]string) map[string]formula.Value {
	t.Helper()
	result := make(map[string]formula.Value, len(data)+len(fieldFormulas))
	for k, v := range data {
		result[k] = v
	}
	fields := make([]string, 0, len(fieldFormulas))
	for field := range fieldFormulas {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	for pass := 0; pass <= len(fields); pass++ {
		progress := false
		for _, field := range fields {
			if _, ok := result[field]; ok {
				continue
			}
			v, err := formula.EvalWithContext(fieldFormulas[field], &formula.EvalContext{Vars: result})
			if err != nil {
				t.Fatalf("%s parse: %v", field, err)
			}
			if v.IsError() {
				continue
			}
			result[field] = v
			progress = true
		}
		if !progress {
			break
		}
	}
	for _, field := range fields {
		v, ok := result[field]
		if !ok {
			t.Fatalf("%s did not evaluate", field)
		}
		if v.IsError() {
			t.Fatalf("%s formula returned %s", field, v.Err())
		}
	}
	return result
}

type roleFormDef struct {
	name   string
	fields map[string]string
}

type roleIntegrationConfig struct {
	formName         string
	sourceField      string
	targetMetric     string
	aggregation      string
	postingStatuses  []string
	dimensionMapping map[string]string
}

func validateRoleIntegration(cfg roleIntegrationConfig, forms map[string]roleFormDef, inputMetrics, dimensions map[string]bool) []string {
	var errs []string
	form, ok := forms[cfg.formName]
	if !ok {
		errs = append(errs, "unknown source form")
		return errs
	}
	if form.fields[cfg.sourceField] != "number" {
		errs = append(errs, "source field must be numeric")
	}
	if !inputMetrics[cfg.targetMetric] {
		errs = append(errs, "target metric must be an input metric")
	}
	if cfg.aggregation != "sum" && cfg.aggregation != "replace" {
		errs = append(errs, "unsupported aggregation")
	}
	if len(cfg.postingStatuses) == 0 {
		errs = append(errs, "posting status rule is required")
	}
	for dim := range dimensions {
		fieldName, ok := cfg.dimensionMapping[dim]
		if !ok {
			errs = append(errs, "missing dimension mapping: "+dim)
			continue
		}
		if _, ok := form.fields[fieldName]; !ok {
			errs = append(errs, "unknown form field for dimension: "+dim)
		}
	}
	return errs
}

func assertSameStrings(t *testing.T, got, want []string, label string) {
	t.Helper()
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
}

func assertFloat(t *testing.T, got, want float64, label string) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
}

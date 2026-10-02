package reporting

// Charts, comparisons and reports, built from grid data as the person reads
// it. Every one runs its grid queries afresh, as the person, when it is
// asked for: nothing is saved in maverickbuilds.app — no dashboard, no
// widget, no stored result — and a chart can show only numbers the engine
// resolved for them. Presentation never computes a metric: it lays out the
// engine's values, and the one calculation here, a comparison's difference,
// is defined below.

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Chart kinds.
const (
	KindBar       = "bar"
	KindLine      = "line"
	KindPie       = "pie"
	KindScatter   = "scatter"
	KindHistogram = "histogram"
)

// Presentation limits.
const (
	maxChartSeries     = 5
	maxChartCategories = 200
	maxReportSections  = 10
)

// ChartSeries is one metric's values along the chart's categories; a nil
// value is not drawn and is not zero.
type ChartSeries struct {
	MetricID string     `json:"metric_id"`
	Label    string     `json:"label"`
	Format   string     `json:"format,omitempty"`
	Decimals int        `json:"decimals,omitempty"`
	Currency string     `json:"currency,omitempty"`
	Values   []*float64 `json:"values"`
	States   []string   `json:"states"`
}

// ChartPoint is a scatter point: one member's two metrics.
type ChartPoint struct {
	Code  string  `json:"code"`
	Label string  `json:"label"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
}

// ChartBin is a histogram bin: values v with Min <= v < Max (the last bin
// includes its Max).
type ChartBin struct {
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Count int     `json:"count"`
}

// ChartSpec is a chart of engine values.
type ChartSpec struct {
	Kind       string        `json:"kind"`
	Title      string        `json:"title"`
	Subtitle   string        `json:"subtitle,omitempty"`
	Categories []Ref         `json:"categories,omitempty"`
	Series     []ChartSeries `json:"series,omitempty"`
	XAxis      *ChartSeries  `json:"x_axis,omitempty"`
	YAxis      *ChartSeries  `json:"y_axis,omitempty"`
	Points     []ChartPoint  `json:"points,omitempty"`
	Bins       []ChartBin    `json:"bins,omitempty"`
	// Population is the number of values a histogram or scatter is drawn
	// from; values with no number are left out and counted in Notes.
	Population int      `json:"population,omitempty"`
	Notes      []string `json:"notes,omitempty"`
}

// ChartOptions select how a grid result is drawn.
type ChartOptions struct {
	Kind  string
	Title string
	// XMetricID and YMetricID pick a scatter's axes (default: the first
	// two metrics).
	XMetricID, YMetricID string
	Bins                 int
}

func filterText(fs []Filter) string {
	var parts []string
	for _, f := range fs {
		parts = append(parts, f.DimensionName+" = "+f.MemberLabel)
	}
	return strings.Join(parts, ", ")
}

// BuildChart lays out a grid result as a chart. It refuses a kind the data
// cannot honestly be drawn as, rather than drawing something misleading.
func BuildChart(res *GridResult, opt ChartOptions) (*ChartSpec, error) {
	kind := opt.Kind
	if kind == "" {
		kind = KindBar
	}
	spec := &ChartSpec{Kind: kind, Title: opt.Title, Subtitle: filterText(res.Filters)}
	metricCols := res.Columns
	if res.GroupBy != nil {
		metricCols = res.Columns[2:]
	}
	if spec.Title == "" {
		var names []string
		for _, m := range res.Metrics {
			names = append(names, m.Name)
		}
		spec.Title = strings.Join(names, ", ")
		if res.GroupBy != nil {
			spec.Title += " by " + res.GroupBy.Name
		}
	}
	series := func() []ChartSeries {
		var out []ChartSeries
		for _, col := range metricCols {
			s := ChartSeries{MetricID: col.ID, Label: col.Label, Format: col.Format, Decimals: col.Decimals, Currency: col.Currency}
			for _, row := range res.Rows {
				v, _ := row[col.ID].(Value)
				s.Values = append(s.Values, v.Value)
				s.States = append(s.States, v.State)
			}
			out = append(out, s)
		}
		return out
	}

	switch kind {
	case KindBar, KindLine, KindPie:
		if res.GroupBy == nil {
			if kind != KindBar {
				return nil, invalid("a %s chart needs group_by: one value per member of a dimension", kind)
			}
			// No breakdown: one bar per metric, each its engine total.
			s := ChartSeries{Label: "Value"}
			for _, row := range res.Rows {
				spec.Categories = append(spec.Categories, Ref{ID: fmt.Sprint(row["metric_id"]), Name: fmt.Sprint(row["metric"])})
				v, _ := row["value"].(Value)
				s.Values = append(s.Values, v.Value)
				s.States = append(s.States, v.State)
			}
			if len(res.Metrics) > 0 {
				s.Format, s.Currency = fmt.Sprint(res.Rows[0]["format"]), fmt.Sprint(res.Rows[0]["currency"])
			}
			if mixedUnits(metricColumnsOf(res)) {
				return nil, invalid("these metrics have different units; chart them separately or as a table")
			}
			spec.Series = []ChartSeries{s}
			break
		}
		if len(metricCols) > maxChartSeries {
			return nil, invalid("a chart draws at most %d metrics; this query has %d", maxChartSeries, len(metricCols))
		}
		if mixedUnits(metricCols) {
			return nil, invalid("these metrics have different units or currencies; one axis cannot carry them — chart them separately")
		}
		for _, row := range res.Rows {
			spec.Categories = append(spec.Categories, Ref{ID: fmt.Sprint(row["member_code"]), Name: fmt.Sprint(row["member"])})
		}
		spec.Series = series()
		if kind == KindPie {
			if len(spec.Series) != 1 {
				return nil, invalid("a pie chart shows one metric")
			}
			for i, v := range spec.Series[0].Values {
				if v == nil || *v < 0 {
					return nil, invalid("a pie chart needs a non-negative value for every slice; %s has none — use a bar chart", spec.Categories[i].Name)
				}
			}
			if !res.LeavesOnly {
				return nil, invalid("a pie chart's slices must not overlap: set leaves_only so parents are not drawn beside their children")
			}
		}
		if len(spec.Categories) > maxChartCategories {
			omitted := len(spec.Categories) - maxChartCategories
			spec.Categories = spec.Categories[:maxChartCategories]
			for i := range spec.Series {
				spec.Series[i].Values = spec.Series[i].Values[:maxChartCategories]
				spec.Series[i].States = spec.Series[i].States[:maxChartCategories]
			}
			spec.Notes = append(spec.Notes, fmt.Sprintf("the first %d members are drawn; %d more are in the data but not in the chart", maxChartCategories, omitted))
		}
		absent := 0
		for _, s := range spec.Series {
			for _, st := range s.States {
				if st != "ok" {
					absent++
				}
			}
		}
		if absent == 1 {
			spec.Notes = append(spec.Notes, "1 value has no number for you and is not drawn (it is not zero)")
		} else if absent > 1 {
			spec.Notes = append(spec.Notes, fmt.Sprintf("%d values have no number for you and are not drawn (they are not zero)", absent))
		}
	case KindScatter:
		if res.GroupBy == nil || len(metricCols) < 2 {
			return nil, invalid("a scatter chart needs group_by and two metrics at the same members")
		}
		all := series()
		pick := func(id string, def int) (*ChartSeries, error) {
			if id == "" {
				return &all[def], nil
			}
			for i := range all {
				if all[i].MetricID == id {
					return &all[i], nil
				}
			}
			return nil, invalid("metric %s is not in the query", id)
		}
		x, err := pick(opt.XMetricID, 0)
		if err != nil {
			return nil, err
		}
		y, err := pick(opt.YMetricID, 1)
		if err != nil {
			return nil, err
		}
		spec.XAxis, spec.YAxis = x, y
		left := 0
		for i, row := range res.Rows {
			if x.Values[i] == nil || y.Values[i] == nil {
				left++
				continue
			}
			spec.Points = append(spec.Points, ChartPoint{Code: fmt.Sprint(row["member_code"]), Label: fmt.Sprint(row["member"]), X: *x.Values[i], Y: *y.Values[i]})
		}
		spec.Population = len(spec.Points)
		if left > 0 {
			spec.Notes = append(spec.Notes, fmt.Sprintf("%d members lack one of the two values and are not drawn", left))
		}
	case KindHistogram:
		if res.GroupBy == nil || len(metricCols) != 1 {
			return nil, invalid("a histogram needs group_by and exactly one metric: the distribution of its values across members")
		}
		if !res.LeavesOnly {
			return nil, invalid("a histogram counts members: set leaves_only so parents are not counted beside their children")
		}
		s := series()[0]
		spec.Series = []ChartSeries{s}
		var vals []float64
		for _, v := range s.Values {
			if v != nil {
				vals = append(vals, *v)
			}
		}
		spec.Population = len(vals)
		if missing := len(s.Values) - len(vals); missing > 0 {
			spec.Notes = append(spec.Notes, fmt.Sprintf("%d members have no value for you and are not counted", missing))
		}
		spec.Bins = bins(vals, opt.Bins)
		spec.Notes = append(spec.Notes, fmt.Sprintf("distribution of %d %s values across %s members", len(vals), s.Label, res.GroupBy.Name))
	default:
		return nil, invalid("chart kind must be bar, line, pie, scatter or histogram")
	}
	return spec, nil
}

func metricColumnsOf(res *GridResult) []Column {
	var out []Column
	for _, row := range res.Rows {
		out = append(out, Column{Format: fmt.Sprint(row["format"]), Currency: fmt.Sprint(row["currency"])})
	}
	return out
}

// mixedUnits reports whether columns differ in format or currency.
func mixedUnits(cols []Column) bool {
	for _, c := range cols[min(1, len(cols)):] {
		if c.Format != cols[0].Format || (c.Format == "currency" && c.Currency != cols[0].Currency) {
			return true
		}
	}
	return false
}

func bins(vals []float64, n int) []ChartBin {
	if len(vals) == 0 {
		return []ChartBin{}
	}
	if n < 3 || n > 30 {
		n = int(math.Min(10, math.Max(3, math.Ceil(math.Sqrt(float64(len(vals)))))))
	}
	lo, hi := vals[0], vals[0]
	for _, v := range vals {
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	if hi == lo {
		return []ChartBin{{Min: lo, Max: hi, Count: len(vals)}}
	}
	width := (hi - lo) / float64(n)
	out := make([]ChartBin, n)
	for i := range out {
		out[i] = ChartBin{Min: lo + float64(i)*width, Max: lo + float64(i+1)*width}
	}
	out[n-1].Max = hi
	for _, v := range vals {
		i := int((v - lo) / width)
		if i >= n {
			i = n - 1
		}
		out[i].Count++
	}
	return out
}

// ── comparison ──────────────────────────────────────────────────────────────

// ComparisonRow matches one member (or the whole slice) across the two
// sides. Difference is current − baseline; Percent is that difference over
// |baseline| × 100. Either is null, with Note saying why, when a side has
// no number or the baseline is zero — an unmatched or absent value is never
// taken as zero.
type ComparisonRow struct {
	Code       string   `json:"code"`
	Label      string   `json:"label"`
	Current    Value    `json:"current"`
	Baseline   Value    `json:"baseline"`
	Difference *float64 `json:"difference"`
	Percent    *float64 `json:"percent_change"`
	Note       string   `json:"note,omitempty"`
}

// Comparison is compare_grid's result.
type Comparison struct {
	Context  Pinned          `json:"context"`
	Current  ComparisonSide  `json:"current"`
	Baseline ComparisonSide  `json:"baseline"`
	GroupBy  *Ref            `json:"group_by,omitempty"`
	Rows     []ComparisonRow `json:"rows"`
	Totals   *ComparisonRow  `json:"totals,omitempty"`
	ReadAt   string          `json:"read_at"`
	Notes    []string        `json:"notes"`
}

// ComparisonSide names one side's source, metric and filters.
type ComparisonSide struct {
	Source  SourceRef `json:"source"`
	Metric  Ref       `json:"metric"`
	Filters []Filter  `json:"filters"`
}

// Compare reads current and baseline — each one metric of a grid, with
// the same group_by — and matches their rows by member.
func (s *Session) Compare(ctx context.Context, p Pinned, current, baseline GridQuery) (*Comparison, error) {
	if len(current.MetricIDs) != 1 || len(baseline.MetricIDs) != 1 {
		return nil, invalid("each side of a comparison is exactly one metric")
	}
	if current.GroupBy != baseline.GroupBy {
		return nil, invalid("both sides must break down by the same dimension (group_by), or neither")
	}
	cur, err := s.QueryGrid(ctx, p, current)
	if err != nil {
		return nil, err
	}
	base, err := s.QueryGrid(ctx, p, baseline)
	if err != nil {
		return nil, err
	}
	cc, bc := metricColumn(cur), metricColumn(base)
	if cc.Format != bc.Format || cc.Currency != bc.Currency {
		return nil, invalid("the two metrics have different units (%s %s vs %s %s); they are not comparable", cc.Format, cc.Currency, bc.Format, bc.Currency)
	}
	out := &Comparison{
		Context:  p,
		Current:  ComparisonSide{Source: cur.Source, Metric: cur.Metrics[0], Filters: cur.Filters},
		Baseline: ComparisonSide{Source: base.Source, Metric: base.Metrics[0], Filters: base.Filters},
		GroupBy:  cur.GroupBy, Rows: []ComparisonRow{}, ReadAt: s.s.now().UTC().Format(time.RFC3339),
		Notes: []string{
			"difference = current − baseline; percent_change = difference ÷ |baseline| × 100 (a change in percent, not percentage points)",
			"members are matched by code; a member on one side only is listed with the other side absent, never as zero",
		},
	}
	if cur.GroupBy == nil {
		row := compareValues2("", "Total", valueAt(cur.Rows[0], "value"), valueAt(base.Rows[0], "value"))
		out.Rows = append(out.Rows, row)
		return out, nil
	}
	baseByCode := map[string]Row{}
	for _, r := range base.Rows {
		baseByCode[fmt.Sprint(r["member_code"])] = r
	}
	seen := map[string]bool{}
	for _, r := range cur.Rows {
		code := fmt.Sprint(r["member_code"])
		seen[code] = true
		bv := Value{State: "absent"}
		if br, ok := baseByCode[code]; ok {
			bv = valueAt(br, bc.ID)
		} else {
			bv.State = "unmatched"
		}
		out.Rows = append(out.Rows, compareValues2(code, fmt.Sprint(r["member"]), valueAt(r, cc.ID), bv))
	}
	for _, r := range base.Rows {
		code := fmt.Sprint(r["member_code"])
		if !seen[code] {
			out.Rows = append(out.Rows, compareValues2(code, fmt.Sprint(r["member"]), Value{State: "unmatched"}, valueAt(r, bc.ID)))
		}
	}
	return out, nil
}

// valueAt is a row's value in column id; absent when it has none.
func valueAt(r Row, id string) Value {
	if v, ok := r[id].(Value); ok {
		return v
	}
	return Value{State: "absent"}
}

func metricColumn(res *GridResult) Column {
	if res.GroupBy != nil {
		return res.Columns[2]
	}
	return Column{ID: "value", Format: fmt.Sprint(res.Rows[0]["format"]), Currency: fmt.Sprint(res.Rows[0]["currency"])}
}

func compareValues2(code, label string, cur, base Value) ComparisonRow {
	row := ComparisonRow{Code: code, Label: label, Current: cur, Baseline: base}
	switch {
	case cur.Value == nil || base.Value == nil:
		row.Note = "no difference: a side has no value (" + cur.State + " / " + base.State + ")"
	default:
		d := *cur.Value - *base.Value
		row.Difference = &d
		if *base.Value == 0 {
			row.Note = "no percent change: the baseline is zero"
		} else {
			pct := d / math.Abs(*base.Value) * 100
			row.Percent = &pct
		}
	}
	return row
}

// ── reports ─────────────────────────────────────────────────────────────────

// ReportSection asks for one section: a grid query presented as a table,
// KPIs, a chart, or a comparison against Baseline.
type ReportSection struct {
	Heading      string
	Commentary   string
	Presentation string
	Query        GridQuery
	Baseline     *GridQuery
	Chart        ChartOptions
}

// RenderedSection is one section with its data.
type RenderedSection struct {
	Heading string `json:"heading"`
	// Commentary is the assistant's, not the engine's: it is shown as such.
	Commentary   string      `json:"commentary,omitempty"`
	Presentation string      `json:"presentation"`
	Status       string      `json:"status"`
	Message      string      `json:"message,omitempty"`
	Data         *GridResult `json:"data,omitempty"`
	Chart        *ChartSpec  `json:"chart,omitempty"`
	Comparison   *Comparison `json:"comparison,omitempty"`
}

// Report is render_report's result.
type Report struct {
	Title    string            `json:"title"`
	Context  Pinned            `json:"context"`
	ReadAt   string            `json:"read_at"`
	Sections []RenderedSection `json:"sections"`
	Complete bool              `json:"complete"`
	Notes    []string          `json:"notes"`
}

// BuildReport reads every section's grid data as the person and lays it
// out. A section that cannot be read is reported as such, and the report is
// then not complete.
func (s *Session) BuildReport(ctx context.Context, p Pinned, title string, sections []ReportSection) (*Report, error) {
	if len(sections) == 0 || len(sections) > maxReportSections {
		return nil, invalid("a report has 1 to %d sections", maxReportSections)
	}
	rep := &Report{Title: title, Context: p, ReadAt: s.s.now().UTC().Format(time.RFC3339), Complete: true,
		Notes: []string{
			"every number is read from the model's grids as you, now; commentary is the assistant's, not the engine's",
			"nothing is saved in maverickbuilds.app",
		}}
	for _, sec := range sections {
		out := RenderedSection{Heading: sec.Heading, Commentary: sec.Commentary, Presentation: sec.Presentation, Status: "ok"}
		fail := func(err error) {
			out.Status, out.Message = "unavailable", "this section could not be read"
			if e, ok := err.(*Error); ok {
				out.Message = e.Message
			}
			rep.Complete = false
		}
		switch sec.Presentation {
		case "comparison":
			if sec.Baseline == nil {
				fail(invalid("a comparison section needs a baseline query"))
				break
			}
			cmp, err := s.Compare(ctx, p, sec.Query, *sec.Baseline)
			if err != nil {
				fail(err)
				break
			}
			out.Comparison = cmp
		case "table", "kpi", KindBar, KindLine, KindPie, KindScatter, KindHistogram:
			res, err := s.QueryGrid(ctx, p, sec.Query)
			if err != nil {
				fail(err)
				break
			}
			out.Data = res
			if sec.Presentation != "table" && sec.Presentation != "kpi" {
				opt := sec.Chart
				opt.Kind = sec.Presentation
				if opt.Title == "" {
					opt.Title = sec.Heading
				}
				spec, err := BuildChart(res, opt)
				if err != nil {
					fail(err)
					break
				}
				out.Chart = spec
			}
		default:
			fail(invalid("presentation must be table, kpi, bar, line, pie, scatter, histogram or comparison"))
		}
		rep.Sections = append(rep.Sections, out)
	}
	return rep, nil
}

// TopAndBottom names the largest and smallest drawn values of a chart's
// first series, for a one-line description.
func (c *ChartSpec) TopAndBottom() (top, bottom string) {
	if len(c.Series) == 0 || len(c.Categories) == 0 {
		return "", ""
	}
	type kv struct {
		name string
		v    float64
	}
	var vals []kv
	for i, v := range c.Series[0].Values {
		if v != nil && i < len(c.Categories) {
			vals = append(vals, kv{c.Categories[i].Name, *v})
		}
	}
	if len(vals) == 0 {
		return "", ""
	}
	sort.SliceStable(vals, func(i, j int) bool { return vals[i].v > vals[j].v })
	return fmt.Sprintf("%s %s", vals[0].name, formatNumber(vals[0].v, c.Series[0])),
		fmt.Sprintf("%s %s", vals[len(vals)-1].name, formatNumber(vals[len(vals)-1].v, c.Series[0]))
}

// formatNumber renders v in a series' format.
func formatNumber(v float64, s ChartSeries) string {
	switch s.Format {
	case "percentage":
		return fmt.Sprintf("%.1f%%", v*100)
	case "currency":
		return strings.TrimSpace(s.Currency) + groupThousands(v, s.Decimals)
	}
	return groupThousands(v, s.Decimals)
}

func groupThousands(v float64, decimals int) string {
	neg := v < 0
	str := fmt.Sprintf("%.*f", decimals, math.Abs(v))
	intPart, frac, _ := strings.Cut(str, ".")
	var b strings.Builder
	for i, ch := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(ch)
	}
	out := b.String()
	if frac != "" {
		out += "." + frac
	}
	if neg {
		out = "-" + out
	}
	return out
}

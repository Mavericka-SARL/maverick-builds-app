package query

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/readset"
	"github.com/mavericks-engine/mavericks/internal/rollup"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
)

// ── Types ────────────────────────────────────────────────────────────────────

type ChartType string

const (
	ChartBar       ChartType = "bar"
	ChartLine      ChartType = "line"
	ChartPie       ChartType = "pie"
	ChartScatter   ChartType = "scatter"
	ChartHistogram ChartType = "histogram"
)

// ChartConfig holds the developer-saved chart configuration from widget_props.chart.
type ChartConfig struct {
	ChartType       ChartType         `json:"chart_type"`
	DimensionID     string            `json:"dimension_id"`
	MetricIDs       []string          `json:"metric_ids"`
	XMetricID       string            `json:"x_metric_id,omitempty"`
	YMetricID       string            `json:"y_metric_id,omitempty"`
	ContextDefaults map[string]string `json:"context_defaults"`
	BinCount        int               `json:"bin_count,omitempty"`
	ShowLegend      *bool             `json:"show_legend,omitempty"`
	ShowValues      *bool             `json:"show_values,omitempty"`
	ValueFormat     string            `json:"value_format,omitempty"`
	RefreshSeconds  int               `json:"refresh_seconds,omitempty"`
	// HideRollupMembers drops the plotted dimension's non-leaf members — the
	// "All Regions" style totals. A total is the sum of everything beside it,
	// so plotting it next to its own children gives one bar that dwarfs the
	// rest and flattens the comparison the chart exists to show.
	//
	// Only the plotted axis: the context selectors keep their rollups, where
	// picking "All Regions" is a meaningful choice rather than a distortion.
	HideRollupMembers bool `json:"hide_rollup_members,omitempty"`
}

// ChartRequest is the runtime request body — only context is overridable.
type ChartRequest struct {
	Context map[string]string `json:"context"`
}

// GridChartCategory is one plotted-dimension member.
type GridChartCategory struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

// GridChartSeries is one metric series matching the categories slice.
type GridChartSeries struct {
	MetricID       string     `json:"metric_id"`
	Label          string     `json:"label"`
	Values         []*float64 `json:"values"`
	Format         string     `json:"format"`
	FormatDecimals int        `json:"format_decimals"`
	FormatCurrency string     `json:"format_currency"`
}

// ChartContextMember is one member of a context-selector dimension, in the
// lean shape the frontend needs to pick a hierarchy-aware default (see
// dashboardLayout.ts's defaultLeafCode) — no id or other hierarchy
// internals, since rollup now happens server-side.
type ChartContextMember struct {
	Code       string `json:"code"`
	Label      string `json:"label"`
	ParentCode string `json:"parent_code,omitempty"`
}

// ChartContextDim is one context-selector dimension for a chart — every
// hidden-filtered grid dimension except the plotted one.
type ChartContextDim struct {
	ID      string               `json:"id"`
	Name    string               `json:"name"`
	Members []ChartContextMember `json:"members"`
}

// CategoryChartData is returned for bar, line, and pie charts.
type CategoryChartData struct {
	ChartType   string              `json:"chart_type"`
	AsOf        string              `json:"as_of"`
	Categories  []GridChartCategory `json:"categories"`
	Series      []GridChartSeries   `json:"series"`
	ContextDims []ChartContextDim   `json:"context_dims"`
	// Context is the context the data was actually resolved for. It differs
	// from the request when a member the caller asked for is hidden from
	// them — the resolver then substitutes that dimension's default visible
	// member instead of failing (see Resolve), and the frontend adopts the
	// value so the selectors show what the chart really shows.
	Context map[string]string `json:"context"`
}

// ScatterPoint is one point on a scatter chart.
type ScatterPoint struct {
	Key   string  `json:"key"`
	Label string  `json:"label"`
	X     float64 `json:"x"`
	Y     float64 `json:"y"`
}

// ScatterChartData is returned for scatter charts.
type ScatterChartData struct {
	ChartType   string            `json:"chart_type"`
	AsOf        string            `json:"as_of"`
	XMetric     metricLabel       `json:"x_metric"`
	YMetric     metricLabel       `json:"y_metric"`
	Points      []ScatterPoint    `json:"points"`
	ContextDims []ChartContextDim `json:"context_dims"`
	Context     map[string]string `json:"context"`
}

// HistogramBin is one bin on a histogram.
type HistogramBin struct {
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Count int     `json:"count"`
}

// HistogramChartData is returned for histogram charts.
type HistogramChartData struct {
	ChartType        string            `json:"chart_type"`
	AsOf             string            `json:"as_of"`
	Metric           metricLabel       `json:"metric"`
	ObservationCount int               `json:"observation_count"`
	Bins             []HistogramBin    `json:"bins"`
	ContextDims      []ChartContextDim `json:"context_dims"`
	Context          map[string]string `json:"context"`
}

type metricLabel struct {
	ID             string `json:"id"`
	Label          string `json:"label"`
	Format         string `json:"format"`
	FormatDecimals int    `json:"format_decimals"`
	FormatCurrency string `json:"format_currency"`
}

// ── Resolver ─────────────────────────────────────────────────────────────────

// ChartResolver evaluates chart data from grid facts and calculated values.
type ChartResolver struct {
	pool *pgxpool.Pool
}

func NewChartResolver(pool *pgxpool.Pool) *ChartResolver {
	return &ChartResolver{pool: pool}
}

// dimMember is one dimension member loaded from the model.
type dimMember struct {
	ID         string
	Code       string
	Label      string
	SortOrder  int
	ParentCode string
}

// metricDef is a metric loaded for the chart.
type metricDef struct {
	ID             string
	Name           string
	Label          string
	IsInput        bool
	Formula        string
	AggRule        string
	Format         string
	FormatDecimals int
	FormatCurrency string
	TimeSummary    string // reduction across a time dimension (sum | average | min | max | first | last | none)
	// DimensionIDs are this metric's own native dimensions (via
	// grid_metric -> grid_dimension on whatever grid it actually lives
	// on), used to resolve its value through rollup.Resolve.
	DimensionIDs []string
}

// ── Resolve ──────────────────────────────────────────────────────────────────

// Resolve evaluates chart data for the given widget configuration.
// modelID, revisionID, gridDefID must already be resolved by the caller.
// userID is used to filter hidden members/metrics from access rules.
func (r *ChartResolver) Resolve(
	ctx context.Context,
	cfg *ChartConfig,
	runtimeContext map[string]string,
	modelID, revisionID, gridDefID, userID string,
) (interface{}, error) {
	// A grid with no grid_metric rows of its own can mirror another grid's
	// metrics (rollup_source_grid_id), rendering them via rollup.Resolve
	// against its own dims instead of direct fact lookups. Only the
	// metrics *list* comes from the source grid; dims/cells/access rules
	// below all stay scoped to the grid actually requested. Mirrors
	// grid()'s identical handler.go pattern.
	metricsGridID := gridDefID
	var rollupSourceGridID *string
	if err := r.pool.QueryRow(ctx,
		`SELECT rollup_source_grid_id::text FROM model.grid_def WHERE id=$1::uuid`, gridDefID,
	).Scan(&rollupSourceGridID); err == nil && rollupSourceGridID != nil {
		metricsGridID = *rollupSourceGridID
	}

	// Load grid metrics (revision-mapped)
	gridMetrics, err := r.loadGridMetrics(ctx, metricsGridID, revisionID, modelID)
	if err != nil {
		return nil, fmt.Errorf("load grid metrics: %w", err)
	}

	// Load access rules for this user
	dimRules, metricRules, err := r.loadAccessRules(ctx, userID, revisionID)
	if err != nil {
		return nil, fmt.Errorf("load access rules: %w", err)
	}

	// Cascade hidden-member rules down the dimension hierarchy — a rule set
	// directly on an ancestor dimension member (e.g. cost_centers) must also
	// hide every descendant member (e.g. employees) that rolls up to it,
	// even when the two dimensions aren't on the same grid this chart is
	// bound to (see loadMemberEdges). Mirrors the same cascade grid() uses.
	if memberEdges, edgeErr := r.loadMemberEdges(ctx, modelID, revisionID); edgeErr != nil {
		return nil, fmt.Errorf("load member edges: %w", edgeErr)
	} else {
		for id := range writeguard.ExpandHidden(memberEdges, dimRules) {
			dimRules[id] = "hidden"
		}
	}

	// Apply metric access rules
	allowedMetrics := make(map[string]*metricDef)
	for _, m := range gridMetrics {
		if metricRules[m.ID] == "hidden" {
			continue
		}
		allowedMetrics[m.ID] = m
	}

	// Load grid dimensions with their members
	gridDims, dimNames, err := r.loadGridDimensions(ctx, gridDefID)
	if err != nil {
		return nil, fmt.Errorf("load grid dimensions: %w", err)
	}

	// Apply dimension member access rules and filter to display level.
	// hiddenCodes remembers what was removed, so a context value naming a
	// hidden member can be told apart from one naming nothing at all.
	hiddenCodes := map[string]map[string]bool{}
	for dimID, members := range gridDims {
		filtered := make([]dimMember, 0, len(members))
		for _, m := range members {
			if dimRules[m.ID] == "hidden" {
				if hiddenCodes[dimID] == nil {
					hiddenCodes[dimID] = map[string]bool{}
				}
				hiddenCodes[dimID][m.Code] = true
				continue
			}
			filtered = append(filtered, m)
		}
		gridDims[dimID] = filtered
	}

	// Validate plotted dimension belongs to grid
	plottedMembers, ok := gridDims[cfg.DimensionID]
	if !ok {
		return nil, fmt.Errorf("plotted dimension not found in grid")
	}
	if cfg.HideRollupMembers {
		plottedMembers = leafMembersOnly(plottedMembers)
	}
	if len(plottedMembers) == 0 {
		return nil, fmt.Errorf("no visible members for plotted dimension")
	}

	// Build effective context (runtime overrides saved defaults)
	effectiveCtx := make(map[string]string)
	for k, v := range cfg.ContextDefaults {
		effectiveCtx[k] = v
	}
	for k, v := range runtimeContext {
		effectiveCtx[k] = v
	}

	// Validate context dimensions
	substituted := map[string]string{}
	for dimID, memberCode := range effectiveCtx {
		if dimID == cfg.DimensionID {
			return nil, fmt.Errorf("context must not include the plotted dimension")
		}
		members, ok := gridDims[dimID]
		if !ok {
			return nil, fmt.Errorf("context dimension %s not in grid", dimID)
		}
		found := false
		for _, m := range members {
			if m.Code == memberCode {
				found = true
				break
			}
		}
		if found {
			continue
		}
		if !hiddenCodes[dimID][memberCode] {
			return nil, fmt.Errorf("context member %q not found for dimension %s", memberCode, dimID)
		}
		// The member exists but is hidden from this viewer. A chart's saved
		// context_defaults are the developer's choice for everyone, and a
		// viewer with a hidden-member rule must still get the chart — so
		// resolve for the dimension's default visible member instead (the
		// same rule the frontend uses to seed a selector: first leaf, else
		// first member). Failing here 403'd the whole widget in a retry loop
		// and, through the shared selectors, poisoned every other widget on
		// the dashboard with the hidden value (reported live 2026-09-13:
		// business user with "Laptop" hidden, line chart pinned to Laptop).
		// Nothing hidden is revealed: the substituted member is visible, and
		// the response says which one was used (Context).
		if sub := defaultVisibleCode(members); sub != "" {
			substituted[dimID] = sub
		} else {
			substituted[dimID] = ""
		}
	}
	for dimID, code := range substituted {
		if code == "" {
			delete(effectiveCtx, dimID)
		} else {
			effectiveCtx[dimID] = code
		}
	}

	// Revision-wide dimension universe (regardless of grid), hidden-filtered,
	// for rollup.Resolve's same-dimension and cross-dimension resolution —
	// mirrors handler.go's all_dimensions query.
	allDims, err := r.loadAllDimensions(ctx, modelID, revisionID, dimRules)
	if err != nil {
		return nil, fmt.Errorf("load all dimensions: %w", err)
	}
	cc, err := r.loadChartCalc(ctx, modelID, revisionID, dimRules)
	if err != nil {
		return nil, fmt.Errorf("load calculated metrics: %w", err)
	}

	contextDims := buildContextDims(gridDims, dimNames, cfg.DimensionID)
	asOf := time.Now().UTC().Format(time.RFC3339)

	switch cfg.ChartType {
	case ChartBar, ChartLine, ChartPie:
		return r.resolveCategoryChart(ctx, cfg, plottedMembers, allowedMetrics, effectiveCtx, modelID, revisionID, allDims, cc, contextDims, asOf)
	case ChartScatter:
		return r.resolveScatterChart(ctx, cfg, plottedMembers, allowedMetrics, effectiveCtx, modelID, revisionID, allDims, cc, contextDims, asOf)
	case ChartHistogram:
		return r.resolveHistogramChart(ctx, cfg, plottedMembers, allowedMetrics, effectiveCtx, modelID, revisionID, allDims, cc, contextDims, asOf)
	default:
		return nil, fmt.Errorf("unsupported chart type: %s", cfg.ChartType)
	}
}

// leafMembersOnly drops members that are a parent of another member in the
// same set — the rollup totals.
//
// Parenthood is judged against the members actually being plotted, not the
// dimension as a whole: a member whose only children are hidden from this
// caller has nothing left to total, so it is a leaf here and dropping it would
// remove real data rather than a duplicate of it.
func leafMembersOnly(members []dimMember) []dimMember {
	hasChild := make(map[string]bool, len(members))
	for _, m := range members {
		if m.ParentCode != "" {
			hasChild[m.ParentCode] = true
		}
	}
	out := make([]dimMember, 0, len(members))
	for _, m := range members {
		if !hasChild[m.Code] {
			out = append(out, m)
		}
	}
	return out
}

// defaultVisibleCode mirrors the frontend's defaultLeafCode: the first
// member that is nobody's parent, else the first member — "" when the
// viewer can see no member of the dimension at all.
func defaultVisibleCode(members []dimMember) string {
	if len(members) == 0 {
		return ""
	}
	hasChild := make(map[string]bool, len(members))
	for _, m := range members {
		if m.ParentCode != "" {
			hasChild[m.ParentCode] = true
		}
	}
	for _, m := range members {
		if !hasChild[m.Code] {
			return m.Code
		}
	}
	return members[0].Code
}

// buildContextDims returns every context-selector dimension for this chart —
// every hidden-filtered grid dimension except the plotted one — in the lean
// shape the frontend needs (see ChartContextDim) without a second /api/grid
// call.
func buildContextDims(gridDims map[string][]dimMember, dimNames map[string]string, plottedDimID string) []ChartContextDim {
	out := make([]ChartContextDim, 0, len(gridDims))
	for dimID, members := range gridDims {
		if dimID == plottedDimID {
			continue
		}
		cms := make([]ChartContextMember, 0, len(members))
		for _, m := range members {
			cms = append(cms, ChartContextMember{Code: m.Code, Label: m.Label, ParentCode: m.ParentCode})
		}
		out = append(out, ChartContextDim{ID: dimID, Name: dimNames[dimID], Members: cms})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ── Category charts (bar, line, pie) ─────────────────────────────────────────

func (r *ChartResolver) resolveCategoryChart(
	ctx context.Context,
	cfg *ChartConfig,
	plottedMembers []dimMember,
	allowedMetrics map[string]*metricDef,
	effectiveCtx map[string]string,
	modelID, revisionID string,
	allDims map[string]*rollup.Dimension,
	cc *chartCalc,
	contextDims []ChartContextDim,
	asOf string,
) (*CategoryChartData, error) {
	// Limit to 500 members
	if len(plottedMembers) > 500 {
		return nil, fmt.Errorf("plotted dimension exceeds 500 members; use a more specific display level")
	}

	// Build series for each requested metric
	var series []GridChartSeries
	for _, metricID := range cfg.MetricIDs {
		m, ok := allowedMetrics[metricID]
		if !ok {
			return nil, fmt.Errorf("metric %s not accessible or not in grid", metricID)
		}
		values, err := r.resolveMetricPerMember(ctx, m, plottedMembers, effectiveCtx, cfg.DimensionID, modelID, revisionID, allDims, cc)
		if err != nil {
			return nil, fmt.Errorf("resolve metric %s: %w", m.Name, err)
		}
		series = append(series, GridChartSeries{
			MetricID:       m.ID,
			Label:          toLabel(m.Name),
			Values:         values,
			Format:         m.Format,
			FormatDecimals: m.FormatDecimals,
			FormatCurrency: m.FormatCurrency,
		})
	}

	categories := make([]GridChartCategory, 0, len(plottedMembers))
	for _, m := range plottedMembers {
		categories = append(categories, GridChartCategory{Key: m.Code, Label: m.Label})
	}

	return &CategoryChartData{
		ChartType:   string(cfg.ChartType),
		AsOf:        asOf,
		Categories:  categories,
		Series:      series,
		ContextDims: contextDims,
		Context:     effectiveCtx,
	}, nil
}

// ── Scatter chart ─────────────────────────────────────────────────────────────

func (r *ChartResolver) resolveScatterChart(
	ctx context.Context,
	cfg *ChartConfig,
	plottedMembers []dimMember,
	allowedMetrics map[string]*metricDef,
	effectiveCtx map[string]string,
	modelID, revisionID string,
	allDims map[string]*rollup.Dimension,
	cc *chartCalc,
	contextDims []ChartContextDim,
	asOf string,
) (*ScatterChartData, error) {
	if len(plottedMembers) > 1000 {
		return nil, fmt.Errorf("plotted dimension exceeds 1000 members")
	}
	xM, ok := allowedMetrics[cfg.XMetricID]
	if !ok {
		return nil, fmt.Errorf("x_metric_id %s not accessible", cfg.XMetricID)
	}
	yM, ok := allowedMetrics[cfg.YMetricID]
	if !ok {
		return nil, fmt.Errorf("y_metric_id %s not accessible", cfg.YMetricID)
	}
	if cfg.XMetricID == cfg.YMetricID {
		return nil, fmt.Errorf("scatter X and Y metrics must be different")
	}

	xVals, err := r.resolveMetricPerMember(ctx, xM, plottedMembers, effectiveCtx, cfg.DimensionID, modelID, revisionID, allDims, cc)
	if err != nil {
		return nil, err
	}
	yVals, err := r.resolveMetricPerMember(ctx, yM, plottedMembers, effectiveCtx, cfg.DimensionID, modelID, revisionID, allDims, cc)
	if err != nil {
		return nil, err
	}

	var points []ScatterPoint
	for i, m := range plottedMembers {
		if xVals[i] == nil || yVals[i] == nil {
			continue
		}
		points = append(points, ScatterPoint{
			Key: m.Code, Label: m.Label, X: *xVals[i], Y: *yVals[i],
		})
	}

	return &ScatterChartData{
		ChartType:   string(cfg.ChartType),
		AsOf:        asOf,
		XMetric:     metricLabel{ID: xM.ID, Label: toLabel(xM.Name), Format: xM.Format, FormatDecimals: xM.FormatDecimals, FormatCurrency: xM.FormatCurrency},
		YMetric:     metricLabel{ID: yM.ID, Label: toLabel(yM.Name), Format: yM.Format, FormatDecimals: yM.FormatDecimals, FormatCurrency: yM.FormatCurrency},
		Points:      points,
		ContextDims: contextDims,
		Context:     effectiveCtx,
	}, nil
}

// ── Histogram chart ───────────────────────────────────────────────────────────

func (r *ChartResolver) resolveHistogramChart(
	ctx context.Context,
	cfg *ChartConfig,
	plottedMembers []dimMember,
	allowedMetrics map[string]*metricDef,
	effectiveCtx map[string]string,
	modelID, revisionID string,
	allDims map[string]*rollup.Dimension,
	cc *chartCalc,
	contextDims []ChartContextDim,
	asOf string,
) (*HistogramChartData, error) {
	m, ok := allowedMetrics[cfg.MetricIDs[0]]
	if !ok {
		return nil, fmt.Errorf("metric not accessible")
	}

	vals, err := r.resolveMetricPerMember(ctx, m, plottedMembers, effectiveCtx, cfg.DimensionID, modelID, revisionID, allDims, cc)
	if err != nil {
		return nil, err
	}

	// Collect finite observations
	var obs []float64
	for _, v := range vals {
		if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
			continue
		}
		obs = append(obs, *v)
	}

	binCount := cfg.BinCount
	if binCount < 3 {
		binCount = 10
	}
	if binCount > 30 {
		binCount = 30
	}

	bins := buildHistogramBins(obs, binCount)

	return &HistogramChartData{
		ChartType:        string(cfg.ChartType),
		AsOf:             asOf,
		Metric:           metricLabel{ID: m.ID, Label: toLabel(m.Name), Format: m.Format, FormatDecimals: m.FormatDecimals, FormatCurrency: m.FormatCurrency},
		ObservationCount: len(obs),
		Bins:             bins,
		ContextDims:      contextDims,
		Context:          effectiveCtx,
	}, nil
}

// buildHistogramBins creates equal-width bins from observations.
func buildHistogramBins(obs []float64, binCount int) []HistogramBin {
	if len(obs) == 0 {
		return []HistogramBin{}
	}

	sort.Float64s(obs)
	mn, mx := obs[0], obs[len(obs)-1]

	// All same value — one bin
	if mn == mx {
		return []HistogramBin{{Min: mn, Max: mx, Count: len(obs)}}
	}

	width := (mx - mn) / float64(binCount)
	bins := make([]HistogramBin, binCount)
	for i := range bins {
		bins[i] = HistogramBin{
			Min: mn + float64(i)*width,
			Max: mn + float64(i+1)*width,
		}
	}

	for _, v := range obs {
		idx := int((v - mn) / width)
		if idx >= binCount {
			idx = binCount - 1
		}
		bins[idx].Count++
	}

	return bins
}

// ── Per-member value resolution ───────────────────────────────────────────────

// resolveMetricPerMember returns one value per plotted dimension member.
// For input metrics: reads from fact_input with the full dimension context.
// For calculated metrics: served metrics from their persisted rows (a
// withheld point is nil), every other one by evaluating its formula. A
// point that cannot be computed is nil, never a guessed 0.
func (r *ChartResolver) resolveMetricPerMember(
	ctx context.Context,
	m *metricDef,
	members []dimMember,
	effectiveCtx map[string]string,
	plottedDimID string,
	modelID, revisionID string,
	allDims map[string]*rollup.Dimension,
	cc *chartCalc,
) ([]*float64, error) {
	results := make([]*float64, len(members))

	if m.IsInput {
		fetch := r.fetchInput(modelID, revisionID)
		aggRule := rollup.AggRule(m.AggRule)
		for i, member := range members {
			combo := buildDimMembers(effectiveCtx, plottedDimID, member.Code)
			val, ok, err := rollup.ResolveTime(ctx, allDims, m.ID, m.DimensionIDs, aggRule, rollup.TimeSummaryRule(m.TimeSummary), combo, fetch)
			if err != nil || !ok {
				continue
			}
			v := val
			results[i] = &v
		}
		return results, nil
	}

	for i, member := range members {
		combo := buildDimMembers(effectiveCtx, plottedDimID, member.Code)
		val, ok, err := r.evalCalcMetric(ctx, m, combo, allDims, cc)
		if err != nil || !ok {
			continue
		}
		v := val
		results[i] = &v
	}
	return results, nil
}

// fetchInput returns a rollup.RawValue that serves an input metric's values
// from an in-memory map loaded ONCE per metric, instead of a DB round-trip per
// combo. rollup.Resolve fans a single chart cell out to the cartesian product
// of every unpinned dimension's leaves (product=ALL over 500 products × every
// period × …), calling fetch for each — thousands of times per cell, per
// member, per series. Querying per call made an unpinned chart (context={},
// the state it loads in before any selector is touched) hang indefinitely.
// Now each metric's whole deduped fact set is read in one query and the
// recursion becomes in-memory map lookups. The cache is keyed by metric so a
// calc metric's input dependencies (resolved through the same closure) each
// load at most once too. A miss is (0, false, nil) — a legitimately-absent
// value, not a hard failure.
func (r *ChartResolver) fetchInput(modelID, revisionID string) rollup.RawValue {
	cache := map[string]map[string]float64{} // metricID -> canonical(combo) -> value
	return func(ctx context.Context, metricID string, combo map[string]string) (float64, bool, error) {
		fm, loaded := cache[metricID]
		if !loaded {
			var err error
			fm, err = r.loadInputFactMap(ctx, modelID, revisionID, metricID)
			if err != nil {
				return 0, false, err
			}
			cache[metricID] = fm
		}
		key, _ := json.Marshal(combo)
		v, ok := fm[string(key)]
		return v, ok, nil
	}
}

// loadInputFactMap reads every recorded combo of an input metric (latest-wins
// per combo, form 'replace'/'last' mappings overriding directly-typed cells,
// plus additive form postings) in one query, keyed by the canonical JSON of
// its dim_members so a fetch(combo) lookup matches. This is getInputValue's
// old per-combo query with the combo filter removed — run once, not per leaf.
func (r *ChartResolver) loadInputFactMap(ctx context.Context, modelID, revisionID, metricID string) (map[string]float64, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT value, dim_members FROM (
			SELECT DISTINCT ON (dim_members) value, dim_members
			FROM runtime.fact_input
			WHERE model_id=$1::uuid AND revision_id=$2::uuid
			  AND metric_id=$3::uuid AND dim_members != '{}'::jsonb
			  AND source_ref IS NULL
			ORDER BY dim_members, entered_at DESC, id DESC
		) direct
		WHERE NOT EXISTS (
			SELECT 1 FROM runtime.fact_input fi2
			JOIN model.form_metric_mapping fmm ON fmm.id = fi2.source_ref
			WHERE fi2.model_id=$1::uuid AND fi2.revision_id=$2::uuid
			  AND fi2.metric_id=$3::uuid AND fi2.dim_members = direct.dim_members
			  AND fmm.aggregation IN ('replace', 'last')
		)
		UNION ALL
		SELECT value, dim_members
		FROM runtime.fact_input
		WHERE model_id=$1::uuid AND revision_id=$2::uuid
		  AND metric_id=$3::uuid AND source_ref IS NOT NULL
		  AND dim_members != '{}'::jsonb
	`, modelID, revisionID, metricID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]float64{}
	for rows.Next() {
		var val float64
		var dm map[string]string
		if err := rows.Scan(&val, &dm); err != nil {
			return nil, err
		}
		key, _ := json.Marshal(dm) // sorted keys, matches fetch's marshal of combo
		m[string(key)] += val      // additive form postings sum; latest-wins already applied above
	}
	return m, rows.Err()
}

// fetchCalc is fetchInput's twin over runtime.calc_result: a calc metric's
// persisted per-combo rows (latest per combo), loaded once per metric.
func (r *ChartResolver) fetchCalc(modelID, revisionID string) rollup.RawValue {
	cache := map[string]map[string]float64{}
	return func(ctx context.Context, metricID string, combo map[string]string) (float64, bool, error) {
		fm, loaded := cache[metricID]
		if !loaded {
			rows, err := r.pool.Query(ctx, `
				SELECT DISTINCT ON (dim_members) value, dim_members
				FROM runtime.calc_result
				WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid
				ORDER BY dim_members, calc_at DESC, id DESC
			`, modelID, revisionID, metricID)
			if err != nil {
				return 0, false, err
			}
			fm = map[string]float64{}
			for rows.Next() {
				var val float64
				var dm map[string]string
				if err := rows.Scan(&val, &dm); err != nil {
					rows.Close()
					return 0, false, err
				}
				key, _ := json.Marshal(dm)
				fm[string(key)] = val
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return 0, false, err
			}
			cache[metricID] = fm
		}
		key, _ := json.Marshal(combo)
		v, ok := fm[string(key)]
		return v, ok, nil
	}
}

// buildDimMembers constructs the dim_members map for a specific plotted member.
func buildDimMembers(effectiveCtx map[string]string, plottedDimID, memberCode string) map[string]string {
	dm := make(map[string]string, len(effectiveCtx)+1)
	for k, v := range effectiveCtx {
		dm[k] = v
	}
	dm[plottedDimID] = memberCode
	return dm
}

// ── Calculated metric evaluation ──────────────────────────────────────────────

type fullMetricDef struct {
	ID      string
	Name    string
	Formula string
	IsInput bool
	AggRule string
	// TimeSummary reduces the metric across its time dimension; only read
	// for time-series metrics, which resolve from persisted rows.
	TimeSummary string
	// DimensionIDs are this metric's own native dimensions — see metricDef's
	// field of the same name; needed here too since a calc metric's
	// dependency may be a metric outside the current grid entirely.
	DimensionIDs []string
	DependsOnID  []string
}

func (r *ChartResolver) loadAllMetricDefs(ctx context.Context, modelID, revisionID string) (map[string]*fullMetricDef, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT id::text, name, COALESCE(formula,''), is_input, COALESCE(agg_rule,'sum'), time_summary
		FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid
	`, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	defs := make(map[string]*fullMetricDef)
	for rows.Next() {
		var d fullMetricDef
		if err := rows.Scan(&d.ID, &d.Name, &d.Formula, &d.IsInput, &d.AggRule, &d.TimeSummary); err != nil {
			return nil, err
		}
		defs[d.ID] = &d
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Load dependencies
	edgeRows, err := r.pool.Query(ctx, `
		SELECT d.metric_id::text, d.depends_on_metric_id::text
		FROM model.calc_dependency d
		JOIN model.metric_def m ON m.id = d.metric_id
		WHERE m.model_id=$1::uuid AND m.revision_id=$2::uuid
	`, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	defer edgeRows.Close()
	for edgeRows.Next() {
		var from, to string
		if err := edgeRows.Scan(&from, &to); err != nil {
			return nil, err
		}
		if def, ok := defs[from]; ok {
			def.DependsOnID = append(def.DependsOnID, to)
		}
	}
	if err := edgeRows.Err(); err != nil {
		return nil, err
	}

	// Each metric's own dimension IDs (via grid_metric -> grid_dimension on
	// whatever grid it actually lives on) — needed to resolve a dependency
	// metric through rollup.Resolve using its OWN dims, which may differ
	// entirely from the metric referencing it.
	metricDims, err := r.loadMetricDimensionIDs(ctx, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	for id, def := range defs {
		def.DimensionIDs = metricDims[id]
	}

	return defs, nil
}

func (r *ChartResolver) evalCalcMetric(
	ctx context.Context,
	m *metricDef,
	dimMembers map[string]string,
	allDims map[string]*rollup.Dimension,
	cc *chartCalc,
) (float64, bool, error) {
	return r.evalCalcMetricVisited(ctx, m.ID, dimMembers, allDims, cc, make(map[string]bool))
}

// evalCalcMetricVisited computes a calculated metric at dimMembers. ok is
// false when there is no value there (a served metric with no persisted
// row, a blank result). visited guards
// against formula cycles.
//
//   - A SERVED metric (time functions, LOOKUP, the *IFS/*IF family, TIMESUM,
//     the *VALUE family; contract C6) is never re-evaluated at one
//     coordinate: its value depends on other periods and members. It is its
//     persisted rows, rolled up across non-time dimensions by its agg_rule
//     and across time by its time_summary — the number the grid shows;
//     with agg_rule formula or rate, or a pure-ratio average (see
//     leafWise), the persisted row at the point itself (the scheduler's own
//     rollup, slice or '{}' row), never a combination of children. A
//     row withheld from the viewer (contract C7: its read set touches a
//     hidden member) fails the read, so any point built on it is missing.
//   - Every other metric evaluates its formula over its dependencies, with
//     the dimension members bound and the UNFILTERED member-metadata
//     context (dim.property, PARENT), like the scheduler. A dependency that
//     fails fails the point; one with no value reads as 0, as in the
//     scheduler. A blank result is no point, never a 0. With agg_rule sum
//     or count, or an average whose formula depends on the members (see
//     leafWise), the formula is evaluated at each leaf under the point and
//     the leaves combined flat (not level by level) by agg_rule and
//     time_summary, leaves with no value left out — the scheduler's own
//     leaves and totals. With formula, rate or a pure-ratio average it is
//     evaluated once at the point, over aggregated inputs, as the
//     scheduler evaluates it at an aggregate.
//   - An input metric, a served metric and every dependency are read with
//     rollup.ResolveTime, which combines average and count flat over the
//     leaves with a recorded value, as the scheduler's totals do.
func (r *ChartResolver) evalCalcMetricVisited(
	ctx context.Context,
	metricID string,
	dimMembers map[string]string,
	allDims map[string]*rollup.Dimension,
	cc *chartCalc,
	visited map[string]bool,
) (float64, bool, error) {
	if visited[metricID] {
		return 0, false, fmt.Errorf("cycle detected")
	}
	def, ok := cc.defs[metricID]
	if !ok {
		return 0, false, fmt.Errorf("metric def not found")
	}

	visited[metricID] = true
	defer func() { delete(visited, metricID) }()

	dimConditional := calculation.FormulaReferencesDims(def.Formula, cc.dimNames)
	if readset.Served(def.Formula) {
		if !leafWise(def.AggRule, dimConditional) {
			// The scheduler re-evaluates these rules (formula, rate, and a
			// pure-ratio average — tsEvaluator.finish's useEval) AT the
			// aggregate and persists that row (rollup combos for formula and
			// rate, one-dimension slices, the '{}' total); combining the
			// children (a mean of ratios, or of PREVIOUS values) would be a
			// number it never computed. The point is the persisted row at
			// its coordinates on the metric's own dimensions, or none — the
			// scoped reads' persistedTotal.
			// A pin on a dimension's sole root constrains nothing and is
			// dropped, as the grid's scope does: World × Feb is the
			// scheduler's {period: Feb} slice row (a pure-ratio average
			// persists no two-dimension rollup rows).
			own := make(map[string]string, len(def.DimensionIDs))
			for _, d := range def.DimensionIDs {
				if code, ok := dimMembers[d]; ok && !soleRoot(allDims[d], code) {
					own[d] = code
				}
			}
			return cc.servedFetch()(ctx, metricID, own)
		}
		return rollup.ResolveTime(ctx, allDims, metricID, def.DimensionIDs, rollup.AggRule(def.AggRule),
			rollup.TimeSummaryRule(def.TimeSummary), dimMembers, cc.servedFetch())
	}

	if leafWise(def.AggRule, dimConditional) {
		// The scheduler evaluates such a metric at each LEAF and combines
		// the leaf rows by agg_rule (time by time_summary) for every
		// aggregate — its total, slice and scoped rows — so does the point.
		// A formula reading member metadata (dim.property, PARENT, a bare
		// dimension) has its value only at the leaves: a parent member's
		// own property is not its children's. A sum of leaf values is not
		// the formula at summed inputs unless the formula is linear in
		// sums (revenue * 2 over an average input is not); a count is the
		// number of leaves with a (non-zero) value. A leaf with no value
		// (blank, or failing there) is left out, as the scheduler writes no
		// row.
		leaf := func(ctx context.Context, _ string, combo map[string]string) (float64, bool, error) {
			point := make(map[string]string, len(dimMembers)+len(combo))
			for k, v := range dimMembers {
				point[k] = v
			}
			for k, v := range combo {
				point[k] = v
			}
			v, ok, err := r.evalFormulaPoint(ctx, def, point, allDims, cc, visited)
			switch {
			case errors.Is(err, errWithheld):
				return 0, false, err
			case err != nil, !ok:
				return 0, false, rollup.ErrNoValue
			}
			return v, true, nil
		}
		// Combined FLAT over the distinct leaves, as the scheduler does:
		// an average two levels up is the mean of its leaves, never the mean
		// of its children's means (ResolveTime does that too). The Flat
		// variant also sums each leaf once when the point pins a parent of
		// a dimension the metric does not carry, where the scheduler, which
		// evaluates only the metric's own leaves, has nothing to multiply.
		return rollup.ResolveTimeFlat(ctx, allDims, metricID, def.DimensionIDs, rollup.AggRule(def.AggRule),
			rollup.TimeSummaryRule(def.TimeSummary), dimMembers, leaf)
	}
	return r.evalFormulaPoint(ctx, def, dimMembers, allDims, cc, visited)
}

// soleRoot reports whether code is the only top-level member of dim, so a
// pin on it covers every member.
func soleRoot(dim *rollup.Dimension, code string) bool {
	if dim == nil {
		return false
	}
	found := false
	for _, m := range dim.Members {
		if m.ParentCode != "" {
			continue
		}
		if m.Code != code {
			return false
		}
		found = true
	}
	return found
}

// leafWise reports whether a non-served calculated metric's value at an
// aggregate is its leaf values combined (sum, count, and an average whose
// formula depends on the members) rather than its formula evaluated once
// at aggregated inputs (formula, rate, and a pure-ratio average) — the
// scheduler's own split (executePartition's collapse, oneDimSliceRows'
// useEval), so a point and the grid's rows are the same number.
func leafWise(aggRule string, dimConditional bool) bool {
	switch aggRule {
	case string(rollup.AggFormula), string(rollup.AggRate):
		return false
	case string(rollup.AggAverage):
		return dimConditional
	}
	return true
}

// evalFormulaPoint evaluates a non-served metric's formula at dimMembers
// over its dependencies (see evalCalcMetricVisited). A blank result is no
// value (ok=false), never a 0.
func (r *ChartResolver) evalFormulaPoint(
	ctx context.Context,
	def *fullMetricDef,
	dimMembers map[string]string,
	allDims map[string]*rollup.Dimension,
	cc *chartCalc,
	visited map[string]bool,
) (float64, bool, error) {
	varValues := make(map[string]float64, len(def.DependsOnID))
	for _, depID := range def.DependsOnID {
		depDef, ok := cc.defs[depID]
		if !ok {
			continue
		}
		var val float64
		if depDef.IsInput {
			v, ok, err := rollup.ResolveTime(ctx, allDims, depID, depDef.DimensionIDs, rollup.AggRule(depDef.AggRule), rollup.TimeSummaryRule(depDef.TimeSummary), dimMembers, cc.fetchInput)
			if err != nil {
				return 0, false, fmt.Errorf("resolve %s: %w", depDef.Name, err)
			}
			if ok {
				val = v
			}
		} else {
			v, ok, err := r.evalCalcMetricVisited(ctx, depID, dimMembers, allDims, cc, visited)
			if err != nil {
				return 0, false, fmt.Errorf("resolve %s: %w", depDef.Name, err)
			}
			if ok {
				val = v
			}
		}
		varValues[depDef.Name] = val
	}
	namedDims := make(map[string]string, len(dimMembers))
	for dimID, code := range dimMembers {
		if name, ok := cc.dimNames[dimID]; ok {
			namedDims[name] = code
		}
	}
	if def.Formula == "" {
		return 0, false, fmt.Errorf("empty formula")
	}
	v, err := calculation.EvaluateWithDimContext(def.Formula, varValues, namedDims, cc.meta.CellContext(dimMembers))
	if errors.Is(err, calculation.ErrBlankResult) {
		return 0, false, nil // no value there: no point, never a plotted 0
	}
	if err != nil {
		return 0, false, err
	}
	return v, true, nil
}

// errWithheld is a read of a persisted row withheld from the viewer.
var errWithheld = errors.New("the value reads a member hidden from the viewer")

// chartCalc is what computing calculated metrics for one chart request
// needs, loaded once per request.
type chartCalc struct {
	defs     map[string]*fullMetricDef
	dimNames map[string]string // dimension ID -> name
	// meta is the UNFILTERED member metadata (dim.property, PARENT).
	meta *calculation.DimMetadata
	// reads decides withheld rows; nil withholds nothing (the viewer has
	// no hidden member).
	reads      *readset.Set
	fetchCalc  rollup.RawValue
	fetchInput rollup.RawValue
}

// servedFetch reads a served metric's persisted rows, refusing any row
// withheld from the viewer.
func (cc *chartCalc) servedFetch() rollup.RawValue {
	return func(ctx context.Context, metricID string, combo map[string]string) (float64, bool, error) {
		if cc.reads.Withheld(metricID, combo) {
			return 0, false, errWithheld
		}
		return cc.fetchCalc(ctx, metricID, combo)
	}
}

// loadChartCalc loads the revision's metric definitions, its UNFILTERED
// dimensions and member metadata, and — when dimRules hides any member —
// the read sets deciding what is withheld (the same rule the grid applies:
// hidden codes of EVERY dimension, not only the chart's).
func (r *ChartResolver) loadChartCalc(ctx context.Context, modelID, revisionID string, dimRules map[string]string) (*chartCalc, error) {
	defs, err := r.loadAllMetricDefs(ctx, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	unfiltered, err := r.loadAllDimensions(ctx, modelID, revisionID, nil)
	if err != nil {
		return nil, err
	}
	store := calculation.NewStore(r.pool)
	names, err := store.LoadDimIDToName(ctx, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	schema, err := store.LoadDimensionSchema(ctx, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	meta := calculation.NewDimMetadata(unfiltered, names, schema)
	cc := &chartCalc{
		defs: defs, dimNames: names, meta: meta,
		fetchCalc:  r.fetchCalc(modelID, revisionID),
		fetchInput: r.fetchInput(modelID, revisionID),
	}
	hidden := map[string]map[string]bool{}
	for dimID, d := range unfiltered {
		for _, m := range d.Members {
			if dimRules[m.ID] != "hidden" {
				continue
			}
			if hidden[dimID] == nil {
				hidden[dimID] = map[string]bool{}
			}
			hidden[dimID][m.Code] = true
		}
	}
	if len(hidden) > 0 {
		metrics := make([]readset.Metric, 0, len(defs))
		for _, d := range defs {
			metrics = append(metrics, readset.Metric{ID: d.ID, Name: d.Name, IsInput: d.IsInput, Formula: d.Formula, Dims: d.DimensionIDs})
		}
		sort.Slice(metrics, func(i, j int) bool { return metrics[i].ID < metrics[j].ID })
		cc.reads = readset.New(unfiltered, names, meta, metrics, hidden)
	}
	return cc, nil
}

// ── Database loaders ──────────────────────────────────────────────────────────

func (r *ChartResolver) loadGridMetrics(ctx context.Context, gridDefID, revisionID, modelID string) (map[string]*metricDef, error) {
	metricDims, err := r.loadMetricDimensionIDs(ctx, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	rows, err := r.pool.Query(ctx, `
		SELECT rev.id::text, rev.name, rev.is_input, COALESCE(rev.formula,''), COALESCE(rev.agg_rule,'sum'),
		       rev.format, rev.format_decimals, rev.format_currency, rev.time_summary
		FROM model.grid_metric gm
		JOIN model.metric_def orig ON orig.id = gm.metric_id
		JOIN model.metric_def rev
		     ON rev.model_id = orig.model_id
		    AND rev.name     = orig.name
		    AND rev.revision_id = $2::uuid
		WHERE gm.grid_id = $1::uuid
		ORDER BY gm.sort_order
	`, gridDefID, revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	defs := make(map[string]*metricDef)
	for rows.Next() {
		var d metricDef
		if err := rows.Scan(&d.ID, &d.Name, &d.IsInput, &d.Formula, &d.AggRule, &d.Format, &d.FormatDecimals, &d.FormatCurrency, &d.TimeSummary); err != nil {
			return nil, err
		}
		d.Label = toLabel(d.Name)
		d.DimensionIDs = metricDims[d.ID]
		defs[d.ID] = &d
	}
	return defs, rows.Err()
}

func (r *ChartResolver) loadGridDimensions(ctx context.Context, gridDefID string) (map[string][]dimMember, map[string]string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT d.id::text, d.name, m.id::text, m.code, m.label, m.sort_order,
		       COALESCE(pm.code,'') AS parent_code
		FROM model.grid_dimension gd
		JOIN model.dimension_def d ON d.id = gd.dimension_id
		JOIN model.dimension_member m ON m.dimension_id = d.id
		LEFT JOIN model.dimension_member pm ON pm.id = m.parent_member_id
		WHERE gd.grid_id = $1::uuid
		ORDER BY d.name, m.sort_order, m.code
	`, gridDefID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	dims := make(map[string][]dimMember)
	names := make(map[string]string)
	for rows.Next() {
		var dimID, dimName string
		var m dimMember
		if err := rows.Scan(&dimID, &dimName, &m.ID, &m.Code, &m.Label, &m.SortOrder, &m.ParentCode); err != nil {
			return nil, nil, err
		}
		dims[dimID] = append(dims[dimID], m)
		names[dimID] = dimName
	}
	return dims, names, rows.Err()
}

// loadAllDimensions loads every dimension in the revision (regardless of
// grid), hidden-member-filtered per dimRules, in the shape rollup.Resolve
// needs for same-dimension and cross-dimension resolution. Mirrors
// handler.go's all_dimensions query.
func (r *ChartResolver) loadAllDimensions(ctx context.Context, modelID, revisionID string, dimRules map[string]string) (map[string]*rollup.Dimension, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT d.id::text, COALESCE(d.parent_dimension_id::text,''),
		       COALESCE(d.source_dimension_id::text,''), COALESCE(d.source_property,''),
		       d.dimension_type = 'time', COALESCE(d.time_granularity,''), COALESCE(d.fiscal_year_start_month,0),
		       m.id::text, m.code, m.properties, COALESCE(pm.code,'') AS parent_code, COALESCE(m.time_index,-1)
		FROM model.dimension_def d
		JOIN model.dimension_member m ON m.dimension_id = d.id
		LEFT JOIN model.dimension_member pm ON pm.id = m.parent_member_id
		WHERE d.model_id = $1::uuid AND (d.revision_id = $2::uuid OR d.revision_id IS NULL)
		ORDER BY d.name, m.time_index NULLS LAST, m.sort_order, m.code
	`, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	dims := make(map[string]*rollup.Dimension)
	for rows.Next() {
		var dimID, parentDimID, sourceDimID, sourceProp string
		var isTime bool
		var granularity string
		var fiscalStart int
		var memberID, code, parentCode string
		var properties []byte
		var timeIndex int
		if err := rows.Scan(&dimID, &parentDimID, &sourceDimID, &sourceProp, &isTime, &granularity, &fiscalStart,
			&memberID, &code, &properties, &parentCode, &timeIndex); err != nil {
			return nil, err
		}
		dim, ok := dims[dimID]
		if !ok {
			dim = &rollup.Dimension{ID: dimID, ParentDimensionID: parentDimID, SourceDimensionID: sourceDimID, SourceProperty: sourceProp,
				IsTime: isTime, TimeGranularity: granularity, FiscalYearStartMonth: fiscalStart}
			dims[dimID] = dim
		}
		if dimRules[memberID] == "hidden" {
			continue
		}
		var props map[string]string
		if len(properties) > 0 {
			_ = json.Unmarshal(properties, &props)
		}
		dim.Members = append(dim.Members, rollup.Member{ID: memberID, Code: code, ParentCode: parentCode, Properties: props, TimeIndex: timeIndex})
	}
	return dims, rows.Err()
}

// loadMetricDimensionIDs returns each metric's own native dimension IDs, via
// grid_metric -> grid_dimension on whatever grid it actually lives on.
// Mirrors handler.go's metricDims query exactly.
func (r *ChartResolver) loadMetricDimensionIDs(ctx context.Context, modelID, revisionID string) (map[string][]string, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT gm.metric_id::text, gd.dimension_id::text
		FROM model.grid_metric gm
		JOIN model.grid_dimension gd ON gd.grid_id = gm.grid_id
		JOIN model.dimension_def d ON d.id = gd.dimension_id
		JOIN model.metric_def m ON m.id = gm.metric_id
		WHERE m.model_id=$1::uuid AND m.revision_id=$2::uuid
		ORDER BY gm.metric_id, d.name
	`, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	dims := make(map[string][]string)
	for rows.Next() {
		var metricID, dimID string
		if err := rows.Scan(&metricID, &dimID); err != nil {
			return nil, err
		}
		dims[metricID] = append(dims[metricID], dimID)
	}
	return dims, rows.Err()
}

// loadMemberEdges returns every dimension_member's structural parent link
// (parent_member_id, which crosses dimensions in one hop when the owning
// dimension declares parent_dimension_id) across the WHOLE model/revision —
// not just loadGridDimensions' single chart-bound grid — so writeguard.
// ExpandHidden can cascade a hidden rule set on an ancestor dimension (e.g.
// cost_centers) down onto a chart plotted directly on a descendant (e.g.
// employees), even when the two dimensions never appear on the same grid.
func (r *ChartResolver) loadMemberEdges(ctx context.Context, modelID, revisionID string) ([]writeguard.MemberEdge, error) {
	rows, err := r.pool.Query(ctx, `
		SELECT m.id::text, COALESCE(m.parent_member_id::text, ''), d.id::text
		FROM model.dimension_def d
		JOIN model.dimension_member m ON m.dimension_id = d.id
		WHERE d.model_id=$1::uuid AND (d.revision_id=$2::uuid OR d.revision_id IS NULL)
	`, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var edges []writeguard.MemberEdge
	for rows.Next() {
		var e writeguard.MemberEdge
		if err := rows.Scan(&e.ID, &e.ParentID, &e.DimID); err != nil {
			return nil, err
		}
		edges = append(edges, e)
	}
	return edges, rows.Err()
}

// loadAccessRules loads userID's rules resolved by lineage against
// revisionID (writeguard.RulesForRevision): a member or metric hidden in the
// active revision stays hidden when an older revision is charted. Any error
// — query, scan or iteration — is returned so the chart fails closed.
func (r *ChartResolver) loadAccessRules(ctx context.Context, userID, revisionID string) (dimRules, metricRules map[string]string, err error) {
	if userID == "" {
		return map[string]string{}, map[string]string{}, nil
	}
	return writeguard.RuleMaps(ctx, r.pool, userID, revisionID)
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// toLabel converts a snake_case metric name to a human-readable label.
func toLabel(name string) string {
	parts := strings.Split(name, "_")
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

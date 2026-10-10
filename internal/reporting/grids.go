package reporting

import (
	"context"
	"encoding/json"
	"net/url"
	"slices"
	"strings"
	"time"
)

// SourceRef names the grid or form a result was read from.
type SourceRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Completeness says how much of what was asked for a result holds.
type Completeness struct {
	// Complete: the rows are every row the query matches for you.
	Complete bool `json:"complete"`
	Returned int  `json:"returned"`
	// NextCursor continues the result where it stopped.
	NextCursor string `json:"next_cursor,omitempty"`
	Note       string `json:"note,omitempty"`
}

// Value is one number as the engine resolved it. State tells a real zero
// from a value that is not there: "ok", "no_value" (nothing recorded or
// calculable), "withheld" (it depends on data hidden from you), or "absent"
// (no value, or withheld — a source that does not tell the two apart).
type Value struct {
	Value *float64 `json:"value"`
	State string   `json:"state"`
}

func okValue(v float64) Value { return Value{Value: &v, State: "ok"} }

// GridSummary is a readable grid.
type GridSummary struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Dimensions  []Ref    `json:"dimensions"`
	MetricCount int      `json:"metric_count"`
	Rollup      bool     `json:"rollup,omitempty"`
	Operations  []string `json:"operations"`
}

// Ref is an id and its name.
type Ref struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

var gridOperations = []string{"describe_source", "list_members", "query_grid"}

// ListGrids lists the grids of p the person can read (GET /api/grids).
func (s *Session) ListGrids(ctx context.Context, p Pinned, search string) ([]GridSummary, error) {
	var grids []struct {
		ID                 string  `json:"id"`
		Name               string  `json:"name"`
		RevisionID         string  `json:"revision_id"`
		Dimensions         []Ref   `json:"dimensions"`
		MetricCount        int     `json:"metric_count"`
		RollupSourceGridID *string `json:"rollup_source_grid_id"`
	}
	if _, err := s.get(ctx, "grids", p.request("/api/grids", url.Values{"revision_id": {p.RevisionID}}), &grids); err != nil {
		return nil, err
	}
	out := []GridSummary{}
	for _, g := range grids {
		if g.RevisionID != p.RevisionID || !matches(search, g.Name) {
			continue
		}
		out = append(out, GridSummary{
			ID: g.ID, Name: g.Name, Dimensions: g.Dimensions, MetricCount: g.MetricCount,
			Rollup: g.RollupSourceGridID != nil, Operations: gridOperations,
		})
	}
	return out, nil
}

// gridMetaResponse is the part of /api/grid?meta_only=1 read here. It is
// the person's own view: hidden members and metrics are already gone.
type gridMetaResponse struct {
	RevisionID string       `json:"revision_id"`
	Metrics    []gridMetric `json:"metrics"`
	Dimensions []struct {
		ID              string   `json:"id"`
		Name            string   `json:"name"`
		DimensionType   string   `json:"dimension_type"`
		TimeGranularity *string  `json:"time_granularity"`
		Members         []member `json:"members"`
	} `json:"dimensions"`
}

type gridMetric struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	Label          string   `json:"label"`
	IsInput        bool     `json:"is_input"`
	AggRule        string   `json:"agg_rule"`
	Format         string   `json:"format"`
	FormatDecimals int      `json:"format_decimals"`
	FormatCurrency string   `json:"format_currency"`
	TimeSummary    string   `json:"time_summary"`
	Readonly       bool     `json:"readonly"`
	DimensionIDs   []string `json:"dimension_ids"`
}

type member struct {
	Code        string  `json:"code"`
	Label       string  `json:"label"`
	ParentCode  string  `json:"parent_code"`
	PeriodStart *string `json:"period_start"`
	PeriodEnd   *string `json:"period_end"`
	Readonly    bool    `json:"readonly"`
}

// grid is a readable grid of p with the person's view of it.
type grid struct {
	summary GridSummary
	meta    gridMetaResponse
}

func (g *grid) dimension(id string) (int, bool) {
	for i, d := range g.meta.Dimensions {
		if d.ID == id {
			return i, true
		}
	}
	return 0, false
}

func (s *Session) loadGrid(ctx context.Context, p Pinned, gridID string) (*grid, error) {
	grids, err := s.ListGrids(ctx, p, "")
	if err != nil {
		return nil, err
	}
	g := &grid{}
	found := false
	for _, sum := range grids {
		if sum.ID == gridID {
			g.summary, found = sum, true
			break
		}
	}
	if !found {
		return nil, notFound("grid")
	}
	q := url.Values{"grid_def_id": {gridID}, "meta_only": {"1"}, "revision_id": {p.RevisionID}}
	if _, err := s.get(ctx, "grid", p.request("/api/grid", q), &g.meta); err != nil {
		return nil, err
	}
	if g.meta.RevisionID != p.RevisionID {
		return nil, notFound("grid")
	}
	return g, nil
}

// MetricInfo describes a metric as the person may read it. Formulas are
// not included: they can name metrics hidden from the reader.
type MetricInfo struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Label        string   `json:"label"`
	Kind         string   `json:"kind"`
	Aggregation  string   `json:"aggregation"`
	TimeSummary  string   `json:"time_summary,omitempty"`
	Format       string   `json:"format"`
	Decimals     int      `json:"decimals"`
	Currency     string   `json:"currency,omitempty"`
	ReadOnly     bool     `json:"read_only,omitempty"`
	DimensionIDs []string `json:"dimension_ids,omitempty"`
}

// DimensionInfo describes a grid dimension.
type DimensionInfo struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Type            string `json:"type"`
	TimeGranularity string `json:"time_granularity,omitempty"`
	MemberCount     int    `json:"member_count"`
}

// GridDescription is describe_source for a grid.
type GridDescription struct {
	Context    Pinned          `json:"context"`
	Source     SourceRef       `json:"source"`
	Metrics    []MetricInfo    `json:"metrics"`
	Dimensions []DimensionInfo `json:"dimensions"`
	Rollup     bool            `json:"rollup,omitempty"`
	Operations []string        `json:"operations"`
	Notes      []string        `json:"notes"`
}

// DescribeGrid describes gridID's metrics and dimensions.
func (s *Session) DescribeGrid(ctx context.Context, p Pinned, gridID string) (*GridDescription, error) {
	g, err := s.loadGrid(ctx, p, gridID)
	if err != nil {
		return nil, err
	}
	d := &GridDescription{
		Context: p, Source: SourceRef{Kind: "grid", ID: gridID, Name: g.summary.Name},
		Metrics: []MetricInfo{}, Dimensions: []DimensionInfo{},
		Rollup: g.summary.Rollup, Operations: gridOperations,
		Notes: []string{
			"Values come from the engine: inputs roll up by each metric's aggregation and time summary, formulas are evaluated by the engine. Ratios, averages and balances are not additive — query the total you need instead of adding rows.",
		},
	}
	for _, m := range g.meta.Metrics {
		kind := "calculated"
		if m.IsInput {
			kind = "input"
		}
		d.Metrics = append(d.Metrics, MetricInfo{
			ID: m.ID, Name: m.Name, Label: m.Label, Kind: kind, Aggregation: m.AggRule, TimeSummary: m.TimeSummary,
			Format: m.Format, Decimals: m.FormatDecimals, Currency: m.FormatCurrency, ReadOnly: m.Readonly,
			DimensionIDs: m.DimensionIDs,
		})
	}
	for _, dim := range g.meta.Dimensions {
		info := DimensionInfo{ID: dim.ID, Name: dim.Name, Type: dim.DimensionType, MemberCount: len(dim.Members)}
		if dim.TimeGranularity != nil {
			info.TimeGranularity = *dim.TimeGranularity
		}
		d.Dimensions = append(d.Dimensions, info)
	}
	return d, nil
}

// MemberInfo is a dimension member the person may see.
type MemberInfo struct {
	Code        string `json:"code"`
	Label       string `json:"label"`
	ParentCode  string `json:"parent_code,omitempty"`
	Leaf        bool   `json:"leaf"`
	PeriodStart string `json:"period_start,omitempty"`
	PeriodEnd   string `json:"period_end,omitempty"`
	ReadOnly    bool   `json:"read_only,omitempty"`
}

// MemberPage is list_members' result.
type MemberPage struct {
	Context   Pinned       `json:"context"`
	Source    SourceRef    `json:"source"`
	Dimension Ref          `json:"dimension"`
	Members   []MemberInfo `json:"members"`
	Completeness
}

// MemberQuery selects members of one grid dimension.
type MemberQuery struct {
	GridID, DimensionID string
	Search, ParentCode  string
	Limit               int
	Cursor              string
}

// ListMembers pages through the members of one grid dimension the person
// may see, in the dimension's own order (calendar order for time).
func (s *Session) ListMembers(ctx context.Context, p Pinned, q MemberQuery) (*MemberPage, error) {
	g, err := s.loadGrid(ctx, p, q.GridID)
	if err != nil {
		return nil, err
	}
	i, ok := g.dimension(q.DimensionID)
	if !ok {
		return nil, invalid("dimension %s is not a dimension of this grid; describe_source lists them", q.DimensionID)
	}
	dim := g.meta.Dimensions[i]
	limit := clampLimit(q.Limit, 200, 1000)
	key := queryKey("list_members", p, q.GridID, q.DimensionID, q.Search, q.ParentCode)
	offset := 0
	if q.Cursor != "" {
		st, err := s.s.cursors.open(q.Cursor, s.subject, key, s.s.now())
		if err != nil {
			return nil, err
		}
		offset = st.Offset
	}
	parents := map[string]bool{}
	for _, m := range dim.Members {
		if m.ParentCode != "" {
			parents[m.ParentCode] = true
		}
	}
	var all []MemberInfo
	for _, m := range dim.Members {
		if q.ParentCode != "" && m.ParentCode != q.ParentCode {
			continue
		}
		if !matches(q.Search, m.Code, m.Label) {
			continue
		}
		info := MemberInfo{Code: m.Code, Label: m.Label, ParentCode: m.ParentCode, Leaf: !parents[m.Code], ReadOnly: m.Readonly}
		if m.PeriodStart != nil {
			info.PeriodStart = *m.PeriodStart
		}
		if m.PeriodEnd != nil {
			info.PeriodEnd = *m.PeriodEnd
		}
		all = append(all, info)
	}
	page := &MemberPage{
		Context: p, Source: SourceRef{Kind: "grid", ID: q.GridID, Name: g.summary.Name},
		Dimension: Ref{ID: dim.ID, Name: dim.Name}, Members: []MemberInfo{},
	}
	if offset < len(all) {
		end := min(offset+limit, len(all))
		page.Members = all[offset:end]
		if end < len(all) {
			page.NextCursor = s.s.cursors.seal(cursorState{Subject: s.subject, Query: key, Offset: end}, s.s.now())
		}
	}
	page.Returned = len(page.Members)
	page.Complete = page.NextCursor == "" && offset == 0
	return page, nil
}

// GridQuery is query_grid's input.
type GridQuery struct {
	GridID    string
	MetricIDs []string
	// Filters fix dimensions to one member each (dimension id → member
	// code). A parent member covers its whole subtree.
	Filters map[string]string
	// GroupBy is the dimension the values are broken down by; empty asks
	// for one total per metric.
	GroupBy string
	// LeavesOnly keeps only the group-by dimension's leaf members (no
	// parent subtotals beside their children).
	LeavesOnly bool
}

// GridResult is query_grid's result: one row per metric (no group-by) or
// per member of the group-by dimension, values resolved by the engine.
type GridResult struct {
	Context Pinned    `json:"context"`
	Source  SourceRef `json:"source"`
	Metrics []Ref     `json:"metrics"`
	Filters []Filter  `json:"filters"`
	GroupBy *Ref      `json:"group_by,omitempty"`
	// LeavesOnly: the breakdown holds leaf members only.
	LeavesOnly bool     `json:"leaves_only,omitempty"`
	Columns    []Column `json:"columns"`
	Rows       []Row    `json:"rows"`
	ReadAt     string   `json:"read_at"`
	Completeness
	Notes []string `json:"notes,omitempty"`
}

// Filter is an applied filter with its readable names.
type Filter struct {
	DimensionID   string `json:"dimension_id"`
	DimensionName string `json:"dimension_name"`
	MemberCode    string `json:"member_code"`
	MemberLabel   string `json:"member_label"`
}

// Column describes a result column.
type Column struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Type     string `json:"type"`
	Format   string `json:"format,omitempty"`
	Decimals int    `json:"decimals,omitempty"`
	Currency string `json:"currency,omitempty"`
	Unit     string `json:"unit,omitempty"`
}

// Row is one result row keyed by column id; number columns hold a Value.
type Row map[string]any

const maxGroupMembers = 500

// QueryGrid reads metric values of a grid: engine totals for the filtered
// slice, or the engine's per-member values along one dimension.
func (s *Session) QueryGrid(ctx context.Context, p Pinned, q GridQuery) (*GridResult, error) {
	g, err := s.loadGrid(ctx, p, q.GridID)
	if err != nil {
		return nil, err
	}
	res := &GridResult{
		Context: p, Source: SourceRef{Kind: "grid", ID: q.GridID, Name: g.summary.Name},
		Metrics: []Ref{}, Filters: []Filter{}, Rows: []Row{},
	}

	// Metrics: every one requested must be one the person sees in this grid.
	type metricCol struct {
		ref Ref
		col Column
	}
	var metrics []metricCol
	byID := map[string]int{}
	for i, m := range g.meta.Metrics {
		byID[m.ID] = i
	}
	ids := q.MetricIDs
	if len(ids) == 0 {
		for _, m := range g.meta.Metrics {
			ids = append(ids, m.ID)
		}
		if len(ids) > 20 {
			ids = ids[:20]
			res.Notes = append(res.Notes, "only the first 20 metrics were read; name metric_ids to read others")
		}
	}
	for _, id := range ids {
		i, ok := byID[id]
		if !ok {
			return nil, invalid("metric %s is not available in this grid; describe_source lists the metrics you can read", id)
		}
		m := g.meta.Metrics[i]
		label := m.Label
		if label == "" {
			label = m.Name
		}
		metrics = append(metrics, metricCol{
			ref: Ref{ID: m.ID, Name: label},
			col: Column{ID: m.ID, Label: label, Type: "number", Format: m.Format, Decimals: m.FormatDecimals, Currency: m.FormatCurrency},
		})
		res.Metrics = append(res.Metrics, Ref{ID: m.ID, Name: label})
	}

	// Filters: a dimension of the grid and a member the person sees in it,
	// both checked here — the engine would substitute a visible member for
	// a hidden one, and report another slice under this one's name.
	for dimID, code := range q.Filters {
		i, ok := g.dimension(dimID)
		if !ok {
			return nil, invalid("dimension %s is not a dimension of this grid", dimID)
		}
		dim := g.meta.Dimensions[i]
		idx := slices.IndexFunc(dim.Members, func(m member) bool { return m.Code == code })
		if idx < 0 {
			return nil, invalid("member %q is not available in dimension %s; list_members lists the members you can read", code, dim.Name)
		}
		res.Filters = append(res.Filters, Filter{DimensionID: dimID, DimensionName: dim.Name, MemberCode: code, MemberLabel: dim.Members[idx].Label})
	}
	slices.SortFunc(res.Filters, func(a, b Filter) int { return strings.Compare(a.DimensionName, b.DimensionName) })
	scope, _ := json.Marshal(q.Filters)

	if q.GroupBy == "" {
		res.Columns = []Column{{ID: "metric", Label: "Metric", Type: "string"}, {ID: "value", Label: "Value", Type: "number"}}
		var totals struct {
			RevisionID string             `json:"revision_id"`
			Totals     map[string]float64 `json:"totals"`
			Withheld   []string           `json:"withheld"`
		}
		qv := url.Values{"grid_def_id": {q.GridID}, "totals_only": {"1"}, "revision_id": {p.RevisionID}}
		if len(q.Filters) > 0 {
			qv.Set("scope", string(scope))
		}
		if _, err := s.get(ctx, "grid", p.request("/api/grid", qv), &totals); err != nil {
			return nil, err
		}
		if totals.RevisionID != p.RevisionID {
			return nil, notFound("grid")
		}
		for _, m := range metrics {
			v := Value{State: "no_value"}
			switch {
			case slices.Contains(totals.Withheld, m.ref.ID):
				v = Value{State: "withheld"}
			default:
				if t, ok := totals.Totals[m.ref.ID]; ok {
					v = okValue(t)
				}
			}
			res.Rows = append(res.Rows, Row{"metric_id": m.ref.ID, "metric": m.ref.Name, "value": v,
				"format": m.col.Format, "currency": m.col.Currency})
		}
	} else {
		i, ok := g.dimension(q.GroupBy)
		if !ok {
			return nil, invalid("group_by %s is not a dimension of this grid", q.GroupBy)
		}
		if _, filtered := q.Filters[q.GroupBy]; filtered {
			return nil, invalid("a dimension cannot be both filtered and grouped by; filter another dimension or drop the group_by")
		}
		dim := g.meta.Dimensions[i]
		res.GroupBy = &Ref{ID: dim.ID, Name: dim.Name}
		res.LeavesOnly = q.LeavesOnly
		res.Columns = []Column{{ID: "member_code", Label: dim.Name + " code", Type: "string"}, {ID: "member", Label: dim.Name, Type: "string"}}
		for _, m := range metrics {
			res.Columns = append(res.Columns, m.col)
		}
		rowsByCode := map[string]Row{}
		var order []string
		for start := 0; start < len(metrics); start += 5 {
			batch := metrics[start:min(start+5, len(metrics))]
			batchIDs := make([]string, len(batch))
			for k, m := range batch {
				batchIDs[k] = m.ref.ID
			}
			qv := url.Values{
				"grid_def_id": {q.GridID}, "revision_id": {p.RevisionID}, "chart_type": {"bar"},
				"dimension_id": {q.GroupBy}, "metric_ids": {strings.Join(batchIDs, ",")},
			}
			if len(q.Filters) > 0 {
				qv.Set("context", string(scope))
			}
			if q.LeavesOnly {
				qv.Set("hide_rollup_members", "1")
			}
			var series struct {
				Categories []struct {
					Key   string `json:"key"`
					Label string `json:"label"`
				} `json:"categories"`
				Series []struct {
					MetricID string     `json:"metric_id"`
					Values   []*float64 `json:"values"`
				} `json:"series"`
				Context map[string]string `json:"context"`
			}
			if _, err := s.get(ctx, "grid", p.request("/api/grid/series", qv), &series); err != nil {
				return nil, err
			}
			for dimID, want := range q.Filters {
				if series.Context[dimID] != want {
					return nil, invalid("a filter could not be applied as given; run describe_source and list_members again")
				}
			}
			if len(series.Categories) > maxGroupMembers {
				return nil, invalid("%s has %d members here; filter it or set leaves_only", dim.Name, len(series.Categories))
			}
			for ci, c := range series.Categories {
				row, seen := rowsByCode[c.Key]
				if !seen {
					row = Row{"member_code": c.Key, "member": c.Label}
					rowsByCode[c.Key] = row
					order = append(order, c.Key)
				}
				for _, sr := range series.Series {
					v := Value{State: "absent"}
					if ci < len(sr.Values) && sr.Values[ci] != nil {
						v = okValue(*sr.Values[ci])
					}
					row[sr.MetricID] = v
				}
			}
		}
		for _, code := range order {
			res.Rows = append(res.Rows, rowsByCode[code])
		}
		res.Notes = append(res.Notes, "\"absent\" values have no number for you: nothing recorded or calculable there, or the value depends on data hidden from you. They are not zero.")
		if !q.LeavesOnly {
			res.Notes = append(res.Notes, "rows include parent members beside their children; do not add parents and children together — set leaves_only to read leaf members only")
		}
	}
	res.ReadAt = s.s.now().UTC().Format(time.RFC3339)
	res.Returned = len(res.Rows)
	res.Complete = true
	return res, nil
}

func clampLimit(n, def, max int) int {
	if n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

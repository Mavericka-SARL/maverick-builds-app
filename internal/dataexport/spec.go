// Package dataexport turns a grid's values into a file in a format someone
// specified: CSV (any delimiter, either decimal separator), XLSX or JSON, in
// a wide, long or pivoted layout, with chosen and renamed columns, member
// codes or labels, member filters and rounding.
//
// A Spec is the stored config of a "file_export" integration
// (model.integration_def). It names metrics, dimensions and members by NAME
// and CODE, never by id, so it survives a revision copy and a model export
// unchanged. Exports are leaf-level: one row per leaf combination, values
// exactly as the grid serves them (inputs and calculated metrics alike).
// Aggregating to a parent level is left to the spreadsheet — a server-side
// rollup here would be a second implementation of what /api/grid computes,
// free to disagree with it.
//
// The package is pure. Its caller supplies the grid's structure (Grid) to
// validate a spec and the requesting user's own view of the grid's values
// (Snapshot) to render one, so a download never shows a member or metric
// that user cannot see.
package dataexport

import (
	"fmt"
	"sort"
	"strings"
)

// Spec is a file_export integration's config.
type Spec struct {
	Format           string              `json:"format,omitempty"`            // csv (default) | xlsx | json
	Layout           string              `json:"layout,omitempty"`            // wide (default) | long | pivot
	PivotDimension   string              `json:"pivot_dimension,omitempty"`   // pivot: this dimension's leaf members become columns
	Metrics          []string            `json:"metrics,omitempty"`           // metric names in column order; empty = every grid metric
	Dimensions       []string            `json:"dimensions,omitempty"`        // dimension names in column order; empty = every grid dimension
	MemberDisplay    string              `json:"member_display,omitempty"`    // code (default) | label | code_and_label
	MetricDisplay    string              `json:"metric_display,omitempty"`    // name (default) | label
	Filters          map[string][]string `json:"filters,omitempty"`           // dimension name -> member codes; a parent stands for every leaf under it
	ColumnNames      map[string]string   `json:"column_names,omitempty"`      // default header -> header written to the file
	Decimals         *int                `json:"decimals,omitempty"`          // round to this many places; omitted = full precision
	Delimiter        string              `json:"delimiter,omitempty"`         // csv: "," (default) ";" "tab" "|"
	DecimalSeparator string              `json:"decimal_separator,omitempty"` // csv: "." (default) or ","
	IncludeHeader    *bool               `json:"include_header,omitempty"`    // csv: default true
	IncludeEmptyRows bool                `json:"include_empty_rows,omitempty"`
	SheetName        string              `json:"sheet_name,omitempty"` // xlsx: default "Data"
	FileName         string              `json:"file_name,omitempty"`  // without extension; default = the export's name
}

const (
	FormatCSV  = "csv"
	FormatXLSX = "xlsx"
	FormatJSON = "json"

	LayoutWide  = "wide"
	LayoutLong  = "long"
	LayoutPivot = "pivot"

	DisplayCode         = "code"
	DisplayLabel        = "label"
	DisplayCodeAndLabel = "code_and_label"
	DisplayName         = "name"

	// MaxRows caps one file. An export that would exceed it is refused with
	// a message naming filters as the way down, rather than truncated.
	MaxRows = 200000
)

// Normalized returns the spec with every default filled in, so the rest of
// the package never re-derives one.
func (s Spec) Normalized() Spec {
	s.Format = strings.ToLower(strings.TrimSpace(s.Format))
	if s.Format == "" {
		s.Format = FormatCSV
	}
	s.Layout = strings.ToLower(strings.TrimSpace(s.Layout))
	if s.Layout == "" {
		s.Layout = LayoutWide
	}
	s.MemberDisplay = strings.ToLower(strings.TrimSpace(s.MemberDisplay))
	if s.MemberDisplay == "" {
		s.MemberDisplay = DisplayCode
	}
	s.MetricDisplay = strings.ToLower(strings.TrimSpace(s.MetricDisplay))
	if s.MetricDisplay == "" {
		s.MetricDisplay = DisplayName
	}
	switch strings.ToLower(s.Delimiter) {
	case "":
		s.Delimiter = ","
	case "tab", `\t`:
		s.Delimiter = "\t"
	}
	if s.DecimalSeparator == "" {
		s.DecimalSeparator = "."
	}
	if s.IncludeHeader == nil {
		t := true
		s.IncludeHeader = &t
	}
	if strings.TrimSpace(s.SheetName) == "" {
		s.SheetName = "Data"
	}
	return s
}

// Member, Dimension and Metric describe a grid's structure. Members are in
// display order; a member whose code is no other member's ParentCode is a
// leaf.
type Member struct {
	Code       string
	Label      string
	ParentCode string
}

type Dimension struct {
	ID      string
	Name    string
	Members []Member
}

type Metric struct {
	ID    string
	Name  string
	Label string
	// DimensionIDs orders the codes of this metric's cell keys
	// ("metricID:code1:code2…"), the way /api/grid keys `cells`.
	DimensionIDs []string
	// Picklist maps a pick-list metric's stored keys to the labels of the
	// members they hold (nil for any other metric): an export writes the
	// member, never the key, and an emptied cell (key 0) as empty.
	Picklist map[float64]string
	// Text marks a text metric: its cells are written as their text.
	Text bool
}

// Grid is a grid's structure: what a spec is validated against.
type Grid struct {
	Name       string
	Dimensions []Dimension
	Metrics    []Metric
}

// MetricLabel is the display label of a metric name ("gross_margin" ->
// "Gross Margin") — the rule /api/grid serves as metrics[].label.
func MetricLabel(name string) string {
	parts := strings.Split(name, "_")
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

func (g Grid) dimension(name string) (Dimension, bool) {
	for _, d := range g.Dimensions {
		if strings.EqualFold(d.Name, strings.TrimSpace(name)) {
			return d, true
		}
	}
	return Dimension{}, false
}

func (g Grid) metric(name string) (Metric, bool) {
	for _, m := range g.Metrics {
		if strings.EqualFold(m.Name, strings.TrimSpace(name)) {
			return m, true
		}
	}
	return Metric{}, false
}

func dimensionNames(ds []Dimension) string {
	names := make([]string, len(ds))
	for i, d := range ds {
		names[i] = d.Name
	}
	return strings.Join(names, ", ")
}

func metricNames(ms []Metric) string {
	names := make([]string, len(ms))
	for i, m := range ms {
		names[i] = m.Name
	}
	return strings.Join(names, ", ")
}

// leaves is a dimension's leaf members in display order.
func (d Dimension) leaves() []Member {
	parents := map[string]bool{}
	for _, m := range d.Members {
		if m.ParentCode != "" {
			parents[m.ParentCode] = true
		}
	}
	out := make([]Member, 0, len(d.Members))
	for _, m := range d.Members {
		if !parents[m.Code] {
			out = append(out, m)
		}
	}
	return out
}

// leavesUnder resolves filter codes to the leaf members they stand for: a
// leaf itself, or every leaf below a parent. Unknown codes are returned
// separately; for a requesting user they include members hidden from them.
func (d Dimension) leavesUnder(codes []string) (leafCodes map[string]bool, unknown []string) {
	children := map[string][]string{}
	known := map[string]bool{}
	for _, m := range d.Members {
		known[m.Code] = true
		if m.ParentCode != "" {
			children[m.ParentCode] = append(children[m.ParentCode], m.Code)
		}
	}
	leafCodes = map[string]bool{}
	var walk func(code string, depth int)
	walk = func(code string, depth int) {
		if depth > len(d.Members) {
			return // a parent cycle; never loop
		}
		kids := children[code]
		if len(kids) == 0 {
			leafCodes[code] = true
			return
		}
		for _, k := range kids {
			walk(k, depth+1)
		}
	}
	for _, c := range codes {
		c = strings.TrimSpace(c)
		if !known[c] {
			unknown = append(unknown, c)
			continue
		}
		walk(c, 0)
	}
	return leafCodes, unknown
}

// plan is a spec resolved against a grid: which dimensions are row
// columns, which is pivoted, which metrics, and each dimension's allowed
// leaves.
type plan struct {
	spec     Spec
	rowDims  []Dimension
	pivotDim *Dimension
	metrics  []Metric
	allowed  map[string]map[string]bool // dimension ID -> allowed leaf codes
	// pinned dimensions are filtered to exactly one leaf and are not a
	// column; their code is part of every cell's identity all the same.
	pinned map[string]string
}

// Validate checks a spec against a grid's structure and returns every
// problem found, so a caller can fix them in one pass.
func Validate(spec Spec, g Grid) []string {
	_, problems := resolve(spec.Normalized(), g, true)
	return problems
}

// ValidationError joins Validate's problems into one error, or nil.
func ValidationError(spec Spec, g Grid) error {
	if problems := Validate(spec, g); len(problems) > 0 {
		return fmt.Errorf("export spec: %s", strings.Join(problems, "; "))
	}
	return nil
}

// resolve builds the plan. strict = validating a stored spec against the
// grid's full structure: unknown names are problems. Not strict = rendering
// for a user whose view may lack hidden metrics and members: those are
// silently absent, exactly as they are from that user's grid.
func resolve(s Spec, g Grid, strict bool) (*plan, []string) {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	switch s.Format {
	case FormatCSV, FormatXLSX, FormatJSON:
	default:
		add("format must be csv, xlsx or json, not %q", s.Format)
	}
	switch s.Layout {
	case LayoutWide, LayoutLong, LayoutPivot:
	default:
		add("layout must be wide, long or pivot, not %q", s.Layout)
	}
	switch s.MemberDisplay {
	case DisplayCode, DisplayLabel, DisplayCodeAndLabel:
	default:
		add("member_display must be code, label or code_and_label, not %q", s.MemberDisplay)
	}
	switch s.MetricDisplay {
	case DisplayName, DisplayLabel:
	default:
		add("metric_display must be name or label, not %q", s.MetricDisplay)
	}
	switch s.Delimiter {
	case ",", ";", "\t", "|":
	default:
		add("delimiter must be \",\", \";\", \"tab\" or \"|\", not %q", s.Delimiter)
	}
	switch s.DecimalSeparator {
	case ".", ",":
	default:
		add("decimal_separator must be \".\" or \",\", not %q", s.DecimalSeparator)
	}
	if s.Format == FormatCSV && s.DecimalSeparator == "," && s.Delimiter == "," {
		add("a \",\" decimal separator needs another delimiter (\";\" is usual)")
	}
	if s.Decimals != nil && (*s.Decimals < 0 || *s.Decimals > 10) {
		add("decimals must be between 0 and 10")
	}
	if len(s.SheetName) > 31 || strings.ContainsAny(s.SheetName, `[]:*?/\`) {
		add("sheet_name must be at most 31 characters, without []:*?/\\")
	}
	if len(s.FileName) > 100 {
		add("file_name must be at most 100 characters")
	}
	if len(g.Dimensions) == 0 {
		add("grid %q has no dimensions", g.Name)
	}

	p := &plan{spec: s, allowed: map[string]map[string]bool{}, pinned: map[string]string{}}

	// Metrics.
	if len(s.Metrics) == 0 {
		p.metrics = append(p.metrics, g.Metrics...)
	} else {
		seen := map[string]bool{}
		for _, name := range s.Metrics {
			m, ok := g.metric(name)
			if !ok {
				if strict {
					add("metric %q is not in grid %q (its metrics: %s)", name, g.Name, metricNames(g.Metrics))
				}
				continue
			}
			if seen[m.ID] {
				add("metric %q is listed twice", name)
				continue
			}
			seen[m.ID] = true
			p.metrics = append(p.metrics, m)
		}
	}
	if strict && len(g.Metrics) == 0 {
		add("grid %q has no metrics", g.Name)
	}

	// Filters, resolved to allowed leaves per dimension.
	for name, codes := range s.Filters {
		d, ok := g.dimension(name)
		if !ok {
			add("filter dimension %q is not in grid %q (its dimensions: %s)", name, g.Name, dimensionNames(g.Dimensions))
			continue
		}
		if len(codes) == 0 {
			add("filter on %q lists no members", d.Name)
			continue
		}
		leaves, unknown := d.leavesUnder(codes)
		if strict && len(unknown) > 0 {
			add("filter on %q names member(s) %s that are not in it", d.Name, strings.Join(unknown, ", "))
		}
		p.allowed[d.ID] = leaves
	}
	for _, d := range g.Dimensions {
		if _, filtered := p.allowed[d.ID]; filtered {
			continue
		}
		all := map[string]bool{}
		for _, m := range d.leaves() {
			all[m.Code] = true
		}
		p.allowed[d.ID] = all
	}

	// Pivot.
	if s.Layout == LayoutPivot {
		if strings.TrimSpace(s.PivotDimension) == "" {
			add("a pivot layout needs pivot_dimension — the dimension whose members become columns")
		} else if d, ok := g.dimension(s.PivotDimension); !ok {
			add("pivot_dimension %q is not in grid %q (its dimensions: %s)", s.PivotDimension, g.Name, dimensionNames(g.Dimensions))
		} else {
			p.pivotDim = &d
		}
	} else if strings.TrimSpace(s.PivotDimension) != "" {
		add("pivot_dimension applies only to layout \"pivot\"")
	}

	// Row dimensions: listed ones in order, else every non-pivot dimension.
	inRows := map[string]bool{}
	if len(s.Dimensions) == 0 {
		for _, d := range g.Dimensions {
			if p.pivotDim != nil && d.ID == p.pivotDim.ID {
				continue
			}
			p.rowDims = append(p.rowDims, d)
			inRows[d.ID] = true
		}
	} else {
		for _, name := range s.Dimensions {
			d, ok := g.dimension(name)
			if !ok {
				add("dimension %q is not in grid %q (its dimensions: %s)", name, g.Name, dimensionNames(g.Dimensions))
				continue
			}
			if inRows[d.ID] {
				add("dimension %q is listed twice", d.Name)
				continue
			}
			if p.pivotDim != nil && d.ID == p.pivotDim.ID {
				add("dimension %q is the pivot dimension; leave it out of dimensions", d.Name)
				continue
			}
			p.rowDims = append(p.rowDims, d)
			inRows[d.ID] = true
		}
	}
	// A dimension that is neither a column nor the pivot must be pinned to
	// one leaf: anything else would be an aggregate, and exports are
	// leaf-level.
	for _, d := range g.Dimensions {
		if inRows[d.ID] || (p.pivotDim != nil && d.ID == p.pivotDim.ID) {
			continue
		}
		// Rendering for a user the pinned member is hidden from leaves no
		// leaf at all: pinned to nothing, the export is simply empty.
		_, filtered := s.filterFor(d.Name)
		if filtered && (len(p.allowed[d.ID]) == 1 || (!strict && len(p.allowed[d.ID]) == 0)) {
			p.pinned[d.ID] = ""
			for code := range p.allowed[d.ID] {
				p.pinned[d.ID] = code
			}
			continue
		}
		add("dimension %q is not a column: add it to dimensions, or filter it to exactly one leaf member — exports are leaf-level, one row per leaf combination", d.Name)
	}

	// Column names: every key must be a column this export has.
	if len(s.ColumnNames) > 0 && len(problems) == 0 {
		defaults := map[string]bool{}
		for _, h := range p.defaultHeader(g) {
			defaults[strings.ToLower(h)] = true
		}
		var keys []string
		for k := range s.ColumnNames {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if strings.TrimSpace(s.ColumnNames[k]) == "" {
				add("column_names[%q] is empty", k)
			}
			if strict && !defaults[strings.ToLower(k)] {
				add("column_names key %q is not a column of this export (its columns: %s)", k, strings.Join(p.defaultHeader(g), ", "))
			}
		}
		final := map[string]bool{}
		for _, h := range p.header(g) {
			if final[strings.ToLower(h)] {
				add("two columns would both be named %q", h)
			}
			final[strings.ToLower(h)] = true
		}
	}
	return p, problems
}

func (s Spec) filterFor(dimName string) ([]string, bool) {
	for k, v := range s.Filters {
		if strings.EqualFold(strings.TrimSpace(k), dimName) {
			return v, true
		}
	}
	return nil, false
}

func (s Spec) rename(header string) string {
	if v, ok := s.ColumnNames[header]; ok && strings.TrimSpace(v) != "" {
		return v
	}
	for k, v := range s.ColumnNames {
		if strings.EqualFold(k, header) && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return header
}

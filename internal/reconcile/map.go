// Package reconcile compares a workbook with a revision, cell by cell
// (2026-11-21): the developer who rebuilds a spreadsheet model keeps the
// workbook with the revision, describes in rules which of its cells are
// which model cells, and gets the differences — per case (scenario) of the
// workbook, recalculated by LibreOffice when a case changes its inputs.
// An inventory build needed hand-written scripts for this (75,228 cells over
// four scenarios); a reconciliation is that, as configuration.
//
// A Map is rules, not cells: "rows 10–33, columns F–AN are these metrics,
// the SKU in column D, the location in E, the scenario the case's", or
// "24 blocks of 26 rows: rows 1–23 of each block are these metrics, the item
// from the block's title, the week from the date in row 5 of the column".
package reconcile

import (
	"errors"
	"fmt"
	"strings"

	"github.com/xuri/excelize/v2"
)

// Map is a reconciliation's description of the workbook in model terms.
type Map struct {
	// Tolerance is the relative difference allowed (1e-9 when 0).
	Tolerance float64 `json:"tolerance,omitempty"`
	// BlankTexts are texts the workbook writes for "nothing" ("n.a."),
	// matched by a blank or zero model value.
	BlankTexts []string `json:"blank_texts,omitempty"`
	Rules      []Rule   `json:"rules"`
}

// Rule is one rectangle of the workbook, repeated over blocks when Blocks
// is set.
type Rule struct {
	Name  string `json:"name"`
	Sheet string `json:"sheet"`
	// Rows: a span, or RowList. With Blocks they count from each block's
	// first row (0 is the first).
	Rows    *Span `json:"rows,omitempty"`
	RowList []int `json:"row_list,omitempty"`
	// Blocks repeats the rows: block k starts at First + k × Height.
	Blocks *Blocks `json:"blocks,omitempty"`
	// Columns: a span of letters ("F" to "AE"), or ColumnList.
	Columns    *ColumnSpan `json:"columns,omitempty"`
	ColumnList []string    `json:"column_list,omitempty"`
	// The metric of a cell: Metric for all, MetricsByColumn by column
	// letter, or MetricsByRow in the rows' order.
	Metric          string            `json:"metric,omitempty"`
	MetricsByColumn map[string]string `json:"metrics_by_column,omitempty"`
	MetricsByRow    []string          `json:"metrics_by_row,omitempty"`
	// Members: dimension name → where a cell's member of it comes from. A
	// dimension left out is read at its total; a binding for a dimension a
	// metric does not have is not used for that metric.
	Members map[string]Binding `json:"members,omitempty"`
	// OnlyWhen: a row is compared only when every condition holds.
	OnlyWhen []Condition `json:"only_when,omitempty"`
	// Tolerances per metric, over the map's.
	Tolerances map[string]float64 `json:"tolerances,omitempty"`
}

// Span is an inclusive range of rows.
type Span struct {
	From int `json:"from"`
	To   int `json:"to"`
}

// ColumnSpan is an inclusive range of column letters.
type ColumnSpan struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Blocks repeats a rule's rows.
type Blocks struct {
	First  int `json:"first"`
	Height int `json:"height"`
	Count  int `json:"count"`
}

// Binding says where a cell's member of one dimension comes from: Code (a
// member code or label), Case (the case's member), the cell of Column in
// the cell's row (with BlockRow: that row of the block), the cell of Row in
// the cell's column, or Cell (A1, or Sheet!A1). The value read names the
// member by its code, its label, or — on a dimension of dated periods — a
// date the period holds; Split and Part take one part of it first
// ("SKU001-DC" split "-" part 2 is DC). Total reads the dimension's total.
type Binding struct {
	Code     string `json:"code,omitempty"`
	Case     bool   `json:"case,omitempty"`
	Column   string `json:"column,omitempty"`
	BlockRow *int   `json:"block_row,omitempty"`
	Row      int    `json:"row,omitempty"`
	Cell     string `json:"cell,omitempty"`
	Split    string `json:"split,omitempty"`
	Part     int    `json:"part,omitempty"`
	Total    bool   `json:"total,omitempty"`
}

// Condition is one test of a row: the value in Column against Value or the
// value of Cell, with Op (=, <>, <, <=, >, >=, not_blank).
type Condition struct {
	Column string `json:"column"`
	Op     string `json:"op"`
	Value  any    `json:"value,omitempty"`
	Cell   string `json:"cell,omitempty"`
}

// Case is one calculation of the workbook: its members of the dimensions
// rules bind to the case, and the input edits that make the workbook
// calculate it. A case without edits reads the values the workbook was
// saved with.
type Case struct {
	Name    string            `json:"name"`
	Members map[string]string `json:"members,omitempty"`
	Edits   []Edit            `json:"edits,omitempty"`
}

// Edit sets Cell (Sheet!A1) to Value, or copies the range CopyFrom onto
// Range (same shape).
type Edit struct {
	Cell     string `json:"cell,omitempty"`
	Value    any    `json:"value,omitempty"`
	Range    string `json:"range,omitempty"`
	CopyFrom string `json:"copy_from,omitempty"`
}

// rows is a rule's rows within a block (or absolute without blocks).
func (r Rule) rows() []int {
	if len(r.RowList) > 0 {
		return r.RowList
	}
	if r.Rows == nil {
		return nil
	}
	var out []int
	for i := r.Rows.From; i <= r.Rows.To; i++ {
		out = append(out, i)
	}
	return out
}

// columns is a rule's column numbers (1-based).
func (r Rule) columns() ([]int, error) {
	if len(r.ColumnList) > 0 {
		out := make([]int, 0, len(r.ColumnList))
		for _, c := range r.ColumnList {
			n, err := excelize.ColumnNameToNumber(strings.TrimSpace(c))
			if err != nil {
				return nil, fmt.Errorf("column %q: %w", c, err)
			}
			out = append(out, n)
		}
		return out, nil
	}
	if r.Columns == nil {
		var out []int
		for c := range r.MetricsByColumn {
			n, err := excelize.ColumnNameToNumber(strings.TrimSpace(c))
			if err != nil {
				return nil, fmt.Errorf("column %q: %w", c, err)
			}
			out = append(out, n)
		}
		return sortedInts(out), nil
	}
	from, err := excelize.ColumnNameToNumber(strings.TrimSpace(r.Columns.From))
	if err != nil {
		return nil, fmt.Errorf("columns.from: %w", err)
	}
	to, err := excelize.ColumnNameToNumber(strings.TrimSpace(r.Columns.To))
	if err != nil {
		return nil, fmt.Errorf("columns.to: %w", err)
	}
	var out []int
	for c := from; c <= to; c++ {
		out = append(out, c)
	}
	return out, nil
}

// Validate checks a map's shape; names are checked against the model when
// it runs.
func (m Map) Validate() error {
	if len(m.Rules) == 0 {
		return errors.New("the map has no rules")
	}
	for i, r := range m.Rules {
		label := fmt.Sprintf("rule %d (%s)", i+1, r.Name)
		if strings.TrimSpace(r.Sheet) == "" {
			return fmt.Errorf("%s: sheet is required", label)
		}
		rows := r.rows()
		if len(rows) == 0 {
			return fmt.Errorf("%s: rows (from, to) or row_list is required", label)
		}
		if len(rows) > 100000 {
			return fmt.Errorf("%s: too many rows", label)
		}
		if r.Blocks != nil && (r.Blocks.Height < 1 || r.Blocks.Count < 1 || r.Blocks.First < 1) {
			return fmt.Errorf("%s: blocks need first, height and count of at least 1", label)
		}
		cols, err := r.columns()
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		if len(cols) == 0 {
			return fmt.Errorf("%s: columns (from, to), column_list or metrics_by_column is required", label)
		}
		n := 0
		if r.Metric != "" {
			n++
		}
		if len(r.MetricsByColumn) > 0 {
			n++
		}
		if len(r.MetricsByRow) > 0 {
			n++
			if len(r.MetricsByRow) != len(rows) {
				return fmt.Errorf("%s: metrics_by_row names %d metrics for %d rows", label, len(r.MetricsByRow), len(rows))
			}
		}
		if n != 1 {
			return fmt.Errorf("%s: give exactly one of metric, metrics_by_column and metrics_by_row", label)
		}
		for dim, b := range r.Members {
			set := 0
			for _, on := range []bool{b.Code != "", b.Case, b.Column != "", b.Row > 0, b.Cell != "", b.Total} {
				if on {
					set++
				}
			}
			if set != 1 {
				return fmt.Errorf("%s: member %s needs exactly one of code, case, column, row, cell, total", label, dim)
			}
			if b.BlockRow != nil && (b.Column == "" || r.Blocks == nil) {
				return fmt.Errorf("%s: member %s: block_row goes with column, in a rule with blocks", label, dim)
			}
		}
		for _, c := range r.OnlyWhen {
			switch c.Op {
			case "=", "<>", "<", "<=", ">", ">=", "not_blank":
			default:
				return fmt.Errorf("%s: only_when op %q (use =, <>, <, <=, >, >=, not_blank)", label, c.Op)
			}
			if strings.TrimSpace(c.Column) == "" {
				return fmt.Errorf("%s: only_when needs a column", label)
			}
		}
	}
	return nil
}

// ValidateCases checks the cases.
func ValidateCases(cases []Case) error {
	seen := map[string]bool{}
	for i, c := range cases {
		name := strings.TrimSpace(c.Name)
		if name == "" {
			return fmt.Errorf("case %d: a name is required", i+1)
		}
		if seen[strings.ToLower(name)] {
			return fmt.Errorf("case %q is listed twice", name)
		}
		seen[strings.ToLower(name)] = true
		for _, e := range c.Edits {
			if (e.Cell == "") == (e.Range == "") {
				return fmt.Errorf("case %q: an edit sets a cell, or copies copy_from onto a range", name)
			}
			if e.Range != "" && e.CopyFrom == "" {
				return fmt.Errorf("case %q: an edit of range %s needs copy_from", name, e.Range)
			}
		}
	}
	return nil
}

func sortedInts(v []int) []int {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j-1] > v[j]; j-- {
			v[j-1], v[j] = v[j], v[j-1]
		}
	}
	return v
}

package crudapp

// A posting's row filter (migration 161): the conditions on a record's
// fields a record meets to post through the posting. A register keeping
// several measures in one table (a code column naming each row's measure)
// posts each measure into its own metric, as the workbook's SUMIFS on the
// code column reads it.

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/mavericks-engine/mavericks/pkg/dbx"
)

// RowCondition is one condition: the field holds one of Is, or none of
// IsNot. Values compare as text, ignoring case and surrounding blanks.
type RowCondition struct {
	Field string   `json:"field"`
	Is    []string `json:"is,omitempty"`
	IsNot []string `json:"is_not,omitempty"`
}

// RowFilterMatches reports whether data meets every condition (an empty
// filter: every record does).
func RowFilterMatches(filter []RowCondition, data map[string]any) bool {
	for _, c := range filter {
		v := ""
		if raw, ok := data[c.Field]; ok && raw != nil {
			v = strings.TrimSpace(fmt.Sprint(raw))
		}
		in := func(list []string) bool {
			return slices.ContainsFunc(list, func(x string) bool { return strings.EqualFold(strings.TrimSpace(x), v) })
		}
		if len(c.Is) > 0 && !in(c.Is) {
			return false
		}
		if len(c.IsNot) > 0 && in(c.IsNot) {
			return false
		}
	}
	return true
}

// checkRowFilter says what of filter a table with fields cannot hold: a
// field it lacks, a condition with neither or both lists, or — on a
// dimension field — a value that is no member of its dimension.
func checkRowFilter(ctx context.Context, db dbx.DB, fields []FormField, filter []RowCondition) error {
	for i, c := range filter {
		at := slices.IndexFunc(fields, func(f FormField) bool { return f.Name == c.Field })
		if at < 0 {
			return refErr("row_filter %d names %q, which is no field of the table", i+1, c.Field)
		}
		if (len(c.Is) == 0) == (len(c.IsNot) == 0) {
			return refErr("row_filter %d (%s) needs \"is\" or \"is_not\": the values the field holds (or must not hold) for a record to post", i+1, c.Field)
		}
		f := fields[at]
		if f.Type != "dimension" || f.DimensionID == "" {
			continue
		}
		for _, v := range append(append([]string{}, c.Is...), c.IsNot...) {
			var ok bool
			if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM model.dimension_member WHERE dimension_id = $1::uuid AND lower(code) = lower($2))`,
				f.DimensionID, strings.TrimSpace(v)).Scan(&ok); err != nil {
				return err
			}
			if !ok {
				return refErr("row_filter %d: %q is no member code of %s's dimension", i+1, v, firstNonEmptyLabel(f))
			}
		}
	}
	return nil
}

func firstNonEmptyLabel(f FormField) string {
	if f.Label != "" {
		return f.Label
	}
	return f.Name
}

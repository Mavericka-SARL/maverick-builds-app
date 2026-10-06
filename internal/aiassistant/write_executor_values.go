package aiassistant

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/importpkg"
)

// ValuesWriteRequest is a write_input_values step resolved to the import
// pipeline's shape: one row per value, the grid's dimensions and the metric
// as columns, so it goes through the checks a typed cell and an imported file
// go through (write guard, plan limits, recalculation, audit).
type ValuesWriteRequest struct {
	RevisionID string
	GridID     string
	MetricID   string
	Header     []string // the grid's dimension names, then the metric's name
	Rows       []importpkg.RawRow
	// ValuesArePercentUnits says values under 1 into a Percentage metric
	// really are percents under 1%, not fractions (the plan check's guard).
	ValuesArePercentUnits bool
	// Clears are the cells a "value": null empties, as the console's and
	// the API's clear does: each {dimension id: member code}.
	Clears []map[string]string
}

// maxValuesPerStep bounds one step: a block larger than this belongs in a
// file (import_file_data), where it can be previewed.
const maxValuesPerStep = 500

// writeInputValues writes values into an input metric, as a developer types
// them into its grid (POST /api/cells): a setting such as Actual Through
// Month = 9, a rate, a one-off driver. Each value names a leaf member of every
// dimension of the metric's grid.
func (e *WriteExecutor) writeInputValues(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		MetricID              string `json:"metric_id"`
		ValuesArePercentUnits bool   `json:"values_are_percent_units"`
		Values                []struct {
			Members map[string]string `json:"members"`
			// Value is a number, or text: for a pick-list the member it
			// holds (code or label), for a text metric its note.
			Value json.RawMessage `json:"value"`
		} `json:"values"`
	}
	if err := decodeParams(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	if strings.TrimSpace(p.MetricID) == "" || len(p.Values) == 0 {
		return "", "", fmt.Errorf(`metric_id and values are required: {"metric_id", "values": [{"members": {"<dimension>": "<member code>"}, "value": 9}]}`)
	}
	if len(p.Values) > maxValuesPerStep {
		return "", "", fmt.Errorf("%d values in one step; at most %d — load a larger block from a file (import_file_data)", len(p.Values), maxValuesPerStep)
	}
	if e.hooks.WriteValues == nil {
		return "", "", fmt.Errorf("writing values is not available here")
	}
	metricID, err := e.requireInModel(ctx, "metric", p.MetricID)
	if err != nil {
		return "", "", err
	}
	var metricName string
	var isInput bool
	if err := e.pool.QueryRow(ctx, `SELECT name, is_input FROM model.metric_def WHERE id=$1::uuid`, metricID).Scan(&metricName, &isInput); err != nil {
		return "", "", fmt.Errorf("load metric: %w", err)
	}
	if !isInput {
		return "", "", fmt.Errorf("%s is calculated: only an input metric holds values (change the formula instead)", metricName)
	}
	var gridID string
	if err := e.pool.QueryRow(ctx, `SELECT grid_id::text FROM model.grid_metric WHERE metric_id=$1::uuid LIMIT 1`, metricID).Scan(&gridID); err != nil {
		return "", "", fmt.Errorf("%s is on no grid: add it to one (add_grid_metric) before writing values", metricName)
	}
	type dim struct{ id, name string }
	var dims []dim
	rows, err := e.pool.Query(ctx, `
		SELECT d.id::text, d.name FROM model.grid_dimension gd JOIN model.dimension_def d ON d.id = gd.dimension_id
		WHERE gd.grid_id=$1::uuid ORDER BY d.name`, gridID)
	if err != nil {
		return "", "", err
	}
	for rows.Next() {
		var d dim
		if err := rows.Scan(&d.id, &d.name); err != nil {
			rows.Close()
			return "", "", err
		}
		dims = append(dims, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	dimNames := make([]string, len(dims))
	for i, d := range dims {
		dimNames[i] = d.name
	}

	req := ValuesWriteRequest{RevisionID: e.revID, GridID: gridID, MetricID: metricID, Header: append(append([]string{}, dimNames...), metricName),
		ValuesArePercentUnits: p.ValuesArePercentUnits}
	for i, v := range p.Values {
		clear := isNullValue(v.Value)
		text := ""
		if !clear {
			t, err := valueText(v.Value)
			if err != nil {
				return "", "", fmt.Errorf("values[%d]: %w", i, err)
			}
			text = t
		}
		cells := map[string]string{metricName: text}
		byID := map[string]string{}
		for key, code := range v.Members {
			d, ok := matchDim(dims, key, func(d dim) (string, string) { return d.id, d.name })
			if !ok {
				return "", "", fmt.Errorf("values[%d] names %q, which is not a dimension of %s's grid — its dimensions are %s",
					i, key, metricName, listOrNone(dimNames))
			}
			if err := e.requireLeafMember(ctx, d.id, d.name, code); err != nil {
				return "", "", fmt.Errorf("values[%d]: %w", i, err)
			}
			cells[d.name] = code
			byID[d.id] = code
		}
		for _, d := range dims {
			if _, ok := cells[d.name]; !ok {
				return "", "", fmt.Errorf("values[%d] names no member of %s: a value goes on one member of every dimension of %s's grid (%s)",
					i, d.name, metricName, listOrNone(dimNames))
			}
		}
		if clear {
			req.Clears = append(req.Clears, byID)
			continue
		}
		req.Rows = append(req.Rows, importpkg.RawRow{RowNumber: i + 1, Cells: cells})
	}
	result, err := e.hooks.WriteValues(ctx, req)
	if err != nil {
		return "", "", err
	}
	return result, "", nil
}

// isNullValue reports a "value": null, which empties the cell.
func isNullValue(raw json.RawMessage) bool {
	return strings.TrimSpace(string(raw)) == "null"
}

// valueText is a write_input_values value as the import pipeline reads a
// cell: a number in full, or text — a pick-list's member by code or label,
// a text metric's note.
func valueText(raw json.RawMessage) (string, error) {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" {
		return "", fmt.Errorf("no value")
	}
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		return strconv.FormatFloat(n, 'f', -1, 64), nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil && strings.TrimSpace(s) != "" {
		return strings.TrimSpace(s), nil
	}
	return "", fmt.Errorf("value must be a number, or text: a pick-list metric's member (code or label), a text metric's note")
}

// matchDim finds a dimension by id or by name, ignoring case.
func matchDim[T any](dims []T, key string, idName func(T) (string, string)) (T, bool) {
	for _, d := range dims {
		id, name := idName(d)
		if id == key || strings.EqualFold(name, strings.TrimSpace(key)) {
			return d, true
		}
	}
	var zero T
	return zero, false
}

// requireLeafMember refuses a member code the dimension lacks, or one with
// members under it: a value is written on a leaf, and totals add up from there.
func (e *WriteExecutor) requireLeafMember(ctx context.Context, dimID, dimName, code string) error {
	var memberID string
	if err := e.pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`, dimID, code).Scan(&memberID); err != nil {
		return fmt.Errorf("%s has no member %q (list_dimensions shows the codes)", dimName, code)
	}
	var calculated bool
	_ = e.pool.QueryRow(ctx, `SELECT NULLIF(btrim(formula),'') IS NOT NULL FROM model.dimension_member WHERE id=$1::uuid`, memberID).Scan(&calculated)
	if calculated {
		return fmt.Errorf("%s %q is a calculated member: its values are computed from the other members, so it takes no input", dimName, code)
	}
	var parent bool
	_ = e.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model.dimension_member WHERE parent_member_id=$1::uuid)`, memberID).Scan(&parent)
	if parent {
		return fmt.Errorf("%s %q has members under it: a value goes on a leaf member, and %q adds up from them", dimName, code, code)
	}
	return nil
}

func listOrNone(names []string) string {
	if len(names) == 0 {
		return "none (one value, with no members)"
	}
	return strings.Join(names, ", ")
}

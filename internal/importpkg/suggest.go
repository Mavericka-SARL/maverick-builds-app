package importpkg

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var isoPeriod = regexp.MustCompile(`^\d{4}-(0[1-9]|1[0-2])$`)

// SuggestReshape reads a sheet as it was read (header row included) and
// proposes the reshape a layout made for people needs, or nil when the sheet
// already reads as one header row over data rows. It finds the header row
// under any title rows — the first row of several text cells followed by a
// row holding a number — and, when three or more of its columns are months
// ("Jan", "January") or periods ("2026-01"), turns them into rows of a
// "Period" column with the amounts in "Value". The AI Developer, given a
// sheet with its header on row 4 and months across, mapped every month
// column to the same metric instead; the preview now hands it the reshape.
func SuggestReshape(grid [][]string) *Reshape {
	headerIdx := -1
	for i := 0; i < len(grid) && i < 30; i++ {
		if textCells(grid[i]) < 2 {
			continue
		}
		for j := i + 1; j < len(grid) && j < i+4; j++ {
			if numberCells(grid[j]) > 0 {
				headerIdx = i
				break
			}
			if nonEmpty(grid[j]) > 0 {
				break
			}
		}
		if headerIdx >= 0 {
			break
		}
	}
	if headerIdx < 0 {
		return nil
	}
	var periods []string
	for _, cell := range grid[headerIdx] {
		c := strings.TrimSpace(cell)
		if monthNumber(c) > 0 || isoPeriod.MatchString(c) {
			periods = append(periods, c)
		}
	}
	r := &Reshape{}
	if headerIdx > 0 {
		r.HeaderRow = headerIdx + 1
	}
	if len(periods) >= 3 {
		r.Unpivot = &Unpivot{Columns: periods, NameColumn: "Period", ValueColumn: "Value"}
	}
	if r.HeaderRow == 0 && r.Unpivot == nil {
		return nil
	}
	return r
}

func nonEmpty(row []string) int {
	n := 0
	for _, c := range row {
		if strings.TrimSpace(c) != "" {
			n++
		}
	}
	return n
}

func numberCells(row []string) int {
	n := 0
	for _, c := range row {
		if _, err := strconv.ParseFloat(strings.TrimSpace(c), 64); err == nil {
			n++
		}
	}
	return n
}

func textCells(row []string) int {
	n := 0
	for _, c := range row {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		if _, err := strconv.ParseFloat(c, 64); err != nil {
			n++
		}
	}
	return n
}

// CheckReshapeKeys refuses a reshape naming a key no step reads. A key in
// the wrong place was ignored without a word: the AI Developer wrote
// {"unpivot": {"scale": {"Value": 100}, …}}, its previews reported clean,
// and the planning percentages imported unscaled.
func CheckReshapeKeys(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var r Reshape
	if err := dec.Decode(&r); err != nil {
		msg := err.Error()
		if i := strings.Index(msg, "unknown field "); i >= 0 {
			return fmt.Errorf("reshape has no %s here — the steps are header_row, fill_down, skip_rows, unpivot {columns|from,to, name_column, value_column}, constants, value_map, number_columns, decimal_comma and scale, each at the top level of reshape",
				msg[i:])
		}
		return fmt.Errorf("reshape: %w", err)
	}
	return nil
}

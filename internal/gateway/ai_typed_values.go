package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/xuri/excelize/v2"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// workbookNumbers are the numeric cell values of the attached workbooks
// (raw, as stored), for telling a typed value from a loaded one.
func workbookNumbers(workbooks map[string][]byte) map[float64]bool {
	out := map[float64]bool{}
	for _, raw := range workbooks {
		f, err := excelize.OpenReader(bytes.NewReader(raw))
		if err != nil {
			continue
		}
		for _, sh := range f.GetSheetList() {
			rows, err := f.GetRows(sh, excelize.Options{RawCellValue: true})
			if err != nil {
				continue
			}
			for _, r := range rows {
				for _, c := range r {
					if v, err := strconv.ParseFloat(strings.TrimSpace(c), 64); err == nil && v != 0 {
						out[roundKey(v)] = true
					}
				}
			}
		}
		_ = f.Close()
	}
	return out
}

func roundKey(v float64) float64 { return math.Round(v*1e9) / 1e9 }

// typedWorkbookValueWarnings names each write_input_values step that types
// values the attached workbook already holds (owner, 2026-11-12: use the
// bulk tools — inputs are loaded, never retyped; gpt-5-mini typed 19 input
// steps of a three-sheet workbook and got its revenue wrong). A request
// asking for sample or test values is exempt; zero is never counted.
func typedWorkbookValueWarnings(steps []aiassistant.ProposalStep, request string, numbers map[float64]bool) []string {
	if len(numbers) == 0 {
		return nil
	}
	low := strings.ToLower(request)
	if strings.Contains(low, "sample") || strings.Contains(low, "test data") || strings.Contains(low, "dummy") {
		return nil
	}
	var out []string
	for i, step := range steps {
		if step.Tool != "write_input_values" {
			continue
		}
		var p struct {
			MetricID string `json:"metric_id"`
			Values   []struct {
				Value json.RawMessage `json:"value"`
			} `json:"values"`
		}
		if json.Unmarshal(step.Params, &p) != nil {
			continue
		}
		var n, hit int
		var shown []string
		for _, v := range p.Values {
			x, err := strconv.ParseFloat(strings.Trim(strings.TrimSpace(string(v.Value)), `"`), 64)
			if err != nil || x == 0 {
				continue
			}
			n++
			if numbers[roundKey(x)] || numbers[roundKey(x/100)] {
				hit++
				if len(shown) < 3 {
					shown = append(shown, strconv.FormatFloat(x, 'f', -1, 64))
				}
			}
		}
		if n >= 2 && hit*2 >= n {
			out = append(out, fmt.Sprintf("step %d (write_input_values %s) types %d of %d values the attached workbook holds (%s): load them from their sheet "+
				"(load_sheet, or import_file_data on the workbook) so they stay tied to their rows and are compared; type a value only where the workbook has none",
				i+1, p.MetricID, hit, n, strings.Join(shown, ", ")))
		}
	}
	return out
}

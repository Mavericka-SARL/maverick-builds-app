package importpkg

import (
	"reflect"
	"strings"
	"testing"
)

// A finance sheet: a title, a note, a blank line, then the header with the
// months across — the layout the AI Developer could not shape by itself.
func TestSuggestReshape(t *testing.T) {
	months := []string{"Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"}
	header := append(append([]string{"Region", "Product"}, months...), "FY", "Source / Note")
	sheet := [][]string{
		{"Prior-Year Revenue by Region × Product"},
		{"Replace sample inputs with company data."},
		{},
		header,
		{"North America", "Snacks", "12.5", "11.9", "13.1", "", "", "", "", "", "", "", "", "", "37.5", "note"},
	}
	got := SuggestReshape(sheet)
	if got == nil || got.HeaderRow != 4 || got.Unpivot == nil || !reflect.DeepEqual(got.Unpivot.Columns, months) ||
		got.Unpivot.NameColumn != "Period" || got.Unpivot.ValueColumn != "Value" {
		t.Fatalf("SuggestReshape = %+v (unpivot %+v), want header_row 4 and the twelve months into Period/Value", got, got.Unpivot)
	}
	if err := got.Validate(); err != nil {
		t.Errorf("the suggestion must be a valid reshape: %v", err)
	}

	// Periods as codes, header already on row 1: only the unpivot.
	if got := SuggestReshape([][]string{{"Account", "2026-01", "2026-02", "2026-03"}, {"Rent", "1", "2", "3"}}); got == nil || got.HeaderRow != 0 || got.Unpivot == nil {
		t.Errorf("period codes across: %+v", got)
	}
	// Already long: nothing to suggest.
	if got := SuggestReshape([][]string{{"Region", "Month", "Amount"}, {"EU", "2026-01", "5"}}); got != nil {
		t.Errorf("a long sheet needs no reshape, got %+v", got)
	}
}

// "scale" inside "unpivot" was ignored and the import ran unscaled.
func TestCheckReshapeKeys(t *testing.T) {
	if err := CheckReshapeKeys([]byte(`{"header_row": 4, "unpivot": {"columns": ["Jan"], "name_column": "P", "value_column": "V"}, "scale": {"V": 100}}`)); err != nil {
		t.Errorf("a well-formed reshape refused: %v", err)
	}
	err := CheckReshapeKeys([]byte(`{"unpivot": {"scale": {"Value": 100}, "columns": ["Jan"], "name_column": "P", "value_column": "V"}}`))
	if err == nil || !strings.Contains(err.Error(), `"scale"`) {
		t.Errorf("scale nested in unpivot: %v, want a refusal naming scale", err)
	}
}

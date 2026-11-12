package importpkg

import (
	"strings"
	"testing"
)

// A reshape that drops data rows says how many and by which filter: the GL
// loader that skipped account 6450 reported every row imported.
func TestShapeGridStatsCountsSkippedRows(t *testing.T) {
	grid := [][]string{
		{"GL code", "Amount"},
		{"4000", "10"},
		{"6450", "5"},
		{"Total", "15"},
	}
	r := &Reshape{SkipRows: []RowFilter{{Column: "GL code", Equals: "6450"}, {Column: "GL code", Contains: "total"}}}
	_, rows, st, err := ShapeGridStats(grid, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || st.SkippedRows != 2 {
		t.Fatalf("rows %d, skipped %d; want 1 and 2", len(rows), st.SkippedRows)
	}
	note := st.Note()
	if !strings.Contains(note, `GL code = "6450": 1`) || !strings.Contains(note, `GL code contains "total": 1`) {
		t.Fatalf("note = %q", note)
	}
	if _, _, st, _ := ShapeGridStats(grid, nil); st.Note() != "" {
		t.Fatalf("no reshape, note %q", st.Note())
	}
}

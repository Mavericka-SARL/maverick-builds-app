package manuals

import (
	"strings"
	"testing"
)

func TestManualsSplitAndSearch(t *testing.T) {
	load()
	if len(sections) < 40 {
		t.Fatalf("only %d sections read from the two manuals", len(sections))
	}
	for _, s := range sections {
		if strings.Contains(s.Text, "<") && strings.Contains(s.Text, "</") {
			t.Fatalf("section %q kept markup: %.200s", s.Title, s.Text)
		}
	}
	hits := Search("SFTP file integration schedule", "developer", 6000)
	if len(hits) == 0 || !strings.Contains(strings.ToLower(hits[0].Text), "sftp") {
		t.Fatalf("no SFTP section found: %+v", hits)
	}
	if got := Search("LOOKUP", "formulas", 6000); len(got) == 0 || got[0].Manual != "formulas" {
		t.Fatalf("LOOKUP not found in the formulas manual: %+v", got)
	}
	if !strings.Contains(Contents(""), "formulas manual") {
		t.Fatal("contents lack the formulas manual")
	}
}

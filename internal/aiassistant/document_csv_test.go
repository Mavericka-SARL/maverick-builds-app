package aiassistant

import (
	"fmt"
	"strings"
	"testing"
)

func csvOf(rows int) string {
	var sb strings.Builder
	sb.WriteString("sku,node,qty\r\n")
	for i := 1; i <= rows; i++ {
		fmt.Fprintf(&sb, "S%d,N1,%d\r\n", i, i)
	}
	return sb.String()
}

func TestCSVOverviewShowsFirstLinesAndCount(t *testing.T) {
	big := CSVOverview(csvOf(140000))
	if strings.Count(big, "\n") != csvOverviewLines || !strings.Contains(big, "140001 lines in all") {
		t.Fatalf("overview of a large file:\n%s", big)
	}
	if strings.Contains(big, "\r") || !strings.HasPrefix(big, "sku,node,qty\nS1,N1,1\n") {
		t.Fatalf("overview keeps line ends or loses the header:\n%s", big[:60])
	}
	if small := CSVOverview(csvOf(3)); small != "sku,node,qty\nS1,N1,1\nS2,N1,2\nS3,N1,3" {
		t.Fatalf("a small file is shown whole: %q", small)
	}
	// A file attached before the overview: its first 60,000 characters.
	old := csvOf(5000)[:60000]
	if clipped := ClipCSVOverview(old); strings.Count(clipped, "\n") != csvOverviewLines {
		t.Fatalf("stored text not clipped:\n%s", clipped)
	}
	if again := CSVOverview(csvOf(140000)); ClipCSVOverview(again) != again {
		t.Fatal("an overview is clipped again")
	}
}

func TestExtractCSVLinesReadsAnyLines(t *testing.T) {
	data := []byte(csvOf(1200))
	out, err := ExtractCSVLines(data, 1000, 1002)
	if err != nil {
		t.Fatal(err)
	}
	want := "1201 lines in all, line 1 the header. Lines 1000–1002:\n1: sku,node,qty\n1000: S999,N1,999\n1001: S1000,N1,1000\n1002: S1001,N1,1001\n(more lines: from_row 1003 continues)\n"
	if out != want {
		t.Fatalf("got\n%s", out)
	}
	all, _ := ExtractCSVLines(data, 0, 0)
	if !strings.Contains(all, "Lines 1–500:") || !strings.Contains(all, "from_row 501 continues") {
		t.Fatalf("a call reads at most %d lines:\n%s", maxSheetRows, all[:80])
	}
	if _, err := ExtractCSVLines(data, 5000, 0); err == nil {
		t.Fatal("a from_row past the file is not refused")
	}
}

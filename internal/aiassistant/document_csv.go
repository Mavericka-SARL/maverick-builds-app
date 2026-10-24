package aiassistant

import (
	"fmt"
	"strings"
)

// An attached CSV file is kept whole (raw_data) for the tools that import
// it, so the system prompt needs only its header and first lines. It used
// to carry the file's first 60,000 characters on every model call of every
// turn: six attached files filled the 120,000-character document budget
// with rows no step needed (a tester's sessions cost $23, 2026-10-24).

// csvOverviewLines is how many lines of an attached CSV file the system
// prompt shows: the header and the first rows.
const csvOverviewLines = 31

// csvLines splits CSV text into its lines, without line ends.
func csvLines(text string) []string {
	lines := strings.Split(strings.TrimRight(text, "\r\n"), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

// CSVOverview is the system prompt's text of an attached CSV file: its
// first csvOverviewLines lines and, past them, how many there are.
func CSVOverview(text string) string {
	lines := csvLines(text)
	if len(lines) <= csvOverviewLines {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[:csvOverviewLines], "\n") + fmt.Sprintf(
		"\n(… %d lines in all, the first a header: only the first %d are shown — read_attached_sheet reads any lines, import_file_data imports the whole file)",
		len(lines), csvOverviewLines)
}

// ClipCSVOverview cuts a CSV document's stored text to its first lines, for
// a file attached before CSVOverview: its text is the file's first 60,000
// characters.
func ClipCSVOverview(text string) string {
	lines := csvLines(text)
	if len(lines) <= csvOverviewLines+1 {
		return text
	}
	return strings.Join(lines[:csvOverviewLines], "\n") + fmt.Sprintf(
		"\n(… only the first %d lines are shown — read_attached_sheet reads any lines and says how many there are, import_file_data imports the whole file)",
		csvOverviewLines)
}

// ExtractCSVLines renders lines from..to (1-based, line 1 the header) of an
// attached CSV file as they are, for read_attached_sheet.
func ExtractCSVLines(data []byte, from, to int) (string, error) {
	lines := csvLines(string(data))
	if from <= 0 {
		from = 1
	}
	if to <= 0 || to > len(lines) {
		to = len(lines)
	}
	if from > to {
		return "", fmt.Errorf("the file has %d lines; from_row %d is past them", len(lines), from)
	}
	if to-from+1 > maxSheetRows {
		to = from + maxSheetRows - 1
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d lines in all, line 1 the header. Lines %d–%d:\n", len(lines), from, to)
	if from > 1 {
		fmt.Fprintf(&sb, "1: %s\n", lines[0])
	}
	for i := from; i <= to; i++ {
		fmt.Fprintf(&sb, "%d: %s\n", i, lines[i-1])
	}
	if to < len(lines) {
		fmt.Fprintf(&sb, "(more lines: from_row %d continues)\n", to+1)
	}
	out := sb.String()
	if len(out) > maxSheetReadChars {
		out = out[:maxSheetReadChars] + "\n(... cut: read fewer lines with from_row/to_row)"
	}
	return out, nil
}

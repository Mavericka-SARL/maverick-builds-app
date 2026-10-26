package importpkg

import (
	"strings"
	"testing"
)

// A file posted as it is cannot name one column twice: the second copy
// replaced the first in every row (2026-10-19). A blank header repeats
// freely, and case or spaces do not make two names different.
func TestParsedFilesRefuseRepeatedColumns(t *testing.T) {
	for _, c := range []struct {
		csv, want string
	}{
		{"region,revenue,revenue\nDE,1,2\n", `two columns named "revenue"`},
		{"region,Revenue, revenue \nDE,1,2\n", `two columns named "revenue"`},
		{"region,revenue,,\nDE,1,,\n", ""},
	} {
		_, _, err := ParseCSVRows([]byte(c.csv))
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%q: %v, want no error", c.csv, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%q: %v, want %q", c.csv, err, c.want)
		}
	}
	if err := RefuseRepeatedColumns([]string{"a", "b", "A"}); err == nil {
		t.Error("an XLSX header naming a column twice was accepted")
	}
}

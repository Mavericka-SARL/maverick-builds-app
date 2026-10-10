package modeledit

import (
	"strings"
	"testing"
)

func TestMemberCodeBase(t *testing.T) {
	cases := map[string]string{
		"North":                    "NORTH",
		"North America":            "NORTH_AMER",
		"Lipitor 10 mg tablets":    "LIPITOR_10",
		"Research and Development": "RESEARCH_A",
		"  r&d — ops ":             "R_D_OPS",
		"Marketing ":               "MARKETING",
		"Abcdefghi jklm":           "ABCDEFGHI",
		"2027":                     "2027",
		"Производительность в сутки": "",
	}
	for label, want := range cases {
		got := MemberCodeBase(label)
		if want == "" {
			// No latin letter or digit: a hashed code.
			if !strings.HasPrefix(got, "M_") || len(got) != 10 {
				t.Errorf("MemberCodeBase(%q) = %q, want M_ + 8 hex", label, got)
			}
			continue
		}
		if got != want {
			t.Errorf("MemberCodeBase(%q) = %q, want %q", label, got, want)
		}
	}
}

func TestFreeMemberCode(t *testing.T) {
	taken := map[string]bool{}
	next := func(label string) string {
		code, err := FreeMemberCode(label, func(c string) (bool, error) { return taken[c], nil })
		if err != nil {
			t.Fatal(err)
		}
		taken[code] = true
		return code
	}
	got := []string{
		next("North America East"), next("North America West"), next("North Amer"),
	}
	want := []string{"NORTH_AMER", "NORTH_AM_2", "NORTH_AM_3"}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("code %d = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
	for c := range taken {
		if len(c) > MaxGeneratedCodeLen || strings.ContainsAny(c, " \t") {
			t.Errorf("code %q is longer than %d or has a space", c, MaxGeneratedCodeLen)
		}
	}
}

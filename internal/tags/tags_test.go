package tags

import (
	"slices"
	"testing"
)

func TestClean(t *testing.T) {
	cases := []struct {
		in, want []string
	}{
		{nil, []string{}},
		{[]string{" Cost  Centre ", "cost-centre", "", "  ", "Sales", "sales"}, []string{"cost-centre", "sales"}},
		{[]string{"b", "a", "B"}, []string{"b", "a"}},
	}
	for _, c := range cases {
		got := Clean(c.in)
		if got == nil || !slices.Equal(got, c.want) {
			t.Errorf("Clean(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

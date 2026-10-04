package aiassistant

import (
	"fmt"
	"strings"
	"testing"
)

// The assistant named the import's grid and left target_type out six plans
// in a row; a target that resolves as exactly one kind now settles it.
func TestImportTargetInfersTheKind(t *testing.T) {
	model := map[string]map[string]string{
		"grid":      {"Revenue": "g1", "Both": "g2"},
		"dimension": {"Region": "d1", "Both": "d2"},
	}
	resolve := func(kind, ref string) (string, error) {
		if id, ok := model[kind][ref]; ok {
			return id, nil
		}
		return "", fmt.Errorf("%s %q not found", kind, ref)
	}
	for _, tc := range []struct {
		typ, ref, wantKind, wantID, wantErr string
	}{
		{"", "Revenue", "grid", "g1", ""},
		{"", "Region", "dimension", "d1", ""},
		{"", "Both", "", "", "both a grid and a dimension"},
		{"", "Nothing", "", "", "neither a grid nor a dimension"},
		{"grid", "Revenue", "grid", "g1", ""},
		{"grid", "Region", "", "", "not found"},
		{"metric", "Revenue", "", "", "target_type must be"},
	} {
		kind, id, err := importTarget(tc.typ, tc.ref, resolve)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("(%q, %q): err = %v, want %q", tc.typ, tc.ref, err, tc.wantErr)
			}
			continue
		}
		if err != nil || kind != tc.wantKind || id != tc.wantID {
			t.Errorf("(%q, %q) = %q, %q, %v; want %q, %q", tc.typ, tc.ref, kind, id, err, tc.wantKind, tc.wantID)
		}
	}
}

func TestEditDistanceWithin(t *testing.T) {
	a := "d09acc7d-d423-4d0d-b1b9-7a5fc4f2bc7a"
	for b, want := range map[string]int{
		a:                                      0,
		"d09acc7d-d423-4d0b-b1b9-7a5fc4f2bc7a": 1,
		"d09acc7d-d423-4d0b-b1b9-7a5fc4f2bc7b": 2,
		"d09acc7d-d423-4d0b-b1b9-7a5fc4f2bc00": -1,
		"422183ef-0799-4819-a1f2-a81fb25dc0b7": -1,
	} {
		if got := editDistanceWithin(b, a, 2); got != want {
			t.Errorf("%s: %d, want %d", b, got, want)
		}
	}
}

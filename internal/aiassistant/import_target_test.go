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

// {"hide_rollup_members": true} sent to a chart used to replace its props
// whole, wiping the chart; it now lands under "chart" and the rest stays.
func TestMergeWidgetProps(t *testing.T) {
	stored := []byte(`{"sync_context":true,"chart":{"chart_type":"line","metric_ids":["a","b"],"dimension_id":"d"}}`)
	out, err := mergeWidgetProps("chart", stored, []byte(`{"hide_rollup_members":true,"sync_context":null}`))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`"hide_rollup_members":true`, `"chart_type":"line"`, `"metric_ids":["a","b"]`} {
		if !strings.Contains(s, want) {
			t.Errorf("merged %s lacks %s", s, want)
		}
	}
	if strings.Contains(s, "sync_context") {
		t.Errorf("null should remove sync_context: %s", s)
	}
	out, _ = mergeWidgetProps("metric_kpi", []byte(`{"background":"white"}`), []byte(`{"kpi_context_mode":"total"}`))
	if string(out) != `{"background":"white","kpi_context_mode":"total"}` {
		t.Errorf("kpi merge = %s", out)
	}
	out, _ = mergeWidgetProps("chart", stored, []byte(`{"chart":{"chart_type":"bar"}}`))
	if !strings.Contains(string(out), `"chart_type":"bar"`) || !strings.Contains(string(out), `"metric_ids":["a","b"]`) {
		t.Errorf("a chart patch should keep the chart's other settings: %s", out)
	}
}

func TestEditDistanceWithinCountsAnInsertedCharacter(t *testing.T) {
	if got := editDistanceWithin("43356b28-9344-41f1-acb91-8aeef92ac94a", "43356b28-9344-41f1-acb9-18aeef92ac94a", 2); got < 0 {
		t.Errorf("a character too many should be within two edits, got %d", got)
	}
}

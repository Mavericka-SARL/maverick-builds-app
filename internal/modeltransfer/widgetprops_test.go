package modeltransfer

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Unit coverage for the widget_props ID remap shared by the package-import
// path, the developer-console revision duplicate (handler.go Step J) and the
// AI draft-revision copy. The DB-backed round trips assert the wiring; these
// pin the rewrite rules themselves, including the two easy-to-miss ones:
// context_defaults is keyed BY dimension ID (so the key moves, the value —
// a member code — does not), and unknown props must survive untouched.
func TestRemapWidgetPropsIDs(t *testing.T) {
	metricMap := map[string]string{"m-old": "m-new", "x-old": "x-new", "y-old": "y-new"}
	dimMap := map[string]string{"d-old": "d-new", "ctx-old": "ctx-new"}

	tests := []struct {
		name        string
		in          string
		want        string
		wantChanged bool
	}{
		{
			name:        "chart series and plotted dimension",
			in:          `{"chart":{"chart_type":"bar","dimension_id":"d-old","metric_ids":["m-old","unmapped"]}}`,
			want:        `{"chart":{"chart_type":"bar","dimension_id":"d-new","metric_ids":["m-new","unmapped"]}}`,
			wantChanged: true,
		},
		{
			name:        "scatter x/y metrics",
			in:          `{"chart":{"chart_type":"scatter","dimension_id":"d-old","metric_ids":[],"x_metric_id":"x-old","y_metric_id":"y-old"}}`,
			want:        `{"chart":{"chart_type":"scatter","dimension_id":"d-new","metric_ids":[],"x_metric_id":"x-new","y_metric_id":"y-new"}}`,
			wantChanged: true,
		},
		{
			name:        "context_defaults key is a dimension id, its value is a member code",
			in:          `{"chart":{"dimension_id":"d-old","context_defaults":{"ctx-old":"DEPT_A"}}}`,
			want:        `{"chart":{"dimension_id":"d-new","context_defaults":{"ctx-new":"DEPT_A"}}}`,
			wantChanged: true,
		},
		{
			name:        "kpi scope dimension",
			in:          `{"kpi_scope":{"dimension_id":"d-old","member_code":"DEPT_A"},"color":"#123456"}`,
			want:        `{"kpi_scope":{"dimension_id":"d-new","member_code":"DEPT_A"},"color":"#123456"}`,
			wantChanged: true,
		},
		{
			name:        "unrelated props are preserved and report no change",
			in:          `{"font_size":14,"font_weight":"bold","button_color":"#fff","sync_context":true}`,
			want:        `{"font_size":14,"font_weight":"bold","button_color":"#fff","sync_context":true}`,
			wantChanged: false,
		},
		{
			name:        "numbers alongside a remap keep their exact form",
			in:          `{"chart":{"dimension_id":"d-old","bin_count":12,"refresh_seconds":30},"font_size":14}`,
			want:        `{"chart":{"dimension_id":"d-new","bin_count":12,"refresh_seconds":30},"font_size":14}`,
			wantChanged: true,
		},
		{
			name:        "IDs absent from the maps are left alone",
			in:          `{"chart":{"dimension_id":"stranger","metric_ids":["stranger-2"]}}`,
			want:        `{"chart":{"dimension_id":"stranger","metric_ids":["stranger-2"]}}`,
			wantChanged: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := RemapWidgetPropsIDs([]byte(tc.in), metricMap, dimMap)
			if changed != tc.wantChanged {
				t.Errorf("changed = %v, want %v", changed, tc.wantChanged)
			}
			var gotAny, wantAny any
			if err := json.Unmarshal(got, &gotAny); err != nil {
				t.Fatalf("result is not valid JSON (%s): %v", got, err)
			}
			if err := json.Unmarshal([]byte(tc.want), &wantAny); err != nil {
				t.Fatalf("bad want fixture: %v", err)
			}
			if !reflect.DeepEqual(gotAny, wantAny) {
				t.Errorf("props =\n  %s\nwant\n  %s", got, tc.want)
			}
		})
	}
}

// Degenerate inputs must be passed through rather than turned into "null" or
// an error — widget_props is nullable, and most widgets have no IDs in it.
func TestRemapWidgetPropsIDsPassesThroughDegenerateInput(t *testing.T) {
	maps := map[string]string{"a": "b"}
	for _, in := range []string{"", "null", "{}", "not json", `{"chart":"not an object"}`} {
		got, changed := RemapWidgetPropsIDs([]byte(in), maps, maps)
		if changed {
			t.Errorf("input %q reported changed", in)
		}
		if string(got) != in {
			t.Errorf("input %q was rewritten to %q, want it returned untouched", in, got)
		}
	}
	// A nil map pair is a no-op even for props full of IDs.
	props := []byte(`{"chart":{"dimension_id":"d-old"}}`)
	if got, changed := RemapWidgetPropsIDs(props, nil, nil); changed || string(got) != string(props) {
		t.Errorf("empty maps rewrote props to %q (changed=%v)", got, changed)
	}
}

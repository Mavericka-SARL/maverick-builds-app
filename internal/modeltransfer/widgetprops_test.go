package modeltransfer

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/modeledit"
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
			name:        "kpi comparison dimension and a chart's actuals flag",
			in:          `{"kpi_compare":{"dimension_id":"d-old","member_code":"BUDGET","label":"Budget"},"kpi_good":"up","chart":{"actual_metric_id":"m-old","stacked":true}}`,
			want:        `{"kpi_compare":{"dimension_id":"d-new","member_code":"BUDGET","label":"Budget"},"kpi_good":"up","chart":{"actual_metric_id":"m-new","stacked":true}}`,
			wantChanged: true,
		},
		{
			// show_members moves its keys; the lists of member codes copy
			// verbatim, in order.
			name:        "grid show_members keys are dimension ids",
			in:          `{"show_members":{"d-old":["FY","Q1"],"unmapped":["X"]}}`,
			want:        `{"show_members":{"d-new":["FY","Q1"],"unmapped":["X"]}}`,
			wantChanged: true,
		},
		{
			// A grid widget's saved layout: axis lists keep their order and
			// the "__metrics__" sentinel; filter_sel moves its keys, not its
			// member-code values. Metric IDs never appear here, so a metric
			// map entry that happens to share a dimension's old ID must not
			// be applied.
			name:        "grid default_view axes and filter selection",
			in:          `{"default_view":{"rows":["__metrics__","d-old"],"cols":["ctx-old"],"context":["unmapped"],"filter_sel":{"ctx-old":"DEPT_A","unmapped":"X"}},"sync_context":true}`,
			want:        `{"default_view":{"rows":["__metrics__","d-new"],"cols":["ctx-new"],"context":["unmapped"],"filter_sel":{"ctx-new":"DEPT_A","unmapped":"X"}},"sync_context":true}`,
			wantChanged: true,
		},
		{
			name:        "default_view with nothing to remap reports no change",
			in:          `{"default_view":{"rows":["__metrics__"],"cols":["stranger"],"context":[],"filter_sel":{}}}`,
			want:        `{"default_view":{"rows":["__metrics__"],"cols":["stranger"],"context":[],"filter_sel":{}}}`,
			wantChanged: false,
		},
		{
			name:        "default_view axes are matched against dimensions only",
			in:          `{"default_view":{"rows":["m-old"],"cols":[],"context":[]}}`,
			want:        `{"default_view":{"rows":["m-old"],"cols":[],"context":[]}}`,
			wantChanged: false,
		},
		{
			name:        "a grid widget's chosen metrics keep their order",
			in:          `{"metric_ids":["x-old","unmapped","m-old"],"default_view":{"rows":["__metrics__"],"cols":["d-old"],"context":[]}}`,
			want:        `{"default_view":{"cols":["d-new"],"context":[],"rows":["__metrics__"]},"metric_ids":["x-new","unmapped","m-new"]}`,
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
	for _, in := range []string{"", "null", "{}", "not json", `{"chart":"not an object"}`,
		`{"default_view":"not an object"}`, `{"default_view":{"rows":"a","filter_sel":["a"]}}`} {
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

// widgetPropRefs says what each widget_props key the console reads holds
// (modeledit.WidgetPropKeys, and "chart." + modeledit.ChartSettingKeys): a
// metric or dimension reference, a list of them, an object keyed by them,
// a grid's saved layout, an object naming a dimension (KPI scope and
// comparison), or data. Both walkers of these documents — an import's
// (rewriteRefs) and a revision copy's (RemapWidgetPropsIDs) — must move
// every reference; a key held by neither stranded its references on the
// source model's IDs (chart.series_names, 2026-11-07).
var widgetPropRefs = map[string]string{
	"selectors_position": "data", "background": "data", "font_size": "data", "font_weight": "data",
	"color": "data", "font_family": "data", "alt": "data", "image_fit": "data", "button_color": "data",
	"default_view": "layout", "metric_ids": "metric list", "chart": "chart",
	// A button's static workflow context: values the workflow reads.
	"context":   "data",
	"kpi_scope": "names a dimension", "kpi_context_mode": "data", "confirm_text": "data",
	"sync_context": "data", "show_members": "dimension-keyed", "rows_collapsed": "data", "multi_select": "data",
	"hide_search": "data", "button_corners": "data", "new_record_button": "data",
	// A board's and a records widget's settings name the table's fields.
	"board": "data", "record_filters": "data", "record_columns": "data", "record_sort": "data",
	"kpi_compare": "names a dimension", "kpi_good": "data", "number_scale": "data",
	"hide_total_row": "data", "label_width": "data", "hide_legend": "data", "hide_badges": "data", "footer": "data",

	"chart.chart_type": "data", "chart.dimension_id": "dimension", "chart.metric_ids": "metric list",
	"chart.x_metric_id": "metric", "chart.y_metric_id": "metric", "chart.context_defaults": "dimension-keyed",
	"chart.bin_count": "data", "chart.show_legend": "data", "chart.show_values": "data", "chart.value_format": "data",
	"chart.refresh_seconds": "data", "chart.hide_rollup_members": "data", "chart.y_axis_from_zero": "data",
	"chart.stacked": "data", "chart.actual_metric_id": "metric", "chart.series_names": "metric-keyed",
	// The plotted dimension's member codes.
	"chart.plotted_members": "data",
}

// Every key the console reads is classified, and every reference among
// them is moved by both walkers. A new key holding an id fails here until
// it is classified and both walkers handle it.
func TestWidgetPropKeysAreClassified(t *testing.T) {
	keys := map[string]bool{}
	for k := range modeledit.WidgetPropKeys {
		keys[k] = true
	}
	for k := range modeledit.ChartSettingKeys {
		keys["chart."+k] = true
	}
	for k := range keys {
		if _, ok := widgetPropRefs[k]; !ok {
			t.Errorf("widget_props key %q is not classified in widgetPropRefs: say whether it holds metric or dimension ids, and make both walkers move them", k)
		}
	}
	for k := range widgetPropRefs {
		if !keys[k] {
			t.Errorf("widgetPropRefs classifies %q, which the console does not read", k)
		}
	}

	metricMap := map[string]string{"m-old": "m-new"}
	dimMap := map[string]string{"d-old": "d-new"}
	refs := newImportRefs()
	refs.add("dimension", dimMap)
	refs.add("metric", metricMap)
	for key, kind := range widgetPropRefs {
		var value any
		switch kind {
		case "data", "chart":
			continue
		case "metric":
			value = "m-old"
		case "dimension":
			value = "d-old"
		case "metric list":
			value = []any{"m-old"}
		case "metric-keyed":
			value = map[string]any{"m-old": "Sales"}
		case "dimension-keyed":
			value = map[string]any{"d-old": "N"}
		case "names a dimension":
			value = map[string]any{"dimension_id": "d-old", "member_code": "N"}
		case "layout":
			value = map[string]any{"rows": []any{"d-old"}, "cols": []any{"__metrics__"}, "context": []any{}, "filter_sel": map[string]any{"d-old": "N"}}
		default:
			t.Fatalf("%s: unknown kind %q", key, kind)
		}
		doc := map[string]any{}
		if sub, ok := strings.CutPrefix(key, "chart."); ok {
			doc["chart"] = map[string]any{sub: value}
		} else {
			doc[key] = value
		}
		in, _ := json.Marshal(doc)
		imported := string(refs.doc(in, jsonRefs{}))
		copied, _ := RemapWidgetPropsIDs(in, metricMap, dimMap)
		for walker, out := range map[string]string{"import (rewriteRefs)": imported, "revision copy (RemapWidgetPropsIDs)": string(copied)} {
			if strings.Contains(out, "-old") || !strings.Contains(out, "-new") {
				t.Errorf("%s: %s left %s as %s", key, walker, in, out)
			}
		}
	}
}

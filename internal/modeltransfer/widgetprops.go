package modeltransfer

import (
	"bytes"
	"encoding/json"
)

// RemapWidgetPropsIDs rewrites every metric and dimension ID embedded in a
// dashboard widget's widget_props JSON through the supplied old→new maps and
// reports whether anything changed.
//
// model.dashboard_widget references other entities in two places, not one:
// the ref_id column (which every copy path already remaps) and this JSON
// blob, which carries five more IDs of its own:
//
//	chart.dimension_id        plotted dimension
//	chart.metric_ids[]        series metrics (bar/line/pie/histogram)
//	chart.x_metric_id         scatter X metric
//	chart.y_metric_id         scatter Y metric
//	kpi_scope.dimension_id    dimension a metric_kpi widget is scoped to
//
// plus chart.context_defaults, whose KEYS are dimension IDs (its values are
// member codes, which copy verbatim and need no remap).
//
// Metrics and dimensions are revision-scoped, so every copy — a revision
// duplicate, a package import, a deployment build — gives them new UUIDs.
// Leaving the blob alone strands the chart on the source's IDs: the chart
// endpoint then rejects it outright ("plotted dimension not found in grid"),
// while a stale kpi_scope fails silently, falling back to the metric's
// unscoped grand total and displaying a number for the whole model where the
// widget promised one member.
//
// Unknown keys are preserved untouched (font_size, button_color, default_view,
// sync_context, the automation_button static context …), as are IDs absent
// from the maps — matching the "skip what doesn't resolve" tolerance the rest
// of this package applies to references it cannot map.
func RemapWidgetPropsIDs(props []byte, metricMap, dimMap map[string]string) ([]byte, bool) {
	if len(props) == 0 || len(metricMap)+len(dimMap) == 0 {
		return props, false
	}
	// UseNumber so untouched numeric props (font_size, bin_count, pos …)
	// round-trip through the re-marshal exactly as written rather than
	// through float64.
	dec := json.NewDecoder(bytes.NewReader(props))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil || root == nil {
		return props, false
	}

	changed := false
	remapStr := func(m map[string]string, container map[string]any, key string) {
		old, ok := container[key].(string)
		if !ok || old == "" {
			return
		}
		if next, ok := m[old]; ok && next != old {
			container[key] = next
			changed = true
		}
	}

	if chart, ok := root["chart"].(map[string]any); ok {
		remapStr(dimMap, chart, "dimension_id")
		remapStr(metricMap, chart, "x_metric_id")
		remapStr(metricMap, chart, "y_metric_id")

		if ids, ok := chart["metric_ids"].([]any); ok {
			for i, raw := range ids {
				old, ok := raw.(string)
				if !ok {
					continue
				}
				if next, ok := metricMap[old]; ok && next != old {
					ids[i] = next
					changed = true
				}
			}
		}

		// context_defaults is keyed by dimension ID; rebuild it rather than
		// mutating in place so a remapped key can't collide with one not yet
		// visited.
		if defaults, ok := chart["context_defaults"].(map[string]any); ok && len(defaults) > 0 {
			rebuilt := make(map[string]any, len(defaults))
			for dimID, code := range defaults {
				if next, ok := dimMap[dimID]; ok && next != dimID {
					dimID = next
					changed = true
				}
				rebuilt[dimID] = code
			}
			chart["context_defaults"] = rebuilt
		}
	}

	if scope, ok := root["kpi_scope"].(map[string]any); ok {
		remapStr(dimMap, scope, "dimension_id")
	}

	if !changed {
		return props, false
	}
	out, err := json.Marshal(root)
	if err != nil {
		return props, false
	}
	return out, true
}

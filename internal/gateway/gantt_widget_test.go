package gateway

import (
	"net/http"
	"strings"
	"testing"
)

// A Gantt widget (widget_type "gantt") over a table: saved with a date field
// to start its bars on, refused without one or naming fields the table does
// not have or of the wrong kind — a misnamed field would draw nothing.
func TestGanttWidgetNamesTheTablesFields(t *testing.T) {
	d := setupSalesDemo(t)
	table := idOf(d.call("POST", "/api/forms", d.dev, map[string]any{
		"name": "activities", "label": "Activities", "edit_access": "everyone",
		"fields": []map[string]any{
			{"name": "code", "label": "Activity", "type": "text", "code": true, "core": true},
			{"name": "title", "label": "Title", "type": "text"},
			{"name": "start", "label": "Start", "type": "date"},
			{"name": "finish", "label": "Finish", "type": "date"},
			{"name": "done", "label": "Done %", "type": "number"},
			{"name": "after", "label": "Follows", "type": "text"},
		},
	}))
	dash := idOf(d.call("POST", "/api/developer/dashboards", d.dev, map[string]any{"name": "Timeline", "revision_id": d.revID}))
	add := func(gantt map[string]any) (int, string) {
		body := map[string]any{"widget_type": "gantt", "ref_id": table, "title": "Plan", "pos_x": 0, "pos_y": 0, "size_w": 1100, "size_h": 400,
			"sort_order": 0, "col_start": 1, "col_span": 12}
		if gantt != nil {
			body["widget_props"] = map[string]any{"gantt": gantt}
		}
		st, raw := d.req("POST", "/api/developer/dashboards/"+dash+"/widgets", d.dev, body)
		return st, string(raw)
	}
	for _, c := range []struct {
		gantt map[string]any
		want  string
	}{
		{nil, "needs widget_props.gantt.start_field"},
		{map[string]any{"start_field": "title"}, `start_field \"title\" is not a date field`},
		{map[string]any{"start_field": "start", "end_field": "end"}, `end_field \"end\" is not a date field`},
		{map[string]any{"start_field": "start", "progress_field": "title"}, `progress_field \"title\" is not a number field`},
		{map[string]any{"start_field": "start", "group_field": "project"}, `group_field \"project\" is not a field`},
	} {
		if st, raw := add(c.gantt); st != http.StatusBadRequest || !strings.Contains(raw, c.want) {
			t.Errorf("gantt %v: %d %s, want 400 saying %q", c.gantt, st, raw, c.want)
		}
	}
	if st, raw := add(map[string]any{"start_field": "start", "end_field": "finish", "label_field": "title", "progress_field": "done",
		"predecessor_field": "after", "color_field": "status"}); st != http.StatusOK && st != http.StatusCreated {
		t.Errorf("a Gantt widget naming the table's fields: %d %s", st, raw)
	}
}

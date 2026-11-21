package modeltransfer

import (
	"encoding/json"
	"testing"
)

// TestVersion1TablesKeepTheirStatus: a version 1 package's tables gave
// every record a status, so importing one gives each table the status field
// that keeps it so; a table with a field of its own named "status" gets
// "record_status". A version 2 package, or one naming no version (made by
// hand), is imported as it is.
func TestVersion1TablesKeepTheirStatus(t *testing.T) {
	title := "Claims"
	pkg := Package{Version: 1, FormRecords: []FormRecord{{FormID: "a"}, {FormID: "a", Status: "approved"}}, Dashboards: []Dashboard{{Name: "D", Widgets: []Widget{
		{WidgetType: "form", Title: &title},
		{WidgetType: "text"},
	}}}, Forms: []Form{
		{Name: "a", Fields: json.RawMessage(`[{"name":"amount","label":"Amount","type":"number"}]`)},
		{Name: "b", Fields: json.RawMessage(`[{"name":"status","label":"Status text","type":"text"}]`)},
		{Name: "c", Fields: json.RawMessage(`[{"name":"state","label":"State","type":"status"}]`)},
	}}
	pkg.upgrade()
	if pkg.Version != PackageVersion {
		t.Errorf("version after upgrade = %d", pkg.Version)
	}
	want := map[string]string{"a": "status", "b": "record_status", "c": "state"}
	for _, f := range pkg.Forms {
		var fields []map[string]any
		if err := json.Unmarshal(f.Fields, &fields); err != nil {
			t.Fatal(err)
		}
		var statusFields []string
		for _, fd := range fields {
			if fd["type"] == "status" {
				statusFields = append(statusFields, fd["name"].(string))
			}
		}
		if len(statusFields) != 1 || statusFields[0] != want[f.Name] {
			t.Errorf("table %s: status fields %v, want [%s]", f.Name, statusFields, want[f.Name])
		}
	}

	if props := string(pkg.Dashboards[0].Widgets[0].Props); props != `{"new_record_button":true}` {
		t.Errorf("a version 1 records widget's props = %s, want the button kept", props)
	}
	if props := pkg.Dashboards[0].Widgets[1].Props; props != nil {
		t.Errorf("a text widget was given props: %s", props)
	}

	if pkg.FormRecords[0].Status != "draft" || pkg.FormRecords[1].Status != "approved" {
		t.Errorf("version 1 records' statuses after upgrade = %q, %q; want draft (the old default) and approved", pkg.FormRecords[0].Status, pkg.FormRecords[1].Status)
	}

	v2 := Package{Version: 2, Forms: []Form{{Name: "a", Fields: json.RawMessage(`[{"name":"amount","label":"Amount","type":"number"}]`)}},
		Dimensions: []Dimension{{Name: "scenario", AggRule: "average"}}}
	v2.upgrade()
	if string(v2.Forms[0].Fields) != `[{"name":"amount","label":"Amount","type":"number"}]` {
		t.Errorf("a version 2 table was changed: %s", v2.Forms[0].Fields)
	}
	// Before version 3 a dimension's agg_rule meant nothing (migration 174):
	// it imports as sum, its members adding up as they did.
	if v2.Dimensions[0].AggRule != "sum" || v2.Version != PackageVersion {
		t.Errorf("a version 2 dimension's agg_rule = %q (version %d), want sum", v2.Dimensions[0].AggRule, v2.Version)
	}
	unversioned := Package{Forms: []Form{{Name: "a", Fields: json.RawMessage(`[]`)}}}
	unversioned.upgrade()
	if string(unversioned.Forms[0].Fields) != `[]` {
		t.Errorf("a package naming no version was upgraded: %s", unversioned.Forms[0].Fields)
	}
}

// An export writes version 4 only when a table uses what an engine before
// migrations 192–193 would drop on import (field rules, person fields, the
// delete and record-scope settings); anything else stays version 3, which
// those engines still take. A table's existing_codes alone does not count.
func TestMinimumVersion(t *testing.T) {
	plain := Package{Forms: []Form{{Name: "a", ExistingCodes: "refuse", Fields: json.RawMessage(`[{"name":"x","type":"select","options":["a"],"history":true}]`)}}}
	if v := plain.MinimumVersion(); v != 3 {
		t.Errorf("a package without the new options = version %d, want 3", v)
	}
	for name, f := range map[string]Form{
		"transitions":  {Fields: json.RawMessage(`[{"name":"s","type":"select","transitions":[{"from":["a"],"to":["b"]}]}]`)},
		"stamp_when":   {Fields: json.RawMessage(`[{"name":"d","type":"date","stamp_when":{"field":"s","is":["b"]}}]`)},
		"person":       {Fields: json.RawMessage(`[{"name":"p","type":"person"}]`)},
		"record_scope": {RecordScope: "open", Fields: json.RawMessage(`[]`)},
		"delete":       {DeleteAccess: "admins", Fields: json.RawMessage(`[]`)},
	} {
		if v := (&Package{Forms: []Form{f}}).MinimumVersion(); v != 4 {
			t.Errorf("%s: version %d, want 4", name, v)
		}
	}
}

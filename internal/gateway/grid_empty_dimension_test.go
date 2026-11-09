package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A grid dimension with no member is not shown as a grid of fewer
// dimensions: the grid read lists it apart, saying to add members first (or
// that a table's records are its members), and makes no member for it.
func TestGridReadSaysWhichDimensionsHaveNoMembers(t *testing.T) {
	d := setupSalesDemo(t)
	dim := func(name string) string {
		return idOf(d.call("POST", "/api/developer/dimensions", d.dev, map[string]any{"name": name, "revision_id": d.revID}))
	}
	entity, account := dim("gl_entity"), dim("gl_account")
	d.call("POST", "/api/forms", d.dev, map[string]any{"name": "gl_map", "label": "GL Mapping", "fields": []map[string]any{
		{"name": "code", "label": "Account", "type": "autocode", "code_prefix": "GL-", "code_digits": 4, "core": true},
		{"name": "name", "label": "Name", "type": "text", "required": true},
	}, "keeps_dimension_id": account, "member_label_field": "name"})
	metric := idOf(d.call("POST", "/api/developer/metrics", d.dev, map[string]any{"name": "gl_amount", "is_input": true, "revision_id": d.revID}))
	grid := idOf(d.call("POST", "/api/developer/grids", d.dev, map[string]any{"name": "GL Actuals", "revision_id": d.revID}))
	d.call("POST", "/api/developer/grids/"+grid+"/dimensions/"+entity, d.dev, nil)
	d.call("POST", "/api/developer/grids/"+grid+"/dimensions/"+account, d.dev, nil)
	d.call("POST", "/api/developer/grids/"+grid+"/metrics/"+metric, d.dev, nil)

	read := func() (dims []string, empty map[string]string) {
		t.Helper()
		status, raw := d.req("GET", "/api/grid?grid_def_id="+grid+"&revision_id="+d.revID+"&meta_only=1", d.dev, nil)
		if status != http.StatusOK {
			t.Fatalf("grid read: %d %s", status, raw)
		}
		var g struct {
			Dimensions []struct {
				Name    string `json:"name"`
				Members []any  `json:"members"`
			} `json:"dimensions"`
			Empty []struct {
				Name     string `json:"name"`
				KeptBy   string `json:"kept_by"`
				Guidance string `json:"guidance"`
			} `json:"dimensions_without_members"`
		}
		_ = json.Unmarshal(raw, &g)
		empty = map[string]string{}
		for _, x := range g.Dimensions {
			dims = append(dims, x.Name)
		}
		for _, e := range g.Empty {
			empty[e.Name] = e.Guidance
		}
		return dims, empty
	}
	_, empty := read()
	if !strings.Contains(empty["gl_entity"], "has no members yet: add members first") {
		t.Errorf("gl_entity: %q, want to be told to add members first", empty["gl_entity"])
	}
	if !strings.Contains(empty["gl_account"], "records of the table GL Mapping") {
		t.Errorf("gl_account: %q, want to be told the table's records are its members", empty["gl_account"])
	}
	var members int
	_ = d.pool.QueryRow(t.Context(), `SELECT count(*) FROM model.dimension_member WHERE dimension_id IN ($1::uuid, $2::uuid)`, entity, account).Scan(&members)
	if members != 0 {
		t.Errorf("the read made %d member(s)", members)
	}

	d.call("POST", "/api/developer/dimensions/"+entity+"/members", d.dev, map[string]any{"code": "HQ", "label": "HQ"})
	dims, empty := read()
	if _, still := empty["gl_entity"]; still || !strings.Contains(strings.Join(dims, ","), "gl_entity") {
		t.Errorf("after a member was added: dimensions %v, without members %v", dims, empty)
	}
}

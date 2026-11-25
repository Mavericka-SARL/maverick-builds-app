package gateway

// A dimension field holding several members ("multiple": true, 2026-11-25):
// a deal's contacts. Over HTTP, as the developer and a sales rep: the
// options it refuses; a list saved (by codes or labels, a file's "a; b"
// text, each member once); required asking for one; a filter for a member
// keeping the records whose list holds it, search by a member's label, sort
// and grouping refused; the contact's page listing the deals naming them; a
// board refusing it (a mapping too: crudapp.CheckMappingRefs); deleting a contact blocked, or with
// "clear" taking them off the lists; and a field switched between one and
// several values carrying its records' values along.

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func TestMultipleDimensionField(t *testing.T) {
	d := setupSalesDemo(t)

	dim := func(name string) string {
		return idOf(d.call("POST", "/api/developer/dimensions", d.dev, map[string]any{"name": name, "revision_id": d.revID}))
	}
	contactDim, dealDim := dim("mc_contact"), dim("mc_deal")
	table := func(name string, fields []map[string]any, settings map[string]any) string {
		t.Helper()
		body := map[string]any{"name": name, "label": strings.ToUpper(name[:1]) + name[1:], "fields": fields, "edit_access": "everyone"}
		for k, v := range settings {
			body[k] = v
		}
		return idOf(d.call("POST", "/api/forms", d.dev, body))
	}
	contacts := table("mc_contacts", []map[string]any{
		{"name": "code", "label": "Code", "type": "autocode", "code_prefix": "CON-"},
		{"name": "full_name", "label": "Full Name", "type": "text", "required": true},
	}, map[string]any{"keeps_dimension_id": contactDim, "member_label_field": "full_name"})
	dealFields := func(multiple bool, extra map[string]any) []map[string]any {
		people := map[string]any{"name": "people", "label": "Contacts", "type": "dimension", "dimension_id": contactDim, "multiple": multiple, "on_delete": "clear"}
		for k, v := range extra {
			people[k] = v
		}
		return []map[string]any{
			{"name": "code", "label": "Code", "type": "autocode", "code_prefix": "DEAL-"},
			{"name": "name", "label": "Deal Name", "type": "text", "required": true},
			people,
			{"name": "value", "label": "Value", "type": "number"},
		}
	}

	t.Run("the options a field holding several members refuses", func(t *testing.T) {
		for _, c := range []struct {
			extra map[string]any
			want  string
		}{
			{map[string]any{"on_delete": "cascade"}, "takes a deleted member off its list"},
			{map[string]any{"default": "CON-0001"}, "has no default"},
			{map[string]any{"notify": true}, "notify and roles"},
		} {
			status, raw := d.req("POST", "/api/forms", d.dev, map[string]any{"name": "mc_bad", "label": "Bad", "fields": dealFields(true, c.extra)})
			if status != http.StatusBadRequest || !strings.Contains(string(raw), c.want) {
				t.Errorf("%v: %d %s, want 400 naming %q", c.extra, status, raw, c.want)
			}
		}
		status, raw := d.req("POST", "/api/forms", d.dev, map[string]any{"name": "mc_bad", "label": "Bad", "fields": []map[string]any{
			{"name": "t", "label": "T", "type": "text", "multiple": true}}})
		if status != http.StatusBadRequest || !strings.Contains(string(raw), "dimension field's option") {
			t.Errorf("multiple on a text field: %d %s, want 400", status, raw)
		}
	})

	deals := table("mc_deals", dealFields(true, nil), map[string]any{"keeps_dimension_id": dealDim, "member_label_field": "name"})
	type rec struct {
		ID   string         `json:"id"`
		Data map[string]any `json:"data"`
	}
	create := func(formID string, data map[string]any) rec {
		t.Helper()
		var r rec
		raw, _ := json.Marshal(d.call("POST", "/api/forms/"+formID+"/records", d.westRep, map[string]any{"data": data}))
		_ = json.Unmarshal(raw, &r)
		return r
	}
	ann := create(contacts, map[string]any{"full_name": "Ann Lee"})
	bob := create(contacts, map[string]any{"full_name": "Bob Stone"})
	cid := create(contacts, map[string]any{"full_name": "Cid Moss"})
	people := func(r rec) []string {
		var out []string
		for _, x := range r.Data["people"].([]any) {
			out = append(out, x.(string))
		}
		return out
	}

	var big, small rec
	t.Run("a list is saved by codes or labels, each member once; a file's cell splits on ;", func(t *testing.T) {
		big = create(deals, map[string]any{"name": "Big", "people": []any{"CON-0001", "Bob Stone", "CON-0001"}, "value": 100})
		if got := people(big); !slices.Equal(got, []string{"CON-0001", "CON-0002"}) {
			t.Errorf("Big's contacts = %v, want [CON-0001 CON-0002]", got)
		}
		small = create(deals, map[string]any{"name": "Small", "people": "Cid Moss; CON-0002"})
		if got := people(small); !slices.Equal(got, []string{"CON-0003", "CON-0002"}) {
			t.Errorf("Small's contacts = %v, want [CON-0003 CON-0002]", got)
		}
		status, raw := d.req("POST", "/api/forms/"+deals+"/records", d.westRep, map[string]any{"data": map[string]any{"name": "Bad", "people": []any{"CON-0001", "Nobody"}}})
		if status != http.StatusBadRequest || !strings.Contains(string(raw), `Nobody\" is not a member`) {
			t.Errorf("an unknown member in the list: %d %s, want 400 naming it", status, raw)
		}
		_ = ann
		_ = bob
	})

	t.Run("required asks for one member at least", func(t *testing.T) {
		req := table("mc_req", []map[string]any{
			{"name": "name", "label": "Name", "type": "text", "required": true},
			{"name": "people", "label": "Contacts", "type": "dimension", "dimension_id": contactDim, "multiple": true, "required": true},
		}, nil)
		status, raw := d.req("POST", "/api/forms/"+req+"/records", d.westRep, map[string]any{"data": map[string]any{"name": "X", "people": []any{}}})
		if status != http.StatusBadRequest || !strings.Contains(string(raw), "name at least one") {
			t.Errorf("an empty required list: %d %s, want 400", status, raw)
		}
	})

	query := func(q map[string]any) (int, []rec, string) {
		t.Helper()
		status, raw := d.req("POST", "/api/forms/"+deals+"/records/query", d.westRep, q)
		var res struct {
			Records []rec `json:"records"`
		}
		_ = json.Unmarshal(raw, &res)
		return status, res.Records, string(raw)
	}
	names := func(rs []rec) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Data["name"].(string))
		}
		slices.Sort(out)
		return out
	}
	t.Run("a filter for a member keeps the records holding it; search reads labels; no sort or grouping", func(t *testing.T) {
		for _, c := range []struct {
			filter map[string]any
			want   []string
		}{
			{map[string]any{"field": "people", "op": "eq", "value": "CON-0002"}, []string{"Big", "Small"}},
			{map[string]any{"field": "people", "op": "eq", "value": "con-0003"}, []string{"Small"}},
			{map[string]any{"field": "people", "op": "ne", "value": "CON-0003"}, []string{"Big"}},
			{map[string]any{"field": "people", "op": "in", "values": []any{"CON-0001", "CON-0003"}}, []string{"Big", "Small"}},
			{map[string]any{"field": "people", "op": "empty"}, nil},
		} {
			status, rs, raw := query(map[string]any{"filters": []any{c.filter}})
			if status != http.StatusOK || !slices.Equal(names(rs), c.want) {
				t.Errorf("filter %v: %d %v %s, want %v", c.filter, status, names(rs), raw, c.want)
			}
		}
		if _, rs, _ := query(map[string]any{"search": "cid mo"}); !slices.Equal(names(rs), []string{"Small"}) {
			t.Errorf("search by a member's label: %v, want [Small]", names(rs))
		}
		if status, _, raw := query(map[string]any{"sort": []any{map[string]any{"field": "people"}}}); status != http.StatusBadRequest {
			t.Errorf("sort by the list: %d %s, want 400", status, raw)
		}
		if status, _, raw := query(map[string]any{"group_by": []any{map[string]any{"field": "people"}}}); status != http.StatusBadRequest {
			t.Errorf("group by the list: %d %s, want 400", status, raw)
		}
	})

	t.Run("a contact's page lists the deals naming them", func(t *testing.T) {
		var res struct {
			Related []struct {
				Field   string `json:"field"`
				Records []rec  `json:"records"`
			} `json:"related"`
		}
		raw, _ := json.Marshal(d.call("GET", "/api/records/"+bob.ID+"/related", d.westRep, nil))
		_ = json.Unmarshal(raw, &res)
		var found bool
		for _, rel := range res.Related {
			if rel.Field == "people" && slices.Equal(names(rel.Records), []string{"Big", "Small"}) {
				found = true
			}
		}
		if !found {
			t.Errorf("Bob's related = %s, want the deals Big and Small through people", raw)
		}
	})

	t.Run("a board's columns refuse it", func(t *testing.T) {
		if status, raw := d.req("GET", "/api/forms/"+deals+"/board-columns?field=people", d.westRep, nil); status != http.StatusBadRequest {
			t.Errorf("board columns of the list: %d %s, want 400", status, raw)
		}
	})

	t.Run("a deleted contact comes off the lists (clear); block keeps them", func(t *testing.T) {
		d.call("DELETE", "/api/records/"+cid.ID, d.westRep, nil)
		_, rs, _ := query(map[string]any{"filters": []any{map[string]any{"field": "name", "op": "eq", "value": "Small"}}})
		if len(rs) != 1 || !slices.Equal(people(rs[0]), []string{"CON-0002"}) {
			t.Errorf("Small after Cid is deleted: %v, want [CON-0002]", rs)
		}
		status, raw := d.req("PATCH", "/api/forms/"+deals, d.dev, map[string]any{"name": "mc_deals", "label": "Deals", "fields": dealFields(true, map[string]any{"on_delete": "block"})})
		if status != http.StatusOK {
			t.Fatalf("on_delete block: %d %s", status, raw)
		}
		if status, raw := d.req("DELETE", "/api/records/"+bob.ID, d.westRep, nil); status != http.StatusConflict {
			t.Errorf("deleting Bob while deals name him: %d %s, want 409", status, raw)
		}
	})

	t.Run("one value and several: records carry over, several refused while a record names several", func(t *testing.T) {
		status, raw := d.req("PATCH", "/api/forms/"+deals, d.dev, map[string]any{"name": "mc_deals", "label": "Deals", "fields": dealFields(false, map[string]any{"on_delete": "block"})})
		if status != http.StatusBadRequest || !strings.Contains(string(raw), "names several members in 1 record") {
			t.Errorf("one value while Big names two: %d %s, want 400", status, raw)
		}
		d.call("PUT", "/api/records/"+big.ID, d.westRep, map[string]any{"data": map[string]any{"name": "Big", "people": []any{"CON-0001"}, "value": 100}})
		if status, raw := d.req("PATCH", "/api/forms/"+deals, d.dev, map[string]any{"name": "mc_deals", "label": "Deals", "fields": dealFields(false, map[string]any{"on_delete": "block"})}); status != http.StatusOK {
			t.Fatalf("one value: %d %s", status, raw)
		}
		_, rs, _ := query(map[string]any{"filters": []any{map[string]any{"field": "name", "op": "eq", "value": "Big"}}})
		if len(rs) != 1 || rs[0].Data["people"] != "CON-0001" {
			t.Errorf("Big holding one value: %v, want people CON-0001", rs)
		}
		if status, raw := d.req("PATCH", "/api/forms/"+deals, d.dev, map[string]any{"name": "mc_deals", "label": "Deals", "fields": dealFields(true, map[string]any{"on_delete": "block"})}); status != http.StatusOK {
			t.Fatalf("several again: %d %s", status, raw)
		}
		_, rs, _ = query(map[string]any{"filters": []any{map[string]any{"field": "name", "op": "eq", "value": "Big"}}})
		if len(rs) != 1 || !slices.Equal(people(rs[0]), []string{"CON-0001"}) {
			t.Errorf("Big holding a list again: %v, want [CON-0001]", rs)
		}
	})
}

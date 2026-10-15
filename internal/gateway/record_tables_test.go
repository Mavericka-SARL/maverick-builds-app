package gateway

// Tables of records over HTTP, on the sales demo model: a table without a
// status field (its records count as they are saved), auto-codes, what a
// person's access rules mean for the records they save and see — a
// read-only member refused where the record posts, a hidden member or
// metric that is no member or metric to them, a metric named by a field
// hidden from them withholding the record — a table showing each person
// their own records, the records query, and a status field's removal
// re-posting the table.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestRecordTables(t *testing.T) {
	d := setupSalesDemo(t)
	ctx := context.Background()

	fields := []map[string]any{
		{"name": "code", "label": "Code", "type": "autocode", "code_prefix": "D-", "code_digits": 3},
		{"name": "region", "label": "Region", "type": "dimension", "required": true, "dimension_id": d.geoDim},
		{"name": "product", "label": "Product", "type": "dimension", "required": true, "dimension_id": d.prodDim},
		{"name": "period", "label": "Period", "type": "dimension", "required": true, "dimension_id": d.periodDim},
		{"name": "amount", "label": "Amount", "type": "number", "required": true},
		{"name": "kind", "label": "Kind", "type": "metric"},
		{"name": "note", "label": "Note", "type": "text"},
	}
	formID := idOf(d.call("POST", "/api/forms", d.dev, map[string]any{"name": "deal", "label": "Deal", "fields": fields}))
	// The mapping's posting statuses say approved; a record of a table
	// without a status field has none, and counts as it is saved.
	mappingID := idOf(d.call("POST", "/api/developer/form-integrations", d.dev, map[string]any{
		"form_id": formID, "grid_id": "", "name": "Deals into revenue",
		"source_field": "amount", "target_metric_id": d.metric["revenue"],
		"aggregation": "sum", "posting_statuses": []string{"approved"},
		"dimension_mappings": map[string]string{d.geoDim: "region", d.prodDim: "product", d.periodDim: "period"},
		"live_posting":       true,
	}))
	// Reed may only read the United Kingdom, cannot see Germany or the cost metric.
	d.call("PUT", "/api/business-admin/users/"+d.roID+"/access-rules", d.admin, map[string]any{"rules": []map[string]string{
		member(d.geo["UK"], "read"), member(d.geo["DE"], "hidden"),
		{"rule_type": "metric", "ref_id": d.metric["cost"], "access": "hidden"},
	}})

	deal := func(region string, amount float64) map[string]any {
		return map[string]any{"region": region, "product": "LAPTOP", "period": "Q1", "amount": amount}
	}
	postings := func(recordID string) int {
		t.Helper()
		var n int
		if err := d.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.form_record_posting WHERE mapping_id=$1::uuid AND form_record_id=$2::uuid`,
			mappingID, recordID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	waitPostings := func(recordID string, want int) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); postings(recordID) != want; time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("record %s holds %d posting(s), want %d", recordID, postings(recordID), want)
			}
		}
	}
	type record struct {
		ID     string         `json:"id"`
		Status string         `json:"status"`
		Data   map[string]any `json:"data"`
	}
	list := func(persona string) []record {
		t.Helper()
		status, raw := d.req("GET", "/api/forms/"+formID+"/records", persona, nil)
		if status != http.StatusOK {
			t.Fatalf("list as %s: %d %s", persona, status, raw)
		}
		var out []record
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	var first string
	t.Run("a record without a status posts as it is saved, with the next code", func(t *testing.T) {
		rec := d.call("POST", "/api/forms/"+formID+"/records", d.westRep, map[string]any{"data": deal("US", 100)})
		first = idOf(rec)
		if rec["status"] != "" {
			t.Errorf("status = %v, want none", rec["status"])
		}
		if data, _ := rec["data"].(map[string]any); data["code"] != "D-001" {
			t.Errorf("code = %v, want D-001", data["code"])
		}
		waitPostings(first, 1)
		if status, raw := d.req("POST", "/api/forms/"+formID+"/records", d.westRep, map[string]any{"data": deal("US", 1), "status": "submitted"}); status != http.StatusBadRequest {
			t.Errorf("a status for a record of a table without one: %d %s, want 400", status, raw)
		}
	})

	t.Run("the code is the server's", func(t *testing.T) {
		data := deal("US", 120)
		data["code"] = "MINE"
		d.call("PUT", "/api/records/"+first, d.westRep, map[string]any{"data": data})
		var code string
		if err := d.pool.QueryRow(ctx, `SELECT data->>'code' FROM runtime.form_record WHERE id=$1::uuid`, first).Scan(&code); err != nil || code != "D-001" {
			t.Errorf("code after an edit naming another = %q (%v), want D-001", code, err)
		}
	})

	t.Run("a member read-only for the person is refused where the record posts", func(t *testing.T) {
		status, raw := d.req("POST", "/api/forms/"+formID+"/records", d.ro, map[string]any{"data": deal("UK", 5)})
		if status != http.StatusForbidden || !strings.Contains(string(raw), "United Kingdom is read-only for you") {
			t.Errorf("Reed's record in the United Kingdom: %d %s, want 403 saying it is read-only", status, raw)
		}
		// What the picker is given: UK marked read-only, Germany absent, and
		// where the table posts.
		var dims []struct {
			ID      string `json:"id"`
			Members []struct {
				Code   string `json:"code"`
				Access string `json:"access"`
			} `json:"members"`
		}
		_, raw = d.req("GET", "/api/dimensions", d.ro, nil)
		_ = json.Unmarshal(raw, &dims)
		seen := map[string]string{}
		for _, dim := range dims {
			if dim.ID == d.geoDim {
				for _, m := range dim.Members {
					seen[m.Code] = m.Access
				}
			}
		}
		if seen["UK"] != "read" {
			t.Errorf("UK's access for Reed = %q, want read", seen["UK"])
		}
		if _, ok := seen["DE"]; ok {
			t.Error("Reed is offered Germany, hidden from him")
		}
		var forms []struct {
			ID            string              `json:"id"`
			PostingFields map[string][]string `json:"posting_fields"`
			Permissions   struct {
				Create         bool     `json:"create"`
				CreateStatuses []string `json:"create_statuses"`
			} `json:"permissions"`
		}
		_, raw = d.req("GET", "/api/forms", d.ro, nil)
		_ = json.Unmarshal(raw, &forms)
		for _, f := range forms {
			if f.ID != formID {
				continue
			}
			if got := f.PostingFields["region"]; len(got) != 1 || got[0] != "" {
				t.Errorf("posting_fields[region] = %v, want [\"\"] (posts on save)", got)
			}
			if !f.Permissions.Create || len(f.Permissions.CreateStatuses) != 0 {
				t.Errorf("Reed's permissions = %+v, want create without statuses", f.Permissions)
			}
		}
	})

	t.Run("a member hidden from the person is no member", func(t *testing.T) {
		var before int
		_ = d.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.form_record WHERE form_id=$1::uuid`, formID).Scan(&before)
		status, raw := d.req("POST", "/api/forms/"+formID+"/records", d.ro, map[string]any{"data": deal("DE", 5)})
		if status != http.StatusBadRequest || !strings.Contains(string(raw), `\"DE\" is not a member of Region`) {
			t.Errorf("Reed's record in Germany: %d %s, want 400 as for an unknown code", status, raw)
		}
		var after int
		_ = d.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.form_record WHERE form_id=$1::uuid`, formID).Scan(&after)
		if after != before {
			t.Error("the refused record was saved")
		}
		if status, raw := d.req("POST", "/api/forms/"+formID+"/records", d.westRep, map[string]any{"data": deal("EMEA", 5)}); status != http.StatusBadRequest {
			t.Errorf("a record at a parent member: %d %s, want 400", status, raw)
		}
	})

	t.Run("a metric named by a field and hidden from the person withholds the record", func(t *testing.T) {
		data := deal("US", 7)
		data["kind"] = "cost"
		costRec := idOf(d.call("POST", "/api/forms/"+formID+"/records", d.eastRep, map[string]any{"data": data}))
		for _, r := range list(d.ro) {
			if r.ID == costRec {
				t.Error("Reed sees a record naming the cost metric, hidden from him")
			}
		}
		if status, _ := d.req("GET", "/api/records/"+costRec, d.ro, nil); status != http.StatusNotFound {
			t.Errorf("Reed reads the record naming cost: %d, want 404", status)
		}
		if status, raw := d.req("POST", "/api/forms/"+formID+"/records", d.ro, map[string]any{"data": map[string]any{
			"region": "US", "product": "LAPTOP", "period": "Q1", "amount": 1, "kind": "cost",
		}}); status != http.StatusBadRequest {
			t.Errorf("Reed names the hidden cost metric: %d %s, want 400", status, raw)
		}
		if status, raw := d.req("POST", "/api/forms/"+formID+"/records", d.westRep, map[string]any{"data": map[string]any{
			"region": "US", "product": "LAPTOP", "period": "Q1", "amount": 1, "kind": "no_such_metric",
		}}); status != http.StatusBadRequest {
			t.Errorf("a metric that does not exist: %d %s, want 400", status, raw)
		}
	})

	t.Run("the records query counts and sums what the person may see", func(t *testing.T) {
		d.call("POST", "/api/forms/"+formID+"/records", d.eastRep, map[string]any{"data": deal("CA", 30)})
		totals := d.call("POST", "/api/forms/"+formID+"/records/query", d.ro, map[string]any{
			"group_by":   []map[string]any{{"field": "region"}},
			"aggregates": []map[string]any{{"op": "count"}, {"op": "sum", "field": "amount"}},
			"sort":       []map[string]any{{"field": "region"}},
		})
		rows, _ := totals["rows"].([]any)
		got := map[string]float64{}
		for _, r := range rows {
			row := r.(map[string]any)
			got[row["region"].(string)+"/"+row["region_label"].(string)] = row["sum_amount"].(float64)
		}
		// US: 120 (first) — the record naming cost is withheld from Reed.
		if len(got) != 2 || got["US/United States"] != 120 || got["CA/Canada"] != 30 {
			t.Errorf("Reed's totals by region = %v, want US 120 and CA 30", got)
		}
		page := d.call("POST", "/api/forms/"+formID+"/records/query", d.westRep, map[string]any{
			"filters": []map[string]any{{"field": "amount", "op": "gte", "value": 30}},
			"search":  "canada", "limit": 10,
		})
		if page["total"] != 1.0 {
			t.Errorf("west rep's search for canada with amount ≥ 30: total %v, want 1", page["total"])
		}
		if status, raw := d.req("POST", "/api/forms/"+formID+"/records/query", d.westRep, map[string]any{
			"filters": []map[string]any{{"field": "amount", "op": "contains", "value": "1"}},
		}); status != http.StatusBadRequest {
			t.Errorf("contains on a number: %d %s, want 400", status, raw)
		}
		if status, raw := d.req("POST", "/api/forms/"+formID+"/records/query", d.westRep, map[string]any{"limt": 3}); status != http.StatusBadRequest {
			t.Errorf("an unknown query key: %d %s, want 400", status, raw)
		}
	})

	t.Run("a table showing each person their own records", func(t *testing.T) {
		d.call("PATCH", "/api/forms/"+formID, d.dev, map[string]any{"name": "deal", "label": "Deal", "fields": fields, "own_records_only": true})
		for _, r := range list(d.eastRep) {
			if r.ID == first {
				t.Error("the east rep sees the west rep's record")
			}
		}
		if status, _ := d.req("GET", "/api/records/"+first, d.eastRep, nil); status != http.StatusNotFound {
			t.Errorf("the east rep reads the west rep's record: %d, want 404", status)
		}
		found := false
		for _, r := range list(d.admin) {
			found = found || r.ID == first
		}
		if !found {
			t.Error("the business admin does not see every record")
		}
		count := d.call("POST", "/api/forms/"+formID+"/records/query", d.westRep, map[string]any{"aggregates": []map[string]any{{"op": "count"}}})
		if rows, _ := count["rows"].([]any); len(rows) != 1 || rows[0].(map[string]any)["count"] != 1.0 {
			t.Errorf("the west rep's count = %v, want their one record", count["rows"])
		}
		d.call("PATCH", "/api/forms/"+formID, d.dev, map[string]any{"name": "deal", "label": "Deal", "fields": fields, "own_records_only": false})
	})

	t.Run("an export holds what the query shows", func(t *testing.T) {
		q := `{"filters":[{"field":"region","value":"CA"}]}`
		status, raw := d.req("GET", "/api/forms/"+formID+"/export?format=csv&query="+url.QueryEscape(q), d.westRep, nil)
		if status != http.StatusOK {
			t.Fatalf("export: %d %s", status, raw)
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		if len(lines) != 2 || !strings.Contains(lines[1], "CA") {
			t.Errorf("export filtered to Canada = %q", lines)
		}
	})

	t.Run("removing a status field re-posts the records that waited", func(t *testing.T) {
		withStatus := append(append([]map[string]any{}, fields...), map[string]any{"name": "status", "label": "Status", "type": "status"})
		adjID := idOf(d.call("POST", "/api/forms", d.dev, map[string]any{"name": "adj", "label": "Adjustment", "fields": withStatus}))
		adjMapping := idOf(d.call("POST", "/api/developer/form-integrations", d.dev, map[string]any{
			"form_id": adjID, "grid_id": "", "name": "Adjustments into revenue",
			"source_field": "amount", "target_metric_id": d.metric["revenue"],
			"aggregation": "sum", "posting_statuses": []string{"approved"},
			"dimension_mappings": map[string]string{d.geoDim: "region", d.prodDim: "product", d.periodDim: "period"},
			"live_posting":       true,
		}))
		draft := d.call("POST", "/api/forms/"+adjID+"/records", d.westRep, map[string]any{"data": deal("US", 9)})
		if draft["status"] != "draft" {
			t.Fatalf("a new record of a table with a status field: %v, want draft", draft["status"])
		}
		count := func() int {
			var n int
			_ = d.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.form_record_posting WHERE mapping_id=$1::uuid`, adjMapping).Scan(&n)
			return n
		}
		time.Sleep(300 * time.Millisecond)
		if count() != 0 {
			t.Fatal("a draft posted")
		}
		patched := d.call("PATCH", "/api/forms/"+adjID, d.dev, map[string]any{"name": "adj", "label": "Adjustment", "fields": fields})
		if patched["status_field_changed"] != true {
			t.Errorf("PATCH answer = %v, want status_field_changed", patched)
		}
		for deadline := time.Now().Add(10 * time.Second); count() != 1; time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("the record never posted once the status field went: %d posting(s)", count())
			}
		}
	})
}

// TestConnectorReachesTheRecordsQueryOnly pins the chat connector's read
// allowlist for tables: the list and the read-only query, never a write.
func TestConnectorReachesTheRecordsQueryOnly(t *testing.T) {
	for _, c := range []struct {
		method, path string
		want         bool
	}{
		{"GET", "/api/forms", true},
		{"POST", "/api/forms/6b0f2f4e-0000-4000-8000-000000000001/records/query", true},
		{"POST", "/api/forms", false},
		{"POST", "/api/forms/6b0f2f4e-0000-4000-8000-000000000001/records", false},
		{"GET", "/api/forms/6b0f2f4e-0000-4000-8000-000000000001/records", false},
		{"GET", "/api/forms/6b0f2f4e-0000-4000-8000-000000000001/export", false},
		{"PUT", "/api/records/6b0f2f4e-0000-4000-8000-000000000001", false},
	} {
		if got := delegatedReadAllowed(c.method, c.path); got != c.want {
			t.Errorf("%s %s: allowed %v, want %v", c.method, c.path, got, c.want)
		}
	}
}

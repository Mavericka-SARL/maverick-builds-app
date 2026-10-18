package gateway

// Tables keeping a dimension's members, over HTTP, as a small CRM: accounts,
// contacts and deals are tables whose records are the members of the
// account, contact and deal dimensions; other tables name them through
// dimension fields. What it covers: the member made, renamed and deleted with
// its record and no other way; defaults, links, paired fields; a table
// everyone edits beside one only its creator does; a field's history; access
// rules on an account hiding everything naming it, pickers included; a
// read-only stage; the delete rules (block, cascade, clear) and the plan the
// confirmation shows; related records; the board and records-widget
// settings; a date posting into its month; and a revision copy keeping the
// tables' settings.

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestKeptDimensionsLinkRecords(t *testing.T) {
	d := setupSalesDemo(t)
	ctx := context.Background()

	dim := func(name string) string {
		return idOf(d.call("POST", "/api/developer/dimensions", d.dev, map[string]any{"name": name, "revision_id": d.revID}))
	}
	accountDim, contactDim, dealDim, stageDim := dim("crm_account"), dim("crm_contact"), dim("crm_deal"), dim("crm_stage")
	stage := map[string]string{}
	for _, c := range []string{"QUAL", "MEET", "WON", "LOST"} {
		stage[c] = idOf(d.call("POST", "/api/developer/dimensions/"+stageDim+"/members", d.dev, map[string]any{"code": c, "label": c}))
	}
	month := idOf(d.call("POST", "/api/developer/dimensions", d.dev, map[string]any{"name": "crm_close_month", "revision_id": d.revID,
		"dimension_type": "time", "time_granularity": "month", "fiscal_year_start_month": 1}))
	d.call("POST", "/api/developer/dimensions/"+month+"/members/generate", d.dev, map[string]any{"start": "2026-01-01", "end": "2026-12-31"})

	table := func(name string, fields []map[string]any, settings map[string]any) string {
		t.Helper()
		body := map[string]any{"name": name, "label": strings.ToUpper(name[:1]) + name[1:], "fields": fields}
		for k, v := range settings {
			body[k] = v
		}
		return idOf(d.call("POST", "/api/forms", d.dev, body))
	}
	accounts := table("accounts", []map[string]any{
		{"name": "code", "label": "Code", "type": "autocode", "code_prefix": "ACC-", "code_digits": 4, "core": true},
		{"name": "name", "label": "Company Name", "type": "text", "required": true, "core": true},
		{"name": "website", "label": "Website", "type": "url"},
		{"name": "country", "label": "Country", "type": "select", "options": []string{"Russia", "Kazakhstan"}, "default": "Russia"},
		{"name": "notes", "label": "Notes", "type": "longtext"},
	}, map[string]any{"keeps_dimension_id": accountDim, "member_label_field": "name", "member_root_label": "All accounts", "edit_access": "everyone"})
	contacts := table("contacts", []map[string]any{
		{"name": "code", "label": "Code", "type": "autocode", "code_prefix": "CON-"},
		{"name": "full_name", "label": "Full Name", "type": "text", "required": true},
		{"name": "account", "label": "Account", "type": "dimension", "dimension_id": accountDim, "required": true},
	}, map[string]any{"keeps_dimension_id": contactDim, "member_label_field": "full_name", "edit_access": "everyone"})
	deals := table("deals", []map[string]any{
		{"name": "code", "label": "Code", "type": "autocode", "code_prefix": "DEAL-"},
		{"name": "name", "label": "Deal Name", "type": "text", "required": true},
		{"name": "account", "label": "Account", "type": "dimension", "dimension_id": accountDim, "required": true},
		{"name": "stage", "label": "Stage", "type": "dimension", "dimension_id": stageDim, "required": true, "history": true},
		{"name": "contact", "label": "Primary Contact", "type": "dimension", "dimension_id": contactDim, "on_delete": "clear"},
		{"name": "value", "label": "Value", "type": "number", "format": "money"},
		{"name": "close", "label": "Expected Close", "type": "date"},
		{"name": "next_desc", "label": "Next Action", "type": "text", "required_with": "next_date"},
		{"name": "next_date", "label": "Next Action Date", "type": "date", "deadline": true},
	}, map[string]any{"keeps_dimension_id": dealDim, "member_label_field": "name", "edit_access": "everyone"})
	tasks := table("tasks", []map[string]any{
		{"name": "title", "label": "Title", "type": "text", "required": true},
		{"name": "deal", "label": "Deal", "type": "dimension", "dimension_id": dealDim, "on_delete": "cascade"},
		{"name": "due", "label": "Due", "type": "date", "deadline": true, "done_field": "state", "done_values": []string{"Completed"}},
		{"name": "state", "label": "State", "type": "select", "options": []string{"Open", "Completed"}, "default": "Open"},
	}, nil)

	type rec struct {
		ID          string         `json:"id"`
		Data        map[string]any `json:"data"`
		Permissions struct {
			Edit   bool `json:"edit"`
			Delete bool `json:"delete"`
		} `json:"permissions"`
	}
	create := func(persona, formID string, data map[string]any) rec {
		t.Helper()
		var r rec
		raw, _ := json.Marshal(d.call("POST", "/api/forms/"+formID+"/records", persona, map[string]any{"data": data}))
		_ = json.Unmarshal(raw, &r)
		return r
	}
	memberLabel := func(dimID, code string) string {
		t.Helper()
		var label string
		if err := d.pool.QueryRow(ctx, `SELECT label FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`, dimID, code).Scan(&label); err != nil {
			return ""
		}
		return label
	}
	list := func(persona, formID string) []rec {
		t.Helper()
		status, raw := d.req("GET", "/api/forms/"+formID+"/records", persona, nil)
		if status != http.StatusOK {
			t.Fatalf("list as %s: %d %s", persona, status, raw)
		}
		var out []rec
		_ = json.Unmarshal(raw, &out)
		return out
	}
	codes := func(rs []rec) []string {
		var out []string
		for _, r := range rs {
			out = append(out, r.Data["code"].(string))
		}
		slices.Sort(out)
		return out
	}

	var acme, globex rec
	t.Run("a record is the member: defaults and links are normalized", func(t *testing.T) {
		acme = create(d.westRep, accounts, map[string]any{"name": "Acme", "website": "acme.example"})
		if acme.Data["code"] != "ACC-0001" || acme.Data["country"] != "Russia" || acme.Data["website"] != "https://acme.example" {
			t.Errorf("Acme = %v, want code ACC-0001, country Russia (default), website https://acme.example", acme.Data)
		}
		if got := memberLabel(accountDim, "ACC-0001"); got != "Acme" {
			t.Errorf("member ACC-0001 label = %q, want Acme", got)
		}
		var parent string
		_ = d.pool.QueryRow(ctx, `SELECT COALESCE(p.code, '') FROM model.dimension_member m LEFT JOIN model.dimension_member p ON p.id = m.parent_member_id
			WHERE m.dimension_id=$1::uuid AND m.code='ACC-0001'`, accountDim).Scan(&parent)
		if parent != "ALL" || memberLabel(accountDim, "ALL") != "All accounts" {
			t.Errorf("Acme's member sits under %q (total %q), want ALL, All accounts", parent, memberLabel(accountDim, "ALL"))
		}
		globex = create(d.eastRep, accounts, map[string]any{"name": "Globex"})
		if status, raw := d.req("POST", "/api/forms/"+accounts+"/records", d.westRep, map[string]any{"data": map[string]any{"name": "Bad", "website": "not a link"}}); status != http.StatusBadRequest {
			t.Errorf("a website that is no link: %d %s, want 400", status, raw)
		}
	})

	t.Run("the members change only with the records", func(t *testing.T) {
		status, raw := d.req("POST", "/api/developer/dimensions/"+accountDim+"/members", d.dev, map[string]any{"code": "X", "label": "X"})
		if status != http.StatusConflict || !strings.Contains(string(raw), "Accounts") {
			t.Errorf("a developer adding a member: %d %s, want 409 naming the table", status, raw)
		}
		var memberID string
		_ = d.pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code='ACC-0001'`, accountDim).Scan(&memberID)
		if status, raw := d.req("PATCH", "/api/developer/dimensions/"+accountDim+"/members/"+memberID, d.dev, map[string]any{"label": "Renamed"}); status != http.StatusConflict {
			t.Errorf("a developer renaming a member: %d %s, want 409", status, raw)
		}
		d.call("PUT", "/api/records/"+acme.ID, d.eastRep, map[string]any{"data": map[string]any{"name": "Acme Corp", "website": "https://acme.example", "country": "Russia"}})
		if got := memberLabel(accountDim, "ACC-0001"); got != "Acme Corp" {
			t.Errorf("after renaming the record the member is %q, want Acme Corp", got)
		}
		// The code field and the label field stay while the table keeps the dimension.
		status, raw = d.req("PATCH", "/api/forms/"+accounts, d.dev, map[string]any{"name": "accounts", "label": "Accounts", "fields": []map[string]any{
			{"name": "code", "label": "Code", "type": "autocode", "code_prefix": "ACC-", "code_digits": 4, "core": true},
		}})
		if status != http.StatusBadRequest || !strings.Contains(string(raw), "core") {
			t.Errorf("removing a core field: %d %s, want 400", status, raw)
		}
	})

	var alice, deal1, deal2 rec
	t.Run("links, paired fields and a table everyone edits", func(t *testing.T) {
		alice = create(d.westRep, contacts, map[string]any{"full_name": "Alice", "account": "ACC-0001"})
		status, raw := d.req("POST", "/api/forms/"+deals+"/records", d.westRep, map[string]any{"data": map[string]any{
			"name": "Big", "account": "ACC-0001", "stage": "QUAL", "next_desc": "Call back"}})
		if status != http.StatusBadRequest || !strings.Contains(string(raw), "Next Action Date is required when Next Action is filled in") {
			t.Errorf("a next action without its date: %d %s, want 400 naming the pair", status, raw)
		}
		deal1 = create(d.westRep, deals, map[string]any{"name": "Big", "account": "ACC-0001", "stage": "QUAL", "contact": alice.Data["code"],
			"value": 1000, "close": "2026-03-15", "next_desc": "Call back", "next_date": "2026-01-10"})
		deal2 = create(d.westRep, deals, map[string]any{"name": "Small", "account": globex.Data["code"], "stage": "MEET", "value": 50})
		// Everyone who sees a deal may change it; a task is its creator's.
		d.call("PUT", "/api/records/"+deal1.ID, d.eastRep, map[string]any{"data": map[string]any{"name": "Big", "account": "ACC-0001", "stage": "MEET",
			"contact": alice.Data["code"], "value": 1000, "close": "2026-03-15", "next_desc": "Call back", "next_date": "2026-01-10"}})
		task := create(d.westRep, tasks, map[string]any{"title": "Send offer", "deal": deal1.Data["code"], "due": "2026-01-05"})
		if task.Data["state"] != "Open" {
			t.Errorf("task state = %v, want the default Open", task.Data["state"])
		}
		if status, raw := d.req("PUT", "/api/records/"+task.ID, d.eastRep, map[string]any{"data": map[string]any{"title": "x", "deal": deal1.Data["code"]}}); status != http.StatusForbidden {
			t.Errorf("another person changing a task (creator-only table): %d %s, want 403 as before", status, raw)
		}
	})

	t.Run("a hidden field a save does not name keeps its value", func(t *testing.T) {
		form := table("hidden_kept", []map[string]any{
			{"name": "title", "label": "Title", "type": "text", "required": true},
			{"name": "one", "label": "One", "type": "number", "hidden": true, "default": 1},
		}, nil)
		r := create(d.westRep, form, map[string]any{"title": "a"})
		d.call("PUT", "/api/records/"+r.ID, d.westRep, map[string]any{"data": map[string]any{"title": "b"}})
		var one *float64
		_ = d.pool.QueryRow(ctx, `SELECT (data->>'one')::float8 FROM runtime.form_record WHERE id=$1::uuid`, r.ID).Scan(&one)
		if one == nil || *one != 1 {
			t.Errorf("hidden field after a save not naming it = %v, want 1", one)
		}
		d.call("PUT", "/api/records/"+r.ID, d.westRep, map[string]any{"data": map[string]any{"title": "c", "one": nil}})
		_ = d.pool.QueryRow(ctx, `SELECT (data->>'one')::float8 FROM runtime.form_record WHERE id=$1::uuid`, r.ID).Scan(&one)
		if one != nil {
			t.Errorf("hidden field after a save clearing it = %v, want empty", *one)
		}
	})

	t.Run("a field's history", func(t *testing.T) {
		var out struct {
			History []struct {
				Field string `json:"field"`
				Old   any    `json:"old"`
				New   any    `json:"new"`
			} `json:"history"`
		}
		raw, _ := json.Marshal(d.call("GET", "/api/records/"+deal1.ID+"/history", d.westRep, nil))
		_ = json.Unmarshal(raw, &out)
		if len(out.History) != 2 || out.History[0].Old != nil || out.History[0].New != "QUAL" || out.History[1].Old != "QUAL" || out.History[1].New != "MEET" {
			t.Errorf("stage history = %+v, want none→QUAL, QUAL→MEET", out.History)
		}
	})

	var deal3 rec
	t.Run("hiding an account hides everything belonging to it", func(t *testing.T) {
		// A deal at Globex whose primary contact works at Acme: a reference,
		// not a belonging — it stays visible when Acme is hidden.
		deal3 = create(d.westRep, deals, map[string]any{"name": "Cross", "account": globex.Data["code"], "stage": "QUAL", "contact": alice.Data["code"]})
		var acmeMember string
		_ = d.pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code='ACC-0001'`, accountDim).Scan(&acmeMember)
		d.call("PUT", "/api/business-admin/users/"+d.eastRepID+"/access-rules", d.admin, map[string]any{"rules": []map[string]string{member(acmeMember, "hidden")}})
		if got := codes(list(d.eastRep, accounts)); !slices.Equal(got, []string{"ACC-0002"}) {
			t.Errorf("east's accounts = %v, want only Globex", got)
		}
		if got := list(d.eastRep, contacts); len(got) != 0 {
			t.Errorf("east sees %d contact(s) of the hidden account", len(got))
		}
		want := []string{deal2.Data["code"].(string), deal3.Data["code"].(string)}
		slices.Sort(want)
		if got := codes(list(d.eastRep, deals)); !slices.Equal(got, want) {
			t.Errorf("east's deals = %v, want Globex's two (%v), the one whose contact works at Acme included", got, want)
		}
		if got := list(d.eastRep, tasks); len(got) != 0 {
			t.Errorf("east sees %d task(s) of a hidden account's deal", len(got))
		}
		// The pickers drop them too.
		status, raw := d.req("GET", "/api/dimensions", d.eastRep, nil)
		if status != http.StatusOK {
			t.Fatalf("dimensions: %d %s", status, raw)
		}
		var dims []struct {
			ID      string `json:"id"`
			Members []struct {
				Code string `json:"code"`
			} `json:"members"`
		}
		_ = json.Unmarshal(raw, &dims)
		for _, dd := range dims {
			for _, m := range dd.Members {
				if (dd.ID == accountDim && m.Code == "ACC-0001") || (dd.ID == contactDim && m.Code == alice.Data["code"]) || (dd.ID == dealDim && m.Code == deal1.Data["code"]) {
					t.Errorf("east's picker offers %s of dimension %s", m.Code, dd.ID)
				}
			}
		}
		if status, raw := d.req("GET", "/api/records/"+deal1.ID, d.eastRep, nil); status != http.StatusNotFound {
			t.Errorf("east reading the hidden account's deal: %d %s, want 404", status, raw)
		}
		d.call("PUT", "/api/business-admin/users/"+d.eastRepID+"/access-rules", d.admin, map[string]any{"rules": []map[string]string{}})
	})

	t.Run("a read-only stage is seen, not changed or moved into", func(t *testing.T) {
		d.call("PUT", "/api/business-admin/users/"+d.eastRepID+"/access-rules", d.admin, map[string]any{"rules": []map[string]string{member(stage["WON"], "read")}})
		defer d.call("PUT", "/api/business-admin/users/"+d.eastRepID+"/access-rules", d.admin, map[string]any{"rules": []map[string]string{}})
		status, raw := d.req("PUT", "/api/records/"+deal2.ID, d.eastRep, map[string]any{"data": map[string]any{"name": "Small", "account": globex.Data["code"], "stage": "WON", "value": 50}})
		if status != http.StatusForbidden || !strings.Contains(string(raw), "read-only") {
			t.Errorf("east moving a deal into a read-only stage: %d %s, want 403", status, raw)
		}
		d.call("PUT", "/api/records/"+deal2.ID, d.westRep, map[string]any{"data": map[string]any{"name": "Small", "account": globex.Data["code"], "stage": "WON", "value": 50}})
		for _, r := range list(d.eastRep, deals) {
			if r.ID == deal2.ID && (r.Permissions.Edit || r.Permissions.Delete) {
				t.Errorf("east may change a deal in a stage read-only for them: %+v", r.Permissions)
			}
		}
	})

	t.Run("related records and the delete rules", func(t *testing.T) {
		var rel struct {
			Related []struct {
				Table struct {
					ID string `json:"id"`
				} `json:"table"`
				Total int `json:"total"`
			} `json:"related"`
		}
		raw, _ := json.Marshal(d.call("GET", "/api/records/"+acme.ID+"/related", d.westRep, nil))
		_ = json.Unmarshal(raw, &rel)
		totals := map[string]int{}
		for _, r := range rel.Related {
			totals[r.Table.ID] = r.Total
		}
		if totals[contacts] != 1 || totals[deals] != 1 {
			t.Errorf("Acme's related = %v, want 1 contact and 1 deal", totals)
		}
		// An account named by contacts and deals cannot go.
		status, body := d.req("DELETE", "/api/records/"+acme.ID, d.westRep, nil)
		if status != http.StatusConflict || !strings.Contains(string(body), "cannot be deleted") {
			t.Errorf("deleting an account with contacts and deals: %d %s, want 409", status, body)
		}
		plan := d.call("GET", "/api/records/"+deal1.ID+"/delete-plan", d.westRep, nil)
		if plan["can_delete"] != true || len(plan["cascade"].([]any)) != 1 {
			t.Errorf("the deal's delete plan = %v, want deletable with its task", plan)
		}
		// Deleting the contact empties the deal's primary contact.
		d.call("DELETE", "/api/records/"+alice.ID, d.westRep, nil)
		var contactLeft *string
		_ = d.pool.QueryRow(ctx, `SELECT data->>'contact' FROM runtime.form_record WHERE id=$1::uuid`, deal1.ID).Scan(&contactLeft)
		if contactLeft != nil {
			t.Errorf("the deal still names the deleted contact %q", *contactLeft)
		}
		// Deleting the deal takes its task, its history and its member.
		out := d.call("DELETE", "/api/records/"+deal1.ID, d.westRep, nil)
		if out["deleted"] != float64(2) {
			t.Errorf("deleting the deal deleted %v record(s), want 2 (it and its task)", out["deleted"])
		}
		var tasksLeft, historyLeft int
		_ = d.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.form_record WHERE form_id=$1::uuid`, tasks).Scan(&tasksLeft)
		_ = d.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.record_field_history WHERE record_id=$1::uuid`, deal1.ID).Scan(&historyLeft)
		if tasksLeft != 0 || historyLeft != 0 || memberLabel(dealDim, deal1.Data["code"].(string)) != "" {
			t.Errorf("after deleting the deal: %d task(s), %d history row(s), member %q left", tasksLeft, historyLeft, memberLabel(dealDim, deal1.Data["code"].(string)))
		}
		d.call("DELETE", "/api/records/"+acme.ID, d.westRep, nil)
		if memberLabel(accountDim, "ACC-0001") != "" {
			t.Error("the deleted account's member is still there")
		}
	})

	t.Run("board and records widget settings are checked against the table", func(t *testing.T) {
		dash := idOf(d.call("POST", "/api/developer/dashboards", d.dev, map[string]any{"name": "Pipeline", "revision_id": d.revID}))
		widget := func(wt, ref string, props map[string]any) (int, string) {
			status, raw := d.req("POST", "/api/developer/dashboards/"+dash+"/widgets", d.dev, map[string]any{"widget_type": wt, "ref_id": ref, "widget_props": props})
			return status, string(raw)
		}
		if status, raw := widget("board", deals, map[string]any{"board": map[string]any{"column_field": "stage", "card_fields": []string{"account", "value", "next_date"}, "sum_field": "value"},
			"record_filters": []map[string]any{{"field": "next_date", "op": "on_or_before_today"}}}); status != http.StatusOK {
			t.Errorf("a board over the deals by stage: %d %s", status, raw)
		}
		if status, raw := widget("board", deals, map[string]any{"board": map[string]any{"column_field": "value"}}); status != http.StatusBadRequest {
			t.Errorf("a board whose columns are a number field: %d %s, want 400", status, raw)
		}
		if status, raw := widget("form", tasks, map[string]any{"record_filters": []map[string]any{{"field": "due", "op": "before_today"}, {"field": "state", "op": "ne", "value": "Completed"}},
			"record_columns": []string{"title", "due"}, "record_sort": map[string]any{"field": "due"}}); status != http.StatusOK {
			t.Errorf("an overdue-tasks records widget: %d %s", status, raw)
		}
		if status, raw := widget("form", tasks, map[string]any{"record_filters": []map[string]any{{"field": "title", "op": "before_today"}}}); status != http.StatusBadRequest {
			t.Errorf("a relative date filter on a text field: %d %s, want 400", status, raw)
		}
		// The columns, for the viewer: a dimension's members in order (a stage
		// read-only for them marked), a select field's choices.
		columns := func(persona, formID, field string) []map[string]any {
			t.Helper()
			var out struct {
				Columns []map[string]any `json:"columns"`
			}
			raw, _ := json.Marshal(d.call("GET", "/api/forms/"+formID+"/board-columns?field="+field, persona, nil))
			_ = json.Unmarshal(raw, &out)
			return out.Columns
		}
		var keys []string
		for _, c := range columns(d.westRep, deals, "stage") {
			keys = append(keys, c["key"].(string))
		}
		if !slices.Equal(keys, []string{"QUAL", "MEET", "WON", "LOST"}) {
			t.Errorf("stage columns = %v, want QUAL MEET WON LOST", keys)
		}
		d.call("PUT", "/api/business-admin/users/"+d.eastRepID+"/access-rules", d.admin, map[string]any{"rules": []map[string]string{
			member(stage["WON"], "read"), member(stage["LOST"], "hidden")}})
		east := columns(d.eastRep, deals, "stage")
		d.call("PUT", "/api/business-admin/users/"+d.eastRepID+"/access-rules", d.admin, map[string]any{"rules": []map[string]string{}})
		if len(east) != 3 || east[2]["key"] != "WON" || east[2]["read_only"] != true {
			t.Errorf("east's stage columns = %v, want QUAL MEET WON (read-only), LOST hidden", east)
		}
		if got := columns(d.westRep, tasks, "state"); len(got) != 2 || got[0]["key"] != "Open" {
			t.Errorf("state columns = %v, want Open, Completed", got)
		}
	})

	t.Run("relative dates count from the asker's day", func(t *testing.T) {
		create(d.westRep, tasks, map[string]any{"title": "Late", "due": "2026-01-05"})
		create(d.westRep, tasks, map[string]any{"title": "Later", "due": "2026-02-05"})
		res := d.call("POST", "/api/forms/"+tasks+"/records/query", d.westRep, map[string]any{
			"filters": []map[string]any{{"field": "due", "op": "before_today"}, {"field": "state", "op": "ne", "value": "Completed"}}, "today": "2026-02-01"})
		if res["total"] != float64(1) {
			t.Errorf("tasks overdue on 1 Feb = %v, want 1", res["total"])
		}
	})

	t.Run("a date posts into the period holding it", func(t *testing.T) {
		valueMetric := idOf(d.call("POST", "/api/developer/metrics", d.dev, map[string]any{"name": "crm_pipeline", "is_input": true,
			"revision_id": d.revID, "agg_rule": "sum", "format": "number"}))
		d.call("POST", "/api/developer/form-integrations", d.dev, map[string]any{
			"form_id": deals, "grid_id": "", "name": "Pipeline", "source_field": "value", "target_metric_id": valueMetric,
			"aggregation": "sum", "dimension_mappings": map[string]string{stageDim: "stage", month: "close"}, "live_posting": true,
		})
		create(d.westRep, deals, map[string]any{"name": "Dated", "account": globex.Data["code"], "stage": "QUAL", "value": 70, "close": "2026-03-15"})
		var monthCode string
		deadline := time.Now().Add(10 * time.Second)
		for monthCode == "" && time.Now().Before(deadline) {
			_ = d.pool.QueryRow(ctx, `SELECT dim_members->>$2 FROM runtime.fact_input WHERE metric_id=$1::uuid AND value = 70`, valueMetric, month).Scan(&monthCode)
			time.Sleep(50 * time.Millisecond)
		}
		if monthCode != "2026-03" {
			t.Errorf("a deal closing 15 March posted into month %q, want 2026-03", monthCode)
		}
	})

	// A revision made wholesale — copied, or imported from a package — has
	// its records but not their postings, which a table's totals are rebuilt
	// from: the first deal saved there rebuilt the pipeline from that one
	// deal, and every other deal's value left the grid (found 2026-10-18).
	t.Run("a revision copy and an imported model keep every record's posting", func(t *testing.T) {
		total := func(revID string) float64 {
			var v float64
			_ = d.pool.QueryRow(ctx, `
				SELECT COALESCE(SUM(fi.value), 0) FROM runtime.fact_input fi JOIN model.metric_def m ON m.id = fi.metric_id
				WHERE fi.revision_id=$1::uuid AND m.name='crm_pipeline' AND fi.source_ref IS NOT NULL`, revID).Scan(&v)
			return v
		}
		settle := func(revID string, want float64) float64 {
			t.Helper()
			deadline := time.Now().Add(15 * time.Second)
			for total(revID) != want && time.Now().Before(deadline) {
				time.Sleep(50 * time.Millisecond)
			}
			return total(revID)
		}
		// Every deal of the source posted (a sync), its total settled.
		d.call("POST", "/api/forms/"+deals+"/sync", d.dev, nil)
		want := total(d.revID)
		for still := 0; still < 10; time.Sleep(50 * time.Millisecond) {
			if now := total(d.revID); now == want {
				still++
			} else {
				want, still = now, 0
			}
		}
		if want == 0 {
			t.Fatal("the source pipeline holds nothing")
		}
		globexCode := globex.Data["code"]
		addDeal := func(persona, formID string) {
			t.Helper()
			create(persona, formID, map[string]any{"name": "After the copy", "account": globexCode, "stage": "QUAL", "value": 5, "close": "2026-04-01"})
		}
		formIn := func(revID string) string {
			var id string
			_ = d.pool.QueryRow(ctx, `SELECT id::text FROM model.form_def WHERE revision_id=$1::uuid AND name='deals'`, revID).Scan(&id)
			return id
		}

		copyRev := idOf(d.call("POST", "/api/developer/revisions", d.dev, map[string]any{"name": "Postings copy", "source_revision_id": d.revID}))
		if got := settle(copyRev, want); got != want {
			t.Fatalf("copied pipeline = %v, want the source's %v", got, want)
		}
		addDeal(d.dev, formIn(copyRev))
		if got := settle(copyRev, want+5); got != want+5 {
			t.Errorf("copied pipeline after one more deal = %v, want %v: the copy's other deals left the grid", got, want+5)
		}

		// The same model, exported with its data and imported by a tenant admin.
		tenantAdmin := "demo-tenant-admin"
		var taID string
		if err := d.pool.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
			VALUES ($1, 'ta@demo.co', 'Tess Admin', $2::uuid) RETURNING id::text`, tenantAdmin, d.custID).Scan(&taID); err != nil {
			t.Fatal(err)
		}
		for _, role := range []string{"tenant_admin", "developer"} {
			if _, err := d.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, $2::identity.user_role, $3::uuid)`,
				taID, role, d.wsID); err != nil {
				t.Fatal(err)
			}
		}
		pkg := d.call("GET", "/api/admin/models/"+d.modelID+"/export?revision_id="+d.revID, tenantAdmin, nil)
		app := idOf(d.call("POST", "/api/admin/applications", tenantAdmin, map[string]any{"customer_id": d.custID, "name": "Imported CRM", "mode": "planning"}))
		imported := d.call("POST", "/api/admin/models/import", tenantAdmin, map[string]any{"application_id": app, "package": pkg})
		impRev, _ := imported["revision_id"].(string)
		if got := settle(impRev, want); got != want {
			t.Fatalf("imported pipeline = %v, want the source's %v", got, want)
		}
		addDeal(tenantAdmin, formIn(impRev))
		if got := settle(impRev, want+5); got != want+5 {
			t.Errorf("imported pipeline after one more deal = %v, want %v: the imported deals left the grid", got, want+5)
		}
	})

	t.Run("a revision copy keeps the tables' settings", func(t *testing.T) {
		newRev := idOf(d.call("POST", "/api/developer/revisions", d.dev, map[string]any{"name": "Copy", "source_revision_id": d.revID}))
		var editAccess, keptName, labelField string
		if err := d.pool.QueryRow(ctx, `
			SELECT f.edit_access, d.name, f.member_label_field FROM model.form_def f
			JOIN model.dimension_def d ON d.id = f.keeps_dimension_id
			WHERE f.revision_id=$1::uuid AND f.name='deals'`, newRev).Scan(&editAccess, &keptName, &labelField); err != nil {
			t.Fatalf("the copied deals table: %v", err)
		}
		var keptRevision string
		_ = d.pool.QueryRow(ctx, `SELECT d.revision_id::text FROM model.form_def f JOIN model.dimension_def d ON d.id = f.keeps_dimension_id
			WHERE f.revision_id=$1::uuid AND f.name='deals'`, newRev).Scan(&keptRevision)
		if editAccess != "everyone" || keptName != "crm_deal" || labelField != "name" || keptRevision != newRev {
			t.Errorf("copied deals: edit_access %q, keeps %q of revision %s (want %s), label %q", editAccess, keptName, keptRevision, newRev, labelField)
		}
	})

	t.Run("a kept dimension and its model can still be deleted", func(t *testing.T) {
		status, raw := d.req("DELETE", "/api/developer/dimensions/"+contactDim, d.dev, nil)
		t.Logf("deleting a kept dimension: %d %s", status, raw)
		if status != http.StatusOK && status != http.StatusConflict {
			t.Errorf("deleting a kept dimension: %d %s, want deleted or a clear refusal", status, raw)
		}
		if status == http.StatusOK {
			var keeps *string
			_ = d.pool.QueryRow(ctx, `SELECT keeps_dimension_id::text FROM model.form_def WHERE id=$1::uuid`, contacts).Scan(&keeps)
			if keeps != nil {
				t.Errorf("the contacts table still keeps the deleted dimension %s", *keeps)
			}
		}
		if _, err := d.pool.Exec(ctx, `DELETE FROM core.model WHERE id=$1::uuid`, d.modelID); err != nil {
			t.Errorf("deleting the model with its kept dimensions: %v", err)
		}
	})
}

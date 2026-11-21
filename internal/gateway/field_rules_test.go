package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Field rules, a person field and a table's delete access (2026-11-20, help
// desk), over HTTP as the people who use them: a ticket's status moves only
// by its transitions (Closed → Open only for managers and administrators),
// a closed ticket is read-only but for its status, a resolution is required
// once resolved and the resolved date is the server's, the assignee is a
// holder of Agents and is told, only administrators delete, a save that
// sends its read time is not written over a change made since, and an
// inactive member is not chosen again.
func TestHelpDeskFieldRules(t *testing.T) {
	f := setupRecordFixture(t)
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var out string
		if err := f.pool.QueryRow(ctx, sql, args...).Scan(&out); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return out
	}
	wsID := q(`SELECT workspace_id::text FROM core.application WHERE id = $1::uuid`, f.appID)
	role := func(name string, members ...string) {
		t.Helper()
		roleID := q(`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, $2) RETURNING id::text`, wsID, name)
		for _, m := range members {
			if _, err := f.pool.Exec(ctx, `INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid, $2::uuid)`, roleID, f.userID[m]); err != nil {
				t.Fatal(err)
			}
		}
	}
	role("Agents", prOwner)
	role("Support managers")
	if _, err := f.pool.Exec(ctx, `UPDATE identity."user" SET display_name = 'Olga Agent' WHERE id = $1::uuid`, f.userID[prOwner]); err != nil {
		t.Fatal(err)
	}

	f.expect(http.StatusOK, http.MethodPatch, "/api/forms/"+f.formID, prDev, map[string]any{
		"name": "tickets", "label": "Tickets", "edit_access": "everyone", "delete_access": "admins",
		"fields": []map[string]any{
			{"name": "code", "label": "Ticket", "type": "autocode", "code_prefix": "TKT-", "code_digits": 6},
			{"name": "title", "label": "Title", "type": "text", "required": true},
			{"name": "state", "label": "Status", "type": "select", "required": true, "history": true,
				"options": []string{"New", "Open", "Pending", "Resolved", "Closed"},
				"transitions": []map[string]any{
					{"from": []string{"New"}, "to": []string{"Open", "Resolved"}},
					{"from": []string{"Open"}, "to": []string{"Pending", "Resolved"}},
					{"from": []string{"Pending"}, "to": []string{"Open", "Resolved"}},
					{"from": []string{"Resolved"}, "to": []string{"Open", "Closed"}},
					{"from": []string{"Closed"}, "to": []string{"Open"}, "roles": []string{"Support managers"}},
				},
				"locks_in": []string{"Closed"}},
			{"name": "resolution", "label": "Resolution", "type": "longtext", "required_when": map[string]any{"field": "state", "is": []string{"Resolved", "Closed"}}},
			{"name": "resolved_on", "label": "Resolved on", "type": "date", "stamp_when": map[string]any{"field": "state", "is": []string{"Resolved"}}},
			{"name": "assignee", "label": "Assignee", "type": "person", "roles": []string{"Agents"}, "notify": true},
		},
	})

	type record struct {
		ID          string         `json:"id"`
		Data        map[string]any `json:"data"`
		UpdatedAt   string         `json:"updated_at"`
		Permissions struct {
			Edit     bool                `json:"edit"`
			Delete   bool                `json:"delete"`
			Moves    map[string][]string `json:"moves"`
			LockedBy string              `json:"locked_by"`
		} `json:"permissions"`
	}
	var tkt record
	if err := json.Unmarshal(f.expect(http.StatusOK, http.MethodPost, "/api/forms/"+f.formID+"/records", prPeer, map[string]any{
		"data": map[string]any{"title": "Printer offline", "state": "New", "assignee": f.userID[prOwner]},
	}), &tkt); err != nil {
		t.Fatal(err)
	}
	if tkt.Data["code"] != "TKT-000001" {
		t.Errorf("the ticket's number: %v", tkt.Data["code"])
	}
	var told int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM notification.notification
		WHERE recipient_user_id = $1::uuid AND template_id = 'record_person_named' AND resource_id = $2 AND channel = 'in_app'`,
		f.userID[prOwner], tkt.ID).Scan(&told); err != nil || told != 1 {
		t.Errorf("the assignee is told once: %d (%v)", told, err)
	}
	if status, raw := f.do(http.MethodPost, "/api/forms/"+f.formID+"/records", prPeer, map[string]any{
		"data": map[string]any{"title": "x", "state": "New", "assignee": f.userID[prPeer]},
	}); status != http.StatusBadRequest || !strings.Contains(string(raw), "cannot name this person") {
		t.Errorf("an assignee outside Agents: %d %s", status, raw)
	}

	get := func(persona string) record {
		t.Helper()
		var r record
		if err := json.Unmarshal(f.expect(http.StatusOK, http.MethodGet, "/api/records/"+tkt.ID, persona, nil), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	put := func(persona string, fields map[string]any) (int, string) {
		t.Helper()
		status, raw := f.do(http.MethodPut, "/api/records/"+tkt.ID, persona, map[string]any{"fields": fields})
		return status, string(raw)
	}
	if got := get(prPeer).Permissions.Moves["state"]; strings.Join(got, ",") != "Open,Resolved" {
		t.Errorf("the moves served from New: %v", got)
	}
	if s, raw := put(prPeer, map[string]any{"state": "Closed"}); s != http.StatusBadRequest || !strings.Contains(raw, "cannot move from New to Closed") {
		t.Errorf("New → Closed: %d %s", s, raw)
	}
	if s, raw := put(prPeer, map[string]any{"state": "Resolved"}); s != http.StatusBadRequest || !strings.Contains(raw, "Resolution is required when Status is Resolved or Closed") {
		t.Errorf("resolved without a resolution: %d %s", s, raw)
	}
	if s, raw := put(prPeer, map[string]any{"state": "Resolved", "resolution": "Cable replaced", "resolved_on": "1999-01-01"}); s != http.StatusOK {
		t.Fatalf("resolved: %d %s", s, raw)
	}
	today := time.Now().UTC().Format("2006-01-02")
	if got := get(prPeer).Data["resolved_on"]; got != today {
		t.Errorf("the server stamps the resolved date: %v, want %s", got, today)
	}
	if s, raw := put(prPeer, map[string]any{"state": "Closed"}); s != http.StatusOK {
		t.Fatalf("closed: %d %s", s, raw)
	}
	closed := get(prPeer)
	if closed.Permissions.LockedBy != "state" || len(closed.Permissions.Moves["state"]) != 0 || closed.Permissions.Delete {
		t.Errorf("a closed ticket for an agent: %+v", closed.Permissions)
	}
	if s, raw := put(prPeer, map[string]any{"title": "Scanner"}); s != http.StatusBadRequest || !strings.Contains(raw, "read-only while its Status is Closed") {
		t.Errorf("a closed ticket's title: %d %s", s, raw)
	}
	if s, raw := put(prPeer, map[string]any{"state": "Open"}); s != http.StatusBadRequest || !strings.Contains(raw, "only Support managers move Status from Closed to Open") {
		t.Errorf("an agent reopens a closed ticket: %d %s", s, raw)
	}
	if got := get(prBA).Permissions.Moves["state"]; strings.Join(got, ",") != "Open" {
		t.Errorf("an administrator may reopen: %v", got)
	}
	if s, raw := put(prBA, map[string]any{"state": "Open"}); s != http.StatusOK {
		t.Fatalf("an administrator reopens: %d %s", s, raw)
	}

	// A save naming the time it read the record is not written over a
	// change made since; one naming none saves as before.
	stale := get(prPeer).UpdatedAt
	if s, raw := put(prPeer, map[string]any{"title": "Printer offline (floor 2)"}); s != http.StatusOK {
		t.Fatalf("a change in between: %d %s", s, raw)
	}
	if s, raw := f.do(http.MethodPut, "/api/records/"+tkt.ID, prOwner, map[string]any{"fields": map[string]any{"title": "Mine"}, "expected_updated_at": stale}); s != http.StatusConflict ||
		!strings.Contains(string(raw), "someone changed this record since you opened it") {
		t.Errorf("a save over a change since: %d %s", s, raw)
	}
	fresh := get(prOwner).UpdatedAt
	var saved struct {
		Record record `json:"record"`
	}
	s, raw := f.do(http.MethodPut, "/api/records/"+tkt.ID, prOwner, map[string]any{"fields": map[string]any{"title": "Mine"}, "expected_updated_at": fresh})
	if s != http.StatusOK || json.Unmarshal(raw, &saved) != nil || saved.Record.UpdatedAt == fresh || saved.Record.UpdatedAt == "" {
		t.Errorf("a save of the record as read: %d %s", s, raw)
	}

	// Only administrators delete; the agents may not.
	if s, _ := f.do(http.MethodDelete, "/api/records/"+tkt.ID, prPeer, nil); s != http.StatusForbidden {
		t.Errorf("an agent deletes a ticket: %d", s)
	}

	// The picker's people: names, no e-mail addresses.
	var choices map[string]any
	rawChoices := f.expect(http.StatusOK, http.MethodGet, "/api/forms/"+f.formID+"/choices", prPeer, nil)
	_ = json.Unmarshal(rawChoices, &choices)
	if !strings.Contains(string(rawChoices), `"name":"Olga Agent"`) || strings.Contains(string(rawChoices), "@") {
		t.Errorf("the people the assignee may name: %s", rawChoices)
	}

	// Revision copies and model packages keep the table's delete access.
	f.expect(http.StatusOK, http.MethodPost, "/api/developer/revisions", prDev, map[string]any{"name": "Copy", "source_revision_id": f.revID, "model_id": f.modelID})
	if got := q(`SELECT delete_access FROM model.form_def WHERE model_id = $1::uuid AND name = 'tickets' AND revision_id <> $2::uuid`, f.modelID, f.revID); got != "admins" {
		t.Errorf("the copy's delete access: %q", got)
	}

	if s, _ := f.do(http.MethodDelete, "/api/records/"+tkt.ID, prBA, nil); s != http.StatusOK {
		t.Errorf("an administrator deletes a ticket: %d", s)
	}
}

// An inactive member (its keeping table's active flag unticked) stays on the
// records naming it and is refused for a new choice.
func TestInactiveMemberIsNotChosenAgain(t *testing.T) {
	f := setupRecordFixture(t)
	var dim map[string]any
	_ = json.Unmarshal(f.expect(http.StatusOK, http.MethodPost, "/api/developer/dimensions", prDev, map[string]any{"name": "hd_category", "revision_id": f.revID}), &dim)
	dimID, _ := dim["id"].(string)
	var cats map[string]any
	_ = json.Unmarshal(f.expect(http.StatusOK, http.MethodPost, "/api/forms", prDev, map[string]any{
		"name": "categories", "label": "Categories", "revision_id": f.revID, "keeps_dimension_id": dimID, "member_label_field": "name", "edit_access": "everyone",
		"fields": []map[string]any{
			{"name": "code", "label": "Code", "type": "autocode", "code_prefix": "CAT-"},
			{"name": "name", "label": "Name", "type": "text", "required": true},
			{"name": "active", "label": "Active", "type": "boolean", "default": true, "active_flag": true},
		},
	}), &cats)
	catsID, _ := cats["id"].(string)
	add := func(name string) map[string]any {
		var r map[string]any
		_ = json.Unmarshal(f.expect(http.StatusOK, http.MethodPost, "/api/forms/"+catsID+"/records", prBA, map[string]any{"data": map[string]any{"name": name}}), &r)
		return r
	}
	hw, net := add("Hardware"), add("Network")
	hwCode := hw["data"].(map[string]any)["code"].(string)
	f.expect(http.StatusOK, http.MethodPatch, "/api/forms/"+f.formID, prDev, map[string]any{
		"name": "tickets", "label": "Tickets", "edit_access": "everyone",
		"fields": []map[string]any{
			{"name": "title", "label": "Title", "type": "text"},
			{"name": "category", "label": "Category", "type": "dimension", "dimension_id": dimID},
		},
	})
	var tkt map[string]any
	_ = json.Unmarshal(f.expect(http.StatusOK, http.MethodPost, "/api/forms/"+f.formID+"/records", prPeer, map[string]any{"data": map[string]any{"title": "a", "category": hwCode}}), &tkt)
	f.expect(http.StatusOK, http.MethodPut, "/api/records/"+hw["id"].(string), prBA, map[string]any{"fields": map[string]any{"active": false}})

	if s, raw := f.do(http.MethodPost, "/api/forms/"+f.formID+"/records", prPeer, map[string]any{"data": map[string]any{"title": "b", "category": hwCode}}); s != http.StatusBadRequest || !strings.Contains(string(raw), "is inactive") {
		t.Errorf("a new ticket in an inactive category: %d %s", s, raw)
	}
	// The ticket naming it keeps it through a change of its other fields.
	f.expect(http.StatusOK, http.MethodPut, "/api/records/"+tkt["id"].(string), prPeer, map[string]any{"fields": map[string]any{"title": "a2"}})
	raw := f.expect(http.StatusOK, http.MethodGet, "/api/forms/"+f.formID+"/choices", prPeer, nil)
	if !strings.Contains(string(raw), hwCode) || strings.Contains(string(raw), net["data"].(map[string]any)["code"].(string)) {
		t.Errorf("the choices name the inactive member only: %s", raw)
	}
}

// A ticket's assignee names an agent record; the agent's person is told
// (notify on a dimension field). A person field filters as a person: "me".
func TestNotifyThroughNamedRecordAndMeFilter(t *testing.T) {
	f := setupRecordFixture(t)
	ctx := context.Background()
	var dim map[string]any
	_ = json.Unmarshal(f.expect(http.StatusOK, http.MethodPost, "/api/developer/dimensions", prDev, map[string]any{"name": "hd_agent", "revision_id": f.revID}), &dim)
	dimID, _ := dim["id"].(string)
	var agents map[string]any
	_ = json.Unmarshal(f.expect(http.StatusOK, http.MethodPost, "/api/forms", prDev, map[string]any{
		"name": "agents", "label": "Agents", "revision_id": f.revID, "keeps_dimension_id": dimID, "member_label_field": "name", "edit_access": "everyone",
		"fields": []map[string]any{
			{"name": "code", "label": "Code", "type": "autocode", "code_prefix": "AGT-"},
			{"name": "name", "label": "Name", "type": "text", "required": true},
			{"name": "person", "label": "Person", "type": "person"},
		},
	}), &agents)
	agentsID, _ := agents["id"].(string)
	var agent map[string]any
	_ = json.Unmarshal(f.expect(http.StatusOK, http.MethodPost, "/api/forms/"+agentsID+"/records", prBA, map[string]any{
		"data": map[string]any{"name": "Olga", "person": f.userID[prOwner]}}), &agent)
	agentCode := agent["data"].(map[string]any)["code"].(string)
	f.expect(http.StatusOK, http.MethodPatch, "/api/forms/"+f.formID, prDev, map[string]any{
		"name": "tickets", "label": "Tickets", "edit_access": "everyone",
		"fields": []map[string]any{
			{"name": "title", "label": "Title", "type": "text"},
			{"name": "assignee", "label": "Assignee", "type": "dimension", "dimension_id": dimID, "notify": true},
			{"name": "watcher", "label": "Watcher", "type": "person"},
		},
	})
	var tkt map[string]any
	_ = json.Unmarshal(f.expect(http.StatusOK, http.MethodPost, "/api/forms/"+f.formID+"/records", prPeer, map[string]any{
		"data": map[string]any{"title": "a", "assignee": agentCode, "watcher": f.userID[prOwner]}}), &tkt)
	var told int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM notification.notification
		WHERE recipient_user_id = $1::uuid AND template_id = 'record_person_named' AND resource_id = $2`, f.userID[prOwner], tkt["id"]).Scan(&told); err != nil || told != 1 {
		t.Errorf("the agent's person is told once: %d (%v)", told, err)
	}
	f.expect(http.StatusOK, http.MethodPost, "/api/forms/"+f.formID+"/records", prPeer, map[string]any{"data": map[string]any{"title": "b"}})
	var res struct {
		Total int `json:"total"`
	}
	_ = json.Unmarshal(f.expect(http.StatusOK, http.MethodPost, "/api/forms/"+f.formID+"/records/query", prOwner, map[string]any{
		"filters": []map[string]any{{"field": "watcher", "op": "eq", "value": "me"}}}), &res)
	if res.Total != 1 {
		t.Errorf("records whose watcher is me: %d, want 1", res.Total)
	}
}

// record_scope "open" (owner decision 2026-11-21): a table scoped by members'
// access refuses moving a record into a member its creator may not see, even
// for a manager; an open table lets everyone see every record, checks a save
// against the saver, and gives a record naming a member hidden from a person
// to them to read only. existing_codes: a new table refuses a file row naming
// its auto code; "update" updates the record holding it.
func TestOpenRecordScopeAndExistingCodes(t *testing.T) {
	f := setupRecordFixture(t)
	ctx := context.Background()
	var dim map[string]any
	_ = json.Unmarshal(f.expect(http.StatusOK, http.MethodPost, "/api/developer/dimensions", prDev, map[string]any{"name": "hd_team", "revision_id": f.revID}), &dim)
	dimID, _ := dim["id"].(string)
	for _, c := range []string{"SD", "APPS"} {
		f.expect(http.StatusOK, http.MethodPost, "/api/developer/dimensions/"+dimID+"/members", prDev, map[string]any{"code": c, "label": c})
	}
	var tickets map[string]any
	_ = json.Unmarshal(f.expect(http.StatusOK, http.MethodPost, "/api/forms", prDev, map[string]any{
		"name": "tickets2", "label": "Tickets", "revision_id": f.revID, "edit_access": "everyone",
		"fields": []map[string]any{
			{"name": "code", "label": "Ticket", "type": "autocode", "code_prefix": "T-"},
			{"name": "title", "label": "Title", "type": "text"},
			{"name": "team", "label": "Team", "type": "dimension", "dimension_id": dimID},
		},
	}), &tickets)
	tk, _ := tickets["id"].(string)
	if tickets["existing_codes"] != "refuse" {
		t.Errorf("a new table refuses file rows naming its code: %v", tickets["existing_codes"])
	}
	// prPeer's role hides APPS; prBA (business admin) sees both.
	wsID := ""
	_ = f.pool.QueryRow(ctx, `SELECT workspace_id::text FROM core.application WHERE id = $1::uuid`, f.appID).Scan(&wsID)
	var roleID, appsID string
	_ = f.pool.QueryRow(ctx, `INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Service Desk team') RETURNING id::text`, wsID).Scan(&roleID)
	_, _ = f.pool.Exec(ctx, `INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid, $2::uuid)`, roleID, f.userID[prPeer])
	_ = f.pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_member WHERE dimension_id = $1::uuid AND code = 'APPS'`, dimID).Scan(&appsID)
	f.expect(http.StatusOK, http.MethodPut, "/api/business-admin/roles/"+roleID+"/access-rules", prBA, map[string]any{
		"rules": []map[string]any{{"rule_type": "dimension_member", "ref_id": appsID, "access": "hidden"}}})

	// The records are counted into a grid by team, as a help desk's backlog.
	var grid, metric map[string]any
	_ = json.Unmarshal(f.expect(http.StatusOK, http.MethodPost, "/api/developer/grids", prDev, map[string]any{"name": "Backlog", "revision_id": f.revID}), &grid)
	gridID, _ := grid["id"].(string)
	f.expect(http.StatusOK, http.MethodPost, "/api/developer/grids/"+gridID+"/dimensions/"+dimID, prDev, nil)
	_ = json.Unmarshal(f.expect(http.StatusOK, http.MethodPost, "/api/developer/metrics", prDev, map[string]any{
		"name": "open_tickets", "label": "Open tickets", "is_input": true, "revision_id": f.revID, "format": "number"}), &metric)
	metricID, _ := metric["id"].(string)
	f.expect(http.StatusOK, http.MethodPost, "/api/developer/grids/"+gridID+"/metrics/"+metricID, prDev, nil)
	f.expect(http.StatusOK, http.MethodPost, "/api/developer/form-integrations", prDev, map[string]any{
		"form_id": tk, "grid_id": gridID, "name": "count", "source_field": "code", "target_metric_id": metricID,
		"aggregation": "count", "dimension_mappings": map[string]string{dimID: "team"}, "live_posting": true, "posting_statuses": []string{""}})

	var rec map[string]any
	_ = json.Unmarshal(f.expect(http.StatusOK, http.MethodPost, "/api/forms/"+tk+"/records", prPeer, map[string]any{"data": map[string]any{"title": "x", "team": "SD"}}), &rec)
	id := rec["id"].(string)
	if s, raw := f.do(http.MethodPut, "/api/records/"+id, prBA, map[string]any{"fields": map[string]any{"team": "APPS"}}); s != http.StatusForbidden || !strings.Contains(string(raw), "its creator cannot change") {
		t.Errorf("scoped: moving the agent's record into a team hidden from them: %d %s", s, raw)
	}
	settings := func(body map[string]any) {
		t.Helper()
		body["name"], body["label"] = "tickets2", "Tickets"
		f.expect(http.StatusOK, http.MethodPatch, "/api/forms/"+tk, prDev, body)
	}
	settings(map[string]any{"record_scope": "open"})
	if s, raw := f.do(http.MethodPut, "/api/records/"+id, prBA, map[string]any{"fields": map[string]any{"team": "APPS"}}); s != http.StatusOK {
		t.Errorf("open: a business admin moves it to APPS: %d %s", s, raw)
	}
	// Its count moves with it: the posting is written within the saver's access too.
	var where string
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		_ = f.pool.QueryRow(ctx, `SELECT COALESCE(string_agg(p.dim_members::text, ','), '') FROM runtime.form_record_posting p WHERE p.form_record_id = $1::uuid`, id).Scan(&where)
		if strings.Contains(where, "APPS") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.Contains(where, "APPS") {
		t.Errorf("open: the record's posting follows it to APPS: %q", where)
	}
	var got struct {
		Data        map[string]any `json:"data"`
		Permissions struct {
			Edit bool `json:"edit"`
		} `json:"permissions"`
	}
	_ = json.Unmarshal(f.expect(http.StatusOK, http.MethodGet, "/api/records/"+id, prPeer, nil), &got)
	if got.Data["team"] != "APPS" || got.Permissions.Edit {
		t.Errorf("open: the agent sees the record in APPS and may not change it: %+v", got)
	}
	if s, _ := f.do(http.MethodPut, "/api/records/"+id, prPeer, map[string]any{"fields": map[string]any{"title": "y"}}); s != http.StatusForbidden {
		t.Errorf("open: the agent changes a record naming a team hidden from them: %d", s)
	}

	csv := "code,title,team\n" + rec["data"].(map[string]any)["code"].(string) + ",changed,SD\n"
	if s, raw := f.do(http.MethodPost, "/api/forms/"+tk+"/import", prBA, map[string]any{"csv": csv, "dry_run": true}); s != http.StatusOK ||
		!strings.Contains(string(raw), "numbers its new records itself") {
		t.Errorf("refuse: a row naming a code: %d %s", s, raw)
	}
	settings(map[string]any{"existing_codes": "update"})
	if s, raw := f.do(http.MethodPost, "/api/forms/"+tk+"/import", prBA, map[string]any{"csv": csv}); s != http.StatusOK {
		t.Fatalf("update: %d %s", s, raw)
	}
	_ = json.Unmarshal(f.expect(http.StatusOK, http.MethodGet, "/api/records/"+id, prBA, nil), &got)
	var n int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.form_record WHERE form_id = $1::uuid`, tk).Scan(&n)
	if got.Data["title"] != "changed" || n != 1 {
		t.Errorf("update: the row updates the record (title %v, %d records)", got.Data["title"], n)
	}
	// A table made before the choice ignores the code and adds a record, as before.
	if _, err := f.pool.Exec(ctx, `UPDATE model.form_def SET existing_codes = '' WHERE id = $1::uuid`, tk); err != nil {
		t.Fatal(err)
	}
	if s, raw := f.do(http.MethodPost, "/api/forms/"+tk+"/import", prBA, map[string]any{"csv": csv}); s != http.StatusOK {
		t.Fatalf("legacy: %d %s", s, raw)
	}
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.form_record WHERE form_id = $1::uuid`, tk).Scan(&n)
	if n != 2 {
		t.Errorf("legacy: the row adds a record: %d", n)
	}
}

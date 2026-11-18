package gateway

// Changing a record from a chat connection (update_record → PUT
// /api/records/{id} with fields): as the person, under the console's record
// checks, only the fields named, only for a token that carries the write
// scope, and only while the tenant allows changes from chat connections.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestChatUpdatesRecords(t *testing.T) {
	f := setupConnector(t)
	ctx := context.Background()
	west := f.as(f.westRep)
	reed := f.as(f.ro)

	formID := idOf(f.call("POST", "/api/forms", f.dev, map[string]any{
		"name": "deal", "label": "Deal", "fields": []map[string]any{
			{"name": "region", "label": "Region", "type": "dimension", "dimension_id": f.geoDim},
			{"name": "amount", "label": "Amount", "type": "number"},
			{"name": "stage", "label": "Stage", "type": "select", "options": []string{"Lead", "Proposal", "Won"}},
			{"name": "note", "label": "Note", "type": "text"},
			{"name": "status", "label": "Status", "type": "status"},
		},
	}))
	otherID := idOf(f.call("POST", "/api/forms", f.dev, map[string]any{
		"name": "visit", "label": "Visit", "fields": []map[string]any{{"name": "note", "label": "Note", "type": "text"}},
	}))
	dealID := idOf(f.call("POST", "/api/forms/"+formID+"/records", f.westRep, map[string]any{
		"status": "draft", "data": map[string]any{"region": "UK", "amount": 100, "stage": "Lead", "note": "first call"}}))
	visitID := idOf(f.call("POST", "/api/forms/"+otherID+"/records", f.westRep, map[string]any{"data": map[string]any{"note": "x"}}))
	if dealID == "" || visitID == "" {
		t.Fatal("records were not created")
	}

	update := func(c *mcpClient, args map[string]any) (map[string]any, string) {
		t.Helper()
		if _, ok := args["table_id"]; !ok {
			args["table_id"] = formID
		}
		if _, ok := args["record_id"]; !ok {
			args["record_id"] = dealID
		}
		return c.call("update_record", f.ctxArgs(args))
	}
	stored := func(t *testing.T) (map[string]any, string) {
		t.Helper()
		var raw []byte
		var status string
		if err := f.pool.QueryRow(ctx, `SELECT data, COALESCE(status::text, '') FROM runtime.form_record WHERE id = $1::uuid`, dealID).Scan(&raw, &status); err != nil {
			t.Fatal(err)
		}
		var data map[string]any
		_ = json.Unmarshal(raw, &data)
		return data, status
	}

	t.Run("a dry run checks the change and saves nothing", func(t *testing.T) {
		res, errText := update(west, map[string]any{"fields": map[string]any{"amount": 250}, "dry_run": true})
		if errText != "" || res["status"] != "valid" {
			t.Fatalf("dry run: %v %q", res, errText)
		}
		if v := res["record"].(map[string]any)["values"].(map[string]any)["amount"]; v != 250.0 {
			t.Errorf("the dry run's record shows amount %v, want 250 as it would be saved", v)
		}
		if data, _ := stored(t); data["amount"] != 100.0 {
			t.Errorf("amount after a dry run = %v, want 100", data["amount"])
		}
	})

	t.Run("only the fields named change", func(t *testing.T) {
		res, errText := update(west, map[string]any{"fields": map[string]any{"amount": 250, "stage": "Proposal"}})
		if errText != "" || res["status"] != "updated" {
			t.Fatalf("update: %v %q", res, errText)
		}
		data, status := stored(t)
		if data["amount"] != 250.0 || data["stage"] != "Proposal" || data["region"] != "UK" || data["note"] != "first call" || status != "draft" {
			t.Errorf("stored = %v %s, want amount 250 and stage Proposal with region, note and status kept", data, status)
		}
	})

	t.Run("a field the table does not have is refused", func(t *testing.T) {
		_, errText := update(west, map[string]any{"fields": map[string]any{"amout": 1}})
		if !strings.HasPrefix(errText, "invalid_request") || !strings.Contains(errText, "amout: not a field of Deal") {
			t.Errorf("unknown field: %q", errText)
		}
	})

	t.Run("a value the field refuses saves nothing", func(t *testing.T) {
		_, errText := update(west, map[string]any{"fields": map[string]any{"stage": "Lost", "amount": 1}})
		if !strings.HasPrefix(errText, "refused: nothing was saved") || !strings.Contains(errText, "stage") {
			t.Errorf("a stage not among the options: %q", errText)
		}
		if data, _ := stored(t); data["amount"] != 250.0 {
			t.Errorf("amount after a refused change = %v, want 250", data["amount"])
		}
	})

	t.Run("a record of another table is not this table's", func(t *testing.T) {
		if _, errText := update(west, map[string]any{"record_id": visitID, "fields": map[string]any{"note": "y"}}); !strings.HasPrefix(errText, "not_found") {
			t.Errorf("a visit through the deal table: %q", errText)
		}
		if _, errText := update(west, map[string]any{"table_id": "00000000-0000-0000-0000-000000000000", "fields": map[string]any{"note": "y"}}); !strings.HasPrefix(errText, "not_found") {
			t.Errorf("an unknown table: %q", errText)
		}
	})

	t.Run("status moves are the console's", func(t *testing.T) {
		res, errText := update(west, map[string]any{"status": "submitted"})
		if errText != "" || res["record"].(map[string]any)["status"] != "submitted" {
			t.Fatalf("the creator submits: %v %q", res, errText)
		}
		if _, errText := update(west, map[string]any{"status": "approved"}); !strings.HasPrefix(errText, "refused: nothing was saved") ||
			!strings.Contains(errText, "from submitted to approved") {
			t.Errorf("the creator approves: %q", errText)
		}
		if _, status := stored(t); status != "submitted" {
			t.Errorf("status = %s, want submitted", status)
		}
	})

	t.Run("someone else's record is refused", func(t *testing.T) {
		if _, errText := update(reed, map[string]any{"fields": map[string]any{"note": "mine now"}}); !strings.HasPrefix(errText, "refused: nothing was saved") {
			t.Errorf("Reed changes Wes's deal: %q", errText)
		}
	})

	t.Run("the change is the person's, made through the chat", func(t *testing.T) {
		var actor, via string
		if err := f.pool.QueryRow(ctx, `SELECT actor_user_id::text, COALESCE(metadata->>'via', '') FROM audit.audit_event
			WHERE event_type = 'form_record.updated' AND resource_id = $1 ORDER BY occurred_at DESC LIMIT 1`, dealID).Scan(&actor, &via); err != nil {
			t.Fatalf("no form_record.updated audit event: %v", err)
		}
		if actor != f.westRepID || via != "dev" {
			t.Errorf("audit: actor %s via %q, want Wes via the dev client", actor, via)
		}
	})

	t.Run("the console's fields option, and data with fields refused", func(t *testing.T) {
		status, raw := f.req("PUT", "/api/records/"+dealID, f.westRep, map[string]any{"fields": map[string]any{"note": "from the API"}})
		if status != http.StatusOK {
			t.Fatalf("PUT fields: %d %s", status, raw)
		}
		if data, _ := stored(t); data["note"] != "from the API" || data["amount"] != 250.0 {
			t.Errorf("stored = %v, want the note changed and the amount kept", data)
		}
		if status, raw := f.req("PUT", "/api/records/"+dealID, f.westRep, map[string]any{"fields": map[string]any{}, "data": map[string]any{}}); status != http.StatusBadRequest {
			t.Errorf("data and fields together: %d %s", status, raw)
		}
	})

	t.Run("a connection granted reading only changes nothing", func(t *testing.T) {
		ro := connectMCP(t, f.url, "dev-read:"+f.westRep)
		if _, errText := update(ro, map[string]any{"fields": map[string]any{"amount": 1}}); !strings.HasPrefix(errText, "read_only:") {
			t.Errorf("update without the write scope: %q", errText)
		}
	})

	t.Run("a tenant administrator's switch stops chat changes, not the console's", func(t *testing.T) {
		var adminID string
		if err := f.pool.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('demo-record-admin', 'ra@demo.co', 'Rae Admin', $1::uuid) RETURNING id::text`, f.custID).Scan(&adminID); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'tenant_admin', $2::uuid)`, adminID, f.wsID); err != nil {
			t.Fatal(err)
		}
		if status, raw := f.req("PUT", "/api/admin/connector-settings", "demo-record-admin", map[string]any{"chat_writes": false}); status != http.StatusOK {
			t.Fatalf("switch off: %d %s", status, raw)
		}
		if _, errText := update(west, map[string]any{"fields": map[string]any{"amount": 300}}); !strings.Contains(errText, "turned off changing data from chat") {
			t.Errorf("a chat change while switched off: %q", errText)
		}
		if status, raw := f.req("PUT", "/api/records/"+dealID, f.westRep, map[string]any{"fields": map[string]any{"amount": 300}}); status != http.StatusOK {
			t.Errorf("the console's change while chat changes are off: %d %s", status, raw)
		}
		if status, raw := f.req("PUT", "/api/admin/connector-settings", "demo-record-admin", map[string]any{"chat_writes": true}); status != http.StatusOK {
			t.Fatalf("switch on: %d %s", status, raw)
		}
		if _, errText := update(west, map[string]any{"fields": map[string]any{"amount": 310}}); errText != "" {
			t.Errorf("a chat change switched back on: %q", errText)
		}
	})
}

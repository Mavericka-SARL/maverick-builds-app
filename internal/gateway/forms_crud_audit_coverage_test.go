// Forms / CRUD app (/api/forms/..., /api/records/...) was entirely
// unaudited, including record deletion — "data deletion with no trail".
package gateway

import (
	"net/http"
	"testing"
)

func TestFormsCRUDMutationsAreAudited(t *testing.T) {
	f := setupDevAuthoringFixture(t)

	// ── form: create, update, delete ───────────────────────────────────
	status, body := f.do(t, "POST", "/api/forms", map[string]any{
		"name": "expenses", "label": "Expenses",
		"fields": []map[string]any{{"name": "amount", "label": "Amount", "type": "number"}},
	})
	if status != http.StatusOK {
		t.Fatalf("create form: status=%d body=%v", status, body)
	}
	formID, _ := body["id"].(string)
	if formID == "" {
		t.Fatalf("expected form id, got %v", body)
	}
	if row := f.latestAuditEvent(t, "form.created"); row.applicationID == nil || *row.applicationID != f.appID {
		t.Errorf("form.created application_id = %v, want %s", row.applicationID, f.appID)
	}

	// ── record: create, update, delete ─────────────────────────────────
	status, body = f.do(t, "POST", "/api/forms/"+formID+"/records", map[string]any{"data": map[string]any{"amount": 10}})
	if status != http.StatusOK {
		t.Fatalf("create record: status=%d body=%v", status, body)
	}
	recordID, _ := body["id"].(string)
	if recordID == "" {
		t.Fatalf("expected record id, got %v", body)
	}
	if row := f.latestAuditEvent(t, "form_record.created"); row.applicationID == nil || *row.applicationID != f.appID {
		t.Errorf("form_record.created application_id = %v, want %s", row.applicationID, f.appID)
	}

	status, _ = f.do(t, "PUT", "/api/records/"+recordID, map[string]any{"data": map[string]any{"amount": 20}, "status": "submitted"})
	if status != http.StatusOK {
		t.Fatalf("update record: status=%d", status)
	}
	if row := f.latestAuditEvent(t, "form_record.updated"); row.resourceID != recordID {
		t.Errorf("form_record.updated resource_id = %s, want %s", row.resourceID, recordID)
	}

	status, _ = f.do(t, "DELETE", "/api/records/"+recordID, nil)
	if status != http.StatusOK {
		t.Fatalf("delete record: status=%d", status)
	}
	if row := f.latestAuditEvent(t, "form_record.deleted"); row.resourceID != recordID {
		t.Errorf("form_record.deleted resource_id = %s, want %s", row.resourceID, recordID)
	}

	// ── form import ─────────────────────────────────────────────────────
	status, body = f.do(t, "POST", "/api/forms/"+formID+"/import", map[string]string{"csv": "amount\n42\n"})
	if status != http.StatusOK {
		t.Fatalf("import form: status=%d body=%v", status, body)
	}
	f.latestAuditEvent(t, "form.imported")

	// ── form sync ────────────────────────────────────────────────────────
	status, body = f.do(t, "POST", "/api/forms/"+formID+"/sync", nil)
	if status != http.StatusOK {
		t.Fatalf("sync form: status=%d body=%v", status, body)
	}
	f.latestAuditEvent(t, "form.synced")

	// ── form update / delete ────────────────────────────────────────────
	status, _ = f.do(t, "PATCH", "/api/forms/"+formID, map[string]any{
		"name": "expenses", "label": "Expenses v2",
		"fields": []map[string]any{{"name": "amount", "label": "Amount", "type": "number"}},
	})
	if status != http.StatusOK {
		t.Fatalf("update form: status=%d", status)
	}
	f.latestAuditEvent(t, "form.updated")

	status, _ = f.do(t, "DELETE", "/api/forms/"+formID, nil)
	if status != http.StatusOK {
		t.Fatalf("delete form: status=%d", status)
	}
	if row := f.latestAuditEvent(t, "form.deleted"); row.resourceID != formID {
		t.Errorf("form.deleted resource_id = %s, want %s", row.resourceID, formID)
	}
}

// Developer Console workflow-authoring, integration, form-integration, and
// schema-migration surfaces were entirely unaudited before this — including
// workflow publish/archive, the explicit "revision-promotion" security-
// sensitive case the backlog item calls out by name.
package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

func TestDeveloperWorkflowIntegrationMigrationMutationsAreAudited(t *testing.T) {
	f := setupDevAuthoringFixture(t)

	// ── workflow def: create, publish, archive, duplicate, update, delete ──
	status, body := f.do(t, "POST", "/api/developer/workflows?application_id="+f.appID, map[string]string{"name": "Approve Budget", "trigger_event": "manual"})
	if status != http.StatusOK {
		t.Fatalf("create workflow: status=%d body=%v", status, body)
	}
	defID, _ := body["id"].(string)
	if defID == "" {
		t.Fatalf("expected workflow id, got %v", body)
	}
	if row := f.latestAuditEvent(t, "workflow_def.created"); row.applicationID == nil || *row.applicationID != f.appID {
		t.Errorf("workflow_def.created application_id = %v, want %s", row.applicationID, f.appID)
	}

	status, body = f.do(t, "PATCH", "/api/developer/workflows/"+defID, map[string]any{
		"name": "Approve Budget v2", "trigger_event": "manual", "subject_type": "form",
		"steps": []map[string]any{
			{"id": "s1", "name": "Notify", "type": "notification"},
		},
		"context_schema": map[string]any{},
	})
	if status != http.StatusOK {
		t.Fatalf("update workflow: status=%d body=%v", status, body)
	}
	f.latestAuditEvent(t, "workflow_def.updated")

	status, body = f.do(t, "POST", "/api/developer/workflows/"+defID+"/publish", nil)
	if status != http.StatusOK {
		t.Fatalf("publish workflow: status=%d body=%v", status, body)
	}
	if row := f.latestAuditEvent(t, "workflow_def.published"); row.resourceID != defID {
		t.Errorf("workflow_def.published resource_id = %s, want %s", row.resourceID, defID)
	}

	status, body = f.do(t, "POST", "/api/developer/workflows/"+defID+"/archive", nil)
	if status != http.StatusOK {
		t.Fatalf("archive workflow: status=%d body=%v", status, body)
	}
	f.latestAuditEvent(t, "workflow_def.archived")

	status, body = f.do(t, "POST", "/api/developer/workflows/"+defID+"/duplicate", map[string]string{"name": "Approve Budget Copy"})
	if status != http.StatusOK {
		t.Fatalf("duplicate workflow: status=%d body=%v", status, body)
	}
	dupID, _ := body["id"].(string)
	if meta := f.latestAuditMetadata(t, "workflow_def.created"); meta["duplicated_from"] != defID {
		t.Errorf("duplicate created duplicated_from = %q, want %q", meta["duplicated_from"], defID)
	}

	status, _ = f.do(t, "DELETE", "/api/developer/workflows/"+dupID, nil)
	if status != http.StatusOK {
		t.Fatalf("delete workflow: status=%d", status)
	}
	f.latestAuditEvent(t, "workflow_def.deleted")

	// ── integration: create, update config, update, run, delete ───────────
	status, body = f.do(t, "POST", "/api/developer/integrations", map[string]string{"name": "CSV Import", "type": "csv_import", "target_type": "grid"})
	if status != http.StatusOK {
		t.Fatalf("create integration: status=%d body=%v", status, body)
	}
	intID, _ := body["id"].(string)
	if intID == "" {
		t.Fatalf("expected integration id, got %v", body)
	}
	if row := f.latestAuditEvent(t, "integration.created"); row.applicationID == nil || *row.applicationID != f.appID {
		t.Errorf("integration.created application_id = %v, want %s", row.applicationID, f.appID)
	}

	status, _ = f.do(t, "PATCH", "/api/developer/integrations/"+intID+"/config", map[string]any{"config": map[string]any{"column_map": map[string]string{}}})
	if status != http.StatusOK {
		t.Fatalf("update integration config: status=%d", status)
	}
	f.latestAuditEvent(t, "integration.updated")

	status, _ = f.do(t, "PATCH", "/api/developer/integrations/"+intID, map[string]string{"name": "CSV Import v2", "target_type": "grid"})
	if status != http.StatusOK {
		t.Fatalf("update integration: status=%d", status)
	}
	f.latestAuditEvent(t, "integration.updated")

	status, _ = f.do(t, "DELETE", "/api/developer/integrations/"+intID, nil)
	if status != http.StatusOK {
		t.Fatalf("delete integration: status=%d", status)
	}
	f.latestAuditEvent(t, "integration.deleted")

	// ── integration run (own event, "any" role) ────────────────────────────
	status, body = f.do(t, "POST", "/api/developer/integrations", map[string]string{"name": "Dim Import", "type": "csv_import", "target_type": "dimension", "target_id": f.dimID})
	if status != http.StatusOK {
		t.Fatalf("create dim integration: status=%d body=%v", status, body)
	}
	dimIntID, _ := body["id"].(string)
	status, _ = f.do(t, "POST", "/api/integrations/"+dimIntID+"/run", map[string]string{"csv": "code,label\nUS,United States\n"})
	if status != http.StatusOK {
		t.Fatalf("run integration: status=%d", status)
	}
	if row := f.latestAuditEvent(t, "integration.run"); row.resourceID != dimIntID {
		t.Errorf("integration.run resource_id = %s, want %s", row.resourceID, dimIntID)
	}

	// ── form integration: create, backfill, update, delete ─────────────────
	status, body = f.do(t, "POST", "/api/developer/form-integrations", map[string]string{
		"form_id": f.formID, "name": "Post to Revenue", "source_field": "amount", "target_metric_id": f.metricID,
	})
	if status != http.StatusOK {
		t.Fatalf("create form integration: status=%d body=%v", status, body)
	}
	mappingID, _ := body["id"].(string)
	if mappingID == "" {
		t.Fatalf("expected mapping id, got %v", body)
	}
	if row := f.latestAuditEvent(t, "form_integration.created"); row.applicationID == nil || *row.applicationID != f.appID {
		t.Errorf("form_integration.created application_id = %v, want %s", row.applicationID, f.appID)
	}

	status, body = f.do(t, "POST", "/api/developer/form-integrations/"+mappingID+"/backfill", nil)
	if status != http.StatusOK {
		t.Fatalf("backfill form integration: status=%d body=%v", status, body)
	}
	f.latestAuditEvent(t, "form_integration.backfilled")

	status, _ = f.do(t, "PATCH", "/api/developer/form-integrations/"+mappingID, map[string]any{
		"name": "Post to Revenue v2", "source_field": "amount", "target_metric_id": f.metricID,
		"aggregation": "sum", "posting_statuses": []string{"approved"},
	})
	if status != http.StatusOK {
		t.Fatalf("update form integration: status=%d", status)
	}
	f.latestAuditEvent(t, "form_integration.updated")

	status, _ = f.do(t, "DELETE", "/api/developer/form-integrations/"+mappingID, nil)
	if status != http.StatusOK {
		t.Fatalf("delete form integration: status=%d", status)
	}
	f.latestAuditEvent(t, "form_integration.deleted")

	// ── schema migration: generate, apply ──────────────────────────────────
	status, body = f.do(t, "POST", "/api/developer/migration/generate", nil)
	if status != http.StatusOK {
		t.Fatalf("generate migration: status=%d body=%v", status, body)
	}
	if row := f.latestAuditEvent(t, "schema_migration.generated"); row.resourceID != f.modelID {
		t.Errorf("schema_migration.generated resource_id = %s, want %s", row.resourceID, f.modelID)
	}

	status, body = f.do(t, "POST", "/api/developer/migration/apply", map[string]any{"model_id": f.modelID, "version_number": 1})
	if status != http.StatusOK {
		t.Fatalf("apply migration: status=%d body=%v", status, body)
	}
	if row := f.latestAuditEvent(t, "schema_migration.applied"); row.resourceID != f.modelID {
		t.Errorf("schema_migration.applied resource_id = %s, want %s", row.resourceID, f.modelID)
	}
}

// TestFormIntegrationPatchTriggeredReapplyIsAudited is a regression test:
// PATCH /api/developer/form-integrations/{id} re-applies the mapping (same
// underlying write as the dedicated POST .../backfill endpoint, which was
// already correctly audited) but never logged a form_integration.backfilled
// event for its own re-apply — the identical user-visible action was
// audited when triggered one way and invisible when triggered the other.
// The re-apply runs from a goroutine after the response returns, so this
// polls rather than checking immediately (unlike the synchronous backfill
// endpoint's own already-passing check a few lines up in the sibling test).
func TestFormIntegrationPatchTriggeredReapplyIsAudited(t *testing.T) {
	f := setupDevAuthoringFixture(t)
	ctx := context.Background()

	status, body := f.do(t, "POST", "/api/developer/form-integrations", map[string]string{
		"form_id": f.formID, "name": "Post to Revenue", "source_field": "amount", "target_metric_id": f.metricID,
	})
	if status != http.StatusOK {
		t.Fatalf("create form integration: status=%d body=%v", status, body)
	}
	mappingID, _ := body["id"].(string)
	if mappingID == "" {
		t.Fatalf("expected mapping id, got %v", body)
	}

	var before int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM audit.audit_event WHERE event_type='form_integration.backfilled' AND resource_id=$1`, mappingID).Scan(&before)

	status, _ = f.do(t, "PATCH", "/api/developer/form-integrations/"+mappingID, map[string]any{
		"name": "Post to Revenue v2", "source_field": "amount", "target_metric_id": f.metricID,
		"aggregation": "sum", "posting_statuses": []string{"approved"},
	})
	if status != http.StatusOK {
		t.Fatalf("update form integration: status=%d", status)
	}

	deadline := time.Now().Add(5 * time.Second)
	var after int
	for time.Now().Before(deadline) {
		_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM audit.audit_event WHERE event_type='form_integration.backfilled' AND resource_id=$1`, mappingID).Scan(&after)
		if after > before {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if after <= before {
		t.Fatal("PATCH-triggered re-apply never produced a form_integration.backfilled audit event")
	}

	var metaRaw []byte
	if err := f.pool.QueryRow(ctx, `
		SELECT metadata FROM audit.audit_event
		WHERE event_type='form_integration.backfilled' AND resource_id=$1
		ORDER BY occurred_at DESC LIMIT 1
	`, mappingID).Scan(&metaRaw); err != nil {
		t.Fatalf("query latest backfilled event metadata: %v", err)
	}
	var meta map[string]string
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatalf("unmarshal metadata: %v", err)
	}
	if meta["trigger"] != "mapping_updated" {
		t.Errorf("metadata[trigger] = %q, want %q", meta["trigger"], "mapping_updated")
	}
}

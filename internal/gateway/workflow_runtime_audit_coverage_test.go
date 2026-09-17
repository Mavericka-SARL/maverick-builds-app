// Workflow runtime (/api/workflow/instances/...) had only workflow.submitted
// (form-triggered starts) — direct instance starts and the business_admin
// status-override path were both unaudited.
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	"github.com/mavericks-engine/mavericks/internal/workflow"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

type wfRuntimeAuditFixture struct {
	pool          *pgxpool.Pool
	srv           *httptest.Server
	appID         string
	devSub, devID string
	baSub, baID   string
	workflowDefID string
}

func setupWFRuntimeAuditFixture(t *testing.T) *wfRuntimeAuditFixture {
	t.Helper()
	ctx := context.Background()

	pool := testdb.New(t, migrationfs.FS, ".")

	f := &wfRuntimeAuditFixture{pool: pool}
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('WFRuntimeCo', 'enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, custID)
	f.appID = q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'App', 'planning') RETURNING id::text`, wsID, custID)

	f.devSub = "wfruntime-dev"
	f.devID = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'dev@wfruntime.com', 'Dev', $2::uuid) RETURNING id::text`, f.devSub, custID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'developer', $2::uuid)`, f.devID, wsID)

	f.baSub = "wfruntime-ba"
	f.baID = q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1, 'ba@wfruntime.com', 'BA', $2::uuid) RETURNING id::text`, f.baSub, custID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_admin', $2::uuid)`, f.baID, wsID)

	// A minimal published workflow def: one notification step, no context
	// vars, so ResolveStartContext needs no RACI scope to start it.
	ws := workflow.NewStore(pool)
	def, err := ws.CreateWorkflowDefFull(ctx, f.appID, "", "Notify Only", "", "manual", f.devID)
	if err != nil {
		t.Fatalf("create workflow def: %v", err)
	}
	steps, _ := json.Marshal([]map[string]any{
		{"id": "s1", "name": "Notify", "type": "notification", "config": map[string]any{"subject": "hi", "message": "hi"}},
	})
	if _, err := ws.UpdateWorkflowDefFull(ctx, def.ID, def.Name, def.Description, def.TriggerEvent, "none", f.devID, steps, []byte("[]"), []byte("{}")); err != nil {
		t.Fatalf("update workflow def steps: %v", err)
	}
	if _, err := ws.PublishWorkflowDef(ctx, def.ID, f.devID); err != nil {
		t.Fatalf("publish workflow def: %v", err)
	}
	f.workflowDefID = def.ID

	t.Setenv("DEV_MODE", "true")
	f.srv = httptest.NewServer(NewHandler(logger.New("test"), pool, nil))
	t.Cleanup(f.srv.Close)

	return f
}

func (f *wfRuntimeAuditFixture) do(t *testing.T, method, path, sub string, body any) (int, map[string]any) {
	t.Helper()
	var buf []byte
	if body != nil {
		var err error
		buf, err = json.Marshal(body)
		if err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req, err := http.NewRequestWithContext(context.Background(), method, f.srv.URL+path, bytes.NewReader(buf))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Dev-User", sub)
	req.Header.Set("X-App-Id", f.appID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var parsed map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&parsed)
	return resp.StatusCode, parsed
}

func (f *wfRuntimeAuditFixture) latestAuditEvent(t *testing.T, eventType string) auditRow {
	t.Helper()
	var row auditRow
	err := f.pool.QueryRow(context.Background(), `
		SELECT category::text, event_type, resource_type, resource_id,
		       application_id::text, revision_id::text
		FROM audit.audit_event
		WHERE event_type = $1
		ORDER BY occurred_at DESC
		LIMIT 1
	`, eventType).Scan(&row.category, &row.eventType, &row.resourceType, &row.resourceID, &row.applicationID, &row.revisionID)
	if err != nil {
		t.Fatalf("no audit row for event_type %q: %v", eventType, err)
	}
	return row
}

func TestWorkflowRuntimeMutationsAreAudited(t *testing.T) {
	f := setupWFRuntimeAuditFixture(t)

	status, body := f.do(t, "POST", "/api/workflow/instances", f.devSub, map[string]string{"workflow_def_id": f.workflowDefID})
	if status != http.StatusOK {
		t.Fatalf("start instance: status=%d body=%v", status, body)
	}
	instanceID, _ := body["instance_id"].(string)
	if instanceID == "" {
		t.Fatalf("expected instance_id, got %v", body)
	}
	if row := f.latestAuditEvent(t, "workflow_instance.started"); row.resourceID != instanceID || row.applicationID == nil || *row.applicationID != f.appID {
		t.Errorf("workflow_instance.started row = %+v, want resource_id=%s application_id=%s", row, instanceID, f.appID)
	}

	status, body = f.do(t, "PATCH", "/api/workflow/instances/"+instanceID, f.baSub, map[string]string{"status": "cancelled"})
	if status != http.StatusOK {
		t.Fatalf("override status: status=%d body=%v", status, body)
	}
	if row := f.latestAuditEvent(t, "workflow_instance.status_overridden"); row.resourceID != instanceID || row.applicationID == nil || *row.applicationID != f.appID {
		t.Errorf("workflow_instance.status_overridden row = %+v, want resource_id=%s application_id=%s", row, instanceID, f.appID)
	}
}

// Platform Admin (/api/admin/...) was, before this test, the single biggest
// audit coverage gap in the codebase: tenant/application/model/revision/user
// management was entirely unlogged. This walks one full lifecycle across
// every admin mutation and asserts a matching audit.audit_event row lands
// for each step, with the right event_type/category/resource/application_id.
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
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

type adminAuditFixture struct {
	pool *pgxpool.Pool
	srv  *httptest.Server
}

func setupAdminAuditFixture(t *testing.T) *adminAuditFixture {
	t.Helper()
	ctx := context.Background()

	pool := testdb.New(t, migrationfs.FS, ".")

	platformAdminID := ""
	if err := pool.QueryRow(ctx,
		`INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('audit-pa', 'pa@audit.dev', 'PA') RETURNING id::text`,
	).Scan(&platformAdminID); err != nil {
		t.Fatalf("insert platform admin: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'platform_admin')`, platformAdminID,
	); err != nil {
		t.Fatalf("assign platform_admin role: %v", err)
	}

	t.Setenv("DEV_MODE", "true")
	srv := httptest.NewServer(NewHandler(logger.New("test"), pool, nil))
	t.Cleanup(srv.Close)

	return &adminAuditFixture{pool: pool, srv: srv}
}

func (f *adminAuditFixture) do(t *testing.T, method, path string, body any) (int, map[string]any) {
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
	req.Header.Set("X-Dev-User", "audit-pa")
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

type auditRow struct {
	category, eventType, resourceType, resourceID string
	applicationID, revisionID                     *string
}

// latestAuditEvent returns the most recently written row for eventType, or
// fails the test if none exists.
func (f *adminAuditFixture) latestAuditEvent(t *testing.T, eventType string) auditRow {
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

func TestAdminMutationsAreAudited(t *testing.T) {
	f := setupAdminAuditFixture(t)

	// ── tenant ──────────────────────────────────────────────────────────
	status, body := f.do(t, "POST", "/api/admin/tenants", map[string]string{"name": "AuditCo", "plan": "standard"})
	if status != http.StatusOK {
		t.Fatalf("create tenant: status=%d body=%v", status, body)
	}
	custID, _ := body["id"].(string)
	if custID == "" {
		t.Fatalf("expected tenant id in response, got %v", body)
	}
	if row := f.latestAuditEvent(t, "tenant.created"); row.resourceID != custID || row.category != "admin" {
		t.Errorf("tenant.created row = %+v, want resource_id=%s category=admin", row, custID)
	}

	status, _ = f.do(t, "PATCH", "/api/admin/tenants/"+custID, map[string]string{"name": "AuditCo Renamed"})
	if status != http.StatusOK {
		t.Fatalf("update tenant: status=%d", status)
	}
	if row := f.latestAuditEvent(t, "tenant.updated"); row.resourceID != custID {
		t.Errorf("tenant.updated resource_id = %s, want %s", row.resourceID, custID)
	}

	// ── application ─────────────────────────────────────────────────────
	status, body = f.do(t, "POST", "/api/admin/applications", map[string]string{"customer_id": custID, "name": "App", "mode": "planning"})
	if status != http.StatusOK {
		t.Fatalf("create application: status=%d body=%v", status, body)
	}
	appID, _ := body["id"].(string)
	if appID == "" {
		t.Fatalf("expected application id in response, got %v", body)
	}
	if row := f.latestAuditEvent(t, "application.created"); row.resourceID != appID || row.applicationID == nil || *row.applicationID != appID {
		t.Errorf("application.created row = %+v, want resource_id=application_id=%s", row, appID)
	}

	status, _ = f.do(t, "PATCH", "/api/admin/applications/"+appID, map[string]string{"name": "App Renamed"})
	if status != http.StatusOK {
		t.Fatalf("update application: status=%d", status)
	}
	f.latestAuditEvent(t, "application.updated")

	// ── model ───────────────────────────────────────────────────────────
	status, body = f.do(t, "POST", "/api/admin/models", map[string]string{"application_id": appID, "name": "Model", "storage_type": "oltp"})
	if status != http.StatusOK {
		t.Fatalf("create model: status=%d body=%v", status, body)
	}
	modelID, _ := body["id"].(string)
	if modelID == "" {
		t.Fatalf("expected model id in response, got %v", body)
	}
	if row := f.latestAuditEvent(t, "model.created"); row.applicationID == nil || *row.applicationID != appID {
		t.Errorf("model.created application_id = %v, want %s", row.applicationID, appID)
	}

	// ── revision ────────────────────────────────────────────────────────
	status, body = f.do(t, "POST", "/api/admin/revisions", map[string]string{"model_id": modelID, "name": "v1", "description": "first"})
	if status != http.StatusOK {
		t.Fatalf("create revision: status=%d body=%v", status, body)
	}
	revID, _ := body["id"].(string)
	if revID == "" {
		t.Fatalf("expected revision id in response, got %v", body)
	}
	if row := f.latestAuditEvent(t, "revision.created"); row.applicationID == nil || *row.applicationID != appID || row.revisionID == nil || *row.revisionID != revID {
		t.Errorf("revision.created row = %+v, want application_id=%s revision_id=%s", row, appID, revID)
	}

	status, _ = f.do(t, "PATCH", "/api/admin/revisions/"+revID, map[string]string{"name": "v1", "description": "updated"})
	if status != http.StatusOK {
		t.Fatalf("update revision: status=%d", status)
	}
	f.latestAuditEvent(t, "revision.updated")

	// ── model active-revision ──────────────────────────────────────────
	status, _ = f.do(t, "PUT", "/api/admin/models/"+modelID+"/active-revision", map[string]string{"revision_name": "v1"})
	if status != http.StatusOK {
		t.Fatalf("set active revision: status=%d", status)
	}
	if row := f.latestAuditEvent(t, "model.active_revision_set"); row.resourceID != modelID || row.revisionID == nil || *row.revisionID != revID {
		t.Errorf("model.active_revision_set row = %+v, want resource_id=%s revision_id=%s", row, modelID, revID)
	}

	// ── user create/update ──────────────────────────────────────────────
	status, body = f.do(t, "POST", "/api/admin/users", map[string]string{"email": "target@audit.dev", "first_name": "Target", "last_name": "User", "role": "business_user"})
	if status != http.StatusOK {
		t.Fatalf("create user: status=%d body=%v", status, body)
	}
	userID, _ := body["id"].(string)
	if userID == "" {
		t.Fatalf("expected user id in response, got %v", body)
	}
	f.latestAuditEvent(t, "user.created")

	status, _ = f.do(t, "PATCH", "/api/admin/users/"+userID, map[string]string{"display_name": "Target Renamed"})
	if status != http.StatusOK {
		t.Fatalf("update user: status=%d", status)
	}
	f.latestAuditEvent(t, "user.updated")

	// ── role grant/revoke ────────────────────────────────────────────────
	status, _ = f.do(t, "POST", "/api/admin/users/"+userID+"/roles", map[string]string{"role": "business_admin"})
	if status != http.StatusOK {
		t.Fatalf("grant role: status=%d", status)
	}
	f.latestAuditEvent(t, "user.role_granted")

	status, _ = f.do(t, "DELETE", "/api/admin/users/"+userID+"/roles/business_admin", nil)
	if status != http.StatusOK {
		t.Fatalf("revoke role: status=%d", status)
	}
	f.latestAuditEvent(t, "user.role_revoked")

	// ── app access grant/revoke ──────────────────────────────────────────
	status, _ = f.do(t, "POST", "/api/admin/users/"+userID+"/access/apps/"+appID, nil)
	if status != http.StatusOK {
		t.Fatalf("grant app access: status=%d", status)
	}
	if row := f.latestAuditEvent(t, "user.app_access_granted"); row.applicationID == nil || *row.applicationID != appID {
		t.Errorf("user.app_access_granted application_id = %v, want %s", row.applicationID, appID)
	}

	status, _ = f.do(t, "DELETE", "/api/admin/users/"+userID+"/access/apps/"+appID, nil)
	if status != http.StatusOK {
		t.Fatalf("revoke app access: status=%d", status)
	}
	f.latestAuditEvent(t, "user.app_access_revoked")

	// ── model access grant/revoke ────────────────────────────────────────
	status, _ = f.do(t, "POST", "/api/admin/users/"+userID+"/access/models/"+modelID, nil)
	if status != http.StatusOK {
		t.Fatalf("grant model access: status=%d", status)
	}
	if row := f.latestAuditEvent(t, "user.model_access_granted"); row.applicationID == nil || *row.applicationID != appID {
		t.Errorf("user.model_access_granted application_id = %v, want %s (derived from model)", row.applicationID, appID)
	}

	status, _ = f.do(t, "DELETE", "/api/admin/users/"+userID+"/access/models/"+modelID, nil)
	if status != http.StatusOK {
		t.Fatalf("revoke model access: status=%d", status)
	}
	f.latestAuditEvent(t, "user.model_access_revoked")

	// ── user delete ─────────────────────────────────────────────────────
	status, _ = f.do(t, "DELETE", "/api/admin/users/"+userID, nil)
	if status != http.StatusOK {
		t.Fatalf("delete user: status=%d", status)
	}
	f.latestAuditEvent(t, "user.deleted")

	// ── delete cascade: revision -> model -> application -> tenant ──────
	// Each delete event must land WITHOUT a dangling FK self-reference (the
	// migration-060 FK on revision_id/application_id would reject an insert
	// that points at the row this very event announces the deletion of).
	status, _ = f.do(t, "DELETE", "/api/admin/revisions/"+revID, nil)
	if status != http.StatusOK {
		t.Fatalf("delete revision: status=%d", status)
	}
	if row := f.latestAuditEvent(t, "revision.deleted"); row.resourceID != revID || row.revisionID != nil {
		t.Errorf("revision.deleted row = %+v, want resource_id=%s revision_id=nil", row, revID)
	}

	status, _ = f.do(t, "DELETE", "/api/admin/models/"+modelID, nil)
	if status != http.StatusOK {
		t.Fatalf("delete model: status=%d", status)
	}
	if row := f.latestAuditEvent(t, "model.deleted"); row.resourceID != modelID || row.applicationID == nil || *row.applicationID != appID {
		t.Errorf("model.deleted row = %+v, want resource_id=%s application_id=%s", row, modelID, appID)
	}

	status, _ = f.do(t, "DELETE", "/api/admin/applications/"+appID, nil)
	if status != http.StatusOK {
		t.Fatalf("delete application: status=%d", status)
	}
	if row := f.latestAuditEvent(t, "application.deleted"); row.resourceID != appID || row.applicationID != nil {
		t.Errorf("application.deleted row = %+v, want resource_id=%s application_id=nil", row, appID)
	}

	status, _ = f.do(t, "DELETE", "/api/admin/tenants/"+custID, nil)
	if status != http.StatusOK {
		t.Fatalf("delete tenant: status=%d", status)
	}
	f.latestAuditEvent(t, "tenant.deleted")
}

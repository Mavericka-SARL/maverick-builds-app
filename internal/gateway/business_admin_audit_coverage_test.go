// baRoleAction's dashboard-assignment and member add/remove sub-actions were
// the only unaudited pieces left in an otherwise fully-covered handler
// family (role.created/updated/deleted already logged before this).
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

type baAuditFixture struct {
	pool           *pgxpool.Pool
	srv            *httptest.Server
	roleID, userID string
}

func setupBAAuditFixture(t *testing.T) *baAuditFixture {
	t.Helper()
	ctx := context.Background()

	pool := testdb.New(t, migrationfs.FS, ".")

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

	custID := q(`INSERT INTO core.customer (name) VALUES ('BA Audit Co') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'WS') RETURNING id::text`, custID)
	appID := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'App', 'planning') RETURNING id::text`, wsID, custID)
	q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Model') RETURNING id::text`, appID)

	baAdminID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('ba-audit-admin', 'baadmin@audit.dev', 'BA Admin') RETURNING id::text`)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_admin', $2::uuid)`, baAdminID, wsID)

	// The member needs a role_assignment in this workspace, not just an
	// identity.user row: /api/business-admin/users (the only user list a
	// business admin has) selects candidates by exactly that assignment, so a
	// bare user could never reach the member picker in the real console —
	// and seating one is now rejected. See TestRoleMemberMustBelongToWorkspace.
	memberID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('ba-audit-member', 'member@audit.dev', 'Member') RETURNING id::text`)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'business_user', $2::uuid)`, memberID, wsID)

	roleID := q(`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Viewers') RETURNING id::text`, wsID)

	t.Setenv("DEV_MODE", "true")
	srv := httptest.NewServer(NewHandler(logger.New("test"), pool, nil))
	t.Cleanup(srv.Close)

	return &baAuditFixture{pool: pool, srv: srv, roleID: roleID, userID: memberID}
}

func (f *baAuditFixture) do(t *testing.T, method, path string, body any) (int, map[string]any) {
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
	req.Header.Set("X-Dev-User", "ba-audit-admin")
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

func (f *baAuditFixture) latestAuditEvent(t *testing.T, eventType string) auditRow {
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

func TestBusinessAdminRoleDashboardsAndMembersAreAudited(t *testing.T) {
	f := setupBAAuditFixture(t)

	status, _ := f.do(t, "PUT", "/api/business-admin/roles/"+f.roleID+"/dashboards", map[string]any{"dashboard_ids": []string{}})
	if status != http.StatusOK {
		t.Fatalf("update dashboards: status=%d", status)
	}
	if row := f.latestAuditEvent(t, "role.dashboards_updated"); row.resourceID != f.roleID {
		t.Errorf("role.dashboards_updated resource_id = %s, want %s", row.resourceID, f.roleID)
	}

	status, _ = f.do(t, "POST", "/api/business-admin/roles/"+f.roleID+"/members", map[string]string{"user_id": f.userID})
	if status != http.StatusOK {
		t.Fatalf("add member: status=%d", status)
	}
	if row := f.latestAuditEvent(t, "role.member_added"); row.resourceID != f.roleID {
		t.Errorf("role.member_added resource_id = %s, want %s", row.resourceID, f.roleID)
	}

	status, _ = f.do(t, "DELETE", "/api/business-admin/roles/"+f.roleID+"/members/"+f.userID, nil)
	if status != http.StatusOK {
		t.Fatalf("remove member: status=%d", status)
	}
	if row := f.latestAuditEvent(t, "role.member_removed"); row.resourceID != f.roleID {
		t.Errorf("role.member_removed resource_id = %s, want %s", row.resourceID, f.roleID)
	}
}

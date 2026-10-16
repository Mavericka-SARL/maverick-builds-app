package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Business roles are the workspace's: a second role of the same name — from
// another app of the workspace — is a 409 that says so, not a raw 500.
func TestBusinessRoleNameTakenIsAConflict(t *testing.T) {
	f := setupRoundTripFixture(t)
	call := func(method, path string, body any) (int, string) {
		t.Helper()
		return doAs(t, f.rollupFixture, method, path, "rollup-test-approver", f.appID, body)
	}
	if status, raw := call("POST", "/api/business-admin/roles", map[string]any{"name": "HR Planners"}); status != http.StatusOK {
		t.Fatalf("first role: %d %s", status, raw)
	}
	status, raw := call("POST", "/api/business-admin/roles", map[string]any{"name": "HR Planners"})
	if status != http.StatusConflict || !strings.Contains(raw, "already exists in this workspace") {
		t.Errorf("a taken role name: %d %s, want 409 saying it exists in the workspace", status, raw)
	}
}

// A role lists each of its dashboards once, however many members it has: the
// member and dashboard joins used to multiply, so a role of 2 members listed
// every dashboard twice (found 2026-10-16: 45 ids for 15 dashboards).
func TestBusinessRoleListsEachDashboardOnce(t *testing.T) {
	f := setupRoundTripFixture(t)
	ctx := context.Background()
	call := func(method, path string, body any) (int, string) {
		t.Helper()
		return doAs(t, f.rollupFixture, method, path, "rollup-test-approver", f.appID, body)
	}
	status, raw := call("POST", "/api/business-admin/roles", map[string]any{"name": "Planners"})
	if status != http.StatusOK {
		t.Fatalf("create role: %d %s", status, raw)
	}
	var roleID, dashID string
	_ = f.pool.QueryRow(ctx, `SELECT id::text FROM identity.business_role WHERE name='Planners'`).Scan(&roleID)
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid, $2::uuid), ($1::uuid, $3::uuid)`,
		roleID, f.managerID, f.approverID); err != nil {
		t.Fatalf("members: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO model.dashboard_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Plan') RETURNING id::text`,
		f.modelID, f.workingRevID).Scan(&dashID); err != nil {
		t.Fatalf("dashboard: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.business_role_dashboard (role_id, dashboard_id) VALUES ($1::uuid, $2::uuid)`, roleID, dashID); err != nil {
		t.Fatalf("grant: %v", err)
	}
	status, raw = call("GET", "/api/business-admin/roles", nil)
	var roles []struct {
		Name         string   `json:"name"`
		MemberCount  int      `json:"member_count"`
		DashboardIDs []string `json:"dashboard_ids"`
	}
	if status != http.StatusOK || json.Unmarshal([]byte(raw), &roles) != nil {
		t.Fatalf("list roles: %d %s", status, raw)
	}
	for _, r := range roles {
		if r.Name == "Planners" && (r.MemberCount != 2 || len(r.DashboardIDs) != 1) {
			t.Errorf("Planners: %d members, dashboard_ids %v; want 2 members and the one dashboard once", r.MemberCount, r.DashboardIDs)
		}
	}
}

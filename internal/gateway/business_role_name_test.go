package gateway

import (
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

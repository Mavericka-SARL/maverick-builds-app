package gateway

// Regression test for a tenant-scope gap found live in production on
// 2026-09-10: a user created through the admin console (no customer_id of
// their own) who was granted tenant_admin WITHOUT a workspace (the Users
// panel offers it as a "platform role") plus business_admin scoped to their
// tenant's workspace resolved to NO tenant in adminScopeCustomerIDs — the
// Applications view was empty and model export/import returned 403, even
// though the role was granted precisely so they could export models.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
)

func decodeJSON(t *testing.T, resp *http.Response, dst any) {
	t.Helper()
	if err := json.NewDecoder(resp.Body).Decode(dst); err != nil {
		t.Fatalf("decode %s: %v", resp.Request.URL.Path, err)
	}
}

func TestUnscopedTenantAdminScopesToWorkspaceRoleTenant(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()

	var custID, wsID string
	if err := f.pool.QueryRow(ctx, `SELECT workspace_id::text, customer_id::text FROM core.application WHERE id=$1::uuid`, f.appID).Scan(&wsID, &custID); err != nil {
		t.Fatalf("resolve app workspace/customer: %v", err)
	}
	// customer_id deliberately NULL: that is what admin-created users look like.
	var uid string
	if err := f.pool.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('test-unscoped-ta', 'unscoped-ta@t.com', 'TA') RETURNING id::text`).Scan(&uid); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	for _, ins := range []struct {
		role string
		ws   *string
	}{{"tenant_admin", nil}, {"business_admin", &wsID}} {
		if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, $2, $3::uuid)`, uid, ins.role, ins.ws); err != nil {
			t.Fatalf("grant %s: %v", ins.role, err)
		}
	}
	devPersonas["rollup-test-unscoped-ta"] = "test-unscoped-ta"
	t.Cleanup(func() { delete(devPersonas, "rollup-test-unscoped-ta") })

	status, body := f.do(t, "GET", "/api/admin/tenants", "rollup-test-unscoped-ta", nil)
	if status != http.StatusOK {
		t.Fatalf("tenants status = %d, body = %v", status, body)
	}
	// f.do decodes into a map; the endpoint returns an array, so re-read raw.
	req, _ := http.NewRequestWithContext(ctx, "GET", f.srv.URL+"/api/admin/tenants", nil)
	req.Header.Set("X-Dev-User", "rollup-test-unscoped-ta")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("tenants: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var tenants []map[string]any
	decodeJSON(t, resp, &tenants)
	found := false
	for _, tn := range tenants {
		if tn["id"] == custID {
			found = true
		}
	}
	if !found {
		t.Errorf("tenants visible to an unscoped tenant_admin with business_admin@%s = %v, want their tenant %s listed", wsID, tenants, custID)
	}

	status, pkg := f.do(t, "GET", fmt.Sprintf("/api/admin/models/%s/export?revision_id=%s", f.modelID, f.workingRevID), "rollup-test-unscoped-ta", nil)
	if status != http.StatusOK {
		t.Errorf("export as unscoped tenant_admin status = %d, want 200 (body %v)", status, pkg)
	}

	// The widening is deliberately narrow: a tenant_admin whose grant IS
	// workspace-scoped does not gain admin scope over some other tenant just
	// because they hold a business role there.
	var otherCust, otherWS string
	if err := f.pool.QueryRow(ctx, `INSERT INTO core.customer (name, plan) VALUES ('Other Co', 'enterprise') RETURNING id::text`).Scan(&otherCust); err != nil {
		t.Fatalf("insert other customer: %v", err)
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Default') RETURNING id::text`, otherCust).Scan(&otherWS); err != nil {
		t.Fatalf("insert other workspace: %v", err)
	}
	var scopedUID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('test-scoped-ta', 'scoped-ta@t.com', 'TA') RETURNING id::text`).Scan(&scopedUID); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	for _, ins := range []struct{ role, ws string }{{"tenant_admin", wsID}, {"business_user", otherWS}} {
		if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, $2, $3::uuid)`, scopedUID, ins.role, ins.ws); err != nil {
			t.Fatalf("grant %s: %v", ins.role, err)
		}
	}
	devPersonas["rollup-test-scoped-ta"] = "test-scoped-ta"
	t.Cleanup(func() { delete(devPersonas, "rollup-test-scoped-ta") })
	req2, _ := http.NewRequestWithContext(ctx, "GET", f.srv.URL+"/api/admin/tenants", nil)
	req2.Header.Set("X-Dev-User", "rollup-test-scoped-ta")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("tenants: %v", err)
	}
	defer resp2.Body.Close() //nolint:errcheck
	var tenants2 []map[string]any
	decodeJSON(t, resp2, &tenants2)
	for _, tn := range tenants2 {
		if tn["id"] == otherCust {
			t.Errorf("workspace-scoped tenant_admin of %s sees Other Co (%s) as an admin tenant via a business_user role there: %v", custID, otherCust, tenants2)
		}
	}
	if len(tenants2) != 1 || tenants2[0]["id"] != custID {
		t.Errorf("workspace-scoped tenant_admin tenants = %v, want exactly [%s]", tenants2, custID)
	}
}

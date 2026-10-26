package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestRenameAndDescribeApplicationsAndModels: the tenant admin who creates
// applications and models renames them and writes what each is for; the
// lists the consoles read return it, and a model's export carries it.
// Nobody else can: a developer who is not an administrator, nor the tenant
// admin of another tenant.
func TestRenameAndDescribeApplicationsAndModels(t *testing.T) {
	f := setupRollupFixture(t)
	ft := seedForeignTenant(t, f)
	ctx := context.Background()

	var custID string
	if err := f.pool.QueryRow(ctx, `SELECT customer_id::text FROM core.workspace WHERE id=(SELECT workspace_id FROM core.application WHERE id=$1::uuid)`, f.appID).Scan(&custID); err != nil {
		t.Fatal(err)
	}
	var taID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('test-details-ta', 'details-ta@t.com', 'TA', $1::uuid) RETURNING id::text`,
		custID).Scan(&taID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx,
		`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'tenant_admin', (SELECT workspace_id FROM core.application WHERE id=$2::uuid))`,
		taID, f.appID); err != nil {
		t.Fatal(err)
	}
	const ta = "test-details-ta"
	modelPath, appPath := "/api/admin/models/"+f.modelID, "/api/admin/applications/"+f.appID

	read := func(table, id string) (name, desc string) {
		t.Helper()
		if err := f.pool.QueryRow(ctx, `SELECT name, description FROM `+table+` WHERE id=$1::uuid`, id).Scan(&name, &desc); err != nil {
			t.Fatal(err)
		}
		return name, desc
	}

	// Renamed and described; what was sent is trimmed.
	if status, body := f.do(t, "PATCH", modelPath, ta, map[string]string{"name": "  Order planning ", "description": " Weekly order proposals per store. "}); status != http.StatusOK {
		t.Fatalf("model patch: %d %v", status, body)
	}
	if name, desc := read("core.model", f.modelID); name != "Order planning" || desc != "Weekly order proposals per store." {
		t.Errorf("model = %q / %q", name, desc)
	}
	var recorded int
	_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM audit.audit_event WHERE event_type='model.updated' AND resource_id=$1 AND metadata->>'name'='Order planning'`, f.modelID).Scan(&recorded)
	if recorded != 1 {
		t.Errorf("model.updated events = %d, want 1", recorded)
	}

	// A field left out stays; an empty description clears it.
	if status, body := f.do(t, "PATCH", modelPath, ta, map[string]string{"description": ""}); status != http.StatusOK {
		t.Fatalf("model clear description: %d %v", status, body)
	}
	if name, desc := read("core.model", f.modelID); name != "Order planning" || desc != "" {
		t.Errorf("after clearing: model = %q / %q", name, desc)
	}
	if status, body := f.do(t, "PATCH", appPath, ta, map[string]string{"description": "Store replenishment."}); status != http.StatusOK {
		t.Fatalf("application patch: %d %v", status, body)
	}
	if name, desc := read("core.application", f.appID); name != "App" || desc != "Store replenishment." {
		t.Errorf("application = %q / %q", name, desc)
	}

	// Refused: nothing to change, an empty name, too long a description.
	for _, bad := range []map[string]string{{}, {"name": "   "}, {"description": strings.Repeat("x", maxDetailsDescription+1)}} {
		for _, path := range []string{modelPath, appPath} {
			if status, _ := f.do(t, "PATCH", path, ta, bad); status != http.StatusBadRequest {
				t.Errorf("PATCH %s %v: status %d, want 400", path, bad, status)
			}
		}
	}

	// A developer who is not an administrator, and another tenant's model.
	if status, _ := f.do(t, "PATCH", modelPath, "rollup-test-approver", map[string]string{"name": "Mine"}); status != http.StatusForbidden {
		t.Errorf("developer rename: status %d, want 403", status)
	}
	if status, _ := f.do(t, "PATCH", "/api/admin/models/"+ft.modelID, ta, map[string]string{"name": "Mine"}); status != http.StatusForbidden {
		t.Errorf("foreign model rename: status %d, want 403", status)
	}
	if status, _ := f.do(t, "PATCH", "/api/admin/applications/"+ft.appID, ta, map[string]string{"name": "Mine"}); status != http.StatusForbidden {
		t.Errorf("foreign application rename: status %d, want 403", status)
	}

	// The business user's list carries both descriptions.
	if _, err := f.pool.Exec(ctx, `UPDATE core.model SET description='Weekly order proposals per store.' WHERE id=$1::uuid`, f.modelID); err != nil {
		t.Fatal(err)
	}
	getArray := func(path, persona string) []map[string]any {
		t.Helper()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, f.srv.URL+path, nil)
		req.Header.Set("X-Dev-User", persona)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		var out []map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("%s: %d %v", path, resp.StatusCode, err)
		}
		return out
	}
	descriptions := func(apps []map[string]any) (appDesc, modelDesc string) {
		for _, a := range apps {
			if a["id"] != f.appID {
				continue
			}
			appDesc, _ = a["description"].(string)
			models, _ := a["models"].([]any)
			for _, m := range models {
				if mm, ok := m.(map[string]any); ok && mm["id"] == f.modelID {
					modelDesc, _ = mm["description"].(string)
				}
			}
		}
		return appDesc, modelDesc
	}
	if a, m := descriptions(getArray("/api/apps", "test-manager")); a != "Store replenishment." || m != "Weekly order proposals per store." {
		t.Errorf("/api/apps descriptions = %q / %q", a, m)
	}
	// And the creator's own list (Developer › Developer Models, Tenant admin).
	var listed bool
	for _, tenant := range getArray("/api/admin/tenants", ta) {
		apps, _ := tenant["applications"].([]any)
		var asMaps []map[string]any
		for _, a := range apps {
			if am, ok := a.(map[string]any); ok {
				asMaps = append(asMaps, am)
			}
		}
		if a, m := descriptions(asMaps); a == "Store replenishment." && m == "Weekly order proposals per store." {
			listed = true
		}
	}
	if !listed {
		t.Error("/api/admin/tenants does not list the application and model descriptions")
	}
}

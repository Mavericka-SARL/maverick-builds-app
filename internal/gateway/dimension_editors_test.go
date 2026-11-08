package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestDimensionEditorRolesKeepItsMembers: a dimension business users keep,
// with editor roles, takes member changes from those roles' members and the
// application's administrators only (migration 161, owner decision
// 2026-11-08). A GL account's reporting category could be changed by any
// business user; the workbook's Mapping is Accounting's.
func TestDimensionEditorRolesKeepItsMembers(t *testing.T) {
	f := setupRestrictedFixture(t)
	ctx := context.Background()
	gl := f.dev_("POST", "/api/developer/dimensions", map[string]any{"name": "gl_account", "revision_id": f.revID, "business_maintained": true})
	add := func(persona, label string) (int, string) {
		status, raw := f.req("POST", "/api/dimensions/"+gl+"/members", persona, map[string]any{"label": label})
		return status, string(raw)
	}
	if st, raw := add(f.viewer, "Rent"); st != http.StatusOK {
		t.Fatalf("no editor roles: a business user adds a member: %d %s", st, raw)
	}

	if st, raw := f.req("PATCH", "/api/developer/dimensions/"+gl, f.dev, map[string]any{"editor_roles": []string{"Accounting"}}); st != http.StatusBadRequest ||
		!strings.Contains(string(raw), "no business role here") {
		t.Errorf("a role that does not exist: %d %s, want 400", st, raw)
	}
	var wsID, roleID string
	if err := f.pool.QueryRow(ctx, `SELECT workspace_id::text FROM core.application WHERE id = $1::uuid`, f.appID).Scan(&wsID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Accounting') RETURNING id::text`, wsID).Scan(&roleID); err != nil {
		t.Fatal(err)
	}
	if st, raw := f.req("PATCH", "/api/developer/dimensions/"+gl, f.dev, map[string]any{"editor_roles": []string{"Accounting"}}); st != http.StatusOK {
		t.Fatalf("editor roles Accounting: %d %s", st, raw)
	}

	if st, raw := add(f.viewer, "Travel"); st != http.StatusForbidden || !strings.Contains(raw, "only Accounting and the application's administrators change the members of gl_account") {
		t.Errorf("a business user outside Accounting adds a member: %d %s, want 403 naming who may", st, raw)
	}
	if st, raw := add(f.admin, "Travel"); st != http.StatusOK {
		t.Errorf("the business admin adds a member: %d %s, want 200", st, raw)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid, $2::uuid)`, roleID, f.viewerID); err != nil {
		t.Fatal(err)
	}
	if st, raw := add(f.viewer, "Insurance"); st != http.StatusOK {
		t.Errorf("a member of Accounting adds a member: %d %s, want 200", st, raw)
	}

	// The developer's list shows the rule; [] lifts it.
	if _, raw := f.req("GET", "/api/developer/dimensions?revision_id="+f.revID, f.dev, nil); !strings.Contains(string(raw), `"editor_roles":["Accounting"]`) {
		t.Errorf("the developer's dimension list does not show editor_roles: %.300s", raw)
	}
	if st, raw := f.req("PATCH", "/api/developer/dimensions/"+gl, f.dev, map[string]any{"editor_roles": []string{}}); st != http.StatusOK {
		t.Errorf("lifting the rule: %d %s", st, raw)
	}
}

// TestKeptDimensionEditorRolesKeepItsRecords: a table that keeps a dimension
// with editor roles is changed — records added, edited, deleted — by those
// roles' members and the administrators only: its records are the
// dimension's members.
func TestKeptDimensionEditorRolesKeepItsRecords(t *testing.T) {
	d := setupSalesDemo(t)
	ctx := context.Background()
	glDim := idOf(d.call("POST", "/api/developer/dimensions", d.dev, map[string]any{"name": "gl_account", "revision_id": d.revID}))
	table := idOf(d.call("POST", "/api/forms", d.dev, map[string]any{
		"name": "gl_accounts", "label": "GL accounts", "edit_access": "everyone",
		"fields": []map[string]any{
			{"name": "account", "label": "Account no.", "type": "text", "code": true, "core": true},
			{"name": "name", "label": "Account name", "type": "text", "required": true},
		},
		"keeps_dimension_id": glDim, "member_label_field": "name",
	}))
	rec := idOf(d.call("POST", "/api/forms/"+table+"/records", d.westRep, map[string]any{"data": map[string]any{"account": "4000", "name": "Sales"}}))
	var roleID string
	if err := d.pool.QueryRow(ctx, `INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Accounting') RETURNING id::text`, d.wsID).Scan(&roleID); err != nil {
		t.Fatal(err)
	}
	d.call("PATCH", "/api/developer/dimensions/"+glDim, d.dev, map[string]any{"editor_roles": []string{"Accounting"}})

	if st, raw := d.req("POST", "/api/forms/"+table+"/records", d.westRep, map[string]any{"data": map[string]any{"account": "5000", "name": "COGS"}}); st != http.StatusForbidden ||
		!strings.Contains(string(raw), "only Accounting and the application's administrators change the members of gl_account") {
		t.Errorf("a rep outside Accounting adds an account: %d %s, want 403", st, raw)
	}
	if st, raw := d.req("PUT", "/api/records/"+rec, d.westRep, map[string]any{"data": map[string]any{"account": "4000", "name": "Revenue"}}); st != http.StatusForbidden {
		t.Errorf("a rep outside Accounting renames an account: %d %s, want 403", st, raw)
	}
	if _, err := d.pool.Exec(ctx, `INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid, $2::uuid)`, roleID, d.westRepID); err != nil {
		t.Fatal(err)
	}
	if st, raw := d.req("PUT", "/api/records/"+rec, d.westRep, map[string]any{"data": map[string]any{"account": "4000", "name": "Revenue"}}); st != http.StatusOK {
		t.Errorf("a member of Accounting renames an account: %d %s, want 200", st, raw)
	}
}

package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Who may add a table's records (migration 157): named, only people of those
// roles and the application's administrators may; named none, anyone who
// reaches the application, as before. Found 2026-11-06: any app user could
// add a plant or a GL account. A revision copy keeps every table setting:
// it kept only own_records_only until 2026-11-07.
func TestTableCreateRoles(t *testing.T) {
	f := setupRecordFixture(t)
	ctx := context.Background()
	fields := []map[string]any{{"name": "amount", "label": "Amount", "type": "number"}, {"name": "status", "label": "Status", "type": "status"}}
	settings := func(roles []string) {
		t.Helper()
		f.expect(http.StatusOK, http.MethodPatch, "/api/forms/"+f.formID, prDev, map[string]any{
			"name": "expense", "label": "Expense", "fields": fields, "create_roles": roles, "edit_access": "everyone",
		})
	}
	add := func(persona string) int {
		t.Helper()
		status, _ := f.do(http.MethodPost, "/api/forms/"+f.formID+"/records", persona, map[string]any{"data": map[string]any{"amount": 1}})
		return status
	}
	mayCreateIn := func(persona, formID string) bool {
		t.Helper()
		var forms []struct {
			ID          string `json:"id"`
			Permissions struct {
				Create bool `json:"create"`
			} `json:"permissions"`
		}
		_ = json.Unmarshal(f.expect(http.StatusOK, http.MethodGet, "/api/forms", persona, nil), &forms)
		for _, x := range forms {
			if x.ID == formID {
				return x.Permissions.Create
			}
		}
		return false
	}
	mayCreate := func(persona string) bool { t.Helper(); return mayCreateIn(persona, f.formID) }
	// A second table of the same model, which anyone may add to: the list
	// resolves the model once, and every table took the first one's
	// create_roles (2026-11-07).
	var openFormID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO model.form_def (model_id, revision_id, name, label, fields)
		SELECT model_id, revision_id, 'aaa_notes', 'Notes', '[{"name":"note","label":"Note","type":"text"}]'::jsonb
		FROM model.form_def WHERE id = $1::uuid RETURNING id::text`, f.formID).Scan(&openFormID); err != nil {
		t.Fatal(err)
	}

	settings([]string{"Supplier admins"})
	if status, raw := f.do(http.MethodPost, "/api/forms/"+f.formID+"/records", prOwner, map[string]any{"data": map[string]any{"amount": 1}}); status != http.StatusForbidden ||
		!strings.Contains(string(raw), "only Supplier admins and the application's administrators add records to Expense") {
		t.Errorf("a business user outside the roles adds a record: %d %s, want 403 naming who may", status, raw)
	}
	if mayCreate(prOwner) {
		t.Error("the table offers New record to a business user outside the roles")
	}
	if !mayCreateIn(prOwner, openFormID) {
		t.Error("the table anyone adds to took the other table's create_roles: no New record offered")
	}
	if got := add(prBA); got != http.StatusOK {
		t.Errorf("the workspace's business admin adds a record: %d, want 200", got)
	}

	var wsID string
	if err := f.pool.QueryRow(ctx, `SELECT workspace_id::text FROM core.application WHERE id = $1::uuid`, f.appID).Scan(&wsID); err != nil {
		t.Fatal(err)
	}
	var roleID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, 'Supplier admins') RETURNING id::text`, wsID).Scan(&roleID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid, $2::uuid)`, roleID, f.userID[prOwner]); err != nil {
		t.Fatal(err)
	}
	if got := add(prOwner); got != http.StatusOK || !mayCreate(prOwner) {
		t.Errorf("a member of the role adds a record: %d (offered %v), want 200", got, mayCreate(prOwner))
	}
	if got := add(prPeer); got != http.StatusForbidden {
		t.Errorf("another business user adds a record: %d, want 403", got)
	}

	// A revision copy keeps the table's settings.
	var modelID, revID string
	if err := f.pool.QueryRow(ctx, `SELECT model_id::text, revision_id::text FROM model.form_def WHERE id = $1::uuid`, f.formID).Scan(&modelID, &revID); err != nil {
		t.Fatal(err)
	}
	f.expect(http.StatusOK, http.MethodPost, "/api/developer/revisions", prDev, map[string]any{"name": "Copy", "source_revision_id": revID, "model_id": modelID})
	var roles, edit string
	if err := f.pool.QueryRow(ctx, `SELECT create_roles::text, edit_access FROM model.form_def
		WHERE model_id = $1::uuid AND name = 'expense' AND revision_id <> $2::uuid`, modelID, revID).Scan(&roles, &edit); err != nil {
		t.Fatalf("the copy's table: %v", err)
	}
	if roles != `["Supplier admins"]` || edit != "everyone" {
		t.Errorf("the copy's table: create_roles %s, edit_access %s; want the original's", roles, edit)
	}

	settings([]string{})
	if got := add(prPeer); got != http.StatusOK {
		t.Errorf("with no roles named, a business user adds a record: %d, want 200", got)
	}
}

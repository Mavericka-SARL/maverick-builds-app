package gateway

import (
	"context"
	"net/http"
	"testing"
)

// A folder update changes only what it is sent, and a folder's parent is a
// folder of the same model and revision that is not inside it (OBSERVATIONS
// "The developer's folder and dashboard updates overwrite fields they were
// not sent"). A rename used to move a nested folder to the top level, and a
// folder could be filed under itself, under its own child, or under another
// model's or revision's folder.
func TestFolderPatchIsPartialAndStaysInItsRevision(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()
	const dev = "rollup-test-approver"
	folders := "/api/developer/folders?revision_id=" + f.workingRevID
	create := func(name, parent string) string {
		t.Helper()
		body := map[string]any{"name": name}
		if parent != "" {
			body["parent_id"] = parent
		}
		status, out := f.do(t, "POST", folders, dev, body)
		if status != http.StatusOK {
			t.Fatalf("create %s: %d %v", name, status, out)
		}
		return out["id"].(string)
	}
	parentOf := func(id string) (parent, name string) {
		t.Helper()
		if err := f.pool.QueryRow(ctx, `SELECT COALESCE(parent_id::text,''), name FROM model.dashboard_folder WHERE id=$1::uuid`, id).Scan(&parent, &name); err != nil {
			t.Fatal(err)
		}
		return
	}
	patch := func(id string, body map[string]any) int {
		t.Helper()
		status, _ := f.do(t, "PATCH", "/api/developer/folders/"+id, dev, body)
		return status
	}

	parent := create("Parent", "")
	child := create("Child", parent)

	// A rename keeps the folder where it is.
	if s := patch(child, map[string]any{"name": "Child renamed"}); s != http.StatusOK {
		t.Fatalf("rename child: %d", s)
	}
	if p, n := parentOf(child); p != parent || n != "Child renamed" {
		t.Errorf("after a rename: parent %q name %q, want parent %q name %q", p, n, parent, "Child renamed")
	}
	// A move keeps the name; null moves to the top level.
	if s := patch(child, map[string]any{"parent_id": nil}); s != http.StatusOK {
		t.Fatalf("move child to the top: %d", s)
	}
	if p, n := parentOf(child); p != "" || n != "Child renamed" {
		t.Errorf("after a move to the top: parent %q name %q", p, n)
	}
	if s := patch(child, map[string]any{"parent_id": parent}); s != http.StatusOK {
		t.Fatalf("move child back: %d", s)
	}
	if p, _ := parentOf(child); p != parent {
		t.Errorf("after a move back: parent %q, want %q", p, parent)
	}

	// Not inside itself, nor inside its own child.
	if s := patch(parent, map[string]any{"parent_id": parent}); s != http.StatusBadRequest {
		t.Errorf("a folder its own parent: %d, want 400", s)
	}
	if s := patch(parent, map[string]any{"parent_id": child}); s != http.StatusBadRequest {
		t.Errorf("a folder under its own child: %d, want 400", s)
	}
	if s := patch(parent, map[string]any{}); s != http.StatusBadRequest {
		t.Errorf("an empty PATCH: %d, want 400", s)
	}
	if s := patch(parent, map[string]any{"name": " "}); s != http.StatusBadRequest {
		t.Errorf("a blank name: %d, want 400", s)
	}

	// Another model's folder, and another revision's, are refused as parents.
	var otherModelFolder, annualFolder string
	if err := f.pool.QueryRow(ctx, `
		WITH m AS (INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Other') RETURNING id)
		INSERT INTO model.dashboard_folder (model_id, name) SELECT id, 'Foreign' FROM m RETURNING id::text`, f.appID).Scan(&otherModelFolder); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO model.dashboard_folder (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'Annual') RETURNING id::text`,
		f.modelID, f.annualRevID).Scan(&annualFolder); err != nil {
		t.Fatal(err)
	}
	for what, foreign := range map[string]string{"another model's folder": otherModelFolder, "another revision's folder": annualFolder} {
		if status, out := f.do(t, "POST", folders, dev, map[string]any{"name": "X", "parent_id": foreign}); status != http.StatusBadRequest {
			t.Errorf("POST under %s: %d %v, want 400", what, status, out)
		}
		if s := patch(parent, map[string]any{"parent_id": foreign}); s != http.StatusBadRequest {
			t.Errorf("PATCH under %s: %d, want 400", what, s)
		}
	}
	if p, _ := parentOf(parent); p != "" {
		t.Errorf("Parent moved to %q by a refused PATCH", p)
	}

	// A dashboard is not filed in another revision's folder either.
	var dashID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO model.dashboard_def (model_id, revision_id, name, tags, category) VALUES ($1::uuid, $2::uuid, 'D', '{}', '') RETURNING id::text`,
		f.modelID, f.workingRevID).Scan(&dashID); err != nil {
		t.Fatal(err)
	}
	if status, out := f.do(t, "PATCH", "/api/developer/dashboards/"+dashID, dev, map[string]any{"folder_id": annualFolder}); status != http.StatusBadRequest {
		t.Errorf("dashboard into another revision's folder: %d %v, want 400", status, out)
	}
	if status, out := f.do(t, "PATCH", "/api/developer/dashboards/"+dashID, dev, map[string]any{"folder_id": child}); status != http.StatusOK {
		t.Errorf("dashboard into its own revision's folder: %d %v, want 200", status, out)
	}
}

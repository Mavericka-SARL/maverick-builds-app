package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/modeledit"
)

// A model is created with its first revision, which is not made active:
// every developer screen works in a revision, and going live is the
// developer's own decision (owner request, 2026-10-10).
func TestNewModelComesWithItsFirstRevision(t *testing.T) {
	f := setupPromoteFixture(t)
	ctx := context.Background()
	var adminID string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		SELECT 'first-rev-admin', 'admin@promoteco.com', 'Admin', customer_id FROM core.application WHERE id=$1::uuid
		RETURNING id::text`, f.appID).Scan(&adminID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'tenant_admin')`, adminID); err != nil {
		t.Fatal(err)
	}
	status, body := f.do(t, "POST", "/api/admin/models", "first-rev-admin", map[string]string{"application_id": f.appID, "name": "Budget"})
	if status != http.StatusOK {
		t.Fatalf("create model: %d %s", status, body)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &created); err != nil || created.ID == "" {
		t.Fatalf("create model response %s (%v)", body, err)
	}
	var names []string
	rows, err := f.pool.Query(ctx, `SELECT name FROM model.revision WHERE model_id=$1::uuid`, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	if len(names) != 1 || names[0] != modeledit.FirstRevisionName {
		t.Errorf("revisions of the new model = %v, want [%s]", names, modeledit.FirstRevisionName)
	}
	var active *string
	_ = f.pool.QueryRow(ctx, `SELECT active_revision_id::text FROM core.model WHERE id=$1::uuid`, created.ID).Scan(&active)
	if active != nil {
		t.Errorf("the first revision was made active (%s) — that is the developer's decision", *active)
	}
}

// A dashboard is created in the revision the developer names — the one
// their list shows — not in the model's active revision, where it vanished
// from a developer working in another revision.
func TestDashboardIsCreatedInTheNamedRevision(t *testing.T) {
	f := setupPromoteFixture(t)
	ctx := context.Background()
	var mineID string
	if err := f.pool.QueryRow(ctx, `INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Mine') RETURNING id::text`, f.modelID).Scan(&mineID); err != nil {
		t.Fatal(err)
	}
	status, body := f.do(t, "POST", "/api/developer/dashboards", f.devSub, map[string]any{"name": "Budget", "tags": []string{"new"}, "revision_id": mineID})
	if status != http.StatusOK {
		t.Fatalf("create dashboard: %d %s", status, body)
	}
	status, body = f.do(t, "GET", "/api/developer/dashboards?revision_id="+mineID, f.devSub, nil)
	if status != http.StatusOK {
		t.Fatalf("list dashboards: %d %s", status, body)
	}
	var list []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &list); err != nil || len(list) != 1 || list[0].Name != "Budget" {
		t.Errorf("dashboards of the named revision = %s (%v), want Budget", body, err)
	}

	// Named nowhere, with no active revision: the newest revision, never
	// no revision at all.
	if _, err := f.pool.Exec(ctx, `UPDATE core.model SET active_revision_id=NULL, active_revision_name=NULL WHERE id=$1::uuid`, f.modelID); err != nil {
		t.Fatal(err)
	}
	if status, body := f.do(t, "POST", "/api/developer/dashboards", f.devSub, map[string]any{"name": "Unnamed"}); status != http.StatusOK {
		t.Fatalf("create dashboard without a revision: %d %s", status, body)
	}
	var rev *string
	_ = f.pool.QueryRow(ctx, `SELECT revision_id::text FROM model.dashboard_def WHERE model_id=$1::uuid AND name='Unnamed'`, f.modelID).Scan(&rev)
	if rev == nil || *rev != mineID {
		t.Errorf("dashboard without a revision filed under %v, want the newest revision %s", rev, mineID)
	}
}

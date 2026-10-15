package gateway

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// A pass cut short (the gateway killed mid-way) leaves a metric stale until
// one of its inputs changes; a developer recalculates the revision on
// request and the values come back.
func TestDeveloperRecalculatesARevision(t *testing.T) {
	f := setupDevAuthoringFixture(t)
	ctx := context.Background()

	status, body := f.do(t, http.MethodPost, "/api/developer/metrics", map[string]any{
		"name": "DoubleRevenue", "is_input": false, "formula": "FixtureRevenue * 2", "format": "number", "revision_id": f.revID,
	})
	if status != http.StatusOK {
		t.Fatalf("create calculated metric: %d %v", status, body)
	}
	calcID, _ := body["id"].(string)
	if status, body := f.do(t, http.MethodPost, "/api/import/upload", map[string]string{
		"csv": "FixtureRevenue\n21\n", "revision_id": f.revID, "import_mode": "replace",
	}); status != http.StatusOK {
		t.Fatalf("import: %d %v", status, body)
	}
	value := func() (float64, bool) {
		var v float64
		err := f.pool.QueryRow(ctx, `SELECT value::float8 FROM runtime.calc_result WHERE metric_id = $1::uuid AND dim_members = '{}'::jsonb`, calcID).Scan(&v)
		return v, err == nil
	}
	waitFor := func(what string, cond func() bool) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for !cond() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	waitFor("the import's recalculation", func() bool { v, ok := value(); return ok && v == 42 })

	// A pass killed mid-way: its results gone, its partition left calculating.
	if _, err := f.pool.Exec(ctx, `DELETE FROM runtime.calc_result WHERE metric_id = $1::uuid`, calcID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE runtime.metric_partition_state SET status = 'calculating' WHERE metric_id = $1::uuid`, calcID); err != nil {
		t.Fatal(err)
	}

	status, body = f.do(t, http.MethodPost, fmt.Sprintf("/api/developer/revisions/%s/recalculate", f.revID), nil)
	if status != http.StatusOK || body["recalculating"] != true || body["metrics"] != 1.0 {
		t.Fatalf("recalculate: %d %v, want 1 calculated metric recalculating", status, body)
	}
	waitFor("the requested recalculation", func() bool { v, ok := value(); return ok && v == 42 })
	var state string
	_ = f.pool.QueryRow(ctx, `SELECT status::text FROM runtime.metric_partition_state WHERE metric_id = $1::uuid`, calcID).Scan(&state)
	if state != "clean" {
		t.Errorf("the partition is %q after the recalculation, want clean", state)
	}
	if row := f.latestAuditEvent(t, "revision.recalculated"); row.resourceID != f.revID {
		t.Errorf("revision.recalculated resource_id = %s, want %s", row.resourceID, f.revID)
	}

	// A developer of another tenant cannot recalculate this one's revision.
	var scoped string
	if err := f.pool.QueryRow(ctx, `
		WITH c AS (INSERT INTO core.customer (name) VALUES ('Elsewhere') RETURNING id),
		     w AS (INSERT INTO core.workspace (customer_id, name) SELECT id, 'Their workspace' FROM c RETURNING id),
		     u AS (INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
		           SELECT 'scoped-dev', 'scoped@elsewhere.dev', 'Scoped', id FROM c RETURNING id)
		INSERT INTO identity.role_assignment (user_id, role, workspace_id) SELECT u.id, 'developer', w.id FROM u, w RETURNING user_id::text`).Scan(&scoped); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.srv.URL+"/api/developer/revisions/"+f.revID+"/recalculate", nil)
	req.Header.Set("X-Dev-User", "scoped-dev")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Error("a developer of another tenant recalculated this tenant's revision")
	}
}

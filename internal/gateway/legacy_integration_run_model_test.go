package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/importpkg"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// A legacy csv_import / google_sheets grid run writes into the
// integration's own model and revision — not the model the request's
// X-App-Id resolves to, which changes when the application's default model
// does. Both models here hold a metric of the same name, so a run that
// resolved the request's model would succeed, into the wrong one.
func TestLegacyIntegrationGridRunWritesIntoItsOwnModel(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('TwoModels','enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid,'ws') RETURNING id::text`, custID)
	appID := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid,$2::uuid,'App','planning') RETURNING id::text`, wsID, custID)
	model := func(name string) (modelID, revID, metricID string) {
		modelID = q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid,$2) RETURNING id::text`, appID, name)
		revID = q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid,'Working') RETURNING id::text`, modelID)
		exec(`UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, revID, modelID)
		metricID = q(`INSERT INTO model.metric_def (model_id, name, is_input, agg_rule, revision_id) VALUES ($1::uuid,'Revenue',true,'sum',$2::uuid) RETURNING id::text`, modelID, revID)
		return
	}
	modelA, revA, metricA := model("A")
	modelB, _, _ := model("B")
	exec(`UPDATE core.application SET default_model_id=$1::uuid WHERE id=$2::uuid`, modelA, appID)
	devSub := "two-models-dev"
	devID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1,'dev@two.com','Dev',$2::uuid) RETURNING id::text`, devSub, custID)
	exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid,'developer',$2::uuid)`, devID, wsID)

	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/csv")
		_, _ = w.Write([]byte("Revenue\n7\n"))
	}))
	t.Cleanup(fake.Close)
	t.Setenv("DEV_MODE", "true")
	h := &handler{
		log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), devMode: true,
		sheets: &importpkg.SheetFetcher{BaseURL: fake.URL, Client: fake.Client()},
	}
	mux := http.NewServeMux()
	h.registerRoutes(mux, nil)
	srv := httptest.NewServer(appIDMiddleware(mux))
	t.Cleanup(srv.Close)
	do := func(method, path string, body any) (int, string) {
		t.Helper()
		var buf []byte
		if body != nil {
			buf, _ = json.Marshal(body)
		}
		req, _ := http.NewRequestWithContext(ctx, method, srv.URL+path, bytes.NewReader(buf))
		req.Header.Set("X-Dev-User", devSub)
		req.Header.Set("X-App-Id", appID)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close() //nolint:errcheck
		out := new(bytes.Buffer)
		_, _ = out.ReadFrom(resp.Body)
		return resp.StatusCode, out.String()
	}
	create := func(typ, name string) string {
		t.Helper()
		status, body := do("POST", "/api/developer/integrations", map[string]any{"type": typ, "name": name, "target_type": "grid"})
		if status != http.StatusOK {
			t.Fatalf("create %s: %d %s", typ, status, body)
		}
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(body), &out)
		return out.ID
	}
	facts := func(modelID string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM runtime.fact_input WHERE model_id=$1::uuid`, modelID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	// Both saved while A is the application's default model.
	csvID := create("csv_import", "csv grid")
	sheetID := create("google_sheets", "sheet grid")
	if status, body := do("PATCH", "/api/developer/integrations/"+sheetID+"/config", map[string]any{
		"config": map[string]any{"sheet_url": sheetURL(sharedSheetID)},
	}); status != http.StatusOK {
		t.Fatalf("sheet config: %d %s", status, body)
	}
	var owner string
	_ = pool.QueryRow(ctx, `SELECT DISTINCT model_id::text FROM model.integration_def WHERE id IN ($1::uuid,$2::uuid)`, csvID, sheetID).Scan(&owner)
	if owner != modelA {
		t.Fatalf("integrations saved in model %s, want A %s", owner, modelA)
	}

	// The default moves to B; the integrations stay A's.
	exec(`UPDATE core.application SET default_model_id=$1::uuid WHERE id=$2::uuid`, modelB, appID)

	status, body := do("POST", "/api/integrations/"+csvID+"/run", map[string]any{"csv": "metric_id,value\n" + metricA + ",5\n"})
	if status != http.StatusOK || !strings.Contains(body, `"rows_imported":1`) {
		t.Fatalf("csv grid run: %d %s", status, body)
	}
	status, body = do("POST", "/api/integrations/"+sheetID+"/run", nil)
	if status != http.StatusOK || !strings.Contains(body, `"rows_imported":1`) {
		t.Fatalf("sheets grid run: %d %s", status, body)
	}
	if n := facts(modelA); n != 2 {
		t.Errorf("model A (the integrations' own) holds %d fact(s), want 2", n)
	}
	if n := facts(modelB); n != 0 {
		t.Errorf("model B (the request's default) holds %d fact(s), want 0", n)
	}
	var wrongRevision int
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM runtime.fact_input WHERE model_id=$1::uuid AND revision_id IS DISTINCT FROM $2::uuid`, modelA, revA).Scan(&wrongRevision)
	if wrongRevision != 0 {
		t.Errorf("%d fact(s) outside the integrations' revision", wrongRevision)
	}
}

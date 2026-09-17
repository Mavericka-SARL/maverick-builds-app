// End-to-end coverage for the Google Sheets import source, using
// handler.sheets — the package-private fetcher seam — to stand an httptest
// server in for docs.google.com. Proves the two entry points against a real
// database: POST /api/import/sheets/fetch (the wizard's preview) and a saved
// type="google_sheets" integration whose run re-fetches the sheet, applies
// its column_map, resolves rows by NAME (the legacy CSV grid path requires
// metric UUIDs — a maintained sheet never has those), and commits
// idempotently (default mode "replace": syncing twice must not double the
// values, which mode "incremental" would).
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

const (
	sharedSheetID  = "1SharedFixtureSheetId_000000000000000000000"
	privateSheetID = "1PrivateFixtureSheetId_00000000000000000000"
)

func setupSheetsFixture(t *testing.T) (do func(t *testing.T, method, path string, body any) (int, map[string]any), setSheetCSV func(string), queryValue func(t *testing.T) (float64, int)) {
	t.Helper()
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")

	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("query %q: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("exec %q: %v", sql, err)
		}
	}

	custID := q(`INSERT INTO core.customer (name) VALUES ('Sheets Co') RETURNING id::text`)
	appID := q(`INSERT INTO core.application (customer_id, name, mode) VALUES ($1::uuid, 'App', 'planning') RETURNING id::text`, custID)
	modelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Model') RETURNING id::text`, appID)
	revID := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'v1') RETURNING id::text`, modelID)
	exec(`UPDATE core.model SET active_revision_id=$2::uuid WHERE id=$1::uuid`, modelID, revID)
	metricID := q(`INSERT INTO model.metric_def (model_id, name, is_input, agg_rule, revision_id) VALUES ($1::uuid, 'FixtureRevenue', true, 'sum', $2::uuid) RETURNING id::text`, modelID, revID)

	devID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name) VALUES ('sheets-dev', 'dev@sheets.dev', 'Dev') RETURNING id::text`)
	exec(`INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'developer')`, devID)

	// Fake docs.google.com: serves whatever CSV the test sets for the
	// shared sheet, and a 200 HTML sign-in page for the private one — the
	// exact shape a real private sheet's export produces.
	sheetCSV := "FixtureRevenue\n100\n"
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/d/"+sharedSheetID+"/"):
			w.Header().Set("Content-Type", "text/csv")
			_, _ = w.Write(append([]byte("\xef\xbb\xbf"), []byte(sheetCSV)...))
		case strings.Contains(r.URL.Path, "/d/"+privateSheetID+"/"):
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<html>Sign in - Google Accounts</html>"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(fake.Close)

	h := &handler{
		log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), devMode: true,
		sheets: &importpkg.SheetFetcher{BaseURL: fake.URL, Client: fake.Client()},
	}
	mux := http.NewServeMux()
	h.registerRoutes(mux, nil)
	srv := httptest.NewServer(appIDMiddleware(mux))
	t.Cleanup(srv.Close)

	do = func(t *testing.T, method, path string, body any) (int, map[string]any) {
		t.Helper()
		var buf []byte
		if body != nil {
			var err error
			buf, err = json.Marshal(body)
			if err != nil {
				t.Fatalf("encode body: %v", err)
			}
		}
		req, err := http.NewRequestWithContext(context.Background(), method, srv.URL+path, bytes.NewReader(buf))
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		req.Header.Set("X-Dev-User", "sheets-dev")
		req.Header.Set("X-App-Id", appID)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do request: %v", err)
		}
		defer resp.Body.Close() //nolint:errcheck
		var parsed map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&parsed)
		return resp.StatusCode, parsed
	}
	setSheetCSV = func(csv string) { sheetCSV = csv }
	queryValue = func(t *testing.T) (float64, int) {
		t.Helper()
		// Latest-wins read, same DISTINCT ON shape every real read path uses.
		var latest float64
		var total int
		err := pool.QueryRow(ctx, `
			SELECT COALESCE((
				SELECT value::float8 FROM runtime.fact_input
				WHERE model_id=$1::uuid AND metric_id=$2::uuid
				ORDER BY entered_at DESC LIMIT 1
			), 0), (SELECT count(*) FROM runtime.fact_input WHERE model_id=$1::uuid AND metric_id=$2::uuid)
		`, modelID, metricID).Scan(&latest, &total)
		if err != nil {
			t.Fatalf("query facts: %v", err)
		}
		return latest, total
	}
	return do, setSheetCSV, queryValue
}

func sheetURL(id string) string {
	return fmt.Sprintf("https://docs.google.com/spreadsheets/d/%s/edit#gid=0", id)
}

func TestImportSheetFetch(t *testing.T) {
	do, _, _ := setupSheetsFixture(t)

	status, body := do(t, "POST", "/api/import/sheets/fetch", map[string]string{"sheet_url": sheetURL(sharedSheetID)})
	if status != http.StatusOK {
		t.Fatalf("fetch shared sheet: status=%d body=%v", status, body)
	}
	if csv, _ := body["csv"].(string); csv != "FixtureRevenue\n100\n" {
		t.Errorf("csv = %q, want the fixture sheet's contents with the BOM stripped", csv)
	}
	if id, _ := body["spreadsheet_id"].(string); id != sharedSheetID {
		t.Errorf("spreadsheet_id = %q, want %q", id, sharedSheetID)
	}

	status, body = do(t, "POST", "/api/import/sheets/fetch", map[string]string{"sheet_url": sheetURL(privateSheetID)})
	if status != http.StatusBadRequest {
		t.Fatalf("private sheet: status=%d body=%v, want 400", status, body)
	}
	if msg, _ := body["error"].(string); !strings.Contains(msg, "link-shared") {
		t.Errorf("private sheet error %q should tell the user to link-share the sheet", msg)
	}

	status, _ = do(t, "POST", "/api/import/sheets/fetch", map[string]string{"sheet_url": "https://example.com/spreadsheets/d/" + sharedSheetID})
	if status != http.StatusBadRequest {
		t.Fatalf("non-google URL: status=%d, want 400", status)
	}
}

func TestGoogleSheetsIntegrationRunSyncsGridFacts(t *testing.T) {
	do, setSheetCSV, queryValue := setupSheetsFixture(t)

	// The sheet's header is a bespoke label, proving the saved column_map
	// is applied before name-based resolution.
	setSheetCSV("Q3 Revenue\n100\n")

	status, body := do(t, "POST", "/api/developer/integrations", map[string]string{
		"name": "Q3 sheet sync", "type": "google_sheets", "target_type": "grid",
	})
	if status != http.StatusOK {
		t.Fatalf("create integration: status=%d body=%v", status, body)
	}
	intID, _ := body["id"].(string)
	if intID == "" {
		t.Fatalf("expected integration id, got %v", body)
	}
	status, body = do(t, "PATCH", "/api/developer/integrations/"+intID+"/config", map[string]any{
		"config": map[string]any{
			"sheet_url":  sheetURL(sharedSheetID),
			"column_map": map[string]string{"Q3 Revenue": "FixtureRevenue"},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("set config: status=%d body=%v", status, body)
	}

	// Run with an empty body — a google_sheets run needs none.
	status, body = do(t, "POST", "/api/integrations/"+intID+"/run", nil)
	if status != http.StatusOK {
		t.Fatalf("run integration: status=%d body=%v", status, body)
	}
	if got, _ := body["rows_imported"].(float64); got != 1 {
		t.Fatalf("rows_imported = %v, want 1 (body=%v)", body["rows_imported"], body)
	}
	if latest, _ := queryValue(t); latest != 100 {
		t.Errorf("latest fact value = %v, want 100", latest)
	}

	// The sheet changes; re-running must converge on the new value, not
	// accumulate — the default "replace" mode is what makes run a sync.
	setSheetCSV("Q3 Revenue\n250\n")
	status, body = do(t, "POST", "/api/integrations/"+intID+"/run", nil)
	if status != http.StatusOK {
		t.Fatalf("re-run integration: status=%d body=%v", status, body)
	}
	latest, total := queryValue(t)
	if latest != 250 {
		t.Errorf("latest fact value after re-sync = %v, want 250 (incremental would give 350)", latest)
	}
	if total != 2 {
		t.Errorf("fact_input rows = %d, want 2 (append-only history, one per sync)", total)
	}

	// A sheet row that fails validation is counted, not committed.
	setSheetCSV("Q3 Revenue\nnot-a-number\n")
	status, body = do(t, "POST", "/api/integrations/"+intID+"/run", nil)
	if status != http.StatusOK {
		t.Fatalf("run with bad row: status=%d body=%v", status, body)
	}
	if got, _ := body["error_rows"].(float64); got != 1 {
		t.Errorf("error_rows = %v, want 1 (body=%v)", body["error_rows"], body)
	}
	if latest, _ := queryValue(t); latest != 250 {
		t.Errorf("latest fact value after failed sync = %v, want unchanged 250", latest)
	}
}

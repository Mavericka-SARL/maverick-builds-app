package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// An import sent again under the same Idempotency-Key is answered from the
// first and not applied twice; one sent while the first still runs is
// refused; one whose first was cut off before its commit runs. Reported
// live: an import cut off by the gateway after it had committed was sent
// again by its client, and an incremental import added every value twice.
func TestImportSentAgainUnderItsKeyIsAppliedOnce(t *testing.T) {
	f := setupDevAuthoringFixture(t)
	ctx := context.Background()
	upload := func(key string) (int, map[string]any) {
		t.Helper()
		buf, _ := json.Marshal(map[string]string{"csv": "FixtureRevenue\n100\n", "revision_id": f.revID, "import_mode": "incremental"})
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.srv.URL+"/api/import/upload", bytes.NewReader(buf))
		req.Header.Set("X-Dev-User", "dev-audit-pa")
		req.Header.Set("X-App-Id", f.appID)
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("upload: %v", err)
		}
		defer resp.Body.Close() //nolint:errcheck
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}
	value := func() float64 {
		t.Helper()
		var v float64
		if err := f.pool.QueryRow(ctx, `SELECT value::float8 FROM runtime.fact_input WHERE metric_id = $1::uuid ORDER BY entered_at DESC, id DESC LIMIT 1`, f.metricID).Scan(&v); err != nil {
			t.Fatalf("read the cell: %v", err)
		}
		return v
	}

	status, first := upload("load-1")
	if status != http.StatusOK {
		t.Fatalf("first upload: %d %v", status, first)
	}
	status, again := upload("load-1")
	if status != http.StatusOK || again["replayed"] != true || again["job_id"] != first["job_id"] {
		t.Fatalf("the same upload sent again under its key: %d %v, want the first's answer replayed", status, again)
	}
	if got := value(); got != 100 {
		t.Errorf("the cell after sending the same import twice under one key = %v, want 100", got)
	}
	if status, body := upload("load-2"); status != http.StatusOK || body["replayed"] == true {
		t.Fatalf("an upload under a new key: %d %v", status, body)
	}
	if got := value(); got != 200 {
		t.Errorf("the cell after a second import under its own key = %v, want 200", got)
	}

	// A job under load-3 that validated and staged and has not committed: a
	// request still running. The same key again is refused...
	var userID string
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM identity.user WHERE keycloak_sub = 'dev-audit-pa'`).Scan(&userID); err != nil {
		t.Fatal(err)
	}
	var stuck string
	if err := f.pool.QueryRow(ctx, `
		INSERT INTO import.import_job (model_id, revision_id, file_url, created_by, status, idempotency_key)
		VALUES ($1::uuid, $2::uuid, 'upload', $3::uuid, 'staged', 'load-3') RETURNING id::text`,
		f.modelID, f.revID, userID).Scan(&stuck); err != nil {
		t.Fatal(err)
	}
	if status, body := upload("load-3"); status != http.StatusConflict {
		t.Errorf("an upload under the key of one still running: %d %v, want 409", status, body)
	}
	// ...and once it is older than any request may run, it was cut off
	// before its commit, nothing of it was saved, and the key runs.
	if _, err := f.pool.Exec(ctx, `UPDATE import.import_job SET created_at = $2 WHERE id = $1::uuid`, stuck, time.Now().Add(-importDeadline-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if status, body := upload("load-3"); status != http.StatusOK || body["replayed"] == true {
		t.Fatalf("an upload under the key of one cut off before its commit: %d %v, want it run", status, body)
	}
	if got := value(); got != 300 {
		t.Errorf("the cell = %v, want 300", got)
	}

	// An import's recalculation is recorded as pending, as a cell write's
	// is, so the grid says its calculated values are about to change.
	var requested int
	_ = f.pool.QueryRow(ctx, `SELECT requested FROM runtime.revision_recalc_state WHERE revision_id = $1::uuid`, f.revID).Scan(&requested)
	if requested < 3 {
		t.Errorf("the revision's recalculation was asked for %d time(s) after three imports, want 3", requested)
	}
}

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

	// An import the write guard refuses at its commit frees its key: sent
	// again, it is refused again — not answered "still running" for ten
	// minutes, as the Import Wizard's Commit sent again after a refusal was.
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'metric', $2, 'read')`, userID, f.metricID); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if status, body := upload("denied-1"); status != http.StatusForbidden {
			t.Errorf("send %d of an import the write guard refuses: %d %v, want 403", i+1, status, body)
		}
	}
}

// A table's records import sent again under the same Idempotency-Key is
// answered from the first and creates nothing: the key is kept on the
// import's Recent Imports line, written in the transaction that creates the
// records. It had no key at all (2026-10-15): a client sending a file again
// after a 502 created every record twice. A refused file holds no key, so
// the corrected file goes under the same one; two sends at once create one
// set of records; a key another kind of import used is refused.
func TestRecordsImportSentAgainUnderItsKeyIsAppliedOnce(t *testing.T) {
	f := setupDevAuthoringFixture(t)
	ctx := context.Background()
	send := func(path, key string, payload map[string]string) (int, map[string]any) {
		t.Helper()
		buf, _ := json.Marshal(payload)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, f.srv.URL+path, bytes.NewReader(buf))
		req.Header.Set("X-Dev-User", "dev-audit-pa")
		req.Header.Set("X-App-Id", f.appID)
		req.Header.Set("Content-Type", "application/json")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Errorf("send: %v", err)
			return 0, nil
		}
		defer resp.Body.Close() //nolint:errcheck
		var body map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&body)
		return resp.StatusCode, body
	}
	load := func(key, csv string) (int, map[string]any) {
		return send("/api/forms/"+f.formID+"/import", key, map[string]string{"csv": csv, "file_name": "spend.csv"})
	}
	records := func() int {
		t.Helper()
		var n int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM runtime.form_record WHERE form_id = $1::uuid`, f.formID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	const file = "amount\n10\n20\n"

	status, first := load("load-1", file)
	if status != http.StatusOK || first["records_created"] != 2.0 {
		t.Fatalf("first load: %d %v", status, first)
	}
	if first["job_id"] == nil {
		t.Errorf("the load's answer names no import job: %v", first)
	}
	status, again := load("load-1", file)
	if status != http.StatusOK || again["replayed"] != true || again["job_id"] != first["job_id"] || again["records_created"] != 2.0 {
		t.Errorf("the same load sent again under its key: %d %v, want the first's answer replayed", status, again)
	}
	if n := records(); n != 2 {
		t.Errorf("the table holds %d records after one file sent twice under one key, want 2", n)
	}
	if status, body := load("load-2", file); status != http.StatusOK || body["replayed"] == true {
		t.Fatalf("a load under a new key: %d %v", status, body)
	}
	if n := records(); n != 4 {
		t.Errorf("the table holds %d records after a second load under its own key, want 4", n)
	}

	// A refused file holds no key: its correction goes under the same one.
	if status, body := load("load-3", "amount\nabc\n"); status != http.StatusUnprocessableEntity {
		t.Fatalf("a refused file: %d %v, want 422", status, body)
	}
	if status, body := load("load-3", "amount\n5\n"); status != http.StatusOK || body["replayed"] == true || body["records_created"] != 1.0 {
		t.Fatalf("the corrected file under the refused one's key: %d %v, want it run", status, body)
	}
	if n := records(); n != 5 {
		t.Errorf("the table holds %d records, want 5", n)
	}

	// Two sends at once under one key: one creates the records, the other
	// answers its result (or that it still runs) and creates none.
	type answer struct {
		status int
		body   map[string]any
	}
	answers := make(chan answer, 2)
	for range 2 {
		go func() {
			s, b := load("load-4", file)
			answers <- answer{s, b}
		}()
	}
	replayedOrBusy := 0
	for range 2 {
		a := <-answers
		switch {
		case a.status == http.StatusConflict, a.status == http.StatusOK && a.body["replayed"] == true:
			replayedOrBusy++
		case a.status != http.StatusOK:
			t.Errorf("a concurrent send: %d %v", a.status, a.body)
		}
	}
	if replayedOrBusy != 1 {
		t.Errorf("of two sends at once under one key, %d were replayed or refused, want 1", replayedOrBusy)
	}
	if n := records(); n != 7 {
		t.Errorf("the table holds %d records after two sends at once under one key, want 7", n)
	}

	// Recent Imports: one committed line per load applied, the refused one.
	var committed, failed int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FILTER (WHERE status = 'committed'), count(*) FILTER (WHERE status = 'failed')
		FROM import.import_job WHERE model_id = $1::uuid AND target_type = 'table'`, f.modelID).Scan(&committed, &failed); err != nil {
		t.Fatal(err)
	}
	if committed != 4 || failed != 1 {
		t.Errorf("Recent Imports holds %d committed and %d failed table loads, want 4 and 1", committed, failed)
	}

	// A key a grid import used is not the records import's answer.
	if status, body := send("/api/import/upload", "grid-1", map[string]string{"csv": "FixtureRevenue\n100\n", "revision_id": f.revID}); status != http.StatusOK {
		t.Fatalf("grid upload: %d %v", status, body)
	}
	if status, body := load("grid-1", file); status != http.StatusConflict {
		t.Errorf("a records load under a grid import's key: %d %v, want 409", status, body)
	}
	if n := records(); n != 7 {
		t.Errorf("the table holds %d records, want 7", n)
	}
}

package gateway

// /api/grid resolves its caller first: an unauthenticated read answered 500,
// and 401 only for a grid that exists. An X-Model-Id the caller may not open
// is refused (404 MODEL_NOT_OPEN) rather than replaced by the application's
// default model without a word.

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestGridAndModelRefusals(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()
	get := func(path string, headers map[string]string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	const unknown = "00000000-0000-4000-8000-000000000001"
	signedIn := map[string]string{"X-Dev-User": "rollup-test-approver", "X-App-Id": f.appID}

	for what, path := range map[string]string{
		"no grid":      "/api/grid",
		"an existing":  "/api/grid?grid_def_id=" + f.gridStaffID,
		"an unknown":   "/api/grid?grid_def_id=" + unknown,
		"not an id at": "/api/grid?grid_def_id=nope",
	} {
		if status, body := get(path, nil); status != http.StatusUnauthorized {
			t.Errorf("unauthenticated read of %s grid: %d %s, want 401", what, status, body)
		}
	}
	for what, path := range map[string]string{"an unknown": "/api/grid?grid_def_id=" + unknown, "not an id at": "/api/grid?grid_def_id=nope"} {
		if status, body := get(path, signedIn); status != http.StatusNotFound {
			t.Errorf("read of %s grid: %d %s, want 404", what, status, body)
		}
	}

	// Another application's model, named as the selected one.
	var otherModel string
	if err := f.pool.QueryRow(ctx, `
		WITH a AS (INSERT INTO core.application (workspace_id, customer_id, name, mode)
		           SELECT workspace_id, customer_id, 'Other', 'planning' FROM core.application WHERE id=$1::uuid RETURNING id)
		INSERT INTO core.model (application_id, name) SELECT id, 'Other model' FROM a RETURNING id::text`, f.appID).Scan(&otherModel); err != nil {
		t.Fatal(err)
	}
	foreign := map[string]string{"X-Dev-User": "rollup-test-approver", "X-App-Id": f.appID, "X-Model-Id": otherModel}
	if status, body := get("/api/grid", foreign); status != http.StatusNotFound || !strings.Contains(body, "MODEL_NOT_OPEN") {
		t.Errorf("read naming another application's model: %d %s, want 404 MODEL_NOT_OPEN", status, body)
	}
	own := map[string]string{"X-Dev-User": "rollup-test-approver", "X-App-Id": f.appID, "X-Model-Id": f.modelID}
	if status, body := get("/api/grid", own); status != http.StatusOK {
		t.Errorf("read naming the application's own model: %d %s, want 200", status, body)
	}
}

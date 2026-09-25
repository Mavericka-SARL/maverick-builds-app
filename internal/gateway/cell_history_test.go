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

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// A cell's history is every row it ever had — including the ones a
// full-reload import or a form re-posting deleted, which the archive trigger
// keeps — newest first, with the value the grid shows marked. It is gated
// like the grid: a member hidden from the caller answers 403.
func TestCellHistory(t *testing.T) {
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
	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('HistCo', 'enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, custID)
	appID := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Plan', 'planning') RETURNING id::text`, wsID, custID)
	modelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Plan model') RETURNING id::text`, appID)
	revID := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, modelID)
	exec(`UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, revID, modelID)
	metricID := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule) VALUES ($1::uuid, $2::uuid, 'spend', true, 'sum') RETURNING id::text`, modelID, revID)
	calcID := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, formula, agg_rule) VALUES ($1::uuid, $2::uuid, 'double', false, '=spend*2', 'sum') RETURNING id::text`, modelID, revID)
	dimID := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'department') RETURNING id::text`, modelID, revID)
	salesID := q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'SALES', 'Sales') RETURNING id::text`, dimID)
	opsID := q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'OPS', 'Ops') RETURNING id::text`, dimID)
	exec(`INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id) VALUES ($1::uuid, 'OPS_EU', 'Ops EU', $2::uuid)`, dimID, opsID)

	user := func(sub, email string) string {
		id := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1,$2,$3,$4::uuid) RETURNING id::text`, sub, email, strings.Split(email, "@")[0], custID)
		exec(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid,'business_user',$2::uuid)`, id, wsID)
		return id
	}
	aliceID := user("hist-alice", "alice@histco.test")
	bobID := user("hist-bob", "bob@histco.test")
	_ = user("hist-carol", "carol@histco.test")
	// Carol may not see Sales, nor Ops (and so nothing under it).
	for _, hiddenID := range []string{salesID, opsID} {
		exec(`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) SELECT id, 'dimension_member', $1, 'hidden' FROM identity.user WHERE keycloak_sub='hist-carol'`, hiddenID)
	}

	t.Setenv("DEV_MODE", "true")
	srv := httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, nil, Deps{License: enterpriseManager(t)}))
	t.Cleanup(srv.Close)
	community := httptest.NewServer(NewHandler(logger.New("test"), pool, nil))
	t.Cleanup(community.Close)

	do := func(s *httptest.Server, persona, method, path string, body any) (int, []byte) {
		t.Helper()
		var buf []byte
		if body != nil {
			buf, _ = json.Marshal(body)
		}
		req, _ := http.NewRequestWithContext(ctx, method, s.URL+path, bytes.NewReader(buf))
		req.Header.Set("X-Dev-User", persona)
		req.Header.Set("X-App-Id", appID)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() //nolint:errcheck
		out := new(bytes.Buffer)
		_, _ = out.ReadFrom(resp.Body)
		return resp.StatusCode, out.Bytes()
	}
	write := func(persona string, value float64) {
		t.Helper()
		code, body := do(srv, persona, http.MethodPost, "/api/cells", map[string]any{
			"model_id": modelID, "revision_id": revID, "metric_id": metricID, "dim_codes": map[string]string{dimID: "SALES"}, "value": value,
		})
		if code != http.StatusOK {
			t.Fatalf("write %v as %s: %d %s", value, persona, code, body)
		}
	}
	// Alice writes 100, Bob writes 250, Alice writes 300; then an import in
	// full-reload mode deletes everything — the trigger keeps the rows.
	write("hist-alice", 100)
	write("hist-bob", 250)
	write("hist-alice", 300)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL mvx.delete_reason = 'import_full_reload'`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM runtime.fact_input WHERE model_id=$1::uuid AND value = 250`, modelID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	dims, _ := json.Marshal(map[string]string{dimID: "SALES"})
	path := fmt.Sprintf("/api/cells/history?model_id=%s&revision_id=%s&metric_id=%s&dim_codes=%s", modelID, revID, metricID, string(dims))

	code, body := do(srv, "hist-bob", http.MethodGet, path, nil)
	if code != http.StatusOK {
		t.Fatalf("history: %d %s", code, body)
	}
	var entries []map[string]any
	if err := json.Unmarshal(body, &entries); err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("%d entries, want 3 (two live, one archived): %s", len(entries), body)
	}
	values := []float64{entries[0]["value"].(float64), entries[1]["value"].(float64), entries[2]["value"].(float64)}
	if values[0] != 300 || values[1] != 250 || values[2] != 100 {
		t.Errorf("order newest first, got %v", values)
	}
	if entries[0]["current"] != true || entries[1]["current"] == true || entries[2]["current"] == true {
		t.Errorf("only the latest live row is current: %s", body)
	}
	by0 := entries[0]["entered_by"].(map[string]any)
	by1 := entries[1]["entered_by"].(map[string]any)
	if by0["id"] != aliceID || by1["id"] != bobID || by1["email"] != "bob@histco.test" {
		t.Errorf("authors: %v / %v", by0, by1)
	}
	if entries[1]["deleted_at"] == nil || !strings.Contains(fmt.Sprint(entries[1]["delete_reason"]), "full-reload") {
		t.Errorf("the archived row must say when and why it went: %v", entries[1])
	}
	if entries[0]["deleted_at"] != nil || entries[0]["source"].(map[string]any)["kind"] != "typed" {
		t.Errorf("live typed row: %v", entries[0])
	}

	// Gated exactly like the grid.
	if code, body := do(srv, "hist-carol", http.MethodGet, path, nil); code != http.StatusForbidden {
		t.Errorf("hidden member: %d %s", code, body)
	}
	// A hidden ancestor hides its descendants, as ExpandHidden does for the grid.
	opsEU, _ := json.Marshal(map[string]string{dimID: "OPS_EU"})
	childPath := strings.Replace(path, string(dims), string(opsEU), 1)
	if code, body := do(srv, "hist-carol", http.MethodGet, childPath, nil); code != http.StatusForbidden {
		t.Errorf("child of a hidden member: %d %s", code, body)
	}
	if code, body := do(srv, "hist-bob", http.MethodGet, childPath, nil); code != http.StatusOK {
		t.Errorf("child visible to an unrestricted user: %d %s", code, body)
	}
	if code, _ := do(community, "hist-bob", http.MethodGet, path, nil); code != http.StatusForbidden {
		t.Errorf("community edition: %d", code)
	}
	if code, _ := do(srv, "hist-bob", http.MethodGet, strings.Replace(path, metricID, calcID, 1), nil); code != http.StatusBadRequest {
		t.Errorf("calculated metric: %d", code)
	}
	unknown, _ := json.Marshal(map[string]string{dimID: "NOPE"})
	if code, _ := do(srv, "hist-bob", http.MethodGet, strings.Replace(path, string(dims), string(unknown), 1), nil); code != http.StatusNotFound {
		t.Errorf("unknown member: %d", code)
	}
	// Another intersection has its own, empty, history.
	ops, _ := json.Marshal(map[string]string{dimID: "OPS"})
	if code, body := do(srv, "hist-bob", http.MethodGet, strings.Replace(path, string(dims), string(ops), 1), nil); code != http.StatusOK || strings.TrimSpace(string(body)) != "[]" {
		t.Errorf("empty history: %d %s", code, body)
	}
}

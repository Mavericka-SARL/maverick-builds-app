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

	"github.com/mavericks-engine/mavericks/internal/integration"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// End-to-end HTTP lifecycle of the REST API connector endpoints: connection
// with write-only secret, atomic create, validate, activation gate, 202
// enqueue, run detail/cancel, duplicate, and the cross-tenant/role guards.
func TestRestAPIIntegrationEndpoints(t *testing.T) {
	t.Setenv("INTEGRATION_CRED_KEY", "0123456789abcdef0123456789abcdef")
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")

	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("seed: %v", err)
		}
		return id
	}
	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('RestCo','enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid,'ws') RETURNING id::text`, custID)
	appID := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid,$2::uuid,'App','planning') RETURNING id::text`, wsID, custID)
	modelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid,'M') RETURNING id::text`, appID)
	revID := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid,'Working') RETURNING id::text`, modelID)
	if _, err := pool.Exec(ctx, `UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, revID, modelID); err != nil {
		t.Fatal(err)
	}
	gridID := q(`INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid,'G',$2::uuid) RETURNING id::text`, modelID, revID)

	devSub := "restapi-dev"
	devID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1,'dev@restco.com','Dev',$2::uuid) RETURNING id::text`, devSub, custID)
	_ = devID
	if _, err := pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid,'developer',$2::uuid)`, devID, wsID); err != nil {
		t.Fatal(err)
	}
	// A business user (no developer role) for the role-gate check.
	bizSub := "restapi-biz"
	bizID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1,'biz@restco.com','Biz',$2::uuid) RETURNING id::text`, bizSub, custID)
	if _, err := pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid,'business_user',$2::uuid)`, bizID, wsID); err != nil {
		t.Fatal(err)
	}
	// A SECOND tenant for cross-tenant checks.
	cust2 := q(`INSERT INTO core.customer (name, plan) VALUES ('OtherCo','enterprise') RETURNING id::text`)
	ws2 := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid,'ws2') RETURNING id::text`, cust2)
	app2 := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid,$2::uuid,'App2','planning') RETURNING id::text`, ws2, cust2)
	foreignSub := "restapi-foreign"
	fID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1,'f@otherco.com','F',$2::uuid) RETURNING id::text`, foreignSub, cust2)
	if _, err := pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid,'developer',$2::uuid)`, fID, ws2); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DEV_MODE", "true")
	h := &handler{log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), devMode: true}
	mux := http.NewServeMux()
	h.registerRoutes(mux, nil)
	srv := httptest.NewServer(appIDMiddleware(mux))
	t.Cleanup(srv.Close)

	do := func(persona, method, path string, body any) (int, []byte) {
		t.Helper()
		var buf []byte
		if body != nil {
			buf, _ = json.Marshal(body)
		}
		req, _ := http.NewRequestWithContext(ctx, method, srv.URL+path, bytes.NewReader(buf))
		req.Header.Set("X-Dev-User", persona)
		req.Header.Set("X-App-Id", appID)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close() //nolint:errcheck
		out := new(bytes.Buffer)
		_, _ = out.ReadFrom(resp.Body)
		return resp.StatusCode, out.Bytes()
	}

	// ── Connection: create with secret; secret never comes back ──
	status, raw := do(devSub, "POST", "/api/developer/integration-connections", map[string]any{
		"name": "billing-api", "auth_type": "bearer",
		"meta":   map[string]any{"note": "prod billing"},
		"secret": map[string]any{"token": "SUPERSECRET-42"},
	})
	if status != 200 {
		t.Fatalf("create connection: %d %s", status, raw)
	}
	if strings.Contains(string(raw), "SUPERSECRET") {
		t.Fatal("create response leaks the secret")
	}
	var conn struct {
		ID        string `json:"id"`
		HasSecret bool   `json:"has_secret"`
	}
	_ = json.Unmarshal(raw, &conn)
	if !conn.HasSecret {
		t.Fatal("has_secret false")
	}
	// List: still no secret anywhere.
	_, raw = do(devSub, "GET", "/api/developer/integration-connections", nil)
	if strings.Contains(string(raw), "SUPERSECRET") || strings.Contains(string(raw), "secret_enc") {
		t.Fatal("list leaks credential material")
	}
	// Connection self-test passes shape check.
	_, raw = do(devSub, "POST", "/api/developer/integration-connections/"+conn.ID+"/test", nil)
	if !strings.Contains(string(raw), `"ok":true`) {
		t.Fatalf("connection test: %s", raw)
	}

	// ── Atomic create (draft) ──
	cfg := map[string]any{
		"kind": "rest_api/v1", "direction": "pull", "target_type": "grid",
		"target_id": gridID, "import_mode": "incremental",
		"request":  map[string]any{"method": "GET", "url": "https://api.example.com/v1/items", "body_mode": "none"},
		"auth":     map[string]any{"type": "bearer"},
		"response": map[string]any{"format": "json", "records_path": "$.data.items"},
		"mapping":  map[string]any{"fields": []map[string]any{{"source": "$.id", "target": "product", "target_kind": "dimension"}}},
	}
	status, raw = do(devSub, "POST", "/api/developer/integrations", map[string]any{
		"type": "rest_api", "name": "Billing pull", "config": cfg,
		"connection_id": conn.ID,
		"schedule":      map[string]any{"kind": "interval", "interval_seconds": 3600, "enabled": false},
	})
	if status != 200 {
		t.Fatalf("create integration: %d %s", status, raw)
	}
	var created struct {
		ID     string              `json:"id"`
		Status string              `json:"status"`
		Tested bool                `json:"tested"`
		Config *integration.Config `json:"config"`
	}
	_ = json.Unmarshal(raw, &created)
	if created.Status != "draft" || created.Tested || created.Config == nil {
		t.Fatalf("created shape: %s", raw)
	}

	// GET by id returns the typed document + schedule.
	status, raw = do(devSub, "GET", "/api/developer/integrations/"+created.ID, nil)
	if status != 200 || !strings.Contains(string(raw), `"records_path":"$.data.items"`) || !strings.Contains(string(raw), `"interval_seconds":3600`) {
		t.Fatalf("get: %d %s", status, raw)
	}

	// Validate endpoint: ok.
	_, raw = do(devSub, "POST", "/api/developer/integrations/"+created.ID+"/validate", nil)
	if !strings.Contains(string(raw), `"valid":true`) {
		t.Fatalf("validate: %s", raw)
	}

	// ── Activation gate: refused before a test, allowed after MarkTested ──
	status, raw = do(devSub, "PATCH", "/api/developer/integrations/"+created.ID, map[string]any{"status": "active"})
	if status == 200 {
		t.Fatalf("activated without test: %s", raw)
	}
	st := integration.NewStore(pool)
	def, _ := st.GetDefinition(ctx, modelID, created.ID)
	if err := st.MarkTested(ctx, created.ID, integration.ConfigHash(def.Config)); err != nil {
		t.Fatal(err)
	}
	status, raw = do(devSub, "PATCH", "/api/developer/integrations/"+created.ID, map[string]any{"status": "active"})
	if status != 200 {
		t.Fatalf("activate after test: %d %s", status, raw)
	}

	// ── /test enqueues 202; mutation methods demand acknowledgement ──
	status, raw = do(devSub, "POST", "/api/developer/integrations/"+created.ID+"/test", nil)
	if status != http.StatusAccepted {
		t.Fatalf("test enqueue: %d %s", status, raw)
	}
	var enq struct {
		RunID string `json:"run_id"`
	}
	_ = json.Unmarshal(raw, &enq)
	if enq.RunID == "" {
		t.Fatal("no run id")
	}
	// Run detail is queued; cancel flips it to cancelled.
	_, raw = do(devSub, "GET", "/api/developer/integration-runs/"+enq.RunID, nil)
	if !strings.Contains(string(raw), `"status":"queued"`) {
		t.Fatalf("run detail: %s", raw)
	}
	_, raw = do(devSub, "POST", "/api/developer/integration-runs/"+enq.RunID+"/cancel", nil)
	if !strings.Contains(string(raw), "cancelled") {
		t.Fatalf("cancel: %s", raw)
	}

	// /run on the active integration → 202 as well (business-button path).
	status, raw = do(devSub, "POST", "/api/integrations/"+created.ID+"/run", map[string]any{})
	if status != http.StatusAccepted {
		t.Fatalf("run enqueue: %d %s", status, raw)
	}

	// Runs listing carries the extended shape.
	_, raw = do(devSub, "GET", "/api/developer/integrations/"+created.ID+"/runs", nil)
	if !strings.Contains(string(raw), `"trigger_type"`) {
		t.Fatalf("runs listing not extended: %s", raw)
	}

	// ── Duplicate: draft copy, test state cleared, schedule disabled ──
	status, raw = do(devSub, "POST", "/api/developer/integrations/"+created.ID+"/duplicate", nil)
	if status != 200 {
		t.Fatalf("duplicate: %d %s", status, raw)
	}
	var dup struct {
		ID       string                `json:"id"`
		Status   string                `json:"status"`
		Tested   bool                  `json:"tested"`
		Schedule *integration.Schedule `json:"schedule"`
	}
	_ = json.Unmarshal(raw, &dup)
	if dup.Status != "draft" || dup.Tested || (dup.Schedule != nil && dup.Schedule.Enabled) {
		t.Fatalf("duplicate shape: %s", raw)
	}

	// ── Guards ──
	// Business role cannot touch developer endpoints.
	if status, _ = do(bizSub, "GET", "/api/developer/integration-connections", nil); status != http.StatusForbidden && status != http.StatusUnauthorized {
		t.Fatalf("business user reached developer connections: %d", status)
	}
	if status, _ = do(bizSub, "POST", "/api/developer/integrations/"+created.ID+"/test", nil); status != http.StatusForbidden && status != http.StatusUnauthorized {
		t.Fatalf("business user enqueued a test: %d", status)
	}
	// Foreign-tenant developer cannot reach the integration or its runs.
	if status, _ = do(foreignSub, "GET", "/api/developer/integrations/"+created.ID, nil); status < 400 {
		t.Fatalf("foreign tenant read the integration: %d", status)
	}
	if status, _ = do(foreignSub, "POST", "/api/developer/integrations/"+created.ID+"/duplicate", nil); status < 400 {
		t.Fatalf("foreign tenant duplicated the integration: %d", status)
	}
	// Foreign target id in config is refused at save.
	model2 := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid,'M2') RETURNING id::text`, app2)
	foreignGrid := q(`INSERT INTO model.grid_def (model_id, name) VALUES ($1::uuid,'FG') RETURNING id::text`, model2)
	badCfg := map[string]any{}
	for k, v := range cfg {
		badCfg[k] = v
	}
	badCfg["target_id"] = foreignGrid
	status, raw = do(devSub, "PATCH", "/api/developer/integrations/"+created.ID, map[string]any{"config": badCfg})
	if status == 200 {
		t.Fatalf("foreign target accepted: %s", raw)
	}
	// Secret PATCH semantics: replace without disclosure, then remove (null).
	status, raw = do(devSub, "PATCH", "/api/developer/integration-connections/"+conn.ID, map[string]any{
		"secret": map[string]any{"token": "ROTATED"},
	})
	if status != 200 || strings.Contains(string(raw), "ROTATED") {
		t.Fatalf("secret replace: %d %s", status, raw)
	}
	_, _, secret, _ := st.OpenCredential(ctx, appID, conn.ID)
	if !strings.Contains(string(secret), "ROTATED") {
		t.Fatal("replace not applied")
	}
	var nullBody = json.RawMessage(`{"secret": null}`)
	req, _ := http.NewRequestWithContext(ctx, "PATCH", srv.URL+"/api/developer/integration-connections/"+conn.ID, bytes.NewReader(nullBody))
	req.Header.Set("X-Dev-User", devSub)
	req.Header.Set("X-App-Id", appID)
	req.Header.Set("Content-Type", "application/json")
	resp, _ := http.DefaultClient.Do(req)
	b := new(bytes.Buffer)
	_, _ = b.ReadFrom(resp.Body)
	resp.Body.Close() //nolint:errcheck
	if !strings.Contains(b.String(), `"has_secret":false`) {
		t.Fatalf("secret remove: %s", b.String())
	}
	// Deleting a referenced connection is refused.
	if status, raw = do(devSub, "DELETE", "/api/developer/integration-connections/"+conn.ID, nil); status == 200 {
		t.Fatalf("referenced connection deleted: %s", raw)
	}
	// No secret anywhere in the integration document either.
	_, raw = do(devSub, "GET", "/api/developer/integrations/"+created.ID, nil)
	for _, needle := range []string{"ROTATED", "SUPERSECRET", "secret_enc", "iv1:"} {
		if strings.Contains(string(raw), needle) {
			t.Fatalf("integration document leaks %q", needle)
		}
	}
	_ = fmt.Sprint() // keep fmt import when assertions above change
}

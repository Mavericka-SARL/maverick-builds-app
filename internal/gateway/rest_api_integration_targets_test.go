package gateway

import (
	"bytes"
	"context"
	"encoding/json"
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

// Every path that stores or uses an integration's target refuses another
// tenant's row with the 400 the checked paths already answered: rest_api
// create (draft too), typed PATCH (config, connection, activation), the
// legacy config PATCH, duplicate, /test, dry-run, run, and the legacy
// csv_import/google_sheets create, PATCH and run. Nothing is stored,
// queued or written. Own-model targets keep working on the same paths, and
// a target deleted since it was chosen blocks activation and runs but not
// a rename, a connection change or a copy.
func TestIntegrationTargetsMustBeOwnModel(t *testing.T) {
	t.Setenv("INTEGRATION_CRED_KEY", "0123456789abcdef0123456789abcdef")
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
	count := func(sql string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", sql, err)
		}
		return n
	}

	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('OwnCo','enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid,'ws') RETURNING id::text`, custID)
	appID := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid,$2::uuid,'App','planning') RETURNING id::text`, wsID, custID)
	modelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid,'M') RETURNING id::text`, appID)
	revID := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid,'Working') RETURNING id::text`, modelID)
	if _, err := pool.Exec(ctx, `UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid`, revID, modelID); err != nil {
		t.Fatal(err)
	}
	ownGrid := q(`INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid,'G',$2::uuid) RETURNING id::text`, modelID, revID)
	ownDim := q(`INSERT INTO model.dimension_def (model_id, name, revision_id) VALUES ($1::uuid,'region',$2::uuid) RETURNING id::text`, modelID, revID)
	devSub := "targets-dev"
	devID := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ($1,'dev@ownco.com','Dev',$2::uuid) RETURNING id::text`, devSub, custID)
	if _, err := pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid,'developer',$2::uuid)`, devID, wsID); err != nil {
		t.Fatal(err)
	}

	// Another tenant's rows, which the developer above knows the ids of.
	cust2 := q(`INSERT INTO core.customer (name, plan) VALUES ('OtherCo','enterprise') RETURNING id::text`)
	ws2 := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid,'ws2') RETURNING id::text`, cust2)
	app2 := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid,$2::uuid,'App2','planning') RETURNING id::text`, ws2, cust2)
	model2 := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid,'M2') RETURNING id::text`, app2)
	foreignDim := q(`INSERT INTO model.dimension_def (model_id, name) VALUES ($1::uuid,'accounts') RETURNING id::text`, model2)
	foreignForm := q(`INSERT INTO model.form_def (model_id, name, label) VALUES ($1::uuid,'payroll','Payroll') RETURNING id::text`, model2)

	st := integration.NewStore(pool)
	foreignConn, err := st.CreateConnection(ctx, app2, "theirs", "none", nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("DEV_MODE", "true")
	h := &handler{log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), devMode: true}
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
	refused := func(what string, status int, body string) {
		t.Helper()
		if status != http.StatusBadRequest {
			t.Errorf("%s: got %d %s, want 400", what, status, body)
		}
	}

	cfg := func(targetType, targetID string) map[string]any {
		return map[string]any{
			"kind": "rest_api/v1", "direction": "pull", "target_type": targetType,
			"target_id": targetID, "import_mode": "incremental",
			"request":  map[string]any{"method": "GET", "url": "https://api.example.com/v1/items", "body_mode": "none"},
			"auth":     map[string]any{"type": "none"},
			"response": map[string]any{"format": "json", "records_path": "$.items"},
			"mapping":  map[string]any{"fields": []map[string]any{{"source": "$.code", "target": "code"}}},
		}
	}
	defsNamed := func(name string) int {
		return count(`SELECT COUNT(*) FROM model.integration_def WHERE model_id=$1::uuid AND name=$2`, modelID, name)
	}
	runsOf := func(id string) int {
		return count(`SELECT COUNT(*) FROM model.integration_run WHERE integration_id=$1::uuid`, id)
	}
	storedTarget := func(id string) string {
		var tid string
		_ = pool.QueryRow(ctx, `SELECT COALESCE(config->>'target_id', target_id::text, '') FROM model.integration_def WHERE id=$1::uuid`, id).Scan(&tid)
		return tid
	}

	// ── rest_api create: a draft is refused too ──
	status, body := do("POST", "/api/developer/integrations", map[string]any{
		"type": "rest_api", "name": "draft-foreign", "config": cfg("dimension", foreignDim)})
	refused("rest_api draft create with a foreign dimension", status, body)
	if defsNamed("draft-foreign") != 0 {
		t.Error("a draft with a foreign target was stored")
	}
	status, body = do("POST", "/api/developer/integrations", map[string]any{
		"type": "rest_api", "name": "draft-foreign-conn", "config": cfg("dimension", ownDim), "connection_id": foreignConn.ID})
	refused("rest_api draft create with a foreign connection", status, body)

	status, body = do("POST", "/api/developer/integrations", map[string]any{
		"type": "rest_api", "name": "own", "config": cfg("dimension", ownDim)})
	if status != http.StatusOK {
		t.Fatalf("own-model draft create: %d %s", status, body)
	}
	var own struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(body), &own)

	// ── typed PATCH: config, connection alone ──
	status, body = do("PATCH", "/api/developer/integrations/"+own.ID, map[string]any{"config": cfg("form", foreignForm)})
	refused("typed PATCH to a foreign form", status, body)
	status, body = do("PATCH", "/api/developer/integrations/"+own.ID, map[string]any{"connection_id": foreignConn.ID})
	refused("typed PATCH to a foreign connection", status, body)

	// ── legacy config PATCH on a rest_api integration: typed path now ──
	status, body = do("PATCH", "/api/developer/integrations/"+own.ID+"/config", map[string]any{"config": cfg("dimension", foreignDim)})
	refused("legacy config PATCH to a foreign dimension", status, body)
	if got := storedTarget(own.ID); got != ownDim {
		t.Errorf("legacy config PATCH stored target %s, want the own dimension %s", got, ownDim)
	}
	status, body = do("PATCH", "/api/developer/integrations/"+own.ID+"/config", map[string]any{"config": cfg("grid", ownGrid)})
	if status != http.StatusOK || !strings.Contains(body, `"status":"ok"`) {
		t.Fatalf("legacy config PATCH to an own grid: %d %s", status, body)
	}
	var colType, colTarget string
	_ = pool.QueryRow(ctx, `SELECT target_type, COALESCE(target_id::text,'') FROM model.integration_def WHERE id=$1::uuid`, own.ID).Scan(&colType, &colTarget)
	if colType != "grid" || colTarget != ownGrid {
		t.Errorf("legacy config PATCH left the typed columns at %s/%s, want grid/%s", colType, colTarget, ownGrid)
	}
	// Own-model paths still work end to end: /test queues.
	if status, body = do("POST", "/api/developer/integrations/"+own.ID+"/test", nil); status != http.StatusAccepted {
		t.Fatalf("own-model /test: %d %s", status, body)
	}
	if status, body = do("POST", "/api/developer/integrations/"+own.ID+"/duplicate", nil); status != http.StatusOK {
		t.Fatalf("own-model duplicate: %d %s", status, body)
	}

	// ── A stored foreign target: a row saved before this check existed ──
	stale, err := st.CreateDefinition(ctx, modelID, revID, "stale", "", nil, "draft", "",
		&integration.Config{
			Kind: integration.ConfigKind, Direction: integration.DirectionPull,
			TargetType: integration.TargetDimension, TargetID: foreignDim,
			ImportMode: integration.ModeIncremental,
			Request:    integration.RequestConfig{Method: "GET", URL: "https://api.example.com/v1/items", BodyMode: integration.BodyNone},
			Auth:       integration.AuthPlacement{Type: "none"},
			Response:   integration.ResponseConfig{Format: integration.FormatJSON, RecordsPath: "$.items"},
			Mapping:    integration.MappingConfig{Fields: []integration.FieldMap{{Source: "$.code", Target: "code"}}},
		}, true)
	if err != nil {
		t.Fatal(err)
	}
	status, body = do("POST", "/api/developer/integrations/"+stale.ID+"/test", nil)
	refused("/test of a stored foreign target", status, body)
	status, body = do("POST", "/api/developer/integrations/"+stale.ID+"/test?dry_run=1", nil)
	refused("dry-run of a stored foreign target", status, body)
	status, body = do("POST", "/api/developer/integrations/"+stale.ID+"/duplicate", nil)
	refused("duplicate of a stored foreign target", status, body)
	if defsNamed("stale (copy)") != 0 {
		t.Error("duplicate copied a foreign target")
	}
	status, body = do("POST", "/api/developer/integrations/"+stale.ID+"/validate", nil)
	if status != http.StatusOK || !strings.Contains(body, `"valid":false`) || !strings.Contains(body, "different model") {
		t.Errorf("validate of a stored foreign target: %d %s", status, body)
	}
	// Activation re-checks the stored config (a test is on record, so only
	// the target stands in the way).
	def, _ := st.GetDefinition(ctx, modelID, stale.ID)
	if err := st.MarkTested(ctx, stale.ID, integration.ConfigHash(def.Config)); err != nil {
		t.Fatal(err)
	}
	status, body = do("PATCH", "/api/developer/integrations/"+stale.ID, map[string]any{"status": "active"})
	refused("activating a stored foreign target", status, body)
	// Run, as an active row that got there some other way.
	if _, err := pool.Exec(ctx, `UPDATE model.integration_def SET status='active', enabled=true WHERE id=$1::uuid`, stale.ID); err != nil {
		t.Fatal(err)
	}
	status, body = do("POST", "/api/integrations/"+stale.ID+"/run", map[string]any{})
	refused("run of a stored foreign target", status, body)
	if n := runsOf(stale.ID); n != 0 {
		t.Errorf("%d run(s) queued for a foreign target", n)
	}

	// ── legacy csv_import / google_sheets create and PATCH ──
	for _, typ := range []string{"csv_import", "google_sheets"} {
		name := typ + "-foreign"
		status, body = do("POST", "/api/developer/integrations", map[string]any{
			"type": typ, "name": name, "target_type": "form", "target_id": foreignForm, "status": "draft"})
		refused(typ+" create with a foreign form", status, body)
		if defsNamed(name) != 0 {
			t.Errorf("%s with a foreign target was stored", typ)
		}
	}
	status, body = do("POST", "/api/developer/integrations", map[string]any{
		"type": "csv_import", "name": "csv-own", "target_type": "dimension", "target_id": ownDim})
	if status != http.StatusOK {
		t.Fatalf("own-model csv create: %d %s", status, body)
	}
	var csvOwn struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(body), &csvOwn)
	status, body = do("PATCH", "/api/developer/integrations/"+csvOwn.ID, map[string]any{
		"name": "csv-own", "target_type": "dimension", "target_id": foreignDim})
	refused("csv PATCH to a foreign dimension", status, body)
	if got := storedTarget(csvOwn.ID); got != ownDim {
		t.Errorf("csv PATCH stored target %s, want the own dimension %s", got, ownDim)
	}
	status, body = do("PATCH", "/api/developer/integrations/"+csvOwn.ID, map[string]any{
		"name": "csv-own", "target_type": "grid", "target_id": ownGrid})
	if status != http.StatusOK {
		t.Fatalf("own-model csv PATCH: %d %s", status, body)
	}

	// ── legacy run of a stored foreign target: a csv_import/google_sheets
	// row saved before the save-time check existed ──
	plant := func(typ, name, targetType, targetID, config string) string {
		return q(`INSERT INTO model.integration_def (model_id, revision_id, name, type, target_type, target_id, status, config)
			VALUES ($1::uuid,$2::uuid,$3,$4,$5,$6::uuid,'active',$7::jsonb) RETURNING id::text`,
			modelID, revID, name, typ, targetType, targetID, config)
	}
	foreignMembers := func() int {
		return count(`SELECT COUNT(*) FROM model.dimension_member WHERE dimension_id=$1::uuid`, foreignDim)
	}
	membersBefore := foreignMembers()
	plantedDim := plant("csv_import", "planted-dim", "dimension", foreignDim, `{}`)
	status, body = do("POST", "/api/integrations/"+plantedDim+"/run", map[string]any{"csv": "code,label\nPWN,Injected\n"})
	refused("legacy run of a stored foreign dimension", status, body)
	if n := foreignMembers(); n != membersBefore {
		t.Errorf("legacy run wrote %d member(s) into another tenant's dimension", n-membersBefore)
	}
	if n := runsOf(plantedDim); n != 0 {
		t.Errorf("%d run(s) recorded for a foreign dimension", n)
	}
	plantedForm := plant("csv_import", "planted-form", "form", foreignForm, `{}`)
	status, body = do("POST", "/api/integrations/"+plantedForm+"/run", map[string]any{"csv": "a\n1\n"})
	refused("legacy run of a stored foreign form", status, body)
	if n := count(`SELECT COUNT(*) FROM runtime.form_record WHERE form_id=$1::uuid`, foreignForm); n != 0 {
		t.Errorf("legacy run wrote %d record(s) into another tenant's form", n)
	}
	if n := runsOf(plantedForm); n != 0 {
		t.Errorf("%d run(s) recorded for a foreign form", n)
	}
	// A google_sheets row is refused before its sheet is fetched: the
	// target, not the missing sheet_url, is what stops it.
	plantedSheet := plant("google_sheets", "planted-sheet", "dimension", foreignDim, `{}`)
	status, body = do("POST", "/api/integrations/"+plantedSheet+"/run", nil)
	if status != http.StatusBadRequest || !strings.Contains(body, "different model") {
		t.Errorf("legacy sheets run of a stored foreign dimension: %d %s, want 400 naming the different model", status, body)
	}
	// Resending a foreign target unchanged (a rename) is still refused.
	status, body = do("PATCH", "/api/developer/integrations/"+plantedDim, map[string]any{
		"name": "renamed", "target_type": "dimension", "target_id": foreignDim})
	refused("rename resending a stored foreign target", status, body)

	// Own-model legacy run still works.
	runDim := q(`INSERT INTO model.dimension_def (model_id, name, revision_id) VALUES ($1::uuid,'dept',$2::uuid) RETURNING id::text`, modelID, revID)
	status, body = do("POST", "/api/developer/integrations", map[string]any{
		"type": "csv_import", "name": "csv-run-own", "target_type": "dimension", "target_id": runDim})
	if status != http.StatusOK {
		t.Fatalf("own-model csv create for a run: %d %s", status, body)
	}
	var runOwn struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(body), &runOwn)
	status, body = do("POST", "/api/integrations/"+runOwn.ID+"/run", map[string]any{"csv": "code,label\nUS,United States\n"})
	if status != http.StatusOK || !strings.Contains(body, `"rows_imported":1`) {
		t.Fatalf("own-model legacy run: %d %s", status, body)
	}
	if n := count(`SELECT COUNT(*) FROM model.dimension_member WHERE dimension_id=$1::uuid`, runDim); n != 1 {
		t.Errorf("own-model legacy run wrote %d member(s), want 1", n)
	}

	// ── A target deleted since it was chosen: nothing clears it, and it
	// must not block a rename, a tag, a connection change or a copy.
	// Activation and runs still refuse it. ──
	goneGrid := q(`INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid,'Gone',$2::uuid) RETURNING id::text`, modelID, revID)
	status, body = do("POST", "/api/developer/integrations", map[string]any{
		"type": "csv_import", "name": "csv-gone", "target_type": "grid", "target_id": goneGrid})
	if status != http.StatusOK {
		t.Fatalf("csv create on a grid about to be deleted: %d %s", status, body)
	}
	var csvGone struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(body), &csvGone)
	restGone := q(`INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid,'GoneToo',$2::uuid) RETURNING id::text`, modelID, revID)
	status, body = do("POST", "/api/developer/integrations", map[string]any{
		"type": "rest_api", "name": "rest-gone", "config": cfg("grid", restGone)})
	if status != http.StatusOK {
		t.Fatalf("rest_api create on a grid about to be deleted: %d %s", status, body)
	}
	var restGoneDef struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(body), &restGoneDef)
	for _, g := range []string{goneGrid, restGone} {
		if status, body = do("DELETE", "/api/developer/grids/"+g, nil); status != http.StatusOK {
			t.Fatalf("delete grid %s: %d %s", g, status, body)
		}
	}
	status, body = do("PATCH", "/api/developer/integrations/"+csvGone.ID, map[string]any{
		"name": "csv renamed", "target_type": "grid", "target_id": goneGrid, "tags": []string{"x"}})
	if status != http.StatusOK {
		t.Errorf("rename of a csv integration whose own target was deleted: %d %s, want 200", status, body)
	}
	status, body = do("PATCH", "/api/developer/integrations/"+csvGone.ID, map[string]any{
		"name": "csv renamed", "target_type": "grid", "target_id": goneGrid, "status": "active"})
	refused("activating a csv integration whose target was deleted", status, body)
	ownConn, err := st.CreateConnection(ctx, appID, "ours", "none", nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	status, body = do("PATCH", "/api/developer/integrations/"+restGoneDef.ID, map[string]any{"connection_id": ownConn.ID})
	if status != http.StatusOK {
		t.Errorf("connection change on a rest_api integration whose own target was deleted: %d %s, want 200", status, body)
	}
	status, body = do("POST", "/api/developer/integrations/"+restGoneDef.ID+"/duplicate", nil)
	if status != http.StatusOK {
		t.Errorf("duplicate of a rest_api integration whose own target was deleted: %d %s, want 200", status, body)
	}
	status, body = do("PATCH", "/api/developer/integrations/"+restGoneDef.ID, map[string]any{"status": "active"})
	refused("activating a rest_api integration whose target was deleted", status, body)
}

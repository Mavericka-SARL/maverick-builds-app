package gateway

// The AI Developer is held to the developer role's own limits and effects
// through hooks the gateway passes its executor (aiWriteHooks): the tenant's
// plan limits, and form-record posting. These tests run the real hooks.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/plan"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// newAIParityFixture builds a tenant on planKey (limits, when given, define
// that plan), a model with an active revision, and a developer; it returns
// the gateway, and an AI executor carrying the gateway's real hooks.
func newAIParityFixture(t *testing.T, planKey, limits string) (context.Context, *httptest.Server, *aiassistant.WriteExecutor, func(string, ...any) string, map[string]string) {
	t.Helper()
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	t.Setenv("DEV_MODE", "true")
	if limits != "" {
		if _, err := pool.Exec(ctx, `INSERT INTO platform.plan (key, name, limits) VALUES ($1, $1, $2::jsonb)`, planKey, limits); err != nil {
			t.Fatal(err)
		}
	}
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	cust := q(`INSERT INTO core.customer (name, plan) VALUES ('Parity Co', $1) RETURNING id::text`, planKey)
	ws := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'ws') RETURNING id::text`, cust)
	app := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid, $2::uuid, 'Parity', 'planning') RETURNING id::text`, ws, cust)
	model := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'Parity model') RETURNING id::text`, app)
	rev := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, model)
	q(`UPDATE core.model SET active_revision_id=$1::uuid, active_revision_name='Working' WHERE id=$2::uuid RETURNING id::text`, rev, model)
	dev := q(`INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('parity-dev', 'dev@parity.test', 'Dev', $1::uuid) RETURNING id::text`, cust)
	q(`INSERT INTO identity.role_assignment (user_id, role, workspace_id) VALUES ($1::uuid, 'developer', $2::uuid) RETURNING user_id::text`, dev, ws)

	enforcer := plan.NewEnforcer(pool)
	h := &handler{log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), devMode: true, plans: enforcer}
	srv := httptest.NewServer(NewHandlerWithDeps(logger.New("test"), pool, nil, Deps{Plans: enforcer}))
	t.Cleanup(srv.Close)
	exec := aiassistant.NewWriteExecutorWithActor(pool, model, rev, dev).WithHooks(h.aiWriteHooks(model, dev))
	return ctx, srv, exec, q, map[string]string{"app": app, "model": model, "rev": rev, "dev": dev}
}

// A plan capped at one metric and two members per dimension refuses the
// assistant exactly where it refuses the developer.
func TestAIWriteHooks_PlanLimitsHoldTheAssistant(t *testing.T) {
	ctx, srv, ai, q, ids := newAIParityFixture(t, "tiny", `{"max_metrics_per_model": 1, "max_members_per_dimension": 2}`)
	app, rev := ids["app"], ids["rev"]
	run := func(tool string, params map[string]any) (string, error) {
		t.Helper()
		res, _, err := ai.Execute(ctx, tool, mustJSONGateway(t, params))
		return res, err
	}

	if _, err := run("create_metric", map[string]any{"name": "revenue", "is_input": true}); err != nil {
		t.Fatalf("first metric: %v", err)
	}
	_, err := run("create_metric", map[string]any{"name": "cost", "is_input": true})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "metric") {
		t.Errorf("assistant's second metric: err %v, want the plan's refusal", err)
	}
	// The developer is refused the same second metric.
	code, body := callJSON(t, srv, "parity-dev", http.MethodPost, "/api/developer/metrics",
		map[string]any{"name": "cost", "is_input": true, "revision_id": rev}, "X-App-Id", app)
	if code != http.StatusPaymentRequired {
		t.Errorf("developer's second metric: %d %v, want 402", code, body)
	}

	if _, err := run("create_dimension", map[string]any{"name": "region", "members": []map[string]any{
		{"code": "A", "label": "A"}, {"code": "B", "label": "B"}, {"code": "C", "label": "C"}}}); err == nil {
		t.Error("assistant created a dimension with 3 members on a 2-member plan")
	}
	if _, err := run("create_dimension", map[string]any{"name": "region", "members": []map[string]any{
		{"code": "A", "label": "A"}, {"code": "B", "label": "B"}}}); err != nil {
		t.Fatalf("2 members: %v", err)
	}
	if _, err := run("add_dimension_member", map[string]any{"dimension_id": "region", "code": "C", "label": "C"}); err == nil {
		t.Error("assistant added a third member on a 2-member plan")
	}
	// The developer's member file is held to the same limit: it passed it
	// by any number of members. A file that only updates existing members
	// still goes through.
	dim := q(`SELECT id::text FROM model.dimension_def WHERE revision_id=$1::uuid AND name='region'`, rev)
	code, body = callJSON(t, srv, "parity-dev", http.MethodPost, "/api/import/dimension-members",
		map[string]any{"dimension_id": dim, "csv": "code,label\nC,C\nD,D\n"}, "X-App-Id", app)
	if code != http.StatusPaymentRequired {
		t.Errorf("developer's file of 2 new members: %d %v, want 402", code, body)
	}
	if n := q(`SELECT count(*)::text FROM model.dimension_member WHERE dimension_id=$1::uuid`, dim); n != "2" {
		t.Errorf("%s members after the refused file, want 2", n)
	}
	code, body = callJSON(t, srv, "parity-dev", http.MethodPost, "/api/import/dimension-members",
		map[string]any{"dimension_id": dim, "csv": "code,label\nA,Alpha\nB,Beta\n"}, "X-App-Id", app)
	if code != http.StatusOK {
		t.Errorf("developer's file updating 2 members: %d %v, want 200", code, body)
	}
}

// backfill_form_integration posts through the gateway's own posting code:
// a saved, approved record reaches the metric as an input value.
func TestAIWriteHooks_BackfillPostsThroughTheGateway(t *testing.T) {
	ctx, _, ai, q, ids := newAIParityFixture(t, "enterprise", "")
	run := func(tool string, params map[string]any) (string, string) {
		t.Helper()
		res, id, err := ai.Execute(ctx, tool, mustJSONGateway(t, params))
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		return res, id
	}
	_, metric := run("create_metric", map[string]any{"name": "spend", "is_input": true})
	_, form := run("create_form_def", map[string]any{"name": "Expenses", "fields": []map[string]any{
		{"name": "amount", "label": "Amount", "type": "number", "required": true}}})
	_, mapping := run("create_form_integration", map[string]any{"form_id": form, "name": "post spend",
		"source_field": "amount", "target_metric_id": metric})
	q(`INSERT INTO runtime.form_record (form_id, data, status, created_by) VALUES ($1::uuid, '{"amount": 42}', 'approved', $2::uuid) RETURNING id::text`, form, ids["dev"])

	res, _ := run("backfill_form_integration", map[string]any{"form_integration_id": mapping})
	if !strings.Contains(res, "Posted 1") {
		t.Errorf("result %q", res)
	}
	if v := q(`SELECT COALESCE(string_agg(value::text, ','), '') FROM runtime.fact_input WHERE metric_id=$1::uuid`, metric); v != "42" {
		t.Errorf("posted values %q, want 42", v)
	}
	// update_form_integration re-posts, as the developer's update does.
	res, _ = run("update_form_integration", map[string]any{"form_integration_id": mapping, "aggregation": "sum"})
	if !strings.Contains(res, "re-posted") {
		t.Errorf("update result %q does not report the re-post", res)
	}
}

// The developer's grid-dimension attach checked nothing about the dimension:
// another revision's (or another model's) dimension could be put on a grid.
func TestDeveloperGridDimension_MustShareTheGridsRevision(t *testing.T) {
	_, srv, _, q, ids := newAIParityFixture(t, "enterprise", "")
	grid := q(`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'G') RETURNING id::text`, ids["model"], ids["rev"])
	mine := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'region') RETURNING id::text`, ids["model"], ids["rev"])
	otherRev := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Other') RETURNING id::text`, ids["model"])
	theirs := q(`INSERT INTO model.dimension_def (model_id, revision_id, name) VALUES ($1::uuid, $2::uuid, 'region') RETURNING id::text`, ids["model"], otherRev)

	if code, body := callJSON(t, srv, "parity-dev", http.MethodPost, "/api/developer/grids/"+grid+"/dimensions/"+theirs, nil, "X-App-Id", ids["app"]); code != http.StatusBadRequest {
		t.Errorf("another revision's dimension: %d %v, want 400", code, body)
	}
	if code, body := callJSON(t, srv, "parity-dev", http.MethodPost, "/api/developer/grids/"+grid+"/dimensions/"+mine, nil, "X-App-Id", ids["app"]); code != http.StatusOK {
		t.Errorf("the grid's own revision's dimension: %d %v, want 200", code, body)
	}
}

func mustJSONGateway(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

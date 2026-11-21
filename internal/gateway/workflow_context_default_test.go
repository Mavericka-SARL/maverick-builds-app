package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/workflow"
)

// TestWorkflowContextDefaults: a context variable's default formula fills a
// start that brings no value — the start dialog shows it, a person's start
// and a scheduled run take it — read from the model (a date setting gives
// the period holding it) and from the variables before it. A value given
// wins; a schedule rule on a workflow with a required variable and no
// default is refused at save; the defaults are checked with the
// conditions, follow a metric rename and keep a metric from being deleted
// (2026-11-21).
func TestWorkflowContextDefaults(t *testing.T) {
	f := setupRoundTripFixture(t)
	ctx := context.Background()
	dev := "rollup-test-approver"
	rev := f.workingRevID
	call := func(who, method, path string, body any) (int, string) {
		t.Helper()
		return doAs(t, f.rollupFixture, method, path, who, f.appID, body)
	}
	must := func(method, path string, body any) string {
		t.Helper()
		status, raw := call(dev, method, path, body)
		if status != http.StatusOK && status != http.StatusCreated {
			t.Fatalf("%s %s: %d %s", method, path, status, raw)
		}
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(raw), &out)
		return out.ID
	}

	// A dated dimension and a date setting.
	var weeksDim string
	weeksDim = must("POST", "/api/developer/dimensions", map[string]any{"name": "weeks", "revision_id": rev, "dimension_type": "time",
		"time_granularity": "week", "fiscal_year_start_month": 1})
	for _, m := range []map[string]any{
		{"code": "W1", "label": "Week 1", "period_start": "2026-11-16", "period_end": "2026-11-22"},
		{"code": "W2", "label": "Week 2", "period_start": "2026-11-23", "period_end": "2026-11-29"}} {
		must("POST", "/api/developer/dimensions/"+weeksDim+"/members", m)
	}
	firstWeek := must("POST", "/api/developer/metrics", map[string]any{"name": "first_week", "is_input": true, "revision_id": rev})
	grid := must("POST", "/api/developer/grids", map[string]any{"name": "Settings", "revision_id": rev})
	must("POST", "/api/developer/grids/"+grid+"/metrics/"+firstWeek, nil)
	serial, _ := formula.DateSerial("2026-11-23")
	body, _ := json.Marshal(map[string]any{"model_id": f.modelID, "revision_id": rev, "metric_id": firstWeek, "value": serial})
	if status, raw := call(dev, "POST", "/api/cells", json.RawMessage(body)); status != http.StatusOK {
		t.Fatalf("write first_week: %d %s", status, raw)
	}

	store := workflow.NewStore(f.pool)
	makeDef := func(name, schema string) *workflow.WorkflowDefFull {
		t.Helper()
		def, err := store.CreateWorkflowDefFull(ctx, f.appID, rev, name, "", "manual", f.approverID)
		if err != nil {
			t.Fatal(err)
		}
		steps := json.RawMessage(`[{"id":"plan","name":"Plan the week","type":"task","assignee_roles":["business_user"],"routes":{"next":"end-completed"}}]`)
		if _, err := store.UpdateWorkflowDefFull(ctx, def.ID, name, "", "manual", "", f.approverID, steps, json.RawMessage(schema), nil); err != nil {
			t.Fatal(err)
		}
		if _, err := store.PublishWorkflowDef(ctx, def.ID, f.approverID); err != nil {
			t.Fatal(err)
		}
		full, err := store.GetWorkflowDefFull(ctx, def.ID)
		if err != nil {
			t.Fatal(err)
		}
		return full
	}
	cycle := makeDef("Weekly cycle", `[
		{"key":"week","label":"Week","data_type":"Dimension member","dimension_id":"`+weeksDim+`","required":true,"default":"=first_week"},
		{"key":"note","label":"Note","data_type":"Text","required":false,"default":"\"Weekly cycle \" & week"},
		{"key":"due","label":"Due","data_type":"Date","required":false,"default":"first_week + 4"}]`)

	// The dialog: the schema says which have a default, never the formula;
	// start-defaults gives the values.
	_, raw := call("rollup-test-manager", "GET", "/api/workflow/definitions", nil)
	if !strings.Contains(raw, `"has_default":true`) || strings.Contains(raw, "first_week") {
		t.Errorf("the definitions list must flag defaults and hide their formulas: %s", raw)
	}
	status, raw := call("rollup-test-manager", "GET", "/api/workflow/definitions/"+cycle.ID+"/start-defaults", nil)
	var dflt struct {
		Values map[string]string `json:"values"`
		Errors map[string]string `json:"errors"`
	}
	_ = json.Unmarshal([]byte(raw), &dflt)
	if status != http.StatusOK || dflt.Values["week"] != "W2" || dflt.Values["note"] != "Weekly cycle W2" || dflt.Values["due"] != "2026-11-27" || len(dflt.Errors) != 0 {
		t.Fatalf("start-defaults: %d %s, want week W2, note, due 2026-11-27", status, raw)
	}

	contextOf := func(instanceID string) map[string]string {
		t.Helper()
		var raw []byte
		if err := f.pool.QueryRow(ctx, `SELECT context FROM workflow.workflow_instance WHERE id=$1::uuid`, instanceID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		_ = json.Unmarshal(raw, &out)
		return out
	}
	start := func(ctxVars map[string]string) (int, map[string]string, string) {
		t.Helper()
		status, raw := call("rollup-test-manager", "POST", "/api/workflow/instances", map[string]any{"workflow_def_id": cycle.ID, "context": ctxVars})
		var out struct {
			InstanceID string `json:"instance_id"`
		}
		_ = json.Unmarshal([]byte(raw), &out)
		if out.InstanceID == "" {
			return status, nil, raw
		}
		got := contextOf(out.InstanceID)
		_, _ = f.pool.Exec(ctx, `UPDATE workflow.workflow_instance SET status='cancelled' WHERE id=$1::uuid`, out.InstanceID)
		return status, got, raw
	}
	if status, got, raw := start(map[string]string{}); status != http.StatusOK || got["week"] != "W2" || got["note"] != "Weekly cycle W2" || got["due"] != "2026-11-27" {
		t.Errorf("a start with nothing given takes the defaults: %d %v %s", status, got, raw)
	}
	if status, got, raw := start(map[string]string{"week": "W1"}); status != http.StatusOK || got["week"] != "W1" || got["note"] != "Weekly cycle W1" {
		t.Errorf("a value given wins, and later defaults read it: %d %v %s", status, got, raw)
	}

	// A schedule: refused for a workflow with a required variable without a
	// default; a schedule of the weekly cycle starts it with its defaults.
	scope := makeDef("Scoped review", `[{"key":"dept","label":"Department","data_type":"Dimension member","dimension_id":"`+f.deptsDimID+`","required":true}]`)
	sched := &workflow.ScheduleConfig{CronExpr: "0 6 * * 1"}
	if _, err := store.CreateAutomationRuleScoped(ctx, f.appID, rev, "Monday review", "", "schedule", scope.Name, scope.ID, "", "", "", sched); !errors.Is(err, workflow.ErrRuleScheduleInvalid) || !strings.Contains(err.Error(), "dept") {
		t.Errorf("a schedule of a workflow needing dept with no default must be refused: %v", err)
	}
	manual, err := store.CreateAutomationRuleScoped(ctx, f.appID, rev, "Review button", "", "manual", scope.Name, scope.ID, "", "", "", nil)
	if err != nil {
		t.Fatalf("a manual rule is not refused: %v", err)
	}
	if _, err := store.UpdateAutomationRuleScoped(ctx, manual.ID, "", "", "schedule", "", "", "", "", "", nil, sched); !errors.Is(err, workflow.ErrRuleScheduleInvalid) {
		t.Errorf("turning it into a schedule must be refused too: %v", err)
	}
	rule, err := store.CreateAutomationRuleScoped(ctx, f.appID, rev, "Monday cycle", "", "schedule", cycle.Name, cycle.ID, "", "", "", sched)
	if err != nil {
		t.Fatalf("a schedule of a workflow whose required variables have defaults: %v", err)
	}
	exec, err := store.TriggerScheduledRule(ctx, *rule, time.Now().Truncate(time.Minute), "test-worker")
	if err != nil {
		t.Fatalf("scheduled fire: %v", err)
	}
	if got := contextOf(exec.InstanceID); got["week"] != "W2" || got["due"] != "2026-11-27" {
		t.Errorf("a scheduled run takes the defaults: %v", got)
	}

	// A default that cannot be worked out refuses the start with the reason.
	broken := makeDef("Broken default", `[{"key":"n","label":"N","data_type":"Number","required":false,"default":"1/0"}]`)
	status, raw = call("rollup-test-manager", "POST", "/api/workflow/instances", map[string]any{"workflow_def_id": broken.ID, "context": map[string]string{}})
	if status != http.StatusBadRequest || !strings.Contains(raw, "#DIV/0!") {
		t.Errorf("a failing default: %d %s, want 400 naming the error", status, raw)
	}

	// Checked with the conditions: unknown names, a text metric, a member
	// default that names no member, a RACI variable with a default.
	if _, err := f.pool.Exec(ctx, `INSERT INTO model.metric_def (model_id, revision_id, name, is_input, format) VALUES ($1::uuid, $2::uuid, 'week_label', true, 'text')`, f.modelID, rev); err != nil {
		t.Fatal(err)
	}
	bad := *cycle
	bad.ContextSchema = json.RawMessage(`[
		{"key":"a","data_type":"Number","default":"no_such_name + 1"},
		{"key":"b","data_type":"Text","default":"week_label"},
		{"key":"c","data_type":"Dimension member","dimension_id":"` + weeksDim + `","default":"\"W9\""},
		{"key":"d","data_type":"Dimension member","dimension_id":"` + f.deptsDimID + `","source_hint":"raci_responsible","default":"\"DEPT_A\""},
		{"key":"e","data_type":"Text","default":"a & \"x\""}]`)
	errs := strings.Join(store.CheckConditionNames(ctx, &bad), "\n")
	for _, want := range []string{`"a": its default names "no_such_name"`, `"b": its default reads week_label, a text metric`, `"c": its default gives "W9"`, `"d": it is taken from the starter's RACI scope`} {
		if !strings.Contains(errs, want) {
			t.Errorf("validation must report %s; got:\n%s", want, errs)
		}
	}
	if strings.Contains(errs, `"e"`) {
		t.Errorf("a default may read a variable before it: %s", errs)
	}

	// A rename of the metric rewrites the defaults; a delete is refused.
	if status, raw := call(dev, "PATCH", "/api/developer/metrics/"+firstWeek, map[string]any{"name": "plan_start"}); status != http.StatusOK {
		t.Fatalf("rename: %d %s", status, raw)
	}
	var schema string
	_ = f.pool.QueryRow(ctx, `SELECT context_schema::text FROM workflow.workflow_def WHERE id=$1::uuid`, cycle.ID).Scan(&schema)
	if !strings.Contains(schema, `=plan_start"`) || !strings.Contains(schema, `plan_start + 4`) || strings.Contains(schema, "first_week") {
		t.Errorf("the defaults must follow the rename: %s", schema)
	}
	if status, raw := call(dev, "DELETE", "/api/developer/metrics/"+firstWeek, nil); status == http.StatusOK || !strings.Contains(raw, "the default of week") {
		t.Errorf("deleting a metric a default reads must be refused naming it: %d %s", status, raw)
	}
}

// TestScheduledFailureIsNotified: a scheduled fire that fails tells the
// application's business admins and the tenant's developers — not its
// business users — once per run of failures, and again after the schedule
// has run (2026-11-21, owner).
func TestScheduledFailureIsNotified(t *testing.T) {
	f := setupRoundTripFixture(t)
	ctx := context.Background()
	rev := f.workingRevID
	var custID string
	if err := f.pool.QueryRow(ctx, `SELECT customer_id::text FROM core.application WHERE id = $1::uuid`, f.appID).Scan(&custID); err != nil {
		t.Fatal(err)
	}
	var tenantDev string
	if err := f.pool.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id) VALUES ('test-dev', 'dev@t.com', 'Dev', $1::uuid) RETURNING id::text`, custID).Scan(&tenantDev); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, 'developer')`, tenantDev); err != nil {
		t.Fatal(err)
	}
	store := workflow.NewStore(f.pool)
	def, err := store.CreateWorkflowDefFull(ctx, f.appID, rev, "Weekly review", "", "manual", f.approverID)
	if err != nil {
		t.Fatal(err)
	}
	setSchema := func(required bool) {
		t.Helper()
		schema := fmt.Sprintf(`[{"key":"dept","label":"Dept","data_type":"Dimension member","dimension_id":"%s","required":%v}]`, f.deptsDimID, required)
		if _, err := f.pool.Exec(ctx, `UPDATE workflow.workflow_def SET context_schema = $2::jsonb WHERE id = $1::uuid`, def.ID, schema); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.UpdateWorkflowDefFull(ctx, def.ID, def.Name, "", "manual", "", f.approverID,
		json.RawMessage(`[{"id":"s","name":"Review","type":"task","assignee_roles":["business_user"],"routes":{"next":"end-completed"}}]`), nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PublishWorkflowDef(ctx, def.ID, f.approverID); err != nil {
		t.Fatal(err)
	}
	// A schedule saved before its workflow needed a value (a later edit).
	rule, err := store.CreateAutomationRuleScoped(ctx, f.appID, rev, "Monday review", "", "schedule", def.Name, def.ID, "", "", "", &workflow.ScheduleConfig{CronExpr: "0 6 * * 1"})
	if err != nil {
		t.Fatal(err)
	}
	notices := func(userID string) int {
		t.Helper()
		var n int
		_ = f.pool.QueryRow(ctx, `SELECT count(*) FROM notification.notification WHERE recipient_user_id = $1::uuid AND resource_type = 'automation_rule' AND resource_id::text = $2`, userID, rule.ID).Scan(&n)
		return n
	}
	tick := time.Now().Truncate(time.Minute)
	fire := func(wantErr bool) {
		t.Helper()
		tick = tick.Add(time.Minute)
		_, err := store.TriggerScheduledRule(ctx, *rule, tick, "test-worker")
		if (err != nil) != wantErr {
			t.Fatalf("scheduled fire: %v (want error %v)", err, wantErr)
		}
	}
	setSchema(true)
	fire(true)
	fire(true)
	for who, want := range map[string]int{"the business admin and developer": 1, "the tenant's developer": 1, "a business user": 0} {
		id := map[string]string{"the business admin and developer": f.approverID, "the tenant's developer": tenantDev, "a business user": f.managerID}[who]
		if got := notices(id); got != want {
			t.Errorf("%s: %d notices after two failed fires, want %d", who, got, want)
		}
	}
	setSchema(false)
	fire(false)
	setSchema(true)
	fire(true)
	if got := notices(tenantDev); got != 2 {
		t.Errorf("after a successful run a new failure is told again: %d notices, want 2", got)
	}
}

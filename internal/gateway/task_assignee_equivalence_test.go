package gateway

import (
	"context"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/workflow"
	"github.com/mavericks-engine/mavericks/internal/workflow/assignee/assigneetest"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// The HTTP inbox and step completion (taskAssigneeSQL with the admin scope
// of adminScopeCustomerIDs) and every other surface of workflow assignment
// (workflow.Store.IsAssigneeEligible, i.e. assignee.SQL) agree for every
// user of every role shape of assigneetest, on every step shape.
func TestTaskAssigneeSQLMatchesTheWorkflowBoundary(t *testing.T) {
	f := assigneetest.New(t)
	ctx := context.Background()
	pool := f.Pool
	h := &handler{log: logger.New("test"), db: tenantdb.NewHandle(pool, nil), devMode: true}
	store := workflow.NewStore(pool)
	type shape struct {
		app   string
		roles []string
	}
	var shapes []shape
	for _, app := range []string{f.App1, f.App2, f.AppT, f.App3} {
		for _, r := range [][]string{nil, {"business_admin"}, {"business_user"}, {"developer"}, {"tenant_admin"}, {"platform_admin"}, {"Approvers"}, {"Approvers", "developer"}} {
			shapes = append(shapes, shape{app, r})
		}
	}
	mismatch := 0
	for _, s := range shapes {
		step := f.Step(t, s.app, s.roles...)
		for name, uid := range f.Users {
			if name == "ba_1a_off" {
				continue // disabled: the gateway never resolves it
			}
			a, err := h.actorByKeycloakSub(ctx, "asg-"+name)
			if err != nil {
				t.Fatal(err)
			}
			if a.UserID != uid {
				t.Fatalf("%s resolved to %s, want %s", name, a.UserID, uid)
			}
			var gw bool
			if err := pool.QueryRow(ctx, `
				SELECT EXISTS (
				    SELECT 1
				    FROM workflow.workflow_step ws
				    JOIN workflow.workflow_instance wi ON wi.id = ws.instance_id
				    JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
				    JOIN core.application app ON app.id = wd.application_id
				    LEFT JOIN core.workspace appws ON appws.id = app.workspace_id
				    CROSS JOIN LATERAL (
				        SELECT elem FROM jsonb_array_elements(COALESCE(wi.steps_snapshot, wd.steps)) AS elem
				        WHERE elem->>'id' = ws.step_def_id LIMIT 1
				    ) step_def
				    WHERE ws.id = $2::uuid AND `+taskAssigneeSQL+`
				)`, a.UserID, step).Scan(&gw); err != nil {
				t.Fatal(err)
			}
			el, err := store.IsAssigneeEligible(ctx, step, uid)
			if err != nil {
				t.Fatal(err)
			}
			if gw != el {
				mismatch++
				t.Errorf("app=%s roles=%v user=%s gateway=%v IsAssigneeEligible=%v", s.app, s.roles, name, gw, el)
			}
		}
	}
	t.Logf("checked %d shapes x %d users, %d mismatches", len(shapes), len(f.Users), mismatch)
}

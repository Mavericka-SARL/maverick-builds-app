package workflow_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/workflow"
	"github.com/mavericks-engine/mavericks/internal/workflow/assignee/assigneetest"
)

// The workspace boundary of workflow assignment (package assignee) holds for
// deciding a step (Store.IsAssigneeEligible, the reference the gateway's
// inbox is held equal to) and for a notification step's role recipients. A
// named platform role used to match in any workspace of any tenant, and a
// notification step naming a role notified its holders in every workspace of
// the tenant and every unscoped holder anywhere.

func TestIsAssigneeEligibleKeepsTheWorkspaceBoundary(t *testing.T) {
	f := assigneetest.New(t)
	store := workflow.NewStore(f.Pool)
	ctx := context.Background()
	for _, c := range assigneetest.Cases {
		t.Run(c.Name, func(t *testing.T) {
			step := f.Step(t, c.App(f), c.Roles...)
			var eligible []string
			for _, id := range f.Users {
				ok, err := store.IsAssigneeEligible(ctx, step, id)
				if err != nil {
					t.Fatalf("IsAssigneeEligible: %v", err)
				}
				if ok {
					eligible = append(eligible, id)
				}
			}
			if got := f.Names(eligible); !slices.Equal(got, c.Want) {
				t.Errorf("eligible = %v, want %v", got, c.Want)
			}
		})
	}
}

func TestNotificationStepRoleRecipientsKeepTheWorkspaceBoundary(t *testing.T) {
	f := assigneetest.New(t)
	store := workflow.NewStore(f.Pool)
	ctx := context.Background()
	starter := f.Users["norole_1"]
	for _, c := range assigneetest.Cases {
		if len(c.Roles) != 1 {
			continue // a notification step names one recipient role
		}
		t.Run(c.Name, func(t *testing.T) {
			def, err := store.CreateWorkflowDefFull(ctx, c.App(f), "", "Notify "+c.Name, "", "manual", starter)
			if err != nil {
				t.Fatalf("create def: %v", err)
			}
			steps := json.RawMessage(fmt.Sprintf(`[
				{"id":"notify","name":"Tell the approvers","type":"notification",
				 "notification":{"recipient_type":"role","recipient_role":%q,"subject":"Budget raised","message":"Please look"}}
			]`, c.Roles[0]))
			if _, err := store.UpdateWorkflowDefFull(ctx, def.ID, def.Name, def.Description, def.TriggerEvent, "", starter, steps, nil, nil); err != nil {
				t.Fatalf("set steps: %v", err)
			}
			if _, err := store.PublishWorkflowDef(ctx, def.ID, starter); err != nil {
				t.Fatalf("publish: %v", err)
			}
			inst, err := store.StartWorkflow(ctx, def.ID, starter, map[string]string{})
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			rows, err := f.Pool.Query(ctx, `
				SELECT DISTINCT recipient_user_id::text FROM notification.notification
				WHERE template_id = 'workflow_step_notification' AND resource_id = $1`, inst.Id)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					t.Fatal(err)
				}
				got = append(got, id)
			}
			rows.Close()
			if names := f.Names(got); !slices.Equal(names, c.Want) {
				t.Errorf("notified %v, want %v", names, c.Want)
			}
		})
	}
}

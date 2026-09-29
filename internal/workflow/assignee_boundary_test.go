package workflow_test

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonv1 "github.com/mavericks-engine/mavericks/gen/go/common/v1"
	workflowv1 "github.com/mavericks-engine/mavericks/gen/go/workflow/v1"
	"github.com/mavericks-engine/mavericks/internal/workflow"
	"github.com/mavericks-engine/mavericks/internal/workflow/assignee/assigneetest"
	"github.com/mavericks-engine/mavericks/pkg/auth"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// The workspace boundary of workflow assignment (package assignee) holds
// for deciding a step outside the gateway, and for a notification step's
// role recipients. Store.IsAssigneeEligible — all the gRPC
// WorkflowService.CompleteStep checks — matched a named platform role held
// in any workspace of any tenant (or none), and a named business role of
// any workspace of the application's tenant; a notification step naming a
// role notified its holders in every workspace of the tenant and every
// unscoped holder anywhere, with the workflow's name and message.

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

func TestGRPCCompleteStepKeepsTheWorkspaceBoundary(t *testing.T) {
	f := assigneetest.New(t)
	store := workflow.NewStore(f.Pool)
	srv := workflow.NewServer(logger.New("test"), store)
	step := f.Step(t, f.App1, "business_admin")

	complete := func(user string) error {
		ctx := auth.WithActor(context.Background(), &commonv1.Actor{UserId: f.Users[user]})
		_, err := srv.CompleteStep(ctx, &workflowv1.CompleteStepRequest{StepId: step, Decision: "approve"})
		return err
	}
	// A business admin of another tenant, of another workspace of the same
	// tenant, one whose role is held in no workspace, a disabled one of
	// this workspace, and a member of this workspace's business role named
	// business_admin (not the platform role).
	for _, user := range []string{"ba_2", "ba_1b", "ba_unscoped", "ba_1a_off", "imp_1a"} {
		if code := status.Code(complete(user)); code != codes.PermissionDenied {
			t.Errorf("%s deciding ws1a's step: code %v, want PermissionDenied", user, code)
		}
	}
	var st string
	if err := f.Pool.QueryRow(context.Background(), `SELECT status::text FROM workflow.workflow_step WHERE id=$1::uuid`, step).Scan(&st); err != nil || st != "in_progress" {
		t.Fatalf("step status after refused decisions = %q (%v), want in_progress", st, err)
	}
	if err := complete("ba_1a"); err != nil {
		t.Fatalf("the business admin of ws1a could not decide its step: %v", err)
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

// Tests for gRPC Server.CompleteStep/GetPendingTasks authorization: both
// now enforce Store.IsAssigneeEligible (the same assignee_roles check the
// HTTP task-inbox path uses) and resolve the acting user exclusively from
// ctx (auth.ActorFromContext), never from the client-supplied req.Actor —
// a synchronization audit found the previous RACI/Policy-service-based
// check was structurally broken (RACI rules are keyed by dimension-scope
// glob patterns, never step IDs, so the lookup could never match) and that
// req.Actor was trusted for authorization despite being fully
// client-controlled.
package workflow_test

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonv1 "github.com/mavericks-engine/mavericks/gen/go/common/v1"
	workflowv1 "github.com/mavericks-engine/mavericks/gen/go/workflow/v1"
	"github.com/mavericks-engine/mavericks/internal/workflow"
	"github.com/mavericks-engine/mavericks/pkg/auth"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// startApprovalInstance creates a single-step approval workflow gated to
// assigneeRoles (nil/empty means anyone), starts it, and returns the
// pending step's ID.
func startApprovalInstance(t *testing.T, store *workflow.Store, appID, starterUserID string, assigneeRoles []string) string {
	t.Helper()
	ctx := context.Background()

	def, err := store.CreateWorkflowDef(ctx, appID, "Approval Flow", "manual", []*workflowv1.WorkflowStepDef{
		{Id: "approve", Name: "Manager Approval", Type: workflowv1.StepType_STEP_TYPE_APPROVAL, AssigneeRoles: assigneeRoles},
	})
	if err != nil {
		t.Fatalf("create workflow def: %v", err)
	}
	inst, err := store.StartWorkflow(ctx, def.Id, starterUserID, nil)
	if err != nil {
		t.Fatalf("start workflow: %v", err)
	}
	_, steps, err := store.GetWorkflowInstance(ctx, inst.Id)
	if err != nil || len(steps) == 0 {
		t.Fatalf("get instance steps: %v (steps=%d)", err, len(steps))
	}
	return steps[0].Id
}

func insertRoleAssignment(t *testing.T, store *workflow.Store, userID, role string) {
	t.Helper()
	if _, err := store.Pool().Exec(context.Background(), `
		INSERT INTO identity.role_assignment (user_id, role) VALUES ($1::uuid, $2::identity.user_role)
	`, userID, role); err != nil {
		t.Fatalf("insert role assignment: %v", err)
	}
}

func TestServerCompleteStepAllowsEligibleAssignee(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	userID, appID := insertFixtures(t, store)
	insertRoleAssignment(t, store, userID, "developer")
	stepID := startApprovalInstance(t, store, appID, userID, []string{"developer"})

	srv := workflow.NewServer(logger.New("test"), store)
	ctx := auth.WithActor(context.Background(), &commonv1.Actor{UserId: userID})

	resp, err := srv.CompleteStep(ctx, &workflowv1.CompleteStepRequest{
		StepId: stepID, Decision: "approve",
	})
	if err != nil {
		t.Fatalf("expected an eligible assignee (role_assignment matching assignee_roles) to complete the step, got: %v", err)
	}
	if resp.Step.Status != workflowv1.StepStatus_STEP_STATUS_COMPLETED {
		t.Errorf("step status = %v, want COMPLETED", resp.Step.Status)
	}
}

func TestServerCompleteStepDeniesIneligibleActor(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	userID, appID := insertFixtures(t, store)
	// No role_assignment/business_role_member row for userID at all.
	stepID := startApprovalInstance(t, store, appID, userID, []string{"developer"})

	srv := workflow.NewServer(logger.New("test"), store)
	ctx := auth.WithActor(context.Background(), &commonv1.Actor{UserId: userID})

	_, err := srv.CompleteStep(ctx, &workflowv1.CompleteStepRequest{
		StepId: stepID, Decision: "approve",
	})
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Errorf("code = %v, want PermissionDenied", got)
	}

	_, steps, ferr := store.GetWorkflowInstance(context.Background(), stepInstanceID(t, store, stepID))
	if ferr != nil {
		t.Fatalf("re-fetch instance: %v", ferr)
	}
	if steps[0].Status == workflowv1.StepStatus_STEP_STATUS_COMPLETED {
		t.Error("step was completed despite the actor being ineligible")
	}
}

func TestServerCompleteStepRequiresAuthenticatedActor(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	userID, appID := insertFixtures(t, store)
	stepID := startApprovalInstance(t, store, appID, userID, nil)

	srv := workflow.NewServer(logger.New("test"), store)

	// No auth.WithActor injected — simulates a call that never passed
	// through AuthInterceptor. req.Actor is populated but must be ignored.
	_, err := srv.CompleteStep(context.Background(), &workflowv1.CompleteStepRequest{
		StepId: stepID, Decision: "approve", Actor: &commonv1.Actor{UserId: userID},
	})
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Errorf("code = %v, want Unauthenticated", got)
	}
}

// TestServerCompleteStepIgnoresSpoofedReqActor proves the actual fix: a
// caller cannot claim a different, more-privileged identity via the
// client-supplied req.Actor field. The real (ctx-resolved) actor is
// eligible; req.Actor names an ineligible one. If req.Actor were still
// trusted (the pre-fix behavior), this would fail with PermissionDenied;
// with the fix, it must succeed using the real ctx actor, and the
// completed step must record the REAL actor's ID, not the spoofed one.
func TestServerCompleteStepIgnoresSpoofedReqActor(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	realUserID, appID := insertFixtures(t, store)
	insertRoleAssignment(t, store, realUserID, "developer")
	stepID := startApprovalInstance(t, store, appID, realUserID, []string{"developer"})

	var spoofedUserID string
	if err := store.Pool().QueryRow(context.Background(), `
		INSERT INTO identity.user (email) VALUES ('spoofed@example.com') RETURNING id::text
	`).Scan(&spoofedUserID); err != nil {
		t.Fatalf("insert spoofed user: %v", err)
	}

	srv := workflow.NewServer(logger.New("test"), store)
	ctx := auth.WithActor(context.Background(), &commonv1.Actor{UserId: realUserID})

	_, err := srv.CompleteStep(ctx, &workflowv1.CompleteStepRequest{
		StepId: stepID, Decision: "approve",
		Actor: &commonv1.Actor{UserId: spoofedUserID, Role: commonv1.Role_ROLE_PLATFORM_ADMIN},
	})
	if err != nil {
		t.Fatalf("expected the real ctx-resolved actor's eligibility to be used, got: %v", err)
	}

	// CompleteStep's own response doesn't echo assignee_user_id back, so
	// re-fetch the step to confirm who it was actually recorded as
	// completed by.
	_, steps, ferr := store.GetWorkflowInstance(context.Background(), stepInstanceID(t, store, stepID))
	if ferr != nil {
		t.Fatalf("re-fetch instance: %v", ferr)
	}
	if steps[0].AssigneeUserId != realUserID {
		t.Errorf("completed step recorded assignee_user_id = %q, want the real ctx actor %q (spoofed req.Actor was %q)",
			steps[0].AssigneeUserId, realUserID, spoofedUserID)
	}
}

func TestServerGetPendingTasksRequiresAuthenticatedActor(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	srv := workflow.NewServer(logger.New("test"), store)

	_, err := srv.GetPendingTasks(context.Background(), &workflowv1.GetPendingTasksRequest{
		UserId: "00000000-0000-0000-0000-000000000099", // must be ignored, not just unauthenticated by omission
	})
	if got := status.Code(err); got != codes.Unauthenticated {
		t.Errorf("code = %v, want Unauthenticated", got)
	}
}

func stepInstanceID(t *testing.T, store *workflow.Store, stepID string) string {
	t.Helper()
	var instanceID string
	if err := store.Pool().QueryRow(context.Background(),
		`SELECT instance_id::text FROM workflow.workflow_step WHERE id=$1::uuid`, stepID,
	).Scan(&instanceID); err != nil {
		t.Fatalf("resolve step's instance id: %v", err)
	}
	return instanceID
}

// ── StartWorkflow ────────────────────────────────────────────────────────────
//
// StartWorkflow had the same two defects CompleteStep was fixed for, but was
// missed at the time: identity came from actorUserID, which PREFERRED the
// client-supplied req.Actor over the ctx identity, and the store's
// StartWorkflow was called directly — skipping ResolveStartContext, the one
// place that enforces "the definition is published" and "the caller may see
// the scope this instance would run under".

func TestServerStartWorkflowRequiresAuthenticatedActor(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	userID, appID := insertFixtures(t, store)

	def, err := store.CreateWorkflowDef(context.Background(), appID, "Flow", "manual", []*workflowv1.WorkflowStepDef{
		{Id: "approve", Name: "Approval", Type: workflowv1.StepType_STEP_TYPE_APPROVAL},
	})
	if err != nil {
		t.Fatalf("create def: %v", err)
	}
	if _, err := store.PublishWorkflowDef(context.Background(), def.Id, userID); err != nil {
		t.Fatalf("publish def: %v", err)
	}

	srv := workflow.NewServer(logger.New("test"), store)
	_, err = srv.StartWorkflow(context.Background(), &workflowv1.StartWorkflowRequest{
		WorkflowDefId: def.Id,
		// An unauthenticated caller supplying an actor must not be able to
		// start anything: identity comes from ctx alone.
		Actor: &commonv1.Actor{UserId: userID, Role: commonv1.Role_ROLE_PLATFORM_ADMIN},
	})
	if status.Code(err) != codes.Unauthenticated {
		t.Errorf("StartWorkflow with no ctx actor: code = %v, want Unauthenticated (err: %v)", status.Code(err), err)
	}
}

func TestServerStartWorkflowIgnoresSpoofedReqActor(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	realUserID, appID := insertFixtures(t, store)

	var spoofedUserID string
	if err := store.Pool().QueryRow(context.Background(), `
		INSERT INTO identity.user (email) VALUES ('start-spoof@example.com') RETURNING id::text
	`).Scan(&spoofedUserID); err != nil {
		t.Fatalf("insert spoofed user: %v", err)
	}

	def, err := store.CreateWorkflowDef(context.Background(), appID, "Flow", "manual", []*workflowv1.WorkflowStepDef{
		{Id: "approve", Name: "Approval", Type: workflowv1.StepType_STEP_TYPE_APPROVAL},
	})
	if err != nil {
		t.Fatalf("create def: %v", err)
	}
	if _, err := store.PublishWorkflowDef(context.Background(), def.Id, realUserID); err != nil {
		t.Fatalf("publish def: %v", err)
	}

	srv := workflow.NewServer(logger.New("test"), store)
	ctx := auth.WithActor(context.Background(), &commonv1.Actor{UserId: realUserID})
	resp, err := srv.StartWorkflow(ctx, &workflowv1.StartWorkflowRequest{
		WorkflowDefId: def.Id,
		Actor:         &commonv1.Actor{UserId: spoofedUserID, Role: commonv1.Role_ROLE_PLATFORM_ADMIN},
	})
	if err != nil {
		t.Fatalf("StartWorkflow: %v", err)
	}
	var startedBy string
	if err := store.Pool().QueryRow(context.Background(),
		`SELECT started_by::text FROM workflow.workflow_instance WHERE id=$1::uuid`, resp.Instance.Id).Scan(&startedBy); err != nil {
		t.Fatalf("read started_by: %v", err)
	}
	if startedBy != realUserID {
		t.Errorf("instance started_by = %q, want the ctx actor %q (spoofed req.Actor was %q)", startedBy, realUserID, spoofedUserID)
	}
}

func TestServerStartWorkflowRejectsUnpublishedDefinition(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	userID, appID := insertFixtures(t, store)

	// Created but never published — the store's StartWorkflow doesn't care,
	// which is exactly why the gRPC path must go through ResolveStartContext.
	def, err := store.CreateWorkflowDef(context.Background(), appID, "Draft Flow", "manual", []*workflowv1.WorkflowStepDef{
		{Id: "approve", Name: "Approval", Type: workflowv1.StepType_STEP_TYPE_APPROVAL},
	})
	if err != nil {
		t.Fatalf("create def: %v", err)
	}

	srv := workflow.NewServer(logger.New("test"), store)
	ctx := auth.WithActor(context.Background(), &commonv1.Actor{UserId: userID})
	_, err = srv.StartWorkflow(ctx, &workflowv1.StartWorkflowRequest{WorkflowDefId: def.Id})
	if status.Code(err) != codes.FailedPrecondition {
		t.Errorf("StartWorkflow on a draft definition: code = %v, want FailedPrecondition (err: %v)", status.Code(err), err)
	}
	var instances int
	if err := store.Pool().QueryRow(context.Background(),
		`SELECT COUNT(*) FROM workflow.workflow_instance WHERE workflow_def_id=$1::uuid`, def.Id).Scan(&instances); err != nil {
		t.Fatalf("count instances: %v", err)
	}
	if instances != 0 {
		t.Errorf("a draft workflow produced %d instance(s); want none", instances)
	}
}

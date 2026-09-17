package workflow

import (
	"context"
	"errors"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	workflowv1 "github.com/mavericks-engine/mavericks/gen/go/workflow/v1"
	"github.com/mavericks-engine/mavericks/pkg/auth"
)

type Server struct {
	workflowv1.UnimplementedWorkflowServiceServer
	log   zerolog.Logger
	store *Store
}

func NewServer(log zerolog.Logger, store *Store) *Server {
	return &Server{log: log, store: store}
}

func (s *Server) CreateWorkflowDef(ctx context.Context, req *workflowv1.CreateWorkflowDefRequest) (*workflowv1.CreateWorkflowDefResponse, error) {
	if req.ApplicationId == "" {
		return nil, status.Error(codes.InvalidArgument, "application_id is required")
	}
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	def, err := s.store.CreateWorkflowDef(ctx, req.ApplicationId, req.Name, req.TriggerEvent, req.Steps)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &workflowv1.CreateWorkflowDefResponse{WorkflowDef: def}, nil
}

func (s *Server) GetWorkflowDef(ctx context.Context, req *workflowv1.GetWorkflowDefRequest) (*workflowv1.GetWorkflowDefResponse, error) {
	if req.WorkflowDefId == "" {
		return nil, status.Error(codes.InvalidArgument, "workflow_def_id is required")
	}
	def, err := s.store.GetWorkflowDef(ctx, req.WorkflowDefId)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return &workflowv1.GetWorkflowDefResponse{WorkflowDef: def}, nil
}

// StartWorkflow resolves the caller from ctx and routes through
// Store.ResolveStartContext, matching the HTTP start path. It previously did
// neither: identity came from actorUserID, which PREFERRED the client-supplied
// req.Actor over the server-verified ctx identity — so any caller could start
// a workflow as anyone — and the store's StartWorkflow was called directly,
// skipping ResolveStartContext entirely. That skip meant an unpublished
// (draft) definition could be started over gRPC, and the context a caller
// supplied was never validated against their own dimension-member access, so
// an instance could be started scoped to data the starter cannot see. Both
// checks already existed; this path just wasn't using them.
func (s *Server) StartWorkflow(ctx context.Context, req *workflowv1.StartWorkflowRequest) (*workflowv1.StartWorkflowResponse, error) {
	if req.WorkflowDefId == "" {
		return nil, status.Error(codes.InvalidArgument, "workflow_def_id is required")
	}

	actor, err := auth.ActorFromContext(ctx)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "no authenticated actor")
	}

	resolved, err := s.store.ResolveStartContext(ctx, req.WorkflowDefId, actor.UserId, req.Context)
	if err != nil {
		switch {
		case errors.Is(err, ErrWorkflowNotPublished):
			return nil, status.Error(codes.FailedPrecondition, err.Error())
		case errors.Is(err, ErrHiddenScope), errors.Is(err, ErrNoRACIScope):
			return nil, status.Error(codes.PermissionDenied, err.Error())
		case errors.Is(err, ErrDuplicateInstance):
			return nil, status.Error(codes.AlreadyExists, err.Error())
		default:
			return nil, status.Errorf(codes.InvalidArgument, "resolve start context: %v", err)
		}
	}

	inst, err := s.store.StartWorkflow(ctx, req.WorkflowDefId, actor.UserId, resolved)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &workflowv1.StartWorkflowResponse{Instance: inst}, nil
}

func (s *Server) GetWorkflowInstance(ctx context.Context, req *workflowv1.GetWorkflowInstanceRequest) (*workflowv1.GetWorkflowInstanceResponse, error) {
	if req.InstanceId == "" {
		return nil, status.Error(codes.InvalidArgument, "instance_id is required")
	}
	inst, steps, err := s.store.GetWorkflowInstance(ctx, req.InstanceId)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return &workflowv1.GetWorkflowInstanceResponse{Instance: inst, Steps: steps}, nil
}

func (s *Server) ListWorkflowInstances(ctx context.Context, req *workflowv1.ListWorkflowInstancesRequest) (*workflowv1.ListWorkflowInstancesResponse, error) {
	if req.ApplicationId == "" {
		return nil, status.Error(codes.InvalidArgument, "application_id is required")
	}
	limit, offset := pageParams(req.Page)
	instances, err := s.store.ListWorkflowInstances(ctx, req.ApplicationId, req.Status, limit, offset)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &workflowv1.ListWorkflowInstancesResponse{Instances: instances}, nil
}

// CompleteStep authorizes via Store.IsAssigneeEligible — the same
// assignee_roles check the HTTP task-inbox completion path
// (internal/gateway/handler.go's taskAction) uses. This used to authorize
// via the Policy service's CheckRACIPermission instead, which turned out to
// be structurally broken for this purpose (found by a synchronization
// audit): CheckRACIPermission unconditionally allows any actor whose role
// isn't business_user, and for business_user it looks up a RACI rule keyed
// by an exact match against WorkflowStepId — but every real RACI rule is
// seeded as a dimension-scope glob pattern, never a step ID, so that lookup
// could never match anything. Net effect: business_user actors were always
// denied regardless of real assignee_roles membership, and every other role
// was always allowed regardless of it — the opposite of the real business
// rule. The actor identity itself is also no longer taken from the
// client-supplied req.Actor (spoofable — any caller could set an arbitrary
// UserId/Role): AuthInterceptor (see cmd/workflow/main.go) already resolves
// and injects a server-verified actor into ctx for every real call, so that
// is now the sole source of identity here.
func (s *Server) CompleteStep(ctx context.Context, req *workflowv1.CompleteStepRequest) (*workflowv1.CompleteStepResponse, error) {
	if req.StepId == "" {
		return nil, status.Error(codes.InvalidArgument, "step_id is required")
	}

	actor, err := auth.ActorFromContext(ctx)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "no authenticated actor")
	}

	eligible, err := s.store.IsAssigneeEligible(ctx, req.StepId, actor.UserId)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "check assignee eligibility: %v", err)
	}
	if !eligible {
		return nil, status.Error(codes.PermissionDenied, "actor is not an eligible assignee for this step")
	}

	step, err := s.store.CompleteStep(ctx, req.StepId, actor.UserId, req.Decision, req.Comment)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &workflowv1.CompleteStepResponse{Step: step}, nil
}

// GetPendingTasks resolves the caller from ctx (see CompleteStep's doc
// comment above) rather than trusting req.UserId — the previous behavior
// let any caller read any other user's pending tasks by simply setting an
// arbitrary UserId, with no cross-check at all. Note: Store.GetPendingTasks
// itself only matches on assignee_user_id-or-unassigned, without the fuller
// assignee_roles eligibility check IsAssigneeEligible/taskAction use — a
// task can appear in this list without the viewer necessarily being
// eligible to complete it. That's a separate, smaller, pre-existing gap
// left out of scope here: CompleteStep above is what actually gates
// completion, so this list being slightly over-inclusive isn't a
// write-authorization bug.
func (s *Server) GetPendingTasks(ctx context.Context, req *workflowv1.GetPendingTasksRequest) (*workflowv1.GetPendingTasksResponse, error) {
	actor, err := auth.ActorFromContext(ctx)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "no authenticated actor")
	}
	tasks, err := s.store.GetPendingTasks(ctx, actor.UserId, req.ApplicationId)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &workflowv1.GetPendingTasksResponse{Tasks: tasks}, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func pageParams(p interface{ GetPageSize() int32 }) (limit, offset int32) {
	if p == nil {
		return 50, 0
	}
	limit = p.GetPageSize()
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	return limit, 0
}

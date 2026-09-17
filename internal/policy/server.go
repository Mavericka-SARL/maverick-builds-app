package policy

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonv1 "github.com/mavericks-engine/mavericks/gen/go/common/v1"
	policyv1 "github.com/mavericks-engine/mavericks/gen/go/policy/v1"
)

// RBAC permission matrix: role → allowed actions on resource types.
// Platform admin and developer have blanket access; business roles are restricted.
var rbacMatrix = map[commonv1.Role]map[string][]policyv1.Action{
	commonv1.Role_ROLE_PLATFORM_ADMIN: {
		"*": {policyv1.Action_ACTION_READ, policyv1.Action_ACTION_WRITE, policyv1.Action_ACTION_APPROVE, policyv1.Action_ACTION_ADMIN},
	},
	commonv1.Role_ROLE_DEVELOPER: {
		"application": {policyv1.Action_ACTION_READ, policyv1.Action_ACTION_WRITE},
		"model":       {policyv1.Action_ACTION_READ, policyv1.Action_ACTION_WRITE},
		"dimension":   {policyv1.Action_ACTION_READ, policyv1.Action_ACTION_WRITE},
		"metric":      {policyv1.Action_ACTION_READ, policyv1.Action_ACTION_WRITE},
		"policy":      {policyv1.Action_ACTION_READ, policyv1.Action_ACTION_WRITE},
		"workflow":    {policyv1.Action_ACTION_READ, policyv1.Action_ACTION_WRITE},
	},
	commonv1.Role_ROLE_BUSINESS_ADMIN: {
		"user":   {policyv1.Action_ACTION_READ, policyv1.Action_ACTION_WRITE},
		"policy": {policyv1.Action_ACTION_READ, policyv1.Action_ACTION_WRITE},
		"raci":   {policyv1.Action_ACTION_READ, policyv1.Action_ACTION_WRITE},
	},
	commonv1.Role_ROLE_BUSINESS_USER: {
		"application": {policyv1.Action_ACTION_READ},
		"data_cell":   {policyv1.Action_ACTION_READ}, // write gated by metric/cell policy
		"workflow":    {policyv1.Action_ACTION_READ},
	},
}

type Server struct {
	policyv1.UnimplementedPolicyServiceServer
	log   zerolog.Logger
	store *Store
	cache *Cache
}

func NewServer(log zerolog.Logger, store *Store, cache *Cache) *Server {
	return &Server{log: log, store: store, cache: cache}
}

func (s *Server) CheckPermission(ctx context.Context, req *policyv1.CheckPermissionRequest) (*policyv1.CheckPermissionResponse, error) {
	if req.Actor == nil {
		return nil, status.Error(codes.InvalidArgument, "actor is required")
	}

	actionStr := req.Action.String()

	// Cache hit
	if allowed, found := s.cache.GetPermission(ctx, req.Actor.UserId, req.ResourceType, req.ResourceId, actionStr); found {
		return &policyv1.CheckPermissionResponse{Allowed: allowed, Reason: "cached"}, nil
	}

	allowed, reason := s.evaluate(ctx, req.Actor, "", req.ResourceType, req.ResourceId, req.Action)

	s.cache.SetPermission(ctx, req.Actor.UserId, req.ResourceType, req.ResourceId, actionStr, allowed, reason)
	return &policyv1.CheckPermissionResponse{Allowed: allowed, Reason: reason}, nil
}

func (s *Server) CheckRACIPermission(ctx context.Context, req *policyv1.CheckRACIPermissionRequest) (*policyv1.CheckRACIPermissionResponse, error) {
	if req.Actor == nil {
		return nil, status.Error(codes.InvalidArgument, "actor is required")
	}

	// Only business_user is subject to RACI
	if req.Actor.Role != commonv1.Role_ROLE_BUSINESS_USER {
		return &policyv1.CheckRACIPermissionResponse{
			Allowed:  true,
			RaciType: policyv1.RACIType_RACI_TYPE_ACCOUNTABLE,
			Reason:   "non-business-user roles bypass RACI",
		}, nil
	}

	raciType, err := s.store.GetRACIType(ctx, req.Actor.UserId, req.ApplicationId, req.WorkflowStepId)
	if err != nil {
		s.log.Error().Err(err).Msg("get raci type")
		return nil, status.Error(codes.Internal, err.Error())
	}

	allowed, reason := raciAllows(raciType, req.Action)
	return &policyv1.CheckRACIPermissionResponse{
		Allowed:  allowed,
		RaciType: raciType,
		Reason:   reason,
	}, nil
}

func (s *Server) GetEffectivePermissions(ctx context.Context, req *policyv1.GetEffectivePermissionsRequest) (*policyv1.GetEffectivePermissionsResponse, error) {
	if req.Actor == nil {
		return nil, status.Error(codes.InvalidArgument, "actor is required")
	}

	metrics, err := s.store.GetAllowedMetrics(ctx, req.Actor.UserId, req.ApplicationId)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	members, err := s.store.GetAllowedDimensionMembers(ctx, req.Actor.UserId, req.ApplicationId)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	actions := actionsForRole(req.Actor.Role)

	return &policyv1.GetEffectivePermissionsResponse{
		AllowedMetrics:          metrics,
		AllowedDimensionMembers: members,
		AllowedActions:          actions,
	}, nil
}

func (s *Server) InvalidatePolicyCache(ctx context.Context, req *policyv1.InvalidatePolicyCacheRequest) (*policyv1.InvalidatePolicyCacheResponse, error) {
	if err := s.cache.InvalidateApplication(ctx, req.ApplicationId); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &policyv1.InvalidatePolicyCacheResponse{Success: true}, nil
}

func (s *Server) UpsertRACIRule(ctx context.Context, req *policyv1.UpsertRACIRuleRequest) (*policyv1.UpsertRACIRuleResponse, error) {
	rule, err := s.store.UpsertRACIRule(ctx, req.Rule)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	_ = s.cache.InvalidateApplication(ctx, req.Rule.ApplicationId)
	return &policyv1.UpsertRACIRuleResponse{Rule: rule}, nil
}

func (s *Server) UpsertDimensionPolicy(ctx context.Context, req *policyv1.UpsertDimensionPolicyRequest) (*policyv1.UpsertDimensionPolicyResponse, error) {
	p, err := s.store.UpsertDimensionPolicy(ctx, req.Policy)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	_ = s.cache.InvalidateApplication(ctx, req.Policy.ApplicationId)
	return &policyv1.UpsertDimensionPolicyResponse{Policy: p}, nil
}

func (s *Server) UpsertMetricPolicy(ctx context.Context, req *policyv1.UpsertMetricPolicyRequest) (*policyv1.UpsertMetricPolicyResponse, error) {
	p, err := s.store.UpsertMetricPolicy(ctx, req.Policy)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	_ = s.cache.InvalidateApplication(ctx, req.Policy.ApplicationId)
	return &policyv1.UpsertMetricPolicyResponse{Policy: p}, nil
}

func (s *Server) UpsertAttributeRule(ctx context.Context, req *policyv1.UpsertAttributeRuleRequest) (*policyv1.UpsertAttributeRuleResponse, error) {
	if req.Rule == nil {
		return nil, status.Error(codes.InvalidArgument, "rule is required")
	}
	r, err := s.store.UpsertAttributeRule(ctx, req.Rule)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	_ = s.cache.InvalidateApplication(ctx, req.Rule.ApplicationId)
	return &policyv1.UpsertAttributeRuleResponse{Rule: r}, nil
}

func (s *Server) UpsertCellPolicy(ctx context.Context, req *policyv1.UpsertCellPolicyRequest) (*policyv1.UpsertCellPolicyResponse, error) {
	if req.Policy == nil {
		return nil, status.Error(codes.InvalidArgument, "policy is required")
	}
	p, err := s.store.UpsertCellPolicy(ctx, req.Policy)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	_ = s.cache.InvalidateApplication(ctx, req.Policy.ApplicationId)
	return &policyv1.UpsertCellPolicyResponse{Policy: p}, nil
}

func (s *Server) CheckCellPermission(ctx context.Context, req *policyv1.CheckCellPermissionRequest) (*policyv1.CheckCellPermissionResponse, error) {
	if req.Actor == nil {
		return nil, status.Error(codes.InvalidArgument, "actor is required")
	}

	cell, err := s.store.CheckCellPolicy(ctx, req.Actor.UserId, req.ApplicationId, req.MetricId, req.DimMembers)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	// No cell-level policy — fall back to metric-level RBAC
	if cell == nil {
		allowed, reason := s.evaluate(ctx, req.Actor, req.ApplicationId, "data_cell", req.MetricId, req.Action)
		return &policyv1.CheckCellPermissionResponse{Allowed: allowed, Reason: reason}, nil
	}

	switch req.Action {
	case policyv1.Action_ACTION_READ:
		return &policyv1.CheckCellPermissionResponse{Allowed: cell.CanRead, Reason: "cell policy"}, nil
	case policyv1.Action_ACTION_WRITE:
		return &policyv1.CheckCellPermissionResponse{Allowed: cell.CanWrite, Reason: "cell policy"}, nil
	default:
		return &policyv1.CheckCellPermissionResponse{Allowed: false, Reason: "cell policy: action not supported"}, nil
	}
}

// ── evaluation helpers ────────────────────────────────────────────────────────

func (s *Server) evaluate(ctx context.Context, actor *commonv1.Actor, applicationID, resourceType, _ string, action policyv1.Action) (bool, string) {
	perms, ok := rbacMatrix[actor.Role]
	if !ok {
		return s.evaluateABAC(ctx, actor, applicationID, resourceType, action)
	}

	// Wildcard check (platform_admin)
	if wildcardActions, hasWild := perms["*"]; hasWild {
		if actionIn(action, wildcardActions) {
			return true, fmt.Sprintf("role %s has wildcard permission", actor.Role)
		}
	}

	allowedActions, ok := perms[resourceType]
	if !ok {
		return s.evaluateABAC(ctx, actor, applicationID, resourceType, action)
	}

	if !actionIn(action, allowedActions) {
		return s.evaluateABAC(ctx, actor, applicationID, resourceType, action)
	}

	return true, "rbac allowed"
}

func (s *Server) evaluateABAC(ctx context.Context, actor *commonv1.Actor, applicationID, resourceType string, action policyv1.Action) (bool, string) {
	if applicationID == "" {
		return false, fmt.Sprintf("role %s has no access to resource type %s", actor.Role, resourceType)
	}

	rules, err := s.store.GetAttributeRules(ctx, applicationID, resourceType)
	if err != nil || len(rules) == 0 {
		return false, fmt.Sprintf("role %s has no access to resource type %s", actor.Role, resourceType)
	}

	actorAttrs := map[string]string{
		"user_id":      actor.UserId,
		"workspace_id": actor.WorkspaceId,
	}

	for _, rule := range rules {
		val, ok := actorAttrs[rule.AttributeName]
		if !ok || val != rule.AttributeValue {
			continue
		}
		if actionIn(action, rule.Actions) {
			return true, "abac allowed: " + rule.Name
		}
	}

	return false, fmt.Sprintf("role %s has no access to resource type %s", actor.Role, resourceType)
}

func actionIn(action policyv1.Action, allowed []policyv1.Action) bool {
	for _, a := range allowed {
		if a == action {
			return true
		}
	}
	return false
}

func raciAllows(raci policyv1.RACIType, action policyv1.Action) (bool, string) {
	switch raci {
	case policyv1.RACIType_RACI_TYPE_RESPONSIBLE:
		return true, "responsible: can read and write"
	case policyv1.RACIType_RACI_TYPE_ACCOUNTABLE:
		if action == policyv1.Action_ACTION_APPROVE {
			return true, "accountable: can approve"
		}
		return true, "accountable: full access"
	case policyv1.RACIType_RACI_TYPE_CONSULTED:
		if action == policyv1.Action_ACTION_READ {
			return true, "consulted: read only"
		}
		return false, "consulted: cannot write"
	case policyv1.RACIType_RACI_TYPE_INFORMED:
		if action == policyv1.Action_ACTION_READ {
			return true, "informed: read only"
		}
		return false, "informed: cannot write or approve"
	default:
		return false, "no raci assignment"
	}
}

func actionsForRole(role commonv1.Role) []policyv1.Action {
	switch role {
	case commonv1.Role_ROLE_PLATFORM_ADMIN:
		return []policyv1.Action{
			policyv1.Action_ACTION_READ, policyv1.Action_ACTION_WRITE,
			policyv1.Action_ACTION_APPROVE, policyv1.Action_ACTION_ADMIN,
		}
	case commonv1.Role_ROLE_DEVELOPER:
		return []policyv1.Action{policyv1.Action_ACTION_READ, policyv1.Action_ACTION_WRITE}
	case commonv1.Role_ROLE_BUSINESS_ADMIN:
		return []policyv1.Action{policyv1.Action_ACTION_READ, policyv1.Action_ACTION_WRITE}
	default:
		return []policyv1.Action{policyv1.Action_ACTION_READ}
	}
}

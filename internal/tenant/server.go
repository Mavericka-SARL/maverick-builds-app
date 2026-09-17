package tenant

import (
	"context"
	"errors"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonv1 "github.com/mavericks-engine/mavericks/gen/go/common/v1"
	tenantv1 "github.com/mavericks-engine/mavericks/gen/go/tenant/v1"
)

type Server struct {
	tenantv1.UnimplementedTenantServiceServer
	log   zerolog.Logger
	store *Store
}

func NewServer(log zerolog.Logger, store *Store) *Server {
	return &Server{log: log, store: store}
}

func (s *Server) CreateCustomer(ctx context.Context, req *tenantv1.CreateCustomerRequest) (*tenantv1.CreateCustomerResponse, error) {
	if req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "name is required")
	}
	plan := req.Plan
	if plan == "" {
		plan = "starter"
	}
	c, err := s.store.CreateCustomer(ctx, req.Name, plan)
	if err != nil {
		s.log.Error().Err(err).Msg("create customer")
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &tenantv1.CreateCustomerResponse{Customer: c}, nil
}

func (s *Server) GetCustomer(ctx context.Context, req *tenantv1.GetCustomerRequest) (*tenantv1.GetCustomerResponse, error) {
	c, err := s.store.GetCustomer(ctx, req.CustomerId)
	if errors.Is(err, ErrNotFound) {
		return nil, status.Error(codes.NotFound, "customer not found")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &tenantv1.GetCustomerResponse{Customer: c}, nil
}

func (s *Server) CreateWorkspace(ctx context.Context, req *tenantv1.CreateWorkspaceRequest) (*tenantv1.CreateWorkspaceResponse, error) {
	if req.CustomerId == "" || req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "customer_id and name are required")
	}
	w, err := s.store.CreateWorkspace(ctx, req.CustomerId, req.Name)
	if err != nil {
		s.log.Error().Err(err).Msg("create workspace")
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &tenantv1.CreateWorkspaceResponse{Workspace: w}, nil
}

func (s *Server) GetWorkspace(ctx context.Context, req *tenantv1.GetWorkspaceRequest) (*tenantv1.GetWorkspaceResponse, error) {
	w, err := s.store.GetWorkspace(ctx, req.WorkspaceId)
	if errors.Is(err, ErrNotFound) {
		return nil, status.Error(codes.NotFound, "workspace not found")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &tenantv1.GetWorkspaceResponse{Workspace: w}, nil
}

func (s *Server) ListWorkspaces(ctx context.Context, req *tenantv1.ListWorkspacesRequest) (*tenantv1.ListWorkspacesResponse, error) {
	limit, offset := pageParams(req.Page)
	workspaces, err := s.store.ListWorkspaces(ctx, req.CustomerId, limit, offset)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &tenantv1.ListWorkspacesResponse{Workspaces: workspaces}, nil
}

func (s *Server) CreateApplication(ctx context.Context, req *tenantv1.CreateApplicationRequest) (*tenantv1.CreateApplicationResponse, error) {
	if req.WorkspaceId == "" || req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "workspace_id and name are required")
	}
	modeStr := modeToString(req.Mode)
	a, err := s.store.CreateApplication(ctx, req.WorkspaceId, req.Name, modeStr)
	if err != nil {
		s.log.Error().Err(err).Msg("create application")
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &tenantv1.CreateApplicationResponse{Application: a}, nil
}

func (s *Server) GetApplication(ctx context.Context, req *tenantv1.GetApplicationRequest) (*tenantv1.GetApplicationResponse, error) {
	a, err := s.store.GetApplication(ctx, req.ApplicationId)
	if errors.Is(err, ErrNotFound) {
		return nil, status.Error(codes.NotFound, "application not found")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &tenantv1.GetApplicationResponse{Application: a}, nil
}

func (s *Server) ListApplications(ctx context.Context, req *tenantv1.ListApplicationsRequest) (*tenantv1.ListApplicationsResponse, error) {
	limit, offset := pageParams(req.Page)
	apps, err := s.store.ListApplications(ctx, req.WorkspaceId, limit, offset)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &tenantv1.ListApplicationsResponse{Applications: apps}, nil
}

// ── Model ──────────────────────────────────────────────────────────────────────

func (s *Server) CreateModel(ctx context.Context, req *tenantv1.CreateModelRequest) (*tenantv1.CreateModelResponse, error) {
	if req.ApplicationId == "" || req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "application_id and name are required")
	}
	m, err := s.store.CreateModel(ctx, req.ApplicationId, req.Name, req.StorageType)
	if err != nil {
		s.log.Error().Err(err).Msg("create model")
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &tenantv1.CreateModelResponse{Model: m}, nil
}

func (s *Server) GetModel(ctx context.Context, req *tenantv1.GetModelRequest) (*tenantv1.GetModelResponse, error) {
	m, err := s.store.GetModel(ctx, req.ModelId)
	if errors.Is(err, ErrNotFound) {
		return nil, status.Error(codes.NotFound, "model not found")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &tenantv1.GetModelResponse{Model: m}, nil
}

func (s *Server) ListModels(ctx context.Context, req *tenantv1.ListModelsRequest) (*tenantv1.ListModelsResponse, error) {
	limit, offset := pageParams(req.Page)
	models, err := s.store.ListModels(ctx, req.ApplicationId, limit, offset)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &tenantv1.ListModelsResponse{Models: models}, nil
}

func pageParams(p *commonv1.PageRequest) (limit, offset int) {
	limit = 50
	if p != nil && p.PageSize > 0 && p.PageSize <= 200 {
		limit = int(p.PageSize)
	}
	return limit, 0 // cursor-based pagination to be added in Phase 2
}

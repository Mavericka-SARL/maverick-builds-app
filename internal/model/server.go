package model

import (
	"context"
	"errors"
	"fmt"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	commonv1 "github.com/mavericks-engine/mavericks/gen/go/common/v1"
	modelv1 "github.com/mavericks-engine/mavericks/gen/go/model/v1"
	"github.com/mavericks-engine/mavericks/internal/timedim"
)

type Server struct {
	modelv1.UnimplementedModelServiceServer
	log   zerolog.Logger
	store *Store
}

func NewServer(log zerolog.Logger, store *Store) *Server {
	return &Server{log: log, store: store}
}

// ── Dimensions ────────────────────────────────────────────────────────────────

func (s *Server) CreateDimension(ctx context.Context, req *modelv1.CreateDimensionRequest) (*modelv1.CreateDimensionResponse, error) {
	if req.ModelId == "" || req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "model_id and name are required")
	}
	d, err := s.store.CreateDimensionTyped(ctx, req.ModelId, req.Name, req.Properties, timedim.Config{
		Type: req.DimensionType, Granularity: req.TimeGranularity, FiscalYearStartMonth: int(req.FiscalYearStartMonth),
	})
	var terr *timedim.Error
	if errors.As(err, &terr) {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if errors.Is(err, ErrDuplicate) {
		return nil, status.Error(codes.AlreadyExists, err.Error())
	}
	if err != nil {
		s.log.Error().Err(err).Msg("create dimension")
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &modelv1.CreateDimensionResponse{Dimension: d}, nil
}

func (s *Server) GetDimension(ctx context.Context, req *modelv1.GetDimensionRequest) (*modelv1.GetDimensionResponse, error) {
	d, err := s.store.GetDimension(ctx, req.DimensionId)
	if errors.Is(err, ErrNotFound) {
		return nil, status.Error(codes.NotFound, "dimension not found")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &modelv1.GetDimensionResponse{Dimension: d}, nil
}

func (s *Server) ListDimensions(ctx context.Context, req *modelv1.ListDimensionsRequest) (*modelv1.ListDimensionsResponse, error) {
	limit, offset := pageParams(req.Page)
	dims, err := s.store.ListDimensions(ctx, req.ModelId, limit, offset)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &modelv1.ListDimensionsResponse{Dimensions: dims}, nil
}

func (s *Server) CreateDimensionMember(ctx context.Context, req *modelv1.CreateDimensionMemberRequest) (*modelv1.CreateDimensionMemberResponse, error) {
	if req.DimensionId == "" || req.Code == "" {
		return nil, status.Error(codes.InvalidArgument, "dimension_id and code are required")
	}
	m, err := s.store.CreateDimensionMemberPeriod(ctx, req.DimensionId, req.Code, req.Label, req.ParentId, req.Properties, req.PeriodStart, req.PeriodEnd)
	var terr *timedim.Error
	if errors.As(err, &terr) {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if errors.Is(err, ErrDuplicate) {
		return nil, status.Error(codes.AlreadyExists, err.Error())
	}
	if err != nil {
		s.log.Error().Err(err).Msg("create dimension member")
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &modelv1.CreateDimensionMemberResponse{Member: m}, nil
}

func (s *Server) ListDimensionMembers(ctx context.Context, req *modelv1.ListDimensionMembersRequest) (*modelv1.ListDimensionMembersResponse, error) {
	limit, offset := pageParams(req.Page)
	members, err := s.store.ListDimensionMembers(ctx, req.DimensionId, limit, offset)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &modelv1.ListDimensionMembersResponse{Members: members}, nil
}

// ── Metrics ───────────────────────────────────────────────────────────────────

func (s *Server) CreateMetric(ctx context.Context, req *modelv1.CreateMetricRequest) (*modelv1.CreateMetricResponse, error) {
	if req.ModelId == "" || req.Name == "" {
		return nil, status.Error(codes.InvalidArgument, "model_id and name are required")
	}
	if !req.IsInput && req.Formula == "" {
		return nil, status.Error(codes.InvalidArgument, "calculated metrics must have a formula")
	}

	storageStr := storageTypeToString(req.StorageType)
	metric, err := s.store.CreateMetricSummary(ctx, req.ModelId, req.Name, req.Formula, storageStr, req.IsInput, req.TimeSummary)
	if errors.Is(err, ErrDuplicate) {
		return nil, status.Error(codes.AlreadyExists, err.Error())
	}
	if err != nil {
		s.log.Error().Err(err).Msg("create metric")
		return nil, status.Error(codes.Internal, err.Error())
	}

	// Extract and resolve formula dependencies, store dependency edges
	if req.Formula != "" {
		if err := s.resolveAndStoreDeps(ctx, req.ModelId, metric.Id, req.Formula); err != nil {
			// Log but don't fail the creation — deps can be re-resolved on validate
			s.log.Warn().Err(err).Str("metric", req.Name).Msg("dependency resolution deferred")
		}
	}

	return &modelv1.CreateMetricResponse{Metric: metric}, nil
}

func (s *Server) GetMetric(ctx context.Context, req *modelv1.GetMetricRequest) (*modelv1.GetMetricResponse, error) {
	m, err := s.store.GetMetric(ctx, req.MetricId)
	if errors.Is(err, ErrNotFound) {
		return nil, status.Error(codes.NotFound, "metric not found")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &modelv1.GetMetricResponse{Metric: m}, nil
}

func (s *Server) ListMetrics(ctx context.Context, req *modelv1.ListMetricsRequest) (*modelv1.ListMetricsResponse, error) {
	limit, offset := pageParams(req.Page)
	metrics, err := s.store.ListMetrics(ctx, req.ModelId, limit, offset)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &modelv1.ListMetricsResponse{Metrics: metrics}, nil
}

// ValidateMetricFormulas checks for cycles across the entire model's dependency graph.
func (s *Server) ValidateMetricFormulas(ctx context.Context, req *modelv1.ValidateMetricFormulasRequest) (*modelv1.ValidateMetricFormulasResponse, error) {
	graph, names, err := s.store.LoadDependencyGraph(ctx, req.ModelId)
	if err != nil {
		return nil, status.Error(codes.Internal, fmt.Sprintf("load dependency graph: %v", err))
	}

	cycles := DetectCycles(graph, names)
	return &modelv1.ValidateMetricFormulasResponse{
		Valid:       len(cycles) == 0,
		CycleErrors: cycles,
	}, nil
}

// resolveAndStoreDeps looks up metric IDs for formula references and persists the edges.
func (s *Server) resolveAndStoreDeps(ctx context.Context, modelID, metricID, formula string) error {
	refs := ExtractFormulaRefs(formula)
	if len(refs) == 0 {
		return nil
	}

	// Build name→id map from the model's metrics
	metrics, err := s.store.ListMetrics(ctx, modelID, 1000, 0)
	if err != nil {
		return err
	}
	nameToID := make(map[string]string, len(metrics))
	for _, m := range metrics {
		nameToID[m.Name] = m.Id
	}

	ids, err := ResolveRefIDs(refs, nameToID)
	if err != nil {
		return err
	}

	return s.store.UpsertDependencies(ctx, metricID, ids)
}

// ── Hierarchies ───────────────────────────────────────────────────────────────

func (s *Server) CreateHierarchy(ctx context.Context, req *modelv1.CreateHierarchyRequest) (*modelv1.CreateHierarchyResponse, error) {
	if req.ModelId == "" || req.Name == "" || req.DimensionId == "" {
		return nil, status.Error(codes.InvalidArgument, "model_id, name, and dimension_id are required")
	}
	h, err := s.store.CreateHierarchy(ctx, req.ModelId, req.Name, req.DimensionId, req.LevelNames)
	if errors.Is(err, ErrDuplicate) {
		return nil, status.Error(codes.AlreadyExists, err.Error())
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &modelv1.CreateHierarchyResponse{Hierarchy: h}, nil
}

func (s *Server) ListHierarchies(ctx context.Context, req *modelv1.ListHierarchiesRequest) (*modelv1.ListHierarchiesResponse, error) {
	h, err := s.store.ListHierarchies(ctx, req.ModelId)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &modelv1.ListHierarchiesResponse{Hierarchies: h}, nil
}

// ── Scenarios ─────────────────────────────────────────────────────────────────

func (s *Server) CreateScenario(ctx context.Context, req *modelv1.CreateScenarioRequest) (*modelv1.CreateScenarioResponse, error) {
	sc, err := s.store.CreateScenario(ctx, req.ModelId, req.Name, req.Description)
	if errors.Is(err, ErrDuplicate) {
		return nil, status.Error(codes.AlreadyExists, err.Error())
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &modelv1.CreateScenarioResponse{Scenario: sc}, nil
}

func (s *Server) ListScenarios(ctx context.Context, req *modelv1.ListScenariosRequest) (*modelv1.ListScenariosResponse, error) {
	list, err := s.store.ListScenarios(ctx, req.ModelId)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &modelv1.ListScenariosResponse{Scenarios: list}, nil
}

func (s *Server) PublishRevision(ctx context.Context, req *modelv1.PublishRevisionRequest) (*modelv1.PublishRevisionResponse, error) {
	if req.ModelId == "" {
		return nil, status.Error(codes.InvalidArgument, "model_id is required")
	}
	rev, err := s.store.PublishRevision(ctx, req.ModelId)
	if err != nil {
		s.log.Error().Err(err).Str("model_id", req.ModelId).Msg("publish revision")
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &modelv1.PublishRevisionResponse{Revision: rev}, nil
}

func (s *Server) GetRevision(ctx context.Context, req *modelv1.GetRevisionRequest) (*modelv1.GetRevisionResponse, error) {
	if req.RevisionId == "" {
		return nil, status.Error(codes.InvalidArgument, "revision_id is required")
	}
	rev, err := s.store.GetRevision(ctx, req.RevisionId)
	if errors.Is(err, ErrNotFound) {
		return nil, status.Error(codes.NotFound, "revision not found")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &modelv1.GetRevisionResponse{Revision: rev}, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func pageParams(p *commonv1.PageRequest) (limit, offset int) {
	limit = 200
	if p != nil && p.PageSize > 0 && p.PageSize <= 1000 {
		limit = int(p.PageSize)
	}
	return limit, 0
}

func storageTypeToString(t modelv1.StorageType) string {
	switch t {
	case modelv1.StorageType_STORAGE_TYPE_COLUMNAR:
		return "columnar"
	case modelv1.StorageType_STORAGE_TYPE_HYBRID:
		return "hybrid"
	default:
		return "oltp"
	}
}

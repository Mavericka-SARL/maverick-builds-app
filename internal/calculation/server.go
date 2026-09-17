package calculation

import (
	"context"
	"fmt"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	calculationv1 "github.com/mavericks-engine/mavericks/gen/go/calculation/v1"
)

type Server struct {
	calculationv1.UnimplementedCalculationServiceServer
	log       zerolog.Logger
	store     *Store
	scheduler *Scheduler
}

func NewServer(log zerolog.Logger, store *Store, scheduler *Scheduler) *Server {
	return &Server{log: log, store: store, scheduler: scheduler}
}

func (s *Server) GetPartitionState(ctx context.Context, req *calculationv1.GetPartitionStateRequest) (*calculationv1.GetPartitionStateResponse, error) {
	pk := partitionKeyFromProto(req.Key)
	state, err := s.store.GetPartitionState(ctx, pk)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	state.Key = req.Key
	return &calculationv1.GetPartitionStateResponse{State: state}, nil
}

func (s *Server) TriggerRecalc(ctx context.Context, req *calculationv1.TriggerRecalcRequest) (*calculationv1.TriggerRecalcResponse, error) {
	if req.Key == nil {
		return nil, status.Error(codes.InvalidArgument, "partition key is required")
	}

	revisionID := req.Key.RevisionId

	go func() { //nolint:contextcheck // intentional: outlives the request
		bgCtx := context.Background()
		if err := s.scheduler.RecalcAffected(bgCtx,
			req.Key.ModelId, revisionID,
			[]string{req.Key.MetricId},
		); err != nil {
			s.log.Error().Err(err).Msg("manual recalc failed")
		}
	}()

	return &calculationv1.TriggerRecalcResponse{
		JobId:  fmt.Sprintf("manual-%s-%s", req.Key.MetricId, req.Key.TimePartition),
		Queued: true,
	}, nil
}

func (s *Server) TriggerFullRecalc(ctx context.Context, req *calculationv1.TriggerFullRecalcRequest) (*calculationv1.TriggerFullRecalcResponse, error) {
	// TriggerFullRecalcRequest carries no revision_id (unlike every other
	// RPC here) — resolve the model's active revision instead of the old
	// hardcoded "" this handler used to pass straight into RecalcAffected,
	// which silently failed every partition (invalid uuid cast) with
	// nothing ever recalculated.
	var revisionID string
	if err := s.store.pool.QueryRow(ctx,
		`SELECT COALESCE(active_revision_id::text,'') FROM core.model WHERE id=$1::uuid`, req.ModelId,
	).Scan(&revisionID); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if revisionID == "" {
		return nil, status.Error(codes.FailedPrecondition, "model has no active revision")
	}

	defs, err := s.store.LoadModelMetrics(ctx, req.ModelId, revisionID)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	// Collect all input metric IDs to trigger a full recalc from roots
	var inputIDs []string
	for id, def := range defs {
		if def.IsInput {
			inputIDs = append(inputIDs, id)
		}
	}

	go func() { //nolint:contextcheck // intentional: outlives the request
		bgCtx := context.Background()
		if err := s.scheduler.RecalcAffected(bgCtx, req.ModelId, revisionID, inputIDs); err != nil {
			s.log.Error().Err(err).Msg("full recalc failed")
		}
	}()

	return &calculationv1.TriggerFullRecalcResponse{
		JobId:            "full-" + req.ModelId,
		PartitionsQueued: int32(len(defs)),
	}, nil
}

func (s *Server) GetDependencyGraph(ctx context.Context, req *calculationv1.GetDependencyGraphRequest) (*calculationv1.GetDependencyGraphResponse, error) {
	graph, err := s.store.LoadDependencyGraph(ctx, req.ModelId, req.RevisionId)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	var edges []*calculationv1.DependencyEdge
	for from, deps := range graph {
		for _, to := range deps {
			edges = append(edges, &calculationv1.DependencyEdge{
				MetricId:          from,
				DependsOnMetricId: to,
				DependencyType:    "direct",
			})
		}
	}
	return &calculationv1.GetDependencyGraphResponse{Edges: edges}, nil
}

func (s *Server) GetCalculationResult(ctx context.Context, req *calculationv1.GetCalculationResultRequest) (*calculationv1.GetCalculationResultResponse, error) {
	if req.Key == nil {
		return nil, status.Error(codes.InvalidArgument, "partition key is required")
	}
	val, err := s.store.GetCalcValue(ctx, req.Key.ModelId, req.Key.RevisionId, req.Key.MetricId, req.Key.DimensionScope)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &calculationv1.GetCalculationResultResponse{
		Result: &calculationv1.CalculationResult{
			Key:   req.Key,
			Value: val,
		},
	}, nil
}

func partitionKeyFromProto(k *calculationv1.PartitionKey) string {
	if k == nil {
		return ""
	}
	return fmt.Sprintf("%s:rev:%s:%s:%s", k.ModelId, k.RevisionId, k.MetricId, k.TimePartition)
}

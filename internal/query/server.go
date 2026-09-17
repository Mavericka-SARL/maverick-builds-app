package query

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	commonv1 "github.com/mavericks-engine/mavericks/gen/go/common/v1"
	policyv1 "github.com/mavericks-engine/mavericks/gen/go/policy/v1"
	queryv1 "github.com/mavericks-engine/mavericks/gen/go/query/v1"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
)

type Server struct {
	queryv1.UnimplementedQueryServiceServer
	log       zerolog.Logger
	store     *Store
	publisher *Publisher
	policy    policyv1.PolicyServiceClient
}

func NewServer(log zerolog.Logger, store *Store, pub *Publisher, policy policyv1.PolicyServiceClient) *Server {
	return &Server{log: log, store: store, publisher: pub, policy: policy}
}

func (s *Server) Writeback(ctx context.Context, req *queryv1.WritebackRequest) (*queryv1.WritebackResponse, error) {
	if req.ModelId == "" || req.RevisionId == "" || req.UserId == "" {
		return nil, status.Error(codes.InvalidArgument, "model_id, revision_id, and user_id are required")
	}
	if len(req.Updates) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one update is required")
	}

	// Check write permission for each metric via Policy Service
	for _, u := range req.Updates {
		resp, err := s.policy.CheckPermission(ctx, &policyv1.CheckPermissionRequest{
			Actor:        &commonv1.Actor{UserId: req.UserId},
			ResourceType: "data_cell",
			ResourceId:   fmt.Sprintf("%s/%s", req.ModelId, u.MetricId),
			Action:       policyv1.Action_ACTION_WRITE,
		})
		if err != nil {
			s.log.Error().Err(err).Msg("policy check failed; denying writeback")
			return nil, status.Errorf(codes.Internal, "policy service unavailable")
		}
		if !resp.Allowed {
			return nil, status.Errorf(codes.PermissionDenied, "write denied for metric %s: %s", u.MetricId, resp.Reason)
		}
	}

	result, err := s.store.Writeback(ctx, req.ModelId, req.RevisionId, req.UserId, req.Updates)
	if err != nil {
		if errors.Is(err, ErrWriteDenied) {
			return nil, status.Error(codes.PermissionDenied, err.Error())
		}
		s.log.Error().Err(err).Msg("writeback")
		return nil, status.Error(codes.Internal, err.Error())
	}

	// Publish event for the Calculation Engine to consume
	jobID := uuid.New().String()
	if s.publisher != nil {
		evt := FactsCommittedEvent{
			ModelID:    req.ModelId,
			RevisionID: req.RevisionId,
			MetricIDs:  result.MetricIDs,
			UserID:     req.UserId,
		}
		if err := s.publisher.PublishFactsCommitted(ctx, evt); err != nil {
			// Non-fatal: log and continue; recalc can be triggered manually
			s.log.Warn().Err(err).Msg("failed to publish facts.committed; recalc may be delayed")
		}
	}

	return &queryv1.WritebackResponse{
		Success:      true,
		CellsUpdated: int32(result.CellsWritten),
		RecalcJobId:  jobID,
	}, nil
}

// Query and GetCell used to authorize purely via the Policy Service
// (security.metric_policy/dimension_member_policy, a separate,
// unpopulated-in-normal-operation subsystem — nothing in the real product
// writes to those tables, so GetEffectivePermissions came back empty =
// "no restriction" for every real user), or — for GetCell — via no check
// at all (GetCellRequest didn't even carry a user_id). Both now check the
// same identity.user_access_rule rules every other read path does, via
// internal/writeguard.
func (s *Server) Query(ctx context.Context, req *queryv1.QueryRequest) (*queryv1.QueryResponse, error) {
	if req.ModelId == "" {
		return nil, status.Error(codes.InvalidArgument, "model_id is required")
	}
	if len(req.MetricIds) == 0 {
		return nil, status.Error(codes.InvalidArgument, "at least one metric_id is required")
	}

	metricIDs := req.MetricIds
	var allowedDimCodes []string
	if req.UserId != "" {
		var err error
		metricIDs, err = filterMetricsByAccess(ctx, s.store.Pool(), req.UserId, req.MetricIds)
		if err != nil {
			s.log.Error().Err(err).Msg("check metric access")
			return nil, status.Error(codes.Internal, "check metric access")
		}
		allowedDimCodes, err = visibleDimCodes(ctx, s.store.Pool(), req.ModelId, req.UserId)
		if err != nil {
			s.log.Error().Err(err).Msg("check dimension access")
			return nil, status.Error(codes.Internal, "check dimension access")
		}
	}

	if len(metricIDs) == 0 {
		return &queryv1.QueryResponse{AsOf: timestamppb.Now()}, nil
	}

	cells, err := s.store.QueryCells(ctx, req.ModelId, req.RevisionId, metricIDs, allowedDimCodes)
	if err != nil {
		s.log.Error().Err(err).Msg("query cells")
		return nil, status.Error(codes.Internal, err.Error())
	}

	return &queryv1.QueryResponse{
		Cells: cells,
		AsOf:  timestamppb.Now(),
	}, nil
}

func (s *Server) GetCell(ctx context.Context, req *queryv1.GetCellRequest) (*queryv1.GetCellResponse, error) {
	if req.ModelId == "" || req.MetricId == "" {
		return nil, status.Error(codes.InvalidArgument, "model_id and metric_id are required")
	}
	if req.UserId == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id is required")
	}

	if access, err := writeguard.MetricAccess(ctx, s.store.Pool(), req.UserId, req.MetricId); err != nil {
		return nil, status.Error(codes.Internal, "check metric access")
	} else if access == "hidden" {
		return nil, status.Error(codes.PermissionDenied, "access denied")
	}
	for dimID, code := range req.DimMembers {
		var memberID string
		if err := s.store.Pool().QueryRow(ctx,
			`SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`,
			dimID, code,
		).Scan(&memberID); err != nil {
			continue // unknown member: nothing to restrict, matches Writeback's own convention
		}
		if access, err := writeguard.HiddenAccess(ctx, s.store.Pool(), req.UserId, memberID); err != nil {
			return nil, status.Error(codes.Internal, "check member access")
		} else if access == "hidden" {
			return nil, status.Error(codes.PermissionDenied, "access denied")
		}
		chain, err := writeguard.AncestorChain(ctx, s.store.Pool(), memberID)
		if err != nil {
			return nil, status.Error(codes.Internal, "check ancestor chain")
		}
		for _, anc := range chain {
			if anc.ID == memberID {
				continue
			}
			if access, err := writeguard.HiddenAccess(ctx, s.store.Pool(), req.UserId, anc.ID); err != nil {
				return nil, status.Error(codes.Internal, "check member access")
			} else if access == "hidden" {
				return nil, status.Error(codes.PermissionDenied, "access denied")
			}
		}
	}

	cell, err := s.store.GetCell(ctx, req.ModelId, req.RevisionId, req.MetricId, req.DimMembers)
	if err != nil {
		return nil, status.Error(codes.NotFound, "cell not found")
	}
	return &queryv1.GetCellResponse{Cell: cell}, nil
}

// filterMetricsByAccess drops any metric "hidden" for userID, per
// writeguard.MetricAccess.
func filterMetricsByAccess(ctx context.Context, pool *pgxpool.Pool, userID string, metricIDs []string) ([]string, error) {
	var kept []string
	for _, id := range metricIDs {
		access, err := writeguard.MetricAccess(ctx, pool, userID, id)
		if err != nil {
			return nil, err
		}
		if access != "hidden" {
			kept = append(kept, id)
		}
	}
	return kept, nil
}

// visibleDimCodes returns every dimension_member CODE in modelID that
// userID is NOT hidden from (cascading a hidden rule down the hierarchy
// exactly like writeguard.CheckWrite does, via writeguard.ExpandHidden),
// or nil if the user has no dimension_member rules at all —
// dimMembersAllowed treats a nil/empty list as "no restriction", matching
// the allow-everything default here too.
func visibleDimCodes(ctx context.Context, pool *pgxpool.Pool, modelID, userID string) ([]string, error) {
	dimRules := map[string]string{}
	arRows, err := pool.Query(ctx,
		`SELECT ref_id, access FROM identity.user_access_rule WHERE user_id=$1::uuid AND rule_type='dimension_member'`, userID)
	if err != nil {
		return nil, err
	}
	for arRows.Next() {
		var refID, access string
		if arRows.Scan(&refID, &access) == nil {
			dimRules[refID] = access
		}
	}
	arRows.Close()
	if err := arRows.Err(); err != nil {
		return nil, err
	}
	if len(dimRules) == 0 {
		return nil, nil
	}

	rows, err := pool.Query(ctx, `
		SELECT m.id::text, m.code, COALESCE(m.parent_member_id::text,''), m.dimension_id::text
		FROM model.dimension_member m JOIN model.dimension_def d ON d.id = m.dimension_id
		WHERE d.model_id = $1::uuid`, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type memberRow struct{ id, code, parentID, dimID string }
	var members []memberRow
	edges := make([]writeguard.MemberEdge, 0, 256)
	for rows.Next() {
		var mr memberRow
		if rows.Scan(&mr.id, &mr.code, &mr.parentID, &mr.dimID) == nil {
			members = append(members, mr)
			edges = append(edges, writeguard.MemberEdge{ID: mr.id, ParentID: mr.parentID, DimID: mr.dimID})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	hiddenIDs := writeguard.ExpandHidden(edges, dimRules)
	for id, access := range dimRules {
		if access == "hidden" {
			hiddenIDs[id] = true
		}
	}
	var visible []string
	for _, mr := range members {
		if !hiddenIDs[mr.id] {
			visible = append(visible, mr.code)
		}
	}
	return visible, nil
}

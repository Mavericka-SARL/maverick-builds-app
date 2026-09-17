package audit

import (
	"context"
	"errors"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	auditv1 "github.com/mavericks-engine/mavericks/gen/go/audit/v1"
)

type Server struct {
	auditv1.UnimplementedAuditServiceServer
	log   zerolog.Logger
	store *Store
}

func NewServer(log zerolog.Logger, store *Store) *Server {
	return &Server{log: log, store: store}
}

func (s *Server) RecordEvent(ctx context.Context, req *auditv1.RecordEventRequest) (*auditv1.RecordEventResponse, error) {
	if req.Event == nil {
		return nil, status.Error(codes.InvalidArgument, "event is required")
	}
	if req.Event.EventType == "" {
		return nil, status.Error(codes.InvalidArgument, "event_type is required")
	}

	id, err := s.store.RecordEvent(ctx, req.Event)
	if err != nil {
		s.log.Error().Err(err).Str("event_type", req.Event.EventType).Msg("record event")
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &auditv1.RecordEventResponse{EventId: id}, nil
}

func (s *Server) QueryEvents(ctx context.Context, req *auditv1.QueryEventsRequest) (*auditv1.QueryEventsResponse, error) {
	events, err := s.store.QueryEvents(ctx, req)
	if err != nil {
		s.log.Error().Err(err).Msg("query events")
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &auditv1.QueryEventsResponse{Events: events}, nil
}

func (s *Server) GetEvent(ctx context.Context, req *auditv1.GetEventRequest) (*auditv1.GetEventResponse, error) {
	// GetEvent queries a single event by ID — implement as targeted query
	events, err := s.store.QueryEvents(ctx, &auditv1.QueryEventsRequest{
		ResourceId: req.EventId,
	})
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	if len(events) == 0 {
		return nil, status.Error(codes.NotFound, "event not found")
	}
	return &auditv1.GetEventResponse{Event: events[0]}, nil
}

var _ = errors.New // keep import

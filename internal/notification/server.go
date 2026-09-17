package notification

import (
	"context"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	notificationv1 "github.com/mavericks-engine/mavericks/gen/go/notification/v1"
)

type Server struct {
	notificationv1.UnimplementedNotificationServiceServer
	log   zerolog.Logger
	store *Store
}

func NewServer(log zerolog.Logger, store *Store) *Server {
	return &Server{log: log, store: store}
}

func (s *Server) SendNotification(ctx context.Context, req *notificationv1.SendNotificationRequest) (*notificationv1.SendNotificationResponse, error) {
	if req.RecipientUserId == "" {
		return nil, status.Error(codes.InvalidArgument, "recipient_user_id is required")
	}
	if req.TemplateId == "" {
		return nil, status.Error(codes.InvalidArgument, "template_id is required")
	}

	// req has no resource_type/resource_id fields (this gRPC surface isn't
	// called by the gateway, which uses Store directly and bypasses it).
	id, err := s.store.Send(ctx, req.RecipientUserId, req.TemplateId, req.Channel, req.TemplateVars, "", "")
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &notificationv1.SendNotificationResponse{NotificationId: id}, nil
}

func (s *Server) ListNotifications(ctx context.Context, req *notificationv1.ListNotificationsRequest) (*notificationv1.ListNotificationsResponse, error) {
	if req.UserId == "" {
		return nil, status.Error(codes.InvalidArgument, "user_id is required")
	}
	limit := req.PageSize
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	notifs, err := s.store.List(ctx, req.UserId, req.UnreadOnly, limit)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &notificationv1.ListNotificationsResponse{Notifications: notifs}, nil
}

func (s *Server) MarkRead(ctx context.Context, req *notificationv1.MarkReadRequest) (*notificationv1.MarkReadResponse, error) {
	if len(req.NotificationIds) == 0 {
		return &notificationv1.MarkReadResponse{UpdatedCount: 0}, nil
	}
	count, err := s.store.MarkRead(ctx, req.NotificationIds)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &notificationv1.MarkReadResponse{UpdatedCount: count}, nil
}

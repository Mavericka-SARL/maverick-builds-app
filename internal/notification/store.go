package notification

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"google.golang.org/protobuf/types/known/timestamppb"

	notificationv1 "github.com/mavericks-engine/mavericks/gen/go/notification/v1"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Send writes a notification. resourceType/resourceID are optional ("" ->
// NULL) — when set, they let a notification center navigate the recipient
// to the object this notification is about (mirrors audit_event's
// resource_type/resource_id design).
func (s *Store) Send(ctx context.Context, recipientUserID, templateID string, channel notificationv1.NotificationChannel, templateVars map[string]string, resourceType, resourceID string) (string, error) {
	if templateVars == nil {
		templateVars = map[string]string{}
	}
	varsJSON, _ := json.Marshal(templateVars)
	channelStr := channelToString(channel)

	var resTypeArg, resIDArg *string
	if resourceType != "" {
		resTypeArg = &resourceType
	}
	if resourceID != "" {
		resIDArg = &resourceID
	}

	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO notification.notification
		    (recipient_user_id, channel, template_id, template_vars, resource_type, resource_id)
		VALUES ($1::uuid, $2::notification.channel, $3, $4, $5, $6)
		RETURNING id::text
	`, recipientUserID, channelStr, templateID, varsJSON, resTypeArg, resIDArg).Scan(&id)
	if err != nil {
		return "", err
	}

	// in_app is delivered by being written: the console reads the row. The
	// outbound channels stay pending for the dispatcher (dispatch.go).
	if channel == notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_IN_APP {
		_, _ = s.pool.Exec(ctx, `
			UPDATE notification.notification
			SET status = 'delivered', delivered_at = now()
			WHERE id = $1::uuid
		`, id)
	}

	return id, nil
}

func (s *Store) List(ctx context.Context, userID string, unreadOnly bool, limit int32) ([]*notificationv1.Notification, error) {
	query := `
		SELECT id::text, recipient_user_id::text, channel::text, template_id,
		       template_vars, status::text, created_at, delivered_at
		FROM notification.notification
		WHERE recipient_user_id = $1::uuid
	`
	args := []any{userID}

	if unreadOnly {
		query += " AND status != 'read'"
	}
	args = append(args, limit)
	query += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d", len(args))

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var notifs []*notificationv1.Notification
	for rows.Next() {
		var id, recipientID, channelStr, templateID, statusStr string
		var varsJSON []byte
		var createdAt time.Time
		var deliveredAt *time.Time

		if err := rows.Scan(&id, &recipientID, &channelStr, &templateID, &varsJSON, &statusStr, &createdAt, &deliveredAt); err != nil {
			return nil, err
		}

		var templateVars map[string]string
		_ = json.Unmarshal(varsJSON, &templateVars)

		n := &notificationv1.Notification{
			Id:              id,
			RecipientUserId: recipientID,
			Channel:         channelFromString(channelStr),
			TemplateId:      templateID,
			TemplateVars:    templateVars,
			Status:          statusFromString(statusStr),
			CreatedAt:       timestamppb.New(createdAt),
		}
		if deliveredAt != nil {
			n.DeliveredAt = timestamppb.New(*deliveredAt)
		}
		notifs = append(notifs, n)
	}
	return notifs, rows.Err()
}

func (s *Store) MarkRead(ctx context.Context, notificationIDs []string) (int32, error) {
	if len(notificationIDs) == 0 {
		return 0, nil
	}

	// Build $1, $2, ... placeholders
	placeholders := make([]string, len(notificationIDs))
	args := make([]any, len(notificationIDs))
	for i, id := range notificationIDs {
		placeholders[i] = fmt.Sprintf("$%d::uuid", i+1)
		args[i] = id
	}

	tag, err := s.pool.Exec(ctx,
		fmt.Sprintf(`
			UPDATE notification.notification SET status = 'read'
			WHERE id IN (%s) AND status != 'read'
		`, strings.Join(placeholders, ",")),
		args...,
	)
	if err != nil {
		return 0, err
	}
	return int32(tag.RowsAffected()), nil
}

func (s *Store) GetByID(ctx context.Context, id string) (*notificationv1.Notification, error) {
	var nID, recipientID, channelStr, templateID, statusStr string
	var varsJSON []byte
	var createdAt time.Time
	var deliveredAt *time.Time

	err := s.pool.QueryRow(ctx, `
		SELECT id::text, recipient_user_id::text, channel::text, template_id,
		       template_vars, status::text, created_at, delivered_at
		FROM notification.notification WHERE id = $1::uuid
	`, id).Scan(&nID, &recipientID, &channelStr, &templateID, &varsJSON, &statusStr, &createdAt, &deliveredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("notification %s not found", id)
	}
	if err != nil {
		return nil, err
	}

	var templateVars map[string]string
	_ = json.Unmarshal(varsJSON, &templateVars)

	n := &notificationv1.Notification{
		Id:              nID,
		RecipientUserId: recipientID,
		Channel:         channelFromString(channelStr),
		TemplateId:      templateID,
		TemplateVars:    templateVars,
		Status:          statusFromString(statusStr),
		CreatedAt:       timestamppb.New(createdAt),
	}
	if deliveredAt != nil {
		n.DeliveredAt = timestamppb.New(*deliveredAt)
	}
	return n, nil
}

// ── conversion helpers ────────────────────────────────────────────────────────

func channelToString(c notificationv1.NotificationChannel) string {
	switch c {
	case notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_EMAIL:
		return "email"
	case notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_WEBHOOK:
		return "webhook"
	default:
		return "in_app"
	}
}

func channelFromString(s string) notificationv1.NotificationChannel {
	switch s {
	case "email":
		return notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_EMAIL
	case "webhook":
		return notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_WEBHOOK
	default:
		return notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_IN_APP
	}
}

func statusFromString(s string) notificationv1.NotificationStatus {
	switch s {
	case "pending":
		return notificationv1.NotificationStatus_NOTIFICATION_STATUS_PENDING
	case "delivered":
		return notificationv1.NotificationStatus_NOTIFICATION_STATUS_DELIVERED
	case "failed":
		return notificationv1.NotificationStatus_NOTIFICATION_STATUS_FAILED
	case "read":
		return notificationv1.NotificationStatus_NOTIFICATION_STATUS_READ
	default:
		return notificationv1.NotificationStatus_NOTIFICATION_STATUS_UNSPECIFIED
	}
}

// Notify is what producers should call: it writes the in-app notification
// every recipient always gets, and, for each outbound channel this database
// has turned on, one more row for the dispatcher to deliver. A settings read
// that fails is logged nowhere and simply yields in-app only — a notification
// that reached the console is better than none.
func (s *Store) Notify(ctx context.Context, recipientUserID, templateID string, templateVars map[string]string, resourceType, resourceID string) (string, error) {
	id, err := s.Send(ctx, recipientUserID, templateID,
		notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_IN_APP, templateVars, resourceType, resourceID)
	if err != nil {
		return "", err
	}
	// Outbound channels are the recipient's tenant's choice (migration
	// 091); a notification that belongs to no tenant stays in-app.
	cid := s.CustomerOf(ctx, recipientUserID, resourceType, resourceID)
	if cid == "" {
		return id, nil
	}
	settings, _, sErr := s.Effective(ctx, cid, DeploymentDefaults)
	if sErr != nil {
		return id, nil //nolint:nilerr // the in-app notification stands on its own
	}
	if settings.EmailEnabled {
		_, _ = s.Send(ctx, recipientUserID, templateID,
			notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_EMAIL, templateVars, resourceType, resourceID)
	}
	if settings.WebhookEnabled && settings.WebhookURL != "" {
		_, _ = s.Send(ctx, recipientUserID, templateID,
			notificationv1.NotificationChannel_NOTIFICATION_CHANNEL_WEBHOOK, templateVars, resourceType, resourceID)
	}
	return id, nil
}

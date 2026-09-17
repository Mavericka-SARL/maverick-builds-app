package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"google.golang.org/protobuf/types/known/timestamppb"

	auditv1 "github.com/mavericks-engine/mavericks/gen/go/audit/v1"
	commonv1 "github.com/mavericks-engine/mavericks/gen/go/common/v1"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) RecordEvent(ctx context.Context, evt *auditv1.AuditEvent) (string, error) {
	// Non-fatal: default partition catches it if EnsurePartition fails
	_ = s.EnsurePartition(ctx, time.Now())

	meta, _ := json.Marshal(evt.Metadata)
	before, _ := json.Marshal(evt.BeforeState)
	after, _ := json.Marshal(evt.AfterState)

	var userID, workspaceID *string
	if evt.Actor != nil && evt.Actor.UserId != "" {
		userID = &evt.Actor.UserId
	}
	if evt.Actor != nil && evt.Actor.WorkspaceId != "" {
		workspaceID = &evt.Actor.WorkspaceId
	}

	var actorRole *string
	if evt.Actor != nil {
		r := evt.Actor.Role.String()
		actorRole = &r
	}

	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO audit.audit_event
		    (category, event_type, actor_user_id, actor_role, workspace_id,
		     resource_type, resource_id, metadata, before_state, after_state)
		VALUES
		    ($1::audit.event_category, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		RETURNING id
	`,
		categoryToString(evt.Category),
		evt.EventType,
		userID,
		actorRole,
		workspaceID,
		nilIfEmpty(evt.ResourceType),
		nilIfEmpty(evt.ResourceId),
		meta,
		before,
		after,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert audit event: %w", err)
	}
	return id, nil
}

func (s *Store) QueryEvents(ctx context.Context, req *auditv1.QueryEventsRequest) ([]*auditv1.AuditEvent, error) {
	limit, offset := pageParams(req.Page)

	rows, err := s.pool.Query(ctx, `
		SELECT id, category::text, event_type, actor_user_id, actor_role, workspace_id,
		       resource_type, resource_id, metadata, occurred_at
		FROM audit.audit_event
		WHERE ($1::text IS NULL OR workspace_id::text = $1)
		  AND ($2::text IS NULL OR category::text = $2)
		  AND ($3::text IS NULL OR resource_type = $3)
		  AND ($4::text IS NULL OR resource_id = $4)
		  AND ($5::timestamptz IS NULL OR occurred_at >= $5)
		  AND ($6::timestamptz IS NULL OR occurred_at <= $6)
		ORDER BY occurred_at DESC
		LIMIT $7 OFFSET $8
	`,
		nilIfEmpty(req.CustomerId), // using workspace as proxy for now
		nilIfEmpty(categoryToString(req.Category)),
		nilIfEmpty(req.ResourceType),
		nilIfEmpty(req.ResourceId),
		timeOrNil(req.From),
		timeOrNil(req.To),
		limit,
		offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []*auditv1.AuditEvent
	for rows.Next() {
		var e auditv1.AuditEvent
		var cat, eventType string
		var actorUserID, actorRole, workspaceID, resourceType, resourceID *string
		var metaBytes []byte
		var occurredAt time.Time

		if err := rows.Scan(&e.Id, &cat, &eventType, &actorUserID, &actorRole,
			&workspaceID, &resourceType, &resourceID, &metaBytes, &occurredAt); err != nil {
			return nil, err
		}
		e.Category = categoryFromString(cat)
		e.EventType = eventType
		e.OccurredAt = timestamppb.New(occurredAt)
		if actorUserID != nil {
			e.Actor = &commonv1.Actor{UserId: *actorUserID}
			if workspaceID != nil {
				e.Actor.WorkspaceId = *workspaceID
			}
		}
		if resourceType != nil {
			e.ResourceType = *resourceType
		}
		if resourceID != nil {
			e.ResourceId = *resourceID
		}
		if len(metaBytes) > 0 {
			_ = json.Unmarshal(metaBytes, &e.Metadata)
		}
		events = append(events, &e)
	}
	return events, rows.Err()
}

// EnsurePartition creates a monthly partition for the given month if it doesn't exist.
func (s *Store) EnsurePartition(ctx context.Context, t time.Time) error {
	start := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	partName := fmt.Sprintf("audit_event_%d_%02d", t.Year(), t.Month())

	// Check if already registered
	var exists bool
	err := s.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM audit.partition_registry WHERE partition_name = $1)`,
		partName,
	).Scan(&exists)
	if err != nil || exists {
		return err
	}

	// Create partition and register atomically
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	_, err = tx.Exec(ctx, fmt.Sprintf(
		`CREATE TABLE IF NOT EXISTS audit.%s
		 PARTITION OF audit.audit_event
		 FOR VALUES FROM ('%s') TO ('%s')`,
		partName,
		start.Format(time.RFC3339),
		end.Format(time.RFC3339),
	))
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO audit.partition_registry (partition_name, range_start, range_end)
		 VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		partName, start, end,
	)
	if err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// ── helpers ───────────────────────────────────────────────────────────────────

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func timeOrNil(ts *timestamppb.Timestamp) *time.Time {
	if ts == nil {
		return nil
	}
	t := ts.AsTime()
	return &t
}

func pageParams(p *commonv1.PageRequest) (limit, offset int) {
	limit = 100
	if p != nil && p.PageSize > 0 && p.PageSize <= 500 {
		limit = int(p.PageSize)
	}
	return limit, 0
}

func categoryToString(c auditv1.EventCategory) string {
	switch c {
	case auditv1.EventCategory_EVENT_CATEGORY_AUTH:
		return "auth"
	case auditv1.EventCategory_EVENT_CATEGORY_DATA_CHANGE:
		return "data_change"
	case auditv1.EventCategory_EVENT_CATEGORY_MODEL_CHANGE:
		return "model_change"
	case auditv1.EventCategory_EVENT_CATEGORY_POLICY_CHANGE:
		return "policy_change"
	case auditv1.EventCategory_EVENT_CATEGORY_ADMIN:
		return "admin"
	case auditv1.EventCategory_EVENT_CATEGORY_AI_ASSISTANT:
		return "ai_assistant"
	default:
		return ""
	}
}

func categoryFromString(s string) auditv1.EventCategory {
	switch s {
	case "auth":
		return auditv1.EventCategory_EVENT_CATEGORY_AUTH
	case "data_change":
		return auditv1.EventCategory_EVENT_CATEGORY_DATA_CHANGE
	case "model_change":
		return auditv1.EventCategory_EVENT_CATEGORY_MODEL_CHANGE
	case "policy_change":
		return auditv1.EventCategory_EVENT_CATEGORY_POLICY_CHANGE
	case "admin":
		return auditv1.EventCategory_EVENT_CATEGORY_ADMIN
	case "ai_assistant":
		return auditv1.EventCategory_EVENT_CATEGORY_AI_ASSISTANT
	default:
		return auditv1.EventCategory_EVENT_CATEGORY_UNSPECIFIED
	}
}

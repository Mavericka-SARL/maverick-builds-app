package aiassistant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"google.golang.org/protobuf/types/known/timestamppb"

	aiassistantv1 "github.com/mavericks-engine/mavericks/gen/go/aiassistant/v1"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// CreateSession inserts a new assistant session and returns the proto representation.
func (s *Store) CreateSession(ctx context.Context, applicationID, userID string) (*aiassistantv1.AssistantSession, error) {
	var id string
	var createdAt time.Time
	err := s.pool.QueryRow(ctx, `
		INSERT INTO ai_assistant.session (application_id, user_id)
		VALUES ($1::uuid, $2::uuid)
		RETURNING id::text, created_at
	`, applicationID, userID).Scan(&id, &createdAt)
	if err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	return &aiassistantv1.AssistantSession{
		Id:            id,
		ApplicationId: applicationID,
		CreatedAt:     timestamppb.New(createdAt),
	}, nil
}

// GetSession retrieves a session and all its actions.
func (s *Store) GetSession(ctx context.Context, sessionID string) (*aiassistantv1.AssistantSession, []*aiassistantv1.AssistantAction, error) {
	var id, appID string
	var createdAt time.Time

	err := s.pool.QueryRow(ctx, `
		SELECT id::text, application_id::text, created_at
		FROM ai_assistant.session WHERE id = $1::uuid
	`, sessionID).Scan(&id, &appID, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, fmt.Errorf("session %s not found", sessionID)
	}
	if err != nil {
		return nil, nil, err
	}

	session := &aiassistantv1.AssistantSession{
		Id:            id,
		ApplicationId: appID,
		CreatedAt:     timestamppb.New(createdAt),
	}

	actions, err := s.listActions(ctx, sessionID)
	if err != nil {
		return nil, nil, err
	}
	return session, actions, nil
}

// CreateAction stores a generated diff action.
func (s *Store) CreateAction(ctx context.Context, sessionID string, actionType aiassistantv1.ActionType, prompt string, diffs []*aiassistantv1.FileDiff, impact *aiassistantv1.ImpactSummary) (*aiassistantv1.AssistantAction, error) {
	diffsJSON, _ := json.Marshal(diffs)
	impactJSON, _ := json.Marshal(impact)

	var id string
	var createdAt time.Time
	err := s.pool.QueryRow(ctx, `
		INSERT INTO ai_assistant.action (session_id, action_type, prompt, diffs, impact)
		VALUES ($1::uuid, $2::ai_assistant.action_type, $3, $4, $5)
		RETURNING id::text, created_at
	`, sessionID, actionTypeToString(actionType), prompt, diffsJSON, impactJSON).Scan(&id, &createdAt)
	if err != nil {
		return nil, fmt.Errorf("create action: %w", err)
	}

	return &aiassistantv1.AssistantAction{
		Id:         id,
		SessionId:  sessionID,
		ActionType: actionType,
		Prompt:     prompt,
		Diffs:      diffs,
		Impact:     impact,
		Applied:    false,
		CreatedAt:  timestamppb.New(createdAt),
	}, nil
}

// GetAction returns a single action by ID.
func (s *Store) GetAction(ctx context.Context, actionID string) (*aiassistantv1.AssistantAction, error) {
	actions, err := s.queryActions(ctx, "WHERE id = $1::uuid", actionID)
	if err != nil {
		return nil, err
	}
	if len(actions) == 0 {
		return nil, fmt.Errorf("action %s not found", actionID)
	}
	return actions[0], nil
}

// MarkApplied marks an action as applied and optionally records the rollback action ID.
func (s *Store) MarkApplied(ctx context.Context, actionID, rollbackActionID string) error {
	var rbPtr *string
	if rollbackActionID != "" {
		rbPtr = &rollbackActionID
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE ai_assistant.action
		SET applied = true, rollback_action_id = $2::uuid
		WHERE id = $1::uuid
	`, actionID, rbPtr)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("action %s not found", actionID)
	}
	return nil
}

// MarkRolledBack creates an inverse action record and marks the original as having a rollback.
func (s *Store) MarkRolledBack(ctx context.Context, actionID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE ai_assistant.action SET applied = false WHERE id = $1::uuid
	`, actionID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("action %s not found", actionID)
	}
	return nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func (s *Store) listActions(ctx context.Context, sessionID string) ([]*aiassistantv1.AssistantAction, error) {
	return s.queryActions(ctx, "WHERE session_id = $1::uuid ORDER BY created_at ASC", sessionID)
}

func (s *Store) queryActions(ctx context.Context, where string, args ...any) ([]*aiassistantv1.AssistantAction, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id::text, session_id::text, action_type::text, prompt,
		        diffs, impact, applied,
		        COALESCE(rollback_action_id::text,''), created_at
		 FROM ai_assistant.action `+where,
		args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var actions []*aiassistantv1.AssistantAction
	for rows.Next() {
		var id, sessionID, actionTypeStr, prompt, rollbackID string
		var diffsJSON, impactJSON []byte
		var applied bool
		var createdAt time.Time

		if err := rows.Scan(&id, &sessionID, &actionTypeStr, &prompt,
			&diffsJSON, &impactJSON, &applied, &rollbackID, &createdAt); err != nil {
			return nil, err
		}

		var diffs []*aiassistantv1.FileDiff
		var impact aiassistantv1.ImpactSummary
		_ = json.Unmarshal(diffsJSON, &diffs)
		_ = json.Unmarshal(impactJSON, &impact)

		actions = append(actions, &aiassistantv1.AssistantAction{
			Id:               id,
			SessionId:        sessionID,
			ActionType:       actionTypeFromString(actionTypeStr),
			Prompt:           prompt,
			Diffs:            diffs,
			Impact:           &impact,
			Applied:          applied,
			RollbackActionId: rollbackID,
			CreatedAt:        timestamppb.New(createdAt),
		})
	}
	return actions, rows.Err()
}

func actionTypeToString(t aiassistantv1.ActionType) string {
	switch t {
	case aiassistantv1.ActionType_ACTION_TYPE_ADD_METRIC:
		return "add_metric"
	case aiassistantv1.ActionType_ACTION_TYPE_MODIFY_FORMULA:
		return "modify_formula"
	case aiassistantv1.ActionType_ACTION_TYPE_ADD_DIMENSION:
		return "add_dimension"
	case aiassistantv1.ActionType_ACTION_TYPE_MODIFY_POLICY:
		return "modify_policy"
	case aiassistantv1.ActionType_ACTION_TYPE_ADD_WORKFLOW:
		return "add_workflow"
	case aiassistantv1.ActionType_ACTION_TYPE_GENERATE_MIGRATION:
		return "generate_migration"
	default:
		return "add_metric"
	}
}

func actionTypeFromString(s string) aiassistantv1.ActionType {
	switch s {
	case "modify_formula":
		return aiassistantv1.ActionType_ACTION_TYPE_MODIFY_FORMULA
	case "add_dimension":
		return aiassistantv1.ActionType_ACTION_TYPE_ADD_DIMENSION
	case "modify_policy":
		return aiassistantv1.ActionType_ACTION_TYPE_MODIFY_POLICY
	case "add_workflow":
		return aiassistantv1.ActionType_ACTION_TYPE_ADD_WORKFLOW
	case "generate_migration":
		return aiassistantv1.ActionType_ACTION_TYPE_GENERATE_MIGRATION
	default:
		return aiassistantv1.ActionType_ACTION_TYPE_ADD_METRIC
	}
}

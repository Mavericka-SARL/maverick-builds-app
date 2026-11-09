package aiassistant

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ProposalStep is one write action inside a proposal.
type ProposalStep struct {
	Tool        string          `json:"tool"`
	Description string          `json:"description"`
	Params      json.RawMessage `json:"params"`
	// Set after execution:
	Status    string `json:"status,omitempty"`     // "pending"|"success"|"failed"
	Result    string `json:"result,omitempty"`     // human-readable outcome
	CreatedID string `json:"created_id,omitempty"` // id of the resource created (for rollback)
}

// Proposal holds an ordered list of steps awaiting developer confirmation.
type Proposal struct {
	ID        string         `json:"id"`
	SessionID string         `json:"session_id"`
	Steps     []ProposalStep `json:"steps"`
	Status    string         `json:"status"` // pending|confirmed|rejected|executed|partial
	// Warnings are the plan check's warnings the proposal was shown with.
	Warnings   []string   `json:"warnings,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	ExecutedAt *time.Time `json:"executed_at,omitempty"`
}

// ProposalStore handles proposal persistence.
type ProposalStore struct {
	pool *pgxpool.Pool
}

func NewProposalStore(pool *pgxpool.Pool) *ProposalStore {
	return &ProposalStore{pool: pool}
}

// CreateProposal inserts a new pending proposal.
func (s *ProposalStore) CreateProposal(ctx context.Context, sessionID string, steps []ProposalStep, warnings []string) (Proposal, error) {
	stepsJSON, err := json.Marshal(steps)
	if err != nil {
		return Proposal{}, fmt.Errorf("marshal steps: %w", err)
	}
	var p Proposal
	var stepsRaw []byte
	err = s.pool.QueryRow(ctx, `
		INSERT INTO ai_assistant.proposal (session_id, steps, warnings)
		VALUES ($1::uuid, $2::jsonb, COALESCE($3::text[], '{}'))
		RETURNING id::text, session_id::text, steps, status, created_at, warnings
	`, sessionID, stepsJSON, warnings).Scan(&p.ID, &p.SessionID, &stepsRaw, &p.Status, &p.CreatedAt, &p.Warnings)
	if err != nil {
		return Proposal{}, err
	}
	_ = json.Unmarshal(stepsRaw, &p.Steps)
	return p, nil
}

// WithdrawPending withdraws the session's pending proposals other than
// keepID — a newer proposal replaces them: the console shows only the newest,
// and the assistant once told the developer the 32 steps of a replaced one
// "remain in the pending plan" (2026-11-08). They are marked rejected, so
// none can still be confirmed. It returns how many were withdrawn and their
// steps in all.
func (s *ProposalStore) WithdrawPending(ctx context.Context, sessionID, keepID string) (proposals, steps int, err error) {
	err = s.pool.QueryRow(ctx, `
		WITH w AS (
			UPDATE ai_assistant.proposal SET status='rejected'
			WHERE session_id=$1::uuid AND status='pending' AND id <> $2::uuid
			RETURNING jsonb_array_length(steps) AS n)
		SELECT count(*), COALESCE(sum(n), 0) FROM w
	`, sessionID, keepID).Scan(&proposals, &steps)
	return proposals, steps, err
}

// GetProposal fetches a proposal by ID.
func (s *ProposalStore) GetProposal(ctx context.Context, proposalID string) (Proposal, error) {
	var p Proposal
	var stepsRaw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, session_id::text, steps, status, created_at, executed_at, warnings
		FROM ai_assistant.proposal WHERE id=$1::uuid
	`, proposalID).Scan(&p.ID, &p.SessionID, &stepsRaw, &p.Status, &p.CreatedAt, &p.ExecutedAt, &p.Warnings)
	if err != nil {
		return Proposal{}, fmt.Errorf("proposal not found: %w", err)
	}
	_ = json.Unmarshal(stepsRaw, &p.Steps)
	return p, nil
}

// ListPendingProposals returns all pending proposals for a session.
func (s *ProposalStore) ListPendingProposals(ctx context.Context, sessionID string) ([]Proposal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, session_id::text, steps, status, created_at, executed_at, warnings
		FROM ai_assistant.proposal
		WHERE session_id=$1::uuid AND status='pending'
		ORDER BY created_at DESC
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Proposal
	for rows.Next() {
		var p Proposal
		var stepsRaw []byte
		_ = rows.Scan(&p.ID, &p.SessionID, &stepsRaw, &p.Status, &p.CreatedAt, &p.ExecutedAt, &p.Warnings)
		_ = json.Unmarshal(stepsRaw, &p.Steps)
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListProposals returns every proposal for a session, most recent first —
// identical to ListPendingProposals minus the status filter. Backs the
// Activity panel: a session's full proposal history, not just what's still
// awaiting confirmation.
func (s *ProposalStore) ListProposals(ctx context.Context, sessionID string) ([]Proposal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, session_id::text, steps, status, created_at, executed_at, warnings
		FROM ai_assistant.proposal
		WHERE session_id=$1::uuid
		ORDER BY created_at DESC
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Proposal
	for rows.Next() {
		var p Proposal
		var stepsRaw []byte
		_ = rows.Scan(&p.ID, &p.SessionID, &stepsRaw, &p.Status, &p.CreatedAt, &p.ExecutedAt, &p.Warnings)
		_ = json.Unmarshal(stepsRaw, &p.Steps)
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetStatus updates the proposal status (and executed_at if transitioning to executed/partial).
func (s *ProposalStore) SetStatus(ctx context.Context, proposalID, status string) error {
	if status == "executed" || status == "partial" {
		_, err := s.pool.Exec(ctx, `
			UPDATE ai_assistant.proposal SET status=$2, executed_at=now() WHERE id=$1::uuid
		`, proposalID, status)
		return err
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE ai_assistant.proposal SET status=$2 WHERE id=$1::uuid
	`, proposalID, status)
	return err
}

// UpdateSteps persists updated step results back to the DB (called after execution).
func (s *ProposalStore) UpdateSteps(ctx context.Context, proposalID string, steps []ProposalStep) error {
	stepsJSON, err := json.Marshal(steps)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE ai_assistant.proposal SET steps=$2::jsonb WHERE id=$1::uuid
	`, proposalID, stepsJSON)
	return err
}

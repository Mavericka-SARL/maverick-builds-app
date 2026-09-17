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
	ID         string         `json:"id"`
	SessionID  string         `json:"session_id"`
	Steps      []ProposalStep `json:"steps"`
	Status     string         `json:"status"` // pending|confirmed|rejected|executed|partial
	CreatedAt  time.Time      `json:"created_at"`
	ExecutedAt *time.Time     `json:"executed_at,omitempty"`
}

// ProposalStore handles proposal persistence.
type ProposalStore struct {
	pool *pgxpool.Pool
}

func NewProposalStore(pool *pgxpool.Pool) *ProposalStore {
	return &ProposalStore{pool: pool}
}

// CreateProposal inserts a new pending proposal.
func (s *ProposalStore) CreateProposal(ctx context.Context, sessionID string, steps []ProposalStep) (Proposal, error) {
	stepsJSON, err := json.Marshal(steps)
	if err != nil {
		return Proposal{}, fmt.Errorf("marshal steps: %w", err)
	}
	var p Proposal
	var stepsRaw []byte
	err = s.pool.QueryRow(ctx, `
		INSERT INTO ai_assistant.proposal (session_id, steps)
		VALUES ($1::uuid, $2::jsonb)
		RETURNING id::text, session_id::text, steps, status, created_at
	`, sessionID, stepsJSON).Scan(&p.ID, &p.SessionID, &stepsRaw, &p.Status, &p.CreatedAt)
	if err != nil {
		return Proposal{}, err
	}
	_ = json.Unmarshal(stepsRaw, &p.Steps)
	return p, nil
}

// GetProposal fetches a proposal by ID.
func (s *ProposalStore) GetProposal(ctx context.Context, proposalID string) (Proposal, error) {
	var p Proposal
	var stepsRaw []byte
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, session_id::text, steps, status, created_at, executed_at
		FROM ai_assistant.proposal WHERE id=$1::uuid
	`, proposalID).Scan(&p.ID, &p.SessionID, &stepsRaw, &p.Status, &p.CreatedAt, &p.ExecutedAt)
	if err != nil {
		return Proposal{}, fmt.Errorf("proposal not found: %w", err)
	}
	_ = json.Unmarshal(stepsRaw, &p.Steps)
	return p, nil
}

// ListPendingProposals returns all pending proposals for a session.
func (s *ProposalStore) ListPendingProposals(ctx context.Context, sessionID string) ([]Proposal, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, session_id::text, steps, status, created_at, executed_at
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
		_ = rows.Scan(&p.ID, &p.SessionID, &stepsRaw, &p.Status, &p.CreatedAt, &p.ExecutedAt)
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
		SELECT id::text, session_id::text, steps, status, created_at, executed_at
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
		_ = rows.Scan(&p.ID, &p.SessionID, &stepsRaw, &p.Status, &p.CreatedAt, &p.ExecutedAt)
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

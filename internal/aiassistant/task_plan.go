package aiassistant

// The task plan of a delegated goal (migration 162): the AI Developer plans
// a goal the developer leaves to it before building it, works it one stage
// per proposal, and marks each stage as it goes. Every turn of the session is
// shown the plan; the developer sees it in the chat.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// TaskPlan is a delegated goal split into stages.
type TaskPlan struct {
	Goal      string      `json:"goal"`
	Stages    []TaskStage `json:"stages"`
	UpdatedAt time.Time   `json:"updated_at"`
	// CarriedFrom is the earlier session this plan was carried over from
	// (CarryTaskPlan): a session ends at its call limit, the goal does not.
	CarriedFrom string `json:"carried_from,omitempty"`
}

// TaskStage is one stage: what it delivers (named, so a plan for it can be
// checked against them) and how the AI checks it once confirmed.
type TaskStage struct {
	Title  string   `json:"title"`
	Items  []string `json:"items"`
	Check  string   `json:"check,omitempty"`
	Status string   `json:"status"` // pending | in_progress | done | skipped
	Note   string   `json:"note,omitempty"`
	// Confirmed counts the plans the developer confirmed while the stage
	// was current, and their steps (RecordConfirmed).
	Confirmed      int `json:"confirmed,omitempty"`
	ConfirmedSteps int `json:"confirmed_steps,omitempty"`
}

// Stage statuses.
const (
	StagePending    = "pending"
	StageInProgress = "in_progress"
	StageDone       = "done"
	StageSkipped    = "skipped"
)

// Current is the first stage not done or skipped (-1 when none is left).
func (p *TaskPlan) Current() int {
	if p == nil {
		return -1
	}
	for i, s := range p.Stages {
		if s.Status != StageDone && s.Status != StageSkipped {
			return i
		}
	}
	return -1
}

// Render is the plan as the AI and the developer read it.
func (p *TaskPlan) Render() string {
	if p == nil || len(p.Stages) == 0 {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Goal: %s\n", p.Goal)
	if p.CarriedFrom != "" {
		b.WriteString("(Carried over from an earlier session in this draft: go on from the CURRENT stage.)\n")
	}
	cur := p.Current()
	for i, s := range p.Stages {
		mark := map[string]string{StageDone: "done", StageSkipped: "skipped", StageInProgress: "in progress"}[s.Status]
		if mark == "" {
			mark = "to do"
		}
		if i == cur {
			mark += ", CURRENT"
		}
		fmt.Fprintf(&b, "%d. [%s] %s", i+1, mark, s.Title)
		if len(s.Items) > 0 {
			fmt.Fprintf(&b, " — items: %s", strings.Join(s.Items, "; "))
		}
		if s.Check != "" {
			fmt.Fprintf(&b, " — check: %s", s.Check)
		}
		if s.Note != "" {
			fmt.Fprintf(&b, " — note: %s", s.Note)
		}
		if s.Confirmed > 0 {
			fmt.Fprintf(&b, " — confirmed: %d plan(s), %d step(s)", s.Confirmed, s.ConfirmedSteps)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// TaskPlan is sessionID's plan (nil when it has none).
func (s *ChatStore) TaskPlan(ctx context.Context, sessionID string) (*TaskPlan, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT task_plan FROM ai_assistant.session WHERE id = $1::uuid`, sessionID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) || len(raw) == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p TaskPlan
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, nil
	}
	return &p, nil
}

// SetTaskPlan replaces sessionID's plan (nil clears it).
func (s *ChatStore) SetTaskPlan(ctx context.Context, sessionID string, p *TaskPlan) error {
	var raw any
	if p != nil {
		p.UpdatedAt = time.Now().UTC()
		b, err := json.Marshal(p)
		if err != nil {
			return err
		}
		raw = string(b)
	}
	_, err := s.pool.Exec(ctx, `UPDATE ai_assistant.session SET task_plan = $2::jsonb WHERE id = $1::uuid`, sessionID, raw)
	return err
}

// RecordConfirmed notes a confirmed plan of steps steps on the current
// stage, which is then in progress. Stage 1 stayed "in progress" after three
// stages were confirmed (2026-11-08): the AI marks a stage done, but the
// server knows a plan for it was confirmed. It reports whether the plan
// changed.
func (p *TaskPlan) RecordConfirmed(steps int) bool {
	cur := p.Current()
	if cur < 0 {
		return false
	}
	st := &p.Stages[cur]
	if st.Status == StagePending {
		st.Status = StageInProgress
	}
	st.Confirmed++
	st.ConfirmedSteps += steps
	return true
}

// Finished reports whether every stage is done or skipped.
func (p *TaskPlan) Finished() bool { return p != nil && len(p.Stages) > 0 && p.Current() < 0 }

// CarryTaskPlan gives sessionID, new and without a plan, the plan of the
// developer's latest earlier session in the model that has one, when that
// plan is unfinished and the session worked in the same draft (draftID) or
// was promoted — its draft became the developer's working revision. The
// Creator & Agency build ran thirteen sessions (200 calls each) and every
// new one started without its plan (2026-11-08). It returns the plan
// carried, or nil.
func (s *ChatStore) CarryTaskPlan(ctx context.Context, sessionID, modelID, userID, draftID string) (*TaskPlan, error) {
	var from string
	var raw []byte
	var sameDraft, promoted bool
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, task_plan, COALESCE(draft_revision_id::text, '') = $4 AND $4 <> '', promoted_at IS NOT NULL
		FROM ai_assistant.session
		WHERE model_id = $1::uuid AND user_id = $2::uuid AND id <> $3::uuid AND task_plan IS NOT NULL
		ORDER BY created_at DESC LIMIT 1`, modelID, userID, sessionID, draftID).Scan(&from, &raw, &sameDraft, &promoted)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p TaskPlan
	if json.Unmarshal(raw, &p) != nil || p.Finished() || (!sameDraft && !promoted) {
		return nil, nil
	}
	p.CarriedFrom = from
	if err := s.SetTaskPlan(ctx, sessionID, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// NewTaskPlan checks a plan the AI wrote and gives each stage its status.
func NewTaskPlan(goal string, stages []TaskStage) (*TaskPlan, error) {
	goal = strings.TrimSpace(goal)
	if goal == "" {
		return nil, fmt.Errorf("plan_task needs the goal, in the developer's words")
	}
	if len(stages) < 2 {
		return nil, fmt.Errorf("plan_task needs two stages or more: a goal one plan can do needs no task plan — propose it")
	}
	if len(stages) > 20 {
		return nil, fmt.Errorf("plan_task: %d stages; group them into 20 or fewer", len(stages))
	}
	for i := range stages {
		st := &stages[i]
		st.Title = strings.TrimSpace(st.Title)
		if st.Title == "" || len(st.Items) == 0 {
			return nil, fmt.Errorf("plan_task: stage %d needs a title and the items it delivers (metrics, grids, pages, postings — named)", i+1)
		}
		st.Status, st.Note = StagePending, ""
	}
	return &TaskPlan{Goal: goal, Stages: stages}, nil
}

// UpdateStage marks stage (1-based) with status and an optional note.
func (p *TaskPlan) UpdateStage(stage int, status, note string) error {
	if stage < 1 || stage > len(p.Stages) {
		return fmt.Errorf("update_task_plan: stage %d — the plan has %d", stage, len(p.Stages))
	}
	switch status {
	case StagePending, StageInProgress, StageDone, StageSkipped:
	default:
		return fmt.Errorf("update_task_plan: status is pending, in_progress, done or skipped")
	}
	if status == StageSkipped && strings.TrimSpace(note) == "" {
		return fmt.Errorf("update_task_plan: a skipped stage needs a note saying why")
	}
	p.Stages[stage-1].Status = status
	if note = strings.TrimSpace(note); note != "" {
		p.Stages[stage-1].Note = note
	}
	return nil
}

package aiassistant

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

// ChatStore handles sessions, messages, and LLM settings.
type ChatStore struct {
	pool *pgxpool.Pool
}

func NewChatStore(pool *pgxpool.Pool) *ChatStore {
	return &ChatStore{pool: pool}
}

// ── Sessions ─────────────────────────────────────────────────────────────────

type Session struct {
	ID              string `json:"id"`
	AppID           string `json:"app_id"`
	ModelID         string `json:"model_id"`
	UserID          string `json:"user_id"`
	LLMProvider     string `json:"llm_provider"`
	LLMModel        string `json:"llm_model"`
	DraftRevisionID string `json:"draft_revision_id,omitempty"`
	// Title is the session's human name — auto-generated from the first
	// request, renameable. Empty = unnamed (client falls back to the date).
	Title     string    `json:"title"`
	CreatedAt time.Time `json:"created_at"`
	// PromotedAt is set once the session's draft was promoted: the session
	// is finished, and its next message is refused (start a new session).
	PromotedAt *time.Time `json:"promoted_at,omitempty"`
	// CallBudget caps the session's provider calls (migration 180); nil is
	// the deployment's per-session cap.
	CallBudget *int `json:"call_budget,omitempty"`
	// LLMCalls, PlanDone and PlanStages show the session's work in the list:
	// its model calls and its task plan's stages done (or skipped) of all.
	LLMCalls   int `json:"llm_calls"`
	PlanDone   int `json:"plan_done,omitempty"`
	PlanStages int `json:"plan_stages,omitempty"`
}

// SetCallBudget sets (n > 0) or clears (nil) a session's call budget.
func (s *ChatStore) SetCallBudget(ctx context.Context, sessionID, userID string, n *int) error {
	tag, err := s.pool.Exec(ctx, `UPDATE ai_assistant.session SET call_budget = $3 WHERE id = $1::uuid AND user_id = $2::uuid`, sessionID, userID, n)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("session not found")
	}
	return nil
}

// CreateSession starts a session in modelID, a model of appID — a model of
// another application is refused (no row comes back). The model is fixed
// for the session's life: the gateway refuses a request on it that
// resolves another one.
func (s *ChatStore) CreateSession(ctx context.Context, appID, modelID, userID, provider, model string) (Session, error) {
	var sess Session
	var draftRevID *string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO ai_assistant.session (application_id, model_id, user_id, llm_provider, llm_model)
		SELECT m.application_id, m.id, $3::uuid, $4, $5
		FROM core.model m WHERE m.id = $2::uuid AND m.application_id = $1::uuid
		RETURNING id::text, application_id::text, model_id::text, user_id::text, llm_provider, llm_model, draft_revision_id::text, title, created_at, promoted_at
	`, appID, modelID, userID, provider, model).Scan(
		&sess.ID, &sess.AppID, &sess.ModelID, &sess.UserID, &sess.LLMProvider, &sess.LLMModel, &draftRevID, &sess.Title, &sess.CreatedAt, &sess.PromotedAt,
	)
	if draftRevID != nil {
		sess.DraftRevisionID = *draftRevID
	}
	return sess, err
}

// ListSessions lists the user's sessions in one model, newest first.
func (s *ChatStore) ListSessions(ctx context.Context, modelID, userID string) ([]Session, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, application_id::text, model_id::text, user_id::text, llm_provider, llm_model, draft_revision_id::text, title, created_at, promoted_at,
		       llm_calls,
		       CASE WHEN jsonb_typeof(task_plan->'stages') = 'array' THEN jsonb_array_length(task_plan->'stages') ELSE 0 END,
		       CASE WHEN jsonb_typeof(task_plan->'stages') = 'array'
		            THEN (SELECT count(*) FROM jsonb_array_elements(task_plan->'stages') e WHERE e->>'status' IN ('done', 'skipped')) ELSE 0 END
		FROM ai_assistant.session
		WHERE model_id=$1::uuid AND user_id=$2::uuid
		ORDER BY created_at DESC LIMIT 20
	`, modelID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []Session
	for rows.Next() {
		var sess Session
		var draftRevID *string
		if rows.Scan(&sess.ID, &sess.AppID, &sess.ModelID, &sess.UserID, &sess.LLMProvider, &sess.LLMModel, &draftRevID, &sess.Title, &sess.CreatedAt, &sess.PromotedAt,
			&sess.LLMCalls, &sess.PlanStages, &sess.PlanDone) == nil {
			if draftRevID != nil {
				sess.DraftRevisionID = *draftRevID
			}
			sessions = append(sessions, sess)
		}
	}
	return sessions, rows.Err()
}

func (s *ChatStore) GetSession(ctx context.Context, sessionID string) (Session, error) {
	var sess Session
	var draftRevID *string
	err := s.pool.QueryRow(ctx, `
		SELECT id::text, application_id::text, model_id::text, user_id::text, llm_provider, llm_model, draft_revision_id::text, title, created_at, promoted_at, call_budget
		FROM ai_assistant.session WHERE id=$1::uuid
	`, sessionID).Scan(&sess.ID, &sess.AppID, &sess.ModelID, &sess.UserID, &sess.LLMProvider, &sess.LLMModel, &draftRevID, &sess.Title, &sess.CreatedAt, &sess.PromotedAt, &sess.CallBudget)
	if err != nil {
		return Session{}, fmt.Errorf("session not found: %w", err)
	}
	if draftRevID != nil {
		sess.DraftRevisionID = *draftRevID
	}
	return sess, nil
}

// SetDraftRevisionID records the isolated draft revision an AI session
// writes into, once one has been created (lazily, on the first confirmed
// proposal — see aiConfirmProposal in internal/gateway/ai_handler.go).
// revisionID == "" clears it (NULLIF), so the session's next confirmed
// proposal lazily creates a fresh draft instead of continuing to write into
// a revision that's since been promoted or discarded.
func (s *ChatStore) SetDraftRevisionID(ctx context.Context, sessionID, revisionID string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE ai_assistant.session SET draft_revision_id=NULLIF($2,'')::uuid WHERE id=$1::uuid
	`, sessionID, revisionID)
	return err
}

// MarkPromoted records that the session's draft was promoted: the session
// is finished.
func (s *ChatStore) MarkPromoted(ctx context.Context, sessionID string) (time.Time, error) {
	var at time.Time
	err := s.pool.QueryRow(ctx, `UPDATE ai_assistant.session SET promoted_at=now() WHERE id=$1::uuid RETURNING promoted_at`, sessionID).Scan(&at)
	return at, err
}

// EarlierSessions are the sessions whose work sessionID goes on with, oldest
// first: the developer's earlier sessions of the model in the same draft, and
// the sessions its task plan was carried from (carriedFrom, and theirs).
func (s *ChatStore) EarlierSessions(ctx context.Context, sessionID, carriedFrom string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		WITH RECURSIVE chain(id) AS (
			SELECT NULLIF($2, '')::uuid
			UNION
			SELECT NULLIF(o.task_plan->>'carried_from', '')::uuid FROM ai_assistant.session o JOIN chain c ON o.id = c.id
		)
		SELECT o.id::text FROM ai_assistant.session o JOIN ai_assistant.session s ON s.id = $1::uuid
		WHERE o.id <> s.id AND o.model_id = s.model_id AND o.user_id = s.user_id AND o.created_at < s.created_at
		  AND ((s.draft_revision_id IS NOT NULL AND o.draft_revision_id = s.draft_revision_id)
		       OR o.id IN (SELECT id FROM chain WHERE id IS NOT NULL))
		ORDER BY o.created_at`, sessionID, carriedFrom)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// FinishDraftSessions finishes userID's other sessions working in
// revisionID, a draft just taken as the working revision: each is marked
// promoted and loses its draft, as the promoting session does. It returns
// their ids.
func (s *ChatStore) FinishDraftSessions(ctx context.Context, revisionID, userID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		UPDATE ai_assistant.session SET draft_revision_id = NULL, promoted_at = now()
		WHERE draft_revision_id = $1::uuid AND user_id = $2::uuid AND promoted_at IS NULL
		RETURNING id::text`, revisionID, userID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// RenameSession sets the session's title (owner-scoped, like delete).
func (s *ChatStore) RenameSession(ctx context.Context, sessionID, userID, title string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE ai_assistant.session SET title=$3 WHERE id=$1::uuid AND user_id=$2::uuid
	`, sessionID, userID, title)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("session not found")
	}
	return nil
}

// SetTitleIfEmpty writes an auto-generated title only when the user has not
// named the session (themselves or by an earlier generation) — a rename must
// never be overwritten by the background namer.
func (s *ChatStore) SetTitleIfEmpty(ctx context.Context, sessionID, title string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE ai_assistant.session SET title=$2 WHERE id=$1::uuid AND title=''
	`, sessionID, title)
	return err
}

func (s *ChatStore) DeleteSession(ctx context.Context, sessionID, userID string) error {
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM ai_assistant.session WHERE id=$1::uuid AND user_id=$2::uuid
	`, sessionID, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("session not found")
	}
	return nil
}

// ── Messages ─────────────────────────────────────────────────────────────────

type ChatMessage struct {
	ID         string               `json:"id"`
	SessionID  string               `json:"session_id"`
	Role       string               `json:"role"`
	Content    string               `json:"content"`
	ToolCalls  []providers.ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string               `json:"tool_call_id,omitempty"`
	ToolName   string               `json:"tool_name,omitempty"`
	CreatedAt  time.Time            `json:"created_at"`
}

func (s *ChatStore) SaveMessage(ctx context.Context, sessionID, role, content string, toolCalls []providers.ToolCall, toolCallID, toolName string) (ChatMessage, error) {
	var tcJSON []byte
	if len(toolCalls) > 0 {
		tcJSON, _ = json.Marshal(toolCalls)
	}

	var msg ChatMessage
	var tcRaw []byte
	err := s.pool.QueryRow(ctx, `
		INSERT INTO ai_assistant.message (session_id, role, content, tool_calls, tool_call_id)
		VALUES ($1::uuid, $2, $3, $4, $5)
		RETURNING id::text, session_id::text, role, content, tool_calls, COALESCE(tool_call_id,''), created_at
	`, sessionID, role, content, tcJSON, nullStr(toolCallID)).Scan(
		&msg.ID, &msg.SessionID, &msg.Role, &msg.Content, &tcRaw, &msg.ToolCallID, &msg.CreatedAt,
	)
	if err != nil {
		return ChatMessage{}, err
	}
	if len(tcRaw) > 0 {
		_ = json.Unmarshal(tcRaw, &msg.ToolCalls)
	}
	msg.ToolName = toolName
	return msg, nil
}

func (s *ChatStore) ListMessages(ctx context.Context, sessionID string) ([]ChatMessage, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, session_id::text, role, content,
		       tool_calls, COALESCE(tool_call_id,''), created_at
		FROM ai_assistant.message
		WHERE session_id=$1::uuid
		ORDER BY created_at ASC
	`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []ChatMessage
	for rows.Next() {
		var m ChatMessage
		var tcRaw []byte
		_ = rows.Scan(&m.ID, &m.SessionID, &m.Role, &m.Content, &tcRaw, &m.ToolCallID, &m.CreatedAt)
		if len(tcRaw) > 0 {
			_ = json.Unmarshal(tcRaw, &m.ToolCalls)
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

// CountLLMCallsInSession returns the model calls a session has made: the
// count AddUsage keeps with the session, the same number the usage panel
// shows. Counting assistant messages took the server's own notes for calls
// (2026-11-12: a session refused at "200 LLM calls" had made 176).
func (s *ChatStore) CountLLMCallsInSession(ctx context.Context, sessionID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT llm_calls FROM ai_assistant.session WHERE id=$1::uuid`, sessionID).Scan(&n)
	return n, err
}

// CountToolMessages counts the tool results a session holds: the tools its
// turns ran.
func (s *ChatStore) CountToolMessages(ctx context.Context, sessionID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM ai_assistant.message WHERE session_id=$1::uuid AND role='tool'`, sessionID).Scan(&n)
	return n, err
}

// CountLLMCallsToday returns the number of LLM API calls a user has made
// across all of their sessions since the start of the current UTC day.
func (s *ChatStore) CountLLMCallsToday(ctx context.Context, userID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM ai_assistant.message m
		JOIN ai_assistant.session s ON s.id = m.session_id
		WHERE s.user_id=$1::uuid AND m.role='assistant' AND m.created_at >= date_trunc('day', now() AT TIME ZONE 'UTC')
	`, userID).Scan(&n)
	return n, err
}

// unansweredToolCall is the result replayed for a stored tool call that never
// got one, so an old session stays usable.
const unansweredToolCall = "No result was recorded for this call; it did not run."

// Older turns' large tool results and tool-call arguments are shortened
// before each model call. A session replays its whole history every call:
// the HR rebuild's sixth stage failed with "Input tokens exceed the
// configured limit of 272000 tokens" — 727k characters, 440k of them old
// sheet reads and previews and most of the rest earlier plans, all decided.
const (
	// keptTurns: the current turn and the one before it stay whole — a
	// correction ("rejected: use January") is about the plan just shown.
	keptTurns = 2
	// compactAbove: an older tool result or argument longer than this is
	// shortened to its first compactKeep characters.
	compactAbove = 1500
	compactKeep  = 400
)

// CompactHistory returns msgs with the tool results and tool-call
// arguments of turns before the last keptTurns shortened. A turn starts at
// a developer message.
func CompactHistory(msgs []ChatMessage) []ChatMessage {
	turnsSeen, cut := 0, -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			turnsSeen++
			if turnsSeen == keptTurns {
				cut = i
				break
			}
		}
	}
	if cut <= 0 {
		return msgs
	}
	out := make([]ChatMessage, len(msgs))
	copy(out, msgs)
	for i := 0; i < cut; i++ {
		m := out[i]
		if m.Role == "tool" && len(m.Content) > compactAbove {
			m.Content = fmt.Sprintf("%s\n… (an earlier result of %d characters, shortened — call the tool again if you need it)",
				clipRunes(m.Content, compactKeep), len(m.Content))
		}
		if len(m.ToolCalls) > 0 {
			calls := make([]providers.ToolCall, len(m.ToolCalls))
			copy(calls, m.ToolCalls)
			for k, tc := range calls {
				if len(tc.Arguments) > compactAbove {
					note, _ := json.Marshal(map[string]string{"omitted": fmt.Sprintf("an earlier %s call of %d characters, decided", tc.Name, len(tc.Arguments))})
					calls[k].Arguments = note
				}
			}
			m.ToolCalls = calls
		}
		out[i] = m
	}
	return out
}

// MessagesToProviderHistory converts DB messages to the provider format for the LLM.
// A tool call with no stored result (sessions saved before every sibling call
// was answered) gets a stand-in result right after its group: providers refuse
// a history with an unanswered tool call, which would break every later turn.
func MessagesToProviderHistory(msgs []ChatMessage) []providers.Message {
	out := make([]providers.Message, 0, len(msgs))
	var pending []providers.ToolCall
	flush := func() {
		for _, tc := range pending {
			out = append(out, providers.Message{Role: "tool", Content: unansweredToolCall, ToolCallID: tc.ID, ToolName: tc.Name})
		}
		pending = nil
	}
	for _, m := range msgs {
		if m.Role == "tool" {
			for i, tc := range pending {
				if tc.ID == m.ToolCallID {
					pending = append(pending[:i], pending[i+1:]...)
					break
				}
			}
		} else {
			flush()
		}
		out = append(out, providers.Message{
			Role:       m.Role,
			Content:    m.Content,
			ToolCalls:  m.ToolCalls,
			ToolCallID: m.ToolCallID,
			ToolName:   m.ToolName,
		})
		if m.Role == "assistant" {
			pending = append(pending, m.ToolCalls...)
		}
	}
	flush()
	return out
}

// ── LLM Settings ──────────────────────────────────────────────────────────────

type LLMSettings struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	HasKey   bool   `json:"has_key"` // true when api_key_enc is set; key itself is never returned
	// ReasoningEffort is sent with every OpenAI call when set (migration
	// 178): "", none, minimal, low, medium, high or xhigh.
	ReasoningEffort string `json:"reasoning_effort"`
}

// ReasoningEfforts are the values ReasoningEffort may hold.
var ReasoningEfforts = []string{"", "none", "minimal", "low", "medium", "high", "xhigh"}

func (s *ChatStore) GetSettings(ctx context.Context, userID string) (LLMSettings, error) {
	var ls LLMSettings
	var apiKeyEnc *string
	err := s.pool.QueryRow(ctx, `
		SELECT provider, model, api_key_enc, reasoning_effort
		FROM ai_assistant.llm_settings WHERE user_id=$1::uuid
	`, userID).Scan(&ls.Provider, &ls.Model, &apiKeyEnc, &ls.ReasoningEffort)
	if err != nil {
		if err == pgx.ErrNoRows {
			// No model: the gateway runs the provider's default.
			return LLMSettings{Provider: "openai"}, nil
		}
		return LLMSettings{}, err
	}
	ls.HasKey = apiKeyEnc != nil && *apiKeyEnc != ""
	return ls, nil
}

func (s *ChatStore) SaveSettings(ctx context.Context, userID, provider, model, apiKey string) error {
	return s.SaveSettingsWithEffort(ctx, userID, provider, model, apiKey, nil)
}

// SaveSettingsWithEffort is SaveSettings that also sets the reasoning effort
// (nil keeps the saved one).
func (s *ChatStore) SaveSettingsWithEffort(ctx context.Context, userID, provider, model, apiKey string, effort *string) error {
	var keyParam *string
	if apiKey != "" {
		enc, err := EncryptAPIKey(apiKey)
		if err != nil {
			return err
		}
		keyParam = &enc
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO ai_assistant.llm_settings (user_id, provider, model, api_key_enc, reasoning_effort, updated_at)
		VALUES ($1::uuid, $2, $3, $4, COALESCE($5, ''), now())
		ON CONFLICT (user_id) DO UPDATE
		  SET provider=EXCLUDED.provider, model=EXCLUDED.model,
		      api_key_enc=COALESCE(EXCLUDED.api_key_enc, ai_assistant.llm_settings.api_key_enc),
		      reasoning_effort=COALESCE($5, ai_assistant.llm_settings.reasoning_effort),
		      updated_at=now()
	`, userID, provider, model, keyParam, effort)
	return err
}

// GetDecryptedAPIKey returns the raw API key for a user. Values written with
// AI_KEY_ENCRYPTION_SECRET set are AES-256-GCM encrypted; legacy plaintext
// rows pass through unchanged.
func (s *ChatStore) GetDecryptedAPIKey(ctx context.Context, userID string) (string, error) {
	var key *string
	err := s.pool.QueryRow(ctx, `
		SELECT api_key_enc FROM ai_assistant.llm_settings WHERE user_id=$1::uuid
	`, userID).Scan(&key)
	if err != nil || key == nil {
		return "", err
	}
	return DecryptAPIKey(*key)
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// SettleHistory drops, from the turns before the last keptTurns, the tool
// calls and results made before settled — the moment the session's last
// plan was confirmed: what they read is decided, and the task plan, its
// decisions and the workbook map carry what is still needed. Each call of a
// session replays its history; in the High-Tech build every call re-sent
// 60–100k tokens, most of them old reads (2026-11-11). Text the AI and the
// developer wrote stays, and so do the server's notes.
func SettleHistory(msgs []ChatMessage, settled time.Time) []ChatMessage {
	if settled.IsZero() {
		return msgs
	}
	turnsSeen, cut := 0, -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			turnsSeen++
			if turnsSeen == settledKeptTurns {
				cut = i
				break
			}
		}
	}
	if cut <= 0 {
		return msgs
	}
	out := make([]ChatMessage, 0, len(msgs))
	dropped := 0
	for i, m := range msgs {
		if i >= cut || !m.CreatedAt.Before(settled) {
			out = append(out, m)
			continue
		}
		switch {
		case m.Role == "tool":
			dropped++
			continue
		case len(m.ToolCalls) > 0:
			dropped += len(m.ToolCalls)
			if strings.TrimSpace(m.Content) == "" {
				continue
			}
			m.ToolCalls = nil
		}
		// The AI's own replies before the last confirmed plan are decided
		// too: the task plan, its decisions and the target carry them
		// (2026-11-12, "send less with every call"). A stub keeps the turn
		// order; the developer's words and the server's notes stay.
		if m.Role == "assistant" && len(m.Content) > settledReplyKeep && !strings.HasPrefix(m.Content, "(From the server") && !strings.HasPrefix(m.Content, "⚠") {
			m.Content = clipRunes(m.Content, settledReplyKeep) + " … (an earlier reply, settled: the task plan carries it)"
			dropped++
		}
		out = append(out, m)
	}
	if dropped == 0 {
		return msgs
	}
	return out
}

// settledReplyKeep is how much of a settled reply of the AI stays (SettleHistory).
const settledReplyKeep = 200

// settledKeptTurns: only the current turn is kept whole by SettleHistory —
// what came before the last confirmed plan is decided, the turn before
// included (2026-11-12: the two kept turns of a long build replayed ~74k
// tokens of sheet reads at every call). A plan rejected or still pending is
// not confirmed, so its turn stays (settled is the last executed plan).
const settledKeptTurns = 1

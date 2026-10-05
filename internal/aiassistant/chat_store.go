package aiassistant

import (
	"context"
	"encoding/json"
	"fmt"
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
		SELECT id::text, application_id::text, model_id::text, user_id::text, llm_provider, llm_model, draft_revision_id::text, title, created_at, promoted_at
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
		if rows.Scan(&sess.ID, &sess.AppID, &sess.ModelID, &sess.UserID, &sess.LLMProvider, &sess.LLMModel, &draftRevID, &sess.Title, &sess.CreatedAt, &sess.PromotedAt) == nil {
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
		SELECT id::text, application_id::text, model_id::text, user_id::text, llm_provider, llm_model, draft_revision_id::text, title, created_at, promoted_at
		FROM ai_assistant.session WHERE id=$1::uuid
	`, sessionID).Scan(&sess.ID, &sess.AppID, &sess.ModelID, &sess.UserID, &sess.LLMProvider, &sess.LLMModel, &draftRevID, &sess.Title, &sess.CreatedAt, &sess.PromotedAt)
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

// CountLLMCallsInSession returns the number of LLM API calls made so far in
// a session. Every Chat() invocation results in exactly one assistant-role
// message being saved (mid-loop tool-call turns, the propose_actions turn,
// and the final reply all save one each), so this is an exact count, not an
// approximation.
func (s *ChatStore) CountLLMCallsInSession(ctx context.Context, sessionID string) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM ai_assistant.message WHERE session_id=$1::uuid AND role='assistant'
	`, sessionID).Scan(&n)
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
}

func (s *ChatStore) GetSettings(ctx context.Context, userID string) (LLMSettings, error) {
	var ls LLMSettings
	var apiKeyEnc *string
	err := s.pool.QueryRow(ctx, `
		SELECT provider, model, api_key_enc
		FROM ai_assistant.llm_settings WHERE user_id=$1::uuid
	`, userID).Scan(&ls.Provider, &ls.Model, &apiKeyEnc)
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
	var keyParam *string
	if apiKey != "" {
		enc, err := EncryptAPIKey(apiKey)
		if err != nil {
			return err
		}
		keyParam = &enc
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO ai_assistant.llm_settings (user_id, provider, model, api_key_enc, updated_at)
		VALUES ($1::uuid, $2, $3, $4, now())
		ON CONFLICT (user_id) DO UPDATE
		  SET provider=EXCLUDED.provider, model=EXCLUDED.model,
		      api_key_enc=COALESCE(EXCLUDED.api_key_enc, ai_assistant.llm_settings.api_key_enc),
		      updated_at=now()
	`, userID, provider, model, keyParam)
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

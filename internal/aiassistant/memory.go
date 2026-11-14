package aiassistant

// The AI Developer's memory of a session (migration 184; owner, 2026-11-13:
// "AI developer must have memory"). A session goes on for as long as the
// developer works in it, so what the AI learned must outlive the turn that
// learned it:
//   - each attachment is gone through ONCE, when it arrives, and the
//     analysis kept here — later turns read the analysis, not the file, and
//     the file is read again only when the developer asks for it directly;
//   - every part of an attachment the AI reads (a sheet's rows, a sheet's
//     plan) is kept, so asking for it again is answered from memory;
//   - the AI's own notes (remember);
//   - the summary of the earlier conversation, once the memory filled up and
//     was compacted — the developer is told when that happens.
// In the High-Tech rebuilds (2026-11-12) the AI read 22 sheets 180 times
// across nine sessions: read results were settled out of the history after
// each confirmed plan, and every new session started from nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool is the store's connection pool (the gateway's tools open the
// session's other stores on it).
func (s *ChatStore) Pool() *pgxpool.Pool { return s.pool }

// MemoryAttachment is what the AI learned from one attached file.
type MemoryAttachment struct {
	File       string `json:"file"`
	DocumentID string `json:"document_id,omitempty"`
	// Analysis: for a workbook the server's reading of every sheet (role,
	// layout, rows, links, build order); for any other file the AI's own
	// analysis, made once when it arrived.
	Analysis string    `json:"analysis"`
	By       string    `json:"by"` // "server" or "ai"
	At       time.Time `json:"at"`
}

// MemoryRead is one read of part of an attachment, kept so that asking for
// it again is answered from memory, not from the file.
type MemoryRead struct {
	Tool    string    `json:"tool"`
	Key     string    `json:"key"`
	File    string    `json:"file,omitempty"`
	Sheet   string    `json:"sheet,omitempty"`
	FromRow int       `json:"from_row,omitempty"`
	ToRow   int       `json:"to_row,omitempty"`
	Result  string    `json:"result"`
	At      time.Time `json:"at"`
	// Compacted: the result was shortened when the memory filled up.
	Compacted bool `json:"compacted,omitempty"`
}

// MemoryNote is a fact the AI chose to keep (remember).
type MemoryNote struct {
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// SessionMemory is the session's memory.
type SessionMemory struct {
	Attachments []MemoryAttachment `json:"attachments,omitempty"`
	Reads       []MemoryRead       `json:"reads,omitempty"`
	Notes       []MemoryNote       `json:"notes,omitempty"`
	// Summary is the earlier conversation, compacted: messages up to
	// SummaryUpTo are no longer sent to the model, the summary is.
	Summary     string     `json:"summary,omitempty"`
	SummaryUpTo *time.Time `json:"summary_up_to,omitempty"`
	Compactions int        `json:"compactions,omitempty"`
	CompactedAt *time.Time `json:"compacted_at,omitempty"`
}

// Memory is sessionID's memory (empty, never nil, when it has none).
func (s *ChatStore) Memory(ctx context.Context, sessionID string) (*SessionMemory, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT memory FROM ai_assistant.session WHERE id = $1::uuid`, sessionID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && len(raw) == 0) {
		return &SessionMemory{}, nil
	}
	if err != nil {
		return &SessionMemory{}, err
	}
	var m SessionMemory
	if json.Unmarshal(raw, &m) != nil {
		return &SessionMemory{}, nil
	}
	return &m, nil
}

// UpdateMemory changes sessionID's memory with fn, under a row lock: a read
// hook and a remember call of the same turn never lose each other's write.
func (s *ChatStore) UpdateMemory(ctx context.Context, sessionID string, fn func(*SessionMemory) error) (*SessionMemory, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT memory FROM ai_assistant.session WHERE id = $1::uuid FOR UPDATE`, sessionID).Scan(&raw); err != nil {
		return nil, err
	}
	m := &SessionMemory{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, m)
	}
	if err := fn(m); err != nil {
		return nil, err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE ai_assistant.session SET memory = $2::jsonb WHERE id = $1::uuid`, sessionID, string(b)); err != nil {
		return nil, err
	}
	return m, tx.Commit(ctx)
}

// Attachment is what the memory holds of file (nil when it was never
// analysed).
func (m *SessionMemory) Attachment(file string) *MemoryAttachment {
	for i := range m.Attachments {
		if strings.EqualFold(m.Attachments[i].File, file) {
			return &m.Attachments[i]
		}
	}
	return nil
}

// SetAttachment keeps file's analysis (replacing an earlier one).
func (m *SessionMemory) SetAttachment(file, docID, analysis, by string) {
	a := MemoryAttachment{File: file, DocumentID: docID, Analysis: strings.TrimSpace(analysis), By: by, At: time.Now().UTC()}
	if cur := m.Attachment(file); cur != nil {
		*cur = a
		return
	}
	m.Attachments = append(m.Attachments, a)
}

// ForgetDocument drops everything the memory holds of the document docID
// (it was removed).
func (m *SessionMemory) ForgetDocument(docID string) {
	var keep []MemoryAttachment
	for _, a := range m.Attachments {
		if a.DocumentID != docID {
			keep = append(keep, a)
		}
	}
	m.Attachments = keep
	var reads []MemoryRead
	for _, r := range m.Reads {
		if !strings.Contains(r.Key, "|"+docID+"|") {
			reads = append(reads, r)
		}
	}
	m.Reads = reads
}

// FindRead returns the read that answers a call of tool: the same key, or
// for read_attached_sheet a read of the same document (docID) and sheet
// whose rows cover fromRow..toRow (0 = to the end; a whole-sheet read covers
// every range).
func (m *SessionMemory) FindRead(tool, key, docID, sheet string, fromRow, toRow int) *MemoryRead {
	for i := len(m.Reads) - 1; i >= 0; i-- {
		r := &m.Reads[i]
		if r.Tool != tool {
			continue
		}
		if r.Key == key {
			return r
		}
		if tool == "read_attached_sheet" && strings.HasPrefix(r.Key, tool+"|"+docID+"|") && strings.EqualFold(r.Sheet, sheet) && !r.Compacted &&
			r.FromRow <= max(fromRow, 1) && (r.ToRow == 0 || (toRow != 0 && toRow <= r.ToRow)) {
			return r
		}
	}
	return nil
}

// PutRead keeps a read (replacing one of the same key).
func (m *SessionMemory) PutRead(r MemoryRead) {
	r.At = time.Now().UTC()
	for i := range m.Reads {
		if m.Reads[i].Key == r.Key {
			m.Reads[i] = r
			return
		}
	}
	m.Reads = append(m.Reads, r)
}

// AddNote keeps text unless the memory holds it already. It reports
// whether it was added.
func (m *SessionMemory) AddNote(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" {
		return false
	}
	for _, n := range m.Notes {
		if strings.EqualFold(n.Text, text) {
			return false
		}
	}
	m.Notes = append(m.Notes, MemoryNote{Text: clipRunes(text, maxNoteRunes), At: time.Now().UTC()})
	return true
}

const (
	maxNoteRunes     = 4000
	maxAnalysisRunes = 24000
)

// ClipAnalysis bounds an attachment's analysis.
func ClipAnalysis(s string) string { return clipRunes(strings.TrimSpace(s), maxAnalysisRunes) }

// Render is the memory as the AI reads it in every turn: the attachments'
// analyses and the notes whole, the summary of the earlier conversation,
// and what it read from the attachments by name (asking for any of it again
// is answered from memory). earlier lists the session's plans no longer
// under way.
func (m *SessionMemory) Render(earlier []EarlierPlan) string {
	if m.empty() && len(earlier) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\n## Your memory of this session\n")
	b.WriteString("What you learned earlier in this session, kept for every turn. Work from it: never read an attachment again to find " +
		"what is here — a read tool asked for a part you read already answers from memory. Keep a fact you will need later " +
		"(a decision, a finding, a figure) with remember.\n")
	if len(m.Attachments) > 0 {
		b.WriteString("\n### Attachments (analysed once, when they arrived)\n")
		for _, a := range m.Attachments {
			fmt.Fprintf(&b, "\n#### %s (analysed %s)\n%s\n", a.File, a.At.Local().Format("2006-01-02 15:04"), a.Analysis)
		}
	}
	if m.Summary != "" {
		b.WriteString("\n### The conversation so far (compacted)\n")
		b.WriteString(m.Summary)
		b.WriteString("\n")
	}
	if len(m.Notes) > 0 {
		b.WriteString("\n### Your notes\n")
		for _, n := range m.Notes {
			fmt.Fprintf(&b, "- %s\n", n.Text)
		}
	}
	if len(m.Reads) > 0 {
		b.WriteString("\n### Parts of attachments you read (asked again, they come from memory)\n")
		for _, r := range m.Reads {
			fmt.Fprintf(&b, "- %s\n", r.Label())
		}
	}
	if len(earlier) > 0 {
		b.WriteString("\n### Earlier plans of this session (one plan per request)\n")
		for i, e := range earlier {
			fmt.Fprintf(&b, "%d. %s — %s\n", i+1, e.Plan.Goal, e.Describe())
		}
		b.WriteString("plan_task {\"resume_plan\": n} takes one up again when the developer asks for it.\n")
	}
	return b.String()
}

func (m *SessionMemory) empty() bool {
	return len(m.Attachments) == 0 && len(m.Reads) == 0 && len(m.Notes) == 0 && m.Summary == ""
}

// Label names a read in one line.
func (r MemoryRead) Label() string {
	var s string
	switch r.Tool {
	case "read_attached_sheet":
		rows := "all rows"
		if r.FromRow > 0 || r.ToRow > 0 {
			rows = fmt.Sprintf("rows %d–%s", max(r.FromRow, 1), map[bool]string{true: "end", false: strconv.Itoa(r.ToRow)}[r.ToRow == 0])
		}
		s = fmt.Sprintf("%s › %s, %s", r.File, r.Sheet, rows)
	case "plan_sheet":
		s = fmt.Sprintf("plan of sheet %s", r.Sheet)
	case "analyse_workbook":
		s = fmt.Sprintf("analysis of %s", r.File)
	default:
		s = r.Tool + " " + r.Key
	}
	s += fmt.Sprintf(" (%s)", r.At.Local().Format("01-02 15:04"))
	if r.Compacted {
		s += " — shortened when the memory was compacted"
	}
	return s
}

// ── How full the memory is ───────────────────────────────────────────────────

// MemoryCapacity is how many tokens the memory holds before it is compacted
// (AI_MEMORY_TOKENS, else 120,000): what the session carries into every call
// — the memory itself and the conversation since it was last compacted.
func MemoryCapacity() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("AI_MEMORY_TOKENS"))); err == nil && n >= 10000 {
		return n
	}
	return 120000
}

// CompactAtPercent is how full the memory is when it is compacted.
const CompactAtPercent = 85

// EstimateTokens is a token count for s (four characters a token).
func EstimateTokens(s string) int { return (utf8.RuneCountInString(s) + 3) / 4 }

// MemoryStatus is how full the memory is and what it holds, for the
// developer.
type MemoryStatus struct {
	CapacityTokens int `json:"capacity_tokens"`
	UsedTokens     int `json:"used_tokens"`
	Percent        int `json:"percent"`
	// Parts of UsedTokens.
	ConversationTokens int `json:"conversation_tokens"`
	AttachmentTokens   int `json:"attachment_tokens"`
	NotesTokens        int `json:"notes_tokens"`
	SummaryTokens      int `json:"summary_tokens"`
	// What it holds.
	Attachments []MemoryAttachmentInfo `json:"attachments"`
	Reads       int                    `json:"reads"`
	Notes       []string               `json:"notes"`
	Summary     string                 `json:"summary,omitempty"`
	Compactions int                    `json:"compactions"`
	CompactedAt *time.Time             `json:"compacted_at,omitempty"`
	CompactAt   int                    `json:"compact_at_percent"`
}

// MemoryAttachmentInfo is one attachment in the status.
type MemoryAttachmentInfo struct {
	File   string    `json:"file"`
	By     string    `json:"by"`
	At     time.Time `json:"at"`
	Tokens int       `json:"tokens"`
}

// Status is the memory's status, with conversationTokens the tokens of the
// conversation carried since the last compaction.
func (m *SessionMemory) Status(conversationTokens int, earlier []EarlierPlan) MemoryStatus {
	st := MemoryStatus{CapacityTokens: MemoryCapacity(), ConversationTokens: conversationTokens, Reads: len(m.Reads),
		Notes: []string{}, Attachments: []MemoryAttachmentInfo{}, Summary: m.Summary, Compactions: m.Compactions,
		CompactedAt: m.CompactedAt, CompactAt: CompactAtPercent}
	for _, a := range m.Attachments {
		t := EstimateTokens(a.Analysis)
		st.AttachmentTokens += t
		st.Attachments = append(st.Attachments, MemoryAttachmentInfo{File: a.File, By: a.By, At: a.At, Tokens: t})
	}
	for _, n := range m.Notes {
		st.NotesTokens += EstimateTokens(n.Text)
		st.Notes = append(st.Notes, n.Text)
	}
	st.SummaryTokens = EstimateTokens(m.Summary)
	rest := EstimateTokens(m.Render(earlier)) - st.AttachmentTokens - st.NotesTokens - st.SummaryTokens
	st.UsedTokens = conversationTokens + st.AttachmentTokens + st.NotesTokens + st.SummaryTokens + max(rest, 0)
	st.Percent = min(100*st.UsedTokens/max(st.CapacityTokens, 1), 999)
	return st
}

// ConversationTokens is the size of the conversation as it is sent.
func ConversationTokens(msgs []ChatMessage) int {
	n := 0
	for _, m := range msgs {
		n += EstimateTokens(m.Content)
		for _, tc := range m.ToolCalls {
			n += EstimateTokens(tc.Name) + EstimateTokens(string(tc.Arguments))
		}
	}
	return n
}

// SinceSummary drops the messages the memory's summary holds.
func (m *SessionMemory) SinceSummary(msgs []ChatMessage) []ChatMessage {
	if m == nil || m.SummaryUpTo == nil {
		return msgs
	}
	for i, msg := range msgs {
		if msg.CreatedAt.After(*m.SummaryUpTo) {
			// Never start on a tool result: its call went into the summary.
			for i < len(msgs) && msgs[i].Role == "tool" {
				i++
			}
			return msgs[i:]
		}
	}
	return nil
}

// ── Compaction ───────────────────────────────────────────────────────────────

// CompactKeepTurns is how many of the latest developer messages (with what
// followed them) stay as they are when the memory is compacted.
const CompactKeepTurns = 2

// CompactSplit is where the memory's compaction cuts msgs (the conversation
// since the last compaction): msgs[:i] go into the summary. 0 = nothing to
// compact.
func CompactSplit(msgs []ChatMessage) int {
	seen := 0
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			seen++
			if seen == CompactKeepTurns {
				return i
			}
		}
	}
	return 0
}

// CompactMemoryPrompt asks for the summary of the earlier conversation.
const CompactMemoryPrompt = `You keep the memory of an AI developer that builds a planning model (dimensions, metrics,
grids, dashboards, tables, workflows) with a developer, in one long session. The memory is full: write the summary of
the earlier conversation that replaces it. You get the summary kept so far (if any) and the conversation after it.

Write one summary of both, for the AI developer to go on from, in these parts:
- Requests: what the developer asked for, in their words, in order, and which are finished, under way or open.
- Decisions: every choice and convention the developer made (units, signs, names, what to skip), exactly.
- Built: what was proposed, confirmed and built (object names exact), and what was checked with what result.
- Findings: what the AI found out (from the model, the attachments, comparisons) that is still needed.
- Open: questions waiting for the developer, problems not solved, what comes next.
Keep names, numbers, sheet and row references exact. No pleasantries, no repetition, plain text with short bullets,
at most 1,500 words. Leave out what the memory keeps anyway: the attachments' analyses and the task plan.`

// CompactTranscript renders msgs for the compaction call: what each side
// said, the tools called and their results, clipped.
func CompactTranscript(summary string, msgs []ChatMessage) string {
	var b strings.Builder
	if summary != "" {
		b.WriteString("SUMMARY KEPT SO FAR:\n")
		b.WriteString(summary)
		b.WriteString("\n\nCONVERSATION AFTER IT:\n")
	}
	for _, m := range msgs {
		switch {
		case m.Role == "tool":
			fmt.Fprintf(&b, "[result of %s] %s\n", m.ToolName, clipRunes(m.Content, 600))
		case len(m.ToolCalls) > 0:
			if t := strings.TrimSpace(m.Content); t != "" {
				fmt.Fprintf(&b, "AI: %s\n", clipRunes(t, 2000))
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "[AI calls %s] %s\n", tc.Name, clipRunes(string(tc.Arguments), 600))
			}
		case m.Role == "user":
			fmt.Fprintf(&b, "DEVELOPER: %s\n", clipRunes(m.Content, 4000))
		default:
			fmt.Fprintf(&b, "AI: %s\n", clipRunes(m.Content, 3000))
		}
	}
	return clipRunes(b.String(), 400000)
}

// CompactReads shortens what the memory keeps of attachment parts once it
// is compacted: reads of sheets no open stage needs keep their first lines.
// open names the sheets the plan still needs (case-insensitive). It returns
// how many reads were shortened.
func (m *SessionMemory) CompactReads(open map[string]bool) int {
	n := 0
	for i := range m.Reads {
		r := &m.Reads[i]
		if r.Compacted || r.Tool == "analyse_workbook" || open[strings.ToLower(r.Sheet)] || utf8.RuneCountInString(r.Result) <= 1200 {
			continue
		}
		r.Result = clipRunes(r.Result, 1200) + "\n(… shortened when the memory was compacted: this sheet's stage is done or not planned; " +
			"the developer can ask to read it again)"
		r.Compacted = true
		n++
	}
	return n
}

// ── Reading an attachment again ─────────────────────────────────────────────

// rereadRE: the developer asks for an attachment to be read or analysed
// again (English and Russian; the testers write both).
var rereadRE = regexp.MustCompile(`(?i)\b(re-?read|re-?analy[sz]e|re-?examine|re-?check the (file|attachment|workbook|sheet|document))` +
	`|\b(read|analy[sz]e|go through|look (at|through)|check|open|scan|review)\b[^.?!\n]{0,40}\b(again|once more|afresh|anew)\b` +
	`|\bfresh (look|read)\b|\brefresh (the |your )?(analysis|memory)\b` +
	`|перечита|пересмотр|переанализ|повторно\s+(прочит|проанализ|изуч|посмотр|просмотр)` +
	`|(заново|снова|ещё раз|еще раз)\s+(прочит|проанализ|изуч|посмотр|просмотр|пройд)` +
	`|(прочит|проанализ|изуч|посмотр|просмотр|пройд)\S*\s+(\S+\s+){0,3}(заново|снова|ещё раз|еще раз)`)

// AsksToReread reports whether the developer's message asks directly for an
// attachment to be read or analysed again — the one case a read tool goes
// back to the file for a part the memory holds.
func AsksToReread(msg string) bool { return rereadRE.MatchString(msg) }

// ── Plans no longer under way ────────────────────────────────────────────────

// EarlierPlan is a plan of the session that is no longer under way: one
// plan per request, so a new request sets the open plan aside, or follows a
// finished one.
type EarlierPlan struct {
	Plan  TaskPlan  `json:"plan"`
	State string    `json:"state"` // "finished" or "set_aside"
	At    time.Time `json:"at"`
}

// Describe says how far the plan got.
func (e EarlierPlan) Describe() string {
	done := 0
	for _, st := range e.Plan.Stages {
		if st.Status == StageDone || st.Status == StageSkipped {
			done++
		}
	}
	state := "finished"
	if e.State == "set_aside" {
		state = "set aside"
	}
	return fmt.Sprintf("%d of %d stages done, %s", done, len(e.Plan.Stages), state)
}

// EarlierPlans are sessionID's plans no longer under way, oldest first.
func (s *ChatStore) EarlierPlans(ctx context.Context, sessionID string) ([]EarlierPlan, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT earlier_plans FROM ai_assistant.session WHERE id = $1::uuid`, sessionID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && len(raw) == 0) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []EarlierPlan
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

// SetEarlierPlans replaces sessionID's earlier plans.
func (s *ChatStore) SetEarlierPlans(ctx context.Context, sessionID string, plans []EarlierPlan) error {
	var raw any
	if len(plans) > 0 {
		b, err := json.Marshal(plans)
		if err != nil {
			return err
		}
		raw = string(b)
	}
	_, err := s.pool.Exec(ctx, `UPDATE ai_assistant.session SET earlier_plans = $2::jsonb WHERE id = $1::uuid`, sessionID, raw)
	return err
}

// PutPlanAside keeps p, the session's plan until now, among its earlier
// plans: finished when every stage is done, else set aside.
func (s *ChatStore) PutPlanAside(ctx context.Context, sessionID string, p *TaskPlan) error {
	if p == nil || len(p.Stages) == 0 {
		return nil
	}
	plans, err := s.EarlierPlans(ctx, sessionID)
	if err != nil {
		return err
	}
	state := "set_aside"
	if p.Finished() {
		state = "finished"
	}
	return s.SetEarlierPlans(ctx, sessionID, append(plans, EarlierPlan{Plan: *p, State: state, At: time.Now().UTC()}))
}

// PlanRef is a plan of one of the developer's sessions, offered to attach
// to another session.
type PlanRef struct {
	SessionID    string    `json:"session_id"`
	SessionTitle string    `json:"session_title"`
	SessionAt    time.Time `json:"session_created_at"`
	// Index: -1 the session's current plan, else its earlier plan Index.
	Index       int       `json:"index"`
	Goal        string    `json:"goal"`
	Stages      int       `json:"stages"`
	Done        int       `json:"done"`
	State       string    `json:"state"` // "current", "finished" or "set_aside"
	UpdatedAt   time.Time `json:"updated_at"`
	Attachments []string  `json:"attachments"`
}

// ListPlanRefs lists the plans of userID's sessions in modelID other than
// exceptSessionID, newest session first.
func (s *ChatStore) ListPlanRefs(ctx context.Context, modelID, userID, exceptSessionID string) ([]PlanRef, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT s.id::text, s.title, s.created_at, s.task_plan, s.earlier_plans,
		       COALESCE((SELECT array_agg(d.filename ORDER BY d.created_at) FROM ai_assistant.document d WHERE d.session_id = s.id), '{}')
		FROM ai_assistant.session s
		WHERE s.model_id = $1::uuid AND s.user_id = $2::uuid AND s.id <> NULLIF($3, '')::uuid
		  AND (s.task_plan IS NOT NULL OR s.earlier_plans IS NOT NULL)
		ORDER BY s.created_at DESC LIMIT 50`, modelID, userID, exceptSessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PlanRef
	for rows.Next() {
		var id, title string
		var at time.Time
		var cur, earlier []byte
		var files []string
		if err := rows.Scan(&id, &title, &at, &cur, &earlier, &files); err != nil {
			return nil, err
		}
		ref := func(p TaskPlan, idx int, state string) PlanRef {
			r := PlanRef{SessionID: id, SessionTitle: title, SessionAt: at, Index: idx, Goal: p.Goal, Stages: len(p.Stages), State: state,
				UpdatedAt: p.UpdatedAt, Attachments: files}
			for _, st := range p.Stages {
				if st.Status == StageDone || st.Status == StageSkipped {
					r.Done++
				}
			}
			if r.Attachments == nil {
				r.Attachments = []string{}
			}
			return r
		}
		var p TaskPlan
		if len(cur) > 0 && json.Unmarshal(cur, &p) == nil && len(p.Stages) > 0 {
			out = append(out, ref(p, -1, "current"))
		}
		var eps []EarlierPlan
		if len(earlier) > 0 && json.Unmarshal(earlier, &eps) == nil {
			for i := len(eps) - 1; i >= 0; i-- {
				out = append(out, ref(eps[i].Plan, i, eps[i].State))
			}
		}
	}
	return out, rows.Err()
}

// PlanOf is the plan PlanRef (sessionID, index) names, with the session's
// memory (nil when there is none).
func (s *ChatStore) PlanOf(ctx context.Context, sessionID string, index int) (*TaskPlan, error) {
	if index < 0 {
		p, err := s.TaskPlan(ctx, sessionID)
		if err == nil && (p == nil || len(p.Stages) == 0) {
			return nil, fmt.Errorf("that session has no plan")
		}
		return p, err
	}
	eps, err := s.EarlierPlans(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	if index >= len(eps) {
		return nil, fmt.Errorf("that session has no plan %d", index+1)
	}
	p := eps[index].Plan
	return &p, nil
}

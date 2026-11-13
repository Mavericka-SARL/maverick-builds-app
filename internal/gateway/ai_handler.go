package gateway

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func (h *handler) aiChatStore(ctx context.Context) *aiassistant.ChatStore {
	return aiassistant.NewChatStore(h.db.For(ctx))
}

// providerEnvKeys maps each provider to the env var used as an API-key
// fallback when the user hasn't stored a key in Settings.
var providerEnvKeys = map[string]string{
	"openai":    "OPENAI_API_KEY",
	"anthropic": "ANTHROPIC_API_KEY",
	"mistral":   "MISTRAL_API_KEY",
	"deepseek":  "DEEPSEEK_API_KEY",
	"google":    "GEMINI_API_KEY",
}

// GeminiOpenAIBaseURL is Google's OpenAI-compatible Gemini API.
const GeminiOpenAIBaseURL = "https://generativelanguage.googleapis.com/v1beta/openai"

// providerDefaultModels is used when settings carry an empty model name. The
// model itself is free text in both settings screens — providers ship new
// models faster than a list here could follow — so these are only defaults,
// and a deployment replaces any of them with AI_DEFAULT_MODEL_<PROVIDER>
// (defaultModel).
var providerDefaultModels = map[string]string{
	// gpt-5-mini built the 13-sheet CPG workbook stage by stage with the
	// fewest developer corrections for its price (benchmark 2026-10-04:
	// gpt-5.2 and o4-mini also passed every stage; gpt-4.1, gpt-4.1-mini and
	// gpt-4o-mini did not finish).
	"openai":    "gpt-5-mini",
	"anthropic": "claude-opus-4-8",
	"mistral":   "mistral-large-latest",
	"deepseek":  "deepseek-chat",
	"google":    "gemini-3.8-flash",
}

// defaultModel is the model a provider runs when neither the person's nor
// the tenant's settings name one: AI_DEFAULT_MODEL_<PROVIDER> (for example
// AI_DEFAULT_MODEL_OPENAI) when the deployment sets it, else the built-in one.
func defaultModel(provider string) string {
	if v := strings.TrimSpace(os.Getenv("AI_DEFAULT_MODEL_" + strings.ToUpper(provider))); v != "" {
		return v
	}
	return providerDefaultModels[provider]
}

// defaultModels is defaultModel for every provider.
func defaultModels() map[string]string {
	out := make(map[string]string, len(providerDefaultModels))
	for p := range providerDefaultModels {
		out[p] = defaultModel(p)
	}
	return out
}

// LLM call caps (SOW Phase 4). Each Chat() invocation counts as one call,
// including intermediate read-tool round-trips within a single chat turn —
// enforced both before a turn starts and before each loop iteration inside it.
// AI_MAX_CALLS_PER_SESSION and AI_MAX_CALLS_PER_DAY set them per deployment.
// A session is its draft and goes on as one thread (owner, 2026-11-13): no
// per-session cap by default — the developer's own call budget, the daily
// cap and the token budget bound it; AI_MAX_CALLS_PER_SESSION sets one per
// deployment. Its history is settled as it grows (SettleHistory).
const (
	defaultLLMCallsPerSession    = 0
	defaultLLMCallsPerUserPerDay = 1000
)

// sessionCapMessage says what to do when a session has used its calls: its
// call budget is the developer's to raise; a deployment's limit is the
// administrator's. A session is its draft, so the work goes on here.
func sessionCapMessage(limit int, budget bool) string {
	if budget {
		return fmt.Sprintf("this session has used its call budget of %d model calls: raise or clear the budget to go on — the work stays in this session's draft", limit)
	}
	return fmt.Sprintf("this session has reached this deployment's limit of %d model calls (AI_MAX_CALLS_PER_SESSION): an administrator can raise it", limit)
}

// sessionLengthNote tells the model, once most of the session's call budget
// is used, to finish the part it is on and say how far the work got, at
// the end of a stage rather than in the middle of one. "" before that, and
// with no budget.
func sessionLengthNote(used, limit int) string {
	if limit <= 0 || used*10 < limit*7 {
		return ""
	}
	return fmt.Sprintf("\n\n## Call budget\nThis session has used %d of its %d-call budget. Finish the part you are on, then tell the "+
		"developer how far the work got and what remains; they can raise the budget to go on.", used, limit)
}

// llmCallCaps returns the per-session and per-user-per-day caps.
// sessionTokenLimit is AI_SESSION_TOKEN_LIMIT: the tokens a session may be
// billed for before its next message is refused; 0 (unset) is no budget.
func sessionTokenLimit() int64 {
	return int64(envPositiveInt("AI_SESSION_TOKEN_LIMIT", 0))
}

func llmCallCaps() (perSession, perDay int) {
	return envPositiveInt("AI_MAX_CALLS_PER_SESSION", defaultLLMCallsPerSession),
		envPositiveInt("AI_MAX_CALLS_PER_DAY", defaultLLMCallsPerUserPerDay)
}

// envPositiveInt reads a positive whole number from the environment, or
// returns def when the variable is unset or not one.
func envPositiveInt(name string, def int) int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil && n > 0 {
		return n
	}
	return def
}

// buildProvider constructs an LLM provider for the given (provider, apiKey).
// Shared by the chat path and the settings-test endpoint.
func buildProvider(provider, apiKey string) (providers.Provider, error) {
	switch provider {
	case "openai", "":
		return providers.NewOpenAI(apiKey), nil
	case "anthropic":
		return providers.NewAnthropic(apiKey), nil
	case "mistral":
		return providers.NewOpenAICompatible(apiKey, "https://api.mistral.ai/v1", "mistral"), nil
	case "deepseek":
		return providers.NewOpenAICompatible(apiKey, "https://api.deepseek.com/v1", "deepseek"), nil
	case "google":
		// Gemini's OpenAI-compatible endpoint: chat completions with tools.
		return providers.NewOpenAICompatible(apiKey, GeminiOpenAIBaseURL, "google"), nil
	default:
		return nil, fmt.Errorf("unknown provider %q — use openai, anthropic, google, mistral, or deepseek", provider)
	}
}

// buildProviderForRequest picks the key every AI call in this request uses.
//
// The order, highest first:
//
//  1. An ENFORCED tenant key (enterprise). The tenant admin has said this is
//     the only account model data may leave through, so a personal key is
//     deliberately ignored rather than merged.
//  2. The caller's own key, the only path community and commercial have.
//  3. A tenant key that is set but not enforced — a convenience so developers
//     in an enterprise tenant need not paste anything.
//  4. The provider's env var, the single-tenant/self-hosted fallback.
//
// A key always brings its own provider with it: the key belongs to that
// account, so using a tenant key means using the tenant's provider and model.
func (h *handler) buildProviderForRequest(r *http.Request, act *actor) (providers.Provider, string, string, error) {
	userID := act.UserID
	if h.testProvider != nil {
		return h.testProvider, "test", "test-model", nil
	}
	// The person's own settings and key are read where they live
	// (personalCtx); the tenant's key in the tenant's database.
	ctx := personalCtx(r.Context(), act)
	store := h.aiChatStore(ctx)
	settings, err := store.GetSettings(ctx, userID)
	if err != nil {
		return nil, "", "", fmt.Errorf("load llm settings: %w", err)
	}
	tenant := h.tenantAIKey(r.Context(), h.requestCustomerID(r.Context(), r, act))

	var provider, model, apiKey string
	switch {
	case tenant.OK && tenant.Enforced:
		provider, model, apiKey = tenant.Provider, tenant.Model, tenant.APIKey
	default:
		provider, model = settings.Provider, settings.Model
		apiKey, _ = store.GetDecryptedAPIKey(ctx, userID)
		if apiKey == "" && tenant.OK {
			provider, model, apiKey = tenant.Provider, tenant.Model, tenant.APIKey
		}
	}
	if provider == "" {
		provider = "openai"
	}
	envKey := providerEnvKeys[provider]
	if apiKey == "" && envKey != "" {
		apiKey = os.Getenv(envKey)
	}
	if apiKey == "" {
		return nil, "", "", fmt.Errorf("no API key configured for %s — set one in Settings or the %s env var", provider, envKey)
	}

	p, err := buildProvider(provider, apiKey)
	if err != nil {
		return nil, "", "", err
	}
	// The developer's reasoning effort, for their own provider's models.
	if provider == settings.Provider || (settings.Provider == "" && provider == "openai") {
		p = withEffort(p, settings.ReasoningEffort)
	}
	if model == "" {
		model = defaultModel(provider)
	}
	return p, provider, model, nil
}

// ── POST /api/ai/sessions ────────────────────────────────────────────────────

func (h *handler) aiCreateSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}

	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	var appID string
	_ = h.db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID)

	store := h.aiChatStore(ctx)
	settings, _ := h.aiChatStore(personalCtx(ctx, a)).GetSettings(personalCtx(ctx, a), a.UserID)

	sess, err := store.CreateSession(ctx, appID, modelID, a.UserID, settings.Provider, settings.Model)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	// A session is its draft (owner, 2026-11-13): a new session starts a
	// fresh draft from the developer's working revision on its first
	// confirmed proposal, and goes on as one thread with no session cap.
	// Continuing earlier work is continuing its session.
	// A plan whose session's draft became the working revision goes on here.
	if _, err := store.CarryTaskPlan(ctx, sess.ID, modelID, a.UserID, sess.DraftRevisionID); err != nil {
		log.Printf("AI session %s: carry the task plan: %v", sess.ID, err)
	}
	jsonOK(w, sess)
}

// ── GET /api/ai/sessions ────────────────────────────────────────────────────

func (h *handler) aiListSessions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}

	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	sessions, err := h.aiChatStore(ctx).ListSessions(ctx, modelID, a.UserID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if sessions == nil {
		sessions = []aiassistant.Session{}
	}
	jsonOK(w, sessions)
}

// ── GET /api/ai/sessions/{id} ────────────────────────────────────────────────

func (h *handler) aiGetSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	sessionID := strings.TrimPrefix(r.URL.Path, "/api/ai/sessions/")
	// Strip /messages suffix if caller hits the message endpoint path accidentally.
	sessionID = strings.TrimSuffix(sessionID, "/messages")
	if sessionID == "" {
		jsonErr(w, fmt.Errorf("session id required"), http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	store := h.aiChatStore(ctx)

	sess, err := store.GetSession(ctx, sessionID)
	if err != nil || sess.UserID != a.UserID {
		jsonErr(w, fmt.Errorf("session not found"), http.StatusNotFound)
		return
	}
	msgs, err := store.ListMessages(ctx, sessionID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if msgs == nil {
		msgs = []aiassistant.ChatMessage{}
	}
	docs, _ := aiassistant.NewDocumentStore(h.db.For(ctx)).ListDocuments(ctx, sessionID)
	if docs == nil {
		docs = []aiassistant.Document{}
	}
	// What the session's calls were billed for (migration 160).
	usage, _ := store.Usage(ctx, sessionID)
	plan, _ := store.TaskPlan(ctx, sessionID)
	jsonOK(w, map[string]any{"session": sess, "messages": msgs, "documents": docs, "usage": withCost(usage, sess.LLMModel), "task_plan": plan})
}

// ── POST /api/ai/sessions/{id}/messages ─────────────────────────────────────

type aiMessageReq struct {
	Content string `json:"content"`
}

// toolCallSignature canonicalizes one LLM turn's tool calls (names + raw
// arguments) so consecutive identical turns can be detected — the repeat
// breaker's key. Argument bytes are compared verbatim: if the model varies
// them at all it is doing something new and the loop continues.
func toolCallSignature(calls []providers.ToolCall) string {
	var b strings.Builder
	for _, tc := range calls {
		b.WriteString(tc.Name)
		b.WriteByte('(')
		b.Write(tc.Arguments)
		b.WriteString(");")
	}
	return b.String()
}

func (h *handler) aiSendMessage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}

	// Extract session ID from /api/ai/sessions/{id}/messages
	path := strings.TrimPrefix(r.URL.Path, "/api/ai/sessions/")
	sessionID := strings.TrimSuffix(path, "/messages")
	if sessionID == "" || sessionID == path {
		jsonErr(w, fmt.Errorf("invalid path"), http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}

	var req aiMessageReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Content) == "" {
		jsonErr(w, fmt.Errorf("content is required"), http.StatusBadRequest)
		return
	}

	store := h.aiChatStore(ctx)

	sess, err := store.GetSession(ctx, sessionID)
	if err != nil || sess.UserID != a.UserID {
		jsonErr(w, fmt.Errorf("session not found"), http.StatusNotFound)
		return
	}
	if sess.PromotedAt != nil {
		jsonCodeErr(w, http.StatusConflict, "SESSION_PROMOTED", sessionPromotedError)
		return
	}

	// Rate limits (SOW Phase 4): checked up front so an already-exhausted
	// caller fails before we save their message or spend a provider round-trip.
	// sessionCalls/dailyCalls are then tracked in-memory and re-checked before
	// every Chat() invocation inside the tool-call loop below, since a single
	// turn can make several calls (one per read-tool round-trip).
	sessionCalls, err := store.CountLLMCallsInSession(ctx, sessionID)
	if err != nil {
		jsonErr(w, fmt.Errorf("check session rate limit: %w", err), http.StatusInternalServerError)
		return
	}
	dailyCalls, err := store.CountLLMCallsToday(ctx, a.UserID)
	if err != nil {
		jsonErr(w, fmt.Errorf("check daily rate limit: %w", err), http.StatusInternalServerError)
		return
	}
	maxLLMCallsPerSession, maxLLMCallsPerUserPerDay := llmCallCaps()
	// The developer's own budget for this session, when they set one (0 =
	// no per-session cap).
	byBudget := false
	if sess.CallBudget != nil && *sess.CallBudget > 0 && (maxLLMCallsPerSession <= 0 || *sess.CallBudget < maxLLMCallsPerSession) {
		maxLLMCallsPerSession, byBudget = *sess.CallBudget, true
	}
	if maxLLMCallsPerSession > 0 && sessionCalls >= maxLLMCallsPerSession {
		jsonErr(w, fmt.Errorf("%s", sessionCapMessage(maxLLMCallsPerSession, byBudget)), http.StatusTooManyRequests)
		return
	}
	if dailyCalls >= maxLLMCallsPerUserPerDay {
		jsonErr(w, fmt.Errorf("you've reached today's limit of %d LLM calls — try again tomorrow", maxLLMCallsPerUserPerDay), http.StatusTooManyRequests)
		return
	}
	// A session's token budget (AI_SESSION_TOKEN_LIMIT; none by default):
	// what its calls were billed for, input, cached and output together.
	if limit := sessionTokenLimit(); limit > 0 {
		if u, uErr := store.Usage(ctx, sessionID); uErr == nil && u.Total() >= limit {
			jsonErr(w, fmt.Errorf("this session has used %d tokens, its budget of %d: start a new session to go on", u.Total(), limit), http.StatusTooManyRequests)
			return
		}
	}
	// The tenant's plan may cap AI messages per day as well (plan.go).
	if cid := h.requestCustomerID(ctx, r, a); cid != "" && h.plans != nil {
		if err := h.plans.CheckAIMessages(ctx, h.db.For(ctx), cid); err != nil {
			h.jsonLimitErr(w, err)
			return
		}
	}

	// Build the LLM provider.
	llmProvider, _, llmModel, err := h.buildProviderForRequest(r, a)
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	log.Printf("AI chat: model=%q userID=%s", llmModel, a.UserID)

	// Resolve model and revision for tool execution and system prompt. Once
	// this session has a draft revision (see aiConfirmProposal), reads and
	// context here must reflect it too — otherwise the assistant would answer
	// questions using the stale active revision while its own writes land in
	// the draft.
	modelID, mErr := h.resolveDemoModelID(ctx, r)
	if mErr != nil {
		jsonAccessErr(w, mErr, "resolve model")
		return
	}
	// revision_id arrives from the caller, so it is confirmed to belong to the
	// model resolved above before anything is scoped to it. Without that the
	// assistant is pointed at one model for access purposes and a different
	// one for work: update_workflow_def and update_form_def both gate on
	// "does this resource live in e.revID", a check worth nothing if e.revID
	// itself can name another tenant's revision.
	revID := r.URL.Query().Get("revision_id")
	if revID != "" && !h.revisionBelongsToModel(ctx, revID, modelID) {
		jsonErr(w, fmt.Errorf("revision does not belong to this model"), http.StatusForbidden)
		return
	}
	if err := h.aiSessionInModel(ctx, sess, modelID); err != nil {
		jsonErr(w, err, http.StatusConflict)
		return
	}
	// The draft outranks the console's revision: the session's writes land
	// there.
	if sess.DraftRevisionID != "" {
		revID = sess.DraftRevisionID
	}
	if revID == "" {
		_ = h.db.QueryRow(ctx, `
			SELECT COALESCE(active_revision_id::text,'') FROM core.model WHERE id=$1::uuid`, modelID,
		).Scan(&revID)
	}

	// 1. Save the user's message.
	_, err = store.SaveMessage(ctx, sessionID, "user", req.Content, nil, "", "")
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}

	// Auto-name the session from its first request (like chat products do):
	// best-effort and in the background — the reply must not wait on it, and
	// a failure just leaves the date fallback. SetTitleIfEmpty means a user
	// rename (or an earlier generation) is never overwritten. The tiny title
	// call is deliberately not counted against the session/daily LLM limits.
	if sess.Title == "" {
		go h.aiAutoNameSession(ctx, sessionID, req.Content, llmProvider, llmModel)
	}

	// 2. Build history for the LLM call.
	history, err := store.ListMessages(ctx, sessionID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}

	// 3. Build system prompt with live model context + any uploaded documents.
	mc := aiassistant.FetchModelContext(ctx, h.db.For(ctx), modelID, revID)
	// The message read as a direct order, a delegated goal or a question,
	// with the playbooks of its tasks and the session's plan (ai_task.go).
	taskPlan, _ := store.TaskPlan(ctx, sessionID)
	// A turn building one sheet of a target model gets only what that needs
	// (aiassistant/prompt_trim.go): every call re-sent ~110k tokens on
	// 2026-11-12, 120k characters of them instructions for workflows,
	// connectors, exports and pages, 60k the workbook's text.
	sheetTurn := false
	if cur := taskPlan.Current(); cur >= 0 && taskPlan.Target != nil && len(taskPlan.Stages[cur].StageSheets()) > 0 {
		sheetTurn = true
	}
	systemPrompt := aiassistant.BuildSystemPrompt(mc)
	if sheetTurn {
		systemPrompt = aiassistant.TrimPromptForSheet(systemPrompt)
	}
	if docs, dErr := aiassistant.NewDocumentStore(h.db.For(ctx)).ListDocumentsWithContent(ctx, sessionID); dErr == nil {
		if taskPlan != nil && taskPlan.Target != nil {
			// The target model holds what the workbook says; the AI reads a
			// sheet when it needs one (read_attached_sheet, plan_sheet).
			systemPrompt += buildDocumentIndex(docs, h.sessionWorkbooks(ctx, sessionID))
		} else {
			systemPrompt += buildDocumentContext(docs)
		}
	}
	// Carried context made right (ai_plan_carry.go): the developer's answer
	// to the AI's question is kept as a decision, and a carried plan's open
	// stages are checked against the model once.
	taskPlan = recordDecision(ctx, store, sessionID, taskPlan, history, req.Content)
	h.checkCarriedPlan(ctx, store, llmProvider, llmModel, sessionID, revID, taskPlan)
	// State proven from the model: a sheet's stage whose rows the workbook
	// map shows matching is done, whatever any call said (ai_task.go).
	taskPlan = h.closeProvenStages(ctx, store, sessionID, modelID, revID, taskPlan)
	// What the turn takes goes to the stage it works on (owner, 2026-11-12:
	// minutes, calls, tokens, tool calls and corrections per stage).
	defer h.measureTurn(ctx, store, sessionID, taskPlan, req.Content)()
	systemPrompt += h.targetProgress(ctx, modelID, revID, taskPlan)
	reqMode := classifyRequest(ctx, llmProvider, llmModel, req.Content, taskPlan, store, sessionID)
	log.Printf("AI request: session=%s mode=%s tasks=%v", sessionID, reqMode.Mode, reqMode.Tasks)
	systemPrompt += "\n\n" + aiassistant.TurnBrief(reqMode, taskPlan)
	// The workbook map: which row became which metric, what differs and what
	// is not mapped yet (ai_workbook_map.go).
	systemPrompt += h.workbookMapBrief(ctx, sessionID, modelID, revID)
	systemPrompt += sessionLengthNote(sessionCalls, maxLLMCallsPerSession)

	// 4. Tool executors.
	readExecutor := aiassistant.NewToolExecutor(h.db.For(ctx), modelID, revID).WithReadHooks(h.aiReadHooks(r, sessionID, modelID, a.UserID))
	writeExecutor := aiassistant.NewWriteExecutor(h.db.For(ctx), modelID, revID)
	planHooks := taskHooks{
		Proof:    func(st aiassistant.TaskStage) string { return h.stageProofFor(ctx, sessionID, modelID, revID, st) },
		Sheets:   func() []string { return h.attachedSheetNames(ctx, sessionID) },
		Compared: func(sheets []string) string { return h.comparedFor(ctx, modelID, revID, sheets) },
	}
	proposalStore := aiassistant.NewProposalStore(h.db.For(ctx))
	tools := aiassistant.TurnTools(reqMode, req.Content)
	if sheetTurn && reqMode.Mode != aiassistant.ModeQuestion {
		tools = aiassistant.SheetStageTools(tools)
	}

	// 5. From here on, respond over SSE so the frontend can render the final
	//    reply token-by-token instead of waiting for the whole thing. Every
	//    failure past this point (including rate limits hit mid-loop) is an
	//    "error" event rather than a jsonErr response — once headers are sent
	//    the HTTP status can no longer change.
	flusher, ok := w.(http.Flusher)
	if !ok {
		jsonErr(w, fmt.Errorf("streaming not supported"), http.StatusInternalServerError)
		return
	}
	// One turn runs several LLM calls, previews and plan checks; the
	// server's 120 s WriteTimeout cut longer turns off mid-stream and the
	// request context's cancel threw the turn's work away.
	extendAIWriteDeadline(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	sendSSE := func(event string, payload any) {
		b, mErr := json.Marshal(payload)
		if mErr != nil {
			return
		}
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
		flusher.Flush()
	}

	// Loop to handle read tool calls. propose_actions (write gateway) breaks
	// the loop and sends a "proposal" event to the frontend.
	// Tool exchanges older than the last confirmed plan are settled: the
	// plan, its decisions and the workbook map carry what they found.
	var settled time.Time
	if props, pErr := aiassistant.NewProposalStore(h.db.For(ctx)).ListProposals(ctx, sessionID); pErr == nil {
		for _, p := range props {
			if p.ExecutedAt != nil && p.ExecutedAt.After(settled) {
				settled = *p.ExecutedAt
			}
		}
	}
	provMessages := aiassistant.MessagesToProviderHistory(aiassistant.CompactHistory(aiassistant.SettleHistory(history, settled)))
	var finalReply string

	// Repeat-call breaker: gpt-4o-mini has been observed calling the SAME
	// read tool with the SAME arguments dozens of times in a row (49×
	// list_workflows, live), burning the entire 50-call session budget in
	// one turn. Three identical consecutive calls means the model is stuck,
	// not exploring — abort with a clear error instead of paying for the
	// rest of the loop.
	const maxIdenticalCalls = 3
	var lastCallSig string
	identicalCalls := 0
	var retries planRetries // plans sent back by aiCheckProposal this turn

	// The turn's time limit, and Stop: the developer's console closes the
	// request, which cancels ctx. Messages about either are saved on a
	// context that outlives them.
	turnLimit := aiTurnTimeout()
	turnCtx, cancelTurn := context.WithTimeoutCause(ctx, turnLimit, errTurnTimeLimit)
	defer cancelTurn()
	saveCtx := context.WithoutCancel(ctx)
	stopTurn := func() bool {
		msg := stoppedTurn(turnCtx, turnLimit)
		if msg == "" {
			return false
		}
		_, _ = store.SaveMessage(saveCtx, sessionID, "assistant", msg, nil, "", "")
		sendSSE("error", map[string]string{"error": msg})
		return true
	}
	progress := &toolProgress{send: sendSSE}
	// warnedPlan is the last plan that ran but went back for its warnings.
	// A model asking "shall I propose it?" instead of proposing it again lost
	// a plan that works (the HR rebuild's monthly plan, sent back once for a
	// harmless SUMIFS); the turn ends showing it with those warnings instead.
	var warnedPlan []aiassistant.ProposalStep
	var warnedWarnings []string
	// After a confirmed plan, the turn starts with the server's own value
	// check of it (ai_build_guards.go): shown to the developer, and given to
	// the model with the developer's message.
	if note := h.stageValueCheck(turnCtx, store, proposalStore, readExecutor, sessionID, revID, history, taskPlan,
		func(s string) { sendSSE("tool_status", map[string]string{"tool": s}) }); note != "" {
		_, _ = store.SaveMessage(saveCtx, sessionID, "assistant", note, nil, "", "")
		if n := len(provMessages); n > 0 && provMessages[n-1].Role == "user" {
			provMessages[n-1].Content += "\n\n" + note
		} else {
			provMessages = append(provMessages, providers.Message{Role: "user", Content: note})
		}
	}
	// And with the results of the plans confirmed since the last reply: blank
	// metrics, failing calculations, IFERROR fallbacks (ai_after_confirm.go).
	if note := h.afterConfirmCheck(turnCtx, proposalStore, sessionID, modelID, revID, history,
		func(s string) { sendSSE("tool_status", map[string]string{"tool": s}) }); note != "" {
		_, _ = store.SaveMessage(saveCtx, sessionID, "assistant", note, nil, "", "")
		if n := len(provMessages); n > 0 && provMessages[n-1].Role == "user" {
			provMessages[n-1].Content += "\n\n" + note
		} else {
			provMessages = append(provMessages, providers.Message{Role: "user", Content: note})
		}
	}
	// stageChecked: the turn's coverage check answered for the task plan's
	// current stage; stageDoes are the stage's open items the plan does,
	// stageOpen the ones it leaves for later plans of the stage.
	stageChecked := false
	var stageDoes, stageOpen []string
	// sentBack: a plan of this turn went back to be fixed. A model that then
	// ends the turn asking leave to propose ("confirm and I will post it" —
	// gpt-5-mini did, stage after stage) is told once to propose instead.
	sendSSE("tool_status", map[string]string{"tool": modeStatus(reqMode)})
	sentBack, nudges := false, 0
	// doneChecked: a "done" claim was checked against the comparisons -- once a
	// turn; the model's answer to the check ends it.
	doneChecked := false
	// emptyNudged: an empty reply went back once; unreadable: propose_actions
	// calls whose arguments were not a plan, sent back to the model.
	emptyNudged, unreadable := false, 0
	// answerCalls saves msg and answers each of its tool calls: a write tool
	// with writeResult, a read tool by running it (a follow-up turn with an
	// unanswered call is refused by the provider).
	answerCalls := func(msg providers.Message, writeResult string) {
		_, _ = store.SaveMessage(ctx, sessionID, "assistant", msg.Content, msg.ToolCalls, "", "")
		provMessages = append(provMessages, msg)
		for _, call := range msg.ToolCalls {
			result := writeResult
			if !aiassistant.IsWriteTool(call.Name) {
				var execErr error
				if result, execErr = runTool(ctx, store, sessionID, planHooks, readExecutor, call.Name, call.Arguments); execErr != nil {
					result = fmt.Sprintf("error: %v", execErr)
				}
			}
			_, _ = store.SaveMessage(ctx, sessionID, "tool", result, nil, call.ID, call.Name)
			provMessages = append(provMessages, providers.Message{Role: "tool", Content: result, ToolCallID: call.ID, ToolName: call.Name})
		}
	}
	// unreadablePlan sends a plan that did not arrive back to the model, or
	// ends the turn once that happened maxUnreadablePlans times.
	unreadablePlan := func(msg providers.Message, result string) bool {
		unreadable++
		if unreadable > maxUnreadablePlans {
			_, _ = store.SaveMessage(ctx, sessionID, "assistant", fmt.Sprintf(
				"⚠️ Stopped: the plan did not arrive whole %d times in this turn. Ask for a smaller part of the work.", unreadable), nil, "", "")
			sendSSE("error", map[string]string{"error": "propose_actions: the plan did not arrive whole — ask for a smaller part of the work"})
			return false
		}
		answerCalls(msg, result)
		sendSSE("tool_status", map[string]string{"tool": "propose_actions"})
		return true
	}
	// coverageChecked: the turn's first plan that runs was checked against
	// the request (ai_coverage.go); later ones are its corrections.
	coverageChecked := false

turn:
	for {
		if stopTurn() {
			return
		}
		if maxLLMCallsPerSession > 0 && sessionCalls >= maxLLMCallsPerSession {
			sendSSE("error", map[string]string{"error": sessionCapMessage(maxLLMCallsPerSession, byBudget)})
			return
		}
		if dailyCalls >= maxLLMCallsPerUserPerDay {
			sendSSE("error", map[string]string{"error": fmt.Sprintf("you've reached today's limit of %d LLM calls — try again tomorrow", maxLLMCallsPerUserPerDay)})
			return
		}
		sessionCalls++
		dailyCalls++

		chatReq := providers.ChatRequest{
			Model:        llmModel,
			SystemPrompt: systemPrompt,
			Messages:     provMessages,
			Tools:        tools,
			OnToolArgs:   progress.onArgs,
		}
		// A call that sends nothing for aiCallIdleLimit, runs past
		// aiCallMaxDuration or loses its connection is stopped and asked
		// once more; the second such end finishes the turn with an error.
		var llmResp providers.ChatResponse
		for attempt := 1; ; attempt++ {
			var stalled bool
			callStart := time.Now()
			stalled, err = watchedAICall(turnCtx, aiCallIdleLimit(), aiCallMaxDuration(), func(callCtx context.Context, touch func()) error {
				req := chatReq
				req.OnToolArgs = func(name string, args []byte) {
					touch()
					if chatReq.OnToolArgs != nil {
						chatReq.OnToolArgs(name, args)
					}
				}
				var cErr error
				if sp, streamable := llmProvider.(providers.StreamingProvider); streamable {
					llmResp, cErr = sp.ChatStream(callCtx, req, func(delta string) {
						touch()
						sendSSE("delta", map[string]string{"content": delta})
					})
				} else {
					llmResp, cErr = llmProvider.Chat(callCtx, req)
				}
				return cErr
			})
			took := time.Since(callStart).Round(time.Second)
			if took > time.Minute {
				log.Printf("AI call: session=%s main call took %s (attempt %d, err=%v)", sessionID, took, attempt, err)
			}
			dropped := !stalled && turnCtx.Err() == nil && transientAICallError(err)
			if !stalled && !dropped {
				break
			}
			why := fmt.Sprintf("sent nothing for %s", aiCallIdleLimit())
			switch {
			case dropped:
				why = "dropped the connection"
			case took >= aiCallMaxDuration():
				why = fmt.Sprintf("did not finish in %s", aiCallMaxDuration())
			}
			if attempt == 2 {
				err = fmt.Errorf("the AI provider %s, twice: %w", why, err)
				break
			}
			log.Printf("AI call: session=%s %s after %s, asked again", sessionID, why, took)
			sendSSE("tool_status", map[string]string{"tool": "the AI provider " + why + " — asking again"})
		}
		if err != nil && stopTurn() {
			return
		}
		if err != nil {
			_, _ = store.SaveMessage(ctx, sessionID, "assistant",
				fmt.Sprintf("⚠️ LLM error: %v", err), nil, "", "")
			sendSSE("error", map[string]string{"error": fmt.Sprintf("LLM request failed: %v", err)})
			return
		}
		if u := llmResp.Usage; u.InputTokens+u.CacheReadTokens+u.CacheWriteTokens > 0 {
			log.Printf("AI call: session=%s input=%d cache_read=%d cache_write=%d output=%d",
				sessionID, u.InputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.OutputTokens)
		}
		// Every call is kept with the session, tokens or not: the session's
		// call cap and a build's cost are read from it.
		if uErr := store.AddUsage(context.WithoutCancel(ctx), sessionID, llmResp.Usage); uErr != nil {
			log.Printf("AI call: session=%s record usage: %v", sessionID, uErr)
		}

		// A reply cut off at the output limit while writing a tool call: its
		// arguments are incomplete, so nothing in it can run.
		if llmResp.FinishReason == "length" && len(llmResp.Message.ToolCalls) > 0 {
			chars := 0
			for _, tc := range llmResp.Message.ToolCalls {
				chars += len(tc.Arguments)
			}
			if !unreadablePlan(llmResp.Message, unreadablePlanResult(chars, true, nil)) {
				return
			}
			continue
		}
		if llmResp.FinishReason != "tool_calls" || len(llmResp.Message.ToolCalls) == 0 {
			if !emptyNudged && strings.TrimSpace(llmResp.Message.Content) == "" {
				emptyNudged = true
				provMessages = append(provMessages, providers.Message{Role: "user", Content: emptyReplyNudge})
				continue
			}
			// How to split the work into proposals is the server's to
			// answer, once and for the whole build: "as many as it needs"
			// (2026-11-12: "A) two proposals or B) three or more?" ended
			// turns as a choice for the developer).
			if nudges < maxTurnNudges && asksHowToSplit(llmResp.Message.Content) {
				nudges++
				if plan, _ := store.TaskPlan(ctx, sessionID); plan != nil {
					if plan.AddDecision(splitDecision) {
						_ = store.SetTaskPlan(ctx, sessionID, plan)
					}
				} else if plan := (&aiassistant.TaskPlan{}); plan.AddDecision(splitDecision) {
					_ = store.SetTaskPlan(ctx, sessionID, plan)
				}
				_, _ = store.SaveMessage(ctx, sessionID, "assistant", llmResp.Message.Content, nil, "", "")
				provMessages = append(provMessages, llmResp.Message, providers.Message{Role: "user", Content: splitNudge})
				continue
			}
			// Asking leave to propose, or to confirm defaults it named, when
			// the developer asked for a change: propose instead (ai_turn.go).
			if nudges < maxTurnNudges && !strings.HasSuffix(strings.TrimSpace(req.Content), "?") && asksLeaveToPropose(llmResp.Message.Content) {
				nudges++
				_, _ = store.SaveMessage(ctx, sessionID, "assistant", llmResp.Message.Content, nil, "", "")
				provMessages = append(provMessages, llmResp.Message, providers.Message{Role: "user", Content: leaveNudge})
				continue
			}
			if sentBack && nudges < maxTurnNudges && asksToPropose(llmResp.Message.Content) {
				nudges++
				_, _ = store.SaveMessage(ctx, sessionID, "assistant", llmResp.Message.Content, nil, "", "")
				provMessages = append(provMessages, llmResp.Message, providers.Message{Role: "user", Content: proposeNudge})
				continue
			}
			// A permission question with no choice in it is answered by the
			// server: the developer's request is the leave (2026-11-11).
			if nudges < maxTurnNudges && reqMode.Mode != aiassistant.ModeQuestion && !strings.HasSuffix(strings.TrimSpace(req.Content), "?") && asksPermission(llmResp.Message.Content) {
				nudges++
				_, _ = store.SaveMessage(ctx, sessionID, "assistant", llmResp.Message.Content, nil, "", "")
				provMessages = append(provMessages, llmResp.Message, providers.Message{Role: "user", Content: permissionNudge})
				continue
			}
			if nudges < maxTurnNudges && !strings.HasSuffix(strings.TrimSpace(req.Content), "?") && pointsAtPlan.MatchString(plainApostrophes(llmResp.Message.Content)) {
				if pending, _ := proposalStore.ListPendingProposals(ctx, sessionID); len(pending) == 0 {
					nudges++
					_, _ = store.SaveMessage(ctx, sessionID, "assistant", llmResp.Message.Content, nil, "", "")
					provMessages = append(provMessages, llmResp.Message, providers.Message{Role: "user", Content: unproposedNudge})
					continue
				}
			}
			if nudges < maxTurnNudges && !strings.HasSuffix(strings.TrimSpace(req.Content), "?") && claimsUnproposedChange(llmResp.Message.Content) {
				nudges++
				_, _ = store.SaveMessage(ctx, sessionID, "assistant", llmResp.Message.Content, nil, "", "")
				provMessages = append(provMessages, llmResp.Message, providers.Message{Role: "user", Content: unproposedNudge})
				continue
			}
			// In a delegated build under a task plan, a turn ending on the
			// work it will do next ("I'm correcting that now", "I need to
			// resubmit this as a clean bundle", "I'll continue from there")
			// with nothing proposed goes back once (2026-11-12: a dozen
			// such turns of 1–3 tool calls).
			if nudges < maxTurnNudges && reqMode.Mode != aiassistant.ModeQuestion && !strings.HasSuffix(strings.TrimSpace(req.Content), "?") &&
				announcesOwnWork(llmResp.Message.Content) {
				if plan, _ := store.TaskPlan(ctx, sessionID); plan.Current() >= 0 {
					if pending, _ := proposalStore.ListPendingProposals(ctx, sessionID); len(pending) == 0 {
						nudges++
						_, _ = store.SaveMessage(ctx, sessionID, "assistant", llmResp.Message.Content, nil, "", "")
						provMessages = append(provMessages, llmResp.Message, providers.Message{Role: "user", Content: unproposedNudge})
						continue
					}
				}
			}
			// A sheet called done is checked against the session's value
			// comparisons first (ai_sheet_check.go).
			if nudges < maxTurnNudges && !doneChecked {
				if nudge := h.pagesDoneNudge(ctx, sessionID, revID, llmResp.Message.Content); nudge != "" {
					nudges++
					doneChecked = true
					_, _ = store.SaveMessage(ctx, sessionID, "assistant", llmResp.Message.Content, nil, "", "")
					provMessages = append(provMessages, llmResp.Message, providers.Message{Role: "user", Content: nudge})
					continue
				}
			}
			if nudges < maxTurnNudges && !doneChecked {
				if nudge := h.sheetDoneNudge(ctx, store, sessionID, llmResp.Message.Content); nudge != "" {
					nudges++
					doneChecked = true
					_, _ = store.SaveMessage(ctx, sessionID, "assistant", llmResp.Message.Content, nil, "", "")
					provMessages = append(provMessages, llmResp.Message, providers.Message{Role: "user", Content: nudge})
					continue
				}
			}
			finalReply = llmResp.Message.Content
			break
		}

		// Repeat-call breaker (see declaration above the loop).
		if sig := toolCallSignature(llmResp.Message.ToolCalls); sig == lastCallSig {
			identicalCalls++
			if identicalCalls >= maxIdenticalCalls {
				_, _ = store.SaveMessage(ctx, sessionID, "assistant",
					"⚠️ Stopped: the model repeated the same tool call "+fmt.Sprintf("%d", identicalCalls)+" times in a row without making progress. Try rephrasing your request.", nil, "", "")
				sendSSE("error", map[string]string{"error": "the model kept repeating the same tool call without making progress — try rephrasing your request"})
				return
			}
		} else {
			lastCallSig = sig
			identicalCalls = 1
		}

		// Oversized-proposal guard: a plan above maxProposalSteps is rejected
		// BEFORE it becomes a proposal, and the constraint is fed back as the
		// tool result so the model immediately re-proposes in batches. This is
		// server-enforced batching — a 500-step bulk edit (e.g. re-parenting
		// every product by a property) otherwise rides on the model's output
		// limit and truncates mid-JSON on smaller models, and even when it
		// fits it's a single unreviewable blob for the developer. Every
		// sibling tool call in the same message still gets a real result
		// (providers reject a follow-up turn with unanswered tool calls).
		const maxProposalSteps = aiassistant.MaxProposalSteps
		oversized := 0
		for _, tc := range llmResp.Message.ToolCalls {
			if !aiassistant.IsWriteTool(tc.Name) {
				continue
			}
			var probe struct {
				Steps []json.RawMessage `json:"steps"`
			}
			if json.Unmarshal(tc.Arguments, &probe) == nil && len(probe.Steps) > maxProposalSteps {
				oversized = len(probe.Steps)
			}
		}
		if oversized > 0 {
			_, _ = store.SaveMessage(ctx, sessionID, "assistant", llmResp.Message.Content,
				llmResp.Message.ToolCalls, "", "")
			provMessages = append(provMessages, llmResp.Message)
			for _, tc := range llmResp.Message.ToolCalls {
				var result string
				if aiassistant.IsWriteTool(tc.Name) {
					result = fmt.Sprintf(
						"Proposal rejected: %d steps exceeds the maximum of %d per proposal. Call propose_actions again with only the FIRST %d steps (keep their order). After the developer confirms and they execute, continue with the next batch of up to %d — and tell the developer how many steps remain overall.",
						oversized, maxProposalSteps, maxProposalSteps, maxProposalSteps)
				} else {
					var execErr error
					result, execErr = runTool(ctx, store, sessionID, planHooks, readExecutor, tc.Name, tc.Arguments)
					if execErr != nil {
						result = fmt.Sprintf("error: %v", execErr)
					}
				}
				_, _ = store.SaveMessage(ctx, sessionID, "tool", result, nil, tc.ID, tc.Name)
				provMessages = append(provMessages, providers.Message{
					Role: "tool", Content: result, ToolCallID: tc.ID, ToolName: tc.Name,
				})
			}
			sendSSE("tool_status", map[string]string{"tool": "propose_actions"})
			continue
		}

		// Check whether the LLM called propose_actions (write gateway).
		for _, tc := range llmResp.Message.ToolCalls {
			if !aiassistant.IsWriteTool(tc.Name) {
				continue
			}
			// Parse propose_actions args into proposal steps.
			var args struct {
				Steps []aiassistant.ProposalStep `json:"steps"`
			}
			if err := json.Unmarshal(tc.Arguments, &args); err != nil || len(args.Steps) == 0 {
				if !unreadablePlan(llmResp.Message, unreadablePlanResult(len(tc.Arguments), false, err)) {
					return
				}
				continue turn
			}
			// A sheet_plan step is the server's plan of a sheet (ai_sheet_plan.go).
			if expanded, xErr := h.expandSheetPlans(ctx, sessionID, revID, args.Steps); xErr != nil {
				answerCalls(llmResp.Message, "Proposal NOT shown to the developer: "+xErr.Error())
				sentBack = true
				continue turn
			} else {
				args.Steps = expanded
			}
			// Each metric before what names it, so a plan written in reading
			// order is checked, shown and run in an order that works.
			args.Steps = aiassistant.OrderByDependencies(args.Steps)
			// Run the plan as confirming would (aiCheckProposal); one that
			// would fail goes back to the model with its errors, not to the
			// developer. A check that cannot run lets the plan through:
			// confirming still stops at the first failing step.
			sendSSE("tool_status", map[string]string{"tool": "checking the plan"})
			check, cErr := h.aiCheckProposal(turnCtx, sessionID, modelID, revID, a.UserID, args.Steps)
			if stopTurn() {
				return
			}
			if cErr == nil {
				// The plan as the server left it: layout steps that fail
				// left out, missing grid placements added (proposalCheck).
				args.Steps = check.planSteps(args.Steps)
				check.warnings = append(check.warnings, inputOverwriteWarnings(turnCtx, h.db.For(ctx), modelID, revID, args.Steps, req.Content)...)
				if len(h.attachedSheetNames(ctx, sessionID)) > 0 {
					check.warnings = append(check.warnings, typedWorkbookValueWarnings(args.Steps, req.Content, workbookNumbers(h.sessionWorkbooks(ctx, sessionID)))...)
				}
				check.warnings = append(check.warnings, calcToInputWarnings(turnCtx, h.db.For(ctx), modelID, revID, args.Steps, req.Content)...)
				if probs, failed := formulaReplacedProblems(turnCtx, h.db.For(ctx), modelID, revID, args.Steps, req.Content); len(probs) > 0 {
					check.problems = append(check.problems, probs...)
					if check.failed == nil {
						check.failed = map[int]bool{}
					}
					for _, i := range failed {
						check.failed[i] = true
					}
				}
				if docs, dErr := aiassistant.NewDocumentStore(h.db.For(ctx)).ListDocuments(turnCtx, sessionID); dErr == nil {
					names := make([]string, 0, len(docs))
					for _, d := range docs {
						names = append(names, d.Filename)
					}
					if probs, failed := missingFileProblems(args.Steps, req.Content, names); len(probs) > 0 {
						check.problems = append(check.problems, probs...)
						if check.failed == nil {
							check.failed = map[int]bool{}
						}
						for _, i := range failed {
							check.failed[i] = true
						}
					}
				}
				if probs, failed := sourceFilterProblems(args.Steps, req.Content); len(probs) > 0 {
					check.problems = append(check.problems, probs...)
					if check.failed == nil {
						check.failed = map[int]bool{}
					}
					for _, i := range failed {
						check.failed[i] = true
					}
				}
				if probs, failed := typedFileProblems(args.Steps, req.Content, h.aiTypedFiles(ctx, sessionID)); len(probs) > 0 {
					check.problems = append(check.problems, probs...)
					if check.failed == nil {
						check.failed = map[int]bool{}
					}
					for _, i := range failed {
						check.failed[i] = true
					}
				}
			}
			// A plan that runs goes back once more when it leaves out part
			// of a request naming several things (ai_coverage.go).
			if cErr == nil && len(check.problems) == 0 && !coverageChecked {
				coverageChecked = true
				plan, _ := store.TaskPlan(ctx, sessionID)
				var stageItems []string
				if cur := plan.Current(); cur >= 0 && reqMode.Mode == aiassistant.ModeDelegated {
					// Only what earlier plans of the stage left open.
					stageItems = plan.Stages[cur].OpenItems()
				}
				missing, extra, usage := planCoverage(turnCtx, llmProvider, llmModel, req.Content, stageItems, reqMode.Mode == aiassistant.ModeDirect, args.Steps,
					modelInventory(turnCtx, h.db.For(ctx), revID))
				if usage.InputTokens+usage.CacheReadTokens > 0 {
					_ = store.AddUsage(context.WithoutCancel(ctx), sessionID, usage)
				}
				// A stage is often several plans: the stage items a plan
				// leaves are information (kept open for the next plans), not
				// a reason to send it back — every plan of an eleven-sheet
				// stage went back as incomplete and the AI looped on
				// four-step plans (2026-11-12). Items of the request itself
				// still send it back.
				reqMissing, stageMissing := splitCoverage(missing, stageItems)
				if len(stageItems) > 0 && usage.InputTokens+usage.CacheReadTokens > 0 {
					stageChecked, stageOpen = true, stageMissing
					stageDoes = nil
					for _, it := range stageItems {
						if !itemListed(stageMissing, it) {
							stageDoes = append(stageDoes, it)
						}
					}
				}
				if len(reqMissing) > 0 {
					check.warnings = append(check.warnings, coverageWarning(reqMissing))
				}
				if drop, notes, rest := unaskedChanges(extra, args.Steps); len(drop) > 0 {
					kept, _ := aiassistant.DropSteps(args.Steps, drop)
					if len(kept) > 0 {
						args.Steps = kept
						check.notes = append(check.notes, notes...)
						extra = rest
					}
				}
				if len(extra) > 0 {
					check.warnings = append(check.warnings, extraWarning(extra))
				}
			}
			// A plan that runs but whose formulas look wrong goes back once;
			// proposed again unchanged, it reaches the developer with them.
			warnBack := cErr == nil && len(check.problems) == 0 && len(check.warnings) > 0 && !retries.warnedBefore(check.warnings)
			if warnBack {
				warnedPlan, warnedWarnings = args.Steps, check.warnings
			}
			serverPartial := false
			planResult := ""
			if cErr != nil {
				log.Printf("AI proposal check: %v", cErr)
			} else if len(check.problems) > 0 || warnBack {
				planResult = warningResult(check.warnings)
				sentBack = true
				if len(check.problems) > 0 {
					mode := retries.next(len(check.problems))
					if retries.repeats(check.problems) && mode == retryFix {
						mode = retries.escalate()
					}
					// Warnings given beside the failures count as given.
					if mode == retryFix {
						retries.warnedBefore(check.warnings)
					}
					if mode == retryPartial || mode == retryStop {
						// The server makes the plan of the steps that pass
						// itself: asked for it, models re-sent the failing
						// step or claimed a plan they never proposed
						// (2026-11-11, five rounds over one reorder step).
						if kept, _ := aiassistant.DropSteps(args.Steps, check.failed); len(kept) > 0 && len(check.failed) > 0 {
							retries.leftOut, retries.partialOf = check.problems, args.Steps
							retries.partialAsked = true
							args.Steps = kept
							serverPartial = true
							sentBack = false
						}
					}
					if mode == retryPartial && !serverPartial {
						retries.leftOut, retries.partialOf = check.problems, args.Steps
					}
					if mode == retryStop {
						sentBack = false // told to stop and explain: no nudge
					}
					planResult = check.rejection(mode)
				}
			}
			if cErr == nil && !serverPartial && (len(check.problems) > 0 || warnBack) {
				_, _ = store.SaveMessage(ctx, sessionID, "assistant", llmResp.Message.Content, llmResp.Message.ToolCalls, "", "")
				provMessages = append(provMessages, llmResp.Message)
				for _, call := range llmResp.Message.ToolCalls {
					result := planResult
					if !aiassistant.IsWriteTool(call.Name) {
						var execErr error
						if result, execErr = runTool(ctx, store, sessionID, planHooks, readExecutor, call.Name, call.Arguments); execErr != nil {
							result = fmt.Sprintf("error: %v", execErr)
						}
					}
					_, _ = store.SaveMessage(ctx, sessionID, "tool", result, nil, call.ID, call.Name)
					provMessages = append(provMessages, providers.Message{Role: "tool", Content: result, ToolCallID: call.ID, ToolName: call.Name})
				}
				sendSSE("tool_status", map[string]string{"tool": "checking the plan"})
				continue turn
			}
			// What the server changed in the plan is shown beside it, as
			// the warnings are, and told to the model.
			shownWarnings := append(append([]string{}, check.notes...), check.warnings...)
			h.labelImportSources(ctx, sessionID, args.Steps)
			proposal, pErr := proposalStore.CreateProposal(ctx, sessionID, args.Steps, shownWarnings)
			if pErr != nil {
				sendSSE("error", map[string]string{"error": fmt.Sprintf("save proposal: %v", pErr)})
				return
			}
			// Save the assistant's tool-call message and a placeholder tool result.
			_, _ = store.SaveMessage(ctx, sessionID, "assistant", llmResp.Message.Content,
				llmResp.Message.ToolCalls, "", "")
			created := fmt.Sprintf("Proposal created (%d step(s)) — awaiting developer confirmation.", len(args.Steps))
			if serverPartial {
				created = fmt.Sprintf("Proposal created by the server with the %d step(s) that pass — awaiting developer confirmation. "+
					"Left out (they fail, or need a step that fails): %s. In your reply, say plainly what was left out and why; do not propose again in this turn.",
					len(args.Steps), strings.Join(check.problems, " | "))
			}
			if len(check.notes) > 0 {
				created += " The server changed the plan: " + strings.Join(check.notes, " | ")
			}
			if len(check.warnings) > 0 {
				created += " Shown with the plan check's warnings: " + strings.Join(check.warnings, " | ")
			}
			if stageChecked {
				if plan, pErr := store.TaskPlan(ctx, sessionID); pErr == nil && plan != nil && plan.NoteCoverage(proposal.ID, stageDoes) {
					_ = store.SetTaskPlan(ctx, sessionID, plan)
				}
				if len(stageOpen) > 0 {
					created += " This plan does part of the current stage; still open in it after this plan: " + strings.Join(stageOpen, "; ") +
						". That is expected — they stay open for your next plans of the stage. Do not propose again in this turn."
				}
			}
			_, replacedSteps, _ := proposalStore.WithdrawPending(ctx, sessionID, proposal.ID)
			if replacedSteps > 0 {
				created += replacedToolNote(replacedSteps)
			}
			_, _ = store.SaveMessage(ctx, sessionID, "tool", created, nil, tc.ID, tc.Name)
			// Every sibling call in the same message needs a result too, or the
			// next turn replays an unanswered tool call and the provider refuses
			// the whole session. One proposal is shown per turn, so a second
			// propose_actions is told to come back after this one is decided.
			for _, other := range llmResp.Message.ToolCalls {
				if other.ID == tc.ID {
					continue
				}
				var result string
				if aiassistant.IsWriteTool(other.Name) {
					result = "Not created: only one proposal is shown per turn. After the developer confirms or rejects the proposal above, call propose_actions again with these steps."
				} else {
					var execErr error
					if result, execErr = runTool(ctx, store, sessionID, planHooks, readExecutor, other.Name, other.Arguments); execErr != nil {
						result = fmt.Sprintf("error: %v", execErr)
					}
				}
				_, _ = store.SaveMessage(ctx, sessionID, "tool", result, nil, other.ID, other.Name)
			}

			if replacedSteps > 0 {
				_, _ = store.SaveMessage(ctx, sessionID, "assistant", replacedChatNote(replacedSteps), nil, "", "")
			}
			if len(retries.leftOut) > 0 {
				_, _ = store.SaveMessage(ctx, sessionID, "assistant", leftOutNote(retries.leftOut, droppedSteps(retries.partialOf, args.Steps, retries.leftOut)), nil, "", "")
			}
			allMsgs, _ := store.ListMessages(ctx, sessionID)
			if allMsgs == nil {
				allMsgs = []aiassistant.ChatMessage{}
			}
			sendSSE("proposal", map[string]any{
				"proposal": proposal,
				"messages": allMsgs,
				"session":  sess,
				"warnings": shownWarnings,
			})
			_ = writeExecutor // suppress unused warning until confirm path uses it
			return
		}

		// All tool calls are read tools — execute them and loop.
		_, _ = store.SaveMessage(ctx, sessionID, "assistant", llmResp.Message.Content,
			llmResp.Message.ToolCalls, "", "")
		provMessages = append(provMessages, llmResp.Message)

		for _, tc := range llmResp.Message.ToolCalls {
			sendSSE("tool_status", map[string]string{"tool": tc.Name})
			result, execErr := runTool(ctx, store, sessionID, planHooks, readExecutor, tc.Name, tc.Arguments)
			if execErr != nil {
				result = fmt.Sprintf("error: %v", execErr)
			} else if isTaskPlanTool(tc.Name) {
				if plan, pErr := store.TaskPlan(ctx, sessionID); pErr == nil && plan != nil {
					sendSSE("task_plan", map[string]any{"task_plan": plan})
				}
			}
			_, _ = store.SaveMessage(ctx, sessionID, "tool", result, nil, tc.ID, tc.Name)
			provMessages = append(provMessages, providers.Message{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
				ToolName:   tc.Name,
			})
		}
	}

	if warnedPlan != nil {
		h.labelImportSources(ctx, sessionID, warnedPlan)
		if proposal, pErr := proposalStore.CreateProposal(ctx, sessionID, warnedPlan, warnedWarnings); pErr == nil {
			if strings.TrimSpace(finalReply) != "" {
				_, _ = store.SaveMessage(ctx, sessionID, "assistant", finalReply, nil, "", "")
			}
			if _, n, _ := proposalStore.WithdrawPending(ctx, sessionID, proposal.ID); n > 0 {
				_, _ = store.SaveMessage(ctx, sessionID, "assistant", replacedChatNote(n), nil, "", "")
			}
			_, _ = store.SaveMessage(ctx, sessionID, "assistant", fmt.Sprintf(
				"The plan below runs (%d step(s)); it is shown with the plan check's warnings, which were not answered: %s",
				len(warnedPlan), strings.Join(warnedWarnings, " | ")), nil, "", "")
			allMsgs, _ := store.ListMessages(ctx, sessionID)
			if allMsgs == nil {
				allMsgs = []aiassistant.ChatMessage{}
			}
			usage, _ := store.Usage(context.WithoutCancel(ctx), sessionID)
			sendSSE("proposal", map[string]any{"proposal": proposal, "messages": allMsgs, "session": sess, "warnings": warnedWarnings, "usage": withCost(usage, llmModel)})
			return
		}
	}

	// 6. Save final assistant reply, and the server's note when the reply
	// follows a refused plan or claims a change no proposal made.
	saved, err := store.SaveMessage(ctx, sessionID, "assistant", finalReply, nil, "", "")
	if err != nil {
		sendSSE("error", map[string]string{"error": err.Error()})
		return
	}
	if note := noChangeNote(finalReply, retries.attempts > 0); note != "" {
		_, _ = store.SaveMessage(ctx, sessionID, "assistant", note, nil, "", "")
	}

	allMsgs, _ := store.ListMessages(ctx, sessionID)
	if allMsgs == nil {
		allMsgs = []aiassistant.ChatMessage{}
	}
	doneUsage, _ := store.Usage(context.WithoutCancel(ctx), sessionID)
	sendSSE("done", map[string]any{
		"reply":     saved,
		"messages":  allMsgs,
		"session":   sess,
		"usage":     withCost(doneUsage, llmModel),
		"task_plan": func() *aiassistant.TaskPlan { p, _ := store.TaskPlan(ctx, sessionID); return p }(),
	})
}

// ── POST /api/ai/sessions/{sid}/proposals/{pid}/confirm ──────────────────────

func (h *handler) aiConfirmProposal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	// Up to aiassistant.MaxProposalSteps steps, file imports and their
	// recalculation included.
	extendAIWriteDeadline(w)
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}

	// What the developer typed into the confirmation card, by 1-based step:
	// a sign-in connection's secret parts. Never stored with the proposal,
	// never shown to the assistant — handed to the connection's save alone.
	var card struct {
		Secrets map[string]map[string]string `json:"secrets"`
	}
	if raw, rerr := readAll(r); rerr == nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &card); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
	}

	// /api/ai/sessions/{sid}/proposals/{pid}/confirm
	path := strings.TrimPrefix(r.URL.Path, "/api/ai/sessions/")
	parts := strings.SplitN(path, "/", 4) // [sid, "proposals", pid, "confirm"]
	if len(parts) < 4 {
		jsonErr(w, fmt.Errorf("invalid path"), http.StatusBadRequest)
		return
	}
	sessionID, proposalID := parts[0], parts[2]

	// Validate proposal belongs to a session owned by this user.
	sess, err := h.aiChatStore(ctx).GetSession(ctx, sessionID)
	if err != nil || sess.UserID != a.UserID {
		jsonErr(w, fmt.Errorf("session not found"), http.StatusNotFound)
		return
	}

	pStore := aiassistant.NewProposalStore(h.db.For(ctx))
	proposal, err := pStore.GetProposal(ctx, proposalID)
	if err != nil || proposal.SessionID != sessionID {
		jsonErr(w, fmt.Errorf("proposal not found"), http.StatusNotFound)
		return
	}
	if proposal.Status != "pending" {
		jsonErr(w, fmt.Errorf("proposal is already %s", proposal.Status), http.StatusConflict)
		return
	}

	// Resolve model/revision for the write executor. AI writes never target an
	// existing revision directly: the session's first confirmed proposal lazily
	// creates an isolated draft — a full copy of the developer's working
	// revision (?revision_id, the revision the session's plans were read
	// from), else of the active one — and every later proposal in the same
	// session reuses it. The developer takes it as their working revision
	// (promote-draft) or discards it once they're happy — or not — with the
	// result; making it active is a separate step.
	// Discarding this error left modelID empty and handed it to the write
	// executor, where it surfaced as a uuid cast failure — a 500 for what is
	// really a 403, on the one endpoint that performs every AI write.
	modelID, mErr := h.resolveDemoModelID(ctx, r)
	if mErr != nil {
		jsonAccessErr(w, mErr, "resolve model")
		return
	}
	// Without this a proposal written in one model could be confirmed into
	// another — its draft, or rows carrying the second model's id written
	// into the first model's draft.
	if err := h.aiSessionInModel(ctx, sess, modelID); err != nil {
		jsonErr(w, err, http.StatusConflict)
		return
	}
	baseRevID := r.URL.Query().Get("revision_id")
	if baseRevID != "" && !h.revisionBelongsToModel(ctx, baseRevID, modelID) {
		jsonErr(w, fmt.Errorf("revision does not belong to this model"), http.StatusForbidden)
		return
	}
	// newDraft copies the base into a new draft, named name or after the
	// time, and makes it the session's.
	newDraft := func(name string) (string, error) {
		// Millisecond precision: minute-granularity previously collided with
		// model.revision's UNIQUE(model_id, name) whenever a second draft
		// was created within the same clock-minute (e.g. promote, then
		// immediately confirm another proposal) — confirm would fail with a
		// raw constraint-violation 500 instead of quietly creating the next
		// draft.
		if strings.TrimSpace(name) == "" {
			name = fmt.Sprintf("AI Draft %s", time.Now().Format("2006-01-02 15:04:05.000"))
		}
		draftParams, _ := json.Marshal(map[string]string{"name": name, "source_revision_id": baseRevID})
		draftExecutor := aiassistant.NewWriteExecutorWithActor(h.db.For(ctx), modelID, "", a.UserID).WithHooks(h.aiWriteHooks(modelID, a.UserID))
		_, newRevID, dErr := draftExecutor.Execute(ctx, "create_revision", draftParams)
		if dErr != nil {
			return "", fmt.Errorf("create draft revision: %w", dErr)
		}
		if dErr := h.aiChatStore(ctx).SetDraftRevisionID(ctx, sessionID, newRevID); dErr != nil {
			return "", fmt.Errorf("save draft revision: %w", dErr)
		}
		sess.DraftRevisionID = newRevID
		return newRevID, nil
	}
	revID := sess.DraftRevisionID
	if revID == "" {
		if revID, err = newDraft(""); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
	}

	// WithActor: create_workflow_def/update_workflow_def cast created_by/
	// updated_by directly to ::uuid with no NULLIF (see NewWriteExecutorWithActor's
	// doc comment) — a plain NewWriteExecutor here would 500 on the very first
	// AI-authored workflow confirmed through this real endpoint.
	hooks := h.aiWriteHooks(modelID, a.UserID)
	hooks.ImportFile = func(ctx context.Context, req aiassistant.FileImportRequest) (string, error) {
		return h.aiImportFile(ctx, a, sessionID, req)
	}
	hooks.WriteValues = func(ctx context.Context, req aiassistant.ValuesWriteRequest) (string, error) {
		return h.aiWriteValues(ctx, a, req)
	}
	hooks.RecallPreview = func(ctx context.Context, file, sheet string) (aiassistant.FileImportRequest, bool) {
		return h.recallCleanPreview(ctx, sessionID, file, sheet)
	}
	var stepCard map[string]string // the card's entries for the step running
	hooks.SaveConnector = func(ctx context.Context, req aiassistant.ConnectorSave) (string, error) {
		return h.aiSaveConnector(ctx, h.db.For(ctx), a, modelID, revID, req)
	}
	hooks.SaveConnection = func(ctx context.Context, req aiassistant.ConnectionSave) (string, error) {
		return h.aiSaveConnection(ctx, h.db.For(ctx), a, modelID, req, stepCard, false)
	}
	hooks.RunIntegration = func(ctx context.Context, integrationID, mode string) (string, error) {
		return h.aiRunIntegration(ctx, r, integrationID, mode)
	}
	hooks.TriggerRule = func(ctx context.Context, ruleID string, payload map[string]any) (string, error) {
		return h.aiTriggerRule(ctx, r, ruleID, payload)
	}
	hooks.Recalculate = func(ctx context.Context, revisionID string) (string, error) {
		return h.aiRecalculate(ctx, r, revisionID)
	}
	hooks.ClearValues = func(ctx context.Context, kind, id string, body json.RawMessage) (string, error) {
		return h.aiClearValues(ctx, r, kind, id, body)
	}
	hooks.FillSample = func(ctx context.Context, gridID string, body json.RawMessage, asFile bool) (string, error) {
		return h.aiFillSample(ctx, r, sessionID, a.UserID, gridID, body, asFile)
	}
	executor := aiassistant.NewWriteExecutorWithActor(h.db.For(ctx), modelID, revID, a.UserID).WithHooks(hooks)
	_ = pStore.SetStatus(ctx, proposalID, "confirmed")

	// Execute each step, substituting "<created in step N>" placeholders with
	// actual IDs returned by earlier steps. The first failure stops the
	// plan: later steps are built on it (a formula naming a metric the
	// failed step was to create), and running them anyway only multiplied
	// one mistake into a page of errors — reported live. What ran before it
	// stays in the draft.
	anyFailed := false
	steps := proposal.Steps
	created := make([]string, len(steps))
	for i, step := range steps {
		if anyFailed {
			steps[i].Status = "skipped"
			steps[i].Result = "not run: an earlier step failed"
			continue
		}
		resolved := resolveParamRefs(step.Params, created[:i])
		stepCard = card.Secrets[strconv.Itoa(i+1)]
		result, createdID, execErr := executor.Execute(ctx, step.Tool, resolved)
		stepCard = nil
		if execErr != nil {
			steps[i].Status = "failed"
			steps[i].Result = execErr.Error()
			anyFailed = true
		} else {
			steps[i].Status = "success"
			steps[i].Result = result
			steps[i].CreatedID = createdID
		}
		created[i] = createdID
	}
	_ = pStore.UpdateSteps(ctx, proposalID, steps)

	finalStatus := "executed"
	if anyFailed {
		finalStatus = "partial"
	}
	_ = pStore.SetStatus(ctx, proposalID, finalStatus)
	// The task plan's current stage records the confirmed plan.
	if ran := countRan(steps); ran > 0 {
		if plan, pErr := h.aiChatStore(ctx).TaskPlan(ctx, sessionID); pErr == nil && plan != nil && plan.RecordConfirmed(proposalID, ran, !anyFailed) {
			_ = h.aiChatStore(ctx).SetTaskPlan(ctx, sessionID, plan)
		}
	}
	// Calculated metrics the plan created, changed or placed on a grid get
	// their results now, as they do when a developer saves one: an AI-built
	// P&L stayed blank until an input happened to change.
	h.recalcRevisionInBackground(ctx, modelID, revID)

	// Save a summary message into the chat.
	summary := buildExecutionSummary(steps)
	chatStore := h.aiChatStore(ctx)
	_, _ = chatStore.SaveMessage(ctx, sessionID, "assistant", summary, nil, "", "")

	proposal.Steps = steps
	proposal.Status = finalStatus
	allMsgs, _ := chatStore.ListMessages(ctx, sessionID)
	if allMsgs == nil {
		allMsgs = []aiassistant.ChatMessage{}
	}
	var appID string
	_ = h.db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID)
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryAIAssistant, EventType: auditlog.EventAIProposalConfirmed,
		ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
		ApplicationID: appID, ResourceType: "ai_proposal", ResourceID: proposalID, RevisionID: revID,
		Metadata: map[string]string{"session_id": sessionID, "status": finalStatus, "step_count": strconv.Itoa(len(steps))},
	})
	jsonOK(w, map[string]any{
		"proposal": proposal,
		"messages": allMsgs,
		"session":  sess,
	})
}

// quotedName pulls the first 'quoted' token out of a step description
// ("Add Secret member 's1'" → "s1") for use as a short label when a batch
// of same-tool steps is collapsed into one line. Falls back to the full
// description when nothing is quoted, so a step never loses information —
// it just doesn't get shortened.
var quotedName = regexp.MustCompile(`'([^']+)'`)

func shortStepLabel(s aiassistant.ProposalStep) string {
	if m := quotedName.FindStringSubmatch(s.Description); m != nil {
		return m[1]
	}
	return s.Description
}

// buildExecutionSummary reports what a confirmed proposal did. Steps of the
// same tool are collapsed into one line (a batch of 18 "add member" calls
// reads as one line, not eighteen) — the full per-step detail, including
// created IDs, remains in proposal.Steps for anyone who needs it; this is
// just the chat-facing summary. Failures are always listed individually and
// in full: they're rare and need enough detail to diagnose, so they're
// never worth collapsing.
// firstFailed is the 1-based number of the first failed step (0: none).
func firstFailed(steps []aiassistant.ProposalStep) int {
	for i, s := range steps {
		if s.Status == "failed" {
			return i + 1
		}
	}
	return 0
}

func buildExecutionSummary(steps []aiassistant.ProposalStep) string {
	failed, skipped := 0, 0
	for _, s := range steps {
		switch s.Status {
		case "failed":
			failed++
		case "skipped":
			skipped++
		}
	}

	var sb strings.Builder
	switch {
	case failed == 0:
		fmt.Fprintf(&sb, "Executed %d step%s successfully.\n", len(steps), plural(len(steps)))
	case skipped > 0:
		fmt.Fprintf(&sb, "Executed %d of %d steps — step %d failed, so the %d after it were not run.\n",
			len(steps)-failed-skipped, len(steps), firstFailed(steps), skipped)
	default:
		fmt.Fprintf(&sb, "Executed %d step%s — %d failed.\n", len(steps), plural(len(steps)), failed)
	}

	type group struct {
		tool   string
		labels []string
	}
	var groups []*group
	byTool := map[string]*group{}
	var failLines []string

	for i, s := range steps {
		if s.Status == "failed" {
			failLines = append(failLines, fmt.Sprintf("✗ Step %d: %s — %s", i+1, s.Description, s.Result))
			continue
		}
		if s.Status == "skipped" {
			failLines = append(failLines, fmt.Sprintf("– Step %d not run: %s", i+1, s.Description))
			continue
		}
		g, ok := byTool[s.Tool]
		if !ok {
			g = &group{tool: s.Tool}
			byTool[s.Tool] = g
			groups = append(groups, g)
		}
		g.labels = append(g.labels, shortStepLabel(s))
	}

	for _, g := range groups {
		if len(g.labels) == 1 {
			fmt.Fprintf(&sb, "✓ %s\n", g.labels[0])
			continue
		}
		const maxShown = 8
		names := g.labels
		suffix := ""
		if len(names) > maxShown {
			suffix = fmt.Sprintf(", +%d more", len(names)-maxShown)
			names = names[:maxShown]
		}
		fmt.Fprintf(&sb, "✓ %d× %s: %s%s\n", len(g.labels), strings.ReplaceAll(g.tool, "_", " "), strings.Join(names, ", "), suffix)
	}

	for _, l := range failLines {
		fmt.Fprintf(&sb, "%s\n", l)
	}

	return sb.String()
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// ── POST /api/ai/sessions/{sid}/promote-draft ─────────────────────────────────

// aiPromoteDraft hands the session's isolated draft revision (created
// lazily on its first confirmed proposal — see aiConfirmProposal above) to
// the developer as their working revision: the console switches to it.
// It does not make it the model's active revision — which revision goes
// live is a separate decision, Set active in Developer › Developer Models (owner
// decision, 2026-10-10). It then clears the session's draft_revision_id,
// which makes the "AI draft" banner disappear, and finishes the session:
// its next confirmed proposal would otherwise write on into a revision the
// developer now builds by hand.
func (h *handler) aiPromoteDraft(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}

	// /api/ai/sessions/{sid}/promote-draft
	sessionID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/ai/sessions/"), "/promote-draft")

	sess, err := h.aiChatStore(ctx).GetSession(ctx, sessionID)
	if err != nil || sess.UserID != a.UserID {
		jsonErr(w, fmt.Errorf("session not found"), http.StatusNotFound)
		return
	}
	if sess.DraftRevisionID == "" {
		jsonErr(w, fmt.Errorf("this session has no draft revision to promote"), http.StatusBadRequest)
		return
	}
	draftRevID := sess.DraftRevisionID

	var modelID, draftName string
	if err := h.db.QueryRow(ctx, `SELECT model_id::text, name FROM model.revision WHERE id=$1::uuid`, draftRevID).Scan(&modelID, &draftName); err != nil {
		jsonErr(w, fmt.Errorf("draft revision not found"), http.StatusNotFound)
		return
	}
	// A name given here is the revision's from now on: a promoted draft
	// otherwise kept "AI Draft" and its time, which the header shows every
	// business user once it goes live (2026-11-08). The body is optional.
	var body struct {
		Name string `json:"name"`
	}
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
	}
	if strings.TrimSpace(body.Name) != "" && strings.TrimSpace(body.Name) != draftName {
		if draftName, err = h.renameRevision(ctx, draftRevID, body.Name, nil); err != nil {
			jsonErr(w, err, renameRevisionStatus(err))
			return
		}
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryAIAssistant, EventType: auditlog.EventAIDraftPromoted,
		ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
		ResourceType: "revision", ResourceID: draftRevID, RevisionID: draftRevID,
		Metadata: map[string]string{"session_id": sessionID, "name": draftName},
	})
	// The assistant's member, property and formula writes landed on the
	// draft, where nothing recalculated them (write_executor has no
	// scheduler): recompute every calculated metric of the draft once, so
	// what the developer looks through in the console is what its
	// definitions say (contract C8). Activation recomputes again, and runs
	// the schema migration (activateRevision).
	h.recalcRevisionInBackground(ctx, modelID, draftRevID)

	if err := h.aiChatStore(ctx).SetDraftRevisionID(ctx, sessionID, ""); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	sess.DraftRevisionID = ""
	// The session is finished: its plans would go on being checked against
	// the base it started from, not the promoted revision.
	promotedAt, err := h.aiChatStore(ctx).MarkPromoted(ctx, sessionID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	sess.PromotedAt = &promotedAt

	chatStore := h.aiChatStore(ctx)
	// The developer's other sessions continued the same draft: they are
	// finished too, or their next proposal would write into the revision
	// the developer now builds by hand.
	others, err := chatStore.FinishDraftSessions(ctx, draftRevID, a.UserID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	for _, id := range others {
		_, _ = chatStore.SaveMessage(ctx, id, "assistant", promotedMessage, nil, "", "")
	}
	_, _ = chatStore.SaveMessage(ctx, sessionID, "assistant", promotedMessage, nil, "", "")
	allMsgs, _ := chatStore.ListMessages(ctx, sessionID)
	if allMsgs == nil {
		allMsgs = []aiassistant.ChatMessage{}
	}

	jsonOK(w, map[string]any{"session": sess, "messages": allMsgs,
		"revision": map[string]string{"id": draftRevID, "name": draftName}})
}

// ── POST /api/ai/sessions/{sid}/discard-draft ─────────────────────────────────

// aiDiscardDraft deletes a session's isolated draft revision and clears
// session.draft_revision_id server-side, in one action. Replaces the
// frontend's previous approach — calling the generic
// DELETE /api/developer/revisions/{id} and only updating local React state
// afterward — which never cleared the field server-side: a page reload (or
// just re-fetching the session) still showed a dangling draft_revision_id
// pointing at a deleted row, and the session's NEXT confirmed proposal
// reused that dead UUID and 500'd on an FK violation instead of starting a
// fresh draft.
func (h *handler) aiDiscardDraft(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}

	// /api/ai/sessions/{sid}/discard-draft
	sessionID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/ai/sessions/"), "/discard-draft")

	sess, err := h.aiChatStore(ctx).GetSession(ctx, sessionID)
	if err != nil || sess.UserID != a.UserID {
		jsonErr(w, fmt.Errorf("session not found"), http.StatusNotFound)
		return
	}
	if sess.DraftRevisionID == "" {
		jsonErr(w, fmt.Errorf("this session has no draft revision to discard"), http.StatusBadRequest)
		return
	}
	draftRevID := sess.DraftRevisionID

	// ON DELETE CASCADE (migration 027 for metric/dimension/grid/dashboard,
	// migration 056 for the form_def/workflow_def/automation_rule rows
	// Batch 0 of this item taught createRevision to also copy) removes
	// every AI-authored row in the draft in this one statement.
	if _, err := h.db.Exec(ctx, `DELETE FROM model.revision WHERE id=$1::uuid`, draftRevID); err != nil {
		jsonErr(w, fmt.Errorf("delete draft revision: %w", err), http.StatusInternalServerError)
		return
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryModelChange, EventType: auditlog.EventRevisionDeleted,
		ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
		// RevisionID intentionally left unset, matching the manual
		// delete-revision handler: the audit_event.revision_id FK would
		// reject a reference to the very revision this row announces the
		// deletion of. resource_id (plain TEXT, no FK) already carries it.
		ResourceType: "revision", ResourceID: draftRevID,
		Metadata: map[string]string{"source": "ai_assistant_draft", "session_id": sessionID},
	})

	if err := h.aiChatStore(ctx).SetDraftRevisionID(ctx, sessionID, ""); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	sess.DraftRevisionID = ""

	chatStore := h.aiChatStore(ctx)
	_, _ = chatStore.SaveMessage(ctx, sessionID, "assistant", "Discarded the draft — every change made in this session was removed.", nil, "", "")
	allMsgs, _ := chatStore.ListMessages(ctx, sessionID)
	if allMsgs == nil {
		allMsgs = []aiassistant.ChatMessage{}
	}

	jsonOK(w, map[string]any{"session": sess, "messages": allMsgs})
}

// ── GET /api/ai/sessions/{sid}/proposals ──────────────────────────────────────

// proposalWithSummary adds a human-readable one-liner to each proposal —
// buildExecutionSummary's per-step-collapsed text for executed/partial
// proposals, a short status line otherwise — so the Activity panel doesn't
// need to re-derive it from raw step JSON.
type proposalWithSummary struct {
	aiassistant.Proposal
	Summary string `json:"summary"`
}

// aiListProposals returns every proposal ever made in a session, most
// recent first — not just the still-pending ones ListPendingProposals (used
// internally by aiSendMessage) returns. Backs the Activity panel: the only
// history of AI actions beyond the linear chat transcript.
func (h *handler) aiListProposals(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}

	// /api/ai/sessions/{sid}/proposals
	sessionID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/ai/sessions/"), "/proposals")

	sess, err := h.aiChatStore(ctx).GetSession(ctx, sessionID)
	if err != nil || sess.UserID != a.UserID {
		jsonErr(w, fmt.Errorf("session not found"), http.StatusNotFound)
		return
	}

	proposals, err := aiassistant.NewProposalStore(h.db.For(ctx)).ListProposals(ctx, sessionID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	out := make([]proposalWithSummary, len(proposals))
	for i, p := range proposals {
		var summary string
		switch p.Status {
		case "executed", "partial":
			summary = buildExecutionSummary(p.Steps)
		case "pending":
			summary = "Awaiting confirmation."
		case "rejected":
			summary = "Rejected — no changes made."
		default:
			summary = "Confirmed — executing."
		}
		out[i] = proposalWithSummary{Proposal: p, Summary: summary}
	}
	jsonOK(w, out)
}

// ── POST /api/ai/sessions/{sid}/proposals/{pid}/reject ───────────────────────

func (h *handler) aiRejectProposal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/api/ai/sessions/")
	parts := strings.SplitN(path, "/", 4)
	if len(parts) < 4 {
		jsonErr(w, fmt.Errorf("invalid path"), http.StatusBadRequest)
		return
	}
	sessionID, proposalID := parts[0], parts[2]

	sess, err := h.aiChatStore(ctx).GetSession(ctx, sessionID)
	if err != nil || sess.UserID != a.UserID {
		jsonErr(w, fmt.Errorf("session not found"), http.StatusNotFound)
		return
	}

	pStore := aiassistant.NewProposalStore(h.db.For(ctx))
	proposal, err := pStore.GetProposal(ctx, proposalID)
	if err != nil || proposal.SessionID != sessionID {
		jsonErr(w, fmt.Errorf("proposal not found"), http.StatusNotFound)
		return
	}
	if proposal.Status != "pending" {
		jsonErr(w, fmt.Errorf("proposal is already %s", proposal.Status), http.StatusConflict)
		return
	}

	_ = pStore.SetStatus(ctx, proposalID, "rejected")

	chatStore := h.aiChatStore(ctx)
	_, _ = chatStore.SaveMessage(ctx, sessionID, "assistant",
		"Proposal rejected. Let me know if you'd like to try something different.", nil, "", "")

	allMsgs, _ := chatStore.ListMessages(ctx, sessionID)
	if allMsgs == nil {
		allMsgs = []aiassistant.ChatMessage{}
	}
	proposal.Status = "rejected"
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryAIAssistant, EventType: auditlog.EventAIProposalRejected,
		ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
		ResourceType: "ai_proposal", ResourceID: proposalID, RevisionID: sess.DraftRevisionID,
		Metadata: map[string]string{"session_id": sessionID},
	})
	jsonOK(w, map[string]any{
		"proposal": proposal,
		"messages": allMsgs,
	})
}

// ── GET /api/ai/settings ─────────────────────────────────────────────────────

func (h *handler) aiGetSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	settings, err := h.aiChatStore(personalCtx(ctx, a)).GetSettings(personalCtx(ctx, a), a.UserID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	// A developer whose tenant provides the key needs to know that before
	// wondering why their own key had no effect, so say it here rather than
	// letting the screen imply a personal key is required.
	tenant := h.tenantAIKey(ctx, h.requestCustomerID(ctx, r, a))
	jsonOK(w, aiSettingsResponse{
		LLMSettings:    settings,
		TenantKey:      tenant.OK,
		TenantEnforced: tenant.OK && tenant.Enforced,
		TenantProvider: tenantProviderLabel(tenant),
		DefaultModels:  defaultModels(),
		BuildAdvice:    buildAdvice(cmp.Or(settings.Provider, "openai"), cmp.Or(settings.Model, defaultModel(cmp.Or(settings.Provider, "openai"))), settings.ReasoningEffort),
	})
}

// aiSettingsResponse adds what the personal settings screen cannot know from
// the row alone: whether the tenant supplies a key, and whether it overrides
// the personal one entirely.
type aiSettingsResponse struct {
	aiassistant.LLMSettings
	TenantKey      bool   `json:"tenant_key"`
	TenantEnforced bool   `json:"tenant_enforced"`
	TenantProvider string `json:"tenant_provider,omitempty"`
	// DefaultModels is the model each provider runs when the setting is blank.
	DefaultModels map[string]string `json:"default_models"`
	// BuildAdvice says whether the model and effort suit building a model
	// (buildAdvice); "" when they do.
	BuildAdvice string `json:"build_advice,omitempty"`
}

func tenantProviderLabel(t tenantAIKeyResult) string {
	if !t.OK {
		return ""
	}
	if t.Provider == "" {
		return "openai"
	}
	return t.Provider
}

// ── PUT /api/ai/settings ─────────────────────────────────────────────────────

type aiSettingsReq struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	APIKey   string `json:"api_key"` // optional; omit to keep existing key
	// ReasoningEffort: omitted keeps the saved one; "" clears it.
	ReasoningEffort *string `json:"reasoning_effort"`
}

// validReasoningEffort refuses an effort OpenAI does not take.
func validReasoningEffort(e *string) error {
	if e == nil || slices.Contains(aiassistant.ReasoningEfforts, strings.TrimSpace(*e)) {
		return nil
	}
	return fmt.Errorf("reasoning_effort %q is not one of none, minimal, low, medium, high, xhigh (or blank for the model's default)", *e)
}

// withEffort gives an OpenAI provider the developer's reasoning effort.
func withEffort(p providers.Provider, effort string) providers.Provider {
	if op, ok := p.(*providers.OpenAIProvider); ok && strings.TrimSpace(effort) != "" {
		return op.WithReasoningEffort(effort)
	}
	return p
}

func (h *handler) aiSaveSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	var req aiSettingsReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	if req.Provider == "" {
		req.Provider = "openai"
	}
	// A blank model stays blank: the provider's default is applied when the
	// key is used (providerDefaultModels). Filling in an OpenAI model here gave
	// every other provider a model it does not have.
	if _, err := buildProvider(req.Provider, "probe"); err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	if err := validReasoningEffort(req.ReasoningEffort); err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	if req.ReasoningEffort != nil {
		e := strings.TrimSpace(*req.ReasoningEffort)
		req.ReasoningEffort = &e
	}
	if err := h.aiChatStore(personalCtx(ctx, a)).SaveSettingsWithEffort(personalCtx(ctx, a), a.UserID, req.Provider, req.Model, req.APIKey, req.ReasoningEffort); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryAIAssistant, EventType: auditlog.EventAISettingsUpdated,
		ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
		ResourceType: "ai_settings", ResourceID: a.UserID,
		Metadata: map[string]string{"provider": req.Provider, "model": req.Model, "api_key_changed": strconv.FormatBool(req.APIKey != "")},
	})
	jsonOK(w, map[string]string{"status": "ok"})
}

// ── POST /api/ai/sessions/{sid}/documents ────────────────────────────────────
//
// Multipart upload. The file is parsed to plain text server-side (pdf, xlsx,
// docx, csv, txt, md, json), and that text is injected into the system
// prompt on every subsequent chat call in the session; a spreadsheet is also
// kept whole for the import tools (a CSV file's text is then only its first
// lines).
func (h *handler) aiUploadDocument(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/ai/sessions/")
	sessionID := strings.TrimSuffix(path, "/documents")

	sess, err := h.aiChatStore(ctx).GetSession(ctx, sessionID)
	if err != nil || sess.UserID != a.UserID {
		jsonErr(w, fmt.Errorf("session not found"), http.StatusNotFound)
		return
	}

	// The file has as long to arrive as an import's: a few megabytes over a
	// slow link took longer than the server's one-minute ReadTimeout, and,
	// cut off mid-file, reached a tester as a 502 while the same rows in
	// files of 20,000 went through (2026-10-24). A normal upload still takes
	// seconds; this is the ceiling for the 20 MB the form reads.
	extendImportDeadlines(w)
	if err := r.ParseMultipartForm(20 << 20); err != nil {
		jsonErr(w, fmt.Errorf("invalid upload: %w", err), http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		jsonErr(w, fmt.Errorf("missing 'file' field: %w", err), http.StatusBadRequest)
		return
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, 20<<20))
	if err != nil {
		jsonErr(w, fmt.Errorf("read upload: %w", err), http.StatusBadRequest)
		return
	}

	text, mimeType, truncated, err := aiassistant.ExtractDocumentText(header.Filename, data)
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}

	// A spreadsheet also keeps its bytes, so the assistant can import the
	// whole file (import_file_data); the text above is only the LLM's view.
	var raw []byte
	if importpkg.IsTabularFile(header.Filename) && len(data) <= maxAIImportBytes {
		raw = data
	}
	doc, err := aiassistant.NewDocumentStore(h.db.For(ctx)).CreateDocument(ctx, sessionID, header.Filename, mimeType, text, truncated, raw)
	if err != nil {
		jsonErr(w, fmt.Errorf("save document: %w", err), http.StatusInternalServerError)
		return
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryAIAssistant, EventType: auditlog.EventAIDocumentUploaded,
		ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
		ResourceType: "ai_document", ResourceID: doc.ID,
		Metadata: map[string]string{"session_id": sessionID, "filename": header.Filename},
	})
	jsonOK(w, doc)
}

// ── DELETE /api/ai/sessions/{sid}/documents/{did} ────────────────────────────

func (h *handler) aiDeleteDocument(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/ai/sessions/")
	parts := strings.SplitN(path, "/", 3) // [sid, "documents", did]
	if len(parts) != 3 || parts[2] == "" {
		jsonErr(w, fmt.Errorf("invalid path"), http.StatusBadRequest)
		return
	}
	sessionID, docID := parts[0], parts[2]

	sess, err := h.aiChatStore(ctx).GetSession(ctx, sessionID)
	if err != nil || sess.UserID != a.UserID {
		jsonErr(w, fmt.Errorf("session not found"), http.StatusNotFound)
		return
	}
	if err := aiassistant.NewDocumentStore(h.db.For(ctx)).DeleteDocument(ctx, sessionID, docID); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryAIAssistant, EventType: auditlog.EventAIDocumentDeleted,
		ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
		ResourceType: "ai_document", ResourceID: docID,
		Metadata: map[string]string{"session_id": sessionID},
	})
	jsonOK(w, map[string]string{"status": "deleted"})
}

// buildDocumentContext renders session documents as a system-prompt section.
// Returns "" when the session has no documents.
func buildDocumentContext(docs []aiassistant.Document) string {
	if len(docs) == 0 {
		return ""
	}
	const totalBudget = 120000 // chars across all docs per request
	var sb strings.Builder
	sb.WriteString("\n\n## Attached documents\n")
	sb.WriteString("The developer uploaded these documents to this session. Use their contents when relevant.\n")
	used := 0
	for _, d := range docs {
		remaining := totalBudget - used
		if remaining <= 0 {
			fmt.Fprintf(&sb, "\n### %s\n(omitted — context budget exhausted)\n", d.Filename)
			continue
		}
		content := d.Content
		clipped := d.Truncated
		if d.Importable && strings.EqualFold(filepath.Ext(d.Filename), ".csv") {
			content, clipped = aiassistant.ClipCSVOverview(content), false
		}
		if len(content) > remaining {
			content = content[:remaining]
			clipped = true
		}
		used += len(content)
		fmt.Fprintf(&sb, "\n### %s\n", d.Filename)
		if d.Importable {
			sb.WriteString("(Spreadsheet kept whole: importable with preview_file_import / import_file_data, and any sheet's rows and whole formulas readable with read_attached_sheet — the text below is an overview, not the data.)\n")
		}
		sb.WriteString(content)
		if clipped {
			sb.WriteString("\n(... document truncated)")
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// ── POST /api/ai/settings/test ───────────────────────────────────────────────
//
// Verifies a provider/model/key combination with a minimal one-shot chat call.
// Body fields are optional — anything omitted falls back to the caller's
// stored settings (and env-var keys), so "Test" works both before and after
// saving. Always responds 200 with {ok: bool, ...} so the frontend can render
// failures inline rather than as transport errors.
func (h *handler) aiTestSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}

	var req struct {
		Provider        string  `json:"provider"`
		Model           string  `json:"model"`
		APIKey          string  `json:"api_key"`
		ReasoningEffort *string `json:"reasoning_effort"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	// The person's own settings and key (personalCtx).
	pctx := personalCtx(ctx, a)
	store := h.aiChatStore(pctx)
	settings, _ := store.GetSettings(pctx, a.UserID)
	effort := settings.ReasoningEffort
	if req.ReasoningEffort != nil {
		if err := validReasoningEffort(req.ReasoningEffort); err != nil {
			jsonOK(w, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		effort = strings.TrimSpace(*req.ReasoningEffort)
	}
	provider := req.Provider
	if provider == "" {
		provider = settings.Provider
	}
	if provider == "" {
		provider = "openai"
	}
	model := req.Model
	if model == "" {
		model = settings.Model
	}
	if model == "" {
		model = defaultModel(provider)
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryAIAssistant, EventType: auditlog.EventAISettingsTested,
		ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
		ResourceType: "ai_settings", ResourceID: a.UserID,
		Metadata: map[string]string{"provider": provider, "model": model},
	})
	apiKey := req.APIKey
	if apiKey == "" {
		apiKey, _ = store.GetDecryptedAPIKey(pctx, a.UserID)
	}
	if apiKey == "" {
		apiKey = os.Getenv(providerEnvKeys[provider])
	}
	if apiKey == "" {
		jsonOK(w, map[string]any{"ok": false, "provider": provider, "model": model,
			"error": fmt.Sprintf("no API key available for %s", provider)})
		return
	}

	res := probeProviderKeyEffort(ctx, provider, model, apiKey, effort)
	if adv := buildAdvice(provider, model, effort); adv != "" {
		res["build_advice"] = adv
	}
	jsonOK(w, res)
}

// ── POST /api/ai/settings/models ─────────────────────────────────────────────

// aiListModels answers the models the developer's key (the one typed, else
// the saved one, else the deployment's) reaches at provider, for the
// settings screen to offer: the model setting was free text, and a name the
// key cannot use failed only on the first call (2026-11-11).
func (h *handler) aiListModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	var req struct {
		Provider string `json:"provider"`
		APIKey   string `json:"api_key"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	pctx := personalCtx(ctx, a)
	store := h.aiChatStore(pctx)
	settings, _ := store.GetSettings(pctx, a.UserID)
	provider := cmp.Or(req.Provider, settings.Provider, "openai")
	apiKey := req.APIKey
	if apiKey == "" && (settings.Provider == provider || (settings.Provider == "" && provider == "openai")) {
		apiKey, _ = store.GetDecryptedAPIKey(pctx, a.UserID)
	}
	if apiKey == "" {
		apiKey = os.Getenv(providerEnvKeys[provider])
	}
	if apiKey == "" {
		jsonOK(w, map[string]any{"provider": provider, "models": []string{}, "error": fmt.Sprintf("no API key available for %s", provider)})
		return
	}
	p, err := buildProvider(provider, apiKey)
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	lister, ok := p.(providers.ModelLister)
	if !ok {
		jsonOK(w, map[string]any{"provider": provider, "models": []string{}, "error": provider + " does not list its models"})
		return
	}
	lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	models, err := lister.ListModels(lctx)
	if err != nil {
		jsonOK(w, map[string]any{"provider": provider, "models": []string{}, "error": err.Error()})
		return
	}
	if models == nil {
		models = []string{}
	}
	jsonOK(w, map[string]any{"provider": provider, "models": models})
}

// probeProviderKey verifies one (provider, model, key) combination with a
// minimal live call and returns the {ok, provider, model, …} body both the
// per-user and the tenant settings screens render inline. Shared so the two
// screens cannot drift on what "the key works" means.
func probeProviderKey(ctx context.Context, provider, model, apiKey string) map[string]any {
	return probeProviderKeyEffort(ctx, provider, model, apiKey, "")
}

// probeProviderKeyEffort is probeProviderKey with the reasoning effort the
// calls would send, so a model that needs one is tested as it will run.
func probeProviderKeyEffort(ctx context.Context, provider, model, apiKey, effort string) map[string]any {
	p, err := buildProvider(provider, apiKey)
	if err != nil {
		return map[string]any{"ok": false, "provider": provider, "model": model, "error": err.Error()}
	}
	p = withEffort(p, effort)

	testCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Sends the real Tools payload every live chat turn attaches (AllTools())
	// and a prompt a tool-calling model is likely to actually act on — a
	// plain-text-only probe ("reply with ok") always succeeds even for a
	// model that can't tool-call at all, so "Test connection" could pass and
	// then break on the assistant's very first real turn. A non-tool-calling
	// model's failure mode (replying in prose, ignoring the tool) is now
	// distinguishable from a tool-calling model's success (finish reason
	// "tool_calls"). The tool call itself is never executed here — this only
	// proves the provider/model CAN emit one.
	resp, err := p.Chat(testCtx, providers.ChatRequest{
		Model: model,
		Messages: []providers.Message{
			{Role: "user", Content: "Call get_model_summary to confirm you can use tools."},
		},
		Tools: aiassistant.AllTools(),
	})
	if err != nil {
		return map[string]any{"ok": false, "provider": provider, "model": model, "error": err.Error()}
	}
	ok, msg := toolCallProbeResult(resp)
	if !ok {
		return map[string]any{"ok": false, "provider": provider, "model": model, "error": msg}
	}
	return map[string]any{"ok": true, "provider": provider, "model": model, "reply": msg}
}

// toolCallProbeResult evaluates whether a Chat response proves the
// model/provider can actually invoke tools, as opposed to replying in prose
// and ignoring the Tools payload it was sent — the exact ambiguity a
// plain-text-only probe ("reply with the word ok") could never distinguish,
// since it always succeeds even for a model with no tool-calling support.
func toolCallProbeResult(resp providers.ChatResponse) (ok bool, msg string) {
	if resp.FinishReason != "tool_calls" || len(resp.Message.ToolCalls) == 0 {
		return false, "model replied without calling a tool — it may not support tool calling, which the AI Assistant requires"
	}
	return true, fmt.Sprintf("Tool calling confirmed (called %s).", resp.Message.ToolCalls[0].Name)
}

// ── router dispatcher ─────────────────────────────────────────────────────────

// aiSessions handles both GET (list) and POST (create) on /api/ai/sessions.
// aiAutoNameSession asks the session's own LLM for a short title based on
// the first request, falling back to a truncation of the request itself.
// Runs detached from the request's cancellation (WithoutCancel + own
// timeout): naming must never delay or fail a chat turn, nor die with it.
func (h *handler) aiAutoNameSession(reqCtx context.Context, sessionID, firstMessage string, llmProvider providers.Provider, llmModel string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(reqCtx), 20*time.Second)
	defer cancel()

	fallback := strings.TrimSpace(firstMessage)
	if len(fallback) > 60 {
		fallback = strings.TrimSpace(fallback[:60]) + "…"
	}
	title := fallback

	resp, err := llmProvider.Chat(ctx, providers.ChatRequest{
		Model:        llmModel,
		SystemPrompt: "You name chat sessions. Reply with ONLY a concise 2-5 word title for a session that starts with the user's message. No quotes, no punctuation at the end, same language as the message.",
		Messages:     []providers.Message{{Role: "user", Content: firstMessage}},
	})
	if err == nil {
		if t := strings.TrimSpace(strings.Trim(strings.TrimSpace(resp.Message.Content), `"'`)); t != "" && len(t) <= 120 {
			title = t
		}
	}
	if sErr := h.aiChatStore(ctx).SetTitleIfEmpty(ctx, sessionID, title); sErr != nil {
		log.Printf("AI session auto-name: %v", sErr)
	}
}

func (h *handler) aiSessions(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.aiListSessions(w, r)
	case http.MethodPost:
		h.aiCreateSession(w, r)
	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// ── DELETE /api/ai/sessions/{id} ─────────────────────────────────────────────

func (h *handler) aiDeleteSession(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if sessionID == "" {
		// fallback for when called from aiSessionDetail without {id} wildcard
		sessionID = strings.TrimPrefix(r.URL.Path, "/api/ai/sessions/")
	}
	if sessionID == "" {
		jsonErr(w, fmt.Errorf("session id required"), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	if err := h.aiChatStore(ctx).DeleteSession(ctx, sessionID, a.UserID); err != nil {
		jsonErr(w, err, http.StatusNotFound)
		return
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryAIAssistant, EventType: auditlog.EventAISessionDeleted,
		ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
		ResourceType: "ai_session", ResourceID: sessionID,
	})
	jsonOK(w, map[string]string{"status": "deleted"})
}

// aiSessionDetail dispatches sub-paths under /api/ai/sessions/{id}/...
//
//	DELETE /api/ai/sessions/{id}                      → aiDeleteSession
//	/api/ai/sessions/{id}/messages                    → aiSendMessage
//	/api/ai/sessions/{id}/promote-draft               → aiPromoteDraft
//	/api/ai/sessions/{id}/proposals/{pid}/confirm     → aiConfirmProposal
//	/api/ai/sessions/{id}/proposals/{pid}/reject      → aiRejectProposal
//	GET /api/ai/sessions/{id}                         → aiGetSession
//
// revisionBelongsToModel reports whether revID is a revision of modelID.
//
// Fails closed: a query error answers "no", because the alternative is
// treating an unreadable database as permission.
func (h *handler) revisionBelongsToModel(ctx context.Context, revID, modelID string) bool {
	var ok bool
	if err := h.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM model.revision WHERE id=$1::uuid AND model_id=$2::uuid)`,
		revID, modelID,
	).Scan(&ok); err != nil {
		return false
	}
	return ok
}

// aiSessionInModel refuses a request on a session from a model other than
// the one the request resolved. A session belongs to the model it was
// started in — its draft is a copy of that model, and its proposals name
// that model's metrics and dimensions. The error names the session's model
// so the developer knows where to go back to.
func (h *handler) aiSessionInModel(ctx context.Context, sess aiassistant.Session, modelID string) error {
	if sess.ModelID == modelID {
		return nil
	}
	var name string
	_ = h.db.QueryRow(ctx, `SELECT name FROM core.model WHERE id=$1::uuid`, sess.ModelID).Scan(&name)
	return fmt.Errorf("this session belongs to the model %q — open that model to continue it, or start a new session for this model", name)
}

// aiRenameSession serves PATCH /api/ai/sessions/{id} {"title": "..."} —
// owner-scoped like delete. An explicit rename always wins over the
// background auto-namer (which only fills empty titles).
func (h *handler) aiRenameSession(w http.ResponseWriter, r *http.Request, sessionID string) {
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	var req struct {
		Title string `json:"title"`
		// CallBudget: a number caps the session's provider calls; null
		// clears it (the deployment's cap). Absent leaves it.
		CallBudget json.RawMessage `json:"call_budget"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, fmt.Errorf("invalid body: %w", err), http.StatusBadRequest)
		return
	}
	if len(req.CallBudget) > 0 {
		var n *int
		if string(req.CallBudget) != "null" {
			var v int
			if json.Unmarshal(req.CallBudget, &v) != nil || v <= 0 {
				jsonErr(w, fmt.Errorf("call_budget is a positive whole number, or null"), http.StatusBadRequest)
				return
			}
			n = &v
		}
		if err := h.aiChatStore(ctx).SetCallBudget(ctx, sessionID, a.UserID, n); err != nil {
			jsonErr(w, err, http.StatusNotFound)
			return
		}
		if strings.TrimSpace(req.Title) == "" {
			jsonOK(w, map[string]any{"id": sessionID, "call_budget": n})
			return
		}
	}
	if strings.TrimSpace(req.Title) == "" {
		jsonErr(w, fmt.Errorf("title is required"), http.StatusBadRequest)
		return
	}
	title := strings.TrimSpace(req.Title)
	if len(title) > 120 {
		title = title[:120]
	}
	if err := h.aiChatStore(ctx).RenameSession(ctx, sessionID, a.UserID, title); err != nil {
		jsonErr(w, err, http.StatusNotFound)
		return
	}
	jsonOK(w, map[string]string{"id": sessionID, "title": title})
}

func (h *handler) aiSessionDetail(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/ai/sessions/")

	if r.Method == http.MethodDelete && !strings.Contains(path, "/") {
		h.aiDeleteSession(w, r)
		return
	}
	if r.Method == http.MethodPatch && !strings.Contains(path, "/") {
		h.aiRenameSession(w, r, path)
		return
	}
	if strings.HasSuffix(path, "/messages") {
		h.aiSendMessage(w, r)
		return
	}
	if strings.HasSuffix(path, "/promote-draft") {
		h.aiPromoteDraft(w, r)
		return
	}
	if strings.HasSuffix(path, "/discard-draft") {
		h.aiDiscardDraft(w, r)
		return
	}
	if strings.HasSuffix(path, "/proposals") && r.Method == http.MethodGet {
		h.aiListProposals(w, r)
		return
	}
	if strings.HasSuffix(path, "/documents") && r.Method == http.MethodPost {
		h.aiUploadDocument(w, r)
		return
	}
	if strings.Contains(path, "/documents/") && r.Method == http.MethodDelete {
		h.aiDeleteDocument(w, r)
		return
	}
	if strings.Contains(path, "/proposals/") {
		if strings.HasSuffix(path, "/confirm") {
			h.aiConfirmProposal(w, r)
		} else if strings.HasSuffix(path, "/reject") {
			h.aiRejectProposal(w, r)
		} else {
			jsonErr(w, fmt.Errorf("not found"), http.StatusNotFound)
		}
		return
	}
	h.aiGetSession(w, r)
}

// resolveParamRefs replaces "<created in step N>" (and similar angle-bracket
// placeholders) in a params JSON blob with the actual IDs returned by earlier
// proposal steps. This lets the LLM express cross-step dependencies without
// knowing UUIDs up front.
//
// Resolution rules (applied in order):
//  1. If the placeholder text contains "step N" (any case), use created[N-1].
//  2. A placeholder naming something — "<rolling_revenue_forecast id>" —
//     becomes that name, which the tools resolve or refuse by name. It used
//     to become the most recently created id: live, every KPI tile of a
//     dashboard plan pointed at the dashboard created in step 1.
//  3. Anything else stays as written, and the step's own check refuses it.
var placeholderRe = regexp.MustCompile(`"<[^"]*>"`)
var stepNumRe = regexp.MustCompile(`(?i)step\s*(\d+)`)

var placeholderIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// placeholderName reads "<revenue id>", "<revenue_id>" or "<revenue>" as the
// name revenue; "" when the text is not one name.
func placeholderName(inner string) string {
	s := strings.TrimSpace(inner)
	for _, suffix := range []string{" id", " ID", " Id", "_id", "_ID"} {
		s = strings.TrimSpace(strings.TrimSuffix(s, suffix))
	}
	if placeholderIdent.MatchString(s) {
		return s
	}
	return ""
}

func resolveParamRefs(params json.RawMessage, created []string) json.RawMessage {
	s := string(params)
	result := placeholderRe.ReplaceAllStringFunc(s, func(match string) string {
		// match is `"<...>"` — strip surrounding `"<` and `>"`
		inner := match[2 : len(match)-2]

		if m := stepNumRe.FindStringSubmatch(inner); m != nil {
			idx, _ := strconv.Atoi(m[1])
			idx-- // 1-based → 0-based
			if idx >= 0 && idx < len(created) && created[idx] != "" {
				return `"` + created[idx] + `"`
			}
		}

		if name := placeholderName(inner); name != "" {
			return `"` + name + `"`
		}
		return match // unresolvable — keep as-is; the step refuses it by name
	})
	return json.RawMessage(result)
}

// aiSettings dispatches GET/PUT on /api/ai/settings.
func (h *handler) aiSettings(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.aiGetSettings(w, r)
	case http.MethodPut:
		h.aiSaveSettings(w, r)
	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// aiWriteHooks hands the AI Developer's executor the gateway operations it
// must share with the developer endpoints rather than copy: the tenant's
// plan limits on metrics and members, and form-record posting.
func (h *handler) aiWriteHooks(modelID, userID string) aiassistant.Hooks {
	return aiassistant.Hooks{
		CheckMetrics: func(ctx context.Context, modelID string, adding int) error {
			cid := h.customerOfModel(ctx, modelID)
			if cid == "" || h.plans == nil {
				return nil
			}
			return h.plans.CheckMetrics(ctx, h.db.For(ctx), cid, modelID, adding)
		},
		CheckMembers: func(ctx context.Context, dimensionID string, adding int) error {
			cid := h.customerOfModel(ctx, modelID)
			if cid == "" || h.plans == nil {
				return nil
			}
			return h.plans.CheckMembers(ctx, h.db.For(ctx), cid, dimensionID, adding)
		},
		PostFormIntegration: func(ctx context.Context, integrationID string) (int, error) {
			return h.backfillFormMapping(ctx, integrationID, userID)
		},
		// As the developer console's table save: in the background.
		RepostTable: func(ctx context.Context, formID string) error {
			bgCtx := context.WithoutCancel(ctx)
			go h.repostTable(bgCtx, formID, userID)
			return nil
		},
		RevisionCopied: func(ctx context.Context, revisionID string) error {
			_, err := h.records().RebuildPostings(ctx, modelID, revisionID, userID)
			return err
		},
	}
}

// aiTurnWriteDeadline bounds one AI Developer turn or confirmation, in place
// of the server's general WriteTimeout (120 s): past the turn's own time
// limit, so a stopped turn can still say so.
func aiTurnWriteDeadline() time.Duration {
	return max(aiTurnTimeout(), 15*time.Minute) + 5*time.Minute
}

// extendAIWriteDeadline lets a long AI turn finish writing its response. A
// writer that cannot take a deadline (a test recorder) is left as it is.
func extendAIWriteDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(aiTurnWriteDeadline()))
}

// usageWithCost is a session's usage with, when the deployment prices its
// model (AI_MODEL_PRICES), what it cost — the developer sees what a build
// spends as it goes (2026-11-11: 1,769 calls before anyone priced them).
type usageWithCost struct {
	aiassistant.SessionUsage
	EstimatedCostUSD *float64 `json:"estimated_cost_usd,omitempty"`
}

// withCost prices u at model's AI_MODEL_PRICES entry, if any.
func withCost(u aiassistant.SessionUsage, model string) usageWithCost {
	out := usageWithCost{SessionUsage: u}
	if p, ok := modelPrices()[strings.ToLower(strings.TrimSpace(model))]; ok {
		c := u.EstimatedCost(p)
		out.EstimatedCostUSD = &c
	}
	return out
}

// modelPrices reads AI_MODEL_PRICES: {"<model>": {"input": $/M tokens,
// "cached": …, "output": …}, …}. Prices change and differ by contract, so the
// deployment states them; none set, no cost is shown.
func modelPrices() map[string]aiassistant.ModelPrice {
	raw := strings.TrimSpace(os.Getenv("AI_MODEL_PRICES"))
	if raw == "" {
		return nil
	}
	var in map[string]aiassistant.ModelPrice
	if json.Unmarshal([]byte(raw), &in) != nil {
		return nil
	}
	out := make(map[string]aiassistant.ModelPrice, len(in))
	for k, v := range in {
		out[strings.ToLower(strings.TrimSpace(k))] = v
	}
	return out
}

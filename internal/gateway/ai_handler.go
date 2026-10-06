package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
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
// The defaults leave room for a whole model: rebuilding a 13-sheet FP&A
// workbook took gpt-5.2 78 calls and gpt-5-mini 74 (each preview and list is
// one), past the earlier 50 per session.
const (
	defaultLLMCallsPerSession    = 200
	defaultLLMCallsPerUserPerDay = 1000
)

// sessionCapMessage says what to do when a session has used its calls: the
// confirmed work is in the session's AI draft, which a new session only
// continues from once it is promoted.
func sessionCapMessage(limit int, draftRevisionID string) string {
	if draftRevisionID != "" {
		return fmt.Sprintf("this session has reached its limit of %d LLM calls. Your confirmed changes are in its AI draft: "+
			"promote the draft (Promote to Active), then start a new session to continue from it", limit)
	}
	return fmt.Sprintf("this session has reached its limit of %d LLM calls — start a new session to continue", limit)
}

// llmCallCaps returns the per-session and per-user-per-day caps.
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
	jsonOK(w, map[string]any{"session": sess, "messages": msgs, "documents": docs})
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
	if sessionCalls >= maxLLMCallsPerSession {
		jsonErr(w, fmt.Errorf("%s", sessionCapMessage(maxLLMCallsPerSession, sess.DraftRevisionID)), http.StatusTooManyRequests)
		return
	}
	if dailyCalls >= maxLLMCallsPerUserPerDay {
		jsonErr(w, fmt.Errorf("you've reached today's limit of %d LLM calls — try again tomorrow", maxLLMCallsPerUserPerDay), http.StatusTooManyRequests)
		return
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
	systemPrompt := aiassistant.BuildSystemPrompt(mc)
	if docs, dErr := aiassistant.NewDocumentStore(h.db.For(ctx)).ListDocumentsWithContent(ctx, sessionID); dErr == nil {
		systemPrompt += buildDocumentContext(docs)
	}

	// 4. Tool executors.
	readExecutor := aiassistant.NewToolExecutor(h.db.For(ctx), modelID, revID).WithReadHooks(h.aiReadHooks(r, sessionID, modelID, a.UserID))
	writeExecutor := aiassistant.NewWriteExecutor(h.db.For(ctx), modelID, revID)
	proposalStore := aiassistant.NewProposalStore(h.db.For(ctx))
	tools := aiassistant.AllTools()

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
	provMessages := aiassistant.MessagesToProviderHistory(aiassistant.CompactHistory(history))
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
	// sentBack: a plan of this turn went back to be fixed. A model that then
	// ends the turn asking leave to propose ("confirm and I will post it" —
	// gpt-5-mini did, stage after stage) is told once to propose instead.
	sentBack, nudged := false, false

turn:
	for {
		if stopTurn() {
			return
		}
		if sessionCalls >= maxLLMCallsPerSession {
			sendSSE("error", map[string]string{"error": sessionCapMessage(maxLLMCallsPerSession, sess.DraftRevisionID)})
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
		var llmResp providers.ChatResponse
		if sp, streamable := llmProvider.(providers.StreamingProvider); streamable {
			llmResp, err = sp.ChatStream(turnCtx, chatReq, func(delta string) {
				sendSSE("delta", map[string]string{"content": delta})
			})
		} else {
			llmResp, err = llmProvider.Chat(turnCtx, chatReq)
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

		if llmResp.FinishReason != "tool_calls" || len(llmResp.Message.ToolCalls) == 0 {
			if sentBack && !nudged && asksToPropose(llmResp.Message.Content) {
				nudged = true
				_, _ = store.SaveMessage(ctx, sessionID, "assistant", llmResp.Message.Content, nil, "", "")
				provMessages = append(provMessages, llmResp.Message, providers.Message{Role: "user", Content: proposeNudge})
				continue
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
					result, execErr = readExecutor.Execute(ctx, tc.Name, tc.Arguments)
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
				sendSSE("error", map[string]string{"error": "propose_actions: invalid steps"})
				return
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
			// A plan that runs but whose formulas look wrong goes back once;
			// proposed again unchanged, it reaches the developer with them.
			warnBack := cErr == nil && len(check.problems) == 0 && len(check.warnings) > 0 && !retries.warnedBefore(check.warnings)
			if warnBack {
				warnedPlan, warnedWarnings = args.Steps, check.warnings
			}
			if cErr != nil {
				log.Printf("AI proposal check: %v", cErr)
			} else if len(check.problems) > 0 || warnBack {
				planResult := warningResult(check.warnings)
				sentBack = true
				if len(check.problems) > 0 {
					mode := retries.next(len(check.problems))
					if mode == retryPartial {
						retries.leftOut, retries.partialOf = check.problems, args.Steps
					}
					if mode == retryStop {
						sentBack = false // told to stop and explain: no nudge
					}
					planResult = check.rejection(mode)
				}
				_, _ = store.SaveMessage(ctx, sessionID, "assistant", llmResp.Message.Content, llmResp.Message.ToolCalls, "", "")
				provMessages = append(provMessages, llmResp.Message)
				for _, call := range llmResp.Message.ToolCalls {
					result := planResult
					if !aiassistant.IsWriteTool(call.Name) {
						var execErr error
						if result, execErr = readExecutor.Execute(ctx, call.Name, call.Arguments); execErr != nil {
							result = fmt.Sprintf("error: %v", execErr)
						}
					}
					_, _ = store.SaveMessage(ctx, sessionID, "tool", result, nil, call.ID, call.Name)
					provMessages = append(provMessages, providers.Message{Role: "tool", Content: result, ToolCallID: call.ID, ToolName: call.Name})
				}
				sendSSE("tool_status", map[string]string{"tool": "checking the plan"})
				continue turn
			}
			proposal, pErr := proposalStore.CreateProposal(ctx, sessionID, args.Steps, check.warnings)
			if pErr != nil {
				sendSSE("error", map[string]string{"error": fmt.Sprintf("save proposal: %v", pErr)})
				return
			}
			// Save the assistant's tool-call message and a placeholder tool result.
			_, _ = store.SaveMessage(ctx, sessionID, "assistant", llmResp.Message.Content,
				llmResp.Message.ToolCalls, "", "")
			created := fmt.Sprintf("Proposal created (%d step(s)) — awaiting developer confirmation.", len(args.Steps))
			if len(check.warnings) > 0 {
				created += " Shown with the plan check's warnings: " + strings.Join(check.warnings, " | ")
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
					if result, execErr = readExecutor.Execute(ctx, other.Name, other.Arguments); execErr != nil {
						result = fmt.Sprintf("error: %v", execErr)
					}
				}
				_, _ = store.SaveMessage(ctx, sessionID, "tool", result, nil, other.ID, other.Name)
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
				"warnings": check.warnings,
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
			result, execErr := readExecutor.Execute(ctx, tc.Name, tc.Arguments)
			if execErr != nil {
				result = fmt.Sprintf("error: %v", execErr)
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
		if proposal, pErr := proposalStore.CreateProposal(ctx, sessionID, warnedPlan, warnedWarnings); pErr == nil {
			if strings.TrimSpace(finalReply) != "" {
				_, _ = store.SaveMessage(ctx, sessionID, "assistant", finalReply, nil, "", "")
			}
			_, _ = store.SaveMessage(ctx, sessionID, "assistant", fmt.Sprintf(
				"The plan below runs (%d step(s)); it is shown with the plan check's warnings, which were not answered: %s",
				len(warnedPlan), strings.Join(warnedWarnings, " | ")), nil, "", "")
			allMsgs, _ := store.ListMessages(ctx, sessionID)
			if allMsgs == nil {
				allMsgs = []aiassistant.ChatMessage{}
			}
			sendSSE("proposal", map[string]any{"proposal": proposal, "messages": allMsgs, "session": sess, "warnings": warnedWarnings})
			return
		}
	}

	// 6. Save final assistant reply.
	saved, err := store.SaveMessage(ctx, sessionID, "assistant", finalReply, nil, "", "")
	if err != nil {
		sendSSE("error", map[string]string{"error": err.Error()})
		return
	}

	allMsgs, _ := store.ListMessages(ctx, sessionID)
	if allMsgs == nil {
		allMsgs = []aiassistant.ChatMessage{}
	}
	sendSSE("done", map[string]any{
		"reply":    saved,
		"messages": allMsgs,
		"session":  sess,
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

	// Resolve model/revision for the write executor. AI writes never target the
	// active revision directly: the session's first confirmed proposal lazily
	// creates an isolated draft (a full copy of whatever's active) and every
	// later proposal in the same session reuses it. The developer promotes it
	// to active (PUT /api/developer/revisions/{id}/activate) or discards it
	// (the existing DELETE) once they're happy — or not — with the result.
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
	revID := sess.DraftRevisionID
	if revID == "" {
		// Millisecond precision: minute-granularity previously collided with
		// model.revision's UNIQUE(model_id, name) whenever a second draft
		// was created within the same clock-minute (e.g. promote, then
		// immediately confirm another proposal) — confirm would fail with a
		// raw constraint-violation 500 instead of quietly creating the next
		// draft.
		draftParams, _ := json.Marshal(map[string]string{
			"name": fmt.Sprintf("AI Draft %s", time.Now().Format("2006-01-02 15:04:05.000")),
		})
		draftExecutor := aiassistant.NewWriteExecutorWithActor(h.db.For(ctx), modelID, "", a.UserID)
		_, newRevID, dErr := draftExecutor.Execute(ctx, "create_revision", draftParams)
		if dErr != nil {
			jsonErr(w, fmt.Errorf("create draft revision: %w", dErr), http.StatusInternalServerError)
			return
		}
		if dErr := h.aiChatStore(ctx).SetDraftRevisionID(ctx, sessionID, newRevID); dErr != nil {
			jsonErr(w, fmt.Errorf("save draft revision: %w", dErr), http.StatusInternalServerError)
			return
		}
		revID = newRevID
		sess.DraftRevisionID = newRevID
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
		result, createdID, execErr := executor.Execute(ctx, step.Tool, resolved)
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
	// Calculated metrics the plan created, changed or placed on a grid get
	// their results now, as they do when a developer saves one: an AI-built
	// P&L stayed blank until an input happened to change.
	go h.recalcRevisionCalculated(context.WithoutCancel(ctx), modelID, revID) //nolint:contextcheck

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

// aiPromoteDraft makes the session's isolated draft revision (created
// lazily on its first confirmed proposal — see aiConfirmProposal above) the
// model's active revision, then clears the session's draft_revision_id.
// That clearing matters for two reasons: it's what makes the "AI draft"
// banner disappear (the frontend's only feedback that promotion actually
// happened — previously nothing cleared it, so the banner and its buttons
// stayed up unchanged after a successful promote and the action looked
// like a no-op), and it means the session's *next* confirmed proposal
// lazily creates a fresh draft instead of continuing to write straight
// into what is now the live model, silently defeating the isolation this
// mechanism exists for.
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

	modelID, err := h.activateRevision(ctx, draftRevID)
	if err != nil {
		if metricformula.IsValidationError(err) {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		jsonErr(w, err, http.StatusNotFound)
		return
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryModelChange, EventType: auditlog.EventRevisionActivated,
		ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
		ResourceType: "revision", ResourceID: draftRevID, RevisionID: draftRevID,
		Metadata: map[string]string{"source": "ai_assistant_draft", "session_id": sessionID},
	})
	// The manual create_metric/update_metric handlers each trigger this
	// per-write; write_executor's AI equivalents never do (no access to
	// *handler across the aiassistant/gateway package boundary), so an
	// AI-authored metric/dimension change had no schema-migration path at
	// all until now. One pass here — right after the draft's structural
	// writes become the live model — reconciles it, rather than per
	// tool-call inside aiConfirmProposal against draft data nothing can
	// query yet.
	go h.autoMigrate(context.Background(), modelID) //nolint:contextcheck
	// The assistant's member, property and formula writes landed on the
	// draft, where nothing recalculated them (write_executor has no
	// scheduler); activateRevision above recomputes every calculated metric
	// of the promoted revision, so what the draft changed is what the live
	// model shows (contract C8).

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
	_, _ = chatStore.SaveMessage(ctx, sessionID, "assistant", promotedMessage, nil, "", "")
	allMsgs, _ := chatStore.ListMessages(ctx, sessionID)
	if allMsgs == nil {
		allMsgs = []aiassistant.ChatMessage{}
	}

	jsonOK(w, map[string]any{"session": sess, "messages": allMsgs})
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
	if err := h.aiChatStore(personalCtx(ctx, a)).SaveSettings(personalCtx(ctx, a), a.UserID, req.Provider, req.Model, req.APIKey); err != nil {
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
// docx, csv, txt, md, json); only the extracted text is stored, and it is
// injected into the system prompt on every subsequent chat call in the session.
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
		Provider string `json:"provider"`
		Model    string `json:"model"`
		APIKey   string `json:"api_key"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	// The person's own settings and key (personalCtx).
	pctx := personalCtx(ctx, a)
	store := h.aiChatStore(pctx)
	settings, _ := store.GetSettings(pctx, a.UserID)
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

	jsonOK(w, probeProviderKey(ctx, provider, model, apiKey))
}

// probeProviderKey verifies one (provider, model, key) combination with a
// minimal live call and returns the {ok, provider, model, …} body both the
// per-user and the tenant settings screens render inline. Shared so the two
// screens cannot drift on what "the key works" means.
func probeProviderKey(ctx context.Context, provider, model, apiKey string) map[string]any {
	p, err := buildProvider(provider, apiKey)
	if err != nil {
		return map[string]any{"ok": false, "provider": provider, "model": model, "error": err.Error()}
	}

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
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Title) == "" {
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
	}
}

// aiTurnWriteDeadline bounds one AI Developer turn or confirmation, in place
// of the server's general WriteTimeout (120 s).
const aiTurnWriteDeadline = 15 * time.Minute

// extendAIWriteDeadline lets a long AI turn finish writing its response. A
// writer that cannot take a deadline (a test recorder) is left as it is.
func extendAIWriteDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(aiTurnWriteDeadline))
}

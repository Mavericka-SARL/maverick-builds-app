package gateway

// Carried context, made right (owner, 2026-11-11: "AI Developer must read
// context from other sessions in new sessions. Such as plan."). The High-Tech
// build carried its plan from session to session, but wrong: every stage still
// "pending" after 29 turns, finished stages redone, and the session-1 answer
// "percent units" lost and asked again. Two fixes live here; the third — a
// covered stage finished on confirm — is TaskPlan.RecordConfirmed.
//   - A carried plan's open stages are checked against the model on the
//     session's first turn: what it already holds is marked, not redone.
//   - The developer's answers to the AI's questions become the plan's
//     decisions, shown in every later turn and session.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

const carriedCheckPrompt = `You check which items of a build plan a model already holds. You get the plan's open
stages, each with its items (as the plan names them), and an inventory of what the model holds (metrics, grids, pages,
workflows, tables). For each stage, list the items the inventory shows are built: an item is built when the metrics,
grids or pages it names (or clearly means) are there. When unsure, leave the item out. Reply with JSON only:
{"found": {"<stage number>": ["<item, as the plan names it>", …], …}} — {} when nothing is built.`

// checkCarriedPlan marks, once per carried plan, the stages of plan the
// model already holds (a call of the session's model, which a mini model
// does reliably: a list against an inventory). It reports whether plan
// changed.
func (h *handler) checkCarriedPlan(ctx context.Context, store *aiassistant.ChatStore, p providers.Provider, model, sessionID, revID string, plan *aiassistant.TaskPlan) bool {
	if plan == nil || plan.CarriedFrom == "" || plan.CarriedChecked || p == nil {
		return false
	}
	plan.CarriedChecked = true
	var stages strings.Builder
	open := 0
	for i, st := range plan.Stages {
		if st.Status == aiassistant.StageDone || st.Status == aiassistant.StageSkipped || len(st.Items) == 0 {
			continue
		}
		open++
		fmt.Fprintf(&stages, "Stage %d: %s — items: %s\n", i+1, st.Title, strings.Join(st.Items, "; "))
	}
	if open > 0 {
		inv := modelInventory(ctx, h.db.For(ctx), revID)
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		resp, err := p.Chat(cctx, providers.ChatRequest{Model: model, SystemPrompt: carriedCheckPrompt,
			Messages: []providers.Message{{Role: "user", Content: "Open stages:\n" + stages.String() + "\nThe model holds:\n" + inv}}})
		cancel()
		if err == nil {
			if u := resp.Usage; u.InputTokens+u.CacheReadTokens > 0 {
				_ = store.AddUsage(context.WithoutCancel(ctx), sessionID, u)
			}
			plan.MarkFound(parseFoundStages(resp.Message.Content, plan))
		}
	}
	_ = store.SetTaskPlan(ctx, sessionID, plan)
	return true
}

// parseFoundStages reads {"found": {"2": [...]}} into 0-based stages, keeping
// only items the stage has.
func parseFoundStages(reply string, plan *aiassistant.TaskPlan) map[int][]string {
	start, end := strings.Index(reply, "{"), strings.LastIndex(reply, "}")
	if start < 0 || end <= start {
		return nil
	}
	var out struct {
		Found map[string][]string `json:"found"`
	}
	if json.Unmarshal([]byte(reply[start:end+1]), &out) != nil {
		return nil
	}
	found := map[int][]string{}
	for k, items := range out.Found {
		var n int
		if _, err := fmt.Sscanf(strings.TrimSpace(k), "%d", &n); err != nil || n < 1 || n > len(plan.Stages) {
			continue
		}
		have := map[string]string{}
		for _, it := range plan.Stages[n-1].Items {
			have[strings.ToLower(strings.TrimSpace(it))] = it
		}
		for _, it := range items {
			if orig, ok := have[strings.ToLower(strings.TrimSpace(it))]; ok {
				found[n-1] = append(found[n-1], orig)
			}
		}
	}
	return found
}

// answeredDecision is the decision a developer's message makes, when it
// answers a question the AI's last reply asked: "" otherwise. A yes to a
// request for leave decides nothing (asksPermission), nor does a question.
func answeredDecision(history []aiassistant.ChatMessage, msg string) string {
	msg = strings.TrimSpace(msg)
	if msg == "" || strings.HasSuffix(msg, "?") || utf8.RuneCountInString(msg) > 800 || isBareGoAhead(msg) {
		return ""
	}
	// The AI's reply before this message (history ends with this message).
	var last string
	for i := len(history) - 1; i >= 0; i-- {
		m := history[i]
		if m.Role == "user" && strings.TrimSpace(m.Content) == msg {
			continue
		}
		if m.Role == "assistant" && strings.TrimSpace(m.Content) != "" && len(m.ToolCalls) == 0 {
			last = m.Content
			break
		}
		if m.Role == "user" {
			break
		}
	}
	if last == "" || asksPermission(last) {
		return ""
	}
	q := lastQuestion(last)
	if q == "" && !presentsChoices(last) {
		return ""
	}
	if q == "" {
		q = "the options the AI laid out"
	}
	return fmt.Sprintf("Asked: %s — the developer answered: %s", q, msg)
}

// lastQuestion is the last sentence of reply that asks something (≤ 300
// characters), or "".
func lastQuestion(reply string) string {
	idx := strings.LastIndex(reply, "?")
	if idx < 0 {
		return ""
	}
	// A sentence ends at ". ", "! " or a line end — not at the point of
	// "0.025".
	head := reply[:idx]
	start := -1
	for _, sep := range []string{". ", "! ", "\n", "? "} {
		if i := strings.LastIndex(head, sep); i >= 0 && i+len(sep)-1 > start {
			start = i + len(sep) - 1
		}
	}
	q := strings.TrimSpace(reply[start+1 : idx+1])
	q = strings.TrimLeft(q, "-*• ")
	if r := []rune(q); len(r) > 300 {
		q = "…" + string(r[len(r)-300:])
	}
	return q
}

// isBareGoAhead: "yes", "ok, go on", "continue" — leave, not a decision.
func isBareGoAhead(msg string) bool {
	low := strings.ToLower(strings.Trim(strings.TrimSpace(msg), ".!"))
	if len(strings.Fields(low)) > 4 {
		return false
	}
	for _, w := range []string{"yes", "ok", "okay", "go", "go on", "go ahead", "continue", "proceed", "sure", "confirmed", "do it", "please do", "yes please", "yes, go on", "ok, continue"} {
		if low == w {
			return true
		}
	}
	return false
}

// recordDecision adds the decision msg makes to the session's plan (a plan
// of decisions only when there is none), so every later turn and session
// reads it.
func recordDecision(ctx context.Context, store *aiassistant.ChatStore, sessionID string, plan *aiassistant.TaskPlan, history []aiassistant.ChatMessage, msg string) *aiassistant.TaskPlan {
	d := answeredDecision(history, msg)
	if d == "" {
		return plan
	}
	if plan == nil {
		plan = &aiassistant.TaskPlan{}
	}
	if plan.AddDecision(d) {
		_ = store.SetTaskPlan(ctx, sessionID, plan)
	}
	return plan
}

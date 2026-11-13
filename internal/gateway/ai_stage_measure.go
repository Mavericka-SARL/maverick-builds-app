package gateway

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// routineReplyRE: a developer message that only confirms or continues —
// not a correction (owner, 2026-11-12: human corrections per stage).
var routineReplyRE = regexp.MustCompile(`(?i)^(confirmed|confirm|continue|go ahead|go on|carry on|yes|ok|okay|proceed|next|looks good|good)\b`)

// isCorrection reports whether a developer message corrects or steers the
// build, rather than confirming or continuing it.
func isCorrection(msg string) bool {
	m := strings.TrimSpace(msg)
	if m == "" {
		return false
	}
	return !routineReplyRE.MatchString(m) || len(m) > 120 || strings.Contains(m, "?")
}

// measureTurn starts measuring a turn of plan's current stage and returns
// what records it when the turn ends (a no-op when no stage is current):
// the turn's minutes, model calls, tokens and tool results, and whether the
// developer's message corrected the build. The stage is found again by its
// title at the end, as the turn may re-cut the plan.
func (h *handler) measureTurn(ctx context.Context, store *aiassistant.ChatStore, sessionID string, plan *aiassistant.TaskPlan, msg string) func() {
	cur := plan.Current()
	if cur < 0 {
		return func() {}
	}
	title := plan.Stages[cur].Title
	start := time.Now()
	u0, err := store.Usage(ctx, sessionID)
	if err != nil {
		return func() {}
	}
	t0, _ := store.CountToolMessages(ctx, sessionID)
	correction := isCorrection(msg)
	return func() {
		bg := context.WithoutCancel(ctx)
		u1, err := store.Usage(bg, sessionID)
		if err != nil {
			return
		}
		t1, _ := store.CountToolMessages(bg, sessionID)
		p, err := store.TaskPlan(bg, sessionID)
		if err != nil || p == nil {
			return
		}
		i := -1
		for j, s := range p.Stages {
			if s.Title == title {
				i = j
				break
			}
		}
		p.AddTurn(i, aiassistant.TurnMeasure{
			Minutes: time.Since(start).Minutes(), Calls: u1.Calls - u0.Calls, ToolCalls: t1 - t0,
			InputTokens: u1.InputTokens - u0.InputTokens, CachedTokens: u1.CacheReadTokens - u0.CacheReadTokens,
			OutputTokens: u1.OutputTokens - u0.OutputTokens, Correction: correction,
		})
		_ = store.SetTaskPlan(bg, sessionID, p)
	}
}

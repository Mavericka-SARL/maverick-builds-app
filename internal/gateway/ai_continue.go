package gateway

// Going on after a confirmed plan (owner, 2026-11-13: "when I confirm and the
// plan step is executed, the AI developer must provide the next step and not
// wait for user commands"). The console sends {continue: true} once a
// confirmed plan has run; the turn's message is written here from what ran
// and the plan under way, shown in the chat as an automatic line.

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// continuationMessage is the message of a turn that goes on after the
// session's last plan ran. It is refused when the last plan has not run (a
// pending, rejected or no plan): there is nothing to go on from.
func (h *handler) continuationMessage(ctx context.Context, store *aiassistant.ChatStore, sessionID string) (string, error) {
	props, err := aiassistant.NewProposalStore(h.db.For(ctx)).ListProposals(ctx, sessionID)
	if err != nil {
		return "", err
	}
	// The developer just approved the plan: build its first open stage.
	if plan, _ := store.TaskPlan(ctx, sessionID); plan != nil && plan.ApprovedAt != nil && !plan.PendingApproval &&
		(len(props) == 0 || props[0].CreatedAt.Before(*plan.ApprovedAt)) {
		return aiassistant.PlanApprovedMessage(plan), nil
	}
	if len(props) == 0 || (props[0].Status != "executed" && props[0].Status != "partial") {
		return "", fmt.Errorf("nothing to go on from: the session's last plan has not run")
	}
	last := props[0]
	failed := 0
	for _, s := range last.Steps {
		if s.Status == "failed" {
			failed++
		}
	}
	plan, _ := store.TaskPlan(ctx, sessionID)
	return aiassistant.ContinuationMessage(len(last.Steps), failed, plan), nil
}

// ── POST /api/ai/sessions/{id}/plan/approve ─────────────────────────────────
//
// The developer approves the session's plan, which waits for it (owner,
// 2026-11-13: the general plan is approved before its first stage is built).
// The console then goes on by itself ({continue: true}): stage 1 is proposed.

func (h *handler) aiApprovePlan(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	sessionID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/ai/sessions/"), "/plan/approve")
	store := h.aiChatStore(ctx)
	sess, err := store.GetSession(ctx, sessionID)
	if err != nil || sess.UserID != a.UserID {
		jsonErr(w, fmt.Errorf("session not found"), http.StatusNotFound)
		return
	}
	plan, err := store.TaskPlan(ctx, sessionID)
	if err != nil || plan == nil || len(plan.Stages) == 0 {
		jsonErr(w, fmt.Errorf("this session has no plan to approve"), http.StatusNotFound)
		return
	}
	if !plan.AwaitsApproval() {
		jsonOK(w, map[string]any{"task_plan": plan})
		return
	}
	plan.Approve()
	if err := store.SetTaskPlan(ctx, sessionID, plan); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"task_plan": plan})
}

var trailingNumberRE = regexp.MustCompile(`^(.*?)\s*(\d+)$`)

// nextFreeRevisionName is name, numbered on ("Create a Model 2", "… 3"), as
// no revision of the model is named yet (names compare without case).
func (h *handler) nextFreeRevisionName(ctx context.Context, modelID, name string) string {
	taken := map[string]bool{}
	if rows, err := h.db.For(ctx).Query(ctx, `SELECT lower(name) FROM model.revision WHERE model_id = $1::uuid`, modelID); err == nil {
		for rows.Next() {
			var n string
			if rows.Scan(&n) == nil {
				taken[n] = true
			}
		}
		rows.Close()
	}
	base, n := strings.TrimSpace(name), 2
	if m := trailingNumberRE.FindStringSubmatch(base); m != nil && strings.TrimSpace(m[1]) != "" {
		base = strings.TrimSpace(m[1])
		if v, err := strconv.Atoi(m[2]); err == nil {
			n = v + 1
		}
	}
	for ; n < 1000; n++ {
		if c := fmt.Sprintf("%s %d", base, n); !taken[strings.ToLower(c)] {
			return c
		}
	}
	return base
}

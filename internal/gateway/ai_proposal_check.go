package gateway

// Checking a proposal before the developer sees it. Reported live: the AI
// Developer proposed calculated metrics whose formulas named a dimension by
// a guessed spelling (setup_item for "Setup Item") and read a property as
// if it were a dimension; confirming failed those steps, and every later
// step built on them failed too. Now each propose_actions call is first run
// exactly as confirming would run it — the same executor, the same
// validation — inside a transaction that is always rolled back, and a plan
// that would fail goes back to the assistant with the errors instead of to
// the developer.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// maxProposalRejections is how many failing plans the assistant may send
// back in one turn before it has to stop and explain.
const maxProposalRejections = 3

// proposalCheck is what a dry run of a proposal found.
type proposalCheck struct {
	problems  []string // one line per step that would fail
	unchecked []int    // 1-based steps a dry run cannot run (or that use their results)
}

// aiCheckProposal runs steps on modelID/revID as confirming them would, in
// a transaction it rolls back. A step that cannot run in a dry run
// (aiassistant.ErrNotDryRunnable) — and every step that uses its result —
// is left to the confirmation; any other failure is a problem.
func (h *handler) aiCheckProposal(ctx context.Context, modelID, revID, userID string, steps []aiassistant.ProposalStep) (proposalCheck, error) {
	var out proposalCheck
	tx, err := h.db.For(ctx).Begin(ctx)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	live := h.aiWriteHooks(modelID, userID)
	ex := aiassistant.NewDryRunWriteExecutor(tx, modelID, revID, userID).
		WithHooks(aiassistant.Hooks{CheckMetrics: live.CheckMetrics, CheckMembers: live.CheckMembers})
	created := make([]string, len(steps))
	blocked := map[int]bool{} // 0-based steps whose result later steps cannot use
	for i, step := range steps {
		if _, ok := usesBlockedStep(step.Params, blocked); ok {
			blocked[i] = true
			out.unchecked = append(out.unchecked, i+1)
			continue
		}
		sp, err := tx.Begin(ctx) // a savepoint: one failure must not poison the rest
		if err != nil {
			return out, err
		}
		id, stepErr := runChecked(ctx, ex, step.Tool, resolveParamRefs(storedParams(step.Params), created[:i]))
		switch {
		case errors.Is(stepErr, aiassistant.ErrNotDryRunnable):
			_ = sp.Rollback(ctx)
			blocked[i] = true
			out.unchecked = append(out.unchecked, i+1)
		case stepErr != nil:
			_ = sp.Rollback(ctx)
			blocked[i] = true
			out.problems = append(out.problems, fmt.Sprintf("step %d (%s — %s): %v", i+1, step.Tool, step.Description, stepErr))
		default:
			if err := sp.Commit(ctx); err != nil {
				return out, err
			}
			created[i] = id
		}
	}
	return out, nil
}

// storedParams is a step's params as confirming reads them back from the
// database: JSONB keeps < and > as they are, while a tool call's JSON may
// spell them \u003c and \u003e (Go's encoder does), which would hide a
// "<created in step N>" placeholder from resolveParamRefs.
func storedParams(raw json.RawMessage) json.RawMessage {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) != nil {
		return raw
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) != nil {
		return raw
	}
	return json.RawMessage(bytes.TrimSpace(buf.Bytes()))
}

// runChecked runs one step, turning a panic into an unchecked step: a dry
// run must never take the chat down with it.
func runChecked(ctx context.Context, ex *aiassistant.WriteExecutor, tool string, params json.RawMessage) (id string, err error) {
	defer func() {
		if r := recover(); r != nil {
			id, err = "", aiassistant.ErrNotDryRunnable
		}
	}()
	_, id, err = ex.Execute(ctx, tool, params)
	return id, err
}

// usesBlockedStep reports whether params refer to the result of a blocked
// step ("<created in step N>"), and which.
func usesBlockedStep(params json.RawMessage, blocked map[int]bool) (int, bool) {
	for _, m := range placeholderRe.FindAllString(string(storedParams(params)), -1) {
		if n := stepNumRe.FindStringSubmatch(m); n != nil {
			if idx, err := strconv.Atoi(n[1]); err == nil && blocked[idx-1] {
				return idx, true
			}
		}
	}
	return 0, false
}

// rejection is the tool result a failing plan gets: what fails, and what to
// do about it.
func (c proposalCheck) rejection(attempt int) string {
	var sb strings.Builder
	sb.WriteString("Proposal NOT shown to the developer: run against the model exactly as confirming would, it fails —\n")
	for _, p := range c.problems {
		sb.WriteString("- " + p + "\n")
	}
	if len(c.unchecked) > 0 {
		fmt.Fprintf(&sb, "(Steps %s could not be checked: they change workflows, forms, access rules or import files, or use the result of a step that failed.)\n", joinInts(c.unchecked))
	}
	if attempt >= maxProposalRejections {
		sb.WriteString("Do not propose again in this turn. Tell the developer which steps fail and why, and ask how to proceed.")
	} else {
		sb.WriteString("Fix every failing step — read the current names with list_metrics / list_dimensions if needed — and call propose_actions again with the WHOLE corrected plan.")
	}
	return sb.String()
}

func joinInts(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ", ")
}

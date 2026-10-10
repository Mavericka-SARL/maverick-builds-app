package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// One AI Developer turn: its time limit, what a stopped turn says, the
// progress shown while a long proposal is being written, how many failing
// plans it may send back, and the end of a session whose draft was promoted
// (found rebuilding a sales target-setting workbook, 2026-10-04: two turns
// ran past 15 minutes with no feedback and no way to stop them, five asked
// "shall I propose it?" after three refused plans, and a promoted session
// went on checking plans against its empty base revision).

// promotedMessage is the session's last message once its draft is promoted.
const promotedMessage = "The draft is now your working revision. It is not live: make it the active revision with " +
	"Set active in Developer › Models when it is ready. This session is finished: start a new session to keep building; " +
	"it works on your working revision."

// sessionPromotedError refuses a message in a promoted session.
const sessionPromotedError = "this session's draft became your working revision, so the session is finished — " +
	"start a new session to keep building; it works on your working revision"

func jsonCodeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg, "code": code})
}

// errTurnTimeLimit is the cause of a turn stopped by its time limit.
var errTurnTimeLimit = errors.New("turn time limit")

// aiTurnTimeout is how long one turn may run: AI_TURN_TIMEOUT (a Go
// duration, e.g. "15m"), else 10 minutes.
func aiTurnTimeout() time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(os.Getenv("AI_TURN_TIMEOUT"))); err == nil && d > 0 {
		return d
	}
	return 10 * time.Minute
}

// stoppedTurn says why a turn's context ended, or "" while it runs: the
// time limit, or the developer pressing Stop (the request closed).
func stoppedTurn(turnCtx context.Context, limit time.Duration) string {
	if turnCtx.Err() == nil {
		return ""
	}
	if errors.Is(context.Cause(turnCtx), errTurnTimeLimit) {
		return fmt.Sprintf("⏱ Stopped after %s: the turn took too long. Ask for a smaller stage — and load sheet data with import_file_data rather than typing the values into a proposal.", limit.Round(time.Minute))
	}
	return "⏹ Stopped by the developer."
}

// toolProgress reports, at most once a second, how much of a tool call the
// model has written so far: a long proposal shows its steps as they come.
type toolProgress struct {
	send func(event string, payload any)
	last time.Time
}

func (p *toolProgress) onArgs(name string, args []byte) {
	if time.Since(p.last) < time.Second {
		return
	}
	p.last = time.Now()
	payload := map[string]any{"tool": name, "chars": len(args)}
	if name == "propose_actions" {
		payload["steps"] = bytes.Count(args, []byte(`"tool"`))
	}
	p.send("progress", payload)
}

// planRetries decides what a failing plan's tool result asks for. A plan may
// be corrected maxProposalRejections times, and more while each attempt
// fails fewer steps than the last (up to maxFallingRejections); then the
// model is asked once for the steps that pass, and after that it stops and
// says what fails — it never asks whether to go on.
type planRetries struct {
	attempts       int
	lastProblems   int
	partialAsked   bool
	warnedSameOnce map[string]bool
	// leftOut are the failing steps of the plan the passing steps were
	// asked for: the developer is told them beside the proposal, whatever
	// the model says.
	leftOut []string
	// partialOf is that plan: a step of it the passing plan does not have,
	// failing or not, is named too — asked for "only the steps that pass",
	// the model once dropped two whole dashboards and the note named two
	// failing steps.
	partialOf []aiassistant.ProposalStep
}

// proposeNudge is sent once to a model that ends a turn with a question
// after its plan went back to be fixed: the developer is waiting for the
// plan, and confirming it is how they say yes.
const proposeNudge = "(Sent by the server, not the developer.) Your plan went back to be fixed and you ended without proposing. " +
	"Do not ask the developer whether to propose — confirming the proposal is how they answer. Call propose_actions now with the whole corrected plan; " +
	"if a step cannot be fixed, propose the steps that pass and say which you left out."

// asksToPropose reports whether a reply asks the developer's leave to
// propose — "Confirm and I will post it", "If you want me to proceed …",
// "Shall I propose …?", as gpt-5-mini ended stage after stage — rather than
// explaining why no plan can be made ("FR is not a country in the model"),
// which is a fine end of a turn and is not nudged.
func asksToPropose(reply string) bool {
	r := strings.ToLower(reply)
	for _, p := range []string{"confirm and i", "confirm you", "confirm if you", "shall i", "should i propose", "do you want me",
		"would you like me", "if you want me to", "want me to proceed", "may i ", "ready to propose", "i will post", "i'll post"} {
		if strings.Contains(r, p) {
			return true
		}
	}
	return false
}

// maxFallingRejections caps the corrections while the failures keep falling.
const maxFallingRejections = 8

type retryMode int

const (
	retryFix     retryMode = iota // fix every failing step and propose the whole plan
	retryPartial                  // propose only the steps that pass
	retryStop                     // stop and say what fails
)

func (r *planRetries) next(problems int) retryMode {
	r.attempts++
	falling := r.attempts > 1 && problems < r.lastProblems
	r.lastProblems = problems
	switch {
	case r.attempts < maxProposalRejections || (falling && r.attempts < maxFallingRejections):
		return retryFix
	case !r.partialAsked:
		r.partialAsked = true
		return retryPartial
	default:
		return retryStop
	}
}

// warnedBefore reports whether these exact warnings were already sent back
// once this turn — a plan proposed again unchanged after a warning is the
// model saying it is intended, and goes to the developer with them.
func (r *planRetries) warnedBefore(warnings []string) bool {
	key := strings.Join(warnings, "\n")
	if r.warnedSameOnce == nil {
		r.warnedSameOnce = map[string]bool{}
	}
	if r.warnedSameOnce[key] {
		return true
	}
	r.warnedSameOnce[key] = true
	return false
}

// warningResult is the tool result of a plan that runs but whose formulas
// look wrong.
func warningResult(warnings []string) string {
	var sb strings.Builder
	sb.WriteString("Proposal NOT shown yet: it runs, but these steps look wrong —\n")
	for _, w := range warnings {
		sb.WriteString("- " + w + "\n")
	}
	sb.WriteString("Fix them and call propose_actions again with the WHOLE plan. If one is intended, propose it again unchanged: it is then shown to the developer with the warning.")
	return sb.String()
}

// leftOutNote is the message saved beside a proposal of only the passing
// steps: what the plan check refused, so the developer sees what is missing.
func leftOutNote(problems []string, dropped []string) string {
	var sb strings.Builder
	sb.WriteString("The plan check refused these steps, so they are left out of this proposal:\n")
	for _, p := range problems {
		sb.WriteString("- " + p + "\n")
	}
	if len(dropped) > 0 {
		sb.WriteString("Also left out — they passed, or used a step that failed:\n")
		for _, d := range dropped {
			sb.WriteString("- " + d + "\n")
		}
	}
	sb.WriteString("Confirm what is proposed, then say how to fix the rest.")
	return sb.String()
}

// droppedSteps names the steps of the failing plan the passing plan lacks
// (matched by tool and params), except the failing ones the problems name
// already ("step N (…").
func droppedSteps(failing, passing []aiassistant.ProposalStep, problems []string) []string {
	key := func(s aiassistant.ProposalStep) string { return s.Tool + "\x00" + compactJSON(s.Params) }
	kept := map[string]int{}
	for _, s := range passing {
		kept[key(s)]++
	}
	named := map[int]bool{}
	for _, p := range problems {
		var n int
		if _, err := fmt.Sscanf(p, "step %d", &n); err == nil {
			named[n] = true
		}
	}
	var out []string
	for i, s := range failing {
		if kept[key(s)] > 0 {
			kept[key(s)]--
			continue
		}
		if named[i+1] {
			continue
		}
		label := s.Description
		if label == "" {
			label = s.Tool
		}
		out = append(out, fmt.Sprintf("step %d (%s — %s)", i+1, s.Tool, label))
	}
	return out
}

func compactJSON(raw json.RawMessage) string {
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return string(raw)
	}
	return b.String()
}

package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
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
	"Set active in Developer › Developer Models when it is ready. This session is finished: start a new session to keep building; " +
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
// duration, e.g. "15m"), else 30 minutes. Ten minutes stopped a tester's
// check of a large model's loaded data mid-way (2026-10-24); a long turn
// shows its progress and the developer can Stop it.
func aiTurnTimeout() time.Duration {
	if d, err := time.ParseDuration(strings.TrimSpace(os.Getenv("AI_TURN_TIMEOUT"))); err == nil && d > 0 {
		return d
	}
	return 30 * time.Minute
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
	lastSig        string
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

// unproposedNudge goes back once when a turn ends with no proposal but its
// reply says the model was changed or a proposal is coming: nothing in the
// model changes until a proposal is confirmed. gpt-5-mini ended five stages
// of the restaurant rebuild that way ("I have created the Fleet metrics",
// "I will now propose the P&L"), and the developer saw nothing to confirm.
const unproposedNudge = "(Sent by the server, not the developer.) You ended this turn without proposing anything, so nothing in the model has changed — " +
	"although your reply says it has, or that a proposal follows. If the developer asked for changes, call propose_actions now with the whole plan; " +
	"if part of it cannot be done, propose the rest and say plainly which part you left out and why. If they asked only a question, answer it without saying anything was changed."

// announcesProposal: a reply saying a plan is about to be sent — "I'll send
// the corrected single proposal now", "I will submit the plan" — which four
// turns of the Creator & Agency build (2026-11-08) ended with, proposing
// nothing; the fixed phrases below had no send, submit or post.
var announcesProposal = regexp.MustCompile(`(?i)\b(i'?ll|i will|i am going to|i'm going to|let me)\s+(now\s+)?(send|submit|post|prepare|propose|resubmit|re-?propose|re-?send|put together)\b[^.?!\n]{0,80}\b(proposal|plan|steps)\b`)

// pointsAtPlan: a reply sending the developer to a plan — "I've prepared
// the plan — please confirm above", "the plan is ready for your review" —
// which gpt-5-mini and gpt-5.4-mini ended turns with after refusals,
// proposing nothing (2026-11-11). Nudged only when no proposal is pending.
var pointsAtPlan = regexp.MustCompile(`(?i)(\b(i'?ve|i have)\s+(now\s+)?prepared\b|\b(plan|proposal)\s+(is\s+)?(prepared|ready)\b|\bprepared\s+(the|a|your)\s+(corrected\s+|updated\s+|new\s+)?(plan|proposal)\b|\bconfirm\s+(it\s+|the\s+(plan|proposal)\s+)?above\b|\breview\s+and\s+confirm\b|\b(plan|proposal)\s+above\b)`)

// claimsUnproposedChange reports whether a reply, ending a turn that
// proposed nothing, says the model was changed or announces a proposal.
func claimsUnproposedChange(reply string) bool {
	if announcesProposal.MatchString(reply) {
		return true
	}
	r := strings.ToLower(reply)
	for _, p := range []string{"i have created", "i've created", "i have added", "i've added", "i have updated", "i've updated",
		"i have made the", "i've made the", "i have set", "i've set", "i have fixed", "i've fixed", "i have removed", "i've removed",
		"has been created", "have been created", "has been added", "have been added", "has been updated", "have been updated",
		"changes have been made", "changes are in place", "is now in place", "are now in place",
		"i will now propose", "i'll now propose", "i will propose", "i'll propose", "next i will propose", "here is the plan", "here's the plan"} {
		if strings.Contains(r, p) {
			return true
		}
	}
	return false
}

// asksToPropose reports whether a reply asks the developer's leave to
// propose — "Confirm and I will post it", "If you want me to proceed …",
// "Shall I propose …?", as gpt-5-mini ended stage after stage — rather than
// explaining why no plan can be made ("FR is not a country in the model"),
// which is a fine end of a turn and is not nudged.
func asksToPropose(reply string) bool {
	// A reply laying out options is a real question about the model (which
	// reading, which source wins), not a request for permission: the
	// developer answers it (owner, 2026-11-07: "if the AI Developer is not
	// sure, it must ask").
	if presentsChoices(reply) {
		return false
	}
	r := strings.ToLower(reply)
	for _, p := range []string{"confirm and i", "confirm you", "confirm if you", "shall i", "should i propose", "do you want me",
		"would you like me", "if you want me to", "want me to proceed", "may i ", "ready to propose", "i will post", "i'll post"} {
		if strings.Contains(r, p) {
			return true
		}
	}
	return false
}

// asksPermission reports a reply asking leave for work the developer asked
// for, or for a step that changes nothing in the model — "Shall I split it
// into parts?", "run compare_with_sheet now, or continue?" (2026-11-11, six
// times in one build) — rather than a choice that changes the model, which
// it lays out as options and the developer answers.
func asksPermission(reply string) bool {
	if presentsChoices(reply) {
		return false
	}
	if asksToPropose(reply) {
		return true
	}
	return permissionAsk.MatchString(reply)
}

// permissionAsk: questions about going on, not about the model.
var permissionAsk = regexp.MustCompile(`(?i)(\b(now|first)\s*,?\s+or\s+(continue|go on|proceed|wait|later)\b[^?]{0,60}\?|\b(shall|should|can|may)\s+(i|we)\s+(split|continue|proceed|go on|go ahead|start|run|check|re-?run|break)\b[^?]{0,120}\?|\b(proceed|continue|go ahead|go on)\s*(with[^?]{0,80})?\?\s*$|\bok(ay)?\s+to\s+(proceed|continue|go ahead|run)\b)`)

// permissionNudge answers such a question for the developer: the request
// is the leave.
const permissionNudge = "(Sent by the server, not the developer.) Do not ask the developer's leave to go on — their request is the leave, and checks you can run (compare_with_sheet, list_*) need none. " +
	"Go on with the request now: call propose_actions with the plan; if it is too large for one proposal, propose the first part now and say what comes next. " +
	"Ask only where the request or the source leaves a choice that changes the model, and then lay the options out."

// leaveToPropose: a reply holding back a plan the developer asked for, for
// leave or for a yes to defaults it named itself (2026-11-08: "Reply 'Yes' to
// accept the owner-title mapping above", "say proceed to propose this plan",
// "once you confirm the titles I will propose") — whatever options it lists.
var leaveToPropose = regexp.MustCompile(`(?i)(reply\s+(with\s*:?)?[\s\-*]*["“']?yes|say\s+["“']?(proceed|yes|go)\b|once you confirm[^.?!]{0,80}\bi will (propose|prepare)|before i (propose|prepare|build) the plan|reply ["“']?proceed)`)

// asksLeaveToPropose reports whether reply holds a plan back for leave.
func asksLeaveToPropose(reply string) bool { return leaveToPropose.MatchString(reply) }

// leaveNudge sends such a reply back: the developer answers on the plan.
const leaveNudge = "Propose the plan now, with the defaults you named: the developer confirms or changes it on the plan itself. Ask before proposing only where the workbook or the request leaves a choice that changes numbers, and then propose the parts you are sure of in the same reply."

// choiceLine is a line starting an option: "1)", "2.", "A)", "Option B".
var choiceLine = regexp.MustCompile(`(?mi)^\s*(?:[-*]\s*)?(?:\*\*)?(?:\d[.)]|[a-c]\)|option [a-c1-3]\b)`)

// presentsChoices reports whether reply offers the developer two or more
// options to choose between.
func presentsChoices(reply string) bool {
	return len(choiceLine.FindAllStringIndex(reply, -1)) >= 2
}

// maxFallingRejections caps the corrections while the failures keep falling.
const maxFallingRejections = 8

// emptyReplyNudge goes back once when a turn would end on an empty reply:
// seven turns of the Creator & Agency build (2026-11-08) ended with nothing
// shown and nothing proposed, and the developer could only say "continue".
const emptyReplyNudge = "(Sent by the server, not the developer.) Your last message was empty, so the developer saw nothing. " +
	"Go on with the request: call propose_actions with the whole plan if it is ready; otherwise say what you found, what is left and what you need."

// maxUnreadablePlans is how many propose_actions calls whose arguments are
// not a plan go back to the model in one turn before the turn ends.
const maxUnreadablePlans = 2

// unreadablePlanResult is the tool result of a propose_actions call whose
// arguments did not arrive as a plan: a long plan cut off at the model's
// output limit used to end the turn with "invalid steps", and the model
// never learned why (a 68-step value plan, 2026-11-08).
func unreadablePlanResult(chars int, cutOff bool, err error) string {
	why := "it held no steps"
	switch {
	case cutOff:
		why = fmt.Sprintf("it was cut off at your output limit after %d characters", chars)
	case err != nil:
		why = fmt.Sprintf("its arguments are not complete JSON (%d characters arrived: %v)", chars, err)
	}
	return "Proposal NOT created: propose_actions was not read — " + why + ". Propose again in parts the developer can confirm one at a time: " +
		"at most about 30 steps per call, and load sheet values with import_file_data instead of typing them into steps. " +
		"Say in your reply which part comes next."
}

// problemSignature is a plan's problems with step numbers and ids left
// out, so the same failure in a renumbered plan reads the same.
var (
	sigStepNum = regexp.MustCompile(`step \d+`)
	sigUUID    = regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)
)

func problemSignature(problems []string) string {
	norm := make([]string, len(problems))
	for i, p := range problems {
		norm[i] = sigUUID.ReplaceAllString(sigStepNum.ReplaceAllString(p, "step #"), "#id")
	}
	return strings.Join(norm, "\n")
}

// repeats reports whether problems are the last failing plan's again: the
// model is not getting closer (2026-11-08: six plans in a row failed on the
// same misnumbered "<created in step N>"), so it is asked for the steps that
// pass, or to stop, instead of a seventh try.
func (r *planRetries) repeats(problems []string) bool {
	sig := problemSignature(problems)
	same := r.lastSig != "" && sig == r.lastSig
	r.lastSig = sig
	return same
}

// escalate is the next mode once a failure repeats: the passing steps,
// then stop.
func (r *planRetries) escalate() retryMode {
	if !r.partialAsked {
		r.partialAsked = true
		return retryPartial
	}
	return retryStop
}

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

// warnedBefore reports whether every one of these warnings was already sent
// back this turn — beside a failing plan or a running one. A plan proposed
// again with only warnings it was given is the model saying they are
// intended, and goes to the developer with them; one round per warning, not
// one per combination of them.
func (r *planRetries) warnedBefore(warnings []string) bool {
	if r.warnedSameOnce == nil {
		r.warnedSameOnce = map[string]bool{}
	}
	all := true
	for _, w := range warnings {
		key := problemSignature([]string{w})
		if !r.warnedSameOnce[key] {
			all = false
		}
		r.warnedSameOnce[key] = true
	}
	return all
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

// A turn changes nothing unless it ends with a proposal the developer then
// confirms. gpt-5-mini twice replied that pages were created, reordered and
// granted to roles ("already applied") in turns whose plans the plan check
// had refused — no proposal was shown, nothing ran (2026-10-20). Under such
// a reply the server says so itself.

// claimsChange matches a reply that reports a change as made.
var claimsChange = regexp.MustCompile(`(?i)\b(i\s*(have|'ve)|i|we\s*(have|'ve)|has been|have been|is now|are now|was|were|already)\s+` +
	`(been\s+)?(created|added|applied|updated|renamed|deleted|removed|granted|reordered|moved|set up|configured|saved|changed|built|wired|connected|made)\b`)

// noChangeNote is the server's note under the final reply of a turn that
// ended without a proposal: refused says the plan check sent a plan back
// this turn. "" when the reply neither follows a refused plan nor claims a
// change (a question answered).
func noChangeNote(reply string, refused bool) string {
	switch {
	case refused:
		return "(From the server.) No change was made in this turn: the plan check refused the plan, so no proposal was shown and nothing ran. " +
			"Anything the reply above describes as done is not in the model."
	case claimsChange.MatchString(reply):
		return "(From the server.) No change was made in this turn: no proposal was made, and the model changes only when you confirm one. " +
			"Anything the reply above describes as done is not in the model."
	}
	return ""
}

// replacedToolNote is what a proposal's tool result adds when it replaced a
// pending one of steps steps.
func replacedToolNote(steps int) string {
	return fmt.Sprintf(" It replaces the earlier pending proposal (%d step(s)), which is withdrawn and will not run: "+
		"whatever of it is still wanted must be in this proposal, or proposed again after it.", steps)
}

// replacedChatNote tells the developer the same.
func replacedChatNote(steps int) string {
	return fmt.Sprintf("(From the server.) This proposal replaces the earlier one (%d step(s)), which is withdrawn: confirming runs only the proposal below.", steps)
}

package aiassistant

import (
	"fmt"
	"regexp"
	"strings"
)

// ContinuePrefix starts the message of a turn that goes on by itself after a
// confirmed plan ran (owner, 2026-11-13); the console shows such a message
// as an automatic line.
const ContinuePrefix = "▶ "

// ContinuationMessage is that message: what ran (steps, failed of them) and
// what to do next — fix the failures first, then the next part of the
// current stage or the next stage; with no plan under way, the next step of
// the request, or the next step recommended once it is done.
func ContinuationMessage(steps, failed int, plan *TaskPlan) string {
	ran := fmt.Sprintf("Plan confirmed: all %d steps ran. Go on without waiting for me:", steps)
	if failed > 0 {
		ran = fmt.Sprintf("Plan confirmed: %d of %d steps ran, %d failed. Go on without waiting for me: first fix what failed (the step results say why), then", steps-failed, steps, failed)
	}
	var next string
	switch cur := plan.Current(); {
	case cur >= 0:
		next = fmt.Sprintf(" check what it did and propose the next part of the current stage (%d. %s) — or, once that stage is done, mark it with its "+
			"evidence and propose the next stage.", cur+1, plan.Stages[cur].Title)
	case plan.Finished():
		next = " check the last stage, report what was built and checked, and name the next step you recommend as a question I can answer yes to."
	default:
		next = " check what it did and propose the next step of my request. If my request is fully done, say what was built and name the next step " +
			"you recommend as a question I can answer yes to."
	}
	return ContinuePrefix + ran + next
}

// CheckBrief is the unseen brief of a turn that goes on after a plan ran:
// what each step changed, and to check only that. The check otherwise
// re-read every table and dashboard, one read a call: in one session the
// automatic turns made 95 of 106 calls (2026-11-25).
func CheckBrief(ran []ProposalStep) string {
	if len(ran) == 0 {
		return ""
	}
	oneLine := func(s string, n int) string { return clipRunes(strings.Join(strings.Fields(s), " "), n) }
	var b strings.Builder
	b.WriteString("\n\nTHE PLAN THAT JUST RAN — its steps and what each changed:\n")
	for i, s := range ran {
		if i == checkBriefSteps {
			fmt.Fprintf(&b, "… and %d more step(s)\n", len(ran)-i)
			break
		}
		outcome := s.Result
		if s.Status == "failed" {
			outcome = "FAILED: " + outcome
		}
		fmt.Fprintf(&b, "%d. %s — %s → %s\n", i+1, s.Tool, oneLine(s.Description, 140), oneLine(outcome, 160))
	}
	b.WriteString("Check only what these steps changed — and, for a field or metric removed or renamed, what shows it: read each once, " +
		"by its name (no list_* call to find it), all those reads together in one reply. What a check after an earlier plan verified " +
		"stays verified: do not read it again. Then go on.\n")
	return b.String()
}

// checkBriefSteps is how many steps CheckBrief lists.
const checkBriefSteps = 20

// PlanApprovalBrief is the turn's brief while the plan waits for the
// developer's approval.
const PlanApprovalBrief = "THE PLAN AWAITS THE DEVELOPER'S APPROVAL: build nothing and propose nothing for it. They see it in a card with its stages " +
	"and architecture scheme; answer their questions about it, and ask them to approve it (the Approve plan button, or a reply) or say what to change. " +
	"When they ask for a change, re-cut it with plan_task replace: true, or change the architecture with set_target_model / update_target_model: " +
	"it is presented again.\n"

// PlanSavedForApproval ends plan_task's result for a plan that waits.
const PlanSavedForApproval = "It is shown to the developer FOR APPROVAL in a card with every stage and the architecture scheme drawn from the target " +
	"model: nothing is built before they approve it. End the turn now. In your reply do not list the stages again — in a few plain sentences say what " +
	"the model will be, the design choices that matter (versions, time, the main dimensions, which dashboards show what) and anything they should look at, " +
	"then ask them to approve it or say what to change. Do not call propose_actions in this turn — it is refused until the plan is approved."

// PlanAwaitsApprovalResult answers a proposal made while the plan waits.
const PlanAwaitsApprovalResult = "Proposal NOT shown to the developer: the plan of this request awaits their approval, and nothing is built before it. " +
	"End the turn: present the whole plan in your reply and ask them to approve it or say what to change."

// approveRE: a reply that approves the plan, as a whole message (English
// and Russian).
var approveWords = `(yes|yep|ok|okay|approve|approved|i approve|agreed|agree|go|go ahead|proceed|start|looks good|lgtm|fine|perfect|great|` +
	`да|ок|окей|хорошо|одобряю|утверждаю|согласен|согласна|принято|поехали|вперед|вперёд|начинай|делай|план)`

var approveRE = regexp.MustCompile(`(?i)^\s*` + approveWords + `([\s,!.]+(` + approveWords[1:len(approveWords)-1] +
	`|with the plan|the plan|this plan|plan|it))*[\s!.]*$`)

// ApprovesPlan reports whether the developer's message approves the plan
// that waits.
func ApprovesPlan(msg string) bool { return approveRE.MatchString(strings.TrimSpace(msg)) }

// PlanApprovedMessage is the message of the turn that starts building once
// the developer approved the plan.
func PlanApprovedMessage(plan *TaskPlan) string {
	cur := plan.Current()
	if cur < 0 {
		return ContinuePrefix + "Plan approved. Go on without waiting for me."
	}
	return fmt.Sprintf("%sPlan approved. Go on without waiting for me: build stage %d (%s) — propose it now.", ContinuePrefix, cur+1, plan.Stages[cur].Title)
}

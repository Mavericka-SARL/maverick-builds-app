package gateway

// The turn reads the developer's message before working on it (owner,
// 2026-11-08): a direct order, a delegated goal or a question, and which
// tasks it is about (aiassistant/task_mode.go). The turn's instructions then
// carry that mode, the playbook of each task and the session's task plan;
// plan_task and update_task_plan write the plan (aiassistant/task_plan.go).

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
)

// classifyRequest reads msg: a short "confirmed, continue" while a plan has
// stages left goes on with the plan; otherwise one call of the session's
// model, and the rules when that cannot be made.
func classifyRequest(ctx context.Context, p providers.Provider, model, msg string, plan *aiassistant.TaskPlan, store *aiassistant.ChatStore, sessionID string) aiassistant.RequestMode {
	rules := aiassistant.ClassifyByRules(msg, plan)
	if p == nil || (rules.Mode == aiassistant.ModeDelegated && plan.Current() >= 0 && len(rules.Tasks) == 0) {
		return rules
	}
	content := msg
	if cur := plan.Current(); cur >= 0 {
		content = fmt.Sprintf("(A task plan is in progress; its current stage: %s.)\n\n%s", plan.Stages[cur].Title, msg)
	}
	// A reading must not hold the turn up: the rules answer after 30s.
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	resp, err := p.Chat(cctx, providers.ChatRequest{Model: model, SystemPrompt: aiassistant.ClassifierPrompt,
		Messages: []providers.Message{{Role: "user", Content: content}}})
	if err != nil {
		return rules
	}
	if u := resp.Usage; u.InputTokens+u.CacheReadTokens > 0 && store != nil {
		_ = store.AddUsage(context.WithoutCancel(ctx), sessionID, u)
	}
	m, ok := aiassistant.ParseClassification(resp.Message.Content)
	if !ok {
		return rules
	}
	return m
}

// modeStatus is what the developer is shown the message was read as.
func modeStatus(m aiassistant.RequestMode) string {
	switch m.Mode {
	case aiassistant.ModeDirect:
		return "read as a direct order: doing exactly what it names"
	case aiassistant.ModeQuestion:
		return "read as a question"
	}
	return "read as a delegated goal: planning, building and checking it"
}

// runTool runs a tool the model calls outside a proposal: the task plan
// tools here, every other through exec. proof (may be nil) checks a stage
// marked done against the workbook map (stageProof).
func runTool(ctx context.Context, store *aiassistant.ChatStore, sessionID string, hooks taskHooks, exec *aiassistant.ToolExecutor, name string, args json.RawMessage) (string, error) {
	var sheets []string
	if hooks.Sheets != nil {
		sheets = hooks.Sheets()
	}
	switch name {
	case aiassistant.SetTargetModelTool:
		var t aiassistant.TargetModel
		if err := json.Unmarshal(args, &t); err != nil {
			return "", fmt.Errorf("set_target_model: %w", err)
		}
		missing, err := aiassistant.CheckTarget(&t, sheets)
		if err != nil {
			return "", err
		}
		plan, _ := store.TaskPlan(ctx, sessionID)
		if plan == nil {
			plan = &aiassistant.TaskPlan{}
		}
		plan.Target = &t
		if err := store.SetTaskPlan(ctx, sessionID, plan); err != nil {
			return "", err
		}
		note := "Target model saved; it is shown in every turn and session from now on.\n"
		if len(missing) > 0 {
			note += "Sheets of the workbook the target does not place yet (add each, as guide or skip when it holds nothing to build): " + strings.Join(missing, ", ") + ".\n"
		}
		if len(plan.Stages) == 0 {
			note += "Next: plan_task from it — one stage per sheet in its build order, then pages, roles and access, workflows.\n"
		}
		return note + t.Render(), nil
	case aiassistant.PlanTaskTool:
		var a struct {
			Goal    string                  `json:"goal"`
			Stages  []aiassistant.TaskStage `json:"stages"`
			Replace bool                    `json:"replace"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("plan_task: %w", err)
		}
		// A build from a workbook is planned from its target model: the
		// whole TO-BE design first (owner, 2026-11-12).
		if len(sheets) > 0 {
			if cur, _ := store.TaskPlan(ctx, sessionID); cur == nil || cur.Target == nil {
				return "", fmt.Errorf("plan_task: a model built from the attached workbook is planned from its target model — first analyse_workbook, " +
					"then set_target_model with the whole TO-BE design (every sheet's role, grid, dimensions and metrics; pages, roles, workflows), then plan_task")
			} else {
				// Stages follow the target's build order.
				if order := cur.Target.BuildSheets(); len(order) > 0 {
					sheets = append(order, sheets...)
				}
			}
		}
		// One sheet or one page per stage when a workbook is attached: a
		// stage of eleven sheets cannot be one proposal, and its plans were
		// judged against all eleven (2026-11-12).
		cutNote := ""
		if len(sheets) > 0 {
			var cut []string
			if a.Stages, cut = cutStages(a.Stages, sheets); len(cut) > 0 {
				cutNote = "The server cut " + strings.Join(cut, "; ") + " into one stage per sheet or page. "
			}
		}
		// Structure first, imports last (owner, 2026-11-12): actuals tables and
		// loaders after the calculations, process and pages; the real data
		// and its reconciliation at the very end.
		if cur, _ := store.TaskPlan(ctx, sessionID); cur != nil && cur.Target != nil {
			if moved := structureFirst(a.Stages, cur.Target); moved != nil {
				a.Stages = moved
				cutNote += "Actuals imports and the real-data reconciliation come last (structure first). "
			}
		}
		// A plan already under way — carried from earlier sessions, or
		// this session's own — is kept: the new stages follow it (one is
		// enough), or with replace re-cut its open stages; its done stages
		// and decisions stay (owner, 2026-11-11: carried context is never
		// dropped).
		prev, _ := store.TaskPlan(ctx, sessionID)
		if prev != nil && len(prev.Stages) > 0 && prev.Current() >= 0 {
			if err := aiassistant.CheckStages(a.Stages); err != nil {
				return "", err
			}
			var note string
			if a.Replace {
				prev.Replace(a.Goal, a.Stages)
				note = "The plan's open stages were replaced by these; its done stages and decisions stay."
			} else {
				note = "The session's plan was under way, so these stages were added after its own (nothing earlier is dropped)."
				if dup := prev.Append(a.Goal, a.Stages); len(dup) > 0 {
					note += " Already in the plan, so not added again: " + strings.Join(dup, "; ") + "."
				}
			}
			if err := store.SetTaskPlan(ctx, sessionID, prev); err != nil {
				return "", err
			}
			log.Printf("AI task plan: session=%s stages=%d replace=%v", sessionID, len(prev.Stages), a.Replace)
			return cutNote + note + " The developer sees it.\n" + prev.Render() +
				"To re-cut the open stages instead of adding to them, call plan_task with replace: true. Now propose the CURRENT stage.", nil
		}
		plan, err := aiassistant.NewTaskPlan(a.Goal, a.Stages)
		if err != nil {
			return "", err
		}
		// The target model recorded before the plan stays with it (2026-11-12:
		// the first plan_task after set_target_model dropped it).
		if prev != nil {
			plan.Target = prev.Target
		}
		if prev != nil && len(prev.Decisions) > 0 {
			plan.Decisions, plan.CarriedFrom, plan.CarriedChecked = prev.Decisions, prev.CarriedFrom, true
		}
		if err := store.SetTaskPlan(ctx, sessionID, plan); err != nil {
			return "", err
		}
		log.Printf("AI task plan: session=%s stages=%d", sessionID, len(plan.Stages))
		return cutNote + "Plan saved; the developer sees it.\n" + plan.Render() +
			"Now propose stage 1 (mark it in_progress with update_task_plan as you do), and say in your reply what the stages are.", nil
	case aiassistant.UpdateTaskPlanTool:
		var a struct {
			Stage    int    `json:"stage"`
			Status   string `json:"status"`
			Note     string `json:"note"`
			Decision string `json:"decision"`
		}
		if err := json.Unmarshal(args, &a); err != nil {
			return "", fmt.Errorf("update_task_plan: %w", err)
		}
		plan, err := store.TaskPlan(ctx, sessionID)
		if err != nil {
			return "", err
		}
		if plan == nil && strings.TrimSpace(a.Decision) != "" && a.Stage == 0 {
			plan = &aiassistant.TaskPlan{}
		}
		if plan == nil {
			return "", fmt.Errorf("update_task_plan: the session has no plan — plan_task first")
		}
		plan.AddDecision(a.Decision)
		// A stage of a sheet, or one that loads or matches values, is done
		// by its numbers: the workbook map is asked first (2026-11-12: Stage
		// 2 closed with 9 of 72 drivers loaded). Asked again with a note, it
		// is taken, the note saying it was not proven.
		if a.Status == aiassistant.StageDone && hooks.Proof != nil && a.Stage >= 1 && a.Stage <= len(plan.Stages) {
			if st := &plan.Stages[a.Stage-1]; st.Status != aiassistant.StageDone {
				if probs := hooks.Proof(*st); probs != "" {
					if !st.ProofAsked || strings.TrimSpace(a.Note) == "" {
						st.ProofAsked = true
						_ = store.SetTaskPlan(ctx, sessionID, plan)
						return "", fmt.Errorf("update_task_plan: stage %d is not done by its numbers — %s. Compare and fix those rows, then mark it done; "+
							"if a row cannot be compared, mark it done again with a note saying why", a.Stage, probs)
					}
					a.Note = "not proven by the workbook map (" + probs + "): " + a.Note
				}
			}
		}
		if a.Stage != 0 || a.Status != "" {
			if err := plan.UpdateStage(a.Stage, a.Status, a.Note); err != nil {
				return "", err
			}
		}
		if err := store.SetTaskPlan(ctx, sessionID, plan); err != nil {
			return "", err
		}
		return plan.Render(), nil
	}
	return exec.Execute(ctx, name, args)
}

// isTaskPlanTool reports whether name writes the session's plan.
func isTaskPlanTool(name string) bool {
	return name == aiassistant.PlanTaskTool || name == aiassistant.UpdateTaskPlanTool || name == aiassistant.SetTargetModelTool
}

// stageProofFor checks a stage marked done against the workbook map: every
// attached sheet the stage names must have rows compared, none differing
// and none left unmapped. A sheet counts when the stage's title names it,
// or its items or check for a stage of values (NeedsProof) — a roles stage
// naming "Hardware Operations" is not a Hardware sheet stage. "" when it
// holds, or when the stage names no sheet (nothing to check it by).
func (h *handler) stageProofFor(ctx context.Context, sessionID, modelID, revID string, st aiassistant.TaskStage) string {
	text := " " + strings.ToLower(st.Title) + " "
	if st.Sheet != "" {
		text = " " + strings.ToLower(st.Sheet) + " "
	} else if st.NeedsProof() {
		text = " " + strings.ToLower(st.Title+" "+strings.Join(st.Items, " ")+" "+st.Check) + " "
	}
	var named []string
	seen := map[string]bool{}
	for _, s := range h.attachedSheetNames(ctx, sessionID) {
		k := strings.ToLower(s)
		if len(k) >= 3 && !seen[k] && sheetNamedIn(text, k) {
			seen[k] = true
			named = append(named, s)
		}
	}
	if len(named) == 0 {
		return ""
	}
	rows, err := aiassistant.LoadWorkbookMap(ctx, h.db.For(ctx), modelID, revID, nil)
	if err != nil {
		return ""
	}
	cov := aiassistant.Coverage(rows, h.sessionWorkbooks(ctx, sessionID))
	var probs []string
	for _, s := range named {
		var c *aiassistant.SheetCoverage
		for i := range cov {
			if strings.EqualFold(cov[i].Sheet, s) {
				c = &cov[i]
				break
			}
		}
		switch {
		case c == nil || c.Mapped == 0:
			probs = append(probs, fmt.Sprintf("%s: no row compared yet (compare_with_sheet records it in the workbook map)", s))
		default:
			if len(c.Differ) > 0 {
				probs = append(probs, fmt.Sprintf("%s: rows %s differ", s, intList(c.Differ, 15)))
			}
			if len(c.NotMapped) > 0 {
				probs = append(probs, fmt.Sprintf("%s: rows %s hold numbers no metric was compared with", s, intList(c.NotMapped, 15)))
			}
		}
	}
	return strings.Join(probs, "; ")
}

// sheetNamedIn: the sheet's name stands as words in text (lower-cased,
// padded with spaces), so "SaaS" is not found in "saas growth role" alone
// by accident of a longer word.
func sheetNamedIn(text, sheet string) bool {
	for i := 0; ; {
		j := strings.Index(text[i:], sheet)
		if j < 0 {
			return false
		}
		j += i
		before, after := text[j-1], byte(' ')
		if j+len(sheet) < len(text) {
			after = text[j+len(sheet)]
		}
		if !isWordByte(before) && !isWordByte(after) {
			return true
		}
		i = j + 1
	}
}

func isWordByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9' || b == '_'
}

func intList(xs []int, max int) string {
	var parts []string
	for i, x := range xs {
		if i == max {
			parts = append(parts, fmt.Sprintf("… (%d more)", len(xs)-max))
			break
		}
		parts = append(parts, fmt.Sprint(x))
	}
	return strings.Join(parts, ", ")
}

// taskHooks are what the task-plan tools need from the turn: the workbook
// map check of a stage marked done, and the attached workbooks' sheets.
type taskHooks struct {
	Proof  func(aiassistant.TaskStage) string
	Sheets func() []string
}

// pageStageRE: a stage about pages (dashboards), cut one page per stage.
var pageStageRE = regexp.MustCompile(`(?i)\b(pages?|dashboards?)\b`)

// cutStages cuts a stage naming several workbook sheets into one stage per
// sheet (in the workbook's order; items naming no sheet go with the last),
// and a stage of pages into one stage per page. It returns the stages and
// the titles it cut.
func cutStages(stages []aiassistant.TaskStage, sheets []string) ([]aiassistant.TaskStage, []string) {
	var out []aiassistant.TaskStage
	var cut []string
	seen := map[string]bool{}
	var order []string
	for _, s := range sheets {
		if k := strings.ToLower(strings.TrimSpace(s)); len(k) >= 3 && !seen[k] {
			seen[k] = true
			order = append(order, s)
		}
	}
	for _, st := range stages {
		if pageStageRE.MatchString(st.Title) && len(st.Items) >= 2 {
			cut = append(cut, st.Title)
			for _, it := range st.Items {
				out = append(out, aiassistant.TaskStage{Title: "Page: " + clip(it, 70), Items: []string{it}, Check: st.Check})
			}
			continue
		}
		text := " " + strings.ToLower(st.Title+" "+strings.Join(st.Items, " ")) + " "
		var named []string
		for _, s := range order {
			if sheetNamedIn(text, strings.ToLower(s)) {
				named = append(named, s)
			}
		}
		if len(named) < 2 {
			// A stage titled by one sheet builds that sheet: done by its numbers.
			if len(named) == 1 && st.Sheet == "" && sheetNamedIn(" "+strings.ToLower(st.Title)+" ", strings.ToLower(named[0])) {
				st.Sheet = named[0]
			}
			out = append(out, st)
			continue
		}
		cut = append(cut, st.Title)
		var generic []string
		per := map[string][]string{}
		for _, it := range st.Items {
			low := " " + strings.ToLower(it) + " "
			hit := false
			for _, s := range named {
				if sheetNamedIn(low, strings.ToLower(s)) {
					per[s] = append(per[s], it)
					hit = true
				}
			}
			if !hit {
				generic = append(generic, it)
			}
		}
		for i, s := range named {
			items := per[s]
			if len(items) == 0 || len(named) > 1 && len(per[s]) == len(st.Items) {
				items = []string{s + " sheet: its rows as metrics on its grid, matching the workbook in every scenario"}
			}
			if i == len(named)-1 {
				items = append(items, generic...)
			}
			out = append(out, aiassistant.TaskStage{Title: s, Items: items, Check: "compare_with_sheet " + s, Sheet: s})
		}
	}
	return out, cut
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// closeProvenStages finishes the plan's current stages that build one sheet
// and whose rows the workbook map shows matching: state read from the model,
// not from what any call said (owner, 2026-11-12). It returns the plan.
func (h *handler) closeProvenStages(ctx context.Context, store *aiassistant.ChatStore, sessionID, modelID, revID string, plan *aiassistant.TaskPlan) *aiassistant.TaskPlan {
	if plan == nil || modelID == "" || revID == "" {
		return plan
	}
	changed := false
	for {
		cur := plan.Current()
		if cur < 0 {
			break
		}
		st := &plan.Stages[cur]
		if st.Sheet == "" || st.Confirmed == 0 {
			break
		}
		if probs := h.stageProofFor(ctx, sessionID, modelID, revID, *st); probs != "" {
			break
		}
		st.Status = aiassistant.StageDone
		st.Note = "done by the server: the workbook map shows every row of " + st.Sheet + " matching"
		changed = true
	}
	if changed {
		_ = store.SetTaskPlan(ctx, sessionID, plan)
	}
	return plan
}

// targetProgress is the target model measured against the model itself:
// per sheet, how many of its metrics exist, and the missing ones of the
// current stage's sheet — so every turn works in the target's context.
func (h *handler) targetProgress(ctx context.Context, modelID, revID string, plan *aiassistant.TaskPlan) string {
	if plan == nil || plan.Target == nil || revID == "" {
		return ""
	}
	have := map[string]bool{}
	rows, err := h.db.For(ctx).Query(ctx, `SELECT lower(name) FROM model.metric_def WHERE revision_id = $1::uuid`, revID)
	if err != nil {
		return ""
	}
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			have[n] = true
		}
	}
	rows.Close()
	curSheet := ""
	if cur := plan.Current(); cur >= 0 {
		curSheet = plan.Stages[cur].Sheet
	}
	var b strings.Builder
	b.WriteString("\n\n## Target progress (read from the model now)\n")
	target := plan.Target.MetricNames()
	for _, s := range plan.Target.Sheets {
		if len(s.Metrics) == 0 {
			continue
		}
		n := 0
		var missing []string
		for _, m := range s.Metrics {
			if have[strings.ToLower(strings.TrimSpace(m))] {
				n++
			} else {
				missing = append(missing, m)
			}
		}
		fmt.Fprintf(&b, "- %s: %d/%d metrics exist", s.Sheet, n, len(s.Metrics))
		if strings.EqualFold(s.Sheet, curSheet) && len(missing) > 0 {
			fmt.Fprintf(&b, " — still to build: %s", strings.Join(missing, ", "))
		}
		b.WriteByte('\n')
	}
	var extra []string
	for n := range have {
		if !target[n] {
			extra = append(extra, n)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		if len(extra) > 15 {
			extra = append(extra[:15], fmt.Sprintf("… %d more", len(extra)-15))
		}
		fmt.Fprintf(&b, "Metrics in the model that the target does not name: %s — fold them into the target (set_target_model) or remove them.\n", strings.Join(extra, ", "))
	}
	return b.String()
}

// buildAdvice says, for AI Settings, whether a model and reasoning effort
// suit building a model from a workbook ("" when nothing is to be said).
// 2026-11-12: gpt-5.4-mini at "none" (the only effort it took with tools on
// chat completions) made ~470 calls and built nothing; gpt-5-mini at low and
// medium asked leave instead of proposing. Models that take tools only with
// "none" on chat completions now run through the Responses API, with the
// effort chosen.
func buildAdvice(provider, model, effort string) string {
	if provider != "openai" {
		return ""
	}
	m := strings.ToLower(strings.TrimSpace(model))
	small := strings.Contains(m, "mini") || strings.Contains(m, "nano")
	switch {
	case effort == "none" || effort == "minimal":
		return "Reasoning effort " + effort + " plans too shallowly to build a model from a workbook: fine for questions, pages and small edits. For builds choose medium or high."
	case small:
		return "A small model: good for pages, wiring and small edits with an exact request. To build a model from a workbook, use a reasoning model (gpt-6.1-sol, gpt-5.5) at medium or high effort."
	}
	return ""
}

// reconcileRE: a stage that loads the real data or reconciles with it.
var reconcileRE = regexp.MustCompile(`(?i)\b(reconcil\w*|real data|final (check|comparison)|compare (every|all))\b`)

// structureFirst orders stages as the owner asked (2026-11-12): the build
// (inputs, calculations, roles, workflows, pages), then the actuals sheets'
// imports, then loading the real data and reconciling. nil when nothing
// moves.
func structureFirst(stages []aiassistant.TaskStage, t *aiassistant.TargetModel) []aiassistant.TaskStage {
	actuals := map[string]bool{}
	for _, s := range t.Sheets {
		if s.Role == "actuals" {
			actuals[strings.ToLower(s.Sheet)] = true
		}
	}
	var build, imports, last []aiassistant.TaskStage
	for _, st := range stages {
		switch {
		case reconcileRE.MatchString(st.Title):
			last = append(last, st)
		case st.Sheet != "" && actuals[strings.ToLower(st.Sheet)],
			st.Sheet == "" && regexp.MustCompile(`(?i)\b(import|actuals?)\b`).MatchString(st.Title):
			imports = append(imports, st)
		default:
			build = append(build, st)
		}
	}
	out := append(append(build, imports...), last...)
	for i := range out {
		if out[i].Title != stages[i].Title {
			return out
		}
	}
	return nil
}

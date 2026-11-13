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
		var open []string
		for _, d := range t.Decisions {
			if d.Open() {
				open = append(open, d.Question)
			}
		}
		if len(open) > 0 {
			note += "Ask the developer the open decisions now, each once with its options, and record the choices with update_target_model; " +
				"the sheets they hold back are not loaded or translated until then: " + strings.Join(open, "; ") + ".\n"
		}
		if len(plan.Stages) == 0 {
			note += "Next: plan_task from it — one coherent module per stage in dependency order (a few dependent sheets proven together), " +
				"then actuals, then the business requirements (roles and access, workflows, the approved revision, pages), then the verification checks.\n"
		}
		return note + t.Render(), nil
	case aiassistant.UpdateTargetModelTool:
		var u aiassistant.TargetUpdate
		if err := json.Unmarshal(args, &u); err != nil {
			return "", fmt.Errorf("update_target_model: %w", err)
		}
		plan, _ := store.TaskPlan(ctx, sessionID)
		if plan == nil || plan.Target == nil {
			return "", fmt.Errorf("update_target_model: no target model yet — set_target_model first")
		}
		resolved, err := plan.Target.Apply(u)
		if err != nil {
			return "", err
		}
		// A decision the developer made stands for every later turn and
		// session, in the plan's decisions as well.
		for _, d := range resolved {
			plan.AddDecision(d.Question + " → " + d.Choice)
		}
		if err := store.SetTaskPlan(ctx, sessionID, plan); err != nil {
			return "", err
		}
		return "Target model updated.\n" + plan.Target.Render(), nil
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
		// One coherent module per stage when a workbook is attached (owner,
		// 2026-11-12): a few dependent sheets proven together, a few pages
		// together. A stage of eleven sheets cannot be one proposal
		// (2026-11-12), and one sheet or page per stage made 80 stages.
		cutNote := ""
		if len(sheets) > 0 {
			var cut []string
			if a.Stages, cut = cutStages(a.Stages, sheets); len(cut) > 0 {
				cutNote = fmt.Sprintf("The server cut %s into modules of at most %d sheets or %d pages. ", strings.Join(cut, "; "), maxModuleSheets, maxStagePages)
			}
		}
		// Calculations first, module by module; then the actuals; then the
		// business requirements, built once the calculations they read
		// reconcile; the verification checks last (owner, 2026-11-12).
		if cur, _ := store.TaskPlan(ctx, sessionID); cur != nil && cur.Target != nil {
			if moved := buildOrder(a.Stages, cur.Target); moved != nil {
				a.Stages = moved
				cutNote += "Stages ordered: calculation modules, then actuals, then roles, workflows and pages, then the verification checks. "
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
			Stage    int                        `json:"stage"`
			Status   string                     `json:"status"`
			Note     string                     `json:"note"`
			Decision string                     `json:"decision"`
			Evidence *aiassistant.StageEvidence `json:"evidence"`
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
		if a.Status == aiassistant.StageDone && a.Stage >= 1 && a.Stage <= len(plan.Stages) && plan.Stages[a.Stage-1].Status != aiassistant.StageDone {
			st := &plan.Stages[a.Stage-1]
			// A stage ends with its evidence (owner, 2026-11-12): what was
			// built, what still differs and why, the next bounded stage.
			if err := stageEvidenceProblem(a.Evidence); err != "" {
				return "", fmt.Errorf("update_task_plan: end stage %d with its evidence — %s", a.Stage, err)
			}
			// "Done" means verified behaviour: the last stage closes once
			// every verification check of the target is recorded.
			if isLastOpen(plan, a.Stage-1) {
				if open := plan.Target.OpenChecks(); len(open) > 0 {
					return "", fmt.Errorf("update_task_plan: the build ends when every verification check is recorded (update_target_model: passed, failed or not_possible, with evidence) — open: %s",
						strings.Join(open, "; "))
				}
			}
			ev := *a.Evidence
			if hooks.Compared != nil {
				ev.Compared = hooks.Compared(st.StageSheets())
			}
			st.Evidence = &ev
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
	return name == aiassistant.PlanTaskTool || name == aiassistant.UpdateTaskPlanTool || name == aiassistant.SetTargetModelTool ||
		name == aiassistant.UpdateTargetModelTool
}

// stageEvidenceProblem says what a stage's evidence lacks ("" when whole).
func stageEvidenceProblem(e *aiassistant.StageEvidence) string {
	if e == nil {
		return "evidence: {built, differences, next} — what was built or repaired, what still differs and why (or none), the next bounded stage"
	}
	var miss []string
	if strings.TrimSpace(e.Built) == "" {
		miss = append(miss, "built")
	}
	if strings.TrimSpace(e.Differences) == "" {
		miss = append(miss, "differences (none when none)")
	}
	if strings.TrimSpace(e.Next) == "" {
		miss = append(miss, "next (none at the end)")
	}
	if len(miss) > 0 {
		return "evidence needs " + strings.Join(miss, ", ")
	}
	return ""
}

// isLastOpen reports whether stage i is the plan's only stage not yet done
// or skipped.
func isLastOpen(p *aiassistant.TaskPlan, i int) bool {
	for j, s := range p.Stages {
		if j != i && s.Status != aiassistant.StageDone && s.Status != aiassistant.StageSkipped {
			return false
		}
	}
	return true
}

// stageProofFor checks a stage marked done against the workbook map: every
// attached sheet the stage names must have rows compared, none differing
// and none left unmapped. A sheet counts when the stage's title names it,
// or its items or check for a stage of values (NeedsProof) — a roles stage
// naming "Hardware Operations" is not a Hardware sheet stage. "" when it
// holds, or when the stage names no sheet (nothing to check it by).
func (h *handler) stageProofFor(ctx context.Context, sessionID, modelID, revID string, st aiassistant.TaskStage) string {
	text := " " + strings.ToLower(st.Title) + " "
	if sh := st.StageSheets(); len(sh) > 0 {
		text = " " + strings.ToLower(strings.Join(sh, " | ")) + " "
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
	// Compared is the workbook map's count of the sheets' rows and periods
	// compared and matching ("" when none were compared).
	Compared func(sheets []string) string
}

// pageStageRE: a stage about pages (dashboards).
var pageStageRE = regexp.MustCompile(`(?i)\b(pages?|dashboards?)\b`)

// maxModuleSheets and maxStagePages bound a stage: a module of a few
// dependent sheets, proven together; a few pages for one audience.
const (
	maxModuleSheets = 4
	maxStagePages   = 3
)

// cutStages keeps a stage naming a few workbook sheets as one module (its
// Sheets, in the workbook's order) and cuts a stage naming more into
// modules of at most maxModuleSheets (items naming no sheet go with the
// last); a stage of more than maxStagePages pages is cut into stages of
// that many. It returns the stages and the titles it cut.
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
		if pageStageRE.MatchString(st.Title) && len(st.Items) > maxStagePages {
			cut = append(cut, st.Title)
			for k := 0; k < len(st.Items); k += maxStagePages {
				part := st.Items[k:min(k+maxStagePages, len(st.Items))]
				out = append(out, aiassistant.TaskStage{Title: "Pages: " + clip(strings.Join(part, ", "), 90), Items: part, Check: st.Check})
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
		if pageStageRE.MatchString(st.Title) && len(named) > 0 && !sheetNamedIn(" "+strings.ToLower(st.Title)+" ", strings.ToLower(named[0])) {
			// A page stage showing sheets is not a module of them.
			out = append(out, st)
			continue
		}
		if len(named) <= maxModuleSheets {
			// A stage titled by sheets builds them: done by their numbers.
			if len(named) > 0 && len(st.StageSheets()) == 0 && (titleNamesAny(st.Title, named) || !requirementRE.MatchString(st.Title)) {
				if len(named) == 1 {
					st.Sheet = named[0]
				} else {
					st.Sheets = named
				}
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
					break
				}
			}
			if !hit {
				generic = append(generic, it)
			}
		}
		for k := 0; k < len(named); k += maxModuleSheets {
			mod := named[k:min(k+maxModuleSheets, len(named))]
			var items []string
			for _, s := range mod {
				if len(per[s]) > 0 && len(per[s]) < len(st.Items) {
					items = append(items, per[s]...)
				} else {
					items = append(items, s+" sheet: its rows as metrics on its grid, matching the workbook in every scenario")
				}
			}
			if k+maxModuleSheets >= len(named) {
				items = append(items, generic...)
			}
			out = append(out, aiassistant.TaskStage{Title: strings.Join(mod, " → "), Items: items,
				Check: "compare_with_sheet " + strings.Join(mod, ", "), Sheets: append([]string(nil), mod...)})
		}
	}
	return out, cut
}

// titleNamesAny reports whether title names one of sheets.
func titleNamesAny(title string, sheets []string) bool {
	t := " " + strings.ToLower(title) + " "
	for _, s := range sheets {
		if sheetNamedIn(t, strings.ToLower(s)) {
			return true
		}
	}
	return false
}

func clip(s string, n int) string {
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// closeProvenStages finishes the plan's current stages that build sheets
// and whose rows the workbook map shows matching: state read from the model,
// not from what any call said (owner, 2026-11-12). The server writes their
// evidence. It returns the plan.
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
		sheets := st.StageSheets()
		if len(sheets) == 0 || st.Confirmed == 0 {
			break
		}
		if probs := h.stageProofFor(ctx, sessionID, modelID, revID, *st); probs != "" {
			break
		}
		next := "none"
		if cur+1 < len(plan.Stages) {
			next = plan.Stages[cur+1].Title
		}
		st.Status = aiassistant.StageDone
		st.Note = "done by the server: the workbook map shows every row of " + strings.Join(sheets, ", ") + " matching"
		st.Evidence = &aiassistant.StageEvidence{Built: strings.Join(st.Items, "; "), Differences: "none",
			Compared: h.comparedFor(ctx, modelID, revID, sheets), Next: next}
		changed = true
	}
	if changed {
		_ = store.SetTaskPlan(ctx, sessionID, plan)
	}
	return plan
}

// comparedFor is the workbook map's count for sheets: per sheet, rows
// compared and matching, and periods compared and matching.
func (h *handler) comparedFor(ctx context.Context, modelID, revID string, sheets []string) string {
	if len(sheets) == 0 {
		return ""
	}
	rows, err := aiassistant.LoadWorkbookMap(ctx, h.db.For(ctx), modelID, revID, nil)
	if err != nil {
		return ""
	}
	var parts []string
	for _, sh := range sheets {
		var n, ok, per, perOK int
		for _, r := range rows {
			if !strings.EqualFold(r.Sheet, sh) || r.Compared == 0 {
				continue
			}
			n++
			per += r.Compared
			perOK += r.Matched
			if r.Matches() {
				ok++
			}
		}
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d/%d rows, %d/%d periods matching", sh, ok, n, perOK, per))
		}
	}
	return strings.Join(parts, "; ")
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
	curSheets := map[string]bool{}
	if cur := plan.Current(); cur >= 0 {
		for _, sh := range plan.Stages[cur].StageSheets() {
			curSheets[strings.ToLower(sh)] = true
		}
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
		if curSheets[strings.ToLower(s.Sheet)] && len(missing) > 0 {
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
// effort chosen. Later that day, building one workbook five ways, gpt-6.1-sol
// at low effort finished in 75 calls and 4 minutes, at medium in 129 and 8;
// gpt-5-mini did not finish in 181 calls (owner: recommend the first).
func buildAdvice(provider, model, effort string) string {
	if provider != "openai" {
		return ""
	}
	m := strings.ToLower(strings.TrimSpace(model))
	small := strings.Contains(m, "mini") || strings.Contains(m, "nano")
	switch {
	case effort == "none" || effort == "minimal":
		return "Reasoning effort " + effort + " plans too shallowly to build a model from a workbook: fine for questions, pages and small edits. For builds choose low or higher."
	case small:
		return "A small model: good for pages, wiring and small edits with an exact request. To build a model from a workbook, use a strong reasoning model (gpt-6.1-sol, gpt-5.5) at low effort: " +
			"it finishes in fewer calls and less time, where a small model asks more and stops short."
	case effort == "high" || effort == "xhigh":
		return "Low effort is usually enough to build a model from a workbook with this model; " + effort + " makes every call longer and costlier. Raise it only when plans come back too shallow."
	}
	return ""
}

// reconcileRE: a stage of final reconciliation or verification checks.
var reconcileRE = regexp.MustCompile(`(?i)\b(reconcil\w*|real data|final (check|comparison)|compare (every|all)|verif\w*|checks?)\b`)

// finalStageRE: the build's closing verification, last even when it names
// a sheet (High-Tech rebuild 4, 2026-11-12: "Final verification" named the
// Checks sheet and was ordered before the workflows its checks needed).
var finalStageRE = regexp.MustCompile(`(?i)\b(final (verification|reconciliation|check\w*)|verification checks|verify the (build|model))\b`)

// requirementRE: a stage of business requirements built on the
// calculations — access, process, the approved revision, pages.
var requirementRE = regexp.MustCompile(`(?i)\b(roles?|access|workflows?|triggers?|approv\w*|automation|requests?|pages?|dashboards?|guide)\b`)

// actualsRE: a stage of actuals imports and reforecasting.
var actualsRE = regexp.MustCompile(`(?i)\b(imports?|actuals?|reforecast\w*|monthly close)\b`)

// buildOrder orders stages as the owner asked (2026-11-12): the
// calculation modules (inputs loaded with them) in the order given, then
// actuals and reforecasting, then the business requirements once the
// calculations they read reconcile (roles and access, workflows, the
// approved revision, pages), then the verification checks. nil when
// nothing moves.
func buildOrder(stages []aiassistant.TaskStage, t *aiassistant.TargetModel) []aiassistant.TaskStage {
	actuals := map[string]bool{}
	for _, s := range t.Sheets {
		if s.Role == "actuals" {
			actuals[strings.ToLower(s.Sheet)] = true
		}
	}
	isActuals := func(st aiassistant.TaskStage) bool {
		sh := st.StageSheets()
		if len(sh) == 0 {
			return actualsRE.MatchString(st.Title)
		}
		for _, x := range sh {
			if !actuals[strings.ToLower(x)] {
				return false
			}
		}
		return true
	}
	var build, acts, reqs, last []aiassistant.TaskStage
	for _, st := range stages {
		switch {
		case finalStageRE.MatchString(st.Title),
			len(st.StageSheets()) == 0 && reconcileRE.MatchString(st.Title) && !requirementRE.MatchString(st.Title):
			last = append(last, st)
		case isActuals(st):
			acts = append(acts, st)
		case len(st.StageSheets()) == 0 && requirementRE.MatchString(st.Title):
			reqs = append(reqs, st)
		default:
			build = append(build, st)
		}
	}
	out := append(append(append(build, acts...), reqs...), last...)
	for i := range out {
		if out[i].Title != stages[i].Title {
			return out
		}
	}
	return nil
}

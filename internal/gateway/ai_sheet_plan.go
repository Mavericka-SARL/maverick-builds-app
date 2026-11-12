package gateway

// plan_sheet and the sheet_plan step (aiassistant/sheet_plan.go): the server
// translates a workbook sheet with the target model and writes the sheet's
// plan; the AI proposes ONE step {"tool": "sheet_plan", "params": {"sheet",
// "fixes", "skip"}} with formulas for the rows the rules could not
// translate, and the server expands it into the full plan before the plan
// check. A sheet takes two to four calls instead of ~18 (2026-11-12).

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
)

// SheetPlanTool is the plan step the server expands.
const SheetPlanTool = "sheet_plan"

// sheetPlanRows translates a sheet for the session's draft.
func (h *handler) sheetPlanRows(ctx context.Context, sessionID, revID, sheet string, overrides map[int]string) ([]aiassistant.SheetRow, *aiassistant.TargetSheet, error) {
	plan, err := h.aiChatStore(ctx).TaskPlan(ctx, sessionID)
	if err != nil || plan == nil || plan.Target == nil {
		return nil, nil, fmt.Errorf("plan_sheet works from the target model: record it first (set_target_model)")
	}
	var target *aiassistant.TargetSheet
	for i := range plan.Target.Sheets {
		if strings.EqualFold(plan.Target.Sheets[i].Sheet, sheet) {
			target = &plan.Target.Sheets[i]
		}
	}
	if target == nil {
		return nil, nil, fmt.Errorf("the target model does not place sheet %q", sheet)
	}
	raw := h.workbookWithSheet(ctx, sessionID, target.Sheet)
	if raw == nil {
		return nil, nil, fmt.Errorf("no attached workbook has a sheet %q", target.Sheet)
	}
	q := h.db.For(ctx)
	in := aiassistant.SheetPlanInput{Sheet: target.Sheet, Target: plan.Target, Overrides: overrides,
		Percent: map[string]bool{}, Labels: map[string]string{}, Members: map[string]map[string]string{}, Known: map[string]map[int]string{}}
	rows, err := q.Query(ctx, `SELECT lower(name), COALESCE(label, ''), COALESCE(format, '') FROM model.metric_def WHERE revision_id = $1::uuid`, revID)
	if err == nil {
		for rows.Next() {
			var n, l, f string
			if rows.Scan(&n, &l, &f) == nil {
				in.Labels[n] = l
				in.Percent[n] = f == "percentage"
			}
		}
		rows.Close()
	}
	rows, err = q.Query(ctx, `
		SELECT d.name, COALESCE(d.dimension_type, ''), mb.code, COALESCE(mb.label, ''), mb.period_start
		FROM model.dimension_def d JOIN model.dimension_member mb ON mb.dimension_id = d.id
		WHERE d.revision_id = $1::uuid`, revID)
	if err == nil {
		var first time.Time
		for rows.Next() {
			var d, kind, code, label string
			var start *time.Time
			if rows.Scan(&d, &kind, &code, &label, &start) != nil {
				continue
			}
			if kind == "time" {
				if start != nil && (first.IsZero() || start.Before(first)) {
					first = *start
				}
				continue
			}
			if strings.Contains(strings.ToLower(d), "scenario") {
				in.Scenario = d
			}
			if in.Members[d] == nil {
				in.Members[d] = map[string]string{}
			}
			in.Members[d][strings.ToLower(code)] = code
			if label != "" {
				in.Members[d][strings.ToLower(label)] = code
			}
		}
		rows.Close()
		if !first.IsZero() {
			in.FirstYear, in.FirstMon = first.Year(), int(first.Month())
		}
	}
	if in.FirstYear == 0 {
		return nil, nil, fmt.Errorf("the model has no time dimension with dated months yet: build the dimensions first")
	}
	if sess, sErr := h.aiChatStore(ctx).GetSession(ctx, sessionID); sErr == nil {
		if mrows, mErr := aiassistant.LoadWorkbookMap(ctx, q, sess.ModelID, revID, nil); mErr == nil {
			for _, r := range mrows {
				k := strings.ToLower(r.Sheet)
				if in.Known[k] == nil {
					in.Known[k] = map[int]string{}
				}
				if len(r.Members) == 0 || r.Members[loadedMember] != "" {
					in.Known[k][r.Row] = r.Metric
				}
			}
		}
	}
	out, err := aiassistant.PlanSheet(raw, in)
	return out, target, err
}

// workbookWithSheet is the first attached workbook (in the order attached)
// that has the sheet.
func (h *handler) workbookWithSheet(ctx context.Context, sessionID, sheet string) []byte {
	store := aiassistant.NewDocumentStore(h.db.For(ctx))
	docs, err := store.ListDocuments(ctx, sessionID)
	if err != nil {
		return nil
	}
	for _, d := range docs {
		low := strings.ToLower(d.Filename)
		if !d.Importable || (!strings.HasSuffix(low, ".xlsx") && !strings.HasSuffix(low, ".xlsm")) {
			continue
		}
		_, raw, err := store.GetRaw(ctx, sessionID, d.Filename)
		if err != nil || raw == nil {
			continue
		}
		names, _ := importpkg.XLSXSheetNames(raw)
		for _, s := range names {
			if strings.EqualFold(s, sheet) {
				return raw
			}
		}
	}
	return nil
}

// aiPlanSheet answers plan_sheet: what the server translated, what it
// could not, and the one step to propose.
func (h *handler) aiPlanSheet(ctx context.Context, sessionID, sheet string, overrides map[int]string) (string, error) {
	sess, err := h.aiChatStore(ctx).GetSession(ctx, sessionID)
	if err != nil {
		return "", err
	}
	if sess.DraftRevisionID == "" {
		return "", fmt.Errorf("plan_sheet reads the AI draft: confirm a first plan so the session has one")
	}
	rows, target, err := h.sheetPlanRows(ctx, sessionID, sess.DraftRevisionID, sheet, overrides)
	if err != nil {
		return "", err
	}
	exists := h.metricNamesOf(ctx, sess.DraftRevisionID)
	var b strings.Builder
	translated, inputs, built := 0, 0, 0
	type need struct {
		metric string
		rows   []string
	}
	needs := map[string]*need{}
	var order []string
	for _, r := range rows {
		switch {
		case r.Metric != "" && exists[strings.ToLower(r.Metric)]:
			built++
		case r.Input:
			inputs++
		case r.Formula != "":
			translated++
		default:
			key := r.Metric
			if key == "" {
				key = fmt.Sprintf("(row %d)", r.Row)
			}
			n := needs[key]
			if n == nil {
				n = &need{metric: r.Metric}
				needs[key] = n
				order = append(order, key)
			}
			line := fmt.Sprintf("row %d %q: %s", r.Row, r.Label, r.Problem)
			if r.Excel != "" {
				line += " — Excel: =" + r.Excel
			}
			n.rows = append(n.rows, line)
		}
	}
	fmt.Fprintf(&b, "Sheet %s → grid %q by %s. %d rows: %d formulas translated by the server, %d inputs, %d metrics already built (left as they are), %d metric(s) need your formula.\n",
		target.Sheet, target.Grid, strings.Join(target.Dimensions, " × "), len(rows), translated, inputs, built, len(order))
	for _, k := range order {
		n := needs[k]
		if n.metric == "" {
			fmt.Fprintf(&b, "- NO METRIC — map the row (rows: {\"<row>\": \"<metric>\"}) or leave it: %s\n", strings.Join(n.rows, "; "))
			continue
		}
		fmt.Fprintf(&b, "- %s: %s\n", n.metric, strings.Join(n.rows, "; "))
	}
	b.WriteString("Translated (check them; a wrong one goes in fixes too):\n")
	for _, r := range rows {
		if r.Formula != "" && !exists[strings.ToLower(r.Metric)] {
			fmt.Fprintf(&b, "  %s = %s\n", r.Metric, r.Formula)
		}
	}
	fmt.Fprintf(&b, "Propose it as ONE step — the server expands it into the sheet's whole plan (grid, metrics, placements):\n"+
		"  propose_actions {\"steps\": [{\"tool\": \"%s\", \"description\": \"Build the %s sheet\", \"params\": {\"sheet\": %q, \"fixes\": {\"<metric>\": \"<formula>\"}, \"skip\": [\"<metric left for later>\"]}}]}\n"+
		"Every metric listed above needs a formula in fixes (or goes in skip). Rows mapped wrongly: pass \"rows\": {\"<row>\": \"<metric>\"} to plan_sheet and the step.\n",
		SheetPlanTool, target.Sheet, target.Sheet)
	return b.String(), nil
}

func (h *handler) metricNamesOf(ctx context.Context, revID string) map[string]bool {
	out := map[string]bool{}
	rows, err := h.db.For(ctx).Query(ctx, `SELECT lower(name) FROM model.metric_def WHERE revision_id = $1::uuid`, revID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			out[n] = true
		}
	}
	return out
}

// expandSheetPlans replaces each sheet_plan step with the sheet's plan: its
// grid (when new) with its dimensions, a create_metric per metric not yet
// built — the server's formula, or the AI's fix — and its placement.
func (h *handler) expandSheetPlans(ctx context.Context, sessionID, revID string, steps []aiassistant.ProposalStep) ([]aiassistant.ProposalStep, error) {
	has := false
	for _, s := range steps {
		if s.Tool == SheetPlanTool {
			has = true
		}
	}
	if !has {
		return steps, nil
	}
	exists := h.metricNamesOf(ctx, revID)
	var out []aiassistant.ProposalStep
	for _, s := range steps {
		if s.Tool != SheetPlanTool {
			out = append(out, s)
			continue
		}
		var p struct {
			Sheet string            `json:"sheet"`
			Fixes map[string]string `json:"fixes"`
			Skip  []string          `json:"skip"`
			Rows  map[string]string `json:"rows"`
		}
		if err := json.Unmarshal(s.Params, &p); err != nil || strings.TrimSpace(p.Sheet) == "" {
			return nil, fmt.Errorf("sheet_plan needs params {\"sheet\": …, \"fixes\": {…}, \"skip\": […]}")
		}
		overrides := map[int]string{}
		for k, v := range p.Rows {
			if n, err := strconv.Atoi(strings.TrimSpace(k)); err == nil {
				overrides[n] = v
			}
		}
		rows, target, err := h.sheetPlanRows(ctx, sessionID, revID, p.Sheet, overrides)
		if err != nil {
			return nil, err
		}
		fixes := map[string]string{}
		for k, v := range p.Fixes {
			fixes[strings.ToLower(strings.TrimSpace(k))] = strings.TrimPrefix(strings.TrimSpace(v), "=")
		}
		skip := map[string]bool{}
		for _, k := range p.Skip {
			skip[strings.ToLower(strings.TrimSpace(k))] = true
		}
		var gridID string
		_ = h.db.QueryRow(ctx, `SELECT id::text FROM model.grid_def WHERE revision_id = $1::uuid AND lower(name) = lower($2)`, revID, target.Grid).Scan(&gridID)
		if gridID == "" {
			gp, _ := json.Marshal(map[string]any{"name": target.Grid})
			out = append(out, aiassistant.ProposalStep{Tool: "create_grid", Description: "Create the " + target.Grid + " grid (sheet " + target.Sheet + ")", Params: gp})
			for _, d := range target.Dimensions {
				dp, _ := json.Marshal(map[string]any{"grid_id": target.Grid, "dimension_id": d})
				out = append(out, aiassistant.ProposalStep{Tool: "add_grid_dimension", Description: "Put " + d + " on " + target.Grid, Params: dp})
			}
		}
		var missing []string
		missingSeen := map[string]bool{}
		done := map[string]bool{}
		var made []string
		for _, r := range rows {
			name := strings.ToLower(r.Metric)
			if r.Metric == "" || exists[name] || done[name] || skip[name] {
				continue
			}
			params := map[string]any{"name": r.Metric, "label": r.Label}
			if _, member := memberRowNote(r); member {
				params["label"] = humanName(r.Metric)
			}
			switch {
			case r.Percentage:
				params["format"] = "percentage"
			case strings.Contains(r.Label, "€") || strings.Contains(strings.ToUpper(r.Label), "EUR"):
				params["format"] = "currency"
			default:
				params["format"] = "number"
			}
			switch {
			case fixes[name] != "":
				params["is_input"], params["formula"] = false, fixes[name]
			case r.Input:
				params["is_input"] = true
			case r.Formula != "":
				params["is_input"], params["formula"] = false, r.Formula
			default:
				if !missingSeen[name] {
					missingSeen[name] = true
					missing = append(missing, fmt.Sprintf("%s (row %d: %s)", r.Metric, r.Row, r.Problem))
				}
				continue
			}
			done[name] = true
			mp, _ := json.Marshal(params)
			desc := "Create " + r.Metric
			if r.Formula != "" && fixes[name] == "" {
				desc += " (formula translated from the workbook by the server)"
			}
			out = append(out, aiassistant.ProposalStep{Tool: "create_metric", Description: desc, Params: mp})
			made = append(made, r.Metric)
		}
		if len(missing) > 0 {
			sort.Strings(missing)
			return nil, fmt.Errorf("sheet_plan %s: these metrics need a formula in fixes, or go in skip: %s", target.Sheet, strings.Join(missing, "; "))
		}
		for _, m := range made {
			ap, _ := json.Marshal(map[string]any{"grid_id": target.Grid, "metric_id": m})
			out = append(out, aiassistant.ProposalStep{Tool: "add_grid_metric", Description: "Place " + m + " on " + target.Grid, Params: ap})
		}
	}
	return out, nil
}

func memberRowNote(r aiassistant.SheetRow) (string, bool) {
	return r.Problem, strings.HasPrefix(r.Problem, "member row:")
}

func humanName(m string) string {
	s := strings.ReplaceAll(m, "_", " ")
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// sheetGeneratedWhole reports whether the server's plan of a sheet leaves
// nothing to the AI: every row mapped, and every metric not yet built an
// input or a translated formula.
func (h *handler) sheetGeneratedWhole(ctx context.Context, sessionID, revID, sheet string) bool {
	rows, _, err := h.sheetPlanRows(ctx, sessionID, revID, sheet, nil)
	if err != nil || len(rows) == 0 {
		return false
	}
	exists := h.metricNamesOf(ctx, revID)
	for _, r := range rows {
		if r.Metric == "" {
			return false
		}
		if !exists[strings.ToLower(r.Metric)] && !r.Input && r.Formula == "" {
			return false
		}
	}
	return true
}

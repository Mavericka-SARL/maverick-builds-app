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

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
)

// maxProposalRejections is how many failing plans the assistant may always
// send back in one turn; more while the failures keep falling (planRetries).
const maxProposalRejections = 3

// proposalCheck is what a dry run of a proposal found.
type proposalCheck struct {
	problems []string // one line per step that would fail
	// placed: metrics the server placed on a grid (placeUnplacedMetrics).
	placed    map[string]bool
	unchecked []int // 1-based steps a dry run cannot run (or that use their results)
	// warnings: formulas that run but look wrong (planWarnings) — only
	// looked for once nothing fails.
	warnings []string
	// failed: the 0-based steps that failed (a step left out with them is
	// found by DropSteps), for the plan of the steps that pass.
	failed map[int]bool
	// dropped: failing layout steps (aiassistant.PresentationTool) the
	// server leaves out instead of refusing the plan around them.
	dropped map[int]bool
	// autoSteps: steps the server adds where the fix is certain — a metric
	// the plan leaves off every grid, read by a metric on a grid.
	autoSteps []aiassistant.InsertedStep
	// notes: what the server changed in the plan, told to the developer
	// beside it and to the model in its tool result; they never send the
	// plan back.
	notes []string
	// steps: the plan as the server left it (nil: as proposed).
	steps []aiassistant.ProposalStep
}

// planSteps is the plan to propose: as the server edited it, or as given.
func (c proposalCheck) planSteps(given []aiassistant.ProposalStep) []aiassistant.ProposalStep {
	if c.steps != nil {
		return c.steps
	}
	return given
}

// aiCheckProposal runs steps on modelID/revID as confirming them would, in
// a transaction it rolls back: every tool, workflows, forms, automation
// rules, access rules and file imports included (their stores are built on
// the transaction). A step that fails is a problem, and a step using its
// result is not run; a step that panics is left to the confirmation.
func (h *handler) aiCheckProposal(ctx context.Context, sessionID, modelID, revID, userID string, steps []aiassistant.ProposalStep) (proposalCheck, error) {
	check, err := h.checkProposalOnce(ctx, sessionID, modelID, revID, userID, steps)
	if err == nil && len(check.autoSteps) > 0 {
		// Placements the server adds: checked again with them in.
		edited := aiassistant.InsertSteps(steps, check.autoSteps)
		if again, aErr := h.checkProposalOnce(ctx, sessionID, modelID, revID, userID, edited); aErr == nil {
			again.notes = append(check.notes, again.notes...)
			again.steps = edited
			check = again
		}
	}
	if err == nil && len(check.problems) == 0 && len(check.dropped) > 0 {
		kept, _ := aiassistant.DropSteps(check.planSteps(steps), check.dropped)
		check.steps = kept
	}
	return check, err
}

// checkProposalOnce is one dry run of steps with its findings.
func (h *handler) checkProposalOnce(ctx context.Context, sessionID, modelID, revID, userID string, steps []aiassistant.ProposalStep) (proposalCheck, error) {
	tx, err := h.db.For(ctx).Begin(ctx)
	if err != nil {
		return proposalCheck{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	out, created, err := h.runStepsOn(ctx, tx, sessionID, modelID, revID, userID, steps)
	if err == nil && len(out.problems) == 0 {
		resolved := make([]aiassistant.ProposalStep, len(steps))
		for i, s := range steps {
			s.Params = resolveParamRefs(storedParams(s.Params), created)
			resolved[i] = s
		}
		out.problems = append(out.problems, aiassistant.PlanWorkflowProblems(ctx, tx, modelID, revID, resolved)...)
		out.problems = append(out.problems, aiassistant.PlanRecordWorkflowProblems(ctx, tx, modelID, revID, resolved)...)
		offGrid, auto := offGridReadProblems(ctx, tx, modelID, revID, resolved, created)
		out.problems = append(out.problems, offGrid...)
		for _, a := range auto {
			out.autoSteps = append(out.autoSteps, a.step)
			out.notes = append(out.notes, a.note)
		}
		placements, placed := h.placeUnplacedMetrics(ctx, tx, sessionID, modelID, revID, resolved, created)
		for _, a := range placements {
			out.autoSteps = append(out.autoSteps, a.step)
			out.notes = append(out.notes, a.note)
		}
		out.placed = placed
		out.problems = append(out.problems, unorderedPagesProblems(ctx, tx, revID, resolved)...)
		out.problems = append(out.problems, dashboardLayoutProblems(ctx, tx, revID, resolved, created)...)
	}
	// The formula and value warnings come with the failures too, read on the
	// steps that ran: a plan used to go back once per kind of finding — the
	// failures, then the warnings, then the request's coverage — and a stage
	// took four rounds where one would do (2026-11-08).
	if err == nil {
		out.warnings = planWarnings(ctx, tx, modelID, revID, steps)
		resolved := make([]aiassistant.ProposalStep, len(steps))
		for i, s := range steps {
			s.Params = resolveParamRefs(storedParams(s.Params), created)
			resolved[i] = s
		}
		out.warnings = append(out.warnings, currencyWarnings(ctx, tx, modelID, revID, resolved)...)
		out.warnings = append(out.warnings, h.constantFormulaWarnings(ctx, revID, "", resolved)...)
	}
	if err == nil && len(out.problems) == 0 {
		resolved := make([]aiassistant.ProposalStep, len(steps))
		for i, s := range steps {
			s.Params = resolveParamRefs(storedParams(s.Params), created)
			resolved[i] = s
		}
		out.warnings = append(out.warnings, unplacedMetricWarnings(ctx, tx, modelID, revID, resolved, created, out.placed)...)
		out.warnings = append(out.warnings, aiassistant.PlanConditionTotalWarnings(ctx, tx, modelID, revID, resolved)...)
		out.warnings = append(out.warnings, aiassistant.ScenarioTotalWarnings(resolved)...)
		out.warnings = append(out.warnings, kpiTileWarnings(ctx, tx, resolved, created)...)
		out.warnings = append(out.warnings, dashboardLayoutWarnings(ctx, tx, revID, resolved, created)...)
		out.warnings = append(out.warnings, otherDashboardsWarnings(ctx, tx, revID, resolved, created)...)
		// Page mistakes (ai_page_warnings.go); a text's old content is read
		// as it is, outside the dry run.
		out.warnings = append(out.warnings, textRewriteWarnings(ctx, h.db.For(ctx), resolved)...)
		out.warnings = append(out.warnings, pageRoleWarnings(ctx, tx, modelID, resolved, created)...)
		out.warnings = append(out.warnings, pageMetricWarnings(ctx, tx, resolved, created)...)
		out.warnings = append(out.warnings, splitWidgetWarnings(ctx, tx, resolved, created)...)
		out.warnings = append(out.warnings, untaggedWarnings(ctx, tx, revID, resolved, created)...)
	}
	return out, err
}

// runStepsOn runs steps on tx as confirming them would and returns what each
// created (by step; "" for a step that created nothing or did not run).
func (h *handler) runStepsOn(ctx context.Context, tx pgx.Tx, sessionID, modelID, revID, userID string, steps []aiassistant.ProposalStep) (proposalCheck, []string, error) {
	var out proposalCheck
	live := h.aiWriteHooks(modelID, userID)
	// Connectors and connections are saved on tx as confirming saves them
	// (a connection without its credential, which only the card holds); a
	// run or a trigger reaches outside the database and is left to the
	// confirmation.
	a, aerr := h.actorByUserID(ctx, userID)
	ex := aiassistant.NewDryRunWriteExecutor(tx, modelID, revID, userID).WithHooks(aiassistant.Hooks{
		SaveConnector: func(ctx context.Context, req aiassistant.ConnectorSave) (string, error) {
			if aerr != nil {
				return "", aerr
			}
			return h.aiSaveConnector(ctx, tx, a, modelID, revID, req)
		},
		SaveConnection: func(ctx context.Context, req aiassistant.ConnectionSave) (string, error) {
			if aerr != nil {
				return "", aerr
			}
			return h.aiSaveConnection(ctx, tx, a, modelID, req, nil, true)
		},
		RunIntegration: func(context.Context, string, string) (string, error) { return "", aiassistant.ErrNotDryRunnable },
		TriggerRule: func(context.Context, string, map[string]any) (string, error) {
			return "", aiassistant.ErrNotDryRunnable
		},
		Recalculate: func(context.Context, string) (string, error) { return "", aiassistant.ErrNotDryRunnable },
		ClearValues: func(context.Context, string, string, json.RawMessage) (string, error) {
			return "", aiassistant.ErrNotDryRunnable
		},
		FillSample: func(context.Context, string, json.RawMessage, bool) (string, error) {
			return "", aiassistant.ErrNotDryRunnable
		},
		CheckMetrics: live.CheckMetrics, CheckMembers: live.CheckMembers,
		ImportFile: func(ctx context.Context, req aiassistant.FileImportRequest) (string, error) {
			return h.aiCheckImportFile(ctx, tx, sessionID, userID, req)
		},
		WriteValues: func(ctx context.Context, req aiassistant.ValuesWriteRequest) (string, error) {
			return h.aiCheckWriteValues(ctx, tx, req)
		},
		PostFormIntegration: func(ctx context.Context, mappingID string) (int, error) {
			return checkFormPosting(ctx, tx, mappingID)
		},
		RecallPreview: func(ctx context.Context, file, sheet string) (aiassistant.FileImportRequest, bool) {
			return h.recallCleanPreview(ctx, sessionID, file, sheet)
		},
	})
	created := make([]string, len(steps))
	blocked := map[int]bool{} // 0-based steps whose result later steps cannot use
	filled := aiassistant.FilledLater(steps)
	// failedNames: a metric or grid a failed step was to create or place,
	// lower-cased, with that step's number. A later step failing over one
	// of them is told in one line, not with its own long error: one wrong
	// formula used to bring a dozen "not found" and "not on a grid with a
	// time dimension" errors, each listing names, and the model chased those
	// (2026-11-07).
	failedNames := map[string]int{}
	out.failed = map[int]bool{}
	fail := func(i int, step aiassistant.ProposalStep, err error) {
		blocked[i] = true
		msg := err.Error()
		if aiassistant.PresentationTool(step.Tool) {
			if out.dropped == nil {
				out.dropped = map[int]bool{}
			}
			out.dropped[i] = true
			out.notes = append(out.notes, fmt.Sprintf("Left out by the server: step %d (%s — %s), a layout step that fails: %s. Everything else in the plan runs without it; propose it again on its own once it is fixed.",
				i+1, step.Tool, step.Description, msg))
			return
		}
		out.failed[i] = true
		low := strings.ToLower(msg)
		for name, root := range failedNames {
			if strings.Contains(low, `"`+name+`"`) || strings.Contains(low, `'`+name+`'`) {
				out.problems = append(out.problems, fmt.Sprintf("step %d (%s — %s): uses %q, which step %d was to make and fails — fix step %d", i+1, step.Tool, step.Description, name, root, root))
				recordFailedNames(failedNames, step, i+1)
				return
			}
		}
		out.problems = append(out.problems, fmt.Sprintf("step %d (%s — %s): %s", i+1, step.Tool, step.Description, msg))
		recordFailedNames(failedNames, step, i+1)
	}
	for i, step := range steps {
		if _, ok := usesBlockedStep(step.Params, blocked); ok {
			blocked[i] = true
			out.unchecked = append(out.unchecked, i+1)
			continue
		}
		lintErr := aiassistant.LintProposalStep(step.Tool, storedParams(step.Params))
		if filled[i] && aiassistant.IsPlaceholderFormula(lintErr) {
			lintErr = nil
		}
		if lintErr == nil {
			lintErr = percentUnitsLint(ctx, tx, modelID, revID, step)
		}
		if lintErr != nil {
			fail(i, step, lintErr)
			continue
		}
		sp, err := tx.Begin(ctx) // a savepoint: one failure must not poison the rest
		if err != nil {
			return out, created, err
		}
		id, stepErr := runChecked(ctx, ex, step.Tool, resolveParamRefs(storedParams(step.Params), created[:i]))
		switch {
		case errors.Is(stepErr, aiassistant.ErrNotDryRunnable):
			_ = sp.Rollback(ctx)
			blocked[i] = true
			out.unchecked = append(out.unchecked, i+1)
		case stepErr != nil:
			_ = sp.Rollback(ctx)
			fail(i, step, stepErr)
		default:
			if err := sp.Commit(ctx); err != nil {
				return out, created, err
			}
			created[i] = id
		}
	}
	return out, created, nil
}

// recordFailedNames notes the metric or grid a failed step (number n) was to
// create, place or change, by name.
func recordFailedNames(failed map[string]int, step aiassistant.ProposalStep, n int) {
	var p struct {
		Name     string `json:"name"`
		Metric   string `json:"metric"`
		MetricID string `json:"metric_id"`
	}
	if json.Unmarshal(storedParams(step.Params), &p) != nil {
		return
	}
	for _, v := range []string{p.Name, p.Metric, p.MetricID} {
		v = strings.ToLower(strings.TrimSpace(v))
		if v == "" || len(v) == 36 && strings.Count(v, "-") == 4 { // an id names nothing a message quotes
			continue
		}
		if _, seen := failed[v]; !seen {
			failed[v] = n
		}
	}
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

// aiCheckImportFile is ImportFile for a proposal's dry run, on tx. A
// dimension file is imported for real into the transaction — members,
// parents, properties, a time dimension's periods — as the plan's later
// steps will find them. A grid file is reshaped, mapped and resolved against
// the model as the plan's earlier steps leave it, and refused exactly as the
// import refuses it; its values are not staged (that store works on the
// pool), so nothing is written for them.
func (h *handler) aiCheckImportFile(ctx context.Context, tx pgx.Tx, sessionID, userID string, req aiassistant.FileImportRequest) (string, error) {
	f, err := h.loadAttachedFile(ctx, sessionID, &req)
	if err != nil {
		return "", err
	}
	if req.TargetType == "form" {
		// Records are checked outside the plan's transaction (the records
		// path runs on its own): a table this plan creates cannot be checked.
		var exists bool
		_ = h.db.For(ctx).QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model.form_def WHERE id=$1::uuid)`, req.TargetID).Scan(&exists)
		if !exists {
			return "", aiassistant.ErrNotDryRunnable
		}
		n, problems, table, err := h.aiTableImport(ctx, userID, f, req.TargetID, h.aiReplaceBy(ctx, req), true)
		if err == nil && problems != "" {
			err = fmt.Errorf("import rejected: %s", strings.TrimSpace(problems))
		}
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d record(s) from %s into table '%s'", n, f.describe(), table), nil
	}
	if req.TargetType == "dimension" {
		cr, colIdx, err := dimensionCSV(f)
		if err != nil {
			return "", err
		}
		imported, _, _, notices, err := h.importDimensionMembersOn(ctx, tx, req.TargetID, cr, colIdx)
		if err != nil {
			return "", err
		}
		msg := fmt.Sprintf("%d member(s) from %s", imported, f.describe())
		for _, n := range notices {
			msg += ". NOTE: " + aiPreexistingValuesOffer(n)
		}
		return msg, nil
	}
	var modelID string
	if err := tx.QueryRow(ctx, `SELECT model_id::text FROM model.revision WHERE id=$1::uuid`, req.RevisionID).Scan(&modelID); err != nil {
		return "", fmt.Errorf("working revision not found")
	}
	if err := importpkg.CheckGridColumns(ctx, tx, req.TargetID, f.mapped); err != nil {
		return "", fmt.Errorf("%w. %s", err, h.suggestColumnMap(ctx, tx, req, f))
	}
	staged, importErrs, err := importpkg.ResolveRows(ctx, tx, modelID, req.RevisionID, f.mapped, f.rows)
	if err != nil {
		return "", fmt.Errorf("%w. Map each column to a model name or \"ignore\" in column_map, or add \"*\": \"ignore\" to drop every column it does not name. %s", err, h.suggestColumnMap(ctx, tx, req, f))
	}
	if len(importErrs) > 0 {
		rows := make([]map[string]any, 0, len(importErrs))
		unknownMetric := ""
		for _, e := range importErrs {
			rows = append(rows, map[string]any{"row": e.RowNumber, "column": e.Column, "message": e.Message})
			if e.ErrorCode == "UNKNOWN_METRIC" && unknownMetric == "" {
				unknownMetric = e.RawValue
			}
		}
		// Row labels where metric names belong: say how to map them — the
		// AI imported the Assumptions sheet's labels five times without a
		// value_map and then gave up (2026-11-12).
		hint := ""
		if unknownMetric != "" {
			col := "<the column mapped to \"metric\">"
			for c, field := range req.ColumnMap {
				if field == "metric" {
					col = c
				}
			}
			hint = fmt.Sprintf("\nThe file holds labels where metric names belong: map each to its metric with reshape.value_map {%q: {%q: \"<metric name>\", …}} "+
				"(list_metrics gives the names), in the same reshape.", col, unknownMetric)
		}
		return "", fmt.Errorf("import rejected: %d value(s) in %s failed validation, nothing would be imported:\n%s%s",
			len(rows), f.describe(), importErrorLines(rows, 10), hint)
	}
	if len(staged) == 0 {
		return "", fmt.Errorf("%s holds no values for any metric", f.describe())
	}
	if req.PercentFractions {
		h.scalePercentFractions(ctx, tx, staged)
	}
	// Fractions into a Percentage metric are refused here, not just warned
	// of: the assistant imported 0.056 for 5.6% after previews that warned.
	if w := h.percentFractionWarning(ctx, tx, staged); w != "" && !req.ValuesArePercentUnits {
		return "", fmt.Errorf("%s (if these really are percents under 1%%, pass \"values_are_percent_units\": true)", strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(w), "WARNING: ")))
	}
	return fmt.Sprintf("%d value(s) from %s resolve", len(staged), f.describe()), nil
}

// checkFormPosting is PostFormIntegration for a proposal's dry run: the
// mapping the plan made or changed is read on tx and the records it would
// post are counted; the postings are written when the plan is confirmed.
func checkFormPosting(ctx context.Context, tx pgx.Tx, mappingID string) (int, error) {
	var n int
	if err := tx.QueryRow(ctx, `
		SELECT count(fr.id) FROM model.form_metric_mapping m
		JOIN model.form_def fd ON fd.id = m.form_id AND fd.model_id = m.model_id
		LEFT JOIN runtime.form_record fr ON fr.form_id = m.form_id
		     AND (fr.status IS NULL OR fr.status::text = ANY(m.posting_statuses::text[])
		          OR NOT EXISTS (SELECT 1 FROM jsonb_array_elements(CASE WHEN jsonb_typeof(fd.fields) = 'array' THEN fd.fields ELSE '[]'::jsonb END) e
		                         WHERE e->>'type' = 'status'))
		WHERE m.id = $1::uuid GROUP BY m.id`, mappingID).Scan(&n); err != nil {
		return 0, fmt.Errorf("form integration %s not found", mappingID)
	}
	return n, nil
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
func (c proposalCheck) rejection(mode retryMode) string {
	var sb strings.Builder
	sb.WriteString("Proposal NOT shown to the developer: run against the model exactly as confirming would, it fails —\n")
	for _, p := range c.problems {
		sb.WriteString("- " + p + "\n")
	}
	if len(c.unchecked) > 0 {
		fmt.Fprintf(&sb, "(Steps %s could not be checked: they use the result of a step that failed.)\n", joinInts(c.unchecked))
	}
	if len(c.warnings) > 0 && mode == retryFix {
		sb.WriteString("Also, in steps that run — fix these in the same plan, or keep one you mean and say why:\n")
		for _, w := range c.warnings {
			sb.WriteString("- " + w + "\n")
		}
	}
	switch mode {
	case retryPartial:
		sb.WriteString("Call propose_actions once more with ONLY the steps that pass — leave out the failing steps and the steps that use them — so the developer can confirm that much now. Do not ask whether to: propose it, and in your reply list the steps left out and why they fail.")
	case retryStop:
		sb.WriteString("Do not propose again in this turn. Tell the developer, in a short list, which steps fail and why, and what you would change; do not ask whether to propose — their next message says how to go on.")
	default:
		sb.WriteString("Fix every failing step, the ones that fail on their own first — read names you are unsure of with list_metrics {\"names\": [...]} or {\"search\": \"...\"}, not the whole list — and call propose_actions again with the WHOLE corrected plan.")
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

// percentUnitsLint refuses a formula adding a Percentage metric to 1: the
// metric holds percent units (5.6 for 5.6%), so the growth factor is
// 1 + p / 100. The revision's metrics as the plan has left them so far are
// visible on the dry run's transaction.
func percentUnitsLint(ctx context.Context, tx pgx.Tx, modelID, revID string, step aiassistant.ProposalStep) error {
	if step.Tool != "create_metric" && step.Tool != "update_metric" {
		return nil
	}
	var p struct {
		Name     string `json:"name"`
		Formula  string `json:"formula"`
		MetricID string `json:"metric_id"`
	}
	if json.Unmarshal(step.Params, &p) != nil || strings.TrimSpace(p.Formula) == "" {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT lower(name) FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND format='percentage'`, modelID, revID)
	if err != nil {
		return nil
	}
	pct := map[string]bool{}
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			pct[n] = true
		}
	}
	rows.Close()
	if m := aiassistant.PercentUnitsMisuse(p.Formula, pct); m != "" {
		return fmt.Errorf("%s is a Percentage metric, stored in percent units (5.6 for 5.6%%): a growth factor is 1 + %s / 100, not 1 + %s", m, m, m)
	}
	return nil
}

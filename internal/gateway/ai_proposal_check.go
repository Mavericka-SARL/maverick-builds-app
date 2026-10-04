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

// maxProposalRejections is how many failing plans the assistant may send
// back in one turn before it has to stop and explain.
const maxProposalRejections = 3

// proposalCheck is what a dry run of a proposal found.
type proposalCheck struct {
	problems  []string // one line per step that would fail
	unchecked []int    // 1-based steps a dry run cannot run (or that use their results)
}

// aiCheckProposal runs steps on modelID/revID as confirming them would, in
// a transaction it rolls back: every tool, workflows, forms, automation
// rules, access rules and file imports included (their stores are built on
// the transaction). A step that fails is a problem, and a step using its
// result is not run; a step that panics is left to the confirmation.
func (h *handler) aiCheckProposal(ctx context.Context, sessionID, modelID, revID, userID string, steps []aiassistant.ProposalStep) (proposalCheck, error) {
	var out proposalCheck
	tx, err := h.db.For(ctx).Begin(ctx)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	live := h.aiWriteHooks(modelID, userID)
	ex := aiassistant.NewDryRunWriteExecutor(tx, modelID, revID, userID).WithHooks(aiassistant.Hooks{
		CheckMetrics: live.CheckMetrics, CheckMembers: live.CheckMembers,
		ImportFile: func(ctx context.Context, req aiassistant.FileImportRequest) (string, error) {
			return h.aiCheckImportFile(ctx, tx, sessionID, req)
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
	for i, step := range steps {
		if _, ok := usesBlockedStep(step.Params, blocked); ok {
			blocked[i] = true
			out.unchecked = append(out.unchecked, i+1)
			continue
		}
		lintErr := aiassistant.LintProposalStep(step.Tool, storedParams(step.Params))
		if lintErr == nil {
			lintErr = percentUnitsLint(ctx, tx, modelID, revID, step)
		}
		if lintErr != nil {
			blocked[i] = true
			out.problems = append(out.problems, fmt.Sprintf("step %d (%s — %s): %v", i+1, step.Tool, step.Description, lintErr))
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

// aiCheckImportFile is ImportFile for a proposal's dry run, on tx. A
// dimension file is imported for real into the transaction — members,
// parents, properties, a time dimension's periods — as the plan's later
// steps will find them. A grid file is reshaped, mapped and resolved against
// the model as the plan's earlier steps leave it, and refused exactly as the
// import refuses it; its values are not staged (that store works on the
// pool), so nothing is written for them.
func (h *handler) aiCheckImportFile(ctx context.Context, tx pgx.Tx, sessionID string, req aiassistant.FileImportRequest) (string, error) {
	f, err := h.loadAttachedFile(ctx, sessionID, req)
	if err != nil {
		return "", err
	}
	if req.TargetType == "dimension" {
		cr, colIdx, err := dimensionCSV(f)
		if err != nil {
			return "", err
		}
		imported, _, _, err := h.importDimensionMembersOn(ctx, tx, req.TargetID, cr, colIdx)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("%d member(s) from %s", imported, f.describe()), nil
	}
	var modelID string
	if err := tx.QueryRow(ctx, `SELECT model_id::text FROM model.revision WHERE id=$1::uuid`, req.RevisionID).Scan(&modelID); err != nil {
		return "", fmt.Errorf("working revision not found")
	}
	if err := importpkg.CheckGridColumns(ctx, tx, req.TargetID, f.mapped); err != nil {
		return "", fmt.Errorf("%w. %s", err, h.suggestColumnMap(ctx, req, f))
	}
	staged, importErrs, err := importpkg.ResolveRows(ctx, tx, modelID, req.RevisionID, f.mapped, f.rows)
	if err != nil {
		return "", fmt.Errorf("%w. Map each column to a model name or \"ignore\" in column_map. %s", err, h.suggestColumnMap(ctx, req, f))
	}
	if len(importErrs) > 0 {
		rows := make([]map[string]any, 0, len(importErrs))
		for _, e := range importErrs {
			rows = append(rows, map[string]any{"row": e.RowNumber, "column": e.Column, "message": e.Message})
		}
		return "", fmt.Errorf("import rejected: %d value(s) in %s failed validation, nothing would be imported:\n%s",
			len(rows), f.describe(), importErrorLines(rows, 10))
	}
	if len(staged) == 0 {
		return "", fmt.Errorf("%s holds no values for any metric", f.describe())
	}
	// Fractions into a Percentage metric are refused here, not just warned
	// of: the assistant imported 0.056 for 5.6% after previews that warned.
	if w := h.percentFractionWarning(ctx, staged); w != "" && !req.ValuesArePercentUnits {
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
		LEFT JOIN runtime.form_record fr ON fr.form_id = m.form_id AND fr.status::text = ANY(m.posting_statuses::text[])
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
func (c proposalCheck) rejection(attempt int) string {
	var sb strings.Builder
	sb.WriteString("Proposal NOT shown to the developer: run against the model exactly as confirming would, it fails —\n")
	for _, p := range c.problems {
		sb.WriteString("- " + p + "\n")
	}
	if len(c.unchecked) > 0 {
		fmt.Fprintf(&sb, "(Steps %s could not be checked: they use the result of a step that failed.)\n", joinInts(c.unchecked))
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

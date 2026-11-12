package gateway

// The server's check of a confirmed plan's results, at the start of the next
// turn, in every session (the value check against an attached file needs one,
// ai_build_guards.go). A plan runs in the plan check without calculating, so
// three faults only show once it is confirmed and recalculated, and each went
// unseen in the High-Tech build (2026-11-11):
//   - a metric with no values at all — a total read over a dimension that
//     totals by none, a cycle that fails: blank cells and no error anywhere;
//   - a calculation that fails, recorded per metric and shown nowhere the AI
//     or the developer was looking;
//   - an IFERROR that returns its fallback in every cell: paid trials read 0
//     in every month for about twenty turns, the reference inside was wrong.

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/calculation"
)

// afterConfirmHead starts the note.
const afterConfirmHead = "Checked by the server after the confirmed plan was calculated:"

// afterConfirmCheck returns the note for the plans confirmed since the
// session's last reply, or "" when they left nothing to report.
func (h *handler) afterConfirmCheck(ctx context.Context, ps *aiassistant.ProposalStore, sessionID, modelID, revID string,
	history []aiassistant.ChatMessage, status func(string)) string {
	// Since the developer's previous message: history holds this turn's
	// message last, and confirming saves a reply of its own after the plan
	// runs.
	var userAt []time.Time
	for _, m := range history {
		if m.Role == "user" {
			userAt = append(userAt, m.CreatedAt)
		}
	}
	if len(userAt) < 2 {
		return ""
	}
	lastReply := userAt[len(userAt)-2]
	props, err := ps.ListProposals(ctx, sessionID)
	if err != nil {
		return ""
	}
	var refs []string
	for _, p := range props {
		if (p.Status != "executed" && p.Status != "partial") || p.ExecutedAt == nil || !p.ExecutedAt.After(lastReply) {
			continue
		}
		for _, st := range p.Steps {
			if st.Tool != "create_metric" && st.Tool != "update_metric" {
				continue
			}
			var sp struct {
				Name     string `json:"name"`
				MetricID string `json:"metric_id"`
			}
			if json.Unmarshal(st.Params, &sp) == nil {
				refs = append(refs, strings.ToLower(cmpOr(sp.MetricID, sp.Name)))
			}
		}
	}
	if len(refs) == 0 {
		return ""
	}
	status("checking the confirmed plan's results")
	if !h.waitRecalculated(ctx, revID, 2*time.Minute) {
		return ""
	}
	q := h.db.For(ctx)
	type touched struct{ id, name, formula string }
	var metrics []touched
	rows, err := q.Query(ctx, `
		SELECT m.id::text, m.name, COALESCE(m.formula,'')
		FROM model.metric_def m
		WHERE m.model_id = $1::uuid AND m.revision_id = $2::uuid AND NOT m.is_input
		  AND COALESCE(btrim(m.formula),'') <> ''
		  AND (lower(m.name) = ANY($3) OR m.id::text = ANY($3))
		  AND EXISTS (SELECT 1 FROM model.grid_metric gm WHERE gm.metric_id = m.id)
		ORDER BY m.name`, modelID, revID, refs)
	if err != nil {
		return ""
	}
	for rows.Next() {
		var t touched
		if rows.Scan(&t.id, &t.name, &t.formula) == nil {
			metrics = append(metrics, t)
		}
	}
	rows.Close()

	store := calculation.NewStore(q)
	var blank, fallback []string
	for _, m := range metrics {
		values, err := store.LoadCalcValueMap(ctx, modelID, revID, m.id)
		if err != nil {
			continue
		}
		if len(values) == 0 {
			blank = append(blank, m.name)
			continue
		}
		if fb, ok := everyCellIsFallback(m.formula, values); ok {
			fallback = append(fallback, fmt.Sprintf("%s: %d cell(s), every one %s — the IFERROR fallback", m.name, len(values), strconv.FormatFloat(fb, 'f', -1, 64)))
		}
	}

	var failed []string
	if erows, err := q.Query(ctx, `
		SELECT DISTINCT ON (m.name) m.name, s.error
		FROM runtime.metric_partition_state s
		JOIN model.metric_def m ON m.id = s.metric_id
		WHERE m.model_id = $1::uuid AND m.revision_id = $2::uuid AND s.status = 'error' AND COALESCE(s.error,'') <> ''
		ORDER BY m.name, s.updated_at DESC
		LIMIT 12`, modelID, revID); err == nil {
		for erows.Next() {
			var name, msg string
			if erows.Scan(&name, &msg) == nil {
				if len(msg) > 300 {
					msg = msg[:300] + "…"
				}
				failed = append(failed, name+": "+msg)
			}
		}
		erows.Close()
	}
	if len(blank)+len(fallback)+len(failed) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(afterConfirmHead)
	if len(failed) > 0 {
		b.WriteString("\nThe calculation fails for (the model shows these blank, and what reads them too):")
		for _, f := range failed {
			b.WriteString("\n- " + f)
		}
	}
	if len(blank) > 0 {
		fmt.Fprintf(&b, "\nNo values at all after the recalculation: %s — typically a total read over a dimension that totals by none, a cycle that fails, or a formula reading a metric with no values. "+
			"Find why before going on; a blank is never a zero.", strings.Join(blank, ", "))
	}
	if len(fallback) > 0 {
		b.WriteString("\nIFERROR returns its fallback in every cell — the expression inside most likely fails everywhere (a wrong name or member):")
		for _, f := range fallback {
			b.WriteString("\n- " + f)
		}
		b.WriteString("\nEvaluate the expression without IFERROR to see its error, and fix it.")
	}
	return b.String()
}

// everyCellIsFallback reports whether every value is the numeric fallback of
// one of formula's IFERROR calls (at least two cells: one is no pattern).
func everyCellIsFallback(formula string, values map[string]float64) (float64, bool) {
	if len(values) < 2 {
		return 0, false
	}
	for _, fb := range iferrorFallbacks(formula) {
		all := true
		for _, v := range values {
			if v != fb {
				all = false
				break
			}
		}
		if all {
			return fb, true
		}
	}
	return 0, false
}

// iferrorFallbacks are the numeric second arguments of formula's IFERROR
// calls.
func iferrorFallbacks(formula string) []float64 {
	var out []float64
	up := strings.ToUpper(formula)
	for i := 0; ; {
		j := strings.Index(up[i:], "IFERROR(")
		if j < 0 {
			break
		}
		start := i + j + len("IFERROR(")
		depth, comma, end := 0, -1, -1
		inQuote := false
		for k := start; k < len(formula) && end < 0; k++ {
			switch c := formula[k]; {
			case c == '"':
				inQuote = !inQuote
			case inQuote:
			case c == '(':
				depth++
			case c == ')':
				if depth == 0 {
					end = k
				} else {
					depth--
				}
			case c == ',' && depth == 0 && comma < 0:
				comma = k
			}
		}
		if comma > 0 && end > comma {
			if v, err := strconv.ParseFloat(strings.TrimSpace(formula[comma+1:end]), 64); err == nil {
				out = append(out, v)
			}
		}
		i = start
	}
	return out
}

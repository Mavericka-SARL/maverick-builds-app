package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// Freezing values: a metric's values as they are now — calculated or typed —
// written into an input metric, so a later formula can compare with them.
// The weekly cash forecast captured for the coming week, kept for the
// forecast-vs-actual review once the week closes; an approved forecast kept
// beside the live one. Copy values copies inputs only and an approval keeps
// its figures where no formula reads them, so the 13-week cash model's
// Treasury typed nine figures every week (2026-11-20).
//
// A freeze names pairs (from → to) and the members it is at ("at"): each
// dimension of a target metric is either pinned there or shared with its
// source (then every leaf member of it is frozen); a dimension only the
// source has is pinned or read at its total. Each value is read by the
// grid's own read (the person's access: a withheld value refuses the freeze)
// and written as a typed one (prepareCell: access rules, locks, closed and
// posted cells), all or nothing.

type freezePair struct {
	From string `json:"from"` // metric id or name
	To   string `json:"to"`   // input metric id or name
}

type freezeSpec struct {
	Pairs []freezePair `json:"pairs"`
	// At pins dimensions, by id or name, to a member code. In a workflow's
	// on_approve freeze a value "{key}" is the run's context field key.
	At map[string]string `json:"at"`
}

type frozenValue struct {
	To      string            `json:"to"`
	Members map[string]string `json:"members"` // dimension name → member code
	Value   float64           `json:"value"`
}

type freezeMetric struct {
	id, name, label, format, gridID string
	isInput                         bool
	dims                            map[string]string // id → name
}

// freezeValues runs spec in revisionID of modelID: values read as the caller
// of r, written as writer. Nothing is written on a dry run, or when any value
// is refused (the answer's refusal says why).
func (h *handler) freezeValues(ctx context.Context, r *http.Request, writer *actor, modelID, revisionID string, spec freezeSpec, dryRun bool) ([]frozenValue, *cellRefusal, error) {
	if len(spec.Pairs) == 0 {
		return nil, refuseCell(http.StatusBadRequest, "name at least one pair of metrics: from and to"), nil
	}
	metric := func(ref string) (*freezeMetric, *cellRefusal, error) {
		m := &freezeMetric{dims: map[string]string{}}
		err := h.db.QueryRow(ctx, `
			SELECT md.id::text, md.name, COALESCE(NULLIF(md.label,''), md.name), COALESCE(md.format,''), md.is_input,
			       COALESCE((SELECT gm.grid_id::text FROM model.grid_metric gm WHERE gm.metric_id = md.id LIMIT 1), '')
			FROM model.metric_def md
			WHERE md.model_id = $1::uuid AND md.revision_id = $2::uuid AND (md.id::text = $3 OR lower(md.name) = lower($3))`,
			modelID, revisionID, strings.TrimSpace(ref)).Scan(&m.id, &m.name, &m.label, &m.format, &m.isInput, &m.gridID)
		if err != nil {
			return nil, refuseCell(http.StatusBadRequest, "no metric %q in this revision", ref), nil
		}
		if m.gridID == "" {
			return nil, refuseCell(http.StatusBadRequest, "%s is on no grid: it has no values to freeze", m.label), nil
		}
		if _, m.dims, err = h.metricOwnDimensions(ctx, m.id); err != nil {
			return nil, nil, err
		}
		return m, nil, nil
	}
	// The pins, by dimension id; a name is resolved in the revision.
	pins := map[string]string{}
	dimName := map[string]string{}
	for ref, code := range spec.At {
		var id, name string
		if err := h.db.QueryRow(ctx, `
			SELECT id::text, name FROM model.dimension_def
			WHERE model_id = $1::uuid AND revision_id = $2::uuid AND (id::text = $3 OR lower(name) = lower($3))`,
			modelID, revisionID, strings.TrimSpace(ref)).Scan(&id, &name); err != nil {
			return nil, refuseCell(http.StatusBadRequest, "no dimension %q in this revision", ref), nil
		}
		pins[id], dimName[id] = strings.TrimSpace(code), name
	}
	numeric := func(f string) bool {
		return f == "" || f == "number" || f == "currency" || f == "percentage" || f == "date"
	}

	type cell struct {
		to     *freezeMetric
		codes  map[string]string // target coordinate, dimension id → code
		source map[string]string // the source's read scope
	}
	var cells []cell
	var froms []*freezeMetric
	for _, p := range spec.Pairs {
		from, ref, err := metric(p.From)
		if err != nil || ref != nil {
			return nil, ref, err
		}
		to, ref, err := metric(p.To)
		if err != nil || ref != nil {
			return nil, ref, err
		}
		switch {
		case !to.isInput:
			return nil, refuseCell(http.StatusBadRequest, "%s is calculated: values are frozen into an input metric", to.label), nil
		case !numeric(from.format) || !numeric(to.format):
			return nil, refuseCell(http.StatusBadRequest, "only numbers, amounts, percentages and dates are frozen (%s → %s)", from.label, to.label), nil
		}
		// Each target dimension: pinned, or shared with the source and frozen
		// member by member over its leaves.
		axes := []string{}
		var choices [][]string
		for id, name := range to.dims {
			dimName[id] = name
			if code, ok := pins[id]; ok {
				axes, choices = append(axes, id), append(choices, []string{code})
				continue
			}
			if _, shared := from.dims[id]; !shared {
				return nil, refuseCell(http.StatusBadRequest, "pin %s: %s has it and %s has not", name, to.label, from.label), nil
			}
			leaves, err := h.leafCodes(ctx, id)
			if err != nil {
				return nil, nil, err
			}
			axes, choices = append(axes, id), append(choices, leaves)
		}
		combos := [][]string{{}}
		for _, cs := range choices {
			next := make([][]string, 0, len(combos)*len(cs))
			for _, c := range combos {
				for _, code := range cs {
					next = append(next, append(append([]string{}, c...), code))
				}
			}
			combos = next
			if len(combos) > maxCopyCells {
				return nil, refuseCell(http.StatusBadRequest, "more than %d cells to freeze: pin more dimensions", maxCopyCells), nil
			}
		}
		for _, combo := range combos {
			codes := map[string]string{}
			source := map[string]string{}
			for i, id := range axes {
				codes[id] = combo[i]
				if _, ok := from.dims[id]; ok {
					source[id] = combo[i]
				}
			}
			for id, code := range pins {
				if _, ok := from.dims[id]; ok {
					source[id] = code
				}
			}
			cells = append(cells, cell{to: to, codes: codes, source: source})
			froms = append(froms, from)
		}
	}

	// Read every value through the grid's own read, one totals read per
	// source grid and scope.
	type readKey struct{ grid, scope string }
	reads := map[readKey]map[string]any{}
	read := func(from *freezeMetric, scope map[string]string) (map[string]any, error) {
		sb, _ := json.Marshal(scope)
		k := readKey{from.gridID, string(sb)}
		if got, ok := reads[k]; ok {
			return got, nil
		}
		q := url.Values{"revision_id": {revisionID}, "grid_def_id": {from.gridID}, "scope": {string(sb)}, "totals_only": {"1"}}
		for deadline := time.Now().Add(20 * time.Second); ; time.Sleep(250 * time.Millisecond) {
			status, body := h.callAsCaller(ctx, r, http.MethodGet, "/api/grid", q, nil)
			if status != http.StatusOK {
				return nil, routeError(status, body)
			}
			var g map[string]any
			if err := json.Unmarshal(body, &g); err != nil {
				return nil, err
			}
			// Values still being recalculated are not frozen: wait for them.
			if pending, _ := g["recalc_pending"].(bool); pending && time.Now().Before(deadline) {
				continue
			}
			reads[k] = g
			return g, nil
		}
	}
	var out []frozenValue
	var prepared []preparedCell
	infos := map[string]cellMetricInfo{}
	for i, c := range cells {
		from := froms[i]
		g, err := read(from, c.source)
		if err != nil {
			return nil, refuseCell(http.StatusForbidden, "%s cannot be read: %v", from.label, err), nil
		}
		if wh, _ := g["withheld"].([]any); containsAny(wh, from.id) {
			return nil, refuseCell(http.StatusForbidden, "%s is withheld from you at these members (it reads a member hidden from you)", from.label), nil
		}
		totals, _ := g["totals"].(map[string]any)
		v, ok := totals[from.id].(float64)
		if !ok {
			continue // no value there: the target cell is left as it is
		}
		members := map[string]string{}
		for id, code := range c.codes {
			members[dimName[id]] = code
		}
		out = append(out, frozenValue{To: c.to.name, Members: members, Value: v})
		info, known := infos[c.to.id]
		if !known {
			info = cellMetricInfo{format: c.to.format, label: c.to.label, dims: c.to.dims}
			infos[c.to.id] = info
		}
		pc, ref := h.prepareCell(ctx, writer, revisionID, writebackReq{ModelID: modelID, RevisionID: revisionID,
			MetricID: c.to.id, DimCodes: c.codes, Value: v}, info)
		if ref != nil {
			if ref.status >= http.StatusInternalServerError {
				return nil, nil, errors.New(ref.message)
			}
			return out, &cellRefusal{status: ref.status, code: ref.code, message: c.to.label + ": " + ref.message}, nil
		}
		prepared = append(prepared, pc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].To < out[j].To })
	if dryRun || len(prepared) == 0 {
		return out, nil, nil
	}
	if err := h.checkCellLimits(ctx, modelID, prepared); err != nil {
		return nil, refuseCell(http.StatusPaymentRequired, "%v", err), nil
	}
	if err := h.storeCells(ctx, modelID, revisionID, writer.UserID, prepared); err != nil {
		return nil, nil, err
	}
	done := context.WithoutCancel(ctx)
	go func() { //nolint:contextcheck
		calcLog := h.log.With().Str("op", "freeze_values").Logger()
		scheduler := calculation.NewScheduler(calcLog, calculation.NewStore(h.db.For(done)))
		if err := recalcWrittenCells(done, scheduler, modelID, revisionID, prepared); err != nil {
			h.log.Warn().Err(err).Msg("recalc failed after freezing values")
		}
	}()
	return out, nil, nil
}

func containsAny(xs []any, s string) bool {
	for _, x := range xs {
		if str, _ := x.(string); str == s {
			return true
		}
	}
	return false
}

// leafCodes is a dimension's members that hold values (no members under
// them), in their order.
func (h *handler) leafCodes(ctx context.Context, dimID string) ([]string, error) {
	rows, err := h.db.Query(ctx, `
		SELECT m.code FROM model.dimension_member m
		WHERE m.dimension_id = $1::uuid AND COALESCE(m.formula, '') = ''
		  AND NOT EXISTS (SELECT 1 FROM model.dimension_member c WHERE c.parent_member_id = m.id)
		ORDER BY m.time_index NULLS LAST, m.sort_order NULLS LAST, m.code`, dimID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// baFreezeValues handles POST /api/business-admin/freeze-values, body
// {"pairs": [{"from", "to"}], "at": {"<dimension>": "<member code>"},
// "dry_run"}, in the model's live revision, as the caller.
func (h *handler) baFreezeValues(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	_, modelID, err := h.baWorkspaceModel(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve workspace model")
		return
	}
	var body struct {
		freezeSpec
		DryRun bool `json:"dry_run"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
		return
	}
	var revisionID string
	if err := h.db.QueryRow(ctx, `SELECT COALESCE(active_revision_id::text, '') FROM core.model WHERE id = $1::uuid`, modelID).Scan(&revisionID); err != nil || revisionID == "" {
		jsonErr(w, fmt.Errorf("the model has no live revision"), http.StatusBadRequest)
		return
	}
	values, ref, err := h.freezeValues(ctx, r, a, modelID, revisionID, body.freezeSpec, body.DryRun)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if ref != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(ref.status)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": ref.message + "; nothing was frozen", "values": values})
		return
	}
	if !body.DryRun && len(values) > 0 {
		done := context.WithoutCancel(ctx)
		auditlog.Log(done, h.db.For(done), h.log, auditlog.Fields{
			Category: auditlog.CategoryDataChange, EventType: auditlog.EventCellsWritten,
			ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
			ResourceType: "revision", ResourceID: revisionID, RevisionID: revisionID,
			Metadata: map[string]string{"model_id": modelID, "cells": strconv.Itoa(len(values)), "via": "freeze_values"},
		})
	}
	jsonOK(w, map[string]any{"cells": len(values), "dry_run": body.DryRun, "values": values})
}

// approvalFreeze is the freeze an approval step declares (on_approve.freeze),
// its "{key}" pins replaced by the run's context fields; nil when it declares
// none.
func (h *handler) approvalFreeze(ctx context.Context, stepID string, instCtx map[string]string) (*freezeSpec, error) {
	var raw []byte
	if err := h.db.QueryRow(ctx, `
		SELECT COALESCE(step_def.elem->'on_approve'->'freeze', 'null'::jsonb)::text
		FROM workflow.workflow_step ws
		JOIN workflow.workflow_instance wi ON wi.id = ws.instance_id
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		CROSS JOIN LATERAL (
			SELECT elem FROM jsonb_array_elements(COALESCE(wi.steps_snapshot, wd.steps)) AS elem
			WHERE elem->>'id' = ws.step_def_id LIMIT 1
		) step_def
		WHERE ws.id = $1::uuid`, stepID).Scan(&raw); err != nil || string(raw) == "null" {
		return nil, nil //nolint:nilerr // a step without a freeze
	}
	var spec freezeSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return nil, fmt.Errorf("the step's on_approve freeze is not readable: %w", err)
	}
	for dim, code := range spec.At {
		if strings.HasPrefix(code, "{") && strings.HasSuffix(code, "}") {
			key := strings.TrimSpace(code[1 : len(code)-1])
			v := strings.TrimSpace(instCtx[key])
			if v == "" {
				return nil, fmt.Errorf("the step freezes at %s = {%s}, a context field this run has no value for", dim, key)
			}
			spec.At[dim] = v
		}
	}
	return &spec, nil
}

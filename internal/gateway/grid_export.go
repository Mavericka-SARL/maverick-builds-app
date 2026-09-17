package gateway

// GET /api/grid/export — exports a grid's raw input-metric values (the same
// facts /api/grid resolves into `cells`) as CSV or XLSX. Column headers are
// dimension and metric NAMES, one row per unique combination of the grid's
// dimension members — exactly the shape importpkg.ResolveRows expects on the
// way back in via POST /api/import/upload, so an exported file round-trips.
//
// Only is_input metrics are exported: calculated metrics have no raw
// fact_input rows of their own (see runtime.calc_result) and re-importing a
// calculated value would be meaningless.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/writeguard"
)

func (h *handler) gridExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()

	gridDefID := r.URL.Query().Get("grid_def_id")
	if gridDefID == "" {
		jsonErr(w, fmt.Errorf("grid_def_id required"), http.StatusBadRequest)
		return
	}
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	if format == "" {
		format = "csv"
	}
	if format != "csv" && format != "xlsx" {
		jsonErr(w, fmt.Errorf("format must be csv or xlsx"), http.StatusBadRequest)
		return
	}

	var modelID, gridName string
	if err := h.db.QueryRow(ctx,
		`SELECT model_id::text, name FROM model.grid_def WHERE id=$1::uuid`, gridDefID,
	).Scan(&modelID, &gridName); err != nil {
		jsonErr(w, fmt.Errorf("grid not found"), http.StatusNotFound)
		return
	}

	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	canAccessModel, err := h.actorCanAccessModel(ctx, act, modelID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !canAccessModel {
		jsonErr(w, fmt.Errorf("forbidden: model is outside your access scope"), http.StatusForbidden)
		return
	}

	revisionID, _, err := h.resolveRevisionCtx(ctx, r.URL.Query().Get("revision_id"), modelID)
	if rejectForeignRevision(w, err) {
		return
	}
	if err != nil {
		jsonErr(w, fmt.Errorf("no revision: %w", err), http.StatusInternalServerError)
		return
	}

	// ── this grid's own dimensions, ordered (columns) ────────────────────────
	type dimCol struct{ ID, Name string }
	var dimCols []dimCol
	{
		rows, qErr := h.db.Query(ctx, `
			SELECT d.id::text, d.name
			FROM model.grid_dimension gd
			JOIN model.dimension_def d ON d.id = gd.dimension_id
			WHERE gd.grid_id = $1::uuid
			ORDER BY d.name`, gridDefID)
		if qErr != nil {
			jsonErr(w, qErr, http.StatusInternalServerError)
			return
		}
		for rows.Next() {
			var dc dimCol
			if sErr := rows.Scan(&dc.ID, &dc.Name); sErr != nil {
				rows.Close()
				jsonErr(w, sErr, http.StatusInternalServerError)
				return
			}
			dimCols = append(dimCols, dc)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
	}
	if len(dimCols) == 0 {
		jsonErr(w, fmt.Errorf("grid has no dimensions configured"), http.StatusBadRequest)
		return
	}

	// ── this grid's own input metrics, ordered (columns) ─────────────────────
	// Joined via metric name against the requested revision, matching /api/grid's
	// own resolution so an export always reflects the viewer's active revision.
	type metricCol struct{ ID, Name string }
	var metricCols []metricCol
	{
		rows, qErr := h.db.Query(ctx, `
			SELECT rev.id::text, rev.name
			FROM model.grid_metric gm
			JOIN model.metric_def orig ON orig.id = gm.metric_id
			JOIN model.metric_def rev
			     ON rev.model_id = orig.model_id
			    AND rev.name     = orig.name
			    AND rev.revision_id = $2::uuid
			WHERE gm.grid_id = $1::uuid AND rev.is_input
			ORDER BY gm.sort_order, rev.name`, gridDefID, revisionID)
		if qErr != nil {
			jsonErr(w, qErr, http.StatusInternalServerError)
			return
		}
		for rows.Next() {
			var mc metricCol
			if sErr := rows.Scan(&mc.ID, &mc.Name); sErr != nil {
				rows.Close()
				jsonErr(w, sErr, http.StatusInternalServerError)
				return
			}
			metricCols = append(metricCols, mc)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
	}
	if len(metricCols) == 0 {
		jsonErr(w, fmt.Errorf("grid has no input metrics to export"), http.StatusBadRequest)
		return
	}

	// ── hidden-member / hidden-metric access-rule filter, mirroring /api/grid ─
	hiddenByDim, metricRules, err := h.hiddenMemberFilter(ctx, act, modelID, revisionID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if len(metricRules) > 0 {
		kept := make([]metricCol, 0, len(metricCols))
		for _, mc := range metricCols {
			if metricRules[mc.ID] != "hidden" {
				kept = append(kept, mc)
			}
		}
		metricCols = kept
	}
	if len(metricCols) == 0 {
		jsonErr(w, fmt.Errorf("grid has no input metrics to export"), http.StatusBadRequest)
		return
	}

	metricIDs := make([]string, len(metricCols))
	metricName := make(map[string]string, len(metricCols))
	for i, mc := range metricCols {
		metricIDs[i] = mc.ID
		metricName[mc.ID] = mc.Name
	}

	factRows, err := h.db.Query(ctx, `
		SELECT metric_id::text, dim_members::text, value::float8
		FROM (
			SELECT DISTINCT ON (metric_id, dim_members) metric_id, dim_members, value
			FROM runtime.fact_input
			WHERE model_id=$1::uuid AND revision_id=$2::uuid AND source_ref IS NULL
			  AND metric_id = ANY($3::uuid[])
			ORDER BY metric_id, dim_members, entered_at DESC, id DESC
		) fi`, modelID, revisionID, metricIDs)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer factRows.Close()

	type rowKey = string
	rowDims := map[rowKey][]string{}
	rowValues := map[rowKey]map[string]float64{}
	var keyOrder []rowKey

	for factRows.Next() {
		var metricID, dmJSON string
		var val float64
		if sErr := factRows.Scan(&metricID, &dmJSON, &val); sErr != nil {
			jsonErr(w, sErr, http.StatusInternalServerError)
			return
		}
		var dm map[string]string
		if jErr := json.Unmarshal([]byte(dmJSON), &dm); jErr != nil {
			continue
		}
		if factRowHidden(dm, hiddenByDim) {
			continue
		}
		codes := make([]string, len(dimCols))
		ok := true
		for i, dc := range dimCols {
			code, exists := dm[dc.ID]
			if !exists {
				ok = false
				break
			}
			codes[i] = code
		}
		if !ok {
			continue // fact doesn't carry all of this grid's dims — not one of this grid's cells
		}
		key := strings.Join(codes, "\x1f")
		if _, seen := rowValues[key]; !seen {
			rowDims[key] = codes
			rowValues[key] = map[string]float64{}
			keyOrder = append(keyOrder, key)
		}
		rowValues[key][metricID] = val
	}
	if err := factRows.Err(); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	sort.Strings(keyOrder)

	header := make([]string, 0, len(dimCols)+len(metricCols))
	for _, dc := range dimCols {
		header = append(header, dc.Name)
	}
	for _, mc := range metricCols {
		header = append(header, mc.Name)
	}

	records := make([][]string, 0, len(keyOrder))
	for _, key := range keyOrder {
		rec := make([]string, 0, len(header))
		rec = append(rec, rowDims[key]...)
		vals := rowValues[key]
		for _, mc := range metricCols {
			if v, ok := vals[mc.ID]; ok {
				rec = append(rec, strconv.FormatFloat(v, 'f', -1, 64))
			} else {
				rec = append(rec, "")
			}
		}
		records = append(records, rec)
	}

	if err := writeTabularResponse(w, format, slugify(gridName), header, records); err != nil {
		h.log.Warn().Err(err).Msg("grid export: write response failed")
	}
}

// hiddenMemberFilter resolves the requesting actor's user_access_rule rows
// into a dimensionID -> hidden-code-set map (cascading a hidden
// dimension_member rule down the hierarchy exactly like /api/grid does,
// via writeguard.ExpandHidden) plus a metricID -> access map for
// rule_type='metric' rows — so an export (or any other caller) can never
// surface a fact, or a metric's own column, /api/grid itself would hide.
func (h *handler) hiddenMemberFilter(ctx context.Context, act *actor, modelID, revisionID string) (hiddenByDim map[string]map[string]bool, metricRules map[string]string, err error) {
	dimRules := map[string]string{}
	metricRules = map[string]string{}
	arRows, err := h.db.Query(ctx,
		`SELECT rule_type, ref_id, access FROM identity.user_access_rule WHERE user_id=$1::uuid`, act.UserID)
	if err != nil {
		return nil, nil, err
	}
	for arRows.Next() {
		var ruleType, refID, access string
		if arRows.Scan(&ruleType, &refID, &access) == nil {
			switch ruleType {
			case "dimension_member":
				dimRules[refID] = access
			case "metric":
				metricRules[refID] = access
			}
		}
	}
	arRows.Close()
	if len(dimRules) == 0 {
		return nil, metricRules, nil
	}

	allDimRows, err := h.db.Query(ctx, `
		SELECT d.id::text, m.id::text, m.code, COALESCE(m.parent_member_id::text, '')
		FROM model.dimension_def d
		JOIN model.dimension_member m ON m.dimension_id = d.id
		WHERE d.model_id = $1::uuid AND (d.revision_id = $2::uuid OR d.revision_id IS NULL)`, modelID, revisionID)
	if err != nil {
		return nil, nil, err
	}
	defer allDimRows.Close()

	type memberRow struct{ dimID, id, code, parentID string }
	var members []memberRow
	edges := make([]writeguard.MemberEdge, 0, 256)
	for allDimRows.Next() {
		var mr memberRow
		if sErr := allDimRows.Scan(&mr.dimID, &mr.id, &mr.code, &mr.parentID); sErr != nil {
			return nil, nil, sErr
		}
		members = append(members, mr)
		edges = append(edges, writeguard.MemberEdge{ID: mr.id, ParentID: mr.parentID, DimID: mr.dimID})
	}
	if err := allDimRows.Err(); err != nil {
		return nil, nil, err
	}

	for id := range writeguard.ExpandHidden(edges, dimRules) {
		dimRules[id] = "hidden"
	}

	hidden := map[string]map[string]bool{}
	for _, mr := range members {
		if dimRules[mr.id] != "hidden" {
			continue
		}
		if hidden[mr.dimID] == nil {
			hidden[mr.dimID] = map[string]bool{}
		}
		hidden[mr.dimID][mr.code] = true
	}
	return hidden, metricRules, nil
}

package gateway

import (
	"cmp"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// sampleMaxCells bounds the cells one fill writes.
const sampleMaxCells = 500_000

// fillSampleValues serves POST /api/developer/grids/{id}/sample-values: the
// grid's input metrics filled with sample values across its leaf
// combinations (importpkg.GenerateSample), committed as a replace import. A
// dry run answers how many cells it would fill and the first of them, or —
// with "format": "csv" — every row as a CSV the import wizard reads.
func (h *handler) fillSampleValues(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !h.requireResourceAccess(w, r, "grid", id) {
		return
	}
	ctx := r.Context()
	extendImportDeadlines(w)
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}
	var body struct {
		Metrics   map[string]importpkg.SampleSpec `json:"metrics"`
		Members   json.RawMessage                 `json:"members"`
		OnlyEmpty bool                            `json:"only_empty"`
		Seed      uint64                          `json:"seed"`
		DryRun    bool                            `json:"dry_run"`
		Format    string                          `json:"format"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, fmt.Errorf("invalid JSON body"), http.StatusBadRequest)
		return
	}
	var modelID, revisionID string
	if err := h.db.QueryRow(ctx, `SELECT model_id::text, COALESCE(revision_id::text, '') FROM model.grid_def WHERE id = $1::uuid`, id).Scan(&modelID, &revisionID); err != nil {
		jsonErr(w, fmt.Errorf("grid not found"), http.StatusNotFound)
		return
	}
	if revisionID == "" {
		revisionID = h.activeRevisionOf(r, modelID)
	}
	type dimRow struct{ id, name string }
	var dims []dimRow
	dRows, err := h.db.Query(ctx, `
		SELECT d.id::text, d.name FROM model.grid_dimension gd JOIN model.dimension_def d ON d.id = gd.dimension_id
		WHERE gd.grid_id = $1::uuid ORDER BY d.name`, id)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	for dRows.Next() {
		var d dimRow
		if err := dRows.Scan(&d.id, &d.name); err != nil {
			dRows.Close()
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		dims = append(dims, d)
	}
	dRows.Close()
	// The grid's numeric input metrics, by id and name; the request's
	// metrics name them by either.
	type metricRef struct{ id, name string }
	var inputs []metricRef
	mRows, err := h.db.Query(ctx, `
		SELECT m.id::text, m.name FROM model.grid_metric gm JOIN model.metric_def m ON m.id = gm.metric_id
		WHERE gm.grid_id = $1::uuid AND m.is_input AND m.format <> 'text' ORDER BY gm.sort_order`, id)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	for mRows.Next() {
		var m metricRef
		if err := mRows.Scan(&m.id, &m.name); err != nil {
			mRows.Close()
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		inputs = append(inputs, m)
	}
	mRows.Close()
	specs := map[string]importpkg.SampleSpec{}
	names := map[string]string{}
	for _, m := range inputs {
		names[m.id] = m.name
	}
	if len(body.Metrics) == 0 {
		for _, m := range inputs {
			specs[m.id] = importpkg.SampleSpec{Min: 0, Max: 1000}
		}
	}
	for ref, spec := range body.Metrics {
		found := ""
		for _, m := range inputs {
			if m.id == ref || strings.EqualFold(m.name, ref) {
				found = m.id
			}
		}
		if found == "" {
			jsonErr(w, fmt.Errorf("%q is not a numeric input metric of this grid", ref), http.StatusBadRequest)
			return
		}
		specs[found] = spec
	}
	members, err := importpkg.ParseMembers(body.Members)
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	dimIDs := make([]string, len(dims))
	for i, d := range dims {
		dimIDs[i] = d.id
	}
	res, err := importpkg.GenerateSample(ctx, h.db.For(ctx), importpkg.SampleRequest{
		ModelID: modelID, RevisionID: revisionID, UserID: act.UserID, DimensionIDs: dimIDs,
		Metrics: specs, Members: members, OnlyEmpty: body.OnlyEmpty, Seed: body.Seed, MaxCells: sampleMaxCells,
	})
	switch {
	case errors.Is(err, importpkg.ErrWriteDenied):
		jsonErr(w, err, http.StatusForbidden)
		return
	case errors.Is(err, importpkg.ErrNothingToClear):
		jsonErr(w, fmt.Errorf("this grid has no numeric input metric to fill"), http.StatusBadRequest)
		return
	case err != nil && strings.Contains(err.Error(), "Internal"):
		jsonErr(w, err, http.StatusInternalServerError)
		return
	case err != nil:
		jsonErr(w, err, http.StatusBadRequest)
		return
	}

	// Rows as people read them: each dimension's code, the metric's name and
	// the value (a pick-list's option code, not its stored key).
	asRecord := func(row importpkg.StagingRow, picklists map[string]map[float64]string) []string {
		rec := make([]string, 0, len(dims)+2)
		for _, d := range dims {
			rec = append(rec, row.DimMembers[d.id])
		}
		v := strconv.FormatFloat(row.Value, 'f', -1, 64)
		if code, ok := picklists[row.MetricID][row.Value]; ok {
			v = code
		}
		return append(rec, names[row.MetricID], v)
	}
	picklists := h.picklistCodes(r, specs)
	if body.DryRun {
		if body.Format == "csv" {
			w.Header().Set("Content-Type", "text/csv; charset=utf-8")
			w.Header().Set("Content-Disposition", `attachment; filename="sample-values.csv"`)
			cw := csv.NewWriter(w)
			header := make([]string, 0, len(dims)+2)
			for _, d := range dims {
				header = append(header, d.name)
			}
			_ = cw.Write(append(header, "metric", "value"))
			for _, row := range res.Rows {
				_ = cw.Write(asRecord(row, picklists))
			}
			cw.Flush()
			return
		}
		sample := make([][]string, 0, 10)
		for _, row := range res.Rows[:min(10, len(res.Rows))] {
			sample = append(sample, asRecord(row, picklists))
		}
		header := make([]string, 0, len(dims)+2)
		for _, d := range dims {
			header = append(header, d.name)
		}
		jsonOK(w, map[string]any{"cells": len(res.Rows), "withheld": res.Withheld, "dry_run": true,
			"columns": append(header, "metric", "value"), "sample": sample})
		return
	}
	if len(res.Rows) == 0 {
		jsonOK(w, map[string]any{"cells": 0, "withheld": res.Withheld})
		return
	}
	if cid := h.customerOfModel(ctx, modelID); cid != "" && h.plans != nil {
		if err := cmp.Or(h.plans.CheckFactRows(ctx, h.db.For(ctx), cid, modelID, len(res.Rows)), h.plans.CheckStorage(ctx, h.db.For(ctx), cid)); err != nil {
			h.jsonLimitErr(w, err)
			return
		}
	}
	store := importpkg.NewStore(h.db.For(ctx))
	job, err := store.CreateImportJob(ctx, modelID, revisionID, "sample", act.UserID, nil)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if err := store.StageRows(ctx, job.Id, res.Rows, nil); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	metricIDs, err := store.CommitImport(ctx, job.Id, modelID, revisionID, act.UserID, importpkg.ModeReplace)
	if errors.Is(err, importpkg.ErrWriteDenied) {
		jsonErr(w, err, http.StatusForbidden)
		return
	} else if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	h.recalcAfterImport(ctx, modelID, revisionID, "sample fill", metricIDs)
	var appID string
	_ = h.db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID)
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryDataChange, EventType: auditlog.EventValuesGenerated,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ApplicationID: appID, ResourceType: "grid", ResourceID: id, RevisionID: revisionID,
		Metadata: map[string]string{"cells": strconv.Itoa(len(res.Rows)), "metrics": strconv.Itoa(len(specs)), "only_empty": strconv.FormatBool(body.OnlyEmpty)},
	})
	jsonOK(w, map[string]any{"cells": len(res.Rows), "withheld": res.Withheld, "job_id": job.Id})
}

// picklistCodes maps each pick-list metric among specs from its stored keys
// to its option codes, to show a drawn option as people read it.
func (h *handler) picklistCodes(r *http.Request, specs map[string]importpkg.SampleSpec) map[string]map[float64]string {
	out := map[string]map[float64]string{}
	for id := range specs {
		rows, err := h.db.Query(r.Context(), `
			SELECT o.code FROM model.metric_def m JOIN model.dimension_member o ON o.dimension_id = m.picklist_dimension_id
			WHERE m.id::text = $1`, id)
		if err != nil {
			continue
		}
		for rows.Next() {
			var code string
			if rows.Scan(&code) == nil {
				if out[id] == nil {
					out[id] = map[float64]string{}
				}
				out[id][formula.PicklistKey(code)] = code
			}
		}
		rows.Close()
	}
	return out
}

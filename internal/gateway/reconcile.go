package gateway

import (
	"context"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/internal/reconcile"
)

// Integrations › Workbook reconciliation (internal/reconcile): a revision keeps the workbook
// it rebuilds, the map of its cells in model terms and the cases to
// compare; a run compares them in the background and keeps the report.

// reconcileRevision resolves the request's revision of the developer's
// model; false after answering.
func (h *handler) reconcileRevision(w http.ResponseWriter, r *http.Request, revisionID string) (modelID, revID string, ok bool) {
	ctx := r.Context()
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return "", "", false
	}
	revID, _, err = h.resolveRevisionCtx(ctx, revisionID, modelID)
	if err != nil {
		jsonErr(w, fmt.Errorf("revision not found"), http.StatusNotFound)
		return "", "", false
	}
	if !h.requireRevisionInModel(w, r, revID, modelID) {
		return "", "", false
	}
	return modelID, revID, true
}

// reconciliation serves GET and DELETE /api/developer/reconciliation.
func (h *handler) reconciliation(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, revID, ok := h.reconcileRevision(w, r, r.URL.Query().Get("revision_id"))
	if !ok {
		return
	}
	db := h.db.For(ctx)
	if r.Method == http.MethodDelete {
		if err := reconcile.Delete(ctx, db, revID); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		jsonOK(w, map[string]any{"deleted": true})
		return
	}
	saved, err := reconcile.Load(ctx, db, revID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if saved == nil {
		saved = &reconcile.Saved{RevisionID: revID, Status: "idle", Map: reconcile.Map{Rules: []reconcile.Rule{}}, Cases: []reconcile.Case{}}
	}
	if saved.WorkbookSize > 0 {
		if data, _, err := reconcile.Workbook(ctx, db, revID); err == nil {
			saved.Sheets, _ = reconcile.SheetNames(data)
		}
	}
	// The differences are many: the page reads the first ones, the CSV all.
	if saved.Report != nil && len(saved.Report.Differences) > 500 {
		saved.Report.Differences = saved.Report.Differences[:500]
	}
	jsonOK(w, map[string]any{"reconciliation": saved, "recalculation": h.sofficePath() != ""})
}

// reconciliationWorkbook serves PUT /api/developer/reconciliation/workbook
// {revision_id, file_name, data_base64}.
func (h *handler) reconciliationWorkbook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		RevisionID string `json:"revision_id"`
		FileName   string `json:"file_name"`
		DataBase64 string `json:"data_base64"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, reconcile.MaxWorkbookBytes*4/3+1<<16)
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, fmt.Errorf("invalid body (a workbook is at most %d MB): %w", reconcile.MaxWorkbookBytes>>20, err), http.StatusBadRequest)
		return
	}
	modelID, revID, ok := h.reconcileRevision(w, r, body.RevisionID)
	if !ok {
		return
	}
	// A workbook is stored data: refused, as a load is, once the workspace
	// is at its storage limit.
	if cid := h.customerOfModel(ctx, modelID); cid != "" && h.plans != nil {
		if err := h.plans.CheckStorage(ctx, h.db.For(ctx), cid); err != nil {
			h.jsonLimitErr(w, err)
			return
		}
	}
	data, err := base64.StdEncoding.DecodeString(body.DataBase64)
	if err != nil || len(data) == 0 {
		jsonErr(w, errors.New("data_base64 is not a file"), http.StatusBadRequest)
		return
	}
	if len(data) > reconcile.MaxWorkbookBytes {
		jsonErr(w, fmt.Errorf("the workbook is larger than %d MB", reconcile.MaxWorkbookBytes>>20), http.StatusBadRequest)
		return
	}
	sheets, err := reconcile.SheetNames(data)
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	name := strings.TrimSpace(body.FileName)
	if name == "" {
		name = "workbook.xlsx"
	}
	if err := reconcile.SaveWorkbook(ctx, h.db.For(ctx), revID, name, data, a.UserID); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"file_name": name, "sheets": sheets, "workbook_size": len(data)})
}

// reconciliationMap serves PUT /api/developer/reconciliation/map
// {revision_id, map, cases}.
func (h *handler) reconciliationMap(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		RevisionID string           `json:"revision_id"`
		Map        reconcile.Map    `json:"map"`
		Cases      []reconcile.Case `json:"cases"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, fmt.Errorf("invalid body: %w", err), http.StatusBadRequest)
		return
	}
	_, revID, ok := h.reconcileRevision(w, r, body.RevisionID)
	if !ok {
		return
	}
	if err := body.Map.Validate(); err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	if err := reconcile.ValidateCases(body.Cases); err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	if err := reconcile.SaveMap(ctx, h.db.For(ctx), revID, body.Map, body.Cases); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]any{"saved": true})
}

// reconciliationPropose serves POST /api/developer/reconciliation/propose
// {revision_id}: a map drafted from the stored workbook, not saved.
func (h *handler) reconciliationPropose(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		RevisionID string `json:"revision_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	modelID, revID, ok := h.reconcileRevision(w, r, body.RevisionID)
	if !ok {
		return
	}
	data, _, err := reconcile.Workbook(ctx, h.db.For(ctx), revID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if len(data) == 0 {
		jsonErr(w, errors.New("upload the workbook first"), http.StatusBadRequest)
		return
	}
	m, notes, err := reconcile.Propose(ctx, h.db.For(ctx), modelID, revID, data)
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	if notes == nil {
		notes = []string{}
	}
	jsonOK(w, map[string]any{"map": m, "notes": notes})
}

// testSoffice stands in for RECONCILE_SOFFICE in tests.
var testSoffice string

// sofficePath is the LibreOffice program cases are recalculated with.
func (h *handler) sofficePath() string {
	if h.soffice != "" {
		return h.soffice
	}
	return testSoffice
}

// ReconcileRunTimeout bounds a run.
var ReconcileRunTimeout = 20 * time.Minute

// reconciliationRun serves POST /api/developer/reconciliation/run
// {revision_id}: the run starts in the background (202); GET
// /api/developer/reconciliation follows it.
func (h *handler) reconciliationRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var body struct {
		RevisionID string `json:"revision_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	modelID, revID, ok := h.reconcileRevision(w, r, body.RevisionID)
	if !ok {
		return
	}
	db := h.db.For(ctx)
	saved, err := reconcile.Load(ctx, db, revID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if saved == nil || saved.WorkbookSize == 0 {
		jsonErr(w, errors.New("upload the workbook first"), http.StatusBadRequest)
		return
	}
	if err := saved.Map.Validate(); err != nil {
		jsonErr(w, fmt.Errorf("the map: %w", err), http.StatusBadRequest)
		return
	}
	if err := reconcile.Begin(ctx, db, revID); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, reconcile.ErrRunning) {
			code = http.StatusConflict
		}
		jsonErr(w, err, code)
		return
	}
	go func() {
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), ReconcileRunTimeout)
		defer cancel()
		rep, err := h.runReconciliation(bg, modelID, revID, saved)
		if ferr := reconcile.Finish(bg, db, revID, rep, err); ferr != nil {
			h.log.Error().Err(ferr).Str("revision_id", revID).Msg("reconciliation: keep the report")
		}
	}()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "running"})
}

// runReconciliation runs a saved reconciliation now.
func (h *handler) runReconciliation(ctx context.Context, modelID, revID string, saved *reconcile.Saved) (*reconcile.Report, error) {
	db := h.db.For(ctx)
	data, _, err := reconcile.Workbook(ctx, db, revID)
	if err != nil {
		return nil, err
	}
	return reconcile.Run(ctx, db, modelID, revID, data, saved.Map, saved.Cases, reconcile.Options{Soffice: h.sofficePath()})
}

// reconciliationCSV serves GET /api/developer/reconciliation/differences.csv:
// every difference of the last run the report keeps.
func (h *handler) reconciliationCSV(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	_, revID, ok := h.reconcileRevision(w, r, r.URL.Query().Get("revision_id"))
	if !ok {
		return
	}
	saved, err := reconcile.Load(ctx, h.db.For(ctx), revID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if saved == nil || saved.Report == nil {
		jsonErr(w, errors.New("no reconciliation has run for this revision"), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="reconciliation-differences.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"case", "sheet", "cell", "metric", "members", "expected", "actual"})
	for _, d := range saved.Report.Differences {
		keys := make([]string, 0, len(d.Members))
		for k := range d.Members {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, k+"="+d.Members[k])
		}
		_ = cw.Write([]string{d.Case, d.Sheet, d.Cell, d.Metric, strings.Join(parts, "; "), d.Expected, d.Actual})
	}
	cw.Flush()
}

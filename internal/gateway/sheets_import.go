package gateway

// Google Sheets import endpoints. Two entry points share
// internal/importpkg's sheet fetcher:
//
//   - POST /api/import/sheets/fetch returns a link-shared sheet's contents
//     as CSV text. The Import wizard calls it and then proceeds exactly as
//     if the user had uploaded that CSV — same client-side mapping, same
//     /api/import/upload commit, same writeguard checks. The server has to
//     do the fetching because Google's export endpoint sends no CORS
//     headers, so the browser cannot; and because the export URL is
//     reconstructed from the parsed spreadsheet ID (see importpkg's package
//     comment), the endpoint can only ever request docs.google.com exports,
//     never an arbitrary caller-supplied URL.
//
//   - Saved integrations with type "google_sheets" (config: sheet_url +
//     optional column_map/import_mode) re-fetch the sheet at run time; see
//     integrationRun and runSheetsGridImport below.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
)

// sheetFetcher returns the injected test fetcher or a real one. h.sheets is
// never set outside tests (NewHandler leaves it nil), mirroring testProvider.
func (h *handler) sheetFetcher() *importpkg.SheetFetcher {
	if h.sheets != nil {
		return h.sheets
	}
	return &importpkg.SheetFetcher{}
}

// jsonSheetErr maps a fetch failure to the right status: problems the user
// can fix (not link-shared, bad URL) are 400s carrying the fix-it message;
// anything else is Google or the network misbehaving, a 502.
func jsonSheetErr(w http.ResponseWriter, err error) {
	if errors.Is(err, importpkg.ErrSheetNotAccessible) {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	jsonErr(w, fmt.Errorf("fetch sheet: %w", err), http.StatusBadGateway)
}

// importSheetFetch handles POST /api/import/sheets/fetch. Read-only: it
// commits nothing, so it carries no audit event; the subsequent
// /api/import/upload (or integration run) is the audited mutation.
func (h *handler) importSheetFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	// Same app-scope resolution as every other /api/import route — an actor
	// with no app access shouldn't be able to use the gateway as a fetch
	// proxy either.
	if _, err := h.resolveDemoModelID(ctx, r); err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}

	var body struct {
		SheetURL string `json:"sheet_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.SheetURL) == "" {
		jsonErr(w, fmt.Errorf("sheet_url field required"), http.StatusBadRequest)
		return
	}
	id, gid, err := importpkg.ParseSheetURL(body.SheetURL)
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	// Through the tenant's own Google service account when it has one —
	// private sheets — else the link-shared export.
	data, err := h.fetchSheetCSV(ctx, h.requestCustomerID(ctx, r, act), id, gid)
	if err != nil {
		jsonSheetErr(w, err)
		return
	}
	jsonOK(w, map[string]any{"csv": string(data), "spreadsheet_id": id, "gid": gid})
}

// applySheetColumnMap renames source headers to the logical names a saved
// integration's column_map recorded ("Q3 Actuals" → "FixtureRevenue"),
// rekeying each row's cells to match, so ResolveRows classifies the renamed
// columns exactly as it would a hand-authored file.
func applySheetColumnMap(header []string, rows []importpkg.RawRow, columnMap map[string]string) {
	if len(columnMap) == 0 {
		return
	}
	renames := map[string]string{}
	for i, col := range header {
		if mapped, ok := columnMap[strings.TrimSpace(col)]; ok && mapped != "" && mapped != col {
			renames[col] = mapped
			header[i] = mapped
		}
	}
	for _, row := range rows {
		for from, to := range renames {
			if v, ok := row.Cells[from]; ok {
				row.Cells[to] = v
				delete(row.Cells, from)
			}
		}
	}
}

// runSheetsGridImport executes a google_sheets integration against a grid
// target. Unlike the legacy CSV grid path (which stages raw metric_id cell
// values and so requires UUIDs), the fetched sheet goes through
// importpkg.ResolveRows — the same name-based classification, leaf-member
// and non-negative validation as /api/import/upload — because a sheet a
// business team maintains holds metric and member names, never UUIDs.
// The response keeps integrationRun's lenient contract: valid rows commit,
// invalid rows are counted and detailed.
func (h *handler) runSheetsGridImport(w http.ResponseWriter, r *http.Request, act *actor, columnMap map[string]string, importMode, csvText string) {
	ctx := r.Context()
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	var revisionID string
	_ = h.db.QueryRow(ctx, `
		SELECT COALESCE(active_revision_id::text, (SELECT id::text FROM model.revision WHERE model_id = m.id ORDER BY created_at LIMIT 1), '')
		FROM core.model m WHERE m.id = $1::uuid
	`, modelID).Scan(&revisionID)

	header, rawRows, err := importpkg.ParseCSVRows([]byte(csvText))
	if err != nil {
		jsonErr(w, fmt.Errorf("parse sheet: %w", err), http.StatusBadRequest)
		return
	}
	applySheetColumnMap(header, rawRows, columnMap)

	staged, importErrs, err := importpkg.ResolveRows(ctx, h.db.For(ctx), modelID, revisionID, header, rawRows)
	if err != nil {
		jsonErr(w, fmt.Errorf("resolve sheet rows: %w", err), http.StatusBadRequest)
		return
	}
	errRows := make([]map[string]any, 0, len(importErrs))
	for _, e := range importErrs {
		errRows = append(errRows, map[string]any{
			"row": e.RowNumber, "column": e.Column, "code": e.ErrorCode,
			"message": e.Message, "raw_value": e.RawValue,
		})
	}
	if len(staged) == 0 {
		jsonOK(w, map[string]any{"rows_imported": 0, "error_rows": len(importErrs), "errors": errRows})
		return
	}

	store := importpkg.NewStore(h.db.For(ctx))
	job, err := store.CreateImportJob(ctx, modelID, revisionID, "google_sheets", act.UserID, nil)
	if err != nil {
		jsonErr(w, fmt.Errorf("create job: %w", err), http.StatusInternalServerError)
		return
	}
	if err := store.StageRows(ctx, job.Id, staged, nil); err != nil {
		jsonErr(w, fmt.Errorf("stage: %w", err), http.StatusInternalServerError)
		return
	}
	// A sheets integration is re-run against the same living sheet, so the
	// default mode must be idempotent: "replace" upserts the cells the sheet
	// lists. Defaulting to CommitImport's own "incremental" would re-add the
	// sheet's values to the totals on every sync. A stored config choice
	// still wins.
	mode := importpkg.ImportMode(importMode)
	if mode == "" {
		mode = importpkg.ModeReplace
	}
	metricIDs, err := store.CommitImport(ctx, job.Id, modelID, revisionID, act.UserID, mode)
	if err != nil {
		if errors.Is(err, importpkg.ErrWriteDenied) {
			jsonErr(w, err, http.StatusForbidden)
			return
		}
		jsonErr(w, fmt.Errorf("commit: %w", err), http.StatusInternalServerError)
		return
	}
	if len(metricIDs) > 0 {
		calcStore := calculation.NewStore(h.db.For(ctx))
		sched := calculation.NewScheduler(h.log, calcStore, nil)
		go func() { _ = sched.RecalcAffected(context.Background(), modelID, revisionID, metricIDs) }() //nolint:contextcheck
	}
	jsonOK(w, map[string]any{"rows_imported": len(staged), "error_rows": len(importErrs), "errors": errRows})
}

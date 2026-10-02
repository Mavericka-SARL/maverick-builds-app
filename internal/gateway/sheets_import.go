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
//     integrationRun, and integration_file_run.go for the grid run every
//     file integration shares.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

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

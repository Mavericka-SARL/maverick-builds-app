package gateway

// The workbooks the console used to read and write in the browser with
// SheetJS, which Dependabot and npm audit cannot see (it is installed from
// SheetJS's own CDN): the Import Wizard parsed the workbook a developer
// chose, and the business Import widget wrote its template. Both are the
// gateway's now, with excelize, which the import itself already reads with.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/xuri/excelize/v2"

	"github.com/mavericks-engine/mavericks/internal/importpkg"
)

// maxWorkbookBody bounds a workbook sent as base64, as the form import does.
const maxWorkbookBody = 16 << 20

// importParseWorkbook serves POST /api/import/parse-workbook: a workbook's
// sheet names and one sheet's header and rows, read as the upload reads
// them (importpkg.ReadXLSXSheet), for the Import Wizard to preview and map.
// Nothing is stored.
func (h *handler) importParseWorkbook(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxWorkbookBody)
	if _, err := h.resolveActor(r.Context(), r); err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	var body struct {
		XLSXBase64 string `json:"xlsx_base64"`
		Sheet      string `json:"sheet"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.XLSXBase64 == "" {
		jsonErr(w, fmt.Errorf("xlsx_base64 is required"), http.StatusBadRequest)
		return
	}
	raw, err := base64.StdEncoding.DecodeString(body.XLSXBase64)
	if err != nil {
		jsonErr(w, fmt.Errorf("xlsx_base64: %w", err), http.StatusBadRequest)
		return
	}
	sheets, name, grid, err := importpkg.ReadXLSXSheet(raw, body.Sheet)
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	var headers []string
	rows := [][]string{}
	if len(grid) > 0 {
		for _, c := range grid[0] {
			headers = append(headers, strings.TrimSpace(c))
		}
		for _, row := range grid[1:] {
			cells := make([]string, len(row))
			for i, c := range row {
				cells[i] = strings.TrimSpace(c)
			}
			rows = append(rows, cells)
		}
	}
	if headers == nil {
		headers = []string{}
	}
	jsonOK(w, map[string]any{"sheets": sheets, "sheet": name, "headers": headers, "rows": rows})
}

// maxTemplateRows bounds a template: a header and a few example rows.
const maxTemplateRows = 100

// importTemplateWorkbook serves POST /api/import/template-workbook: the
// rows given, as a one-sheet .xlsx to download. The business Import widget
// sends its grid's header and an example row, built from the grid the
// caller sees; the gateway only writes them.
func (h *handler) importTemplateWorkbook(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if _, err := h.resolveActor(r.Context(), r); err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	var body struct {
		Sheet    string  `json:"sheet"`
		Filename string  `json:"filename"`
		Rows     [][]any `json:"rows"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Rows) == 0 {
		jsonErr(w, fmt.Errorf("rows are required: the header row first"), http.StatusBadRequest)
		return
	}
	if len(body.Rows) > maxTemplateRows {
		jsonErr(w, fmt.Errorf("a template has at most %d rows", maxTemplateRows), http.StatusBadRequest)
		return
	}
	sheet := strings.TrimSpace(body.Sheet)
	if sheet == "" {
		sheet = "Import"
	}
	f := excelize.NewFile()
	defer func() { _ = f.Close() }()
	if err := f.SetSheetName(f.GetSheetName(0), sheet); err != nil {
		jsonErr(w, fmt.Errorf("sheet name: %w", err), http.StatusBadRequest)
		return
	}
	for i, row := range body.Rows {
		cell, _ := excelize.CoordinatesToCellName(1, i+1)
		values := row
		if err := f.SetSheetRow(sheet, cell, &values); err != nil {
			jsonErr(w, fmt.Errorf("row %d: %w", i+1, err), http.StatusBadRequest)
			return
		}
	}
	var buf bytes.Buffer
	if err := f.Write(&buf); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	name := safeDownloadName(body.Filename, "import-template.xlsx")
	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, name))
	_, _ = w.Write(buf.Bytes())
}

// safeDownloadName is name reduced to a plain .xlsx file name, or fallback.
func safeDownloadName(name, fallback string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.', r == ' ':
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return fallback
	}
	if !strings.HasSuffix(strings.ToLower(out), ".xlsx") {
		out += ".xlsx"
	}
	return out
}

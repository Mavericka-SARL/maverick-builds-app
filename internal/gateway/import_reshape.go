package gateway

// POST /api/import/reshape-preview: the Import Wizard's Shape step. The
// developer builds a reshape (importpkg.Reshape) on a sample file and sees
// it applied by the same code every run of a saved integration applies —
// so what the wizard shows is what an import reads, and a developer sets a
// reshape up without the AI Developer. Nothing is stored.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/mavericks-engine/mavericks/internal/importpkg"
)

// reshapePreviewRaw is how many of the sheet's rows, as read, the preview
// returns for locating the header.
const reshapePreviewRaw = 12

func (h *handler) importReshapePreview(w http.ResponseWriter, r *http.Request) {
	var body struct {
		CSV        string             `json:"csv"`
		XLSXBase64 string             `json:"xlsx_base64"`
		Sheet      string             `json:"sheet"`
		Reshape    *importpkg.Reshape `json:"reshape"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRunBodyBytes)).Decode(&body); err != nil {
		jsonErr(w, fmt.Errorf("a preview needs the file: \"csv\" text or \"xlsx_base64\" (%v)", err), http.StatusBadRequest)
		return
	}
	filename, data := "file.csv", []byte(body.CSV)
	var sheets []string
	switch {
	case body.XLSXBase64 != "":
		raw, err := base64.StdEncoding.DecodeString(body.XLSXBase64)
		if err != nil {
			jsonErr(w, fmt.Errorf("xlsx_base64: %w", err), http.StatusBadRequest)
			return
		}
		filename, data = "file.xlsx", raw
		if sheets, err = importpkg.XLSXSheetNames(raw); err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
	case body.CSV == "":
		jsonErr(w, fmt.Errorf("csv or xlsx_base64 field required"), http.StatusBadRequest)
		return
	}
	if err := body.Reshape.Validate(); err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	delim, err := body.Reshape.Comma()
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	grid, err := importpkg.ReadGrid(filename, data, body.Sheet, delim)
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	raw := grid
	if len(raw) > reshapePreviewRaw {
		raw = raw[:reshapePreviewRaw]
	}
	header, rows, err := importpkg.ShapeGrid(grid, body.Reshape)
	if err != nil {
		// The sheet as read still comes back: it is what the developer
		// needs to correct a header row or a column name.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error(), "raw": raw, "sheets": sheets})
		return
	}
	out := make([][]string, len(rows))
	for i, row := range rows {
		line := make([]string, len(header))
		for j, col := range header {
			line[j] = row.Cells[col]
		}
		out[i] = line
	}
	jsonOK(w, map[string]any{"sheets": sheets, "raw": raw, "header": header, "rows": out, "row_count": len(out)})
}

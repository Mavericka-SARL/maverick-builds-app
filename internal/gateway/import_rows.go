package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"

	importpkgv1 "github.com/mavericks-engine/mavericks/gen/go/importpkg/v1"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
)

// fileRows counts a grid file's rows: those that resolved and those with a
// problem. A wide file's row stages one value per metric, so its values
// are not its rows: a 12-row file of units and price read "24 rows
// imported" (2026-10-21).
func fileRows(staged []importpkg.StagingRow, errs []*importpkgv1.ImportError) (ok, failed int) {
	bad := map[int32]bool{}
	for _, e := range errs {
		bad[e.RowNumber] = true
	}
	good := map[int]bool{}
	for _, s := range staged {
		if !bad[int32(s.RowNumber)] {
			good[s.RowNumber] = true
		}
	}
	return len(good), len(bad)
}

// importErrorRows is each problem ResolveRows found, as an import's answer
// lists them: the row's line in the file, the column, the problem.
func importErrorRows(errs []*importpkgv1.ImportError) []map[string]any {
	rows := make([]map[string]any, 0, len(errs))
	for _, e := range errs {
		rows = append(rows, map[string]any{
			"row": e.RowNumber, "column": e.Column, "code": e.ErrorCode,
			"message": e.Message, "raw_value": e.RawValue,
		})
	}
	return rows
}

// jsonGridRowErrs answers a refused grid file: 422, nothing imported, every
// row with a problem listed. A file imports whole or not at all — from the
// Import Wizard, a saved integration's run and a dashboard's Integration
// button alike (the owner's decision of 2026-10-22); a run used to import
// the rows that passed.
func jsonGridRowErrs(w http.ResponseWriter, staged []importpkg.StagingRow, errs []*importpkgv1.ImportError) {
	_, failed := fileRows(staged, errs)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":      fmt.Sprintf("import rejected: %d row(s) failed validation, nothing was imported", failed),
		"error_rows": failed,
		"errors":     importErrorRows(errs),
	})
}

// firstRowProblem names a refused file's first problem for a run's history.
func firstRowProblem(errs []*importpkgv1.ImportError) string {
	if len(errs) == 0 {
		return ""
	}
	e := errs[0]
	if e.Column != "" {
		return fmt.Sprintf("row %d, %s: %s", e.RowNumber, e.Column, e.Message)
	}
	return fmt.Sprintf("row %d: %s", e.RowNumber, e.Message)
}

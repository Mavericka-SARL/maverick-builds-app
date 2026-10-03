package gateway

// Converted files: an attachment the AI Developer has reshaped and
// column-mapped into the layout an import reads, handed back to the
// developer as CSV or Excel (prepare_converted_file). Only the recipe is
// stored — the attachment plus the reshape and column map — and every
// download rebuilds the file from the attachment with the same code an
// import uses, so what is downloaded is what an import would read.
//
//	GET /api/ai/sessions/{id}/conversions              the session's conversions
//	GET /api/ai/sessions/{id}/conversions/{cid}?format=csv|xlsx   the file
//
// Both are the session owner's, as every AI session route is.

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/internal/dataexport"
)

func (h *handler) aiPrepareConversion(ctx context.Context, sessionID string, req aiassistant.FileImportRequest) (string, error) {
	f, err := h.loadAttachedFile(ctx, sessionID, req)
	if err != nil {
		return "", err
	}
	if len(f.rows) == 0 {
		return f.layout(req) + "Nothing to convert: no rows are left after the reshape.", nil
	}
	base := strings.TrimSuffix(f.filename, filepath.Ext(f.filename))
	c, err := aiassistant.NewConversionStore(h.db.For(ctx)).Save(ctx, aiassistant.Conversion{
		SessionID: sessionID, DocumentID: f.docID, Sheet: req.Sheet, Reshape: req.Reshape, ColumnMap: req.ColumnMap,
		Filename: base + " (converted)", RowCount: len(f.rows), Columns: f.mapped,
	})
	if err != nil {
		return "", fmt.Errorf("save the conversion: %w", err)
	}
	return f.layout(req) + fmt.Sprintf("Converted file %q is ready: %d row(s), columns %s. The developer downloads it as CSV or Excel under \"Converted files\" in this chat (id %s). Nothing was imported.",
		c.Filename, c.RowCount, strings.Join(c.Columns, ", "), c.ID), nil
}

// aiConversions serves both conversion routes.
func (h *handler) aiConversions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/ai/sessions/"), "/") // [sid, "conversions", cid?]
	sess, err := h.aiChatStore(ctx).GetSession(ctx, parts[0])
	if err != nil || sess.UserID != a.UserID {
		jsonErr(w, fmt.Errorf("session not found"), http.StatusNotFound)
		return
	}
	store := aiassistant.NewConversionStore(h.db.For(ctx))
	if len(parts) < 3 || parts[2] == "" {
		list, err := store.List(ctx, sess.ID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		jsonOK(w, list)
		return
	}
	c, err := store.Get(ctx, sess.ID, parts[2])
	if err != nil {
		jsonErr(w, fmt.Errorf("conversion not found"), http.StatusNotFound)
		return
	}
	format := strings.ToLower(r.URL.Query().Get("format"))
	if format == "" {
		format = dataexport.FormatCSV
	}
	if format != dataexport.FormatCSV && format != dataexport.FormatXLSX {
		jsonErr(w, fmt.Errorf("format must be csv or xlsx"), http.StatusBadRequest)
		return
	}
	f, err := h.loadAttachedFile(ctx, sess.ID, aiassistant.FileImportRequest{
		File: c.DocumentID, Sheet: c.Sheet, Reshape: c.Reshape, ColumnMap: c.ColumnMap,
	})
	if err != nil {
		jsonErr(w, err, http.StatusUnprocessableEntity)
		return
	}
	var buf bytes.Buffer
	if err := dataexport.Write(&buf, dataexport.NewTable(f.mapped, conversionCells(f), format)); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", dataexport.ContentType(format))
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, dataexport.FileName(dataexport.Spec{Format: format}, c.Filename)))
	_, _ = w.Write(buf.Bytes())
}

// plainNumber is a number as an import writes and reads it back unchanged:
// no leading zeros, so a member code such as "007" stays text.
var plainNumber = regexp.MustCompile(`^-?(0|[1-9][0-9]*)(\.[0-9]+)?$`)

// conversionCells is the file's rows in its column order. A column whose
// every filled cell is a plain number is written as numbers (Excel then
// sums it); any other stays text, exactly as read.
func conversionCells(f *attachedFile) [][]dataexport.Value {
	numeric := make([]bool, len(f.mapped))
	for j, col := range f.mapped {
		numeric[j] = true
		filled := false
		for _, r := range f.rows {
			v := strings.TrimSpace(r.Cells[col])
			if v == "" {
				continue
			}
			filled = true
			if !plainNumber.MatchString(v) {
				numeric[j] = false
				break
			}
		}
		numeric[j] = numeric[j] && filled
	}
	out := make([][]dataexport.Value, len(f.rows))
	for i, r := range f.rows {
		row := make([]dataexport.Value, len(f.mapped))
		for j, col := range f.mapped {
			v := strings.TrimSpace(r.Cells[col])
			if numeric[j] && v != "" {
				n, _ := strconv.ParseFloat(v, 64)
				row[j] = dataexport.Value{Num: n, IsNum: true}
			} else {
				row[j] = dataexport.Value{Text: r.Cells[col]}
			}
		}
		out[i] = row
	}
	return out
}

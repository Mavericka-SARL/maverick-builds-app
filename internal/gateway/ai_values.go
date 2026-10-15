package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// The AI Developer's values tools (internal/aiassistant/tools_values.go):
// each goes through the developer's own route as the developer, and a file
// it writes becomes an attachment of the chat with a converted file the
// developer downloads.

// aiClearValues is clear_input_values.
func (h *handler) aiClearValues(ctx context.Context, r *http.Request, kind, id string, body json.RawMessage) (string, error) {
	status, out := h.callAsCaller(ctx, r, http.MethodPost, "/api/developer/"+kind+"s/"+url.PathEscape(id)+"/clear-values", nil, body)
	if status >= 300 {
		return "", routeError(status, out)
	}
	var res struct{ Cells, Withheld, Posted int }
	_ = json.Unmarshal(out, &res)
	msg := fmt.Sprintf("Cleared %d cell(s)", res.Cells)
	if res.Withheld > 0 {
		msg += fmt.Sprintf("; %d left that the developer may not write", res.Withheld)
	}
	if res.Posted > 0 {
		msg += fmt.Sprintf("; %d left that tables of records post (clear those records instead)", res.Posted)
	}
	return msg + ". Calculated metrics are recalculating.", nil
}

// aiFillSample is fill_sample_values.
func (h *handler) aiFillSample(ctx context.Context, r *http.Request, sessionID, userID, gridID string, body json.RawMessage, asFile bool) (string, error) {
	path := "/api/developer/grids/" + url.PathEscape(gridID) + "/sample-values"
	if !asFile {
		status, out := h.callAsCaller(ctx, r, http.MethodPost, path, nil, body)
		if status >= 300 {
			return "", routeError(status, out)
		}
		var res struct{ Cells, Withheld int }
		_ = json.Unmarshal(out, &res)
		msg := fmt.Sprintf("Filled %d cell(s) with sample values", res.Cells)
		if res.Withheld > 0 {
			msg += fmt.Sprintf("; %d left that the developer may not write", res.Withheld)
		}
		return msg + ". Calculated metrics are recalculating.", nil
	}
	var b map[string]any
	_ = json.Unmarshal(body, &b)
	if b == nil {
		b = map[string]any{}
	}
	b["dry_run"], b["format"] = true, "csv"
	status, out := h.callAsCaller(ctx, r, http.MethodPost, path, nil, b)
	if status >= 300 {
		return "", routeError(status, out)
	}
	var name string
	_ = h.db.QueryRow(ctx, `SELECT name FROM model.grid_def WHERE id::text = $1`, gridID).Scan(&name)
	return h.aiAttachCSV(ctx, sessionID, userID, "sample values - "+safeFileName(name)+".csv", out, "sample_values")
}

// aiAttachCSV attaches data, a CSV, to the chat as name, with a converted
// file the developer downloads as CSV or Excel.
func (h *handler) aiAttachCSV(ctx context.Context, sessionID, userID, name string, data []byte, source string) (string, error) {
	if len(data) > maxAIImportBytes {
		return "", fmt.Errorf("the file would be %d MB, more than an attachment keeps (%d MB): fill the grid on the server instead, or name members to make it smaller",
			len(data)>>20, maxAIImportBytes>>20)
	}
	text, mimeType, truncated, err := aiassistant.ExtractDocumentText(name, data)
	if err != nil {
		return "", err
	}
	doc, err := aiassistant.NewDocumentStore(h.db.For(ctx)).CreateDocument(ctx, sessionID, name, mimeType, text, truncated, data)
	if err != nil {
		return "", fmt.Errorf("attach the file: %w", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\r\n"), "\n")
	columns := strings.Split(strings.TrimRight(lines[0], "\r"), ",")
	if _, err := aiassistant.NewConversionStore(h.db.For(ctx)).Save(ctx, aiassistant.Conversion{
		SessionID: sessionID, DocumentID: doc.ID, Filename: strings.TrimSuffix(name, filepath.Ext(name)),
		RowCount: len(lines) - 1, Columns: columns,
	}); err != nil {
		return "", fmt.Errorf("offer the file for download: %w", err)
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryAIAssistant, EventType: auditlog.EventAIDocumentUploaded,
		ActorUserID: userID, ResourceType: "ai_document", ResourceID: doc.ID,
		Metadata: map[string]string{"session_id": sessionID, "filename": name, "source": source},
	})
	return fmt.Sprintf("Attached %s: %d data row(s), columns %s. The developer downloads it under \"Converted files\" as CSV or Excel; import_file_data {\"file\": %q, ...} imports it (preview_file_import first).",
		name, len(lines)-1, strings.Join(columns, ", "), name), nil
}

// safeFileName keeps a name usable as a file name.
func safeFileName(s string) string {
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune(`/\:*?"<>|`, r) {
			return '-'
		}
		return r
	}, strings.TrimSpace(s))
	if s == "" {
		return "grid"
	}
	return s
}

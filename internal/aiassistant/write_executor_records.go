package aiassistant

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/importpkg"
)

// RecordsSave is a save_records step in the import pipeline's shape: one
// row per record, the fields it names as columns, so each record is checked
// and saved as a record saved by hand or loaded from a file is (its values,
// who may add it, its status, where it posts, the edition's members) and a
// row whose typed code a record holds updates that record.
type RecordsSave struct {
	RevisionID string
	TableID    string
	Header     []string
	Rows       []importpkg.RawRow
	// MatchBy names the field a record is matched to the one it updates by
	// (recordpost.ImportRequest.MatchBy); "" matches by a typed code.
	MatchBy string
}

// maxRecordsPerStep bounds one step: more records belong in a file
// (import_file_data), where they can be previewed.
const maxRecordsPerStep = 200

// saveRecords adds records to a table, or updates those whose typed code a
// record holds, as the developer would on a dashboard's records widget: a
// placeholder account a later import names, a handful of reference rows.
// The assistant had no such step and asked the developer to type the record
// themselves (2026-11-16).
func (e *WriteExecutor) saveRecords(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		Table   string                       `json:"table"`
		TableID string                       `json:"table_id"`
		FormID  string                       `json:"form_id"`
		Records []map[string]json.RawMessage `json:"records"`
		MatchBy string                       `json:"match_by"`
	}
	if err := decodeParams(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	ref := strings.TrimSpace(firstNonEmpty(p.TableID, p.FormID, p.Table))
	if ref == "" || len(p.Records) == 0 {
		return "", "", fmt.Errorf(`table and records are required: {"table": "<table id or name>", "records": [{"<field name or label>": "value", ...}]}`)
	}
	if len(p.Records) > maxRecordsPerStep {
		return "", "", fmt.Errorf("%d records in one step; at most %d — load more from a file (import_file_data)", len(p.Records), maxRecordsPerStep)
	}
	if e.hooks.SaveRecords == nil {
		return "", "", fmt.Errorf("saving records is not available here")
	}
	tableID, err := e.requireInModel(ctx, "form", ref)
	if err != nil {
		return "", "", err
	}
	matchBy := ""
	if mb := strings.TrimSpace(p.MatchBy); mb != "" {
		if matchBy, err = e.tableMatchBy(ctx, tableID, mb); err != nil {
			return "", "", err
		}
	}
	var header []string
	seen := map[string]bool{}
	for _, rec := range p.Records {
		for k := range rec {
			if !seen[strings.ToLower(k)] {
				seen[strings.ToLower(k)] = true
				header = append(header, k)
			}
		}
	}
	sort.Strings(header)
	rows := make([]importpkg.RawRow, 0, len(p.Records))
	for i, rec := range p.Records {
		cells := make(map[string]string, len(header))
		for _, col := range header {
			v, ok := lookupFoldRaw(rec, col)
			if !ok {
				continue
			}
			s, err := recordCellText(v)
			if err != nil {
				return "", "", fmt.Errorf("record %d, %s: %w", i+1, col, err)
			}
			cells[col] = s
		}
		rows = append(rows, importpkg.RawRow{RowNumber: i + 1, Cells: cells})
	}
	result, err := e.hooks.SaveRecords(ctx, RecordsSave{RevisionID: e.revID, TableID: tableID, Header: header, Rows: rows, MatchBy: matchBy})
	if err != nil {
		return "", "", err
	}
	return result, "", nil
}

// recordCellText is a record value as an import cell holds it: text as it
// is, a number as written, true/false, null as empty.
func recordCellText(v json.RawMessage) (string, error) {
	t := strings.TrimSpace(string(v))
	switch {
	case t == "" || t == "null":
		return "", nil
	case t == "true" || t == "false":
		return t, nil
	case strings.HasPrefix(t, `"`):
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return "", err
		}
		return s, nil
	case strings.HasPrefix(t, "{") || strings.HasPrefix(t, "["):
		return "", fmt.Errorf("a field holds one value (text, a number, true or false), not %s", t)
	}
	return t, nil // a number, as written
}

func lookupFoldRaw(m map[string]json.RawMessage, key string) (json.RawMessage, bool) {
	if v, ok := m[key]; ok {
		return v, true
	}
	for k, v := range m {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return nil, false
}

// RecordsDelete is a delete_records step: the records of a table to delete,
// each named by its value of MatchBy (the table's code field when "") or by
// its record id.
type RecordsDelete struct {
	RevisionID string
	TableID    string
	MatchBy    string
	Values     []string
}

// deleteRecords deletes records of a table the developer names — the
// duplicates a load made, deals the source system dropped — as the
// developer would one by one on a records widget. The assistant could
// neither delete nor update a table's auto-coded records and asked the
// developer to do it by hand (2026-11-17).
func (e *WriteExecutor) deleteRecords(ctx context.Context, raw json.RawMessage) (string, string, error) {
	var p struct {
		Table   string            `json:"table"`
		TableID string            `json:"table_id"`
		FormID  string            `json:"form_id"`
		MatchBy string            `json:"match_by"`
		Records []json.RawMessage `json:"records"`
	}
	if err := decodeParams(raw, &p); err != nil {
		return "", "", fmt.Errorf("invalid params: %w", err)
	}
	ref := strings.TrimSpace(firstNonEmpty(p.TableID, p.FormID, p.Table))
	if ref == "" || len(p.Records) == 0 {
		return "", "", fmt.Errorf(`table and records are required: {"table": "<table id or name>", "records": ["<code>", ...]} — or "match_by": "<field>" with each record's value of it`)
	}
	if len(p.Records) > maxRecordsPerStep {
		return "", "", fmt.Errorf("%d records in one step; at most %d — split them over several steps", len(p.Records), maxRecordsPerStep)
	}
	if e.hooks.DeleteRecords == nil {
		return "", "", fmt.Errorf("deleting records is not available here")
	}
	tableID, err := e.requireInModel(ctx, "form", ref)
	if err != nil {
		return "", "", err
	}
	matchBy := ""
	if mb := strings.TrimSpace(p.MatchBy); mb != "" {
		if matchBy, err = e.tableMatchBy(ctx, tableID, mb); err != nil {
			return "", "", err
		}
	}
	values := make([]string, 0, len(p.Records))
	seen := map[string]bool{}
	for i, v := range p.Records {
		s, err := recordCellText(v)
		if err != nil {
			return "", "", fmt.Errorf("records[%d]: %w", i, err)
		}
		if s = strings.TrimSpace(s); s == "" {
			return "", "", fmt.Errorf("records[%d] is empty: name each record by its code (or its match_by value)", i)
		}
		if !seen[s] {
			seen[s] = true
			values = append(values, s)
		}
	}
	result, err := e.hooks.DeleteRecords(ctx, RecordsDelete{RevisionID: e.revID, TableID: tableID, MatchBy: matchBy, Values: values})
	if err != nil {
		return "", "", err
	}
	return result, "", nil
}

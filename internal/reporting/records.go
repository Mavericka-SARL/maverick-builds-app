package reporting

// Tables of records, read for the chat connector: which tables a model has
// and what their fields are (GET /api/forms), and the records query (POST
// /api/forms/{id}/records/query) — a page of records, or totals over them
// grouped by fields. Read-only, as the person: the route serves exactly
// the records the person sees in the console, and every count and total is
// made over those alone.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
)

// TableSummary is a table of records the person can read.
type TableSummary struct {
	ID     string       `json:"id"`
	Name   string       `json:"name"`
	Label  string       `json:"label"`
	Fields []TableField `json:"fields"`
	// HasStatus: the table's records have a status (draft, submitted,
	// approved, rejected), filtered and grouped as "status".
	HasStatus bool `json:"has_status"`
	// OwnRecordsOnly: the table shows each person only the records they
	// created, unless they administer it.
	OwnRecordsOnly bool `json:"own_records_only,omitempty"`
}

// TableField is a field of a table, as a records query names it.
type TableField struct {
	Name    string   `json:"name"`
	Label   string   `json:"label"`
	Type    string   `json:"type"`
	Options []string `json:"options,omitempty"`
}

// ListTables lists the tables of records of p (GET /api/forms).
func (s *Session) ListTables(ctx context.Context, p Pinned, search string) ([]TableSummary, error) {
	var forms []struct {
		ID             string `json:"id"`
		Name           string `json:"name"`
		Label          string `json:"label"`
		OwnRecordsOnly bool   `json:"own_records_only"`
		Fields         []struct {
			Name    string   `json:"name"`
			Label   string   `json:"label"`
			Type    string   `json:"type"`
			Options []string `json:"options"`
		} `json:"fields"`
	}
	if _, err := s.get(ctx, "tables", p.request("/api/forms", url.Values{"revision_id": {p.RevisionID}}), &forms); err != nil {
		return nil, err
	}
	out := []TableSummary{}
	for _, f := range forms {
		if !matches(search, f.Name, f.Label) {
			continue
		}
		t := TableSummary{ID: f.ID, Name: f.Name, Label: f.Label, OwnRecordsOnly: f.OwnRecordsOnly, Fields: []TableField{}}
		for _, fd := range f.Fields {
			if fd.Type == "status" {
				t.HasStatus = true
			}
			t.Fields = append(t.Fields, TableField{Name: fd.Name, Label: fd.Label, Type: fd.Type, Options: fd.Options})
		}
		out = append(out, t)
	}
	return out, nil
}

// RecordsQuery is a records query: filters, a search and a sort over a
// table's records, and optionally group_by and aggregates for totals.
type RecordsQuery struct {
	Filters    []RecordFilter    `json:"filters,omitempty"`
	Search     string            `json:"search,omitempty"`
	Sort       []RecordSort      `json:"sort,omitempty"`
	GroupBy    []RecordGroup     `json:"group_by,omitempty"`
	Aggregates []RecordAggregate `json:"aggregates,omitempty"`
	Limit      int               `json:"limit,omitempty"`
	Offset     int               `json:"offset,omitempty"`
}

// RecordFilter keeps the records whose field compares to a value.
type RecordFilter struct {
	Field  string `json:"field"`
	Op     string `json:"op,omitempty"`
	Value  any    `json:"value,omitempty"`
	Values []any  `json:"values,omitempty"`
}

// RecordSort orders records, or groups, by a field or an aggregate.
type RecordSort struct {
	Field string `json:"field"`
	Desc  bool   `json:"desc,omitempty"`
}

// RecordGroup groups by a field, a date field optionally by a period.
type RecordGroup struct {
	Field  string `json:"field"`
	Period string `json:"period,omitempty"`
}

// RecordAggregate is a total over each group.
type RecordAggregate struct {
	Op    string `json:"op"`
	Field string `json:"field,omitempty"`
}

// RecordsAnswer is the records query's answer: a page of records with how
// many match in all, or totals' rows with how many groups there are.
type RecordsAnswer struct {
	Context Pinned          `json:"context"`
	Table   json.RawMessage `json:"table"`
	Kind    string          `json:"kind"`
	Columns json.RawMessage `json:"columns"`
	// A page of records: each with its id, status (when the table has
	// one), when it was created, and its field values.
	Records []RecordRow `json:"records,omitempty"`
	Total   *int        `json:"total,omitempty"`
	// Totals: one row per group.
	Rows        []map[string]any `json:"rows,omitempty"`
	TotalGroups *int             `json:"total_groups,omitempty"`
	Statuses    []string         `json:"statuses,omitempty"`
}

// RecordRow is one record of a page.
type RecordRow struct {
	ID        string         `json:"id"`
	Status    string         `json:"status,omitempty"`
	CreatedAt string         `json:"created_at"`
	Values    map[string]any `json:"values"`
}

// QueryRecords runs a records query on table tableID of p.
func (s *Session) QueryRecords(ctx context.Context, p Pinned, tableID string, q RecordsQuery) (*RecordsAnswer, error) {
	if tableID == "" {
		return nil, invalid("table_id is required; list_tables returns it")
	}
	var resp struct {
		Kind    string          `json:"kind"`
		Table   json.RawMessage `json:"table"`
		Columns json.RawMessage `json:"columns"`
		Records []struct {
			ID        string         `json:"id"`
			Status    string         `json:"status"`
			CreatedAt string         `json:"created_at"`
			Data      map[string]any `json:"data"`
		} `json:"records"`
		Total       *int             `json:"total"`
		Rows        []map[string]any `json:"rows"`
		TotalGroups *int             `json:"total_groups"`
		Statuses    []string         `json:"statuses"`
	}
	req := p.request("/api/forms/"+url.PathEscape(tableID)+"/records/query", nil)
	req.Method, req.Body = http.MethodPost, q
	if _, err := s.get(ctx, "table", req, &resp); err != nil {
		return nil, err
	}
	out := &RecordsAnswer{
		Context: p, Table: resp.Table, Kind: resp.Kind, Columns: resp.Columns,
		Total: resp.Total, Rows: resp.Rows, TotalGroups: resp.TotalGroups, Statuses: resp.Statuses,
	}
	for _, r := range resp.Records {
		out.Records = append(out.Records, RecordRow{ID: r.ID, Status: r.Status, CreatedAt: r.CreatedAt, Values: r.Data})
	}
	return out, nil
}

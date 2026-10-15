package mcpserver

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mavericks-engine/mavericks/internal/reporting"
)

type tablesIn struct {
	ModelContext
	Search string `json:"search,omitempty" jsonschema:"optional text to match in table names"`
}

type recordsIn struct {
	ModelContext
	TableID    string                      `json:"table_id" jsonschema:"table id from list_tables"`
	Filters    []reporting.RecordFilter    `json:"filters,omitempty" jsonschema:"all must hold. field: a field name from list_tables, or status, created_at, updated_at, created_by ('me' is you). op: eq (default), ne, in, not_in, gt, gte, lt, lte, between (values: [low, high]), contains, starts_with, empty, not_empty. Dates are YYYY-MM-DD; text compares without case"`
	Search     string                      `json:"search,omitempty" jsonschema:"optional text any field, member name or status of a record contains"`
	Sort       []reporting.RecordSort      `json:"sort,omitempty" jsonschema:"fields to sort by (records end newest first); with group_by, a group or an aggregate key such as sum_amount"`
	GroupBy    []reporting.RecordGroup     `json:"group_by,omitempty" jsonschema:"fields to group totals by, at most 4; a date field may take period: day, week, month, quarter or year"`
	Aggregates []reporting.RecordAggregate `json:"aggregates,omitempty" jsonschema:"totals per group: count (records), count_distinct, sum, avg, min or max of a field; a group_by without aggregates counts"`
	Limit      int                         `json:"limit,omitempty" jsonschema:"records per page (default 100, at most 1000) or groups (default 1000, at most 5000)"`
	Offset     int                         `json:"offset,omitempty" jsonschema:"records or groups to skip, for the next page"`
}

// registerRecords adds the tools that read tables of records: the records
// people save in a model (requests, expenses, deals), apart from its grids.
func (t *tools) registerRecords(s *sdk.Server) {
	sdk.AddTool(s, &sdk.Tool{Name: "list_tables", Annotations: readOnly("List tables of records"),
		Description: "Tables of records of a model you can open — requests, expenses, deals and other records people save — with their fields, and whether their records have a status. Query them with query_records."},
		read(t, "list_tables", func(ctx context.Context, s *reporting.Session, in tablesIn) (any, error) {
			p, err := in.pin(ctx, s)
			if err != nil {
				return nil, err
			}
			list, err := s.ListTables(ctx, p, in.Search)
			if err != nil {
				return nil, err
			}
			return map[string]any{"context": p, "tables": list}, nil
		}))

	sdk.AddTool(s, &sdk.Tool{Name: "query_records", Annotations: readOnly("Query records"),
		Description: "Reads a table's records, read-only: a page of records matching filters, a search and a sort, with how many match in all; or, with group_by and/or aggregates, totals over every matching record — how many, how much, by what (a field, or a date field by month or another period). Totals are counted by the engine over all the records you may see, never estimated from a page. Only the records the console shows you are read."},
		read(t, "query_records", func(ctx context.Context, s *reporting.Session, in recordsIn) (any, error) {
			p, err := in.pin(ctx, s)
			if err != nil {
				return nil, err
			}
			return s.QueryRecords(ctx, p, in.TableID, reporting.RecordsQuery{
				Filters: in.Filters, Search: in.Search, Sort: in.Sort,
				GroupBy: in.GroupBy, Aggregates: in.Aggregates, Limit: in.Limit, Offset: in.Offset,
			})
		}))
}

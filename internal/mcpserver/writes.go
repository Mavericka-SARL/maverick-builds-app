package mcpserver

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mavericks-engine/mavericks/internal/reporting"
)

// errReadOnly is a write through a connection granted reading only: one
// made before the write scope existed, or whose person did not allow it.
var errReadOnly = &reporting.Error{Code: "read_only",
	Message: "this connection may only read. To change data from this chat, disconnect maverickbuilds.app and connect it again, allowing it to change grid values and records"}

type cellIn struct {
	MetricID string            `json:"metric_id" jsonschema:"an input metric's id from describe_source"`
	Members  map[string]string `json:"members" jsonschema:"one leaf member for each of the metric's dimensions: dimension id to member code, from list_members"`
	Value    *float64          `json:"value,omitempty" jsonschema:"the number to store; a percent metric takes a fraction (0.12 for 12%)"`
	Text     *string           `json:"text,omitempty" jsonschema:"a text metric's text, or a date metric's date as yyyy-mm-dd"`
	Member   *string           `json:"member,omitempty" jsonschema:"a pick-list metric's choice, by member code or label"`
	Clear    bool              `json:"clear,omitempty" jsonschema:"true empties the cell: no value, not 0"`
}

type writeIn struct {
	ModelContext
	Cells  []cellIn `json:"cells" jsonschema:"the cells to write, 1 to 500; give each exactly one of value, text, member or clear"`
	DryRun bool     `json:"dry_run,omitempty" jsonschema:"check every cell and write none"`
}

type recordUpdateIn struct {
	ModelContext
	TableID  string         `json:"table_id" jsonschema:"the table's id from list_tables"`
	RecordID string         `json:"record_id" jsonschema:"the record's id from query_records"`
	Fields   map[string]any `json:"fields,omitempty" jsonschema:"the fields to change, by field name from list_tables, with their new values as query_records shows them: a number, a text, a select's option, a date as yyyy-mm-dd, true or false, a dimension field's member code or label; null empties a field. Fields not named keep their values"`
	Status   string         `json:"status,omitempty" jsonschema:"the record's new status (draft, submitted, approved or rejected), for a table whose records have one; omitted, the status stays"`
	DryRun   bool           `json:"dry_run,omitempty" jsonschema:"check the change and save nothing"`
}

func (t *tools) registerWrites(s *sdk.Server) {
	f, yes := false, true
	sdk.AddTool(s, &sdk.Tool{Name: "write_cells",
		Annotations: &sdk.ToolAnnotations{Title: "Write grid cells", ReadOnlyHint: false, DestructiveHint: &yes, IdempotentHint: true, OpenWorldHint: &f},
		Description: "Write input cells of a model's active revision as you: numbers, texts, dates, pick-list choices, or clears. All or nothing — every cell is checked as the console checks a typed one (input metrics only, your access rules, workflow locks, the metric's input_rules from describe_source, the workspace's plan) before any is written, and the grid is recalculated once. A refusal lists each refused cell by its index. Confirm the values with the person before writing; dry_run checks them without writing. Overwrites what the cells held; the cell history keeps the earlier values."},
		called(t, "write_cells", func(ctx context.Context, s *reporting.Session, c call, in writeIn) (any, error) {
			if !c.writes {
				return nil, errReadOnly
			}
			p, err := in.pin(ctx, s)
			if err != nil {
				return nil, err
			}
			cells := make([]reporting.CellWrite, 0, len(in.Cells))
			for _, cell := range in.Cells {
				cells = append(cells, reporting.CellWrite{MetricID: cell.MetricID, Members: cell.Members,
					Value: cell.Value, Text: cell.Text, Member: cell.Member, Clear: cell.Clear})
			}
			return s.WriteCells(ctx, p, cells, in.DryRun)
		}))

	sdk.AddTool(s, &sdk.Tool{Name: "update_record",
		Annotations: &sdk.ToolAnnotations{Title: "Update a record", ReadOnlyHint: false, DestructiveHint: &yes, IdempotentHint: true, OpenWorldHint: &f},
		Description: "Change one record of a model's table as you — a deal, a request, an expense: the fields you name (the others keep their values) and/or its status. Checked as the console's record form checks a save: your permissions on the record, which status moves you may make, a parent record already decided, and each field's rules; a refusal says why and saves nothing. Grids the table feeds follow the record — change the deal, not the grid it feeds. Confirm the change with the person before saving; dry_run checks it without saving. The record's history keeps the earlier values."},
		called(t, "update_record", func(ctx context.Context, s *reporting.Session, c call, in recordUpdateIn) (any, error) {
			if !c.writes {
				return nil, errReadOnly
			}
			p, err := in.pin(ctx, s)
			if err != nil {
				return nil, err
			}
			return s.UpdateRecord(ctx, p, in.TableID, in.RecordID, in.Fields, in.Status, in.DryRun)
		}))
}

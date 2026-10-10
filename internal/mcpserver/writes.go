package mcpserver

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mavericks-engine/mavericks/internal/reporting"
)

// errReadOnly is a write through a connection granted reading only: one
// made before the write scope existed, or whose person did not allow it.
var errReadOnly = &reporting.Error{Code: "read_only",
	Message: "this connection may only read. To change data from this chat, disconnect maverickbuilds.app and connect it again, allowing it to change grid values"}

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

func (t *tools) registerWrites(s *sdk.Server) {
	f, yes := false, true
	sdk.AddTool(s, &sdk.Tool{Name: "write_cells",
		Annotations: &sdk.ToolAnnotations{Title: "Write grid cells", ReadOnlyHint: false, DestructiveHint: &yes, IdempotentHint: true, OpenWorldHint: &f},
		Description: "Write input cells of a model's active revision as you: numbers, texts, dates, pick-list choices, or clears. All or nothing — every cell is checked as the console checks a typed one (input metrics only, your access rules, workflow locks, the workspace's plan) before any is written, and the grid is recalculated once. A refusal lists each refused cell by its index. Confirm the values with the person before writing; dry_run checks them without writing. Overwrites what the cells held; the cell history keeps the earlier values."},
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
}

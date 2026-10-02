// Package mcpserver is the chat connector's Model Context Protocol server:
// the tools ChatGPT, Claude and other MCP hosts call to analyse a person's
// grid data and chart it, compare it and report on it in the conversation.
// The protocol lives here; the reads are internal/reporting's, made as the
// person the host's token was issued for (the token's subject, verified by
// the gateway before any call reaches a tool).
//
// Only grid data is read. Charts and reports are made in the chat from
// grids — never from dashboards — and nothing is saved in maverickbuilds.app.
// No tool writes, publishes, runs or schedules anything, and the gateway
// refuses any route outside its grid-read allowlist to a connector's reads
// besides.
package mcpserver

import (
	"context"
	"errors"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mavericks-engine/mavericks/internal/reporting"
)

// Observer is told about every tool call: who made it, which tool, how long
// it took and how it ended. It must not be given arguments or results.
type Observer func(ctx context.Context, subject, tool string, took time.Duration, err error)

// Instructions is what the server tells a host about itself.
const Instructions = `Read-only analysis of maverickbuilds.app grid data, as the signed-in person.

Start with list_models, then pass its application_id and model_id to every other tool. Discover before querying: list_sources lists the grids, describe_source their metrics and dimensions, list_members the members you can filter or group by. Never guess an id.

query_grid reads the engine's own values: one total per metric for a filtered slice, or values per member of a group_by dimension. compare_grid compares two readings (actual against budget, one period against another). render_chart and render_report draw charts and reports from grid queries in this conversation; they are not saved in maverickbuilds.app and do not use dashboards.

Every read is made under the access the person's administrators configured; hidden members and metrics are absent, and values that depend on them are withheld. A value whose state is not "ok" is not zero. Ratios, averages and balances are not additive: ask query_grid for the total you need instead of adding rows. This connection cannot change data.`

// New returns the connector's MCP server over svc.
func New(svc *reporting.Service, version string, observe Observer) *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{Name: "maverickbuilds-reporting", Title: "maverickbuilds.app reporting", Version: version},
		&sdk.ServerOptions{Instructions: Instructions})
	t := &tools{svc: svc, observe: observe}
	t.register(s)
	t.registerPresentation(s)
	return s
}

type tools struct {
	svc     *reporting.Service
	observe Observer
}

// errNotSignedIn is a call without a verified subject — never reached
// through the gateway, which verifies every request first.
var errNotSignedIn = errors.New("unauthorized: this connection is not signed in")

// readOnly marks a tool as a pure read: no side effects, safe to repeat,
// reading only maverickbuilds.app.
func readOnly(title string) *sdk.ToolAnnotations {
	f := false
	return &sdk.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: &f, OpenWorldHint: &f}
}

// read wraps a tool body: it binds the reporting session to the verified
// subject of this call, and reports the call to the observer.
func read[In any](t *tools, name string, body func(ctx context.Context, s *reporting.Session, in In) (any, error)) sdk.ToolHandlerFor[In, any] {
	return func(ctx context.Context, req *sdk.CallToolRequest, in In) (*sdk.CallToolResult, any, error) {
		start := time.Now()
		subject := ""
		if req != nil && req.Extra != nil && req.Extra.TokenInfo != nil {
			subject = req.Extra.TokenInfo.UserID
		}
		var out any
		var err error
		if subject == "" {
			err = errNotSignedIn
		} else {
			out, err = body(ctx, t.svc.As(subject), in)
		}
		if t.observe != nil {
			t.observe(ctx, subject, name, time.Since(start), err)
		}
		if err != nil {
			var re *reporting.Error
			if errors.As(err, &re) {
				return nil, nil, errors.New(re.Code + ": " + re.Message)
			}
			if errors.Is(err, errNotSignedIn) {
				return nil, nil, err
			}
			return nil, nil, errors.New("unavailable: the read could not be completed; try again")
		}
		return nil, out, nil
	}
}

// ModelContext selects the model a tool reads.
type ModelContext struct {
	ApplicationID string `json:"application_id" jsonschema:"application id from list_models"`
	ModelID       string `json:"model_id" jsonschema:"model id from list_models"`
	RevisionID    string `json:"revision_id,omitempty" jsonschema:"optional; if given it must be the model's active revision, which every read uses"`
}

func (c ModelContext) pin(ctx context.Context, s *reporting.Session) (reporting.Pinned, error) {
	return s.Pin(ctx, c.ApplicationID, c.ModelID, c.RevisionID)
}

type accessIn struct{}

type searchIn struct {
	Search string `json:"search,omitempty" jsonschema:"optional case-insensitive text to match in names"`
}

type sourcesIn struct {
	ModelContext
	Search string `json:"search,omitempty" jsonschema:"optional text to match in grid names"`
}

type describeIn struct {
	ModelContext
	SourceID string `json:"source_id" jsonschema:"grid id from list_sources"`
}

type membersIn struct {
	ModelContext
	GridID      string `json:"grid_id" jsonschema:"grid id from list_sources"`
	DimensionID string `json:"dimension_id" jsonschema:"dimension id from describe_source"`
	Search      string `json:"search,omitempty" jsonschema:"optional text to match in member codes and labels"`
	ParentCode  string `json:"parent_code,omitempty" jsonschema:"optional: only the children of this member"`
	Limit       int    `json:"limit,omitempty" jsonschema:"page size, default 200, at most 1000"`
	Cursor      string `json:"cursor,omitempty" jsonschema:"next_cursor of the previous page"`
}

type gridQueryIn struct {
	ModelContext
	GridID     string            `json:"grid_id" jsonschema:"grid id from list_sources"`
	MetricIDs  []string          `json:"metric_ids,omitempty" jsonschema:"metric ids from describe_source; empty reads the grid's first 20 metrics"`
	Filters    map[string]string `json:"filters,omitempty" jsonschema:"dimension id to member code; a parent member covers its subtree. Members must come from list_members"`
	GroupBy    string            `json:"group_by,omitempty" jsonschema:"dimension id to break values down by; empty returns one engine total per metric"`
	LeavesOnly bool              `json:"leaves_only,omitempty" jsonschema:"with group_by: only leaf members, so rows can be compared without parents beside their children"`
}

func (t *tools) register(s *sdk.Server) {
	sdk.AddTool(s, &sdk.Tool{Name: "get_connection_access", Annotations: readOnly("What this connection can read"),
		Description: "Your identity, your roles and the kinds of resources this read-only connection reads for you, with what decides each. Lists nothing about anyone else."},
		read(t, "get_connection_access", func(ctx context.Context, s *reporting.Session, _ accessIn) (any, error) {
			return s.Access(ctx)
		}))

	sdk.AddTool(s, &sdk.Tool{Name: "list_models", Annotations: readOnly("List models"),
		Description: "Models you can open, across your workspaces, with the application_id and model_id every other model tool takes."},
		read(t, "list_models", func(ctx context.Context, s *reporting.Session, in searchIn) (any, error) {
			models, err := s.ListModels(ctx, in.Search)
			return map[string]any{"models": models}, err
		}))

	sdk.AddTool(s, &sdk.Tool{Name: "list_sources", Annotations: readOnly("List grids"),
		Description: "Grids of a model you can read, with their dimensions and how many metrics you can see. Charts and reports are made from these grids."},
		read(t, "list_sources", func(ctx context.Context, s *reporting.Session, in sourcesIn) (any, error) {
			p, err := in.pin(ctx, s)
			if err != nil {
				return nil, err
			}
			grids, err := s.ListGrids(ctx, p, in.Search)
			if err != nil {
				return nil, err
			}
			return map[string]any{"context": p, "grids": grids}, nil
		}))

	sdk.AddTool(s, &sdk.Tool{Name: "describe_source", Annotations: readOnly("Describe a grid"),
		Description: "A grid's metrics (input or calculated, aggregation, time summary, format) and dimensions."},
		read(t, "describe_source", func(ctx context.Context, s *reporting.Session, in describeIn) (any, error) {
			p, err := in.pin(ctx, s)
			if err != nil {
				return nil, err
			}
			return s.DescribeGrid(ctx, p, in.SourceID)
		}))

	sdk.AddTool(s, &sdk.Tool{Name: "list_members", Annotations: readOnly("List dimension members"),
		Description: "Members of one grid dimension you can see, in the dimension's own order (calendar order for time), for filters and group-by."},
		read(t, "list_members", func(ctx context.Context, s *reporting.Session, in membersIn) (any, error) {
			p, err := in.pin(ctx, s)
			if err != nil {
				return nil, err
			}
			return s.ListMembers(ctx, p, reporting.MemberQuery{GridID: in.GridID, DimensionID: in.DimensionID,
				Search: in.Search, ParentCode: in.ParentCode, Limit: in.Limit, Cursor: in.Cursor})
		}))

	sdk.AddTool(s, &sdk.Tool{Name: "query_grid", Annotations: readOnly("Query a grid"),
		Description: "Metric values of a grid, resolved by the engine: one total per metric for the filtered slice, or values per member of a group_by dimension (up to 500 members). Each value carries a state; only \"ok\" values are numbers. A filter naming a member you cannot see is refused, never replaced."},
		read(t, "query_grid", func(ctx context.Context, s *reporting.Session, in gridQueryIn) (any, error) {
			p, err := in.pin(ctx, s)
			if err != nil {
				return nil, err
			}
			return s.QueryGrid(ctx, p, reporting.GridQuery{GridID: in.GridID, MetricIDs: in.MetricIDs, Filters: in.Filters,
				GroupBy: in.GroupBy, LeavesOnly: in.LeavesOnly})
		}))
}

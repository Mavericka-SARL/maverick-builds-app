package mcpserver

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mavericks-engine/mavericks/internal/reporting"
)

type dashboardsIn struct {
	ModelContext
	Search string `json:"search,omitempty" jsonschema:"optional text to match in dashboard names, folders and tags"`
}

type dashboardIn struct {
	ModelContext
	DashboardID string `json:"dashboard_id" jsonschema:"dashboard id from list_dashboards, or a link's dashboard_id from describe_dashboard"`
}

type workflowsIn struct {
	ModelContext
	Search string `json:"search,omitempty" jsonschema:"optional text to match in workflow names and descriptions"`
}

type workflowIn struct {
	ModelContext
	WorkflowID string `json:"workflow_id" jsonschema:"workflow id from list_workflows"`
}

// registerProcess adds the tools that read the process around the grids:
// dashboards as pages, and published workflows with their steps.
func (t *tools) registerProcess(s *sdk.Server) {
	sdk.AddTool(s, &sdk.Tool{Name: "list_dashboards", Annotations: readOnly("List dashboards"),
		Description: "Dashboards of a model you can open, with their folders and tags. Dashboards are where a model's pages explain its process and lead from one step to the next."},
		read(t, "list_dashboards", func(ctx context.Context, s *reporting.Session, in dashboardsIn) (any, error) {
			p, err := in.pin(ctx, s)
			if err != nil {
				return nil, err
			}
			list, err := s.ListDashboards(ctx, p, in.Search)
			if err != nil {
				return nil, err
			}
			return map[string]any{"context": p, "dashboards": list}, nil
		}))

	sdk.AddTool(s, &sdk.Tool{Name: "describe_dashboard", Annotations: readOnly("Describe a dashboard"),
		Description: "A dashboard as a page, in reading order: the text of its text widgets (instructions, explanations), its links to other dashboards resolved so you can follow them, and what each widget shows — which grid, metrics, axis and fixed filters — or does. No numbers: query the widget's grid with query_grid for them."},
		read(t, "describe_dashboard", func(ctx context.Context, s *reporting.Session, in dashboardIn) (any, error) {
			p, err := in.pin(ctx, s)
			if err != nil {
				return nil, err
			}
			return s.DescribeDashboard(ctx, p, in.DashboardID)
		}))

	sdk.AddTool(s, &sdk.Tool{Name: "list_workflows", Annotations: readOnly("List workflows"),
		Description: "Published workflows of a model — approval and submission processes — with their descriptions."},
		read(t, "list_workflows", func(ctx context.Context, s *reporting.Session, in workflowsIn) (any, error) {
			p, err := in.pin(ctx, s)
			if err != nil {
				return nil, err
			}
			list, err := s.ListWorkflows(ctx, p, in.Search)
			if err != nil {
				return nil, err
			}
			return map[string]any{"context": p, "workflows": list}, nil
		}))

	sdk.AddTool(s, &sdk.Tool{Name: "describe_workflow", Annotations: readOnly("Describe a workflow"),
		Description: "One published workflow: what is asked when it starts, and its steps — name, type, written instructions, the roles that act on each, deadlines, and which step follows. Describes only; never starts, approves or submits."},
		read(t, "describe_workflow", func(ctx context.Context, s *reporting.Session, in workflowIn) (any, error) {
			p, err := in.pin(ctx, s)
			if err != nil {
				return nil, err
			}
			return s.DescribeWorkflow(ctx, p, in.WorkflowID)
		}))
}

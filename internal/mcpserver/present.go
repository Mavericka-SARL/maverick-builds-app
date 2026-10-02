package mcpserver

// Charts, comparisons and reports in the conversation, built directly from
// grid data (internal/reporting/presentation.go). The tools take grid
// queries, never numbers: each call reads the grids as the person, now, and
// nothing is saved in maverickbuilds.app — no dashboard, widget or stored
// result. A host with MCP Apps shows the interactive view (view.html); every
// host gets a text summary with the data table, and a PNG of each chart.

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mavericks-engine/mavericks/internal/reporting"
)

//go:embed view.html
var viewHTML string

const (
	viewURI  = "ui://maverickbuilds/view.html"
	viewMIME = "text/html;profile=mcp-app"
)

// uiMeta ties a tool to the view: the MCP Apps key, and ChatGPT's alias.
func uiMeta() sdk.Meta {
	return sdk.Meta{"ui": map[string]any{"resourceUri": viewURI}, "openai/outputTemplate": viewURI}
}

// GridQueryArgs is one grid query, as the chart, comparison and report
// tools take it.
type GridQueryArgs struct {
	GridID     string            `json:"grid_id" jsonschema:"grid id from list_sources"`
	MetricIDs  []string          `json:"metric_ids,omitempty" jsonschema:"metric ids from describe_source"`
	Filters    map[string]string `json:"filters,omitempty" jsonschema:"dimension id to member code (from list_members)"`
	GroupBy    string            `json:"group_by,omitempty" jsonschema:"dimension id to break values down by"`
	LeavesOnly bool              `json:"leaves_only,omitempty" jsonschema:"with group_by: leaf members only (needed for pie and histogram)"`
}

func (a GridQueryArgs) query() reporting.GridQuery {
	return reporting.GridQuery{GridID: a.GridID, MetricIDs: a.MetricIDs, Filters: a.Filters, GroupBy: a.GroupBy, LeavesOnly: a.LeavesOnly}
}

type chartIn struct {
	ModelContext
	GridQueryArgs
	Kind      string `json:"kind,omitempty" jsonschema:"bar (default), line, pie, scatter or histogram"`
	Title     string `json:"title,omitempty" jsonschema:"optional title"`
	XMetricID string `json:"x_metric_id,omitempty" jsonschema:"scatter: the x-axis metric (default the first)"`
	YMetricID string `json:"y_metric_id,omitempty" jsonschema:"scatter: the y-axis metric (default the second)"`
	Bins      int    `json:"bins,omitempty" jsonschema:"histogram: number of bins, 3 to 30"`
}

type chartView struct {
	View         string                `json:"view"`
	Context      reporting.Pinned      `json:"context"`
	Chart        *reporting.ChartSpec  `json:"chart"`
	Data         *reporting.GridResult `json:"data"`
	ReadAt       string                `json:"read_at"`
	Alternatives []string              `json:"alternatives"`
	Request      chartIn               `json:"request"`
}

type compareIn struct {
	ModelContext
	Current  GridQueryArgs `json:"current" jsonschema:"one metric, e.g. actual revenue by product"`
	Baseline GridQueryArgs `json:"baseline" jsonschema:"one metric with the same group_by, e.g. budget by product, or the same metric under other filters"`
}

type sectionIn struct {
	Heading      string         `json:"heading"`
	Commentary   string         `json:"commentary,omitempty" jsonschema:"your explanation; shown as the assistant's, not the engine's"`
	Presentation string         `json:"presentation" jsonschema:"table, kpi, bar, line, pie, scatter, histogram or comparison"`
	Query        GridQueryArgs  `json:"query"`
	Baseline     *GridQueryArgs `json:"baseline,omitempty" jsonschema:"comparison: the baseline query"`
	XMetricID    string         `json:"x_metric_id,omitempty"`
	YMetricID    string         `json:"y_metric_id,omitempty"`
	Bins         int            `json:"bins,omitempty"`
}

type reportIn struct {
	ModelContext
	Title    string      `json:"title"`
	Sections []sectionIn `json:"sections" jsonschema:"1 to 10 sections, each read from grid data"`
}

type reportView struct {
	View   string            `json:"view"`
	Report *reporting.Report `json:"report"`
}

// present wraps a tool whose result carries its own content (summary text,
// images) beside the structured view.
func present[In any](t *tools, name string, body func(ctx context.Context, s *reporting.Session, in In) (any, []sdk.Content, error)) sdk.ToolHandlerFor[In, any] {
	return func(ctx context.Context, req *sdk.CallToolRequest, in In) (*sdk.CallToolResult, any, error) {
		var content []sdk.Content
		h := read(t, name, func(ctx context.Context, s *reporting.Session, in In) (any, error) {
			out, c, err := body(ctx, s, in)
			content = c
			return out, err
		})
		_, out, err := h(ctx, req, in)
		if err != nil {
			return nil, nil, err
		}
		return &sdk.CallToolResult{Content: content}, out, nil
	}
}

func (t *tools) registerPresentation(s *sdk.Server) {
	s.AddResource(&sdk.Resource{URI: viewURI, Name: "reporting-view", Title: "maverickbuilds.app chart and report view", MIMEType: viewMIME},
		func(context.Context, *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
			return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{
				URI: viewURI, MIMEType: viewMIME, Text: viewHTML,
				// Self-contained: no network, no outside assets.
				Meta: sdk.Meta{"ui": map[string]any{"prefersBorder": true, "csp": map[string]any{"connectDomains": []string{}, "resourceDomains": []string{}}}},
			}}}, nil
		})

	sdk.AddTool(s, &sdk.Tool{Name: "render_chart", Meta: uiMeta(), Annotations: readOnly("Chart grid data"),
		Description: "A chart (bar, line, pie, scatter, histogram) of grid data, read as you now and drawn from the engine's values — nothing is saved in maverickbuilds.app. Returns the chart, its data table and a picture. Pie and histogram need group_by with leaves_only; scatter needs two metrics; line needs group_by (a time dimension reads best)."},
		present(t, "render_chart", func(ctx context.Context, s *reporting.Session, in chartIn) (any, []sdk.Content, error) {
			p, err := in.pin(ctx, s)
			if err != nil {
				return nil, nil, err
			}
			res, err := s.QueryGrid(ctx, p, in.query())
			if err != nil {
				return nil, nil, err
			}
			opt := reporting.ChartOptions{Kind: in.Kind, Title: in.Title, XMetricID: in.XMetricID, YMetricID: in.YMetricID, Bins: in.Bins}
			spec, err := reporting.BuildChart(res, opt)
			if err != nil {
				return nil, nil, err
			}
			view := chartView{View: "chart", Context: p, Chart: spec, Data: res, ReadAt: res.ReadAt, Request: in}
			for _, k := range []string{reporting.KindBar, reporting.KindLine, reporting.KindPie, reporting.KindScatter, reporting.KindHistogram} {
				o := opt
				o.Kind, o.Title = k, spec.Title
				if _, err := reporting.BuildChart(res, o); err == nil {
					view.Alternatives = append(view.Alternatives, k)
				}
			}
			content := []sdk.Content{&sdk.TextContent{Text: chartSummary(p, spec, res)}}
			if img, err := reporting.RenderPNG(spec); err == nil {
				content = append(content, &sdk.ImageContent{Data: img, MIMEType: "image/png"})
			}
			return view, content, nil
		}))

	sdk.AddTool(s, &sdk.Tool{Name: "compare_grid", Annotations: readOnly("Compare grid data"),
		Description: "Compare two grid readings member by member — actual against budget, this period against the last: each side one metric with the same group_by. difference = current − baseline; percent_change = difference ÷ |baseline| × 100, null with a reason when a side has no value or the baseline is zero. Unmatched members are listed, never taken as zero."},
		read(t, "compare_grid", func(ctx context.Context, s *reporting.Session, in compareIn) (any, error) {
			p, err := in.pin(ctx, s)
			if err != nil {
				return nil, err
			}
			return s.Compare(ctx, p, in.Current.query(), in.Baseline.query())
		}))

	sdk.AddTool(s, &sdk.Tool{Name: "render_report", Meta: uiMeta(), Annotations: readOnly("Report on grid data"),
		Description: "A report of up to 10 sections — tables, KPIs, charts and comparisons — each read from grid data as you now; nothing is saved in maverickbuilds.app. Your commentary is shown as yours, not the engine's: keep each number in it traceable to a section. Returns the report, a text version with the tables, and a picture of each chart."},
		present(t, "render_report", func(ctx context.Context, s *reporting.Session, in reportIn) (any, []sdk.Content, error) {
			p, err := in.pin(ctx, s)
			if err != nil {
				return nil, nil, err
			}
			var secs []reporting.ReportSection
			for _, sec := range in.Sections {
				rs := reporting.ReportSection{Heading: sec.Heading, Commentary: sec.Commentary, Presentation: sec.Presentation,
					Query: sec.Query.query(), Chart: reporting.ChartOptions{XMetricID: sec.XMetricID, YMetricID: sec.YMetricID, Bins: sec.Bins}}
				if sec.Baseline != nil {
					b := sec.Baseline.query()
					rs.Baseline = &b
				}
				secs = append(secs, rs)
			}
			rep, err := s.BuildReport(ctx, p, in.Title, secs)
			if err != nil {
				return nil, nil, err
			}
			content := []sdk.Content{&sdk.TextContent{Text: reportSummary(rep)}}
			for _, sec := range rep.Sections {
				if sec.Chart != nil {
					if img, err := reporting.RenderPNG(sec.Chart); err == nil {
						content = append(content, &sdk.ImageContent{Data: img, MIMEType: "image/png"})
					}
				}
			}
			return reportView{View: "report", Report: rep}, content, nil
		}))
}

// ── text for every host ─────────────────────────────────────────────────────

func chartSummary(p reporting.Pinned, spec *reporting.ChartSpec, res *reporting.GridResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s chart “%s” — %s, %s (revision %s), grid %s", titleCase(spec.Kind), spec.Title, p.ApplicationName, p.ModelName, p.RevisionName, res.Source.Name)
	if spec.Subtitle != "" {
		fmt.Fprintf(&b, "; filters: %s", spec.Subtitle)
	}
	b.WriteString(".\n")
	if top, bottom := spec.TopAndBottom(); top != "" && len(spec.Categories) > 1 {
		fmt.Fprintf(&b, "Highest %s; lowest %s.\n", top, bottom)
	}
	for _, n := range spec.Notes {
		b.WriteString(n + ".\n")
	}
	b.WriteString("\n" + gridMarkdown(res, 50))
	fmt.Fprintf(&b, "\nRead from maverickbuilds.app as the signed-in person at %s; values are the engine's.", res.ReadAt)
	return b.String()
}

func reportSummary(r *reporting.Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Report “%s” — %s, %s (revision %s), read %s.\n", r.Title, r.Context.ApplicationName, r.Context.ModelName, r.Context.RevisionName, r.ReadAt)
	if !r.Complete {
		b.WriteString("Incomplete: some sections could not be read.\n")
	}
	for _, sec := range r.Sections {
		fmt.Fprintf(&b, "\n## %s\n", sec.Heading)
		if sec.Commentary != "" {
			fmt.Fprintf(&b, "(assistant's commentary) %s\n", sec.Commentary)
		}
		switch {
		case sec.Status != "ok":
			fmt.Fprintf(&b, "Unavailable: %s\n", sec.Message)
		case sec.Comparison != nil:
			b.WriteString(comparisonMarkdown(sec.Comparison))
		case sec.Data != nil:
			b.WriteString(gridMarkdown(sec.Data, 30))
		}
	}
	return b.String()
}

func gridMarkdown(res *reporting.GridResult, maxRows int) string {
	var b strings.Builder
	cols := res.Columns
	head := make([]string, len(cols))
	for i, c := range cols {
		head[i] = c.Label
	}
	b.WriteString("| " + strings.Join(head, " | ") + " |\n|" + strings.Repeat(" --- |", len(cols)) + "\n")
	for i, row := range res.Rows {
		if i == maxRows {
			fmt.Fprintf(&b, "| … %d more rows |\n", len(res.Rows)-maxRows)
			break
		}
		cells := make([]string, len(cols))
		for j, c := range cols {
			cells[j] = cell(row[c.ID])
		}
		b.WriteString("| " + strings.Join(cells, " | ") + " |\n")
	}
	return b.String()
}

func comparisonMarkdown(c *reporting.Comparison) string {
	var b strings.Builder
	fmt.Fprintf(&b, "| | %s | %s | difference | change %% |\n| --- | --- | --- | --- | --- |\n", c.Current.Metric.Name, c.Baseline.Metric.Name)
	for _, r := range c.Rows {
		d, p := "—", "—"
		if r.Difference != nil {
			d = fmt.Sprintf("%.2f", *r.Difference)
		}
		if r.Percent != nil {
			p = fmt.Sprintf("%.1f%%", *r.Percent)
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", r.Label, cell(r.Current), cell(r.Baseline), d, p)
	}
	return b.String()
}

func cell(v any) string {
	switch x := v.(type) {
	case reporting.Value:
		if x.Value == nil {
			return "(" + strings.ReplaceAll(x.State, "_", " ") + ")"
		}
		return fmt.Sprintf("%.2f", *x.Value)
	case nil:
		return ""
	}
	return strings.ReplaceAll(fmt.Sprint(v), "|", "/")
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

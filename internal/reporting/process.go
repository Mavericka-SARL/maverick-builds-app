package reporting

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// The process around the grids: dashboards and workflows, read so that the
// assistant can explain how a model is meant to be used — what a page says,
// where it leads, which grid each widget shows, which steps a submission
// goes through and who acts on each.
//
// A dashboard is read as a page: its text widgets' Markdown, its links to
// other dashboards resolved so they can be followed, and what each widget
// shows (grid, metrics, axis, fixed filters). Never a widget's data: numbers
// come from the grid reads, one path with one set of checks (owner decision,
// 2026-10-10). What a widget names is checked against the person's own view
// of the grid it reads, so a metric or member hidden from them is left out,
// as the grid reads leave it out; a widget on a grid they cannot read says
// only that. A workflow is read with its steps as the route gives them:
// names, instructions, the roles that act, deadlines and order — never a
// condition or a named person.

// DashboardRef is a dashboard the person may open.
type DashboardRef struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Folder string   `json:"folder,omitempty"`
	Tags   []string `json:"tags"`
}

// ListDashboards lists p's dashboards the person may open, with their
// folder paths, optionally narrowed to those whose name, folder or tags
// contain search.
func (s *Session) ListDashboards(ctx context.Context, p Pinned, search string) ([]DashboardRef, error) {
	var list []struct {
		ID       string   `json:"id"`
		Name     string   `json:"name"`
		Tags     []string `json:"tags"`
		FolderID *string  `json:"folder_id"`
	}
	if _, err := s.get(ctx, "dashboards", p.request("/api/dashboards", nil), &list); err != nil {
		return nil, err
	}
	var folders []struct {
		ID       string  `json:"id"`
		Name     string  `json:"name"`
		ParentID *string `json:"parent_id"`
	}
	if _, err := s.get(ctx, "folders", p.request("/api/folders", nil), &folders); err != nil {
		return nil, err
	}
	byID := map[string]int{}
	for i, f := range folders {
		byID[f.ID] = i
	}
	path := func(id *string) string {
		var parts []string
		for seen := 0; id != nil && seen < len(folders); seen++ {
			i, ok := byID[*id]
			if !ok {
				break
			}
			parts = append([]string{folders[i].Name}, parts...)
			id = folders[i].ParentID
		}
		return strings.Join(parts, " / ")
	}
	out := []DashboardRef{}
	for _, d := range list {
		ref := DashboardRef{ID: d.ID, Name: d.Name, Folder: path(d.FolderID), Tags: d.Tags}
		if ref.Tags == nil {
			ref.Tags = []string{}
		}
		if matches(search, append([]string{ref.Name, ref.Folder}, ref.Tags...)...) {
			out = append(out, ref)
		}
	}
	return out, nil
}

// DashboardPage is describe_dashboard: a dashboard as a page.
type DashboardPage struct {
	Context   Pinned       `json:"context"`
	Dashboard DashboardRef `json:"dashboard"`
	// Widgets are in reading order: top to bottom, left to right.
	Widgets []WidgetInfo `json:"widgets"`
	Notes   []string     `json:"notes"`
}

// WidgetInfo is one widget: what it says or shows, never its data.
type WidgetInfo struct {
	// Type is text, grid, chart, kpi, form, button, image, import, or the
	// widget's own type when it is none of those.
	Type  string `json:"type"`
	Title string `json:"title,omitempty"`
	// Text is a text widget's Markdown.
	Text  string          `json:"text,omitempty"`
	Links []DashboardLink `json:"links,omitempty"`
	// Grid is the grid a grid, chart, KPI or import widget reads; query it
	// with query_grid for the numbers.
	Grid      *Ref     `json:"grid,omitempty"`
	ChartType string   `json:"chart_type,omitempty"`
	Metrics   []Ref    `json:"metrics,omitempty"`
	Dimension *Ref     `json:"dimension,omitempty"`
	Filters   []Filter `json:"filters,omitempty"`
	// Action says in words what a button, form or import widget does when
	// used in the console; the connector never uses it.
	Action string `json:"action,omitempty"`
	Note   string `json:"note,omitempty"`
}

// DashboardLink is a link in a text widget: to another dashboard, resolved
// against what the person may open, or to a web address.
type DashboardLink struct {
	Label string `json:"label"`
	// Href is a web address; a dashboard link has the fields below.
	Href          string `json:"href,omitempty"`
	ApplicationID string `json:"application_id,omitempty"`
	ModelID       string `json:"model_id,omitempty"`
	ModelName     string `json:"model_name,omitempty"`
	DashboardID   string `json:"dashboard_id,omitempty"`
	DashboardName string `json:"dashboard_name,omitempty"`
	// Note says why a dashboard link leads nowhere for the person.
	Note string `json:"note,omitempty"`
}

// dashboardWidget is the part of a widget /api/dashboards/{id} gives that
// is read here.
type dashboardWidget struct {
	WidgetType  string          `json:"widget_type"`
	RefID       *string         `json:"ref_id"`
	Content     *string         `json:"content"`
	Title       *string         `json:"title"`
	WidgetProps json.RawMessage `json:"widget_props"`
	PosX        int             `json:"pos_x"`
	PosY        int             `json:"pos_y"`
}

type widgetProps struct {
	Alt         string              `json:"alt"`
	MetricIDs   []string            `json:"metric_ids"`
	ConfirmText string              `json:"confirm_text"`
	ShowMembers map[string][]string `json:"show_members"`
	Chart       *struct {
		ChartType       string            `json:"chart_type"`
		DimensionID     string            `json:"dimension_id"`
		MetricIDs       []string          `json:"metric_ids"`
		XMetricID       string            `json:"x_metric_id"`
		YMetricID       string            `json:"y_metric_id"`
		ContextDefaults map[string]string `json:"context_defaults"`
	} `json:"chart"`
	KPIScope *struct {
		DimensionID string `json:"dimension_id"`
		MemberCode  string `json:"member_code"`
	} `json:"kpi_scope"`
}

// DescribeDashboard reads dashboardID of p as a page.
func (s *Session) DescribeDashboard(ctx context.Context, p Pinned, dashboardID string) (*DashboardPage, error) {
	// Only a dashboard the person is listed in p — its model, its active
	// revision, their roles — is read.
	list, err := s.ListDashboards(ctx, p, "")
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(list, func(d DashboardRef) bool { return d.ID == dashboardID })
	if i < 0 {
		return nil, notFound("dashboard")
	}
	var def struct {
		Widgets []dashboardWidget `json:"widgets"`
	}
	if _, err := s.get(ctx, "dashboard", p.request("/api/dashboards/"+url.PathEscape(dashboardID), nil), &def); err != nil {
		return nil, err
	}
	slices.SortStableFunc(def.Widgets, func(a, b dashboardWidget) int {
		if a.PosY != b.PosY {
			return a.PosY - b.PosY
		}
		return a.PosX - b.PosX
	})

	page := &DashboardPage{Context: p, Dashboard: list[i], Widgets: []WidgetInfo{}, Notes: []string{
		"Widgets show no numbers here: ask query_grid (or compare_grid, render_chart) for a widget's grid with its metrics and filters.",
		"Links name other dashboards; describe_dashboard follows one with its model_id and dashboard_id (and that model's application_id).",
	}}
	views := &gridViews{s: s, p: p}
	links := &linkResolver{s: s, p: p}
	str := func(v *string) string {
		if v == nil {
			return ""
		}
		return *v
	}
	for _, w := range def.Widgets {
		var props widgetProps
		if len(w.WidgetProps) > 0 {
			_ = json.Unmarshal(w.WidgetProps, &props)
		}
		info := WidgetInfo{Type: w.WidgetType, Title: str(w.Title)}
		ref := str(w.RefID)
		switch w.WidgetType {
		case "text":
			info.Text = str(w.Content)
			info.Links, err = links.resolve(ctx, info.Text)
			if err != nil {
				return nil, err
			}
		case "image":
			// The picture is inline data: only what it is said to show.
			info.Title = firstNonEmpty(props.Alt, info.Title)
		case "grid", "import":
			g, ok, err := views.get(ctx, ref)
			if err != nil {
				return nil, err
			}
			if !ok {
				info.Note = "reads a grid you cannot read"
				break
			}
			info.Grid = &Ref{ID: ref, Name: g.summary.Name}
			if w.WidgetType == "import" {
				info.Action = "uploads a spreadsheet into this grid"
				break
			}
			if len(props.MetricIDs) > 0 {
				info.Metrics = g.metricRefs(props.MetricIDs)
			} else {
				info.Note = "shows every metric of the grid you can see"
			}
		case "chart":
			g, ok, err := views.get(ctx, ref)
			if err != nil {
				return nil, err
			}
			if !ok || props.Chart == nil {
				info.Note = "reads a grid you cannot read"
				break
			}
			info.Grid = &Ref{ID: ref, Name: g.summary.Name}
			c := props.Chart
			info.ChartType = c.ChartType
			ids := append(slices.Clone(c.MetricIDs), c.XMetricID, c.YMetricID)
			info.Metrics = g.metricRefs(ids)
			if d, ok := g.dimension(c.DimensionID); ok {
				info.Dimension = &Ref{ID: c.DimensionID, Name: g.meta.Dimensions[d].Name}
			}
			info.Filters = g.filters(c.ContextDefaults)
		case "metric_kpi":
			info.Type = "kpi"
			g, gridID, err := views.ofMetric(ctx, ref)
			if err != nil {
				return nil, err
			}
			if g == nil {
				info.Note = "shows a metric you cannot read"
				break
			}
			info.Grid = &Ref{ID: gridID, Name: g.summary.Name}
			info.Metrics = g.metricRefs([]string{ref})
			if props.KPIScope != nil {
				info.Filters = g.filters(map[string]string{props.KPIScope.DimensionID: props.KPIScope.MemberCode})
			}
		case "form":
			info.Action = "a form people fill in on this dashboard"
		case "automation_button":
			info.Type = "button"
			info.Title = firstNonEmpty(str(w.Content), info.Title)
			info.Action = "runs an automation when clicked"
			if props.ConfirmText != "" {
				info.Action += `, after asking "` + props.ConfirmText + `"`
			}
		case "integration_button":
			info.Type = "button"
			info.Title = firstNonEmpty(str(w.Content), info.Title)
			info.Action = "runs a data import when clicked"
		}
		page.Widgets = append(page.Widgets, info)
	}
	return page, nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// gridViews loads the person's view of each grid a dashboard reads, once.
type gridViews struct {
	s      *Session
	p      Pinned
	grids  []GridSummary
	listed bool
	loaded map[string]*grid
}

// list reads the model's readable grids, once.
func (v *gridViews) list(ctx context.Context) error {
	if v.listed {
		return nil
	}
	grids, err := v.s.ListGrids(ctx, v.p, "")
	if err != nil {
		return err
	}
	v.grids, v.listed, v.loaded = grids, true, map[string]*grid{}
	return nil
}

// get is the person's view of gridID; ok is false when they cannot read it.
func (v *gridViews) get(ctx context.Context, gridID string) (*grid, bool, error) {
	if err := v.list(ctx); err != nil {
		return nil, false, err
	}
	if g, ok := v.loaded[gridID]; ok {
		return g, g != nil, nil
	}
	i := slices.IndexFunc(v.grids, func(g GridSummary) bool { return g.ID == gridID })
	if i < 0 || gridID == "" {
		v.loaded[gridID] = nil
		return nil, false, nil
	}
	g := &grid{summary: v.grids[i]}
	q := url.Values{"grid_def_id": {gridID}, "meta_only": {"1"}, "revision_id": {v.p.RevisionID}}
	if _, err := v.s.get(ctx, "grid", v.p.request("/api/grid", q), &g.meta); err != nil {
		var re *Error
		if errors.As(err, &re) && re.Code == CodeNotFound {
			v.loaded[gridID] = nil
			return nil, false, nil
		}
		return nil, false, err
	}
	if g.meta.RevisionID != v.p.RevisionID {
		v.loaded[gridID] = nil
		return nil, false, nil
	}
	v.loaded[gridID] = g
	return g, true, nil
}

// ofMetric is a readable grid holding metricID visibly, if any.
func (v *gridViews) ofMetric(ctx context.Context, metricID string) (*grid, string, error) {
	if err := v.list(ctx); err != nil {
		return nil, "", err
	}
	for _, sum := range v.grids {
		g, ok, err := v.get(ctx, sum.ID)
		if err != nil {
			return nil, "", err
		}
		if ok && slices.ContainsFunc(g.meta.Metrics, func(m gridMetric) bool { return m.ID == metricID }) {
			return g, sum.ID, nil
		}
	}
	return nil, "", nil
}

// metricRefs keeps the ids of metrics the person sees in g, named.
func (g *grid) metricRefs(ids []string) []Ref {
	out := []Ref{}
	for _, id := range ids {
		for _, m := range g.meta.Metrics {
			if m.ID == id && id != "" && !slices.ContainsFunc(out, func(r Ref) bool { return r.ID == id }) {
				out = append(out, Ref{ID: id, Name: firstNonEmpty(m.Label, m.Name)})
			}
		}
	}
	return out
}

// filters keeps the fixed members (dimension id → code) the person sees in
// g, named; one they cannot see is left out.
func (g *grid) filters(byDim map[string]string) []Filter {
	out := []Filter{}
	for dimID, code := range byDim {
		d, ok := g.dimension(dimID)
		if !ok {
			continue
		}
		dim := g.meta.Dimensions[d]
		for _, m := range dim.Members {
			if m.Code == code {
				out = append(out, Filter{DimensionID: dimID, DimensionName: dim.Name, MemberCode: code, MemberLabel: firstNonEmpty(m.Label, m.Code)})
			}
		}
	}
	slices.SortFunc(out, func(a, b Filter) int { return strings.Compare(a.DimensionName, b.DimensionName) })
	return out
}

// markdownLink is a Markdown link: [label](target). A dashboard link's
// target holds spaces ("dashboard:2 · The model you work in").
var markdownLink = regexp.MustCompile(`\[([^\]]*)\]\(([^)]+)\)`)

// dashboardScheme starts a link to a dashboard, by name (the console's
// dashboardLinks.ts): dashboard:Name, dashboard:Model/Name, dashboard:Model/.
const dashboardScheme = "dashboard:"

// linkResolver resolves a text widget's dashboard links against the models
// and dashboards the person may open — as following one in the console
// does, by name.
type linkResolver struct {
	s      *Session
	p      Pinned
	models []ModelRef
	lists  map[string][]DashboardRef // model id → its dashboards
}

func (l *linkResolver) resolve(ctx context.Context, text string) ([]DashboardLink, error) {
	var out []DashboardLink
	for _, m := range markdownLink.FindAllStringSubmatch(text, -1) {
		label, href := m[1], m[2]
		body, ok := strings.CutPrefix(href, dashboardScheme)
		if !ok {
			if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
				out = append(out, DashboardLink{Label: label, Href: href})
			}
			continue
		}
		modelName, dashName := "", body
		if slash := strings.Index(body, "/"); slash >= 0 {
			modelName, dashName = body[:slash], body[slash+1:]
		}
		modelName, dashName = decodeLinkPart(modelName), decodeLinkPart(dashName)
		link := DashboardLink{Label: label}
		target, err := l.model(ctx, modelName)
		if err != nil {
			return nil, err
		}
		if target == nil {
			link.ModelName, link.DashboardName = modelName, dashName
			link.Note = "the linked model is not one you can open, or was renamed"
			out = append(out, link)
			continue
		}
		link.ApplicationID, link.ModelID, link.ModelName = target.ApplicationID, target.ModelID, target.ModelName
		dashboards, err := l.dashboards(ctx, *target)
		if err != nil {
			return nil, err
		}
		var found *DashboardRef
		for i := range dashboards {
			if dashName == "" || dashboards[i].Name == dashName {
				found = &dashboards[i]
				break
			}
		}
		if found == nil {
			link.DashboardName = dashName
			link.Note = "the linked dashboard is not one you can open, or was renamed"
		} else {
			link.DashboardID, link.DashboardName = found.ID, found.Name
		}
		out = append(out, link)
	}
	return out, nil
}

// model is the model a link names: the pinned one when the name is empty,
// else one the person opens by that name — in the pinned application first,
// then its tenant, then anywhere.
func (l *linkResolver) model(ctx context.Context, name string) (*ModelRef, error) {
	if l.models == nil {
		models, err := l.s.ListModels(ctx, "")
		if err != nil {
			return nil, err
		}
		l.models = models
	}
	var pinned *ModelRef
	for i := range l.models {
		if l.models[i].ModelID == l.p.ModelID && l.models[i].ApplicationID == l.p.ApplicationID {
			pinned = &l.models[i]
		}
	}
	if name == "" {
		return pinned, nil
	}
	rank := func(m ModelRef) int {
		switch {
		case m.ApplicationID == l.p.ApplicationID:
			return 0
		case pinned != nil && m.TenantID == pinned.TenantID:
			return 1
		}
		return 2
	}
	var best *ModelRef
	for i := range l.models {
		if l.models[i].ModelName == name && (best == nil || rank(l.models[i]) < rank(*best)) {
			best = &l.models[i]
		}
	}
	return best, nil
}

func (l *linkResolver) dashboards(ctx context.Context, m ModelRef) ([]DashboardRef, error) {
	if list, ok := l.lists[m.ModelID]; ok {
		return list, nil
	}
	if l.lists == nil {
		l.lists = map[string][]DashboardRef{}
	}
	p, err := l.s.Pin(ctx, m.ApplicationID, m.ModelID, "")
	if err != nil {
		return nil, err
	}
	list, err := l.s.ListDashboards(ctx, p, "")
	if err != nil {
		return nil, err
	}
	l.lists[m.ModelID] = list
	return list, nil
}

// decodeLinkPart undoes the percent-encoding a dashboard link's name parts
// carry for "/", ")" and "%".
func decodeLinkPart(s string) string {
	if d, err := url.PathUnescape(s); err == nil {
		return d
	}
	return s
}

// ── workflows ───────────────────────────────────────────────────────────────

// WorkflowRef is a published workflow of the model.
type WorkflowRef struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Description  string `json:"description"`
	TriggerEvent string `json:"trigger_event"`
}

// ListWorkflows lists p's published workflows, optionally narrowed to
// those whose name or description contains search.
func (s *Session) ListWorkflows(ctx context.Context, p Pinned, search string) ([]WorkflowRef, error) {
	var list []WorkflowRef
	if _, err := s.get(ctx, "workflows", p.request("/api/workflow/definitions", nil), &list); err != nil {
		return nil, err
	}
	out := []WorkflowRef{}
	for _, w := range list {
		if matches(search, w.Name, w.Description) {
			out = append(out, w)
		}
	}
	return out, nil
}

// WorkflowDescription is describe_workflow.
type WorkflowDescription struct {
	Context  Pinned          `json:"context"`
	Workflow WorkflowRef     `json:"workflow"`
	Start    json.RawMessage `json:"start_fields"`
	Steps    json.RawMessage `json:"steps"`
	Notes    []string        `json:"notes"`
}

// DescribeWorkflow reads one published workflow of p with its steps.
func (s *Session) DescribeWorkflow(ctx context.Context, p Pinned, workflowID string) (*WorkflowDescription, error) {
	var def struct {
		WorkflowRef
		ContextSchema json.RawMessage `json:"context_schema"`
		Steps         json.RawMessage `json:"steps"`
	}
	if _, err := s.get(ctx, "workflow", p.request("/api/workflow/definitions/"+url.PathEscape(workflowID), nil), &def); err != nil {
		return nil, err
	}
	return &WorkflowDescription{Context: p, Workflow: def.WorkflowRef, Start: def.ContextSchema, Steps: def.Steps, Notes: []string{
		"Steps are in the order the designer listed them; routes and next_step_ids say which step follows (by step id), per decision where a step has routes.",
		"assignee_roles are role names: a business role, or a platform role such as business_admin. Conditions and notification recipients are not shown.",
		"This connection describes workflows; it never starts, approves or submits one.",
	}}, nil
}

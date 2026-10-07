// Package starter is the model a self-service tenant starts with: a guided
// tour of the platform, taught on a deliberately tiny example — two teams,
// four quarters, three metrics — small enough to hold in your head and real
// enough that the grid, the chart and the KPI on the tour's own dashboards
// are the working screens, not pictures of them.
//
// It is expressed as a modeltransfer.Package and created through
// modeltransfer.Import — the same path a tenant admin's package import
// takes — so the starter exercises code every customer relies on and needs
// no fixture file to regenerate when the schema moves.
//
// The prose lives in text widgets, which read a small Markdown subset
// (web/src/ui/RichText.tsx), and the diagrams in image widgets, which carry
// their picture inline (internal/imagedata). Both are ordinary widgets: a
// tenant can edit, move or delete any of it, and can build the same thing
// for its own people. Nothing here is a special "tutorial" mode.
package starter

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/internal/modeltransfer"
)

// Names of what the package creates, for callers that want to find them.
const (
	ModelName     = "Learn the platform"
	RevisionName  = "First revision"
	GridName      = "Team cost by quarter"
	DashboardName = "1 · Start here"
)

// Where the tour and the guides send a reader for the whole story. The two
// manuals ship inside the console on every deployment (web/Dockerfile copies
// them from docs/), so these paths answer wherever the console runs; the
// rest of the documentation is read in the public repository, where GitHub
// renders Markdown.
const (
	formulasManual  = "/docs/formulas-manual/manual.html"
	developerManual = "/docs/developer-manual/manual.html"
	publicDocs      = "https://github.com/Mavericka-SARL/maverick-builds-app/blob/main/docs/"
)

// dashLink is a text widget's link to a dashboard of another model
// (web/src/consoles/business/dashboardLinks.ts). It names the model and the
// dashboard, never their ids — sign-up's import allocates those, and each
// starter is imported on its own — so it resolves in every tenant that
// holds both. A link within the same model is written in place as
// [label](dashboard:Dashboard name).
func dashLink(label, model, dashboard string) string {
	esc := strings.NewReplacer("%", "%25", "/", "%2F", ")", "%29")
	return "[" + label + "](dashboard:" + esc.Replace(model) + "/" + esc.Replace(dashboard) + ")"
}

// The first page of each starter, where a link to it lands.
const (
	devFirstPage = "1 · Your first model"
	baFirstPage  = "1 · Your part"
	taFirstPage  = "1 · What you own"
)

// Starter is one starter package and the key that names it for good.
// internal/startersync records, per tenant, which model holds each key and
// the content it was last brought to; the key is what survives a rename of
// the model, so it never changes.
type Starter struct {
	Key     string
	Package modeltransfer.Package
}

// TourKey is the tour's key.
const TourKey = "tour"

// DeveloperGuideKey is the developer guide's key.
const DeveloperGuideKey = "developer_guide"

// LandingKey names the starter a new tenant lands on: sign-up makes it the
// application's business default, which every console screen opens until
// someone picks another model, and its first dashboard is the page they see.
// The tour, whose first page sends each reader on to the guide for what
// they came to do (2026-10-07; migration 124 moved existing tenants back).
// From 2026-10-06 to then it was the developer guide (migration 122).
const LandingKey = TourKey

// Starters is everything sign-up creates, in order: the tour, then one guide
// for each role that builds or runs the workspace. Which of them a tenant
// lands on is LandingKey's, not the order's. Workspaces that signed up before
// a starter existed, or before its content last changed, are brought up to
// date by internal/startersync.
func Starters() []Starter {
	return []Starter{
		{Key: TourKey, Package: Package()},
		{Key: DeveloperGuideKey, Package: DeveloperGuide()},
		{Key: "business_admin_guide", Package: BusinessAdminGuide()},
		{Key: "tenant_admin_guide", Package: TenantAdminGuide()},
	}
}

// Packages is Starters without their keys.
func Packages() []modeltransfer.Package {
	var out []modeltransfer.Package
	for _, s := range Starters() {
		out = append(out, s.Package)
	}
	return out
}

// Placeholder ids: Import allocates real ones and remaps every reference.
const (
	dimTeam     = "dim-team"
	dimQuarter  = "dim-quarter"
	metHeadcnt  = "m-headcount"
	metPerHead  = "m-cost-per-head"
	metCost     = "m-cost"
	gridMain    = "grid-main"
	dashStart   = "dash-1-start"
	dashNumbers = "dash-2-numbers"
	dashViews   = "dash-3-views"
	dashBeyond  = "dash-4-beyond"
)

// The example: two teams, four quarters. Headcount grows a little; cost per
// head differs by team. cost is calculated from both, so editing either
// input visibly moves it.
var teams = []struct {
	code, label string
	headcount   [4]float64
	perHead     float64
}{
	{"SALES", "Sales", [4]float64{4, 4, 5, 5}, 12000},
	{"ENG", "Engineering", [4]float64{6, 6, 7, 8}, 15000},
}

// The quarters are the dated periods of a time dimension, so the year above
// them can total each metric the way it should over time: cost adds up,
// headcount is the year-end figure, cost per head the mean.
var quarters = []struct{ code, start, end string }{
	{"Q1", "2026-01-01", "2026-03-31"},
	{"Q2", "2026-04-01", "2026-06-30"},
	{"Q3", "2026-07-01", "2026-09-30"},
	{"Q4", "2026-10-01", "2026-12-31"},
}

// tick is a Markdown code fence inside a raw string, which cannot hold one.
const tick = "`"

func str(s string) *string { return &s }
func num(n int) *int       { return &n }

// pageWidth is the tour's reading measure: wide enough for a diagram,
// narrow enough that a line of prose is comfortable and that nothing is cut
// off in the console's content area on an ordinary laptop.
const pageWidth = 900

// kpiHeight is every guide's metric_kpi height: a KPI's height is a minimum, and the titled tile renders at 161 px.
const kpiHeight = 164

// text builds a prose widget. Markdown: headings, **bold**, lists, links.
func text(body string, y, h int, sortOrder int) modeltransfer.Widget {
	return modeltransfer.Widget{
		WidgetType: "text", Content: str(body), SortOrder: sortOrder,
		ColStart: 1, ColSpan: 12, PosX: num(0), PosY: num(y), SizeW: num(pageWidth), SizeH: num(h),
		Props: json.RawMessage(`{"background":"none"}`),
	}
}

// picture builds an image widget from SVG source.
func picture(svg, alt string, y, w, h int, sortOrder int) modeltransfer.Widget {
	props, _ := json.Marshal(map[string]any{"alt": alt, "image_fit": "contain", "background": "none"})
	return modeltransfer.Widget{
		WidgetType: "image", Content: str(dataURL(svg)), SortOrder: sortOrder,
		ColStart: 1, ColSpan: 12, PosX: num((pageWidth - w) / 2), PosY: num(y), SizeW: num(w), SizeH: num(h),
		Props: json.RawMessage(props),
	}
}

// tourPage stacks a page's widgets top-down, 24 px apart, numbering them in
// reading order, so a height changed in one place moves everything below it.
type tourPage struct {
	y  int
	ws []modeltransfer.Widget
}

const tourGap = 24

func (p *tourPage) text(body string, h int) {
	p.ws = append(p.ws, text(body, p.y, h, len(p.ws)))
	p.y += h + tourGap
}

func (p *tourPage) picture(svg, alt string, w, h int) {
	p.ws = append(p.ws, picture(svg, alt, p.y, w, h, len(p.ws)))
	p.y += h + tourGap
}

// row places widgets side by side at the current height; the page moves on
// below the tallest.
func (p *tourPage) row(ws ...modeltransfer.Widget) {
	tallest := 0
	for _, w := range ws {
		w.PosY, w.SortOrder = num(p.y), len(p.ws)
		p.ws = append(p.ws, w)
		if w.SizeH != nil && *w.SizeH > tallest {
			tallest = *w.SizeH
		}
	}
	p.y += tallest + tourGap
}

// Package builds the tour. Deterministic: the same package every time, so a
// test can count what it contains.
func Package() modeltransfer.Package {
	team := modeltransfer.Dimension{ID: dimTeam, Name: "team", AggRule: "sum"}
	team.Members = append(team.Members, modeltransfer.Member{ID: "t-all", Code: "COMPANY", Label: "Whole company", SortOrder: 0})
	for i, t := range teams {
		team.Members = append(team.Members, modeltransfer.Member{ID: "t-" + t.code, Code: t.code, Label: t.label, ParentMemberID: str("t-all"), SortOrder: i + 1})
	}

	// Two levels here too: the year above its quarters, so the tour can show
	// that a total is the platform's answer, not a row someone typed. quarter
	// is a time dimension — declared, not guessed from its name — so the year
	// is an aggregate period and each metric's time summary decides it.
	quarter := modeltransfer.Dimension{
		ID: dimQuarter, Name: "quarter", AggRule: "sum",
		DimensionType: "time", TimeGranularity: str("quarter"), FiscalYearStart: num(1),
	}
	quarter.Members = append(quarter.Members, modeltransfer.Member{ID: "q-year", Code: "FY2026", Label: "FY 2026", SortOrder: 0})
	for i, q := range quarters {
		quarter.Members = append(quarter.Members, modeltransfer.Member{
			ID: "q-" + q.code, Code: q.code, Label: q.code + " 2026", ParentMemberID: str("q-year"), SortOrder: i + 1,
			PeriodStart: str(q.start), PeriodEnd: str(q.end), TimeIndex: num(i),
		})
	}

	metrics := []modeltransfer.Metric{
		// People are counted at a point in time: the year shows its last quarter.
		{ID: metHeadcnt, Name: "headcount", IsInput: true, StorageType: "oltp", AggRule: "sum", Format: "number", TimeSummary: "last"},
		{ID: metPerHead, Name: "cost_per_head", IsInput: true, StorageType: "oltp", AggRule: "average", Format: "currency", FormatCurrency: "$", TimeSummary: "average"},
		{ID: metCost, Name: "cost", Formula: str("=headcount*cost_per_head"), StorageType: "oltp", AggRule: "sum", Format: "currency", FormatCurrency: "$", TimeSummary: "sum"},
	}
	deps := []modeltransfer.Dependency{
		{MetricID: metCost, DependsOn: metHeadcnt},
		{MetricID: metCost, DependsOn: metPerHead},
	}

	grid := modeltransfer.Grid{
		ID: gridMain, Name: GridName,
		Metrics:    []modeltransfer.GridMetric{{MetricID: metHeadcnt, SortOrder: 0}, {MetricID: metPerHead, SortOrder: 1}, {MetricID: metCost, SortOrder: 2}},
		Dimensions: []modeltransfer.GridDimension{{DimensionID: dimTeam}, {DimensionID: dimQuarter}},
	}

	return modeltransfer.Package{
		Format: modeltransfer.PackageFormat, Version: modeltransfer.PackageVersion, ExportedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ModelName: ModelName, StorageType: "oltp", RevisionName: RevisionName, IncludeData: true,
		Dimensions: []modeltransfer.Dimension{team, quarter}, Metrics: metrics, Dependencies: deps,
		Grids: []modeltransfer.Grid{grid},
		Dashboards: []modeltransfer.Dashboard{
			startHere(), whereNumbersLive(), howPeopleLook(), beyondTheNumbers(),
		},
		Facts: facts(),
	}
}

func facts() []modeltransfer.Fact {
	var out []modeltransfer.Fact
	for _, t := range teams {
		for i, q := range quarters {
			members, _ := json.Marshal(map[string]string{dimTeam: t.code, dimQuarter: q.code})
			out = append(out,
				modeltransfer.Fact{MetricID: metHeadcnt, DimMembers: members, Value: t.headcount[i]},
				modeltransfer.Fact{MetricID: metPerHead, DimMembers: members, Value: t.perHead},
			)
		}
	}
	return out
}

// tourProps marshals a widget's props; the tour's are static, so an error is a
// programming mistake the tests catch as missing props.
func tourProps(v map[string]any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func tourDashboard(id, name string, p *tourPage) modeltransfer.Dashboard {
	return modeltransfer.Dashboard{ID: id, Name: name, Tags: []string{"guide"}, Category: "Getting started", Widgets: p.ws}
}

func startHere() modeltransfer.Dashboard {
	p := &tourPage{}
	p.text(`# Welcome — this is your own workspace

Everything you see here is **yours to change**: these pages, the numbers behind them, and the model they come from. Nothing is a locked demo, and nothing here is a special tutorial mode — the tour is built from the same dashboards, text and pictures you can build yourself.

Four pages, in order: pick each one from the tabs at the top. Read them, then delete them and build your own.`, 144)

	p.text(`## Where to go from here

Pick what you came to do. Each guide is a model of its own, next to this one; its link opens its first page.

- **See how it all fits** — read on here: four short pages, starting with [2 · Where numbers live](dashboard:2 · Where numbers live).
- **Build a model** — the `+dashLink("Developer guide", DeveloperGuideName, devFirstPage)+` goes from nothing to a model people type numbers into, in eight steps.
- **Decide who sees what, and keep requests moving** — the `+dashLink("Business admin guide", BusinessAdminGuideName, baFirstPage)+`.
- **Invite people and look after the workspace** — the `+dashLink("Tenant admin guide", TenantAdminGuideName, taFirstPage)+`.

Every guide links back here. You can also come back by picking *Learn the platform* in the **Model** list at the top of this page.`, 229)

	p.text(`## A model starts with four things

You describe **what you slice by** and **what you measure**. Then you lay out grids to hold the numbers and dashboards to show them, and the platform keeps every total and calculation in step. Forms, workflows and triggers belong to the model too; page 4 covers them.`, 108)

	p.picture(buildFlow(), "Dimensions and metrics feed a grid, which feeds a dashboard", 880, 155)

	p.text(`## What you are looking at right now

A **dashboard**: a page of widgets you arrange yourself. This one holds text and pictures; the next one holds a real grid you can type into. You are reading it under **User › Dashboards**, where the people who use a model see its pages.

To build one, open **Developer › Dashboards**, press **New dashboard**, give it a name and press **Create**. Then press **Design** beside it: click a widget type in the palette — Grid, Chart, Text and the rest — choose what it shows and press **Add widget**. Drag and resize it on the canvas, and press **Save**.`, 188)

	p.text(`## The badge at the top names the revision you are in

A **revision** is a complete copy of the model: its dimensions, metrics, grids, dashboards, forms, workflows and the numbers entered so far. One revision is *live*: it is what everyone else sees.

The revision you are reading is the live one, so a change you make here shows at once. For anything bigger, press **New revision** under this model in **Developer › Models**, name it and press **Save**: you get a copy to work in, out of everyone's way. Change it as much as you like, then press **Set active** beside it when it is right.

Every change to a model belongs to a revision; people, roles and access rules sit outside revisions and apply to every one. A workflow that is already running keeps the definition it started with.`, 240)

	p.picture(revisions(), "A model has revisions; one of them is live, and New revision makes the next", 720, 180)

	p.text(`---
Next: [2 · Where numbers live](dashboard:2 · Where numbers live) — or pick its tab at the top of this page.`, 72)

	return tourDashboard(dashStart, DashboardName, p)
}

func whereNumbersLive() modeltransfer.Dashboard {
	p := &tourPage{}
	p.text(`# Dimensions, metrics, and one number

A **dimension** is a way of slicing: this example has *team* and *quarter*. A **metric** is a thing you measure: *headcount*, *cost per head*, *cost*.

Pick one member of every dimension and one metric, and you have addressed exactly one number.`, 124)

	number, numberH := oneNumber()
	p.picture(number, "One member of each dimension plus one metric addresses one value", oneNumberWidth, numberH)

	p.text(`## Two kinds of metric

- **Input** — someone types it. *headcount* and *cost per head* are inputs.
- **Calculated** — the platform works it out, every time an input changes. *cost* is `+tick+`=headcount * cost_per_head`+tick+`.

You cannot type over a calculated number: the formula is the single answer to how it is worked out. Formulas reach well beyond arithmetic — conditions, lookups, other members, time — and the [formulas manual](`+formulasManual+`) lists every function.`, 168)

	// The cards say what their titles say: the whole year's cost, and the
	// headcount at year end (Q4) — not the dashboard's first selection.
	p.row(
		modeltransfer.Widget{
			WidgetType: "metric_kpi", RefID: str(metCost), ColStart: 1, ColSpan: 6,
			PosX: num(0), SizeW: num(440), SizeH: num(kpiHeight),
			Title: str("Total cost, FY 2026"), ShowTitle: true,
			Props: tourProps(map[string]any{"kpi_context_mode": "total"}),
		},
		modeltransfer.Widget{
			WidgetType: "metric_kpi", RefID: str(metHeadcnt), ColStart: 7, ColSpan: 6,
			PosX: num(460), SizeW: num(440), SizeH: num(kpiHeight),
			Title: str("Headcount at year end"), ShowTitle: true,
			Props: tourProps(map[string]any{"kpi_context_mode": "pin", "kpi_scope": map[string]any{"dimension_id": dimQuarter, "member_code": "Q4"}}),
		},
	)

	p.text(`## Try it

The card on the left is the cost of the whole year; the one on the right is headcount at the end of Q4. The grid below is the real thing, with the whole company above its two teams.

Click one of the boxed cells in a *Headcount* row — the legend under the grid calls them *Editable* — type a different number and press Enter. *Cost* in the same column answers at once, and so do the *Whole company* rows, the *Total* column and the cost card. *Cost* is marked CALC: the platform works it out, and you cannot type there or in any total.`, 188)

	// Team on the rows, so Whole company is a visible total above its teams;
	// quarters across, so FY 2026 is a visible total column.
	p.row(modeltransfer.Widget{
		WidgetType: "grid", RefID: str(gridMain), ColStart: 1, ColSpan: 12,
		PosX: num(0), SizeW: num(pageWidth), SizeH: num(620),
		Title: str(GridName), ShowTitle: true,
		Props: tourProps(map[string]any{
			"sync_context": true,
			"default_view": map[string]any{"rows": []string{dimTeam, "__metrics__"}, "cols": []string{dimQuarter}, "context": []string{}},
		}),
	})

	p.text(`## Every total follows its metric's own rule

A total is what the platform makes of the numbers underneath it, and each metric says how. *Cost* adds up, over teams and over the year. *Cost per head* is averaged. *Headcount* adds up over teams, but the year shows its last quarter: people are counted at a point in time, not added up over it. That works because *quarter* is a **time dimension**: each metric has an **Aggregation rule** for its totals and a **Time summary** for its totals over time, both set under **Developer › Metrics**.

Grids of your own are built under **Developer › Grids**: press **New grid**, name it, press **Create**, then **Configure** it with metrics and dimensions. Where each dimension sits — row, column or context — is set on the grid widget in a dashboard's **Design**, under **Default view**.

---
Next: [3 · How people look at it](dashboard:3 · How people look at it).`, 256)

	return tourDashboard(dashNumbers, "2 · Where numbers live", p)
}

func howPeopleLook() modeltransfer.Dashboard {
	p := &tourPage{}
	p.text(`# Charts, cards and pages

The same numbers, shown the way each person needs them. The chart below is the example model again — no second copy of the data, no export step. Change a number on the previous page and this moves. It opens on the whole company; pick a team on its selector to see just that team.`, 136)

	// A chart draws from a grid: RefID is the grid, the props say which
	// metric over which dimension. The year is left out of the bars — it
	// would dwarf the quarters it is made of.
	p.row(modeltransfer.Widget{
		WidgetType: "chart", RefID: str(gridMain), ColStart: 1, ColSpan: 12,
		PosX: num(0), SizeW: num(pageWidth), SizeH: num(340),
		Title: str("Cost by quarter"), ShowTitle: true,
		Props: tourProps(map[string]any{
			"chart": map[string]any{
				"chart_type": "bar", "metric_ids": []string{metCost}, "dimension_id": dimQuarter,
				"hide_rollup_members": true, "context_defaults": map[string]string{dimTeam: "COMPANY"},
			},
			"sync_context": true,
		}),
	})

	p.text(`## What you can put on a page

- **Grid** — numbers to read and to type into
- **Chart** — bar, line, pie, scatter or histogram, drawn from a grid
- **Metric KPI** — one number, large
- **Form** — structured entry, filled in on the page
- **Trigger** — a button that starts a workflow, through a manual trigger made under **Developer › Triggers**
- **Integration** — a button that runs one of your integrations
- **Import** — an upload box that loads a spreadsheet into a grid, with a template to download
- **Text** and **Image** — the ones this tour is written in`, 272)

	p.text(`## One set of selectors per page

Widgets on a page share their selectors: pick a team or a quarter once, and every widget that follows the page answers for it. Clicking a bar, or a row or column label in a grid, picks that member too. A widget can instead keep its own selectors, show the whole-model total, or be pinned to one member — the two cards on page 2 do the last two. So one page serves everybody, without anyone building four copies of it.

To set this up, open **Developer › Dashboards**, press **Design** beside a dashboard and click a widget. The **Widget Properties** panel holds **Context sync** for grids and charts, a card's **Default context** and a grid's **Default view**.`, 208)

	p.text(`---
Next: [4 · Beyond the numbers](dashboard:4 · Beyond the numbers).`, 72)

	return tourDashboard(dashViews, "3 · How people look at it", p)
}

func beyondTheNumbers() modeltransfer.Dashboard {
	p := &tourPage{}
	p.text(`# Getting data in, and working with other people

Numbers rarely start in one place, and they are rarely agreed by one person. This page covers both, and where to go next.

## Bringing numbers in

Under **Developer › Integrations**:

- **Excel / CSV Import** — upload a file, map its columns, validate, commit
- **Google Sheets** — paste the link of a link-shared sheet. For a private sheet, first store a Google service account under *Private sheets* on the same page, then share the sheet with it
- **REST API** — connect a JSON API, map its fields, and run it on demand or on a schedule

Press **Save as Integration** while mapping a file or a sheet to keep the mapping; a saved sheet then re-syncs in one click with **Sync Now**.`, 292)

	p.text(`## Asking people for numbers

A **form** collects structured entries — an expense, a request, a headcount change. Forms are designed under **Developer › Forms**, and people — users and business admins alike — make entries through a **Form** widget placed on a dashboard. To turn entries into numbers, add a **Form Records** integration under **Developer › Integrations**: it posts each record into an input metric once the record reaches a status you choose, such as approved.

## Getting things agreed

A **workflow** moves something through the people who must see it: submit, review, approve, rework. Steps are assigned to *roles*, not to named people, so the chain keeps working when someone is away or leaves.

Workflows are built under **Developer › Workflows**, tried with **Test Run** and opened to people with **Publish**. A user starts one from their **Workflow Inbox**; a step waiting for you appears under **User › Workflow Inbox**. A **trigger**, made under **Developer › Triggers**, starts one for you: from a button on a dashboard, on a schedule, or each time a form entry is submitted or an integration run ends. A running workflow keeps the definition it started with, so changing the process never rewrites history.`, 340)

	p.text(`## Who sees what

Your account is this workspace's **tenant admin**, **developer** and **business admin**, which between them open every screen in this tour. Each person you add gets only the roles you give them.`, 108)

	p.picture(roles(), "The four roles and what each one does; your account holds three of them", 780, 206)

	p.text(`Invite people under **Tenant admin › Users** with **Invite user**, and give each a role. Everyone sees every dashboard until you put them in a business role: under **Business Admin › Roles**, a role's members see only the dashboards ticked under **Visible Dashboards**.

Access can be narrower still. Under **Business Admin › Access Rules**, pick a person and mark dimension members or metrics **Read** or **Hidden**; everything else stays **Write**. Hide the other teams and a person sees only their own team's rows, and a total built from hidden numbers is withheld from them.

## Building by describing it

**Developer › AI Developer** builds the same things you can build by hand. It needs an AI provider key: if none is set up for your workspace, add your own in its **Settings** (one key for the whole workspace, under **Tenant admin › AI keys**, needs the Enterprise edition). Describe what you want, review its action plan step by step and press **Confirm & Execute**. Its changes land in a separate AI draft revision until you press **Promote to Active**, and it has no more power than you do.`, 288)

	p.text(`## Three more guides

Next to this model sit three more, one for each role your account holds: the `+dashLink("Developer guide", DeveloperGuideName, devFirstPage)+`, the `+dashLink("Business admin guide", BusinessAdminGuideName, baFirstPage)+` and the `+dashLink("Tenant admin guide", TenantAdminGuideName, taFirstPage)+`. A link opens a guide's first page. You can also pick one in the **Model** list at the top of **User › Dashboards**, or press **Open** beside its name under **Business Admin › Models**.

A link like these is ordinary text-widget Markdown, `+tick+`[label](dashboard:Model name/Dashboard name)`+tick+`: your own dashboards can point at each other the same way.

For the whole story, the [developer manual](`+developerManual+`) covers the Developer screens and the [formulas manual](`+formulasManual+`) every function.

---

That is the tour. This model is yours: change it, or delete it and start on the real thing. To delete it, press the bin beside *Learn the platform* under **Tenant admin › Applications** — not the bin on the application above it, which removes every model, the guides included.`, 232)

	return tourDashboard(dashBeyond, "4 · Beyond the numbers", p)
}

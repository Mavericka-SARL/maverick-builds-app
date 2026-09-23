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

var quarters = []string{"Q1", "Q2", "Q3", "Q4"}

// tick is a Markdown code fence inside a raw string, which cannot hold one.
const tick = "`"

func str(s string) *string { return &s }
func num(n int) *int       { return &n }

// pageWidth is the tour's reading measure: wide enough for a diagram,
// narrow enough that a line of prose is comfortable and that nothing is cut
// off in the console's content area on an ordinary laptop.
const pageWidth = 900

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

// Package builds the tour. Deterministic: the same package every time, so a
// test can count what it contains.
func Package() modeltransfer.Package {
	team := modeltransfer.Dimension{ID: dimTeam, Name: "team", AggRule: "sum"}
	team.Members = append(team.Members, modeltransfer.Member{ID: "t-all", Code: "COMPANY", Label: "Whole company", SortOrder: 0})
	for i, t := range teams {
		team.Members = append(team.Members, modeltransfer.Member{ID: "t-" + t.code, Code: t.code, Label: t.label, ParentMemberID: str("t-all"), SortOrder: i + 1})
	}

	// Two levels here too: the year above its quarters, so the tour can show
	// that a total is the platform's answer, not a row someone typed.
	quarter := modeltransfer.Dimension{ID: dimQuarter, Name: "quarter", AggRule: "sum"}
	quarter.Members = append(quarter.Members, modeltransfer.Member{ID: "q-year", Code: "FY2026", Label: "FY 2026", SortOrder: 0})
	for i, q := range quarters {
		quarter.Members = append(quarter.Members, modeltransfer.Member{ID: "q-" + q, Code: q, Label: q + " 2026", ParentMemberID: str("q-year"), SortOrder: i + 1})
	}

	metrics := []modeltransfer.Metric{
		{ID: metHeadcnt, Name: "headcount", IsInput: true, StorageType: "oltp", AggRule: "sum", Format: "number"},
		{ID: metPerHead, Name: "cost_per_head", IsInput: true, StorageType: "oltp", AggRule: "average", Format: "currency", FormatCurrency: "$"},
		{ID: metCost, Name: "cost", Formula: str("=headcount*cost_per_head"), StorageType: "oltp", AggRule: "sum", Format: "currency", FormatCurrency: "$"},
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

	chartProps, _ := json.Marshal(map[string]any{
		"chart":        map[string]any{"chart_type": "bar", "metric_ids": []string{metCost}, "dimension_id": dimQuarter},
		"sync_context": true,
	})
	gridProps, _ := json.Marshal(map[string]any{"sync_context": true})

	return modeltransfer.Package{
		Format: modeltransfer.PackageFormat, Version: modeltransfer.PackageVersion, ExportedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ModelName: ModelName, StorageType: "oltp", RevisionName: RevisionName, IncludeData: true,
		Dimensions: []modeltransfer.Dimension{team, quarter}, Metrics: metrics, Dependencies: deps,
		Grids: []modeltransfer.Grid{grid},
		Dashboards: []modeltransfer.Dashboard{
			startHere(), whereNumbersLive(gridProps), howPeopleLook(chartProps), beyondTheNumbers(),
		},
		Facts: facts(),
	}
}

func facts() []modeltransfer.Fact {
	var out []modeltransfer.Fact
	for _, t := range teams {
		for i, q := range quarters {
			members, _ := json.Marshal(map[string]string{dimTeam: t.code, dimQuarter: q})
			out = append(out,
				modeltransfer.Fact{MetricID: metHeadcnt, DimMembers: members, Value: t.headcount[i]},
				modeltransfer.Fact{MetricID: metPerHead, DimMembers: members, Value: t.perHead},
			)
		}
	}
	return out
}

func startHere() modeltransfer.Dashboard {
	return modeltransfer.Dashboard{
		ID: dashStart, Name: DashboardName, Tags: []string{"tour"}, Category: "Getting started",
		Widgets: []modeltransfer.Widget{
			text(`# Welcome — this is your own workspace

Everything you see here is **yours to change**: these pages, the numbers behind them, and the model they come from. Nothing is a locked demo, and nothing here is a special tutorial mode — the tour is built from the same dashboards, text and pictures you can build yourself.

Four pages, in order. Read them, then delete them and build your own.`, 0, 165, 0),

			text(`## A model is four things

You describe **what you slice by** and **what you measure**. The platform gives you the places to put numbers and the ways to look at them.`, 189, 106, 1),

			picture(buildFlow(), "Dimensions and metrics feed a grid, which feeds a dashboard", 319, 880, 155, 2),

			text(`## What you are looking at right now

A **dashboard**: a page of widgets you arrange yourself. This one holds text and a picture; the next one holds a real grid you can type into.

To build one: open **Dashboards** in the sidebar, press **New**, then **Design** — drag widgets in from the palette, drop a grid or a chart onto the canvas, and save.`, 498, 161, 3),

			text(`## The badge at the top says which revision you are in

A **revision** is a complete copy of the model — its dimensions, metrics, grids and dashboards. One revision is *live*: it is what everyone else sees. You make the next one, change it as much as you like, and publish when it is right. Work in progress is never in anyone's way.

The unit of change is always a revision — every edit you make belongs to one, and a workflow that is already running keeps the revision it started with.`, 683, 183, 4),

			picture(revisions(), "A model has revisions; one of them is live", 890, 660, 170, 5),

			text(`---
Next: **2 · Where numbers live** — open it from **Dashboards** in the sidebar.`, 1084, 82, 6),
		},
	}
}

func whereNumbersLive(gridProps []byte) modeltransfer.Dashboard {
	return modeltransfer.Dashboard{
		ID: dashNumbers, Name: "2 · Where numbers live", Tags: []string{"tour"}, Category: "Getting started",
		Widgets: []modeltransfer.Widget{
			text(`# Dimensions, metrics, and one number

A **dimension** is a way of slicing: this example has *team* and *quarter*. A **metric** is a thing you measure: *headcount*, *cost per head*, *cost*.

Pick one member of every dimension and one metric, and you have addressed exactly one number.`, 0, 142, 0),

			picture(oneNumber(), "One member of each dimension plus one metric addresses one value", 166, 640, 230, 1),

			text(`## Two kinds of metric

- **Input** — someone types it. *headcount* and *cost per head* are inputs.
- **Calculated** — the platform works it out, every time an input changes. *cost* is `+tick+`=headcount * cost_per_head`+tick+`.

You never refresh anything, and you cannot type over a calculated number: the formula is the single answer to how it is worked out.`, 420, 142, 2),

			modeltransfer.Widget{
				WidgetType: "metric_kpi", RefID: str(metCost), SortOrder: 3, ColStart: 1, ColSpan: 6,
				PosX: num(0), PosY: num(586), SizeW: num(440), SizeH: num(120),
				Title: str("Total cost, FY 2026"), ShowTitle: true, Props: json.RawMessage(`{}`),
			},
			modeltransfer.Widget{
				WidgetType: "metric_kpi", RefID: str(metHeadcnt), SortOrder: 4, ColStart: 7, ColSpan: 6,
				PosX: num(460), PosY: num(586), SizeW: num(440), SizeH: num(120),
				Title: str("Headcount, FY 2026"), ShowTitle: true, Props: json.RawMessage(`{}`),
			},

			text(`## Try it

The grid below is the real thing. **Click a white cell under headcount and type a different number.** Watch *cost* — and the totals above — answer immediately. The grey cells are calculated; the platform owns those.

The rows named *Whole company* and *FY 2026* are not typed by anyone either: a total is what the platform makes of the numbers underneath it.`, 730, 161, 5),

			modeltransfer.Widget{
				WidgetType: "grid", RefID: str(gridMain), SortOrder: 6, ColStart: 1, ColSpan: 12,
				PosX: num(0), PosY: num(915), SizeW: num(pageWidth), SizeH: num(420),
				Title: str(GridName), ShowTitle: true, Props: json.RawMessage(gridProps),
			},

			text(`Grids of your own are built under **Grids** in the sidebar: choose the metrics, choose the dimensions, and that is the screen.

---
Next: **3 · How people look at it**.`, 1359, 104, 7),
		},
	}
}

func howPeopleLook(chartProps []byte) modeltransfer.Dashboard {
	return modeltransfer.Dashboard{
		ID: dashViews, Name: "3 · How people look at it", Tags: []string{"tour"}, Category: "Getting started",
		Widgets: []modeltransfer.Widget{
			text(`# Charts, cards and pages

The same numbers, shown the way each person needs them. The chart below is the example model again — no second copy of the data, no export step. Change a number on the previous page and this moves.`, 0, 110, 0),

			modeltransfer.Widget{
				WidgetType: "chart", SortOrder: 1, ColStart: 1, ColSpan: 12,
				PosX: num(0), PosY: num(134), SizeW: num(pageWidth), SizeH: num(320),
				Title: str("Cost by quarter"), ShowTitle: true, Props: json.RawMessage(chartProps),
			},

			text(`## What you can put on a page

- **Grid** — numbers to read and to type into
- **Chart** — bars, lines, pies, from any grid
- **Metric KPI** — one number, large
- **Form** — structured entry that writes into the model
- **Trigger** and **Integration** — run a rule or a data sync from a button
- **Text** and **Image** — the ones this tour is written in

## Pinning a page to what someone cares about

A dashboard can be scoped — to one team, one quarter — so the same page serves everybody without anyone building four copies of it. Open **Design** on any dashboard and use the selector strip above a widget.`, 478, 301, 2),

			text(`---
Next: **4 · Beyond the numbers**.`, 803, 82, 3),
		},
	}
}

func beyondTheNumbers() modeltransfer.Dashboard {
	return modeltransfer.Dashboard{
		ID: dashBeyond, Name: "4 · Beyond the numbers", Tags: []string{"tour"}, Category: "Getting started",
		Widgets: []modeltransfer.Widget{
			text(`# Getting data in, and working with other people

## Bringing numbers in

Under **Integrations** in the sidebar:

- **Excel / CSV** — upload a file, map its columns once, commit
- **Google Sheets** — paste a link-shared sheet, or share a private one with your workspace's Google service account, and re-sync on demand
- **REST API** — connect any JSON API, map its fields, run it on a schedule

Each one keeps its mapping, so the second import is one click.

## Asking people for numbers

A **form** collects structured entries — an expense, a request, a headcount change — and writes them straight into the model's metrics. Built under **Forms**.`, 0, 348, 0),

			text(`## Getting things agreed

A **workflow** moves something through the people who must see it: submit, review, approve, rework. Steps are assigned to *roles*, not to named people, so the chain keeps working when someone is away or leaves. A running workflow keeps the definition it started with, so changing the process never rewrites history.

Built under **Workflows**; what is waiting for you appears in **Workflow Inbox**.`, 372, 161, 1),

			text(`## Who sees what

Your account holds every role, because it is your workspace. As you add people, each gets only what they need.`, 557, 84, 2),

			picture(roles(), "The four roles and what each one does", 665, 780, 210, 3),

			text(`Access can be narrower still: a person can be limited to certain dimension members — their own team's rows and nobody else's — under **Access Rules**.

## Building by describing it

**AI Developer** in the sidebar builds the same things you can build by hand: describe the model you want, review what it proposes step by step, and apply it. It has no more power than you do.

---

That is the tour. This model is yours: change it, or delete it under **Models** and start on the real thing.`, 899, 212, 4),
		},
	}
}

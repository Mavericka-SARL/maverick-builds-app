package starter

import (
	"encoding/json"
	"time"

	"github.com/mavericks-engine/mavericks/internal/modeltransfer"
)

// The developer guide: seven pages for the person who builds models — a
// first model from nothing, working in a revision, dimensions and metrics,
// formulas, grids and dashboards, forms/workflows/integrations, and the AI
// Developer.
//
// It carries its own example, as small as the tour's: two products over the
// four quarters of a Time dimension, two inputs and three calculated metrics.
// Each calculated metric is there to show one thing the prose explains — a
// plain product, the Formula aggregation rule against a plain average, and a
// time function with a time summary — and page 4's grid and chart are that
// example, live, so what the page says can be checked by typing a number.
//
// Private identifiers carry the "dev" prefix so the guides, written side by
// side in this package, never collide.

// DeveloperGuideName is the model DeveloperGuide creates.
const DeveloperGuideName = "Developer guide"

// Placeholder ids: Import allocates real ones and remaps every reference.
const (
	devDimProduct  = "dev-dim-product"
	devDimPeriod   = "dev-dim-period"
	devMetUnits    = "dev-m-units"
	devMetPrice    = "dev-m-price"
	devMetRevenue  = "dev-m-revenue"
	devMetAvgPrice = "dev-m-avg-price"
	devMetYTD      = "dev-m-revenue-ytd"
	devGrid        = "dev-grid-sales"
	devGridName    = "Sales by quarter"
)

// devGap is the space between two stacked widgets, as on the tour's pages.
const devGap = 24

// The example: what two products sold, quarter by quarter. Coffee's price
// rises in the second half, Tea's holds, so the average price of the two
// (price, aggregation Average) and the price customers actually paid on
// average (avg_price, aggregation Formula) visibly differ.
var devProducts = []struct {
	code, label, origin string
	units, price        [4]float64
}{
	{"COFFEE", "Coffee", "Colombia", [4]float64{1200, 1100, 1000, 1400}, [4]float64{3.5, 3.5, 3.8, 3.8}},
	{"TEA", "Tea", "India", [4]float64{600, 650, 700, 900}, [4]float64{2.8, 2.8, 2.8, 2.8}},
}

// devQuarters are the dated leaf periods of the Time dimension; FY26 above
// them has no dates and is their total.
var devQuarters = []struct{ code, start, end string }{
	{"Q1", "2026-01-01", "2026-03-31"},
	{"Q2", "2026-04-01", "2026-06-30"},
	{"Q3", "2026-07-01", "2026-09-30"},
	{"Q4", "2026-10-01", "2026-12-31"},
}

// devCode is inline Markdown code: the raw strings below cannot hold a
// backtick.
func devCode(s string) string { return tick + s + tick }

// devText is one prose block as a row of its own.
func devText(body string, h int) []modeltransfer.Widget {
	return []modeltransfer.Widget{text(body, 0, h, 0)}
}

// devPicture is one diagram as a row of its own.
func devPicture(svg, alt string, w, h int) []modeltransfer.Widget {
	return []modeltransfer.Widget{picture(svg, alt, 0, w, h, 0)}
}

// devStack lays a page out top-down: each row starts devGap below the
// tallest widget of the row above, and sort order follows reading order.
// Heights stay explicit, so changing one re-flows the page below it.
func devStack(rows ...[]modeltransfer.Widget) []modeltransfer.Widget {
	var out []modeltransfer.Widget
	y := 0
	for _, row := range rows {
		tallest := 0
		for _, w := range row {
			w.PosY = num(y)
			w.SortOrder = len(out)
			out = append(out, w)
			if w.SizeH != nil && *w.SizeH > tallest {
				tallest = *w.SizeH
			}
		}
		y += tallest + devGap
	}
	return out
}

func devJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// DeveloperGuide builds the guide. Deterministic, like Package.
func DeveloperGuide() modeltransfer.Package {
	product := modeltransfer.Dimension{
		ID: devDimProduct, Name: "product", AggRule: "sum",
		TypedProperties: []modeltransfer.DimProperty{{Name: "origin", DataType: "text"}},
	}
	product.Members = append(product.Members, modeltransfer.Member{ID: "dev-p-all", Code: "ALL", Label: "All products", SortOrder: 0})
	for i, p := range devProducts {
		product.Members = append(product.Members, modeltransfer.Member{
			ID: "dev-p-" + p.code, Code: p.code, Label: p.label, ParentMemberID: str("dev-p-all"), SortOrder: i + 1,
			Properties: devJSON(map[string]string{"origin": p.origin}),
		})
	}

	// A real Time dimension — declared, not inferred from a name — so
	// YEARTODATE can move along it and the year total follows each metric's
	// time summary.
	period := modeltransfer.Dimension{
		ID: devDimPeriod, Name: "period", AggRule: "sum",
		DimensionType: "time", TimeGranularity: str("quarter"), FiscalYearStart: num(1),
	}
	period.Members = append(period.Members, modeltransfer.Member{ID: "dev-t-FY26", Code: "FY26", Label: "FY 2026", SortOrder: 0})
	for i, q := range devQuarters {
		period.Members = append(period.Members, modeltransfer.Member{
			ID: "dev-t-" + q.code, Code: q.code, Label: q.code + " 2026", ParentMemberID: str("dev-t-FY26"), SortOrder: i + 1,
			PeriodStart: str(q.start), PeriodEnd: str(q.end), TimeIndex: num(i),
		})
	}

	tags := []string{"sales"}
	metrics := []modeltransfer.Metric{
		{ID: devMetUnits, Name: "units", IsInput: true, StorageType: "oltp", AggRule: "sum", TimeSummary: "sum", Format: "number", Tags: tags},
		{ID: devMetPrice, Name: "price", IsInput: true, StorageType: "oltp", AggRule: "average", TimeSummary: "average", Format: "currency", FormatDecimals: 2, FormatCurrency: "$", Tags: tags},
		{ID: devMetRevenue, Name: "revenue", Formula: str("=units * price"), StorageType: "oltp", AggRule: "sum", TimeSummary: "sum", Format: "currency", FormatCurrency: "$", Tags: tags},
		{ID: devMetAvgPrice, Name: "avg_price", Formula: str("=IF(units = 0, 0, revenue / units)"), StorageType: "oltp", AggRule: "formula", TimeSummary: "sum", Format: "currency", FormatDecimals: 2, FormatCurrency: "$", Tags: tags},
		{ID: devMetYTD, Name: "revenue_ytd", Formula: str("=YEARTODATE(revenue)"), StorageType: "oltp", AggRule: "sum", TimeSummary: "last", Format: "currency", FormatCurrency: "$", Tags: tags},
	}
	// The edges the formula save would write (metricformula.Validate):
	// YEARTODATE reads revenue at every earlier period of the year.
	deps := []modeltransfer.Dependency{
		{MetricID: devMetRevenue, DependsOn: devMetUnits},
		{MetricID: devMetRevenue, DependsOn: devMetPrice},
		{MetricID: devMetAvgPrice, DependsOn: devMetUnits},
		{MetricID: devMetAvgPrice, DependsOn: devMetRevenue},
		{MetricID: devMetYTD, DependsOn: devMetRevenue, UnboundedPast: true},
	}

	grid := modeltransfer.Grid{
		ID: devGrid, Name: devGridName,
		Metrics: []modeltransfer.GridMetric{
			{MetricID: devMetUnits, SortOrder: 0}, {MetricID: devMetPrice, SortOrder: 1}, {MetricID: devMetRevenue, SortOrder: 2},
			{MetricID: devMetAvgPrice, SortOrder: 3}, {MetricID: devMetYTD, SortOrder: 4},
		},
		Dimensions: []modeltransfer.GridDimension{{DimensionID: devDimProduct}, {DimensionID: devDimPeriod}},
	}

	return modeltransfer.Package{
		Format: modeltransfer.PackageFormat, Version: modeltransfer.PackageVersion, ExportedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ModelName: DeveloperGuideName, StorageType: "oltp", RevisionName: RevisionName, IncludeData: true,
		Dimensions: []modeltransfer.Dimension{product, period}, Metrics: metrics, Dependencies: deps,
		Grids: []modeltransfer.Grid{grid},
		Dashboards: []modeltransfer.Dashboard{
			devFirstModelPage(), devRevisionsPage(), devModelPage(), devFormulasPage(), devScreensPage(), devFlowPage(), devAIPage(),
		},
		Facts: devFacts(),
	}
}

func devFacts() []modeltransfer.Fact {
	var out []modeltransfer.Fact
	for _, p := range devProducts {
		for i, q := range devQuarters {
			members, _ := json.Marshal(map[string]string{devDimProduct: p.code, devDimPeriod: q.code})
			out = append(out,
				modeltransfer.Fact{MetricID: devMetUnits, DimMembers: members, Value: p.units[i]},
				modeltransfer.Fact{MetricID: devMetPrice, DimMembers: members, Value: p.price[i]},
			)
		}
	}
	return out
}

func devDashboard(id, name string, rows ...[]modeltransfer.Widget) modeltransfer.Dashboard {
	return modeltransfer.Dashboard{ID: id, Name: name, Tags: []string{"guide"}, Category: "Getting started", Widgets: devStack(rows...)}
}

func devFirstModelPage() modeltransfer.Dashboard {
	return devDashboard("dev-dash-1-first-model", "1 · Your first model",
		devText(`# Your first model

This guide is for the person who builds models. This page goes from nothing to a model people can type numbers into, in eight steps; the pages after it explain each step in depth.

The guide is a model too, *Developer guide*, with a small example — two products over four quarters — that page 4 uses. Open it in the Build screens and change it as much as you like, in a revision of your own.`, 164),

		devPicture(devFirstModel(), "A tenant admin creates the application and the model; you add a revision, dimensions and metrics, a grid and a dashboard, then press Set active", 860, 190),

		devText(`## 1–2 · An application and a model

An **application** groups related models; a **model** holds what you build. Creating them belongs to the **tenant admin**. If you signed up for this workspace yourself, that is you: your account is tenant admin, developer and business admin at once. If it is not, ask your tenant admin for the two, and start at step 3.

1. Open **Build › Models** and press **New application** at the top of the page. Type a name, such as *Sales*, and press **Create application**.
2. The model form opens next, with the new application already chosen. Type a name, such as *Sales plan*, and press **Create model**. For another model in an application you already have, press **New model** instead.

**Tenant admin › Applications** does the same: **New application** at the top of your tenant, **Add model** at the top of each application.`, 244),

		devText(`## 3 · A revision to build in

A new model is empty and has no revision yet. Under it in **Build › Models**, press **New revision**, type a name — *Working*, say — and press **Save**. It becomes your working revision: the bar at the top names the model and the revision, and every Build screen now changes them. Page 2 covers revisions.`, 132),

		devText(`## 4 · Dimensions

**Build › Dimensions** → **New dimension**. Make a **Standard** one for what you slice by — products, regions, teams — and give it members with **Add root member**. If your numbers run over time, add a **Time** one as well; **Generate periods…** fills it. Page 3.

## 5 · Metrics

**Build › Metrics** → **Add metric**. First what people type, Type **Input** — `+devCode("units")+` and `+devCode("price")+`, say. Then what is worked out from them, Type **Calc**, with a **Formula** such as `+devCode("=units * price")+`. Pages 3 and 4.

## 6 · A grid

**Build › Grids** → **New grid**, name it, **Create**. Press **Configure** on its card, move your metrics and dimensions to *In this grid*, and press **Done**. A metric takes its dimensions from the grid it sits on. Page 5.

## 7 · A dashboard

**Build › Dashboards** → **New dashboard**, name it, **Create**, then **Design** on its row. Click **Grid** in the palette, choose your grid, press **Add widget**, then **Save**. Switch to **Preview** and type a few numbers: the calculated metrics and the totals follow at once. Page 5.`, 366),

		devText(`## 8 · Make it live

Back in **Build › Models**, press **Set active** on your revision: from now on it is what people see in this model. Press **Set as business default** if people should open this model first, and give business roles its dashboards — page 5 shows where. Before a later change goes live, page 7 has a short checklist.

## Or describe it to the AI Developer

After step 3 you can hand steps 4 to 7 to **Build › AI Developer**. Describe the model in plain words — *products and quarters; units and price typed in; revenue is units times price; a grid and a dashboard to type into* — and confirm the plan it proposes. Its changes land in a draft revision of their own, which **Promote to Active** makes live. Page 7.

---
Next: **2 · Revisions**.`, 284),
	)
}

func devRevisionsPage() modeltransfer.Dashboard {
	return devDashboard("dev-dash-2-revisions", "2 · Revisions",
		devText(`# Work in a revision of your own

Everything you build — dimensions, metrics, grids, dashboards, forms, workflows, triggers, integrations — belongs to a **revision** of a model: a complete copy of it, including the numbers entered so far. One revision is *live*: it is what everyone else sees. Build in a copy, and make it live when it is ready.`, 132),

		devText(`## Check where you are

Every Build screen shows two things in the bar at the top: **Model** (the application and the model) and **Revision**. Everything you add, change or delete goes into that revision.

Build starts in the live revision of the model you have open, and nothing stops an edit there: it reaches people at once. Look at the bar before you change anything.`, 164),

		devPicture(devRevisionFlow(), "New revision copies the live one; Set active makes your copy live, and the old one stays in the list", 860, 170),

		devText(`## Make a revision

1. Open **Build › Models**. It lists each application you can reach, its models, and each model's revisions.
2. Under the model you want to change, press **New revision**, type a name where it says *Revision name…* and press **Save**.
3. The revision is made in that model. It copies your working revision if that is in the same model, and the model's live revision if not. It becomes your working revision at once: its row is marked **Working**, and the bar names it.

To work in another revision later, click its row.`, 196),

		devText(`## Make it live

When the revision is ready, press **Set active** on its row and confirm with **Set active**. It becomes the live revision everyone sees. The previous one stays in the list, so going back is the same two clicks.

The live revision cannot be deleted. Any other can, with the trash icon on its row (**Delete revision**); that cannot be undone.`, 140),

		devText(`## Which model people open

An application can hold several models: *Getting started* holds the tour and three guides. Business screens open the model marked **business default** unless someone has opened another one under **Models** in their own sidebar. To change the default, press **Set as business default** beside a model in **Build › Models**.`, 132),

		devText(`## Two more things to know

- The **AI Developer** puts its model changes into a revision of its own, named *AI Draft* and the date, which the first change you confirm creates. Page 7 covers it, and the few kinds of change that take effect at once.
- People, business roles and access rules belong to the whole workspace, not to a revision. A workflow that is already running keeps the definition it started with, whatever you change afterwards.

---
Next: **3 · Dimensions, metrics**.`, 204),
	)
}

func devModelPage() modeltransfer.Dashboard {
	return devDashboard("dev-dash-3-model", "3 · Dimensions, metrics",
		devText(`# Dimensions and metrics

A model is built from two lists: the ways you slice numbers — **dimensions** — and the things you measure — **metrics**. Grids, formulas and charts all read them, so they are worth getting right first.`, 112),

		devText(`## Dimensions

Open **Build › Dimensions** and press **New dimension**: a **Name**, a **Dimension type**, **Tags** if you like, then **Create**.

- **Standard** — members you define: products, regions, teams. **Add root member** adds one, with a code and a label; **Add child** on a member puts another under it, and every parent becomes a total of its children. The **Move up** and **Move down** arrows on a member put it in order among its siblings; grids list members in that order.
- **Time** — periods with dates. Choose the **Granularity** (day, week, month, quarter, half year, year or custom) and the month the fiscal year starts; both are fixed once created. **Generate periods…** creates the dated periods, and periods without dates, such as H1 or FY 2026, group them. Only a Time dimension can carry time functions such as PREVIOUS or YEARTODATE — a dimension merely named *month* cannot.

A standard dimension can also be the child of another dimension (**Parent dimension**), or group another dimension's members by one of their properties (**Group members of**).`, 312),

		devText(`## Properties

Each standard dimension shows its **Dimension Properties** above its members (**Hide props** folds them away): attributes each member can carry, typed text, number or date — a product's origin, a region's currency. A formula reads them as `+devCode("product.origin")+`.

A member or a property that a formula names cannot be deleted, and renaming a property rewrites the formulas that read it.`, 140),

		devText(`## Metrics

Open **Build › Metrics** and press **Add metric**:

- **Metric name** — snake_case, such as `+devCode("gross_margin")+`. Names ignore case: `+devCode("Revenue")+` and `+devCode("revenue")+` count as the same name. **Display name**, if you give one, is what grids and charts show instead.
- **Type** — **Input** is typed or imported. **Calc** has a **Formula** and is worked out by the platform whenever an input changes. The type cannot be changed afterwards.
- **Format** — Number, Percentage, Currency, Boolean, Text, **Pick-list** (each cell holds a member of the dimension chosen under **Members of**, picked from a list in the grid) or **Date**, with decimal places and a currency symbol where they apply.
- **Aggregation rule** and **Time summary** — how its totals are made, below.
- **Tags** — for the tag filter and the *Search metrics…* box.

A metric has no dimensions of its own: it takes those of the grid it sits on (page 5), and sits on one grid at most. Until it is on a grid it has none, and a time function has nothing to move along.

To change a metric later, use **Edit** on its row. After a formula change, a *Recalc results* banner shows the new values.`, 336),

		devText(`## How totals are made

A formula works out the leaf cells: one product, one quarter. Every total above them comes from two settings on the metric.

- **Aggregation rule**, across ordinary dimensions. **Sum** adds the members. **Average** takes the mean of the members that have a value. **Count** counts the members holding a value other than 0. **Formula** works the metric's own formula out again on the totals of what it reads — the right choice for a ratio or a percentage. **Rate** divides the total of one metric by the total of another. **None** leaves the totals empty, for a figure that does not add up — a date, a setting.
- **Time summary**, across a time dimension — offered by **Add metric** once the revision has one, and by **Edit** once the metric's grid has one: *sum* for flows such as revenue, *last* for a closing balance, *first* for an opening balance, *average*, *min*, *max*, or *none* where a total over time means nothing.

A new metric totals by Sum unless you pick another rule, and **Edit** on its row changes it later. Formula is for calculated metrics only.`, 268),

		devPicture(devTotals(), "Revenue for two products over four quarters: the All products row comes from the aggregation rule, the FY 2026 column from the time summary", 760, 250),

		devText(`## Before you delete

A metric that another metric reads cannot be deleted: the **Used by** column shows who reads it, and the delete is refused until those formulas change. The same protects a dimension member or property that a formula names.

---
Next: **4 · Formulas**.`, 156),
	)
}

func devFormulasPage() modeltransfer.Dashboard {
	gridProps := devJSON(map[string]any{
		"sync_context": true,
		"default_view": map[string]any{
			"rows":       []string{"__metrics__"},
			"cols":       []string{devDimPeriod},
			"context":    []string{devDimProduct},
			"filter_sel": map[string]string{devDimProduct: "COFFEE"},
		},
	})
	chartProps := devJSON(map[string]any{
		"chart": map[string]any{
			"chart_type": "line", "metric_ids": []string{devMetRevenue, devMetYTD}, "dimension_id": devDimPeriod,
			"context_defaults": map[string]string{devDimProduct: "COFFEE"}, "hide_rollup_members": true,
			"show_legend": true, "value_format": "currency",
		},
		"sync_context": true,
	})

	return devDashboard("dev-dash-4-formulas", "4 · Formulas",
		devText(`# Formulas

A calculated metric is one line of formula. The platform works it out for every cell, and again whenever an input changes: there is nothing to refresh, and nobody can type over the result.

## How a formula reads

A formula names metrics and combines them: `+devCode("=units * price")+`. It is worked out once for each leaf cell — one member of every dimension — so *revenue* for Coffee in Q2 reads Coffee's units and price in Q2. A metric's totals are not written into its formula; they come from its aggregation rule and time summary.

A formula can also ask where it is: `+devCode("product")+` is the code of the cell's product, `+devCode("product.origin")+` one of its properties, and `+devCode("PARENT(product)")+` the code of its parent.`, 276),

		devText(`## The example on this page

Two products, Coffee and Tea, over the four quarters of 2026, on a Time dimension called *period*:

- `+devCode("units")+` and `+devCode("price")+` are inputs. *units* totals by Sum, *price* by Average.
- `+devCode("revenue")+` is `+devCode("=units * price")+`, totalled by Sum.
- `+devCode("avg_price")+` is `+devCode("=IF(units = 0, 0, revenue / units)")+` with the **Formula** rule, so each of its totals is total revenue divided by total units.
- `+devCode("revenue_ytd")+` is `+devCode("=YEARTODATE(revenue)")+`: revenue from the start of the fiscal year. Its time summary is *last*, so the year shows the figure at year end.`, 244),

		[]modeltransfer.Widget{{
			WidgetType: "grid", RefID: str(devGrid), ColStart: 1, ColSpan: 12,
			PosX: num(0), SizeW: num(pageWidth), SizeH: num(368),
			Title: str(devGridName), ShowTitle: true, Props: gridProps,
		}},

		devText(`## Try it

Type a new number into one of the **units** cells the legend calls *Editable*, and press Enter. *revenue* answers in that quarter, *revenue_ytd* in that quarter and every one after it, and the FY 2026 column follows.

Then set the product selector to **All products**. There *price* is a plain average of the two products' prices, while *avg_price* is what customers paid on average — total revenue over total units. Same inputs, a different aggregation rule.`, 164),

		[]modeltransfer.Widget{{
			WidgetType: "chart", RefID: str(devGrid), ColStart: 1, ColSpan: 12,
			PosX: num(0), SizeW: num(pageWidth), SizeH: num(320),
			Title: str("Revenue and revenue to date"), ShowTitle: true, Props: chartProps,
		}},

		devText(`## More you can write

Each of these is valid in this model. Try them in a revision of your own: add the metric under **Build › Metrics**, then move it into *Sales by quarter* under **Build › Grids** → **Configure**, so that it has the grid's product and period.

- `+devCode("IF(PREVIOUS(revenue) = 0, 0, ROUND((revenue / PREVIOUS(revenue) - 1) * 100, 1))")+` — growth on the previous quarter, in percent. Give it time summary *none*: a year of growth rates does not add up.
- `+devCode("MOVINGSUM(revenue, -1, 0, AVERAGE)")+` — the average of this quarter and the one before.
- `+devCode(`ROUND(revenue / SUMIFS(revenue, product, "*") * 100, 1)`)+` — each product's share of all products, in percent.
- `+devCode("revenue / LOOKUP(revenue, product, PARENT(product))")+` — the share of the parent member.
- `+devCode(`IF(product.origin = "India", price * 1.1, price)`)+` — one member treated differently, chosen by a property.`, 272),

		devText(`## When something is wrong

The **Formula** field warns while you type when a name is not a metric or dimension of this revision. The server checks the whole formula when you save and refuses it with the reason. A metric whose calculation fails gets a red mark on its row in **Build › Metrics**; hover it for the message.

**Build › Dependency Graph** draws every metric — inputs and calculated ones in different colours — with arrows showing which metric reads which. **Find metric…** jumps to one.`, 188),

		devText(`## The whole language

The formulas manual describes every function with examples the engine computed, how totals are made, recipes for common needs, and what each error message means. It opens in a new tab.

[Open the formulas manual](`+formulasManual+`)

---
Next: **5 · Grids, dashboards**.`, 188),
	)
}

func devScreensPage() modeltransfer.Dashboard {
	total := devJSON(map[string]any{"kpi_context_mode": "total"})
	pinned := devJSON(map[string]any{"kpi_context_mode": "pin", "kpi_scope": map[string]string{"dimension_id": devDimProduct, "member_code": "COFFEE"}})

	return devDashboard("dev-dash-5-screens", "5 · Grids, dashboards",
		devText(`# Grids and dashboards

A grid decides which numbers belong together; a dashboard decides how people see them. Business users never open a grid on its own — they meet it inside a dashboard.

## A grid

Open **Build › Grids**, press **New grid**, name it — **Tags** too, if you like — and press **Create**. Then press **Configure** on its card and move metrics and dimensions from *Available* to *In this grid*. For each dimension, **Show:** picks all levels, the root only, one level, or the leaves only. Every change is saved as you make it; **Done** closes the card.

A grid has no layout of its own. Where each dimension sits — rows, columns or a selector — is set on each dashboard that shows it.`, 252),

		devText(`## A dashboard

1. Open **Build › Dashboards** and press **New dashboard**. Give it a **Name** — a **Folder** and **Tags** too, if you like — and press **Create**.
2. Press **Design** on its row.
3. Click a widget type in the palette — Grid, Chart, Form, Metric KPI, Trigger, Integration, Text, Image or Import — choose what it shows under *Configure & place*, and press **Add widget**. It lands on the canvas, already saved.
4. Drag a widget to move it; pull its handles to resize it.
5. Moves, resizes and property changes wait for **Save**, in the bar that appears when there is something to save.`, 216),

		devText(`## Widget Properties

Click a widget in Design and **Widget Properties** opens beside it:

- **Default view** (grids) — place *Metrics* and each dimension in a Row, a Column or the Context. Without one, a grid shows metrics in rows, one dimension across the columns and the rest as selectors.
- **Metrics shown** (grids) — which of the grid's metrics this widget shows, and in what order; and, for a row or column dimension, which of its members (*All members* by default).
- **Context sync** (grids and charts) — on by default: widgets that share a dimension share one selector, so choosing a product once moves them all. Untick it to give a widget selectors of its own.
- **Default context** (Metric KPI) — **Whole-model total**, **Follow dashboard selectors**, or **Pin to a member**.
- **Chart configuration**, **Header**, **Appearance**, where the **Selectors** sit, and an exact **Position** and **Size**.`, 244),

		[]modeltransfer.Widget{
			{
				WidgetType: "metric_kpi", RefID: str(devMetRevenue), ColStart: 1, ColSpan: 6,
				PosX: num(0), SizeW: num(440), SizeH: num(kpiHeight),
				Title: str("Revenue, FY 2026, all products"), ShowTitle: true, Props: total,
			},
			{
				WidgetType: "metric_kpi", RefID: str(devMetRevenue), ColStart: 7, ColSpan: 6,
				PosX: num(460), SizeW: num(440), SizeH: num(kpiHeight),
				Title: str("Revenue, FY 2026, Coffee"), ShowTitle: true, Props: pinned,
			},
		},

		devText(`The left card's **Default context** is **Whole-model total**: revenue for both products over the whole year. The right one is pinned to Coffee. Change a number on page 4 and both follow.`, 84),

		devText(`## Preview, and who sees it

Beside the dashboard's name, switch from **Design** to **Preview** to use the page as people will: type into grids, use the selectors.

**Run › Dashboards** shows the dashboards of the live revision of the model a person has open, so a dashboard you build in your own revision reaches people when you press **Set active**. Which dashboards a business role can open is set per role under **Build › Roles**, on your working revision; the grants go live with it. If you are also a business admin, Build has no Roles screen: use **Business Admin › Roles**, which changes the live revision of the model selected under **Business Admin › Models**.

## Keep them findable

**New folder** groups dashboards. Once a folder exists, each row gets a list that files it in a folder, and an *All folders* list above the rows narrows the view; the tag chips and *Search dashboards…* narrow it too. The pencil on a row edits its name and tags. Metrics, dimensions and grids carry tags as well, and the Metrics, Dimensions, Forms, Grids and Workflows lists each have a search box.

---
Next: **6 · Data in, workflows**.`, 340),
	)
}

func devFlowPage() modeltransfer.Dashboard {
	return devDashboard("dev-dash-6-flow", "6 · Data in, workflows",
		devText(`# Forms, workflows and integrations

Numbers do not only arrive by typing into a grid. Forms collect entries, integrations bring data in from files and other systems, and workflows move a request through the people who must agree to it. Each is built in its own Build screen and belongs to the revision like everything else.`, 136),

		devPicture(devWiring(), "A form posts into an input metric through a Form Records mapping; an event fires a trigger that starts a workflow; imports fill a grid, a form or a dimension", 860, 270),

		devText(`## Forms

**Build › Forms** → **New form**: a **Form name**, a **Display label**, then the fields — **Add field** for each: Text, Number, Date, Select, Boolean, Metric, or one of the model's dimensions — and **Create form**. People — business users and business admins alike — work on its records only through a **Form** widget on a dashboard, so place each form on a dashboard their role can open: there they add, edit, export and import records, and an administrator changes their status and syncs them.

A form on its own only stores its records. To post their numbers into the model, map it: **Build › Integrations** → **Form Records** → **New integration**. Choose the **Source form**, the **Target grid** and its **Target input metric**, the **Source value field**, how records combine (**Aggregation**), which statuses post (**Post when status is**), and the **Dimension field mappings**. With **Live update** on, a record posts as soon as it reaches one of those statuses; **Backfill** posts the ones already there.`, 232),

		devText(`## Workflows

**Build › Workflows** → **From Template** (Simple Approval, Two-Level Approval, Amount-Based Approval or Notify Only) or **New Workflow**, which asks for a **Name** and a **Trigger event**: *Manual*, a form submitted, an import completed or failed, or an *API trigger*. A template starts as *Manual*. Open it and add steps with **Add step**: Task, Approval, Condition, Notification or Join. An approval or a task goes to roles, not to named people (**Approver roles**, **Assignee roles**), so it keeps working when someone is away.

Then **Save**, **Validate**, try it with **Test Run**, and **Publish**. What you save later applies to new requests; one already running keeps the definition it started with.`, 208),

		devText(`## Triggers

Once published, a *Manual* workflow can be started by people themselves: it is listed under *Start a workflow* in **Run › Workflow Inbox**. Anything else — a dashboard button, a schedule, a form, an import or an outside call — starts a workflow through a rule.

**Build › Triggers** → **New rule**: choose the **Workflow**, and the **Trigger type** follows its trigger event. A *Manual* workflow offers *manual* (a button) or *schedule* (a cron expression); any other is fixed to its event — *form_submit*, *integration_completed*, *integration_failed* or *api*. Then press **Create rule**. **Create Trigger** in a workflow's row menu does the same from the Workflows list.

A **Trigger** widget on a dashboard runs a manual rule that is enabled. Until a workflow has a rule, its editor says *No rule yet*; a *Manual* workflow that people start themselves needs none.`, 240),

		devText(`## Integrations

Everything that brings data in is under **Build › Integrations**:

- **Excel / CSV Import** — **New Import** opens a wizard: upload the file and choose what it fills (a grid, a form, or a dimension's members), **Shape** a sheet laid out for people (titles, merged headers, months across) into rows, map its columns, validate, and commit. **Save as Integration** keeps the mapping for the next file.
- **Google Sheets** — **Import from Sheet**, paste the sheet's link, **Fetch sheet**. A sheet shared as *Anyone with the link* works as it is. A private one needs a Google service account: store its key under *Private sheets*, and share the sheet with that account. A saved sheet refreshes with **Sync Now**.
- **REST API** — **New integration** walks through six steps, from *Basics* to *Run & schedule*, and **Activate** turns it on. It runs by hand or on a schedule.
- **Form Records** — the form mapping above.
- **Data Export** — **New export** saves a fixed download of a grid's values as CSV, Excel or JSON: which metrics, which dimensions as columns, and which members.

On a dashboard, an **Integration** widget runs a saved integration from a button, and an **Import** widget gives people an upload box for a grid, with a template to download.

---
Next: **7 · AI and going live**.`, 388),
	)
}

func devAIPage() modeltransfer.Dashboard {
	return devDashboard("dev-dash-7-ai", "7 · AI and going live",
		devText(`# The AI Developer, and going live

**Build › AI Developer** builds with you. Describe a change in plain words and it proposes the steps — dimensions, metrics, grids, dashboards, forms, workflows — for you to confirm. It works under the same checks as the Build screens, and nothing is written until you confirm.

## Set it up

It needs an AI provider key. Press **AI Settings** (the gear), choose the **LLM Provider** — OpenAI, Anthropic (Claude), Google (Gemini), Mistral or DeepSeek — and, if you like, a **Model**. Paste your **API Key**, then press **Save settings** and **Test connection**. That key is yours alone. One key for the whole workspace, set by a tenant admin under **Tenant admin › AI keys**, needs the Enterprise edition.

## Work with it

Press **New session** and describe what you want. **Attach a document** adds a pdf, xlsx, docx, csv, txt or md file for it to work from. Each proposal arrives as an **Action plan**: read the steps, then press **Confirm & Execute** or **Cancel**.`, 328),

		devText(`## Where its changes land

The first plan you confirm in a session creates a revision for it, named *AI Draft* and the date, copied from the live revision. The chat then carries an **AI draft** badge, and every later change in the session goes into that draft — never into your working revision or the live one.

The draft is listed in **Build › Models** like any revision: click its row to look through the result in every Build screen. Back in the chat, the draft's banner has **Promote to Active**, which makes it the live revision, and **Discard**, which throws it away.

Three kinds of change are not revision-scoped and take effect as soon as you confirm: business roles, user access rules, and form records posted into the model.

## What stays with you

Publishing and test-running workflows, making a revision live or deleting one, the business default, who is in a role, users, the REST API and Google Sheets connectors and their runs, and the business data itself — except that it imports a spreadsheet you attach to the chat, and writes single input values such as a setting, into its draft. [What the AI Developer can and cannot do](`+publicDocs+`AI_DEVELOPER.md) lists it all.`, 304),

		devText(`## Going live

Before you press **Set active**:

- **Build › Metrics** — no red mark beside a formula.
- **Build › Dependency Graph** — every calculated metric reads what you meant it to.
- **Build › Dashboards** — each page checked in **Preview**.
- **Build › Workflows** — the ones people need are published, and each that should start on a button, a schedule or an event has a rule under **Build › Triggers**.

Then open **Build › Models**, press **Set active** on your revision, and **Set as business default** if people should open this model first.`, 252),

		devText(`## Read further

- [Developer manual](`+developerManual+`) — every Build screen, in detail
- [Formulas manual](`+formulasManual+`) — the formula language, function by function
- [The AI Developer](`+publicDocs+`AI_DEVELOPER.md) — what it reads, writes and leaves to you
- [HTTP API](`+publicDocs+`API.md) — for the *api* trigger type and scripts of your own

---

That is the developer guide. Its example model is yours to change — in a revision of your own. When you no longer need it, a tenant admin can delete it under **Tenant admin › Applications**.`, 236),
	)
}

// ── Diagrams ────────────────────────────────────────────────────────────────
//
// Each draws on its own paper card, so the diagram stays legible whatever
// the page behind it (the console has a dark theme).

func devCard(w, h int) string {
	return `<rect x="1" y="1" width="` + itoa(w-2) + `" height="` + itoa(h-2) + `" rx="12" fill="` + paper + `" stroke="` + line + `"/>`
}

func devLabel(x, y int, s string, anchor string) string {
	return `<text x="` + itoa(x) + `" y="` + itoa(y) + `" text-anchor="` + anchor + `" font-size="12" fill="` + muted + `">` + s + `</text>`
}

func devPill(x, y int, s string) string {
	return `<rect x="` + itoa(x) + `" y="` + itoa(y) + `" width="48" height="22" rx="11" fill="#dcfce7" stroke="#16a34a"/>` +
		`<text x="` + itoa(x+24) + `" y="` + itoa(y+12) + `" text-anchor="middle" dominant-baseline="middle" font-size="11" font-weight="700" fill="#15803d">` + s + `</text>`
}

// devFirstModel: who makes what, in the order the first page's steps go.
func devFirstModel() string {
	s := svgHead(860, 190) + devCard(860, 190)
	steps := []struct{ step, label, sub string }{
		{"1", "Application", "groups models"},
		{"2", "Model", "empty at first"},
		{"3", "Revision", "where you build"},
		{"4–5", "Dimensions", "and metrics"},
		{"6", "Grid", "numbers together"},
		{"7", "Dashboard", "what people use"},
	}
	const x0, w, gap, y, h = 21, 118, 22, 52, 64
	for i, st := range steps {
		x := x0 + i*(w+gap)
		s += devLabel(x+w/2, y-12, "step "+st.step, "middle")
		s += box(x, y, w, h, st.label, st.sub, i >= 2)
		if i+1 < len(steps) {
			s += arrow(x+w+2, y+h/2, gap-4)
		}
	}
	// Brackets under who does which part.
	bracket := func(from, to int, text string) string {
		x1, x2, by := x0+from*(w+gap), x0+to*(w+gap)+w, y+h+16
		return `<path d="M` + itoa(x1) + ` ` + itoa(by-6) + `v6h` + itoa(x2-x1) + `v-6" fill="none" stroke="` + muted + `" stroke-width="1.5"/>` +
			devLabel((x1+x2)/2, by+20, text, "middle")
	}
	s += bracket(0, 1, "a tenant admin creates these")
	s += bracket(2, 5, "you build these — then Set active (step 8) makes them live")
	return s + `</svg>`
}

// devRevisionFlow: the safe way to change a model.
func devRevisionFlow() string {
	s := svgHead(860, 170) + devCard(860, 170)
	s += box(20, 48, 210, 72, "Revision 1", "what everyone sees", false)
	s += devPill(172, 37, "live")
	s += devLabel(280, 72, "New revision", "middle")
	s += arrow(236, 84, 90)
	s += box(330, 48, 210, 72, "Your revision", "a copy you change", true)
	s += devLabel(590, 72, "Set active", "middle")
	s += arrow(546, 84, 90)
	s += box(640, 48, 200, 72, "Your revision", "now everyone sees it", false)
	s += devPill(782, 37, "live")
	s += devLabel(430, 150, "Revision 1 stays in the list: Set active on it takes you back.", "middle")
	return s + `</svg>`
}

// devTotals: where a total comes from, on the example's opening numbers.
func devTotals() string {
	const w, h = 760, 250
	s := svgHead(w, h) + devCard(w, h)
	cols := []string{"Q1", "Q2", "Q3", "Q4", "FY 2026"}
	rows := []struct {
		label string
		vals  []string
	}{
		{"Coffee", []string{"4,200", "3,850", "3,800", "5,320", "17,170"}},
		{"Tea", []string{"1,680", "1,820", "1,960", "2,520", "7,980"}},
		{"All products", []string{"5,880", "5,670", "5,760", "7,840", "25,150"}},
	}
	x0, y0, cw, ch := 150, 58, 94, 38
	s += devLabel(x0, 28, "revenue, as the example starts", "start")
	for i, c := range cols {
		s += `<text x="` + itoa(x0+i*cw+cw/2) + `" y="` + itoa(y0-10) + `" text-anchor="middle" font-size="13" font-weight="600" fill="` + ink + `">` + c + `</text>`
	}
	for r, row := range rows {
		y := y0 + r*ch
		s += `<text x="` + itoa(x0-12) + `" y="` + itoa(y+ch/2) + `" text-anchor="end" dominant-baseline="middle" font-size="13" font-weight="600" fill="` + ink + `">` + row.label + `</text>`
		for c, v := range row.vals {
			bg, stroke, weight := paper, line, "400"
			if r == len(rows)-1 || c == len(cols)-1 {
				bg, stroke, weight = fill, accent, "700"
			}
			x := x0 + c*cw
			s += `<rect x="` + itoa(x) + `" y="` + itoa(y) + `" width="` + itoa(cw) + `" height="` + itoa(ch) + `" fill="` + bg + `" stroke="` + stroke + `" stroke-width="1"/>`
			s += `<text x="` + itoa(x+cw-10) + `" y="` + itoa(y+ch/2) + `" text-anchor="end" dominant-baseline="middle" font-size="13" font-weight="` + weight + `" fill="` + ink + `">` + v + `</text>`
		}
	}
	right := x0 + len(cols)*cw
	allY := y0 + (len(rows)-1)*ch + ch/2
	s += devLabel(x0, y0+len(rows)*ch+30, "the All products row: each metric's aggregation rule", "start")
	s += devLabel(right-cw/2, y0+len(rows)*ch+54, "the FY 2026 column: each metric's time summary", "middle")
	s += `<line x1="` + itoa(x0+cw) + `" y1="` + itoa(y0+len(rows)*ch+18) + `" x2="` + itoa(x0+cw) + `" y2="` + itoa(allY+10) + `" stroke="` + accent + `" stroke-width="1.2" stroke-dasharray="4 3"/>`
	s += `<line x1="` + itoa(right-cw/2) + `" y1="` + itoa(y0+len(rows)*ch+40) + `" x2="` + itoa(right-cw/2) + `" y2="` + itoa(allY+10) + `" stroke="` + accent + `" stroke-width="1.2" stroke-dasharray="4 3"/>`
	return s + `</svg>`
}

// devWiring: how forms, triggers, workflows and imports connect to the model.
func devWiring() string {
	s := svgHead(860, 270) + devCard(860, 270)
	// Row 1: a form reaches a metric only through a mapping.
	s += box(20, 20, 180, 62, "Form", "entries people submit", false)
	s += arrow(206, 51, 38)
	s += box(250, 20, 180, 62, "Form Records", "a mapping", true)
	s += arrow(436, 51, 38)
	s += box(480, 20, 180, 62, "Input metric", "numbers on a grid", false)
	// Row 2: nothing starts a workflow but a rule.
	s += box(20, 104, 180, 62, "An event", "button, schedule, form…", false)
	s += arrow(206, 135, 38)
	s += box(250, 104, 180, 62, "Trigger", "a rule", true)
	s += arrow(436, 135, 38)
	s += box(480, 104, 180, 62, "Workflow", "published", false)
	s += arrow(666, 135, 30)
	s += box(700, 104, 140, 62, "Workflow Inbox", "the step's roles", false)
	// Row 3: imports fill the model.
	s += box(20, 188, 410, 62, "Excel / CSV · Google Sheets · REST API", "Build › Integrations", false)
	s += arrow(436, 219, 38)
	s += box(480, 188, 180, 62, "Grid, form", "or dimension members", false)
	return s + `</svg>`
}

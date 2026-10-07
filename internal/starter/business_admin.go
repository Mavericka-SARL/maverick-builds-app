package starter

import (
	"encoding/json"
	"time"

	"github.com/mavericks-engine/mavericks/internal/modeltransfer"
)

// BusinessAdminGuideName is the model BusinessAdminGuide creates.
const BusinessAdminGuideName = "Business admin guide"

// The business admin guide: five pages for the person who runs a workspace
// day to day — who opens which dashboards, who sees or changes which
// numbers, and how requests get decided. Most of what a business admin
// sets up (business roles, their dashboard grants and members, access
// rules) cannot travel in a Package, so the guide teaches the screens in
// words and pictures and has the reader make those things by hand. The
// fourth page carries a tiny model of its own — four offices in two regions
// — so the one access rule the reader can safely set on themselves (Read on
// an office) has a grid to show up in.
//
// Private identifiers carry the "ba" prefix so the guides, written side by
// side in this package, never collide.

// Placeholder ids: Import allocates real ones and remaps every reference.
const (
	baDimOffice    = "ba-dim-office"
	baMetBudget    = "ba-m-budget"
	baMetSpent     = "ba-m-spent"
	baMetRemaining = "ba-m-remaining"
	baGridID       = "ba-grid-offices"
	baGridName     = "Budget by office"
)

// baGap is the vertical space between stacked widgets, as on the tour.
const baGap = 24

// baOffices: two regions, two offices each. Codes and sort orders both
// follow the order the rows should read, so the grid and Access Rules list
// the offices the same way whichever of the two they sort by.
var baOffices = []struct {
	code, label, region string
	budget, spent       float64
}{
	{"LISBON", "Lisbon", "IBERIA", 70000, 66500},
	{"MADRID", "Madrid", "IBERIA", 90000, 52000},
	{"OSLO", "Oslo", "NORDICS", 120000, 84000},
	{"STOCKHOLM", "Stockholm", "NORDICS", 100000, 91000},
}

// BusinessAdminGuide builds the guide. Deterministic, like Package.
func BusinessAdminGuide() modeltransfer.Package {
	office := modeltransfer.Dimension{ID: baDimOffice, Name: "office", AggRule: "sum"}
	office.Members = append(office.Members, modeltransfer.Member{ID: "ba-o-all", Code: "ALL", Label: "All offices", SortOrder: 0})
	order := 1
	for _, region := range []struct{ code, label string }{{"IBERIA", "Iberia"}, {"NORDICS", "Nordics"}} {
		regionID := "ba-o-" + region.code
		office.Members = append(office.Members, modeltransfer.Member{ID: regionID, Code: region.code, Label: region.label, ParentMemberID: str("ba-o-all"), SortOrder: order})
		order++
		for _, o := range baOffices {
			if o.region != region.code {
				continue
			}
			office.Members = append(office.Members, modeltransfer.Member{ID: "ba-o-" + o.code, Code: o.code, Label: o.label, ParentMemberID: str(regionID), SortOrder: order})
			order++
		}
	}

	// Money that adds up: a region's budget is its offices' budgets, the
	// company's is the regions'. No time dimension, so no time summary to get
	// wrong.
	metrics := []modeltransfer.Metric{
		{ID: baMetBudget, Name: "budget", IsInput: true, StorageType: "oltp", AggRule: "sum", Format: "currency", FormatCurrency: "€"},
		{ID: baMetSpent, Name: "spent", IsInput: true, StorageType: "oltp", AggRule: "sum", Format: "currency", FormatCurrency: "€"},
		{ID: baMetRemaining, Name: "remaining", Formula: str("=budget-spent"), StorageType: "oltp", AggRule: "sum", Format: "currency", FormatCurrency: "€"},
	}
	deps := []modeltransfer.Dependency{
		{MetricID: baMetRemaining, DependsOn: baMetBudget},
		{MetricID: baMetRemaining, DependsOn: baMetSpent},
	}

	grid := modeltransfer.Grid{
		ID: baGridID, Name: baGridName,
		Metrics:    []modeltransfer.GridMetric{{MetricID: baMetBudget, SortOrder: 0}, {MetricID: baMetSpent, SortOrder: 1}, {MetricID: baMetRemaining, SortOrder: 2}},
		Dimensions: []modeltransfer.GridDimension{{DimensionID: baDimOffice}},
	}

	return modeltransfer.Package{
		Format: modeltransfer.PackageFormat, Version: modeltransfer.PackageVersion, ExportedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ModelName: BusinessAdminGuideName, StorageType: "oltp", RevisionName: RevisionName, IncludeData: true,
		Dimensions: []modeltransfer.Dimension{office}, Metrics: metrics, Dependencies: deps,
		Grids: []modeltransfer.Grid{grid},
		Dashboards: []modeltransfer.Dashboard{
			baYourPart(), baTheModel(), baWhoOpens(), baWhoSees(), baRequests(),
		},
		Facts: baFacts(),
	}
}

func baFacts() []modeltransfer.Fact {
	var out []modeltransfer.Fact
	for _, o := range baOffices {
		members, _ := json.Marshal(map[string]string{baDimOffice: o.code})
		out = append(out,
			modeltransfer.Fact{MetricID: baMetBudget, DimMembers: members, Value: o.budget},
			modeltransfer.Fact{MetricID: baMetSpent, DimMembers: members, Value: o.spent},
		)
	}
	return out
}

// baPage makes a guide dashboard and stacks its rows top-down: every
// widget in a row shares a top edge, the next row starts baGap below the
// row's tallest widget, and sort order follows reading order.
func baPage(id, name string, rows ...[]modeltransfer.Widget) modeltransfer.Dashboard {
	var widgets []modeltransfer.Widget
	y := 0
	for _, row := range rows {
		tallest := 0
		for _, w := range row {
			w.PosY = num(y)
			w.SortOrder = len(widgets)
			if *w.SizeH > tallest {
				tallest = *w.SizeH
			}
			widgets = append(widgets, w)
		}
		y += tallest + baGap
	}
	return modeltransfer.Dashboard{ID: id, Name: name, Tags: []string{"guide"}, Category: "Getting started", Widgets: widgets}
}

// baText and baPicture are one-widget rows; the position and order are
// set by baPage.
func baText(body string, h int) []modeltransfer.Widget {
	return []modeltransfer.Widget{text(body, 0, h, 0)}
}
func baPicture(svg, alt string, w, h int) []modeltransfer.Widget {
	return []modeltransfer.Widget{picture(svg, alt, 0, w, h, 0)}
}

func baYourPart() modeltransfer.Dashboard {
	return baPage("ba-dash-1-part", baFirstPage,
		baText(`# Your part: who sees what, and what gets decided

A business admin runs one workspace day to day. You decide who opens which dashboards and who can see or change which numbers, and you keep requests moving: approving, rejecting, unsticking. A developer builds the model and a tenant admin adds the people; everything in between is yours.

Five pages, in order. The fourth has a small model of its own to try things on. New to the platform? The `+dashLink("tour", ModelName, DashboardName)+` shows how it all fits first.`, 168),

		baText(`## Your sidebar

**User** is where the day's work happens:

- **Dashboards** — every dashboard of the model you have open, with grids you can type into, and forms whose records you review
- **Workflow Inbox** — the requests waiting for your decision

**Business Admin** is where you set things up:

- **History** — the latest requests, with every decision and comment; where you unstick one
- **Roles** — groups of people, and which dashboards each group may open
- **Access Rules** — per person, which numbers they may change, only read, or not see
- **Models** — which model you are working in, and which revision of it is live`, 316),

		baText(`## Three layers decide what someone can do

Each one narrows the one before. **Roles** decide which dashboards a person can open. **Access rules** decide which numbers on them the person can change, only read, or not see at all. And an open request locks the numbers it is about; once it is approved, they stay locked if the approval was its last step.`, 132),

		baPicture(baLayers(), "Roles decide which dashboards, access rules decide which numbers, open requests lock the numbers they are about", 880, 255),

		baText(`Roles never narrow a business admin, a developer or a tenant admin: you always see every dashboard. Access rules and locks apply to you as well.

## If you signed up for this workspace

Your account is also a developer and a tenant admin, so your sidebar has **Developer** and **Tenant admin** too. This guide stays with **User** and **Business Admin**; the builder's side is in the [developer manual](`+developerManual+`).

---
Next: [2 · The model you work in](dashboard:2 · The model you work in).`, 216),
	)
}

func baTheModel() modeltransfer.Dashboard {
	return baPage("ba-dash-2-model", "2 · The model you work in",
		baText(`# The model you work in

**Dashboards** (with the forms on them), **Roles** and **Access Rules** work on one model at a time: the one you have open. An application can hold several — *Getting started* holds the tour and a guide for each role — so before you change who sees what, check which model you are in: the **Model** in the bar at the top of the page names it, beside the **Revision**. The inbox and **History** are different: they list requests from every model at once.`, 156),

		baText(`## Switching models

The quickest way is the **Model** list at the top of **User › Dashboards**: pick another model and its dashboards appear. **Business Admin › Models** shows the same models with more detail: each card is an application, and inside it each model has a row with the name of its live revision. **Working here** marks the one you have open, and **Open** switches to another; the console reloads on that model's **User › Dashboards**.

The row marked **default** is the model everyone lands on until they choose another; a developer decides which one that is. Your own choice is remembered in this browser only.`, 164),

		baText(`## The live revision

A model changes in **revisions**: complete copies a developer works on, then makes live. The **Revision** in the bar at the top of the page names the live one. Everything under **User** — for you and for every business user — happens in the live revision.

As a business admin you cannot make a revision or choose which one is live; developers do that under **Developer › Models**, and tenant admins under **Tenant admin › Applications**. That is deliberate: what you look at is what everyone else looks at.`, 164),

		baText(`## What stays when a new revision goes live

Roles, their members and access rules are not part of any revision. When a developer makes a new revision from the live one, it carries your dashboard grants along, and your access rules follow each member and metric into it. A dashboard that is new in that revision belongs to no role yet: people in roles will not see it until you tick it.

---
Next: [3 · Who opens which dashboards](dashboard:3 · Who opens which dashboards).`, 180),
	)
}

func baWhoOpens() modeltransfer.Dashboard {
	return baPage("ba-dash-3-roles", "3 · Who opens which dashboards",
		baText(`# Who opens which dashboards

A model soon has more pages than any one person needs. A **role** is a named group of people, and you decide which dashboards each role may open. Workflow steps are assigned to roles as well, so a role is also how a request finds the people who decide it.`, 112),

		baText(`## First, the people

You cannot add people yourself. A tenant admin invites them under **Tenant admin › Users** with **Invite user**, choosing *business user* as the **Initial Role** and this workspace; a developer can invite people too. The people lists on **Roles** and **Access Rules** hold only this workspace's business users and business admins — in a new workspace, just you.`, 132),

		baText(`## Make a role and fill it

1. Open **Business Admin › Roles**, type a name into **New role name…** and press **Add role**.
2. Press **Configure** on the role. On the left, **Visible Dashboards** lists the dashboards of the model you have open, grouped by tag; these pages are under *guide*. Tick the ones the role may open, then press **Save dashboard access** in the bar that appears.
3. On the right, under **Members**, press **Add member**, choose a person from **— select user —** and press **Add**.

**Rename** and the bin sit on the same row. Rename with care: a workflow step names the role it is assigned to, so after a rename that step reaches nobody until a developer updates it.`, 216),

		baPicture(baGrants(), "Someone in no role sees every dashboard; someone in roles sees only what the roles are granted; admins and developers see every dashboard", 712, 214),

		baText(`## Who sees which dashboards

- Someone in **no role** sees every dashboard of the model. Roles are opt-in: nobody is narrowed until you put them in one.
- Someone in **one or more roles** sees only what those roles are granted, taken together — in every model of this workspace. When you put someone in a role, grant them what they need in each model they use.
- **Business admins, developers and tenant admins** always see every dashboard. Your own **User › Dashboards** cannot show you what a role member sees; ask them.

Roles decide pages, not numbers. To narrow the numbers on a page, use access rules.

---
Next: [4 · Who sees which numbers](dashboard:4 · Who sees which numbers).`, 264),
	)
}

func baWhoSees() modeltransfer.Dashboard {
	// Offices down the side, the three metrics across: the rows read like
	// the member list on Access Rules, so "the Oslo row" means one thing on
	// both screens. No shared selectors — there is nothing to select.
	gridProps, _ := json.Marshal(map[string]any{
		"sync_context": false,
		"default_view": map[string]any{"rows": []string{baDimOffice}, "cols": []string{"__metrics__"}, "context": []string{}},
	})
	kpi := func(metricID, title string, colStart, x int) modeltransfer.Widget {
		return modeltransfer.Widget{
			WidgetType: "metric_kpi", RefID: str(metricID), ColStart: colStart, ColSpan: 6,
			PosX: num(x), SizeW: num(440), SizeH: num(kpiHeight),
			Title: str(title), ShowTitle: true, Props: json.RawMessage(`{"kpi_context_mode":"total"}`),
		}
	}

	return baPage("ba-dash-4-access", "4 · Who sees which numbers",
		baText(`# Who sees which numbers

Roles decide which pages a person opens. **Access rules** decide, person by person, which numbers on those pages they can change, only read, or not see at all. A rule is set on a dimension member — an office, a region — or on a metric.`, 112),

		baText(`## Three levels

- **Write** — the default. The person can change every input number they can see.
- **Read** — they see the numbers but cannot change them. It applies to that member alone: *Read* on *Nordics* leaves Oslo and Stockholm open.
- **Hidden** — gone from grids, charts and selectors, and so is everything under it: hide *Iberia*, and Lisbon and Madrid go with it.

A metric takes the same three levels: *Read* on *spent* makes it read-only for that person in every grid.`, 196),

		baText(`## Totals for someone who cannot see everything

Their totals are made from what they can see: for someone who cannot see Lisbon, *Iberia* and *All offices* leave Lisbon out. Some calculated metrics read other cells — time functions, LOOKUP, the SUMIFS family — and cannot be recomputed that way; where one of those is built from something hidden, the person sees a blank "—", never a partial number. Chapter 15 of the [formulas manual](`+formulasManual+`) has the detail.`, 152),

		baText(`## A small model to try it on

This page has a model of its own: four offices in two regions, each with a *budget* and what has been *spent* so far. *remaining* is calculated from the two, and the region and company rows are totals.`, 108),

		[]modeltransfer.Widget{
			kpi(baMetBudget, "Budget, all offices", 1, 0),
			kpi(baMetRemaining, "Remaining, all offices", 7, 460),
		},

		[]modeltransfer.Widget{{
			WidgetType: "grid", RefID: str(baGridID), ColStart: 1, ColSpan: 12,
			PosX: num(0), SizeW: num(pageWidth), SizeH: num(420),
			Title: str(baGridName), ShowTitle: true, Props: json.RawMessage(gridProps),
		}},

		baText(`## Try it on yourself

Access rules apply to you too. **Read** is safe to try on yourself: you undo it on the same screen.

1. Open **Business Admin › Access Rules** and choose yourself under **User:**.
2. Under **office**, set **Oslo** to **Read** and press **Save rules** in the bar that appears.
3. Come back to **User › Dashboards** and open **4 · Who sees which numbers** again. Within half a minute, or at once if you reload the page, Oslo's *budget* and *spent* turn grey — **Read-only / total** in the grid's legend. You can read them; you cannot type into them. Read hides nothing, so no total moves.
4. Back on **Access Rules**, choose yourself under **User:** again, set **Oslo** to **Write** and press **Save rules**. Write is the default, so saving it removes the rule.

Leave **Hidden** alone on yourself: a member or metric hidden from you drops out of your own **Access Rules** list, so you could not set it back yourself. Another business admin could (a tenant admin can invite one) or, for a member, a developer through **Developer › AI Developer**.`, 320),

		baText(`## Rules belong to people

A rule is set for one person, not for a role, and it stays when a new revision goes live. The **User:** list holds this workspace's business users and business admins.

---
Next: [5 · Requests and history](dashboard:5 · Requests and history).`, 156),
	)
}

func baRequests() modeltransfer.Dashboard {
	return baPage("ba-dash-5-requests", "5 · Requests and history",
		baText(`# Requests, approvals and history

A developer designs the workflows; business users submit requests through them; you decide the requests and keep them moving. Until a developer publishes a workflow and someone submits a request, your inbox stays empty — this page is for when it fills.`, 112),

		baText(`## Workflow Inbox

**User › Workflow Inbox** lists the steps waiting for you: those assigned to business admins (or to developers or tenant admins, if your account is one too) and those assigned to a role you are in. Each card names the workflow and the step, who asked (**Requested by**), and what the request is about — an office, a region.

- Type a comment in the box. Some steps require one; their buttons stay disabled until you do.
- An approval step has **Approve** and **Reject**.
- A task step has one button, named by the developer: **Complete** unless they chose other words.
- Now and then, **Continue as true** and **Continue as false**: the engine could not decide a condition and asks you which way to go.

You decide requests; you do not submit them — business users do. An account that is also a developer or tenant admin, like the one that signed up, can submit as well.`, 296),

		baPicture(baRequest(), "A business user submits, you decide in the Workflow Inbox; approved numbers stay locked if the approval is its last step, rejected or cancelled ones are released", 860, 232),

		baText(`## Locks keep agreed numbers still

A request about part of the model — Oslo's budget, say — locks those numbers while it waits for your approval; while it is at a task (a planner correcting Oslo, say), they stay editable. Once approved, it keeps them locked so an agreed figure does not drift, but only if the approval is its last step: a step after it, such as a notification, releases them when it finishes. Anyone who types there while it is locked, you included, sees **Save failed** with the reason: the cell is locked because a workflow about it is awaiting approval or was approved. A rejected or cancelled request releases them.`, 152),

		baText(`## History

**Business Admin › History** shows the 50 most recent requests, newest first, with each step's decision and comment. It covers the application you have open, or your whole workspace if you have not opened one, and leaves out a developer's test runs. Narrow it with **Search by workflow name…** and **All statuses**. Each request has its own status list and **Apply**:

- **Cancelled** stops a stuck request: its open steps are skipped and its lock is released.
- **Running** reopens a finished one and puts its most recent step back in the inbox.

Open steps are listed under **Pending Actions**, where you can decide the ones assigned to you.`, 224),

		baText(`## Forms

Forms have no screen of their own: a developer places each one on a dashboard as a **Form** widget, and you work on its records there, under **User › Dashboards**, as business users do. The widget lists the records entered so far. **New record** adds one. The pencil edits one, and the status list on its row changes its **Status**: draft, submitted, approved or rejected. Changing the status is a decision in its own right: moving a record to submitted or approved can start a workflow, if a developer set one to listen for it.

**Export** downloads the records as Excel; **Import** reads CSV or Excel. **Sync to grid** posts the records into the model through the form's mappings. A form that is on no dashboard cannot be reached; ask a developer to place it.`, 188),

		baText(`## Notifications and cell history

The bell at the top of the page collects what workflows send you: notification steps, and reminders when a task you can act on falls due — if a tenant admin has turned on **Task reminders** under **Tenant admin › Notification delivery**. A notification about a request opens it in **History**.

To see who changed a number, right-click an input cell in a grid: its history lists every value it has held, when, by whom and how. Cell history is part of the Enterprise edition; on other editions the panel says so. See [cell history](`+publicDocs+`CELL_HISTORY.md) and [editions](`+publicDocs+`LICENSING.md).

---
That is the business admin guide. For how models, dashboards and workflows are built, see the [developer manual](`+developerManual+`).`, 232),
	)
}

// ── Diagrams ─────────────────────────────────────────────────────────────────

// baDown draws a short downward arrow from (x,y) for length px.
func baDown(x, y, length int) string {
	return `<line x1="` + itoa(x) + `" y1="` + itoa(y) + `" x2="` + itoa(x) + `" y2="` + itoa(y+length-7) +
		`" stroke="` + muted + `" stroke-width="1.5"/><path d="M` + itoa(x) + ` ` + itoa(y+length) + `l-4-8h8z" fill="` + muted + `"/>`
}

// baNote writes one muted line of text, left-aligned at (x,y).
func baNote(x, y int, s string) string {
	return `<text x="` + itoa(x) + `" y="` + itoa(y) + `" dominant-baseline="middle" font-size="13" fill="` + muted + `">` + s + `</text>`
}

// baLayers: the three things that decide what a person can do, each
// narrowing the one before.
func baLayers() string {
	s := svgHead(860, 250)
	rows := []struct{ name, where, what, how, note string }{
		{"Roles", "Business Admin › Roles", "Which dashboards", "a person can open", "admins and developers see all"},
		{"Access rules", "Business Admin › Access Rules", "Which numbers", "Write · Read · Hidden", "set per person, you included"},
		{"Open requests", "running or approved workflows", "Which numbers are locked", "while it waits, and once approved", "after approval, if that is the last step"},
	}
	for i, r := range rows {
		y := 10 + i*84
		s += box(10, y, 240, 62, r.name, r.where, true)
		s += arrow(258, y+31, 46)
		s += box(312, y, 250, 62, r.what, r.how, false)
		s += baNote(582, y+31, r.note)
		if i < len(rows)-1 {
			s += baDown(130, y+64, 18)
		}
	}
	return s + `</svg>`
}

// baGrants: who sees which dashboards.
func baGrants() string {
	s := svgHead(712, 214)
	rows := []struct {
		who, whoSub, sees, seesSub string
		strong                     bool
	}{
		{"Person in no role", "the default", "Every dashboard", "of the model", false},
		{"Person in one or more roles", "you put them there", "Only granted dashboards", "all their roles' grants, together", true},
		{"Admins and developers", "business admin, developer, tenant admin", "Every dashboard", "roles never narrow them", false},
	}
	for i, r := range rows {
		y := 10 + i*68
		s += box(10, y, 300, 58, r.who, r.whoSub, false)
		s += arrow(318, y+29, 46)
		s += box(372, y, 330, 58, r.sees, r.seesSub, r.strong)
	}
	return s + `</svg>`
}

// baRequest: what happens to a request, and to the numbers it is about.
func baRequest() string {
	s := svgHead(860, 232)
	s += box(10, 84, 190, 64, "Submitted", "by a business user", false)
	s += arrow(208, 116, 40)
	s += box(256, 84, 220, 64, "Your decision", "User › Workflow Inbox", true)
	s += `<text x="366" y="68" text-anchor="middle" font-size="12" fill="` + muted + `">numbers locked while it waits</text>`
	s += `<text x="366" y="170" text-anchor="middle" font-size="12" fill="` + muted + `">stuck? cancel it in History</text>`
	s += `<line x1="476" y1="116" x2="512" y2="116" stroke="` + muted + `" stroke-width="1.5"/>`
	s += `<line x1="512" y1="45" x2="512" y2="187" stroke="` + muted + `" stroke-width="1.5"/>`
	s += arrow(512, 45, 40)
	s += arrow(512, 187, 40)
	s += box(560, 13, 290, 64, "Approved", "stay locked if approval is the last step", false)
	s += box(560, 155, 290, 64, "Rejected or cancelled", "numbers are released", false)
	return s + `</svg>`
}

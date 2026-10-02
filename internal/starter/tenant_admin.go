package starter

import (
	"encoding/json"
	"time"

	"github.com/mavericks-engine/mavericks/internal/modeltransfer"
)

// TenantAdminGuideName is the model TenantAdminGuide creates.
const TenantAdminGuideName = "Tenant admin guide"

// TenantAdminGuide is the tenant admin's guide: what the role owns (the
// tenant, not what is built in it), people and access, models as packages,
// and the tenant-wide settings — audit, delivery, AI keys, sign-in,
// branding — each with the edition it needs. It ends on a small live
// checklist, so the one thing the guide asks the reader to do is recorded
// in an ordinary grid whose totals the platform keeps.
//
// Every label and sidebar path in the prose is the console's own
// (web/src/consoles/platform-admin/section.tsx, admin/UsersPanel.tsx,
// platform-admin/PlatformAdminConsole.tsx, ee/*), and every edition gate is
// pkg/license/features.go's. Private identifiers carry the "ta" prefix so
// the guides never collide.
func TenantAdminGuide() modeltransfer.Package {
	return modeltransfer.Package{
		Format: modeltransfer.PackageFormat, Version: modeltransfer.PackageVersion, ExportedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		ModelName: TenantAdminGuideName, StorageType: "oltp", RevisionName: RevisionName, IncludeData: true,
		Dimensions: []modeltransfer.Dimension{taStepDimension()},
		Metrics: []modeltransfer.Metric{
			// 1 when a step is done, 0 while it is not: a sum is the number of
			// steps done, per group and in all.
			{ID: taMetDone, Name: "done", IsInput: true, StorageType: "oltp", AggRule: "sum", Format: "number"},
		},
		Grids: []modeltransfer.Grid{{
			ID: taGridSteps, Name: "Setup steps",
			Metrics:    []modeltransfer.GridMetric{{MetricID: taMetDone, SortOrder: 0}},
			Dimensions: []modeltransfer.GridDimension{{DimensionID: taDimStep}},
		}},
		Dashboards: []modeltransfer.Dashboard{
			taWhatYouOwn(), taPeople(), taPackages(), taRecords(), taChecklist(),
		},
		Facts: taFacts(),
	}
}

// Placeholder ids: Import allocates real ones and remaps every reference.
const (
	taDimStep   = "ta-dim-step"
	taMetDone   = "ta-m-done"
	taGridSteps = "ta-grid-steps"
)

// taGroups is the checklist: four groups of first steps. The labels are
// what the grid shows, and page 5's list names each one the same way.
var taGroups = []struct {
	code, label string
	steps       [][2]string // code, label
}{
	{"PEOPLE", "People", [][2]string{{"INVITE", "Invite a colleague"}, {"REACH", "Choose what they reach"}}},
	{"SAFETY", "Data safety", [][2]string{{"EXPORT", "Export a model with its data"}, {"AUDIT", "Filter the audit log"}}},
	{"DELIVERY", "Delivery", [][2]string{{"EMAIL", "Turn on e-mail delivery"}, {"REMIND", "Decide on task reminders"}}},
	{"IDENTITY", "Identity", [][2]string{{"EDITION", "Check your edition"}, {"SSO", "Single sign-on (Enterprise)"}}},
}

// taStepCount is how many steps the checklist has; page 5 and the KPI's
// title say it in words.
func taStepCount() int {
	n := 0
	for _, g := range taGroups {
		n += len(g.steps)
	}
	return n
}

func taStepDimension() modeltransfer.Dimension {
	d := modeltransfer.Dimension{ID: taDimStep, Name: "step", AggRule: "sum"}
	d.Members = append(d.Members, modeltransfer.Member{ID: "ta-s-ALL", Code: "ALL", Label: "All steps", SortOrder: 0})
	order := 1
	for _, g := range taGroups {
		d.Members = append(d.Members, modeltransfer.Member{ID: "ta-s-" + g.code, Code: g.code, Label: g.label, ParentMemberID: str("ta-s-ALL"), SortOrder: order})
		order++
		for _, s := range g.steps {
			d.Members = append(d.Members, modeltransfer.Member{ID: "ta-s-" + s[0], Code: s[0], Label: s[1], ParentMemberID: str("ta-s-" + g.code), SortOrder: order})
			order++
		}
	}
	return d
}

// taFacts starts every step at 0, so the grid shows where to type and the
// totals read 0 rather than blank before anything is done.
func taFacts() []modeltransfer.Fact {
	var out []modeltransfer.Fact
	for _, g := range taGroups {
		for _, s := range g.steps {
			members, _ := json.Marshal(map[string]string{taDimStep: s[0]})
			out = append(out, modeltransfer.Fact{MetricID: taMetDone, DimMembers: members, Value: 0})
		}
	}
	return out
}

// ── Layout ───────────────────────────────────────────────────────────────────

// taGap is the space between stacked widgets, as on the tour's pages.
const taGap = 24

// taPiece is one widget of a page and its height; taStack places the pieces
// top-down, so changing one height moves everything below it.
type taPiece struct {
	h    int
	make func(y, sortOrder int) modeltransfer.Widget
}

func taText(body string, h int) taPiece {
	return taPiece{h, func(y, s int) modeltransfer.Widget { return text(body, y, h, s) }}
}

func taPicture(svg, alt string, w, h int) taPiece {
	return taPiece{h, func(y, s int) modeltransfer.Widget { return picture(svg, alt, y, w, h, s) }}
}

// taLive places a live widget (grid, KPI) at the left edge.
func taLive(w modeltransfer.Widget) taPiece {
	return taPiece{*w.SizeH, func(y, s int) modeltransfer.Widget {
		w.PosY, w.SortOrder = num(y), s
		return w
	}}
}

func taStack(pieces ...taPiece) []modeltransfer.Widget {
	out := make([]modeltransfer.Widget, 0, len(pieces))
	y := 0
	for i, p := range pieces {
		out = append(out, p.make(y, i))
		y += p.h + taGap
	}
	return out
}

func taDashboard(id, name string, pieces ...taPiece) modeltransfer.Dashboard {
	return modeltransfer.Dashboard{ID: id, Name: name, Tags: []string{"guide"}, Category: "Getting started", Widgets: taStack(pieces...)}
}

// taDoc links a public document in the repository's docs/ folder.
func taDoc(label, file string) string { return "[" + label + "](" + publicDocs + file + ")" }

// ── Pages ────────────────────────────────────────────────────────────────────

func taWhatYouOwn() modeltransfer.Dashboard {
	return taDashboard("ta-dash-1-own", "1 · What you own",
		taText(`# What you own

As tenant admin you look after the tenant itself, not what is built inside it: who comes in, what each person reaches, where models go, what happened and when, and how the platform reaches people outside the console. Developers build the models; business admins decide who sees what inside one. Five short pages cover your part.`, 136),

		taText(`## How the pieces fit

Your company is one **tenant**. Everything else sits inside it.`, 88),

		taPicture(taOwnership(), "A tenant holds applications, an application holds models, a model holds revisions, one of them live; people get business roles in a workspace", 860, 200),

		taText(`- **Workspace** — where people are given business roles. A tenant that began with self-service sign-up has one, named *Default*.
- **Application** — a group of related models. At sign-up the tenant gets one, *Getting started*, holding the tour and the guides.
- **Model** — the dimensions, metrics, grids, dashboards, forms and workflows that belong together.
- **Revision** — a complete copy of a model. One revision is live; a developer prepares the next and presses **Set active** when it is ready.`, 140),

		taText(`## Your screens

They are the **Tenant admin** group in the sidebar:

- **Applications** — your tenant's applications, models and revisions: export, import and delete
- **Users** — invite people, give them roles, decide what they reach
- **Audit Log** — who did what, and when
- **Usage** — activity and size over a period *(Enterprise)*
- **Notification delivery** — e-mail, webhook and task reminders
- **AI keys** — one AI provider key for the whole tenant *(Enterprise)*
- **Single sign-on** — sign in with your company's identity provider *(Enterprise)*
- **Provisioning (SCIM)** — let your directory create and deactivate accounts *(Enterprise)*
- **Branding** — your name, colour and logo on the console *(Commercial and Enterprise)*`, 332),

		taText(`## Edition and plan

Two different things. The **edition** is what this deployment can do — *Community*, *Commercial* or *Enterprise*, decided by its licence key. Click your initials at the top right: the account menu names the edition under your roles, and holds the **Theme** switch (**Light**, **Dark** or **System**), which your account remembers.

The **plan** is how much your tenant may use — storage, for example, or AI messages a day. Whoever runs the platform sets it; you cannot change it yourself. Its name is on your tenant's card under **Tenant admin › Applications**, and below it the storage your data uses — on a plan with a storage limit, also how much is left, measured every few minutes. If the tenant goes over a limit, it turns read-only and a banner at the top of the console says why. Deleting still works, so removing what you no longer need is how you make room.

More: `+taDoc("editions and licensing", "LICENSING.md")+` · `+taDoc("plans", "PLANS_AND_SIGNUP.md")+``, 240),

		taText(`## Reading this guide

These pages are read in **Run › Dashboards**, a screen that comes with a business role — the sign-up account holds business admin for that reason. A person who is only a tenant admin has the **Tenant admin** group and no dashboards; give them a business role in a workspace as well (page 2 shows how) and they can read these pages too. To move between the guides, open **Business Admin › Models** (or **Run › Models**) and press **Open** beside a model.

---
Next: **2 · People and access** — pick its tab at the top of this page.`, 200),
	)
}

func taPeople() modeltransfer.Dashboard {
	return taDashboard("ta-dash-2-people", "2 · People and access",
		taText(`# People and access

What a person sees in the console depends on their roles: each role adds its own groups to the sidebar. This page covers inviting someone, choosing their role, deciding what they reach, and taking access away. All of it happens under **Tenant admin › Users**.`, 112),

		taText(`## Inviting someone

1. Press **Invite user**.
2. Fill in **Email**, **First name** and **Last name** — all three are needed.
3. Choose an **Initial Role**: developer, business admin or business user. For a business role, also choose the **Workspace**: a business role grants nothing until it has one.
4. Press **Create user**.

They get an e-mail asking them to set a password, and its link works for three days. The address is their sign-in and cannot be changed later, so check it first. If the invitation cannot be sent, no account is created and the screen says why.`, 244),

		taText(`## Which role to give

- **Business user** — enters numbers, fills in forms, submits requests, reads dashboards.
- **Business admin** — approves requests, and decides who sees which dashboards and which data inside a model.
- **Developer** — builds models: dimensions, metrics, grids, dashboards, forms, workflows, integrations.

You can give these three here, but not your own role; a platform admin — whoever runs this deployment — can make another tenant admin. A developer can invite people too, with the two business roles only, and cannot decide which applications or models they reach.`, 196),

		taPicture(taGrants(), "Who may grant what: a platform admin every role, a tenant admin the developer and business roles plus application and model access, a developer the business roles", 880, 214),

		taText(`## Deciding what someone reaches

Press the pencil (**Edit**) on a person's row. Under **Resource access**:

- **Add workspace** places them in a workspace with a role — choose the workspace and the role, then press **Grant**.
- Below it, every application and model has a checkbox. With none ticked, the person reaches all of them, and the **Access** column says *All apps/models*. Tick one and they reach only what is ticked.

Under **Platform roles**, **+ Add...** then **Add** gives someone the developer role, which needs no workspace.

What a person sees *inside* a model — which dashboards, which rows of data — is the business admin's to decide, under **Business Admin › Roles** and **Business Admin › Access Rules**. The account that signed the tenant up holds that role as well; the *Business admin guide* model explains it.`, 276),

		taText(`## Taking access away

- **One role** — in the edit row, click the × on the role's chip.
- **An application or a model** — tick what the person keeps. An empty list means *everything*, so unticking the last box lifts the restriction rather than closing it.
- **Everything** — press the bin icon on their row and confirm **Delete user**. It revokes all their access.

You cannot delete your own account, or remove the last role that lets you manage users; another administrator has to. Each of these changes is recorded in the **Audit Log** (page 4).

---
Next: **3 · Models as packages**.`, 264),
	)
}

func taPackages() modeltransfer.Dashboard {
	return taDashboard("ta-dash-3-packages", "3 · Models as packages",
		taText(`# Models as packages

A model can leave your tenant as one file and arrive somewhere else as a new model: a copy kept safe, a model handed to another tenant, a revision kept before it is deleted. Export and import belong to the tenant admin; a developer does not have these buttons. All of it is under **Tenant admin › Applications**.`, 136),

		taText(`## Exporting

Beside each model's name there are three icons; hover one to see its name.

- **Download** — *Export model (active revision)*: the live revision with its data, as a `+tick+`.mavericks-model.json`+tick+` file
- **Package** — *Download standalone deployment package*: the same model and data with the database migrations and a manifest, as a `+tick+`.tar.gz`+tick+`, for a deployment of its own
- **Bin** — *Delete model*, below

Under **Revisions**, each revision has two export icons of its own: *with data* (its values and form records) and *definitions only* (the structure with no numbers — the usual choice when you hand a model to someone else).`, 248),

		taText(`## Importing

Under an application, press **Import model** and choose a `+tick+`.mavericks-model.json`+tick+` file. It arrives as a new model with a single revision, which is live, and its calculated values are worked out as it lands. Your plan's limits are checked first: an import that would not fit is refused before anything is created.`, 132),

		taText(`## What a package carries

- **It carries** the model: dimensions, metrics, grids, dashboards, forms, workflows, triggers and integrations (without stored connection credentials) — and, with data, the values and form records.
- **It does not carry** people, their roles, business roles, access rules, or the tenant's own settings: notifications, AI keys, sign-in and branding. Set those up again where the model lands.`, 156),

		taText(`## Which model people open first

When an application holds several models, people land on its *business default*. A developer chooses it under **Build › Models** with **Set as business default**. Each person can move to another model for themselves with **Open**, under **Business Admin › Models** or **Run › Models**.`, 132),

		taText(`## Deleting

Applications and models are deleted here and nowhere else. The bin beside a model (**Delete model**) removes it with all its revisions; the bin beside an application (**Delete application**) removes it with all its models; the bin beside a revision that is not live (**Delete revision**) removes just that revision. None of it can be undone, so export first if you might want it back.

---
Next: **4 · Records, delivery and sign-in**.`, 180),
	)
}

func taRecords() modeltransfer.Dashboard {
	return taDashboard("ta-dash-4-records", "4 · Records, delivery and sign-in",
		taText(`# Records, delivery and sign-in

The rest of the **Tenant admin** group holds settings for the whole tenant, and some of it depends on the edition this deployment runs. A screen your edition does not include stays in the sidebar and says which edition it needs, so nothing is hidden from you.`, 112),

		taPicture(taEditions(), "What each edition includes in the Tenant admin group", 870, 236),

		taText(`## Audit Log

Changes that matter are recorded as they happen: data and model changes, users and roles, exports and imports. **Tenant admin › Audit Log** shows the latest 200 events. Type into the filter — `+tick+`user.created`+tick+`, `+tick+`model.exported`+tick+`, a person's name — to narrow them; each row names who acted, on what, and the details.

On Enterprise an **Export** (CSV or JSON Lines, for any date range) and a **Retention** setting sit above the table. Retention deletes older events rather than archiving them, so export before you shorten it. `+taDoc("Audit export", "AUDIT_EXPORT.md")+``, 188),

		taText(`## Notification delivery

Notifications always reach the bell at the top of the console. **Tenant admin › Notification delivery** sends them further, on every edition:

- **Send notifications by e-mail** — to each recipient's own address. Where the deployment has a mail relay, **Send me a test e-mail** proves it works; where it has none, the screen says so, and whoever runs the platform has to add one.
- **Send notifications to a webhook** — each one posted as JSON to your **Endpoint URL**, and signed when you set a **Signing secret**.
- **Remind assignees about tasks that come due** — for workflow steps a developer gave a due time (SLA hours), optionally some hours before.

Press **Save settings** to keep them. `+taDoc("Notifications", "NOTIFICATIONS.md")+``, 248),

		taText(`## AI keys — Enterprise

One AI provider key for the whole tenant, used by every developer's **AI Developer**. You can also require it, so that no personal key is ever used. Without it, each developer adds a key of their own under **Build › AI Developer**, behind the settings (gear) icon. `+taDoc("AI keys", "AI_KEYS.md")+`

## Single sign-on and Provisioning (SCIM) — Enterprise

**Single sign-on** registers your company's OpenID Connect or SAML 2.0 identity provider, so people sign in with their company account; you choose the e-mail domains allowed and the role a new account gets. If the deployment is not ready for it, the screen says what its operator has to set. **Provisioning (SCIM)** issues a token with which your directory — Entra ID, Okta and similar — creates and deactivates accounts. `+taDoc("Single sign-on and SCIM", "SSO_SCIM.md")+``, 240),

		taText(`## Branding — Commercial and Enterprise

Your product name, tagline, colour, logo and favicon on the console and the sign-in page, a sender name for e-mail, and a custom domain, whose DNS and certificate are set up by whoever runs the platform. `+taDoc("White-labelling", "WHITE_LABEL.md")+`

## Usage — Enterprise

What your tenant holds now: applications and models, revisions, data rows, form records and storage. Plus what happened over the last 7, 30 or 90 days: active users, workflow and integration runs, AI messages and audit events. `+taDoc("Usage analytics", "USAGE_ANALYTICS.md")+`

---
Next: **5 · Your setup checklist**.`, 244),
	)
}

func taChecklist() modeltransfer.Dashboard {
	gridProps, _ := json.Marshal(map[string]any{
		"sync_context": false,
		// Steps down the side, the one metric across: the grid's own
		// default (metrics in rows) would lay every step out as a column.
		"default_view": map[string]any{
			"rows": []string{taDimStep}, "cols": []string{"__metrics__"}, "context": []string{},
		},
	})
	return taDashboard("ta-dash-5-checklist", "5 · Your setup checklist",
		taText(`# Your setup checklist

Eight first steps, in four groups. Type **1** beside a step and press Enter when it is done — or when it does not apply to you, such as single sign-on on an edition without it. Nobody types the totals: each group adds up its own steps, and the card below counts them all.`, 112),

		// Sized to what each shows at 884 px: the titled tile renders 161 px
		// (a KPI's height is a minimum, so a shorter one only misleads the
		// designer); the grid's title, header, 13 rows and legend take 604.
		taLive(modeltransfer.Widget{
			WidgetType: "metric_kpi", RefID: str(taMetDone), ColStart: 1, ColSpan: 6,
			PosX: num(0), SizeW: num(440), SizeH: num(kpiHeight),
			Title: str("Setup steps done, of " + itoa(taStepCount())), ShowTitle: true, Props: json.RawMessage(`{"kpi_context_mode":"total"}`),
		}),

		taLive(modeltransfer.Widget{
			WidgetType: "grid", RefID: str(taGridSteps), ColStart: 1, ColSpan: 12,
			PosX: num(0), SizeW: num(pageWidth), SizeH: num(640),
			Title: str("Setup steps"), ShowTitle: true, Props: json.RawMessage(gridProps),
		}),

		taText(`## Where each step is done

- **Invite a colleague** — **Tenant admin › Users**, **Invite user**
- **Choose what they reach** — **Edit** (the pencil) on their row, then **Resource access**
- **Export a model with its data** — **Tenant admin › Applications**, the download icon beside a model
- **Filter the audit log** — **Tenant admin › Audit Log**; find your invitation, `+tick+`user.created`+tick+`
- **Turn on e-mail delivery** — **Tenant admin › Notification delivery**
- **Decide on task reminders** — the same screen, under **Task reminders**
- **Check your edition** — your initials at the top right
- **Single sign-on (Enterprise)** — **Tenant admin › Single sign-on**

---

That is the guide. It is an ordinary model: change it, or delete it under **Tenant admin › Applications** when you no longer need it. Your colleagues have guides of their own — the *Developer guide* and *Business admin guide* models — and developers also have the [developer manual](`+developerManual+`).`, 364),
	)
}

// ── Diagrams ─────────────────────────────────────────────────────────────────

// taOwnership: what a tenant holds, and where people are given roles.
func taOwnership() string {
	s := svgHead(860, 200)
	s += box(10, 30, 170, 64, "Tenant", "your company", true)
	s += arrow(186, 62, 38)
	s += box(230, 30, 170, 64, "Application", "Getting started", false)
	s += arrow(406, 62, 38)
	s += box(450, 30, 170, 64, "Model", "Tenant admin guide", false)
	s += arrow(626, 62, 38)
	s += box(670, 30, 180, 64, "Revision", "First revision", false)
	s += `<rect x="800" y="21" width="42" height="18" rx="9" fill="#dcfce7" stroke="#16a34a"/><text x="821" y="30" text-anchor="middle" dominant-baseline="middle" font-size="10" font-weight="700" fill="#15803d">live</text>`
	for _, c := range []struct {
		x    int
		text string
	}{{315, "a group of models"}, {535, "travels as one file"}, {760, "one of them is live"}} {
		s += `<text x="` + itoa(c.x) + `" y="116" text-anchor="middle" font-size="12" fill="` + muted + `">` + c.text + `</text>`
	}
	s += `<line x1="95" y1="94" x2="95" y2="128" stroke="` + muted + `" stroke-width="1.5" stroke-dasharray="4 3"/>`
	s += box(10, 128, 170, 56, "Workspace", "Default", false)
	s += `<text x="196" y="156" dominant-baseline="middle" font-size="12" fill="` + muted + `">where people are given business roles</text>`
	return s + `</svg>`
}

// taGrants: who may give which role, and who decides application and model
// access (assignableRoles and canManageResourceAccess in the gateway).
func taGrants() string {
	s := svgHead(880, 214)
	const chipX, chipW, chipStep = 262, 116, 124
	s += `<text x="` + itoa(chipX+(4*chipStep-10)/2) + `" y="16" text-anchor="middle" font-size="12" fill="` + muted + `">can give the role</text>`
	s += `<text x="` + itoa(chipX+4*chipStep+chipW/2) + `" y="16" text-anchor="middle" font-size="12" fill="` + muted + `">can restrict</text>`
	chips := []string{"tenant admin", "developer", "business admin", "business user", "apps and models"}
	rows := []struct {
		who, sub string
		strong   bool
		from, to int // the chips this role reaches, inclusive
	}{
		{"Platform admin", "runs the deployment", false, 0, 4},
		{"Tenant admin", "you", true, 1, 4},
		{"Developer", "builds models", false, 2, 3},
	}
	for r, row := range rows {
		y := 32 + r*64
		s += box(10, y, 200, 46, row.who, row.sub, row.strong)
		s += arrow(216, y+23, chipX+row.from*chipStep-8-216)
		for c := row.from; c <= row.to; c++ {
			x := chipX + c*chipStep
			dash := "" // access is a decision about reach, not a role
			if c == 4 {
				dash = ` stroke-dasharray="4 3"`
			}
			s += `<rect x="` + itoa(x) + `" y="` + itoa(y+8) + `" width="` + itoa(chipW) + `" height="30" rx="15" fill="` + fill + `" stroke="` + accent + `" stroke-width="1.5"` + dash + `/>`
			s += `<text x="` + itoa(x+chipW/2) + `" y="` + itoa(y+23) + `" text-anchor="middle" dominant-baseline="middle" font-size="12" font-weight="600" fill="` + ink + `">` + chips[c] + `</text>`
		}
	}
	return s + `</svg>`
}

// taEditions: the Tenant admin group by the edition that includes it
// (pkg/license/features.go: white_label from Commercial, the rest of the
// gated features Enterprise).
func taEditions() string {
	s := svgHead(870, 236)
	cols := []struct {
		title, sub string
		items      []string
		strong     bool
	}{
		{"Every edition", "Community included", []string{"Applications", "Users", "Audit Log (the table)", "Notification delivery"}, false},
		{"Commercial and Enterprise", "white-labelling", []string{"Branding"}, false},
		{"Enterprise", "oversight and identity", []string{"Usage", "AI keys", "Single sign-on", "Provisioning (SCIM)", "Audit export and retention"}, true},
	}
	for i, c := range cols {
		x := 10 + i*290
		bg, stroke := paper, line
		if c.strong {
			bg, stroke = fill, accent
		}
		s += `<rect x="` + itoa(x) + `" y="10" width="270" height="216" rx="10" fill="` + bg + `" stroke="` + stroke + `" stroke-width="1.5"/>`
		s += `<text x="` + itoa(x+20) + `" y="40" font-size="15" font-weight="700" fill="` + ink + `">` + c.title + `</text>`
		s += `<text x="` + itoa(x+20) + `" y="60" font-size="12" fill="` + muted + `">` + c.sub + `</text>`
		s += `<line x1="` + itoa(x+20) + `" y1="76" x2="` + itoa(x+250) + `" y2="76" stroke="` + line + `" stroke-width="1"/>`
		for j, item := range c.items {
			y := 102 + j*26
			s += `<circle cx="` + itoa(x+26) + `" cy="` + itoa(y) + `" r="3" fill="` + accent + `"/>`
			s += `<text x="` + itoa(x+38) + `" y="` + itoa(y) + `" dominant-baseline="middle" font-size="13" fill="` + ink + `">` + item + `</text>`
		}
	}
	return s + `</svg>`
}

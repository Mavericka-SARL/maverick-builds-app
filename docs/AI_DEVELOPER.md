# The AI Developer: what it can build, and what stays human

> **Last verified:** 2026-11-12

The AI Developer is the assistant inside the developer console. Its rule is
parity: it can build, change and remove what a developer builds, changes and
removes through the screens, under the same checks — with one deliberate
exception below, and the short list under [What stays human](#what-stays-human)
that it leaves to people. It reads through **read tools** it calls freely, and
writes only by **proposing** an ordered plan that the developer confirms.
Nothing is written until then.

**Where its writes land.** A session is its draft (owner decision,
2026-11-13, replacing one draft shared by every session of 2026-10-24): the
session's first confirmed proposal copies the developer's working revision
(else the active one) into a new AI draft, and every later proposal of the
session writes there until the developer promotes or discards it. A new
session starts a fresh draft; there is no separate "new draft" action. The
session goes on as one thread — no per-session call cap by default
(`AI_MAX_CALLS_PER_SESSION` sets one per deployment; the developer's call
budget, the daily cap and `AI_SESSION_TOKEN_LIMIT` still apply), its history
settled as it grows. Promoting makes the draft the
developer's **working** revision, not the live one: making a revision active
is a separate step (**Set active** in Developer › Developer Models; owner decision,
2026-10-10). Data the assistant imports from a spreadsheet
attached to the chat lands in the draft too. Three kinds of write are not revision-scoped, exactly as in the
console, so they take effect on confirmation and discarding the draft does not
undo them:

- business roles (`create_business_role`, `update_business_role`,
  `delete_business_role`) — the tenant's roles, as on the Roles screens;
- user access rules (`set_user_access_rules`, the exception below);
- record posting (`backfill_form_integration`, and the re-post
  `update_form_integration` runs) — input values posted into the working
  revision.

`set_role_dashboards` writes live grant rows too, but on the working
revision's dashboards: grants on the draft's dashboards matter once it is
made active.

**The exception.** `set_user_access_rules` is a business-admin capability
given to the assistant at the owner's direction (2026-08-26). Access rules are
not revision-scoped, so it writes live `identity.user_access_rule` rows
against the active revision: they take effect when the proposal is confirmed,
not when the draft is promoted, and discarding the draft does not undo them.
It replaces only the user's member rules that resolve in the active revision;
metric and button rules, and rules on members no longer in the active revision
(which still restrict older revisions), are kept.

**Held to the same limits.** The tenant's edition limits on metrics per model and
members per dimension refuse the assistant where they refuse the developer:
the gateway hands the executor its own checks (`aiassistant.Hooks`), and form
posting runs the gateway's own posting code the same way.

## Read tools

| Tool | Answers |
|---|---|
| `get_model_summary`, `list_metrics`, `list_dimensions`, `list_revisions`, `list_users` | the model as the developer console shows it |
| `list_grids` | each grid's metrics and dimensions, with a dimension's display level |
| `list_dashboards` | the folders, then each dashboard's folder, tags and widgets — each widget's id, type, what it shows, place and size |
| `describe_dashboard` | one dashboard of the working revision as its users see it: the business roles that may open it, its selector bar, then each widget in reading order — a text widget's whole Markdown with where each link leads (a dashboard of this model is checked in the working revision; another model's is named as written), what each other widget reads, a button's label, and its `widget_props` with the metrics, dimensions and grids their ids name. An image's picture is left out. The assistant reads it to explain how an app is used, and before changing a text widget's content, which `update_dashboard_widget` replaces whole |
| `validate_formulas`, `check_grid_completeness` | the audit checks the console runs |
| `list_workflows`, `get_workflow` | definitions; one in full — steps, context, subject, rules, and the Validate verdict |
| `validate_workflow` | the editor's Validate button, for stored or not-yet-proposed steps |
| `list_workflow_roles` | what `assignee_roles` may contain: built-in role codes (the tenant roles and `platform_admin`) and the tenant's business roles, with each role's member count and the dashboards of the revision it may open |
| `list_automation_rules` | what starts each workflow |
| `list_forms`, `get_form` | tables of records (the Records tab); one in full — fields, "own records only", integrations, record count |
| `query_records` | a table's records, read-only, through the records query as the developer: a page (filters, search, sort) or totals (count, count_distinct, sum, avg, min, max, grouped by fields or a date period) |
| `list_form_integrations` | which field of a table posts into which metric |
| `list_integrations` | every data integration — Excel/CSV imports, Google Sheets, REST API, data exports — with its target, column map or export spec, and last run |
| `preview_file_import` | a dry run of importing an attached spreadsheet: its first rows as read, its columns and first rows after the reshape and column map, every row that would fail and why, what would be imported. Writes nothing |
| `read_attached_sheet` | one sheet of an attached workbook: any rows as stored, the whole formulas of those rows (compressed into ranges sharing a formula), its layout, drop-down lists, highlights and comments; or any lines of an attached CSV file, with its line count. Writes nothing |
| `compare_with_sheet` | one row of an attached sheet against the metric it became, period by period along the time dimension at the members named for its other dimensions: how many periods match and each that differs, sheet value and model value. A percentage metric matches the sheet's fraction. The plan check also warns when a plan creates input metrics that hold no value and nothing loads |
| `prepare_converted_file` | saves an attached spreadsheet reshaped and column-mapped into the import layout, for the developer to download as CSV or Excel from the chat. Imports nothing |
| `preview_export` | an export spec rendered against the grid's current values — columns, first rows, row count — or every problem with the spec. Saves nothing |
| `get_integration`, `list_integration_runs` | a REST API connector in full (typed config, connection, schedule, switches, tested state) and run history with error codes |
| `test_integration` | the developer's own Test route, in-process as the developer: a GET request, an SFTP file or a model link, run now, writing nothing; answers the outcome and the first records. A test of a mutating request is refused here (it is a plan step) |
| `list_model_link_sources`, `list_connections` | the models a link may read (the developer's own route); the application's sign-in connections, never their secrets |
| `attach_google_sheet` | the Sheets import's own fetch route, as the developer; attaches the sheet to the chat as CSV for `preview_file_import` |
| `read_manual` | sections of the Developer and Formulas manuals (`internal/manuals`, embedded from `docs/`) matching a query, or their contents |

## Write tools (through `propose_actions`)

The list is `aiassistant.WriteToolNames`; the tool schema's enum and the
prompt are built from it, and a test checks that the executor runs every tool
on it.

| Area | Tools | Mirrors |
|---|---|---|
| Metrics | `create_metric`, `update_metric`, `delete_metric` | the Metrics screen |
| Dimensions | `create_dimension`, `update_dimension`, `delete_dimension` | the Dimensions screen; delete refused with `DIMENSION_IN_USE` or while a grouping groups it |
| Members | `add_dimension_member`, `update_dimension_member`, `delete_dimension_member`, `generate_time_members`, `reorder_dimension_members` | member add, edit (code, label, parent, period dates, properties), delete (refused with `MEMBER_IN_USE`), **Generate periods**, and the order of one level of members (`PUT …/members/order`; not on a time dimension) |
| Properties | `add_dimension_property`, `update_dimension_property`, `delete_dimension_property` | the Dimension Properties panel |
| Grids | `create_grid`, `update_grid`, `delete_grid`, `add_grid_metric`, `remove_grid_metric`, `reorder_grid_metrics`, `add_grid_dimension`, `update_grid_dimension`, `remove_grid_dimension` | the Grids screen, including a dimension's display level |
| Dashboards | `create_dashboard_folder`, `update_dashboard_folder`, `delete_dashboard_folder`, `create_dashboard`, `update_dashboard`, `delete_dashboard`, `add_dashboard_widget`, `update_dashboard_widget`, `delete_dashboard_widget` | the Dashboards screen and its designer; `selector_bar` on `create_dashboard` and `update_dashboard` is the designer's **Selector bar**; a `board` widget (`widget_props.board`) and a records widget's `record_filters` / `record_columns` / `record_sort` are checked against the table's fields as the designer's are; a KPI tile in `bar` mode may also carry a `kpi_scope` |
| Integrations | `create_file_integration`, `import_file_data`, `create_export_integration`, `update_integration`, `delete_integration` | the Integrations tab: the Import Wizard's file import and **Save as integration**, the **Data Export** editor, an integration's rename, tags and delete (REST API connectors are configured only in their wizard) |
| Other | `set_tags`, `set_date_format`, `create_revision`, `set_user_access_rules` | tag editors, the model's **Dates** choice (Developer › Models), **New revision**, **Start a new draft** (only when the developer asks; see above), the exception above |

What an edit does to stored data is the developer's own code, not a copy:
`internal/modeledit` holds the member delete (input values to history, time
re-index), the member code rename (facts, results and widget settings
re-keyed), the time-member write, period generation, the move of a parent's
values to its first child, and the widget cleanup after a metric or grid is
deleted. The gateway endpoints and the assistant both call it.

`update_dimension_member` names the member by its current `code` and changes
what it carries: `new_code`, `label`, `parent_code` or `clear_parent`,
`period_start` and `period_end` (a time member keeps its dates when they are
left out), `properties` (merged). A `new_code` is carried into every formula
of the revision that names the old code (a LOOKUP member, a criterion,
`"<>CODE"` included, a comparison, a SWITCH value, a TIMESUM bound on the
time dimension) and into the data exports filtering by it
(`metricformula.RenameMemberInFormulas`, from `modeledit.RekeyMemberCode`), as
the developer's member edit does. `create_grid`'s `metric_ids` and
`dimension_ids` go through `add_grid_metric` and `add_grid_dimension`, so they
get the same model, revision and one-grid checks. Values already stored move
with a grid change as they do in the console: `add_grid_dimension` takes
`member` (the member of the new dimension the grid's values belong to,
required while its metrics hold any), `add_grid_metric` and `create_grid` take
`members` (`{dimension: member}` for each grid dimension the metric's values
lack), and a metric moved onto a grid without a dimension its values carry has
them combined along it by its own rule, or the step is refused when the rule
cannot combine stored values. A widget, folder, form or
integration id listed before the draft existed resolves to the draft's copy.

`update_metric` changes only the fields the step carries (a field left out
keeps its value, as the developer's `PATCH /api/developer/metrics/{id}`). A
new `name` is carried into the highlight rules, closed rules and data exports
of the revision that name the metric (`metricformula.RenameMetricInRules`).
`delete_metric` is refused with `METRIC_IN_USE` while a formula, a Rate
total, a highlight rule, a closed rule, a workflow condition or a data export
of the revision names the metric; the message names each of them. A
name the revision already uses is refused on `create_metric`,
`update_metric`, `create_dimension` and `update_dimension` with `METRIC_NAME_TAKEN` /
`DIMENSION_NAME_TAKEN`, and a code the dimension already has on
`add_dimension_member` with `MEMBER_CODE_TAKEN`. The assistant has no tool that deletes a dimension
member; a developer deleting one a formula names gets `MEMBER_IN_USE`.

`update_metric` also switches a metric between input and calculated with
`is_input`, keeping its id: to a calculation it needs a `formula` and is
refused for an input holding values unless `drop_values` is true. A metric's
format may be `date` (the cell holds DATE()'s serial, typed and shown as
yyyy-mm-dd, never summed) or, on a calculation, `text` (its formula's text,
stored per cell; totals none or formula; it reads text inputs and other text
calculations by name, and no number formula reads it). A pick-list
holds leaf members unless `picklist_allow_parents` is true. A grid widget's
`widget_props.show_members` (`{"<dimension>": ["<code>", …]}`) shows only
those members on a row or column axis, in that order; `update_workflow`'s
`approver_may_start` lets the workflow's approver start it, and its
`start_roles` (role names, as a step's `assignee_roles`) says who may start
it by hand: others are not offered its Start button or inbox entry, and the
start is refused (`workflow.Store.StartRefusal`); `[]` lets anyone.

`add_dimension_property` declares a typed member property (`dimension_id`,
`name`, `data_type`: text, number or date) under the same rules as the
developer's Dimension Properties panel — an identifier name, unique in the
dimension regardless of case — so a formula can then read it as
`dimension.property`. `update_dimension_property` (`dimension_id`, `property`
— its current name or id — and `name` and/or `data_type`) renames or retypes a
declaration like the panel's edit: a field left out keeps its value, and a
rename moves every member's value, any dimension grouped by the property and
every formula that reads it to the new name. A retype is refused with
`INVALID_PROPERTY_VALUE` while a member holds a value the new type cannot read,
and a member value a number or date property cannot read is refused on the
member tools. `clear_values: true` removes the property's value from every
member, like the panel's Clear values; the assistant sends it only when the
developer asked. `delete_dimension_property` (`dimension_id`, `property`)
removes the declaration; member values stay stored but no formula can read them
until the name is declared again, in any case: `add_dimension_property` then
says "N member(s) already hold values under … from before; formulas read them
now", and the assistant asks the developer whether to clear them. The delete is
refused with `PROPERTY_IN_USE` while a formula still reads the property, and
the error names the metrics to change first. Two declarations differing only in
case made at the same moment leave one; the other is refused with
`PROPERTY_NAME_TAKEN` naming it. Both act only on the assistant's draft revision
and refuse a property of another dimension or model. A property id from
another revision (the active one, listed before a draft exists) is matched to
the draft's same-named property only while the draft's declarations on that
dimension are still the copied ones, and keeps that match for the rest of the
proposal; otherwise it is refused with a request for the current name. Promoting the draft
recomputes every calculated metric of the revision. The assistant's formulas use the same validation as the
console, including `dim.property`, `PARENT`, `LOOKUP`, the `SUMIFS` family and
the time additions (see the [formulas manual](formulas-manual/)); so a formula
reading a dimension its metric's grid does not carry is refused with
`DIMENSION_NOT_ON_METRIC` when the assistant saves it on a placed metric, places
the metric (`add_grid_metric`) or removes the dimension from the grid, and
activating a revision that holds one is refused too. Its system prompt's "Formula language"
section lists the functions from the engine's own registry (`formula.BuiltinNames`,
held there by `TestPromptListsEveryFormulaFunction`), so the assistant is never
told of a function the engine lacks, and names what does not exist (VLOOKUP,
SUMPRODUCT, the IS… functions).

`create_dimension` also creates a property grouping, as the developer's
**Group members of / By property**: `source_dimension_id` (the grouped
dimension's id or exact name; `source_dimension_name` also works) and
`source_property` (declared on it), with `derive_members: true` to add one
member per distinct value. The developer endpoint's validator
(`metricformula.ValidateGrouping`) refuses a bad one with `INVALID_GROUPING`:
a source of another revision, the dimension itself, an undeclared property,
a time dimension on either side, or a grouping together with
`parent_dimension_name`. `list_dimensions` marks a grouping "(groups X by its
property p …)". A parent dimension (`parent_dimension_name` or
`parent_dimension_id`) resolves within the draft revision and is checked by
the developer endpoint's `metricformula.ValidateParentDimension`: another
revision's dimension, the dimension itself, a hierarchy cycle or a time
dimension is refused with `INVALID_PARENT_DIMENSION`.

`update_dimension` is the developer's `PATCH /api/developer/dimensions/{id}`:
`dimension_id` (id or exact name) and only the fields to change — `name`,
`tags`, `parent_dimension_id` or `parent_dimension_name` (null
detaches), `source_dimension_id` or `source_dimension_name` (null clears the
grouping), `source_property`, and `derive_members: true` to add a member for
every source value with none yet (also on its own, after new values appear).
Neither it nor `create_dimension` takes an `agg_rule`: a dimension's is read by
nothing — each metric's own `agg_rule` decides how it rolls up. A new `name`
is carried into every formula naming the dimension (bare, braced, as
`dimension.property`, as a LOOKUP or criteria dimension, inside `PARENT()`),
the highlight rules limited to its members and the data exports naming it
(`metricformula.RenameDimensionInFormulas`), as the developer's rename does;
the result says how many formulas changed.
It shares the PATCH's rules (`metricformula.ValidateParentDimension`,
`metricformula.PlanGroupingPatch`): `INVALID_PARENT_DIMENSION`,
`INVALID_GROUPING`, `DIMENSION_IN_USE` when clearing or replacing a grouping's
source while a formula names the dimension, and `DIMENSION_NAME_TAKEN`. The
dimension type and time settings cannot change. It acts only on the draft
revision's dimensions and refuses another model's; where the PATCH
recalculates the metrics reading a changed grouping, promoting the draft
recomputes every calculated metric of the revision.

Tags mirror the console's tag editors: `create_metric`, `update_metric`,
`create_dimension`, `create_grid` and `create_dashboard` take `tags`, and
`set_tags` (`kind`: metric, dimension, grid or dashboard; `id`: id or exact
name; `tags`) replaces the tags of one that already exists. `list_metrics`,
`list_dimensions`, `list_grids` and `list_dashboards` show them.

Workflows and tables of records (added 2026-09-16, programme item 5; tables 2026-10-14):

| Tool | Mirrors |
|---|---|
| `create_workflow_def`, `update_workflow_def`, `delete_workflow_def` | the Workflows editor: every step type and field, `context_schema`, subject, `single_active_instance`; delete is drafts-only, as in the console |
| `archive_workflow_def`, `restore_workflow_def`, `duplicate_workflow_def` | the Workflows list's Archive, Restore and Duplicate |
| `create_automation_rule`, `update_automation_rule`, `delete_automation_rule` | the Triggers screen: every trigger type, sources, cron schedules |
| `create_business_role`, `update_business_role`, `delete_business_role`, `set_role_dashboards` | the developer's Roles tab (the `baOrDev` guard); a rename or delete names the workflows whose steps still name the old role |
| `create_form_def`, `update_form_def`, `delete_form_def` | the Records tab: every field type — autocode (`code_prefix`, `code_digits`), status, longtext and url included — every field option (default, required_with, hidden, core, history, on_delete, format, deadline), `own_records_only`, `edit_access`, and `keeps_dimension` (by name) with `member_label_field` and `member_root_label`; an update that adds or removes the status field re-posts the table (`Hooks.RepostTable`), as the tab's save does |
| `create_form_integration`, `update_form_integration`, `delete_form_integration`, `backfill_form_integration` | the record-to-metric posting screen (Integrations › Records) and its **Backfill**; an update re-posts, as the screen's does. Creating one, or pointing one at another metric, says how many typed values of the target it sets aside (a metric records feed shows its records only; `clear_input_values` removes them), as the screen does |

## A plan is checked before the developer sees it

Before anything else the plan's steps are put in dependency order
(`aiassistant.OrderByDependencies`): a metric is created before the formulas,
grids and attachments that name it, every other step keeps its place, a step
of another kind is a fixed point nothing moves across, and a plan naming
`<created in step N>` or holding a cycle is left as written.

Every `propose_actions` call is first run exactly as confirming would run it —
the same write executor, the same validation — inside a database transaction
that is always rolled back (`internal/gateway/ai_proposal_check.go`,
`aiassistant.NewDryRunWriteExecutor`). A plan with a failing step goes back to
the model as the tool result, listing each failing step with its error, and is
never shown; the model corrects it and proposes the whole plan again — three
times, and more (up to eight) while each attempt fails fewer steps
(`planRetries`, `internal/gateway/ai_turn.go`). Then it is asked for the steps
that pass, which are shown with a message listing what the check left out, and
after that it stops and says what fails; it is never told to ask whether to go
on; the message names every step the partial plan lacks, not only the
failing ones (`droppedSteps`). A plan that runs but looks like the model's
typical mistakes goes back once with the warning — a formula (`planWarnings`:
a Percentage metric multiplied in without dividing by 100, `SUMIFS(src, D, D)`
on a cell carrying every dimension of `src`) or a recurring modelling slip
(`recurringMistakeWarnings`: five or more 1s written to count rows, which
COUNTIFS counts; a headcount, balance or stock summed over time; a grid widget
without `metric_ids` on a grid of more than twelve metrics); proposed again unchanged, it is shown with it — and
so it is when the turn ends without a new plan. A model that ends a turn asking leave
to propose after its plan went back ("confirm and I will post it",
`asksToPropose`) is told once, by the server, to propose instead; one that
explains why no plan can be made ends the turn as it is. Unknown
names in formulas say what they most likely meant (`setup_item` → `{Setup
Item}`; `p_and_l_line` → `{Cost Center}.p_and_l_line`), so one correction
round usually suffices. Every tool is checked: the workflow, form,
calculation and notification stores are built on the transaction
(`NewStoreOn`, `pkg/dbx`), a file import into a dimension writes its members
into it and one into a grid resolves its values against the model as the
plan's earlier steps leave it, and a form integration's posting counts what
it would post. A step that uses a failed step's result is not run. Once every
step runs, the whole plan is checked as well: a plan that adds a dashboard
while any dashboard of the revision has no place in the order fails, naming
them and the page everyone would land on, until it carries
`reorder_dashboards` (`unorderedPagesProblems`). A text widget's Markdown sent
with `\n` but no line break gets its line breaks (`textWidgetContent`). The
prompt's *The pages people use* describes the pages of the published models
(the Models category of the public repository's Discussions) for the model to
follow: *Start here* first with the workflow buttons, their status and the
guide, the results, the input pages, *How to adapt · Developer* last.

**Dashboard layout.** `arrange_dashboard {dashboard_id, rows, weights?}` is
the designer's **Arrange** (`POST /api/developer/dashboards/{id}/arrange`,
`modeledit.ArrangeDashboard`): every widget once, row by row; a row's widgets
side by side over the 1100px canvas with 20px between them, each as tall as
what it shows (`modeledit.WidgetHeight`: a KPI tile 120, a button 40, a chart
360, a records table 540, a grid its header and rows up to 640, a text its
lines, a workflow's status its steps). A widget added without a place lands
under the dashboard's widgets, full width, so it never overlaps; a grid
widget's `default_view` naming a dimension its grid lacks is refused
(`CheckGridWidgetView`); a title set on `update_dashboard_widget` shows unless
`show_title` says not. For every dashboard a plan touches the check refuses a
widget drawn under a row it starts inside, one past the canvas, overlapping
ones, a grid box far shorter or taller than its rows, KPI tiles in `bar` mode
without a bar, a chart or grid without a shown title, and a dimension two or
more widgets choose on their own while the bar lacks it
(`dashboardLayoutProblems`); it warns once of tiles adding up every period of
every year, of identical tiles, of tiles that read alike, of a tile pinned on
a bar dimension and of a chart titled by its grid's name, and names the other
pages that still have problems (`dashboardLayoutWarnings`,
`otherDashboardsWarnings`). The prompt's *Rules for each thing you build* is a
checklist per object (dimension, metric, grid, dashboard, widget, workflow,
rule, table, loader, roles), marking what the check enforces.

**The history** is replayed on every model call, so turns before the last two
keep their messages but not their bulk: a tool result or tool-call argument
over 1,500 characters is cut to its first 400 with a note
(`aiassistant.CompactHistory`). A long build otherwise passed the provider's
input limit (gpt-5-mini: 272,000 tokens) at its sixth stage.

**A turn** runs at most `AI_TURN_TIMEOUT` (30 minutes; 10 until 2026-10-24,
which stopped a check of a large model's loaded data half-way); the console
streams "progress" while the model writes a tool call (the providers report
its arguments as they arrive) and offers Stop, which closes the request; both
end with a saved message. Each round of a turn sends the model the tools, the
system prompt and the conversation again; with Anthropic models all three end
in a prompt-cache breakpoint, so a round pays full price only for what the
round before it added (uncached, a tester's four sessions on a large model
cost $23). The gateway logs each call's `input`, `cache_read`, `cache_write`
and `output` tokens. Promoting a session's draft finishes it and every other
session of the developer working in that draft: their next message is refused
with 409 `SESSION_PROMOTED`, and a new session works on the developer's
working revision — the promoted draft, which the console has switched to.

**What keeps a large build moving** (found building the Creator & Agency
workbook, 2026-11-08/09; `internal/gateway/ai_build_guards.go`, `ai_turn.go`):

- *A plan that does not arrive* — cut off at the model's output limit
  (`finish_reason` `length`) or arguments that are not a plan — goes back to
  the model saying why and to propose in parts (`unreadablePlanResult`); the
  third in a turn ends it with a message. An empty reply goes back once
  (`emptyReplyNudge`). A reply announcing a proposal it did not make ("I'll
  send the corrected proposal now") is told to propose (`announcesProposal`);
  a reply offering the developer a choice is never sent back as a done claim.
- *A new proposal withdraws the pending one* it replaces (marked rejected, so
  it cannot be confirmed): the model's tool result and the chat both say so.
- *One round per finding, not per kind of finding*: a failing plan's
  rejection also lists the warnings of the steps that ran; a warning counts as
  given once it was shown, alone or with others; a plan failing the same way
  twice (step numbers and ids aside) is asked for the steps that pass, then to
  stop (`planRetries.repeats`).
- *Warnings for the build's silent mistakes*: a `write_input_values` step that
  changes values an input already holds, unless the request names that input
  (`inputOverwriteWarnings` — the model had moved two assumptions until a row
  matched); an `update_metric` turning a calculated metric into an input
  (`calcToInputWarnings` — it had proposed typing the workbook's values into
  three calculated rows); a formula reading `dim.property` that no member has
  a value for (`emptyPropertyWarnings`); a total by formula given a time
  summary other than sum, which never applies.
- *The coverage check* is told what the revision already has, that a heading
  or a source named as input is not an item, and that "rows 6-39" is covered
  row by row.
- *The server checks values after each confirmed plan*: in a session with an
  attached file, the next turn starts by comparing again every row the
  session compared (`compare_with_sheet`), naming the rows that matched at
  their last comparison and differ now, the rows that differ, and the metrics
  the plan made or changed that nothing compared yet (`stageValueCheck`). The
  note is shown in the chat and given to the model with the developer's
  message.

**A session carries its own work on** (`aiCreateSession`, `CarryTaskPlan`,
`EarlierSessions`). A build runs in one session, its draft and its task plan
together; nothing has to be handed over. A new session starts a fresh draft:

- it takes over the task plan of the developer's previous session in the
  model only when that plan is unfinished and that session's draft became the
  working revision (`carried_from`) — then the work the plan did is in what the
  new draft copies; a confirmed plan marks the current stage in progress and
  counts its plans and steps (`RecordConfirmed`);
- the value check reads the comparisons of the sessions the plan came from, and
  a session's first turn opens with a handover check of those rows;
- with a call budget set, from 70% of it the model is asked to finish the part
  it is on and say how far the work got (`sessionLengthNote`); at the budget,
  the message says to raise it — the work stays in the session's draft.

**Modelling know-how in the tools** (2026-11-09):

- `<created in step N>` placeholders that stand for a metric, grid, dimension or
  page the plan creates become that object's name (`aiassistant.NamePlaceholders`)
  — one pointing at a step that creates no such object reads as the object of
  that kind the step's description names, or the metric an update / add /
  write step works on — so a plan using positions is put in dependency order
  and its carried-forward cycles are split (created with 0, formula set
  later). A formula set by `update_metric` follows the steps placing its
  metrics on their grids: a cycle broken by time needs its time grid first.
- The refusal of a LOOKUP or a conditional aggregation inside such a cycle
  says what to do: the call in a metric of its own, read here.
- A formula that does not parse because it names a dimension or metric with
  a space bare is told the name in braces (`bracesHint`); `create_dimension`
  takes a `label`, and the prompt asks for identifier names with labels.
- The prompt asks the developer for a missing record (business data) instead
  of a stand-in table, and never asks whether to submit a plan on the card.
- A property declared on a dimension a table keeps is filled from the table's
  field of that name and kept up to date (`recordpost.KeepMember`,
  `FillKeptProperties`).

**Engine fixes found by the same build** (2026-11-09): a table loader's
column map is checked against the table's fields when it is saved and when it
runs, and "metric" is the grid import's long-file keyword only for a grid (a
table field named Metric loads); a workflow condition waits for its revision's
pending recalculation (up to 20 s, then held and decided when it finishes);
`list_grids` marks a grid dimension with no members, and the plan check warns
of a plan that puts one on a grid (`memberlessDimensionWarnings`).

Renaming a metric — by the AI Developer or in the console — renames it in
every formula and workflow condition of its revision that names it
(`metricformula.RenameMetricInFormulas`); before, they stayed on the old name
and went blank at the next draft copy.

Confirming runs the steps in order and **stops at the first failure**: the
steps after it are marked *not run*, because they are usually built on it.
What ran before it stays in the draft. Clearing a grid without input metrics
is not a failure: it has nothing to clear, so "clear all the data" goes on to
the next grid.

## Building a model from a workbook

A build from an attached workbook follows one process (owner, 2026-11-12):
agree one target design, use the server's bulk tools for routine work, build
one coherent module per stage with evidence before going on, add the
platform's advantages as explicit business requirements, and call the build
done only when its behaviour is verified. The playbook (`task_mode.go`,
`workbook_build`) tells the model so; the server holds it to it.

**How to work with it.** Building one three-sheet workbook five ways
(2026-11-12): a strong reasoning model at low effort (gpt-6.1-sol) finished
correctly in 75 calls and 4.2 minutes with no questions; at medium effort
129 calls and 8.4 minutes; gpt-5-mini, with a short instruction, a detailed
brief or five step-by-step instructions, did not finish in up to 181 calls —
it asked what the workbook already said and typed inputs instead of loading
them. Step-by-step instructions do not steer it: after the first step it
plans the whole model. So the recommended way is: a strong model at **low**
effort (AI Settings says so: `buildAdvice`); the workbook attached; one
short instruction naming the model, its input and calculation sheets, the
dimensions, the business requirements and dashboards; confirm each plan, answer
the decisions it asks; for a large workbook a call budget with a checkpoint.
The optional second model for generated sheet plans was removed the same
day: one model runs every turn.

**One agreed design** (`set_target_model`, `update_target_model`,
`target_model.go`). The target model records, besides each sheet's role,
grid, dimensions, rows and metrics: the **conventions** every row follows
(units, signs, percentages, opening balances, scenarios, monthly against
annual — required when a workbook is attached, as are the metric names of
calculation and report sheets, which `plan_sheet` maps rows onto); each sheet's **owner** and
how it is **verified**; **row notes** (a row's business meaning, or a row
changed on purpose with its reason); the **decisions** the workbook leaves
open, with their options and the sheets they hold back — `load_sheet` and
`plan_sheet` refuse a held-back sheet until the developer has chosen, and the
choice is also kept with the plan's decisions; the **business requirements**
(area, behaviour, open/built/verified); and the **verification checks**
beyond the saved workbook (open/passed/failed/not_possible, with evidence).
`update_target_model` changes one decision, requirement, check or row note
without re-sending the target. Every turn shows the target, the current
module's sheets whole and the others one line each.

**Bulk tools for routine work.** `analyse_workbook` (the whole workbook and
its dependencies), `load_sheet` (inputs, matched to metrics and members),
`plan_sheet` and the `sheet_plan` step (formulas translated on the server),
`compare_with_sheet` (many rows over all their periods) and `workbook_map`
(what each row became and how it compared). The translator reads **every
period**: runs of periods that translate alike become one formula, a first
period that differs an opening rule, later changes an `IF` on the month
number; a formula that cannot be translated from some month on, or a typed
value among formulas, is a problem naming the month
(`translateRuns`). The plan check warns when a plan types values the
attached workbook holds (`typedWorkbookValueWarnings`) and, as before, when
it moves an input the developer did not name (`inputOverwriteWarnings`).

**One module per stage, with evidence** (`cutStages`, `buildOrder`,
`update_task_plan`). A stage may build a few dependent sheets together
(`TaskStage.Sheets`, at most 4; a few dashboards, at most 3) and is proven by all
of them. Stages run: calculation modules in dependency order, actuals, the
business requirements (roles and access, workflows, the approved revision,
dashboards), the verification checks. A stage closes with its **evidence** —
what was built or repaired, what still differs and why, the next stage — and
the server adds the rows and periods compared from the workbook map
(`comparedFor`); a stage the map shows matching is closed by the server with
the same evidence. The build's last stage closes only when every
verification check is recorded. Each stage also carries what it took
(`StageMeasure`: turns, minutes, calls, tokens, tool calls and the
developer's corrections — messages other than confirming or continuing),
added at the end of every turn (`measureTurn`). The console's plan card
shows the evidence, the measures, the decision waiting for the developer
and how many checks are recorded.

**Precise corrections.** A difference is reported with the draft, metric,
month, scenario, expected and actual value and how to reproduce it, and is
fixed at its upstream cause, then the outputs it feeds compared again; an
input is never moved to make a calculated row match.

**The session's call limit counts model calls** (`CountLLMCallsInSession`
reads the session's recorded `llm_calls`, which every call now adds to);
it used to count assistant messages, the server's own notes among them.

## Attached spreadsheets and data exports

A `.xlsx`, `.xlsm` or `.csv` file attached to the chat (up to 16 MB) keeps its
original bytes (`ai_assistant.document.raw_data`) next to the text the
language model reads, so the assistant imports the **whole** file. Files are
attached with the paperclip — several at a time — or dropped on the message
box; once sent with a message they leave the box and stay in the session
(*Show the session's files*). The upload has ten minutes to arrive, as an
import has: a file of a few megabytes over a slow link took longer than the
server's one-minute read limit and was cut off. A CSV file's text is its
first 31 lines and how many lines it has — `read_attached_sheet` reads any
lines of it (500 a call); its first 60,000 characters used to go to the model
on every call. A workbook's text (60,000 characters at most) lists every sheet first, then each sheet's rows,
layout, formulas, lists and highlights; when the whole workbook does not fit,
each large sheet shows fewer rows and clipped formulas (a sheet of 30 rows or
fewer is always whole) and says which `read_attached_sheet` call shows the
rest. Cut off at the end instead, a workbook whose data sheets came first lost
every later sheet — its calculation sheets. A table pasted into the chat (two rows and two columns or more, tab-separated as
spreadsheets copy it) is attached the same way, as `pasted-data-1.csv`
(`web/src/consoles/developer/pastedTable.ts`), so a block of values imports instead
of being retyped through `write_input_values`. `preview_file_import` and `import_file_data`
take the file's name, an optional sheet, a target (a grid — the metric values
its columns name — or a dimension — members) and a `column_map` in the Import
Wizard's vocabulary (a metric or dimension name; `metric` + `value` for a long
file; `code`, `label`, `parent_code`, `property:<name>`; `ignore`, and
`"*": "ignore"` for every column the map does not name), applied after an
optional `reshape`. A date-formatted cell reads as its ISO date
(`2014-11-24`), and as its serial number into a numeric metric. The import
is the developer's own pipeline, run by the gateway through the executor's
`ImportFile` hook (`internal/gateway/ai_file_import.go`): stored cell values,
not displayed ones (`1234.5`, not `"1,234.50"`), `importpkg.ResolveRows`, the
revision's write guard, the edition's fact, storage and member limits,
recalculation and an `import.uploaded` audit row. It is all-or-nothing: one bad
row and nothing is written, with the rows named. A grid import defaults to
`replace` — re-importing the same file converges instead of adding to the
totals; values may be negative. `create_file_integration` saves the target,
map and mode as a re-runnable `csv_import` integration; `import_file_data`
with its `integration_id` uses them and records the run in its history. The
same integration is re-run with a new file of the same columns from the
Integrations tab, or by a user from a dashboard **Integration**
button with a `.csv` or `.xlsx` (`POST /api/integrations/{id}/run` applies the
saved reshape and map; `internal/gateway/integration_file_run.go`).

**Reshaping a file laid out for people** (`internal/importpkg/reshape.go`). A
`reshape` turns a sheet as finance teams send it into one row per value before
the column map, declaratively — nothing in it is code. Its steps run in a
fixed order: `delimiter` (a `;`-separated CSV), `header_row` (titles above the
header), `fill_down` (a group label written once), `skip_rows` (total and
blank rows), `unpivot` (months across the columns become a column of month
names and a value column), `constants` (what the file means but does not say:
`{"Scenario": "Budget"}`), `value_map` (labels into member codes:
`{"Month": {"Jan": "2026-01"}}`), and numbers written for people
(`number_columns`, `decimal_comma`, `scale` for a file in thousands;
`(123)`, `12%`, thousands separators and currency signs are read; a separator
that cannot be a thousands separator is never guessed, so `1,5` is not 15).
The preview shows the sheet's first rows as read and the first rows as the
import reads them, so the assistant can see a layout and correct its reshape.
`create_file_integration` and `update_integration` save a reshape with the
integration, and every run applies it — a user's monthly upload of
the same layout from a dashboard button included. In the Integrations tab such
an integration says what its reshape does and runs with **Run with a file**,
which sends the file as it is. A developer sets one up without the assistant
in the Import Wizard's **Shape** step (previewed by the server's own code,
`POST /api/import/reshape-preview`) and changes a saved one with **Edit shape
and mapping**.

**Converted files.** `prepare_converted_file` records the attachment with its
reshape and column map (`ai_assistant.conversion`, migration 107); the panel's
**Converted files** strip downloads it as CSV or Excel
(`GET /api/ai/sessions/{id}/conversions/{cid}?format=csv|xlsx`, the session
owner only). The file is rebuilt from the attachment on every download by the
code an import uses, so it is exactly what an import would read; a column of
plain numbers is written as numbers in Excel, member codes stay text.

`create_export_integration` saves a `file_export` integration: a grid and a
spec (`internal/dataexport`) — CSV (delimiter, decimal separator), XLSX or
JSON; a wide, long or pivoted layout; chosen and renamed columns; member codes
or labels; member filters; rounding. Specs name metrics, dimensions and
members, never ids, so they survive revision copies and model export. Exports
are leaf-level: a dimension that is not a column must be filtered to one leaf.
The spec is validated by the same code as the console's editor, and a download
(`GET /api/integrations/{id}/export`) is rendered for whoever asks from their
own `/api/grid` view, so hidden members and metrics never reach the file and
every value is the one their grid shows. The panel lists the session's exports
with a **Download** button; developers find them in the Integrations tab, and
a dashboard's Integration button downloads one for users.

## What stays human

- **Publishing a workflow**, and its test run. The assistant creates and
  edits drafts. A draft is invisible to end users until a developer publishes
  it from the Workflows tab, and the assistant says so at the end of every
  workflow proposal. Because a rule cannot bind by id to an unpublished
  definition, the assistant binds rules **by name**; the engine resolves the
  name at fire time, so the rule goes live the moment the developer publishes.
- **Role membership.** The assistant creates and changes business roles; who
  is in them is set on the Roles screen, by a developer or a business admin.
- **Promoting or discarding the draft revision**, making a revision active,
  deleting one, and choosing the model users open by default.
- **Cancelling a running workflow run** — a developer's (Developer › Triggers,
  the execution log's **Cancel**) or a business admin's (Business Admin ›
  History). The assistant starts manual runs but does not stop them.
- **Users** — invitations, deletion, platform and business role grants.
- **Secrets.** The assistant configures REST API connectors (HTTPS, SFTP,
  model links), Google Sheets integrations and sign-in connections, tests and
  runs them, and fires manual triggers (since 2026-10-09: `create_api_integration`,
  `update_api_integration`, `create_connection`, `update_connection`,
  `run_integration`, `trigger_automation_rule`, and since 2026-10-15
  `recalculate_revision` (Models › revision › Recalculate), since 2026-11-08
  `rename_draft` (the session's own draft; Models › revision › Rename), `clear_input_values`
  and `fill_sample_values` (Grids › Values; `as_file` attaches the rows as a CSV
  instead of writing them), and the immediate `create_csv_file` (a CSV it writes,
  attached to the chat and listed under Converted files); saves go through the
  wizard's own `restAPISave`, tests, runs and triggers through the
  developer's own routes in-process, `callAsCaller`). A credential's secret
  parts never pass through it: a proposal carrying one is refused, and the
  plan card asks the developer for them; they travel with the confirmation
  (`secrets` by step) to `SaveConnection` alone, and are in no proposal,
  message or audit row. An OAuth authorization-code connection's **Connect**
  click and trusting an SFTP host key after comparing its fingerprint stay
  the developer's.
- **Business data** — typing cells and entering records. The assistant
  imports a spreadsheet the developer attaches to its chat (into the draft)
  and posts saved records through a table integration, and nothing else. It
  reads records (`query_records`) but never changes one.
- **Applications**, which the assistant does not create: a session works in
  one model.
- **Database migrations.** There is no migration tool: the gateway migrates a
  model's tables after the developer creates, changes or deletes a metric or
  creates a dimension, and whenever a revision is made active
  (until 2026-09-28 the assistant offered `generate_migration` and
  `apply_migration`; neither did anything).

## Where validation lives

`internal/workflow/validate.go` is the one definition of "this workflow
could run" — the editor's Validate button, the Publish gate and the
assistant's `validate_workflow` all call it, and `update_workflow_def`
reports its verdict with every change. It moved out of the gateway for the
same reason `internal/metricformula` did: two copies of a rule drift, and
the assistant ends up stricter or looser than the developer it must match.

## How parity is checked

`internal/gateway/ai_builds_workflows_test.go` builds a form, its
integration, a business role, a three-step workflow and two automation rules
using nothing but the assistant's tools, then walks the human gate —
publish, fire, approve — and proves the instance completes and the
requester is notified. `ai_builds_model_test.go` does the same for the
model itself. `internal/aiassistant/edit_tools_test.go` changes and removes
what was built — every change-and-remove tool, against the refusals and data
effects of its endpoint — and `internal/gateway/ai_edit_parity_test.go` runs
the gateway's hooks: the edition limits refuse the assistant where they refuse
the developer, and a backfill posts through the gateway's own posting code.
When a divergence between the assistant and the developer role appears, it is
found by building something, not by reading the tool list.

The 2026-09-28 audit compared every route the developer role can reach with
the tool list. Besides the missing change-and-remove tools, it found the
assistant doing things the developer cannot: `create_grid` attached another
model's metric, `add_dashboard_widget` took another model's form or grid for
the widget types it did not check, and edition limits did not apply; each now
goes through the developer's check.

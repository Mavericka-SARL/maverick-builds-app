# The AI Developer: what it can build, and what stays human

> **Classification:** Current — The assistant's tool surface and its limits.

> **Last verified:** 2026-10-02

The AI Developer is the assistant inside the developer console. Its rule is
parity: it can build, change and remove what a developer builds, changes and
removes through the screens, under the same checks — with one deliberate
exception below, and the short list under [What stays human](#what-stays-human)
that it leaves to people. It reads through **read tools** it calls freely, and
writes only by **proposing** an ordered plan that the developer confirms.
Nothing is written until then.

**Where its writes land.** Model changes land in an isolated draft revision,
created on the session's first confirmed proposal and promoted or discarded by
the developer — and so does data the assistant imports from a spreadsheet
attached to the chat. Three kinds of write are not revision-scoped, exactly as in the
console, so they take effect on confirmation and discarding the draft does not
undo them:

- business roles (`create_business_role`, `update_business_role`,
  `delete_business_role`) — the tenant's roles, as on the Roles screens;
- user access rules (`set_user_access_rules`, the exception below);
- form-record posting (`backfill_form_integration`, and the re-post
  `update_form_integration` runs) — input values posted into the working
  revision.

`set_role_dashboards` writes live grant rows too, but on the working
revision's dashboards: grants on the draft's dashboards matter once it is
promoted.

**The exception.** `set_user_access_rules` is a business-admin capability
given to the assistant at the owner's direction (2026-08-26). Access rules are
not revision-scoped, so it writes live `identity.user_access_rule` rows
against the active revision: they take effect when the proposal is confirmed,
not when the draft is promoted, and discarding the draft does not undo them.
It replaces only the user's member rules that resolve in the active revision;
metric and button rules, and rules on members no longer in the active revision
(which still restrict older revisions), are kept.

**Held to the same limits.** The tenant's plan limits on metrics per model and
members per dimension refuse the assistant where they refuse the developer:
the gateway hands the executor its own checks (`aiassistant.Hooks`), and form
posting runs the gateway's own posting code the same way.

## Read tools

| Tool | Answers |
|---|---|
| `get_model_summary`, `list_metrics`, `list_dimensions`, `list_revisions`, `list_users` | the model as the developer console shows it |
| `list_grids` | each grid's metrics and dimensions, with a dimension's display level |
| `list_dashboards` | the folders, then each dashboard's folder, tags and widgets — each widget's id, type, what it shows, place and size |
| `validate_formulas`, `check_grid_completeness` | the audit checks the console runs |
| `list_workflows`, `get_workflow` | definitions; one in full — steps, context, subject, rules, and the Validate verdict |
| `validate_workflow` | the editor's Validate button, for stored or not-yet-proposed steps |
| `list_workflow_roles` | what `assignee_roles` may contain: platform role codes and the tenant's business roles, with each role's member count and the dashboards of the revision it may open |
| `list_automation_rules` | what starts each workflow |
| `list_forms`, `get_form` | forms; one in full — fields, integrations, record count |
| `list_form_integrations` | which form field posts into which metric |
| `list_integrations` | every data integration — Excel/CSV imports, Google Sheets, REST API, data exports — with its target, column map or export spec, and last run |
| `preview_file_import` | a dry run of importing an attached spreadsheet: its first rows as read, its columns and first rows after the reshape and column map, every row that would fail and why, what would be imported. Writes nothing |
| `prepare_converted_file` | saves an attached spreadsheet reshaped and column-mapped into the import layout, for the developer to download as CSV or Excel from the chat. Imports nothing |
| `preview_export` | an export spec rendered against the grid's current values — columns, first rows, row count — or every problem with the spec. Saves nothing |

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
| Dashboards | `create_dashboard_folder`, `update_dashboard_folder`, `delete_dashboard_folder`, `create_dashboard`, `update_dashboard`, `delete_dashboard`, `add_dashboard_widget`, `update_dashboard_widget`, `delete_dashboard_widget` | the Dashboards screen and its designer |
| Integrations | `create_file_integration`, `import_file_data`, `create_export_integration`, `update_integration`, `delete_integration` | the Integrations tab: the Import Wizard's file import and **Save as integration**, the **Data Export** editor, an integration's rename, tags and delete (REST API connectors are configured only in their wizard) |
| Other | `set_tags`, `create_revision`, `set_user_access_rules` | tag editors, **New revision**, the exception above |

What an edit does to stored data is the developer's own code, not a copy:
`internal/modeledit` holds the member delete (input values to history, time
re-index), the member code rename (facts, results and widget settings
re-keyed), the time-member write, period generation, the move of a parent's
values to its first child, and the widget cleanup after a metric or grid is
deleted. The gateway endpoints and the assistant both call it.

`update_dimension_member` names the member by its current `code` and changes
what it carries: `new_code`, `label`, `parent_code` or `clear_parent`,
`period_start` and `period_end` (a time member keeps its dates when they are
left out), `properties` (merged). `create_grid`'s `metric_ids` and
`dimension_ids` go through `add_grid_metric` and `add_grid_dimension`, so they
get the same model, revision and one-grid checks. A widget, folder, form or
integration id listed before the draft existed resolves to the draft's copy.

`update_metric` changes only the fields the step carries (a field left out
keeps its value, as the developer's `PATCH /api/developer/metrics/{id}`). A
name the revision already uses is refused on `create_metric`,
`update_metric`, `create_dimension` and `update_dimension` with `METRIC_NAME_TAKEN` /
`DIMENSION_NAME_TAKEN`, and a code the dimension already has on
`add_dimension_member` with `MEMBER_CODE_TAKEN`. The assistant has no tool that deletes a dimension
member; a developer deleting one a formula names gets `MEMBER_IN_USE`.

`add_dimension_property` declares a typed member property (`dimension_id`,
`name`, `data_type`: text, number or date) under the same rules as the
developer's Dimension Properties panel — an identifier name, unique in the
dimension regardless of case — so a formula can then read it as
`dimension.property`. `update_dimension_property` (`dimension_id`, `property`
— its current name or id — and `name` and/or `data_type`) renames or retypes a
declaration like the panel's edit: a field left out keeps its value, and a
rename moves every member's value, any dimension grouped by the property and
every formula that reads it to the new name. `delete_dimension_property`
(`dimension_id`, `property`) removes the declaration; member values stay stored
but no formula can read them. The delete is refused with `PROPERTY_IN_USE` while
a formula still reads the property, and the error names the metrics to change
first. Both act only on the assistant's draft revision
and refuse a property of another dimension or model. A property id from
another revision (the active one, listed before a draft exists) is matched to
the draft's same-named property only while the draft's declarations on that
dimension are still the copied ones, and keeps that match for the rest of the
proposal; otherwise it is refused with a request for the current name. Promoting the draft
recomputes every calculated metric of the revision. The assistant's formulas use the same validation as the
console, including `dim.property`, `PARENT`, `LOOKUP`, the `SUMIFS` family and
the time additions (see
`FORMULA_CALCULATION_INSTRUCTIONS.md`). Its system prompt's "Formula language"
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
`agg_rule`, `tags`, `parent_dimension_id` or `parent_dimension_name` (null
detaches), `source_dimension_id` or `source_dimension_name` (null clears the
grouping), `source_property`, and `derive_members: true` to add a member for
every source value with none yet (also on its own, after new values appear).
It shares the PATCH's rules (`metricformula.ValidateParentDimension`,
`metricformula.PlanGroupingPatch`): `INVALID_PARENT_DIMENSION`,
`INVALID_GROUPING`, `DIMENSION_IN_USE` when clearing or replacing a grouping's
source while a formula names the dimension, and `DIMENSION_NAME_TAKEN`. The
dimension type and time settings cannot change. It acts only on the draft
revision's dimensions and refuses another model's; where the PATCH
recalculates the metrics reading a changed grouping, promoting the draft
recomputes every calculated metric of the revision.

Tags mirror the console's tag editors: `create_metric`, `update_metric`,
`create_dimension` and `create_dashboard` take `tags`, and `set_tags`
(`kind`: metric, dimension or dashboard; `id`: id or exact name; `tags`)
replaces the tags of one that already exists. `list_metrics`,
`list_dimensions` and `list_dashboards` show them.

Workflows and forms (added 2026-09-16, programme item 5):

| Tool | Mirrors |
|---|---|
| `create_workflow_def`, `update_workflow_def`, `delete_workflow_def` | the Workflows editor: every step type and field, `context_schema`, subject, `single_active_instance`; delete is drafts-only, as in the console |
| `archive_workflow_def`, `restore_workflow_def`, `duplicate_workflow_def` | the Workflows list's Archive, Restore and Duplicate |
| `create_automation_rule`, `update_automation_rule`, `delete_automation_rule` | the Triggers screen: every trigger type, sources, cron schedules |
| `create_business_role`, `update_business_role`, `delete_business_role`, `set_role_dashboards` | the developer's Roles tab (the `baOrDev` guard); a rename or delete names the workflows whose steps still name the old role |
| `create_form_def`, `update_form_def`, `delete_form_def` | the Forms builder |
| `create_form_integration`, `update_form_integration`, `delete_form_integration`, `backfill_form_integration` | the form-to-metric posting screen and its **Backfill**; an update re-posts, as the screen's does |

## A plan is checked before the developer sees it

Every `propose_actions` call is first run exactly as confirming would run it —
the same write executor, the same validation — inside a database transaction
that is always rolled back (`internal/gateway/ai_proposal_check.go`,
`aiassistant.NewDryRunWriteExecutor`). A plan with a failing step goes back to
the model as the tool result, listing each failing step with its error, and is
never shown; the model corrects it and proposes the whole plan again, at most
three times in one turn before it must explain the problem instead. Unknown
names in formulas say what they most likely meant (`setup_item` → `{Setup
Item}`; `p_and_l_line` → `{Cost Center}.p_and_l_line`), so one correction
round usually suffices. Every tool is checked: the workflow, form,
calculation and notification stores are built on the transaction
(`NewStoreOn`, `pkg/dbx`), a file import into a dimension writes its members
into it and one into a grid resolves its values against the model as the
plan's earlier steps leave it, and a form integration's posting counts what
it would post. A step that uses a failed step's result is not run.

Confirming runs the steps in order and **stops at the first failure**: the
steps after it are marked *not run*, because they are usually built on it.
What ran before it stays in the draft.

## Attached spreadsheets and data exports

A `.xlsx`, `.xlsm` or `.csv` file attached to the chat (up to 16 MB) keeps its
original bytes (`ai_assistant.document.raw_data`) next to the text sample the
language model reads, so the assistant imports the **whole** file, not the
500 rows a sheet the model sees. `preview_file_import` and `import_file_data`
take the file's name, an optional sheet, a target (a grid — the metric values
its columns name — or a dimension — members) and a `column_map` in the Import
Wizard's vocabulary (a metric or dimension name; `metric` + `value` for a long
file; `code`, `label`, `parent_code`, `property:<name>`; `ignore`), applied
after an optional `reshape`. The import
is the developer's own pipeline, run by the gateway through the executor's
`ImportFile` hook (`internal/gateway/ai_file_import.go`): stored cell values,
not displayed ones (`1234.5`, not `"1,234.50"`), `importpkg.ResolveRows`, the
revision's write guard, the plan's fact, storage and member limits,
recalculation and an `import.uploaded` audit row. It is all-or-nothing: one bad
row and nothing is written, with the rows named. A grid import defaults to
`replace` — re-importing the same file converges instead of adding to the
totals; values may be negative. `create_file_integration` saves the target,
map and mode as a re-runnable `csv_import` integration; `import_file_data`
with its `integration_id` uses them and records the run in its history. The
same integration is re-run with a new file of the same columns from the
Integrations tab, or by a business user from a dashboard **Integration**
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
integration, and every run applies it — a business user's monthly upload of
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
a dashboard's Integration button downloads one for business users.

## What stays human

- **Publishing a workflow**, and its test run. The assistant creates and
  edits drafts. A draft is invisible to end users until a developer publishes
  it from the Workflows tab, and the assistant says so at the end of every
  workflow proposal. Because a rule cannot bind by id to an unpublished
  definition, the assistant binds rules **by name**; the engine resolves the
  name at fire time, so the rule goes live the moment the developer publishes.
- **Role membership.** The assistant creates and changes business roles; a
  business admin decides who is in them.
- **Promoting or discarding the draft revision**, deleting a revision, and
  choosing the model business users open by default.
- **Users** — invitations, deletion, platform and business role grants.
- **Data connectors** — configuring REST API and Google Sheets integrations,
  their connections and credentials, and their runs. The assistant can list,
  rename, retag or delete them.
- **Business data** — typing cells and entering form records. The assistant
  imports a spreadsheet the developer attaches to its chat (into the draft)
  and posts saved form records through a form integration, and nothing else.
- **Applications**, which the assistant does not create: a session works in
  one model.
- **Database migrations.** There is no migration tool: the gateway migrates a
  model's tables after the developer creates, changes or deletes a metric or
  creates a dimension, and when an AI draft is promoted
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
the gateway's hooks: the plan limits refuse the assistant where they refuse
the developer, and a backfill posts through the gateway's own posting code.
When a divergence between the assistant and the developer role appears, it is
found by building something, not by reading the tool list.

The 2026-09-28 audit compared every route the developer role can reach with
the tool list. Besides the missing change-and-remove tools, it found the
assistant doing things the developer cannot: `create_grid` attached another
model's metric, `add_dashboard_widget` took another model's form or grid for
the widget types it did not check, and plan limits did not apply; each now
goes through the developer's check.

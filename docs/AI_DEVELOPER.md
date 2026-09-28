# The AI Developer: what it can build, and what stays human

> **Classification:** Current — The assistant's tool surface and its limits.

> **Last verified:** 2026-09-25

The AI Developer is the assistant inside the developer console. Its rule is
parity: it can do what a developer can do through the screens, no more and
no less — with one deliberate exception below. It reads through **read tools**
it calls freely, and writes only by **proposing** an ordered plan that the
developer confirms — nothing is written until then, and everything else lands
in an isolated draft revision.

**The exception.** `set_user_access_rules` is a business-admin capability
given to the assistant at the owner's direction (2026-08-26). Access rules are
not revision-scoped, so it writes live `identity.user_access_rule` rows
against the active revision: they take effect when the proposal is confirmed,
not when the draft is promoted, and discarding the draft does not undo them.
It replaces only the user's member rules that resolve in the active revision;
metric and button rules, and rules on members no longer in the active revision
(which still restrict older revisions), are kept.

## Read tools

| Tool | Answers |
|---|---|
| `get_model_summary`, `list_metrics`, `list_dimensions`, `list_grids`, `list_dashboards`, `list_revisions`, `list_users` | the model as the developer console shows it |
| `validate_formulas`, `check_grid_completeness` | the audit checks the console runs |
| `list_workflows`, `get_workflow` | definitions; one in full — steps, context, subject, rules, and the Validate verdict |
| `validate_workflow` | the editor's Validate button, for stored or not-yet-proposed steps |
| `list_workflow_roles` | what `assignee_roles` may contain: platform role codes and the tenant's business roles |
| `list_automation_rules` | what starts each workflow |
| `list_forms`, `get_form` | forms; one in full — fields, integrations, record count |
| `list_form_integrations` | which form field posts into which metric |

## Write tools (through `propose_actions`)

Model: `create_metric`, `update_metric`, `delete_metric`, `create_dimension`,
`update_dimension`, `add_dimension_member`, `update_dimension_member`, `add_dimension_property`,
`update_dimension_property`, `delete_dimension_property`, `create_grid`, `add_grid_metric`, `add_grid_dimension`, `create_dashboard`,
`add_dashboard_widget`, `set_tags`, `create_revision`, `generate_migration`,
`apply_migration`, `set_user_access_rules`.

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
`FORMULA_CALCULATION_INSTRUCTIONS.md`).

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
| `create_automation_rule`, `update_automation_rule`, `delete_automation_rule` | the Triggers screen: every trigger type, sources, cron schedules |
| `create_business_role` | the Roles screen (the `baOrDev` guard) |
| `create_form_def`, `update_form_def`, `delete_form_def` | the Forms builder |
| `create_form_integration`, `update_form_integration`, `delete_form_integration` | the form-to-metric posting screen |

## What stays human

- **Publishing a workflow.** The assistant creates and edits drafts. A draft
  is invisible to end users until a developer publishes it from the
  Workflows tab, and the assistant says so at the end of every workflow
  proposal. Because a rule cannot bind by id to an unpublished definition,
  the assistant binds rules **by name**; the engine resolves the name at
  fire time, so the rule goes live the moment the developer publishes.
- **Role membership.** The assistant creates a business role; a business
  admin decides who is in it.
- **Promoting the draft revision.** The developer promotes or discards.

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
model itself. When a divergence between the assistant and the developer
role appears, it is found by building something, not by reading the tool
list.

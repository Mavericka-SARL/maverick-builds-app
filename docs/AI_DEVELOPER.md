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
`add_dimension_member`, `update_dimension_member`, `create_grid`,
`add_grid_metric`, `add_grid_dimension`, `create_dashboard`,
`add_dashboard_widget`, `create_revision`, `generate_migration`,
`apply_migration`, `set_user_access_rules`.

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

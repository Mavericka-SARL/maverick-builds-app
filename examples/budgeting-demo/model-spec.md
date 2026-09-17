# Salary Budget Model — Reference

> **Classification:** Current — Readable contract for the model cmd/seed-payroll builds.

> **Last verified:** 2026-07-15
> **Authority:** `cmd/seed-payroll/main.go`; this file is a readable contract.

Human-readable reference for the model [`cmd/seed-payroll/main.go`](../../cmd/seed-payroll/main.go)
builds. This documents the Go source; it is not itself consumed by
anything — see the parent [README](README.md) for why.

Application: **Budgeting Demo**, workspace **Demo Workspace**, model
**Salary Budget Model**.

## Revisions

| Name | `system_managed` | Purpose |
|---|---|---|
| `Budget (Working)` | false | Active, editable planning cycle. |
| `Budget (Annual)` | true | Populated only by workflow approval (`on_approve`); `cells()` rejects direct writes to it unconditionally. |

## Dimensions

### `cost_centers` (flat, 3 members)

| Code | Label |
|---|---|
| `CC_SALES` | Sales |
| `CC_OPS` | Operations |
| `CC_GA` | General & Admin |

### `regions` (flat, 4 members) — property-derived

`source_dimension_id` → `employees`, `source_property` = `"region"`. Not a
structural child of `employees` — a dimension can only declare one
`parent_dimension_id`, and `employees` already uses it for `cost_centers`.
Instead, `grid()`/`resolveCrossDimensionValue` group `employees` members by
`properties->>'region'` when rolling up into a grid dimensioned by
`regions`.

| Code | Label |
|---|---|
| `LUX` | Luxembourg |
| `BE` | Belgium |
| `DE` | Germany |
| `FR` | France |

### `employees` (12 members) — cross-dimension child of `cost_centers`

`parent_dimension_id` → `cost_centers`; each member's `parent_member_id`
points at its owning cost-center member (the real, structural link — this
is what Grid 2's rollup and the write-lock's ancestor walk both use).
`properties` JSONB carries `{"region": "<code>"}`, round-robined LUX → BE →
DE → FR across the 12 employees (3 each).

4 employees per cost center, codes `E-<CC>-01`..`04` (e.g. `E-SALES-01`).
Monthly salary varies by employee (a spread from ~$4,800 to ~$7,650/month
for 2026), with a 5% raise applied for 2027.

### `months` (26 members) — same-dimension hierarchy, no separate `years` dimension

`2026` and `2027` are parent-less members *within* `months`; the 24 leaf
months (`2026-01` .. `2027-12`) each have `parent_member_id` pointing at
their year. This is deliberate per the original spec: years are hierarchy
levels inside `months`, not a second dimension.

## Metrics

| Name | Type | Formula | Format |
|---|---|---|---|
| `salary` | input | — | currency, 0 decimals |
| `social_tax` | calculated | `{salary} * 0.25` | currency, 0 decimals |

`social_tax`'s rate lives only in the formula string (the same mechanism
every calculated metric on this platform uses — there's no separate "rate
parameter" concept to hang it off). To change it, edit the metric's
formula (Developer Console → Metrics, or directly in `model.metric_def`).

## Grids

| Grid | Dimensions | `rollup_source_grid_id` | Editable |
|---|---|---|---|
| Salary by Employee & Month | `employees` × `months` | — | yes (the only editable grid) |
| Cost Center Summary | `cost_centers` × `months` | → Salary by Employee & Month | no |
| Region Summary | `regions` × `months` (`display_level=0`, i.e. year level) | → Salary by Employee & Month | no |

Cost Center Summary and Region Summary have **no `grid_metric` rows of
their own** — `rollup_source_grid_id` tells `grid()` to serve them the
source grid's metric list, and the frontend's cross-dimension resolver
(`resolveCrossDimensionValue`, `BusinessConsole.tsx`) computes each cell
from the source grid's `cells`/`all_dimensions` payload. That payload is
already scoped server-side to the caller (see "Access rules" below) before
it ever reaches the client, so the rollup arithmetic itself needs no
knowledge of who's asking. See the README's "What this demo added to the
platform" for the mechanism.

## Access rules

For each of the three cost-center managers, `identity.user_access_rule`
rows (`rule_type='dimension_member', access='hidden'`) hide:

- the 8 `employees` members belonging to the *other* two cost centers, and
- the 2 other `cost_centers` members themselves.

That's the only access-rule configuration this model needs — no `regions`-
specific rule exists, and none is required. `grid()` applies the hidden
`employees` rule to `cells`, `totals`, and `all_dimensions` on *every* grid
that references `employees` — directly (Salary by Employee & Month) or
transitively through a rollup (Cost Center Summary's structural rollup,
Region Summary's property-based rollup) — so all three grids end up
correctly scoped from these two rules alone. A region that happens to
contain another manager's employee still never leaks that employee's
value: only the caller's own visible employees contribute to any total they
see, on any grid.

`general_mgr`, `payroll_admin`, `developer`, and `platform_admin` have
no such rules — full, company-wide visibility on every grid.

## RACI

| User | Pattern | Type |
|---|---|---|
| `general_mgr` | `*` | `accountable` |
| `cc_mgr_sales` | `CC_SALES.*` | `responsible` |
| `cc_mgr_ops` | `CC_OPS.*` | `responsible` |
| `cc_mgr_ga` | `CC_GA.*` | `responsible` |

Read by `workflowStartInstance` to resolve a manager's own cost center
server-side (`source_hint: "raci_responsible"` — see below) and by
`cells()`'s write guard indirectly (via the workflow instance's resolved
`department` context value, not RACI directly).

## Business roles (dashboard visibility)

| Role | Members | Dashboard |
|---|---|---|
| Cost Center Managers | the 3 `cc_mgr_*` users | Cost Center Manager Dashboard |
| General Managers | `general_mgr` | General Manager Dashboard |

## Dashboards

**Cost Center Manager Dashboard**: `workflow_action` widget ("Submit
Plan", `widget_props.context = {"target_revision_id": "<Annual revision
id>"}`) + `grid` × 3 (all three grids above, in order).

**General Manager Dashboard**: `grid` × 2 (Cost Center Summary, Region
Summary — both unscoped for this viewer).

No dashboard widget in this demo has a `widget_type` other than `grid` or
`workflow_action` — both already existed generically before this demo.

## Workflow: Salary Budget Submission

One `workflow.workflow_def`, one approval step (`business_admin`
assignee), published.

`context_schema`:

| Key | Type | Required | Source |
|---|---|---|---|
| `department` | Dimension member (`cost_centers`) | yes | `raci_responsible` (server-resolved, overrides any client value) |
| `revision_id` | Text | yes | manual (always sent by `WorkflowActionWidget`'s baseline context) |
| `target_revision_id` | Text | yes | manual (from the Submit Plan widget's static `widget_props.context`) |

Step's raw JSON (beyond the typed `workflowv1.WorkflowStepDef` proto —
same pattern as the platform's existing `routes` field):

```json
{
  "required_comment": true,
  "on_approve": {
    "scope_context_key": "department",
    "copy_facts_to_context_key": "target_revision_id"
  }
}
```

On final approval: every `employees` member descending from the resolved
`department` cost center (walked generically via `parent_member_id`, not
hardcoded) has its latest `salary` fact copied from `context.revision_id`
into `context.target_revision_id`, then `social_tax` is recalculated
there.

## Audit

The seed program writes five representative `audit.audit_event` rows
(user creation ×2, metric creation ×2, access-rule creation ×1) so the
Audit Log isn't empty on first load. Interactive actions (writebacks, task
completions, workflow instance creation) are audited generically by the
handlers that already do this for every demo — nothing payroll-specific
there either.

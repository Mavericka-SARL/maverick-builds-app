# Salary Budgeting Demo — Build Instructions

> **Classification:** Current — Acceptance contract for the demo built by cmd/seed-payroll.

> **Status:** Implemented acceptance contract
> **Last verified:** 2026-07-15
> **Operational guide:** [`README.md`](README.md)
>
> The seeded model, seven personas, three manager scopes, three grids, native
> XLSX/CSV import, approval/rejection flow, and protected annual revision are
> live. The generic approval action is currently post-commit/best-effort and its
> copy query supports this demo's one writable salary metric but can drop sibling
> metrics at the same intersection in a broader model. See the README's known
> limitations and the root `ARCHITECTURE.md`.

## Objective

Build a platform-native salary budgeting demo in which three cost-center
managers plan employee salaries for 2026 and 2027, submit their own plans to a
General Manager, and have approved values copied into a protected annual budget
revision.

The demo must exercise maverickbuilds.app's generic dimensions, member
hierarchies, member properties, metrics, grids, access rules, dashboards,
revisions, imports, workflows, calculations, and audit history. Do not add
payroll-specific endpoints, database tables, handlers, roles, or dashboard
widget types.

Use the existing implementation as the starting point:

- seed: `cmd/seed-payroll/main.go`
- demo documentation: `examples/budgeting-demo/README.md`
- model reference: `examples/budgeting-demo/model-spec.md`
- generic rollup/workflow tests:
  `internal/gateway/generic_rollup_workflow_test.go`
- grid API and workflow actions: `internal/gateway/handler.go`
- grid rendering and rollups:
  `web/src/consoles/business/BusinessConsole.tsx`

The finished seed must remain idempotent. Re-running it must restore the demo to
its initial open state without creating duplicate users, definitions, role
assignments, access rules, dashboard widgets, or workflow instances.

## Non-negotiable business rules

1. There are exactly three cost centers and exactly one manager for each cost
   center.
2. A cost-center manager can read, edit, import, submit, and review only data
   belonging to that manager's cost center.
3. The restriction applies to every representation of the data, including raw
   grid cells, calculated metrics, totals, cost-center rollups, region/year
   rollups, exports, and import validation errors. Hiding rows in the frontend
   is not sufficient.
4. The General Manager can see all three cost centers and can approve or reject
   each submitted plan independently.
5. Only salary is writable. Social tax and both summary grids are calculated or
   aggregated and are always read-only.
6. Approval copies only the approved cost center's salary plan into the annual
   budget revision. It must not copy facts for either sibling cost center.
7. The annual budget revision is system-managed and cannot be changed through
   normal cell writeback or import.
8. Years are parent members of the `months` dimension. Do not create a separate
   `years` dimension.
9. Regions are derived from an employee property. Do not make regions a second
   structural parent of employees.
10. The demo must support direct entry and native `.xlsx` import for salary.

## Seeded organization and users

Create or reuse one customer, one workspace, one application, and one model:

| Object | Name |
|---|---|
| Workspace | Demo Workspace |
| Application | Budgeting Demo |
| Model | Salary Budget Model |

Seed these seven personas:

| Persona key | Platform role | Business responsibility | Data scope |
|---|---|---|---|
| `platform_admin` | `platform_admin` | Platform administration | Full platform |
| `developer` | `developer` | Model and demo development | Full demo model |
| `payroll_admin` | `tenant_admin` | Workspace/user administration | Full workspace |
| `general_mgr` | `business_admin` | Approves or rejects plans | All three cost centers |
| `cc_mgr_sales` | `business_user` | Sales cost-center manager | `CC_SALES` only |
| `cc_mgr_ops` | `business_user` | Operations cost-center manager | `CC_OPS` only |
| `cc_mgr_ga` | `business_user` | General & Admin manager | `CC_GA` only |

Reuse the standard platform-admin and developer identities used by other demos
when possible. Assign the General Manager an `accountable` RACI rule for `*`.
Assign each cost-center manager one `responsible` RACI rule matching only that
manager's cost center.

## Revisions

Create two model revisions:

| Revision | System managed | Purpose |
|---|---:|---|
| `Budget (Working)` | no | Editable salary plans and calculated social tax |
| `Budget (Annual)` | yes | Approved salary plans copied by workflow action |

All planning starts in `Budget (Working)`. `Budget (Annual)` must start empty so
the approval action is visible during the demo.

An approval is scoped by cost center. Approving Sales must populate only Sales
employee facts in `Budget (Annual)`; Operations and General & Admin remain empty
until their own plans are approved.

## Dimensions

### 1. `cost_centers`

Create a flat dimension with exactly three members:

| Code | Label |
|---|---|
| `CC_SALES` | Sales |
| `CC_OPS` | Operations |
| `CC_GA` | General & Admin |

### 2. `regions`

Create a flat, property-derived dimension. Use deterministic example members:

| Code | Label |
|---|---|
| `LUX` | Luxembourg |
| `BE` | Belgium |
| `DE` | Germany |
| `FR` | France |

Configure:

```text
regions.source_dimension_id -> employees
regions.source_property = "region"
```

### 3. `employees`

Create 12 leaf members, four per cost center. Configure the dimension's
cross-dimension parent as:

```text
employees.parent_dimension_id -> cost_centers
```

Each employee member must have:

- `parent_member_id` pointing to the employee's cost-center member;
- a stable employee code;
- a readable employee label; and
- `properties = {"region": "<region code>"}`.

Distribute the region properties across cost centers so the region rollup proves
that property grouping works within each manager's security scope.

### 4. `months`

Create one same-dimension hierarchy containing 26 members:

- root members `2026` and `2027`;
- leaf members `2026-01` through `2026-12`; and
- leaf members `2027-01` through `2027-12`.

Each month leaf's `parent_member_id` must point to the appropriate year member.
The year is a hierarchy level inside `months`, not a separate dimension.

## Metrics

Create two currency metrics in both revisions:

| Metric | Type | Formula | Aggregation | Editable |
|---|---|---|---|---:|
| `salary` | input | none | sum | Working revision only |
| `social_tax` | calculated | `{salary} * 0.25` | sum | never |

For this demo, use a 25% social-tax rate. Keep the rate in the generic metric
formula rather than adding payroll-specific configuration.

Recalculate social tax after all of these events:

- seed data creation;
- direct salary writeback;
- salary import; and
- approval copy into `Budget (Annual)`.

## Grids

### Grid 1 — Salary by Employee and Month

Configure:

```text
dimensions: employees x months (leaf months)
metrics: salary, social_tax
revision: Budget (Working)
```

Behavior:

- salary is editable by direct cell input;
- social tax is calculated and read-only;
- salary can be imported from an Excel workbook;
- only the manager's own employees are returned to that manager; and
- submitting or approving the manager's cost center locks the applicable cells
  according to the workflow rules below.

### Grid 2 — Salary by Cost Center and Month

Configure:

```text
dimensions: cost_centers x months (leaf months)
source: Grid 1
metrics: salary, social_tax inherited from Grid 1
aggregation: employees -> cost_centers, sum
editable: no
```

This grid must aggregate Grid 1 through the structural relationship between
employees and cost centers. A manager sees exactly one cost-center row. The
General Manager sees all three.

### Grid 3 — Salary by Region and Year

Configure:

```text
dimensions: regions x months (display_level = 0, year parents only)
source: Grid 1
metrics: salary, social_tax inherited from Grid 1
aggregation:
  employees -> regions through employees.properties.region
  leaf months -> 2026/2027 through months.parent_member_id
editable: no
```

For a cost-center manager, calculate each region/year value using only employees
from that manager's cost center. A region may therefore appear for more than one
manager but with different values. The General Manager sees the company-wide
sum.

This requirement deliberately supersedes the current demo behavior documented
in `README.md`, where Grid 3 is company-wide for every viewer.

## Access-control implementation

Seed hidden `dimension_member` access rules for each manager:

- hide the eight employees belonging to the other two cost centers; and
- hide the other two cost-center members.

These rules are necessary but are not sufficient by themselves. The current
grid response can contain unfiltered `all_dimensions`, `cells`, and `totals`
even after visible dimension rows are filtered. Correct the generic server-side
grid path so unauthorized facts cannot reach the browser.

The effective fact scope for a cost-center manager must be applied before:

- serializing `cells`;
- calculating `totals`;
- supplying `all_dimensions` member data used by cross-dimension rollups;
- resolving employee-to-cost-center rollups;
- resolving employee-property-to-region rollups; and
- exporting grid data.

Preserve any visible parent members needed to render a permitted hierarchy, but
do not preserve hidden leaf members or their fact values. Never depend on a
React filter for confidentiality.

Apply the same member-scope validation to writes and imports. Reject a request
that includes an employee outside the caller's cost center even if the caller
constructs the request manually.

## Excel import

Provide a downloadable `.xlsx` template and accept `.xlsx` uploads through the
Import Wizard. The workbook must use a long fact format with one row per salary
cell:

| employees | months | salary |
|---|---|---:|
| `E-SALES-01` | `2026-01` | 5200 |

Import rules:

- map the `salary` column to the salary metric without requiring users to paste
  an internal metric UUID into Excel;
- validate employee and month codes;
- accept only leaf months;
- accept numeric, non-negative salary values;
- use replace/upsert semantics for the listed cells, not additive delta
  semantics;
- recalculate social tax after a successful commit;
- reject the whole workbook atomically if any row is invalid;
- reject employees outside the manager's cost-center scope;
- reject imports while that cost center is submitted or approved; and
- reject all direct imports into `Budget (Annual)`.

If the generic import service currently supports only CSV, add generic `.xlsx`
parsing and metric-name mapping there. Do not implement an endpoint that works
only for this demo. Documenting “save the workbook as CSV” does not satisfy the
native Excel-import acceptance criterion.

## Dashboards

### Cost Center Manager Dashboard

Create one reusable dashboard and assign it to a `Cost Center Managers`
business role containing all three manager users. Access rules and RACI context
must personalize the shared dashboard, so three duplicate dashboard definitions
are unnecessary.

Include:

1. Submit Salary Plan workflow action;
2. Salary by Employee and Month grid;
3. Salary by Cost Center and Month grid; and
4. Salary by Region and Year grid.

Show the manager's cost center and plan status (`Open`, `Submitted`, `Rejected`,
or `Approved`). Disable Submit when the plan is already submitted or approved.
After rejection, display the rejection comment and allow edits and resubmission.

### General Manager Dashboard

Assign a second dashboard to a `General Managers` business role containing the
General Manager. Include:

- all-cost-center monthly summary;
- company-wide region/year summary;
- pending submission count; and
- a direct route to the Workflow Inbox.

Approval and rejection must use the generic Workflow Inbox rather than a custom
payroll approval widget.

## Workflow

Create and publish one workflow definition named `Salary Budget Submission`
with one approval step assigned to `business_admin`.

Required context:

| Key | Type | Source |
|---|---|---|
| `cost_center` | Dimension member (`cost_centers`) | resolved server-side from caller's responsible RACI rule |
| `revision_id` | Text | `Budget (Working)` revision ID |
| `target_revision_id` | Text | `Budget (Annual)` revision ID |

The server must override any client-supplied cost center with the value resolved
from RACI. A manager must not be able to submit another manager's cost center by
changing request JSON.

Workflow states and effects:

| Event | Result |
|---|---|
| Manager submits | Create one pending instance for that cost center; lock its Working salary cells and imports |
| General Manager rejects | Require a comment; complete as rejected; unlock Working cells; allow resubmission |
| General Manager approves | Require a comment; copy only that cost center's latest salary facts to Annual; recalculate Annual social tax; keep the submitted Working scope locked |

Prevent duplicate active submissions for the same cost center. Submissions from
different cost centers may be pending at the same time.

Approval copy must be atomic: either all scoped salary facts and their resulting
calculated facts are available in Annual, or none are.

## Audit requirements

Record at least these events with actor, timestamp, application/model, revision,
and cost-center context where applicable:

- salary cell write;
- salary import;
- workflow submission;
- rejection and comment;
- approval and comment; and
- approval copy into Annual.

Managers can review their own workflow history. The General Manager and admin
personas can review all three cost centers.

## Verification

### Seed verification

At the end of `go run ./cmd/seed-payroll`, verify and print pass/fail checks for:

- seven users and their expected roles;
- exactly three cost centers;
- 12 employees, four under each cost center;
- every employee having a valid region property;
- two year parents and 24 month leaves;
- metric formulas and dependencies;
- all three grid definitions and rollup links;
- manager access rules and business-role memberships;
- workflow context and approval action configuration;
- empty Annual facts after a clean reseed; and
- no active workflow instances after a clean reseed.

### Automated gateway tests

Extend `internal/gateway/generic_rollup_workflow_test.go` with deterministic
tests proving:

1. Sales manager's Grid 1 response contains only Sales employees and Sales fact
   keys.
2. Sales manager's Grid 2 contains only `CC_SALES` and equals the sum of Sales
   employees.
3. Sales manager's Grid 3 region/year values exclude Operations and G&A facts,
   including from `cells`, `totals`, and `all_dimensions`.
4. The General Manager receives all employees and correct company-wide rollups.
5. Direct write and Excel import reject an out-of-scope employee.
6. Excel import uses replacement semantics and recalculates social tax.
7. Empty-comment approve/reject is rejected.
8. Rejection unlocks only the rejected cost center.
9. Approval copies only the submitted cost center into Annual.
10. Annual rejects direct writeback and import.
11. A forged workflow context cannot change the submitter's cost center.
12. Two cost centers can have independent workflow instances and decisions.

For every rollup assertion, use hand-computable fixture values rather than only
checking that a non-zero number was returned.

### Frontend scenario

Add a Playwright scenario that switches among the three manager personas and
the General Manager. It must demonstrate:

1. each manager sees a different employee set;
2. each manager sees only their scoped values in all three grids;
3. direct editing updates social tax and both summaries;
4. an `.xlsx` upload replaces salary values and updates summaries;
5. submission locks the correct cost center;
6. rejection with a comment unlocks and permits resubmission;
7. approval populates the correct Annual facts; and
8. another cost center remains editable and absent from Annual until separately
   approved.

## Completion criteria

The demo is complete only when all of the following are true:

- the seed is idempotent and its structural checks pass;
- the automated access, rollup, import, and workflow tests pass;
- the frontend scenario passes;
- the manager browser/API responses contain no facts from sibling cost centers;
- approved facts are present in Annual only for approved cost centers;
- the README and model specification match the implemented manager-scoped
  region/year behavior and native Excel import; and
- the implementation contains no budgeting- or payroll-specific platform
  endpoint, table, role, or widget type.

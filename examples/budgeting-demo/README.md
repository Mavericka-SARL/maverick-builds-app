# Budgeting Demo — Salary Budget Submission

> **Classification:** Current — Walkthrough of a demo that still ships in cmd/.

> **Last verified:** 2026-07-15
> Build and hardening requirements are defined in
> [`BUILD_INSTRUCTIONS.md`](BUILD_INSTRUCTIONS.md). This README reflects
> that instruction's fully-implemented state — manager-scoped Grid 3, native
> `.xlsx` import — not an earlier, superseded version of the demo.

A cost-center salary planning and approval scenario built entirely from
Mavericks Engine's generic platform primitives (dimensions, metrics, grids,
dashboards, workflows, access rules). It demonstrates:

- multidimensional planning (a structural hierarchy *and* a property-based
  grouping crossed against a third, independent dimension)
- calculated metrics with a real formula
- cross-dimension and same-dimension **rollup** (two different generic
  mechanisms, both exercised), fully scoped to each viewer's visible facts
- row-level business-user access scoping, enforced on every representation
  of the data a grid response returns — not just the rows a UI renders
- a RACI-scoped approval workflow with a comment-required approval/rejection
  path and an approval action that copies data into a locked revision
- native `.xlsx` and CSV import, open to any authenticated user and scoped
  by the same access rules as interactive writes
- audit history

**Nothing in this demo is bespoke code.** There is no `payroll_handler.go`,
no `internal/payroll` package, no `payroll_*` dashboard widget type. Every
piece of behavior below is configuration (seeded via
[`cmd/seed-payroll/main.go`](../../cmd/seed-payroll/main.go)) sitting on top
of engine features in `internal/gateway` and `web/src/consoles/business/BusinessConsole.tsx`
that are equally available to any other application on the platform. Where
the engine didn't already support what this demo needed, the engine itself
was extended — see "What this demo added to the platform" below.

## Why a Go program, not YAML

Every existing demo on this platform (`cmd/seed`, `cmd/seed-budget`,
`cmd/seed-sales`, `cmd/seed-procurement`) is a Go program that calls the
same store packages and SQL a developer's own tooling would use — that Go
source *is* this platform's code-first definition today; there is no
YAML/config-loader anywhere in the codebase. `cmd/seed-payroll/main.go`
follows that same convention. This README and `model-spec.md` are
human-readable documentation of what that program builds, not a consumed
config format.

## Running it

```bash
# Postgres, Keycloak, MinIO, Redis, NATS
make dev-up

# Seed the demo (idempotent — safe to re-run any time to reset to a fresh state)
DATABASE_URL=postgres://mavericks:mavericks@localhost:5432/mavericks?sslmode=disable \
  go run ./cmd/seed-payroll

# Backend
bash dev.sh

# Frontend
cd web && VITE_DEV_MODE=true npm run dev
```

Open `http://localhost:5173`. Use the "Dev — Persona" switcher in the
sidebar footer to switch between the seven users below.

Re-running `go run ./cmd/seed-payroll` at any point resets the demo:
dimensions/metrics/grids/dashboards/workflow config are upserted in place,
and any workflow instances left over from prior interactive use are cleared,
so every cost center always starts back at "open" after a reseed. The seed
program's own verification pass is structural only (see "Tests" below) — it
does not submit or approve anything, so `Budget (Annual)` and the General
Manager Dashboard's summaries are empty until you walk through the demo
script below at least once.

## Users

All users are seeded into one workspace ("Demo Workspace"). `platform_admin`
and `demo_developer` reuse the same accounts every other demo on this
platform reuses (`demo-platform-admin-001` / `demo-dev-001`), to avoid
piling up duplicate admin/dev accounts across demos.

| Persona switcher key | User | Role | Scope |
|---|---|---|---|
| `platform_admin` | platform-admin@acme.com | `platform_admin` | everything |
| `developer` | dev@acme.com | `developer` | everything |
| `payroll_admin` | tenant-admin+payroll@acme.com | `tenant_admin` | this workspace |
| `general_mgr` | general.manager@acme.com | `business_admin` | all 3 cost centers (RACI `accountable` on `*`); approves/rejects |
| `cc_mgr_sales` | cc.manager.sales@acme.com | `business_user` | `CC_SALES` only (RACI `responsible` on `CC_SALES.*`) |
| `cc_mgr_ops` | cc.manager.ops@acme.com | `business_user` | `CC_OPS` only |
| `cc_mgr_ga` | cc.manager.ga@acme.com | `business_user` | `CC_GA` only |

A cost-center manager's scope is enforced two independent ways, both
generic (not payroll-specific):

- **Read**: `identity.user_access_rule` rows (`rule_type='dimension_member',
  access='hidden'`) hide the other two cost centers' 8 employees *and* the
  other two `cost_centers` members themselves — the existing `grid()`
  filter already applies to any dimension, so hiding the `cost_centers`
  rows is what scopes Grid 2 (see below), not just Grid 1.
- **Write**: `cells()`'s write guard checks the same access rules, plus a
  workflow-instance lock (see "What this demo added to the platform").

## Data model

Full detail in [`model-spec.md`](model-spec.md). Summary:

- **`cost_centers`** (3, flat): `CC_SALES`, `CC_OPS`, `CC_GA`
- **`regions`** (4, flat): `LUX`, `BE`, `DE`, `FR` — **property-derived**
  from `employees` (`source_dimension_id`/`source_property`), not a
  structural parent (a dimension can only have one, and `employees` already
  spends it on `cost_centers`)
- **`employees`** (12): cross-dimension parent → `cost_centers`; each
  member's `properties` JSONB carries `{"region": "..."}`
- **`months`** (26): `2026`/`2027` as same-dimension parent ("year")
  members, with 24 leaf months underneath — **no separate `years`
  dimension**
- **`salary`** — input metric, `[employees, months]`
- **`social_tax`** — calculated metric, formula `{salary} * 0.25`

Three grids, all reachable from the dashboards:

1. **Salary by Employee & Month** — the only editable grid, dimensioned by
   `[employees, months]`.
2. **Cost Center Summary** — read-only, dimensioned by
   `[cost_centers, months]`, `rollup_source_grid_id` → Grid 1. Cross-dimension
   rollup (employees → cost_centers).
3. **Region Summary** — read-only, dimensioned by `[regions, months@year-level]`,
   `rollup_source_grid_id` → Grid 1. Property-derived rollup (employees →
   regions) *and* same-dimension hierarchy rollup (months → years) at once.

Grid 2's rows are scoped per cost-center manager the same way Grid 1's are
(hidden `cost_centers` members). **Grid 3 is also manager-scoped** — a
region's total only ever sums that manager's own visible employees, never a
sibling cost center's, even though a region cuts across cost centers.
Getting this right needed no region-specific access rule at all: `grid()`
now excludes a hidden `employees` member from `all_dimensions` (the payload
the region rollup groups by) and from every fact row referencing it,
wherever that member is hidden — the existing employee-hiding rule alone is
enough to correctly scope Grid 1, 2, *and* 3. See "What this demo added to
the platform" below for the fix. The General Manager, who has no hidden
members, still sees the company-wide sum on Grid 3.

## Workflow: Salary Budget Submission

One workflow def, one approval step, assignee role `business_admin`
(General Manager). Three things make it generic rather than payroll-specific:

- **`context_schema`** declares `department` as a `"Dimension member"`
  variable bound to `cost_centers`, with `source_hint: "raci_responsible"` —
  the server resolves and *overrides* this from the caller's
  `security.raci_rule` grant every time the workflow starts, so the same
  "Submit Plan" dashboard button works correctly for all three managers
  without hardcoding a cost center per widget instance, and a manager can't
  spoof another cost center's scope by editing the request.
- **`required_comment: true`** on the step — enforced server-side (see
  below), so General Manager approvals and rejections both need a note.
- **`on_approve: {scope_context_key: "department", copy_facts_to_context_key: "target_revision_id"}`**
  on the step's raw JSON — on final approval, every fact under the approved
  cost center's employees is copied from `Budget (Working)` into
  `Budget (Annual)` (a `system_managed`, read-only-via-`cells()` revision),
  and `social_tax` is recalculated there.

Submitting is the existing generic `workflow_action` dashboard widget
(`POST /api/workflow/instances`) — the "Submit Plan" button on the Cost
Center Manager Dashboard is just that widget, configured with
`widget_props.context = {"target_revision_id": "<Annual revision id>"}`.
Approving/rejecting is the existing Workflow Inbox — General Manager
reviews and decides there, same as every other demo's approval workflow.
Status and rejection comments are visible in the Workflow Inbox and History
tabs; there's no bespoke "status" dashboard widget.

## What this demo added to the platform

Building this surfaced real, general gaps — each fixed in the engine, not
routed around. Two rounds: the original rollup/workflow work, and a second
pass closing the read-side security gap and rebuilding import natively.

### Round 2 — grid security scoping and native import

1. **`grid()` now scopes `cells`, `totals`, and `all_dimensions` by the
   caller's hidden `dimension_member` access rules — not just the primary
   `dimensions` member list.** Previously only the member *list* was
   filtered; the fact values (`cells`), aggregate (`totals`), and the
   full-model member payload (`all_dimensions`, what a rollup grid's
   cross-dimension aggregation actually reads) were sent unfiltered to every
   caller. A cost-center manager's Grid 3 region total was silently
   including every other manager's staff. Fixed once, generically
   (`hiddenCodesByDim`/`factRowHidden`/`filterHiddenMembers` in
   `internal/gateway/handler.go`), so it applies to any grid, any dimension,
   any demo — no region- or payroll-specific code. A calculated metric's
   scoped total (e.g. `social_tax`) is re-evaluated from the caller's scoped
   input totals using the platform's own formula engine
   (`internal/formula.EvalNumber`), not a re-implementation of `* 0.25`
   somewhere in Go.
2. **`internal/writeguard`** — the system-managed / hidden-access /
   workflow-lock write checks used to live only inline in the gateway's HTTP
   handlers, which meant the standalone gRPC `ImportService` could commit
   facts that bypassed every one of them. Extracted into a shared package
   with no dependency on `gateway`, so `importpkg.Store.CommitImport` (used
   by both the gRPC service and the HTTP handler) enforces the identical
   check regardless of transport.
3. **Native `.xlsx` import, generically, in `internal/importpkg`** —
   `ParseXLSXRows` (via `excelize`, already a repo dependency) and
   `ResolveRows` replace the old CSV-only, explicit-`metric_id`-column-only
   pipeline. A column header now matches a metric or dimension by *name*
   (`salary`, `employees`, `months`) — no UUID pasting — with the old
   `metric_id`/`value` columns still accepted for backward compatibility.
   Every referenced member is validated as a leaf (structurally — no
   dimension-specific config), every value must be non-negative, and if any
   row fails validation the whole file is rejected atomically: nothing is
   staged or committed, matching "replace/upsert semantics for the listed
   cells" rather than a silent partial import. A new `replace` import mode
   sits alongside the existing `incremental`/`full_reload` — it inserts the
   file's values as-is without deleting anything, staying consistent with
   `fact_input`'s append-only/latest-wins design (see
   `importpkg.ImportMode`).
4. **Import is reachable by any authenticated user, not just `developer`.**
   `/api/import/upload` was gated to the literal `developer` role, so a
   cost-center manager — who can write their own cells directly — had no
   way to import them. The route is now open like `/api/cells`; the generic
   write guard decides what's allowed, not a platform role. `BusinessConsole.tsx`'s
   `ImportTab` (previously dead code, never rendered) is wired into the
   business console's nav and does the real thing: uploads a native `.xlsx`
   file (or CSV), downloads a template generated from the caller's own
   *visible* grid (so a manager's template already only shows their own
   dimension members), and renders the atomic per-row validation errors
   inline if the file is rejected.

### Round 1 — cross-dimension rollup and RACI-scoped workflow

5. **`resolveCrossDimensionValue`** (`BusinessConsole.tsx`) only resolved
   metrics assigned to exactly one dimension. Rewritten to resolve *every*
   one of a metric's dimensions against the current grid's dimensions —
   exact match (same dimension, including collapsing a same-dimension
   hierarchy to a parent level), structural relation
   (`parent_dimension_id`), or property relation (`source_dimension_id`/
   `source_property`) — then combines them via Cartesian product. This is
   what makes Grid 2 and Grid 3 possible as plain `grid` widgets.
6. **`grid_def.rollup_source_grid_id`** (migration `055`) — a grid with no
   `grid_metric` rows of its own can mirror another grid's metrics, rolled
   up. `resolveCell`/`getVal` fall back to the cross-dimension resolver
   when a direct cell lookup misses (needed for input-metric cells, not
   just calculated ones), and such cells render read-only.
7. **`dimension_def.source_dimension_id`/`source_property`** (migration
   `055`) — the property-based counterpart to `parent_dimension_id`.
   `dimension_member.properties` existed but nothing read it back; `grid()`
   now exposes it.
8. **RACI-scoped, spoof-proof workflow context** — `workflowStartInstance`
   resolves any `context_schema` entry with `source_hint: "raci_responsible"`
   from `security.raci_rule` server-side, and dedupes concurrent submissions
   for the same resolved scope.
9. **Generic workflow-instance write lock** — `cells()` now rejects a write
   if the member (or an ancestor) is the scope of a workflow instance that's
   running, *or* was approved (this second half — approve vs. reject both
   leave `taskAction`'s instance status "completed"; the true state was
   only readable from the step's `decision` — is exactly the kind of
   pre-existing platform quirk this work surfaced and fixed).
10. **`required_comment` enforcement** — the field already existed as a
    read-side UI hint (`tasks()`); `taskAction` never checked it. Now does,
    as a blanket requirement (matches the field's existing UI semantics,
    not decision-specific — confirmed for both approve and reject).

Smaller, adjacent fixes from round 1:

- `cells()` was missing a generic `rule_type='dimension_member'` write
  check entirely (only `grid()`'s read path filtered hidden members) —
  fixed independently of the rest of this work, benefits every demo.
- Revision duplication (`POST /api/developer/revisions`, used when a
  developer branches a new revision from an existing one) didn't remap
  `rollup_source_grid_id` / `source_dimension_id` / `grid_dimension.display_level`
  for the copy, leaving a duplicated revision with dangling references back
  into the revision it was copied from — fixed so a copied revision is
  fully self-contained. This is the property that also matters for a model
  being exported/transferred between applications.
- `POST /api/import/upload` called `importpkg.CommitImport` directly,
  bypassing every one of `cells()`'s write checks. Patched inline at the
  time; superseded in round 2 by moving the check into
  `internal/writeguard` so it can't be bypassed by a *different* transport
  either (see round 2, item 2).

All of the above is covered by `internal/gateway/generic_rollup_workflow_test.go`
(`go test ./internal/gateway/... -run TestGridExposesRollup` etc. — see
"Tests" below). The rollup *arithmetic* itself is frontend TypeScript with
no existing unit-test runner in this repo (only mocked Playwright E2E for
UI structure); it was verified by hand against a real running instance —
browser rendering cross-checked against hand-computed sums — not by a
permanent automated frontend test. Round 2's server-side scoping fix is
covered by a Go test (`TestGridScopesCellsTotalsAndAllDimensionsByHiddenMembers`)
that reads the raw `cells`/`totals`/`all_dimensions` payload directly, so it
doesn't depend on the frontend rollup code at all.

## Demo script

1. Switch persona to `cc_mgr_sales` (or either of the other two cost-center
   managers — all three start `open` after a reseed).
2. **Dashboards → Cost Center Manager Dashboard.** Edit a salary cell in
   "Salary by Employee & Month" (e.g. change `Jan 2026` for one employee).
   Watch "Social Tax" recompute (25% of salary) and the "Cost Center
   Summary"/"Region / Year Summary" widgets update.
3. Try editing an employee outside your cost center directly via the API
   (or note that you can't see them at all in the grid) — the generic
   `dimension_member` access rule hides and blocks them. Also confirm
   "Region / Year Summary" only ever totals *your* employees' regions, even
   when a sibling manager's employee shares one of your regions.
4. **Import → Download .xlsx template**, edit a couple of salary values in
   the downloaded workbook, then **Upload .xlsx** it back. The columns are
   `employees`/`months`/`salary` by name; the committed values replace
   exactly the cells the file lists. Try uploading a workbook with a
   negative value or a non-leaf `months` code (e.g. `2026` instead of
   `2026-01`) — the whole file is rejected, nothing is committed.
5. Click **Submit Plan**. The button becomes disabled; the plan is now
   locked (try the edit again — `cells()` rejects it, "locked by an
   in-progress or approved workflow").
6. Switch persona to `general_mgr`. **Workflow Inbox** — see the pending
   approval, with the submitting cost center in its context.
7. Try **Approve** or **Reject** with an empty comment — both rejected
   client- and server-side (`required_comment`).
8. Reject with a real comment (e.g. "Please revise headcount"). The
   instance closes.
9. Switch back to the cost-center manager persona — the plan is unlocked
   again, editable, and the rejection comment is visible in **My
   History**.
10. Edit a cell, **Submit Plan** again.
11. Switch to `general_mgr`, **Approve** with a comment in the Workflow
    Inbox.
12. Switch back to the cost-center manager — the plan is now permanently
    locked (`approved`).
13. Switch to `general_mgr` → **General Manager Dashboard** — "Cost Center
    Summary (All)" and "Region / Year Summary (All)" show every cost
    center, unscoped.
14. Switch to `developer` → check `runtime.fact_input` for the
    `Budget (Annual)` revision (or use `psql`/the grid API with
    `revision_id` set to the Annual revision) — the just-approved cost
    center's facts are there; the others aren't yet.
15. As any persona, confirm **Models → (this app's audit trail)** shows the
    seed's initial audit events (user creation, metric creation, access
    rule creation).
16. Repeat steps 4–14 for a second cost-center manager (e.g. `cc_mgr_ops`)
    concurrently with the first still pending/rejected — the two scopes'
    workflow instances are entirely independent; approving or rejecting one
    never touches the other's lock state or Annual facts.

## Import

`POST /api/import/upload` accepts either a native `.xlsx` workbook
(`xlsx_base64`, parsed server-side by `internal/importpkg.ParseXLSXRows`) or
CSV text (`csv`) — both go through the same
`internal/importpkg.ResolveRows` validation, so neither is a second-class
path. **Business Console → Import** downloads a template generated from the
caller's own visible grid and uploads directly; the endpoint itself needs no
special role, only the same access a manager already has for interactive
cell writes.

**Column headers are names, not IDs.** `employees`, `months`, `regions` map
to their dimensions; a column named after a metric (`salary`) is that
metric's value column. The old explicit `metric_id`/`value` columns (the
metric referenced by raw UUID or name) still work for backward
compatibility — see [`salary-import-template.csv`](salary-import-template.csv)
for that form. Every referenced member must be a **leaf** (rejected
otherwise — e.g. a `months` value of `2026` rather than `2026-01`), and
every value must be **non-negative**.

**The whole file is atomic.** If any row fails validation (unknown
column, unknown member, non-leaf member, unparseable or negative value),
the entire upload is rejected with a `422` and a per-row error list —
nothing is staged or committed, not even the file's otherwise-valid rows.

**Import mode.** `replace` (the Business Console's default) inserts the
file's values as-is for exactly the cells it lists — untouched cells keep
their prior value, and nothing is deleted (`fact_input` is an append-only,
latest-wins log; "replace" means "this file's cells are now the latest
value," not a destructive delete). `incremental` *adds* the imported value
to the cell's current latest value — a delta, useful for accumulating
partial exports over a period. `full_reload` deletes **every** fact in the
target revision first, then inserts only the file's rows — a from-scratch
load, not a partial correction.

Also: the import endpoint resolves the target model from ambient
`X-App-Id` request context (the currently-selected app), not from anything
in the file — importing from outside the running UI (e.g. `curl`) needs
that header set explicitly, or the import silently lands in whichever
application happens to be the caller's default.

## Tests

```bash
go test ./internal/gateway/... -run "TestGridExposesRollupAndPropertyMetadata|TestWorkflowStartInstanceRACIResolutionAndDedup|TestCellsWriteLockDistinguishesApproveFromReject|TestOnApproveCopiesFactsToTargetRevision|TestRevisionDuplicationRemapsRollupAndSourceDimension|TestImportRespectsWorkflowLockAndSystemManaged|TestGridScopesCellsTotalsAndAllDimensionsByHiddenMembers|TestImportXLSXNativeParsingNameBasedMapping|TestImportRejectsNegativeValueAtomically|TestWorkflowTwoScopesIndependentInstancesAndApproveRequiresComment|TestCellsRejectsDirectWriteToHiddenMember" -v
```

Spins up a real, ephemeral Postgres via `testcontainers-go` (same pattern as
`internal/gateway/trigger_catalog_test.go`) — no `DATABASE_URL` needed, and
independent of `cmd/seed-payroll`'s own data (a small synthetic
departments/staff/regions model with two department-scoped manager personas,
so these run fast and don't depend on the demo being seeded). Covers: grid
rollup metadata exposure, RACI-scoped workflow start (including spoofing
prevention and dedup), the approve-vs-reject write-lock distinction,
`required_comment` enforcement for *both* decisions, `system_managed`
revision protection, `on_approve`'s copy action (correctly scoped — a
sibling department's facts are confirmed *not* copied), revision-duplication
remapping, the import path respecting the same write-lock/`system_managed`
checks as interactive writes, **grid()'s hidden-member scoping across
`cells`/`totals`/`all_dimensions` on both a direct and a rollup grid**,
**`cells()` rejecting a direct write to a hidden member independent of any
workflow lock**, **native `.xlsx` parsing with name-based column mapping
and leaf-member rejection**, **atomic whole-file rejection on a single
invalid row**, and **two department scopes running fully independent
workflow instances** (rejecting one doesn't unlock or otherwise affect the
other).

`go run ./cmd/seed-payroll` also runs a structural verification pass at the
end (grid/dimension wiring, user roles, employee/region/month counts, access
rules, business-role membership, workflow config, empty-Annual and
zero-instances after a clean reseed) — printing ✓/✗ per check. It's
intentionally *not* a behavioral (submit/approve/reject) check: that logic
now lives in generic gateway code with no reusable Store package for a seed
program to call into directly, so behavioral verification is the Go tests
above plus the manual demo script.

## Known limitations

- HTTP approval commits the task before running `on_approve` copy and
  recalculation. Those actions are best-effort, so a copy failure can leave an
  approved task without a complete Annual update. The copy query also selects
  one fact per dimensional intersection instead of per metric and intersection;
  this demo has only one writable input metric (`salary`), so it does not hit
  that multi-metric defect.
- `writeguard.HiddenAccess` currently treats a database error like “no matching
  rule” and therefore unrestricted. Normal manager scoping is enforced when the
  rule lookup succeeds, but the guard must fail closed before this is a
  production security boundary.
- `internal/workflow.Store.CompleteStep` — used by the gRPC
  `WorkflowService` and by other demos' seed scripts — does not trigger
  `on_approve`. Only `taskAction` (`POST /api/tasks/{id}/complete`, what
  the real Workflow Inbox UI calls) does. Nothing in this codebase drives
  workflow completion through gRPC today, so this hasn't mattered in
  practice, but a future gRPC-driven approval flow would need the same
  `on_approve` handling added there.
- The standalone gRPC `ImportService.ValidateImport` (a separate binary,
  `cmd/import`) still uses its own older CSV-only parsing — it doesn't yet
  call the shared `importpkg.ResolveRows` the HTTP path uses, so it has no
  native `.xlsx` support, name-based metric-column mapping, leaf-member
  check, or atomic whole-file rejection. Its `CommitImport` *does* go
  through the shared `internal/writeguard` check (see round 2, item 2
  above), so it can no longer bypass access rules/workflow locks/
  `system_managed` — only the richer validation is HTTP-only for now.
- The legacy direct-UUID metric import path does not validate model/revision
  ownership or `is_input`. The demo workbook uses scoped name-based resolution;
  integrations should do the same until the compatibility path is fixed.

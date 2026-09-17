# CapEx Demo — Engine Findings & Recommendations

> **Classification:** Historical — Findings from a 2026-07 stress run; the engine bugs it records were fixed.

Findings were **reproduced live** by `go run ./cmd/seed-capex` (verification
phase) on 2026-07-19, except where marked *code-read*. On 2026-07-20 the
engine was patched for 12 of the 13 reproduced findings and the demo was
**retested live** — the recap dropped from 13 findings to 1. Each entry below
is marked ✅ **FIXED (verified)** or still open, with the root-cause location
and the fix that was applied (or, for the one remaining item, recommended).

Severity: **P0** = security/correctness, fix first · **P1** = core
functionality insufficient · **P2** = quality/robustness · **P3** = cleanup.

---

## P0 — Platform console correctness

### 18. ✅ FIXED (verified) — Workspace-scoped applications were invisible in the Developer Console, for every role
Found 2026-07-20 by manually testing the demo end-to-end in the web UI (not by
the automated verification phase). `core.application` carries two independent
ownership columns — `workspace_id` (an app scoped to one workspace) and
`customer_id` (an app scoped to the whole tenant, no workspace) — and roughly
a dozen other queries across the codebase correctly resolve tenant ownership
through `(a.workspace_id = w.id OR a.customer_id = w.customer_id)`. The
`adminTenants` handler backing `/api/developer/applications` — the endpoint
the Developer Console's Models/Workflows/Dashboards/etc. tabs all read
from — did not: it matched `a.customer_id=$1::uuid` alone, in **both** its
`platform_admin`/`tenant_admin` branch and its `developer` branch. Any
application created with only `workspace_id` set (`customer_id` left NULL) —
which is how this seed, and the pre-existing `seed-procurement` demo, both
create their application — was completely invisible in the Developer Console
to **every** role, including `platform_admin`.
**Reproduced:** switching to any of the 7 CapEx demo personas (developer,
and by further testing, also platform_admin and tenant_admin) showed only
the pre-existing "OPEX Planning 2026" app; "CapEx Portfolio FY2026" did not
appear anywhere, with no error — it simply wasn't in the list. The
Procurement demo has had this exact bug since it was written; nothing
surfaced it because nobody had tested a second, workspace-scoped app in the
same tenant as an existing customer-wide one.
**Fix applied:** both branches in `adminTenants` (`internal/gateway/handler.go`)
now resolve an application's tenant via `LEFT JOIN core.workspace aw ON
aw.id = a.workspace_id` and match `a.customer_id=$1 OR aw.customer_id=$1`,
consistent with the idiom used everywhere else in the codebase.
**Retest:** confirmed live via direct API calls (not just the seed's DB-level
verification) — `demo-capex-dev`, the pre-existing `demo-dev-001`, and
`platform_admin` all now list both `CapEx Portfolio FY2026` and
`OPEX Planning 2026`. A SQL-mirroring regression probe (VERIFY 10) was also
added to the seed itself so this stays covered without a manual UI pass.
**Also fixed in the same pass (found alongside this):** the seed's own
`dev.capex@acme.com` persona was named "Sam (Developer)" — identical to the
pre-existing OPEX demo's `dev@acme.com` persona — so the dev-persona
switcher showed two indistinguishable "Sam (Developer)" entries. Renamed to
"Priya (CapEx Developer)"; the seed's user-upsert also now updates
`display_name`/`email` on conflict (previously a no-op `keycloak_sub =
EXCLUDED.keycloak_sub`) so a persona rename actually takes effect on rerun
instead of leaving the first-ever-inserted name in place forever.

### 19. ✅ FIXED (verified) — Fixing #18 exposed a second bug: developers could see an app in the list but not open it
Found immediately after retesting #18 — a screenshot of the actual Developer
Console showed the *original* OPEX developer (`dev@acme.com`, role_assignment
tied to the "Finance" workspace) now correctly saw "CapEx Portfolio FY2026"
in the app list (thanks to #18's fix), but selecting it showed **zero**
metrics, dimensions, forms, etc. — the console silently rendered empty
states everywhere instead of an error. Separately, the Dashboards tab kept
showing the *previous* app's dashboard ("OPEX Planning") even after CapEx was
selected.
**Root cause (two independent bugs, one masking the other until #18 was fixed):**
- **Access check, not just listing:** `resolveDemoModelID` — the single
  function gatekeeping all 21 Developer Console data endpoints
  (`internal/gateway/handler.go`) — required a developer's
  `role_assignment.workspace_id` to equal the app's *own* workspace exactly.
  `adminTenants` (the app-listing endpoint, already customer-wide for
  developers even before #18's fix — see its pre-existing `rw.customer_id =
  a.customer_id` EXISTS clause) was always more permissive than the endpoint
  that actually resolves and returns the app's data. A developer's role in
  *any* workspace of the tenant was enough to see an app in the list, but not
  enough to open a workspace-scoped one living in a different workspace than
  their own role_assignment.
- **Stale query, unrelated to access:** `DashboardsTab` in
  `web/src/consoles/developer/DeveloperConsole.tsx` accepted a `revisionId`
  prop but never used it — `useQuery({ queryKey: ["dev-dashboards"], queryFn:
  () => api.listDashboards() })` — even though the underlying API client
  function (`api.listDashboards(revisionId)`) and backend endpoint both
  already supported scoping by revision. Every sibling tab (Forms, Grids,
  Metrics) correctly threads `revisionId` through its query key; Dashboards
  was the one left behind, so switching the selected app/revision without a
  full page reload kept showing whichever dashboard had been fetched first.
**Fix applied:**
- `resolveDemoModelID`'s two developer/business-user branches now resolve an
  app's owning customer as `COALESCE(app.customer_id, appWorkspace.customer_id)`
  and match a role assignment when *either* the role is `developer` (matching
  `adminTenants`' existing tenant-wide trust for that role) *or* the app has
  no specific workspace (customer-wide app) *or* the assignment is in the
  app's exact workspace (unchanged, still-strict business_user/business_admin
  behavior — workspace boundaries between business units of one corporate
  tenant remain enforced for those operational roles).
- `DashboardsTab`'s query now includes `revisionId` in its key and passes it
  to `listDashboards`, matching every sibling tab.
**Retest:** direct API calls confirm `demo-dev-001` (role only in the
"Finance"/OPEX workspace) now correctly loads CapEx's 3 dashboards, 9
metrics, and 4 dimensions when given CapEx's app ID — the exact request that
previously returned `"forbidden: resolve model"`.
**Not covered by an automated regression probe:** finding #19's access-check
half lives in HTTP-layer role resolution the seed can mirror in SQL (same
limitation as #18); the stale-query half is a pure frontend bug no Go-side
probe can catch at all. Both require the manual UI walkthrough in the
[README](README.md#how-to-test-it-yourself) to guard against regressing.

---

## P0 — Security

### 1. ✅ FIXED (verified) — Cross-tenant task visibility in `/api/tasks`
Platform-role matching ignored workspace: `identity.role_assignment` rows are
workspace-scoped, but the tasks query matched `ra.role::text IN (assignee_roles)`
with no workspace/customer filter (`internal/gateway/handler.go`).
**Was reproduced:** `eve.other@othercorp.com` (business_user of a *different
customer*) could see this tenant's open approval tasks, including amounts.
**Fix applied:** the query now joins `role_assignment.workspace_id` (via
`core.workspace`) to the task's application workspace/customer, same pattern
the business-role branch already used; a NULL workspace (platform-wide role)
still matches everywhere.
**Retest:** eve sees 0 tasks; same-workspace user david still sees the task.

### 2. ✅ FIXED (verified) — Cross-tenant workflow notifications
`resolveNotificationRecipients` matched platform roles globally: a
notification step targeting role `business_admin` notified every
business_admin **of every customer**.
**Was reproduced:** eve received "Compliance review complete" from
GlobalCorp's workflow.
**Fix applied:** the `role_assignment` EXISTS clause is now scoped to the
workflow's application workspace/customer (mirrors the business-role branch).
**Retest:** same-tenant admins (fatima, carlos) still receive the
notification; eve does not.

---

## P0 — Workflow correctness

### 3. ✅ FIXED (verified) — Join after a condition could never fire
`allPredecessorsComplete` required *every* step routing into a join to be
completed/rejected/skipped — but a branch not taken by a condition stayed
`pending` forever, blocking the join; instance-close logic then marked the
join **and everything after it** `skipped`, closing the instance as
"completed" without ever notifying anyone.
**Was reproduced:** `Threshold Diamond` — condition→false branch completed,
final notification silently skipped.
**Fix applied:** added `reconcileInstance`/`reachableStepDefIDs` — after any
step completes, a reachability sweep marks steps on now-unreachable branches
`skipped` immediately, and any join whose remaining predecessors are all
resolved (with at least one actually `completed`, so a join on an entirely
dead branch still doesn't fire) is activated and continues along its routes.
Runs to a fixpoint so a chain of conditions/joins resolves in one call.
**Retest:** the diamond's join fires and the final notification is delivered.

### 4. ✅ FIXED (verified) — Condition steps were not evaluated by the engine
The designer stored `{left, operator, right}` but no runtime evaluator
existed — routing relied on a human completing the step with decision
`true`/`false`; condition steps surfaced as tasks in `/api/tasks`.
**Was reproduced:** `Amount Threshold` stayed `in_progress` until manually
answered.
**Fix applied:** `processAutoStep` + `evalStepCondition` now evaluate a
condition step's expression against instance context on activation (all 9
designer operators: equals, not_equals, greater/less-than[-or-equal],
contains, is_empty, is_not_empty; numeric coercion when both sides parse as
numbers). A step whose context key is missing is left `in_progress` for a
human decision — the pre-fix behavior is now the fallback, not the default.
Applies to the workflow's first step too (a workflow can now open on a
condition or notification with no human step before it).
**Retest:** `Amount Threshold` auto-evaluates true/false correctly on both
branches of VERIFY 1/3/4.

### 5. ✅ FIXED (verified) — Event trigger payloads carried no form data
`form_submit`/`form_approval` dispatched only `{form_id, record_id, status}`.
A condition on `amount` had nothing to read even with an evaluator (finding
4), and `context_schema` variables were never populated.
**Was reproduced:** instance context contained only the three IDs.
**Fix applied:** `enrichFormPayload` merges the triggering record's scalar
field values into the payload (existing keys win) before dispatch.
**Retest:** instance context now contains `amount` (and other record fields).

---

## P1 — Insufficient functionality

### 6. ✅ FIXED (verified) — Fact granularity contract was contradictory (calc vs charts)
`GetInputValue` with empty dims summed the latest value of *every* distinct
dim-combo, while the chart resolver reads facts by exact single-dim keys and
does not roll up — so charts need per-department, per-quarter, per-category
slices of the same metric, and storing them corrupted every calc total.
**Was reproduced:** `remaining_budget` total = 2,910,000 vs true 485,000 (6×
double-count).
**Fix applied (calc engine, `internal/calculation/scheduler.go` +
`store.go`):**
- `GetInputValue`'s `{}` lookup now prefers an explicit total row when one
  exists, falling back to summing distinct combos only when it doesn't.
- `executePartition` now evaluates a formula's dependencies over exactly one
  fact-granularity **family** (grouped by dimension key-set; `finestFamily`
  picks the finest one), instead of every combo across every family — so
  facts stored at multiple granularities for chart purposes no longer get
  counted once per granularity.
- `agg_rule = 'average'` metrics (ratios like `utilization_pct`) are now
  evaluated once on total-level inputs (total ÷ total) unless the formula is
  dimension-conditional, in which case per-combo results are averaged rather
  than summed.
**Retest:** `remaining_budget` = 485,000 (exact); `utilization_pct` = 60.1%
(matches `spent/approved × 100` on totals) — both confirmed against
hand-computed expected values.
**Still recommended:** document the fact-granularity contract explicitly (a
metric's per-slice families are implicit today, inferred at calc time) and
add a design-time check that flags metrics no chart ever reads, so writing
extra granularities isn't the *only* way to plot them (see #8 below).

### 7. ⏳ OPEN — A metric can belong to only one grid; charts are chained to that grid
`grid_metric_metric_id_key` (migration 051) plus chart validation ("metric
not in grid", "plotted dimension not found in grid") means every chartable
slice of a metric forces more dimensions onto the single grid that owns it;
purpose-built grids per audience are impossible except via read-only rollup
grids.
**Reproduced (still fails):** inserting `spent_amount` into a second grid
violates the constraint.
**Why not fixed here:** this is a structural schema/API decision (decouple
chart data-resolution from grid membership, or allow N:M metric↔grid with a
single writable "home" grid) that changes the chart-authoring contract on
both frontend and backend — out of scope for a targeted bug-fix pass, but
the single highest-leverage remaining change for dashboard flexibility.
**Recommended fix:** let a chart widget reference `{metric_id, dimension_id,
revision_id}` directly (with the same ABAC checks `ChartResolver` already
does) instead of resolving through a grid; grids keep being the read/write
data-entry surface, charts become a separate read-only view.

### 8. ⏳ OPEN — Charts have no hierarchy awareness *(code-read + visible in demo)*
All members of the plotted dimension render flat — parent divisions and leaf
departments plot side by side, and parents show empty unless parent-level
facts are written (the demo writes division aggregates as a workaround).
**Recommended fix:** add `display_level` to chart config (grids already have
it, migration 039) and roll up parent values in the resolver using the
metric's `agg_rule` — this would also reduce the pressure behind #6/#7, since
one leaf-level fact family could serve every rollup level.

### 9. ✅ FIXED (verified) — Automation failures were silent
- `TriggerRule` inserted the execution row only on success; a failed fire
  left no trace, though the schema has `status='failed'` + `error` columns.
- `DispatchEventRules` swallowed errors.
- Rules could target unpublished workflows and fail only at fire time.
**Was reproduced:** a rule pointing at a draft workflow failed with
"workflow is not published" and left zero execution rows.
**Fix applied:** `TriggerRule` now always writes an execution row — `failed`
with the error text on any failure path (disabled rule, def not found,
context resolution error, start error), `running`/`completed`/`cancelled`
mirroring the real instance lifecycle on success. `CreateAutomationRule` now
rejects rules bound to a `workflow_def_id` that isn't `published`.
**Retest:** creating a rule against the draft workflow is rejected at save
time; firing a rule whose name-based workflow lookup fails now leaves a
`status='failed'` execution row with the error captured.

### 10. ✅ FIXED (verified) — `grid_change` source scoping could never match
The cell-writeback handler dispatched with an empty source ID, and the
dispatch SQL skipped the source filter entirely when the source was empty —
so rules scoped via `source_grid_id` fired for *every* grid's changes.
**Was reproduced:** a rule scoped to the quarterly grid fired for an
unrelated grid's cell change.
**Fix applied:** the cell-writeback handler now resolves the changed metric's
owning grid (`model.grid_metric`, unique per metric) and passes it as the
dispatch source; `DispatchEventRules`' SQL now treats an empty source as
matching only *unscoped* rules (`source_grid_id IS NULL`) instead of
matching everything.
**Retest:** a rule scoped to the metric's actual grid fires; a rule scoped to
a different grid does not; a sourceless dispatch matches neither.

---

## P2 — Quality / robustness

### 11. ✅ FIXED (verified) — SLA `due_at` was anchored at instance start; no escalation
All steps got `due_at = instance start + own SLA` at creation, so a 72h CFO
step was "due" 72h after start even though 72h of upstream SLAs preceded it.
**Fix applied:** `due_at` is now set when a step actually **activates**
(`StartWorkflow` for the first step, `activateNextSteps` for every
subsequent one) rather than at instance creation — pending steps have no
`due_at` until they're live.
**Retest:** a pending CFO step has `due_at = NULL`; once finance approves and
CFO activates, `due_at` is set to `now() + 72h`.
**Still open:** nothing monitors `due_at` for overdue steps — a background
scanner/escalation job is still recommended (not implemented; genuinely new
functionality, not a bug fix).

### 12. ✅ FIXED (verified) — `form_submit` fired on record creation, even for drafts
POST `/records` dispatched `form_submit` immediately regardless of status.
**Was reproduced:** a draft record started an approval workflow.
**Fix applied:** `form_submit` dispatch moved from record *creation* to the
record *reaching* `submitted` status via `PUT /records/{id}` (dispatched only
when the status actually changed); `DispatchEventRules` additionally guards
on `payload["status"] != "submitted"` as a second line of defense for any
other caller. `form_approval` dispatch also now requires an actual status
transition (previously it re-fired on every PUT while already approved).
**Retest:** creating a draft record no longer starts an instance; submitting
it does.

### 13. ✅ FIXED (verified) — Execution status lied about outcomes
Executions were written `status='completed'` the moment the instance
*started*.
**Was reproduced:** execution "completed" while its instance was still
running.
**Fix applied:** `TriggerRule` now writes `running` when the started instance
is still running, or the instance's real terminal status if auto-processing
(condition/notification steps) already closed it synchronously.
`closeInstanceIfIdle` updates the matching execution row to
`completed`/`cancelled` whenever an instance it's linked to finishes later.
**Retest:** a diamond-workflow execution reports `running` immediately after
trigger, then `completed` once the (human) fast-track step is done.

### 14. ✅ FIXED (verified) — Dashboard gating fallback inverted privilege
A user with **no** business-role memberships saw **all** dashboards; joining
one role *reduced* visibility to that role's list.
**Was reproduced:** david (no role) saw all 3 dashboards; maria (role member)
saw 1.
**Fix applied:** the fallback ("show everything") now triggers only when the
dashboard's **workspace has no business roles defined at all** — a
demo/dev-workspace affordance — not per-user. Once a workspace defines any
business role, unassigned users see nothing, matching least-privilege intent.
Applied to both `/api/dashboards` (list) and `/api/dashboards/{id}` (detail).
**Retest:** maria (member of CapEx Requesters) sees 1 dashboard; david (no
membership, workspace has roles) sees 0 — least-privilege confirmed. (The
seed now also enrolls david into a role afterward so the demo itself isn't
left with an empty console.)

### 15. ⏳ OPEN — Rejection terminates as `cancelled` even when handled
A reject route that flows into a notify step and ends normally still closes
the instance as `cancelled` (any `rejected` step forces final status).
Reporting can't distinguish "rejected and handled" from "aborted".
**Not fixed:** would need a new terminal status (`rejected`) or a status
model change; left as a documented gap rather than bundled into this pass.
**Recommended fix:** add a `rejected` terminal status distinct from
`cancelled`, or treat reaching `end-completed` as authoritative regardless of
intermediate step decisions.

---

## P3 — Cleanup (not addressed this pass)

### 16. `workflow_action` widget type is documented but unimplemented
Migration 043 comments it into the valid widget-type list; no renderer or
backend handling exists anywhere. Remove the mention or implement it.

### 17. Editing a published workflow def with running instances orphans steps *(code-read)*
`/api/tasks` resolves step metadata via a LATERAL join into the def's current
`steps` JSON; steps whose `step_def_id` no longer exists silently disappear
from task lists while the instance stays `running`. Consider def versioning
(instances pin the steps JSON they started with).

---

## Retest summary (2026-07-20)

`go run ./cmd/seed-capex` verification recap: **13 findings → 1 finding.**
The one remaining live finding (#7, one-grid-per-metric) is a structural
schema/API constraint, not a bug — see its entry above for the recommended
follow-up. Full package test suites (`internal/workflow`, `internal/gateway`,
`internal/calculation`, `internal/query`, `internal/crudapp`, `internal/model`,
`internal/notification`, `internal/identity`) pass, including two pre-existing
tests updated to assert the corrected (not the buggy) behavior:
`TestTriggerRule` now expects `running` instead of `completed` immediately
after trigger, and `TestAutomationRuleWorkflowLinkage` accepts either
`running` or `completed` for the linked execution row.

**Same-day follow-up:** manually testing the demo in the actual web UI (not
just the seed's own DB-level verification) immediately surfaced finding
#18 — a P0 the automated verification phase structurally couldn't catch,
since it lives entirely in an HTTP endpoint's tenant-resolution logic
(`/api/developer/applications`), not in workflow/calc/security behavior the
seed drives directly. Fixing #18 then *exposed* finding #19 (also fixed) —
a stricter, inconsistent access check one layer deeper that #18's fix made
reachable for the first time, plus an unrelated frontend staleness bug in
the same tab. Both were confirmed live against the running gateway. This is
a good illustration of why "run it as a user" remains necessary even with a
thorough automated verification phase, and why fixing one visibility bug can
uncover the next one behind it — see the demo's
[README](README.md#how-to-test-it-yourself) for the manual test flow.

**Running total: 14 of 19 findings fixed and verified.** 5 remain open: #7
(structural schema decision), #8 (new feature — chart hierarchy), #15 (new
terminal-status feature), #16, #17 (both P3 cleanup, code-read only).

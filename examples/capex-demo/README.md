# CapEx Portfolio FY2026 — Engine Stress Demo

> **Classification:** Current — Walkthrough of the stress demo built by cmd/seed-capex.

A capital-expenditure planning application built **entirely from generic engine
primitives** (no bespoke endpoints, no custom widget types). Its purpose is to
exercise every subsystem at once — workflows, roles, triggers, dashboards,
calculation, security — and to surface inconsistencies. The seed ends with a
verification phase that drives real workflow instances and prints
`PASS` / `FINDING` for each probe.

**Status:** the automated verification phase (2026-07-19) surfaced 13
findings; the engine was patched and retested live (2026-07-20) — 12 of 13
fixed and verified, 1 open as a structural schema decision. A same-day manual
pass through the actual web UI (see below) then caught two more, back to
back: #18, a P0 that hid every workspace-scoped application (including this
demo, and the pre-existing Procurement demo) from the Developer Console for
every role — and #19, an access-check inconsistency #18's fix exposed for
the first time (an app could now be *listed* for a developer whose role
lives in a different workspace, but not actually *opened*), plus an
unrelated frontend staleness bug in the same tab. All fixed and verified the
same session. 14 of 19 total findings are now fixed; full history in
[FINDINGS.md](FINDINGS.md).

## Run

```bash
DATABASE_URL=postgres://mavericks:mavericks@localhost:5432/mavericks?sslmode=disable \
  go run ./cmd/seed-capex
```

Each run creates a fresh `Capital Planning` workspace under the first existing
customer (plus a small `OtherCorp` probe tenant used by the isolation tests).

## What gets built

| Layer | Contents |
|---|---|
| Model | `CapEx Model`, revision `FY2026 Plan` (active) |
| Dimensions | `department` (9 members, 2-level hierarchy TECH/COMM/CORP → 6 leaves), `category` (4), `quarter` (4), `project` (10, source of truth for all facts) |
| Metrics | 6 input (`requested/approved/spent/committed_amount`, `risk_score`, `roi_pct`) + 3 calculated (`remaining_budget`, `utilization_pct`, `approval_rate_pct`), currency/percentage formats, sum/average agg rules |
| Grids | `CapEx Overview` (money metrics × dept/quarter/category — a metric may only belong to one grid, see FINDINGS #7), `Project Portfolio` (risk/roi/calc × project), `Spend by Quarter (rollup)` (rollup of Overview) |
| Dashboards | **Executive Overview** (text, 3 KPI tiles, pie, bar, line), **Portfolio Analysis** (scatter, histogram, grid), **CapEx Requests** (text, form, automation button, grid) |
| Form | `capex_request` (8 fields) with 2 form→metric mappings (`amount` → requested on submit, → approved on approval) |
| Workflows | `CapEx Request Approval` (approval → condition >50k → finance → CFO, SLAs, reject routes), `Compliance Dual Review` (parallel fan-out + join), `Threshold Diamond` (condition → branches → join), `Budget Replan Review` (grid-change), `Draft Trap` (deliberately unpublished) |
| Automation | 6 rules covering **all 5 trigger types**: `form_submit`, `form_approval`, `grid_change`, `manual` (dashboard button), `api` |
| Roles | Platform roles per user + 4 business roles (`CapEx Requesters`, `Finance Review`, `Legal Review`, `Executives`) gating dashboards; RACI rules; metric policies; per-user access rules (hidden/read-only metrics) |

## Demo users

| User | Platform role | Business role | Purpose |
|---|---|---|---|
| maria.requester@acme.com | business_user | CapEx Requesters | submits requests |
| david.depthead@acme.com | business_user | — (intentionally none) | dept approvals; dashboard-fallback probe |
| fatima.finance@acme.com | business_admin | Finance Review | finance approvals, compliance |
| carlos.cfo@acme.com | business_admin | Executives | CFO sign-off |
| lena.legal@acme.com | business_user | Legal Review | legal compliance branch |
| dev.capex@acme.com — "Priya (CapEx Developer)" | developer | — | authoring |
| eve.other@othercorp.com | business_admin + business_user **in OtherCorp** | — | cross-tenant isolation probe |

## How to test it yourself

The demo's users are ordinary rows in `identity.user` — no separate accounts
or passwords. In dev mode the gateway accepts an `X-Dev-User` header naming
any user's `keycloak_sub` (or a few short legacy aliases) and impersonates
them; the web app's sidebar has a **"Dev — Persona"** dropdown that lists
every user in the database and does this for you.

1. Start the stack (skip anything already running):
   ```bash
   # infra (Postgres etc.) — see deploy/docker/docker-compose.dev.yml
   DEV_MODE=true DATABASE_URL=postgres://mavericks:mavericks@localhost:5432/mavericks?sslmode=disable \
     go run ./cmd/gateway
   cd web && npm run dev
   ```
2. Seed the demo (see [Run](#run) above), then open [http://localhost:5173](http://localhost:5173).
3. Pick a persona from the sidebar dropdown (e.g. "Maria (Requester)"), then
   pick **CapEx Portfolio FY2026** in the app/workspace selector.
4. Follow the [manual UI test script](#manual-ui-test-script) below,
   switching personas as it directs.

**Note:** this application is deliberately created with only `workspace_id`
set (no `customer_id`) — the same pattern the pre-existing Procurement demo
uses. That combination used to make the app invisible in the Developer
Console for every role (FINDINGS #18) and, for a developer whose role lives
in a different workspace than the app's own, unopenable even once visible
(FINDINGS #19) — both fixed 2026-07-20. If an app ever vanishes from that
console again, or loads with everything empty despite appearing in the app
list, check `resolveDemoModelID` and `adminTenants` in
`internal/gateway/handler.go` first.

## Manual UI test script

1. **Dashboards** (business console, per user): maria sees only *CapEx
   Requests*; fatima all three; carlos only *Executive Overview*; david (now
   also a CapEx Requesters member after the seed's verification probe) sees
   *CapEx Requests* — an unassigned user in this workspace would see nothing
   (least-privilege, fixed per FINDINGS #14).
2. **Charts**: pie (category), bar (department — note divisions and leaves
   still plot side by side; hierarchy-aware charts remain open, FINDINGS #8),
   line (quarter), scatter (risk × ROI), histogram (ROI bins). KPI tiles read
   totals — `remaining_budget` and `utilization_pct` are now correct despite
   facts existing at 6 granularities (FINDINGS #6).
3. **Form → workflow**: submit a request >$50k as maria; approve as david;
   the *Amount Threshold* condition step now auto-evaluates (no human task,
   FINDINGS #4) and routes straight to finance; approve as fatima, then
   carlos; check maria's notifications.
4. **Automation button**: press *Start Portfolio Review* on the Requests
   dashboard; drive the diamond workflow — the join now fires correctly after
   the taken branch and the final notification is delivered (FINDINGS #3).
5. **Tasks as eve** (OtherCorp): open tasks — she can no longer see this
   tenant's approval tasks (FINDINGS #1, cross-tenant fix).

## Files

- Seed + verification: [`cmd/seed-capex/main.go`](../../cmd/seed-capex/main.go)
- Findings and engine recommendations: [FINDINGS.md](FINDINGS.md)

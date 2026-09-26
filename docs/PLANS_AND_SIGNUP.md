# Plans and self-service sign-up

> **Classification:** Current — How a tenant's plan is defined and enforced, and how a visitor becomes a tenant.

> **Last verified:** 2026-09-20 (tests; migration 090 removed trials)

A tenant is on one **plan**. The plan says how much the tenant may use
(limits) and whether a visitor may pick it at public **sign-up**. A plan
bounds how much, never for how long: **there is no trial** (migration 090
removed the concept). Plans are rows the platform administrators — the
operator's own people, with reach over every tenant — edit in the console,
not constants in code: tuning the basic workspace is an edit, not a release.

This is a community feature: a deployment with no license key has plans too,
because a sign-up funnel is not an enterprise capability. The license key
(`docs/LICENSING.md`) decides the *edition* a deployment runs — community,
commercial (white-labelling and the right to commercial use) or enterprise,
keys sent by e-mail on request; a plan decides how much *one tenant* may use.

On the hosted service the two never meet: sign-up hands out the test
workspace, whose only limit is its size, and a sign-up account is never a
platform administrator — that role is the operator's, and whoever they
delegate it to. Everything beyond the basic workspace is a licence key and
running the platform yourself.

## The catalog

`platform.plan` (migration 085) holds the plans; the console shows it under
**Platform › Plans**. Each plan has a key, a name, a `self_service` flag, a
`sort_order`, and limits:

| Limit | Counts |
|---|---|
| `max_users` | Enabled users with the tenant as their own, or a role in one of its workspaces |
| `max_applications` | Applications of the tenant |
| `max_models` | Models across the tenant's applications |
| `max_metrics_per_model` | Metrics in a model's largest revision |
| `max_members_per_dimension` | Members of one dimension |
| `max_fact_rows_per_model` | `runtime.fact_input` rows of a model, all revisions |
| `max_ai_messages_per_day` | User messages to the AI assistant today (UTC) |
| `max_integration_runs_per_day` | Integration runs today (UTC) |
| `max_storage_mb` | Bytes the tenant's data occupies, in MB (see below) |

0 means unlimited. Migration 085 seeds `starter`, `standard` and
`enterprise` without limits, so that every tenant that already existed keeps
working exactly as before. Migration 089 adds `test` ("Test workspace", renamed "Basic workspace" by 096) —
no end date, `max_storage_mb: 100` plus the two daily caps — and makes it
the self-service plan. Migration 090 drops the fourteen-day `trial` plan 085
had seeded, together with `trial_days` and `core.customer.trial_ends_at`;
a tenant that had been put on it by hand moves to the basic workspace.
Sign-up offers the first `self_service` plan by `sort_order`.

**Where to go from here** is data too: `limit_note`. Every refusal ends with
it — the 402 body, the read-only reason in the banner, the terms of service
— in place of the engine's default "Change the plan to add more." A
deployment whose answer to a full workspace is another plan leaves it empty;
the basic workspace's note says to run the platform on your own
infrastructure (free and unlimited for non-commercial use, under a
commercial licence otherwise) or to get the enterprise edition. The console
shows the plan's `contact_url` link as "Learn more" when a note is set and
"Change plan" when it is not.

**How storage is measured** (`plan.StorageBytes`). In a dedicated tenant
database (`TENANT_DB_MODE=dedicated`, docs/TENANT_DATABASES.md) it is the
size of every table in that database, indexes and TOAST included, catalogs
excluded — exact, and a catalog read. In a shared database the tables hold
every tenant at once, so the tenant's share is an **estimate**: its row
count in each data-bearing table (`runtime.fact_input`, `runtime.calc_result`,
`runtime.fact_input_history`) times that table's average bytes per row from
the planner statistics (200 bytes until the table has been analysed).
Definitions, users and audit rows are not counted in the estimate. A
deployment that means the number to be exact runs its self-service tenants
dedicated.

**Making room.** `runtime.fact_input` is append-only and deleted facts are
archived to `runtime.fact_input_history` (migration 081), so deleting rows
or clearing an import does not shrink a tenant. Deleting a **model** (or the
application holding it) does: since migration 089 the archive skips facts
whose model is being deleted and drops the model's earlier history — history
no screen could show once the model is gone. That is what "read-only,
except for deleting" means for a storage-bounded plan, and it was proven
live: a basic workspace at 128 MB, application deleted by its own tenant
administrator while read-only, back to 33 MB and writable at the next sweep.
The residual is PostgreSQL free space the tenant's next writes reuse. A tenant that names a plan with no row is treated
as unlimited and shown as "(no such plan)": a catalog gap must never lock a
paying customer out.

## What a plan does

Everything below is in `internal/plan`; the gateway applies it in
`internal/gateway/plan.go`.

**Limits, at the moment of creation.** The handlers that create users
(console, SCIM, single sign-on's first login), applications, models
(including package import), metrics, dimension members and data rows, and the
AI message and integration run endpoints, ask the plan first. The storage
limit is asked where data rows arrive in bulk (cell writes, CSV commit,
package import): a tenant already at its `max_storage_mb` is refused there
with `402 {"code": "plan_limit", "limit": "max_storage_mb", …}`; a write
that crosses the line is caught by the sweep. A creation that
would cross the limit is refused with `402 {"code": "plan_limit", "limit":
"max_models", "max": 3, "current": 3, "error": "The Small plan allows 3
models; this tenant has 3. Change the plan to add more."}`.

**Limits, by the sweep.** Not every path that can add rows goes through
those handlers (the AI assistant's write executor, scheduled integrations, a
package restore). Every five minutes the gateway recounts each tenant against
its plan (`plan.RunSweep`, one pass per database) and records the verdict on
the tenant's row (`limit_state`, `limit_reason`, `usage_checked_at`). A tenant
found **over** a limit is read-only — except for **deleting**, so it can get
back under — with `402 {"code": "over_limit"}` and the reason naming the
limit and the object ("… 100000 data rows per model; this model has 120000
(model "Budget")"). The next sweep that finds it within limits clears the
state. The gateway and the sweep share one enforcer, so a verdict applies to
requests at once; a plan edit reaches every tenant on the plan within a
minute (the per-tenant state is cached for 60 seconds).

**Who is exempt.** Platform administrators are never refused: they are who
changes the plan. A request that belongs to no tenant (a platform-level
developer, an admin acting across tenants) has no plan applied. The tenant of
a request is the routed tenant in dedicated-database mode, else the
application named by `X-App-Id`, else the caller's own tenant.

Every refusal carries `contact_url` (`PLAN_CONTACT_URL`), which the console
shows as "Change plan": a pricing page, or a `mailto:`.

## Self-service sign-up

`POST /api/signup` turns a visitor into a tenant. It is off unless the
deployment opts in:

| Variable | Meaning |
|---|---|
| `SIGNUP_ENABLED` | `true` opens `/signup` and `POST /api/signup` |
| `PLAN_CONTACT_URL` | Where "change the plan" leads in plan-limit refusals. Leave it unset until there is a page or a mailbox that answers: the console then omits the link rather than showing one that goes nowhere |

A public sign-up funnel also needs the two documents it asks a visitor to
agree to. They are configuration of the same kind — see
[`LEGAL_AND_PRIVACY.md`](LEGAL_AND_PRIVACY.md).

and it needs a plan flagged `self_service` (the first one in display order is
assigned) and, outside the dev stack, the Keycloak provisioning credentials
(`KEYCLOAK_ADMIN_CLIENT_ID/SECRET`) plus SMTP on the realm, because the
account is only usable once the invitation mail is completed.

The page at `/signup` (rendered before any sign-in; `web/src/signup/`) asks
`GET /api/signup/options` what is on offer and shows exactly that — the
plan's name and limits — so a closed deployment or a changed plan never has
a page promising something else. One request creates:

1. the identity-provider account, with the `tenant_admin`, `developer` and
   `business_admin` realm roles — nothing else exists yet, so a refusal here
   costs nothing;
2. the tenant (its own database in dedicated mode), on the self-service plan;
3. in one transaction on the tenant's database: the person (with the same
   three roles, `business_admin` scoped to the default workspace), an
   application named "Getting started", and the **starter model** —
   `internal/starter`, a guided tour of the platform: four dashboards of
   prose and diagrams, taught on a deliberately tiny example (two teams,
   four quarters, three metrics of which one calculated) so that the grid,
   the chart and the KPI cards on the tour's own pages are the working
   screens rather than pictures of them. Created through
   `modeltransfer.Import`, the same path a package import takes — then a
   recalculation, so the first screen shows real numbers. Nothing about it
   is a special tutorial mode: it is ordinary text, image, grid, chart and
   KPI widgets, which the tenant can edit or delete like any other;
4. the invitation to set a password (three days).

A failure after step 1 undoes everything made so far; an invitation that
cannot be sent undoes all of it ("nothing was created"). The request is
rate-limited per address (three attempts, then one every five minutes), an
address that already has an account — in the control plane, in any dedicated
tenant's directory, or at the identity provider — is refused with 409, and
every sign-up is an audit event (`tenant.signed_up`) with the address,
company, plan and source address.

On the dev stack (no identity provider) the response carries `dev_persona`,
and the page offers to open the console as the new account.

## Administration

- **Platform › Plans** (`GET/PUT /api/admin/plans[/{key}]`): edit names,
  descriptions, the self-service flag and every limit; add a
  plan. Plans cannot be deleted while tenants may name them; zero the limits
  or rename instead. Tenant administrators can read the catalog (to see what
  an upgrade is) but not change it.
- **Applications › tenant card**: the meta line shows the plan and
  "read-only" when it applies; **Change plan** (platform administrators)
  moves the tenant. `PATCH /api/admin/tenants/{id}` takes `plan`; the next
  usage sweep judges the tenant by the new limits. A tenant administrator
  has the same console for their own tenant, minus this: lifting one's own
  limits is the platform's side of the relationship.
- `GET /api/admin/tenants` carries `plan_state` per tenant; `GET /api/me`
  carries `plan` for a member of a tenant.

## Limits of this design

- Daily limits (AI messages, integration runs) refuse at request time only;
  they are not part of the sweep and reset by themselves at midnight UTC.
- The sweep counts; it does not archive. A tenant over a storage limit has to
  delete, or be moved to a larger plan.
- Sign-up creates one person per tenant. Inviting colleagues is the tenant
  administrator's first job, under Users, and counts against `max_users`.
- Keycloak's own registration is not used and stays off: the platform must
  create the tenant with the account.

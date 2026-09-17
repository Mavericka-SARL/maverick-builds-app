# Plans, trials and self-service sign-up

> **Classification:** Current — How a tenant's plan is defined and enforced, and how a visitor becomes a tenant.

> **Last verified:** 2026-09-17

A tenant is on one **plan**. The plan says how much the tenant may use
(limits), whether it started as a **trial** and for how long, and whether a
visitor may pick it at public **sign-up**. Plans are rows the platform
administrator edits in the console, not constants in code: pricing a tier or
changing the trial is an edit, not a release.

This is a community feature: a deployment with no license key has plans too,
because a trial funnel is not an enterprise capability. The license key
(`docs/LICENSING.md`) decides the *edition* a deployment runs; a plan decides
how much *one tenant* may use.

## The catalog

`platform.plan` (migration 085) holds the plans; the console shows it under
**Platform › Plans**. Each plan has a key, a name, `trial_days` (0 for a plan
that is not a trial), a `self_service` flag, a `sort_order`, and limits:

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

0 means unlimited. The migration seeds four plans: `trial` (14 days,
self-service, limited), and `starter`, `standard`, `enterprise` without
limits — the last three so that every tenant that already existed keeps
working exactly as before. A tenant that names a plan with no row is treated
as unlimited and shown as "(no such plan)": a catalog gap must never lock a
paying customer out.

## What a plan does

Everything below is in `internal/plan`; the gateway applies it in
`internal/gateway/plan.go`.

**Trial.** A tenant put on a trial plan gets `core.customer.trial_ends_at =
now() + trial_days`. While it runs, `/api/me` reports `plan.trial = true` and
`days_left`, and the console shows "Trial · N days left" above every screen.
When it passes, the tenant is **read-only**: every mutating request (POST,
PUT, PATCH, DELETE) is refused with `402 {"code": "trial_expired", "error":
"The trial ended on … The workspace is read-only until the plan is
changed."}`. Reads keep working, so the tenant can look at and export its
data. The platform administrator lifts it from the tenant's card
(**Applications › tenant › Change plan**): another plan, or a later trial end.

**Limits, at the moment of creation.** The handlers that create users
(console, SCIM, single sign-on's first login), applications, models
(including package import), metrics, dimension members and data rows, and the
AI message and integration run endpoints, ask the plan first. A creation that
would cross the limit is refused with `402 {"code": "plan_limit", "limit":
"max_models", "max": 3, "current": 3, "error": "The Trial plan allows 3
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
| `PLAN_CONTACT_URL` | Where "change the plan" leads in trial notices and refusals |

and it needs a plan flagged `self_service` (the first one in display order is
assigned) and, outside the dev stack, the Keycloak provisioning credentials
(`KEYCLOAK_ADMIN_CLIENT_ID/SECRET`) plus SMTP on the realm, because the
account is only usable once the invitation mail is completed.

The page at `/signup` (rendered before any sign-in; `web/src/signup/`) asks
`GET /api/signup/options` what is on offer and shows exactly that — the
plan's name, trial length and limits — so a closed deployment or a changed
trial never has a page promising something else. One request creates:

1. the identity-provider account, with the `tenant_admin`, `developer` and
   `business_admin` realm roles — nothing else exists yet, so a refusal here
   costs nothing;
2. the tenant (its own database in dedicated mode), on the self-service plan,
   with `trial_ends_at` set;
3. in one transaction on the tenant's database: the person (with the same
   three roles, `business_admin` scoped to the default workspace), an
   application named "Getting started", and the **starter model** —
   `internal/starter`, a budget-versus-actual model with two dimensions of
   three levels, four metrics, a grid, a dashboard and a year of figures,
   created through `modeltransfer.Import`, the same path a package import
   takes — then a recalculation, so the first screen shows numbers;
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
  descriptions, trial days, the self-service flag and every limit; add a
  plan. Plans cannot be deleted while tenants may name them; zero the limits
  or rename instead. Tenant administrators can read the catalog (to see what
  an upgrade is) but not change it.
- **Applications › tenant card**: the meta line shows the plan, the trial and
  its days, and "read-only" when it applies; **Change plan** (platform
  administrators) moves the tenant or sets the trial's end. `PATCH
  /api/admin/tenants/{id}` takes `plan` and/or `trial_ends_at` (RFC 3339;
  `""` ends the trial). Moving onto a trial plan starts its clock; moving off
  one ends the trial.
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

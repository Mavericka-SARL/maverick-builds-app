# Editions and self-service sign-up

> **Last verified:** 2026-10-11 (tests; migration 129: the Community note matches the licence)

A tenant runs one **edition** — **Community**, **Commercial** or
**Enterprise**. The edition says how much the tenant may use (limits) and
whether a visitor may pick it at public **sign-up**. Editions are rows the
platform administrators — the operator's own people, with reach over every
tenant — edit in the console, not constants in code: tuning the Community
Edition is an edit, not a release.

**Community** is where a self-service sign-up lands (in the Playground its
one limit is 100 MB of data), **Commercial** is where a tenant an
administrator creates starts, and **Enterprise** is the third.

The 100 MB is the Playground's (`PLAYGROUND=true`). Migrations seed it on
every deployment, since a migration cannot tell which one is the Playground,
so the gateway of any other deployment takes it off the Community Edition at
start-up (`plan.ReleasePlaygroundAllowance`) — only while the row still holds
exactly the seeded limit, description and note, so a limit an administrator
set stays.

A deployment with no licence key has editions too, because a sign-up funnel is
not an enterprise capability. The licence key (`docs/LICENSING.md`) decides the
edition the *deployment* runs — Community, Commercial (white-labelling and the
right to commercial use) or Enterprise, each key issued for an accepted order,
which it names; a tenant's edition decides how much *that tenant* may use.

The hosted **Playground** is the Community Edition Mavericka runs for the
public: sign-up hands out the Community Edition, whose only limit is its size,
and a sign-up account is never a platform administrator — that role is the
operator's, and whoever they delegate it to. Only in the Playground are
workspaces nobody uses retired ("Unused workspaces" below). Everything beyond
the Community Edition is a licence key and running the platform yourself.

## The catalog

`platform.plan` (migration 085) holds the editions; the console shows it under
**Platform › Editions**. Each edition has a key, a name, a `self_service` flag, a
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
working exactly as before. Migration 089 adds `test` — no end date,
`max_storage_mb: 100` plus the two daily caps — and makes it the
self-service edition. Migration 090 removes the time-limited edition 085 had
seeded, with its columns; a tenant that had been put on it by hand moves to
`test`.
Migration 123 leaves three editions: `test` becomes `community` ("Community",
`max_storage_mb: 100` only — the two daily caps go), `standard` becomes
`commercial` ("Commercial"), `enterprise` stays; tenants on `starter` move
to `commercial` and the edition goes, a tenant left on the removed
time-limited edition moves to `community`, and both places a tenant names its edition (`core.customer.plan`,
`platform.tenant_database.plan`) now default to `commercial`. Keys were
renamed in place, so each edition kept whatever the platform administrator had
set on it. Sign-up offers the first `self_service` edition by `sort_order`.

**Where to go from here** is data too: `limit_note`. Every refusal ends with
it — the 402 body, the read-only reason in the banner, the terms of service
— in place of the engine's default "Change the edition to add more." A
deployment whose answer to a full workspace is another edition leaves it empty;
the Community Edition's note says to run the platform on your own
infrastructure (free for your company's internal use, as the Sustainable
Use License says) or to get the Commercial
or Enterprise Edition (migration 129 corrected the seeded wording, which
understated the licence). The console
shows the edition's `contact_url` link as "Learn more" when a note is set and
"Change edition" when it is not.

**How storage is measured** (`plan.StorageBytes`). The figure is the data a
tenant holds, not the size of the files it sits in. PostgreSQL keeps a deleted
row's space in a table's files for later writes to reuse, so the files do not
shrink when data is deleted; measured by them, a workspace that deleted a model
stayed the size it was. Each model's data — its rows in the data-bearing
tables (`runtime.fact_input`, `runtime.calc_result`,
`runtime.fact_input_history`), counted exactly — is multiplied by that table's
live bytes per row: a row's width from the planner statistics (`pg_stats`) plus
its share of the table's indexes and TOAST (200 bytes until the table has been
analysed). In a dedicated tenant database (`TENANT_DB_MODE=dedicated`,
docs/TENANT_DATABASES.md) every other table is added the same way from its
live-row count (`pg_stat_user_tables`); in a shared database those tables hold
every tenant at once, so definitions, users and audit rows are left out of the
estimate. It is an estimate either way: on freshly written tables it comes
within about a tenth of their files' size.

**Making room.** Deleting a **model**, a **revision**, a **metric** or the
application holding them gives the space back at once, in every edition.
Everything the model held goes with it (ON DELETE CASCADE: facts, calculated
results, imports and their staged rows, records, grants), and its cell
history by trigger — the archive skips facts whose model or metric is being
deleted and drops their earlier history, which no screen could show once they
are gone (migration 089 for models; 111 lost the check, 133 restored it and
covers metrics, which a revision's deletion goes through). The delete then
re-measures the workspace before it answers (`remeasureStorage`), with its
transaction having flushed its row counts to the statistics
(`pg_stat_force_next_flush`), so the console shows the smaller figure straight
away and a workspace read-only for its storage is writable again. Other paths
that delete — an import in full-reload mode, the AI developer — show it at the
next sweep. Deleting single cells does not shrink a tenant: `runtime.fact_input`
is append-only and deleted facts are archived to `runtime.fact_input_history`
(migration 081).

Recalculating writes only the cells whose value changed
(`calculation.Store.ReplaceResultsTx`). It used to delete and re-insert every
cell of a metric, and add a `'{}'` total per pass without removing the last
one, so a busy workspace held mostly rows nobody would read again — one tenant
260 MB for 200,000 live rows; migration 134 removed the piled-up rows.

**The disk goes back too.** PostgreSQL keeps a deleted row's space in the
table's files for later writes; `plan.Compact` rewrites a table without it
(`VACUUM (FULL, SKIP_LOCKED)`) in a tenant's own database, in every edition:
straight after a console delete, in the background, for every table that
changed since it was last rewritten and holds a megabyte more on disk than in
data; and at each usage sweep for those holding a fifth of their files or more
that way — any path that deleted, the AI developer and imports included
(`runtime.table_compaction` remembers what was rewritten). The rewrite locks a
table while it runs: about a second at a Playground workspace's size. A
committed import's staged rows are deleted at commit (migration 135 removed the
ones kept before). A shared database's tables hold every tenant at once and are
not rewritten.

That is what "read-only, except for deleting" means for a storage-bounded
edition. Proven by
`TestDeletingAModelBringsAWorkspaceBackUnderItsStorageLimit` (a real gateway
and a dedicated tenant database: a model deleted over HTTP, 49 MB to 2 MB and
writable again in the listing that follows),
`TestDeletingAModelLowersStorageInAnUnlimitedEdition` (an Enterprise tenant,
37 MB to 2 MB) and `TestStorageLimitRefusesTheWriteThatWouldCrossIt` (the
database's files 72 MB to 16 MB within seconds of the delete).

**A write that would cross the limit is refused.** On an edition with a storage
limit — on the hosted service, the Community Edition of the Playground; the
Commercial and Enterprise Editions set none — the sweep puts a guard on the
tables a workspace fills: facts, staged imports, records and dimension
members (`runtime.set_storage_guard`, migration 135, with the limit and its
sentence on `core.customer.storage_limit_bytes` / `storage_limit_text`). Every
statement that adds rows there takes them, at the table's bytes per row
(`runtime.bytes_per_row`), from the room the workspace has left, measured once
per transaction (`runtime.storage_used`: the sweep's last measurement plus the
rows inserted and deleted since, by the statistics' exact counters, taken just
before it measured — `runtime.storage_mark`); the statement that would go past the
limit fails the whole transaction with SQLSTATE `MV402`, whichever path wrote
it — cell entry, a record, an import, a model copy, the AI developer. The gateway
answers `402 {"code": "plan_limit", "limit": "max_storage_mb"}` with the
sentence: "Not saved: this would take the workspace to about 104 MB. The
Community Edition allows 100 MB of storage. A Community workspace holds up to
100 MB. …". A dimension-member file is refused whole, not up to the row that
fits. A transaction that only deletes is never refused. Moving a workspace to
another edition puts the guard on or takes it off at once; an edition without a
limit has no guard and pays nothing for it. A shared database has no guard: its
tenants are checked before bulk writes (`plan.CheckStorage`) and by the sweep.

A tenant that names an edition with no row is treated
as unlimited and shown as "(no such edition)": a catalog gap must never lock a
paying customer out.

## What an edition does

Everything below is in `internal/plan`; the gateway applies it in
`internal/gateway/plan.go`.

**Limits, at the moment of creation.** The handlers that create users
(console, SCIM, single sign-on's first login), applications, models
(including package import), metrics, dimension members and data rows, and the
AI message and integration run endpoints, ask the edition first. The storage
limit is asked where data rows arrive in bulk (cell writes, CSV commit,
package import): a tenant already at its `max_storage_mb` is refused there
with `402 {"code": "plan_limit", "limit": "max_storage_mb", …}`; a write
that crosses the line is caught by the sweep. A creation that
would cross the limit is refused with `402 {"code": "plan_limit", "limit":
"max_models", "max": 3, "current": 3, "error": "The Small Edition allows 3
models; this tenant has 3. Change the edition to add more."}`.

**Limits, by the sweep.** Not every path that can add rows goes through
those handlers (the AI assistant's write executor, scheduled integrations, a
package restore). Every five minutes the gateway recounts each tenant against
its edition (`plan.RunSweep`, one pass per database) and records the verdict on
the tenant's row (`limit_state`, `limit_reason`, `usage_checked_at`), with the
storage it measured (`storage_bytes`, migration 106). Storage is measured on
every edition in a dedicated database, where it is a catalog read, and in a
shared one only for an edition that limits it, since the estimate counts the
tenant's rows. A tenant
found **over** a limit is read-only — except for **deleting**, so it can get
back under — with `402 {"code": "over_limit"}` and the reason naming the
limit and the object ("… 100000 data rows per model; this model has 120000
(model "Budget")"). The next sweep that finds it within limits clears the
state. The gateway and the sweep share one enforcer, so a verdict applies to
requests at once; an edition edit reaches every tenant on the edition within a
minute (the per-tenant state is cached for 60 seconds).

**Who is exempt.** Platform administrators are never refused: they are who
changes the edition. A request that belongs to no tenant (a platform-level
developer, an admin acting across tenants) has no edition applied. The tenant of
a request is the routed tenant in dedicated-database mode, else the
application named by `X-App-Id`, else the caller's own tenant. A person's own
display preferences (`PATCH /api/me/preferences`, e.g. the console theme) are
never refused: they are not tenant data.

Every refusal carries `contact_url` (`PLAN_CONTACT_URL`), which the console
shows as "Change edition": a pricing page, or a `mailto:`.

## Self-service sign-up

`POST /api/signup` turns a visitor into a tenant. It is off unless the
deployment opts in:

| Variable | Meaning |
|---|---|
| `SIGNUP_ENABLED` | `true` opens `/signup` and `POST /api/signup` |
| `PLAN_CONTACT_URL` | Where "change the edition" leads in edition-limit refusals. Leave it unset until there is a page or a mailbox that answers: the console then omits the link rather than showing one that goes nowhere |

A public sign-up funnel also needs the two documents it asks a visitor to
agree to. They are configuration of the same kind — see
[`LEGAL_AND_PRIVACY.md`](LEGAL_AND_PRIVACY.md).

and it needs an edition flagged `self_service` (the first one in display order is
assigned) and, outside the dev stack, the Keycloak provisioning credentials
(`KEYCLOAK_ADMIN_CLIENT_ID/SECRET`) plus SMTP on the realm, because the
account is only usable once the invitation mail is completed.

The page at `/signup` (rendered before any sign-in; `web/src/signup/`) asks
`GET /api/signup/options` what is on offer and shows exactly that — the
edition's name and limits — so a closed deployment or a changed edition never has
a page promising something else. One request creates:

1. the identity-provider account, with the `tenant_admin`, `developer` and
   `business_admin` realm roles — nothing else exists yet, so a refusal here
   costs nothing;
2. the tenant (its own database in dedicated mode), on the self-service edition;
3. in one transaction on the tenant's database: the person (with the same
   three roles, `business_admin` scoped to the default workspace), an
   application named "Getting started", and the **starter models** —
   `starter.Starters()` in `internal/starter`, four models in that one
   application, each recorded in `core.starter_model` with its key and a
   hash of its content (see *Starter models after sign-up* below):
   - **Learn the platform**, the guided tour: dashboards of prose and
     diagrams taught on a deliberately tiny example (two teams, four
     quarters, three metrics of which one calculated) so that the grid, the
     chart and the KPI cards on the tour's own pages are the working screens
     rather than pictures of them;
   - **Developer guide**, **Business admin guide** and **Tenant admin
     guide**, one per role that builds or runs the workspace, each a model
     of its own dashboards.

   The tour, **Learn the platform**, is made the application's **default
   model** (`core.application.default_model_id`, the same setting as
   Developer › Developer Models › "Set as business default"; `starter.LandingKey`),
   so it is what the console opens on, at its first page, *1 · Start here*.
   That page links each reader on to the guide for what they came to do,
   and every guide links back (text-widget links to dashboards, whose
   target is `dashboard:Model name/Dashboard name`); the model switcher
   (User › Models or Business Admin › Models) reaches them too. From
   2026-10-06 to 2026-10-07 the Developer guide was the default (migration
   122); migration 124 moved every existing tenant whose default was still
   that guide back to the tour, and left any default someone had changed. The four are created in
   one transaction and share a creation time, so the default — not "newest
   model" — is what decides; if the default model is later deleted the
   default clears, and listings and resolution fall back to name order. The
   same tie-break picks the model behind what an application keeps against
   its oldest model when a request names no revision — automation rules,
   workflow definitions, the workflow trigger events: the default model,
   until the default moves (what was made there stays with that model). Each model is created through
   `modeltransfer.Import`, the same path a package import takes, and after
   the transaction commits every calculated metric of every model is
   recalculated, so each first screen shows real numbers. Nothing about them
   is a special tutorial mode: they are ordinary text, image, grid, chart
   and KPI widgets, which the tenant can edit or delete like any other;
4. the invitation to set a password (three days).

A failure after step 1 undoes everything made so far; an invitation that
cannot be sent undoes all of it ("nothing was created"). The request is
rate-limited per address (three attempts, then one every five minutes), an
address that already has an account — in the control plane, in any dedicated
tenant's directory, or at the identity provider — is refused with 409, and
every sign-up is an audit event (`tenant.signed_up`) with the address,
company, edition and source address, the application, the tour as `model_id`
and the guides as `guide_model_ids` (comma-separated). The response's
`model_id` is the tour.

Sign-up itself is not checked against the edition's limits, but the periodic
sweep is: a self-service edition whose `max_models` is set below the number of
starter models (four), or whose per-model limits are below what a starter
model holds, puts every new tenant over its limit — and read-only — at the
next sweep. Keep `max_models` at 0 (unlimited) or at least 4 on the edition
flagged `self_service`.

On the dev stack (no identity provider) the response carries `dev_persona`,
and the page offers to open the console as the new account.

### Starter models after sign-up

A tenant keeps the starter models of the day it signed up unless something
brings them forward, and tenants that signed up before the guides existed
(a "Budget vs Actual" model until 2026-09-20, the tour alone until
2026-09-29) never had them. `internal/startersync` runs on every gateway
start, once per database, for every tenant that signed up for itself (its
`tenant.signed_up` audit event) or holds a `core.starter_model` record —
which includes every tenant a platform administrator created with the
**Getting started application** box ticked, the default since 2026-10-13
(`POST /api/admin/tenants` `getting_started`). One created with it unticked,
or before that day, has no record and is left alone. Per starter key:

- **never had it** — imported into the tenant's "Getting started"
  application (made again in its first workspace if it was deleted), as by
  sign-up; the tour becomes the application's default when it has none;
- **content changed since** (the recorded hash differs) — the current
  content is imported as a new revision of the same model, named "Updated
  <date>", and made live the way Set active does it: the time check, the
  access rules moved by lineage (the new revision takes the lineage of the
  live revision's dimensions, members and metrics, by name and code), the
  business roles' dashboard grants carried by dashboard name. The old
  revision stays, with anything typed into it;
- **deleted by the tenant** (`model_id` gone NULL) — not put back;
- **another revision made live by the tenant** — left alone: the model is
  theirs now.

A starter model a tenant got before records were kept is found by its name
in "Getting started" and treated as content changed. Each tenant is one
transaction under an advisory lock, so replicas starting together do the
work once; installs and updates are audit events (`model.imported`,
`revision.activated`) with actor role `platform(starter sync)`, and every
changed revision is recalculated. A deploy that changes no starter content
changes nothing.

## Unused workspaces (the Playground only)

In the hosted Playground, and nowhere else, a workspace nobody uses is retired
(`internal/dormancy`). The deployment says it is the Playground with
`PLAYGROUND=true`; without it the sweep never runs, **Platform › Editions**
hides the settings, the terms leave the clause out, `GET /api/admin/dormancy`
lists nothing, and `PUT /api/admin/plans/{key}` refuses non-zero periods. In the
Playground each edition has two settings — `dormant_warn_days` and
`dormant_delete_days`; both 0 means never, which is how Commercial and
Enterprise keep their workspaces:

- **Use** is a request routed to the workspace from its own people or API
  clients — the console, an API token, the chat connector. Its scheduled
  jobs running on their own do not count, and neither does a platform
  administrator looking at it. `platform.tenant_database.last_used_at`
  records it (at most hourly per gateway).
- After `dormant_warn_days` without use, the workspace's administrators get
  an e-mail naming the deletion date, with the sign-in link and, when
  `LEGAL_EMAIL` is set, where to ask for a restore.
- The workspace is deleted once `dormant_delete_days` have passed without
  use **and** at least the notice period (the difference) since that
  e-mail — the same deletion as `DELETE /api/admin/tenants/{id}`: its
  database, its people and their sign-in accounts. The administrators are
  told again. Backups keep it for `BACKUP_RETENTION_DAYS` (14 by default),
  the window both mails promise a restore in.
- Any use in between withdraws the notice; the next one needs a full
  quiet period again.
- Without a mail relay (`SMTP_HOST`) nothing is retired: a workspace is
  never deleted without notice. The sweep runs on every gateway every six
  hours; an advisory lock lets one run at a time.
- Workspaces existing when this shipped (migration 131) count from that
  release, so none is deleted sooner than a full period after it.
- **Platform › Applications** shows each workspace's last use and, once its
  administrators are told, its deletion date. Moving a tenant to an edition
  with 0 and 0 keeps it.
- The terms (`/terms`) then say "In the Playground, a workspace nobody uses
  for N days … is deleted", quoting the periods and the backup retention, and
  the mails say "Workspaces in the Playground that are not used are deleted".
  Publish the change to existing workspaces as section 8 of the terms
  requires before setting the periods.
- Only workspaces with a database of their own (`TENANT_DB_MODE=dedicated`)
  are covered: that is where use is recorded.

## Administration

- **Platform › Editions** (`GET/PUT /api/admin/plans[/{key}]`): edit names,
  descriptions, the self-service flag and every limit of the three editions —
  Community, Commercial and Enterprise. Nothing adds or deletes an edition (the
  PUT changes an existing key and answers 404 for any other; the console
  has no "New edition" since 2026-10-06). Tenant administrators can read the catalog (to see what
  an upgrade is) but not change it.
- **Applications › tenant card**: the meta line shows the edition and
  "read-only" when it applies. Below it, the storage the last sweep measured:
  on an edition with a storage limit a meter with "40 MB of 100 MB used · 60 MB
  free" (amber from 80%, red and "N MB over the limit" past it), otherwise
  "Storage: N MB used"; hovering shows when it was measured. This is how a
  tenant administrator sees the space left before the workspace turns
  read-only. Under it, each model shows its own share (`runtime.model_storage`,
  written by the same sweep — for every tenant with its own database, and in a
  shared one on an edition with a storage limit): its rows in the three data
  tables times each table's live bytes per row, so the tenant administrator can
  see which model to delete. The shares do not add up to the meter —
  definitions, people and audit are the rest. **Change edition** (platform administrators)
  moves the tenant. `PATCH /api/admin/tenants/{id}` takes `plan`; the next
  usage sweep judges the tenant by the new limits. A tenant administrator
  has the same console for their own tenant, minus this: lifting one's own
  limits is the platform's side of the relationship.
- `GET /api/admin/tenants` carries `plan_state` per tenant; `GET /api/me`
  carries `plan` for a member of a tenant. Both include `storage_bytes` once
  a sweep has measured it, and the tenant listing has each model's
  `storage_bytes` as well.

## Limits of this design

- Daily limits (AI messages, integration runs) refuse at request time only;
  they are not part of the sweep and reset by themselves at midnight UTC.
- The sweep counts; it does not archive. A tenant over a storage limit has to
  delete, or be moved to a larger edition.
- The storage guard measures the room left once per transaction and prices a
  row at its table's average: two writes at the same moment can each fit and
  together pass the limit, and rows much wider than their table's average
  count for less than they take. The next sweep finds such a workspace over
  and makes it read-only.
- Sign-up creates one person per tenant. Inviting colleagues is the tenant
  administrator's first job, under Users, and counts against `max_users`.
- Keycloak's own registration is not used and stays off: the platform must
  create the tenant with the account.

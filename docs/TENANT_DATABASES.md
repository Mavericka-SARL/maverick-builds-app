# A database per tenant

> **Classification:** Current — How dedicated tenant databases work and how to operate them.

> **Last verified:** 2026-10-01

Every tenant can have its own PostgreSQL database on the shared server. The
boundary is then enforced by the database itself rather than by a `WHERE`
clause in each handler, which is what makes a mistake in application code
unable to leak one customer's data into another's.

The behaviour is opt-in per deployment: `TENANT_DB_MODE=shared` (the default)
keeps every tenant in the one database, exactly as before.

## The two kinds of database

| | Control plane | Tenant database |
|---|---|---|
| Which one | the database `DATABASE_URL` points at | `tenant_<customer id without dashes>` on the same server |
| Holds | the tenant catalog and directories (schema `platform`), platform administrators, and every tenant created while in shared mode | one `core.customer` row and everything under it: workspaces, applications, models, revisions, definitions, facts, workflows, audit |
| Schema | all migrations | all migrations (the `platform` tables simply stay empty) |

A tenant database is self-contained: the same SQL the platform already runs
works unchanged, because inside it there is exactly one customer.

## Routing a request

`internal/gateway/tenant.go` decides, once per request, which database the
request belongs to, from three sources in order:

1. **`X-Tenant-Id`** — a tenant to act on. Honoured for a member of that
   tenant, and for anyone platform-level: someone the control plane holds
   with platform reach (a platform admin or a platform-wide builder), even
   when a tenant also holds them. Having no membership is not platform
   level: a subject held nowhere yet — a first sign-in through a tenant's
   identity provider — stays on the control plane. A tenant without a
   database of its own, and the reserved address `control-plane`, name the
   control plane, honoured for anyone platform-level or held there.
2. **`X-App-Id`** — resolved through `platform.application_directory`. This
   covers every business and developer request, because the console already
   sends the header. An application missing from the directory is the
   control plane's, for a caller it holds. A platform-level caller's
   `/api/admin/` requests, and the deployment's notification settings,
   ignore it: their administration is addressed by `X-Tenant-Id`, never by
   the application last opened.
3. **the caller's membership** — `platform.user_directory`, keyed by the
   Keycloak subject, which is known before any tenant data is read; the
   oldest first, among the tenants whose database holds an active account
   of theirs (a deactivated home is no default). Not for someone
   platform-level, whose default is the control plane.

Nothing found means the control plane. That is where platform admins live, so
their own `/api/me`, the audit log of their actions and the tenant catalog are
all read there. Whether the control plane holds a person, and with what reach,
is cached for 30 seconds for routing only: their roles are read again where
the request lands.

The chosen pool travels in the request context, and the gateway's handlers use
it through `pkg/tenantdb.Handle`, which has the same `QueryRow`, `Query`,
`Exec` and `Begin` methods a pool has. Background work started from a request
keeps the routing, because the scope survives `context.WithoutCancel`.

### Admin actions across tenants

An admin request touches two databases: the actor lives in one (the control
plane), the data in another. Two arrangements exist.

- **Handlers that take the tenant from the body** (a new application's
  `customer_id`, a tenant's id in the path) keep them apart: `ctx` stays
  where the actor is, so audit rows land beside the user they name, and a
  second context reaches the tenant's data. Getting this wrong shows up
  immediately as a foreign-key violation on `audit_event.actor_user_id`.
- **Requests addressed to a tenant** (`X-Tenant-Id`, or `X-App-Id` of one
  of its applications) by someone platform-level run in the tenant's
  database, as a member's would. Their actor is read from the control plane
  (`internal/gateway/stand_in.go`), and only a platform admin or a
  platform-wide builder is resolved that way; anyone else resolves in the
  tenant's database as before. So that what they write there can name them,
  the tenant's database holds a **stand-in** under their id, with a subject
  of its own (`stand-in:<id>`), their name and a reserved address
  (`<id>@stand-in.invalid`, never mailed) (migration 103): no tenant, no
  role, no grant (a trigger refuses them), left out of every list and count,
  never an actor by itself, and answered 404 by the users routes; no other
  account may take an address in that domain. Roles always come from the
  control plane, so a demoted platform admin loses every tenant at once; a
  platform-wide builder is only a developer there, whatever it holds in
  shared tenants' workspaces. A tenant's own row of someone with platform
  reach — a platform admin who is also the tenant's person — is a platform
  account's to the tenant's administrators and SCIM token
  (`platformReachOf`): only a platform admin changes it.
  Nothing platform-level is held in a tenant's database: `platform_admin`
  is refused there and ignored if found, and `developer` or `tenant_admin`
  without a workspace is refused on an account with no tenant of its own,
  where it would be platform-wide (`dedicatedGrantErr`).
  This is how the platform console's per-tenant actions work: the users
  list's actions, the settings tabs' tenant picker, and a tenant's model
  export and revisions. A platform admin's or builder's own settings —
  preferences, AI provider and key — stay in the control plane.

### People in more than one database

One identity can be held by several databases: dedicated tenants the
directory lists, and the control plane. Each is one of the person's
**homes**, with an account of its own there. Their requests go to the oldest
membership unless they address another home — by one of its applications,
by `X-Tenant-Id`, or as `control-plane`.

- **Lists span homes.** The applications (`/api/apps`, the Applications
  lists), the workflow inbox, notifications, users, workspaces and the audit
  log are read once per home, by the person's own account there, and merged
  (`homesOf`). A home lists only by what the person holds in it: a tenant
  admin of one tenant who is a user of another sees none of the
  other's administration. Each row says which home it lives in (`tenant_id`),
  and what the console does with it next is addressed there and resolved
  again.
- **Invitations add members.** An invitation of someone another database
  holds — another dedicated tenant's, or across the control plane — adds
  them as a **member**: a row under their subject that belongs to no tenant
  (its tenant is in another database), shown as from another organisation,
  which the inviting tenant's administrators cannot rename, re-invite or
  delete. Its name and e-mail follow the person's sign-in. Nothing on their
  identity-provider account changes, and no mail is sent. Remove from this
  tenant takes their roles and grants and the directory entry; inviting them
  again restores it. Someone with platform reach is changed only by a
  platform admin. SCIM answers 409 for anyone another database holds, and may
  deactivate, reactivate or delete a member's row in its tenant, and nothing
  of the member's account.
- **The clean-up.** Before 2026-09-30 such invitations and SCIM creations
  adopted the person instead. The gateway undoes that at every start
  (`ReconcileAdoptedAccounts`, after the tenant databases are migrated). A
  platform admin's or platform-wide builder's row in a dedicated tenant, and
  its directory entry, are removed. Anyone else's adopted row becomes a
  member's, with a role it held without a workspace moved into each of the
  tenant's workspaces, where it reached before. A tenant adopted a
  control-plane account only if that account is older than the tenant's
  directory entry; in the reverse case the tenant's row is the person's, and
  the control-plane row, when a shared-database tenant owned it, becomes a
  member's there. Every `platform_admin` grant found in a tenant's database
  is removed: that role is held only in the control plane. Each change is
  logged and audited for the platform only; once it has run, nothing
  matches.

### What a platform admin sees

Everything, in every database: one users list, workspaces, tenants with
their applications, models and revisions, the audit log, and usage — the
control plane's tenants and each dedicated tenant's — each row naming its
database. A dedicated tenant whose database cannot be read is listed with
why, on the tenants list and in usage, instead of being left out. The
platform Users tab narrows the list with **People of**, and addresses each
action to the row's database. A platform-wide builder sees every tenant's
applications the same way, and builds in each through a stand-in.

## Lifecycle

- **Provisioning** (`POST /api/admin/tenants`): a catalog row, `CREATE DATABASE`,
  every migration, the `core.customer` row with the same id as the catalog,
  and a `Default` workspace. A failure after the database exists leaves the
  catalog row `failed` with the reason instead of retrying silently.
- **Upgrades**: the gateway migrates the control plane and then every ready
  tenant at start-up, then undoes adoptions across databases (above). A tenant whose migration fails is marked `failed` and is
  not served, so it can never run against a schema it was not migrated to;
  the other tenants start normally.
- **Deletion** (`DELETE /api/admin/tenants/{id}`): `DROP DATABASE … WITH (FORCE)`
  and the catalog row. Final, and the only way the data goes away. The
  tenant's people go with it — every user it owns (signed up into it or
  created by its administrators) loses their identity row and their
  Keycloak account, exactly as `DELETE /api/admin/users/{id}` removes one
  person; a platform administrator is never a tenant's to delete. In the
  shared database the same cascade runs before the customer row is deleted.
  Deleting a person never deletes the tenant's data: what they entered,
  posted, started or imported stays, authored by "a former user"
  (migration 094).
- **Pools**: opened on first use, capped at `TENANT_DB_MAX_CONNS` (5 by
  default) and closed after ten idle minutes. Hundreds of test-workspace tenants
  therefore cost nothing while idle.

## Background work

Workflow schedulers and integration workers run per database, one goroutine
set each, discovered through `Router.Watch` — including tenants created while
the process is running. A run is only ever claimed by a worker connected to
the database that holds it.

## Configuration

| Variable | Default | Meaning |
|---|---|---|
| `TENANT_DB_MODE` | `shared` | `dedicated` gives each tenant its own database |
| `TENANT_DB_ADMIN_URL` | empty | Connection string with `CREATEDB` rights, used only to create and drop databases. Empty reuses `DATABASE_URL`'s credentials, which is right when the application role owns the server |
| `TENANT_DB_MAX_CONNS` | `5` | Cap per tenant pool |

The gateway reads all three. `cmd/integration` reads `TENANT_DB_MODE` and
`TENANT_DB_MAX_CONNS` only: it never creates or drops a database, so it has
no use for `TENANT_DB_ADMIN_URL`.

### PgBouncer

The shipped PgBouncer (`deploy/k8s/base/infra/pgbouncer.yaml`) runs in
transaction pooling with a wildcard database entry (`* = host=postgres`), so
any database name is proxied and dedicated tenants open `tenant_<id>`
through the same address. A PgBouncer pinned to one database name
(`DB_NAME` set on that image) refuses them with "no such database" — which
is why the base leaves it unset.

## Backups

Each database is backed up and restored on its own, which is the point: one
tenant can be restored without touching another. `deploy/docker/pg-backup`
takes `BACKUP_DATABASES` (`all` dumps the control plane plus every
`tenant_*` database, one object per database). WAL-G continues to cover the
whole server for point-in-time recovery.

## Migrating an existing deployment

Nothing happens automatically. A deployment that has been running in shared
mode keeps every existing tenant in the control plane, where they go on
working; switching `TENANT_DB_MODE` to `dedicated` only changes where *new*
tenants are created. Moving an existing tenant out is a deliberate operation:
export its revision as a package (tenant admin, with data), create the tenant
in dedicated mode, import the package, then delete the old rows.

## Limits

- One PostgreSQL server holds every tenant database, so it remains the shared
  resource: isolation here is about access, not about noisy neighbours.
- A tenant's users are rows in its own database, so one person who belongs to
  two tenants has two user rows, with different ids; the directory maps their
  identity to both. Lists that span a person's homes show one row per home.
- The cross-tenant admin listing opens one pool per tenant. That is fine for
  hundreds of tenants and would need paging for thousands.

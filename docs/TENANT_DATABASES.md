# A database per tenant

> **Classification:** Current — How dedicated tenant databases work and how to operate them.

> **Last verified:** 2026-09-15

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

1. **`X-Tenant-Id`** — a platform admin acting on a tenant that is not their
   own. Honoured only for callers who have no tenant of their own, or who are
   members of the tenant they name.
2. **`X-App-Id`** — resolved through `platform.application_directory`. This
   covers every business and developer request, because the console already
   sends the header.
3. **the caller's membership** — `platform.user_directory`, keyed by the
   Keycloak subject, which is known before any tenant data is read.

Nothing found means the control plane. That is where platform admins live, so
their own `/api/me`, the audit log of their actions and the tenant catalog are
all read there.

The chosen pool travels in the request context, and the gateway's handlers use
it through `pkg/tenantdb.Handle`, which has the same `QueryRow`, `Query`,
`Exec` and `Begin` methods a pool has. Background work started from a request
keeps the routing, because the scope survives `context.WithoutCancel`.

### Admin actions across tenants

An admin request touches two databases: the actor lives in one (the control
plane), the data in another. The handlers keep them apart deliberately —
`ctx` stays where the actor is, so audit rows land beside the user they name,
and a second context reaches the tenant's data. Getting this wrong shows up
immediately as a foreign-key violation on `audit_event.actor_user_id`.

## Lifecycle

- **Provisioning** (`POST /api/admin/tenants`): a catalog row, `CREATE DATABASE`,
  every migration, the `core.customer` row with the same id as the catalog,
  and a `Default` workspace. A failure after the database exists leaves the
  catalog row `failed` with the reason instead of retrying silently.
- **Upgrades**: the gateway migrates the control plane and then every ready
  tenant at start-up. A tenant whose migration fails is marked `failed` and is
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

Both the gateway and `cmd/integration` read them.

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
  two tenants has two user rows; the directory maps their identity to both,
  and requests route to the first membership unless a header says otherwise.
- The cross-tenant admin listing opens one pool per tenant. That is fine for
  hundreds of tenants and would need paging for thousands.

# Audit log export and retention

> **Classification:** Current — Getting the audit log out, feeding a SIEM, and how long it is kept.

> **Last verified:** 2026-09-16

An enterprise feature (`audit_export`). Every write to the audit log goes
through `pkg/auditlog.Log`; this is how the result leaves the platform.

## Export

**Admin › Audit Log › Export**: choose a format and a date range and
download. The scope is exactly the listing's: a tenant admin gets the
events of their own tenant, a platform admin everything. The download is
streamed, so it is not bound by the listing's 200 rows.

- **CSV** — one row per event with a stable column order:
  `occurred_at, category, event_type, actor_name, actor_email, actor_role,
  application_id, application_name, revision_id, revision_name,
  resource_type, resource_id, metadata (JSON), id`.
- **JSON Lines** — one object per event, with `metadata`, `before_state`
  and `after_state` as JSON.

Every export is itself recorded (`audit.exported`: who, format, range).

## Feeding a SIEM (collector pull)

The same endpoint streams JSON Lines with a cursor, oldest first, so a
collector can pull everything and then only what is new:

```
GET /api/admin/audit/export?format=jsonl&since=2026-09-01&limit=10000
```

When a page is full, the final line is `{"next_cursor": "…"}`; pass it back
as `after=` for the next page and stop when a response ends without one.
The cursor is the position after an event (its time and id), so it is
stable across events with the same timestamp and across restarts. Poll on a
schedule with the last cursor you stored; nothing is missed and nothing is
repeated. Authenticate as an administrator (a service account with the
`tenant_admin` role, or a personal token).

## Retention

**Admin › Audit Log › Retention**: how many days of events to keep. `0`
(the default) keeps them forever. Any other value is at least **30 days** —
an audit log that a setting can empty is not an audit log — and the
platform refuses less. A daily sweep removes events older than the
retention. It runs only while the licence includes the feature: if a key
lapses, events are kept, never deleted. Export before shortening retention;
the sweep does not archive.

Retention is **per tenant** (migration 091): the sweep removes each
tenant's events — those of its applications, or of its users where an
event names no application — by that tenant's own days; platform-level
events with neither are kept. A platform admin sets any tenant's retention
by naming it (`X-Tenant-Id`), and on `deployment_settings` the deployment's
default, which a tenant follows until it sets its own (`DELETE
/api/admin/audit/settings` returns it to following).

Changing retention is recorded (`audit.retention_updated`).

## Where the code lives

- `ee/auditexport` — streaming, formats, the cursor, retention and the sweep.
- `internal/gateway/audit_export.go` — the routes, behind
  `requireFeature(license.FeatureAuditExport)`; `auditScope` in
  `handler.go` is the one definition of "your events", shared with the
  listing.
- `migrations/082_audit_export.sql` — `audit.settings` and the cursor index.
- `web/src/ee/auditexport/AuditExportPanel.tsx` — the panel above the
  Audit Log table.

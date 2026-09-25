# Usage analytics per tenant

> **Classification:** Current — What a tenant uses, counted from what the platform already keeps.

> **Last verified:** 2026-09-16

An enterprise feature (`usage_analytics`). **Admin › Usage** shows every
tenant to a platform admin and their own tenant to a tenant admin, for a
period of 7, 30 or 90 days (the API, `?period=`, accepts any whole number of
days from 1 to 365; default 30).

## What is counted

| Number | Source |
|---|---|
| users, active users | `identity.user` of the tenant; active = seen by the gateway (`last_seen_at`) or signed in (`last_login_at`) within the period |
| applications, models, revisions | `core.application` (by customer or workspace), `core.model`, `model.revision` |
| fact rows, calculated rows, form records | `runtime.fact_input`, `runtime.calc_result`, `runtime.form_record` of the tenant's models |
| uploaded files | `storage.object` bytes by uploading user |
| database | `pg_database_size` of the tenant's own database (dedicated databases only; 0 on a shared one, because that number would be everyone's) |
| workflow instances, integration runs, AI messages, audit events | started / run / sent / recorded within the period |
| last activity | the tenant's newest audit event |

Nothing is sampled or estimated: each is a count over a table that already
exists. `last_seen_at` is new: the gateway marks an account at most every
five minutes while it is making requests, so a busy session costs one write
per interval rather than one per request.

## Where the code lives

- `ee/usage` — the query, one tenant at a time.
- `internal/gateway/usage.go` — `GET /api/admin/usage?period=`, behind
  `requireFeature(license.FeatureUsageAnalytics)`; with dedicated databases
  a platform admin's request counts each tenant in its own database.
- `migrations/083_usage_last_seen.sql` — `last_seen_at`; `touchLastSeen` in
  `handler.go` maintains it.
- `web/src/ee/usage/UsageTab.tsx`.

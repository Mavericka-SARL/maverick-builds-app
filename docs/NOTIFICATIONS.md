# Notifications: in-app, e-mail, webhooks and reminders

> **Classification:** Current — How notifications are produced and delivered.

> **Last verified:** 2026-09-15

Every notification the platform produces is written to the recipient's
notification centre, the bell in each console. A tenant can additionally have
each one delivered by e-mail to the recipient, or posted as JSON to a webhook,
and can have overdue workflow tasks remind the people who can act on them.

## What produces a notification

| Producer | Template | Points at |
|---|---|---|
| A workflow **notification step** | `workflow_step_notification` | the workflow instance |
| A step **sent back for rework** | `workflow_step_notification` | the workflow instance |
| An automation rule that **could not start** its workflow | `workflow_step_notification` | the automation rule |
| A workflow task that **came due** | `workflow_task_reminder` | the workflow instance |

Producers call `Store.Notify`, which writes the in-app row and then one row
per outbound channel the tenant has turned on. A notification therefore
reaches the console even when every outbound channel is off, or misconfigured.

## Turning delivery on

**Admin › Notifications** in the console (platform or tenant admin):

- **E-mail** — send each notification to the recipient's address as well.
- **Webhook** — post each notification to a URL, with a signing secret.
- **Task reminders** — remind assignees when a step reaches the due time its
  designer set through SLA hours, optionally a configurable number of hours
  before.

The settings belong to the database they are stored in, so with dedicated
tenant databases each tenant decides for itself.

## The mail relay is deployment configuration

SMTP credentials are not a tenant setting: they belong to whoever runs the
deployment, and a tenant admin must not be able to redirect the platform's
mail. The gateway reads them from its environment:

| Variable | Default | Meaning |
|---|---|---|
| `SMTP_HOST` | empty | Relay host. Empty means no relay: e-mail notifications are reported as undeliverable rather than queueing forever |
| `SMTP_PORT` | `587` | Relay port |
| `SMTP_USERNAME` / `SMTP_PASSWORD` | empty | Credentials; omit both for an unauthenticated local relay |
| `SMTP_FROM` | `mavericks@localhost` | Envelope and header sender |

The console shows a warning on the settings screen when no relay is
configured, so e-mail is never switched on into a void.

## Webhook deliveries

One POST per notification, `Content-Type: application/json`:

```json
{
  "id": "…",
  "template_id": "workflow_step_notification",
  "subject": "Approval needed",
  "message": "The FY2027 budget is waiting.",
  "recipient_user_id": "…",
  "recipient_email": "jo@example.com",
  "resource_type": "workflow_instance",
  "resource_id": "…",
  "vars": { "subject": "…", "message": "…" },
  "created_at": "2026-09-15T09:30:00Z"
}
```

When a signing secret is set, the request carries
`X-Mavericks-Signature: sha256=<hex HMAC of the exact body>`. Verify it with
the same secret before trusting a delivery. Any 2xx response counts as
delivered; anything else is retried.

## Delivery, retries and failure

Outbound notifications are written pending and delivered by a dispatcher that
runs beside the gateway, one per database, every twenty seconds.

- A pass claims a batch, so several gateway replicas never deliver the same
  notification twice.
- A failed attempt is retried with a growing delay: one minute, five, fifteen,
  then hourly.
- After **five** attempts the notification is marked `failed` and keeps the
  reason on the row. A notification is never silently dropped, and never
  retried forever.
- A channel switched off after rows were queued fails them with that reason
  instead of delivering them.

## Reminders

The reminder loop runs every five minutes per database and looks for steps
that are in progress, have a due time, and have not been reminded about. For
each it notifies every user who could act on the step — the same platform-role
and named-business-role matching the task inbox uses — and records that it
did, so a task that stays overdue is reminded about **once**, not on every
pass. Test runs are skipped: a rehearsal must never page real people.

A step only has a due time when its designer set **SLA hours** on it, so
reminders and SLA hours are two halves of the same feature.

## Editions

Everything on this page is in the community edition.

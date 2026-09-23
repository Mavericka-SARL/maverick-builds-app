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

The settings are **per tenant** (migration 091): each tenant's row decides
its own channels, in a shared database as much as in a dedicated one —
tenant A's webhook never receives tenant B's notifications. A tenant admin
edits their own tenant's; a platform admin any tenant's, by naming it
(`X-Tenant-Id`, the console's *Settings of* picker at platform scope).

On the enterprise edition (`deployment_settings`) the platform admin can
also set the **deployment's defaults** — the row with no tenant, edited at
platform scope without naming a tenant — which every tenant follows until
it saves settings of its own; *Follow the deployment's defaults instead*
(`DELETE /api/notifications/settings`) drops a tenant's own row again.
Every settings response carries `scope` saying whose values it shows and
whether they are inherited. Other editions have no deployment row: each
tenant only ever has its own.

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

On Kubernetes (`deploy/k8s`), the host, port and sender are keys of the
`mavericks-config` ConfigMap and the credentials are the `mavericks-smtp`
Secret, sealed with `scripts/seal-smtp-secret.sh` — a Secret of its own so a
relay key can be rotated without re-sealing the database password. The
backup watchdog's alert mail reads the same Secret. The gateway reads all of
it once, at start-up: after any change, `kubectl rollout restart
deployment/gateway`. The gateway's NetworkPolicy admits egress on 587 and
465 for this; a prod overlay that restates the gateway's egress list (the
object-storage one does) has to repeat that rule, or every send hangs until
the dispatch deadline and looks like a dead relay.

Two things bite on the first roll-out. A pod that starts in the same
apply that creates the sealed Secret comes up with empty credentials —
`optional: true` on a Secret that does not exist *yet* is silently empty,
and the relay then answers `530 authentication Required` — so restart the
gateway once more after the Secret exists. And a NetworkPolicy rule is easy
to put in the wrong policy: several service policies end in the same
pgbouncer-plus-DNS tail; `kubectl diff` before `apply` is what shows which
policies actually change.

The port decides the transport: 587 is submission with STARTTLS, which
`net/smtp` negotiates on its own and which Resend, Postmark, SES and the
like all serve; 465 (implicit TLS) is not supported by the gateway — the
watchdog's `curl` handles both.

**Prove it by sending.** Reading the configuration back shows only that it
was stored; a wrong key, an unverified sending domain, or a NetworkPolicy
that drops the port is invisible until something actually tries to send. So
the settings screen has *Send me a test e-mail*: the signed-in administrator
is mailed, synchronously, and the relay's own answer comes back — "535
authentication failed", "450 domain not verified", or "no answer within
20s" with the NetworkPolicy hint. The recipient is fixed to the caller's own
address; the relay cannot be used to mail anyone else. Each attempt is an
audit event (`notification.test_sent`, with the outcome).

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

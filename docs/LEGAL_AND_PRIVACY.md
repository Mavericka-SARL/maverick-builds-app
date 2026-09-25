# Terms of service and privacy notice

> **Classification:** Current — What the engine publishes at `/terms` and `/privacy`, what the operator must configure, and the rule the text follows.

> **Last verified:** 2026-09-18

A deployment that lets strangers sign up collects personal data from them, and
owes them two documents before it does: what the agreement is, and what happens
to their data. This repository ships both, because what the software does with
data is the same wherever the engine runs. Who is accountable for it is not, so
every deployment-specific value is configuration and a clause with no value
configured is left out rather than guessed at.

## What a visitor sees

| Page | Source |
|---|---|
| `/terms` | `web/src/public/legal/documents.ts`, `termsDocument` |
| `/privacy` | `web/src/public/legal/documents.ts`, `privacyDocument` |
| The line under the sign-up form | `web/src/signup/SignUpPage.tsx`, shown only when both documents exist |
| The links under the sign-in form | `web/src/public/LegalFooter.tsx` |

Both pages render outside the auth layer (`web/src/main.tsx`): requiring an
account in order to read the terms of the account would be absurd.

## Configuration

The gateway reads these and serves them at `GET /api/legal`
(`internal/gateway/legal.go`). Every one is optional, and every one is a
deployment's own answer.

| Variable | What it is |
|---|---|
| `LEGAL_ENTITY` | The operator's legal name. It is the counterparty of the terms and the controller of the personal data |
| `LEGAL_ADDRESS` | The operator's postal address, which an EU privacy notice has to carry |
| `LEGAL_EMAIL` | Where legal and data-protection correspondence goes. A mailbox someone reads, **not** a no-reply sender |
| `LEGAL_JURISDICTION` | Governing law and courts, as prose (`Luxembourg`). Unset omits the clause rather than inventing one |
| `LEGAL_HOSTING` | Who hosts the deployment and where, for the recipients and processing-location sections |
| `LEGAL_UPDATED` | `YYYY-MM-DD`, shown as "In force from". Move it whenever the substance changes |
| `LEGAL_TERMS_URL` | An operator's own terms hosted elsewhere. Set it and `/terms` redirects there instead of rendering the shipped text |
| `LEGAL_PRIVACY_URL` | The same for the privacy notice |

Two states are worth knowing:

- **Nothing configured.** `/api/legal` reports `published: false`, the two
  pages say the deployment has published nothing, and the sign-up form shows no
  "you agree to" line at all. An install that has written no documents never
  claims agreement to documents that do not exist — that is the one property
  the code actually guarantees, and `internal/gateway/legal_test.go` holds it.
- **`LEGAL_ENTITY` and `LEGAL_EMAIL` set.** The shipped documents render with
  the operator filled in. A name with no mailbox is not enough: there would be
  nowhere to exercise a right the notice grants.

## The rule the text follows

**Only state what the code does.** Every factual claim in either document is
checkable in this repository:

| The text says | Where it comes from |
|---|---|
| The free plan's limits, and that the workspace goes read-only | `GET /api/signup/options` at render time — the seeded plan, not a number typed into the page |
| Backups are kept for fourteen days | `BACKUP_RETENTION_DAYS` in `deploy/docker/pg-backup/pg-backup.sh` |
| No analytics, tracking, or third-party script | `web/index.html` loads none, and the CSP-relevant surface is the app's own bundle |
| Sign-in stores a cookie and an in-memory token | `web/src/auth/AuthProvider.tsx` — `check-sso` with PKCE, the token in React state |
| You can take your data out | The model, grid, form and audit export endpoints in `internal/gateway/handler.go` |
| An audit record of who changed what | `pkg/auditlog`; retention per tenant, else the deployment default (`ee/auditexport`, enterprise) |
| The assistant runs only on a configured key — the tenant's, the developer's own, or the deployment's env key | `buildProviderForRequest` in `internal/gateway/ai_handler.go` |

A sentence nobody can check is a liability, not a policy. When behaviour
changes, the text changes with it and `LEGAL_UPDATED` moves.

## Before going live

1. Set `LEGAL_ENTITY`, `LEGAL_ADDRESS`, `LEGAL_EMAIL` and `LEGAL_JURISDICTION`
   in the deployment's ConfigMap, and `LEGAL_HOSTING` to the infrastructure
   provider. Set `LEGAL_UPDATED` to the day you publish.
2. Confirm the mailbox in `LEGAL_EMAIL` receives mail. A domain with no MX
   record does not, and a privacy notice whose contact address bounces is
   worse than none.
3. Read both documents end to end as they render, not as they are written
   here. They are a starting point and this repository is not a law firm: an
   operator taking money or holding other people's data should have them
   reviewed, and should say so to itself in writing if it chooses not to.
4. Check `/signup` shows the agreement line, and that both links open.

## Related

- [Plans, trials and self-service sign-up](PLANS_AND_SIGNUP.md) — the plan the
  terms quote, and the read-only state they describe.
- [The look of the pre-account pages](BRAND.md) — the frame both documents
  render in.

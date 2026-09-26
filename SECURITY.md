# Security policy

> **Classification:** Current — How to report a vulnerability in maverickbuilds.app.

## Reporting a vulnerability

Please report it privately, never in a public issue, pull request or
discussion:

1. **Preferred:** open a private report from the repository's **Security** tab
   (*Report a vulnerability*). Only the maintainers see it, and we can work on
   the fix with you in a private advisory.
2. **Or e-mail** team@maverickans.com with "Security" in the subject.

Include what you can of:

- the component (gateway, a service, the console, a deployment manifest) and
  the commit or snapshot you tested;
- the steps to reproduce it, or a proof of concept;
- the impact as you see it — whose data, which role, which tenant boundary;
- whether it is already public or known to anyone else.

## What happens next

- We acknowledge a report within five working days.
- We confirm or rule out the issue, tell you which, and agree a disclosure date
  with you once a fix exists. Please give us that time before publishing.
- We credit you in the advisory unless you ask us not to.

## Scope

Everything in this repository: the Community edition and the enterprise code
under `ee/`, the deployment manifests and the self-hosting setup. Only the
latest snapshot on `main` receives fixes; there are no maintained older
releases yet.

Problems in a dependency (Go modules, npm packages, Keycloak, PostgreSQL,
MinIO) belong with that project, unless the way maverickbuilds.app uses it is
what makes it exploitable.

For hardening a deployment of your own, see
[the self-hosting guide](docs/SELF_HOSTING.md).

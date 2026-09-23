# Editions and licensing

> **Classification:** Current — How editions, license keys and the `ee/` tree work.

> **Last verified:** 2026-09-15

maverickbuilds.app ships as one code base in three editions. The edition a
deployment runs is decided by a signed license key the gateway reads at
start-up; nothing calls home, and a missing, invalid or expired key never
stops the platform — it runs the Community edition and says why.

## Editions

| Edition | License | What it adds |
|---|---|---|
| Community | [Sustainable Use License](../LICENSE): use and modify for your own internal business, non-commercial or personal purposes; no hosting or reselling for third parties | The whole product except the gated features below |
| Commercial | Commercial License (a contract with the licensor) | The right to build solutions and services for clients; white-labelling |
| Enterprise | Commercial terms plus the [Enterprise License](../ee/LICENSE) for the `ee/` source | Single sign-on brokering and SCIM, per-cell change history, audit export, usage analytics, tenant-level AI keys, white-labelling, and support |

The legal texts in `LICENSE` and `ee/LICENSE` are the repository's first
drafts, modelled on the fair-code "sustainable use" family. Have them reviewed
by a lawyer before publishing the repository.

## Gated features

Feature keys are the contract between a key and a binary. They are defined
once, in `pkg/license/features.go`, and reported by `GET /api/license`.

| Key | Feature | Included from |
|---|---|---|
| `sso` | Single sign-on (SAML, OIDC brokering) | Enterprise |
| `scim` | SCIM provisioning | Enterprise |
| `cell_history` | Per-intersection change history | Enterprise |
| `audit_export` | Audit log export and streaming | Enterprise |
| `usage_analytics` | Usage analytics per tenant | Enterprise |
| `white_label` | Custom branding of the console | Commercial |
| `tenant_ai_keys` | Tenant-level AI provider keys | Enterprise |
| `deployment_settings` | Deployment-wide defaults for notification delivery, audit retention and the AI key, inherited by tenants without their own | Enterprise |

A key can name extra features beyond its edition's defaults (for tailored
contracts) and carry numeric limits (`max_users`, `max_tenants`, …). Key
limits are transported and displayed; what a *tenant* may use is a plan
question, enforced per tenant — see
[`docs/PLANS_AND_SIGNUP.md`](PLANS_AND_SIGNUP.md).

## The license key

A key is a self-contained token:

```text
MVX1.<base64url(payload JSON)>.<base64url(Ed25519 signature)>
```

The payload holds `id`, `edition`, `customer`, `contact`, `issued_at`,
`expires_at`, optional `features`, `limits` and `notes`. The signature is made
with the licensor's private key and verified with the public key compiled into
every binary (`pkg/license/pubkey.go`). A deployment can override that public
key with `MAVERICKS_LICENSE_PUBLIC_KEY` (used by tests, and by forks that issue
their own keys).

Verification happens in `pkg/license`:

- signature, format and edition are checked once at start-up;
- expiry is checked on every request, so a key that runs out while the
  gateway is up starts refusing gated calls without a restart;
- a key issued more than a day in the future is refused (clock skew guard).

## Applying a key to a deployment

Set one of these on the gateway and restart it:

| Variable | Meaning |
|---|---|
| `MAVERICKS_LICENSE_KEY` | The token itself |
| `MAVERICKS_LICENSE_FILE` | Path to a file holding the token (the key variable wins when both are set) |
| `MAVERICKS_LICENSE_PUBLIC_KEY` | Optional: base64 public key overriding the compiled-in verifier |

On Kubernetes the gateway deployment reads `MAVERICKS_LICENSE_KEY` from an
optional secret named `mavericks-license` (key `key`); create it with
`kubectl create secret generic mavericks-license --from-literal=key=MVX1...`
and restart the gateway. On the local stack export the variable before
`bash dev.sh`.

The gateway logs the outcome at start-up ("license key accepted",
"has EXPIRED", "could not be verified", or "no license key configured"). The
console shows it in the account menu of every role ("Enterprise edition") and
in detail under **Platform › License** for platform administrators.

## What a gated feature looks like in code

- **HTTP:** the route is registered in `internal/gateway` through
  `h.requireFeature(license.FeatureX, handler)`, inside the role guard. A
  locked feature answers `403 {"error": "<Feature> requires the enterprise
  edition; this deployment runs the community edition"}`.
- **Background work and gRPC:** call `Manager.Require(feature)` before doing
  gated work.
- **Frontend:** `useFeature("x")` or `<FeatureGate feature="x">…</FeatureGate>`
  from `web/src/license/`, which renders a short lock note naming the needed
  edition when the feature is off.
- **Source location:** enterprise and commercial implementation lives under
  [`ee/`](../ee/README.md) (Go) and `web/src/ee/` (frontend). It is compiled
  into every build; the key decides whether it runs.

## Issuing keys (licensor only)

`cmd/license` is the vendor tool. Customers never run it.

```bash
# once: generate the signing key pair; embed the public key in pkg/license/pubkey.go
go run ./cmd/license keygen -out ~/.mavericks/license-signing.key

# per customer
go run ./cmd/license sign -key ~/.mavericks/license-signing.key \
  -edition enterprise -customer "Acme Corp" -contact ops@acme.com \
  -expires 2027-12-31 -limit max_users=50 -out acme.license

# check any token against the compiled-in public key
go run ./cmd/license inspect -file acme.license
```

The private key must never enter a repository (`.gitignore` excludes
`*license-signing*`). Losing it means generating a new pair and shipping a
build with the new public key; leaking it means the same plus revoking trust in
every key signed with the old one, so keep it in a password manager or a
hardware token with an offline backup.

## Behaviour summary

| Situation | Edition in force | Reported as |
|---|---|---|
| No key configured | Community | `state: community` |
| Valid key | The key's edition | `state: active` |
| Key expired | Community | `state: expired`, with the key's customer and dates |
| Key unreadable, tampered, wrong signer, unknown edition, not yet valid | Community | `state: invalid`, with the reason |

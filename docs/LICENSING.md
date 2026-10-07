# Editions and licensing

> **Classification:** Current — How editions, license keys and the `ee/` tree work.

> **Last verified:** 2026-10-07

maverickbuilds.app ships as one code base in three editions. The edition a
deployment runs is decided by a signed license key the gateway reads at
start-up; nothing calls home, and a missing, invalid or expired key never
stops the platform — it runs the Community edition and says why.

A key is a technical credential that implements a contract. The rights to use
the paid editions come from the accepted order and agreement the key names,
not from possessing a key, and a key does not widen them.

## Editions

| Edition | License | What it adds |
|---|---|---|
| Community | [Sustainable Use License](../LICENSE): use and modify for your own internal business, non-commercial or personal purposes; no hosting or reselling for third parties | The whole product except the gated features below |
| Commercial | Commercial License (a contract with the licensor) | The right to build solutions and services for clients; white-labelling |
| Enterprise | Commercial terms plus the [Enterprise License](../ee/LICENSE) for the `ee/` source | Single sign-on brokering and SCIM, per-cell change history, audit export, usage analytics, tenant-level AI keys, deployment-wide settings, white-labelling, and support |

`LICENSE` and `ee/LICENSE` are version 1.0 (September 2026) and are the
licences in force. A paid order states what its customer may do; this page
only describes what the software does.

## Gated features

Feature keys are the contract between a key and a binary. They are defined
once, in `pkg/license/features.go`, and reported by `GET /api/license`.

| Key | Feature | Included from (latest schedule) |
|---|---|---|
| `sso` | Single sign-on (SAML, OIDC brokering) | Enterprise |
| `scim` | SCIM provisioning | Enterprise |
| `cell_history` | Per-intersection change history | Enterprise |
| `audit_export` | Audit log export and streaming | Enterprise |
| `usage_analytics` | Usage analytics per tenant | Enterprise |
| `white_label` | Custom branding of the console | Commercial |
| `tenant_ai_keys` | Tenant-level AI provider keys | Enterprise |
| `deployment_settings` | Deployment-wide defaults for notification delivery, audit retention and the AI key, inherited by tenants without their own | Enterprise |

A key can name extra features beyond its edition's defaults, for tailored
contracts. Keys carry no numeric limits: what a *tenant* may use is a plan
question, enforced per tenant — see
[`docs/PLANS_AND_SIGNUP.md`](PLANS_AND_SIGNUP.md). (Keys issued before
2026-10-07 could carry `limits`; nothing ever enforced them, and they are now
ignored.)

### Feature schedules

Which features an edition includes by default is a **feature schedule**: a
frozen list per edition, named by the month it was frozen. Every key names
the schedule it was sold under, and its default features come from that
schedule, never from whatever the running binary lists. A feature released
later therefore does not silently extend keys sold before it.

| Schedule | Commercial | Enterprise |
|---|---|---|
| `2026-10` | `white_label` | `sso`, `scim`, `cell_history`, `audit_export`, `usage_analytics`, `white_label`, `tenant_ai_keys`, `deployment_settings` |

- A key that names no schedule (every key issued before 2026-10-07) reads as
  `2026-10`, which is exactly what those keys unlocked when they were issued.
- A released schedule is never edited; `TestSchedulesAreFrozen` in
  `pkg/license` fails if one is. Including a new feature in an edition by
  default means adding a new schedule. Keys sold afterwards name it, and
  existing customers get it with a reissued key or by naming the feature.
- Adding a feature to the catalogue unlocks it for no key until a schedule
  or a key names it.
- A key naming a schedule newer than the binary gets the newest schedule the
  binary knows. Schedules only ever add features.

`go run ./cmd/license schedules` prints the schedules a build knows.

## The license key

A key is a self-contained token:

```text
MVX1.<base64url(payload JSON)>.<base64url(Ed25519 signature)>
```

The payload holds `id`, `edition`, `schedule`, `customer`, `contact`,
`order`, `agreement`, `deployment`, `issued_at`, `expires_at`, and optional
`features` and `notes`. `order`, `agreement` and `deployment` name the
accepted order, the agreement text the customer accepted, and the
deployment(s) the order covers. They are records for both parties, shown in
the console, and grant nothing by themselves.

The signature is made with the licensor's private key and verified against
the public keys compiled into every binary (`trustedPublicKeys` in
`pkg/license/pubkey.go`). A deployment cannot add a key of its own: there is
no setting for it. `MAVERICKS_LICENSE_PUBLIC_KEY` was removed on 2026-10-07,
and a gateway that still has it set logs a warning and ignores it. Tests
inject their own key through `license.Options.PublicKey`, which the gateway
never sets.

Verification happens in `pkg/license`:

- signature, format, edition and schedule are checked once at start-up;
- expiry is checked on every request, so a key that runs out while the
  gateway is up moves into its transition period without a restart (see
  [Renewal, expiry and transition](#renewal-expiry-and-transition));
- a key issued more than a day in the future is refused (clock skew guard).

## Applying a key to a deployment

Set one of these on the gateway and restart it:

| Variable | Meaning |
|---|---|
| `MAVERICKS_LICENSE_KEY` | The token itself |
| `MAVERICKS_LICENSE_FILE` | Path to a file holding the token (the key variable wins when both are set) |

On Kubernetes the gateway deployment reads `MAVERICKS_LICENSE_KEY` from an
optional secret named `mavericks-license` (key `key`); create it with
`kubectl create secret generic mavericks-license --from-literal=key=MVX1...`
and restart the gateway. On the local stack export the variable before
`bash dev.sh`.

The gateway logs the outcome at start-up ("license key accepted", with the
order and schedule; "has EXPIRED"; "could not be verified"; or "no license
key configured"). The console shows it in the account menu of every role
("Enterprise edition") and in detail under **Platform › License** for
platform administrators.

## Renewal, expiry and transition

| When | State | What happens |
|---|---|---|
| More than 30 days before expiry | `active` | Everything the key includes works |
| Within 30 days of expiry | `active`, `renewal_due` | The same, and the account menu and Platform › License say when the key expires |
| Up to 30 days after expiry | `transition` | The paid features keep working as configured: sign-in through single sign-on, SCIM provisioning, branding, inherited deployment settings and tenant AI keys. Their data can be read and exported, and their configuration can be switched off (DELETE, e.g. revoking a SCIM token), but nothing paid can be configured or changed. Audit retention stops deleting events. Every screen of a paid feature says so |
| After that | `expired` | The deployment runs the Community edition until a new key is installed |

No state deletes data. Installing a renewed key (and restarting the gateway)
returns to `active` at once. The transition period is a technical grace for
export and migration. What a customer may do in it is the agreement's
question, and a third-party hosting or reseller business cannot rely on it to
continue a commercial service.

## What a gated feature looks like in code

- **HTTP:** the route is registered in `internal/gateway` through
  `h.requireFeature(license.FeatureX, handler)`, inside the role guard. A
  locked feature answers `403 {"error": "<Feature> requires the <edition>
  edition; this deployment runs the community edition"}`, naming the edition
  the feature belongs to (commercial for `white_label`, enterprise for most).
  During the transition, GET, HEAD and DELETE pass and every other method
  answers `403` "… is read and export only …". A public route that must answer
  on every deployment checks inline and says "no" instead (`/api/sso/discover`
  answers `{"sso": false}`).
- **Two checks:** `Manager.Has(f)` is the strict one (an active key) for
  anything that configures, changes or irreversibly acts on a paid feature;
  `Manager.Usable(f)` also holds during the transition, for behaviour that
  keeps working as configured. Use `Has` unless the transition must keep the
  behaviour working.
- **Background work:** the jobs the gateway runs check the licence on every
  pass: audit retention with `Has`, white-label mail names and deployment
  settings inheritance with `Usable`. Only the gateway loads a licence; the
  gRPC services serve no gated feature.
- **Enforcement is the server's:** `TestPaidRoutesFollowTheLicence`
  (`internal/gateway/license_routes_test.go`) drives every route under the
  paid features' prefixes through the real router as community, transition
  and enterprise. A paid feature with a new route prefix adds it there.
- **Frontend:** `useFeature("x")` or `<FeatureGate feature="x">…</FeatureGate>`
  from `web/src/license/`, which renders a short lock note naming the needed
  edition when the feature is off, and a "read and export only until …" note
  above the feature during the transition. The console only explains; the
  gateway refuses.
- **Source location:** enterprise and commercial implementation lives under
  [`ee/`](../ee/README.md) (Go) and `web/src/ee/` (frontend). It is compiled
  into every build; the key decides whether it runs.

## Issuing keys (licensor only)

`cmd/license` is the vendor tool. Customers never run it. Issue a key only
for an accepted order. The order and agreement references are required.

```bash
# once: generate the signing key pair; append the public key to
# trustedPublicKeys in pkg/license/pubkey.go and release
go run ./cmd/license keygen -out ~/.mavericks/license-signing.key

# which schedules this build can issue under (default: the latest)
go run ./cmd/license schedules

# per accepted order
go run ./cmd/license sign -key ~/.mavericks/license-signing.key \
  -edition enterprise -customer "Acme Corp" -contact ops@acme.com \
  -order ORD-2026-014 -agreement "PFA 2026-10" -deployment acme-prod \
  -expires 2027-12-31 -out acme.license

# check any token against the compiled-in public keys
go run ./cmd/license inspect -file acme.license
```

The private key must never enter a repository (`.gitignore` excludes
`*license-signing*`). Keep it in a password manager or a hardware token with
an offline backup.

**Rotating the signing key:** generate a new pair, append its public key to
`trustedPublicKeys` and release. Sign new licences with the new key only once
that release is what customers run. Remove the old public key only after
every licence signed with it has expired, transition included. A leaked
private key means the same steps, then removing the old key from the list
early and reissuing every key it signed.

## Behaviour summary

| Situation | Edition in force | Reported as |
|---|---|---|
| No key configured | Community | `state: community` |
| Valid key | The key's edition | `state: active` (`renewal_due` in its last 30 days) |
| Key expired less than 30 days ago | The key's edition, read and export only | `state: transition`, with `transition_ends_at` |
| Key expired longer ago | Community | `state: expired`, with the key's customer and dates |
| Key unreadable, tampered, wrong signer, unknown edition or schedule, not yet valid | Community | `state: invalid`, with the reason |

## What ships with a release

Every image carries the licences that apply to what is in it, at `/licenses/`:

| Image | Contents of `/licenses/` |
|---|---|
| Go services (`deploy/docker/Dockerfile`) | `LICENSE`, `ee-LICENSE`, and `THIRD_PARTY_NOTICES.txt`: Go's licence and the licence and NOTICE texts of every module linked into that service, then grpc-health-probe's |
| Console (`web/Dockerfile`) | Served at `/licenses/`: `LICENSE.txt`, `ee-LICENSE.txt`, and `THIRD_PARTY_NOTICES.txt` for every production npm package, plus Tailwind, whose stylesheet is bundled |
| `minio` | `minio/`: MinIO's AGPLv3 `LICENSE`, `CREDITS`, `NOTICE`, `SOURCE.txt`, the build recipe and the complete corresponding source (tagged source plus vendored modules) |
| `pg-backup` | `mc/`: the same for the MinIO client |
| `postgres-walg` | `mc/` as above; `wal-g/`: its Apache-2.0 licence and the licence files of every module it vendors |

- **Notices are generated, not hand-kept.** `cmd/third-party-notices` (Go)
  and `web/scripts/third-party-notices.mjs` (npm) write them during the image
  build. They fail the build when a dependency's licence is missing or not on
  the reviewed list (MIT, Apache-2.0, BSD, ISC, MPL-2.0, …), so a dependency
  with new obligations stops a release rather than shipping unnoticed. The Go
  check also runs in `go test ./...`.
- **AGPL components ship with their source.** MinIO's server and client are
  built unmodified from their tagged source, and their images carry that
  source. The published image is then complete by itself, whether or not
  upstream keeps its tags. The engine talks to them over the network and links
  only the Apache-2.0 client SDK (`minio-go`).
- **SBOMs:** CI publishes every image with an SPDX SBOM attestation
  (`docker buildx imagetools inspect <image> --format '{{ json .SBOM }}'`).
- Our licences never replace a dependency's. The notices sit beside them and
  say so.

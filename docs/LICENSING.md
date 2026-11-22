# Editions and licensing

> **Last verified:** 2026-11-21 (getting and applying a key: run end to end for a Commercial and an Enterprise key)

maverickbuilds.app ships as one code base in three editions. The edition a
deployment runs is decided by a signed license key the gateway reads at
start-up; nothing calls home, and a missing, invalid or expired key never
stops the platform — it runs the Community Edition and says why.

A key is a technical credential that implements a contract. The rights to use
the paid editions come from the accepted order and agreement the key names,
not from possessing a key, and a key does not widen them.

## Editions

| Edition | License | What it adds |
|---|---|---|
| Community | [Sustainable Use License](../LICENSE): internal use only. Your company (or you, for personal use) runs and modifies it for its own operations, and may share the reports and exports it produces with anyone; nobody outside the company may sign in to it, and building on it for clients, hosting it for others or reselling it needs a Commercial License | The whole product except the gated features below |
| Commercial | Commercial License (a contract with the licensor), which switches on its gated features under the [Enterprise Features License](../ee/LICENSE) | The right to build solutions and services for clients; white-labelling |
| Enterprise | Commercial terms, which switch on its gated features under the [Enterprise Features License](../ee/LICENSE) | Single sign-on brokering and SCIM, per-cell change history, audit export, usage analytics, tenant-level AI keys, deployment-wide settings, white-labelling, and support |

`LICENSE` and `ee/LICENSE` are version 1.1 (October 2026) and are the
licences in force. Version 1.1 replaced version 1.0 (September 2026) on
11 October 2026 without withdrawing rights granted under it. The gated
features' code under `ee/` is in every build and image; without a key it runs
with those features switched off, which the Enterprise Features License permits.
The licensor owns the code and the maverickbuilds.app and Mavericka marks
(`LICENSE` section 7), and takes outside contributions only under the
contributor terms in [CONTRIBUTING.md](../CONTRIBUTING.md#contributor-terms).
A paid order states what its customer may do; this page only describes what
the software does.

## Getting a Commercial or Enterprise key

The Commercial and Enterprise Editions are switched on by a licence key that
Mavericka issues for an accepted order. The steps are the same for both:

1. **Ask.** Write to [team@maverickans.com](mailto:team@maverickans.com)
   with the edition you want and:
   - your company's legal name and address;
   - what you will use it for: your own company's work, building or running
     solutions for named clients, or hosting it for others;
   - the deployments it is for, with a name for each (for example
     `acme-prod`, `acme-staging`, `acme-dr`);
   - when it should start and end.
2. **Accept the order.** Mavericka sends an order form with an order number
   (`ORD-YYYY-NNN`), the agreement it refers to (`PFA YYYY-MM`), the edition,
   the feature schedule, the deployments and the term. Sign it, or accept it
   in the way the order says. No key is issued before the order is accepted.
3. **Receive the key.** It is one line of text starting `MVX1.`, sent to the
   contact named in the order by the channel the order names, with its
   licence ID. Keep it as you keep a password: anyone who holds it can run
   that edition until it expires.
4. **Install it** on each deployment the order names: paste it under
   **Platform › License** in the console, or set it in the server's
   configuration — see [Applying a key to a deployment](#applying-a-key-to-a-deployment).

A key is checked offline, inside the gateway: nothing is sent to Mavericka,
and the platform needs no internet access to accept it.

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
contracts. Keys carry no numeric limits: what a *tenant* may use is a question of
its edition, enforced per tenant — see
[`docs/EDITIONS_AND_SIGNUP.md`](EDITIONS_AND_SIGNUP.md). (Keys issued before
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
  gateway is up stops unlocking anything at that moment, without a restart
  (see [Renewal and expiry](#renewal-and-expiry));
- a key issued more than a day in the future is refused (clock skew guard).

## Applying a key to a deployment

**In the console** (the simplest way): a platform administrator opens
**Platform › License**, pastes the key into **License key** and presses
**Install key**. The gateway checks it on the spot — signature, edition and
dates — and refuses a key that is not genuine or has already expired, saying
why. A good key is in force at once, with no restart: on every gateway of
the deployment within a minute, and at every later start. It is kept in the
platform's database, encrypted when `SECRETS_ENCRYPTION_KEY` is set. **Remove
key** on the same page takes it away; installing another key replaces it.

**In the server's configuration**, for whoever runs the servers. A key set
this way wins over one installed in the console, and the License page then
says where it is set instead of offering the field:

| Installation | Steps |
|---|---|
| Docker Compose ([self-hosting guide](SELF_HOSTING.md)) | Put `MAVERICKS_LICENSE_KEY=MVX1...` in `.env` next to `docker-compose.yml`, then run `docker compose up -d`. Compose restarts the gateway with the key. |
| Kubernetes | `kubectl create secret generic mavericks-license --from-literal=key=MVX1...` in the platform's namespace (the gateway reads its `key` as `MAVERICKS_LICENSE_KEY`), then `kubectl rollout restart deployment/gateway`. |
| A file instead | `MAVERICKS_LICENSE_FILE=/path/to/file` holding the key. `MAVERICKS_LICENSE_KEY` wins when both are set. |
| Local development stack | Export `MAVERICKS_LICENSE_KEY` before `bash dev.sh`. |

**Check that it took.** Three places say the same:

- the gateway's log at start-up: `license key accepted`, with the edition,
  customer, order and schedule. "has EXPIRED", "could not be verified" or "no
  license key configured" mean the deployment runs the Community Edition, and
  why;
- **Platform › License** in the console (platform administrators): the
  edition, customer, order, agreement, deployment, schedule, licence ID and
  expiry date, and which features the key unlocks;
- the account menu of every person: "Commercial Edition" or "Enterprise
  Edition".

**What changes once the key is in.**

- The key's features work for the whole deployment at once: white-labelling
  with either edition; with Enterprise also single sign-on, SCIM, per-cell
  history, audit export, usage analytics, tenant AI keys and deployment-wide
  settings.
- **Tenant editions.** A deployment's tenants run its edition or one below
  it, never above: without a key, every tenant runs the Community Edition and
  **Applications › New tenant** offers no choice. With a key, a new tenant
  starts on the key's edition (Commercial or Enterprise), and the list beside
  its name offers the editions below as well. A tenant created before the
  key keeps its edition until a platform administrator moves it under
  **Applications ›** the tenant **› Change edition**, which is there only
  while a key is in force. A tenant's edition
  bounds only what that tenant may use (the limits under **Platform ›
  Editions**, none by default on a self-hosted deployment); the features
  come from the key.
- **Your own logo and name.** With either key, each tenant's administrator
  sets the console's logo, product name, tagline, favicon and colour under
  **Tenant admin › Branding** (a platform administrator: **Platform ›
  Branding**, after choosing the tenant). The logo replaces the
  maverickbuilds.app mark at the top of the sidebar for everyone in that
  tenant, and, with a custom host, on its sign-in page. Without a key the
  screen says which edition it needs.

**Changing the key later.**

- *Renewal:* from 30 days before the expiry date, Platform › License and
  the account menu say when the key expires. The renewal order brings a new
  key: paste it on the License page (it replaces the old one at once), or
  replace it in the configuration and restart the gateway. The old key needs
  no removal elsewhere; it simply expires.
- *From Commercial to Enterprise* (or more deployments, or extra features):
  a new or amended order, then a new key, installed the same way. Existing
  tenants keep their edition until moved under Change edition.
- *Another server:* the same key works on any deployment the order covers;
  install it there the same way.
- *Removing a key* installed in the console (Platform › License › Remove
  key): the deployment runs the Community Edition at once, nothing the
  paid features stored is deleted, and every tenant moves to the Community
  Edition. Installing a lower key moves the tenants above it down to it the
  same way; a higher key moves nobody up.

## Renewal and expiry

| When | State | What happens |
|---|---|---|
| More than 30 days before expiry | `active` | Everything the key includes works |
| Within 30 days of expiry | `active`, `renewal_due` | The same, and the account menu and Platform › License say when the key expires |
| From the day after the expiry date | `expired` | The deployment runs the Community Edition at once, and so does every tenant: the paid features stop, and within a minute the gateway moves every tenant above the Community Edition to it (with a database per tenant, each tenant's own database at the gateway's next start). Until a new key is installed |

There is no transition period after expiry (owner, 2026-11-22; keys used to
keep their features read-only for 30 days). No state deletes data: what the
paid features stored stays where it is, and comes back with a new key.
Installing a renewed key returns to `active` at once — on the License page
with no restart, or in the configuration with one. Tenants moved to the
Community Edition stay there until moved under Change edition.

## What a gated feature looks like in code

- **HTTP:** the route is registered in `internal/gateway` through
  `h.requireFeature(license.FeatureX, handler)`, inside the role guard. A
  locked feature answers `403 {"error": "<Feature> requires the <edition>
  edition; this deployment runs the community edition"}`, naming the edition
  the feature belongs to (commercial for `white_label`, enterprise for most).
  An expired key answers the same way, on every method. A public route that must answer
  on every deployment checks inline and says "no" instead (`/api/sso/discover`
  answers `{"sso": false}`).
- **One check:** `Manager.Has(f)` (an active key that includes `f`), or
  `Manager.Require(f)` for the refusal. `Usable`, which also held during the
  transition period, went with it on 2026-11-22.
- **Background work:** the jobs the gateway runs check the licence on every
  pass with `Has`: audit retention, white-label mail names and deployment
  settings inheritance. Only the gateway loads a licence.
- **Enforcement is the server's:** `TestPaidRoutesFollowTheLicence`
  (`internal/gateway/license_routes_test.go`) drives every route under the
  enterprise features' prefixes through the real router as community, an
  expired key and enterprise. An enterprise feature with a new route prefix adds it there.
- **Frontend:** `useFeature("x")` or `<FeatureGate feature="x">…</FeatureGate>`
  from `web/src/license/`, which renders a short lock note naming the needed
  edition when the feature is off (an expired key included). The console only
  explains; the gateway refuses.
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
every licence signed with it has expired. A leaked
private key means the same steps, then removing the old key from the list
early and reissuing every key it signed.

## Behaviour summary

| Situation | Edition in force | Reported as |
|---|---|---|
| No key configured | Community | `state: community` |
| Valid key | The key's edition | `state: active` (`renewal_due` in its last 30 days) |
| Key expired | Community, every tenant too | `state: expired`, with the key's customer and dates |
| Key unreadable, tampered, wrong signer, unknown edition or schedule, not yet valid | Community | `state: invalid`, with the reason |

## What ships with a release

Every image carries the licences that apply to what is in it, at `/licenses/`:

| Image | Contents of `/licenses/` |
|---|---|
| Go services (`deploy/docker/Dockerfile`) | `LICENSE`, `ee-LICENSE`, and `THIRD_PARTY_NOTICES.txt`: Go's licence and the licence and NOTICE texts of every module linked into that service |
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

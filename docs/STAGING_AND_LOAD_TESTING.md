# Staging and load testing

> **Classification:** Current — The staging environment, the load harness, and the measured baseline before the first hundreds of concurrent users.

> **Last verified:** 2026-09-17

## Why

Cell writes recalculate every dependent metric synchronously, inside the
request (`POST /api/cells` → `calculation.Scheduler.RecalcAffected`). That
is what makes a planning grid consistent the moment a number changes, and it
is also the first thing that will give under many concurrent planners. Before
anyone quotes a number of users, the platform needs two things it did not
have: an environment that runs exactly what production runs but can be
hammered and discarded, and a tool that measures the write path the way real
use does — rather than a guess from reading the scheduler.

## Staging

`deploy/k8s/overlays/staging.example/` is the overlay: production's shape in
namespace `mavericks-staging` on the same cluster — same images, mTLS between
services, Keycloak on Postgres, network policies — with one replica of
everything, its own hostnames, a smaller database volume, WAL-G and dumps
under their own prefixes (never production's point-in-time chain), and
`SIGNUP_ENABLED: "true"` so the sign-up funnel can be tried end to end. Its
README walks through DNS, the hand-created secrets (staging holds nothing
worth sealing), Keycloak's database, the provisioning service account, the
first administrator (`scripts/bootstrap-platform-admin.sh`, new — the
deployment guide had no step for how the first person comes to exist) and
SMTP.

The hosted service's own staging overlay lives in the private deployment
repository next to `prod/`, generated from it; the namespace, secrets and
accounts were brought up on 2026-09-17. Two notes from doing that:

- The namespace's network policies admit traffic to Keycloak only from the
  ingress and the gateway, so the setup scripts run **through the public
  ingress** — before DNS exists, with the hostnames pinned to the load
  balancer and the placeholder certificate accepted (`CURL_OPTS="--resolve
  auth-staging.example.com:443:<lb ip> -k"`, which both scripts honour).
- Staging runs the images the private CI last published. A change reaches
  staging only after `workflow_dispatch` with `publish_images` — the same
  build that moves `:latest`, which production also names. Pin production to
  the `:<git sha>` tag CI publishes alongside, as `prod.example` says, so a
  staging publish can never surprise a production pod restart.
  Note that the `deploy` input of that dispatch defaults to `true`, and the
  deploy job rolls **production**: to publish for staging only, untick
  `deploy`.

## The harness

`cmd/loadtest` drives a deployment over HTTP like the console does: N
virtual users, each in a loop of *write one cell at a random leaf
intersection of one grid* (`POST /api/cells`) or *read the grid back*
(`GET /api/grid`), in a configurable ratio, for a fixed duration after a
ramp. It reports requests, errors, throughput and p50/p90/p95/p99/max
latency per operation, optionally as JSON, and exits non-zero when a
`-target-p95` is missed or any request failed.

```bash
# dev stack: sign up a fresh test-workspace tenant and load its starter model
go run ./cmd/loadtest -base http://localhost:8080 -signup -users 100 -duration 60s -write-ratio 0.2

# staging or production: a real account; -seed creates an application with
# the starter model in the account's tenant (tenant_admin + developer)
go run ./cmd/loadtest -base https://staging.example.com \
  -keycloak https://auth-staging.example.com -username loadtest@example.com -password … \
  -seed -users 100 -duration 2m -target-p95 2s -json staging.json
# before DNS/certificates: -resolve host=ip (twice) -insecure
```

The account is an ordinary tenant_admin + developer, made once with
`scripts/bootstrap-platform-admin.sh` (`ROLES=tenant_admin,developer
CUSTOMER_ID=<tenant>`); it prints the password once and stores it nowhere, so
keep it in a password manager. The model under load is discovered from that
account (`/api/demo` → grids, metrics, dimensions); `-grid`, `-metric`,
`-app`, `-model`, `-revision` pin it. On the dev stack `-signup` exercises the real registration path, so a
run also proves the funnel; note that sign-up is rate-limited per address
(three attempts, then one every five minutes), so a fourth `-signup` from one
machine within the window is refused — reuse the account with `-dev-user`.

## Baseline (2026-09-17)

The starter model (`internal/starter`: 2 dimensions of 2 levels, 3 metrics
of which 1 calculated, 16 input cells) on a 2024 laptop running the whole dev
stack — gateway, Postgres in Docker, the harness itself — so absolute numbers
are conservative; the shape is what matters.

| Profile | Op | Requests | Errors | rps | p50 | p95 | p99 | max |
|---|---|---|---|---|---|---|---|---|
| 20 users, 50 % writes, 20 s | GET /api/grid | 866 | 0 | 37.7 | 31 ms | 73 ms | 112 ms | 136 ms |
| | POST /api/cells | 875 | 0 | 38.0 | 400 ms | 1.05 s | 1.27 s | 1.41 s |
| 100 users, 50 % writes, 60 s | GET /api/grid | 5 366 | 0 | 76.7 | 406 ms | 568 ms | 697 ms | 951 ms |
| | POST /api/cells | 5 303 | 0 | 75.8 | 815 ms | 1.26 s | 1.44 s | 1.62 s |
| 100 users, 20 % writes, 60 s | GET /api/grid | 6 194 | 0 | 88.5 | 689 ms | 954 ms | 1.09 s | 1.33 s |
| | POST /api/cells | 1 579 | 0 | 22.6 | 1.49 s | 2.19 s | 2.49 s | 2.92 s |
| 200 users, 20 % writes, 60 s | GET /api/grid | 12 857 | 0 | 171.4 | 715 ms | 936 ms | 1.06 s | 1.33 s |
| | POST /api/cells | 3 273 | 0 | 43.6 | 1.64 s | 2.29 s | 2.52 s | 2.76 s |

What the numbers say:

- **No errors at any level**, up to 200 concurrent users on one model. The
  gateway and Postgres pool hold; nothing times out at the ingress limits
  production sets (300 s).
- **Reads stay interactive** — a grid read is tens of milliseconds alone and
  under a second at p95 with a hundred or two hundred people on the same
  model. A full grid read is the heavier of the two operations per request
  (it assembles every intersection with rollups), so the 20 %-write profiles,
  with more concurrent readers, show slower reads than the 50 % ones.
- **The write path is the cost, and it is a per-model cost.** A cell write
  recalculates its dependants before answering and writes on one model
  serialise on the same rows: p95 goes from about 1 s at 20 users to 1.3 s
  at 100 (50 % writes) and 2.2–2.3 s at 100–200 users when most of the
  others are reading the same grid. Writes and reads compete for the same
  gateway CPU and the same rows, which is why the mix matters as much as the
  count.
- Because the contention is **per model**, hundreds of users spread over
  many tenants and models — the sign-up funnel's shape — cost far less than
  hundreds on one model; the numbers above are the worst case, everyone in
  one grid. The measurements were taken on an otherwise idle machine; the
  same profiles with a compiler running alongside were two to four times
  slower, which is worth remembering when comparing runs.

## Staging (2026-09-17)

The same harness against the staging namespace — one gateway replica with
the base manifest's limits (500m CPU, 512Mi), PgBouncer in front of a
single Postgres, mTLS between services, and every request crossing the
internet and the ingress from a laptop — after `-seed` had created the
starter model in the load-test tenant:

| Profile | Op | Requests | Errors | rps | p50 | p95 | p99 | max |
|---|---|---|---|---|---|---|---|---|
| 50 users, 20 % writes, 60 s | GET /api/grid | 3 276 | 0 | 46.8 | 606 ms | 786 ms | 851 ms | 964 ms |
| | POST /api/cells | 832 | 0 | 11.9 | 1.56 s | 2.18 s | 2.46 s | 2.76 s |
| 100 users, 20 % writes, 60 s | GET /api/grid | 2 812 | 0 | 40.2 | 1.40 s | 1.74 s | 1.85 s | 65.6 s |
| | POST /api/cells | 686 | 0 | 9.8 | 3.47 s | 4.98 s | 5.55 s | 6.33 s |

Still no errors, but the cluster is slower than the laptop and saturates
earlier: throughput stops growing between 50 and 100 users (about 50
requests a second in total), which is a single CPU-limited gateway pod doing
grid assembly and recalculation for everyone. That is the number to plan
against for one busy model on one replica; a second gateway replica and a
higher CPU limit are the first, cheapest changes, ahead of any work on the
write path. The one 65-second read at 100 users was a single stalled
connection (p99 was 1.85 s), the kind of outlier to watch for at the
ingress's keep-alive settings rather than in the engine.

### After the release (2026-09-18)

The same profile against staging once it ran the release that adds plans,
quotas and sign-up — every mutating request now passes the plan guard, and
creations ask the enforcer:

| Op | Requests | Errors | rps | p50 | p95 | p99 |
|---|---|---|---|---|---|---|
| GET /api/grid | 6 022 | 0 | 44.6 | 1.40 s | 1.81 s | 2.01 s |
| POST /api/cells | 1 457 | 0 | 10.8 | 3.27 s | 4.86 s | 5.64 s |

Within noise of the numbers above (1.74 s / 4.98 s), so the guard costs
nothing measurable: its per-tenant state is cached for a minute and the
limit counts only run for limits a plan actually sets.

## What to do with them

The measured ceiling is the write path, and the levers are known:

1. **Asynchronous recalculation** for models above a size or concurrency
   threshold — accept the cell write, recalculate in the scheduler, and let
   the grid show "recalculating" — which turns the write's cost into
   background work and is the single largest lever.
2. **Batching** writes within a short window per model, so ten cell edits
   in a second trigger one recalculation instead of ten.
3. **A second gateway replica** helps reads (CPU-bound JSON and pool use)
   but not writes on one model, which serialise in Postgres either way.

None of these is needed to open sign-up: at 20–200 users per model the
platform answers within a second or two on a laptop. Rerun the harness against staging after each change to the write
path and keep the JSON reports next to the release notes.

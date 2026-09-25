# Development and Validation

> **Classification:** Current — How to build, run and validate the repository.

> **Last verified:** 2026-07-15

## Toolchain

| Tool | Version/source |
|---|---|
| Go | `go 1.26.3` in `go.mod` |
| Node.js | 24 in CI |
| npm dependencies | `web/package-lock.json` |
| PostgreSQL | 16 in the Docker development stack |
| Buf | 1.69.0 in CI |
| sqlc | v1.31.1, pinned in the `Makefile` (`SQLC_VERSION`) and CI |
| ogen | v1.20.3, pinned in the `Makefile` (`OGEN_VERSION`) and CI; other versions produce drift |

## Local workflow

```bash
make dev-up
bash dev.sh
make demo      # in a second terminal, once the gateway answers
```

Then:

```bash
cd web
npm ci
VITE_DEV_MODE=true npm run dev
```

To run a second console against a gateway on another port — for a real-auth
check next to the dev one — point the `/api` proxy at it:
`API_PROXY=http://localhost:8090 VITE_DEV_MODE=false npm run dev -- --port 3000`.

The infrastructure, gateway, and frontend are separate processes. `make
dev-up` does not start application binaries.

Useful targets:

```bash
make dev-ps
make dev-logs
make obs-up
make obs-down
make dev-down
```

## Demo seeds

| Command | Scenario | Writes through |
|---|---|---|
| `go run ./cmd/seed-regional-planning` | Regional expense planning, the current reference demo | the gateway (HTTP) |
| `go run ./cmd/seed-sales-planning` | Sales planning with time-series functions | the gateway (HTTP) |
| `go run ./cmd/seed-simple-budget` | Minimal budget-vs-actual model created and verified | the gateway (HTTP) |
| `go run ./cmd/seed-sandbox` | Sandbox model | the gateway (HTTP) |

Every seed needs a running gateway in dev mode and drives it over HTTP as the
roles a real application has, per the repository rule (`CLAUDE.md`); start
new demos from `cmd/seed-regional-planning`. Read a seed's source before
assuming it is idempotent.

## Go checks

```bash
go build ./...
go test ./...
go test -race -coverprofile=coverage.out ./...
golangci-lint run ./...
```

Gateway tests that use Testcontainers require a working Docker daemon. Package
tests with local SQL fixtures do not all require an external `DATABASE_URL`.

The object-storage tests run MinIO from an image built out of
`deploy/docker/minio` (`internal/testobjectstore`), because no registry serves
MinIO to anonymous pulls any more. The first run on a machine compiles MinIO
from source — about four minutes — and later runs reuse the kept image. Build
it on its own first, as CI does, so that time does not count against another
package's test timeout:

```bash
go test -timeout 20m ./internal/testobjectstore/
```

## Frontend checks

```bash
cd web
npm ci
npx eslint src/
npx tsc --noEmit
npm run e2e
npm run build
```

Playwright starts Vite in `VITE_DEV_MODE=true`. Most scenarios mock browser API
responses; use a separately running gateway and real database when validating
end-to-end business semantics.

## Code generation

Run all generators:

```bash
bash scripts/codegen.sh
```

Or separately:

```bash
make proto   # buf generate
make oas     # ogen at the pinned OGEN_VERSION
make sqlc    # sqlc at the pinned SQLC_VERSION
make gen     # all three
```

Run the pinned versions through `make`: a different ogen or sqlc produces
different output, which CI rejects as drift.

Generated locations:

| Source | Output |
|---|---|
| `proto/` + `buf.gen.yaml` | `gen/go/` |
| migrations + `internal/{audit,tenant}/queries` | `internal/{audit,tenant}/db` |
| `api/openapi.yaml` | `internal/gateway/oas` |

After generation, review `git diff`. CI rejects drift in sqlc and ogen output.

## Configuration

Common environment variables come from `pkg/config.BaseConfig`:

- `DATABASE_URL`
- `GRPC_PORT`
- `METRICS_PORT`
- `LOG_LEVEL`
- `NATS_URL`
- `REDIS_URL`

Gateway-specific variables include `HTTP_PORT`,
`OTEL_EXPORTER_OTLP_ENDPOINT`, `SERVICE_VERSION`, and `DEV_MODE`. With
`DEV_MODE=false` the gateway needs `KEYCLOAK_URL`, `KEYCLOAK_REALM` and
`KEYCLOAK_ISSUER` (the issuer the tokens carry, when it differs from the URL
the gateway fetches keys from) and exits if it cannot build its JWKS
validator. Tenant databases, MinIO, the licence, SMTP, sign-up and the legal
documents have their own variables; the complete list, with defaults, is the
`mavericks-config` ConfigMap in `deploy/k8s/base/configmap.yaml` and the
Compose `.env` described in [SELF_HOSTING.md](SELF_HOSTING.md).

Frontend variables include:

- `VITE_DEV_MODE`
- `VITE_KEYCLOAK_URL`
- `VITE_KEYCLOAK_REALM`
- `VITE_KEYCLOAK_CLIENT_ID`

The HTTP AI assistant can use a per-user key or a platform fallback:

- `OPENAI_API_KEY`
- `ANTHROPIC_API_KEY`
- `MISTRAL_API_KEY`
- `DEEPSEEK_API_KEY`
- `SECRETS_ENCRYPTION_KEY` (the older name `AI_KEY_ENCRYPTION_SECRET` still works)

Per-user and per-tenant keys are AES-256-GCM encrypted only when
`SECRETS_ENCRYPTION_KEY` is set (`internal/secretbox`); without it, the
store retains a legacy plaintext path. Treat that variable as mandatory
outside disposable local development, together with `INTEGRATION_CRED_KEY`,
which seals connector credentials and has no plaintext fallback at all
(docs/HETZNER_DEPLOYMENT.md, step 6). The separate legacy gRPC assistant
uses `ANTHROPIC_API_KEY`.

## Migrations are frozen once released

`pkg/migrate` matches an already-applied migration by filename **and**
checksum. Editing a released file therefore does not "update" anything: on
the next start-up every database that already ran it refuses to come up
with `migration NNN_x.sql checksum mismatch`, all of them at once. Renaming
one is the mirror image — the runner sees an unapplied migration and runs it
again, against a schema that already has it.

So a change to the schema, or even to a `COMMENT`, goes in a **new**
migration. `migrations/checksums.txt` records what has been released and
`go test ./migrations/` enforces it; a new migration appends a line:

```bash
UPDATE_MIGRATION_LOCK=1 go test ./migrations/
```

Regenerate the lock for an *existing* migration only when it has
demonstrably never been applied anywhere, including staging and a
colleague's dev database. Name migrations `NNN_lower_snake_case.sql`: the
number orders them, and two files may not share one.

Never commit real credentials. The Kubernetes secret manifest contains
development placeholders and must be replaced for any deployed environment.

## Change-specific validation

- API schema change: run ogen and its drift check.
- SQL query/schema change: run migrations, sqlc, and affected package tests.
- Protobuf change: run `buf lint`, `buf generate`, and Go tests.
- Access or workflow change: run gateway integration tests with real
  PostgreSQL, including negative authorization cases.
- UI behavior change: run ESLint, TypeScript, focused Playwright scenarios, and
  the production build.
- Documentation change: run `git diff --check`, verify local links, and compare
  commands with CI/Makefile rather than older planning documents.

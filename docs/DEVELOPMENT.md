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
| sqlc | 1.31.1 in CI |
| ogen | installed from latest in CI; generated output is drift-checked |

## Local workflow

```bash
make dev-up
go run ./cmd/seed
bash dev.sh
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

| Command | Scenario |
|---|---|
| `go run ./cmd/seed` | Base OPEX planning demo |
| `go run ./cmd/seed-budget` | Budget planning demo |
| `go run ./cmd/seed-sales` | Sales Tracker CRUD/workflow demo |
| `go run ./cmd/seed-procurement` | Procurement demo |
| `go run ./cmd/seed-payroll` | Salary budgeting, scoped rollups, Excel import, and approval copy |
| `go run ./cmd/seed-simple-budget` | Minimal budget-vs-actual model created and verified through the HTTP API (needs a running gateway) |

Seed programs apply migrations and write deterministic configuration/data. Read
the seed source before assuming every seed is destructive or fully idempotent;
the salary-budgeting seed explicitly supports reset/re-run behavior.

## Go checks

```bash
go build ./...
go test ./...
go test -race -coverprofile=coverage.out ./...
golangci-lint run ./...
```

Gateway tests that use Testcontainers require a working Docker daemon. Package
tests with local SQL fixtures do not all require an external `DATABASE_URL`.

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
buf generate
sqlc generate
ogen --target internal/gateway/oas --package oas --clean api/openapi.yaml
```

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
`OTEL_EXPORTER_OTLP_ENDPOINT`, `SERVICE_VERSION`, and `DEV_MODE`.

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
- `AI_KEY_ENCRYPTION_SECRET`

Per-user keys are AES-256-GCM encrypted only when
`AI_KEY_ENCRYPTION_SECRET` is set; without it, the store retains a legacy
plaintext path. Treat that variable as mandatory outside disposable local
development. The separate legacy gRPC assistant uses `ANTHROPIC_API_KEY`.

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

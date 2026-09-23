# maverickbuilds.app

> **Classification:** Current — Product overview of what the platform does today.

maverickbuilds.app is a Go and React platform for building governed business
applications. The current product combines multidimensional planning grids,
calculated metrics, forms, imports, dashboards, workflows, automations,
role-scoped access, revision management, audit history, and an AI-assisted
developer workspace.

This repository is the source of truth for the running implementation. The
current architecture is described in [ARCHITECTURE.md](ARCHITECTURE.md), and
the status of every project document is indexed in
[docs/README.md](docs/README.md).

## Current runtime

The primary local runtime is:

```text
React/Vite web app :5173
        |
        | /api via Vite proxy
        v
Go HTTP gateway :8080
        |
        v
PostgreSQL :5432
```

The gateway owns the currently exercised browser API and calls domain packages
in-process. The repository also contains independently buildable gRPC services
and Kubernetes manifests for a distributed topology. See
[Runtime topology](ARCHITECTURE.md#runtime-topology) before assuming that local
development starts all service binaries.

## Prerequisites

- Go 1.26.3, matching `go.mod`
- Node.js 24, matching CI
- Docker with Compose v2
- npm
- optional code-generation tools: Buf, sqlc, and ogen

## Quick start

Start PostgreSQL and the supporting local infrastructure:

```bash
make dev-up
```

Seed one demo:

```bash
go run ./cmd/seed
# or
go run ./cmd/seed-budget
go run ./cmd/seed-sales
go run ./cmd/seed-procurement
go run ./cmd/seed-payroll
```

Start the API in development-persona mode:

```bash
bash dev.sh
```

In a second terminal, start the web application:

```bash
cd web
npm ci
VITE_DEV_MODE=true npm run dev
```

Open <http://localhost:5173>. The frontend proxies `/api` to
`http://localhost:8080`. Use the persona switcher in the shell footer to change
roles in development mode.

`make dev-up` starts infrastructure only: PostgreSQL, PgBouncer, NATS,
Redis, Keycloak, and MinIO. It does not start the Go gateway, web application,
or the separate gRPC services.

## Verification

The CI-equivalent checks are:

```bash
go test -race -coverprofile=coverage.out ./...
go build ./...
golangci-lint run ./...
buf lint
sqlc generate
ogen --target internal/gateway/oas --package oas --clean api/openapi.yaml

cd web
npm ci
npx eslint src/
npx tsc --noEmit
npm run e2e
npm run build
```

The sqlc and ogen steps are drift checks: generated files must remain unchanged
after generation.

## Repository map

| Path | Purpose |
|---|---|
| `cmd/` | HTTP gateway, gRPC services, diagnostics, and demo seed binaries |
| `internal/gateway/` | Browser-facing HTTP API and current composition root |
| `internal/` | Domain packages for model, formula, calculation, query, workflow, import, identity, policy, AI, deployment, and supporting services |
| `pkg/` | Shared configuration, database, auth, gRPC, logging, migration, and telemetry helpers |
| `migrations/` | Ordered PostgreSQL schema history embedded into Go binaries |
| `proto/` and `gen/go/` | Protobuf contracts and generated gRPC code |
| `api/openapi.yaml` | Partial OpenAPI contract for the stabilized core HTTP surface |
| `web/` | React 19, TypeScript, Vite, React Query, Recharts, and Playwright application |
| `deploy/docker/` | Local infrastructure and optional observability stack |
| `deploy/k8s/` | Base and overlay manifests for the distributed service topology |
| `examples/` | Demo documentation and model references |
| `docs/` | Current documentation index and engineering guides |

## Editions and licensing

maverickbuilds.app is one code base in three editions. The Community edition is
licensed under the [Sustainable Use License](LICENSE) (use and modify it for
your own internal business; do not host or resell it for others). The
Commercial edition adds the right to build for clients, and the Enterprise
edition adds gated features such as single sign-on, audit export and per-cell
history, whose source lives under [`ee/`](ee/README.md) under the
[Enterprise License](ee/LICENSE). A signed, offline license key unlocks the
paid editions; see [docs/LICENSING.md](docs/LICENSING.md).

## Main documentation

- [Architecture](ARCHITECTURE.md)
- [Documentation index and status](docs/README.md)
- [Development and validation](docs/DEVELOPMENT.md)
- [HTTP API guide](docs/API.md)
- [Editions and licensing](docs/LICENSING.md)
- [Plans and self-service sign-up](docs/PLANS_AND_SIGNUP.md)
- [Terms of service and privacy notice](docs/LEGAL_AND_PRIVACY.md)
- [The product's look](docs/BRAND.md)
- [Staging and load testing](docs/STAGING_AND_LOAD_TESTING.md)
- [A database per tenant](docs/TENANT_DATABASES.md)
- [Notifications and reminders](docs/NOTIFICATIONS.md)
- [Web application](web/README.md)
- [UI design system](web/src/ui/DESIGN_SYSTEM.md)
- [Salary budgeting demo](examples/budgeting-demo/README.md)

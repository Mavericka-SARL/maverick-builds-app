# Mavericks Web Application

> **Last verified:** 2026-07-15

The `web` package is the role-based maverickbuilds.app frontend. It is not the
original Vite starter: it implements business, business-admin, developer,
tenant-admin, and platform-admin product surfaces against the Go gateway.

## Stack

- React 19
- TypeScript 6
- Vite 8
- React Router 7
- TanStack React Query 5
- Recharts 3
- Lucide React
- Tailwind 4 plus the Mavericks CSS-token design system
- Keycloak JS for the intended production OIDC flow
- Playwright for browser tests

Exact versions are pinned in `package.json` and `package-lock.json`.

## Entrypoint and composition

`src/main.tsx` mounts:

```text
BrowserRouter
  -> QueryClientProvider
    -> AuthProvider
      -> RoleRouter
        -> one role console
```

Role priority is:

```text
platform_admin
tenant_admin
developer
business_admin
business_user
```

Both admin roles currently render `PlatformAdminConsole`, with server-side
scope and operation checks determining what each may do.

`BrowserRouter` is only a wrapper today: there are no `Routes`, `Route`, or
navigation links. Each console owns local tab state, so views are not
deep-linkable and tab changes do not participate in browser history.
`src/App.tsx` is an unused duplicate composition; `src/main.tsx` is the runtime
authority.

## Main surfaces

| Path | Surface |
|---|---|
| `src/consoles/business/BusinessConsole.tsx` | Dashboards, planning grids, forms, task inbox, history, and runtime actions |
| `src/consoles/business-admin/BusinessAdminConsole.tsx` | Business roles, members, dashboard assignment, and access rules |
| `src/consoles/developer/DeveloperConsole.tsx` | Model/revision builder, dimensions, metrics, grids, forms, dashboards, workflows, automations, integrations, users, and AI entrypoint |
| `src/consoles/developer/AIAssistant.tsx` | Model-aware chat, provider settings, documents, proposals, isolated draft revisions, promotion/discard |
| `src/consoles/platform-admin/PlatformAdminConsole.tsx` | Tenant hierarchy, applications, models, revisions, users, grants, model transfer, and audit |
| `src/consoles/dashboard/` | Chart editor, client chart calculation, and Recharts rendering |
| `src/ui/` | Shared shell, controls, tables, panels, state components, and CSS tokens |

The Developer Console has no general schema-migration tab. Definition changes
can auto-migrate, and API client/AI tools expose migration operations. The API
client also has notification and server-chart methods without corresponding
current UI consumers.

## API state

`src/api/client.ts` is the shared API facade. It sends:

- `X-Dev-User` from `localStorage.dev_persona`;
- `X-App-Id` from `localStorage.selected_app_id`; and
- JSON content headers where applicable.

React Query owns request caching and invalidation. Vite proxies `/api` to
`http://localhost:8080` in development.

Application selection is stored in `localStorage`; consoles currently use
ad-hoc picker/reload flows, while the exported shared `AppPicker` is unused.
Some manually composed React Query keys omit application/revision context, so
new queries must include every server-side scope explicitly.

Production authentication is not complete. `AuthProvider` obtains a Keycloak
token, but the API client, SSE helper, and document upload helper do not send an
`Authorization` header. The gateway also does not initialize its JWKS validator.
Do not treat the production OIDC path as operational until both sides are wired
and covered by an end-to-end test.

## Charts and calculations

Dashboard chart configuration lives in `dashboard_widget.widget_props.chart`.
The runtime `ChartWidget` currently calls `/api/grid` and uses
`src/consoles/dashboard/chartCalc.ts` in the browser. The API client also
contains `getChartData` for a server-side chart resolver, but no current
component calls it.

This means formula/rollup behavior has three relevant paths:

1. the Go formula/calculation packages;
2. Business Console grid evaluation; and
3. dashboard `chartCalc.ts` evaluation.

Changes to formulas, hierarchy, or access filtering must be checked across all
three until chart and grid calculation share one resolver.

The Go engine has a larger function set than either browser evaluator. In
particular, server-supported `IFNA`, `COUNTA`, `TEXTJOIN`, `TEXT`, `SUBSTITUTE`,
and date functions can currently render as zero in grids/charts. The chart path
also lacks parts of the Planning Grid's cross-dimension/hierarchy resolution.

## Development

From `web/`:

```bash
npm ci
VITE_DEV_MODE=true npm run dev
```

The gateway must be available separately on port 8080 for live API behavior.

Available scripts:

```bash
npm run dev
npm run lint
npm run build
npm run preview
npm run e2e
npm run e2e:ui
```

CI uses the more focused commands `npx eslint src/` and `npx tsc --noEmit`.

## Tests

Playwright configuration is in `playwright.config.ts`; tests live under `e2e/`:

- `console-smoke.spec.ts` checks role-console navigation and principal surfaces;
- `ui-audit.tmp.spec.ts` uses a broad mocked API to check UI structure;
- `ux-gates.spec.ts` enforces shared-shell, responsive, accessibility, and
  design-system constraints.

The Playwright web server starts Vite with `VITE_DEV_MODE=true`. These tests do
not prove the real Keycloak flow, PostgreSQL behavior, or all gateway business
semantics.

The real sign-in is covered separately by `e2e-keycloak/`
(`playwright.keycloak.config.ts`): a genuine Keycloak PKCE login against a
gateway running with `DEV_MODE=false`, then an authenticated `/api/me`. CI
runs it (job "Playwright e2e (real Keycloak login)") on its own Postgres and
Keycloak from `deploy/docker/docker-compose.ci-keycloak.yml`, ports 15432
and 18180. Locally, beside a running dev stack and console:

```bash
docker compose -f ../deploy/docker/docker-compose.ci-keycloak.yml up -d --wait
docker compose -f ../deploy/docker/docker-compose.ci-keycloak.yml exec -T keycloak sh -c \
  '/opt/keycloak/bin/kcadm.sh config credentials --server http://localhost:8080 --realm master --user admin --password admin && /opt/keycloak/bin/kcadm.sh update realms/master -s sslRequired=NONE'
CI=1 E2E_WEB_PORT=3000 E2E_GATEWAY_PORT=8090 \
  DATABASE_URL='postgres://mavericks:mavericks@localhost:15432/mavericks?sslmode=disable' \
  KEYCLOAK_URL=http://localhost:18180 VITE_KEYCLOAK_URL=http://localhost:18180 \
  npx playwright test --config=playwright.keycloak.config.ts
```

`E2E_WEB_PORT` is 5173 or 3000 — the only redirect URIs the dev realm's
client allows; `CI=1` makes the config start its own gateway and console
rather than reusing yours.

## Design system

Use exports from `src/ui/index.ts` and the tokens/classes in
`src/ui/design-system.css`. Read [DESIGN_SYSTEM.md](src/ui/DESIGN_SYSTEM.md)
before adding a console-local control, layout, status color, or dialog.

Keep runtime filters immediate, but stage configuration edits with explicit
Save/Cancel where the surface already follows that contract. Never introduce
native `confirm()`; use the shared confirmation flow.

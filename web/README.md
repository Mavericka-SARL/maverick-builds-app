# Mavericks Web Application

> **Classification:** Current — The console as built.

> **Last verified:** 2026-09-25

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
- Keycloak JS for the production OIDC flow (PKCE)
- Playwright for browser tests

Exact versions are pinned in `package.json` and `package-lock.json`.

## Entrypoint and composition

`src/main.tsx` mounts:

```text
BrowserRouter
  -> QueryClientProvider
    -> AuthProvider
      -> RoleRouter
        -> UnifiedConsole
```

There is **one console**. Each role contributes a section — one or more
sidebar groups and the screens behind them — and a user with several roles
sees the union in one sidebar, never a second console or a switcher. The
mapping lives in `src/router/sections.ts` (React-free, so it can be tested on
its own; section hooks live in component-free files for the react-refresh
lint rule) and is rendered by `src/router/UnifiedConsole.tsx`. Where two
roles offer the same screens, the wider one wins: business_admin over
business_user, platform_admin over tenant_admin, and the developer's Users tab
is hidden once an admin section provides it. Server-side scope checks still
decide what each role may do.

`BrowserRouter` is only a wrapper today: there are no `Routes`, `Route`, or
navigation links, so views are not deep-linkable and section changes do not
participate in browser history. `src/App.tsx` is an unused duplicate
composition; `src/main.tsx` is the runtime authority.

## Main surfaces

| Path | Surface |
|---|---|
| `src/router/UnifiedConsole.tsx`, `src/router/sections.ts` | The one console and which sections each role adds |
| `src/consoles/business/BusinessConsole.tsx` | Dashboards (whose widgets include grids, charts, KPIs, forms, imports, automation buttons and images), task inbox, history, and runtime actions |
| `src/consoles/business-admin/BusinessAdminConsole.tsx` | Business roles, members, dashboard assignment, and access rules |
| `src/consoles/developer/DeveloperConsole.tsx` | Model/revision builder, dimensions, metrics, grids, forms, dashboards, workflows, automations, integrations, users, and AI entrypoint |
| `src/consoles/developer/AIAssistant.tsx` | Model-aware chat, provider settings, documents, proposals, isolated draft revisions, promotion/discard |
| `src/consoles/platform-admin/PlatformAdminConsole.tsx` | Tenant hierarchy, applications, models, revisions, users, grants, model transfer, audit, and per-tenant settings (SSO/SCIM, branding, AI keys, e-mail delivery, retention, usage) |
| `src/consoles/dashboard/` | Chart editor and Recharts rendering of server-resolved series |
| `src/ui/` | Shared shell, controls, tables, panels, state components, and CSS tokens |

The Developer Console has no general schema-migration tab. Definition changes
can auto-migrate, and API client/AI tools expose migration operations.
`NotificationCenter` sits in the console header for every role.

## API state

`src/api/client.ts` is the shared API facade. It sends:

- `Authorization: Bearer <token>` from the Keycloak singleton on every
  request — REST, the AI stream and document uploads — refreshed in the
  background by `AuthProvider`;
- `X-Dev-User` from `localStorage.dev_persona` in dev mode;
- `X-App-Id` from `localStorage.selected_app_id`; and
- JSON content headers where applicable.

React Query owns request caching and invalidation. Vite proxies `/api` to
`http://localhost:8080` in development.

Application selection is stored in `localStorage`; consoles currently use
ad-hoc picker/reload flows, while the exported shared `AppPicker` is unused.
Some manually composed React Query keys omit application/revision context, so
new queries must include every server-side scope explicitly.

## Charts and calculations

Dashboard chart configuration lives in `dashboard_widget.widget_props.chart`.
`ChartWidget` calls `POST /api/dashboard-widgets/{id}/chart-data`, which
resolves and rolls up the series on the server with the viewer's access rules
applied; grids likewise show server-computed calculated values. There is no
formula evaluator in the browser, so a formula the Go engine accepts displays
the same everywhere.

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

CI uses the more focused commands `npx eslint src/` and `npx tsc -b --noEmit`.

## Tests

Playwright configuration is in `playwright.config.ts`; tests live under `e2e/`:

- `console-smoke.spec.ts` checks console navigation and principal surfaces;
- `role-sections.spec.ts` checks which sidebar groups each role combination adds;
- feature specs (audit export, cell history, SSO/SCIM, sign-up, workflows,
  automation schedules, …) run against the broad mocked API in `e2e/mocks.ts`;
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

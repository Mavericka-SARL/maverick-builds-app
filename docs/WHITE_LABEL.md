# White-labelling

> **Classification:** Current — A tenant's own name, logo, colours and domain on the console, and its name on outbound mail.

> **Last verified:** 2026-09-16

A commercial and enterprise feature (`white_label`). **Admin › Branding** lets
a tenant admin set:

| Setting | Where it shows |
|---|---|
| product name | the sidebar heading, the browser tab, the sign-in page, and every place the console names the product (integration wizard copy included) |
| tagline | the sign-in page |
| logo, favicon | the sidebar and the sign-in page; the browser tab. Stored inline as data URLs — logo up to 256 KB, favicon up to 32 KB, PNG/JPEG/SVG/WebP/GIF/ICO — so a brand arrives in one request and needs no object store |
| brand colour | the six `--color-brand-*` tokens the design system uses: the colour itself, two darker steps for hover and active, three tints for backgrounds |
| e-mail sender name | notifications arrive as `"Acme Planning" <relay address>` with "Notification from Acme Planning" as the fallback subject |
| custom domain | visitors of that host see this brand before they sign in |

## How the brand reaches the console

`GET /api/branding` is public. A signed-in caller gets their tenant's brand.
An anonymous request gets the brand of the tenant whose custom domain
matches the request's host (`X-Forwarded-Host` first, then `Host`), or the
platform's own look. The console reads it once before anything renders and
again once it knows who is signed in, and applies it to the document —
title, favicon, colour tokens — and to the shell. Applying a brand works on
every edition; only editing one is gated, so a lapsed licence keeps the
console usable and simply stops serving the brand.

## The custom domain

Registering `planning.acme.com` here only says whose brand that host
carries. Pointing the host at this platform — DNS, a certificate, the
ingress rule — is the operator's work, exactly as for the platform's own
host. Two tenants cannot claim the same host.

## What it does not cover

The Keycloak login page keeps the realm's display name and theme; per-tenant
login themes are Keycloak configuration, outside this platform. Tenants
with single sign-on rarely see that page.

## Where the code lives

- `ee/branding` — the row, validation (sizes, types, colour, host), the
  host index `platform.branding_domain`, and `EmailName` for the mailer.
- `internal/gateway/branding.go` — `GET /api/branding` (public) and
  `GET/PUT/DELETE /api/admin/branding` behind `requireFeature(license.FeatureWhiteLabel)`.
- `internal/notification/dispatch.go` — `Dispatcher.BrandName`, `BrandedMailer`.
- `web/src/branding/brand.ts` (tokens, `applyBrand`, `useBrand`) and
  `web/src/branding/BrandingProvider.tsx` (reads the brand before and after
  sign-in) — not under `ee/`, since every edition must apply what the server
  reports; `web/src/ee/branding/BrandingTab.tsx` — the editor.
- `migrations/084_branding.sql`.

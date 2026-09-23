# The product's look

> **Classification:** Current — The identity of the pages a visitor sees before they have an account, and how a deployment overrides it.

> **Last verified:** 2026-09-18

The console is a working surface: light, dense, neutral. The pages in front of
it — `/signup`, the sign-in chooser, the account-refused notice, and Keycloak's
own login — are the product's front door and carry its identity instead:

| | |
|---|---|
| Ground | `#0a0a0a`, with a wide, very faint warm radial from the upper left |
| Surface | `#141414` fields and notes, `#262626` lines |
| Accent | `#eeb900`, used for one primary action per screen and for links |
| Text | `#ffffff`, `#b4b4b4` secondary, `#7c7c7c` quiet |
| Type | Avenir where installed, then the closest geometric system faces; headlines 800 weight, `-0.02em` tracking |

No webfont is fetched: the first paint of a sign-up page should not wait on a
font server, and a self-hosted deployment should not depend on one.

## Where it lives

- **The console's public pages** — `web/src/public/public.css`, one class
  namespace (`.mvx-public`) so none of it can reach the console, and
  `web/src/public/PublicPage.tsx`, the frame they share.
- **Keycloak's login** — `deploy/docker/config/keycloak/themes/maverickbuilds/`,
  a CSS-only theme that inherits every template and message from the stock
  login theme. The same file is embedded in the `keycloak-theme` ConfigMap in
  `deploy/k8s/base/infra/keycloak.yaml`, because a ConfigMap cannot reference a
  directory; `TestKeycloakThemeMatchesTheManifest` fails if the two drift.

A realm uses the theme when its `loginTheme` is `maverickbuilds`. The realm
import sets that for a fresh install; an existing realm is switched in the
admin console, or with

```bash
curl -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  "$KEYCLOAK_URL/admin/realms/mavericks" \
  -d '{"realm":"mavericks","loginTheme":"maverickbuilds","displayName":"maverickbuilds.app"}'
```

The realm's `displayName` is what the login page prints above the form, so it
should read as the product's name.

## A deployment's own brand

Nothing here is a company's logo, and no image ships with the code: an
open-source deployment must not wear someone else's mark. A deployment that
licenses white-labelling sets its name, tagline, logo and colour under
**Admin › Branding** (`docs/WHITE_LABEL.md`), and the public pages use it —
the logo replaces the wordmark, the product name replaces "maverickbuilds.app".
Keycloak's page is separate: point its realm at a theme of your own, or
override `--mvx-accent` in a copy of `brand.css`.

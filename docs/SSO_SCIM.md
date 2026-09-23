# Single sign-on and SCIM provisioning

> **Classification:** Current — How an enterprise tenant signs in through its own identity provider and provisions its users.

> **Last verified:** 2026-09-16

Both are enterprise features (`sso`, `scim` in `pkg/license/features.go`)
and both are per tenant — one row per tenant since migration 091, in a
shared database as much as in a dedicated one — so one tenant's provider
and tokens never touch another's. There is no deployment-wide provider or
token: a person signed in or provisioned has to land in one tenant.

## Single sign-on

**How it works.** The platform's Keycloak realm stays the only issuer the
gateway trusts. A tenant's provider (OIDC or SAML) is registered as a
Keycloak *identity provider* under the alias `mvx-<customer id>`, hidden
from the shared login page, and reached through `kc_idp_hint`. The
provider's secret is sent to Keycloak and stored nowhere else.

**Setting it up — the tenant admin, under Admin › Single sign-on:**

1. Create an application for this platform at your provider. Register the
   **redirect URI** (OIDC) or **assertion consumer service URL** (SAML) the
   tab shows: `https://<auth host>/realms/<realm>/broker/mvx-<id>/endpoint`.
   SAML providers also need the service-provider entity id shown there.
2. Paste the provider's **discovery document** (OIDC:
   `…/.well-known/openid-configuration`) or **metadata document** (SAML)
   URL; for OIDC also the client id and secret. **Test provider document**
   reads it through Keycloak and reports the issuer or entity id.
3. Enter the **allowed e-mail domains**. A provider can assert any address;
   the list is what keeps it to its own people. Two tenants cannot claim the
   same domain.
4. Choose whether accounts are **created on first sign-in** and with which
   role, then switch it on.

**Signing in.** On a deployment where any tenant has a provider, the console
shows a sign-in page first. A person types their work address;
`GET /api/sso/discover?email=` (public, rate-limited) answers with the
provider alias when the domain is registered and enabled, and the console
sends them there. Everyone else goes to the usual login. On a deployment
with no provider at all the page never appears.

**First login.** The token that comes back is a normal realm token whose
subject the platform has never seen. The gateway asks Keycloak which
identity provider authenticated the person (`federated-identity`), reads the
tenant from the alias, and — if the tenant allows it and the address is in
an allowed domain — creates the `identity.user` row with the default role in
the tenant's first workspace, and the directory entry that routes their
requests. A refused first login answers 403 with the reason, which the
console shows.

**What needs to exist on the deployment.** The gateway's Keycloak service
account (`mavericks-admin`) must hold `view-identity-providers` and
`manage-identity-providers` on `realm-management`, in addition to the user
roles it already has. `scripts/keycloak-provisioning-setup.sh` grants all of
them. `KEYCLOAK_ISSUER` (or `KEYCLOAK_URL`) and `KEYCLOAK_REALM` are what the
tab uses to show the broker endpoint.

## SCIM provisioning

**What it is.** A SCIM 2.0 endpoint at `<console origin>/api/scim/v2`
(under `/api` so the ingress routes it to the gateway). Entra ID, Okta and
any RFC 7644 client can create, update, deactivate and delete this tenant's
users, and keep its groups in step with business roles.

**Tokens.** The tenant admin issues tokens under Admin › Provisioning
(SCIM). A token is `mvx_scim_<tenant>_<secret>`: the tenant part routes the
request to the right database before the hash is checked; the secret is the
only part that authenticates. The plaintext is shown once; the database
keeps a SHA-256. A token carries the platform role a provisioned user gets
and the workspace groups are created in. Revoking is immediate.

**What the endpoint does:**

| SCIM | Platform |
|---|---|
| `POST /Users` | exactly what the console's Users screen does: the Keycloak account (adopting one that exists), `identity.user`, the default role, the directory entry — and an invitation mail only when the tenant has no SSO |
| `active: false` (PATCH or PUT) | `identity.user.disabled_at` set and the Keycloak account disabled: sign-in stops at once, every actor resolver refuses the user, rows and history stay |
| `DELETE /Users/{id}` | the console's delete: the row, the directory entry, the Keycloak account |
| `POST /Groups`, members | a business role in the token's workspace and its members |
| `GET /Users?filter=userName eq "…"` | equality filters on userName, externalId, id, emails.value, displayName, active, joined by `and` — what Entra and Okta send |

`ServiceProviderConfig`, `ResourceTypes` and `Schemas` are served for
clients that read them.

**Entra ID.** Enterprise application → Provisioning → Automatic; Tenant URL
= the base URL from the tab, Secret Token = the token. Entra patches
`active` as the string `"False"` and sometimes without a path; both are
handled. **Okta.** SCIM 2.0 app, base URL and token as above; Okta uses PUT
for updates.

## Where the code lives

- `ee/sso` — settings, the domain index (`platform.sso_domain`), Keycloak
  registration, first-login provisioning. `ee/scim` — tokens and the
  service. Both under `ee/LICENSE`.
- `internal/gateway/sso.go`, `internal/gateway/scim.go` — the HTTP surface;
  `resolveJWTActor` in `handler.go` calls `jitProvision`.
- `pkg/keycloak/identity_providers.go` — identity providers, federated
  identities, account enable/disable.
- `migrations/080_sso_scim.sql`.
- `web/src/ee/sso/SsoTab.tsx`, `web/src/ee/scim/ScimTab.tsx`, and the
  sign-in page in `web/src/auth/AuthProvider.tsx`.

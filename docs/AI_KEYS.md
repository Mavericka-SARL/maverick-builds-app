# AI provider keys: per user, per tenant

> **Classification:** Current — Where the AI assistant's API key comes from.

> **Last verified:** 2026-09-16

The AI assistant calls a third-party model provider, and somebody's account
pays for it. This describes whose.

## Two places a key can live

| Key | Stored in | Managed by | Editions |
|---|---|---|---|
| **Personal** | `ai_assistant.llm_settings`, one row per user | the developer, in **AI Assistant › Settings** | all |
| **Tenant** | `ai_assistant.tenant_llm_settings`, one row per tenant database | the tenant admin, in **Admin › AI keys** | enterprise (`tenant_ai_keys`) |

A community or commercial deployment has personal keys only: every developer
pastes their own. The routes behind **Admin › AI keys** answer 403 there, and
the tab renders the feature gate naming the edition that would unlock it.

## Which key a call uses

`buildProviderForRequest` picks one, highest first:

1. **An enforced tenant key.** The tenant admin has said this is the only
   account model data may leave through, so a personal key is ignored — not
   merged, not preferred.
2. **The caller's personal key.**
3. **A tenant key that is set but not enforced** — a convenience, so a
   developer who has not pasted a key still has a working assistant.
4. **The provider's environment variable** (`OPENAI_API_KEY`,
   `ANTHROPIC_API_KEY`, `MISTRAL_API_KEY`, `DEEPSEEK_API_KEY`), the
   single-tenant and self-hosted fallback.

A key always brings its own provider and model with it: the key belongs to one
provider's account, so using the tenant key means using the tenant's provider.

## Enforcing

**Admin › AI keys › Use the tenant key for every AI call** is the governance
switch. Until it is on, the tenant key only fills gaps. With it on, no model
data can reach an employee's private provider account.

Enforcing with no key stored is refused — it would take the assistant away
from every developer in the tenant at once. Test the key first: the **Test
connection** button runs the same live tool-calling probe the personal
settings screen uses, because a key that cannot call tools passes a naive
"say ok" check and then fails on the assistant's first real turn.

## How keys are stored

Both tables hold the key AES-256-GCM encrypted under
`AI_KEY_ENCRYPTION_SECRET` (`internal/aiassistant/crypto.go`). Without that
variable set, values are written in plaintext and a previously encrypted value
cannot be read back — set it before storing any key.

Neither key is ever returned by the API. `has_key` says whether one exists;
an empty `api_key` on update keeps the stored one, so a form that never
receives the key can still be saved.

## Where the code lives

- `ee/aikeys` — the tenant key store and resolver. Licensed under `ee/LICENSE`.
- `internal/gateway/tenant_ai_settings.go` — the four admin routes, behind
  `requireFeature(license.FeatureTenantAIKeys, …)`.
- `internal/gateway/ai_handler.go` — `buildProviderForRequest`, the order above.
- `web/src/ee/aikeys/TenantAIKeysTab.tsx` — **Admin › AI keys**.
- `migrations/079_tenant_ai_keys.sql` — the tenant table.

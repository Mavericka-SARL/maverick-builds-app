import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { KeyRound } from "lucide-react";
import { SettingsScopeNotice } from "../../consoles/admin/SettingsScopeNotice";
import { api, type ProviderProbeResult, type TenantAISettings } from "../../api/client";
import { FeatureGate } from "../../license/FeatureGate";
import { Button, Card, Checkbox, Field, InlineAlert, LoadingState, Select, TextInput, useConfirm } from "../../ui";

/** The providers the gateway can build (internal/gateway/ai_handler.go).
 *  Keys must match `buildProvider`, which refuses anything else. */
const PROVIDERS = [
  { value: "openai", label: "OpenAI" },
  { value: "anthropic", label: "Anthropic" },
  { value: "mistral", label: "Mistral" },
  { value: "deepseek", label: "DeepSeek" },
];

const EMPTY: TenantAISettings = { provider: "openai", model: "", has_key: false, enforced: false };

/**
 * Admin › AI keys: one AI provider key for the whole tenant.
 *
 * Without this, every developer pastes their own key and the tenant's AI spend
 * is spread across personal accounts. With it, the tenant admin holds one key
 * and — if they enforce it — it is the only account model data ever leaves
 * through, which is the reason an enterprise buys this rather than a
 * convenience.
 *
 * Enterprise only: the routes behind this screen answer 403 on any other
 * edition, so the gate here explains before the call rather than after.
 */
export function TenantAIKeysTab() {
  return (
    <FeatureGate feature="tenant_ai_keys">
      <TenantAIKeysForm />
    </FeatureGate>
  );
}

function TenantAIKeysForm() {
  const qc = useQueryClient();
  const { confirm, confirmElement } = useConfirm();
  const { data, isLoading, error } = useQuery({ queryKey: ["tenant-ai-settings"], queryFn: api.getTenantAISettings });
  const [draft, setDraft] = useState<TenantAISettings | null>(null);
  const [key, setKey] = useState("");
  const [probe, setProbe] = useState<ProviderProbeResult | null>(null);
  const [saved, setSaved] = useState(false);
  const form = draft ?? data ?? EMPTY;

  const onSettings = (next: TenantAISettings) => {
    qc.setQueryData(["tenant-ai-settings"], next);
    setDraft(null);
    setKey("");
  };

  const save = useMutation({
    mutationFn: () => api.updateTenantAISettings({ ...form, api_key: key || undefined }),
    onSuccess: (next) => {
      onSettings(next);
      setSaved(true);
      setTimeout(() => setSaved(false), 2000);
    },
  });
  const test = useMutation({
    mutationFn: () => api.testTenantAISettings({ provider: form.provider, model: form.model, api_key: key || undefined }),
    onSuccess: setProbe,
  });
  const clear = useMutation({ mutationFn: api.clearTenantAIKey, onSuccess: onSettings });
  // No DELETE here: clearing the key is what "follow the deployment" means
  // for an AI key, since a tenant row without a key inherits the deployment's.
  const inherit = useMutation({ mutationFn: api.clearTenantAIKey, onSuccess: onSettings });

  if (isLoading) return <LoadingState label="Loading tenant AI settings…" />;
  if (error) return <InlineAlert tone="danger">{(error as Error).message}</InlineAlert>;

  const set = (patch: Partial<TenantAISettings>) => {
    setProbe(null);
    setDraft({ ...form, ...patch });
  };
  const canEnforce = form.has_key || key.trim() !== "";

  const removeKey = () =>
    confirm({
      title: "Remove the tenant key?",
      body: "Developers fall back to their own keys. Anyone without one loses the assistant until they add a key.",
      confirmLabel: "Remove key",
      destructive: true,
      onConfirm: () => clear.mutate(),
    });

  return (
    <div className="mvx-admin-stack" data-testid="tenant-ai-settings">
      <SettingsScopeNotice scope={data?.scope} onInherit={() => inherit.mutate()} inheriting={inherit.isPending} />
      <Card>
        <div style={{ fontWeight: 700, marginBottom: 4 }}>Provider</div>
        <p className="mvx-admin-muted" style={{ marginTop: 0 }}>
          The account every AI call in this tenant is billed to. A key belongs to one provider, so changing the provider
          means supplying that provider&apos;s key.
        </p>
        <div style={{ display: "grid", gap: 12, maxWidth: 520 }}>
          <Field label="Provider">
            <Select value={form.provider} onChange={(e) => set({ provider: e.target.value })} aria-label="AI provider">
              {PROVIDERS.map((p) => (
                <option key={p.value} value={p.value}>{p.label}</option>
              ))}
            </Select>
          </Field>
          <Field label="Model" description="Leave blank to use the provider's default">
            <TextInput
              value={form.model}
              onChange={(e) => set({ model: e.target.value })}
              placeholder="provider default"
              aria-label="AI model"
            />
          </Field>
          <Field
            label="API key"
            description={form.has_key ? "A key is stored; leave blank to keep it" : "Stored encrypted, never shown again"}
          >
            <TextInput
              type="password"
              value={key}
              onChange={(e) => { setProbe(null); setKey(e.target.value); }}
              placeholder={form.has_key ? "••••••••" : "sk-…"}
              aria-label="Tenant API key"
            />
          </Field>
        </div>
        <div className="mvx-admin-inline-form" style={{ marginTop: 12 }}>
          <Button loading={test.isPending} loadingLabel="Testing…" onClick={() => test.mutate()} icon={<KeyRound size={14} />}>
            Test connection
          </Button>
          {form.has_key && (
            <Button variant="danger" loading={clear.isPending} loadingLabel="Removing…" onClick={removeKey}>
              Remove key
            </Button>
          )}
        </div>
        {test.isError && <InlineAlert tone="danger">{(test.error as Error).message}</InlineAlert>}
        {probe && (
          <InlineAlert tone={probe.ok ? "success" : "danger"}>
            {probe.ok
              ? `${probe.provider} / ${probe.model}: ${probe.reply}`
              : `${probe.provider} / ${probe.model}: ${probe.error}`}
          </InlineAlert>
        )}
      </Card>

      <Card>
        <div style={{ fontWeight: 700, marginBottom: 4 }}>Who may use their own key</div>
        <p className="mvx-admin-muted" style={{ marginTop: 0 }}>
          By default the tenant key only fills in for developers who have not set one of their own. Enforce it when model
          data must never leave through a personal provider account.
        </p>
        <Checkbox
          checked={form.enforced}
          disabled={!canEnforce}
          onChange={(e) => set({ enforced: e.target.checked })}
          label="Use the tenant key for every AI call and ignore personal keys"
        />
        {!canEnforce && (
          <InlineAlert tone="info">Store a key first — enforcing without one would leave every developer without the assistant.</InlineAlert>
        )}
      </Card>

      <div className="mvx-admin-inline-form">
        <Button variant="primary" loading={save.isPending} loadingLabel="Saving…" onClick={() => save.mutate()}>
          {saved ? "Saved" : "Save settings"}
        </Button>
        {save.isError && <span className="mvx-admin-error">{(save.error as Error).message}</span>}
      </div>
      {confirmElement}
    </div>
  );
}

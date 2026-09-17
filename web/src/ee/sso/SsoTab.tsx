import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ShieldCheck } from "lucide-react";
import { api, type SsoSettings } from "../../api/client";
import { FeatureGate } from "../../license/FeatureGate";
import { Button, Card, Checkbox, Field, InlineAlert, LoadingState, Select, TextInput, Textarea, useConfirm } from "../../ui";

const EMPTY: SsoSettings = {
  protocol: "oidc", display_name: "Company account", metadata_url: "", client_id: "",
  allowed_domains: [], jit_provisioning: true, default_role: "business_user", enabled: false,
};

/**
 * Admin › Single sign-on: the tenant's own identity provider.
 *
 * The tenant admin pastes the provider's discovery or metadata URL (and,
 * for OIDC, the client id and secret they created there), names the e-mail
 * domains it may vouch for, and switches it on. The gateway registers it in
 * Keycloak under an alias for this tenant; the provider's secret goes there
 * and nowhere else. What the admin must register on their side — the
 * redirect / assertion consumer URL and the entity id — is shown here.
 *
 * Enterprise only: the tab renders the feature gate on any other edition.
 */
export function SsoTab() {
  return (
    <FeatureGate feature="sso">
      <SsoForm />
    </FeatureGate>
  );
}

function SsoForm() {
  const qc = useQueryClient();
  const { confirm, confirmElement } = useConfirm();
  const { data, isLoading, error } = useQuery({ queryKey: ["sso-settings"], queryFn: api.getSsoSettings });
  const [draft, setDraft] = useState<SsoSettings | null>(null);
  const [secret, setSecret] = useState("");
  const [domainsText, setDomainsText] = useState<string | null>(null);
  const [probe, setProbe] = useState<{ ok: boolean; details?: Record<string, string>; error?: string } | null>(null);
  const [saved, setSaved] = useState(false);
  const form = draft ?? data ?? EMPTY;
  const domains = domainsText ?? form.allowed_domains.join("\n");

  const onSettings = (next: SsoSettings) => {
    qc.setQueryData(["sso-settings"], next);
    setDraft(null);
    setSecret("");
    setDomainsText(null);
  };
  const save = useMutation({
    mutationFn: () => api.updateSsoSettings({
      ...form,
      allowed_domains: domains.split(/[\n,;\s]+/).map((d) => d.trim()).filter(Boolean),
      client_secret: secret || undefined,
    }),
    onSuccess: (next) => { onSettings(next); setSaved(true); setTimeout(() => setSaved(false), 2000); },
  });
  const test = useMutation({
    mutationFn: () => api.testSsoProvider({ protocol: form.protocol, metadata_url: form.metadata_url }),
    onSuccess: setProbe,
  });
  const remove = useMutation({ mutationFn: api.removeSso, onSuccess: onSettings });

  if (isLoading) return <LoadingState label="Loading single sign-on settings…" />;
  if (error) return <InlineAlert tone="danger">{(error as Error).message}</InlineAlert>;

  const set = (patch: Partial<SsoSettings>) => { setProbe(null); setDraft({ ...form, ...patch }); };
  const removeProvider = () =>
    confirm({
      title: "Remove the identity provider?",
      body: "People stop being able to sign in through it. Accounts it created stay and can use a password reset.",
      confirmLabel: "Remove provider",
      destructive: true,
      onConfirm: () => remove.mutate(),
    });

  return (
    <div className="mvx-admin-stack" data-testid="sso-settings">
      {data?.registry_configured === false && (
        <InlineAlert tone="warning">
          This gateway has no identity-provider registry credentials (<code>KEYCLOAK_ADMIN_CLIENT_ID/SECRET</code>), so a
          provider cannot be registered until an operator sets them.
        </InlineAlert>
      )}
      <Card>
        <div style={{ fontWeight: 700, marginBottom: 4 }}>Identity provider</div>
        <p className="mvx-admin-muted" style={{ marginTop: 0 }}>
          Your people sign in through your own provider; their accounts here are created on first sign-in. The provider&apos;s
          secret is stored in the sign-in registry, never in this platform&apos;s database.
        </p>
        <div style={{ display: "grid", gap: 12, maxWidth: 560 }}>
          <Field label="Protocol">
            <Select value={form.protocol} onChange={(e) => set({ protocol: e.target.value as SsoSettings["protocol"] })} aria-label="SSO protocol">
              <option value="oidc">OpenID Connect (Entra ID, Okta, Google, Keycloak…)</option>
              <option value="saml">SAML 2.0</option>
            </Select>
          </Field>
          <Field label="Name shown on the sign-in page">
            <TextInput value={form.display_name} onChange={(e) => set({ display_name: e.target.value })} aria-label="Provider display name" />
          </Field>
          <Field
            label={form.protocol === "oidc" ? "Discovery document URL" : "Metadata document URL"}
            description={form.protocol === "oidc" ? "Usually ends in /.well-known/openid-configuration" : "The IdP metadata XML your provider publishes"}
          >
            <TextInput value={form.metadata_url} onChange={(e) => set({ metadata_url: e.target.value })} placeholder="https://" aria-label="Provider metadata URL" />
          </Field>
          {form.protocol === "oidc" && (
            <>
              <Field label="Client id" description="The application you registered at your provider for this platform">
                <TextInput value={form.client_id} onChange={(e) => set({ client_id: e.target.value })} aria-label="OIDC client id" />
              </Field>
              <Field label="Client secret" description={form.configured ? "A secret is stored; leave blank to keep it" : "Stored in the sign-in registry only"}>
                <TextInput type="password" value={secret} onChange={(e) => { setProbe(null); setSecret(e.target.value); }}
                  placeholder={form.configured ? "••••••••" : ""} aria-label="OIDC client secret" />
              </Field>
            </>
          )}
        </div>
        <div className="mvx-admin-inline-form" style={{ marginTop: 12 }}>
          <Button loading={test.isPending} loadingLabel="Testing…" onClick={() => test.mutate()} icon={<ShieldCheck size={14} />}>
            Test provider document
          </Button>
        </div>
        {test.isError && <InlineAlert tone="danger">{(test.error as Error).message}</InlineAlert>}
        {probe && (
          <InlineAlert tone={probe.ok ? "success" : "danger"}>
            {probe.ok
              ? <>Document read. {Object.entries(probe.details ?? {}).map(([k, v]) => <span key={k}><b>{k}</b>: {v} </span>)}</>
              : probe.error}
          </InlineAlert>
        )}
      </Card>

      <Card>
        <div style={{ fontWeight: 700, marginBottom: 4 }}>Register this platform at your provider</div>
        <p className="mvx-admin-muted" style={{ marginTop: 0 }}>
          Your provider needs to know where to send people back. Register these values there.
        </p>
        <table className="mvx-table">
          <tbody>
            <tr><td style={{ width: 220, fontWeight: 600 }}>{form.protocol === "oidc" ? "Redirect URI" : "Assertion consumer service URL"}</td><td><code data-testid="broker-endpoint">{data?.broker_endpoint}</code></td></tr>
            {form.protocol === "saml" && <tr><td style={{ fontWeight: 600 }}>Service provider entity id</td><td><code>{data?.sp_entity_id}</code></td></tr>}
          </tbody>
        </table>
      </Card>

      <Card>
        <div style={{ fontWeight: 700, marginBottom: 4 }}>Who may sign in this way</div>
        <div style={{ display: "grid", gap: 12, maxWidth: 560 }}>
          <Field label="Allowed e-mail domains" description="One per line. A provider can assert any address; this keeps it to your own people.">
            <Textarea rows={3} value={domains} onChange={(e) => { setProbe(null); setDomainsText(e.target.value); }} placeholder="example.com" aria-label="Allowed e-mail domains" />
          </Field>
          <Checkbox checked={form.jit_provisioning} onChange={(e) => set({ jit_provisioning: e.target.checked })}
            label="Create the account on first sign-in (otherwise accounts come from SCIM or an invitation)" />
          <Field label="Role a new account gets">
            <Select value={form.default_role} onChange={(e) => set({ default_role: e.target.value })} aria-label="Default role">
              <option value="business_user">business_user</option>
              <option value="business_admin">business_admin</option>
              <option value="developer">developer</option>
              <option value="tenant_admin">tenant_admin</option>
            </Select>
          </Field>
          <Checkbox checked={form.enabled} onChange={(e) => set({ enabled: e.target.checked })} label="Single sign-on is on" />
        </div>
      </Card>

      <div className="mvx-admin-inline-form">
        <Button variant="primary" loading={save.isPending} loadingLabel="Saving…" onClick={() => save.mutate()}>
          {saved ? "Saved" : form.configured ? "Save changes" : "Register provider"}
        </Button>
        {form.configured && (
          <Button variant="danger" loading={remove.isPending} loadingLabel="Removing…" onClick={removeProvider}>Remove provider</Button>
        )}
        {save.isError && <span className="mvx-admin-error">{(save.error as Error).message}</span>}
        {remove.isError && <span className="mvx-admin-error">{(remove.error as Error).message}</span>}
      </div>
      {confirmElement}
    </div>
  );
}

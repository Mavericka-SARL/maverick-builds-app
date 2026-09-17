import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type ApiAuthType, type ApiIntegrationConfig, type IntegrationConnection } from "../../../api/client";
import { Button, Field, InlineAlert, Select, StatusBadge, TextInput } from "../../../ui";

// Credentials are reusable named connections stored server-side; secret
// fields here are WRITE-ONLY. An existing credential renders as
// "Credential configured" with Keep / Replace / Remove — the value itself
// never returns to the browser.
export function AuthStep({
  config, onConfig, connectionId, setConnectionId,
}: {
  config: ApiIntegrationConfig;
  onConfig: (patch: Partial<ApiIntegrationConfig>) => void;
  connectionId: string;
  setConnectionId: (id: string) => void;
}) {
  const qc = useQueryClient();
  const { data: connections = [] } = useQuery({
    queryKey: ["integration-connections"],
    queryFn: () => api.listIntegrationConnections(),
  });
  const selected = (connections as IntegrationConnection[]).find(c => c.id === connectionId);
  const [creating, setCreating] = useState(false);
  const [testResult, setTestResult] = useState<string | null>(null);

  const authType = config.auth.type;
  const patchAuth = (p: Partial<ApiIntegrationConfig["auth"]>) => onConfig({ auth: { ...config.auth, ...p } });

  return (
    <div style={{ display: "grid", gap: 12, maxWidth: 560 }}>
      <Field label="Authentication type">
        <Select value={authType} aria-label="Authentication type"
          onChange={e => {
            const t = e.target.value as ApiAuthType;
            patchAuth({ type: t, header_name: t === "api_key" ? "X-Api-Key" : undefined, query_param: undefined });
            if (t === "none") setConnectionId("");
          }}>
          <option value="none">None</option>
          <option value="api_key">API key (header or query)</option>
          <option value="bearer">Bearer token</option>
          <option value="basic">Basic auth</option>
          <option value="oauth2_client_credentials">OAuth 2.0 client credentials</option>
        </Select>
      </Field>

      {authType === "api_key" && (
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12 }}>
          <Field label="Header name" description="leave empty to use a query parameter">
            <TextInput value={config.auth.header_name ?? ""} aria-label="API key header name"
              onChange={e => patchAuth({ header_name: e.target.value, query_param: e.target.value ? "" : config.auth.query_param })} />
          </Field>
          <Field label="Query parameter">
            <TextInput value={config.auth.query_param ?? ""} aria-label="API key query parameter"
              onChange={e => patchAuth({ query_param: e.target.value, header_name: e.target.value ? "" : config.auth.header_name })} />
          </Field>
        </div>
      )}

      {authType !== "none" && (
        <>
          <Field label="Connection" description="Reusable credential, stored encrypted server-side">
            <div style={{ display: "flex", gap: 8 }}>
              <Select value={connectionId} aria-label="Connection" onChange={e => setConnectionId(e.target.value)} style={{ flex: 1 }}>
                <option value="">Select a connection…</option>
                {(connections as IntegrationConnection[])
                  .filter(c => c.auth_type === authType)
                  .map(c => <option key={c.id} value={c.id}>{c.name}</option>)}
              </Select>
              <Button variant="secondary" size="sm" onClick={() => setCreating(v => !v)} aria-expanded={creating}>
                {creating ? "Cancel" : "New connection"}
              </Button>
            </div>
          </Field>

          {selected && (
            <div className="mvx-admin-object" style={{ padding: 12 }}>
              <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                {selected.has_secret
                  ? <StatusBadge tone="success">Credential configured</StatusBadge>
                  : <StatusBadge tone="warning">No credential stored</StatusBadge>}
                <span className="mvx-admin-muted" style={{ fontSize: 12 }}>{selected.name} · {selected.auth_type}</span>
              </div>
              <ConnectionSecretActions conn={selected} onChanged={() => qc.invalidateQueries({ queryKey: ["integration-connections"] })} />
              <div style={{ marginTop: 8 }}>
                <Button variant="secondary" size="sm" onClick={async () => {
                  const r = await api.testIntegrationConnection(selected.id);
                  setTestResult(r.ok ? "Credential is decryptable and complete." : (r.error ?? "failed"));
                }}>Check credential</Button>
                {testResult && <span aria-live="polite" style={{ marginLeft: 8, fontSize: 12 }}>{testResult}</span>}
              </div>
            </div>
          )}

          {creating && (
            <NewConnectionForm authType={authType} onCreated={id => {
              setConnectionId(id);
              setCreating(false);
              qc.invalidateQueries({ queryKey: ["integration-connections"] });
            }} />
          )}
        </>
      )}
    </div>
  );
}

// Keep / Replace / Remove on an existing connection's secret. Replace opens
// write-only fields; nothing stored is ever displayed.
function ConnectionSecretActions({ conn, onChanged }: { conn: IntegrationConnection; onChanged: () => void }) {
  const [mode, setMode] = useState<"keep" | "replace">("keep");
  const [fields, setFields] = useState<Record<string, string>>({});
  const save = useMutation({
    mutationFn: async (secret: Record<string, string> | null) =>
      api.updateIntegrationConnection(conn.id, { secret }),
    onSuccess: () => { setMode("keep"); setFields({}); onChanged(); },
  });
  return (
    <div style={{ marginTop: 8 }}>
      <div style={{ display: "flex", gap: 6 }}>
        <Button variant={mode === "keep" ? "primary" : "secondary"} size="sm" aria-pressed={mode === "keep"} onClick={() => setMode("keep")}>Keep</Button>
        <Button variant={mode === "replace" ? "primary" : "secondary"} size="sm" aria-pressed={mode === "replace"} onClick={() => setMode("replace")}>Replace</Button>
        <Button variant="danger" size="sm" disabled={!conn.has_secret || save.isPending} onClick={() => save.mutate(null)}>Remove</Button>
      </div>
      {mode === "replace" && (
        <div style={{ display: "grid", gap: 6, marginTop: 8 }}>
          <SecretFields authType={conn.auth_type} fields={fields} setFields={setFields} />
          <Button size="sm" loading={save.isPending} onClick={() => save.mutate(fields)}>Save new credential</Button>
          {save.isError && <InlineAlert tone="danger">{(save.error as Error).message}</InlineAlert>}
        </div>
      )}
    </div>
  );
}

function SecretFields({ authType, fields, setFields }: {
  authType: ApiAuthType; fields: Record<string, string>; setFields: (f: Record<string, string>) => void;
}) {
  const set = (k: string, v: string) => setFields({ ...fields, [k]: v });
  const input = (k: string, label: string, type = "password") => (
    <Field label={label} key={k}>
      <TextInput type={type} value={fields[k] ?? ""} onChange={e => set(k, e.target.value)} aria-label={label} autoComplete="off" />
    </Field>
  );
  switch (authType) {
    case "api_key": return input("value", "API key");
    case "bearer": return input("token", "Bearer token");
    case "basic": return <>{input("username", "Username", "text")}{input("password", "Password")}</>;
    case "oauth2_client_credentials": return <>{input("client_id", "Client ID", "text")}{input("client_secret", "Client secret")}</>;
    default: return null;
  }
}

function NewConnectionForm({ authType, onCreated }: { authType: ApiAuthType; onCreated: (id: string) => void }) {
  const [name, setName] = useState("");
  const [fields, setFields] = useState<Record<string, string>>({});
  const [tokenURL, setTokenURL] = useState("");
  const create = useMutation({
    mutationFn: async () => {
      const meta: Record<string, unknown> = {};
      if (authType === "oauth2_client_credentials") meta.token_url = tokenURL;
      return api.createIntegrationConnection({ name, auth_type: authType, meta, secret: fields });
    },
    onSuccess: c => onCreated(c.id),
  });
  return (
    <div className="mvx-admin-object" style={{ padding: 12, display: "grid", gap: 8 }}>
      <Field label="Connection name" required>
        <TextInput value={name} onChange={e => setName(e.target.value)} aria-label="Connection name" />
      </Field>
      {authType === "oauth2_client_credentials" && (
        <Field label="Token URL" required description="HTTPS; validated like any destination">
          <TextInput value={tokenURL} onChange={e => setTokenURL(e.target.value)} placeholder="https://auth.example.com/oauth/token" aria-label="Token URL" />
        </Field>
      )}
      <SecretFields authType={authType} fields={fields} setFields={setFields} />
      <Button size="sm" loading={create.isPending} disabled={!name} onClick={() => create.mutate()}>Create connection</Button>
      {create.isError && <InlineAlert tone="danger">{(create.error as Error).message}</InlineAlert>}
    </div>
  );
}

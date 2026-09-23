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
  // Back from a provider's consent page: the gateway's callback returned
  // the browser here with the outcome in the query (never the tokens).
  const oauthOutcome = new URLSearchParams(window.location.search);
  const oauthStatus = oauthOutcome.get("oauth");
  const oauthError = oauthOutcome.get("oauth_error");

  return (
    <div style={{ display: "grid", gap: 12, maxWidth: 560 }}>
      {oauthStatus === "connected" && <InlineAlert tone="success">Connected — the provider issued tokens for this connection.</InlineAlert>}
      {oauthStatus === "error" && <InlineAlert tone="danger">The provider did not connect: {oauthError || "unknown error"}</InlineAlert>}
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
          <option value="oauth2_authorization_code">OAuth 2.0 authorization code (Connect)</option>
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
              {selected.auth_type === "oauth2_authorization_code" && (
                <OAuthConnectControls conn={selected} onChanged={() => qc.invalidateQueries({ queryKey: ["integration-connections"] })} />
              )}
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
    // The client id is public (it goes into the browser's consent URL) and
    // lives in meta; the secret is the only sealed field a developer types.
    case "oauth2_authorization_code": return input("client_secret", "Client secret");
    default: return null;
  }
}

/**
 * Connect / Disconnect for an authorization-code connection. Connect asks
 * the gateway where to send the browser, opens the provider's consent
 * page in this tab, and the provider returns the person to the console
 * (?oauth=connected) — the tokens never pass through the browser.
 */
function OAuthConnectControls({ conn, onChanged }: { conn: IntegrationConnection; onChanged: () => void }) {
  const connectedAt = typeof conn.meta.connected_at === "string" ? conn.meta.connected_at : "";
  const connectedBy = typeof conn.meta.connected_by === "string" ? conn.meta.connected_by : "";
  const start = useMutation({
    mutationFn: () => api.startIntegrationOAuth(conn.id, window.location.pathname + window.location.search + window.location.hash),
    onSuccess: ({ authorization_url }) => { window.location.assign(authorization_url); },
  });
  const disconnect = useMutation({ mutationFn: () => api.disconnectIntegrationOAuth(conn.id), onSuccess: onChanged });
  return (
    <div style={{ marginTop: 8, display: "grid", gap: 6 }} data-testid="oauth-connect">
      <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
        {connectedAt
          ? <StatusBadge tone="success">Connected {new Date(connectedAt).toLocaleString()}{connectedBy ? ` by ${connectedBy}` : ""}</StatusBadge>
          : <StatusBadge tone="warning">Not connected</StatusBadge>}
        <Button variant="primary" size="sm" loading={start.isPending} onClick={() => start.mutate()}>
          {connectedAt ? "Connect again" : "Connect"}
        </Button>
        {connectedAt && <Button variant="secondary" size="sm" loading={disconnect.isPending} onClick={() => disconnect.mutate()}>Disconnect</Button>}
      </div>
      <span className="mvx-admin-muted" style={{ fontSize: 12 }}>
        Register this callback with the provider: <code>{window.location.origin}/api/integrations/oauth/callback</code>. The person who
        connects consents on the tenant&apos;s behalf; scheduled runs use the connection&apos;s tokens.
      </span>
      {start.isError && <InlineAlert tone="danger">{(start.error as Error).message}</InlineAlert>}
      {disconnect.isError && <InlineAlert tone="danger">{(disconnect.error as Error).message}</InlineAlert>}
    </div>
  );
}

function NewConnectionForm({ authType, onCreated }: { authType: ApiAuthType; onCreated: (id: string) => void }) {
  const [name, setName] = useState("");
  const [fields, setFields] = useState<Record<string, string>>({});
  const [tokenURL, setTokenURL] = useState("");
  const [authURL, setAuthURL] = useState("");
  const [clientID, setClientID] = useState("");
  const [scope, setScope] = useState("");
  const [tokenClientAuth, setTokenClientAuth] = useState<"basic" | "post">("basic");
  const create = useMutation({
    mutationFn: async () => {
      const meta: Record<string, unknown> = {};
      if (authType === "oauth2_client_credentials") meta.token_url = tokenURL;
      if (authType === "oauth2_authorization_code") {
        Object.assign(meta, { authorization_url: authURL, token_url: tokenURL, client_id: clientID, scope, token_client_auth: tokenClientAuth });
      }
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
      {authType === "oauth2_authorization_code" && (
        <>
          <Field label="Authorization URL" required description="The provider's consent page; provider-specific parameters may stay in its query">
            <TextInput value={authURL} onChange={e => setAuthURL(e.target.value)} placeholder="https://auth.example.com/oauth/authorize" aria-label="Authorization URL" />
          </Field>
          <Field label="Token URL" required>
            <TextInput value={tokenURL} onChange={e => setTokenURL(e.target.value)} placeholder="https://auth.example.com/oauth/token" aria-label="Token URL" />
          </Field>
          <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12 }}>
            <Field label="Client ID" required>
              <TextInput value={clientID} onChange={e => setClientID(e.target.value)} aria-label="Client ID" autoComplete="off" />
            </Field>
            <Field label="Scope" description="space-separated">
              <TextInput value={scope} onChange={e => setScope(e.target.value)} aria-label="Scope" />
            </Field>
          </div>
          <Field label="Client authentication at the token endpoint">
            <Select value={tokenClientAuth} aria-label="Token client authentication" onChange={e => setTokenClientAuth(e.target.value as "basic" | "post")}>
              <option value="basic">HTTP Basic (the default)</option>
              <option value="post">In the request body (client_secret_post)</option>
            </Select>
          </Field>
        </>
      )}
      <SecretFields authType={authType} fields={fields} setFields={setFields} />
      <Button size="sm" loading={create.isPending} disabled={!name} onClick={() => create.mutate()}>Create connection</Button>
      {create.isError && <InlineAlert tone="danger">{(create.error as Error).message}</InlineAlert>}
    </div>
  );
}

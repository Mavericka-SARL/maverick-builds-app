import { useState } from "react";
import { Check } from "lucide-react";
import { Button, Dialog, Field, InlineAlert, TextInput } from "../ui";
import { useConnectorInfo } from "./connectorInfo";

/**
 * How to use your models in ChatGPT and Claude: the read-only chat
 * connector's URL and each host's client ID and secret (GET /api/connector),
 * opened from the account menu so every person finds it, whatever their role.
 * The client ID and secret are the host's, shared by everyone on the
 * deployment; each person still signs in with their own account and reads
 * only grid data their own access allows.
 */
function CopyField({ label, value, secret }: { label: string; value: string; secret?: boolean }) {
  const [shown, setShown] = useState(!secret);
  const [copied, setCopied] = useState(false);
  const copy = () => {
    void navigator.clipboard?.writeText(value).then(() => {
      setCopied(true);
      setTimeout(() => setCopied(false), 1500);
    });
  };
  return (
    <Field label={label}>
      <div style={{ display: "flex", gap: 6 }}>
        <TextInput value={shown ? value : "••••••••••••••••"} readOnly aria-label={label} style={{ flex: 1, fontFamily: "var(--font-mono, monospace)" }} />
        {secret && (
          <Button variant="ghost" onClick={() => setShown(s => !s)} aria-pressed={shown}>
            {shown ? "Hide" : "Show"}
          </Button>
        )}
        <Button variant="ghost" leadingIcon={copied ? <Check size={14} /> : undefined} onClick={copy}>
          {copied ? "Copied" : "Copy"}
        </Button>
      </div>
    </Field>
  );
}

const STEPS: Record<string, string> = {
  Claude: "In Claude: Settings → Connectors → Add custom connector. Enter the URL, then under Advanced settings the client ID and secret below, and connect.",
  ChatGPT: "In ChatGPT: Settings → Apps & Connectors → Advanced → turn on developer mode, then create a connector with the URL and OAuth, entering the client ID and secret below.",
};

export function ChatConnectorDialog({ open, onClose }: { open: boolean; onClose: () => void }) {
  const { data, isLoading, error } = useConnectorInfo();
  return (
    <Dialog open={open} onClose={onClose} title="Use your models in ChatGPT and Claude" width={520}>
      {isLoading && <p style={{ fontSize: 12, margin: 0 }}>Loading…</p>}
      {error && <p className="mvx-admin-error">{(error as Error).message}</p>}
      {data && !data.enabled && (
        <InlineAlert tone="info">The chat connector is not switched on for this deployment.</InlineAlert>
      )}
      {data?.enabled && (
        <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
          <p style={{ fontSize: 12, margin: 0, color: "var(--color-text-muted)" }}>
            Ask ChatGPT or Claude about your grids and get tables, charts and reports in the conversation. You sign in
            with your own maverickbuilds.app account and read only what your access allows; the connection reads grid
            data of each model&apos;s active revision and never changes anything.
          </p>
          <CopyField label="Connector URL" value={data.url ?? ""} />
          {data.hosts.map(host => (
            <div key={host.client_id} style={{ display: "flex", flexDirection: "column", gap: 8 }}>
              <strong style={{ fontSize: 13 }}>{host.name}</strong>
              {STEPS[host.name] && <p style={{ fontSize: 12, margin: 0 }}>{STEPS[host.name]}</p>}
              <CopyField label={`${host.name} client ID`} value={host.client_id} />
              {host.client_secret
                ? <CopyField label={`${host.name} client secret`} value={host.client_secret} secret />
                : <InlineAlert tone="warning">The {host.name} client secret is not configured on this deployment yet.</InlineAlert>}
            </div>
          ))}
        </div>
      )}
    </Dialog>
  );
}

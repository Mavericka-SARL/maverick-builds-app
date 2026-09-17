import { useState } from "react";
import { Plus, Trash2, ArrowUp, ArrowDown } from "lucide-react";
import type { ApiIntegrationConfig, ApiKV } from "../../../api/client";
import { Button, Checkbox, Field, IconButton, NumberInput, Select, TextInput, Textarea } from "../../../ui";
import { TEMPLATE_VARIABLES } from "./apiIntegrationTypes";

// KVEditor: the visual key/value rows with enable/disable, remove, and
// keyboard-reachable reorder (explicit up/down buttons — fully operable
// without a pointer).
function KVEditor({ rows, onChange, label }: { rows: ApiKV[]; onChange: (rows: ApiKV[]) => void; label: string }) {
  const upd = (i: number, patch: Partial<ApiKV>) => {
    const next = rows.slice();
    next[i] = { ...next[i], ...patch };
    onChange(next);
  };
  const move = (i: number, d: number) => {
    const j = i + d;
    if (j < 0 || j >= rows.length) return;
    const next = rows.slice();
    [next[i], next[j]] = [next[j], next[i]];
    onChange(next);
  };
  return (
    <div role="group" aria-label={label} style={{ display: "grid", gap: 6 }}>
      {rows.map((kv, i) => (
        <div key={i} style={{ display: "flex", gap: 6, alignItems: "center" }}>
          <Checkbox checked={kv.enabled} onChange={e => upd(i, { enabled: e.target.checked })} label="" aria-label={`${label} row ${i + 1} enabled`} />
          <TextInput value={kv.key} onChange={e => upd(i, { key: e.target.value })} placeholder="key" aria-label={`${label} row ${i + 1} key`} style={{ flex: 1 }} />
          <TextInput value={kv.value} onChange={e => upd(i, { value: e.target.value })} placeholder="value" aria-label={`${label} row ${i + 1} value`} style={{ flex: 2 }} />
          <IconButton aria-label={`Move ${label} row ${i + 1} up`} onClick={() => move(i, -1)} disabled={i === 0}><ArrowUp size={14} /></IconButton>
          <IconButton aria-label={`Move ${label} row ${i + 1} down`} onClick={() => move(i, 1)} disabled={i === rows.length - 1}><ArrowDown size={14} /></IconButton>
          <IconButton aria-label={`Remove ${label} row ${i + 1}`} onClick={() => onChange(rows.filter((_, x) => x !== i))}><Trash2 size={14} /></IconButton>
        </div>
      ))}
      <Button variant="secondary" size="sm" icon={<Plus size={14} />} onClick={() => onChange([...rows, { key: "", value: "", enabled: true }])}>
        Add {label.toLowerCase()}
      </Button>
    </div>
  );
}

// VariablePicker inserts a template variable into the focused concept via a
// simple append callback — constrained to the documented namespace.
function VariablePicker({ onPick, rowFields }: { onPick: (v: string) => void; rowFields: string[] }) {
  const vars = [...TEMPLATE_VARIABLES, ...rowFields.map(f => `{{row.${f}}}`)];
  return (
    <Select value="" aria-label="Insert variable" onChange={e => { if (e.target.value) onPick(e.target.value); e.target.value = ""; }} style={{ maxWidth: 240 }}>
      <option value="">Insert variable…</option>
      {vars.map(v => <option key={v} value={v}>{v}</option>)}
    </Select>
  );
}

export function RequestStep({ config, onConfig }: {
  config: ApiIntegrationConfig;
  onConfig: (patch: Partial<ApiIntegrationConfig>) => void;
}) {
  const r = config.request;
  const patchReq = (p: Partial<ApiIntegrationConfig["request"]>) => onConfig({ request: { ...r, ...p } });
  const [jsonError, setJsonError] = useState<string | null>(null);
  const rowFields = config.direction === "push" ? config.mapping.fields.map(f => f.source).filter(Boolean) : [];

  return (
    <div style={{ display: "grid", gap: 12 }}>
      <div style={{ display: "flex", gap: 8 }}>
        <Select value={r.method} aria-label="Method" onChange={e => patchReq({ method: e.target.value })} style={{ width: 120 }}>
          {["GET", "POST", "PUT", "PATCH", "DELETE"].map(m => <option key={m}>{m}</option>)}
        </Select>
        <TextInput value={r.url} onChange={e => patchReq({ url: e.target.value })}
          placeholder="https://api.example.com/v1/items" aria-label="Request URL" style={{ flex: 1 }} />
      </div>
      <p className="mvx-admin-muted" style={{ margin: 0, fontSize: 12 }}>
        HTTPS only. Variables are allowed in the path, query and body — never in the scheme or host.
      </p>
      <VariablePicker rowFields={rowFields} onPick={v => patchReq({ url: r.url + v })} />

      <Field label="Query parameters">
        <KVEditor label="Query parameter" rows={r.query ?? []} onChange={query => patchReq({ query })} />
      </Field>
      <Field label="Headers">
        <KVEditor label="Header" rows={r.headers ?? []} onChange={headers => patchReq({ headers })} />
      </Field>

      <Field label="Body">
        <Select value={r.body_mode} aria-label="Body mode" onChange={e => patchReq({ body_mode: e.target.value as ApiIntegrationConfig["request"]["body_mode"] })} style={{ maxWidth: 240 }}>
          <option value="none">None</option>
          <option value="json">JSON</option>
          <option value="form">Form URL-encoded</option>
          <option value="raw">Raw text</option>
        </Select>
      </Field>
      {r.body_mode === "json" && (
        <>
          <Textarea rows={8} value={r.body_json ?? ""} aria-label="JSON body" spellCheck={false}
            onChange={e => {
              patchReq({ body_json: e.target.value });
              try {
                if (e.target.value.trim() !== "") JSON.parse(e.target.value.replace(/\{\{[^}]+\}\}/g, "0"));
                setJsonError(null);
              } catch { setJsonError("Not valid JSON (template variables are substituted before checking)"); }
            }} />
          {jsonError && <p role="alert" style={{ color: "var(--color-danger)", fontSize: 12, margin: 0 }}>{jsonError}</p>}
        </>
      )}
      {r.body_mode === "form" && (
        <KVEditor label="Form field" rows={r.body_form ?? []} onChange={body_form => patchReq({ body_form })} />
      )}
      {r.body_mode === "raw" && (
        <>
          <Textarea rows={6} value={r.body_raw ?? ""} aria-label="Raw body" onChange={e => patchReq({ body_raw: e.target.value })} />
          <Field label="Content type">
            <TextInput value={r.content_type ?? ""} onChange={e => patchReq({ content_type: e.target.value })} placeholder="text/plain" aria-label="Content type" />
          </Field>
        </>
      )}

      <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(160px, 1fr))", gap: 12 }}>
        <Field label="Timeout (s)" description="default 30, max 120">
          <NumberInput value={r.timeout_seconds ?? 0} onChange={e => patchReq({ timeout_seconds: Number(e.target.value) || 0 })} min={0} max={120} aria-label="Timeout seconds" />
        </Field>
        <Field label="Max retries" description="mutations need an idempotency key">
          <NumberInput value={r.max_retries ?? 0} onChange={e => patchReq({ max_retries: Number(e.target.value) || 0 })} min={0} max={5} aria-label="Max retries" />
        </Field>
        <Field label="Rate limit (req/s)">
          <NumberInput value={r.rate_limit_rps ?? 0} onChange={e => patchReq({ rate_limit_rps: Number(e.target.value) || 0 })} min={0} max={100} aria-label="Rate limit" />
        </Field>
        <Field label="Idempotency key header">
          <TextInput value={r.idempotency_key_header ?? ""} onChange={e => patchReq({ idempotency_key_header: e.target.value })} placeholder="Idempotency-Key" aria-label="Idempotency key header" />
        </Field>
      </div>
    </div>
  );
}

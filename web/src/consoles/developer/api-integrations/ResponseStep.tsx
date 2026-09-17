import { useBrand } from "../../../branding/brand";
import { useEffect, useRef, useState } from "react";
import { api, type ApiIntegrationConfig, type ApiRun } from "../../../api/client";
import { Button, Field, InlineAlert, Select, StatusBadge, TextInput, useConfirm } from "../../../ui";
import { inferFields, parsePreviewRecords, type InferredField } from "./integrationMapping";

// "Send test request" ALWAYS executes through the backend worker (202 +
// poll) — the browser never contacts the external API. Mutation methods
// demand an explicit acknowledgement that the test may change external data.
export function ResponseStep({
  config, onConfig, integrationId, tested, onTestOutcome, onFieldsInferred, saveDraft,
}: {
  config: ApiIntegrationConfig;
  onConfig: (patch: Partial<ApiIntegrationConfig>) => void;
  integrationId: string | null;
  tested: boolean;
  onTestOutcome: (ok: boolean) => void;
  onFieldsInferred: (fields: InferredField[], records: unknown[]) => void;
  saveDraft: () => Promise<string | null>;
}) {
  const brand = useBrand();
  const { confirm, confirmElement } = useConfirm();
  const [run, setRun] = useState<ApiRun | null>(null);
  const [testing, setTesting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const pollRef = useRef<number | null>(null);

  useEffect(() => () => { if (pollRef.current) window.clearInterval(pollRef.current); }, []);

  const startTest = async () => {
    setError(null);
    const go = async () => {
      setTesting(true);
      try {
        const id = integrationId ?? await saveDraft();
        if (!id) { setTesting(false); return; }
        const { run_id } = await api.testApiIntegration(id, {
          acknowledgeSideEffects: config.request.method !== "GET",
        });
        pollRef.current = window.setInterval(async () => {
          const r = await api.getApiIntegrationRun(run_id);
          setRun(r);
          if (r.status !== "queued" && r.status !== "running") {
            if (pollRef.current) window.clearInterval(pollRef.current);
            setTesting(false);
            onTestOutcome(r.status === "success");
            if (r.status === "success" && r.meta?.preview_body) {
              const records = parsePreviewRecords(r.meta.preview_body, config.response?.records_path ?? "");
              onFieldsInferred(inferFields(records), records);
            }
          }
        }, 1000);
      } catch (e) {
        setError((e as Error).message);
        setTesting(false);
      }
    };
    if (config.request.method !== "GET") {
      confirm({
        title: "This test may change external data",
        body: `A test sends a real ${config.request.method} request to ${hostOf(config.request.url)}. Continue?`,
        confirmLabel: "Send anyway",
        onConfirm: go,
      });
    } else {
      await go();
    }
  };

  const meta = run?.meta ?? {};
  const records = run?.status === "success" && meta.preview_body
    ? parsePreviewRecords(meta.preview_body, config.response?.records_path ?? "")
    : [];
  const fields = records.length > 0 ? inferFields(records) : [];

  return (
    <div style={{ display: "grid", gap: 12 }}>
      {confirmElement}
      <div style={{ display: "flex", gap: 12, alignItems: "end", flexWrap: "wrap" }}>
        <Field label="Response format">
          <Select value={config.response?.format ?? "json"} aria-label="Response format"
            onChange={e => onConfig({ response: { ...config.response, format: e.target.value as "json" | "csv" } })}>
            <option value="json">JSON</option>
            <option value="csv">CSV</option>
          </Select>
        </Field>
        <Field label="Records path" description="e.g. $.data.items — where the record collection lives">
          <TextInput value={config.response?.records_path ?? ""} aria-label="Records path"
            onChange={e => onConfig({ response: { format: config.response?.format ?? "json", records_path: e.target.value } })}
            placeholder="$.data.items" />
        </Field>
        <Button onClick={startTest} loading={testing} loadingLabel="Testing…" aria-busy={testing}>
          Send test request
        </Button>
        {tested
          ? <StatusBadge tone="success">Tested ✓</StatusBadge>
          : <StatusBadge tone="warning">Not tested for current config</StatusBadge>}
      </div>
      <p className="mvx-admin-muted" style={{ margin: 0, fontSize: 12 }}>
        The request is executed by the {brand.name} backend — never from your browser. Changing the
        request, auth, response or pagination invalidates a previous successful test.
      </p>
      {error && <InlineAlert tone="danger">{error}</InlineAlert>}

      {run && (
        <div className="mvx-admin-object" style={{ padding: 12 }} aria-live="polite">
          <div style={{ display: "flex", gap: 12, flexWrap: "wrap", alignItems: "center" }}>
            <StatusBadge tone={run.status === "success" ? "success" : run.status === "queued" || run.status === "running" ? "brand" : "danger"}>
              {run.status}
            </StatusBadge>
            {meta.preview_status && <span>HTTP {meta.preview_status}</span>}
            <span>{run.duration_ms} ms</span>
            {meta.preview_content_type && <span className="mvx-admin-muted">{meta.preview_content_type}</span>}
            {meta.preview_truncated === "true" && <StatusBadge tone="warning">truncated</StatusBadge>}
            {run.error_code && <StatusBadge tone="danger">{run.error_code}</StatusBadge>}
          </div>
          {run.message && <p style={{ marginTop: 6, fontSize: 12 }}>{run.message}</p>}
          {meta.preview_headers && (
            <details style={{ marginTop: 6 }}>
              <summary style={{ cursor: "pointer", fontSize: 12 }}>Response headers (safe subset)</summary>
              <pre style={{ fontSize: 11, overflowX: "auto" }}>{formatHeaders(meta.preview_headers)}</pre>
            </details>
          )}
          {meta.preview_body && (
            <pre style={{ maxHeight: 260, overflow: "auto", fontSize: 11, background: "var(--color-surface-sunken)", padding: 8, borderRadius: 6 }}>
              {prettyJSON(meta.preview_body)}
            </pre>
          )}
        </div>
      )}

      {records.length > 0 && (
        <div>
          <h4 style={{ margin: "4px 0" }}>Records preview ({Math.min(records.length, 20)} of first page)</h4>
          <div className="mvx-table-wrap" style={{ maxHeight: 220, overflow: "auto" }}>
            <table className="mvx-table">
              <thead><tr>{fields.slice(0, 8).map(f => <th key={f.path}>{f.path}</th>)}</tr></thead>
              <tbody>
                {records.slice(0, 20).map((rec, i) => (
                  <tr key={i}>
                    {fields.slice(0, 8).map(f => <td key={f.path}>{sampleAt(rec, f.path)}</td>)}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <p className="mvx-admin-muted" style={{ fontSize: 12 }}>
            {fields.length} field(s) inferred: {fields.map(f => `${f.path} (${f.types.join("|")})`).slice(0, 6).join(", ")}{fields.length > 6 ? "…" : ""}
          </p>
        </div>
      )}
    </div>
  );
}

function hostOf(url: string): string {
  try { return new URL(url).hostname; } catch { return url; }
}
function prettyJSON(s: string): string {
  try { return JSON.stringify(JSON.parse(s), null, 2).slice(0, 20000); } catch { return s.slice(0, 20000); }
}
function formatHeaders(raw: string): string {
  try {
    const h = JSON.parse(raw) as Record<string, string>;
    return Object.entries(h).map(([k, v]) => `${k}: ${v}`).join("\n");
  } catch { return raw; }
}
function sampleAt(rec: unknown, path: string): string {
  let cur: unknown = rec;
  for (const seg of path.replace(/^\$\.?/, "").split(".")) {
    if (cur === null || typeof cur !== "object") return "";
    const clean = seg.replace(/\[\d+\]/g, "");
    cur = (cur as Record<string, unknown>)[clean];
    const idx = seg.match(/\[(\d+)\]/);
    if (idx && Array.isArray(cur)) cur = cur[Number(idx[1])];
  }
  if (cur === null || cur === undefined || typeof cur === "object") return "";
  return String(cur).slice(0, 60);
}

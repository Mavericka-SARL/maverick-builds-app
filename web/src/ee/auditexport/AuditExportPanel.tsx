import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Download } from "lucide-react";
import { api } from "../../api/client";
import { FeatureGate } from "../../license/FeatureGate";
import { Button, Card, Field, InlineAlert, NumberInput, Select, TextInput } from "../../ui";
import { downloadBlob } from "../../consoles/business/blobUtils";

/**
 * Above the Audit Log table: get the log out, and decide how long it is kept.
 * Export streams the caller's own scope (a tenant admin's tenant, a platform
 * admin's everything) as CSV or JSON Lines, unbound by the table's 200 rows.
 * Retention is 0 (forever) or at least the floor the server reports.
 * Enterprise: other editions see the gate.
 */
export function AuditExportPanel() {
  return (
    <FeatureGate feature="audit_export">
      <Panel />
    </FeatureGate>
  );
}

function Panel() {
  const qc = useQueryClient();
  const { data: settings } = useQuery({ queryKey: ["audit-settings"], queryFn: api.getAuditSettings });
  const [format, setFormat] = useState<"csv" | "jsonl">("csv");
  const [since, setSince] = useState("");
  const [until, setUntil] = useState("");
  const [days, setDays] = useState<number | null>(null);
  const [saved, setSaved] = useState(false);
  const retention = days ?? settings?.retention_days ?? 0;
  const floor = settings?.min_retention_days ?? 30;

  const download = useMutation({
    mutationFn: async () => {
      const { blob, filename } = await api.exportAudit({ format, since: since || undefined, until: until || undefined });
      downloadBlob(blob, filename || `audit.${format}`);
    },
  });
  const save = useMutation({
    mutationFn: () => api.updateAuditSettings({ retention_days: retention }),
    onSuccess: (next) => { qc.setQueryData(["audit-settings"], next); setDays(null); setSaved(true); setTimeout(() => setSaved(false), 2000); },
  });

  return (
    <div className="mvx-admin-stack" data-testid="audit-export" style={{ marginBottom: 16 }}>
      <Card>
        <div style={{ fontWeight: 700, marginBottom: 4 }}>Export</div>
        <p className="mvx-admin-muted" style={{ marginTop: 0 }}>
          Everything in your scope, oldest first — not just the rows below. JSON Lines is what a SIEM collector pulls; it
          ends with a cursor line when there is more, which the collector passes back as <code>after=</code>.
        </p>
        <div style={{ display: "flex", gap: 12, flexWrap: "wrap", alignItems: "flex-end" }}>
          <Field label="Format">
            <Select value={format} onChange={(e) => setFormat(e.target.value as "csv" | "jsonl")} aria-label="Export format">
              <option value="csv">CSV</option>
              <option value="jsonl">JSON Lines</option>
            </Select>
          </Field>
          <Field label="From">
            <TextInput type="date" value={since} onChange={(e) => setSince(e.target.value)} aria-label="Export from" />
          </Field>
          <Field label="To">
            <TextInput type="date" value={until} onChange={(e) => setUntil(e.target.value)} aria-label="Export to" />
          </Field>
          <Button variant="primary" icon={<Download size={14} />} loading={download.isPending} loadingLabel="Exporting…" onClick={() => download.mutate()}>
            Export
          </Button>
        </div>
        {download.isError && <InlineAlert tone="danger">{(download.error as Error).message}</InlineAlert>}
      </Card>
      <Card>
        <div style={{ fontWeight: 700, marginBottom: 4 }}>Retention</div>
        <p className="mvx-admin-muted" style={{ marginTop: 0 }}>
          How many days of events to keep. 0 keeps them forever; anything else is at least {floor} days. A daily sweep
          removes older events and does not archive them — export first.
        </p>
        <div style={{ display: "flex", gap: 12, alignItems: "flex-end" }}>
          <Field label="Days">
            <NumberInput min={0} value={retention} onChange={(e) => setDays(Number(e.target.value) || 0)} aria-label="Retention days" />
          </Field>
          <Button loading={save.isPending} loadingLabel="Saving…" onClick={() => save.mutate()}>{saved ? "Saved" : "Save retention"}</Button>
          {save.isError && <span className="mvx-admin-error">{(save.error as Error).message}</span>}
        </div>
      </Card>
    </div>
  );
}

import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { api, type TenantUsage } from "../../api/client";
import { FeatureGate } from "../../license/FeatureGate";
import { Card, DataTable, InlineAlert, LoadingState, SegmentedControl, Toolbar, ToolbarGroup, type DataTableColumn } from "../../ui";

const PERIODS = [{ id: "7d", label: "7 days" }, { id: "30d", label: "30 days" }, { id: "90d", label: "90 days" }];

function bytes(n: number): string {
  if (n <= 0) return "—";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0; let v = n;
  while (v >= 1024 && i < units.length - 1) { v /= 1024; i++; }
  return `${v.toFixed(i === 0 ? 0 : 1)} ${units[i]}`;
}
const num = (n: number) => n.toLocaleString();
const when = (iso?: string) => (iso ? new Date(iso).toLocaleDateString() : "never");

/**
 * Admin › Usage: every number the platform keeps about a tenant, counted —
 * people active in the period, what exists, what ran, how much space. A
 * platform admin sees a row per tenant; a tenant admin sees their own.
 * Enterprise: other editions get the gate.
 */
export function UsageTab({ scope }: { scope: "platform" | "tenant" }) {
  return (
    <FeatureGate feature="usage_analytics">
      <UsageView scope={scope} />
    </FeatureGate>
  );
}

const COLUMNS: DataTableColumn<TenantUsage>[] = [
  { id: "name", header: "Tenant", cell: (t) => <span style={{ fontWeight: 600 }}>{t.name}</span> },
  { id: "plan", header: "Plan", cell: (t) => t.plan },
  { id: "active_users", header: "Active / users", align: "right", cell: (t) => `${num(t.active_users)} / ${num(t.users)}` },
  { id: "models", header: "Apps / models / revisions", align: "right", cell: (t) => `${num(t.applications)} / ${num(t.models)} / ${num(t.revisions)}` },
  { id: "fact_rows", header: "Facts / calc rows", align: "right", cell: (t) => `${num(t.fact_rows)} / ${num(t.calc_rows)}` },
  { id: "workflow_instances", header: "Workflows / integrations / AI msgs", align: "right", cell: (t) => `${num(t.workflow_instances)} / ${num(t.integration_runs)} / ${num(t.ai_messages)}` },
  { id: "audit_events", header: "Audit events", align: "right", cell: (t) => num(t.audit_events) },
  { id: "db_bytes", header: "Storage", align: "right", cell: (t) => `${bytes(t.db_bytes)}${t.object_bytes > 0 ? ` + ${bytes(t.object_bytes)} files` : ""}` },
  { id: "last_activity_at", header: "Last activity", cell: (t) => when(t.last_activity_at) },
];

function UsageView({ scope }: { scope: "platform" | "tenant" }) {
  const [period, setPeriod] = useState("30d");
  const { data, isLoading, error } = useQuery({ queryKey: ["usage", period], queryFn: () => api.getUsage(period) });
  if (isLoading) return <LoadingState label="Counting…" />;
  if (error) return <InlineAlert tone="danger">{(error as Error).message}</InlineAlert>;
  const tenants = data?.tenants ?? [];
  return (
    <div className="mvx-admin-stack" data-testid="usage">
      <Toolbar>
        <ToolbarGroup>
          <span className="mvx-admin-muted">Activity in the last</span>
          <SegmentedControl value={period} onChange={setPeriod} segments={PERIODS} aria-label="Usage period" />
        </ToolbarGroup>
      </Toolbar>
      {scope === "platform" ? (
        <DataTable columns={COLUMNS} rows={tenants} getRowKey={(t) => t.customer_id} density="compact" emptyTitle="No tenants yet." />
      ) : (
        tenants.map((t) => (
          <Card key={t.customer_id}>
            <div style={{ fontWeight: 700, marginBottom: 8 }}>{t.name} <span className="mvx-admin-muted">· {t.plan}</span></div>
            <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(180px, 1fr))", gap: 12 }} data-testid="usage-cards">
              <Stat label={`Active users (${period})`} value={`${num(t.active_users)} of ${num(t.users)}`} />
              <Stat label="Applications / models" value={`${num(t.applications)} / ${num(t.models)}`} />
              <Stat label="Revisions" value={num(t.revisions)} />
              <Stat label="Fact rows" value={num(t.fact_rows)} />
              <Stat label="Calculated rows" value={num(t.calc_rows)} />
              <Stat label="Form records" value={num(t.form_records)} />
              <Stat label={`Workflow instances (${period})`} value={num(t.workflow_instances)} />
              <Stat label={`Integration runs (${period})`} value={num(t.integration_runs)} />
              <Stat label={`AI messages (${period})`} value={num(t.ai_messages)} />
              <Stat label={`Audit events (${period})`} value={num(t.audit_events)} />
              <Stat label="Database" value={t.db_bytes > 0 ? bytes(t.db_bytes) : "shared"} />
              <Stat label="Uploaded files" value={bytes(t.object_bytes)} />
              <Stat label="Last activity" value={when(t.last_activity_at)} />
            </div>
          </Card>
        ))
      )}
    </div>
  );
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <div className="mvx-admin-muted" style={{ fontSize: 12 }}>{label}</div>
      <div style={{ fontSize: 18, fontWeight: 600, fontVariantNumeric: "tabular-nums" }}>{value}</div>
    </div>
  );
}

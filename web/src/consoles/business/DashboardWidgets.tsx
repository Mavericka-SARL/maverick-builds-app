import React, { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { Check as CheckIcon, Trash2, Plus as PlusIcon } from "lucide-react";
import { api, type AutomationRule, type DashboardWidget, type DemoContext, type WidgetProps, type GridData, type IntegrationDef, type Metric, type DevDimension, type FormDef, type FormRecord, type FormField, type ChartContextDim } from "../../api/client";
import { ContextSelectors } from "../dashboard/ChartWidget";
import { useSelectorOwnership, useWidgetContextSync } from "../dashboardContextSync";
import { WidgetErrorBoundary, CommandButton, InlineAlert, LoadingState, EmptyState, Select, IconButton, Button, Field, useConfirm } from "../../ui";
import { groupWidgetsIntoRows, INTRINSIC_HEIGHT_WIDGET_TYPES } from "../dashboardLayout";
import { DashboardContextSyncProvider } from "../DashboardContextSyncProvider";
import { ChartWidget } from "../dashboard/ChartWidget";
import { FormFieldInput } from "./FormsTab";
import { ImportWidget } from "./ImportWidget";
import { PlanningGrid } from "./PlanningGrid";

// The one place row-grouped widgets actually get laid out and painted —
// used both here (the real business-facing page) and by the Developer
// Console's Preview mode, so what a developer previews is pixel-for-pixel
// what a business user gets: same borderless widgets, same minHeight (not
// a fixed height + overflow:hidden) so taller-than-designed content grows
// the row instead of getting silently clipped. See INTRINSIC_HEIGHT_WIDGET_TYPES
// for why small widgets don't stretch to match a taller row-mate.
export function DashboardWidgetGrid({ widgets, ctx, dashboardId, onOpenInstance }: { widgets: DashboardWidget[]; ctx: DemoContext; dashboardId?: string; onOpenInstance?: (instanceId: string) => void }) {
  // The rendered dashboard is exactly as wide as the design canvas: at least
  // 1200px (60 columns of 20px), growing to the right edge of the widest
  // placement — the design canvas grows the same way. Pinning 60 columns
  // regardless squeezed or cut anything designed beyond 1200px (reported
  // live: the fourth KPI clipped, chart and grid at different widths than
  // Design). Wider than the viewport, the dashboard scrolls sideways
  // rather than reflowing, so Design and Preview stay identical.
  const rightEdge = Math.max(0, ...widgets.map(w => (w.pos_x || 0) + (w.size_w || 300)));
  const columns = Math.max(60, Math.ceil(rightEdge / 20));
  const width = columns * 20;
  return (
    <DashboardContextSyncProvider key={dashboardId ?? "default"}>
      {/* maxWidth pins the rendered dashboard to the design canvas's
          coordinate space (CANVAS_MIN_W in DashboardCanvas.tsx), so a
          widget designed at 600px wide IS 600px wide here — previously the
          60-column grid spanned the whole viewport and silently scaled
          every widget up on wide monitors, making Preview disagree with
          Design (reported live). Below that width the grid still shrinks
          proportionally, so nothing overflows on smaller screens. */}
      <div style={{ overflowX: "auto" }}>
      <div style={{ display: "flex", flexDirection: "column", gap: 8, width, maxWidth: width }}>
        {groupWidgetsIntoRows(widgets).map((row, i) => (
          <div key={i} style={{ display: "grid", gridTemplateColumns: `repeat(${columns}, 20px)` }}>
            {row.map(widget => (
              <div
                key={widget.id}
                className={[
                  "mvx-dash-cell",
                  widget.widget_type === "metric_kpi" ? "mvx-dash-cell--kpi" : "",
                  // Developer-chosen surface (widget_props.background): a card
                  // for a bare widget type, or nothing at all.
                  widget.widget_type !== "metric_kpi" && widget.widget_props?.background === "white" ? "mvx-dash-cell--card" : "",
                ].filter(Boolean).join(" ")}
                style={{
                  "--dash-col-start": Math.round((widget.pos_x || 0) / 20) + 1,
                  "--dash-col-span": Math.max(1, Math.round((widget.size_w || 300) / 20)),
                  minWidth: 0, padding: "0 8px",
                  // A designed size_h is EXACT, not a minimum: taller
                  // content scrolls inside its widget (overflow auto — the
                  // scrollbar makes the cut visible, unlike the silent
                  // clipping that motivated the old grow-the-row behavior).
                  // Widgets that never got an explicit height keep the old
                  // semantics: intrinsic types size themselves, everything
                  // else stretches to the row.
                  // A KPI tile takes its designed height as a MINIMUM and
                  // stretches to its row: a row of tiles stays aligned (as in
                  // Design) and one carrying a selector row grows instead of
                  // clipping its value.
                  ...(widget.widget_type === "metric_kpi"
                    ? { minHeight: widget.size_h || undefined, alignSelf: "stretch" as const }
                    : widget.size_h && !INTRINSIC_HEIGHT_WIDGET_TYPES.has(widget.widget_type)
                    ? { height: widget.size_h, overflow: "auto" as const, alignSelf: "start" as const }
                    : {
                        minHeight: widget.size_h || (widget.widget_type === "chart" ? 360 : undefined),
                        alignSelf: INTRINSIC_HEIGHT_WIDGET_TYPES.has(widget.widget_type) ? ("start" as const) : ("stretch" as const),
                      }),
                } as unknown as React.CSSProperties}
              >
                <WidgetRenderer widget={widget} ctx={ctx} onOpenInstance={onOpenInstance} />
              </div>
            ))}
          </div>
        ))}
      </div>
      </div>
    </DashboardContextSyncProvider>
  );
}

// onOpenInstance is optional because this same renderer powers the Developer
// Console's Preview, where there's no console tab to navigate to.
export function WidgetRenderer({ widget, ctx, onOpenInstance }: { widget: DashboardWidget; ctx: DemoContext; onOpenInstance?: (instanceId: string) => void }) {
  let inner: React.ReactNode = null;

  if (widget.widget_type === "grid" && widget.ref_id) {
    // sync_context defaults ON: one dashboard, one shared context (and one
    // set of selectors) — widgets opt OUT with an explicit false. The title
    // goes INTO the grid's own toolbar row (beside the ··· actions) instead
    // of a separate header bar — see the show_title branch below, which
    // deliberately skips grid widgets.
    inner = <PlanningGrid ctx={ctx} gridDefId={widget.ref_id} defaultView={widget.widget_props?.default_view} syncContext={widget.widget_props?.sync_context !== false} selectorsPosition={widget.widget_props?.selectors_position} title={widget.show_title && widget.title ? widget.title : undefined} />;
  } else if (widget.widget_type === "text" && widget.content) {
    const wp = widget.widget_props ?? {};
    inner = (
      <p style={{ fontSize: wp.font_size ?? 14, color: wp.color ?? "var(--color-text)", margin: 0, whiteSpace: "pre-wrap", lineHeight: 1.6, fontWeight: wp.font_weight ?? "normal", fontFamily: wp.font_family ?? "sans-serif" }}>
        {widget.content}
      </p>
    );
  } else if (widget.widget_type === "automation_button" && widget.ref_id) {
    inner = <AutomationButtonWidget ruleId={widget.ref_id} label={widget.content ?? "Trigger"} buttonColor={widget.widget_props?.button_color} ctx={ctx} staticContext={widget.widget_props?.context} confirmText={widget.widget_props?.confirm_text} onOpenInstance={onOpenInstance} />;
  } else if (widget.widget_type === "form" && widget.ref_id) {
    inner = (
      <div className="mvx-panel" style={{ padding: 0, overflow: "hidden" }}>
        <FormWidgetPanel formId={widget.ref_id} ctx={ctx} />
      </div>
    );
  } else if (widget.widget_type === "integration_button" && widget.ref_id) {
    inner = <IntegrationButtonWidget integrationId={widget.ref_id} label={widget.content ?? "Import"} buttonColor={widget.widget_props?.button_color} />;
  } else if (widget.widget_type === "chart" && widget.ref_id && widget.widget_props?.chart) {
    inner = <ChartWidget widget={widget} ctx={ctx} />;
  } else if (widget.widget_type === "metric_kpi" && widget.ref_id) {
    inner = <MetricKpiWidget metricId={widget.ref_id} ctx={ctx} widgetProps={widget.widget_props ?? undefined} />;
  } else if (widget.widget_type === "import" && widget.ref_id) {
    inner = (
      <div className="mvx-panel" style={{ padding: 16 }}>
        <ImportWidget gridDefId={widget.ref_id} ctx={ctx} />
      </div>
    );
  }

  if (!inner) return null;

  const isNoPad = widget.widget_type === "grid" || widget.widget_type === "form" || widget.widget_type === "chart";
  const widgetLabel = widget.title || widget.widget_type.replace(/_/g, " ");

  if (widget.show_title && widget.title && widget.widget_type !== "grid") {
    return (
      <WidgetErrorBoundary widgetLabel={widgetLabel}>
        <div style={{ display: "flex", flexDirection: "column", height: "100%" }}>
          <div className="mvx-widget__header">{widget.title}</div>
          <div className={["mvx-widget__body", isNoPad ? "mvx-widget__body--flush" : ""].filter(Boolean).join(" ")}>
            {inner}
          </div>
        </div>
      </WidgetErrorBoundary>
    );
  }

  return <WidgetErrorBoundary widgetLabel={widgetLabel}>{inner}</WidgetErrorBoundary>;
}

export function MetricKpiWidget({ metricId, ctx, widgetProps }: { metricId: string; ctx: DemoContext; widgetProps?: WidgetProps }) {
  const staticScope = widgetProps?.kpi_scope;
  // How this tile picks its context, set by the developer in the widget
  // editor. Legacy widgets (no explicit mode) infer it from the old fields so
  // their behaviour is unchanged: a kpi_scope means "pin", sync_context===false
  // means "total", otherwise "sync".
  const mode: "total" | "sync" | "pin" =
    widgetProps?.kpi_context_mode ??
    (staticScope ? "pin" : widgetProps?.sync_context === false ? "total" : "sync");

  // Meta-only read: dimensions + metrics with no cells/totals, so it stays
  // fast on large models and gives us the metric's own dims for the selectors.
  const { data: meta } = useQuery({
    queryKey: ["grid-meta", ctx.revision_id],
    queryFn: () => api.getGrid(ctx.revision_id, undefined, { metaOnly: true }),
    staleTime: 30_000,
  });
  const m = meta as GridData | undefined;
  const metric = (m?.all_metrics ?? m?.metrics)?.find(mm => mm.id === metricId);

  // "sync" mode: this KPI's selectors join the dashboard-wide context sync —
  // one deduped selector row drives every synced KPI and the grid/chart.
  const syncOn = mode === "sync";
  const kpiDims = (m?.all_dimensions ?? m?.dimensions ?? []).filter(
    d => metric?.dimension_ids?.includes(d.id),
  );
  const [context, setContext] = useState<Record<string, string>>({});
  const setContextValue = useWidgetContextSync(kpiDims, syncOn, context, setContext);
  const ownsSelector = useSelectorOwnership(syncOn);
  const selectorDims = syncOn ? kpiDims.filter(d => ownsSelector(d.id)) : [];

  // The scope sent to the server, per mode. "pin" fixes one member (works for
  // input AND calc metrics now — the server resolves the scoped total, fast
  // via a precomputed slice); "sync" follows the shared context once it has
  // seeded; "total" sends nothing (whole-model grand total, the '{}' fast
  // path). Every mode reads totals_only and keys on (revision, scope) so KPIs
  // sharing a scope dedup to a single fetch.
  const syncScoped = syncOn && Object.keys(context).length > 0;
  const scope: Record<string, string> | undefined =
    mode === "pin" && staticScope
      ? { [staticScope.dimension_id]: staticScope.member_code }
      : syncScoped
        ? context
        : undefined;
  const valueReady = mode !== "sync" || kpiDims.length === 0 || syncScoped;
  const scopeKey = JSON.stringify(scope ?? {});
  const { data: grid } = useQuery({
    queryKey: ["kpi-totals", ctx.revision_id, scopeKey],
    queryFn: () => api.getGrid(ctx.revision_id, undefined, { totalsOnly: true, ...(scope ? { scope } : {}) }),
    enabled: valueReady,
    staleTime: 0,
  });
  const g = grid as GridData | undefined;

  // Absent ≠ zero: a scoped read omits a calc metric whose slice genuinely
  // has no value (the scheduler writes no row where the formula can't
  // evaluate), and rendering 0 for it would fabricate a number. Keep the
  // value undefined and show "—" below — the same display contract the grid
  // follows.
  const value: number | undefined = g?.totals?.[metricId];
  const fmt = metric?.format ?? "number";

  const formatted = value === undefined
    ? "—"
    : fmt === "currency"
    ? `${metric?.format_currency || "$"}${value.toLocaleString("en-US", { minimumFractionDigits: 0, maximumFractionDigits: 0 })}`
    : fmt === "percentage"
    ? `${(value * 100).toFixed(1)}%`
    : value.toLocaleString("en-US");

  // No fallback to scope.member_code: a member absent from all_dimensions/
  // dimensions is exactly what a hidden dimension_member access rule looks
  // like (the server already filters it out) — falling back to the raw
  // code would leak its identity even though its value stays correctly
  // hidden above.
  const scopeLabel = mode === "pin" && staticScope
    ? ((m?.all_dimensions ?? m?.dimensions)?.find(d => d.id === staticScope.dimension_id)?.members.find(mm => mm.code === staticScope.member_code)?.label ?? null)
    : null;

  // Developer-chosen selector placement (widget_props.selectors_position).
  const selPos = widgetProps?.selectors_position ?? "top";
  const selVertical = selPos === "left" || selPos === "right";
  return (
    <div className={widgetProps?.background === "none" ? "mvx-kpi mvx-kpi--plain" : "mvx-kpi"} style={selVertical ? { flexDirection: selPos === "left" ? "row" : "row-reverse", alignItems: "center", gap: 16, flexWrap: "wrap", minWidth: 0, overflow: "hidden" } : undefined}>
      {selectorDims.length > 0 && (
        <div style={{ order: selPos === "bottom" ? 2 : 0, flexShrink: 0 }}>
          <ContextSelectors
            dims={selectorDims as unknown as ChartContextDim[]}
            context={context}
            onChange={setContextValue}
            vertical={selVertical}
          />
        </div>
      )}
      <div style={{ display: "flex", flexDirection: "column", alignItems: "flex-start", minWidth: 0, flex: selVertical ? 1 : undefined }}>
        {metric && (
          <span className="mvx-kpi__label">
            {metric.label || metric.name}{scopeLabel && <span className="mvx-kpi__scope"> · {scopeLabel}</span>}
          </span>
        )}
        <span className="mvx-kpi__value" style={widgetProps?.color ? { color: widgetProps.color } : undefined}>{valueReady && g ? formatted : "—"}</span>
        <div className="mvx-kpi__accent" />
      </div>
    </div>
  );
}

export function IntegrationButtonWidget({ integrationId, label, buttonColor }: { integrationId: string; label: string; buttonColor?: string }) {
  const fileRef = React.useRef<HTMLInputElement>(null);
  const [result, setResult] = useState<{ rows_imported?: number; error_rows?: number; message?: string } | null>(null);

  // A google_sheets integration needs no file — the server re-fetches its
  // configured sheet on every run — so its button syncs on click instead of
  // opening a picker. The list is how the widget learns the type.
  const { data: integrations = [] } = useQuery({ queryKey: ["integrations"], queryFn: () => api.listIntegrations() });
  const isSheets = (integrations as IntegrationDef[]).find(i => i.id === integrationId)?.type === "google_sheets";

  const run = useMutation({
    mutationFn: (csv?: string) => api.runIntegration(integrationId, csv),
    onSuccess: (data) => setResult(data as typeof result),
    onError: (err) => setResult({ message: (err as Error).message }),
  });

  function handleFile(e: React.ChangeEvent<HTMLInputElement>) {
    const file = e.target.files?.[0];
    if (!file) return;
    const reader = new FileReader();
    reader.onload = (ev) => {
      const text = ev.target?.result as string;
      run.mutate(text);
    };
    reader.readAsText(file);
    e.target.value = "";
  }

  return (
    <div style={{ display: "flex", flexDirection: "column", width: "100%", height: "100%" }}>
      <input ref={fileRef} type="file" accept=".csv,text/csv" style={{ display: "none" }} onChange={handleFile} />
      <CommandButton
        color={run.isSuccess ? "var(--color-info)" : buttonColor}
        onClick={() => {
          setResult(null);
          if (isSheets) run.mutate(undefined);
          else fileRef.current?.click();
        }}
        disabled={run.isPending}
      >
        {run.isSuccess && <CheckIcon size={15} aria-hidden="true" />}
        {run.isPending ? (isSheets ? "Syncing…" : "Importing…") : run.isSuccess ? "Done" : label}
      </CommandButton>
      {result && !run.isPending && (
        <div style={{ fontSize: 12, color: result.message ? "var(--color-danger)" : "var(--color-live)" }}>
          {result.message
            ? result.message
            : `${result.rows_imported ?? 0} rows imported${result.error_rows ? `, ${result.error_rows} errors` : ""}`}
        </div>
      )}
    </div>
  );
}

// ctx/staticContext give every automation_button the same baseline
// (model_id, revision_id) plus the widget's own static widget_props.context
// (e.g. {"target_revision_id": "<uuid>"}) that a WorkflowActionWidget used
// to provide directly — TriggerRule/ResolveStartContext (internal/workflow)
// merges this payload with server-resolved RACI context before starting the
// instance, so a manual trigger button gets the same scoping a direct
// POST /api/workflow/start call would.
export function AutomationButtonWidget({ ruleId, label, buttonColor, ctx, staticContext, confirmText, onOpenInstance }: { ruleId: string; label: string; buttonColor?: string; ctx: DemoContext; staticContext?: Record<string, string>; confirmText?: string; onOpenInstance?: (instanceId: string) => void }) {
  const { confirm, confirmElement } = useConfirm();

  // The rule list is the only way this widget can tell a runnable rule from
  // one that is disabled, event-triggered, or gone — the trigger endpoint
  // only reports that after a click. Rules outside the caller's access scope
  // simply don't appear, which is the "unauthorized" case below.
  // Scoped to the dashboard's revision (ctx): a dashboard previewed in a
  // non-active revision showed its own button as "not available" because the
  // rule list defaulted to the active revision (found live, 2026-09-11).
  const { data: rulesRaw = [], isLoading: rulesLoading } = useQuery({
    queryKey: ["automation-rules", ctx.revision_id],
    queryFn: () => api.listAutomationRules(ctx.revision_id),
  });
  const rule = (rulesRaw as AutomationRule[]).find(r => r.id === ruleId);

  const trigger = useMutation({
    mutationFn: () => api.triggerRule(ruleId, { model_id: ctx.model_id, revision_id: ctx.revision_id, ...staticContext }),
  });

  // Explicit states, rather than a button that looks live and fails on click.
  const blocked =
    rulesLoading ? null
    : !rule ? "This automation isn't available to you, or no longer exists."
    : !rule.enabled ? `"${rule.name}" is disabled.`
    : rule.trigger_type !== "manual" ? `"${rule.name}" runs on ${rule.trigger_type.replace(/_/g, " ")}, not on demand.`
    : null;

  if (blocked) {
    return (
      <div style={{ display: "flex", flexDirection: "column", width: "100%", height: "100%", gap: 4 }}>
        <CommandButton color="var(--color-disabled)" disabled onClick={() => {}}>
          {label}
        </CommandButton>
        <span style={{ fontSize: 12, color: "var(--color-text-muted)" }}>{blocked}</span>
      </div>
    );
  }

  const run = () => {
    if (confirmText) {
      confirm({ title: label, body: confirmText, confirmLabel: "Run", onConfirm: () => trigger.mutate() });
      return;
    }
    trigger.mutate();
  };

  const instanceId = trigger.data?.instance_id;

  return (
    <div style={{ display: "flex", flexDirection: "column", width: "100%", height: "100%", gap: 4 }}>
      <CommandButton
        color={trigger.isSuccess ? "var(--color-success)" : (buttonColor ?? "var(--color-success)")}
        onClick={run}
        disabled={trigger.isPending || rulesLoading}
      >
        {trigger.isSuccess && <CheckIcon size={15} aria-hidden="true" />}
        {trigger.isPending ? "Running…" : trigger.isSuccess ? "Started" : label}
      </CommandButton>
      {trigger.isSuccess && (
        <div style={{ fontSize: 12, color: "var(--color-text-muted)" }}>
          {instanceId ? (
            onOpenInstance ? (
              <>
                Run started —{" "}
                <Button size="sm" variant="ghost" onClick={() => onOpenInstance(instanceId)}>
                  view its progress
                </Button>
              </>
            ) : (
              <>Run started — instance <code>{instanceId.slice(0, 8)}</code></>
            )
          ) : (
            <>Started, but no instance was recorded.</>
          )}
        </div>
      )}
      {trigger.isError && <p className="mvx-admin-error">{(trigger.error as Error).message}</p>}
      {confirmElement}
    </div>
  );
}

export function FormWidgetPanel({ formId, ctx: hostCtx }: { formId: string; ctx?: DemoContext }) {
  const qc = useQueryClient();
  const [showNewRecord, setShowNewRecord] = useState(false);
  const [draft, setDraft] = useState<Record<string, unknown>>({});
  const [syncMsg, setSyncMsg] = useState<string | null>(null);
  const { confirm, confirmElement } = useConfirm();

  // Forms of the dashboard's revision (hostCtx) — "Form not found" appeared
  // for a dashboard previewed in a non-active revision because the list
  // defaulted to the active one (found live, 2026-09-11).
  const { data: formsRaw = [] } = useQuery({
    queryKey: ["forms", hostCtx?.revision_id ?? ""],
    queryFn: () => api.listForms(hostCtx?.revision_id),
    refetchInterval: 20_000,
  });
  const { data: demoCtx } = useQuery({ queryKey: ["demo"], queryFn: api.getDemo, enabled: !hostCtx });
  const ctx = hostCtx ?? demoCtx;
  const { data: dimsRaw = [] } = useQuery({ queryKey: ["dimensions"], queryFn: api.getDimensions });
  const dims = dimsRaw as DevDimension[];
  const revisionId = (ctx as DemoContext | undefined)?.revision_id ?? "";
  const { data: metricsRaw = [] } = useQuery({
    queryKey: ["metrics", revisionId],
    queryFn: () => api.getMetrics(revisionId),
    enabled: !!revisionId,
  });
  const metrics = metricsRaw as Metric[];
  const form = (formsRaw as FormDef[]).find((f) => f.id === formId);

  const { data: records = [], isLoading: recLoading } = useQuery({
    queryKey: ["records", formId],
    queryFn: () => api.listRecords(formId),
    enabled: !!form,
  });

  const createRecord = useMutation({
    mutationFn: () => api.createRecord(formId, draft, (ctx as DemoContext | undefined)?.revision_id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records"] });
      qc.invalidateQueries({ queryKey: ["grid"] });
      setDraft({});
      setShowNewRecord(false);
    },
  });

  const updateStatus = useMutation({
    mutationFn: ({ id, status, data }: { id: string; status: string; data: Record<string, unknown> }) =>
      api.updateRecord(id, status, data),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["records"] }),
  });

  const deleteRec = useMutation({
    mutationFn: (id: string) => api.deleteRecord(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["records"] }),
  });

  const syncForm = useMutation({
    mutationFn: (fid: string) => api.syncForm(fid),
    onSuccess: (data) => {
      qc.invalidateQueries({ queryKey: ["grid"] });
      qc.invalidateQueries({ queryKey: ["records"] });
      if (data.mappings === 0) {
        setSyncMsg("No active integrations configured for this form.");
      } else {
        setSyncMsg(`Synced — ${data.records_processed} record(s) across ${data.mappings} integration(s).`);
      }
      setTimeout(() => setSyncMsg(null), 5000);
    },
    onError: (err: Error) => {
      setSyncMsg(`Sync failed: ${err.message}`);
      setTimeout(() => setSyncMsg(null), 6000);
    },
  });

  if (!form) return <EmptyState label="Form not found." />;

  return (
    <div>
      <div style={{ borderBottom: "1px solid var(--color-border)" }}>
        <div style={{ padding: "12px 16px", background: "var(--color-surface-subtle)", display: "flex", alignItems: "center", justifyContent: "space-between" }}>
          <span style={{ fontWeight: 600, fontSize: 14 }}>{form.label}</span>
          <Button
            size="sm"
            loading={syncForm.isPending}
            loadingLabel="Syncing…"
            onClick={() => { setSyncMsg(null); syncForm.mutate(formId); }}
          >
            Sync to grid
          </Button>
        </div>
        {syncMsg && (
          <InlineAlert tone={syncMsg.startsWith("Sync failed") ? "danger" : "success"}>
            {syncMsg}
          </InlineAlert>
        )}
      </div>
      {/* Records table */}
      {recLoading ? <LoadingState /> : (
        <div className="mvx-table-wrap" style={{ overflowY: "auto", maxHeight: 320, marginBottom: 20 }}>
          <table className="mvx-table mvx-table--compact">
            <thead>
              <tr>
                {form.fields.map((f) => (
                  <th key={f.name}>{f.label}</th>
                ))}
                <th>Status</th>
                <th>Created</th>
                <th style={{ width: 70 }} />
              </tr>
            </thead>
            <tbody>
              {(records as FormRecord[]).map((rec) => (
                <tr key={rec.id}>
                  {form.fields.map((f) => (
                    <td key={f.name}>{String(rec.data[f.name] ?? "—")}</td>
                  ))}
                  <td>
                    <Select
                      value={rec.status}
                      onChange={(e) => updateStatus.mutate({ id: rec.id, status: e.target.value, data: rec.data })}
                      aria-label="Record status"
                    >
                      {["draft", "submitted", "approved", "rejected"].map((s) => (
                        <option key={s} value={s}>{s}</option>
                      ))}
                    </Select>
                  </td>
                  <td className="mvx-admin-muted">
                    {new Date(rec.created_at).toLocaleDateString()}
                  </td>
                  <td>
                    <IconButton aria-label="Delete record" title="Delete" danger size={26}
                      onClick={() => confirm({ title: "Delete record?", body: "This record will be permanently removed.", confirmLabel: "Delete", onConfirm: () => deleteRec.mutate(rec.id) })}>
                      <Trash2 size={13} />
                    </IconButton>
                  </td>
                </tr>
              ))}
              {(records as FormRecord[]).length === 0 && (
                <tr><td colSpan={form.fields.length + 3} className="mvx-admin-muted" style={{ textAlign: "center", padding: 24 }}>
                  No records yet.
                </td></tr>
              )}
            </tbody>
          </table>
          {confirmElement}
        </div>
      )}

      {/* New record form */}
      <Button
        leadingIcon={showNewRecord ? undefined : <PlusIcon size={14} />}
        onClick={() => setShowNewRecord((v) => !v)}
      >
        {showNewRecord ? "Cancel" : "New record"}
      </Button>

      {showNewRecord && (
        <div className="mvx-panel" style={{ marginTop: 16, maxWidth: 560, padding: 20 }}>
          <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
            {form.fields.map((f: FormField) => (
              <Field key={f.name} label={f.label} required={f.required}>
                <FormFieldInput f={f} value={draft[f.name]} onChange={v => setDraft(d => ({ ...d, [f.name]: v }))} dims={dims} metrics={metrics} />
              </Field>
            ))}
            <Button variant="primary" style={{ alignSelf: "flex-start" }} loading={createRecord.isPending} loadingLabel="Saving…" onClick={() => createRecord.mutate()}>
              Save
            </Button>
            {createRecord.isError && <p className="mvx-admin-error">{(createRecord.error as Error).message}</p>}
          </div>
        </div>
      )}
    </div>
  );
}

import { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { Plus, X, Pencil, Trash2, Check, AlertTriangle } from "lucide-react";
import { api, type DevModel, type DevMetric } from "../../api/client";
import { Toolbar, ToolbarGroup, SearchInput, Button, TextInput, Select, NumberInput, StatusBadge, IconButton, useConfirm, Field, SegmentedControl } from "../../ui";

function fmtBadge(format: string, decimals: number, currency: string): string {
  switch (format) {
    case "percentage": return decimals > 0 ? `%.${decimals}` : "%";
    case "currency":   return `${currency || "$"}${decimals > 0 ? `.${decimals}` : ""}`;
    case "boolean":    return "Y/N";
    case "text":       return "TXT";
    default:           return decimals > 0 ? `#.${decimals}` : "#";
  }
}

/**
 * The two operands for agg_rule "rate" — Anaplan's Ratio summary. The total
 * becomes numerator ÷ denominator, taken from two other metrics rather than
 * from this metric's own member values, which is what lets a ratio work on an
 * input metric: a price typed per product totals as revenue ÷ volume, never as
 * a sum or a mean of the prices.
 *
 * The metric being edited is excluded from both lists — dividing by itself, or
 * feeding its own total back in, is what the server rejects.
 */
function RatioOperands({
  metrics, selfId, numeratorId, denominatorId, onNumerator, onDenominator,
}: {
  metrics: DevMetric[];
  selfId?: string;
  numeratorId: string;
  denominatorId: string;
  onNumerator: (id: string) => void;
  onDenominator: (id: string) => void;
}) {
  const options = metrics.filter(m => m.id !== selfId);
  const render = (m: DevMetric) => <option key={m.id} value={m.id}>{m.label || m.name}</option>;
  return (
    <>
      <Field label="Numerator">
        <Select value={numeratorId} onChange={e => onNumerator(e.target.value)} aria-label="Ratio numerator">
          <option value="">Select a metric…</option>
          {options.map(render)}
        </Select>
      </Field>
      <Field label="Denominator" description="total = numerator ÷ denominator">
        <Select value={denominatorId} onChange={e => onDenominator(e.target.value)} aria-label="Ratio denominator">
          <option value="">Select a metric…</option>
          {options.map(render)}
        </Select>
      </Field>
    </>
  );
}

export function MetricsTab({ model, revisionId }: { model: DevModel; revisionId?: string }) {
  const [showAdd, setShowAdd] = useState(false);
  const [search, setSearch] = useState("");

  const q = search.toLowerCase();
  const match = (m: DevMetric) =>
    !q ||
    m.name.toLowerCase().includes(q) ||
    m.label?.toLowerCase().includes(q) ||
    m.formula?.toLowerCase().includes(q);

  const inputs = model.metrics.filter((m) => m.is_input && match(m));
  const calcs = model.metrics.filter((m) => !m.is_input && match(m));
  const allInputs = model.metrics.filter((m) => m.is_input);
  const allCalcs = model.metrics.filter((m) => !m.is_input);

  return (
    <div>
      <Toolbar className="mvx-toolbar--spaced">
        <ToolbarGroup>
          <span className="mvx-admin-muted">
            App: <strong>{model.app_name}</strong> · Model: <strong>{model.model_name}</strong> ·{" "}
            <span className="mvx-admin-mono">{model.model_id.slice(0, 8)}</span>
          </span>
        </ToolbarGroup>
        <ToolbarGroup align="end">
          <SearchInput
            placeholder="Search metrics…"
            value={search}
            onChange={e => setSearch(e.target.value)}
            width={200}
          />
        </ToolbarGroup>
      </Toolbar>

      <div className="mvx-table-wrap">
        <table className="mvx-table mvx-table--compact">
          <thead>
            <tr>
              <th>Name</th>
              <th>Label</th>
              <th style={{ width: 60 }}>Type</th>
              <th>Formula / Dependencies</th>
              <th>Used by</th>
              <th style={{ width: 80 }}>Actions</th>
            </tr>
          </thead>
          <tbody>
            {inputs.map((m) => <MetricRow key={m.id} m={m} allMetrics={model.metrics} />)}
            {inputs.length > 0 && calcs.length > 0 && (
              <tr><td colSpan={6} className="mvx-table__group-row">Calculated</td></tr>
            )}
            {calcs.map((m) => <MetricRow key={m.id} m={m} allMetrics={model.metrics} />)}
            {inputs.length === 0 && calcs.length === 0 && q && (
              <tr><td colSpan={6} style={{ padding: 20, textAlign: "center" }} className="mvx-admin-muted">No metrics match "{search}"</td></tr>
            )}
          </tbody>
        </table>
      </div>

      <div className="mvx-admin-muted" style={{ marginTop: 12 }}>
        {q
          ? `${inputs.length + calcs.length} result${inputs.length + calcs.length !== 1 ? "s" : ""} of ${allInputs.length + allCalcs.length} metrics`
          : `${allInputs.length} input metrics · ${allCalcs.length} calculated metrics`
        }
      </div>

      <div style={{ marginTop: 28, borderTop: "1px solid var(--color-border)", paddingTop: 20 }}>
        <Button
          variant="ghost"
          leadingIcon={showAdd ? undefined : <Plus size={14} />}
          onClick={() => setShowAdd((v) => !v)}
        >
          {showAdd ? "Cancel" : "Add metric"}
        </Button>
        {showAdd && (
          <div style={{ marginTop: 16 }}>
            <AddMetricForm model={model} revisionId={revisionId} onSuccess={() => setShowAdd(false)} />
          </div>
        )}
      </div>
    </div>
  );
}

type RecalcRow = { revision: string; metric: string; value: number | null };

function MetricRow({ m, allMetrics }: { m: DevMetric; allMetrics: DevMetric[] }) {
  const qc = useQueryClient();
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState(m.name);
  const [formula, setFormula] = useState(m.formula ?? "");
  const [aggRule, setAggRule] = useState(m.agg_rule ?? "sum");
  const [numeratorId, setNumeratorId] = useState(m.agg_numerator_metric_id ?? "");
  const [denominatorId, setDenominatorId] = useState(m.agg_denominator_metric_id ?? "");
  const [format, setFormat] = useState(m.format ?? "number");
  const [formatDecimals, setFormatDecimals] = useState(m.format_decimals ?? 0);
  const [formatCurrency, setFormatCurrency] = useState(m.format_currency ?? "$");
  const [recalcResults, setRecalcResults] = useState<RecalcRow[] | null>(null);

  // Validate formula refs via backend parser (handles both =ident and {ident} syntax).
  const { data: formulaRefsData } = useQuery({
    queryKey: ["formula-refs", formula],
    queryFn: () => api.formulaRefs(formula),
    enabled: !m.is_input && formula.trim() !== "",
    staleTime: Infinity,
  });
  const formulaRefs = formulaRefsData?.refs ?? [];
  const invalidRefs = formulaRefs.filter(ref => !allMetrics.some(x => x.name === ref));
  const canSave = name.trim() !== "" && (m.is_input || (formula.trim() !== "" && invalidRefs.length === 0));

  const update = useMutation({
    mutationFn: () => api.updateMetric(m.id, {
      name, formula, agg_rule: aggRule,
      agg_numerator_metric_id: aggRule === "rate" ? numeratorId : "",
      agg_denominator_metric_id: aggRule === "rate" ? denominatorId : "",
      format, format_decimals: formatDecimals, format_currency: formatCurrency,
    }),
    onSuccess: (data) => {
      qc.invalidateQueries({ queryKey: ["dev-model"] });
      qc.invalidateQueries({ queryKey: ["grid"] });
      setEditing(false);
      if (data.recalc && data.recalc.length > 0) setRecalcResults(data.recalc);
    },
  });
  const del = useMutation({
    mutationFn: () => api.deleteMetric(m.id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["dev-model"] }),
  });
  const { confirm, confirmElement } = useConfirm();

  const resultsBanner = recalcResults && (
    <tr style={{ background: "var(--color-success-bg)" }}>
      <td colSpan={6} style={{ padding: "8px 12px" }}>
        <div style={{ display: "flex", alignItems: "flex-start", gap: 12 }}>
          <span style={{ color: "var(--color-live)", fontWeight: 600, fontSize: 12, whiteSpace: "nowrap" }}>Recalc results</span>
          <div style={{ display: "flex", flexDirection: "column", gap: 2, flex: 1 }}>
            {recalcResults.map((r, i) => (
              <span key={i} className="mvx-admin-mono">
                {r.revision} → <strong>{r.metric}</strong> = {r.value != null ? r.value.toLocaleString() : "—"}
              </span>
            ))}
          </div>
          <IconButton aria-label="Dismiss recalc results" size={24} onClick={() => setRecalcResults(null)}>
            <X size={13} />
          </IconButton>
        </div>
      </td>
    </tr>
  );

  if (editing) {
    return (
      <>
        <tr style={{ background: "var(--color-surface-subtle)" }}>
          <td colSpan={6} style={{ padding: "10px 12px" }}>
            <div className="mvx-admin-inline-form">
              <TextInput value={name} onChange={(e) => setName(e.target.value)}
                style={{ width: 160 }} placeholder="name" />
              {!m.is_input && (
                <TextInput value={formula} onChange={(e) => setFormula(e.target.value)}
                  error={invalidRefs.length > 0}
                  style={{ width: 260, fontFamily: "var(--font-mono)" }} placeholder="formula" />
              )}
              <Select value={aggRule} onChange={(e) => setAggRule(e.target.value)} style={{ width: 130 }} aria-label="Aggregation rule">
                <option value="sum">Sum</option>
                <option value="average">Average</option>
                <option value="count">Count</option>
                <option value="rate">Rate</option>
                {/* Calculated metrics only: "formula" means evaluating this
                    metric's formula at the total level, which an input metric
                    has none to do. The server rejects it for inputs too. */}
                {!m.is_input && <option value="formula">Formula</option>}
              </Select>
              {aggRule === "rate" && (
                <RatioOperands
                  metrics={allMetrics}
                  selfId={m.id}
                  numeratorId={numeratorId}
                  denominatorId={denominatorId}
                  onNumerator={setNumeratorId}
                  onDenominator={setDenominatorId}
                />
              )}
              <Select value={format} onChange={(e) => setFormat(e.target.value)} style={{ width: 120 }} aria-label="Format">
                <option value="number">Number</option>
                <option value="percentage">Percentage</option>
                <option value="currency">Currency</option>
                <option value="boolean">Boolean</option>
                <option value="text">Text</option>
              </Select>
              {(format === "number" || format === "percentage" || format === "currency") && (
                <NumberInput min={0} max={4} value={formatDecimals}
                  onChange={e => setFormatDecimals(Math.min(4, Math.max(0, parseInt(e.target.value) || 0)))}
                  style={{ width: 56 }} title="Decimal places" placeholder="0" />
              )}
              {format === "currency" && (
                <TextInput value={formatCurrency} onChange={e => setFormatCurrency(e.target.value)}
                  style={{ width: 52 }} placeholder="$" title="Currency symbol" />
              )}
              <Button
                variant="primary"
                size="sm"
                disabled={!canSave}
                loading={update.isPending}
                loadingLabel="Saving…"
                onClick={() => update.mutate()}
              >
                Save
              </Button>
              <Button size="sm" onClick={() => { setEditing(false); setRecalcResults(null); }}>Cancel</Button>
            </div>
            {invalidRefs.length > 0 && (
              <p className="mvx-admin-error" style={{ marginTop: 4 }}>
                Unknown or invalid references: {invalidRefs.map(r => `{${r}}`).join(", ")}
              </p>
            )}
            {update.isError && <p className="mvx-admin-error" style={{ marginTop: 4 }}>{(update.error as Error).message}</p>}
          </td>
        </tr>
      </>
    );
  }

  return (
    <>
      <tr>
        <td className="mvx-admin-mono">{m.name}</td>
        <td>{m.label}</td>
        <td>
          <div style={{ display: "flex", flexDirection: "column", gap: 3, alignItems: "flex-start" }}>
            <StatusBadge tone={m.is_input ? "info" : "success"}>{m.is_input ? "Input" : "Calc"}</StatusBadge>
            <StatusBadge>{(m.agg_rule ?? "sum").toUpperCase()}</StatusBadge>
            <StatusBadge tone="warning">{fmtBadge(m.format ?? "number", m.format_decimals ?? 0, m.format_currency ?? "$")}</StatusBadge>
          </div>
        </td>
        <td className="mvx-admin-mono mvx-admin-muted">
          <div style={{ display: "flex", alignItems: "center", gap: 6 }}>
            <span>{m.formula ?? (m.depends_on.length ? m.depends_on.join(" + ") : "—")}</span>
            {m.calc_error && (
              <span title={`Calculation failing: ${m.calc_error}`} style={{ display: "inline-flex", color: "var(--color-danger)" }}>
                <AlertTriangle size={13} />
              </span>
            )}
          </div>
        </td>
        <td className="mvx-admin-muted">
          {m.depended_by.length ? m.depended_by.join(", ") : "—"}
        </td>
        <td>
          <div style={{ display: "flex", gap: 4 }}>
            <IconButton aria-label={`Edit metric ${m.name}`} title="Edit" size={26}
              onClick={() => { setEditing(true); setRecalcResults(null); }}>
              <Pencil size={13} />
            </IconButton>
            <IconButton aria-label={`Delete metric ${m.name}`} title="Delete" danger size={26} disabled={del.isPending}
              onClick={() => confirm({ title: "Delete metric?", body: `This removes "${m.name}" from all formulas that reference it.`, confirmLabel: "Delete metric", onConfirm: () => del.mutate() })}>
              <Trash2 size={13} />
            </IconButton>
          </div>
        </td>
      </tr>
      {resultsBanner}
      {confirmElement}
    </>
  );
}

function AddMetricForm({ model, revisionId, onSuccess }: { model: DevModel; revisionId?: string; onSuccess?: () => void }) {
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [isInput, setIsInput] = useState(false);
  const [formula, setFormula] = useState("");
  const [format, setFormat] = useState("number");
  const [formatDecimals, setFormatDecimals] = useState(0);
  const [formatCurrency, setFormatCurrency] = useState("$");
  const [aggRule, setAggRule] = useState("sum");
  const [numeratorId, setNumeratorId] = useState("");
  const [denominatorId, setDenominatorId] = useState("");

  const add = useMutation({
    mutationFn: () => api.addMetric({
      name, is_input: isInput, formula, revision_id: revisionId, agg_rule: aggRule,
      // Only sent for "rate"; any other rule has no operands and the server
      // rejects a pair it did not ask for.
      agg_numerator_metric_id: aggRule === "rate" ? numeratorId : "",
      agg_denominator_metric_id: aggRule === "rate" ? denominatorId : "",
      format, format_decimals: formatDecimals, format_currency: formatCurrency,
    }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["dev-model"] });
      setName(""); setFormula(""); setNumeratorId(""); setDenominatorId("");
      onSuccess?.();
    },
  });

  const refs = formula.match(/\{([^}]+)\}/g)?.map((r) => r.slice(1, -1)) ?? [];
  const unknownRefs = refs.filter((r) => !model.metrics.find((m) => m.name === r));

  return (
    <div style={{ maxWidth: 520 }}>
      <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
        <Field label="Metric name" description="snake_case">
          <TextInput value={name}
            onChange={(e) => setName(e.target.value.toLowerCase().replace(/[^a-z0-9_]/g, "_"))}
            placeholder="e.g. gross_margin" />
        </Field>

        <Field label="Type">
          <SegmentedControl
            aria-label="Metric type"
            segments={[
              { id: "calc", label: "Calc" },
              { id: "input", label: "Input" },
            ]}
            value={isInput ? "input" : "calc"}
            onChange={(v) => { const inputSel = v === "input"; setIsInput(inputSel); if (inputSel) setFormula(""); }}
          />
        </Field>

        <Field label="Format">
          <SegmentedControl
            aria-label="Metric format"
            segments={[
              { id: "number", label: "Number" },
              { id: "percentage", label: "Percentage" },
              { id: "currency", label: "Currency" },
              { id: "boolean", label: "Boolean" },
              { id: "text", label: "Text" },
            ]}
            value={format}
            onChange={setFormat}
          />
        </Field>
        {(format === "number" || format === "percentage" || format === "currency") && (
          <div style={{ display: "flex", gap: 12 }}>
            <Field label="Decimal places">
              <NumberInput min={0} max={4} value={formatDecimals}
                onChange={e => setFormatDecimals(Math.min(4, Math.max(0, parseInt(e.target.value) || 0)))}
                style={{ width: 80 }} />
            </Field>
            {format === "currency" && (
              <Field label="Symbol">
                <TextInput value={formatCurrency} onChange={e => setFormatCurrency(e.target.value)}
                  style={{ width: 70 }} placeholder="$" />
              </Field>
            )}
          </div>
        )}

        {!isInput && (
          <Field label="Formula" description="use {metric_name} references">
            <TextInput value={formula} onChange={(e) => setFormula(e.target.value)}
              placeholder={`e.g. {hc_cost} + {software_cost}`}
              style={{ fontFamily: "var(--font-mono)" }} />
            {refs.length > 0 && (
              <div style={{ marginTop: 6, display: "flex", gap: 6, flexWrap: "wrap" }}>
                {refs.map((r) => {
                  const known = !unknownRefs.includes(r);
                  return (
                    <StatusBadge key={r} tone={known ? "success" : "danger"}>
                      {r} {known ? "✓" : "✗ unknown"}
                    </StatusBadge>
                  );
                })}
              </div>
            )}
          </Field>
        )}

        {!isInput && (
          <Field
            label="Aggregation rule"
            description={aggRule === "formula"
              ? "the total is this metric's formula evaluated against aggregated inputs — use for ratios and percentages, where summing members is meaningless"
              : aggRule === "rate"
              ? "the total is one metric divided by another, so it reflects weight rather than averaging members equally"
              : "how per-member results combine into this metric's total"}
          >
            <Select value={aggRule} onChange={(e) => setAggRule(e.target.value)} style={{ width: 160 }} aria-label="Aggregation rule">
              <option value="sum">Sum</option>
              <option value="average">Average</option>
              <option value="count">Count</option>
              <option value="rate">Rate</option>
              <option value="formula">Formula</option>
            </Select>
          </Field>
        )}

        {!isInput && aggRule === "rate" && (
          <RatioOperands
            metrics={model.metrics}
            numeratorId={numeratorId}
            denominatorId={denominatorId}
            onNumerator={setNumeratorId}
            onDenominator={setDenominatorId}
          />
        )}

        <Button
          variant="primary"
          style={{ alignSelf: "flex-start" }}
          leadingIcon={add.isSuccess ? <Check size={14} /> : undefined}
          disabled={!name || (!isInput && !formula) || unknownRefs.length > 0 ||
            (aggRule === "rate" && (!numeratorId || !denominatorId))}
          loading={add.isPending}
          loadingLabel="Adding…"
          onClick={() => add.mutate()}
        >
          {add.isSuccess ? "Added" : "Add metric"}
        </Button>

        {add.isError && <p className="mvx-admin-error">{(add.error as Error).message}</p>}
        {add.isSuccess && (
          <p style={{ color: "var(--color-live)", fontSize: 13, margin: 0 }}>
            Metric <code>{name}</code> added.
          </p>
        )}
      </div>
    </div>
  );
}

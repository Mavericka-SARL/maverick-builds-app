import { useMemo, useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { invalidateModelData } from "../modelDataQueries";
import { Plus, X, Pencil, Trash2, Check, AlertTriangle } from "lucide-react";
import { api, type DevModel, type DevMetric, type TimeSummary, TIME_SUMMARIES } from "../../api/client";
import { Toolbar, ToolbarGroup, SearchInput, Button, TextInput, Select, NumberInput, StatusBadge, IconButton, useConfirm, Field, SegmentedControl, FilterChip, TagInput, TagFilter } from "../../ui";

// Time summary (how a metric totals ACROSS its time dimension) is only
// meaningful for a metric on a grid that carries a time dimension, so the
// selector is shown only there. Computed once per tab from the revision's
// grids and dimensions: metric id → true when its grid has a time dimension.
function useTimeDimensionedMetrics(revisionId?: string): Set<string> {
  const { data: grids = [] } = useQuery({ queryKey: ["dev-grids", revisionId], queryFn: () => api.listGrids(revisionId) });
  const { data: dims = [] } = useQuery({ queryKey: ["dev-dimensions", revisionId], queryFn: () => api.getDevDimensions(revisionId) });
  const timeDimIDs = new Set(dims.filter(d => d.dimension_type === "time").map(d => d.id));
  const out = new Set<string>();
  for (const g of grids) {
    if (g.dimension_ids.some(id => timeDimIDs.has(id))) for (const mid of g.metric_ids) out.add(mid);
  }
  return out;
}

const TIME_SUMMARY_HELP: Record<TimeSummary, string> = {
  sum: "flows — revenue, cost, units — add up over periods",
  average: "the mean of the periods",
  min: "the smallest period value",
  max: "the largest period value",
  first: "an opening balance: the first period's value",
  last: "a closing balance: the last period's value",
  none: "a time total is meaningless for this metric and is not shown",
};

function TimeSummarySelect({ value, onChange, width = 130 }: { value: TimeSummary; onChange: (v: TimeSummary) => void; width?: number }) {
  return (
    <Select value={value} onChange={(e) => onChange(e.target.value as TimeSummary)} style={{ width }} aria-label="Time summary"
      title={`Time summary: ${TIME_SUMMARY_HELP[value]}`}>
      {TIME_SUMMARIES.map(t => <option key={t} value={t}>{t === "sum" ? "Time: sum" : `Time: ${t}`}</option>)}
    </Select>
  );
}

function fmtBadge(format: string, decimals: number, currency: string): string {
  switch (format) {
    case "percentage": return decimals > 0 ? `%.${decimals}` : "%";
    case "currency":   return `${currency || "$"}${decimals > 0 ? `.${decimals}` : ""}`;
    case "boolean":    return "Y/N";
    case "text":       return "TXT";
    case "picklist":   return "LIST";
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
  const timeMetrics = useTimeDimensionedMetrics(revisionId);
  // Same query key as useTimeDimensionedMetrics, so this reads the cache.
  const { data: revisionDims = [] } = useQuery({ queryKey: ["dev-dimensions", revisionId], queryFn: () => api.getDevDimensions(revisionId) });
  const dimNames = revisionDims.map(d => d.name);
  const [showAdd, setShowAdd] = useState(false);
  const [search, setSearch] = useState("");
  const [filterTag, setFilterTag] = useState<string | null>(null);
  const allTags = useMemo(() => [...new Set(model.metrics.flatMap(m => m.tags ?? []))].sort(), [model.metrics]);
  const toggleTag = (t: string) => setFilterTag(cur => cur === t ? null : t);

  const q = search.trim().toLowerCase();
  const match = (m: DevMetric) =>
    (!filterTag || (m.tags ?? []).includes(filterTag)) && (
      !q ||
      m.name.toLowerCase().includes(q) ||
      m.label?.toLowerCase().includes(q) ||
      m.formula?.toLowerCase().includes(q) ||
      (m.tags ?? []).some(t => t.includes(q)));
  const filtering = !!q || !!filterTag;

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
          <TagFilter tags={allTags} active={filterTag} onChange={setFilterTag} />
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
            {inputs.map((m) => <MetricRow key={m.id} m={m} modelId={model.model_id} allMetrics={model.metrics} dimNames={dimNames} dims={revisionDims} onTimeGrid={timeMetrics.has(m.id)} activeTag={filterTag} onTagClick={toggleTag} />)}
            {inputs.length > 0 && calcs.length > 0 && (
              <tr><td colSpan={6} className="mvx-table__group-row">Calculated</td></tr>
            )}
            {calcs.map((m) => <MetricRow key={m.id} m={m} modelId={model.model_id} allMetrics={model.metrics} dimNames={dimNames} dims={revisionDims} onTimeGrid={timeMetrics.has(m.id)} activeTag={filterTag} onTagClick={toggleTag} />)}
            {inputs.length === 0 && calcs.length === 0 && filtering && (
              <tr><td colSpan={6} style={{ padding: 20, textAlign: "center" }} className="mvx-admin-muted">
                {q ? `No metrics match "${search.trim()}"${filterTag ? ` with tag "${filterTag}"` : ""}` : `No metrics have tag "${filterTag}"`}
              </td></tr>
            )}
          </tbody>
        </table>
      </div>

      <div className="mvx-admin-muted" style={{ marginTop: 12 }}>
        {filtering
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

type RecalcRow = { revision_id: string; metric: string; value: number | null };

// useUnknownFormulaNames lists the names in a formula that are neither a
// metric nor a dimension of the revision, from the server's own parser
// (/api/formula/refs: plain and {name} references, keyword arguments left
// out), matched regardless of case as the save matches them. A formula may
// name dimensions too (`region = "EMEA"`, LOOKUP's dimension arguments). It
// is only a hint and never blocks saving: the server validates on save and
// its 400 message is shown — blocking once made every formula that mentions a
// dimension impossible to edit.
function useUnknownFormulaNames(formula: string, metricNames: string[], dimNames: string[], enabled: boolean): string[] {
  const { data } = useQuery({
    queryKey: ["formula-refs", formula],
    queryFn: () => api.formulaRefs(formula),
    enabled: enabled && formula.trim() !== "",
    staleTime: Infinity,
  });
  const known = new Set([...metricNames, ...dimNames].map(n => n.toLowerCase()));
  return (data?.refs ?? []).filter(ref => !known.has(ref.toLowerCase()));
}

function MetricRow({ m, modelId, allMetrics, dimNames, dims, onTimeGrid, activeTag, onTagClick }: {
  m: DevMetric;
  modelId: string;
  allMetrics: DevMetric[];
  dimNames: string[];
  dims: { id: string; name: string }[];
  onTimeGrid: boolean;
  activeTag: string | null;
  onTagClick: (tag: string) => void;
}) {
  const qc = useQueryClient();
  const [editing, setEditing] = useState(false);
  const [name, setName] = useState(m.name);
  const [label, setLabel] = useState(m.label_set ? m.label : "");
  const [formula, setFormula] = useState(m.formula ?? "");
  const [aggRule, setAggRule] = useState(m.agg_rule ?? "sum");
  const [timeSummary, setTimeSummary] = useState<TimeSummary>(m.time_summary ?? "sum");
  const [numeratorId, setNumeratorId] = useState(m.agg_numerator_metric_id ?? "");
  const [denominatorId, setDenominatorId] = useState(m.agg_denominator_metric_id ?? "");
  const [format, setFormat] = useState(m.format ?? "number");
  const [formatDecimals, setFormatDecimals] = useState(m.format_decimals ?? 0);
  const [formatCurrency, setFormatCurrency] = useState(m.format_currency ?? "$");
  const [picklistDim, setPicklistDim] = useState(m.picklist_dimension_id ?? "");
  const [tags, setTags] = useState<string[]>(m.tags ?? []);
  const [recalcResults, setRecalcResults] = useState<RecalcRow[] | null>(null);
  // The recalc reports revision ids; the banner names them. Fetched only
  // while a banner is showing, for this metric's own model.
  const { data: modelRevisions = [] } = useQuery({
    queryKey: ["dev-revisions", modelId],
    queryFn: () => api.getDevRevisions(modelId),
    enabled: !!recalcResults && !!modelId,
  });
  const revisionLabel = (id: string) => modelRevisions.find(r => r.id === id)?.name ?? id.slice(0, 8);

  const invalidRefs = useUnknownFormulaNames(formula, allMetrics.map(x => x.name), dimNames, !m.is_input);
  const isPicklist = format === "picklist";
  const canSave = name.trim() !== "" && (m.is_input || formula.trim() !== "") && (!isPicklist || picklistDim !== "");

  const update = useMutation({
    mutationFn: () => api.updateMetric(m.id, {
      name, label, formula,
      // A pick-list never totals: "none", or "formula" for a calculated one.
      agg_rule: isPicklist && aggRule !== "formula" ? "none" : aggRule,
      agg_numerator_metric_id: aggRule === "rate" ? numeratorId : "",
      agg_denominator_metric_id: aggRule === "rate" ? denominatorId : "",
      format, format_decimals: formatDecimals, format_currency: formatCurrency,
      time_summary: isPicklist ? "none" : timeSummary, tags,
      picklist_dimension_id: isPicklist ? picklistDim : "",
    }),
    onSuccess: (data) => {
      qc.invalidateQueries({ queryKey: ["dev-model"] });
      invalidateModelData(qc);
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
                {revisionLabel(r.revision_id)} → <strong>{r.metric}</strong> = {r.value != null ? r.value.toLocaleString() : "—"}
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
              <TextInput value={label} onChange={(e) => setLabel(e.target.value)} aria-label="Display name"
                style={{ width: 160 }} placeholder="display name (optional)" />
              {!m.is_input && (
                <TextInput value={formula} onChange={(e) => setFormula(e.target.value)}
                  error={invalidRefs.length > 0}
                  style={{ width: 260, fontFamily: "var(--font-mono)" }} placeholder="formula" />
              )}
              <Select value={aggRule} onChange={(e) => setAggRule(e.target.value)} style={{ width: 130 }} aria-label="Aggregation rule">
                {!isPicklist && <>
                  <option value="sum">Sum</option>
                  <option value="average">Average</option>
                  <option value="count">Count</option>
                  <option value="rate">Rate</option>
                </>}
                {/* Calculated metrics only: "formula" means evaluating this
                    metric's formula at the total level, which an input metric
                    has none to do. The server rejects it for inputs too. */}
                {!m.is_input && <option value="formula">Formula</option>}
                <option value="none">None (no total)</option>
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
              {onTimeGrid && !isPicklist && <TimeSummarySelect value={timeSummary} onChange={setTimeSummary} />}
              <Select value={format} onChange={(e) => {
                setFormat(e.target.value);
                if (e.target.value === "picklist" && aggRule !== "formula" && aggRule !== "none") setAggRule(m.is_input ? "none" : "formula");
              }} style={{ width: 120 }} aria-label="Format">
                <option value="number">Number</option>
                <option value="percentage">Percentage</option>
                <option value="currency">Currency</option>
                <option value="boolean">Boolean</option>
                <option value="text">Text</option>
                <option value="picklist">Pick-list</option>
              </Select>
              {isPicklist && (
                <PicklistDimensionSelect dims={dims} value={picklistDim} onChange={setPicklistDim} />
              )}
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
            <div style={{ display: "flex", gap: 6, alignItems: "center", flexWrap: "wrap", marginTop: 8 }}>
              <span className="mvx-admin-muted">Tags:</span>
              <TagInput value={tags} onChange={setTags} inputWidth={110} />
            </div>
            {invalidRefs.length > 0 && (
              <p className="mvx-admin-muted" style={{ marginTop: 4 }}>
                Not a metric or dimension in this revision: {invalidRefs.join(", ")}
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
        <td>
          <div>{m.label}</div>
          {(m.tags ?? []).length > 0 && (
            <div style={{ display: "flex", gap: 4, flexWrap: "wrap", marginTop: 4 }}>
              {(m.tags ?? []).map(t => (
                <FilterChip key={t} active={t === activeTag} onClick={() => onTagClick(t)} title={t === activeTag ? "Clear tag filter" : "Filter by this tag"}>
                  {t}
                </FilterChip>
              ))}
            </div>
          )}
        </td>
        <td>
          <div style={{ display: "flex", flexDirection: "column", gap: 3, alignItems: "flex-start" }}>
            <StatusBadge tone={m.is_input ? "info" : "success"}>{m.is_input ? "Input" : "Calc"}</StatusBadge>
            <StatusBadge>{(m.agg_rule ?? "sum").toUpperCase()}</StatusBadge>
            {onTimeGrid && (m.time_summary ?? "sum") !== "sum" && (
              <span title={`Time summary: ${TIME_SUMMARY_HELP[m.time_summary ?? "sum"]}`}>
                <StatusBadge tone="brand">time: {m.time_summary}</StatusBadge>
              </span>
            )}
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
              onClick={() => { setEditing(true); setRecalcResults(null); setTags(m.tags ?? []); }}>
              <Pencil size={13} />
            </IconButton>
            <IconButton aria-label={`Delete metric ${m.name}`} title="Delete" danger size={26} disabled={del.isPending}
              onClick={() => confirm({
                title: "Delete metric?",
                // The server refuses the delete while another metric reads
                // this one (METRIC_IN_USE), in a formula or as a Rate operand.
                body: m.depended_by.length
                  ? `"${m.name}" is read by ${m.depended_by.join(", ")}. The delete is refused until no metric reads it: change those formulas first.`
                  : `This permanently removes "${m.name}".`,
                confirmLabel: "Delete metric", onConfirm: () => del.mutate(),
              })}>
              <Trash2 size={13} />
            </IconButton>
          </div>
        </td>
      </tr>
      {resultsBanner}
      {del.isError && (
        <tr>
          <td colSpan={6} className="mvx-admin-error" style={{ padding: "6px 12px" }}>{(del.error as Error).message}</td>
        </tr>
      )}
      {confirmElement}
    </>
  );
}

// The dimension whose members a pick-list's cells hold.
function PicklistDimensionSelect({ dims, value, onChange, width = 160 }: {
  dims: { id: string; name: string }[]; value: string; onChange: (v: string) => void; width?: number;
}) {
  return (
    <Select value={value} onChange={(e) => onChange(e.target.value)} style={{ width }} aria-label="Pick-list dimension"
      title="Each cell holds one member of this dimension">
      <option value="">Members of…</option>
      {dims.map(d => <option key={d.id} value={d.id}>{d.name}</option>)}
    </Select>
  );
}

function AddMetricForm({ model, revisionId, onSuccess }: { model: DevModel; revisionId?: string; onSuccess?: () => void }) {
  const qc = useQueryClient();
  const [name, setName] = useState("");
  const [label, setLabel] = useState("");
  const [isInput, setIsInput] = useState(false);
  const [formula, setFormula] = useState("");
  const [timeSummary, setTimeSummary] = useState<TimeSummary>("sum");
  // A new metric is not on any grid yet; offer the time summary whenever the
  // revision has a time dimension it could land on.
  const { data: revisionDims = [] } = useQuery({ queryKey: ["dev-dimensions", revisionId], queryFn: () => api.getDevDimensions(revisionId) });
  const hasTimeDim = revisionDims.some(d => d.dimension_type === "time");
  const [format, setFormat] = useState("number");
  const [formatDecimals, setFormatDecimals] = useState(0);
  const [formatCurrency, setFormatCurrency] = useState("$");
  const [aggRule, setAggRule] = useState("sum");
  const [numeratorId, setNumeratorId] = useState("");
  const [denominatorId, setDenominatorId] = useState("");
  const [picklistDim, setPicklistDim] = useState("");
  const [tags, setTags] = useState<string[]>([]);
  const isPicklist = format === "picklist";

  const add = useMutation({
    mutationFn: () => api.addMetric({
      name, label, is_input: isInput, formula, revision_id: revisionId,
      // A pick-list never totals: "none", or "formula" for a calculated one.
      agg_rule: !isPicklist ? aggRule : isInput || aggRule === "none" ? "none" : "formula",
      // Only sent for "rate"; any other rule has no operands and the server
      // rejects a pair it did not ask for.
      agg_numerator_metric_id: aggRule === "rate" ? numeratorId : "",
      agg_denominator_metric_id: aggRule === "rate" ? denominatorId : "",
      format, format_decimals: formatDecimals, format_currency: formatCurrency,
      time_summary: isPicklist ? "none" : timeSummary, tags,
      picklist_dimension_id: isPicklist ? picklistDim : undefined,
    }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["dev-model"] });
      setName(""); setLabel(""); setFormula(""); setNumeratorId(""); setDenominatorId(""); setTimeSummary("sum"); setTags([]); setPicklistDim("");
      onSuccess?.();
    },
  });

  const unknownNames = useUnknownFormulaNames(formula, model.metrics.map(m => m.name), revisionDims.map(d => d.name), !isInput);

  return (
    <div style={{ maxWidth: 520 }}>
      <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
        <Field label="Metric name" description="snake_case">
          <TextInput value={name}
            onChange={(e) => setName(e.target.value.toLowerCase().replace(/[^a-z0-9_]/g, "_"))}
            placeholder="e.g. gross_margin" />
        </Field>
        <Field label="Display name" description="optional — shown in grids, charts and KPIs; defaults to the name">
          <TextInput value={label} onChange={(e) => setLabel(e.target.value)}
            placeholder="e.g. Gross Margin %" />
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
              { id: "picklist", label: "Pick-list" },
            ]}
            value={format}
            onChange={setFormat}
          />
        </Field>
        {isPicklist && (
          <Field label="Members of" description="each cell holds one member of this dimension — a drop-down list such as Yes/No, a status, or a region">
            <PicklistDimensionSelect dims={revisionDims} value={picklistDim} onChange={setPicklistDim} width={220} />
          </Field>
        )}
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
          <Field label="Formula" description="name metrics and the cell's dimensions, e.g. revenue - cost">
            <TextInput value={formula} onChange={(e) => setFormula(e.target.value)}
              placeholder="e.g. revenue - cost"
              style={{ fontFamily: "var(--font-mono)" }} />
            {unknownNames.length > 0 && (
              <p className="mvx-admin-muted" style={{ marginTop: 6, marginBottom: 0 }}>
                Not a metric or dimension in this revision: {unknownNames.join(", ")}
              </p>
            )}
          </Field>
        )}

        {!(isInput && isPicklist) && (
          <Field
            label="Aggregation rule"
            description={aggRule === "formula"
              ? "the total is this metric's formula evaluated against aggregated inputs — use for ratios and percentages, where summing members is meaningless"
              : aggRule === "rate"
              ? "the total is one metric divided by another, so it reflects weight rather than averaging members equally"
              : aggRule === "none"
              ? "no total: values at the members only, nothing on total rows — an index, a correction %, a pick-list"
              : "how per-member results combine into this metric's total"}
          >
            <Select value={aggRule} onChange={(e) => setAggRule(e.target.value)} style={{ width: 160 }} aria-label="Aggregation rule">
              {!isPicklist && <>
                <option value="sum">Sum</option>
                <option value="average">Average</option>
                <option value="count">Count</option>
                <option value="rate">Rate</option>
              </>}
              {!isInput && <option value="formula">Formula</option>}
              <option value="none">None (no total)</option>
            </Select>
          </Field>
        )}

        {aggRule === "rate" && (
          <RatioOperands
            metrics={model.metrics}
            numeratorId={numeratorId}
            denominatorId={denominatorId}
            onNumerator={setNumeratorId}
            onDenominator={setDenominatorId}
          />
        )}

        {hasTimeDim && !isPicklist && (
          <Field label="Time summary" description={`across the time dimension: ${TIME_SUMMARY_HELP[timeSummary]}`}>
            <TimeSummarySelect value={timeSummary} onChange={setTimeSummary} width={160} />
          </Field>
        )}

        <Field label="Tags">
          <TagInput value={tags} onChange={setTags} />
        </Field>

        <Button
          variant="primary"
          style={{ alignSelf: "flex-start" }}
          leadingIcon={add.isSuccess ? <Check size={14} /> : undefined}
          disabled={!name || (!isInput && !formula) || (isPicklist && !picklistDim) ||
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

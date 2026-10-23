import { useState, useCallback, useMemo, useRef } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { FileSpreadsheet, FileText, FolderOpen, ArrowLeft, ArrowRight, History, Pencil, Trash2, Play, Plus as PlusIcon, X as XIcon } from "lucide-react";
import { Button, IconButton, TextInput, Select, Field, StatusBadge, Stepper, InlineAlert, FilterChip, useConfirm, type DesignTone } from "../../ui";
import { GoogleServiceAccountPanel } from "./GoogleServiceAccountPanel";
import { fileToBase64 } from "../business/blobUtils";
import { ShapeStep, type Shaped, type ShapeSource } from "./ShapeStep";
import { invalidateModelData } from "../modelDataQueries";
import {
  api,
  type ApiError,
  type IntegrationDef,
  type GridDef,
  type FormDef,
  type DevDimension,
  type DevMetric,
  type DevModel,
  type DevRevision,
  type FileReshape,
  type ImportCheck,
  describeReshape,
  cleanReshape,
  isEmptyReshape,
} from "../../api/client";

// ── Types ──────────────────────────────────────────────────────────────────────

type WizardStep = "upload" | "shape" | "map" | "validate" | "commit";
type TargetType = "grid" | "form" | "dimension";
type MappingStatus = "auto" | "manual" | "ignored";

interface ParsedFile {
  name: string;
  ext: "csv" | "xlsx";
  sheets: string[];
  activeSheet: string;
  headers: string[];
  rows: string[][];
  // lines[i] is rows[i]'s line in the file (the Shape step's, from the
  // server), which every step names a row by.
  lines?: number[];
  // The file as it came, for the Shape step's server preview.
  source?: ShapeSource;
}

interface FieldOption {
  value: string;
  label: string;
  required: boolean;
  group?: string; // fields sharing a group are alternatives; at least one must be mapped
  groupLabel?: string; // how a missing group is named; its fields' labels by default
  requires?: string; // a field that is mapped only together with this one
}

interface ColMapping {
  sourceCol: string;
  targetField: string;
  status: MappingStatus;
}

interface ValError {
  row: number; // the row's line in the file
  index: number; // the row in ParsedFile.rows; -1 for the file as a whole
  col: string;
  value: string;
  issue: string;
  sev: "error" | "warning";
}

interface WizardConfig {
  targetType: TargetType;
  targetId: string;
  targetLabel: string;
  revisionId?: string;
  revisionName?: string;
  importMode: "incremental" | "replace" | "full_reload";
  integrationId?: string;
  integrationName?: string;
  // Resuming a saved DRAFT integration: finishing the wizard activates it.
  draftId?: string;
  // Editing a saved integration's shape and mapping on a sample file:
  // "Save changes" writes them back; nothing need be imported.
  editIntegration?: IntegrationDef;
  // The draft's saved reshape, written back when the wizard finishes it (the
  // config is replaced whole, and the wizard has no editor for it).
  reshape?: FileReshape;
  // "file" (default) uploads a local .csv/.xlsx; "google_sheets" fetches a
  // link-shared sheet server-side and then flows through the same steps.
  source?: "file" | "google_sheets";
  sheetUrl?: string;
  // Running a saved integration: its column map, which the run applies on
  // the server, is the mapping the map step starts from.
  savedColumnMap?: Record<string, string>;
}

interface CommitResult {
  rowsImported: number;
  values?: number; // a grid's: a row of a wide file is one value per metric
  errorRows: number;
  status: "success" | "error";
  message?: string;
  // A refused import's rows, as the server reported them.
  rowErrors?: Array<{ row: number; column: string; message: string; raw_value?: string }>;
}

// ── Parsing utilities ──────────────────────────────────────────────────────────

function parseCSVText(text: string): { headers: string[]; rows: string[][] } {
  const parseRow = (line: string): string[] => {
    const cells: string[] = [];
    let cur = "", inQ = false;
    for (let i = 0; i < line.length; i++) {
      const ch = line[i];
      if (ch === '"') {
        if (inQ && line[i + 1] === '"') { cur += '"'; i++; }
        else inQ = !inQ;
      } else if (ch === "," && !inQ) {
        cells.push(cur.trim()); cur = "";
      } else cur += ch;
    }
    cells.push(cur.trim());
    return cells;
  };
  const lines = text.split(/\r?\n/).filter(l => l.trim());
  if (!lines.length) return { headers: [], rows: [] };
  return { headers: parseRow(lines[0]), rows: lines.slice(1).map(parseRow) };
}

async function parseFile(file: File, sheet?: string): Promise<ParsedFile> {
  const isXlsx = /\.(xlsx|xls)$/i.test(file.name);
  if (isXlsx) {
    // Read by the gateway, as the import itself reads it: the browser used
    // to parse the chosen workbook with SheetJS, which no scanner covers.
    const xlsxBase64 = await fileToBase64(file);
    const wb = await api.parseWorkbook(xlsxBase64, sheet);
    return { name: file.name, ext: "xlsx", sheets: wb.sheets, activeSheet: wb.sheet, headers: wb.headers, rows: wb.rows,
      source: { xlsx_base64: xlsxBase64, sheet: wb.sheet } };
  } else {
    const text = await file.text();
    const { headers, rows } = parseCSVText(text);
    return { name: file.name, ext: "csv", sheets: [], activeSheet: "", headers, rows, source: { csv: text } };
  }
}

// ── Auto-mapping ───────────────────────────────────────────────────────────────

// Each target is given to one column at most. Every column naming a target
// exactly is matched first; only then may a column naming one loosely
// ("Region code" for region) take a target still free, and only when exactly
// one fits. A first-come loose match mapped parent_code onto code, because
// "parentcode" contains "code", and the dimension import then took each
// member's parent as its code: members relabelled, the new ones never made.
function autoMap(headers: string[], opts: FieldOption[]): ColMapping[] {
  const norm = (s: string) => s.toLowerCase().replace(/[\s_-]+/g, "");
  // A target's names: its value, its label, and the label without a
  // "dim: " or "metric: " prefix.
  const names = (o: FieldOption) => [o.value, o.label, o.label.replace(/^[^:]*:\s*/, "")].map(norm).filter(Boolean);
  const taken = new Set<string>();
  const target = new Map<string, string>();
  const claim = (col: string, o: FieldOption) => { taken.add(o.value); target.set(col, o.value); };
  headers.forEach(col => {
    const cn = norm(col);
    const exact = cn ? opts.find(o => !taken.has(o.value) && names(o).includes(cn)) : undefined;
    if (exact) claim(col, exact);
  });
  headers.forEach(col => {
    const cn = norm(col);
    if (target.has(col) || !cn) return;
    const fits = opts.filter(o => !taken.has(o.value) && names(o).some(n => n.includes(cn) || cn.includes(n)));
    if (fits.length === 1) claim(col, fits[0]);
  });
  return headers.map(col => ({
    sourceCol: col,
    targetField: target.get(col) ?? "",
    status: (target.has(col) ? "auto" : "ignored") as MappingStatus,
  }));
}

// The column map an integration saves: every column of the file, a column
// left unmapped as "ignore". A column the map does not name is imported
// under its own header when the integration runs, so an unnamed one would
// be imported by the run although the wizard left it out.
function columnMapOf(mappings: ColMapping[]): Record<string, string> {
  return Object.fromEntries(mappings.map(m => [m.sourceCol, m.targetField || "ignore"]));
}

// ── Target field builders ──────────────────────────────────────────────────────

function getFieldOptions(
  cfg: WizardConfig,
  grids: GridDef[],
  forms: FormDef[],
  dims: DevDimension[],
  knownProps: string[] = [],
  metrics: DevMetric[] = [],
): FieldOption[] {
  const opts: FieldOption[] = [];
  if (cfg.targetType === "grid") {
    const grid = grids.find(g => g.id === cfg.targetId);
    // A grid's values come in one column per metric — the layout of the
    // grid's export and of the dashboard import — or in a metric column
    // naming each row's metric beside a value column.
    const values = { required: false, group: "values", groupLabel: "a metric column, or metric and value" };
    opts.push({ value: "metric", label: "metric", ...values, requires: "value" });
    opts.push({ value: "value", label: "value", ...values, requires: "metric" });
    const gridMetricIds = new Set(grid?.metric_ids ?? []);
    metrics
      .filter(m => m.is_input && (!grid || gridMetricIds.has(m.id)))
      .forEach(m => opts.push({ value: m.name, label: `metric: ${m.label || m.name}`, ...values }));
    const gridDimIds = new Set(grid?.dimension_ids ?? []);
    dims
      .filter(d => !grid || gridDimIds.has(d.id))
      .forEach(d => {
        opts.push({ value: d.name, label: `dim: ${d.name}`, required: false, group: `dim_${d.name}` });
      });
  } else if (cfg.targetType === "form") {
    const form = (forms as FormDef[]).find(f => f.id === cfg.targetId);
    form?.fields.forEach(f =>
      opts.push({ value: f.name, label: f.label || f.name, required: f.required })
    );
  } else if (cfg.targetType === "dimension") {
    // code is optional: rows without one get a code auto-generated from the
    // label server-side (stable slug, reused on re-import).
    opts.push({ value: "code", label: "code", required: false });
    opts.push({ value: "label", label: "label", required: true });
    opts.push({ value: "parent_code", label: "parent_code", required: false });
    // Existing properties — declared on the dimension or already present on
    // its members — as ready-made targets; a brand-new one can still be
    // typed via the "property…" option in the map step.
    for (const name of knownProps) {
      opts.push({ value: "property:" + name, label: "property: " + name, required: false });
    }
  }
  // Deduplicate by value in case the API returns the same item more than once
  const seen = new Set<string>();
  return opts.filter(o => {
    if (seen.has(o.value)) return false;
    seen.add(o.value);
    return true;
  });
}

// ── CSV serialiser with name→ID resolution ────────────────────────────────────

interface ResolveContext {
  metrics: DevMetric[];
  dims: DevDimension[];
}

function resolveAndSerialize(
  headers: string[],
  rows: string[][],
  mappings: ColMapping[],
  ctx: ResolveContext,
): string {
  const escape = (v: string) => /[,"\n]/.test(v) ? `"${v.replace(/"/g, '""')}"` : v;
  const active = mappings.filter(m => m.targetField);

  // A metric named by its label is sent by its name, which the server
  // resolves in the target revision itself. The wizard used to send the
  // metric's id from the metrics it had loaded, so a stale list sent ids the
  // revision no longer has and failed every row.
  const metricName = new Map(ctx.metrics.map(m => [m.name.toLowerCase(), m.name]));
  for (const m of ctx.metrics) {
    if (m.label && !metricName.has(m.label.toLowerCase())) metricName.set(m.label.toLowerCase(), m.name);
  }
  const resolveMetric = (v: string) => metricName.get(v.trim().toLowerCase()) ?? v;

  // Build dimension member lookups: dim.name → Map(label.lower → code)
  const dimLabelToCode = new Map<string, Map<string, string>>();
  for (const d of ctx.dims) {
    const lookup = new Map(d.members.map(mb => [mb.label.toLowerCase(), mb.code]));
    dimLabelToCode.set(d.name, lookup);
  }

  // Build dim code sets for auto-detection: dim.name → Set(code.lower)
  const dimCodeSet = new Map<string, Set<string>>();
  for (const d of ctx.dims) {
    dimCodeSet.set(d.name, new Set(d.members.map(mb => mb.code.toLowerCase())));
  }

  // Determine output header name and a value transformer per active mapping
  const cols = active.map(m => {
    const tf = m.targetField;
    const srcIdx = headers.indexOf(m.sourceCol);

    // "metric" (unified) or legacy "metric_id"/"metric_name": the metric's name
    if (tf === "metric" || tf === "metric_id" || tf === "metric_name") {
      return { header: "metric_id", srcIdx, transform: resolveMetric };
    }

    // Legacy "_label" suffix: resolve label → code
    if (tf.endsWith("_label")) {
      const dimName = tf.slice(0, -"_label".length);
      const lookup = dimLabelToCode.get(dimName);
      return { header: dimName, srcIdx, transform: (v: string) => lookup?.get(v.toLowerCase()) ?? v };
    }

    // Dimension name (unified): auto-detect — if value matches a known label, convert to code;
    // if it already matches a code, pass through; otherwise pass through unchanged.
    if (dimLabelToCode.has(tf)) {
      const labelLookup = dimLabelToCode.get(tf)!;
      const codes = dimCodeSet.get(tf)!;
      return {
        header: tf,
        srcIdx,
        transform: (v: string) => {
          const lo = v.toLowerCase();
          if (codes.has(lo)) return v; // already a code
          return labelLookup.get(lo) ?? v; // try label→code, else pass through
        },
      };
    }

    // All other fields: pass through unchanged
    return { header: tf, srcIdx, transform: (v: string) => v };
  });

  const lines = [cols.map(c => escape(c.header)).join(",")];
  for (const r of rows) {
    lines.push(cols.map(c => escape(c.transform(r[c.srcIdx] ?? ""))).join(","));
  }
  return lines.join("\n");
}

// ── Validation ─────────────────────────────────────────────────────────────────

// lineOf is row i's line in the file.
function lineOf(pf: ParsedFile, i: number): number {
  return pf.lines?.[i] ?? i + 2;
}

function runValidation(
  pf: ParsedFile,
  mappings: ColMapping[],
  reqFields: string[],
): ValError[] {
  const errs: ValError[] = [];
  const reqMapped = mappings.filter(m => m.targetField && reqFields.includes(m.targetField));
  pf.rows.forEach((row, ri) => {
    if (row.every(v => !v.trim())) {
      errs.push({ row: lineOf(pf, ri), index: ri, col: "—", value: "", issue: "Empty row — will be skipped", sev: "warning" });
      return;
    }
    reqMapped.forEach(m => {
      const idx = pf.headers.indexOf(m.sourceCol);
      const val = (row[idx] ?? "").trim();
      if (!val) {
        errs.push({ row: lineOf(pf, ri), index: ri, col: m.sourceCol, value: "", issue: `Required "${m.targetField}" is empty`, sev: "error" });
      }
    });
  });
  return errs;
}

// checkOnServer asks the server what the import would refuse — a member or
// metric the model does not have, a value that is no number, a table's
// field rules — without importing anything (a dry run). Validate used to
// check only that required fields were filled, so a grid file naming an
// unknown product read "All rows are valid" and the commit refused all of
// it (2026-10-21). The rows go with their lines in the file (row_lines),
// which the server's answers name.
async function checkOnServer(cfg: WizardConfig, pf: ParsedFile, csv: string): Promise<ValError[]> {
  const lines = pf.rows.map((_, i) => lineOf(pf, i));
  const indexOf = new Map(lines.map((l, i) => [l, i]));
  let res: ImportCheck | undefined;
  try {
    if (cfg.integrationId) res = await api.checkIntegrationRun(cfg.integrationId, csv, lines);
    else if (cfg.targetType === "grid") res = await api.checkImport(csv, cfg.revisionId, lines);
    else res = await api.checkFormRecords(cfg.targetId, csv, lines);
  } catch (e) {
    const message = e instanceof Error ? e.message : String(e);
    // Refused as a whole (a column naming no metric or dimension) — or not
    // checked at all, which the commit then does.
    return [(e as ApiError).status === 400
      ? { row: 0, index: -1, col: "—", value: "", issue: message, sev: "error" }
      : { row: 0, index: -1, col: "—", value: "", issue: `The rows could not be checked (${message}); the import checks them when you commit`, sev: "warning" }];
  }
  return (res?.errors ?? []).map(e => (
    { row: e.row, index: indexOf.get(e.row) ?? -1, col: e.column || "—", value: e.raw_value ?? "", issue: e.message, sev: "error" as const }
  ));
}

// savedMode is the import mode a saved integration keeps: a grid's, from
// a file as well as a sheet. A file integration used to keep none and run
// as incremental, whatever was chosen (2026-10-21).
function savedMode(cfg: WizardConfig): { import_mode?: WizardConfig["importMode"] } {
  return cfg.targetType === "grid" || cfg.source === "google_sheets" ? { import_mode: cfg.importMode } : {};
}

const MODE_LABEL: Record<string, string> = {
  incremental: "Incremental (adds to each cell)",
  replace: "Replace listed cells",
  full_reload: "Full reload",
};

// ── Styles ─────────────────────────────────────────────────────────────────────

// ── Step indicator ─────────────────────────────────────────────────────────────

const STEPS: { id: WizardStep; label: string }[] = [
  { id: "upload", label: "Upload" },
  { id: "shape", label: "Shape" },
  { id: "map", label: "Map Columns" },
  { id: "validate", label: "Validate" },
  { id: "commit", label: "Commit" },
];

function StepIndicator({ current }: { current: WizardStep }) {
  return <Stepper steps={STEPS} current={current} />;
}

// ── Upload Step ────────────────────────────────────────────────────────────────

function UploadStep({
  config,
  grids,
  forms,
  dims,
  workingRevisionId,
  onNext,
  onConfigChange,
}: {
  config: WizardConfig;
  grids: GridDef[];
  forms: FormDef[];
  dims: DevDimension[];
  workingRevisionId?: string;
  onNext: (file: ParsedFile) => void;
  onConfigChange: (c: WizardConfig) => void;
}) {
  // The target-revision choices are the revisions of the working revision's
  // model. Unnamed, the server resolved a model of its own (X-Model-Id or
  // the default), so building another model listed the wrong revisions.
  const { data: workingModel } = useQuery({
    queryKey: ["dev-model", workingRevisionId],
    queryFn: () => api.getDevModel(workingRevisionId),
    enabled: !!workingRevisionId,
  });
  const workingModelId = (workingModel as DevModel | undefined)?.model_id;
  const { data: revisions = [] } = useQuery({
    queryKey: ["dev-revisions", workingModelId],
    queryFn: () => api.getDevRevisions(workingModelId),
    enabled: !workingRevisionId || !!workingModelId,
  });
  const [dragging, setDragging] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [parsedFile, setParsedFile] = useState<ParsedFile | null>(null);
  const [sheetUrl, setSheetUrl] = useState(config.sheetUrl ?? "");
  const fileRef = useRef<HTMLInputElement>(null);
  const rawFileRef = useRef<File | null>(null);
  const fromSheet = config.source === "google_sheets";

  const handleFetchSheet = useCallback(async () => {
    const url = sheetUrl.trim();
    if (!url) return;
    setError(null);
    setLoading(true);
    try {
      const res = await api.fetchSheetPreview(url);
      const { headers, rows } = parseCSVText(res.csv);
      setParsedFile({ name: "Google Sheet", ext: "csv", sheets: [], activeSheet: "", headers, rows, source: { csv: res.csv } });
      onConfigChange({ ...config, sheetUrl: url });
    } catch (e) {
      setParsedFile(null);
      setError(e instanceof Error ? e.message : "Could not fetch the sheet");
    } finally {
      setLoading(false);
    }
  }, [sheetUrl, config, onConfigChange]);

  const handleFile = useCallback(async (file: File) => {
    setError(null);
    setLoading(true);
    rawFileRef.current = file;
    try {
      const pf = await parseFile(file);
      setParsedFile(pf);
    } catch (e) {
      setError(`Could not parse file: ${e instanceof Error ? e.message : String(e)}`);
    } finally {
      setLoading(false);
    }
  }, []);

  const handleSheetChange = useCallback(async (sheet: string) => {
    if (!rawFileRef.current) return;
    setLoading(true);
    try {
      const pf = await parseFile(rawFileRef.current, sheet);
      setParsedFile(pf);
    } finally {
      setLoading(false);
    }
  }, []);

  const dedupByName = <T extends { id: string; name: string }>(arr: T[]) =>
    arr.filter((x, i, a) => a.findIndex(y => y.name === x.name) === i);
  const targetOpts =
    config.targetType === "grid" ? dedupByName(grids).map(g => ({ id: g.id, label: g.name })) :
    config.targetType === "form" ? dedupByName(forms as (FormDef & { name: string })[]).map(f => ({ id: f.id, label: f.label || f.name })) :
    dedupByName(dims as DevDimension[]).map(d => ({ id: d.id, label: d.name }));

  const canProceed = parsedFile && config.targetId && parsedFile.headers.length > 0
    && (config.targetType !== "grid" || !!config.revisionId);

  return (
    <div>
      {/* Target config */}
      <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 16, marginBottom: config.targetType === "grid" ? 12 : 24 }}>
        <Field label="Import target type">
          <Select
            value={config.targetType}
            onChange={e => onConfigChange({ ...config, targetType: e.target.value as TargetType, targetId: "", targetLabel: "", revisionId: workingRevisionId ?? "", revisionName: "" })}
          >
            <option value="grid">Grid</option>
            <option value="form">Table of records</option>
            <option value="dimension">Dimension</option>
          </Select>
        </Field>
        <Field label={config.targetType === "grid" ? "Grid" : config.targetType === "form" ? "Table" : "Dimension"}>
          <Select
            value={config.targetId}
            onChange={e => {
              const opt = targetOpts.find(o => o.id === e.target.value);
              onConfigChange({ ...config, targetId: e.target.value, targetLabel: opt?.label ?? "" });
            }}
          >
            <option value="">— select —</option>
            {targetOpts.map(o => <option key={o.id} value={o.id}>{o.label}</option>)}
          </Select>
        </Field>
      </div>
      {config.targetType === "grid" && (
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 16, marginBottom: 24 }}>
          <Field label="Target revision" required>
            <Select
              value={config.revisionId ?? ""}
              onChange={e => {
                const rev = (revisions as DevRevision[]).find(r => r.id === e.target.value);
                onConfigChange({ ...config, revisionId: e.target.value, revisionName: rev?.name ?? "" });
              }}
            >
              <option value="">— select revision —</option>
              {(revisions as DevRevision[]).map(r => (
                <option key={r.id} value={r.id}>{r.name}{r.is_active ? " (active)" : ""}</option>
              ))}
            </Select>
          </Field>
          <Field label="Import mode">
            <div style={{ display: "flex", flexDirection: "column", gap: 8, paddingTop: 4 }}>
              {/* A sheet import is meant to be re-run against the same living
                  sheet, so its modes are the idempotent ones — "incremental"
                  would re-add the sheet's values on every sync. */}
              {(fromSheet
                ? [
                    { value: "replace", label: "Sync (replace listed cells)", hint: "The sheet is authoritative for the cells it contains — re-syncing converges instead of double-counting" },
                    { value: "full_reload", label: "Full reload", hint: "Delete all data for this revision, then insert" },
                  ] as const
                : [
                    // A file imported again — a corrected one, or the same
                    // one sent twice — used to have only adding (which
                    // doubled it) or wiping the revision: "Add / update rows"
                    // hid that incremental adds.
                    { value: "incremental", label: "Incremental", hint: "Add each value to the cell's current value — importing the same file again adds it again" },
                    { value: "replace", label: "Replace listed cells", hint: "The file's cells take its values, other cells keep theirs — importing the same file again changes nothing" },
                    { value: "full_reload", label: "Full reload", hint: "Delete all data for this revision, then insert" },
                  ] as const
              ).map(mode => (
                <label key={mode.value} style={{ display: "flex", alignItems: "flex-start", gap: 8, cursor: "pointer" }}>
                  <input
                    type="radio"
                    name="importMode"
                    value={mode.value}
                    checked={config.importMode === mode.value}
                    onChange={() => onConfigChange({ ...config, importMode: mode.value })}
                    style={{ marginTop: 2, flexShrink: 0 }}
                  />
                  <span>
                    <span style={{ fontSize: 13, fontWeight: 500 }}>{mode.label}</span>
                    <br />
                    <span className="mvx-admin-muted" style={{ fontSize: 11 }}>{mode.hint}</span>
                  </span>
                </label>
              ))}
            </div>
          </Field>
        </div>
      )}

      {/* Source: sheet URL or file drop zone */}
      {fromSheet ? (
        <div className="mvx-panel" style={{ padding: 20, marginBottom: 16 }}>
          <Field
            label="Google Sheet URL"
            description={'Link-shared ("Anyone with the link" → Viewer), or a private sheet shared with the tenant\'s Google service account (set up under Google Sheets). The worksheet tab in the URL (gid) is the one fetched.'}
          >
            <div style={{ display: "flex", gap: 8 }}>
              <TextInput
                value={sheetUrl}
                onChange={e => setSheetUrl(e.target.value)}
                placeholder="https://docs.google.com/spreadsheets/d/…"
                style={{ flex: 1 }}
                onKeyDown={e => e.key === "Enter" && handleFetchSheet()}
              />
              <Button variant="primary" loading={loading} loadingLabel="Fetching…" disabled={!sheetUrl.trim()} onClick={handleFetchSheet}>
                Fetch sheet
              </Button>
            </div>
          </Field>
          {parsedFile && (
            <div className="mvx-admin-muted" style={{ marginTop: 8 }}>
              Fetched — {parsedFile.headers.length} columns · {parsedFile.rows.length} data rows. Fetch again to refresh.
            </div>
          )}
        </div>
      ) : (
      <div
        onDragOver={e => { e.preventDefault(); setDragging(true); }}
        onDragLeave={() => setDragging(false)}
        onDrop={e => { e.preventDefault(); setDragging(false); const f = e.dataTransfer.files[0]; if (f) handleFile(f); }}
        onClick={() => fileRef.current?.click()}
        className={["mvx-dropzone", dragging ? "mvx-dropzone--active" : "", parsedFile && !dragging ? "mvx-dropzone--filled" : ""].filter(Boolean).join(" ")}
      >
        <input
          ref={fileRef}
          type="file"
          accept=".csv,.xlsx,.xls"
          style={{ display: "none" }}
          onChange={e => { const f = e.target.files?.[0]; if (f) handleFile(f); }}
        />
        {loading ? (
          <p className="mvx-admin-muted" style={{ margin: 0 }}>Parsing file…</p>
        ) : parsedFile ? (
          <>
            {parsedFile.ext === "xlsx"
              ? <FileSpreadsheet size={32} aria-hidden="true" style={{ marginBottom: 8, color: "var(--color-live)" }} />
              : <FileText size={32} aria-hidden="true" style={{ marginBottom: 8, color: "var(--color-live)" }} />}
            <div style={{ fontWeight: 600 }}>{parsedFile.name}</div>
            <div className="mvx-admin-muted" style={{ marginTop: 4 }}>
              {parsedFile.headers.length} columns · {parsedFile.rows.length} data rows detected
            </div>
            <div className="mvx-admin-muted" style={{ marginTop: 4 }}>Click to change file</div>
          </>
        ) : (
          <>
            <FolderOpen size={36} aria-hidden="true" style={{ marginBottom: 8, color: "var(--color-text-subtle)" }} />
            <div style={{ fontWeight: 500 }}>Drop a file here or click to browse</div>
            <div className="mvx-admin-muted" style={{ marginTop: 6 }}>Supports .csv, .xlsx, .xls</div>
          </>
        )}
      </div>
      )}

      {error && <p className="mvx-admin-error" style={{ marginBottom: 12 }}>{error}</p>}

      {/* Sheet picker for Excel with multiple sheets */}
      {parsedFile?.ext === "xlsx" && parsedFile.sheets.length > 1 && (
        <div style={{ marginBottom: 16, display: "flex", alignItems: "center", gap: 12 }}>
          <span style={{ fontSize: 13, fontWeight: 500, flexShrink: 0 }}>Sheet:</span>
          <Select
            value={parsedFile.activeSheet}
            onChange={e => handleSheetChange(e.target.value)}
            style={{ minWidth: 200 }}
            aria-label="Sheet"
          >
            {parsedFile.sheets.map(s => <option key={s} value={s}>{s}</option>)}
          </Select>
        </div>
      )}

      {/* Preview table */}
      {parsedFile && parsedFile.headers.length > 0 && (
        <div style={{ marginBottom: 20 }}>
          <div className="mvx-prop-section">
            Preview — first {Math.min(5, parsedFile.rows.length)} of {parsedFile.rows.length} rows
          </div>
          <div className="mvx-table-wrap">
            <table className="mvx-table mvx-table--compact">
              <thead>
                <tr>
                  {parsedFile.headers.map(h => (
                    <th key={h} style={{ whiteSpace: "nowrap" }}>{h}</th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {parsedFile.rows.slice(0, 5).map((row, ri) => (
                  <tr key={ri}>
                    {parsedFile.headers.map((_, ci) => (
                      <td key={ci} className="mvx-admin-mono mvx-admin-muted" style={{ whiteSpace: "nowrap" }}>
                        {row[ci] ?? ""}
                      </td>
                    ))}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}

      <div style={{ display: "flex", justifyContent: "flex-end" }}>
        <Button variant="primary" trailingIcon={<ArrowRight size={14} />} disabled={!canProceed}
          onClick={() => {
            if (!parsedFile) return;
            // A target revision taken from the working revision was never
            // picked in the list, so its name (shown on Review) is filled here.
            if (config.revisionId && !config.revisionName) {
              const rev = (revisions as DevRevision[]).find(r => r.id === config.revisionId);
              if (rev) onConfigChange({ ...config, revisionName: rev.name });
            }
            onNext(parsedFile);
          }}>
          Next: Shape
        </Button>
      </div>
    </div>
  );
}

// ── Map Columns Step ───────────────────────────────────────────────────────────

function MapColumnsStep({
  parsedFile,
  fieldOptions,
  mappings,
  onMappingChange,
  onNext,
  onBack,
  onSaveAsIntegration,
  savedIntegrationName,
  allowProperties,
  editingName,
  onSaveChanges,
  savingChanges,
}: {
  parsedFile: ParsedFile;
  fieldOptions: FieldOption[];
  mappings: ColMapping[];
  onMappingChange: (m: ColMapping[]) => void;
  onNext: () => void;
  onBack: () => void;
  onSaveAsIntegration: (name: string, asDraft?: boolean) => void;
  savedIntegrationName?: string;
  // Dimension targets may map a column into an arbitrary member property
  // (dimension_member.properties JSONB) via a typed-in property name.
  allowProperties?: boolean;
  // Editing a saved integration: its shape and mapping are saved back.
  editingName?: string;
  onSaveChanges?: () => void;
  savingChanges?: boolean;
}) {
  const [saveName, setSaveName] = useState(savedIntegrationName ?? "");
  const [showSave, setShowSave] = useState(false);
  const [justSaved, setJustSaved] = useState(false);

  // Ungrouped required fields must each be mapped individually.
  // Grouped fields are alternatives — at least one per group must be mapped.
  const mappedTargets = new Set(mappings.map(m => m.targetField).filter(Boolean));
  const ungroupedOk = fieldOptions
    .filter(f => f.required && !f.group)
    .every(f => mappedTargets.has(f.value));
  const groups = [...new Set(fieldOptions.map(f => f.group).filter(Boolean))] as string[];
  const groupsOk = groups.every(g =>
    fieldOptions.filter(f => f.group === g).some(f => mappedTargets.has(f.value))
  );
  const unnamedProperty = mappings.some(m => m.targetField === "property:");
  // A field mapped without the one it goes with (metric without value).
  const unpaired = fieldOptions.filter(f => f.requires && mappedTargets.has(f.value) && !mappedTargets.has(f.requires));
  // Two columns into one target: the import would keep one of them per row
  // and drop the other without a word. A saved integration's run refuses
  // such a map on the server.
  const sharedTargets = [...mappedTargets]
    .filter(t => t !== "property:")
    .map(t => ({ target: t, cols: mappings.filter(m => m.targetField === t).map(m => m.sourceCol) }))
    .filter(s => s.cols.length > 1);
  const requiredMapped = ungroupedOk && groupsOk && unpaired.length === 0 && sharedTargets.length === 0 && !unnamedProperty;

  const missingLabels = [
    ...fieldOptions.filter(f => f.required && !f.group && !mappedTargets.has(f.value)).map(f => f.label),
    ...groups
      .filter(g => !fieldOptions.filter(f => f.group === g).some(f => mappedTargets.has(f.value)))
      .map(g => fieldOptions.find(f => f.group === g && f.groupLabel)?.groupLabel
        ?? fieldOptions.filter(f => f.group === g).map(f => f.label.split(" —")[0]).join(" or ")),
    ...unpaired.map(f => `${f.requires} (to go with ${f.value})`),
  ];
  const targetLabel = (t: string) => fieldOptions.find(o => o.value === t)?.label ?? t;

  const setTarget = (sourceCol: string, targetField: string) => {
    onMappingChange(mappings.map(m =>
      m.sourceCol === sourceCol
        ? { ...m, targetField, status: "manual" as MappingStatus }
        : m
    ));
  };

  const statusBadge = (status: MappingStatus, targetField: string) => {
    if (!targetField) return <span className="mvx-admin-muted">ignored</span>;
    if (targetField === "property:") return <StatusBadge tone="warning">needs a name</StatusBadge>;
    if (status === "auto") return <StatusBadge tone="success">auto-mapped</StatusBadge>;
    return <StatusBadge>manual</StatusBadge>;
  };

  const sampleValues = (col: string) => {
    const idx = parsedFile.headers.indexOf(col);
    if (idx < 0) return "";
    return parsedFile.rows
      .slice(0, 3)
      .map(r => r[idx] ?? "")
      .filter(Boolean)
      .join(", ");
  };

  const handleSave = (asDraft?: boolean) => {
    if (!saveName.trim()) return;
    onSaveAsIntegration(saveName.trim(), asDraft);
    setJustSaved(true);
    setShowSave(false);
    setTimeout(() => setJustSaved(false), 3000);
  };

  return (
    <div>
      {/* Required fields check */}
      {missingLabels.length > 0 && (
        <InlineAlert tone="warning" className="mvx-toolbar--spaced">
          Map all required target fields before proceeding.
          Still missing: {missingLabels.join(", ")}
        </InlineAlert>
      )}
      {sharedTargets.map(s => (
        <InlineAlert key={s.target} tone="warning" className="mvx-toolbar--spaced">
          Columns {s.cols.map(c => `"${c}"`).join(" and ")} both map to {targetLabel(s.target)} — map all but one of them to (ignore).
        </InlineAlert>
      ))}

      {justSaved && (
        <InlineAlert tone="success" className="mvx-toolbar--spaced">
          Integration saved. This mapping can be reused from the integrations list.
        </InlineAlert>
      )}

      {/* Mapping table */}
      <div className="mvx-table-wrap" style={{ marginBottom: 16 }}>
        <table className="mvx-table mvx-table--compact">
          <thead>
            <tr>
              <th style={{ width: 200 }}>Source Column</th>
              <th>Sample Values</th>
              <th>Map To</th>
              <th style={{ width: 110 }}>Status</th>
            </tr>
          </thead>
          <tbody>
            {mappings.map(m => (
              <tr key={m.sourceCol}>
                <td className="mvx-admin-mono" style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", maxWidth: 200 }}>
                  {m.sourceCol}
                </td>
                <td className="mvx-admin-mono mvx-admin-muted" style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", maxWidth: 240 }}>
                  {sampleValues(m.sourceCol) || "—"}
                </td>
                <td>
                  <div style={{ display: "flex", gap: 6 }}>
                    <Select
                      value={fieldOptions.some(o => o.value === m.targetField) ? m.targetField : m.targetField.startsWith("property:") ? "property:" : m.targetField}
                      onChange={e => setTarget(m.sourceCol, e.target.value)}
                      style={{ width: "100%" }}
                      aria-label={`Map ${m.sourceCol} to`}
                    >
                      <option value="">(ignore)</option>
                      {fieldOptions.map(o => (
                        <option key={o.value} value={o.value}>
                          {o.label}{o.required ? " *" : ""}
                        </option>
                      ))}
                      {allowProperties && <option value="property:">new property…</option>}
                    </Select>
                    {m.targetField.startsWith("property:") && !fieldOptions.some(o => o.value === m.targetField) && (
                      <TextInput
                        value={m.targetField.slice("property:".length)}
                        onChange={e => setTarget(m.sourceCol, "property:" + e.target.value.trim())}
                        placeholder="property name"
                        aria-label={`Property name for ${m.sourceCol}`}
                        style={{ width: 140 }}
                      />
                    )}
                  </div>
                </td>
                <td>{statusBadge(m.status, m.targetField)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>

      {/* Save as integration — or, editing one, save its changes */}
      <div className="mvx-panel" style={{ marginBottom: 20, padding: "12px 16px", background: "var(--color-surface-subtle)" }}>
        {onSaveChanges ? (
          <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between" }}>
            <span className="mvx-admin-muted" style={{ fontSize: 13 }}>Save this shape and mapping to <b>{editingName}</b>; its next runs use them</span>
            <Button size="sm" variant="primary" loading={savingChanges} loadingLabel="Saving…" disabled={!requiredMapped} onClick={onSaveChanges}>
              Save changes
            </Button>
          </div>
        ) : showSave ? (
          <div className="mvx-admin-inline-form" style={{ flexWrap: "nowrap" }}>
            <TextInput
              value={saveName}
              onChange={e => setSaveName(e.target.value)}
              placeholder="Integration name (e.g. Import OPEX Data)"
              style={{ flex: 1 }}
              autoFocus
              onKeyDown={e => e.key === "Enter" && handleSave()}
            />
            <Button variant="primary" size="sm" disabled={!saveName.trim()} onClick={() => handleSave(false)}>Save</Button>
            <Button size="sm" disabled={!saveName.trim()} title="Keep as a work-in-progress draft — finish and activate it later" onClick={() => handleSave(true)}>Save as draft</Button>
            <Button size="sm" onClick={() => setShowSave(false)}>Cancel</Button>
          </div>
        ) : (
          <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between" }}>
            <span className="mvx-admin-muted" style={{ fontSize: 13 }}>Save this column mapping as a reusable integration — or as a draft to finish later</span>
            <Button size="sm" variant="ghost" onClick={() => setShowSave(true)}>
              Save as Integration
            </Button>
          </div>
        )}
      </div>

      <div style={{ display: "flex", justifyContent: "space-between" }}>
        <Button leadingIcon={<ArrowLeft size={14} />} onClick={onBack}>Back</Button>
        <Button variant="primary" trailingIcon={<ArrowRight size={14} />} disabled={!requiredMapped} onClick={onNext}>
          Next: Validate
        </Button>
      </div>
    </div>
  );
}

// ── Validate Step ──────────────────────────────────────────────────────────────

function ValidateStep({
  parsedFile,
  errors,
  checking,
  onNext,
  onBack,
  onBackToShape,
}: {
  parsedFile: ParsedFile;
  errors: ValError[];
  checking: boolean;
  onNext: (importValidOnly: boolean) => void;
  onBack: () => void;
  onBackToShape: () => void;
}) {
  const [filter, setFilter] = useState<"all" | "errors" | "warnings" | "valid">("all");

  const errCount = errors.filter(e => e.sev === "error").length;
  const warnCount = errors.filter(e => e.sev === "warning").length;
  // Rows, not problems: a row can have two.
  const errorRows = new Set(errors.filter(e => e.sev === "error" && e.index >= 0).map(e => e.index));
  const wholeFileError = errors.some(e => e.sev === "error" && e.index < 0);
  const validRows = parsedFile.rows.filter((r, i) => !r.every(v => !v.trim()) && !errorRows.has(i)).length;
  const hasErrors = errCount > 0;

  const shown = errors.filter(e =>
    filter === "all" ? true :
    filter === "errors" ? e.sev === "error" :
    filter === "warnings" ? e.sev === "warning" :
    false
  );

  return (
    <div>
      {/* Summary cards */}
      <div style={{ display: "grid", gridTemplateColumns: "repeat(4, 1fr)", gap: 12, marginBottom: 20 }}>
        {([
          { label: "Total rows", value: parsedFile.rows.length, color: "var(--color-text)", bg: "var(--color-surface-subtle)", border: "var(--color-border)" },
          { label: "Valid", value: Math.max(0, validRows), color: "var(--color-live)", bg: "var(--color-success-bg)", border: "var(--color-success-border)" },
          { label: "Errors", value: errCount, color: "var(--color-danger)", bg: "var(--color-danger-bg)", border: "var(--color-danger-border)" },
          { label: "Warnings", value: warnCount, color: "var(--color-draft)", bg: "var(--color-warning-bg)", border: "var(--color-warning-border)" },
        ]).map(s => (
          <div key={s.label} style={{ padding: "14px 16px", border: `1px solid ${s.border}`, borderRadius: "var(--radius-card)", background: s.bg, textAlign: "center" }}>
            <div style={{ fontSize: 24, fontWeight: 700, color: s.color }}>{s.value}</div>
            <div className="mvx-admin-muted">{s.label}</div>
          </div>
        ))}
      </div>

      {checking && (
        <InlineAlert tone="info" className="mvx-toolbar--spaced">
          Checking the rows as the import will read them…
        </InlineAlert>
      )}
      {errors.length === 0 ? (
        !checking && (
          <InlineAlert tone="success" className="mvx-toolbar--spaced">
            All rows are valid — ready to commit
          </InlineAlert>
        )
      ) : (
        <>
          {/* Filter tabs */}
          <div style={{ display: "flex", gap: 4, marginBottom: 12 }}>
            {(["all", "errors", "warnings"] as const).map(f => (
              <FilterChip key={f} active={filter === f} onClick={() => setFilter(f)}>
                {f === "all" ? `All (${errors.length})` : f === "errors" ? `Errors (${errCount})` : `Warnings (${warnCount})`}
              </FilterChip>
            ))}
          </div>

          {/* Error table */}
          <div className="mvx-table-wrap" style={{ marginBottom: 20 }}>
            <table className="mvx-table mvx-table--compact">
              <thead>
                <tr>
                  {["Row", "Severity", "Column", "Value", "Issue"].map(h => (
                    <th key={h}>{h}</th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {shown.slice(0, 100).map((e, i) => (
                  <tr key={i}>
                    <td className="mvx-admin-mono mvx-admin-muted">{e.index < 0 ? "—" : e.row}</td>
                    <td>
                      <StatusBadge tone={e.sev === "error" ? "danger" : "warning"}>{e.sev}</StatusBadge>
                    </td>
                    <td className="mvx-admin-mono">{e.col}</td>
                    <td className="mvx-admin-mono mvx-admin-muted">{e.value || "—"}</td>
                    <td>{e.issue}</td>
                  </tr>
                ))}
                {shown.length > 100 && (
                  <tr><td colSpan={5} className="mvx-admin-muted" style={{ textAlign: "center" }}>
                    … {shown.length - 100} more rows not shown
                  </td></tr>
                )}
              </tbody>
            </table>
          </div>
        </>
      )}

      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}>
        <div style={{ display: "flex", gap: 10 }}>
          <Button leadingIcon={<ArrowLeft size={14} />} onClick={onBack}>Back</Button>
          {/* A wrong member name is fixed where the file is shaped (Rename
              values), not by starting over. */}
          {hasErrors && <Button variant="ghost" onClick={onBackToShape}>Back to Shape</Button>}
        </div>
        <div style={{ display: "flex", gap: 10 }}>
          {errorRows.size > 0 && !wholeFileError && (
            <Button disabled={checking} onClick={() => onNext(true)}>
              Import Valid Rows Only ({Math.max(0, validRows)})
            </Button>
          )}
          <Button variant="primary" disabled={checking} trailingIcon={<ArrowRight size={14} />} onClick={() => onNext(false)}>
            {hasErrors ? "Import All Anyway" : "Commit Import"}
          </Button>
        </div>
      </div>
    </div>
  );
}

// ── Commit Step ────────────────────────────────────────────────────────────────

function CommitStep({
  config,
  parsedFile,
  mappings,
  errors,
  importValidOnly,
  resolveCtx,
  onBack,
  onDone,
}: {
  config: WizardConfig;
  parsedFile: ParsedFile;
  mappings: ColMapping[];
  errors: ValError[];
  importValidOnly: boolean;
  resolveCtx: ResolveContext;
  onBack: () => void;
  onDone: () => void;
}) {
  const qc = useQueryClient();
  const [result, setResult] = useState<CommitResult | null>(null);

  const errRows = new Set(errors.filter(e => e.sev === "error" && e.index >= 0).map(e => e.index));
  const sent = parsedFile.rows.map((_, i) => i).filter(i => !importValidOnly || !errRows.has(i));
  const rowsToImport = sent.map(i => parsedFile.rows[i]);
  // Each row goes with its line in the file, which a refusal and the
  // import's history then name — also after Import Valid Rows Only.
  const rowLines = sent.map(i => lineOf(parsedFile, i));

  const commit = useMutation({
    mutationFn: async () => {
      const csv = resolveAndSerialize(parsedFile.headers, rowsToImport, mappings, resolveCtx);
      let out: { rows_imported: number; error_rows: number; values?: number };
      if (config.integrationId) {
        const res = await api.runIntegration(config.integrationId, csv, rowLines);
        out = { rows_imported: res.rows_imported, error_rows: res.error_rows, values: res.values_imported };
      } else if (config.targetType === "dimension") {
        // One-off dimension imports have their own endpoint — the generic
        // upload endpoint is the GRID importer and rejects dimension CSVs
        // ("label matches no metric or dimension", reported live).
        out = await api.importDimensionMembers(config.targetId, csv);
      } else if (config.targetType === "form") {
        // Records too: sent to the grid importer, a table's columns were
        // refused as matching no metric or dimension.
        const res = await api.importFormRecords(config.targetId, csv, rowLines);
        out = { rows_imported: res.records_created, error_rows: 0 };
      } else {
        const res = await api.uploadImport(csv, config.revisionId, config.importMode, rowLines);
        out = { rows_imported: res.rows ?? res.valid_rows, error_rows: res.error_rows, values: res.values };
      }
      // Finishing the wizard from a resumed draft activates it, with the
      // final column mapping saved back. Best-effort: the import itself
      // already succeeded.
      if (config.draftId) {
        try {
          await api.updateIntegration(config.draftId, {
            name: config.integrationName || "Import",
            target_type: config.targetType,
            target_id: config.targetId,
            status: "active",
          });
          const shaped = cleanReshape(config.reshape ?? {});
          await api.updateIntegrationConfig(config.draftId, {
            ...(isEmptyReshape(shaped) ? {} : { reshape: shaped }),
            column_map: columnMapOf(mappings),
            ...(config.source === "google_sheets" ? { sheet_url: config.sheetUrl } : {}),
            ...savedMode(config),
          });
        } catch { /* draft stays a draft; the import result stands */ }
      }
      return out;
    },
    onSuccess: (data) => {
      qc.invalidateQueries({ queryKey: ["import-jobs"] });
      qc.invalidateQueries({ queryKey: ["dev-integrations"] });
      // What the import changed, refetched: the console holds the
      // dimensions above its pages, so the Dimensions page went on showing
      // the members from before the import until the page was reloaded.
      if (config.targetType === "dimension") {
        for (const key of ["dev-dimensions", "dev-dimensions-all", "dim-props"]) qc.invalidateQueries({ queryKey: [key] });
      } else {
        if (config.targetType === "form") qc.invalidateQueries({ queryKey: ["records"] });
        void invalidateModelData(qc);
      }
      setResult({ rowsImported: data.rows_imported, values: data.values, errorRows: data.error_rows, status: "success" });
    },
    onError: (e) => {
      const rows = (e as ApiError).body?.errors;
      setResult({ rowsImported: 0, errorRows: 0, status: "error", message: e instanceof Error ? e.message : "Import failed",
        rowErrors: Array.isArray(rows) ? rows as CommitResult["rowErrors"] : undefined });
    },
  });

  const TARGET_LABEL: Record<string, string> = { grid: "Grid", form: "Table", dimension: "Dimension" };

  if (result) {
    return (
      <div style={{ textAlign: "center", padding: "24px 0" }}>
        {result.status === "success" ? (
          <>
            <h3 style={{ margin: "0 0 8px", color: "var(--color-live)" }}>Import Complete</h3>
            <p className="mvx-admin-muted" style={{ margin: "0 0 20px", fontSize: 13 }}>
              {result.rowsImported} rows imported
              {result.values && result.values !== result.rowsImported ? ` (${result.values} values)` : ""}
              {result.errorRows > 0 ? `, ${result.errorRows} rows had errors` : ""}
            </p>
          </>
        ) : (
          <>
            <h3 style={{ margin: "0 0 8px", color: "var(--color-danger)" }}>Import Failed</h3>
            <p className="mvx-admin-muted" style={{ margin: "0 0 20px", fontSize: 13 }}>{result.message}</p>
            {result.rowErrors && result.rowErrors.length > 0 && (
              <div className="mvx-table-wrap" style={{ margin: "0 0 20px", textAlign: "left" }}>
                <table className="mvx-table mvx-table--compact">
                  <thead><tr><th>Row</th><th>Column</th><th>Problem</th><th>Value</th></tr></thead>
                  <tbody>
                    {result.rowErrors.slice(0, 100).map((e, i) => (
                      <tr key={i}>
                        <td className="mvx-admin-mono mvx-admin-muted">{e.row}</td>
                        <td className="mvx-admin-mono">{e.column}</td>
                        <td>{e.message}</td>
                        <td className="mvx-admin-mono mvx-admin-muted">{e.raw_value ?? ""}</td>
                      </tr>
                    ))}
                    {result.rowErrors.length > 100 && (
                      <tr><td colSpan={4} className="mvx-admin-muted" style={{ textAlign: "center" }}>
                        … {result.rowErrors.length - 100} more not shown
                      </td></tr>
                    )}
                  </tbody>
                </table>
              </div>
            )}
          </>
        )}
        <div style={{ display: "flex", gap: 10, justifyContent: "center" }}>
          {/* A refused file goes back to Validate with the wizard's work
              kept; it used to offer only Done, which closed the wizard. */}
          {result.status === "error" && (
            <Button leadingIcon={<ArrowLeft size={14} />} onClick={() => { setResult(null); onBack(); }}>Back</Button>
          )}
          <Button variant="primary" onClick={onDone}>Done</Button>
        </div>
      </div>
    );
  }

  return (
    <div>
      {/* Confirmation summary */}
      <div className="mvx-panel" style={{ padding: 20, marginBottom: 20 }}>
        <h3 style={{ margin: "0 0 16px", fontSize: 16 }}>Confirm Import</h3>
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12 }}>
          {[
            ["Target type", TARGET_LABEL[config.targetType]],
            ["Target", config.targetLabel || config.targetId],
            config.revisionName ? ["Revision", config.revisionName] : null,
            config.integrationId ? ["Integration", config.integrationName ?? config.integrationId] : null,
            ["Rows to import", String(rowsToImport.length)],
            ["Columns mapped", String(mappings.filter(m => m.targetField).length)],
            errRows.size > 0 ? ["Rows skipped (errors)", importValidOnly ? String(errRows.size) : "0 (importing all)"] : null,
          ].filter((x): x is [string, string] => x !== null).map(([k, v]) => (
            <div key={k}>
              <div className="mvx-admin-muted" style={{ marginBottom: 2 }}>{k}</div>
              <div style={{ fontSize: 13, fontWeight: 500 }}>{v}</div>
            </div>
          ))}
        </div>
      </div>

      <div style={{ display: "flex", justifyContent: "space-between" }}>
        <Button leadingIcon={<ArrowLeft size={14} />} disabled={commit.isPending} onClick={onBack}>Back</Button>
        <Button
          variant="primary"
          loading={commit.isPending}
          loadingLabel="Importing…"
          onClick={() => commit.mutate()}
        >
          Commit Import ({rowsToImport.length} rows)
        </Button>
      </div>
    </div>
  );
}

// ── Import History ─────────────────────────────────────────────────────────────

// created_at comes from Go as a protobuf Timestamp → {"seconds":N,"nanos":N}
// or as an ISO string — handle both.
function fmtDate(v: unknown): string {
  if (!v) return "—";
  if (typeof v === "object" && v !== null && "seconds" in v) {
    return new Date((v as { seconds: number }).seconds * 1000).toLocaleString();
  }
  const d = new Date(v as string);
  return isNaN(d.getTime()) ? "—" : d.toLocaleString();
}

const STATUS: Record<number, { label: string; tone: DesignTone }> = {
  0: { label: "Unknown",    tone: "neutral" },
  1: { label: "Validating", tone: "info" },
  2: { label: "Staged",     tone: "draft" },
  3: { label: "Committed",  tone: "success" },
  4: { label: "Failed",     tone: "danger" },
  5: { label: "Aborted",    tone: "neutral" },
};

function ImportHistory() {
  const qc = useQueryClient();
  const [delError, setDelError] = useState<string | null>(null);
  const { data: jobs = [] } = useQuery({ queryKey: ["import-jobs"], queryFn: () => api.getImportJobs() });

  const del = useMutation({
    mutationFn: (id: string) => api.deleteImportJob(id),
    onSuccess: () => { setDelError(null); qc.invalidateQueries({ queryKey: ["import-jobs"] }); },
    onError: (e) => setDelError(e instanceof Error ? e.message : "Delete failed"),
  });

  if (!jobs.length) return null;

  return (
    <div style={{ marginTop: 32 }}>
      <div style={{ fontSize: 13, fontWeight: 600, marginBottom: 12 }}>Recent Imports</div>
      {delError && <p className="mvx-admin-error" style={{ marginBottom: 8 }}>Delete failed: {delError}</p>}
      <div className="mvx-table-wrap">
        <table className="mvx-table mvx-table--compact">
          <thead>
            <tr>
              {/* Values: a row of a wide file is one value per metric. */}
              {["Date", "Revision", "Values", "Errors", "Status", ""].map((h, i) => (
                <th key={i}>{h}</th>
              ))}
            </tr>
          </thead>
          <tbody>
            {jobs.slice(0, 20).map(j => {
              const s = STATUS[j.status] ?? STATUS[0];
              return (
                <tr key={j.id}>
                  <td className="mvx-admin-muted" style={{ whiteSpace: "nowrap" }}>{fmtDate(j.created_at)}</td>
                  <td>{j.revision_name || "—"}</td>
                  <td className="mvx-admin-mono">{j.total_rows ?? "—"}</td>
                  <td className="mvx-admin-mono" style={{ color: (j.error_rows ?? 0) > 0 ? "var(--color-danger)" : "var(--color-text-subtle)" }}>
                    {j.error_rows ?? 0}
                  </td>
                  <td>
                    <StatusBadge tone={s.tone}>{s.label}</StatusBadge>
                    {j.error && <div className="mvx-admin-muted" style={{ fontSize: 11, marginTop: 2 }}>{j.error}</div>}
                  </td>
                  <td style={{ textAlign: "right" }}>
                    <IconButton
                      aria-label="Delete this import record"
                      title="Delete this import record"
                      danger
                      size={26}
                      disabled={del.isPending}
                      onClick={() => del.mutate(j.id)}
                    >
                      <Trash2 size={13} />
                    </IconButton>
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </div>
  );
}

// ── Main Wizard ────────────────────────────────────────────────────────────────

function ImportWizard({
  initialConfig,
  workingRevisionId,
  onClose,
}: {
  initialConfig?: Partial<WizardConfig>;
  workingRevisionId?: string;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const [step, setStep] = useState<WizardStep>("upload");
  // The target revision starts as the working revision: that is the one
  // being built, and the one the target lists below come from.
  const [config, setConfig] = useState<WizardConfig>({
    targetType: "grid",
    targetId: "",
    targetLabel: "",
    importMode: "incremental",
    ...(workingRevisionId ? { revisionId: workingRevisionId } : {}),
    ...initialConfig,
  });
  const [sourceFile, setSourceFile] = useState<ParsedFile | null>(null);
  const [parsedFile, setParsedFile] = useState<ParsedFile | null>(null);
  const [mappings, setMappings] = useState<ColMapping[]>([]);
  const [errors, setErrors] = useState<ValError[]>([]);
  const [checking, setChecking] = useState(false);
  const checkRun = useRef(0); // the server check whose answer counts
  const [importValidOnly, setImportValidOnly] = useState(false);

  // Targets, and the dimensions labels are resolved against, come from the
  // working revision. Unnamed, the server answered from X-Model-Id or the
  // default model, so building another model mixed that model's grids and
  // dimensions with the working model's revisions and metrics.
  const { data: grids = [] } = useQuery({ queryKey: ["dev-grids", workingRevisionId], queryFn: () => api.listGrids(workingRevisionId) });
  const { data: forms = [] } = useQuery({ queryKey: ["forms", workingRevisionId], queryFn: () => api.listForms(workingRevisionId) });
  const { data: dims = [] } = useQuery({ queryKey: ["dev-dimensions", workingRevisionId], queryFn: () => api.getDevDimensions(workingRevisionId) });
  // Scope metrics to the selected revision so name→UUID resolution uses the right IDs.
  const { data: model } = useQuery({
    queryKey: ["dev-model", config.revisionId],
    queryFn: () => api.getDevModel(config.revisionId),
    enabled: !!config.revisionId || config.targetType !== "grid",
  });

  const resolveCtx: ResolveContext = {
    metrics: (model as DevModel | undefined)?.metrics ?? [],
    dims: dims as DevDimension[],
  };
  // The grid's metrics as map targets: the working revision's, which the
  // grid's metric_ids name (the console holds the same query).
  const { data: workingModel } = useQuery({
    queryKey: ["dev-model", workingRevisionId],
    queryFn: () => api.getDevModel(workingRevisionId),
    enabled: config.targetType === "grid",
  });
  const gridMetrics = (workingModel as DevModel | undefined)?.metrics ?? [];

  // Known property names for the target dimension: declared attributes
  // (model.dimension_property) plus keys already present on its members.
  const { data: declaredProps = [] } = useQuery({
    queryKey: ["dim-props", config.targetId],
    queryFn: () => api.listDimProperties(config.targetId),
    enabled: config.targetType === "dimension" && !!config.targetId,
  });
  const knownProps = useMemo(() => {
    if (config.targetType !== "dimension") return [];
    const names = new Set<string>(declaredProps.map(p => p.name));
    const dim = (dims as DevDimension[]).find(d => d.id === config.targetId);
    dim?.members.forEach(m => Object.keys(m.properties ?? {}).forEach(k => names.add(k)));
    return [...names].sort();
  }, [config.targetType, config.targetId, declaredProps, dims]);

  const fieldOptions = parsedFile
    ? getFieldOptions(config, grids as GridDef[], forms as FormDef[], dims as DevDimension[], knownProps, gridMetrics)
    : [];

  // For row-level validation: only flag fields that are actually mapped and required
  const reqFields = fieldOptions
    .filter(f => f.required && !f.group)
    .map(f => f.value);

  const handleFileReady = (pf: ParsedFile) => {
    setSourceFile(pf);
    setStep("shape");
  };

  // The Shape step's result is the file the rest of the wizard maps: its
  // columns are the shaped ones, which a saved column map names.
  const handleShaped = (shaped: Shaped) => {
    if (!sourceFile) return;
    const pf = { ...sourceFile, headers: shaped.headers, rows: shaped.rows, lines: shaped.lines };
    setParsedFile(pf);
    const opts = getFieldOptions(config, grids as GridDef[], forms as FormDef[], dims as DevDimension[], knownProps, gridMetrics);
    const saved = config.editIntegration?.config.column_map ?? config.savedColumnMap ?? {};
    // Back to Shape and on again keeps what was mapped by hand.
    const chosen = new Map(mappings.filter(m => m.status !== "auto").map(m => [m.sourceCol, m]));
    setMappings(autoMap(pf.headers, opts).map(m => chosen.get(m.sourceCol) ?? (saved[m.sourceCol] !== undefined
      ? { ...m, targetField: saved[m.sourceCol] === "ignore" ? "" : saved[m.sourceCol], status: saved[m.sourceCol] === "ignore" ? "ignored" : "manual" }
      : m)));
    setStep("map");
  };
  const savedReshape = () => {
    const r = cleanReshape(config.reshape ?? {});
    return isEmptyReshape(r) ? {} : { reshape: r };
  };
  const columnMap = () => columnMapOf(mappings);

  // Edit mode: the integration's other settings stay as they are.
  const saveChanges = useMutation({
    mutationFn: async () => {
      const it = config.editIntegration!;
      const { reshape: _old, column_map: _oldMap, ...rest } = it.config;
      void _old; void _oldMap;
      await api.updateIntegrationConfig(it.id, { ...rest, ...savedReshape(), column_map: columnMap(), ...savedMode(config) });
    },
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["dev-integrations"] }); onClose(); },
  });

  const handleMappingDone = () => {
    if (!parsedFile) return;
    const own = runValidation(parsedFile, mappings, reqFields);
    setErrors(own);
    setStep("validate");
    const run = ++checkRun.current;
    if (config.targetType === "dimension") {
      setChecking(false);
      return;
    }
    setChecking(true);
    const csv = resolveAndSerialize(parsedFile.headers, parsedFile.rows, mappings, resolveCtx);
    // checkOnServer answers what it could not check as a warning; it does
    // not throw. The rows and the end of the check show together.
    void checkOnServer(config, parsedFile, csv).then(found => {
      if (run !== checkRun.current) return;
      setErrors([...own, ...found]);
      setChecking(false);
    });
  };
  const leaveValidate = (to: WizardStep) => {
    checkRun.current++;
    setChecking(false);
    setStep(to);
  };

  const handleValidateDone = (validOnly: boolean) => {
    setImportValidOnly(validOnly);
    setStep("commit");
  };

  const saveAsIntegration = useMutation({
    mutationFn: ({ name, asDraft }: { name: string; asDraft?: boolean }) =>
      api.createIntegration({
        name,
        type: config.source === "google_sheets" ? "google_sheets" : "csv_import",
        target_type: config.targetType,
        target_id: config.targetId,
        status: asDraft ? "draft" : "active",
      }, workingRevisionId).then(res =>
        api.updateIntegrationConfig(res.id, {
          ...savedReshape(),
          column_map: columnMap(),
          // A google_sheets integration stores its source so a run needs no
          // input at all — the server re-fetches the sheet.
          ...(config.source === "google_sheets" ? { sheet_url: config.sheetUrl } : {}),
          ...savedMode(config),
        })
      ),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["dev-integrations"] }),
  });

  return (
    <div className="mvx-panel" style={{ padding: 24 }}>
      <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", marginBottom: 20 }}>
        <h2 style={{ margin: 0, fontSize: 18, fontWeight: 700 }}>Import Wizard</h2>
        <IconButton aria-label="Close import wizard" title="Close" size={28} onClick={onClose}>
          <XIcon size={15} />
        </IconButton>
      </div>

      <StepIndicator current={step} />

      {step === "upload" && (
        <UploadStep
          config={config}
          grids={grids as GridDef[]}
          forms={forms as FormDef[]}
          dims={dims as DevDimension[]}
          workingRevisionId={workingRevisionId}
          onNext={handleFileReady}
          onConfigChange={setConfig}
        />
      )}

      {step === "shape" && sourceFile && (
        <ShapeStep
          fileName={sourceFile.name}
          source={sourceFile.source ?? {}}
          asRead={{ headers: sourceFile.headers, rows: sourceFile.rows }}
          isCsv={sourceFile.ext === "csv"}
          reshape={config.reshape ?? {}}
          onReshapeChange={reshape => setConfig(c => ({ ...c, reshape }))}
          onNext={handleShaped}
          onBack={() => setStep("upload")}
        />
      )}

      {step === "map" && parsedFile && (
        <MapColumnsStep
          parsedFile={parsedFile}
          fieldOptions={fieldOptions}
          mappings={mappings}
          onMappingChange={setMappings}
          onNext={handleMappingDone}
          onBack={() => setStep("shape")}
          editingName={config.editIntegration?.name}
          onSaveChanges={config.editIntegration ? () => saveChanges.mutate() : undefined}
          savingChanges={saveChanges.isPending}
          onSaveAsIntegration={(name, asDraft) => saveAsIntegration.mutate({ name, asDraft })}
          savedIntegrationName={config.integrationName}
          allowProperties={config.targetType === "dimension"}
        />
      )}

      {step === "validate" && parsedFile && (
        <ValidateStep
          parsedFile={parsedFile}
          errors={errors}
          checking={checking}
          onNext={handleValidateDone}
          onBack={() => leaveValidate("map")}
          onBackToShape={() => leaveValidate("shape")}
        />
      )}

      {step === "commit" && parsedFile && (
        <CommitStep
          config={config}
          parsedFile={parsedFile}
          mappings={mappings}
          errors={errors}
          importValidOnly={importValidOnly}
          resolveCtx={resolveCtx}
          onBack={() => setStep("validate")}
          onDone={onClose}
        />
      )}
    </div>
  );
}

// ── Saved Integrations List ────────────────────────────────────────────────────

function SavedIntegrationsList({
  sourceType,
  revisionId,
  onRun,
}: {
  sourceType: "csv_import" | "google_sheets";
  revisionId?: string;
  onRun: (cfg: Partial<WizardConfig>) => void;
}) {
  const qc = useQueryClient();
  // The working revision's integrations — the ones "Save as integration"
  // creates there.
  const { data: integrations = [] } = useQuery({ queryKey: ["dev-integrations", revisionId], queryFn: () => api.listDevIntegrations(revisionId) });
  const [syncMsg, setSyncMsg] = useState<Record<string, string>>({});

  const del = useMutation({
    mutationFn: (id: string) => api.deleteIntegration(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["dev-integrations"] }),
  });
  const [renamingId, setRenamingId] = useState<string | null>(null);
  const [historyId, setHistoryId] = useState<string | null>(null);
  const [tagDraft, setTagDraft] = useState<Record<string, string>>({});
  const patch = useMutation({
    mutationFn: ({ d, name, tags }: { d: IntegrationDef; name?: string; tags?: string[] }) =>
      api.updateIntegration(d.id, {
        name: name ?? d.name,
        target_type: d.target_type,
        target_id: d.target_id,
        ...(tags ? { tags } : {}),
      }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["dev-integrations"] }),
  });
  // google_sheets integrations run with no input — the server re-fetches the
  // configured sheet — so "Sync Now" completes in place instead of opening
  // the wizard the way a CSV run (which needs a file) does.
  const sync = useMutation({
    mutationFn: (id: string) => api.runIntegration(id),
    onSuccess: (res, id) => {
      qc.invalidateQueries({ queryKey: ["import-jobs"] });
      setSyncMsg(prev => ({
        ...prev,
        [id]: `${res.rows_imported} row(s) synced${res.error_rows ? `, ${res.error_rows} row(s) failed validation` : ""}`,
      }));
    },
    onError: (e, id) => {
      setSyncMsg(prev => ({ ...prev, [id]: `Sync failed: ${e instanceof Error ? e.message : "unknown error"}` }));
    },
  });
  const { confirm, confirmElement } = useConfirm();

  const TARGET_LABEL: Record<string, string> = { grid: "Grid", form: "Table", dimension: "Dimension" };
  const shown = (integrations as IntegrationDef[]).filter(d =>
    sourceType === "google_sheets" ? d.type === "google_sheets" : d.type !== "google_sheets"
  );

  if (!shown.length) return null;

  return (
    <div style={{ marginBottom: 24 }}>
      <div style={{ fontSize: 13, fontWeight: 600, marginBottom: 12 }}>Saved Integrations</div>
      <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
        {shown.map(d => (
          <div key={d.id} className="mvx-panel" style={{ padding: "12px 16px" }}>
            <div style={{ display: "flex", alignItems: "center", gap: 12 }}>
              <div style={{ flex: 1, display: "flex", alignItems: "center", gap: 6, flexWrap: "wrap" }}>
                {renamingId === d.id ? (
                  <TextInput
                    autoFocus
                    defaultValue={d.name}
                    aria-label={`Rename ${d.name}`}
                    style={{ width: 220, height: 28, fontSize: 13 }}
                    onKeyDown={e => {
                      if (e.key === "Enter") {
                        const v = (e.target as HTMLInputElement).value.trim();
                        if (v && v !== d.name) patch.mutate({ d, name: v });
                        setRenamingId(null);
                      }
                      if (e.key === "Escape") setRenamingId(null);
                    }}
                    onBlur={e => {
                      const v = e.target.value.trim();
                      if (v && v !== d.name) patch.mutate({ d, name: v });
                      setRenamingId(null);
                    }}
                  />
                ) : (
                  <span
                    style={{ fontWeight: 600, fontSize: 13, cursor: "text" }}
                    title="Double-click to rename"
                    onDoubleClick={() => setRenamingId(d.id)}
                  >
                    {d.name}
                  </span>
                )}
                <IconButton aria-label={`Rename ${d.name}`} title="Rename" size={22} onClick={() => setRenamingId(d.id)}>
                  <Pencil size={11} />
                </IconButton>
                {d.status === "draft" && <StatusBadge tone="draft">draft</StatusBadge>}
                <StatusBadge tone="info">{d.type === "google_sheets" ? "Google Sheets" : "CSV Import"}</StatusBadge>
                <StatusBadge>→ {TARGET_LABEL[d.target_type] ?? d.target_type}</StatusBadge>
                {(d.tags ?? []).map(t => (
                  <StatusBadge key={t}>
                    {t}
                    <button
                      type="button"
                      aria-label={`Remove tag ${t}`}
                      onClick={() => patch.mutate({ d, tags: (d.tags ?? []).filter(x => x !== t) })}
                      style={{ marginLeft: 3, border: "none", background: "none", cursor: "pointer", color: "inherit", padding: 0 }}
                    >
                      ×
                    </button>
                  </StatusBadge>
                ))}
                <input
                  value={tagDraft[d.id] ?? ""}
                  onChange={e => setTagDraft(prev => ({ ...prev, [d.id]: e.target.value }))}
                  onKeyDown={e => {
                    if (e.key !== "Enter") return;
                    const t = (tagDraft[d.id] ?? "").trim();
                    if (!t || (d.tags ?? []).includes(t)) return;
                    patch.mutate({ d, tags: [...(d.tags ?? []), t] });
                    setTagDraft(prev => ({ ...prev, [d.id]: "" }));
                  }}
                  placeholder="+ tag"
                  aria-label={`Add tag to ${d.name}`}
                  style={{ width: 64, fontSize: 11, border: "1px dashed var(--color-border-muted)", borderRadius: 4, padding: "2px 6px", background: "transparent" }}
                />
                {d.config?.column_map && (
                  <span className="mvx-admin-muted" style={{ fontSize: 11 }}>
                    {Object.values(d.config.column_map).filter(t => t !== "ignore").length} columns mapped
                  </span>
                )}
                {d.target_type === "grid" && (d.type === "csv_import" || d.type === "google_sheets") && (
                  <span className="mvx-admin-muted" style={{ fontSize: 11 }} data-testid={`integration-mode-${d.id}`} title="How each run writes the grid; change it with Edit shape and mapping">
                    Mode: {MODE_LABEL[d.config?.import_mode ?? (d.type === "google_sheets" ? "replace" : "incremental")] ?? d.config?.import_mode}
                  </span>
                )}
                {d.config?.reshape && describeReshape(d.config.reshape) && (
                  <span className="mvx-admin-muted" style={{ fontSize: 11 }} data-testid={`integration-reshape-${d.id}`} title="Applies to every run; change it with Edit shape and mapping">
                    Reshapes each file: {describeReshape(d.config.reshape)}
                  </span>
                )}
              </div>
              {d.status === "draft" ? (
                <Button
                  size="sm"
                  leadingIcon={<ArrowRight size={13} />}
                  onClick={() => onRun({
                    draftId: d.id,
                    integrationName: d.name,
                    targetType: d.target_type as TargetType,
                    targetId: d.target_id,
                    ...(d.config?.import_mode ? { importMode: d.config.import_mode as WizardConfig["importMode"] } : {}),
                    ...(d.config?.reshape ? { reshape: d.config.reshape } : {}),
                    ...(d.type === "google_sheets" ? { source: "google_sheets" as const, sheetUrl: d.config?.sheet_url } : {}),
                  })}
                >
                  Continue draft
                </Button>
              ) : d.type === "google_sheets" ? (
                <Button
                  size="sm"
                  leadingIcon={<Play size={13} />}
                  loading={sync.isPending && sync.variables === d.id}
                  loadingLabel="Syncing…"
                  onClick={() => sync.mutate(d.id)}
                >
                  Sync Now
                </Button>
              ) : d.config?.reshape ? (
                <RunReshapedIntegration integration={d} />
              ) : (
                <Button
                  size="sm"
                  leadingIcon={<Play size={13} />}
                  onClick={() => onRun({ integrationId: d.id, integrationName: d.name, targetType: d.target_type as TargetType, targetId: d.target_id,
                    ...(d.config?.column_map ? { savedColumnMap: d.config.column_map } : {}) })}
                >
                  Run Import
                </Button>
              )}
              {d.status !== "draft" && (
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => onRun({
                    editIntegration: d,
                    integrationName: d.name,
                    targetType: d.target_type as TargetType,
                    targetId: d.target_id,
                    importMode: (d.config?.import_mode as WizardConfig["importMode"] | undefined) ?? (d.type === "google_sheets" ? "replace" : "incremental"),
                    ...(d.config?.reshape ? { reshape: d.config.reshape } : {}),
                    ...(d.type === "google_sheets" ? { source: "google_sheets" as const, sheetUrl: d.config?.sheet_url } : {}),
                  })}
                  aria-label={`Edit shape and mapping of ${d.name}`}
                >
                  Edit shape and mapping
                </Button>
              )}
              <IconButton
                aria-label={`Run history of ${d.name}`}
                title="Run history"
                onClick={() => setHistoryId(h => (h === d.id ? null : d.id))}
              >
                <History size={14} />
              </IconButton>
              <IconButton
                aria-label={`Delete integration ${d.name}`}
                title="Delete integration"
                danger
                onClick={() => confirm({ title: "Delete integration?", body: `This removes "${d.name}" and its import history.`, confirmLabel: "Delete integration", onConfirm: () => del.mutate(d.id) })}
              >
                <Trash2 size={14} />
              </IconButton>
            </div>
            {historyId === d.id && <IntegrationRunHistory integrationId={d.id} />}
            {d.type === "google_sheets" && d.config?.sheet_url && (
              <div className="mvx-admin-muted mvx-admin-mono" style={{ fontSize: 11, marginTop: 6, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                {d.config.sheet_url}
              </div>
            )}
            {syncMsg[d.id] && (
              <div style={{ fontSize: 12, marginTop: 6, color: syncMsg[d.id].startsWith("Sync failed") ? "var(--color-danger)" : "var(--color-success)" }}>
                {syncMsg[d.id]}
              </div>
            )}
          </div>
        ))}
      </div>
      {confirmElement}
    </div>
  );
}

// ── Per-integration run history ────────────────────────────────────────────────

// RunReshapedIntegration runs a file integration whose files are reshaped
// before mapping (set up by the AI Developer). The wizard reads a file's
// header from its first row and has no editor for a reshape, so the file
// goes to the server as it is — as a dashboard's Integration button sends
// it — and the saved reshape, column map and mode apply there.
function RunReshapedIntegration({ integration }: { integration: IntegrationDef }) {
  const qc = useQueryClient();
  const fileRef = useRef<HTMLInputElement>(null);
  const run = useMutation({
    mutationFn: (file: { csv?: string; xlsx_base64?: string }) => api.runIntegration(integration.id, file),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: ["integration-runs", integration.id] });
      qc.invalidateQueries({ queryKey: ["dev-integrations"] });
    },
  });
  const pick = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0];
    e.target.value = "";
    if (!file) return;
    if (/\.xls[xm]$/i.test(file.name)) run.mutate({ xlsx_base64: await fileToBase64(file) });
    else run.mutate({ csv: await file.text() });
  };
  // A refused file (422) lists its rows in the error's body: nothing of it
  // went in. A run used to import the rows that passed.
  const refused = ((run.error as ApiError | null)?.body?.errors ?? []) as Array<{ row: number; column: string; message: string }>;
  return (
    <span style={{ display: "inline-flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
      <input ref={fileRef} type="file" accept=".csv,.xlsx,.xlsm,text/csv" style={{ display: "none" }} onChange={pick}
        aria-label={`File for ${integration.name}`} />
      <Button size="sm" leadingIcon={<Play size={13} />} loading={run.isPending} loadingLabel="Importing…"
        onClick={() => fileRef.current?.click()}>
        Run with a file
      </Button>
      {run.isSuccess && (
        <span className="mvx-admin-muted" style={{ fontSize: 12 }} role="status">
          {run.data.rows_imported} row(s) imported
          {run.data.values_imported && run.data.values_imported !== run.data.rows_imported ? ` (${run.data.values_imported} values)` : ""}
        </span>
      )}
      {run.isError && (
        <span className="mvx-admin-error" style={{ fontSize: 12 }}>
          {(run.error as Error).message}
          {refused[0] ? ` · row ${refused[0].row}${refused[0].column ? `, ${refused[0].column}` : ""}: ${refused[0].message}${refused.length > 1 ? ` (+${refused.length - 1} more)` : ""}` : ""}
        </span>
      )}
    </span>
  );
}

function IntegrationRunHistory({ integrationId }: { integrationId: string }) {
  const { data: runs = [], isLoading } = useQuery({
    queryKey: ["integration-runs", integrationId],
    queryFn: () => api.listIntegrationRuns(integrationId),
  });
  if (isLoading) return <div className="mvx-admin-muted" style={{ fontSize: 12, padding: "8px 0" }}>Loading history…</div>;
  if (!runs.length) return <div className="mvx-admin-muted" style={{ fontSize: 12, padding: "8px 0" }}>No runs yet.</div>;
  return (
    <div style={{ marginTop: 8, display: "flex", flexDirection: "column", gap: 4 }}>
      {runs.map(r => (
        <div key={r.id} style={{ display: "flex", alignItems: "center", gap: 10, fontSize: 12, padding: "4px 0", borderTop: "1px solid var(--color-border)" }}>
          <StatusBadge tone={r.status === "success" ? "success" : "danger"}>{r.status}</StatusBadge>
          <span>{r.rows_imported} row{r.rows_imported !== 1 ? "s" : ""}{r.error_rows > 0 ? ` · ${r.error_rows} failed` : ""}</span>
          {r.message && <span className="mvx-admin-muted" style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", maxWidth: 320 }}>{r.message}</span>}
          <span className="mvx-admin-muted" style={{ marginLeft: "auto", whiteSpace: "nowrap" }}>
            {r.run_by && `${r.run_by} · `}{fmtDate(r.created_at)}
          </span>
        </div>
      ))}
    </div>
  );
}

// ── Main Export ────────────────────────────────────────────────────────────────

// revisionId: the Build working revision (Integrations passes it).
export function ExcelImportSection({ revisionId }: { revisionId?: string } = {}) {
  const [wizardConfig, setWizardConfig] = useState<Partial<WizardConfig> | null>(null);

  const openWizard = (cfg?: Partial<WizardConfig>) => setWizardConfig(cfg ?? {});
  const closeWizard = () => setWizardConfig(null);

  return (
    <div>
      {wizardConfig !== null ? (
        <ImportWizard initialConfig={wizardConfig} workingRevisionId={revisionId} onClose={closeWizard} />
      ) : (
        <>
          <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: 12, marginBottom: 20 }}>
            <p className="mvx-admin-muted" style={{ fontSize: 13, margin: 0 }}>
              Upload a CSV or Excel file, map columns to your model fields, validate, and commit data to a Grid, a Table of records, or a Dimension.
            </p>
            <Button variant="primary" leadingIcon={<PlusIcon size={14} />} style={{ whiteSpace: "nowrap" }} onClick={() => openWizard()}>
              New Import
            </Button>
          </div>

          <SavedIntegrationsList sourceType="csv_import" revisionId={revisionId} onRun={cfg => openWizard(cfg)} />
          <ImportHistory />

          <div className="mvx-panel" style={{ marginTop: 24, padding: 16, background: "var(--color-surface-subtle)", fontSize: 13, color: "var(--color-text-muted)" }}>
            <strong style={{ color: "var(--color-text)" }}>CSV format by target type:</strong>
            <ul style={{ margin: "8px 0 0", paddingLeft: 20, lineHeight: 1.8 }}>
              <li><strong>Grid:</strong> one column per dimension with member codes, and one column per metric (e.g. <code>region, month, revenue, qty</code>) — or a <code>metric</code> column naming each row's metric beside a <code>value</code> column</li>
              <li><strong>Table of records:</strong> columns = field names (e.g. <code>amount, description, date</code>)</li>
              <li><strong>Dimension:</strong> <code>code, label</code> and optionally <code>parent_code</code></li>
            </ul>
          </div>
        </>
      )}
    </div>
  );
}

export function GoogleSheetsImportSection({ revisionId }: { revisionId?: string } = {}) {
  const [wizardConfig, setWizardConfig] = useState<Partial<WizardConfig> | null>(null);

  // "replace" is the sheet default on purpose: a sheet import is re-run
  // against the same living sheet, and replace converges where incremental
  // would double-count. The wizard's mode step still lets the user change it.
  const openWizard = (cfg?: Partial<WizardConfig>) =>
    setWizardConfig({ source: "google_sheets", importMode: "replace", ...cfg });
  const closeWizard = () => setWizardConfig(null);

  return (
    <div>
      {wizardConfig !== null ? (
        <ImportWizard initialConfig={wizardConfig} workingRevisionId={revisionId} onClose={closeWizard} />
      ) : (
        <>
          <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: 12, marginBottom: 20 }}>
            <p className="mvx-admin-muted" style={{ fontSize: 13, margin: 0 }}>
              Paste a link-shared Google Sheet URL, map its columns, validate, and commit — then save it as an
              integration to re-sync the sheet with one click.
            </p>
            <Button variant="primary" leadingIcon={<PlusIcon size={14} />} style={{ whiteSpace: "nowrap" }} onClick={() => openWizard()}>
              Import from Sheet
            </Button>
          </div>

          <SavedIntegrationsList sourceType="google_sheets" revisionId={revisionId} onRun={cfg => openWizard(cfg)} />
          <ImportHistory />

          <div className="mvx-panel" style={{ marginTop: 24, padding: 16, background: "var(--color-surface-subtle)", fontSize: 13, color: "var(--color-text-muted)" }}>
            <strong style={{ color: "var(--color-text)" }}>How it works:</strong>
            <ul style={{ margin: "8px 0 0", paddingLeft: 20, lineHeight: 1.8 }}>
              <li>The sheet is shared as <strong>"Anyone with the link" → Viewer</strong> — or, with a Google service account stored below, a private sheet shared with that account&apos;s address.</li>
              <li>Sheet layout matches the CSV formats: for a Grid, columns named after metrics and dimensions (member codes or labels).</li>
              <li>A saved integration re-fetches the sheet on every <strong>Sync Now</strong>, so the sheet stays the source of truth.</li>
            </ul>
          </div>
          <GoogleServiceAccountPanel />
        </>
      )}
    </div>
  );
}

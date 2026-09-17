import { useState, useCallback, useMemo, useRef } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import * as XLSX from "xlsx";
import { FileSpreadsheet, FileText, FolderOpen, ArrowLeft, ArrowRight, History, Pencil, Trash2, Play, Plus as PlusIcon, X as XIcon } from "lucide-react";
import { Button, IconButton, TextInput, Select, Field, StatusBadge, Stepper, InlineAlert, FilterChip, useConfirm, type DesignTone } from "../../ui";
import {
  api,
  type IntegrationDef,
  type GridDef,
  type FormDef,
  type DevDimension,
  type DevMetric,
  type DevModel,
  type DevRevision,
} from "../../api/client";

// ── Types ──────────────────────────────────────────────────────────────────────

type WizardStep = "upload" | "map" | "validate" | "commit";
type TargetType = "grid" | "form" | "dimension";
type MappingStatus = "auto" | "manual" | "ignored";

interface ParsedFile {
  name: string;
  ext: "csv" | "xlsx";
  sheets: string[];
  activeSheet: string;
  headers: string[];
  rows: string[][];
}

interface FieldOption {
  value: string;
  label: string;
  required: boolean;
  group?: string; // fields sharing a group are alternatives; at least one must be mapped
}

interface ColMapping {
  sourceCol: string;
  targetField: string;
  status: MappingStatus;
}

interface ValError {
  row: number;
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
  // "file" (default) uploads a local .csv/.xlsx; "google_sheets" fetches a
  // link-shared sheet server-side and then flows through the same steps.
  source?: "file" | "google_sheets";
  sheetUrl?: string;
}

interface CommitResult {
  rowsImported: number;
  errorRows: number;
  status: "success" | "error";
  message?: string;
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
    const buf = await file.arrayBuffer();
    const wb = XLSX.read(buf, { type: "array" });
    const activeSheet = sheet ?? wb.SheetNames[0];
    const ws = wb.Sheets[activeSheet];
    const data = ws ? XLSX.utils.sheet_to_json<unknown[]>(ws, { header: 1, defval: "" }) : [];
    const headers = data.length ? (data[0] as unknown[]).map(v => String(v ?? "").trim()) : [];
    const rows = data.slice(1).map(r => (r as unknown[]).map(v => String(v ?? "").trim()));
    return { name: file.name, ext: "xlsx", sheets: wb.SheetNames, activeSheet, headers, rows };
  } else {
    const text = await file.text();
    const { headers, rows } = parseCSVText(text);
    return { name: file.name, ext: "csv", sheets: [], activeSheet: "", headers, rows };
  }
}

// ── Auto-mapping ───────────────────────────────────────────────────────────────

function autoMap(headers: string[], opts: FieldOption[]): ColMapping[] {
  const norm = (s: string) => s.toLowerCase().replace(/[\s_-]+/g, "");
  return headers.map(col => {
    const cn = norm(col);
    const match = opts.find(o =>
      norm(o.value) === cn || norm(o.label) === cn ||
      norm(o.value).includes(cn) || cn.includes(norm(o.value)) ||
      norm(o.label).includes(cn) || cn.includes(norm(o.label))
    );
    return {
      sourceCol: col,
      targetField: match?.value ?? "",
      status: (match ? "auto" : "ignored") as MappingStatus,
    };
  });
}

// ── Target field builders ──────────────────────────────────────────────────────

function getFieldOptions(
  cfg: WizardConfig,
  grids: GridDef[],
  forms: FormDef[],
  dims: DevDimension[],
  knownProps: string[] = [],
): FieldOption[] {
  const opts: FieldOption[] = [];
  if (cfg.targetType === "grid") {
    opts.push({ value: "metric", label: "metric", required: false, group: "metric" });
    opts.push({ value: "value",  label: "value",  required: true });
    const grid = grids.find(g => g.id === cfg.targetId);
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

  // Resolve any metric identifier (label or name/code) → UUID required by the backend
  const metricByLabel = new Map(ctx.metrics.map(m => [m.label.toLowerCase(), m.id]));
  const metricByName  = new Map(ctx.metrics.map(m => [m.name.toLowerCase(),  m.id]));
  const resolveMetric = (v: string) =>
    metricByLabel.get(v.toLowerCase()) ?? metricByName.get(v.toLowerCase()) ?? v;

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

    // "metric" (unified) or legacy "metric_id"/"metric_name" → always resolve to UUID
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

function runValidation(
  headers: string[],
  rows: string[][],
  mappings: ColMapping[],
  reqFields: string[],
): ValError[] {
  const errs: ValError[] = [];
  const reqMapped = mappings.filter(m => m.targetField && reqFields.includes(m.targetField));
  rows.forEach((row, ri) => {
    if (row.every(v => !v.trim())) {
      errs.push({ row: ri + 2, col: "—", value: "", issue: "Empty row — will be skipped", sev: "warning" });
      return;
    }
    reqMapped.forEach(m => {
      const idx = headers.indexOf(m.sourceCol);
      const val = (row[idx] ?? "").trim();
      if (!val) {
        errs.push({ row: ri + 2, col: m.sourceCol, value: "", issue: `Required "${m.targetField}" is empty`, sev: "error" });
      }
    });
  });
  return errs;
}

// ── Styles ─────────────────────────────────────────────────────────────────────

// ── Step indicator ─────────────────────────────────────────────────────────────

const STEPS: { id: WizardStep; label: string }[] = [
  { id: "upload", label: "Upload" },
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
  onNext,
  onConfigChange,
}: {
  config: WizardConfig;
  grids: GridDef[];
  forms: FormDef[];
  dims: DevDimension[];
  onNext: (file: ParsedFile) => void;
  onConfigChange: (c: WizardConfig) => void;
}) {
  const { data: revisions = [] } = useQuery({ queryKey: ["dev-revisions"], queryFn: () => api.getDevRevisions() });
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
      setParsedFile({ name: "Google Sheet", ext: "csv", sheets: [], activeSheet: "", headers, rows });
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
            onChange={e => onConfigChange({ ...config, targetType: e.target.value as TargetType, targetId: "", targetLabel: "", revisionId: "", revisionName: "" })}
          >
            <option value="grid">Grid</option>
            <option value="form">Form</option>
            <option value="dimension">Dimension</option>
          </Select>
        </Field>
        <Field label={config.targetType === "grid" ? "Grid" : config.targetType === "form" ? "Form" : "Dimension"}>
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
                    { value: "incremental", label: "Incremental", hint: "Add / update rows, keep existing data" },
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
            description={'The sheet must be link-shared ("Anyone with the link" → Viewer). The worksheet tab in the URL (gid) is the one fetched.'}
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
          onClick={() => parsedFile && onNext(parsedFile)}>
          Next: Map Columns
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
  const requiredMapped = ungroupedOk && groupsOk && !unnamedProperty;

  const missingLabels = [
    ...fieldOptions.filter(f => f.required && !f.group && !mappedTargets.has(f.value)).map(f => f.label),
    ...groups
      .filter(g => !fieldOptions.filter(f => f.group === g).some(f => mappedTargets.has(f.value)))
      .map(g => fieldOptions.filter(f => f.group === g).map(f => f.label.split(" —")[0]).join(" or ")),
  ];

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
      {!requiredMapped && (
        <InlineAlert tone="warning" className="mvx-toolbar--spaced">
          Map all required target fields before proceeding.
          Still missing: {missingLabels.join(", ")}
        </InlineAlert>
      )}

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

      {/* Save as integration */}
      <div className="mvx-panel" style={{ marginBottom: 20, padding: "12px 16px", background: "var(--color-surface-subtle)" }}>
        {showSave ? (
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
  onNext,
  onBack,
}: {
  parsedFile: ParsedFile;
  errors: ValError[];
  onNext: (importValidOnly: boolean) => void;
  onBack: () => void;
}) {
  const [filter, setFilter] = useState<"all" | "errors" | "warnings" | "valid">("all");

  const errCount = errors.filter(e => e.sev === "error").length;
  const warnCount = errors.filter(e => e.sev === "warning").length;
  const validRows = parsedFile.rows.filter(r => !r.every(v => !v.trim())).length - errCount;
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

      {errors.length === 0 ? (
        <InlineAlert tone="success" className="mvx-toolbar--spaced">
          All rows are valid — ready to commit
        </InlineAlert>
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
                    <td className="mvx-admin-mono mvx-admin-muted">{e.row}</td>
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
        <Button leadingIcon={<ArrowLeft size={14} />} onClick={onBack}>Back</Button>
        <div style={{ display: "flex", gap: 10 }}>
          {hasErrors && (
            <Button onClick={() => onNext(true)}>
              Import Valid Rows Only ({Math.max(0, validRows)})
            </Button>
          )}
          <Button variant="primary" trailingIcon={<ArrowRight size={14} />} onClick={() => onNext(false)}>
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

  const errRows = new Set(errors.filter(e => e.sev === "error").map(e => e.row - 2));
  const rowsToImport = importValidOnly
    ? parsedFile.rows.filter((_, i) => !errRows.has(i))
    : parsedFile.rows;

  const commit = useMutation({
    mutationFn: async () => {
      const csv = resolveAndSerialize(parsedFile.headers, rowsToImport, mappings, resolveCtx);
      let out: { rows_imported: number; error_rows: number };
      if (config.integrationId) {
        out = await api.runIntegration(config.integrationId, csv);
      } else if (config.targetType === "dimension") {
        // One-off dimension imports have their own endpoint — the generic
        // upload endpoint is the GRID importer and rejects dimension CSVs
        // ("label matches no metric or dimension", reported live).
        out = await api.importDimensionMembers(config.targetId, csv);
      } else {
        const res = await api.uploadImport(csv, config.revisionId, config.importMode);
        out = { rows_imported: res.valid_rows, error_rows: res.error_rows };
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
          await api.updateIntegrationConfig(config.draftId, {
            column_map: Object.fromEntries(mappings.filter(m => m.targetField).map(m => [m.sourceCol, m.targetField])),
            ...(config.source === "google_sheets" ? { sheet_url: config.sheetUrl, import_mode: config.importMode } : {}),
          });
        } catch { /* draft stays a draft; the import result stands */ }
      }
      return out;
    },
    onSuccess: (data) => {
      qc.invalidateQueries({ queryKey: ["import-jobs"] });
      qc.invalidateQueries({ queryKey: ["dev-integrations"] });
      setResult({ rowsImported: data.rows_imported, errorRows: data.error_rows, status: "success" });
    },
    onError: (e) => {
      setResult({ rowsImported: 0, errorRows: 0, status: "error", message: e instanceof Error ? e.message : "Import failed" });
    },
  });

  const TARGET_LABEL: Record<string, string> = { grid: "Grid", form: "Form", dimension: "Dimension" };

  if (result) {
    return (
      <div style={{ textAlign: "center", padding: "24px 0" }}>
        {result.status === "success" ? (
          <>
            <h3 style={{ margin: "0 0 8px", color: "var(--color-live)" }}>Import Complete</h3>
            <p className="mvx-admin-muted" style={{ margin: "0 0 20px", fontSize: 13 }}>
              {result.rowsImported} rows imported
              {result.errorRows > 0 ? `, ${result.errorRows} rows had errors` : ""}
            </p>
          </>
        ) : (
          <>
            <h3 style={{ margin: "0 0 8px", color: "var(--color-danger)" }}>Import Failed</h3>
            <p className="mvx-admin-muted" style={{ margin: "0 0 20px", fontSize: 13 }}>{result.message}</p>
          </>
        )}
        <Button variant="primary" onClick={onDone}>Done</Button>
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
            errors.length > 0 ? ["Rows skipped (errors)", importValidOnly ? String(errRows.size) : "0 (importing all)"] : null,
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
              {["Date", "Revision", "Rows", "Errors", "Status", ""].map((h, i) => (
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
                  <td>{j.revision_name}</td>
                  <td className="mvx-admin-mono">{j.total_rows ?? "—"}</td>
                  <td className="mvx-admin-mono" style={{ color: (j.error_rows ?? 0) > 0 ? "var(--color-danger)" : "var(--color-text-subtle)" }}>
                    {j.error_rows ?? 0}
                  </td>
                  <td>
                    <StatusBadge tone={s.tone}>{s.label}</StatusBadge>
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
  onClose,
}: {
  initialConfig?: Partial<WizardConfig>;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const [step, setStep] = useState<WizardStep>("upload");
  const [config, setConfig] = useState<WizardConfig>({
    targetType: "grid",
    targetId: "",
    targetLabel: "",
    importMode: "incremental",
    ...initialConfig,
  });
  const [parsedFile, setParsedFile] = useState<ParsedFile | null>(null);
  const [mappings, setMappings] = useState<ColMapping[]>([]);
  const [errors, setErrors] = useState<ValError[]>([]);
  const [importValidOnly, setImportValidOnly] = useState(false);

  const { data: grids = [] } = useQuery({ queryKey: ["dev-grids"], queryFn: () => api.listGrids() });
  const { data: forms = [] } = useQuery({ queryKey: ["forms"], queryFn: () => api.listForms() });
  const { data: dims = [] } = useQuery({ queryKey: ["dev-dimensions"], queryFn: () => api.getDevDimensions() });
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
    ? getFieldOptions(config, grids as GridDef[], forms as FormDef[], dims as DevDimension[], knownProps)
    : [];

  // For row-level validation: only flag fields that are actually mapped and required
  const reqFields = fieldOptions
    .filter(f => f.required && !f.group)
    .map(f => f.value);

  const handleFileReady = (pf: ParsedFile) => {
    setParsedFile(pf);
    const opts = getFieldOptions(config, grids as GridDef[], forms as FormDef[], dims as DevDimension[], knownProps);
    setMappings(autoMap(pf.headers, opts));
    setStep("map");
  };

  const handleMappingDone = () => {
    if (!parsedFile) return;
    const errs = runValidation(parsedFile.headers, parsedFile.rows, mappings, reqFields);
    setErrors(errs);
    setStep("validate");
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
      }).then(res =>
        api.updateIntegrationConfig(res.id, {
          column_map: Object.fromEntries(mappings.filter(m => m.targetField).map(m => [m.sourceCol, m.targetField])),
          // A google_sheets integration stores its source and mode so a run
          // needs no input at all — the server re-fetches the sheet.
          ...(config.source === "google_sheets"
            ? { sheet_url: config.sheetUrl, import_mode: config.importMode }
            : {}),
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
          onNext={handleFileReady}
          onConfigChange={setConfig}
        />
      )}

      {step === "map" && parsedFile && (
        <MapColumnsStep
          parsedFile={parsedFile}
          fieldOptions={fieldOptions}
          mappings={mappings}
          onMappingChange={setMappings}
          onNext={handleMappingDone}
          onBack={() => setStep("upload")}
          onSaveAsIntegration={(name, asDraft) => saveAsIntegration.mutate({ name, asDraft })}
          savedIntegrationName={config.integrationName}
          allowProperties={config.targetType === "dimension"}
        />
      )}

      {step === "validate" && parsedFile && (
        <ValidateStep
          parsedFile={parsedFile}
          errors={errors}
          onNext={handleValidateDone}
          onBack={() => setStep("map")}
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
  onRun,
}: {
  sourceType: "csv_import" | "google_sheets";
  onRun: (cfg: Partial<WizardConfig>) => void;
}) {
  const qc = useQueryClient();
  const { data: integrations = [] } = useQuery({ queryKey: ["dev-integrations"], queryFn: () => api.listDevIntegrations() });
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

  const TARGET_LABEL: Record<string, string> = { grid: "Grid", form: "Form", dimension: "Dimension" };
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
                    {Object.keys(d.config.column_map).length} columns mapped
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
              ) : (
                <Button
                  size="sm"
                  leadingIcon={<Play size={13} />}
                  onClick={() => onRun({ integrationId: d.id, integrationName: d.name, targetType: d.target_type as TargetType, targetId: d.target_id })}
                >
                  Run Import
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

export function ExcelImportSection() {
  const [wizardConfig, setWizardConfig] = useState<Partial<WizardConfig> | null>(null);

  const openWizard = (cfg?: Partial<WizardConfig>) => setWizardConfig(cfg ?? {});
  const closeWizard = () => setWizardConfig(null);

  return (
    <div>
      {wizardConfig !== null ? (
        <ImportWizard initialConfig={wizardConfig} onClose={closeWizard} />
      ) : (
        <>
          <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center", gap: 12, marginBottom: 20 }}>
            <p className="mvx-admin-muted" style={{ fontSize: 13, margin: 0 }}>
              Upload a CSV or Excel file, map columns to your model fields, validate, and commit data to a Grid, Form, or Dimension.
            </p>
            <Button variant="primary" leadingIcon={<PlusIcon size={14} />} style={{ whiteSpace: "nowrap" }} onClick={() => openWizard()}>
              New Import
            </Button>
          </div>

          <SavedIntegrationsList sourceType="csv_import" onRun={cfg => openWizard(cfg)} />
          <ImportHistory />

          <div className="mvx-panel" style={{ marginTop: 24, padding: 16, background: "var(--color-surface-subtle)", fontSize: 13, color: "var(--color-text-muted)" }}>
            <strong style={{ color: "var(--color-text)" }}>CSV format by target type:</strong>
            <ul style={{ margin: "8px 0 0", paddingLeft: 20, lineHeight: 1.8 }}>
              <li><strong>Grid:</strong> <code>metric_id, value</code> + one column per dimension with member codes</li>
              <li><strong>Form:</strong> columns = form field names (e.g. <code>amount, description, date</code>)</li>
              <li><strong>Dimension:</strong> <code>code, label</code> and optionally <code>parent_code</code></li>
            </ul>
          </div>
        </>
      )}
    </div>
  );
}

export function GoogleSheetsImportSection() {
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
        <ImportWizard initialConfig={wizardConfig} onClose={closeWizard} />
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

          <SavedIntegrationsList sourceType="google_sheets" onRun={cfg => openWizard(cfg)} />
          <ImportHistory />

          <div className="mvx-panel" style={{ marginTop: 24, padding: 16, background: "var(--color-surface-subtle)", fontSize: 13, color: "var(--color-text-muted)" }}>
            <strong style={{ color: "var(--color-text)" }}>How it works:</strong>
            <ul style={{ margin: "8px 0 0", paddingLeft: 20, lineHeight: 1.8 }}>
              <li>The sheet must be shared as <strong>"Anyone with the link" → Viewer</strong>; no Google account is connected.</li>
              <li>Sheet layout matches the CSV formats: for a Grid, columns named after metrics and dimensions (member codes or labels).</li>
              <li>A saved integration re-fetches the sheet on every <strong>Sync Now</strong>, so the sheet stays the source of truth.</li>
            </ul>
          </div>
        </>
      )}
    </div>
  );
}

import { useMemo, useState } from "react";
import { ArrowLeft, ArrowRight, Eye, Plus as PlusIcon, X as XIcon } from "lucide-react";
import { api, cleanReshape, isEmptyReshape, type ApiError, type FileReshape } from "../../api/client";
import { Button, IconButton, TextInput, Select, Field, InlineAlert } from "../../ui";

// The Import Wizard's Shape step: turns a sheet laid out for people — a
// title above the header, months across the columns, a label written once
// per group, total rows, figures in thousands — into one row per value,
// before the columns are mapped. The preview is the server's
// (POST /api/import/reshape-preview), by the same code every run of a saved
// integration applies, so what is shown here is what an import reads.

export interface ShapeSource {
  csv?: string;
  xlsx_base64?: string;
  sheet?: string;
}

export interface Shaped {
  headers: string[];
  rows: string[][];
}

interface Preview {
  raw: string[][];
  header?: string[];
  rows?: string[][];
  row_count?: number;
  error?: string;
}

const SKIP_KINDS = [
  { value: "contains", label: "contains" },
  { value: "equals", label: "is" },
  { value: "blank", label: "is blank" },
] as const;

export function ShapeStep({
  fileName,
  source,
  asRead,
  isCsv,
  reshape,
  onReshapeChange,
  onNext,
  onBack,
}: {
  fileName: string;
  source: ShapeSource;
  /** The file as the browser parsed it: its first row is taken for the header. */
  asRead: Shaped;
  isCsv: boolean;
  reshape: FileReshape;
  onReshapeChange: (r: FileReshape) => void;
  onNext: (shaped: Shaped) => void;
  onBack: () => void;
}) {
  const [preview, setPreview] = useState<Preview | null>(null);
  const [busy, setBusy] = useState(false);
  const r = reshape;
  const set = (patch: Partial<FileReshape>) => { onReshapeChange({ ...r, ...patch }); setPreview(null); };

  // Column names to offer: the header row as read, then what the unpivot
  // and the fixed values add. Free text is accepted too.
  const rawGrid = preview?.raw ?? [asRead.headers, ...asRead.rows.slice(0, 11)];
  const headerAt = (rawGrid[(r.header_row ?? 1) - 1] ?? []).map(c => String(c ?? "").trim()).filter(Boolean);
  const columns = useMemo(() => {
    const out = new Set(headerAt);
    if (r.unpivot?.name_column) out.add(r.unpivot.name_column);
    if (r.unpivot?.value_column) out.add(r.unpivot.value_column);
    Object.keys(r.constants ?? {}).forEach(k => k && out.add(k));
    return [...out];
  }, [headerAt, r.unpivot, r.constants]);

  const run = async (): Promise<Preview | null> => {
    setBusy(true);
    try {
      const res = await api.reshapePreview({ ...source, reshape: cleanReshape(r) });
      setPreview(res);
      return res;
    } catch (e) {
      const body = (e as ApiError).body as { error?: string; raw?: string[][] } | undefined;
      const p = { raw: body?.raw ?? rawGrid, error: body?.error ?? (e as Error).message };
      setPreview(p);
      return p;
    } finally {
      setBusy(false);
    }
  };

  const next = async () => {
    if (isEmptyReshape(cleanReshape(r))) {
      onNext(asRead);
      return;
    }
    const p = preview?.header ? preview : await run();
    if (p?.header && p.rows) onNext({ headers: p.header, rows: p.rows });
  };

  const list = "mvx-shape-cols";
  const colInput = (value: string, onChange: (v: string) => void, label: string) => (
    <TextInput value={value} onChange={e => onChange(e.target.value)} list={list} placeholder="column" aria-label={label} style={{ width: 150 }} />
  );
  // numbered: the sheet as read shows each row's number — the one Header
  // row takes (a CSV reader skips blank lines; a workbook keeps them).
  const rows = (cells: string[][], header?: string[], numbered?: boolean) => (
    <div style={{ overflowX: "auto", border: "1px solid var(--color-border)", borderRadius: 6 }}>
      <table className="mvx-table" style={{ fontSize: 12, width: "100%" }}>
        {header && <thead><tr>{header.map((h, i) => <th key={i}>{h}</th>)}</tr></thead>}
        <tbody>{cells.map((row, i) => (
          <tr key={i}>
            {numbered && <td className="mvx-admin-muted" style={{ width: 28, textAlign: "right" }}>{i + 1}</td>}
            {row.map((c, j) => <td key={j}>{c}</td>)}
          </tr>
        ))}</tbody>
      </table>
    </div>
  );
  const unpivotOn = !!r.unpivot;
  const skip = r.skip_rows ?? [];
  const constants = Object.entries(r.constants ?? {});
  const valueMap = Object.entries(r.value_map ?? {}).flatMap(([col, m]) => Object.entries(m).map(([from, to]) => ({ col, from, to })));
  const scale = Object.entries(r.scale ?? {});
  const setValueMap = (entries: { col: string; from: string; to: string }[]) => {
    const vm: Record<string, Record<string, string>> = {};
    entries.forEach(({ col, from, to }) => { (vm[col] ??= {})[from] = to; });
    set({ value_map: vm });
  };

  return (
    <div data-testid="shape-step">
      <datalist id={list}>{columns.map(c => <option key={c} value={c} />)}</datalist>
      <p className="mvx-admin-muted" style={{ fontSize: 13, marginTop: 0 }}>
        Optional. Shape <b>{fileName}</b> when it is laid out for people — a title above the header, months across the columns,
        a label written once per group, total rows, figures in thousands. A saved integration shapes every later file the same way.
      </p>

      <div style={{ display: "grid", gap: 14, marginBottom: 16 }}>
        <div style={{ display: "flex", gap: 16, flexWrap: "wrap" }}>
          <Field label="Header row" description="The row holding the column names; rows above it are dropped.">
            <TextInput type="number" min={1} value={String(r.header_row ?? 1)} aria-label="Header row" style={{ width: 90 }}
              onChange={e => set({ header_row: Math.max(1, Number(e.target.value) || 1) })} />
          </Field>
          {isCsv && (
            <Field label="Separator">
              <Select value={r.delimiter ?? ","} aria-label="Separator" onChange={e => set({ delimiter: e.target.value === "," ? undefined : e.target.value })}>
                <option value=",">comma</option><option value=";">semicolon</option><option value={"\\t"}>tab</option><option value="|">bar</option>
              </Select>
            </Field>
          )}
          <Field label="Fill down" description="Columns whose blank cells take the value above.">
            {colInput((r.fill_down ?? []).join(", "), v => set({ fill_down: v.split(",").map(s => s.trim()).filter(Boolean) }), "Fill down columns")}
          </Field>
        </div>

        <Field label="Skip rows">
          <div style={{ display: "grid", gap: 6 }}>
            {skip.map((f, i) => {
              const kind = f.blank ? "blank" : f.equals !== undefined ? "equals" : "contains";
              const update = (patch: Partial<typeof f>) => set({ skip_rows: skip.map((g, j) => j === i ? { ...g, ...patch } : g) });
              return (
                <div key={i} className="mvx-admin-inline-form" style={{ flexWrap: "nowrap" }}>
                  {colInput(f.column ?? "", v => update({ column: v || undefined }), `Skip rule ${i + 1} column`)}
                  <Select value={kind} aria-label={`Skip rule ${i + 1} test`} style={{ width: 120 }} onChange={e => {
                    const k = e.target.value;
                    const v = f.equals ?? f.contains ?? "";
                    update(k === "blank" ? { blank: true, equals: undefined, contains: undefined } : k === "equals" ? { equals: v, contains: undefined, blank: undefined } : { contains: v, equals: undefined, blank: undefined });
                  }}>
                    {SKIP_KINDS.map(k => <option key={k.value} value={k.value}>{k.label}</option>)}
                  </Select>
                  {kind !== "blank" && (
                    <TextInput value={f.equals ?? f.contains ?? ""} aria-label={`Skip rule ${i + 1} value`} placeholder="Total" style={{ width: 140 }}
                      onChange={e => update(kind === "equals" ? { equals: e.target.value } : { contains: e.target.value })} />
                  )}
                  <IconButton aria-label={`Remove skip rule ${i + 1}`} onClick={() => set({ skip_rows: skip.filter((_, j) => j !== i) })}><XIcon size={13} /></IconButton>
                </div>
              );
            })}
            <Button size="sm" variant="ghost" leadingIcon={<PlusIcon size={13} />} style={{ justifySelf: "start" }}
              onClick={() => set({ skip_rows: [...skip, { contains: "" }] })}>Add a rule</Button>
          </div>
        </Field>

        <Field label="Columns into rows" description="Months (or any values) across the columns become one row each.">
          <div className="mvx-admin-inline-form">
            <label style={{ display: "inline-flex", gap: 6, alignItems: "center", fontSize: 13 }}>
              <input type="checkbox" checked={unpivotOn} aria-label="Turn columns into rows"
                onChange={e => set({ unpivot: e.target.checked ? { from: "", to: "", name_column: "", value_column: "" } : undefined })} />
              Turn columns into rows
            </label>
            {unpivotOn && r.unpivot && (
              <>
                from {colInput(r.unpivot.from ?? "", v => set({ unpivot: { ...r.unpivot!, from: v, columns: undefined } }), "First column to turn")}
                to {colInput(r.unpivot.to ?? "", v => set({ unpivot: { ...r.unpivot!, to: v, columns: undefined } }), "Last column to turn")}
                into
                <TextInput value={r.unpivot.name_column} placeholder="Month" aria-label="Column for the headers" style={{ width: 110 }}
                  onChange={e => set({ unpivot: { ...r.unpivot!, name_column: e.target.value } })} />
                and
                <TextInput value={r.unpivot.value_column} placeholder="value" aria-label="Column for the values" style={{ width: 110 }}
                  onChange={e => set({ unpivot: { ...r.unpivot!, value_column: e.target.value } })} />
              </>
            )}
          </div>
        </Field>

        <Field label="Fixed values" description="What the file means but does not say, on every row (Scenario = Budget).">
          <div style={{ display: "grid", gap: 6 }}>
            {constants.map(([k, v], i) => (
              <div key={i} className="mvx-admin-inline-form" style={{ flexWrap: "nowrap" }}>
                <TextInput value={k} placeholder="Scenario" aria-label={`Fixed value ${i + 1} column`} style={{ width: 150 }}
                  onChange={e => set({ constants: Object.fromEntries(constants.map(([k2, v2], j) => j === i ? [e.target.value, v2] : [k2, v2])) })} />
                =
                <TextInput value={v} placeholder="Budget" aria-label={`Fixed value ${i + 1} value`} style={{ width: 150 }}
                  onChange={e => set({ constants: Object.fromEntries(constants.map(([k2, v2], j) => j === i ? [k2, e.target.value] : [k2, v2])) })} />
                <IconButton aria-label={`Remove fixed value ${i + 1}`} onClick={() => set({ constants: Object.fromEntries(constants.filter((_, j) => j !== i)) })}><XIcon size={13} /></IconButton>
              </div>
            ))}
            <Button size="sm" variant="ghost" leadingIcon={<PlusIcon size={13} />} style={{ justifySelf: "start" }}
              onClick={() => set({ constants: { ...(r.constants ?? {}), [""]: "" } })}>Add a fixed value</Button>
          </div>
        </Field>

        <Field label="Rename values" description="Labels in the file to the model's member codes (Canada → CA, Jan → 2026-01).">
          <div style={{ display: "grid", gap: 6 }}>
            {valueMap.map((m, i) => (
              <div key={i} className="mvx-admin-inline-form" style={{ flexWrap: "nowrap" }}>
                in {colInput(m.col, v => setValueMap(valueMap.map((n, j) => j === i ? { ...n, col: v } : n)), `Rename ${i + 1} column`)}
                <TextInput value={m.from} placeholder="Canada" aria-label={`Rename ${i + 1} from`} style={{ width: 130 }}
                  onChange={e => setValueMap(valueMap.map((n, j) => j === i ? { ...n, from: e.target.value } : n))} />
                →
                <TextInput value={m.to} placeholder="CA" aria-label={`Rename ${i + 1} to`} style={{ width: 130 }}
                  onChange={e => setValueMap(valueMap.map((n, j) => j === i ? { ...n, to: e.target.value } : n))} />
                <IconButton aria-label={`Remove rename ${i + 1}`} onClick={() => setValueMap(valueMap.filter((_, j) => j !== i))}><XIcon size={13} /></IconButton>
              </div>
            ))}
            <Button size="sm" variant="ghost" leadingIcon={<PlusIcon size={13} />} style={{ justifySelf: "start" }}
              onClick={() => setValueMap([...valueMap, { col: valueMap.at(-1)?.col ?? "", from: "", to: "" }])}>Add a rename</Button>
          </div>
        </Field>

        <Field label="Numbers" description='Read "1,234.50", "(123)", "12%" and currency signs; the turned-into-rows values are always read so.'>
          <div className="mvx-admin-inline-form">
            {colInput((r.number_columns ?? []).join(", "), v => set({ number_columns: v.split(",").map(s => s.trim()).filter(Boolean) }), "Number columns")}
            <label style={{ display: "inline-flex", gap: 6, alignItems: "center", fontSize: 13 }}>
              <input type="checkbox" checked={!!r.decimal_comma} aria-label="Decimal comma" onChange={e => set({ decimal_comma: e.target.checked || undefined })} />
              decimal comma (1.234,5)
            </label>
            {scale.map(([col, f], i) => (
              <span key={i} className="mvx-admin-inline-form" style={{ flexWrap: "nowrap" }}>
                {colInput(col, v => set({ scale: Object.fromEntries(scale.map(([c2, f2], j) => j === i ? [v, f2] : [c2, f2])) }), `Scale ${i + 1} column`)}
                ×
                <TextInput type="number" value={String(f)} aria-label={`Scale ${i + 1} factor`} style={{ width: 90 }}
                  onChange={e => set({ scale: Object.fromEntries(scale.map(([c2, f2], j) => j === i ? [c2, Number(e.target.value)] : [c2, f2])) })} />
                <IconButton aria-label={`Remove scale ${i + 1}`} onClick={() => set({ scale: Object.fromEntries(scale.filter((_, j) => j !== i)) })}><XIcon size={13} /></IconButton>
              </span>
            ))}
            <Button size="sm" variant="ghost" leadingIcon={<PlusIcon size={13} />}
              onClick={() => set({ scale: { ...(r.scale ?? {}), [r.unpivot?.value_column || ""]: 1000 } })}>Scale a column</Button>
          </div>
        </Field>
      </div>

      <div style={{ display: "flex", gap: 8, alignItems: "center", marginBottom: 12 }}>
        <Button size="sm" leadingIcon={<Eye size={13} />} loading={busy} loadingLabel="Shaping…" onClick={() => void run()}>Preview</Button>
        {preview?.row_count !== undefined && <span className="mvx-admin-muted" style={{ fontSize: 13 }} role="status">{preview.row_count} row(s) after shaping</span>}
      </div>
      {preview?.error && <div style={{ marginBottom: 12 }}><InlineAlert tone="danger">{preview.error}</InlineAlert></div>}
      <div style={{ display: "grid", gap: 12, marginBottom: 20 }}>
        <div>
          <div className="mvx-admin-muted" style={{ fontSize: 12, marginBottom: 4 }}>The file as read (first rows, numbered as Header row counts them){!preview && " — Preview to read it as the import will"}</div>
          {rows(rawGrid.slice(0, 8), undefined, true)}
        </div>
        {preview?.header && preview.rows && (
          <div data-testid="shape-preview">
            <div className="mvx-admin-muted" style={{ fontSize: 12, marginBottom: 4 }}>After shaping (first rows)</div>
            {rows(preview.rows.slice(0, 8), preview.header)}
          </div>
        )}
      </div>

      <div style={{ display: "flex", justifyContent: "space-between" }}>
        <Button leadingIcon={<ArrowLeft size={14} />} onClick={onBack}>Back</Button>
        <Button variant="primary" trailingIcon={<ArrowRight size={14} />} loading={busy} loadingLabel="Shaping…" onClick={() => void next()}>
          {isEmptyReshape(cleanReshape(r)) ? "Next: Map Columns (no shaping)" : "Next: Map Columns"}
        </Button>
      </div>
    </div>
  );
}

import { useState } from "react";
import { X, ArrowUp, ArrowDown } from "lucide-react";
import type { BoardSettings, DevDimension, FormDef, Metric, RecordFilter, WidgetProps } from "../../api/client";
import { Button, Checkbox, Field, IconButton, Select } from "../../ui";
import { FilterBuilder } from "../business/FormPanel";
import { describeFilter, listColumns } from "../business/recordList";

const hint = { fontSize: 11, color: "var(--color-text-quiet)", marginTop: 4 } as const;

/**
 * A board's layout, for its developer: the column field (any dimension
 * field — its members in order are the columns — or a select field), what
 * titles and fills each card, the number totalled per column and the member
 * property colouring each column.
 */
export function BoardSettingsEditor({ form, dims, value, onChange }: {
  form: FormDef;
  dims: DevDimension[];
  value: BoardSettings;
  onChange: (next: BoardSettings) => void;
}) {
  const columnFields = form.fields.filter(f => (f.type === "dimension" && f.dimension_id && !f.multiple) || f.type === "select"); // a card stands in one column
  const colField = form.fields.find(f => f.name === value.column_field);
  const colDim = colField?.type === "dimension" ? dims.find(d => d.id === colField.dimension_id) : undefined;
  const colorProps = [...new Set((colDim?.members ?? []).flatMap(m => Object.keys(m.properties ?? {})))].sort();
  const set = (patch: Partial<BoardSettings>) => onChange({ ...value, ...patch });
  const cardFields = value.card_fields ?? [];
  const shown = form.fields.filter(f => f.type !== "status" && f.name !== value.column_field);
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
      <Field label="Columns" description="Any dimension field — its members, in their order, are the columns — or a select field's choices. Moving a card sets it.">
        <Select value={value.column_field} onChange={e => set({ column_field: e.target.value, color_property: undefined })} aria-label="Board columns">
          <option value="">— choose a field —</option>
          {columnFields.map(f => <option key={f.name} value={f.name}>{f.label || f.name}</option>)}
        </Select>
      </Field>
      <Field label="Card title">
        <Select value={value.title_field ?? ""} onChange={e => set({ title_field: e.target.value || undefined })} aria-label="Card title">
          <option value="">The record's name</option>
          {shown.map(f => <option key={f.name} value={f.name}>{f.label || f.name}</option>)}
        </Select>
      </Field>
      <div>
        <div className="mvx-prop-section">Card shows</div>
        <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
          {shown.map(f => (
            <Checkbox key={f.name} checked={cardFields.includes(f.name)} label={f.label || f.name}
              onChange={e => set({ card_fields: e.target.checked ? [...cardFields, f.name] : cardFields.filter(n => n !== f.name) })} />
          ))}
        </div>
      </div>
      <Field label="Total per column">
        <Select value={value.sum_field ?? ""} onChange={e => set({ sum_field: e.target.value || undefined })} aria-label="Total per column">
          <option value="">Count only</option>
          {form.fields.filter(f => f.type === "number").map(f => <option key={f.name} value={f.name}>{f.label || f.name}</option>)}
        </Select>
      </Field>
      {colDim && (
        <Field label="Column colour" description="A property of the column dimension's members holding a colour (#2f80ed, teal).">
          <Select value={value.color_property ?? ""} onChange={e => set({ color_property: e.target.value || undefined })} aria-label="Column colour">
            <option value="">None</option>
            {colorProps.map(p => <option key={p} value={p}>{p}</option>)}
          </Select>
        </Field>
      )}
      <Checkbox checked={!!value.hide_empty} label="Hide columns without cards" onChange={e => set({ hide_empty: e.target.checked || undefined })} />
    </div>
  );
}

/**
 * The filters a records widget or board keeps whatever its viewer does —
 * relative dates count from the viewer's day ("Due before today") — and,
 * for a records widget, the columns it shows and its order.
 */
export function RecordPresetsEditor({ form, dims, metrics, value, onChange, withColumns }: {
  form: FormDef;
  dims: DevDimension[];
  metrics: Metric[];
  value: WidgetProps;
  onChange: (patch: Partial<WidgetProps>) => void;
  withColumns: boolean;
}) {
  const [adding, setAdding] = useState(false);
  const cols = listColumns(form);
  const filters: RecordFilter[] = value.record_filters ?? [];
  const chosen = value.record_columns ?? [];
  const move = (i: number, by: number) => {
    const next = [...chosen];
    const j = i + by;
    if (j < 0 || j >= next.length) return;
    [next[i], next[j]] = [next[j], next[i]];
    onChange({ record_columns: next });
  };
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
      <div>
        <div className="mvx-prop-section">Fixed filters</div>
        {filters.length === 0 && <p style={hint}>None: every record the viewer may see.</p>}
        {filters.map((f, i) => (
          <div key={i} style={{ display: "flex", alignItems: "center", gap: 4, fontSize: 12 }}>
            <span style={{ flex: 1 }}>{describeFilter(f, cols, dims, metrics)}</span>
            <IconButton aria-label={`Remove fixed filter ${describeFilter(f, cols, dims, metrics)}`} size={22}
              onClick={() => onChange({ record_filters: filters.filter((_, j) => j !== i) })}>
              <X size={12} />
            </IconButton>
          </div>
        ))}
        {adding ? (
          <FilterBuilder cols={cols} dims={dims} metrics={metrics} statuses={["draft", "submitted", "approved", "rejected"]}
            onAdd={f => { onChange({ record_filters: [...filters, f] }); setAdding(false); }} onCancel={() => setAdding(false)} />
        ) : (
          <Button size="sm" variant="ghost" onClick={() => setAdding(true)}>Add a fixed filter</Button>
        )}
        <p style={hint}>Viewers see them as locked chips and cannot remove them; their own search and filters narrow further.</p>
      </div>
      {withColumns && (
        <div>
          <div className="mvx-prop-section">Columns</div>
          <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
            {cols.map(c => (
              <Checkbox key={c.key} checked={chosen.includes(c.key)} label={c.label}
                onChange={e => onChange({ record_columns: e.target.checked ? [...chosen, c.key] : chosen.filter(k => k !== c.key) })} />
            ))}
          </div>
          {chosen.length > 1 && (
            <div style={{ marginTop: 6 }}>
              {chosen.map((k, i) => (
                <div key={k} style={{ display: "flex", alignItems: "center", gap: 4, fontSize: 12 }}>
                  <span style={{ flex: 1 }}>{cols.find(c => c.key === k)?.label ?? k}</span>
                  <IconButton aria-label={`Move column ${k} up`} size={22} disabled={i === 0} onClick={() => move(i, -1)}><ArrowUp size={12} /></IconButton>
                  <IconButton aria-label={`Move column ${k} down`} size={22} disabled={i === chosen.length - 1} onClick={() => move(i, 1)}><ArrowDown size={12} /></IconButton>
                </div>
              ))}
            </div>
          )}
          <p style={hint}>None chosen: every field.</p>
          <Field label="Order">
            <div style={{ display: "flex", gap: 6 }}>
              <Select value={value.record_sort?.field ?? ""} aria-label="Order by"
                onChange={e => onChange({ record_sort: e.target.value ? { field: e.target.value, desc: value.record_sort?.desc } : undefined })}>
                <option value="">Newest first</option>
                {cols.map(c => <option key={c.key} value={c.key}>{c.label}</option>)}
              </Select>
              {value.record_sort && (
                <Select value={value.record_sort.desc ? "desc" : "asc"} aria-label="Direction"
                  onChange={e => onChange({ record_sort: { field: value.record_sort!.field, desc: e.target.value === "desc" } })}>
                  <option value="asc">Ascending</option>
                  <option value="desc">Descending</option>
                </Select>
              )}
            </div>
          </Field>
        </div>
      )}
    </div>
  );
}

/**
 * How a records widget or board takes part in its dashboard's selection — one member per dimension, which the
 * selector bar and synced grids share: a records widget over a table that keeps a dimension may select its clicked
 * record's member (record_selects), and any of them may show only the records whose dimension fields hold the
 * selected members (record_follows) — a journal clicked above, its lines below.
 */
export function RecordSelectionEditor({ form, dims, value, onChange, canSelect }: {
  form: FormDef;
  dims: DevDimension[];
  value: WidgetProps;
  onChange: (patch: Partial<WidgetProps>) => void;
  /** A records widget, not a board: a card clicked opens its record. */
  canSelect: boolean;
}) {
  const keeps = !!form.keeps_dimension_id;
  const keptLabel = dims.find(d => d.id === form.keeps_dimension_id)?.label || dims.find(d => d.id === form.keeps_dimension_id)?.name || "its dimension";
  const dimFields = form.fields.filter(f => f.type === "dimension" && f.dimension_id);
  const follows = value.record_follows ?? [];
  const toggle = (name: string, on: boolean) => {
    const next = on ? [...follows.filter(n => n !== name), name] : follows.filter(n => n !== name);
    onChange({ record_follows: next.length > 0 ? next : undefined });
  };
  if (!(canSelect && keeps) && dimFields.length === 0) return null;
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
      <div className="mvx-prop-section">Dashboard selection</div>
      {canSelect && keeps && (
        <>
          <Checkbox checked={!!value.record_selects} label="Clicking a record selects it on the dashboard"
            onChange={e => onChange({ record_selects: e.target.checked || undefined })} />
          <p style={hint}>Its {keptLabel} member becomes the dashboard's selection, as on the selector bar: widgets
            following {keptLabel} show what belongs to it. The eye beside a record opens it.</p>
        </>
      )}
      {dimFields.length > 0 && (
        <>
          <p style={{ ...hint, marginTop: canSelect && keeps ? 6 : 0 }}>Show only the records whose field holds the dashboard's selected member:</p>
          {dimFields.map(f => {
            const sameDim = follows.some(n => n !== f.name && form.fields.find(x => x.name === n)?.dimension_id === f.dimension_id);
            return (
              <Checkbox key={f.name} checked={follows.includes(f.name)} disabled={sameDim && !follows.includes(f.name)}
                label={f.label || f.name} onChange={e => toggle(f.name, e.target.checked)} />
            );
          })}
          <p style={hint}>Nothing selected: every record. A parent selected: the records under it. + New starts with the
            selected member.</p>
        </>
      )}
    </div>
  );
}

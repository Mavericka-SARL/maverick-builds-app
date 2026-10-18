// What the records widget shows of a table and how its list is asked for:
// columns, the sort a header click moves to, the filters a person can add,
// and how a stored value reads on screen. The list itself is the server's
// records query (POST /api/forms/{id}/records/query), so search, filters,
// sort and pages cover every record the person may see, not one page.
import type { DevDimension, FormDef, FormField, FormRecord, Metric, RecordFilter, RecordsQuery } from "../../api/client";
import { localToday, statusFieldOf } from "../../api/client";

/** A sortable, filterable column of the list. */
export interface ListColumn {
  key: string;
  label: string;
  kind: "text" | "number" | "date" | "boolean" | "datetime" | "status";
  field?: FormField;
}

/** The kind a field compares and sorts as. */
export function fieldKind(f: FormField): ListColumn["kind"] {
  if (f.type === "status") return "status";
  if (f.type === "number" || (f.type === "metric" && !!f.metric_id)) return "number";
  if (f.type === "date") return "date";
  if (f.type === "boolean") return "boolean";
  return "text";
}

/**
 * The list's columns: the table's fields — its status field last among
 * them, under the name the developer gave it — then when each record was
 * created. A table without a status field has no status column.
 */
export function listColumns(form: FormDef, only?: string[]): ListColumn[] {
  const cols: ListColumn[] = form.fields
    .filter(f => f.type !== "status" && !f.hidden)
    .map(f => ({ key: f.name, label: f.label || f.name, kind: fieldKind(f), field: f }));
  const status = statusFieldOf(form);
  if (status) cols.push({ key: status.name, label: status.label || "Status", kind: "status", field: status });
  cols.push({ key: "created_at", label: "Created", kind: "datetime" });
  // A widget naming its columns shows those, in its order.
  if (only && only.length > 0) {
    const byKey = new Map(cols.map(c => [c.key, c]));
    const picked = only.map(k => byKey.get(k)).filter((c): c is ListColumn => !!c);
    if (picked.length > 0) return picked;
  }
  return cols;
}

export type ListSort = { field: string; desc: boolean } | null;

/** A header click: ascending, then descending, then back to newest first. */
export function nextSort(sort: ListSort, field: string): ListSort {
  if (!sort || sort.field !== field) return { field, desc: false };
  if (!sort.desc) return { field, desc: true };
  return null;
}

export type FilterOp = NonNullable<RecordFilter["op"]>;

/** The operations a column offers, with what they read as. */
export function filterOps(kind: ListColumn["kind"]): { op: FilterOp; label: string; takesValue: boolean }[] {
  const empty = [
    { op: "empty" as const, label: "is empty", takesValue: false },
    { op: "not_empty" as const, label: "is not empty", takesValue: false },
  ];
  switch (kind) {
    case "date":
      return [
        ...filterOps("datetime"),
        ...relativeDateOps,
        ...empty,
      ];
    case "number":
    case "datetime":
      return [
        { op: "eq", label: kind === "number" ? "=" : "on", takesValue: true },
        { op: "ne", label: kind === "number" ? "≠" : "not on", takesValue: true },
        { op: "gt", label: kind === "number" ? ">" : "after", takesValue: true },
        { op: "gte", label: kind === "number" ? "≥" : "on or after", takesValue: true },
        { op: "lt", label: kind === "number" ? "<" : "before", takesValue: true },
        { op: "lte", label: kind === "number" ? "≤" : "on or before", takesValue: true },
        ...(kind === "datetime" ? [] : empty),
      ];
    case "boolean":
      return [{ op: "eq", label: "is", takesValue: true }, ...empty];
    case "status":
      return [{ op: "eq", label: "is", takesValue: true }, { op: "ne", label: "is not", takesValue: true }];
    default:
      return [
        { op: "eq", label: "is", takesValue: true },
        { op: "ne", label: "is not", takesValue: true },
        { op: "contains", label: "contains", takesValue: true },
        ...empty,
      ];
  }
}

/** The date filters counted from the viewer's day. */
export const relativeDateOps: { op: FilterOp; label: string; takesValue: boolean }[] = [
  { op: "before_today", label: "before today", takesValue: false },
  { op: "on_or_before_today", label: "today or earlier", takesValue: false },
  { op: "today", label: "is today", takesValue: false },
  { op: "on_or_after_today", label: "today or later", takesValue: false },
  { op: "after_today", label: "after today", takesValue: false },
  { op: "next_days", label: "in the next … days", takesValue: true },
  { op: "last_days", label: "in the last … days", takesValue: true },
];

/** Members of a dimension by code, for its fields' values. */
export function memberLabels(dims: DevDimension[], dimensionId?: string): Map<string, string> {
  const dim = dims.find(d => d.id === dimensionId) ?? dims.find(d => d.name === dimensionId);
  return new Map((dim?.members ?? []).map(m => [m.code, m.label || m.code]));
}

/** How a stored value reads in the list: a member's or metric's name, Yes/No, a grouped number. */
export function displayValue(f: FormField | undefined, value: unknown, dims: DevDimension[], metrics: Metric[]): string {
  if (value === null || value === undefined || value === "") return "—";
  if (!f) return String(value);
  if (f.type === "dimension") return memberLabels(dims, f.dimension_id).get(String(value)) ?? String(value);
  if (f.type === "metric" && !f.metric_id) {
    const m = metrics.find(x => x.name === value || x.label === value);
    return m ? (m.label || m.name) : String(value);
  }
  if (f.type === "boolean") return value === true || value === "true" ? "Yes" : "No";
  if (f.format === "money" && typeof value === "number") return value.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 2 });
  if (typeof value === "number") return value.toLocaleString(undefined, { maximumFractionDigits: 6 });
  return String(value);
}

/** Whether a deadline field of rec is past and its done field does not say it is done. */
export function isOverdue(f: FormField, data: Record<string, unknown>, today = localToday()): boolean {
  if (!f.deadline) return false;
  const v = data[f.name];
  if (typeof v !== "string" || v === "" || v.slice(0, 10) >= today) return false;
  if (f.done_field) {
    const done = data[f.done_field];
    const doneValues = f.done_values && f.done_values.length > 0 ? f.done_values : ["true"];
    if (done !== undefined && done !== null && doneValues.includes(String(done))) return false;
  }
  return true;
}

/** Whether a deadline field of rec falls today (and is not done). */
export function isDueToday(f: FormField, data: Record<string, unknown>, today = localToday()): boolean {
  if (!f.deadline) return false;
  const v = data[f.name];
  if (typeof v !== "string" || v.slice(0, 10) !== today) return false;
  if (f.done_field && (f.done_values ?? ["true"]).includes(String(data[f.done_field]))) return false;
  return true;
}

/** A new record's starting values: each field's default ("today" for a date is the viewer's day). */
export function defaultsOf(form: FormDef): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const f of form.fields) {
    if (f.default === undefined || f.default === null || f.type === "autocode" || f.type === "status") continue;
    out[f.name] = f.type === "date" && f.default === "today" ? localToday() : f.default;
  }
  return out;
}

/** The fields a person fills in, as a form shows them: not the status (set apart) and not hidden ones. */
export function editableFields(form: FormDef): FormField[] {
  return form.fields.filter(f => f.type !== "status" && !f.hidden);
}

/** The field paired with f by required_with, either way round. */
export function pairedField(form: FormDef, f: FormField): FormField | undefined {
  if (f.required_with) return form.fields.find(x => x.name === f.required_with);
  return form.fields.find(x => x.required_with === f.name);
}

/** The table keeping the dimension a field names, when one does. */
export function keeperOf(forms: FormDef[], f: FormField): FormDef | undefined {
  if (f.type !== "dimension" || !f.dimension_id) return undefined;
  return forms.find(t => t.keeps_dimension_id === f.dimension_id);
}

/** A record's title: its table's member label field, else its first text field, else its code. */
export function recordTitle(form: FormDef, rec: Pick<FormRecord, "data" | "id">): string {
  const pick = (name?: string) => (name && rec.data[name] !== undefined && rec.data[name] !== null && rec.data[name] !== "" ? String(rec.data[name]) : "");
  return pick(form.member_label_field)
    || pick(form.fields.find(f => f.type === "text")?.name)
    || pick(form.fields.find(f => f.type === "autocode")?.name)
    || "Record";
}

/** How a status reads: a record without one reads as no status. */
export function statusLabel(status: string): string {
  return status ? status : "no status";
}

/** A filter as a chip reads it: "Region is Europe". */
export function describeFilter(filter: RecordFilter, cols: ListColumn[], dims: DevDimension[], metrics: Metric[]): string {
  const col = cols.find(c => c.key === filter.field);
  const label = col?.label ?? filter.field;
  const op = filterOps(col?.kind ?? "text").find(o => o.op === (filter.op ?? "eq"));
  if (filter.op === "next_days" || filter.op === "last_days") {
    return `${label} in the ${filter.op === "next_days" ? "next" : "last"} ${String(filter.value ?? 0)} days`;
  }
  if (filter.op === "in" || filter.op === "not_in") {
    const values = filter.values ?? (Array.isArray(filter.value) ? filter.value : [filter.value]);
    const shown = values.map(v => (col?.kind === "status" ? statusLabel(String(v ?? "")) : displayValue(col?.field, v, dims, metrics))).join(", ");
    return `${label} ${filter.op === "in" ? "is one of" : "is not one of"} ${shown}`;
  }
  if (!op?.takesValue) return `${label} ${op?.label ?? filter.op}`;
  const shown = col?.kind === "status" ? statusLabel(String(filter.value ?? "")) : displayValue(col?.field, filter.value, dims, metrics);
  return `${label} ${op.label} ${shown}`;
}

/** The records query for what the widget shows. */
export function listQuery(opts: { search: string; filters: RecordFilter[]; sort: ListSort; limit: number; fixedFilters?: RecordFilter[] }): RecordsQuery {
  const q: RecordsQuery = { limit: opts.limit };
  const search = opts.search.trim();
  if (search) q.search = search;
  const filters = [...(opts.fixedFilters ?? []), ...opts.filters];
  if (filters.length > 0) q.filters = filters;
  if (opts.sort) q.sort = [{ field: opts.sort.field, desc: opts.sort.desc }];
  // Relative dates count from the viewer's own day.
  if (filters.some(f => f.op && relativeDateOps.some(r => r.op === f.op))) q.today = localToday();
  return q;
}

/**
 * Whether the field's member is posted into a metric, in any status a
 * record posts in. A record posts within its creator's access, so where it
 * is, a member the creator may only read cannot be chosen for their record.
 */
export function postsMember(form: FormDef, field: FormField): boolean {
  return (form.posting_fields?.[field.name] ?? []).length > 0;
}

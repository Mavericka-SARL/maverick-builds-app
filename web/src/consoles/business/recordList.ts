// What the records widget shows of a table and how its list is asked for:
// columns, the sort a header click moves to, the filters a person can add,
// and how a stored value reads on screen. The list itself is the server's
// records query (POST /api/forms/{id}/records/query), so search, filters,
// sort and pages cover every record the person may see, not one page.
import type { DevDimension, FormDef, FormField, Metric, RecordFilter, RecordsQuery } from "../../api/client";
import { statusFieldOf } from "../../api/client";

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
export function listColumns(form: FormDef): ListColumn[] {
  const cols: ListColumn[] = form.fields
    .filter(f => f.type !== "status")
    .map(f => ({ key: f.name, label: f.label || f.name, kind: fieldKind(f), field: f }));
  const status = statusFieldOf(form);
  if (status) cols.push({ key: status.name, label: status.label || "Status", kind: "status", field: status });
  cols.push({ key: "created_at", label: "Created", kind: "datetime" });
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
    case "number":
    case "date":
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
  if (typeof value === "number") return value.toLocaleString(undefined, { maximumFractionDigits: 6 });
  return String(value);
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
  if (!op?.takesValue) return `${label} ${op?.label ?? filter.op}`;
  const shown = col?.kind === "status" ? statusLabel(String(filter.value ?? "")) : displayValue(col?.field, filter.value, dims, metrics);
  return `${label} ${op.label} ${shown}`;
}

/** The records query for what the widget shows. */
export function listQuery(opts: { search: string; filters: RecordFilter[]; sort: ListSort; limit: number }): RecordsQuery {
  const q: RecordsQuery = { limit: opts.limit };
  const search = opts.search.trim();
  if (search) q.search = search;
  if (opts.filters.length > 0) q.filters = opts.filters;
  if (opts.sort) q.sort = [{ field: opts.sort.field, desc: opts.sort.desc }];
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

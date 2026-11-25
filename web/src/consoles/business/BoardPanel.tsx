import { useDateFormat } from "../dateFormat";
import { shownDecimals } from "../shownDecimals";
import { Fragment, useEffect, useMemo, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient, keepPreviousData } from "@tanstack/react-query";
import { Filter, Link2, Lock, Plus as PlusIcon } from "lucide-react";
import {
  api, type BoardColumn, type BoardSettings, type DevDimension, type FormDef, type FormRecord, type Metric, type RecordFilter, type RecordsQuery,
} from "../../api/client";
import { Button, FilterBar, FilterBarGroup, FilterChip, InlineAlert, LoadingState, SearchInput, Select, Txt } from "../../ui";
import { invalidateModelData } from "../modelDataQueries";
import { useRecordChoices } from "./useRecordChoices";
import { canCreateRecords } from "./formPermissions";
import { describeFilter, displayValue, listColumns, listQuery, newRecordLabel, recordTitle } from "./recordList";
import { FilterBuilder } from "./FormPanel";
import { RecordDialog, RecordFieldValue } from "./RecordLinks";

/** How many cards a board reads at most; the column totals always count every record. */
const BOARD_LIMIT = 1000;

/** The column a record with no value in the column field stands in. */
const NONE = "";

/** How near the board's left or right edge (px) a dragged card scrolls it, and how fast (px a frame) at the edge itself. */
const EDGE_ZONE = 80;
const EDGE_SPEED = 18;

/**
 * A table's records as cards in columns — the members of a dimension field
 * (any dimension: a sales pipeline by stage, tasks by status, requests by
 * team) or the choices of a select field, in their order. Dragging a card to
 * another column, or choosing one in its "Move to", sets that field; the
 * server decides as for any save, so a card the viewer may not change does
 * not move, and no card moves into a column read-only for them. Each column
 * counts its records and totals the sum field over all of them, not only the
 * cards shown. A card opens the record.
 */
export function BoardPanel({ form, revisionId, settings, fixedFilters, newRecordButton = false, title, prefill, followFilters }: {
  form: FormDef;
  revisionId: string;
  settings: BoardSettings;
  fixedFilters?: RecordFilter[];
  newRecordButton?: boolean;
  /** Values a new card starts with: the dashboard's selection it follows. */
  prefill?: Record<string, unknown>;
  /** The dashboard's selection the board follows, applied like fixed filters and shown apart from them. */
  followFilters?: RecordFilter[];
  title?: string;
}) {
  const dateFormat = useDateFormat();
  const qc = useQueryClient();
  const colField = form.fields.find(f => f.name === settings.column_field);
  const sumField = settings.sum_field ? form.fields.find(f => f.name === settings.sum_field) : undefined;
  const titleField = settings.title_field ? form.fields.find(f => f.name === settings.title_field) : undefined;
  const cardFields = (settings.card_fields ?? []).map(n => form.fields.find(f => f.name === n)).filter((f): f is NonNullable<typeof f> => !!f && !f.hidden);
  const listCols = useMemo(() => listColumns(form), [form]);

  const [searchText, setSearchText] = useState("");
  const [search, setSearch] = useState("");
  useEffect(() => {
    const t = setTimeout(() => setSearch(searchText), 250);
    return () => clearTimeout(t);
  }, [searchText]);
  const [filters, setFilters] = useState<RecordFilter[]>([]);
  const [buildingFilter, setBuildingFilter] = useState(false);
  const [opened, setOpened] = useState<FormRecord | null>(null);
  const [creatingIn, setCreatingIn] = useState<string | null>(null);
  const [dragOver, setDragOver] = useState<string | null>(null);
  const [moveError, setMoveError] = useState<string | null>(null);

  const { data: dimsRaw = [] } = useQuery({ queryKey: ["dimensions"], queryFn: api.getDimensions });
  const dims = dimsRaw as DevDimension[];
  const { data: metricsRaw = [] } = useQuery({ queryKey: ["metrics", revisionId], queryFn: () => api.getMetrics(revisionId), enabled: !!revisionId });
  const metrics = metricsRaw as Metric[];
  const { data: formsRaw = [] } = useQuery({ queryKey: ["forms", revisionId], queryFn: () => api.listForms(revisionId || undefined) });
  const forms = formsRaw as FormDef[];
  useRecordChoices(form, forms); // a person field reads by name

  const { data: colData, error: colError } = useQuery({
    queryKey: ["records", "board-columns", form.id, settings.column_field, settings.color_property ?? ""],
    queryFn: () => api.boardColumns(form.id, settings.column_field, settings.color_property),
    enabled: !!colField,
  });
  const query: RecordsQuery = listQuery({ search, filters, sort: null, limit: BOARD_LIMIT, fixedFilters: followFilters?.length ? [...(fixedFilters ?? []), ...followFilters] : fixedFilters });
  const { data: page, isLoading, error: listError } = useQuery({
    queryKey: ["records", form.id, "board", query],
    queryFn: () => api.queryRecords(form.id, query),
    placeholderData: keepPreviousData,
  });
  const totalsQuery: RecordsQuery = {
    filters: query.filters, search: query.search, today: query.today,
    group_by: [{ field: settings.column_field }],
    aggregates: [{ op: "count" }, ...(sumField ? [{ op: "sum" as const, field: sumField.name }] : [])],
    limit: 5000,
  };
  const { data: totals } = useQuery({
    queryKey: ["records", form.id, "board-totals", totalsQuery],
    queryFn: () => api.queryRecords(form.id, totalsQuery),
    placeholderData: keepPreviousData,
  });

  const columns: BoardColumn[] = useMemo(() => colData?.columns ?? [], [colData]);
  const records = useMemo(() => page?.records ?? [], [page]);
  const byColumn = useMemo(() => {
    const m = new Map<string, FormRecord[]>();
    for (const r of records) {
      const v = r.data[settings.column_field];
      const key = v === undefined || v === null ? NONE : String(v);
      m.set(key, [...(m.get(key) ?? []), r]);
    }
    return m;
  }, [records, settings.column_field]);
  const totalsBy = useMemo(() => {
    const m = new Map<string, { count: number; sum?: number }>();
    for (const row of totals?.rows ?? []) {
      const v = row[settings.column_field];
      m.set(v === null || v === undefined ? NONE : String(v), {
        count: Number(row.count ?? 0),
        sum: sumField ? Number(row[`sum_${sumField.name}`] ?? 0) : undefined,
      });
    }
    return m;
  }, [totals, settings.column_field, sumField]);
  const shownColumns: BoardColumn[] = [
    ...columns,
    ...((byColumn.get(NONE)?.length ?? 0) > 0 ? [{ key: NONE, label: `No ${(colField?.label ?? "value").toLowerCase()}` }] : []),
  ].filter(c => !settings.hide_empty || (totalsBy.get(c.key)?.count ?? 0) > 0);

  const move = useMutation({
    mutationFn: ({ rec, to }: { rec: FormRecord; to: string }) => api.updateRecord(rec.id, { data: { ...rec.data, [settings.column_field]: to } }, rec.updated_at),
    onMutate: ({ rec, to }) => {
      setMoveError(null);
      // The card moves at once; a refusal puts it back.
      qc.setQueryData(["records", form.id, "board", query], (old: typeof page) =>
        old ? { ...old, records: (old.records ?? []).map(r => (r.id === rec.id ? { ...r, data: { ...r.data, [settings.column_field]: to } } : r)) } : old);
    },
    onError: (err: Error, { rec }) => setMoveError(`${recordTitle(form, rec)} was not moved — ${err.message}`),
    onSettled: () => {
      qc.invalidateQueries({ queryKey: ["records", form.id] });
      invalidateModelData(qc);
    },
  });

  // A field with transitions moves a card only where the server allows it
  // (permissions.moves); a record locked by another field does not move.
  const [dragging, setDragging] = useState<FormRecord | null>(null);
  const movesOf = (rec: FormRecord) => rec.permissions?.moves?.[settings.column_field];
  const cardMovable = (rec: FormRecord) => {
    const lockedBy = rec.permissions?.locked_by;
    const moves = movesOf(rec);
    return !!rec.permissions?.edit && (!lockedBy || lockedBy === settings.column_field) && (moves === undefined || moves.length > 0);
  };
  const canMoveInto = (col: BoardColumn, rec: FormRecord | null = dragging) => {
    if (col.key === NONE || col.read_only) return false;
    const moves = rec ? movesOf(rec) : undefined;
    return moves === undefined || moves.includes(col.key) || String(rec?.data[settings.column_field] ?? "") === col.key;
  };
  // A card held near the board's left or right edge scrolls the board that
  // way, so a column scrolled out of sight can still take the card; the
  // browser's own drag scrolling misses a box that scrolls sideways.
  const boardRef = useRef<HTMLDivElement>(null);
  const draggingId = dragging?.id;
  useEffect(() => {
    if (!draggingId) return;
    let speed = 0;
    let frame: number | null = null;
    const step = () => {
      const el = boardRef.current;
      if (!el || speed === 0) { frame = null; return; }
      el.scrollLeft += speed;
      frame = requestAnimationFrame(step);
    };
    const onDragOver = (e: DragEvent) => {
      const el = boardRef.current;
      if (!el) return;
      // The edges the viewer sees: a dashboard narrower than the board's
      // widget scrolls sideways itself and hides part of the board.
      const r = el.getBoundingClientRect();
      let left = Math.max(r.left, 0), right = Math.min(r.right, window.innerWidth);
      for (let a = el.parentElement; a; a = a.parentElement) {
        if (getComputedStyle(a).overflowX === "visible") continue;
        const ar = a.getBoundingClientRect();
        left = Math.max(left, ar.left);
        right = Math.min(right, ar.right);
      }
      if (right - left < 40) { speed = 0; return; }
      const zone = Math.min(EDGE_ZONE, (right - left) / 4);
      const pace = (fromEdge: number) => Math.ceil(EDGE_SPEED * Math.min(1, Math.max(0, 1 - fromEdge / zone)));
      speed = e.clientY < r.top || e.clientY > r.bottom ? 0
        : e.clientX < left + zone ? -pace(e.clientX - left)
        : e.clientX > right - zone ? pace(right - e.clientX)
        : 0;
      if (speed !== 0 && frame === null) frame = requestAnimationFrame(step);
    };
    document.addEventListener("dragover", onDragOver);
    return () => {
      document.removeEventListener("dragover", onDragOver);
      if (frame !== null) cancelAnimationFrame(frame);
    };
  }, [draggingId]);
  const moveTo = (rec: FormRecord, to: string) => {
    if (String(rec.data[settings.column_field] ?? "") === to) return;
    move.mutate({ rec, to });
  };
  const money = sumField?.format === "money";
  // A column's total reads at a glance — whole (1,050), large ones short
  // (15.8M); its title has the figure in full.
  const fmtSum = (n: number) => n.toLocaleString(undefined, Math.abs(n) >= 100_000 ? { notation: "compact", maximumFractionDigits: 1 } : { maximumFractionDigits: 0 });
  const fullSum = (n: number) => {
    const d = shownDecimals(n, 2); // a large amount is written whole
    return n.toLocaleString(undefined, money ? { minimumFractionDigits: d, maximumFractionDigits: d } : { maximumFractionDigits: d });
  };
  const canCreate = newRecordButton && canCreateRecords(form);

  if (!colField) {
    return <InlineAlert tone="danger">This board's column field “{settings.column_field}” is not a field of {form.label}.</InlineAlert>;
  }
  return (
    <div className="mvx-board-wrap">
      <div className="mvx-panel-head">
        <div className="mvx-panel-head__titles">
          <span className="mvx-panel-head__title">{title || form.label}</span>
          {(fixedFilters ?? []).length > 0 && (
            <span className="mvx-panel-head__note" title="Set on this widget">
              <Lock size={11} aria-hidden="true" />
              {(fixedFilters ?? []).map((f, i) => (
                <Fragment key={i}>
                  <Txt>{i > 0 && " · "}</Txt>
                  <span aria-label={`Filter set on this widget: ${describeFilter(f, listCols, dims, metrics)}`}>{describeFilter(f, listCols, dims, metrics)}</span>
                </Fragment>
              ))}
            </span>
          )}
          {(followFilters ?? []).length > 0 && (
            <span className="mvx-panel-head__note" title="Follows the record selected on the dashboard">
              <Link2 size={11} aria-hidden="true" />
              {(followFilters ?? []).map((f, i) => (
                <Fragment key={i}>
                  <Txt>{i > 0 && " · "}</Txt>
                  <span aria-label={`Follows the dashboard's selection: ${describeFilter(f, listCols, dims, metrics)}`}>{describeFilter(f, listCols, dims, metrics)}</span>
                </Fragment>
              ))}
            </span>
          )}
        </div>
        {canCreate && (
          <div className="mvx-panel-head__actions">
            <Button size="sm" variant="primary" leadingIcon={<PlusIcon size={13} />} onClick={() => setCreatingIn(columns[0]?.key ?? NONE)}>
              {newRecordLabel(form)}
            </Button>
          </div>
        )}
      </div>
      <FilterBar className="mvx-filter-bar--inset">
        <FilterBarGroup>
          <SearchInput value={searchText} onChange={e => setSearchText(e.target.value)} placeholder="Search cards…" width={200} aria-label={`Search ${form.label}`} />
          <Button size="sm" variant="ghost" leadingIcon={<Filter size={13} />} onClick={() => setBuildingFilter(v => !v)} aria-expanded={buildingFilter}>Filter</Button>
          {filters.map((f, i) => (
            <FilterChip key={i} active aria-label={`Remove filter: ${describeFilter(f, listCols, dims, metrics)}`}
              onClick={() => setFilters(fs => fs.filter((_, j) => j !== i))} onClear={() => setFilters(fs => fs.filter((_, j) => j !== i))}>
              {describeFilter(f, listCols, dims, metrics)}
            </FilterChip>
          ))}
        </FilterBarGroup>
        <FilterBarGroup>
          <span className="mvx-admin-muted" style={{ fontSize: 12 }} aria-live="polite">
            {page ? `${page.total ?? 0} card${page.total === 1 ? "" : "s"}${(page.total ?? 0) > records.length ? ` · showing ${records.length}` : ""}` : ""}
          </span>
        </FilterBarGroup>
      </FilterBar>
      {buildingFilter && (
        <FilterBuilder cols={listCols} dims={dims} metrics={metrics} statuses={page?.statuses ?? []}
          onAdd={f => { setFilters(fs => [...fs, f]); setBuildingFilter(false); }} onCancel={() => setBuildingFilter(false)} />
      )}
      {(listError || colError) && <InlineAlert tone="danger">The board could not be read — {((listError ?? colError) as Error).message}</InlineAlert>}
      {moveError && <InlineAlert tone="danger">{moveError}</InlineAlert>}
      {isLoading ? <LoadingState /> : (
        <div ref={boardRef} className="mvx-board" role="list" aria-label={`${title || form.label} by ${colField.label || colField.name}`}>
          {shownColumns.map(col => {
            const cards = byColumn.get(col.key) ?? [];
            const t = totalsBy.get(col.key);
            return (
              <section key={col.key || "none"} role="listitem" aria-label={col.label}
                className={["mvx-board__column", dragOver === col.key ? "mvx-board__column--over" : "", col.read_only ? "mvx-board__column--readonly" : ""].filter(Boolean).join(" ")}
                style={col.color ? { borderTopColor: col.color } : undefined}
                onDragOver={e => { if (canMoveInto(col)) { e.preventDefault(); setDragOver(col.key); } }}
                onDragLeave={() => setDragOver(d => (d === col.key ? null : d))}
                onDrop={e => {
                  e.preventDefault();
                  setDragOver(null);
                  const id = e.dataTransfer.getData("text/plain");
                  const rec = records.find(r => r.id === id);
                  setDragging(null);
                  if (rec && canMoveInto(col, rec)) moveTo(rec, col.key);
                }}>
                <header className="mvx-board__column-head">
                  <span className="mvx-board__column-title">
                    {col.read_only && <Lock size={11} aria-label="Read-only for you" style={{ marginRight: 4 }} />}
                    <Txt>{col.label}</Txt>
                  </span>
                  <span className="mvx-board__column-count" aria-label={`${t?.count ?? 0} records`}>{t?.count ?? 0}</span>
                  {sumField && <span className="mvx-board__column-sum" title={`${sumField.label} total: ${fullSum(t?.sum ?? 0)}`}>{fmtSum(t?.sum ?? 0)}</span>}
                  {canCreate && canMoveInto(col, null) && (
                    <button type="button" className="mvx-board__add" aria-label={`New card in ${col.label}`} onClick={() => setCreatingIn(col.key)}>
                      <PlusIcon size={13} aria-hidden="true" />
                    </button>
                  )}
                </header>
                <div className="mvx-board__cards">
                  {cards.map(rec => {
                    const movable = cardMovable(rec);
                    const cardTitle = titleField ? displayValue(titleField, rec.data[titleField.name], dims, metrics, dateFormat) : recordTitle(form, rec);
                    return (
                      <article key={rec.id} className="mvx-board__card" draggable={movable}
                        aria-label={cardTitle}
                        onDragStart={e => { e.dataTransfer.setData("text/plain", rec.id); e.dataTransfer.effectAllowed = "move"; setDragging(rec); }}
                        onDragEnd={() => setDragging(null)}
                        onClick={() => setOpened(rec)}
                        onKeyDown={e => { if (e.key === "Enter" && e.target === e.currentTarget) setOpened(rec); }}
                        tabIndex={0}>
                        <div className="mvx-board__card-title">{cardTitle}</div>
                        {cardFields.map(f => (
                          <div key={f.name} className="mvx-board__card-field">
                            <span className="mvx-admin-muted">{f.label || f.name}: </span>
                            <RecordFieldValue form={form} f={f} data={rec.data} dims={dims} metrics={metrics} />
                          </div>
                        ))}
                        {movable && (
                          <Select aria-label={`Move ${cardTitle} to`} className="mvx-board__move" value={String(rec.data[settings.column_field] ?? "")}
                            onClick={e => e.stopPropagation()} onKeyDown={e => e.stopPropagation()}
                            onChange={e => moveTo(rec, e.target.value)}>
                            {!columns.some(c => c.key === String(rec.data[settings.column_field] ?? "")) && (
                              <option value={String(rec.data[settings.column_field] ?? "")} disabled>{col.label}</option>
                            )}
                            {columns.map(c => <option key={c.key} value={c.key} disabled={!canMoveInto(c, rec)}>{c.label}</option>)}
                          </Select>
                        )}
                      </article>
                    );
                  })}
                  {cards.length === 0 && <div className="mvx-board__empty">No cards</div>}
                </div>
              </section>
            );
          })}
          {shownColumns.length === 0 && <div className="mvx-admin-muted" style={{ padding: 16 }}>No columns: {colField.label} has no members you can see.</div>}
        </div>
      )}
      {opened && (
        <RecordDialog form={form} forms={forms} record={records.find(r => r.id === opened.id) ?? opened} revisionId={revisionId} onClose={() => setOpened(null)} />
      )}
      {creatingIn !== null && (
        <RecordDialog form={form} forms={forms} record={null} revisionId={revisionId}
          initialData={creatingIn || prefill ? { ...prefill, ...(creatingIn ? { [settings.column_field]: creatingIn } : {}) } : undefined} onClose={() => setCreatingIn(null)} />
      )}
    </div>
  );
}

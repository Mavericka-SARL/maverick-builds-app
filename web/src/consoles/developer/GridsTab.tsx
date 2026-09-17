import { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { Plus, Trash2 } from "lucide-react";
import { api, type GridDef, type DevDimension } from "../../api/client";
import { Toolbar, ToolbarGroup, Button, Field, TextInput, SearchInput, EmptyState, IconButton, Checkbox, StatusBadge, Select, useConfirm } from "../../ui";

/**
 * A grid's picker lists every metric and dimension in the model. At a hundred
 * metrics that is a wall of checkboxes in which the handful actually in the
 * grid are invisible, so each list is searched and split: what is already in
 * the grid first, everything else below.
 */
function PickerSection({ label, count, children }: { label: string; count: number; children: React.ReactNode }) {
  return (
    <div style={{ marginBottom: 12 }}>
      <div className="mvx-admin-muted" style={{ fontSize: 11, fontWeight: 700, letterSpacing: "0.04em", marginBottom: 6 }}>
        {label} ({count})
      </div>
      {count === 0
        ? <p className="mvx-admin-muted" style={{ margin: 0 }}>None</p>
        : children}
    </div>
  );
}

export function GridsTab({ revisionId }: { revisionId?: string }) {
  const qc = useQueryClient();
  const [showCreate, setShowCreate] = useState(false);
  const [newName, setNewName] = useState("");
  const [expandedGrid, setExpandedGrid] = useState<string | null>(null);
  // One pair of filters, not one per grid: only a single grid is open at a
  // time, and carrying a stale search into the next one you open would hide
  // most of its contents for no reason.
  const [metricFilter, setMetricFilter] = useState("");
  const [dimFilter, setDimFilter] = useState("");
  const openGrid = (id: string | null) => { setExpandedGrid(id); setMetricFilter(""); setDimFilter(""); };

  const { data: model } = useQuery({ queryKey: ["dev-model", revisionId], queryFn: () => api.getDevModel(revisionId) });
  const { data: dims = [] } = useQuery({ queryKey: ["dev-dimensions", revisionId], queryFn: () => api.getDevDimensions(revisionId) });
  const { data: grids = [] } = useQuery({ queryKey: ["dev-grids", revisionId], queryFn: () => api.listGrids(revisionId) });

  const inv = () => qc.invalidateQueries({ queryKey: ["dev-grids", revisionId] });

  const createGrid = useMutation({
    mutationFn: () => api.createGrid({ name: newName, revision_id: revisionId }),
    onSuccess: () => { inv(); setNewName(""); setShowCreate(false); },
  });
  const deleteGrid = useMutation({
    mutationFn: (id: string) => api.deleteGrid(id),
    onSuccess: inv,
  });
  const addMetric = useMutation({
    mutationFn: ({ gridId, metricId }: { gridId: string; metricId: string }) => api.addGridMetric(gridId, metricId),
    onSuccess: inv,
  });
  const removeMetric = useMutation({
    mutationFn: ({ gridId, metricId }: { gridId: string; metricId: string }) => api.removeGridMetric(gridId, metricId),
    onSuccess: inv,
  });
  const addDim = useMutation({
    mutationFn: ({ gridId, dimId }: { gridId: string; dimId: string }) => api.addGridDimension(gridId, dimId),
    onSuccess: inv,
  });
  const removeDim = useMutation({
    mutationFn: ({ gridId, dimId }: { gridId: string; dimId: string }) => api.removeGridDimension(gridId, dimId),
    onSuccess: inv,
  });
  const setDimLevel = useMutation({
    mutationFn: ({ gridId, dimId, level }: { gridId: string; dimId: string; level: number | null }) =>
      api.setGridDimLevel(gridId, dimId, level),
    onSuccess: inv,
  });
  const { confirm, confirmElement } = useConfirm();

  return (
    <div>
      <Toolbar className="mvx-toolbar--spaced">
        <ToolbarGroup>
          <span className="mvx-admin-muted">
            Grids combine metrics and dimensions. Business users see grids inside dashboards.
          </span>
        </ToolbarGroup>
        <ToolbarGroup align="end">
          <Button leadingIcon={showCreate ? undefined : <Plus size={14} />} onClick={() => setShowCreate((v) => !v)}>
            {showCreate ? "Cancel" : "New grid"}
          </Button>
        </ToolbarGroup>
      </Toolbar>

      {showCreate && (
        <div className="mvx-admin-inline-form" style={{ marginBottom: 20, alignItems: "flex-end" }}>
          <Field label="Grid name">
            <TextInput value={newName} onChange={(e) => setNewName(e.target.value)} placeholder="OPEX Budget" style={{ width: 280 }} />
          </Field>
          <Button variant="primary" disabled={!newName} loading={createGrid.isPending} onClick={() => createGrid.mutate()}>
            Create
          </Button>
        </div>
      )}

      {(grids as GridDef[]).length === 0 ? (
        <EmptyState label="No grids yet. Create one above." />
      ) : (
        <div className="mvx-admin-stack">
          {(grids as GridDef[]).map((g) => (
            <div key={g.id} className="mvx-admin-object">
              <div className="mvx-admin-object__header" style={expandedGrid === g.id ? undefined : { borderBottom: "none" }}>
                <div className="mvx-admin-object__title">
                  <span className="mvx-admin-object__name">{g.name}</span>
                  <div className="mvx-admin-object__meta">{g.metric_ids.length} metrics · {g.dimension_ids.length} dimensions</div>
                </div>
                {/* "Done", not "Close": every change here is written as it is
                    made, so there is nothing pending and nothing to discard —
                    "Close" next to a delete button reads as though there might
                    be. */}
                <Button size="sm" onClick={() => openGrid(expandedGrid === g.id ? null : g.id)}>
                  {expandedGrid === g.id ? "Done" : "Configure"}
                </Button>
                <IconButton
                  aria-label={`Delete grid ${g.name}`}
                  title="Delete grid"
                  danger
                  onClick={() => confirm({ title: "Delete grid?", body: `This removes "${g.name}" from all dashboards that use it.`, confirmLabel: "Delete grid", onConfirm: () => deleteGrid.mutate(g.id) })}
                >
                  <Trash2 size={14} />
                </IconButton>
              </div>
              {expandedGrid === g.id && (
                <div className="mvx-admin-object__body" style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 24 }}>
                  <div>
                    <div className="mvx-admin-revisions__label" style={{ marginBottom: 10 }}>Metrics</div>
                    <SearchInput
                      aria-label="Search metrics"
                      placeholder="Search metrics…"
                      value={metricFilter}
                      onChange={e => setMetricFilter(e.target.value)}
                      style={{ marginBottom: 12 }}
                    />
                    {!model ? <p className="mvx-admin-muted">Loading…</p> : (() => {
                      // Search matches the display label and the snake_case
                      // name: a developer looking for a metric knows one or the
                      // other, rarely both.
                      const q = metricFilter.trim().toLowerCase();
                      const matches = model.metrics.filter(m =>
                        !q || (m.label || "").toLowerCase().includes(q) || m.name.toLowerCase().includes(q));
                      const renderMetric = (m: typeof matches[number]) => {
                        const included = g.metric_ids.includes(m.id);
                        return (
                          <Checkbox
                            key={m.id}
                            checked={included}
                            onChange={() => included ? removeMetric.mutate({ gridId: g.id, metricId: m.id }) : addMetric.mutate({ gridId: g.id, metricId: m.id })}
                            label={
                              <>
                                {m.label || m.name}{" "}
                                <StatusBadge tone={m.is_input ? "info" : "success"}>{m.is_input ? "Input" : "Calc"}</StatusBadge>
                              </>
                            }
                          />
                        );
                      };
                      const inGrid = matches.filter(m => g.metric_ids.includes(m.id));
                      const available = matches.filter(m => !g.metric_ids.includes(m.id));
                      return (
                        <>
                          <PickerSection label="In this grid" count={inGrid.length}>
                            {inGrid.map(renderMetric)}
                          </PickerSection>
                          <PickerSection label="Available" count={available.length}>
                            {available.map(renderMetric)}
                          </PickerSection>
                        </>
                      );
                    })()}
                  </div>
                  <div>
                    <div className="mvx-admin-revisions__label" style={{ marginBottom: 10 }}>Dimensions</div>
                    <SearchInput
                      aria-label="Search dimensions"
                      placeholder="Search dimensions…"
                      value={dimFilter}
                      onChange={e => setDimFilter(e.target.value)}
                      style={{ marginBottom: 12 }}
                    />
                    {(dims as DevDimension[]).length === 0 ? (
                      <p className="mvx-admin-muted">No dimensions defined yet.</p>
                    ) : (() => {
                    const q = dimFilter.trim().toLowerCase();
                    const matches = (dims as DevDimension[]).filter(d => !q || d.name.toLowerCase().includes(q));
                    const renderDim = (d: DevDimension) => {
                      const included = g.dimension_ids.includes(d.id);
                      const hasHierarchy = d.members.some(m => m.parent_member_id);
                      const currentLevel = g.dimension_levels?.[d.id] ?? null;
                      // Compute max depth for dynamic level options
                      const maxDepth = d.members.reduce((mx, m) => {
                        let depth = 0, cur = m;
                        while (cur.parent_member_id) {
                          const parent = d.members.find(x => x.id === cur.parent_member_id);
                          if (!parent) break;
                          depth++; cur = parent;
                        }
                        return Math.max(mx, depth);
                      }, 0);
                      return (
                        <div key={d.id} style={{ marginBottom: 10 }}>
                          <Checkbox
                            checked={included}
                            onChange={() => included ? removeDim.mutate({ gridId: g.id, dimId: d.id }) : addDim.mutate({ gridId: g.id, dimId: d.id })}
                            label={<>{d.name} <span className="mvx-admin-muted">{d.members.length} members</span></>}
                          />
                          {included && hasHierarchy && (
                            <div style={{ marginLeft: 24, marginTop: 4, display: "flex", alignItems: "center", gap: 6 }}>
                              <span className="mvx-admin-muted">Show:</span>
                              <Select
                                aria-label={`Hierarchy level for ${d.name}`}
                                value={currentLevel === null ? "" : String(currentLevel)}
                                onChange={e => setDimLevel.mutate({
                                  gridId: g.id, dimId: d.id,
                                  level: e.target.value === "" ? null : parseInt(e.target.value),
                                })}
                              >
                                <option value="">All levels (hierarchy)</option>
                                <option value="0">Root only (level 0)</option>
                                {Array.from({ length: maxDepth }, (_, i) => i + 1).map(lv => (
                                  <option key={lv} value={String(lv)}>Level {lv} only</option>
                                ))}
                                <option value="-1">Leaves only</option>
                              </Select>
                            </div>
                          )}
                        </div>
                      );
                    };
                    const inGrid = matches.filter(d => g.dimension_ids.includes(d.id));
                    const available = matches.filter(d => !g.dimension_ids.includes(d.id));
                    return (
                      <>
                        <PickerSection label="In this grid" count={inGrid.length}>
                          {inGrid.map(renderDim)}
                        </PickerSection>
                        <PickerSection label="Available" count={available.length}>
                          {available.map(renderDim)}
                        </PickerSection>
                      </>
                    );
                    })()}
                  </div>
                </div>
              )}
            </div>
          ))}
        </div>
      )}
      {confirmElement}
    </div>
  );
}

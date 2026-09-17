import React, { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { Plus, X, Pencil, Trash2, ChevronDown, ChevronRight, ChevronsDown, ChevronsUp } from "lucide-react";
import { api, type DevDimensionMember, type DevDimension, type DimProperty } from "../../api/client";
import { Button, Field, TextInput, Select, EmptyState, IconButton, StatusBadge, SearchInput, useConfirm } from "../../ui";

// Tree node type — flat DevDimensionMember enriched with hierarchy metadata.
interface DimMemberNode extends DevDimensionMember {
  children: DimMemberNode[];
  level: number;
  childCount: number;
  descendantCount: number;
}

function buildDimensionTree(members: DevDimensionMember[]): DimMemberNode[] {
  const byId = new Map<string, DimMemberNode>();
  for (const m of members) {
    byId.set(m.id, { ...m, children: [], level: 0, childCount: 0, descendantCount: 0 });
  }
  const roots: DimMemberNode[] = [];
  for (const node of byId.values()) {
    const parentNode = node.parent_member_id ? byId.get(node.parent_member_id) : undefined;
    if (parentNode) parentNode.children.push(node);
    else roots.push(node);
  }
  function assign(nodes: DimMemberNode[], level: number) {
    for (const n of nodes) {
      n.level = level;
      n.childCount = n.children.length;
      assign(n.children, level + 1);
      n.descendantCount = n.children.reduce((s, c) => s + 1 + c.descendantCount, 0);
    }
  }
  assign(roots, 0);
  function sortSiblings(nodes: DimMemberNode[]) {
    nodes.sort((a, b) => a.code.localeCompare(b.code));
    for (const n of nodes) sortSiblings(n.children);
  }
  sortSiblings(roots);
  return roots;
}

function flattenVisible(roots: DimMemberNode[], expanded: Set<string>, search: string): DimMemberNode[] {
  if (search) {
    const q = search.toLowerCase();
    function hasMatch(n: DimMemberNode): boolean {
      return n.code.toLowerCase().includes(q) || n.label.toLowerCase().includes(q) || n.children.some(hasMatch);
    }
    function collect(n: DimMemberNode): DimMemberNode[] {
      if (!hasMatch(n)) return [];
      return [n, ...n.children.flatMap(collect)];
    }
    return roots.flatMap(collect);
  }
  const result: DimMemberNode[] = [];
  function walk(n: DimMemberNode) {
    result.push(n);
    if (expanded.has(n.id)) for (const c of n.children) walk(c);
  }
  for (const r of roots) walk(r);
  return result;
}

function dimMaxDepth(roots: DimMemberNode[]): number {
  let max = 0;
  function walk(n: DimMemberNode) { if (n.level > max) max = n.level; for (const c of n.children) walk(c); }
  for (const r of roots) walk(r);
  return max;
}

export function DimensionsView({ dims, revisionId }: { dims: DevDimension[]; revisionId?: string }) {
  const qc = useQueryClient();
  const [showAdd, setShowAdd] = useState(false);
  const [newName, setNewName] = useState("");
  const [newParentDim, setNewParentDim] = useState("");

  const create = useMutation({
    mutationFn: () => api.createDimension({ name: newName, revision_id: revisionId, parent_dimension_id: newParentDim || undefined }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["dev-dimensions"] }); setNewName(""); setNewParentDim(""); setShowAdd(false); },
  });

  return (
    <div>
      <div style={{ marginBottom: 20 }}>
        <Button
          leadingIcon={showAdd ? undefined : <Plus size={14} />}
          onClick={() => setShowAdd((v) => !v)}
          style={{ marginBottom: showAdd ? 12 : 0 }}
        >
          {showAdd ? "Cancel" : "New dimension"}
        </Button>
        {showAdd && (
          <div className="mvx-admin-inline-form" style={{ alignItems: "flex-end" }}>
            <Field label="Name">
              <TextInput value={newName} onChange={(e) => setNewName(e.target.value)} placeholder="department"
                style={{ width: 180 }} onKeyDown={(e) => e.key === "Enter" && newName && create.mutate()} />
            </Field>
            <Field label="Parent dimension (optional)">
              <Select value={newParentDim} onChange={(e) => setNewParentDim(e.target.value)} style={{ width: 180 }}>
                <option value="">— none, top-level —</option>
                {dims.map(d => <option key={d.id} value={d.id}>{d.name}</option>)}
              </Select>
            </Field>
            <Button variant="primary" disabled={!newName} loading={create.isPending} onClick={() => create.mutate()}>Create</Button>
          </div>
        )}
      </div>
      {dims.length === 0 ? (
        <EmptyState label="No dimensions defined yet. Create dimensions such as department, region, product, or entity." />
      ) : (
        <div className="mvx-admin-stack">
          {dims.map((d) => <DimensionCard key={d.id} dim={d} allDims={dims} />)}
        </div>
      )}
    </div>
  );
}

// Inline add/edit row rendered inside the tree table.
function DimInlineRow({
  defaultParentId, members, rootLabel, onSave, onCancel, isPending,
}: {
  defaultParentId?: string;
  members: DevDimensionMember[];
  rootLabel?: string;
  onSave: (code: string, label: string, parentId: string) => void;
  onCancel: () => void;
  isPending: boolean;
}) {
  const [code, setCode] = useState("");
  const [label, setLabel] = useState("");
  const [parentId, setParentId] = useState(defaultParentId ?? "");
  return (
    <tr style={{ background: "var(--color-brand-50)" }}>
      <td colSpan={2}>
        <div className="mvx-admin-inline-form" style={{ flexWrap: "nowrap" }}>
          <TextInput value={code} onChange={(e) => setCode(e.target.value.toUpperCase().replace(/[^A-Z0-9_-]/g, ""))}
            placeholder="CODE" style={{ width: 90, fontFamily: "var(--font-mono)" }} autoFocus />
          <TextInput value={label} onChange={(e) => setLabel(e.target.value)}
            placeholder="Label" style={{ flex: 1 }}
            onKeyDown={(e) => e.key === "Enter" && code && label && onSave(code, label, parentId)} />
        </div>
      </td>
      <td>
        <Select value={parentId} onChange={(e) => setParentId(e.target.value)} style={{ width: "100%" }} aria-label="Parent member">
          <option value="">{rootLabel ?? "— root"}</option>
          {members.map(m => <option key={m.id} value={m.id}>{m.label} ({m.code})</option>)}
        </Select>
      </td>
      <td>
        <div className="mvx-admin-inline-form" style={{ flexWrap: "nowrap" }}>
          <Button variant="primary" size="sm" disabled={!code || !label || isPending}
            onClick={() => code && label && onSave(code, label, parentId)}>
            Save
          </Button>
          <IconButton aria-label="Cancel" size={26} onClick={onCancel}>
            <X size={13} />
          </IconButton>
        </div>
      </td>
    </tr>
  );
}

function DimensionCard({ dim, allDims }: { dim: DevDimension; allDims: DevDimension[] }) {
  const qc = useQueryClient();
  const roots = React.useMemo(() => buildDimensionTree(dim.members), [dim.members]);
  const maxDepth = dimMaxDepth(roots);
  const isHierarchy = dim.members.some(m => m.parent_member_id);

  // When this dimension declares a parent (e.g. Cabinet -> Department), every
  // member's parent picker sources from the PARENT dimension's members instead
  // of same-dimension nesting — the two are mutually exclusive (see backend
  // validateMemberParent in handler.go).
  const parentDim = dim.parent_dimension_id ? allDims.find(d => d.id === dim.parent_dimension_id) : undefined;
  const parentPickerMembers = parentDim ? parentDim.members : dim.members;
  const parentLabelOf = (parentMemberId?: string | null) =>
    parentMemberId ? parentPickerMembers.find(m => m.id === parentMemberId)?.label : undefined;

  // Expand/collapse state — start with roots expanded.
  const [expanded, setExpanded] = useState<Set<string>>(() => new Set(roots.map(r => r.id)));
  const [search, setSearch] = useState("");

  // UI mode state
  const [editDimName, setEditDimName] = useState(false);
  const [editNameVal, setEditNameVal] = useState(dim.name);
  const [showProps, setShowProps] = useState(true);
  const [addingChildOf, setAddingChildOf] = useState<string | null>(null); // null = root
  const [addingRoot, setAddingRoot] = useState(false);
  const [editMId, setEditMId] = useState<string | null>(null);
  const [editMCode, setEditMCode] = useState("");
  const [editMLabel, setEditMLabel] = useState("");
  const [editMParent, setEditMParent] = useState("");

  const invalidate = () => qc.invalidateQueries({ queryKey: ["dev-dimensions"] });

  const updateDim = useMutation({
    mutationFn: () => api.updateDimension(dim.id, { name: editNameVal }),
    onSuccess: () => { invalidate(); setEditDimName(false); },
  });
  const delDim = useMutation({
    mutationFn: () => api.deleteDimension(dim.id),
    onSuccess: invalidate,
  });
  const addMember = useMutation({
    mutationFn: ({ code, label, parentId }: { code: string; label: string; parentId: string }) =>
      api.addDimMember(dim.id, { code, label, parent_member_id: parentId || undefined }),
    onSuccess: () => { invalidate(); setAddingChildOf(null); setAddingRoot(false); },
  });
  const updateMember = useMutation({
    mutationFn: () => api.updateDimMember(dim.id, editMId!, {
      code: editMCode, label: editMLabel, parent_member_id: editMParent || null,
      ...(Object.keys(editProps).length ? { properties: editProps } : {}),
    }),
    onSuccess: () => { invalidate(); setEditMId(null); setEditProps({}); },
  });
  const delMember = useMutation({
    mutationFn: (id: string) => api.deleteDimMember(dim.id, id),
    onSuccess: invalidate,
  });
  const { confirm, confirmElement } = useConfirm();

  const toggle = (id: string) => setExpanded(prev => {
    const next = new Set(prev);
    if (next.has(id)) next.delete(id); else next.add(id);
    return next;
  });
  const expandAll = () => setExpanded(new Set(dim.members.map(m => m.id)));
  const collapseAll = () => setExpanded(new Set());

  // Declared properties (model.dimension_property) PLUS any key already
  // present on a member — union, so a property you defined shows as a
  // column even before any member has a value (reported live:
  // "Производительность в сутки" was declared but invisible with no values,
  // leaving nowhere to enter the first one).
  const { data: declaredProps = [] } = useQuery({
    queryKey: ["dim-props", dim.id],
    queryFn: () => api.listDimProperties(dim.id),
  });
  const propKeys = [...new Set([
    ...declaredProps.map(p => p.name),
    ...dim.members.flatMap(m => Object.keys(m.properties ?? {})),
  ])].sort();

  // Draft property edits while a member row is in edit mode.
  const [editProps, setEditProps] = useState<Record<string, string>>({});


  const visibleRows = flattenVisible(roots, expanded, search);

  const startEdit = (m: DevDimensionMember) => {
    setEditMId(m.id); setEditMCode(m.code); setEditMLabel(m.label); setEditMParent(m.parent_member_id ?? ""); setEditProps({ ...(m.properties ?? {}) });
    setAddingChildOf(null); setAddingRoot(false);
  };

  return (
    <div className="mvx-admin-object">

      {/* ── Header ── */}
      <div style={{ background: "var(--color-surface-subtle)", padding: "10px 16px", borderBottom: "1px solid var(--color-border)" }}>
        {editDimName ? (
          <div className="mvx-admin-inline-form">
            <TextInput value={editNameVal} onChange={(e) => setEditNameVal(e.target.value)}
              style={{ width: 180 }}
              onKeyDown={(e) => e.key === "Enter" && updateDim.mutate()} />
            <Button variant="primary" size="sm" loading={updateDim.isPending} onClick={() => updateDim.mutate()}>Save</Button>
            <Button size="sm" onClick={() => { setEditDimName(false); setEditNameVal(dim.name); }}>Cancel</Button>
          </div>
        ) : (
          <div style={{ display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
            <code style={{ fontSize: 14, fontWeight: 700, color: "var(--color-text)" }}>{dim.name}</code>
            <span className="mvx-admin-muted">
              {dim.members.length} member{dim.members.length !== 1 ? "s" : ""}
              {roots.length > 0 && ` · ${roots.length} root${roots.length !== 1 ? "s" : ""}`}
              {isHierarchy && ` · depth ${maxDepth + 1}`}
              {dim.agg_rule && ` · rollup: ${dim.agg_rule}`}
            </span>
            {isHierarchy && <StatusBadge tone="success">hierarchy</StatusBadge>}
            {parentDim && <StatusBadge tone="brand">child of {parentDim.name}</StatusBadge>}
            <div style={{ marginLeft: "auto", display: "flex", gap: 4, alignItems: "center" }}>
              <Button size="sm" variant="ghost" onClick={() => setShowProps(v => !v)}>
                {showProps ? "Hide props" : "Props"}
              </Button>
              <IconButton aria-label="Expand all" title="Expand all" size={26} onClick={expandAll}>
                <ChevronsDown size={14} />
              </IconButton>
              <IconButton aria-label="Collapse all" title="Collapse all" size={26} onClick={collapseAll}>
                <ChevronsUp size={14} />
              </IconButton>
              <IconButton aria-label={`Edit dimension ${dim.name}`} title="Edit dimension name" size={26}
                onClick={() => { setEditDimName(true); setEditNameVal(dim.name); }}>
                <Pencil size={13} />
              </IconButton>
              <IconButton aria-label={`Delete dimension ${dim.name}`} title="Delete dimension" danger size={26}
                onClick={() => confirm({ title: "Delete dimension?", body: `This removes "${dim.name}" and all ${dim.members.length} member${dim.members.length !== 1 ? "s" : ""}.`, confirmLabel: "Delete dimension", onConfirm: () => delDim.mutate() })}>
                <Trash2 size={13} />
              </IconButton>
            </div>
          </div>
        )}

        {/* Search */}
        {!editDimName && dim.members.length > 3 && (
          <div style={{ marginTop: 8, display: "flex", alignItems: "center", gap: 8 }}>
            <SearchInput value={search} onChange={(e) => setSearch(e.target.value)}
              placeholder="Search members…" width={220} />
            {search && (
              <span className="mvx-admin-muted">
                {visibleRows.length} result{visibleRows.length !== 1 ? "s" : ""}
              </span>
            )}
          </div>
        )}
      </div>

      {/* Declared properties — shown ABOVE the members so the schema is
          visible before the data (reported live). Toggled with the members'
          property columns by the same "Props/Hide props" control. */}
      {showProps && <DimPropertiesPanel dimId={dim.id} />}

      {/* ── Tree table ── */}
      <div className="mvx-table-wrap">
      <table className="mvx-table mvx-table--compact" role="treegrid">
        <thead>
          <tr>
            <th style={{ textAlign: "left" }}>Member</th>
            <th style={{ width: 90 }}>Code</th>
            {showProps && propKeys.map(k => (
              <th key={k} style={{ textAlign: "left", whiteSpace: "nowrap", maxWidth: 180, overflow: "hidden", textOverflow: "ellipsis" }} title={k}>{k}</th>
            ))}
            <th style={{ width: 64, textAlign: "center" }}>Children</th>
            <th style={{ width: 110 }}>Actions</th>
          </tr>
        </thead>
        <tbody>
          {dim.members.length === 0 && (
            <tr>
              <td colSpan={4 + (showProps ? propKeys.length : 0)} className="mvx-admin-muted" style={{ textAlign: "center", padding: "24px 16px" }}>
                No members yet. Add a root member below.
              </td>
            </tr>
          )}

          {visibleRows.map((node) => {
            const indent = node.level * 24;
            const isEditing = editMId === node.id;

            return (
              <React.Fragment key={node.id}>
                {/* Member row */}
                <tr role="row" aria-level={node.level + 1} aria-expanded={node.childCount > 0 ? expanded.has(node.id) : undefined}
                  style={isEditing ? { background: "var(--color-brand-50)" } : undefined}>

                  {/* Member column */}
                  <td style={{ paddingLeft: indent + 8 }}>
                    {isEditing ? (
                      <TextInput value={editMLabel} onChange={(e) => setEditMLabel(e.target.value)}
                        style={{ width: "100%" }}
                        onKeyDown={(e) => e.key === "Enter" && updateMember.mutate()} autoFocus />
                    ) : (
                      <div style={{ display: "flex", alignItems: "center", gap: 4, minWidth: 0 }}>
                        {/* Chevron */}
                        {node.childCount > 0 ? (
                          <button
                            type="button"
                            className="mvx-tree-toggle"
                            onClick={() => toggle(node.id)}
                            aria-label={expanded.has(node.id) ? "Collapse" : "Expand"}
                          >
                            {expanded.has(node.id) ? <ChevronDown size={13} /> : <ChevronRight size={13} />}
                          </button>
                        ) : (
                          <span style={{ display: "inline-block", width: 20, flexShrink: 0 }} />
                        )}
                        {/* Label */}
                        <span style={{ fontWeight: node.level === 0 ? 600 : 400, color: "var(--color-text)",
                          overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                          {node.label}
                        </span>
                        {parentDim && (
                          <span className="mvx-admin-muted" style={{ flexShrink: 0 }}>
                            {parentLabelOf(node.parent_member_id) ? `→ ${parentLabelOf(node.parent_member_id)}` : "→ unassigned"}
                          </span>
                        )}
                      </div>
                    )}
                  </td>

                  {/* Code column */}
                  <td className="mvx-admin-mono" style={{ fontWeight: 600, color: "var(--color-brand-600)" }}>
                    {isEditing ? (
                      <TextInput value={editMCode} onChange={(e) => setEditMCode(e.target.value.toUpperCase().replace(/[^A-Z0-9_-]/g, ""))}
                        style={{ width: 70, fontFamily: "var(--font-mono)" }} />
                    ) : node.code}
                  </td>

                  {/* Property values (dimension_member.properties) — one
                      column per declared/observed key, editable inline; the
                      whole set hides with the Props toggle. */}
                  {showProps && propKeys.map(k => (
                    <td key={k} style={{ fontSize: 12, maxWidth: 180 }} title={node.properties?.[k] ?? ""}>
                      {isEditing ? (
                        <TextInput
                          value={editProps[k] ?? ""}
                          onChange={e => setEditProps(p => ({ ...p, [k]: e.target.value }))}
                          placeholder="—"
                          aria-label={`${k} for ${node.label}`}
                          style={{ width: "100%", fontSize: 12 }}
                        />
                      ) : (
                        <span style={{ overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap", display: "block" }}>
                          {node.properties?.[k] ?? <span className="mvx-admin-muted">—</span>}
                        </span>
                      )}
                    </td>
                  ))}

                  {/* Children count */}
                  <td className="mvx-admin-muted" style={{ textAlign: "center" }}>
                    {isEditing ? (
                      <Select value={editMParent} onChange={(e) => setEditMParent(e.target.value)} aria-label="Parent member">
                        <option value="">{parentDim ? "— unassigned" : "— root"}</option>
                        {parentPickerMembers.filter(x => x.id !== node.id).map(x =>
                          <option key={x.id} value={x.id}>{x.label}</option>)}
                      </Select>
                    ) : node.childCount > 0 ? (
                      <button
                        type="button"
                        className="mvx-tree-toggle"
                        onClick={() => toggle(node.id)}
                        title={`${node.childCount} direct child${node.childCount !== 1 ? "ren" : ""}`}
                      >
                        {node.childCount}
                      </button>
                    ) : "—"}
                  </td>

                  {/* Actions */}
                  <td>
                    {isEditing ? (
                      <div className="mvx-admin-inline-form" style={{ flexWrap: "nowrap" }}>
                        <Button variant="primary" size="sm" loading={updateMember.isPending} onClick={() => updateMember.mutate()}>Save</Button>
                        <IconButton aria-label="Cancel edit" size={26} onClick={() => setEditMId(null)}>
                          <X size={13} />
                        </IconButton>
                      </div>
                    ) : (
                      <div style={{ display: "flex", gap: 2 }}>
                        {!parentDim && (
                          <IconButton aria-label={`Add child member under ${node.label}`} title="Add child" size={26}
                            onClick={() => { setAddingChildOf(node.id); setAddingRoot(false); setEditMId(null);
                              if (!expanded.has(node.id)) toggle(node.id); }}>
                            <Plus size={13} />
                          </IconButton>
                        )}
                        <IconButton aria-label={`Edit member ${node.label}`} title="Edit" size={26} onClick={() => startEdit(node)}>
                          <Pencil size={13} />
                        </IconButton>
                        <IconButton aria-label={`Delete member ${node.label}`} title="Delete" danger size={26}
                          onClick={() => confirm({ title: "Delete member?", body: `This removes "${node.code}"${node.descendantCount > 0 ? ` and its ${node.descendantCount} descendant${node.descendantCount !== 1 ? "s" : ""}` : ""}.`, confirmLabel: "Delete member", onConfirm: () => delMember.mutate(node.id) })}>
                          <Trash2 size={13} />
                        </IconButton>
                      </div>
                    )}
                  </td>
                </tr>

                {/* Inline add-child row — appears immediately after the target parent (same-dimension nesting only) */}
                {addingChildOf === node.id && !parentDim && (
                  <DimInlineRow
                    defaultParentId={node.id}
                    members={dim.members}
                    onSave={(code, label, parentId) => addMember.mutate({ code, label, parentId })}
                    onCancel={() => setAddingChildOf(null)}
                    isPending={addMember.isPending}
                  />
                )}
              </React.Fragment>
            );
          })}

          {/* Add root row */}
          {addingRoot && (
            <DimInlineRow
              members={parentPickerMembers}
              rootLabel={parentDim ? "— unassigned" : "— root"}
              onSave={(code, label, parentId) => addMember.mutate({ code, label, parentId })}
              onCancel={() => setAddingRoot(false)}
              isPending={addMember.isPending}
            />
          )}
        </tbody>
      </table>
      </div>

      {/* ── Footer ── */}
      <div style={{ padding: "8px 16px", borderTop: "1px dashed var(--color-border)", background: "var(--color-surface-subtle)" }}>
        <Button size="sm" variant="ghost" leadingIcon={<Plus size={13} />}
          onClick={() => { setAddingRoot(true); setAddingChildOf(null); setEditMId(null); }}>
          {parentDim ? "Add member" : "Add root member"}
        </Button>
      </div>

      {confirmElement}
    </div>
  );
}

function DimPropertiesPanel({ dimId }: { dimId: string }) {
  const qc = useQueryClient();
  const [addName, setAddName] = useState("");
  const [addType, setAddType] = useState("text");
  const [editId, setEditId] = useState<string | null>(null);
  const [editName, setEditName] = useState("");
  const [editType, setEditType] = useState("text");

  const { data: props = [] } = useQuery({
    queryKey: ["dim-props", dimId],
    queryFn: () => api.listDimProperties(dimId),
  });

  const add = useMutation({
    mutationFn: () => api.addDimProperty(dimId, { name: addName, data_type: addType }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["dim-props", dimId] }); setAddName(""); },
  });
  const update = useMutation({
    mutationFn: () => api.updateDimProperty(dimId, editId!, { name: editName, data_type: editType }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["dim-props", dimId] }); setEditId(null); },
  });
  const del = useMutation({
    mutationFn: (propId: string) => api.deleteDimProperty(dimId, propId),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["dim-props", dimId] }),
  });

  const startEdit = (p: DimProperty) => { setEditId(p.id); setEditName(p.name); setEditType(p.data_type); };

  return (
    <div style={{ padding: "12px 16px", borderTop: "1px solid var(--color-border)", background: "var(--color-surface-subtle)" }}>
      <div style={{ fontSize: 12, fontWeight: 600, marginBottom: 10 }}>
        Dimension Properties
        <span className="mvx-admin-muted" style={{ fontWeight: 400, marginLeft: 8 }}>usable in formulas as dim.propertyName</span>
      </div>

      {(props as DimProperty[]).length === 0 && (
        <p className="mvx-admin-muted" style={{ margin: "0 0 10px" }}>No properties defined yet.</p>
      )}

      {(props as DimProperty[]).map((p) => editId === p.id ? (
        <div key={p.id} className="mvx-admin-inline-form" style={{ marginBottom: 6 }}>
          <TextInput value={editName} onChange={(e) => setEditName(e.target.value)} style={{ width: 140 }} />
          <Select value={editType} onChange={(e) => setEditType(e.target.value)} style={{ width: 90 }} aria-label="Property type">
            <option value="text">text</option>
            <option value="number">number</option>
            <option value="date">date</option>
          </Select>
          <Button variant="primary" size="sm" loading={update.isPending} onClick={() => update.mutate()}>Save</Button>
          <Button size="sm" onClick={() => setEditId(null)}>Cancel</Button>
        </div>
      ) : (
        <div key={p.id} style={{ display: "flex", gap: 8, marginBottom: 6, alignItems: "center" }}>
          <code style={{ fontSize: 12, minWidth: 120 }}>{p.name}</code>
          <StatusBadge>{p.data_type}</StatusBadge>
          <IconButton aria-label={`Edit property ${p.name}`} title="Edit" size={26} onClick={() => startEdit(p)}>
            <Pencil size={13} />
          </IconButton>
          <IconButton aria-label={`Delete property ${p.name}`} title="Delete" danger size={26} onClick={() => del.mutate(p.id)}>
            <Trash2 size={13} />
          </IconButton>
        </div>
      ))}

      <div className="mvx-admin-inline-form" style={{ marginTop: 8 }}>
        <TextInput value={addName} onChange={(e) => setAddName(e.target.value)}
          placeholder="property_name" style={{ width: 140 }} />
        <Select value={addType} onChange={(e) => setAddType(e.target.value)} style={{ width: 90 }} aria-label="New property type">
          <option value="text">text</option>
          <option value="number">number</option>
          <option value="date">date</option>
        </Select>
        <Button variant="primary" size="sm" leadingIcon={<Plus size={13} />} disabled={!addName} loading={add.isPending} onClick={() => add.mutate()}>
          Add
        </Button>
      </div>
    </div>
  );
}

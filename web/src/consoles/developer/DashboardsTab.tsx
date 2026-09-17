import { useState, useMemo } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { Plus, LayoutDashboard, Trash2, FolderPlus, Folder } from "lucide-react";
import { api, type DashboardDef, type DashboardFolder } from "../../api/client";
import { Toolbar, ToolbarGroup, SearchInput, FilterChip, Button, Field, TextInput, Select, EmptyState, IconButton, useConfirm } from "../../ui";
import { DashboardCanvas } from "./DashboardCanvas";
import { flattenFolders, UNFILED_LABEL, type FolderNode } from "./folderTree";

// Sentinel for "not in any folder" in the folder selects — distinct from the
// filter's "" (= no folder filter at all), which is why the move control
// can't just reuse an empty option.
const ROOT_OPTION = "__root__";

export function DashboardsTab({ revisionId }: { revisionId?: string }) {
  const qc = useQueryClient();
  const [canvasDashId, setCanvasDashId] = useState<string | null>(null);
  const [canvasDashName, setCanvasDashName] = useState("");

  const [search, setSearch] = useState("");
  const [filterTag, setFilterTag] = useState<string | null>(null);
  const [filterFolder, setFilterFolder] = useState("");
  const [showDashCreate, setShowDashCreate] = useState(false);
  const [newDashName, setNewDashName] = useState("");
  const [newDashTagInput, setNewDashTagInput] = useState("");
  const [newDashTags, setNewDashTags] = useState<string[]>([]);
  const [newDashFolder, setNewDashFolder] = useState(ROOT_OPTION);
  const [editingDashId, setEditingDashId] = useState<string | null>(null);
  const [editDashName, setEditDashName] = useState("");
  const [editDashTags, setEditDashTags] = useState<string[]>([]);
  const [editTagInput, setEditTagInput] = useState("");
  const [showFolderCreate, setShowFolderCreate] = useState(false);
  const [newFolderName, setNewFolderName] = useState("");
  const [newFolderParent, setNewFolderParent] = useState(ROOT_OPTION);

  const { data: dashboardsRaw = [] } = useQuery({
    queryKey: ["dev-dashboards", revisionId],
    queryFn: () => api.listDashboards(revisionId),
  });
  const dashboards = dashboardsRaw as DashboardDef[];

  const { data: foldersRaw = [] } = useQuery({
    queryKey: ["dev-folders", revisionId],
    queryFn: () => api.listFolders(revisionId),
  });
  const folders = useMemo(() => flattenFolders(foldersRaw as DashboardFolder[]), [foldersRaw]);
  const folderByID = useMemo(() => new Map(folders.map(f => [f.id, f])), [folders]);

  const allTags = useMemo(() => {
    const set = new Set<string>();
    dashboards.forEach(d => (d.tags ?? []).forEach(t => set.add(t)));
    return [...set].sort();
  }, [dashboards]);

  const filtered = useMemo(() => {
    return dashboards.filter(d => {
      if (search.trim() && !d.name.toLowerCase().includes(search.trim().toLowerCase())) return false;
      if (filterTag && !(d.tags ?? []).includes(filterTag)) return false;
      if (filterFolder === ROOT_OPTION && d.folder_id) return false;
      if (filterFolder && filterFolder !== ROOT_OPTION && d.folder_id !== filterFolder) return false;
      return true;
    });
  }, [dashboards, search, filterTag, filterFolder]);

  // Dashboards grouped under their folder, in folder-tree order, with the
  // unfiled ones last — so the list mirrors the tree a business user sees.
  const groups = useMemo(() => {
    const byFolder = new Map<string, DashboardDef[]>();
    const unfiled: DashboardDef[] = [];
    for (const d of filtered) {
      if (!d.folder_id) { unfiled.push(d); continue; }
      const bucket = byFolder.get(d.folder_id);
      if (bucket) bucket.push(d);
      else byFolder.set(d.folder_id, [d]);
    }
    const out: { key: string; label: string; folder: FolderNode | null; items: DashboardDef[] }[] = [];
    for (const f of folders) {
      const items = byFolder.get(f.id) ?? [];
      // Empty folders still show, so a developer can see where a dashboard
      // can be moved to (and delete a folder they no longer want).
      out.push({ key: f.id, label: f.path, folder: f, items });
      byFolder.delete(f.id);
    }
    // A dashboard filed under a folder from another revision would otherwise
    // disappear from the list entirely.
    for (const [folderID, items] of byFolder) {
      out.push({ key: folderID, label: `${UNFILED_LABEL} (folder not in this revision)`, folder: null, items });
    }
    if (unfiled.length > 0 || out.length === 0) {
      out.push({ key: ROOT_OPTION, label: UNFILED_LABEL, folder: null, items: unfiled });
    }
    return out.filter(g => g.items.length > 0 || g.folder !== null);
  }, [filtered, folders]);

  const createDashboard = useMutation({
    mutationFn: () => api.createDashboard({
      name: newDashName,
      tags: newDashTags,
      folder_id: newDashFolder === ROOT_OPTION ? "" : newDashFolder,
    }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["dev-dashboards"] });
      setNewDashName(""); setNewDashTags([]); setNewDashTagInput(""); setNewDashFolder(ROOT_OPTION); setShowDashCreate(false);
    },
  });
  const saveDashboard = useMutation({
    mutationFn: ({ id, name, tags }: { id: string; name: string; tags: string[] }) =>
      api.updateDashboard(id, { name, tags }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["dev-dashboards"] }); setEditingDashId(null); },
  });
  // Moving is its own mutation rather than a flag on saveDashboard: it fires
  // straight from the row's select, with no rename in flight.
  const moveDashboard = useMutation({
    mutationFn: ({ dash, folderID }: { dash: DashboardDef; folderID: string }) =>
      api.updateDashboard(dash.id, { name: dash.name, tags: dash.tags ?? [], folder_id: folderID }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["dev-dashboards"] }),
  });
  const deleteDashboard = useMutation({
    mutationFn: (id: string) => api.deleteDashboard(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["dev-dashboards"] }),
  });
  const createFolder = useMutation({
    mutationFn: () => api.createFolder({
      name: newFolderName,
      parent_id: newFolderParent === ROOT_OPTION ? "" : newFolderParent,
    }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["dev-folders"] });
      setNewFolderName(""); setNewFolderParent(ROOT_OPTION); setShowFolderCreate(false);
    },
  });
  const deleteFolder = useMutation({
    mutationFn: (id: string) => api.deleteFolder(id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["dev-folders"] });
      // Dashboards inside it come back unfiled (folder_id ON DELETE SET NULL).
      qc.invalidateQueries({ queryKey: ["dev-dashboards"] });
    },
  });
  const { confirm, confirmElement } = useConfirm();

  const addTag = (tag: string, list: string[], setList: (t: string[]) => void) => {
    const t = tag.trim().toLowerCase().replace(/\s+/g, "-");
    if (t && !list.includes(t)) setList([...list, t]);
  };
  const removeTag = (tag: string, list: string[], setList: (t: string[]) => void) =>
    setList(list.filter(t => t !== tag));

  // With no folders in this revision every folder control collapses to the
  // single choice "Unfiled": the group header says what the count line above
  // already says and the per-row "Move to folder" select has nowhere to move
  // to. Folder UI appears once the developer creates the first folder (the
  // "New folder" button is always there); a dashboard filed under a folder
  // from another revision keeps its explanatory header regardless.
  const hasFolders = folders.length > 0;
  const folderOptions = (rootLabel: string) => (
    <>
      <option value={ROOT_OPTION}>{rootLabel}</option>
      {folders.map(f => (
        <option key={f.id} value={f.id}>{`${"  ".repeat(f.depth)}${f.name}`}</option>
      ))}
    </>
  );

  // Show canvas if a dashboard is selected for design
  if (canvasDashId) {
    return <DashboardCanvas dashId={canvasDashId} dashName={canvasDashName} revisionId={revisionId} onBack={() => setCanvasDashId(null)} />;
  }

  return (
    <div>
      {/* ── Toolbar ── */}
      <Toolbar className="mvx-toolbar--spaced">
        <ToolbarGroup>
          <SearchInput value={search} onChange={e => setSearch(e.target.value)}
            placeholder="Search dashboards…" width={220} />
          {hasFolders && (
            <Select
              value={filterFolder}
              onChange={e => setFilterFolder(e.target.value)}
              aria-label="Filter by folder"
              style={{ width: 180 }}
            >
              <option value="">All folders</option>
              <option value={ROOT_OPTION}>{UNFILED_LABEL}</option>
              {folders.map(f => (
                <option key={f.id} value={f.id}>{`${"  ".repeat(f.depth)}${f.name}`}</option>
              ))}
            </Select>
          )}
          {allTags.map(t => (
            <FilterChip
              key={t}
              active={filterTag === t}
              onClick={() => setFilterTag(t === filterTag ? null : t)}
              onClear={filterTag === t ? () => setFilterTag(null) : undefined}
            >
              {t}
            </FilterChip>
          ))}
        </ToolbarGroup>
        <ToolbarGroup align="end">
          <Button
            leadingIcon={showFolderCreate ? undefined : <FolderPlus size={14} />}
            onClick={() => { setShowFolderCreate(v => !v); setNewFolderName(""); setNewFolderParent(ROOT_OPTION); }}
          >
            {showFolderCreate ? "Cancel" : "New folder"}
          </Button>
          <Button
            leadingIcon={showDashCreate ? undefined : <Plus size={14} />}
            onClick={() => { setShowDashCreate(v => !v); setNewDashName(""); setNewDashTags([]); setNewDashTagInput(""); setNewDashFolder(filterFolder && filterFolder !== ROOT_OPTION ? filterFolder : ROOT_OPTION); }}
          >
            {showDashCreate ? "Cancel" : "New dashboard"}
          </Button>
        </ToolbarGroup>
      </Toolbar>

      {/* ── Create folder ── */}
      {showFolderCreate && (
        <div className="mvx-panel" style={{ padding: 16, marginBottom: 16 }}>
          <div style={{ display: "flex", gap: 12, flexWrap: "wrap", alignItems: "flex-end" }}>
            <Field label="Folder name">
              <TextInput value={newFolderName} onChange={e => setNewFolderName(e.target.value)}
                onKeyDown={e => { if (e.key === "Enter" && newFolderName) createFolder.mutate(); }}
                placeholder="e.g. Finance" style={{ width: 220 }} autoFocus />
            </Field>
            <Field label="Inside">
              <Select value={newFolderParent} onChange={e => setNewFolderParent(e.target.value)} style={{ width: 200 }}>
                {folderOptions("Top level")}
              </Select>
            </Field>
            <Button variant="primary" disabled={!newFolderName} loading={createFolder.isPending} onClick={() => createFolder.mutate()}>
              Create folder
            </Button>
          </div>
          {createFolder.isError && <p className="mvx-admin-error">{(createFolder.error as Error).message}</p>}
        </div>
      )}

      {/* ── Create dashboard ── */}
      {showDashCreate && (
        <div className="mvx-panel" style={{ padding: 16, marginBottom: 16 }}>
          <div style={{ display: "flex", gap: 12, flexWrap: "wrap", alignItems: "flex-end" }}>
            <Field label="Name">
              <TextInput value={newDashName} onChange={e => setNewDashName(e.target.value)}
                placeholder="e.g. OPEX Dashboard" style={{ width: 220 }} autoFocus />
            </Field>
            {hasFolders && (
              <Field label="Folder">
                <Select value={newDashFolder} onChange={e => setNewDashFolder(e.target.value)} style={{ width: 200 }}>
                  {folderOptions(UNFILED_LABEL)}
                </Select>
              </Field>
            )}
            <Field label="Tags">
              <div style={{ display: "flex", gap: 4, alignItems: "center", flexWrap: "wrap" }}>
                {newDashTags.map(t => (
                  <FilterChip key={t} onClear={() => removeTag(t, newDashTags, setNewDashTags)}>
                    {t}
                  </FilterChip>
                ))}
                <TextInput value={newDashTagInput} onChange={e => setNewDashTagInput(e.target.value)}
                  onKeyDown={e => { if (e.key === "Enter" || e.key === ",") { addTag(newDashTagInput, newDashTags, setNewDashTags); setNewDashTagInput(""); e.preventDefault(); } }}
                  placeholder="Add tag…" style={{ width: 120 }} />
              </div>
            </Field>
            <Button variant="primary" disabled={!newDashName} loading={createDashboard.isPending} onClick={() => createDashboard.mutate()}>
              Create
            </Button>
          </div>
        </div>
      )}

      <p className="mvx-admin-muted" style={{ margin: "0 0 12px" }}>
        {filtered.length} of {dashboards.length} dashboard{dashboards.length !== 1 ? "s" : ""}
        {filterTag ? ` tagged "${filterTag}"` : ""}
        {filterFolder === ROOT_OPTION ? ` outside any folder` : ""}
        {filterFolder && filterFolder !== ROOT_OPTION ? ` in "${folderByID.get(filterFolder)?.path ?? "folder"}"` : ""}
      </p>

      {filtered.length === 0 && folders.length === 0 && <EmptyState label="No dashboards match." />}

      <div style={{ display: "flex", flexDirection: "column", gap: 20 }}>
        {groups.map(group => (
          <div key={group.key} style={{ display: "flex", flexDirection: "column", gap: 12 }}>
            {(hasFolders || group.key !== ROOT_OPTION) && (
            <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
              <Folder size={14} aria-hidden="true" style={{ color: "var(--color-text-muted)" }} />
              <span style={{ fontWeight: 600, fontSize: 13 }}>{group.label}</span>
              <span className="mvx-admin-muted" style={{ fontSize: 12 }}>
                {group.items.length} dashboard{group.items.length !== 1 ? "s" : ""}
              </span>
              {group.folder && (
                <IconButton
                  aria-label={`Delete folder ${group.folder.name}`}
                  title="Delete folder"
                  danger
                  size={26}
                  onClick={() => confirm({
                    title: "Delete folder?",
                    body: `"${group.folder!.name}" and any folders inside it are removed. Dashboards in them are kept and become unfiled.`,
                    confirmLabel: "Delete folder",
                    onConfirm: () => deleteFolder.mutate(group.folder!.id),
                  })}
                >
                  <Trash2 size={13} />
                </IconButton>
              )}
            </div>
            )}

            {group.items.length === 0 && (
              <p className="mvx-admin-muted" style={{ margin: 0, fontSize: 13 }}>Empty — move a dashboard here with its Folder control.</p>
            )}

            {group.items.map(dash => (
              <div key={dash.id} className="mvx-admin-object">
                <div className="mvx-admin-object__header" style={{ flexWrap: "wrap", borderBottom: "none" }}>
                  {editingDashId === dash.id ? (
                    <div style={{ flex: 1, display: "flex", flexDirection: "column", gap: 10 }}>
                      <div className="mvx-admin-inline-form">
                        <TextInput value={editDashName} onChange={e => setEditDashName(e.target.value)}
                          onKeyDown={e => e.key === "Enter" && editDashName && saveDashboard.mutate({ id: dash.id, name: editDashName, tags: editDashTags })}
                          style={{ width: 220 }} autoFocus />
                        <Button variant="primary" size="sm" disabled={!editDashName}
                          onClick={() => editDashName && saveDashboard.mutate({ id: dash.id, name: editDashName, tags: editDashTags })}>
                          Save
                        </Button>
                        <Button size="sm" onClick={() => setEditingDashId(null)}>Cancel</Button>
                      </div>
                      <div style={{ display: "flex", gap: 6, alignItems: "center", flexWrap: "wrap" }}>
                        <span className="mvx-admin-muted">Tags:</span>
                        {editDashTags.map(t => (
                          <FilterChip key={t} onClear={() => removeTag(t, editDashTags, setEditDashTags)}>
                            {t}
                          </FilterChip>
                        ))}
                        <TextInput value={editTagInput} onChange={e => setEditTagInput(e.target.value)}
                          onKeyDown={e => { if (e.key === "Enter" || e.key === ",") { addTag(editTagInput, editDashTags, setEditDashTags); setEditTagInput(""); e.preventDefault(); } }}
                          placeholder="Add tag…" style={{ width: 110 }} />
                      </div>
                    </div>
                  ) : (
                    <>
                      <button
                        type="button"
                        title="Click to rename"
                        className="mvx-rename-target"
                        onClick={() => { setEditingDashId(dash.id); setEditDashName(dash.name); setEditDashTags([...(dash.tags ?? [])]); setEditTagInput(""); }}
                      >{dash.name}</button>
                      {(dash.tags ?? []).map(t => (
                        <FilterChip key={t} active={t === filterTag} onClick={() => setFilterTag(t === filterTag ? null : t)}>
                          {t}
                        </FilterChip>
                      ))}
                      <span className="mvx-admin-muted">{(dash.widgets ?? []).length} widget{(dash.widgets ?? []).length !== 1 ? "s" : ""}</span>
                      <div style={{ marginLeft: "auto", display: "flex", gap: 6, alignItems: "center" }}>
                        {hasFolders && (
                        <Select
                          value={dash.folder_id ?? ROOT_OPTION}
                          aria-label={`Folder for ${dash.name}`}
                          title="Move to folder"
                          disabled={moveDashboard.isPending}
                          onChange={e => moveDashboard.mutate({ dash, folderID: e.target.value === ROOT_OPTION ? "" : e.target.value })}
                          style={{ width: 170 }}
                        >
                          {folderOptions(UNFILED_LABEL)}
                        </Select>
                        )}
                        <Button size="sm" leadingIcon={<LayoutDashboard size={13} />}
                          onClick={() => { setCanvasDashId(dash.id); setCanvasDashName(dash.name); }}>
                          Design
                        </Button>
                        <IconButton
                          aria-label={`Delete dashboard ${dash.name}`}
                          title="Delete dashboard"
                          danger
                          onClick={() => confirm({ title: "Delete dashboard?", body: `This removes "${dash.name}" from all assigned business roles.`, confirmLabel: "Delete dashboard", onConfirm: () => deleteDashboard.mutate(dash.id) })}
                        >
                          <Trash2 size={14} />
                        </IconButton>
                      </div>
                    </>
                  )}
                </div>
              </div>
            ))}
          </div>
        ))}
      </div>
      {moveDashboard.isError && <p className="mvx-admin-error">{(moveDashboard.error as Error).message}</p>}
      {confirmElement}
    </div>
  );
}

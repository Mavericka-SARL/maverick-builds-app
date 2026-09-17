import { useState, useMemo, useEffect } from "react";
import { useQuery } from "@tanstack/react-query";
import { api, type DemoContext, type DashboardDef, type DashboardFolder } from "../../api/client";
import { LoadingState, EmptyState, Toolbar, ToolbarGroup, SearchInput, FilterChip, Button, Select, Tabs, SectionHeader } from "../../ui";
import { flattenFolders, UNFILED_LABEL } from "../developer/folderTree";
import { DashboardWidgetGrid } from "./DashboardWidgets";

// Matches the Developer Console's folder controls: "" filters nothing,
// this sentinel narrows to dashboards that aren't in any folder.
const UNFILED_OPTION = "__root__";

export function DashboardsView({ ctx, onOpenInstance }: { ctx: DemoContext; onOpenInstance?: (instanceId: string) => void }) {
  const [search, setSearch] = useState("");
  const [filterTag, setFilterTag] = useState<string | null>(null);
  const [filterFolder, setFilterFolder] = useState("");
  const [activeDashId, setActiveDashId] = useState<string | null>(null);

  const { data: dashboardsRaw = [], isLoading } = useQuery({
    queryKey: ["user-dashboards"],
    queryFn: api.listUserDashboards,
  });
  const allDashboards = dashboardsRaw as DashboardDef[];

  const { data: foldersRaw = [] } = useQuery({
    queryKey: ["user-folders"],
    queryFn: api.listUserFolders,
  });
  // Only folders that actually hold a dashboard this user can see — the
  // endpoint already filters by the same role grants, and an empty folder is
  // noise on a read-only view (unlike the Developer Console, where an empty
  // folder is a place to move things into).
  const folders = useMemo(() => {
    const populated = new Set(allDashboards.map(d => d.folder_id).filter(Boolean) as string[]);
    return flattenFolders(foldersRaw as DashboardFolder[]).filter(f => populated.has(f.id));
  }, [foldersRaw, allDashboards]);
  const hasUnfiled = useMemo(() => allDashboards.some(d => !d.folder_id), [allDashboards]);

  const allTags = useMemo(() => {
    const set = new Set<string>();
    allDashboards.forEach(d => (d.tags ?? []).forEach(t => set.add(t)));
    return [...set].sort();
  }, [allDashboards]);

  const visible = useMemo(() => {
    return allDashboards.filter(d => {
      if (search.trim() && !d.name.toLowerCase().includes(search.trim().toLowerCase())) return false;
      if (filterTag && !(d.tags ?? []).includes(filterTag)) return false;
      if (filterFolder === UNFILED_OPTION && d.folder_id) return false;
      if (filterFolder && filterFolder !== UNFILED_OPTION && d.folder_id !== filterFolder) return false;
      return true;
    });
  }, [allDashboards, search, filterTag, filterFolder]);

  // Reset active when filter changes
  useEffect(() => {
    // eslint-disable-next-line react-hooks/set-state-in-effect -- keep selection valid when filter changes
    setActiveDashId(prev => visible.some(d => d.id === prev) ? prev : (visible[0]?.id ?? null));
  }, [filterTag, search, filterFolder]); // eslint-disable-line react-hooks/exhaustive-deps

  const activeId = activeDashId ?? visible[0]?.id ?? null;

  const { data: detail, isLoading: detailLoading } = useQuery({
    queryKey: ["user-dashboard-detail", activeId],
    queryFn: () => api.getUserDashboard(activeId!),
    enabled: !!activeId,
    staleTime: 5_000,
  });

  if (isLoading) return <LoadingState label="Loading dashboards…" />;

  if (allDashboards.length === 0) {
    return <EmptyState label="No dashboards available. Ask a Developer to create dashboards in the Developer Console." />;
  }

  return (
    <div>
      {/* ── Search + tag filter bar ── */}
      <Toolbar className="mvx-toolbar--spaced">
        <ToolbarGroup>
          <SearchInput
            value={search}
            onChange={e => setSearch(e.target.value)}
            placeholder="Search dashboards…"
            width={200}
          />
          {folders.length > 0 && (
            <Select
              value={filterFolder}
              onChange={e => setFilterFolder(e.target.value)}
              aria-label="Filter by folder"
              style={{ width: 180 }}
            >
              <option value="">All folders</option>
              {folders.map(f => (
                <option key={f.id} value={f.id}>{`${"  ".repeat(f.depth)}${f.name}`}</option>
              ))}
              {hasUnfiled && <option value={UNFILED_OPTION}>{UNFILED_LABEL}</option>}
            </Select>
          )}
          {allTags.map(t => (
            <FilterChip
              key={t}
              active={filterTag === t}
              onClick={() => setFilterTag(filterTag === t ? null : t)}
            >
              {t}
            </FilterChip>
          ))}
          {(filterTag || search || filterFolder) && (
            <Button size="sm" variant="ghost" onClick={() => { setFilterTag(null); setSearch(""); setFilterFolder(""); }}>
              Clear
            </Button>
          )}
        </ToolbarGroup>
      </Toolbar>

      {visible.length === 0 && (
        <EmptyState label="No dashboards match your search." />
      )}

      {/* ── Dashboard tab bar ── */}
      {visible.length > 1 && (
        <Tabs
          tabs={visible.map(d => ({ id: d.id, label: d.name }))}
          active={activeId ?? visible[0].id}
          onChange={(id) => setActiveDashId(id)}
          style={{ marginBottom: 24 }}
        />
      )}

      {visible.length === 1 && (
        <SectionHeader title={visible[0].name} />
      )}

      {detailLoading && <LoadingState />}

      {detail && (
        <div>
          {(detail.widgets ?? []).length === 0 ? (
            <p style={{ color: "var(--color-disabled)" }}>This dashboard has no widgets yet.</p>
          ) : (
            <DashboardWidgetGrid widgets={detail.widgets ?? []} ctx={ctx} dashboardId={activeId ?? undefined} onOpenInstance={onOpenInstance} />
          )}
        </div>
      )}
    </div>
  );
}

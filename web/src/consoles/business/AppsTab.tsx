import { useQuery } from "@tanstack/react-query";
import { api, type AppInfo, type AppModelInfo } from "../../api/client";
import { Button, LoadingState, EmptyState, StatusBadge } from "../../ui";
import { SELECTED_APP_KEY, SELECTED_MODEL_KEY, selectModel } from "./modelSelection";

export function AppsTab() {
  const { data: apps = [], isLoading } = useQuery({ queryKey: ["apps"], queryFn: api.getApps });
  const currentId = localStorage.getItem(SELECTED_APP_KEY) ?? "";

  const currentModelId = localStorage.getItem(SELECTED_MODEL_KEY) ?? "";

  function select(id: string) {
    if (id === currentId) return;
    localStorage.setItem(SELECTED_APP_KEY, id);
    localStorage.removeItem(SELECTED_MODEL_KEY);
    window.location.reload();
  }

  if (isLoading) return <LoadingState />;
  if (apps.length === 0) return <EmptyState label="No models available." />;

  const list = apps as AppInfo[];
  const active = list.find(a => a.id === currentId) ?? list[0];
  // The model the consoles show for an application: the one opened here (in
  // the current application only), else its default — what X-Model-Id and
  // the bar at the top say. The application's own model_name and
  // active_revision are its default's, which named the wrong model once
  // another was opened.
  const workingModel = (app: AppInfo, isCurrent: boolean): AppModelInfo | undefined => {
    const models = app.models ?? [];
    return (isCurrent && currentModelId ? models.find(m => m.id === currentModelId) : undefined)
      ?? models.find(m => m.is_default) ?? models[0];
  };
  const activeModel = workingModel(active, true);

  return (
    <div className="mvx-admin-stack" style={{ maxWidth: 700 }}>
      {/* Working banner */}
      <div className="mvx-context-banner">
        Working in: <strong>{active.name}{activeModel ? ` · ${activeModel.name}` : ""}</strong>
        <span style={{ marginLeft: 8, color: "var(--color-text-muted)" }}>
          · revision <strong>{(activeModel ? activeModel.active_revision : active.active_revision) || "—"}</strong>
        </span>
      </div>

      {list.map((app) => {
        const isCurrent = app.id === (currentId || list[0]?.id);
        const shown = workingModel(app, isCurrent);
        const shownRevision = shown ? shown.active_revision : app.active_revision;
        return (
          <div
            key={app.id}
            className="mvx-admin-object"
            onClick={() => select(app.id)}
            style={{ cursor: "pointer", boxShadow: isCurrent ? "0 0 0 2px var(--color-brand-600)" : undefined }}
          >
            {/* App header */}
            <div className="mvx-admin-object__header">
              <div className="mvx-admin-avatar mvx-admin-avatar--app">{app.name[0]}</div>
              <div className="mvx-admin-object__title">
                <div className="mvx-admin-object__name">{app.name}</div>
                <div className="mvx-admin-object__meta">
                  {app.tenant_name && new Set(apps.map((a) => a.tenant_id ?? "")).size > 1 ? `${app.tenant_name} · ${app.workspace_name}` : app.workspace_name}
                </div>
              </div>
              {isCurrent && <StatusBadge tone="brand">Active</StatusBadge>}
            </div>

            {/* Models — every model the user may access is switchable; the
                consoles follow the selection via the X-Model-Id header
                (reported live: access to Solo existed but nothing offered
                a way to open it). */}
            <div className="mvx-admin-object__body">
              {(app.models ?? []).length > 1 && (
                <div style={{ display: "flex", flexDirection: "column", gap: 6, marginBottom: 10 }}>
                  {(app.models ?? []).map(m => {
                    const isActiveModel = isCurrent && shown?.id === m.id;
                    return (
                      <div key={m.id} className="mvx-panel" style={{ padding: "8px 12px", display: "flex", alignItems: "center", gap: 8 }}>
                        <span style={{ fontWeight: 600, fontSize: 13 }}>{m.name}</span>
                        {m.is_default && <StatusBadge tone="success">default</StatusBadge>}
                        {m.active_revision && <span className="mvx-admin-muted" style={{ fontSize: 11 }}>rev: {m.active_revision}</span>}
                        <span style={{ marginLeft: "auto" }}>
                          {isActiveModel ? (
                            <StatusBadge tone="brand">Working here</StatusBadge>
                          ) : (
                            <Button size="sm" onClick={e => { e.stopPropagation(); selectModel(app.id, m.id, m.is_default); }}>
                              Open
                            </Button>
                          )}
                        </span>
                      </div>
                    );
                  })}
                </div>
              )}
              <div className="mvx-admin-model">
                <div className="mvx-admin-model__header">
                  <span className="mvx-admin-model__name">{shown?.name || app.model_name || app.name}</span>
                </div>
                <div className="mvx-admin-revisions">
                  <div className="mvx-admin-revisions__label">Active Revision</div>
                  {shownRevision ? (
                    <div className="mvx-admin-revision" style={{ alignSelf: "stretch" }}>
                      <div className="mvx-admin-revision__info">
                        <span className="mvx-admin-revision__name">{shownRevision}</span>
                      </div>
                      <StatusBadge tone="live">Live</StatusBadge>
                    </div>
                  ) : (
                    <p className="mvx-admin-muted">No active revision set.</p>
                  )}
                </div>
              </div>
            </div>
          </div>
        );
      })}
    </div>
  );
}

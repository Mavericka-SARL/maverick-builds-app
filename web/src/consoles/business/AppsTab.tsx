import { useQuery } from "@tanstack/react-query";
import { api, type AppInfo } from "../../api/client";
import { Button, LoadingState, EmptyState, StatusBadge } from "../../ui";

export const SELECTED_APP_KEY = "selected_app_id";

export function AppsTab() {
  const { data: apps = [], isLoading } = useQuery({ queryKey: ["apps"], queryFn: api.getApps });
  const currentId = localStorage.getItem(SELECTED_APP_KEY) ?? "";

  const currentModelId = localStorage.getItem("selected_model_id") ?? "";

  function select(id: string) {
    if (id === currentId) return;
    localStorage.setItem(SELECTED_APP_KEY, id);
    localStorage.removeItem("selected_model_id");
    window.location.reload();
  }

  function selectModel(appId: string, modelId: string, isDefault: boolean) {
    localStorage.setItem(SELECTED_APP_KEY, appId);
    // The default needs no pin — clearing keeps behavior stable if the
    // developer later changes which model is the default.
    if (isDefault) localStorage.removeItem("selected_model_id");
    else localStorage.setItem("selected_model_id", modelId);
    window.location.reload();
  }

  if (isLoading) return <LoadingState />;
  if (apps.length === 0) return <EmptyState label="No models available." />;

  const list = apps as AppInfo[];
  const active = list.find(a => a.id === currentId) ?? list[0];

  return (
    <div className="mvx-admin-stack" style={{ maxWidth: 700 }}>
      {/* Working banner */}
      <div className="mvx-context-banner">
        Working in: <strong>{active.name}</strong>
        <span style={{ marginLeft: 8, color: "var(--color-text-muted)" }}>
          · revision <strong>{active.active_revision || "—"}</strong>
        </span>
      </div>

      {list.map((app) => {
        const isCurrent = app.id === (currentId || list[0]?.id);
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
                <div className="mvx-admin-object__meta">{app.workspace_name}</div>
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
                    const isActiveModel = isCurrent && (currentModelId ? currentModelId === m.id : m.is_default);
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
                  <span className="mvx-admin-model__name">{app.model_name || app.name}</span>
                </div>
                <div className="mvx-admin-revisions">
                  <div className="mvx-admin-revisions__label">Active Revision</div>
                  {app.active_revision ? (
                    <div className="mvx-admin-revision" style={{ alignSelf: "stretch" }}>
                      <div className="mvx-admin-revision__info">
                        <span className="mvx-admin-revision__name">{app.active_revision}</span>
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

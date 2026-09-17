import React, { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { Trash2, Plus } from "lucide-react";
import { api, type AdminModel } from "../../api/client";
import { Button, IconButton, TextInput, LoadingState, EmptyState, StatusBadge, RevisionBadge, useConfirm } from "../../ui";

function DevModelRevisions({
  model,
  appId,
  revisionId,
  onSelect,
}: {
  model: AdminModel;
  appId: string;
  revisionId: string;
  onSelect: (id: string, name: string) => void;
}) {
  const qc = useQueryClient();
  const [showNew, setShowNew] = useState(false);
  const [newName, setNewName] = useState("");

  // Source revision for copying: prefer the global working revision if it belongs to THIS model,
  // otherwise fall back to the model's active revision, then the latest revision in the list.
  const localRevs = model.revisions ?? [];
  const globalRevInThisModel = localRevs.find((r) => r.id === revisionId);
  const activeRev = localRevs.find((r) => r.name === model.active_revision);
  const sourceRevId = (globalRevInThisModel ?? activeRev ?? localRevs[localRevs.length - 1])?.id ?? "";

  const inv = () => {
    qc.invalidateQueries({ queryKey: ["dev-applications"] });
    qc.invalidateQueries({ queryKey: ["dev-model"] });
    qc.invalidateQueries({ queryKey: ["dev-dimensions"] });
  };

  const create = useMutation({
    mutationFn: () => {
      localStorage.setItem("selected_app_id", appId);
      return api.createDevRevision(newName.trim(), sourceRevId || undefined);
    },
    onSuccess: (data) => {
      inv();
      setShowNew(false);
      setNewName("");
      onSelect(data.id, newName.trim());
    },
  });

  const activate = useMutation({
    mutationFn: (id: string) => api.activateDevRevision(id),
    onSuccess: () => inv(),
  });

  const del = useMutation({
    mutationFn: (id: string) => api.deleteDevRevision(id),
    onSuccess: (_, id) => {
      inv();
      if (revisionId === id) onSelect("", "");
    },
  });
  const { confirm, confirmElement } = useConfirm();

  return (
    <div className="mvx-admin-revisions">
      <div className="mvx-admin-revisions__label">Revisions</div>

      {(model.revisions ?? []).length === 0 && (
        <p className="mvx-admin-muted">No revisions yet.</p>
      )}

      <div className="mvx-admin-revisions__list">
        {(model.revisions ?? []).map((s) => {
          const isWorking = s.id === revisionId;
          return (
            <div
              key={s.id}
              className={["mvx-admin-revision", isWorking ? "mvx-admin-revision--working" : ""].filter(Boolean).join(" ")}
              style={{ cursor: "pointer" }}
              onClick={() => { localStorage.setItem("selected_app_id", appId); onSelect(s.id, s.name); }}
            >
              <div className="mvx-admin-revision__info">
                <div className="mvx-admin-revision__name" style={{ whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }}>
                  {s.name}
                </div>
                <div className="mvx-admin-revision__meta">
                  {s.id.slice(0, 8)} · {s.created_at ? s.created_at.slice(0, 10) : ""}
                </div>
              </div>
              {s.name === model.active_revision && <RevisionBadge status="live" />}
              {isWorking && <StatusBadge tone="brand">Working</StatusBadge>}
              {/* Set Active + Delete — not allowed on the LIVE revision */}
              {s.name !== model.active_revision && (
                <>
                  <Button
                    size="sm"
                    title="Set as active revision"
                    disabled={activate.isPending}
                    onClick={(e) => {
                      e.stopPropagation();
                      confirm({ title: "Set as active revision?", body: `"${s.name}" becomes the live revision everyone sees. This can be changed again later.`, confirmLabel: "Set active", destructive: false, onConfirm: () => activate.mutate(s.id) });
                    }}
                  >
                    Set active
                  </Button>
                  <IconButton
                    aria-label={`Delete revision ${s.name}`}
                    title="Delete revision"
                    danger
                    size={26}
                    onClick={(e) => {
                      e.stopPropagation();
                      confirm({ title: "Delete revision?", body: `This permanently removes "${s.name}". This cannot be undone.`, confirmLabel: "Delete revision", onConfirm: () => del.mutate(s.id) });
                    }}
                  >
                    <Trash2 size={13} />
                  </IconButton>
                </>
              )}
            </div>
          );
        })}
      </div>

      {/* New revision */}
      {showNew ? (
        <div className="mvx-admin-inline-form">
          <TextInput
            value={newName}
            onChange={(e) => setNewName(e.target.value)}
            onKeyDown={(e) => { if (e.key === "Enter" && newName.trim()) create.mutate(); if (e.key === "Escape") setShowNew(false); }}
            placeholder="Revision name…"
            autoFocus
            style={{ width: 220 }}
          />
          <Button
            variant="primary"
            size="sm"
            disabled={!newName.trim()}
            loading={create.isPending}
            loadingLabel="Saving…"
            onClick={() => create.mutate()}
          >
            Save
          </Button>
          <Button size="sm" onClick={() => setShowNew(false)}>Cancel</Button>
        </div>
      ) : (
        <Button size="sm" variant="ghost" leadingIcon={<Plus size={13} />} onClick={() => { setShowNew(true); setNewName(""); }}>
          New revision
        </Button>
      )}
      {confirmElement}
    </div>
  );
}

export function DevApplicationsTab({
  revisionId,
  revisionName,
  onSelect,
}: {
  revisionId: string;
  revisionName: string;
  onSelect: (id: string, name: string) => void;
}) {
  const { data: tenants = [], isLoading } = useQuery({
    queryKey: ["dev-applications"],
    queryFn: api.getDevApplications,
  });
  const qc = useQueryClient();
  const setDefault = useMutation({
    mutationFn: (modelId: string) => api.setDefaultModel(modelId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["dev-applications"] });
      // The default drives what /api/demo and every business list resolve.
      qc.invalidateQueries({ queryKey: ["demo"] });
    },
  });

  const apps = tenants.flatMap((t) => t.applications ?? []);

  // Auto-select the last (most recently created) revision on first load
  React.useEffect(() => {
    if (revisionId || isLoading || apps.length === 0) return;
    const allScenarios = apps.flatMap((a) => a.models.flatMap((m) =>
      (m.revisions ?? []).map((s) => ({ ...s, modelId: m.id, appId: a.id }))
    ));
    if (allScenarios.length === 0) return;
    const last = allScenarios.sort((a, b) =>
      (b.created_at ?? "").localeCompare(a.created_at ?? "")
    )[0];
    localStorage.setItem("selected_app_id", last.appId);
    onSelect(last.id, last.name);
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [isLoading]);

  if (isLoading) return <LoadingState />;
  if (apps.length === 0) return <EmptyState label="No applications found." />;

  return (
    <div className="mvx-admin-stack">
      {/* Current working revision banner */}
      {revisionId ? (
        <div className="mvx-context-banner">
          Working in revision: <strong>{revisionName}</strong>
          <span className="mvx-admin-mono mvx-admin-muted" style={{ marginLeft: 8 }}>
            {revisionId.slice(0, 8)}
          </span>
        </div>
      ) : (
        <div className="mvx-context-banner mvx-context-banner--warning">
          No revision selected — select a revision below to start working
        </div>
      )}

      {apps.map((app) => (
        <div key={app.id} className="mvx-admin-object">
          {/* App header */}
          <div className="mvx-admin-object__header">
            <div className="mvx-admin-avatar mvx-admin-avatar--app">{app.name[0]}</div>
            <div className="mvx-admin-object__title">
              <div className="mvx-admin-object__name">{app.name}</div>
              <div className="mvx-admin-object__meta">{app.models.length} model{app.models.length !== 1 ? "s" : ""}</div>
            </div>
          </div>

          {/* Models */}
          <div className="mvx-admin-object__body">
            {app.models.length === 0 && <p className="mvx-admin-muted">No models.</p>}
            {app.models.map((m) => (
              <div key={m.id} className="mvx-admin-model">
                <div className="mvx-admin-model__header" style={{ display: "flex", alignItems: "center", gap: 8 }}>
                  <span className="mvx-admin-model__name">{m.name}</span>
                  {m.is_default ? (
                    <StatusBadge tone="success">business default</StatusBadge>
                  ) : (
                    <Button
                      size="sm"
                      variant="ghost"
                      title="Business consoles (dashboards, workflows, grids) show the default model. Developers pin other models via the revision selector."
                      onClick={() => setDefault.mutate(m.id)}
                    >
                      Set as business default
                    </Button>
                  )}
                </div>
                <DevModelRevisions model={m} appId={app.id} revisionId={revisionId} onSelect={onSelect} />
              </div>
            ))}
          </div>
        </div>
      ))}
    </div>
  );
}

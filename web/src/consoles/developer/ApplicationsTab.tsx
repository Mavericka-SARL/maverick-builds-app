import React, { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { Trash2, Plus } from "lucide-react";
import { api, withTenant, type AdminModel, type AdminTenant } from "../../api/client";
import { Button, IconButton, TextInput, Select, Field, Toolbar, ToolbarGroup, LoadingState, EmptyState, StatusBadge, RevisionBadge, useConfirm } from "../../ui";

function DevModelRevisions({
  model,
  appId,
  tenantId,
  revisionId,
  onSelect,
}: {
  model: AdminModel;
  appId: string;
  /** The tenant the model belongs to: the list spans databases, and each
   *  revision action is addressed to the model's own (X-Tenant-Id). */
  tenantId: string;
  revisionId: string;
  onSelect: (id: string, name: string) => void;
}) {
  const qc = useQueryClient();
  const at = <T,>(call: () => Promise<T>): Promise<T> => (tenantId ? withTenant(tenantId, call) : call());
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
    // Tenant admin › Applications lists the same revisions.
    qc.invalidateQueries({ queryKey: ["admin-tenants"] });
    qc.invalidateQueries({ queryKey: ["dev-model"] });
    qc.invalidateQueries({ queryKey: ["dev-dimensions"] });
  };

  const create = useMutation({
    mutationFn: () => {
      localStorage.setItem("selected_app_id", appId);
      // This row's model, not whichever model the server would resolve.
      return at(() => api.createDevRevision(model.id, newName.trim(), sourceRevId || undefined));
    },
    onSuccess: (data) => {
      inv();
      setShowNew(false);
      setNewName("");
      onSelect(data.id, newName.trim());
    },
  });

  const activate = useMutation({
    mutationFn: (id: string) => at(() => api.activateDevRevision(id)),
    onSuccess: () => inv(),
  });

  const del = useMutation({
    mutationFn: (id: string) => at(() => api.deleteDevRevision(id)),
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

/**
 * New application and New model, at the top of Developer › Models. Creating them
 * belongs to the tenant admin (and platform admin): the bar offers only the
 * tenants of this list that the person administers, and is absent for a
 * developer alone, whose guide sends them to their tenant admin.
 */
function CreateBar({ tenants }: { tenants: AdminTenant[] }) {
  const qc = useQueryClient();
  const { data: adminTenants = [] } = useQuery({ queryKey: ["admin-tenants"], queryFn: api.getAdminTenants });
  const administered = new Set(adminTenants.map((t) => t.id));
  const own = tenants.filter((t) => administered.has(t.id));
  const apps = own.flatMap((t) => (t.applications ?? []).map((a) => ({ id: a.id, name: a.name, tenantId: t.id, tenantName: t.name })));

  const [open, setOpen] = useState<"" | "app" | "model">("");
  const [name, setName] = useState("");
  const [tenantId, setTenantId] = useState("");
  const [appId, setAppId] = useState("");

  const inv = () => {
    qc.invalidateQueries({ queryKey: ["dev-applications"] });
    qc.invalidateQueries({ queryKey: ["admin-tenants"] });
  };
  const show = (which: "app" | "model", app = "") => {
    setOpen((cur) => (cur === which && !app ? "" : which));
    setName("");
    setTenantId(own[0]?.id ?? "");
    setAppId(app || apps[0]?.id || "");
  };
  const createApp = useMutation({
    mutationFn: () => api.createAdminApplication({ customer_id: tenantId || own[0].id, name: name.trim(), mode: "planning" }),
    // A new application is empty: go straight on to its first model.
    onSuccess: (data) => { inv(); show("model", data.id); },
  });
  const createModel = useMutation({
    mutationFn: () => {
      const app = apps.find((a) => a.id === appId);
      return withTenant(app?.tenantId ?? "", () => api.createAdminModel({ application_id: appId, name: name.trim(), storage_type: "oltp" }));
    },
    onSuccess: () => { inv(); setOpen(""); setName(""); },
  });

  if (own.length === 0) return null;
  const pending = open === "app" ? createApp : createModel;
  const submit = () => { if (name.trim() && (open === "app" || appId)) pending.mutate(); };
  const multiTenant = own.length > 1;

  return (
    <>
      <Toolbar>
        <ToolbarGroup align="end">
          <Button leadingIcon={open === "app" ? undefined : <Plus size={14} />} onClick={() => show("app")}>
            {open === "app" ? "Cancel" : "New application"}
          </Button>
          <Button leadingIcon={open === "model" ? undefined : <Plus size={14} />} disabled={apps.length === 0 && open !== "model"} onClick={() => show("model")}>
            {open === "model" ? "Cancel" : "New model"}
          </Button>
        </ToolbarGroup>
      </Toolbar>
      {open && (
        <div className="mvx-panel" style={{ padding: 16 }}>
          <div style={{ display: "flex", gap: 12, flexWrap: "wrap", alignItems: "flex-end" }}>
            {open === "app" && multiTenant && (
              <Field label="Tenant">
                <Select value={tenantId} onChange={(e) => setTenantId(e.target.value)} style={{ width: 200 }}>
                  {own.map((t) => <option key={t.id} value={t.id}>{t.name}</option>)}
                </Select>
              </Field>
            )}
            {open === "model" && (
              <Field label="Application">
                <Select value={appId} onChange={(e) => setAppId(e.target.value)} style={{ width: 220 }}>
                  {apps.map((a) => <option key={a.id} value={a.id}>{multiTenant ? `${a.tenantName} · ${a.name}` : a.name}</option>)}
                </Select>
              </Field>
            )}
            <Field label={open === "app" ? "Application name" : "Model name"}>
              <TextInput
                value={name}
                onChange={(e) => setName(e.target.value)}
                onKeyDown={(e) => { if (e.key === "Enter") submit(); if (e.key === "Escape") setOpen(""); }}
                placeholder={open === "app" ? "e.g. Finance" : "e.g. Budget 2027"}
                style={{ width: 220 }}
                autoFocus
              />
            </Field>
            <Button variant="primary" disabled={!name.trim() || (open === "model" && !appId)} loading={pending.isPending} loadingLabel="Creating…" onClick={submit}>
              {open === "app" ? "Create application" : "Create model"}
            </Button>
          </div>
          {open === "model" && <p className="mvx-admin-muted" style={{ margin: "10px 0 0" }}>A new model starts empty. Press <strong>New revision</strong> under it to start building.</p>}
          {pending.isError && <p className="mvx-admin-error" role="alert">{(pending.error as Error).message}</p>}
        </div>
      )}
    </>
  );
}

export function DevApplicationsTab({
  revisionId,
  revisionName,
  defaultRevisionPending = false,
  canCreate = false,
  onSelect,
}: {
  revisionId: string;
  revisionName: string;
  /** The console is still asking for the default model's revisions; until it
      answers, revisionId is "" only because the answer has not arrived. */
  defaultRevisionPending?: boolean;
  /** The person holds tenant or platform admin, so may create applications
      and models (in the tenants they administer). */
  canCreate?: boolean;
  onSelect: (id: string, name: string) => void;
}) {
  const { data: tenants = [], isLoading } = useQuery({
    queryKey: ["dev-applications"],
    queryFn: api.getDevApplications,
  });
  const qc = useQueryClient();
  const setDefault = useMutation({
    mutationFn: ({ modelId, tenantId }: { modelId: string; tenantId: string }) =>
      withTenant(tenantId, () => api.setDefaultModel(modelId)),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["dev-applications"] });
      // The default drives what /api/demo and every business list resolve.
      qc.invalidateQueries({ queryKey: ["demo"] });
    },
  });

  const apps = tenants.flatMap((t) => t.applications ?? []);
  // The list spans every database the caller reaches; an action on a row is
  // addressed to the row's tenant.
  const tenantOfApp = new Map(tenants.flatMap((t) => (t.applications ?? []).map((a) => [a.id, t.id] as const)));

  // With no working revision and none in the default model, fall back to the
  // most recently created revision. It waits for the default model's answer:
  // deciding before it arrived picked the newest revision of ANY model, so
  // which model Build (and Triggers) opened in depended on request timing.
  React.useEffect(() => {
    if (revisionId || isLoading || defaultRevisionPending || apps.length === 0) return;
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
  }, [isLoading, defaultRevisionPending]);

  if (isLoading) return <LoadingState />;
  const createBar = canCreate ? <CreateBar tenants={tenants} /> : null;
  if (apps.length === 0) return <div className="mvx-admin-stack">{createBar}<EmptyState label="No applications found." /></div>;

  return (
    <div className="mvx-admin-stack">
      {createBar}

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
                      onClick={() => setDefault.mutate({ modelId: m.id, tenantId: tenantOfApp.get(app.id) ?? "" })}
                    >
                      Set as business default
                    </Button>
                  )}
                </div>
                <DevModelRevisions model={m} appId={app.id} tenantId={tenantOfApp.get(app.id) ?? ""} revisionId={revisionId} onSelect={onSelect} />
              </div>
            ))}
          </div>
        </div>
      ))}
    </div>
  );
}

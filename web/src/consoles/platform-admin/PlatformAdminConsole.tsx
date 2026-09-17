import { useRef, useState } from "react";
import { AuditExportPanel } from "../../ee/auditexport/AuditExportPanel";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Plus, Pencil, Trash2, Download, Upload, Package, Braces } from "lucide-react";
import { api, withTenant, type AdminTenant, type AdminModel, type AdminApp, type AdminAuditEvent, type AdminRevision, type ModelExportPackage } from "../../api/client";
import { downloadJSON } from "../../api/download";
import {
  Button,
  IconButton,
  TextInput,
  Select,
  DataTable,
  type DataTableColumn,
  StatusBadge,
  EmptyState,
  SearchInput,
  Toolbar,
  ToolbarGroup,
  useConfirm,
  type DesignTone,
} from "../../ui";

// ── Model Revisions Section ────────────────────────────────────────────────────

function ModelRevisionsSection({ model, tenantId, isTenantAdmin }: { model: AdminModel; tenantId: string; isTenantAdmin: boolean }) {
  const qc = useQueryClient();
  const [addRevision, setAddRevision] = useState(false);
  const [revisionName, setRevisionName] = useState("");

  const inv = () => qc.invalidateQueries({ queryKey: ["admin-tenants"] });
  // Export of ONE specific revision (the model-level button exports the
  // active one). The server accepts ?revision_id= for exactly this; without
  // a per-revision button here a tenant admin had no way to export a
  // scenario that isn't the active revision.
  // Two flavours: with data (entered values + form records) or definitions
  // only — the structure without the numbers, for handing a model to
  // another tenant.
  const exportRevision = useMutation({
    mutationFn: async ({ s, includeData }: { s: AdminRevision; includeData: boolean }) => {
      const pkg = await withTenant(tenantId, () => api.exportModel(model.id, s.id, includeData));
      downloadJSON(`${model.name}-${pkg.revision_name || s.name}${includeData ? "" : "-definitions"}.mavericks-model.json`, pkg);
    },
  });

  const createRevision = useMutation({
    mutationFn: () => withTenant(tenantId, () => api.createAdminRevision({ model_id: model.id, name: revisionName })),
    onSuccess: () => { inv(); setAddRevision(false); setRevisionName(""); },
  });
  const deleteRevision = useMutation({
    mutationFn: (id: string) => withTenant(tenantId, () => api.deleteAdminRevision(id)),
    onSuccess: inv,
  });
  const { confirm, confirmElement } = useConfirm();
  const setActive = useMutation({
    mutationFn: (revisionName: string) => withTenant(tenantId, () => api.setActiveRevision(model.id, {
      revision_name: revisionName,
    })),
    onSuccess: inv,
  });

  return (
    <div className="mvx-admin-revisions">
      <div className="mvx-admin-revisions__label">Revisions</div>
      {(model.revisions ?? []).length === 0 && (
        <p className="mvx-admin-muted">No revisions yet.</p>
      )}
      <div className="mvx-admin-revisions__list">
        {(model.revisions ?? []).map((s: AdminRevision) => {
          const isActive = s.name === model.active_revision;
          return (
            <div key={s.id} className={["mvx-admin-revision", isActive ? "mvx-admin-revision--active" : ""].filter(Boolean).join(" ")}>
              <div className="mvx-admin-revision__info">
                <div className="mvx-admin-revision__name">{s.name}</div>
                <div className="mvx-admin-revision__meta">
                  {s.id.slice(0, 8)} · {s.created_at ? s.created_at.slice(0, 16).replace("T", " ") : ""}
                </div>
              </div>
              {isTenantAdmin && (
                <>
                  <IconButton
                    aria-label={`Export revision ${s.name} of ${model.name} with data`}
                    title={`Export this revision with data (${s.name}: values and form records)`}
                    size={26}
                    onClick={() => exportRevision.mutate({ s, includeData: true })}
                    disabled={exportRevision.isPending}
                  >
                    <Download size={13} />
                  </IconButton>
                  <IconButton
                    aria-label={`Export revision ${s.name} of ${model.name} definitions only`}
                    title={`Export this revision, definitions only (${s.name}: no values, no form records)`}
                    size={26}
                    onClick={() => exportRevision.mutate({ s, includeData: false })}
                    disabled={exportRevision.isPending}
                  >
                    <Braces size={13} />
                  </IconButton>
                </>
              )}
              {isActive ? (
                <StatusBadge tone="live">Active</StatusBadge>
              ) : (
                <>
                  <Button
                    size="sm"
                    onClick={() => setActive.mutate(s.name)}
                    disabled={setActive.isPending}
                  >
                    Set active
                  </Button>
                  <IconButton
                    aria-label={`Delete revision ${s.name}`}
                    title="Delete revision"
                    danger
                    size={26}
                    onClick={() => confirm({ title: "Delete revision?", body: `This permanently removes "${s.name}". This cannot be undone.`, confirmLabel: "Delete revision", onConfirm: () => deleteRevision.mutate(s.id) })}
                  >
                    <Trash2 size={13} />
                  </IconButton>
                </>
              )}
            </div>
          );
        })}
      </div>
      {addRevision ? (
        <div className="mvx-admin-inline-form">
          <TextInput
            value={revisionName}
            onChange={e => setRevisionName(e.target.value)}
            placeholder="Revision name (e.g. FY2027 Budget)"
            style={{ width: 240 }}
            autoFocus
          />
          <Button
            variant="primary"
            size="sm"
            onClick={() => createRevision.mutate()}
            disabled={!revisionName}
            loading={createRevision.isPending}
            loadingLabel="Adding…"
          >
            Add
          </Button>
          <Button size="sm" onClick={() => setAddRevision(false)}>Cancel</Button>
        </div>
      ) : (
        <Button size="sm" variant="ghost" leadingIcon={<Plus size={13} />} onClick={() => setAddRevision(true)}>
          New revision
        </Button>
      )}
      {confirmElement}
    </div>
  );
}

// ── App Section ────────────────────────────────────────────────────────────────

function AppSection({ app, tenantId, onDelete, isTenantAdmin }: { app: AdminApp; tenantId: string; onDelete: () => void; isTenantAdmin: boolean }) {
  const qc = useQueryClient();
  const [addModel, setAddModel] = useState(false);
  const [modelName, setModelName] = useState("");
  const [importError, setImportError] = useState<string | null>(null);
  const importFileRef = useRef<HTMLInputElement>(null);

  const inv = () => qc.invalidateQueries({ queryKey: ["admin-tenants"] });

  const createModel = useMutation({
    mutationFn: () => withTenant(tenantId, () => api.createAdminModel({ application_id: app.id, name: modelName, storage_type: "oltp" })),
    onSuccess: () => { inv(); setAddModel(false); setModelName(""); },
  });
  const deleteModel = useMutation({
    mutationFn: (id: string) => withTenant(tenantId, () => api.deleteAdminModel(id)),
    onSuccess: inv,
  });
  // Export/import is tenant_admin only — the server rejects everyone else,
  // the buttons are simply hidden for other roles.
  const exportModel = useMutation({
    mutationFn: async (m: AdminModel) => {
      const pkg = await withTenant(tenantId, () => api.exportModel(m.id));
      downloadJSON(`${m.name}-${pkg.revision_name || "export"}.mavericks-model.json`, pkg);
    },
  });
  // The standalone tar.gz package (definitions + facts + full migrations +
  // manifest + infra-only compose) — distinct from exportModel's plain JSON
  // above, which carries only the entity graph.
  const exportModelPackage = useMutation({
    mutationFn: async (m: AdminModel) => {
      const { blob, filename } = await withTenant(tenantId, () => api.exportModelPackage(m.id));
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = filename;
      a.click();
      URL.revokeObjectURL(url);
    },
  });
  const importModel = useMutation({
    mutationFn: async (file: File) => {
      const pkg = JSON.parse(await file.text()) as ModelExportPackage;
      if (pkg.format !== "mavericks-model-export") throw new Error("Not a Mavericks model export file");
      return withTenant(tenantId, () => api.importModel({ application_id: app.id, package: pkg }));
    },
    onSuccess: () => { setImportError(null); inv(); },
    onError: (e) => setImportError((e as Error).message),
  });
  const { confirm, confirmElement } = useConfirm();

  return (
    <div className="mvx-admin-object">
      <div className="mvx-admin-object__header">
        <div className="mvx-admin-avatar mvx-admin-avatar--app">{app.name[0]}</div>
        <div className="mvx-admin-object__title">
          <div className="mvx-admin-object__name">{app.name}</div>
          <div className="mvx-admin-object__meta">{app.models.length} model{app.models.length !== 1 ? "s" : ""}</div>
        </div>
        <IconButton
          aria-label={`Delete application ${app.name}`}
          title="Delete application"
          danger
          onClick={() => confirm({ title: "Delete application?", body: `This removes "${app.name}" and all its models. This cannot be undone.`, confirmLabel: "Delete application", onConfirm: onDelete })}
        >
          <Trash2 size={14} />
        </IconButton>
      </div>

      <div className="mvx-admin-object__body">
        {app.models.length === 0 && <p className="mvx-admin-muted">No models yet.</p>}
        {app.models.map(m => (
          <div key={m.id} className="mvx-admin-model">
            <div className="mvx-admin-model__header">
              <span className="mvx-admin-model__name">{m.name}</span>
              {isTenantAdmin && (
                <IconButton
                  aria-label={`Export model ${m.name}`}
                  title="Export model (active revision)"
                  size={26}
                  onClick={() => exportModel.mutate(m)}
                >
                  <Download size={13} />
                </IconButton>
              )}
              {isTenantAdmin && (
                <IconButton
                  aria-label={`Download deployment package for ${m.name}`}
                  title="Download standalone deployment package (data + migrations + manifest)"
                  size={26}
                  onClick={() => exportModelPackage.mutate(m)}
                >
                  <Package size={13} />
                </IconButton>
              )}
              <IconButton
                aria-label={`Delete model ${m.name}`}
                title="Delete model"
                danger
                size={26}
                onClick={() => confirm({ title: "Delete model?", body: `This removes "${m.name}" and all its revisions.`, confirmLabel: "Delete model", onConfirm: () => deleteModel.mutate(m.id) })}
              >
                <Trash2 size={13} />
              </IconButton>
            </div>
            <ModelRevisionsSection model={m} tenantId={tenantId} isTenantAdmin={isTenantAdmin} />
          </div>
        ))}

        {addModel ? (
          <div className="mvx-admin-inline-form">
            <TextInput
              value={modelName}
              onChange={e => setModelName(e.target.value)}
              placeholder="Model name"
              style={{ width: 220 }}
              autoFocus
            />
            <Button
              variant="primary"
              size="sm"
              onClick={() => createModel.mutate()}
              disabled={!modelName}
              loading={createModel.isPending}
              loadingLabel="Adding…"
            >
              Add model
            </Button>
            <Button size="sm" onClick={() => setAddModel(false)}>Cancel</Button>
          </div>
        ) : (
          <div style={{ display: "flex", gap: 8, alignSelf: "flex-start", alignItems: "center" }}>
            <Button size="sm" variant="ghost" leadingIcon={<Plus size={13} />} onClick={() => { setAddModel(true); setModelName(""); }}>
              Add model
            </Button>
            {isTenantAdmin && (
              <>
                <Button
                  size="sm"
                  variant="ghost"
                  leadingIcon={<Upload size={13} />}
                  onClick={() => importFileRef.current?.click()}
                  loading={importModel.isPending}
                  loadingLabel="Importing…"
                >
                  Import model
                </Button>
                <input
                  ref={importFileRef}
                  type="file"
                  accept="application/json,.json"
                  style={{ display: "none" }}
                  onChange={e => {
                    const file = e.target.files?.[0];
                    e.target.value = "";
                    if (file) importModel.mutate(file);
                  }}
                />
              </>
            )}
          </div>
        )}
        {importError && <p className="mvx-admin-muted" role="alert">Import failed: {importError}</p>}
      </div>
      {confirmElement}
    </div>
  );
}

// ── Tenant Section ─────────────────────────────────────────────────────────────

// planLabel is the tenant's plan as the card's meta line says it: the plan's
// name, the trial and its days, and whether the tenant is read-only.
function planLabel(tenant: AdminTenant): string {
  const st = tenant.plan_state;
  if (!st) return tenant.plan;
  const parts = [st.plan_known ? st.plan.name : `${tenant.plan} (no such plan)`];
  if (st.trial) parts.push(st.read_only && st.code === "trial_expired" ? "trial ended" : `trial, ${st.days_left} day${st.days_left === 1 ? "" : "s"} left`);
  if (st.read_only) parts.push("read-only");
  return parts.join(" · ");
}

/**
 * The platform admin's plan controls on a tenant card: move the tenant to
 * another plan (a trial plan starts its clock), or set the trial's end
 * date directly — an extension, or an early end with an empty date.
 */
function TenantPlanControls({ tenant }: { tenant: AdminTenant }) {
  const qc = useQueryClient();
  const { data: plans } = useQuery({ queryKey: ["admin-plans"], queryFn: api.getPlans });
  const [open, setOpen] = useState(false);
  const [plan, setPlan] = useState(tenant.plan);
  const [ends, setEnds] = useState(tenant.plan_state?.trial_ends_at ? tenant.plan_state.trial_ends_at.slice(0, 10) : "");
  const save = useMutation({
    mutationFn: () => {
      const body: { plan?: string; trial_ends_at?: string } = {};
      if (plan !== tenant.plan) body.plan = plan;
      const current = tenant.plan_state?.trial_ends_at ? tenant.plan_state.trial_ends_at.slice(0, 10) : "";
      if (ends !== current) body.trial_ends_at = ends ? new Date(`${ends}T23:59:59Z`).toISOString() : "";
      return api.updateAdminTenant(tenant.id, body);
    },
    onSuccess: () => { void qc.invalidateQueries({ queryKey: ["admin-tenants"] }); setOpen(false); },
  });
  if (!open) {
    return (
      <Button size="sm" variant="ghost" onClick={() => setOpen(true)} style={{ alignSelf: "flex-start" }} aria-label={`Change plan of ${tenant.name}`}>
        Change plan
      </Button>
    );
  }
  return (
    <div className="mvx-admin-inline-form mvx-admin-inline-form--boxed" data-testid={`tenant-plan-${tenant.id}`}>
      <Select value={plan} onChange={(e) => setPlan(e.target.value)} style={{ width: 160 }} aria-label="Plan">
        {(plans ?? []).map((p) => <option key={p.key} value={p.key}>{p.name}</option>)}
        {plans && !plans.some((p) => p.key === tenant.plan) && <option value={tenant.plan}>{tenant.plan}</option>}
      </Select>
      <label className="mvx-admin-muted" style={{ display: "flex", alignItems: "center", gap: 6 }}>
        Trial ends
        <TextInput type="date" value={ends} onChange={(e) => setEnds(e.target.value)} aria-label="Trial ends" style={{ width: 160 }} />
      </label>
      <Button variant="primary" size="sm" loading={save.isPending} loadingLabel="Saving…" onClick={() => save.mutate()}>Save</Button>
      <Button size="sm" onClick={() => setOpen(false)}>Cancel</Button>
      {save.isError && <span className="mvx-admin-error">{(save.error as Error).message}</span>}
    </div>
  );
}

function TenantSection({ tenant, onDelete, isPlatformAdmin, isTenantAdmin }: { tenant: AdminTenant; onDelete: () => void; isPlatformAdmin: boolean; isTenantAdmin: boolean }) {
  const qc = useQueryClient();
  const [addApp, setAddApp] = useState(false);
  const [appName, setAppName] = useState("");
  const [editing, setEditing] = useState(false);
  const [editName, setEditName] = useState(tenant.name);

  const inv = () => qc.invalidateQueries({ queryKey: ["admin-tenants"] });

  const createApp = useMutation({
    mutationFn: () => api.createAdminApplication({ customer_id: tenant.id, name: appName, mode: "planning" }),
    onSuccess: () => { inv(); setAddApp(false); setAppName(""); },
  });
  const deleteApp = useMutation({
    mutationFn: (id: string) => api.deleteAdminApplication(id),
    onSuccess: inv,
  });
  const renameTenant = useMutation({
    mutationFn: () => api.updateAdminTenant(tenant.id, { name: editName }),
    onSuccess: () => { inv(); setEditing(false); },
  });
  const { confirm, confirmElement } = useConfirm();

  return (
    <div className="mvx-admin-object mvx-admin-object--tenant">
      <div className="mvx-admin-object__header mvx-admin-object__header--tenant">
        <div className="mvx-admin-avatar mvx-admin-avatar--tenant">{tenant.name[0].toUpperCase()}</div>
        {editing ? (
          <div className="mvx-admin-inline-form" style={{ flex: 1 }}>
            <TextInput
              value={editName}
              onChange={e => setEditName(e.target.value)}
              style={{ width: 240 }}
              autoFocus
              onKeyDown={e => { if (e.key === "Enter") renameTenant.mutate(); if (e.key === "Escape") setEditing(false); }}
            />
            <Button
              variant="primary"
              size="sm"
              onClick={() => renameTenant.mutate()}
              disabled={!editName}
              loading={renameTenant.isPending}
              loadingLabel="Saving…"
            >
              Save
            </Button>
            <Button size="sm" onClick={() => setEditing(false)}>Cancel</Button>
          </div>
        ) : (
          <div className="mvx-admin-object__title">
            <div className="mvx-admin-object__name">{tenant.name}</div>
            <div className="mvx-admin-object__meta" data-testid={`tenant-meta-${tenant.id}`}>
              {planLabel(tenant)} · {(tenant.applications ?? []).length} app{(tenant.applications ?? []).length !== 1 ? "s" : ""}
            </div>
          </div>
        )}
        {!editing && (
          <>
            <IconButton
              aria-label={`Rename tenant ${tenant.name}`}
              title="Rename tenant"
              onClick={() => { setEditing(true); setEditName(tenant.name); }}
            >
              <Pencil size={14} />
            </IconButton>
            {isPlatformAdmin && (
              <IconButton
                aria-label={`Delete tenant ${tenant.name}`}
                title="Delete tenant"
                danger
                onClick={() => confirm({ title: "Delete tenant?", body: `This removes "${tenant.name}" and all its applications. This cannot be undone.`, confirmLabel: "Delete tenant", onConfirm: onDelete })}
              >
                <Trash2 size={14} />
              </IconButton>
            )}
          </>
        )}
      </div>

      <div className="mvx-admin-object__body">
        {isPlatformAdmin && <TenantPlanControls key={`${tenant.plan}-${tenant.plan_state?.trial_ends_at ?? ""}`} tenant={tenant} />}
        {(tenant.applications ?? []).length === 0 && (
          <p className="mvx-admin-muted">No applications yet.</p>
        )}
        {(tenant.applications ?? []).map(app => (
          <AppSection key={app.id} app={app} tenantId={tenant.id} onDelete={() => deleteApp.mutate(app.id)} isTenantAdmin={isTenantAdmin} />
        ))}
        {deleteApp.isError && <p className="mvx-admin-error">{(deleteApp.error as Error).message}</p>}

        {addApp ? (
          <div className="mvx-admin-inline-form mvx-admin-inline-form--boxed">
            <TextInput
              value={appName}
              onChange={e => setAppName(e.target.value)}
              placeholder="Application name"
              style={{ width: 240 }}
              autoFocus
            />
            <Button
              variant="primary"
              size="sm"
              onClick={() => createApp.mutate()}
              disabled={!appName}
              loading={createApp.isPending}
              loadingLabel="Creating…"
            >
              Create
            </Button>
            <Button size="sm" onClick={() => setAddApp(false)}>Cancel</Button>
            {createApp.isError && <span className="mvx-admin-error">{(createApp.error as Error).message}</span>}
          </div>
        ) : (
          <Button size="sm" variant="ghost" leadingIcon={<Plus size={13} />} onClick={() => { setAddApp(true); setAppName(""); }} style={{ alignSelf: "flex-start" }}>
            New application
          </Button>
        )}
      </div>
      {confirmElement}
    </div>
  );
}

// ── Applications View ─────────────────────────────────────────────────────────

export function ApplicationsView({ tenants, isPlatformAdmin, isTenantAdmin }: { tenants: AdminTenant[]; isPlatformAdmin: boolean; isTenantAdmin: boolean }) {
  const qc = useQueryClient();
  const [addTenant, setAddTenant] = useState(false);
  const [tenantName, setTenantName] = useState("");
  const [tenantPlan, setTenantPlan] = useState("standard");
  // The plans a new tenant can start on come from the catalog (Platform › Plans).
  const { data: plans } = useQuery({ queryKey: ["admin-plans"], queryFn: api.getPlans, enabled: isPlatformAdmin });

  const inv = () => qc.invalidateQueries({ queryKey: ["admin-tenants"] });

  const createTenant = useMutation({
    mutationFn: () => api.createAdminTenant({ name: tenantName, plan: tenantPlan }),
    onSuccess: () => { inv(); setAddTenant(false); setTenantName(""); setTenantPlan("standard"); },
  });
  const deleteTenant = useMutation({
    mutationFn: (id: string) => api.deleteAdminTenant(id),
    onSuccess: inv,
  });

  return (
    <div className="mvx-admin-stack">
      {tenants.length === 0 && !addTenant && (
        <EmptyState
          label="No tenants yet. Create the first one."
          action={isPlatformAdmin ? "New tenant" : undefined}
          onAction={isPlatformAdmin ? () => { setAddTenant(true); setTenantName(""); } : undefined}
        />
      )}

      {tenants.map(tenant => (
        <TenantSection key={tenant.id} tenant={tenant} onDelete={() => deleteTenant.mutate(tenant.id)} isPlatformAdmin={isPlatformAdmin} isTenantAdmin={isTenantAdmin} />
      ))}

      {isPlatformAdmin && addTenant ? (
        <div className="mvx-admin-inline-form mvx-admin-inline-form--boxed">
          <TextInput
            value={tenantName}
            onChange={e => setTenantName(e.target.value)}
            placeholder="Tenant name (e.g. Acme Corp)"
            style={{ width: 280 }}
            autoFocus
            onKeyDown={e => { if (e.key === "Enter" && tenantName) createTenant.mutate(); }}
          />
          <Select value={tenantPlan} onChange={e => setTenantPlan(e.target.value)} style={{ width: 160 }} aria-label="Plan of the new tenant">
            {(plans ?? [{ key: "standard", name: "Standard" }]).map(p => <option key={p.key} value={p.key}>{p.name}</option>)}
          </Select>
          <Button
            variant="primary"
            onClick={() => createTenant.mutate()}
            disabled={!tenantName}
            loading={createTenant.isPending}
            loadingLabel="Creating…"
          >
            Create tenant
          </Button>
          <Button onClick={() => setAddTenant(false)}>Cancel</Button>
          {createTenant.isError && <span className="mvx-admin-error">{(createTenant.error as Error).message}</span>}
        </div>
      ) : isPlatformAdmin && tenants.length > 0 ? (
        <Button leadingIcon={<Plus size={14} />} onClick={() => { setAddTenant(true); setTenantName(""); }} style={{ alignSelf: "flex-start" }}>
          New tenant
        </Button>
      ) : null}
    </div>
  );
}

// ── Audit Log ─────────────────────────────────────────────────────────────────

const CATEGORY_TONE: Record<string, DesignTone> = {
  data_change:   "info",
  model_change:  "success",
  admin:         "brand",
  auth:          "warning",
  policy_change: "danger",
  ai_assistant:  "info",
};

const AUDIT_COLUMNS: DataTableColumn<AdminAuditEvent>[] = [
  {
    id: "time",
    header: "Time",
    cell: (e) => (
      <span className="mvx-admin-mono mvx-admin-muted" style={{ whiteSpace: "nowrap" }}>
        {e.occurred_at.slice(0, 19).replace("T", " ")}
      </span>
    ),
  },
  {
    id: "category",
    header: "Category",
    cell: (e) => (
      <StatusBadge tone={CATEGORY_TONE[e.category] ?? "neutral"}>{e.category.replace("_", " ")}</StatusBadge>
    ),
  },
  { id: "event", header: "Event", cell: (e) => <span className="mvx-admin-mono">{e.event_type}</span> },
  {
    id: "actor",
    header: "Actor",
    cell: (e) => (
      <>
        <div style={{ fontWeight: 500 }}>{e.actor_name}</div>
        {e.actor_role && <div className="mvx-admin-muted">{e.actor_role}</div>}
      </>
    ),
  },
  {
    id: "resource",
    header: "Resource",
    cell: (e) => e.resource_type ? (
      <>
        <span>{e.resource_type}</span>
        <div className="mvx-admin-mono mvx-admin-muted">{e.resource_id.slice(0, 8)}</div>
      </>
    ) : "—",
  },
  {
    id: "application",
    header: "Application",
    cell: (e) => e.application_name ? <StatusBadge tone="neutral">{e.application_name}</StatusBadge> : "—",
  },
  {
    id: "revision",
    header: "Revision",
    cell: (e) => e.revision_name ? <StatusBadge tone="draft">{e.revision_name}</StatusBadge> : "—",
  },
  {
    id: "details",
    header: "Details",
    cell: (e) => (
      <span className="mvx-admin-mono mvx-admin-muted" style={{ maxWidth: 280, wordBreak: "break-all", display: "inline-block" }}>
        {e.metadata && Object.keys(e.metadata).length > 0 ? JSON.stringify(e.metadata) : "—"}
      </span>
    ),
  },
];

export function AuditView({ events }: { events: AdminAuditEvent[] }) {
  const [filter, setFilter] = useState("");
  const filtered = filter
    ? events.filter((e) =>
        e.event_type.includes(filter) || e.category.includes(filter) ||
        e.actor_name.toLowerCase().includes(filter.toLowerCase()) || e.resource_type.includes(filter) ||
        (e.application_name ?? "").toLowerCase().includes(filter.toLowerCase()) ||
        (e.revision_name ?? "").toLowerCase().includes(filter.toLowerCase()) ||
        (e.revision_id ?? "").startsWith(filter)
      )
    : events;

  return (
    <div>
      <AuditExportPanel />
      <Toolbar>
        <ToolbarGroup>
          <SearchInput
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            placeholder="Filter by event, actor, or resource…"
            width={360}
          />
          {filter && <span className="mvx-admin-muted">{filtered.length} of {events.length} events</span>}
        </ToolbarGroup>
      </Toolbar>
      <DataTable
        columns={AUDIT_COLUMNS}
        rows={filtered}
        getRowKey={(e) => e.id}
        density="compact"
        emptyTitle="No events match the filter."
      />
    </div>
  );
}

// ── Main Console ───────────────────────────────────────────────────────────────

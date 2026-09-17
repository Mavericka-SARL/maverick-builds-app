import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Pencil, Plus, Trash2, X } from "lucide-react";
import { api, type AdminTenant, type AdminUser, type AdminWorkspace, type UserAssignment } from "../../api/client";
import {
  Badge,
  Button,
  Checkbox,
  Field,
  IconButton,
  RoleBadge,
  Select,
  StatusBadge,
  TextInput,
  useConfirm,
  type DesignTone,
} from "../../ui";

const ROLE_TONE: Record<string, DesignTone> = {
  platform_admin: "danger",
  tenant_admin: "info",
  developer: "brand",
  business_admin: "warning",
  business_user: "success",
};

const PLATFORM_ROLES = ["platform_admin", "developer", "tenant_admin"] as const;
const WORKSPACE_ROLES = ["tenant_admin", "developer", "business_admin", "business_user"] as const;

/**
 * The roles that gate the user-administration screens — mirrors the userAdm
 * route gate in internal/gateway/handler.go. Losing your last one of these is
 * what makes self-service role removal a one-way door.
 */
const ADMIN_ROLES: string[] = ["platform_admin", "tenant_admin", "developer"];

/**
 * Roles that are inert unless scoped to a workspace: actorCanAccessApp
 * resolves a business user's reach by joining role_assignment.workspace_id to
 * the application's workspace, so a NULL-workspace grant matches nothing.
 */
const BUSINESS_ROLES: string[] = ["business_admin", "business_user"];

function assignmentKey(a: UserAssignment) {
  return `${a.role}::${a.workspace_id}`;
}

/**
 * Whether removing this grant would leave the signed-in admin unable to reach
 * user administration at all — in which case nothing in the product can give
 * it back, and another administrator has to. The server refuses these anyway
 * (see the roles DELETE branch in internal/gateway/handler.go); this is so the
 * control explains itself instead of failing on click.
 */
function isLastOwnAdminGrant(u: AdminUser, a: UserAssignment, currentUserId?: string) {
  if (!currentUserId || u.id !== currentUserId) return false;
  if (!ADMIN_ROLES.includes(a.role)) return false;
  const key = assignmentKey(a);
  return !(u.assignments ?? []).some(other => ADMIN_ROLES.includes(other.role) && assignmentKey(other) !== key);
}

function RemovableRoleChip({
  role,
  label,
  onRemove,
  removing,
  blockedReason,
}: {
  role: string;
  label: string;
  onRemove?: () => void;
  removing?: boolean;
  /** Shown on a disabled ✕ instead of hiding it, when the reason is worth stating. */
  blockedReason?: string;
}) {
  return (
    <Badge color={ROLE_TONE[role] ?? "neutral"}>
      {label}
      {onRemove && (
        <button
          type="button"
          className="mvx-badge__remove"
          onClick={blockedReason ? undefined : onRemove}
          disabled={removing || !!blockedReason}
          title={blockedReason ?? `Remove ${role}`}
          aria-label={blockedReason ?? `Remove ${role}`}
        >
          <X size={12} aria-hidden="true" />
        </button>
      )}
    </Badge>
  );
}

export function UsersPanel({
  users,
  tenants,
  assignableRoles = [],
  canManageResourceAccess = false,
  currentUserId,
}: {
  users: AdminUser[];
  tenants: AdminTenant[];
  /** Role values this actor may grant/revoke — see computeAssignableRoles(). */
  assignableRoles?: string[];
  /**
   * Whether this actor may grant application/model access and
   * workspace-scoped roles. Developers may administer users but not decide
   * who reaches which application or model. Mirrors canManageResourceAccess()
   * in internal/gateway/handler.go, which is the actual boundary — this only
   * avoids showing controls whose every request would be refused.
   */
  canManageResourceAccess?: boolean;
  /**
   * The signed-in admin's own user id, so their own row can refuse the two
   * controls that would end their own access — see isLastOwnAdminGrant().
   */
  currentUserId?: string;
}) {
  const qc = useQueryClient();
  const [showCreate, setShowCreate] = useState(false);
  const [newUser, setNewUser] = useState({ email: "", first_name: "", last_name: "", role: "", workspace_id: "" });
  const [editId, setEditId] = useState<string | null>(null);
  const [editUser, setEditUser] = useState({ email: "", display_name: "" });
  const [addPlatformRole, setAddPlatformRole] = useState("");
  const [addWsRole, setAddWsRole] = useState("business_user");
  const [addWsId, setAddWsId] = useState("");
  const [addWsCustomerId, setAddWsCustomerId] = useState<string | null>(null);

  const { data: workspaces = [] } = useQuery<AdminWorkspace[]>({
    queryKey: ["admin-workspaces"],
    queryFn: api.getAdminWorkspaces,
  });

  const modelNames = useMemo(() => {
    const map = new Map<string, string>();
    for (const t of tenants) for (const app of t.applications) for (const m of app.models) map.set(m.id, m.name);
    return map;
  }, [tenants]);
  const appNames = useMemo(() => {
    const map = new Map<string, string>();
    for (const t of tenants) for (const app of t.applications) map.set(app.id, app.name);
    return map;
  }, [tenants]);

  const inv = () => {
    qc.invalidateQueries({ queryKey: ["admin-users"] });
    qc.invalidateQueries({ queryKey: ["admin-workspaces"] });
    // New/changed users must show up in the dev persona switcher right away.
    qc.invalidateQueries({ queryKey: ["dev-personas"] });
  };

  const createUser = useMutation({
    mutationFn: () => api.createAdminUser(newUser),
    onSuccess: () => { inv(); setShowCreate(false); setNewUser({ email: "", first_name: "", last_name: "", role: "", workspace_id: "" }); },
  });
  const updateUser = useMutation({
    mutationFn: () => api.updateAdminUser(editId!, editUser),
    onSuccess: () => { inv(); setEditId(null); },
  });
  const addRole = useMutation({
    mutationFn: ({ userId, role, workspaceId }: { userId: string; role: string; workspaceId?: string }) =>
      api.addAdminUserRole(userId, role, workspaceId),
    onSuccess: () => { inv(); setAddPlatformRole(""); setAddWsId(""); setAddWsCustomerId(null); },
  });
  const removeRole = useMutation({
    mutationFn: ({ userId, role, workspaceId }: { userId: string; role: string; workspaceId?: string }) =>
      api.removeAdminUserRole(userId, role, workspaceId),
    onSuccess: inv,
  });
  const deleteUser = useMutation({
    mutationFn: (id: string) => api.deleteAdminUser(id),
    onSuccess: inv,
  });
  const grantAppAccess = useMutation({
    mutationFn: ({ userId, appId }: { userId: string; appId: string }) => api.grantUserAppAccess(userId, appId),
    onSuccess: inv,
  });
  const revokeAppAccess = useMutation({
    mutationFn: ({ userId, appId }: { userId: string; appId: string }) => api.revokeUserAppAccess(userId, appId),
    onSuccess: inv,
  });
  const grantModelAccess = useMutation({
    mutationFn: ({ userId, modelId }: { userId: string; modelId: string }) => api.grantUserModelAccess(userId, modelId),
    onSuccess: inv,
  });
  const revokeModelAccess = useMutation({
    mutationFn: ({ userId, modelId }: { userId: string; modelId: string }) => api.revokeUserModelAccess(userId, modelId),
    onSuccess: inv,
  });
  const { confirm, confirmElement } = useConfirm();

  // A developer may place people into workspaces with a business role, though
  // not decide which applications or models they reach. Derived from the
  // assignable set rather than passed in: whoever can grant a business role at
  // all is exactly who should be able to scope one.
  const canGrantWorkspaceRoles = canManageResourceAccess || assignableRoles.some(r => BUSINESS_ROLES.includes(r));

  return (
    <div>
      <div style={{ marginBottom: 16 }}>
        <Button
          leadingIcon={showCreate ? undefined : <Plus size={14} />}
          onClick={() => setShowCreate((v) => !v)}
          style={{ marginBottom: showCreate ? 16 : 0 }}
        >
          {showCreate ? "Cancel" : "Invite user"}
        </Button>
        {showCreate && (
          <div className="mvx-panel" style={{ padding: 16, maxWidth: 640 }}>
            <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 10, marginBottom: 12 }}>
              <Field label="Email">
                <TextInput value={newUser.email} onChange={e => setNewUser(u => ({ ...u, email: e.target.value }))}
                  placeholder="user@acme.com" />
              </Field>
              {/* Both names, not one "display name": Keycloak's realm marks
                  first and last mandatory, so an account missing either meets
                  the invited user with an "Update Account Information" form
                  before they can finish signing in — asking them for something
                  whoever invited them already knew. */}
              <Field label="First name">
                <TextInput value={newUser.first_name} onChange={e => setNewUser(u => ({ ...u, first_name: e.target.value }))}
                  placeholder="Jane" />
              </Field>
              <Field label="Last name">
                <TextInput value={newUser.last_name} onChange={e => setNewUser(u => ({ ...u, last_name: e.target.value }))}
                  placeholder="Smith" />
              </Field>
              {assignableRoles.length > 0 && (
                <Field label="Initial Role">
                  <Select
                    value={newUser.role}
                    onChange={e => setNewUser(u => ({
                      ...u,
                      role: e.target.value,
                      // A workspace only means something for a business role.
                      workspace_id: BUSINESS_ROLES.includes(e.target.value) ? u.workspace_id : "",
                    }))}
                  >
                    <option value="">None</option>
                    {/* Every role this actor may assign, not just the platform
                        tiers. A developer's whole assignable set is the
                        business roles, so filtering to PLATFORM_ROLES left
                        them an empty dropdown and no way to give an invited
                        user any role at all. */}
                    {assignableRoles.map((role) => <option key={role} value={role}>{role.replace(/_/g, " ")}</option>)}
                  </Select>
                </Field>
              )}
              {BUSINESS_ROLES.includes(newUser.role) && (
                <Field
                  label="Workspace"
                  description="a business role grants nothing until it is scoped to a workspace"
                >
                  <Select value={newUser.workspace_id} onChange={e => setNewUser(u => ({ ...u, workspace_id: e.target.value }))}>
                    <option value="">Select a workspace…</option>
                    {workspaces.map(w => (
                      <option key={w.id} value={w.id}>{w.customer_name} — {w.name}</option>
                    ))}
                  </Select>
                </Field>
              )}
            </div>
            <Button
              variant="primary"
              onClick={() => createUser.mutate()}
              disabled={!newUser.email || !newUser.first_name || !newUser.last_name ||
                (BUSINESS_ROLES.includes(newUser.role) && !newUser.workspace_id)}
              loading={createUser.isPending}
              loadingLabel="Creating…"
            >
              Create user
            </Button>
            {createUser.isError && <p className="mvx-admin-error" style={{ marginTop: 8 }}>{(createUser.error as Error).message}</p>}
          </div>
        )}
      </div>

      <div className="mvx-table-wrap">
        <table className="mvx-table">
          <thead>
            <tr>
              <th>Name</th>
              <th>Email</th>
              <th>Role</th>
              <th>Access</th>
              <th>Joined</th>
              <th style={{ width: 80 }}>Actions</th>
            </tr>
          </thead>
          <tbody>
            {users.map((u) => editId === u.id ? (
              <tr key={u.id}>
                <td colSpan={6} style={{ padding: "16px 20px", background: "var(--color-surface-subtle)" }}>
                  <div className="mvx-admin-inline-form" style={{ marginBottom: 16 }}>
                    {/* Read-only: the address is also the sign-in identity in
                        the identity provider, and editing it here would change
                        only this side. The account would keep signing in under
                        the old address while the app displayed the new one,
                        and invitations would go to whichever the provider
                        still holds. Changing someone's address means
                        re-inviting them. */}
                    <TextInput value={editUser.email} readOnly disabled
                      aria-label="Email (cannot be changed)"
                      title="Email is the sign-in identity and cannot be changed here"
                      style={{ width: 220 }} />
                    <TextInput value={editUser.display_name} onChange={e => setEditUser(x => ({ ...x, display_name: e.target.value }))}
                      placeholder="display name" style={{ width: 180 }} />
                    <Button variant="primary" onClick={() => updateUser.mutate()} loading={updateUser.isPending} loadingLabel="Saving…">Save</Button>
                    <Button onClick={() => setEditId(null)}>Cancel</Button>
                  </div>

                  <div style={{ marginBottom: 14 }}>
                    <div className="mvx-admin-revisions__label" style={{ marginBottom: 6 }}>
                      Platform roles
                    </div>
                    <div style={{ display: "flex", gap: 6, flexWrap: "wrap", alignItems: "center" }}>
                      {(u.assignments ?? []).filter(a => a.workspace_id === "").map(a => (
                        <RemovableRoleChip
                          key={assignmentKey(a)}
                          role={a.role}
                          label={a.role.replace(/_/g, " ")}
                          onRemove={assignableRoles.includes(a.role) ? () => removeRole.mutate({ userId: u.id, role: a.role }) : undefined}
                          blockedReason={isLastOwnAdminGrant(u, a, currentUserId)
                            ? "This is what grants you user administration — another administrator has to remove it"
                            : undefined}
                          removing={removeRole.isPending}
                        />
                      ))}
                      {(u.assignments ?? []).filter(a => a.workspace_id === "").length === 0 && (
                        <span className="mvx-admin-muted">None</span>
                      )}
                      {(() => {
                        const existing = (u.assignments ?? []).filter(a => a.workspace_id === "").map(a => a.role);
                        const available = PLATFORM_ROLES.filter(r => !existing.includes(r) && assignableRoles.includes(r));
                        if (!available.length) return null;
                        return (
                          <div className="mvx-admin-inline-form">
                            <Select value={addPlatformRole} onChange={e => setAddPlatformRole(e.target.value)}
                              style={{ width: 140 }} aria-label="Add platform role">
                              <option value="">+ Add...</option>
                              {available.map(r => <option key={r} value={r}>{r}</option>)}
                            </Select>
                            {addPlatformRole && (
                              <Button variant="primary" size="sm" disabled={addRole.isPending}
                                onClick={() => addRole.mutate({ userId: u.id, role: addPlatformRole })}>
                                Add
                              </Button>
                            )}
                          </div>
                        );
                      })()}
                    </div>
                  </div>

                  {/* Two different permissions share this block. Placing
                      someone in a workspace with a business role is open to
                      developers; deciding which application or model they
                      reach is not, and stays behind canManageResourceAccess
                      below. Mirrors canGrantWorkspaceRole in the gateway. */}
                  {(canManageResourceAccess || canGrantWorkspaceRoles) && (
                  <div>
                    <div className="mvx-admin-revisions__label" style={{ marginBottom: 8 }}>
                      {canManageResourceAccess ? "Resource access" : "Workspace roles"}
                    </div>
                    {tenants.map(tenant => {
                      const wsAssignments = (u.assignments ?? []).filter(a => a.workspace_id !== "" && a.customer_name === tenant.name);
                      const custWorkspaces = workspaces.filter(w => w.customer_id === tenant.id);
                      const appIds = u.app_ids ?? [];
                      const modelIds = u.model_ids ?? [];
                      const isGrantingHere = addWsCustomerId === tenant.id;

                      return (
                        <div key={tenant.id} className="mvx-admin-object" style={{ marginBottom: 10 }}>
                          <div className="mvx-admin-object__header" style={{ flexWrap: "wrap" }}>
                            <span style={{ fontSize: 12, fontWeight: 700, color: "var(--color-info)", marginRight: 4 }}>{tenant.name}</span>
                            {wsAssignments.map(a => (
                              <RemovableRoleChip
                                key={assignmentKey(a)}
                                role={a.role}
                                label={`${a.workspace_name} · ${a.role.replace(/_/g, " ")}`}
                                onRemove={assignableRoles.includes(a.role) ? () => removeRole.mutate({ userId: u.id, role: a.role, workspaceId: a.workspace_id }) : undefined}
                                blockedReason={isLastOwnAdminGrant(u, a, currentUserId)
                                  ? "This is what grants you user administration — another administrator has to remove it"
                                  : undefined}
                                removing={removeRole.isPending}
                              />
                            ))}
                            {wsAssignments.length === 0 && <span className="mvx-admin-muted">No workspace access</span>}
                            <Button
                              size="sm"
                              variant="ghost"
                              style={{ marginLeft: "auto" }}
                              leadingIcon={isGrantingHere ? undefined : <Plus size={13} />}
                              onClick={() => { setAddWsCustomerId(isGrantingHere ? null : tenant.id); setAddWsId(""); setAddWsRole("business_user"); }}
                            >
                              {isGrantingHere ? "Cancel" : "Add workspace"}
                            </Button>
                          </div>

                          {isGrantingHere && (
                            <div className="mvx-admin-inline-form" style={{ padding: "8px 12px", borderBottom: "1px solid var(--color-border)", background: "var(--color-warning-bg)" }}>
                              <Select value={addWsId} onChange={e => setAddWsId(e.target.value)}
                                style={{ flex: 2 }} aria-label="Select workspace">
                                <option value="">Select workspace...</option>
                                {custWorkspaces.map(w => <option key={w.id} value={w.id}>{w.name}</option>)}
                              </Select>
                              <Select value={addWsRole} onChange={e => setAddWsRole(e.target.value)}
                                style={{ flex: 1 }} aria-label="Select role">
                                {WORKSPACE_ROLES.filter(r => assignableRoles.includes(r)).map(r => <option key={r} value={r}>{r}</option>)}
                              </Select>
                              <Button
                                variant="primary"
                                size="sm"
                                disabled={!addWsId || addRole.isPending}
                                onClick={() => addRole.mutate({ userId: u.id, role: addWsRole, workspaceId: addWsId })}
                              >
                                Grant
                              </Button>
                            </div>
                          )}

                          {canManageResourceAccess && tenant.applications.length === 0 && (
                            <div className="mvx-admin-muted" style={{ padding: "8px 12px" }}>No applications.</div>
                          )}
                          {canManageResourceAccess && tenant.applications.map((app, ai) => {
                            const appChecked = appIds.includes(app.id);
                            const appUnrestricted = appIds.length === 0;
                            const appVisible = appChecked || appUnrestricted;

                            return (
                              <div key={app.id} style={{ borderBottom: ai < tenant.applications.length - 1 ? "1px solid var(--color-border)" : "none" }}>
                                <div style={{ padding: "7px 12px 7px 16px" }}>
                                  <Checkbox
                                    checked={appChecked}
                                    title={appUnrestricted && !appChecked ? "Currently unrestricted - check to restrict to specific apps" : undefined}
                                    onChange={() => {
                                      if (appChecked) revokeAppAccess.mutate({ userId: u.id, appId: app.id });
                                      else grantAppAccess.mutate({ userId: u.id, appId: app.id });
                                    }}
                                    label={
                                      <>
                                        <span style={{ fontSize: 12, fontWeight: 600 }}>{app.name}</span>
                                        {appUnrestricted && <span className="mvx-admin-muted" style={{ fontStyle: "italic", marginLeft: 6 }}>unrestricted</span>}
                                      </>
                                    }
                                  />
                                </div>
                                {appVisible && app.models.length > 0 && (
                                  <div style={{ paddingLeft: 44, paddingBottom: 6 }}>
                                    {app.models.map(model => {
                                      const modelChecked = modelIds.includes(model.id);
                                      const modelUnrestricted = modelIds.length === 0;
                                      return (
                                        <div key={model.id} style={{ padding: "3px 0" }}>
                                          <Checkbox
                                            checked={modelChecked}
                                            title={modelUnrestricted && !modelChecked ? "Currently unrestricted - check to restrict to specific models" : undefined}
                                            onChange={() => {
                                              if (modelChecked) revokeModelAccess.mutate({ userId: u.id, modelId: model.id });
                                              else grantModelAccess.mutate({ userId: u.id, modelId: model.id });
                                            }}
                                            label={
                                              <>
                                                <span style={{ fontSize: 11 }}>{model.name}</span>
                                                {modelUnrestricted && <span className="mvx-admin-muted" style={{ fontStyle: "italic", marginLeft: 6 }}>unrestricted</span>}
                                              </>
                                            }
                                          />
                                        </div>
                                      );
                                    })}
                                  </div>
                                )}
                              </div>
                            );
                          })}
                        </div>
                      );
                    })}
                    {addRole.isError && <p className="mvx-admin-error" style={{ marginTop: 4 }}>{(addRole.error as Error).message}</p>}
                  </div>
                  )}
                </td>
              </tr>
            ) : (
              <tr key={u.id}>
                <td style={{ fontWeight: 500 }}>
                  {u.display_name}
                  {u.disabled_at && <> <StatusBadge tone="warning">Disabled</StatusBadge></>}
                </td>
                <td className="mvx-admin-mono mvx-admin-muted">{u.email}</td>
                <td>
                  <div style={{ display: "flex", gap: 4, flexWrap: "wrap" }}>
                    {(() => {
                      const unique = [...new Set((u.assignments ?? []).map(a => a.role))];
                      if (unique.length === 0) return <span className="mvx-admin-muted">-</span>;
                      return unique.map(role => <RoleBadge key={role} role={role} />);
                    })()}
                  </div>
                </td>
                <td>
                  <div style={{ display: "flex", gap: 4, flexWrap: "wrap" }}>
                    {(() => {
                      const appIds = u.app_ids ?? [];
                      const modelIds = u.model_ids ?? [];
                      if (appIds.length === 0 && modelIds.length === 0) {
                        return <span className="mvx-admin-muted">All apps/models</span>;
                      }
                      return (
                        <>
                          {appIds.map(aid => (
                            <StatusBadge key={aid} tone="brand">App: {appNames.get(aid) ?? aid.slice(0, 8)}</StatusBadge>
                          ))}
                          {modelIds.map(mid => (
                            <StatusBadge key={mid} tone="info">Model: {modelNames.get(mid) ?? mid.slice(0, 8)}</StatusBadge>
                          ))}
                        </>
                      );
                    })()}
                  </div>
                </td>
                <td className="mvx-admin-muted">{u.created_at.slice(0, 10)}</td>
                <td>
                  {/* A platform admin can only be modified by another platform
                      admin — the server enforces this; hide the controls too. */}
                  {(assignableRoles.includes("platform_admin") || !u.assignments.some(a => a.role === "platform_admin")) && (
                    <div style={{ display: "flex", gap: 4 }}>
                      <IconButton
                        aria-label={`Edit ${u.display_name}`}
                        title="Edit"
                        size={28}
                        onClick={() => { setEditId(u.id); setEditUser({ email: u.email, display_name: u.display_name }); setAddPlatformRole(""); setAddWsId(""); setAddWsRole("business_user"); setAddWsCustomerId(null); }}
                      >
                        <Pencil size={14} aria-hidden="true" />
                      </IconButton>
                      {/* Deleting your own account takes the row and the
                          identity-provider login with it, so there is no way
                          back through the console — least of all for the sole
                          platform admin. Left visible but inert, because a
                          missing button just reads as a bug. */}
                      <IconButton
                        aria-label={u.id === currentUserId ? "You cannot delete your own account" : `Delete ${u.display_name}`}
                        title={u.id === currentUserId ? "You cannot delete your own account — ask another administrator" : "Delete"}
                        danger
                        size={28}
                        disabled={u.id === currentUserId}
                        onClick={() => confirm({ title: "Delete user?", body: `This removes "${u.display_name}" and revokes all their access.`, confirmLabel: "Delete user", onConfirm: () => deleteUser.mutate(u.id) })}
                      >
                        <Trash2 size={14} aria-hidden="true" />
                      </IconButton>
                    </div>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
      {confirmElement}
    </div>
  );
}

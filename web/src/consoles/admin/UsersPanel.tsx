import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Mail, Pencil, Plus, Trash2, UserMinus, X } from "lucide-react";
import {
  api,
  type AccessRemovalResult,
  type AdminTenant,
  type AdminUser,
  type AdminUserPermissions,
  type AdminWorkspace,
  type UserAssignment,
  CONTROL_PLANE,
  withTenant,
} from "../../api/client";
import {
  Badge,
  Button,
  Checkbox,
  Field,
  IconButton,
  InlineAlert,
  RoleBadge,
  SearchInput,
  Select,
  StatusBadge,
  TextInput,
  useConfirm,
  type DesignTone,
} from "../../ui";
import { accessRemovalNotice } from "./accessRemoval";

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

/**
 * Roles that reach a whole tenant when granted with no workspace. Given to an
 * address that already has an account, they are only ever given inside a
 * workspace, so the invite form lets whoever may scope them pick one.
 */
const BUILDER_ROLES: string[] = ["developer", "tenant_admin"];

const NO_PERMISSIONS: AdminUserPermissions = {
  rename: false, delete: false, disable: false, reinvite: false, remove_from_tenant: false,
};

/**
 * What the signed-in administrator may do to this account as a whole, as the
 * server computed it with the function its mutations check. A server that
 * sends no permissions predates them, so none are offered rather than ones
 * it might refuse.
 */
function permissionsOf(u: AdminUser): AdminUserPermissions {
  return u.permissions ?? NO_PERMISSIONS;
}

/**
 * An account of another organisation, or of none, that this administrator
 * may not change — only take back what it holds in their own tenant.
 */
function isForeignAccount(u: AdminUser) {
  const p = u.permissions;
  return !!p && (u.home_tenant === "other" || u.home_tenant === "none") && !p.rename && !p.delete
    && !(u.assignments ?? []).some(a => a.role === "platform_admin");
}

/**
 * Says whose account a foreign one is — another organisation's, or none's,
 * as the server words its refusals — and, only when this administrator is
 * offered "Remove from this tenant", that this is what they can do with it.
 */
function foreignAccountNote(u: AdminUser) {
  const whose = u.home_tenant === "none" ? "Not a member of any organisation" : "Member of another organisation";
  return u.permissions?.remove_from_tenant ? `${whose} — you can only remove their access here` : whose;
}

/**
 * A developer grant with no workspace on an account with no organisation is
 * narrowed only by application and model grants; removing the last of them
 * also revokes it (revokeUnnarrowed in internal/gateway), so it never reaches
 * every tenant.
 */
function hasNarrowedDeveloperGrant(u: AdminUser) {
  return u.home_tenant === "none" && (u.assignments ?? []).some(a => a.role === "developer" && a.workspace_id === "");
}

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

/**
 * Whether a tenant lives in the database a user row does, so that a role in
 * its workspaces can be granted to that row: the row's own dedicated tenant,
 * or — for a control-plane row — any tenant without a database of its own.
 */
function inRowDatabase(tenant: AdminTenant, u: AdminUser): boolean {
  const db = u.tenant_id ?? "";
  if (!db) return true;
  if (db === CONTROL_PLANE) return !tenant.dedicated;
  return tenant.id === db;
}

export function UsersPanel({
  users,
  tenants,
  assignableRoles = [],
  canManageResourceAccess = false,
  currentUserId,
  tenantId = "",
  rolesLoading = false,
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
  /**
   * The caller's own roles have not arrived yet (/api/me): assignableRoles is
   * empty only for now, and the invite form says so instead of hiding its
   * role field and leaving Create user disabled without a reason.
   */
  rolesLoading?: boolean;
  /**
   * The dedicated tenant whose people these are, for a platform admin: every
   * call the panel makes is addressed to its database (X-Tenant-Id). Empty
   * for the control plane, and for everyone else, whom the server routes.
   */
  tenantId?: string;
}) {
  const qc = useQueryClient();
  // The list spans more than one database (a platform admin's, or a person
  // in several tenants): each row says which.
  const manyDatabases = new Set(users.map((u) => u.tenant_id ?? "")).size > 1;
  // Each row lives in one database (AdminUser.tenant_id): what is done to it
  // is addressed there, and an invitation is addressed to the chosen
  // workspace's. tenantId addresses what has neither.
  const address = <T,>(tid: string | undefined, call: () => Promise<T>): Promise<T> => {
    const target = tid || tenantId;
    return target ? withTenant(target, call) : call();
  };
  const tenantOfUser = useMemo(() => new Map(users.map((u) => [u.id, u.tenant_id ?? ""])), [users]);
  const atUser = <T,>(userId: string, call: () => Promise<T>): Promise<T> => address(tenantOfUser.get(userId), call);
  const [showCreate, setShowCreate] = useState(false);
  const [newUser, setNewUser] = useState({ email: "", first_name: "", last_name: "", role: "", workspace_id: "" });
  const [editId, setEditId] = useState<string | null>(null);
  // An account a revoke or a removal takes out of the list stops being
  // edited: its row used to reopen in edit mode if it was listed again later.
  const listedIds = users.map(u => u.id).join(",");
  const [seenIds, setSeenIds] = useState(listedIds);
  if (listedIds !== seenIds) {
    setSeenIds(listedIds);
    if (editId && !users.some(u => u.id === editId)) setEditId(null);
  }
  const [editUser, setEditUser] = useState({ email: "", display_name: "" });
  const [search, setSearch] = useState("");
  const [addPlatformRole, setAddPlatformRole] = useState("");
  const [addWsRole, setAddWsRole] = useState("business_user");
  const [addWsId, setAddWsId] = useState("");
  const [addWsCustomerId, setAddWsCustomerId] = useState<string | null>(null);

  const { data: workspaces = [] } = useQuery<AdminWorkspace[]>({
    queryKey: ["admin-workspaces", tenantId],
    queryFn: () => address("", api.getAdminWorkspaces),
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

  // The outcome of the last account-level action, shown above the list: the
  // row it was about may have left the list (removed from this tenant, or a
  // revoke that took the grant it was listed by), and the invite form has
  // closed.
  const [notice, setNotice] = useState<{ tone: "success" | "warning" | "danger"; text: string } | null>(null);
  const failed = (e: unknown) => setNotice({ tone: "danger", text: (e as Error).message });

  const createUser = useMutation({
    mutationFn: (body: typeof newUser) =>
      address(workspaces.find((w) => w.id === body.workspace_id)?.tenant_id, () => api.createAdminUser(body)),
    onMutate: () => setNotice(null),
    // Worded from what was asked, never from the reply: an address that
    // already had an account is only given the role, and the confirmation
    // does not tell the two apart. (The refreshed list still shows such an
    // account as it is — its own name and joined date — see
    // docs/OBSERVATIONS.md.)
    onSuccess: (_res, body) => {
      inv();
      setShowCreate(false);
      setNewUser({ email: "", first_name: "", last_name: "", role: "", workspace_id: "" });
      setNotice({ tone: "success", text: `Invited ${body.email.trim()}. They will be notified of their access.` });
    },
  });
  const updateUser = useMutation({
    mutationFn: () => atUser(editId!, () => api.updateAdminUser(editId!, editUser)),
    onSuccess: () => { inv(); setEditId(null); },
  });
  const reinviteUser = useMutation({
    mutationFn: (u: AdminUser) => atUser(u.id, () => api.resendAdminUserInvite(u.id)),
    onMutate: () => setNotice(null),
    onSuccess: (res, u) => setNotice({ tone: "success", text: `Invitation sent to ${res.email || u.email}.` }),
    onError: failed,
  });
  const removeFromTenant = useMutation({
    mutationFn: (u: AdminUser) => atUser(u.id, () => api.removeAdminUserFromTenant(u.id)),
    onMutate: () => setNotice(null),
    // A developer with no organisation whose grants here were the last to
    // narrow it also loses that developer role; the reply lists it.
    onSuccess: (res, u) => {
      inv();
      if (editId === u.id) setEditId(null);
      const alsoRevoked = accessRemovalNotice(res);
      const removed = `Removed ${u.display_name || u.email} from this tenant.`;
      setNotice(alsoRevoked
        ? { tone: "warning", text: `${removed} ${alsoRevoked}` }
        : { tone: "success", text: removed });
    },
    onError: failed,
  });
  const addRole = useMutation({
    mutationFn: ({ userId, role, workspaceId }: { userId: string; role: string; workspaceId?: string }) =>
      atUser(userId, () => api.addAdminUserRole(userId, role, workspaceId)),
    onSuccess: () => { inv(); setAddPlatformRole(""); setAddWsId(""); setAddWsCustomerId(null); },
  });
  const removeRole = useMutation({
    mutationFn: ({ userId, role, workspaceId }: { userId: string; role: string; workspaceId?: string }) =>
      atUser(userId, () => api.removeAdminUserRole(userId, role, workspaceId)),
    onSuccess: inv,
  });
  const deleteUser = useMutation({
    mutationFn: (id: string) => atUser(id, () => api.deleteAdminUser(id)),
    onMutate: () => setNotice(null),
    onSuccess: inv,
    onError: failed,
  });
  const grantAppAccess = useMutation({
    mutationFn: ({ userId, appId }: { userId: string; appId: string }) => atUser(userId, () => api.grantUserAppAccess(userId, appId)),
    onSuccess: inv,
  });
  // A revoke that goes ahead can take more with it: removing the last
  // application or model that narrows a developer with no tenant also revokes
  // that developer grant, so the account never becomes a builder of every
  // tenant. The reply says what it removed, and it is said above the list —
  // the account may no longer be listed once that grant is gone.
  const revokeNoticed = (res: AccessRemovalResult) => {
    inv();
    const alsoRevoked = accessRemovalNotice(res);
    if (alsoRevoked) setNotice({ tone: "warning", text: alsoRevoked });
  };
  const revokeAppAccess = useMutation({
    mutationFn: ({ userId, appId }: { userId: string; appId: string }) => atUser(userId, () => api.revokeUserAppAccess(userId, appId)),
    onMutate: () => setNotice(null),
    onSuccess: revokeNoticed,
  });
  const grantModelAccess = useMutation({
    mutationFn: ({ userId, modelId }: { userId: string; modelId: string }) => atUser(userId, () => api.grantUserModelAccess(userId, modelId)),
    onSuccess: inv,
  });
  const revokeModelAccess = useMutation({
    mutationFn: ({ userId, modelId }: { userId: string; modelId: string }) => atUser(userId, () => api.revokeUserModelAccess(userId, modelId)),
    onMutate: () => setNotice(null),
    onSuccess: revokeNoticed,
  });
  const { confirm, confirmElement } = useConfirm();

  // A developer may place people into workspaces with a business role, though
  // not decide which applications or models they reach. Derived from the
  // assignable set rather than passed in: whoever can grant a business role at
  // all is exactly who should be able to scope one.
  const canGrantWorkspaceRoles = canManageResourceAccess || assignableRoles.some(r => BUSINESS_ROLES.includes(r));
  // Only a platform admin may assign platform_admin (computeAssignableRoles),
  // so this is exactly "the caller is a platform admin".
  const callerIsPlatformAdmin = assignableRoles.includes("platform_admin");
  // Anyone else always adds a person with a role inside a workspace: the
  // gateway refuses POST /api/admin/users from them without both, and refuses
  // it the same way whether or not the address already has an account, so
  // the answer never tells the two apart. The form asks for both up front.
  const inviteNeedsRoleAndWorkspace = !callerIsPlatformAdmin;
  // Whether the invite form asks for a workspace for this role — always when
  // it needs one, and for a platform admin always for a business role and
  // optionally for developer and tenant_admin.
  const inviteNeedsWorkspace = (role: string) =>
    inviteNeedsRoleAndWorkspace || BUSINESS_ROLES.includes(role);
  const inviteTakesWorkspace = (role: string) =>
    inviteNeedsWorkspace(role) || (canManageResourceAccess && BUILDER_ROLES.includes(role));

  // A person matches by name, e-mail, a role they hold, or the workspace or
  // tenant a role is held in; the one being edited stays in view.
  const q = search.trim().toLowerCase();
  const shownUsers = users.filter(u => !q || u.id === editId
    || [u.display_name, u.email, ...(u.assignments ?? []).flatMap(a => [a.role, a.role.replace(/_/g, " "), a.workspace_name, a.customer_name])]
      .some(v => v?.toLowerCase().includes(q)));

  return (
    <div>
      <div style={{ marginBottom: 16 }}>
        <div style={{ display: "flex", gap: 12, alignItems: "center", flexWrap: "wrap", marginBottom: showCreate ? 16 : 0 }}>
          <Button
            leadingIcon={showCreate ? undefined : <Plus size={14} />}
            onClick={() => setShowCreate((v) => !v)}
          >
            {showCreate ? "Cancel" : "Invite user"}
          </Button>
          <SearchInput value={search} onChange={(e) => setSearch(e.target.value)}
            placeholder="Search name, e-mail, role, workspace…" width={320} />
          {q && <span className="mvx-admin-muted">{shownUsers.length} of {users.length} users</span>}
        </div>
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
              {rolesLoading && (
                <Field label="Initial Role">
                  <span role="status" className="mvx-admin-muted" style={{ fontSize: 13 }}>Loading the roles you can give…</span>
                </Field>
              )}
              {!rolesLoading && assignableRoles.length > 0 && (
                <Field label="Initial Role" required={inviteNeedsRoleAndWorkspace}>
                  <Select
                    value={newUser.role}
                    required={inviteNeedsRoleAndWorkspace}
                    onChange={e => setNewUser(u => ({
                      ...u,
                      role: e.target.value,
                      workspace_id: inviteTakesWorkspace(e.target.value) ? u.workspace_id : "",
                    }))}
                  >
                    {inviteNeedsRoleAndWorkspace
                      ? <option value="" disabled>Select a role…</option>
                      : <option value="">None</option>}
                    {/* Every role this actor may assign, not just the platform
                        tiers. A developer's whole assignable set is the
                        business roles, so filtering to PLATFORM_ROLES left
                        them an empty dropdown and no way to give an invited
                        user any role at all. */}
                    {assignableRoles.map((role) => <option key={role} value={role}>{role.replace(/_/g, " ")}</option>)}
                  </Select>
                </Field>
              )}
              {inviteTakesWorkspace(newUser.role) && (
                <Field
                  label="Workspace"
                  required={inviteNeedsRoleAndWorkspace}
                  description={inviteNeedsRoleAndWorkspace
                    ? "People are always added to a workspace"
                    : BUSINESS_ROLES.includes(newUser.role)
                      ? "a business role grants nothing until it is scoped to a workspace"
                      : "optional — limits the role to one workspace"}
                >
                  {/* The native required (no <form> here, so no browser
                      popup) is what tells assistive tech the field is
                      required; Field's asterisk is visual only. */}
                  <Select value={newUser.workspace_id} required={inviteNeedsRoleAndWorkspace}
                    onChange={e => setNewUser(u => ({ ...u, workspace_id: e.target.value }))}>
                    {inviteNeedsWorkspace(newUser.role)
                      ? <option value="" disabled={inviteNeedsRoleAndWorkspace}>Select a workspace…</option>
                      : <option value="">No specific workspace</option>}
                    {workspaces.map(w => (
                      <option key={w.id} value={w.id}>{w.customer_name} — {w.name}</option>
                    ))}
                  </Select>
                </Field>
              )}
            </div>
            <Button
              variant="primary"
              onClick={() => createUser.mutate(newUser)}
              disabled={rolesLoading || !newUser.email || !newUser.first_name || !newUser.last_name ||
                (inviteNeedsRoleAndWorkspace && !newUser.role) ||
                (inviteNeedsWorkspace(newUser.role) && !newUser.workspace_id)}
              loading={createUser.isPending}
              loadingLabel="Creating…"
            >
              Create user
            </Button>
            {createUser.isError && <p className="mvx-admin-error" style={{ marginTop: 8 }}>{(createUser.error as Error).message}</p>}
          </div>
        )}
      </div>

      {notice && (
        <div role={notice.tone === "danger" ? "alert" : "status"} style={{ marginBottom: 12 }}>
          <InlineAlert tone={notice.tone}>{notice.text}</InlineAlert>
        </div>
      )}

      <div className="mvx-table-wrap">
        <table className="mvx-table">
          <thead>
            <tr>
              <th>Name</th>
              <th>Email</th>
              <th>Role</th>
              <th>Access</th>
              <th>Joined</th>
              <th style={{ width: 132 }}>Actions</th>
            </tr>
          </thead>
          <tbody>
            {q && shownUsers.length === 0 && (
              <tr><td colSpan={6} style={{ padding: 20, textAlign: "center" }} className="mvx-admin-muted">No users match "{search.trim()}"</td></tr>
            )}
            {shownUsers.map((u) => editId === u.id ? (
              <tr key={`${u.tenant_id ?? ""}:${u.id}`}>
                <td colSpan={6} style={{ padding: "16px 20px", background: "var(--color-surface-subtle)" }}>
                  <div className="mvx-admin-inline-form" style={{ marginBottom: isForeignAccount(u) || updateUser.isError ? 8 : 16 }}>
                    {permissionsOf(u).rename ? (
                      <>
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
                      </>
                    ) : (
                      <>
                        {/* The name is not this administrator's to change —
                            the account is another organisation's, or the
                            server says no — so it is shown, not offered. */}
                        <span style={{ fontWeight: 600 }}>{u.display_name}</span>
                        <span className="mvx-admin-mono mvx-admin-muted">{u.email}</span>
                        <Button onClick={() => setEditId(null)}>Close</Button>
                      </>
                    )}
                  </div>
                  {isForeignAccount(u) && (
                    <p className="mvx-admin-muted" style={{ margin: "0 0 16px" }}>{foreignAccountNote(u)}</p>
                  )}
                  {updateUser.isError && (
                    <p className="mvx-admin-error" role="alert" style={{ margin: "0 0 16px" }}>{(updateUser.error as Error).message}</p>
                  )}

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
                          // A role with no workspace belongs to the account's
                          // own organisation; on another's it is not this
                          // administrator's to take away.
                          onRemove={assignableRoles.includes(a.role) && !isForeignAccount(u) ? () => removeRole.mutate({ userId: u.id, role: a.role }) : undefined}
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
                        const available = PLATFORM_ROLES.filter(r => !existing.includes(r) && assignableRoles.includes(r) &&
                          (u.grantable_roles ? u.grantable_roles.includes(r) : true));
                        if (!available.length || isForeignAccount(u)) return null;
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
                    {tenants.filter(tenant => inRowDatabase(tenant, u)).map(tenant => {
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
                    {/* The server may refuse a grant or revoke (the last application
                        or model narrowing a developer with no tenant, say); its
                        reason is shown under the account it was about, as sent. */}
                    {[grantAppAccess, revokeAppAccess, grantModelAccess, revokeModelAccess]
                      .filter(m => m.isError && m.variables?.userId === u.id)
                      .map((m, i) => <p key={i} className="mvx-admin-error" role="alert" style={{ marginTop: 4 }}>{(m.error as Error).message}</p>)}
                  </div>
                  )}
                </td>
              </tr>
            ) : (
              <tr key={`${u.tenant_id ?? ""}:${u.id}`}>
                <td style={{ fontWeight: 500 }}>
                  {u.display_name}
                  {u.disabled_at && <> <StatusBadge tone="warning">Disabled</StatusBadge></>}
                  {isForeignAccount(u) && (
                    <div className="mvx-admin-muted" style={{ fontSize: 11, fontWeight: 400 }}>{foreignAccountNote(u)}</div>
                  )}
                  {manyDatabases && (
                    <div className="mvx-admin-muted" style={{ fontSize: 11, fontWeight: 400 }} data-testid="user-database">
                      {u.tenant_id === CONTROL_PLANE ? "Control plane" : u.tenant_name || "This tenant"}
                    </div>
                  )}
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
                        onClick={() => { setEditId(u.id); updateUser.reset(); setEditUser({ email: u.email, display_name: u.display_name }); setAddPlatformRole(""); setAddWsId(""); setAddWsRole("business_user"); setAddWsCustomerId(null); }}
                      >
                        <Pencil size={14} aria-hidden="true" />
                      </IconButton>
                      {/* Account-level actions are offered as the server's
                          permissions say, never guessed: a tenant admin
                          changes only its own tenant's accounts. */}
                      {permissionsOf(u).reinvite && (
                        <IconButton
                          aria-label={`Resend invitation to ${u.display_name}`}
                          title="Resend invitation"
                          size={28}
                          disabled={reinviteUser.isPending}
                          onClick={() => reinviteUser.mutate(u)}
                        >
                          <Mail size={14} aria-hidden="true" />
                        </IconButton>
                      )}
                      {/* Another organisation's account (or one with none)
                          is not this administrator's to delete — only what
                          it holds here is theirs to take back. */}
                      {permissionsOf(u).remove_from_tenant && u.home_tenant !== "own" && (
                        <IconButton
                          aria-label={`Remove ${u.display_name} from this tenant`}
                          title="Remove from this tenant"
                          danger
                          size={28}
                          disabled={removeFromTenant.isPending}
                          onClick={() => confirm({
                            title: "Remove from this tenant?",
                            body: `This removes every role and access grant "${u.display_name}" holds in your tenant's workspaces, applications and models. `
                              + (hasNarrowedDeveloperGrant(u)
                                ? "Their developer role belongs to no organisation and is limited to the applications or models it was given: "
                                  + "if yours are the last of them, that developer role is revoked too, so it never reaches every tenant. "
                                  + "Nothing else about their account changes."
                                : "Their account itself is not changed."),
                            confirmLabel: "Remove from this tenant",
                            onConfirm: () => removeFromTenant.mutate(u),
                          })}
                        >
                          <UserMinus size={14} aria-hidden="true" />
                        </IconButton>
                      )}
                      {/* Deleting your own account takes the row and the
                          identity-provider login with it, so there is no way
                          back through the console — least of all for the sole
                          platform admin. Left visible but inert, because a
                          missing button just reads as a bug. */}
                      {u.id === currentUserId ? (
                        u.permissions && (
                          <IconButton
                            aria-label="You cannot delete your own account"
                            title="You cannot delete your own account — ask another administrator"
                            danger
                            size={28}
                            disabled
                          >
                            <Trash2 size={14} aria-hidden="true" />
                          </IconButton>
                        )
                      ) : permissionsOf(u).delete && (
                        <IconButton
                          aria-label={`Delete ${u.display_name}`}
                          title="Delete"
                          danger
                          size={28}
                          onClick={() => confirm({ title: "Delete user?", body: `This removes "${u.display_name}" and revokes all their access.`, confirmLabel: "Delete user", onConfirm: () => deleteUser.mutate(u.id) })}
                        >
                          <Trash2 size={14} aria-hidden="true" />
                        </IconButton>
                      )}
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

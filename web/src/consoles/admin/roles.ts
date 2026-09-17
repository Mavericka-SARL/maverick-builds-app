// Mirrors assignableRoles() in internal/gateway/handler.go — the server is
// the actual security boundary (it re-validates every grant/revoke), this
// only keeps the UI from offering choices the actor cannot make. Strictly
// below the actor's own tier: a tenant_admin can grant developer/business
// roles but never another tenant_admin; a developer can grant business
// roles but never a developer or tenant_admin.
const ROLE_HIERARCHY: Record<string, string[]> = {
  platform_admin: ["platform_admin", "tenant_admin", "developer", "business_admin", "business_user"],
  tenant_admin: ["developer", "business_admin", "business_user"],
  developer: ["business_admin", "business_user"],
};

export function computeAssignableRoles(actorRoles: string[]): string[] {
  for (const tier of ["platform_admin", "tenant_admin", "developer"]) {
    if (actorRoles.includes(tier)) return ROLE_HIERARCHY[tier];
  }
  return [];
}

/**
 * Whether this actor may grant application/model access and workspace-scoped
 * roles — the "Resource access" section of the users screen.
 *
 * A developer may administer users but not decide who reaches which
 * application or model: developers build within a model, while deciding who
 * may open it belongs to whoever owns the tenant.
 *
 * Mirrors canManageResourceAccess() in internal/gateway/handler.go, which is
 * the real boundary and refuses these requests regardless of what the console
 * shows.
 */
export function canManageResourceAccess(actorRoles: string[]): boolean {
  return actorRoles.includes("platform_admin") || actorRoles.includes("tenant_admin");
}

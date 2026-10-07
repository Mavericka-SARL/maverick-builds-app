/**
 * A role's display name: its key with spaces, except business_user, which is
 * shown as "user". The key itself stays business_user in the identity
 * provider, the database and the API.
 */
export function roleLabel(role: string): string {
  return role === "business_user" ? "user" : role.replace(/_/g, " ");
}

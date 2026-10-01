import type { AdminUser } from "../../api/client";

/** Every database's people: the platform Users tab's default. */
export const ALL_PEOPLE = "";

/**
 * people narrowed to one database: CONTROL_PLANE for platform accounts and
 * tenants without a database of their own, or a dedicated tenant's id.
 */
export function peopleIn(people: AdminUser[], scope: string): AdminUser[] {
  return scope === ALL_PEOPLE ? people : people.filter((u) => (u.tenant_id ?? "") === scope);
}

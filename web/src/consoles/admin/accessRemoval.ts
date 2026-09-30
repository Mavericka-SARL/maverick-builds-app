import type { AccessRemovalResult } from "../../api/client";

/**
 * What to tell the administrator about a removal that took more with it, or
 * "" when it did not. Removing the last application or model grant that
 * narrows a developer with no tenant — directly, or by deleting the
 * application or model — also revokes that developer grant (removeGrants in
 * internal/gateway), and the reply says so: in words when it sends a
 * message, otherwise as the list of revoked grants.
 */
export function accessRemovalNotice(res: AccessRemovalResult | undefined): string {
  if (!res) return "";
  if (res.message) return res.message;
  const revoked = res.revoked ?? [];
  if (revoked.length === 0) return "";
  const what = revoked.map((g) => `${g.role.replace(/_/g, " ")} from ${g.email}`).join("; ");
  const one = revoked.length === 1;
  return `Also revoked ${what}. ${one ? "That account belongs" : "Those accounts belong"} to no tenant and ` +
    `${one ? "was" : "were"} limited to what was removed: left in place, the grant would have made ` +
    `${one ? "it a builder" : "them builders"} of every tenant.`;
}

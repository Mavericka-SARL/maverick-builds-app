import type { SettingsScope } from "../../api/client";
import { Button, InlineAlert } from "../../ui";

/**
 * One line above a per-tenant settings form saying whose values these are:
 * the deployment's own row (what every tenant inherits until it sets its
 * own), a tenant's own, or the deployment's shown to a tenant that has
 * none — with the way back to inheriting once it has. Silent when the
 * edition has no deployment row: then a tenant only ever has its own.
 */
export function SettingsScopeNotice({ scope, onInherit, inheriting }: { scope?: SettingsScope; onInherit: () => void; inheriting: boolean }) {
  if (!scope) return null;
  if (scope.deployment) {
    return (
      <InlineAlert tone="info">
        These are the <strong>deployment&apos;s defaults</strong>: every tenant that has not set its own follows them.
      </InlineAlert>
    );
  }
  if (!scope.deployment_settings_available) return null;
  if (scope.inherited) {
    return (
      <InlineAlert tone="info">
        This tenant follows the <strong>deployment&apos;s defaults</strong>. Saving here gives it settings of its own.
      </InlineAlert>
    );
  }
  return (
    <div style={{ display: "flex", alignItems: "center", gap: 12, flexWrap: "wrap" }}>
      <span className="mvx-admin-muted">This tenant has settings of its own.</span>
      <Button size="sm" variant="ghost" onClick={onInherit} loading={inheriting} loadingLabel="Switching…" data-testid="inherit-settings">
        Follow the deployment&apos;s defaults instead
      </Button>
    </div>
  );
}

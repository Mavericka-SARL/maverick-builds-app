import type { ReactNode } from "react";
import { Clock, Lock } from "lucide-react";
import { InlineAlert } from "../ui";
import { EDITION_LABELS, licenseDate, useLicense } from "./useLicense";

/**
 * Renders its children only when the license in force unlocks `feature`;
 * otherwise a short note naming the feature and the edition it needs (or the
 * caller's own fallback). Use it around enterprise/commercial UI so a locked
 * feature is visible but inert — the server refuses the calls anyway, this
 * just says why before the user tries.
 */
export function FeatureGate({ feature, children, fallback }: { feature: string; children: ReactNode; fallback?: ReactNode }) {
  const license = useLicense();
  if (license.features.includes(feature)) {
    if (license.state !== "transition") return <>{children}</>;
    // The key expired less than 30 days ago: the server still answers
    // reads, exports and switching off, and refuses changes. Say so first.
    const name = license.catalog.find((c) => c.key === feature)?.name ?? feature;
    return (
      <>
        <div data-testid="feature-transition">
          <InlineAlert tone="warning" icon={<Clock size={14} />} className="mvx-feature-gate">
            <b>{name}</b> is read and export only until {licenseDate(license.transition_ends_at)}: the license key expired on {licenseDate(license.expires_at)}. A renewed key makes it editable again.
          </InlineAlert>
        </div>
        {children}
      </>
    );
  }
  if (fallback !== undefined) return <>{fallback}</>;
  const info = license.catalog.find((c) => c.key === feature);
  const needed = info?.min_edition ?? "enterprise";
  return (
    <InlineAlert tone="info" icon={<Lock size={14} />} className="mvx-feature-gate">
      <b>{info?.name ?? feature}</b> requires the {EDITION_LABELS[needed]} edition; this deployment runs the {EDITION_LABELS[license.edition]} edition.
    </InlineAlert>
  );
}

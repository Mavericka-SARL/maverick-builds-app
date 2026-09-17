import type { ReactNode } from "react";
import { Lock } from "lucide-react";
import { InlineAlert } from "../ui";
import { EDITION_LABELS, useLicense } from "./useLicense";

/**
 * Renders its children only when the license in force unlocks `feature`;
 * otherwise a short note naming the feature and the edition it needs (or the
 * caller's own fallback). Use it around enterprise/commercial UI so a locked
 * feature is visible but inert — the server refuses the calls anyway, this
 * just says why before the user tries.
 */
export function FeatureGate({ feature, children, fallback }: { feature: string; children: ReactNode; fallback?: ReactNode }) {
  const license = useLicense();
  if (license.features.includes(feature)) return <>{children}</>;
  if (fallback !== undefined) return <>{fallback}</>;
  const info = license.catalog.find((c) => c.key === feature);
  const needed = info?.min_edition ?? "enterprise";
  return (
    <InlineAlert tone="info" icon={<Lock size={14} />} className="mvx-feature-gate">
      <b>{info?.name ?? feature}</b> requires the {EDITION_LABELS[needed]} edition; this deployment runs the {EDITION_LABELS[license.edition]} edition.
    </InlineAlert>
  );
}

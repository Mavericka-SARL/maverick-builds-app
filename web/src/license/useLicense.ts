import { useQuery } from "@tanstack/react-query";
import { api, type Edition, type LicenseInfo } from "../api/client";

/** What the console assumes until GET /api/license answers, and whenever it
 *  answers with something that is not a license (an old gateway, a mocked
 *  catch-all): nothing unlocked, nothing labelled. */
export const COMMUNITY_LICENSE: LicenseInfo = {
  edition: "community",
  state: "community",
  features: [],
  catalog: [],
  source: "none",
};

export const EDITION_LABELS: Record<Edition, string> = {
  community: "Community",
  commercial: "Commercial",
  enterprise: "Enterprise",
};

function isLicenseInfo(v: unknown): v is LicenseInfo {
  return !!v && typeof v === "object" && Array.isArray((v as LicenseInfo).features) && typeof (v as LicenseInfo).edition === "string";
}

/**
 * The license in force for this deployment. Read once per session and shared
 * by every console; a fetch failure or a malformed body degrades to the
 * community edition rather than throwing inside the shell.
 */
export function useLicense(): LicenseInfo {
  const { data } = useQuery({
    queryKey: ["license"],
    queryFn: api.getLicense,
    staleTime: 5 * 60_000,
    retry: false,
  });
  return isLicenseInfo(data) ? data : COMMUNITY_LICENSE;
}

/** True when the license in force unlocks `feature` (a pkg/license key) —
 *  during a key's transition too, when it is read and export only. */
export function useFeature(feature: string): boolean {
  return useLicense().features.includes(feature);
}

/** A date as the console shows licence dates. */
export function licenseDate(iso?: string): string {
  if (!iso) return "—";
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleDateString();
}

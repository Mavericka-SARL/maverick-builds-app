import { useEffect, useMemo, useState, type ReactNode } from "react";
import type { BrandView } from "../api/client";
import { BrandContext, DEFAULT_BRAND, applyBrand, fetchPublicBrand } from "./brand";

/**
 * Reads the brand once before anything renders (public: by the request's
 * host) and again once the console can identify the caller (by tenant), and
 * keeps the document in step. Outside the ee tree on purpose: every edition
 * must be able to APPLY a brand the server reports; only editing it is gated.
 */
export function BrandingProvider({ children }: { children: ReactNode }) {
  const [brand, setBrand] = useState<BrandView>(DEFAULT_BRAND);
  useEffect(() => {
    let cancelled = false;
    // The signed-in refresh (useBrandRefresh) may already have answered by
    // tenant; a later public answer by host must not overwrite it.
    void fetchPublicBrand().then((b) => { if (!cancelled) setBrand((prev) => (prev.source === "tenant" ? prev : b)); });
    return () => { cancelled = true; };
  }, []);
  useEffect(() => { applyBrand(brand); }, [brand]);
  const value = useMemo(() => ({ brand, setBrand }), [brand]);
  return <BrandContext.Provider value={value}>{children}</BrandContext.Provider>;
}

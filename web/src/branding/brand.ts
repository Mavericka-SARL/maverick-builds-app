import { createContext, useContext, useEffect } from "react";
import { api, type BrandView } from "../api/client";

/** The platform's own look, applied when no tenant brand is in force. */
export const DEFAULT_BRAND: BrandView = { product_name: "", tagline: "", logo_data_url: "", favicon_data_url: "", brand_color: "", configured: false, source: "default" };

export interface BrandState {
  brand: BrandView;
  setBrand: (b: BrandView) => void;
}

export const BrandContext = createContext<BrandState>({ brand: DEFAULT_BRAND, setBrand: () => {} });

/** The brand in force. product_name is "maverickbuilds.app" when none is set, so copy
 *  that names the product never has to special-case the default. */
export function useBrand(): BrandView & { name: string } {
  const { brand: b } = useContext(BrandContext);
  return { ...b, name: b.product_name || "maverickbuilds.app" };
}

/**
 * Re-reads the brand as the signed-in person: the provider above the auth
 * layer can only ask by host, and a tenant's brand is by tenant. Call it from
 * a component that renders only once the caller is known; it refreshes again
 * when the identity changes (the dev persona switcher).
 */
export function useBrandRefresh(identity: string | undefined) {
  const { setBrand } = useContext(BrandContext);
  useEffect(() => {
    let cancelled = false;
    api.getBranding().then((b) => { if (!cancelled) setBrand(b); }).catch(() => { /* keep what the host said */ });
    return () => { cancelled = true; };
  }, [identity, setBrand]);
}

// ── colour derivation ─────────────────────────────────────────────────────────
// One brand colour becomes the six tokens the design system uses: the colour
// itself as 500, two darker steps for hover/active, three tints for
// backgrounds. Kept in sRGB for predictability.

function hexToRgb(hex: string): [number, number, number] | null {
  const m = /^#([0-9a-f]{6})$/i.exec(hex.trim());
  if (!m) return null;
  const n = parseInt(m[1], 16);
  return [(n >> 16) & 255, (n >> 8) & 255, n & 255];
}
const clamp = (v: number) => Math.max(0, Math.min(255, Math.round(v)));
const toHex = (rgb: [number, number, number]) => "#" + rgb.map((c) => clamp(c).toString(16).padStart(2, "0")).join("");
const mixWhite = (rgb: [number, number, number], t: number): [number, number, number] => [rgb[0] + (255 - rgb[0]) * t, rgb[1] + (255 - rgb[1]) * t, rgb[2] + (255 - rgb[2]) * t];
const darken = (rgb: [number, number, number], f: number): [number, number, number] => [rgb[0] * f, rgb[1] * f, rgb[2] * f];

/** The token values for one brand colour; exported for tests. */
export function brandTokens(hex: string): Record<string, string> | null {
  const rgb = hexToRgb(hex);
  if (!rgb) return null;
  return {
    "--color-brand-50": toHex(mixWhite(rgb, 0.92)),
    "--color-brand-100": toHex(mixWhite(rgb, 0.85)),
    "--color-brand-200": toHex(mixWhite(rgb, 0.7)),
    "--color-brand-500": toHex(rgb),
    "--color-brand-600": toHex(darken(rgb, 0.85)),
    "--color-brand-700": toHex(darken(rgb, 0.72)),
  };
}

const TOKEN_NAMES = ["--color-brand-50", "--color-brand-100", "--color-brand-200", "--color-brand-500", "--color-brand-600", "--color-brand-700"];

/** The icon links as the document was served with them (index.html), read
 *  once at load — before any brand has been applied. */
const DEFAULT_ICONS = Array.from(document.querySelectorAll<HTMLLinkElement>("link[rel~='icon']")).map((link) => ({
  link,
  href: link.getAttribute("href") ?? "/favicon.svg",
  type: link.getAttribute("type") ?? "",
}));

/** Applies a brand to the document: title, favicon, colour tokens. */
export function applyBrand(b: BrandView) {
  const root = document.documentElement;
  const tokens = b.configured && b.brand_color ? brandTokens(b.brand_color) : null;
  for (const name of TOKEN_NAMES) {
    if (tokens) root.style.setProperty(name, tokens[name]);
    else root.style.removeProperty(name);
  }
  document.title = b.configured && b.product_name ? b.product_name : "maverickbuilds.app";
  // Every icon link, not just the first: index.html declares an SVG and an
  // .ico fallback, and leaving one of them pointing at the product's own mark
  // would show it to a tenant that has replaced it. Defaults are the ones the
  // document was served with, so removing a brand puts both back.
  for (const icon of DEFAULT_ICONS) {
    if (b.configured && b.favicon_data_url) {
      icon.link.href = b.favicon_data_url;
      icon.link.type = "";
    } else {
      icon.link.href = icon.href;
      icon.link.type = icon.type;
    }
  }
}

export async function fetchPublicBrand(): Promise<BrandView> {
  try {
    const res = await fetch("/api/branding");
    if (!res.ok) return DEFAULT_BRAND;
    return (await res.json()) as BrandView;
  } catch {
    return DEFAULT_BRAND;
  }
}

/** A tab that re-applies a brand after an admin edits it. */
export function useApplyBrand() {
  return applyBrand;
}

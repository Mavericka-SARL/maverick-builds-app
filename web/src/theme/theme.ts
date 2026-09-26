import { useCallback, useEffect, useState } from "react";

/**
 * Light / dark theme, chosen per person in the account menu.
 *
 * The choice is kept in this browser (localStorage), not on the account: it
 * is a display preference with nothing to authorise or audit. "system"
 * follows the operating system and tracks it live.
 *
 * The resolved theme is written to <html data-theme>, which is all the design
 * system reads (design-system.css, :root[data-theme="dark"]). index.html runs
 * the same resolution inline before the bundle loads so a dark page never
 * paints white first — it reads THEME_STORAGE_KEY by name, so keep the two in
 * step.
 */
export type ThemePreference = "light" | "dark" | "system";
export type ResolvedTheme = "light" | "dark";

export const THEME_STORAGE_KEY = "mvx-theme";

/** Light until someone opts in, so nobody's console changes under them. */
const DEFAULT_PREFERENCE: ThemePreference = "light";

const DARK_QUERY = "(prefers-color-scheme: dark)";

function isPreference(v: unknown): v is ThemePreference {
  return v === "light" || v === "dark" || v === "system";
}

export function readThemePreference(): ThemePreference {
  try {
    const v = localStorage.getItem(THEME_STORAGE_KEY);
    return isPreference(v) ? v : DEFAULT_PREFERENCE;
  } catch {
    return DEFAULT_PREFERENCE;
  }
}

function systemPrefersDark(): boolean {
  return typeof window.matchMedia === "function" && window.matchMedia(DARK_QUERY).matches;
}

export function resolveTheme(pref: ThemePreference): ResolvedTheme {
  if (pref === "system") return systemPrefersDark() ? "dark" : "light";
  return pref;
}

export function applyTheme(theme: ResolvedTheme) {
  document.documentElement.setAttribute("data-theme", theme);
}

/** The current preference and a setter that stores and applies it. Also
 *  follows the OS while on "system", and other tabs of this app. */
export function useThemePreference(): [ThemePreference, (p: ThemePreference) => void] {
  const [pref, setPrefState] = useState<ThemePreference>(readThemePreference);

  useEffect(() => {
    applyTheme(resolveTheme(pref));
    if (pref !== "system" || typeof window.matchMedia !== "function") return;
    const mq = window.matchMedia(DARK_QUERY);
    const onChange = () => applyTheme(resolveTheme("system"));
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, [pref]);

  useEffect(() => {
    const onStorage = (e: StorageEvent) => {
      if (e.key === THEME_STORAGE_KEY) setPrefState(readThemePreference());
    };
    window.addEventListener("storage", onStorage);
    return () => window.removeEventListener("storage", onStorage);
  }, []);

  const setPref = useCallback((p: ThemePreference) => {
    try {
      localStorage.setItem(THEME_STORAGE_KEY, p);
    } catch {
      // Storage blocked (private mode, policy): the choice still applies for
      // this page, it just is not remembered.
    }
    setPrefState(p);
  }, []);

  return [pref, setPref];
}

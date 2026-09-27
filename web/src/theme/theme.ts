import { useCallback, useEffect, useRef, useSyncExternalStore } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type Me } from "../api/client";

/**
 * Light / dark theme, chosen per person in the account menu.
 *
 * The choice is kept on the person's account (PATCH /api/me/preferences), so
 * it follows them to every browser and device; the account's value wins once
 * /api/me has loaded. This browser keeps a copy in localStorage, because the
 * theme has to be applied before the bundle loads — index.html runs the same
 * resolution inline so a dark page never paints white first, reading
 * THEME_STORAGE_KEY by name, so keep the two in step. "system" follows the
 * operating system and tracks it live.
 *
 * The resolved theme is written to <html data-theme>, which is all the design
 * system reads (design-system.css, :root[data-theme="dark"]).
 */
export type ThemePreference = "light" | "dark" | "system";
export type ResolvedTheme = "light" | "dark";

export const THEME_STORAGE_KEY = "mvx-theme";
/** Which account the stored copy belongs to, so a browser shared by two
 *  people does not hand one person's choice to the other's account. */
const THEME_OWNER_KEY = "mvx-theme-account";
/** Set while a choice made here has not reached the account yet (the save is
 *  in flight, failed, or the tab closed first). While it is set, the next
 *  /api/me load sends the choice again instead of letting the account's older
 *  value undo it. */
const THEME_UNSAVED_KEY = "mvx-theme-unsaved";

/** Light until someone opts in, so nobody's console changes under them. */
const DEFAULT_PREFERENCE: ThemePreference = "light";

const DARK_QUERY = "(prefers-color-scheme: dark)";

function isPreference(v: unknown): v is ThemePreference {
  return v === "light" || v === "dark" || v === "system";
}

// ── the stored copy, as an external store ────────────────────────────────────
// localStorage, with an in-memory stand-in where storage is blocked (private
// mode, policy) so a choice still applies for the life of the page.

const memory = new Map<string, string>();
const listeners = new Set<() => void>();

function readStored(key: string): string | null {
  try {
    return localStorage.getItem(key);
  } catch {
    return memory.get(key) ?? null;
  }
}

function writeStored(key: string, value: string | null) {
  if (value === null) memory.delete(key);
  else memory.set(key, value);
  try {
    if (value === null) localStorage.removeItem(key);
    else localStorage.setItem(key, value);
  } catch {
    // Blocked: the in-memory copy above stands in.
  }
  if (key === THEME_STORAGE_KEY) listeners.forEach((notify) => notify());
}

/** This tab's writes notify directly; other tabs' arrive as storage events. */
function subscribe(notify: () => void) {
  listeners.add(notify);
  const onStorage = (e: StorageEvent) => {
    if (e.key === THEME_STORAGE_KEY || e.key === null) notify();
  };
  window.addEventListener("storage", onStorage);
  return () => {
    listeners.delete(notify);
    window.removeEventListener("storage", onStorage);
  };
}

export function readThemePreference(): ThemePreference {
  const v = readStored(THEME_STORAGE_KEY);
  return isPreference(v) ? v : DEFAULT_PREFERENCE;
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

// Saves go out one at a time, so two quick clicks reach the account in the
// order they were made and the last one is what it keeps.
let saving: Promise<unknown> = Promise.resolve();
let latestSave = 0;
function saveToAccount(theme: ThemePreference) {
  const n = ++latestSave;
  writeStored(THEME_UNSAVED_KEY, "1");
  saving = saving.then(() =>
    api.updateMyPreferences({ theme }).then(
      () => {
        if (n === latestSave) writeStored(THEME_UNSAVED_KEY, null);
      },
      (err: unknown) => {
        // A refusal (4xx) will not change on a retry: let the account's value
        // stand. Offline or a server error keeps the unsaved mark, so the
        // next /api/me load sends the choice again.
        const status = (err as { status?: number }).status;
        if (n === latestSave && status !== undefined && status >= 400 && status < 500) writeStored(THEME_UNSAVED_KEY, null);
      },
    ),
  );
}

/** The current preference and a setter that applies it, stores it in this
 *  browser and saves it to the account. Also follows the OS while on
 *  "system", other tabs of this app, and the account once it has loaded. */
export function useThemePreference(): [ThemePreference, (p: ThemePreference) => void] {
  const qc = useQueryClient();
  const { data: me } = useQuery({ queryKey: ["me"], queryFn: api.getMe });
  const pref = useSyncExternalStore(subscribe, readThemePreference, () => DEFAULT_PREFERENCE);
  // A choice made before /api/me answered: that answer predates it, so it
  // must not undo the click.
  const chosenBeforeLoad = useRef(false);

  const userId = me?.user_id;
  const saved = me?.preferences?.theme;
  // undefined: not known — /api/me not loaded yet, or it could not read the
  // preferences (null); null: the account has not chosen.
  const accountTheme = me === undefined || me.preferences == null ? undefined : isPreference(saved) ? saved : null;

  useEffect(() => {
    applyTheme(resolveTheme(pref));
    if (pref !== "system" || typeof window.matchMedia !== "function") return;
    const mq = window.matchMedia(DARK_QUERY);
    const onChange = () => applyTheme(resolveTheme("system"));
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, [pref]);

  const setAccountTheme = useCallback(
    (theme: ThemePreference, save = true) => {
      // Only when there is an answer to protect: cancelling the first
      // /api/me fetch would leave the query empty with nothing to refetch it.
      if (qc.getQueryData<Me>(["me"]) !== undefined) {
        void qc.cancelQueries({ queryKey: ["me"] });
        qc.setQueryData<Me>(["me"], (old) => (old ? { ...old, preferences: { ...old.preferences, theme } } : old));
      }
      if (save) saveToAccount(theme);
    },
    [qc],
  );

  // The account is the source of truth once it has loaded.
  useEffect(() => {
    if (accountTheme === undefined || !userId) return;
    const owner = readStored(THEME_OWNER_KEY);
    writeStored(THEME_OWNER_KEY, userId);
    const stored = readStored(THEME_STORAGE_KEY);
    const mine = owner === null || owner === userId;
    if (chosenBeforeLoad.current) {
      chosenBeforeLoad.current = false;
      if (isPreference(stored) && stored !== accountTheme) {
        // The click's save is already queued; only the cache is behind.
        setAccountTheme(stored, false);
        return;
      }
    }
    if (!mine) writeStored(THEME_UNSAVED_KEY, null);
    else if (isPreference(stored) && readStored(THEME_UNSAVED_KEY) && stored !== accountTheme) {
      setAccountTheme(stored);
      return;
    }
    if (accountTheme) {
      if (accountTheme !== stored) writeStored(THEME_STORAGE_KEY, accountTheme);
      return;
    }
    if (isPreference(stored) && owner === null) {
      // A choice this browser made before the theme was kept on the account
      // becomes the account's. (One made since carries an owner, and an
      // unsaved one was re-sent above.)
      setAccountTheme(stored);
      return;
    }
    // Nothing chosen on this account, and what this browser holds is either
    // someone else's or a copy of a choice the account has since dropped:
    // the default.
    if (stored !== null) writeStored(THEME_STORAGE_KEY, null);
  }, [accountTheme, userId, setAccountTheme]);

  const setPref = useCallback(
    (p: ThemePreference) => {
      if (userId) writeStored(THEME_OWNER_KEY, userId);
      else chosenBeforeLoad.current = true;
      writeStored(THEME_STORAGE_KEY, p);
      setAccountTheme(p);
    },
    [userId, setAccountTheme],
  );

  return [pref, setPref];
}

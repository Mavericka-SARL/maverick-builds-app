import { useCallback, useSyncExternalStore } from "react";
import { useQueryClient, type QueryClient } from "@tanstack/react-query";
import type { AppInfo } from "../../api/client";

// Which application and model the business consoles work in. The API client
// sends both as X-App-Id / X-Model-Id on every request, and the gateway
// answers only for a model the caller may access.
export const SELECTED_APP_KEY = "selected_app_id";
export const SELECTED_MODEL_KEY = "selected_model_id";

// Queries that are the person's or the tenant's, not the model's: the shell
// shows them, and they answer the same in every model of one tenant, so a
// switch keeps them. Every other query is dropped by a switch and asked again
// in the new model; one missing here is only fetched once more.
const PERSON_QUERY_KEYS = new Set(["apps", "me", "me-plan", "dev-personas", "notifications"]);

// Counts in-place switches. The console keys its page on it, so every screen
// opens afresh in the new model, as it did when a switch reloaded the page.
let switches = 0;
const listeners = new Set<() => void>();

function subscribe(listener: () => void) {
  listeners.add(listener);
  return () => { listeners.delete(listener); };
}

/** Changes whenever the console switched model in place. */
export function useModelSwitches(): number {
  return useSyncExternalStore(subscribe, () => switches);
}

/**
 * Switches every console view to one model (null: the application's default).
 * Within the tenant already open the console stays where it is — the
 * sidebar, the header, the open tab — and only the page opens again in the
 * new model; it used to reload the whole console each time (reported on the
 * Playground, 2026-10-31). An application of another tenant still reloads:
 * the gateway routes a request by its application, and the person's roles,
 * brand and edition there are that tenant's.
 */
export function useSelectModel(): (appId: string, modelId: string | null) => void {
  const qc = useQueryClient();
  return useCallback((appId: string, modelId: string | null) => selectModel(qc, appId, modelId), [qc]);
}

function selectModel(qc: QueryClient, appId: string, modelId: string | null) {
  const apps = qc.getQueryData<AppInfo[]>(["apps"]) ?? [];
  const tenantOf = (id: string | null) => {
    const app = apps.find((a) => a.id === id);
    return app ? (app.tenant_id ?? "") : null;
  };
  const from = tenantOf(localStorage.getItem(SELECTED_APP_KEY));
  const isDefault = !!apps.find((a) => a.id === appId)?.models?.find((m) => m.id === modelId)?.is_default;

  localStorage.setItem(SELECTED_APP_KEY, appId);
  // The default needs no pin — clearing keeps behavior stable if the
  // developer later changes which model is the default.
  if (!modelId || isDefault) localStorage.removeItem(SELECTED_MODEL_KEY);
  else localStorage.setItem(SELECTED_MODEL_KEY, modelId);

  if (from === null || from !== tenantOf(appId)) {
    window.location.reload();
    return;
  }
  qc.removeQueries({ predicate: (q) => !PERSON_QUERY_KEYS.has(String(q.queryKey[0])) });
  switches++;
  listeners.forEach((listener) => listener());
  window.scrollTo({ top: 0 });
}

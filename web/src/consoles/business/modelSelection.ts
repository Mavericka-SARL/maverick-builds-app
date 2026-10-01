// Which application and model the business consoles work in. The API client
// sends both as X-App-Id / X-Model-Id on every request, and the gateway
// answers only for a model the caller may access.
export const SELECTED_APP_KEY = "selected_app_id";
export const SELECTED_MODEL_KEY = "selected_model_id";

/** Switches every console view to one model and reloads so each query follows. */
export function selectModel(appId: string, modelId: string, isDefault: boolean) {
  localStorage.setItem(SELECTED_APP_KEY, appId);
  // The default needs no pin — clearing keeps behavior stable if the
  // developer later changes which model is the default.
  if (isDefault) localStorage.removeItem(SELECTED_MODEL_KEY);
  else localStorage.setItem(SELECTED_MODEL_KEY, modelId);
  window.location.reload();
}

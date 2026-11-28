// A selector-bar choice kept across dashboards (SelectorBarEntry.keep_choice,
// owner 2026-11-27): the member a person picked for a dimension of a model,
// for this browser session. The project picked on Project · Overview opens
// Project · Costs on it, instead of its default project again. Storage can
// be missing or refuse (a private window): then nothing is kept and every
// dashboard opens on its default, as without the setting.

const key = (modelId: string, dimensionId: string) => `mvx.keptChoice.${modelId}.${dimensionId}`;

export function readKeptChoice(modelId: string, dimensionId: string): string | undefined {
  try {
    return sessionStorage.getItem(key(modelId, dimensionId)) || undefined;
  } catch {
    return undefined;
  }
}

export function writeKeptChoice(modelId: string, dimensionId: string, code: string): void {
  try {
    sessionStorage.setItem(key(modelId, dimensionId), code);
  } catch {
    // Nothing kept: the next dashboard opens on its default.
  }
}

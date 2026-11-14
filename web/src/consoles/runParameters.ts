/** A value a loader asks for at each run (its config's "parameters"),
 *  stamped by the server on every row of the file as a fixed-value column. */
export type RunParameter = { column: string; label?: string; type?: "date" | "text"; required?: boolean };

/** runParametersOf is what a saved loader's config asks for at each run. */
export function runParametersOf(config: unknown): RunParameter[] {
  const ps = (config as { parameters?: RunParameter[] } | undefined)?.parameters;
  return Array.isArray(ps) ? ps.filter(p => p && p.column) : [];
}

/** replaceByFields are the fields a table loader's replace_by names: saved as
 *  one field, a comma list, or a list. */
export function replaceByFields(v: unknown): string[] {
  const list = Array.isArray(v) ? v.map(String) : typeof v === "string" ? v.split(",") : [];
  return list.map(s => s.trim()).filter(Boolean);
}

import { useQuery, useQueryClient, type QueryClient } from "@tanstack/react-query";
import type { ModelCheckFinding } from "../../api/client";

// The model check's findings about what was just saved (a save answers with
// them), shown above the list until dismissed or the next save.
const KEY = ["model-check-save-warnings"] as const;

export function showSaveWarnings(qc: QueryClient, warnings: ModelCheckFinding[] | undefined) {
  qc.setQueryData(KEY, warnings ?? []);
}

export function useSaveWarnings() {
  const qc = useQueryClient();
  const { data } = useQuery({ queryKey: KEY, queryFn: () => [] as ModelCheckFinding[], initialData: [] as ModelCheckFinding[], staleTime: Infinity });
  return { warnings: data, clear: () => qc.setQueryData(KEY, []) };
}

export const SEVERITY_TONE = { error: "danger", warning: "warning", info: "info" } as const;

/** What a finding is about, in words. */
export function objectLabel(f: ModelCheckFinding): string {
  const kind = { metric: "Metric", mapping: "Mapping", widget: "Widget", dashboard: "Dashboard", role: "Role", automation_rule: "Trigger" }[f.object.kind] ?? f.object.kind;
  return `${kind} · ${f.object.name}${f.object.dashboard ? ` (dashboard ${f.object.dashboard})` : ""}`;
}

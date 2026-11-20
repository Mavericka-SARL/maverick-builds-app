import { useQuery } from "@tanstack/react-query";
import { api, type TimeWindow } from "../../api/client";

// A widget's rolling window (widget_props.time_window): the periods the
// server works out from the date its metric holds now. Keyed under
// "grid-meta" so a write that moves the date (Settings: first forecast week
// + 7) refreshes the window with the rest of the model's data
// (invalidateModelData). undefined: no window, or no date yet.
export function useTimeWindowCodes(revisionId: string | undefined, w: TimeWindow | undefined): string[] | undefined {
  const { data } = useQuery({
    queryKey: ["grid-meta", revisionId, "time-window", w?.dimension_id, w?.from_metric_id, w?.periods, w?.before ?? 0],
    queryFn: () => api.getTimeWindow(revisionId!, w!),
    enabled: !!revisionId && !!w?.dimension_id && !!w?.from_metric_id && (w?.periods ?? 0) > 0,
    staleTime: 10_000,
  });
  return data?.codes ?? undefined;
}

// The members a widget shows on the window's dimension: the window's
// periods, then those the widget lists besides them (a total).
export function windowedShowMembers(
  show: Record<string, string[]> | undefined, w: TimeWindow | undefined, codes: string[] | undefined,
): Record<string, string[]> | undefined {
  if (!w || !codes) return show;
  const listed = (show?.[w.dimension_id] ?? []).filter(c => !codes.includes(c));
  return { ...(show ?? {}), [w.dimension_id]: [...codes, ...listed] };
}

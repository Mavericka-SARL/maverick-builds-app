import { useQuery } from "@tanstack/react-query";
import { api, type AppInfo, type DemoContext } from "../../api/client";

/**
 * "Application · Model" for the model the User and Business Admin screens
 * work in, as the Developer screens name theirs. An application can hold
 * several models (sign-up's holds the tour and three guides), and the
 * revision alone does not say which one is open. Names come from the same
 * list the Model switcher reads; null until it has loaded.
 */
export function useModelLabel(ctx: DemoContext | undefined, enabled: boolean): string | null {
  const { data: apps = [] } = useQuery({ queryKey: ["apps"], queryFn: api.getApps, enabled: enabled && !!ctx });
  if (!ctx) return null;
  const list = apps as AppInfo[];
  const app = list.find(a => a.models?.some(m => m.id === ctx.model_id));
  const model = app?.models?.find(m => m.id === ctx.model_id);
  return app && model ? `${app.name} · ${model.name}` : null;
}

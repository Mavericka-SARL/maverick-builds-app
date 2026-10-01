import { useQuery } from "@tanstack/react-query";
import { api, type AppInfo, type DemoContext } from "../../api/client";
import { Select } from "../../ui";
import { selectModel } from "./modelSelection";

/**
 * Picks which of the caller's models the Dashboards view shows. Lists every
 * model of every application the caller may access (the same list as the
 * Models tab) and renders nothing when there is only one.
 */
export function ModelSwitcher({ ctx }: { ctx: DemoContext }) {
  const { data: apps = [] } = useQuery({ queryKey: ["apps"], queryFn: api.getApps });
  const list = apps as AppInfo[];
  const models = list.flatMap(app => (app.models ?? []).map(m => ({ app, model: m })));
  if (models.length <= 1) return null;

  // Applications can come from more than one tenant; then each is named with it.
  const manyTenants = new Set(list.map(a => a.tenant_id ?? "")).size > 1;
  const appLabel = (a: AppInfo) => (manyTenants && a.tenant_name ? `${a.tenant_name} — ${a.name}` : a.name);
  const withModels = list.filter(a => (a.models ?? []).length > 0);

  function onChange(modelId: string) {
    const hit = models.find(x => x.model.id === modelId);
    if (!hit || modelId === ctx.model_id) return;
    selectModel(hit.app.id, hit.model.id, hit.model.is_default);
  }

  return (
    <Select
      value={ctx.model_id}
      onChange={e => onChange(e.target.value)}
      aria-label="Model"
      title="Model"
      style={{ width: 220 }}
    >
      {withModels.length === 1
        ? withModels[0].models!.map(m => <option key={m.id} value={m.id}>{m.name}</option>)
        : withModels.map(a => (
            <optgroup key={a.id} label={appLabel(a)}>
              {a.models!.map(m => <option key={m.id} value={m.id}>{m.name}</option>)}
            </optgroup>
          ))}
    </Select>
  );
}

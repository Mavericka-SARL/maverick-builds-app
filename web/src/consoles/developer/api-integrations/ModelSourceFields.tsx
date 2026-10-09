import { useQuery } from "@tanstack/react-query";
import { api, type ApiModelSource } from "../../../api/client";
import { Checkbox, Field, InlineAlert, Select } from "../../../ui";

// The model-link side of the Request step: a grid of another model of this
// tenant, read in that model's active revision as the link's owner — the
// developer of both models who set it up. Only models you are a developer of
// are offered: setting up and testing a link needs a developer of both.
export function ModelSourceFields({ source, onSource }: {
  source: ApiModelSource;
  onSource: (s: ApiModelSource) => void;
}) {
  const patch = (p: Partial<ApiModelSource>) => onSource({ ...source, ...p });
  const { data: models = [], isLoading } = useQuery({
    queryKey: ["model-link-sources"],
    queryFn: () => api.listModelLinkSources(),
  });
  const model = models.find(m => m.model_id === source.model_id);
  const grid = model?.grids.find(g => g.name.toLowerCase() === source.grid.toLowerCase());
  // Members only for the chosen grid, for its filters.
  const { data: detail } = useQuery({
    queryKey: ["model-link-source-grid", source.model_id, source.grid],
    queryFn: () => api.listModelLinkSources({ modelId: source.model_id, grid: source.grid }),
    enabled: !!grid,
  });
  const dims = detail?.[0]?.grids[0]?.dimensions ?? [];
  const metrics = source.metrics ?? [];
  const filters = source.filters ?? {};
  const setFilter = (dim: string, codes: string[]) => {
    const next = { ...filters };
    if (codes.length > 0) next[dim] = codes; else delete next[dim];
    patch({ filters: Object.keys(next).length > 0 ? next : undefined });
  };

  return (
    <div style={{ display: "grid", gap: 12, maxWidth: 640 }}>
      {!isLoading && models.length === 0 && (
        <InlineAlert tone="info">
          No other model of this tenant can be linked: you must be a developer of the model a link reads.
        </InlineAlert>
      )}
      <Field label="Model" required description="Another model of this tenant that you are a developer of.">
        <Select value={source.model_id} aria-label="Source model"
          onChange={e => onSource({ model_id: e.target.value, grid: "" })}>
          <option value="">Select…</option>
          {models.map(m => (
            <option key={m.model_id} value={m.model_id}>{m.application_name} › {m.model_name}</option>
          ))}
        </Select>
      </Field>
      {model && (
        <Field label="Grid" required
          description={model.revision
            ? `Read in the model's active revision (${model.revision}) at each run, so the link follows its promotions.`
            : "This model has no active revision yet."}>
          <Select value={grid?.name ?? ""} aria-label="Source grid"
            onChange={e => patch({ grid: e.target.value, metrics: undefined, filters: undefined })}>
            <option value="">Select…</option>
            {model.grids.map(g => <option key={g.name} value={g.name}>{g.name}</option>)}
          </Select>
        </Field>
      )}
      {grid && (
        <>
          <Field label="Metrics" description="None ticked = every metric of the grid. Calculated metrics come as the values they show.">
            <div role="group" aria-label="Source metrics" style={{ display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(180px, 1fr))", gap: 4 }}>
              {grid.metrics.map(m => (
                <Checkbox key={m.name} label={m.label && m.label !== m.name ? `${m.label} (${m.name})` : m.name}
                  checked={metrics.includes(m.name)}
                  onChange={e => {
                    const next = e.target.checked ? [...metrics, m.name] : metrics.filter(x => x !== m.name);
                    patch({ metrics: next.length > 0 ? next : undefined });
                  }} />
              ))}
            </div>
          </Field>
          <Field label="Filters" description="Keep only these members (a parent keeps every member under it). None chosen = all.">
            <div style={{ display: "grid", gap: 8 }}>
              {dims.map(d => (
                <label key={d.name} style={{ display: "grid", gap: 4 }}>
                  <span style={{ fontSize: 12 }}>{d.name}</span>
                  <Select multiple aria-label={`Filter ${d.name}`} size={Math.min(6, Math.max(2, d.members?.length ?? 2))}
                    style={{ height: "auto", backgroundImage: "none", paddingRight: 8 }}
                    value={filters[d.name] ?? []}
                    onChange={e => setFilter(d.name, Array.from(e.target.selectedOptions, o => o.value))}>
                    {(d.members ?? []).map(m => (
                      <option key={m.code} value={m.code}>{m.code}{m.label && m.label !== m.code ? ` — ${m.label}` : ""}</option>
                    ))}
                  </Select>
                </label>
              ))}
            </div>
          </Field>
          <Field label="Members as" description="What each record's dimension fields carry, for matching this model's members.">
            <Select value={source.member_display ?? "code"} aria-label="Members as"
              onChange={e => patch({ member_display: e.target.value as ApiModelSource["member_display"] })}>
              <option value="code">Codes</option>
              <option value="label">Labels</option>
            </Select>
          </Field>
        </>
      )}
      <p className="mvx-admin-muted" style={{ margin: 0, fontSize: 12 }}>
        Each record is one combination of members with a value: a field for every dimension of the grid and for
        every metric chosen. Saving or activating the link makes you its owner: every run reads what you see in the
        source — whoever starts it, including a user pressing a dashboard button who has no access to the source
        model. If you stop being a developer of both models, the link stops until a developer of both activates it
        again. The source model&apos;s developers can switch it off.
      </p>
    </div>
  );
}

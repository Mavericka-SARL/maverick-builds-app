import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type PlanDef, type PlanLimits } from "../../api/client";
import { Button, Card, Checkbox, Field, InlineAlert, LoadingState, NumberInput, Textarea, TextInput } from "../../ui";

const LIMIT_FIELDS: { key: keyof PlanLimits; label: string }[] = [
  { key: "max_users", label: "Users" },
  { key: "max_applications", label: "Applications" },
  { key: "max_models", label: "Models" },
  { key: "max_metrics_per_model", label: "Metrics per model" },
  { key: "max_members_per_dimension", label: "Members per dimension" },
  { key: "max_fact_rows_per_model", label: "Data rows per model" },
  { key: "max_ai_messages_per_day", label: "AI messages per day" },
  { key: "max_integration_runs_per_day", label: "Integration runs per day" },
  { key: "max_storage_mb", label: "Storage (MB)" },
];

const EMPTY_LIMITS: PlanLimits = {
  max_users: 0, max_applications: 0, max_models: 0, max_metrics_per_model: 0,
  max_members_per_dimension: 0, max_fact_rows_per_model: 0, max_ai_messages_per_day: 0, max_integration_runs_per_day: 0,
  max_storage_mb: 0,
};

/**
 * Platform › Plans: the catalog every tenant's plan points at, kept by the
 * platform administrators for every tenant. A plan bounds how much a tenant
 * may use — never for how long: there is no trial. Limits are numbers here
 * rather than constants in code, so tuning the test workspace is an edit,
 * not a release. 0 means unlimited. A change applies to every tenant on the
 * plan within a minute; the read-only verdict for a tenant already over a
 * new limit follows at the next usage sweep.
 */
export function PlansTab() {
  const { data, isLoading, error } = useQuery({ queryKey: ["admin-plans"], queryFn: api.getPlans });
  if (isLoading) return <LoadingState label="Loading plans…" />;
  if (error) return <InlineAlert tone="danger">{(error as Error).message}</InlineAlert>;
  return (
    <div className="mvx-admin-stack" data-testid="plans-tab">
      <p className="mvx-admin-muted" style={{ marginTop: 0 }}>
        Plans apply to every tenant of this deployment; each tenant is on one (Applications › tenant › Change plan). A plan
        bounds how much a tenant may use, never for how long. A limit of 0 means unlimited. The first self-service plan is
        the one public sign-up assigns — the test workspace. Editions (community, commercial, enterprise) are a licence
        key, not a plan.
      </p>
      {(data ?? []).map((p) => <PlanCard key={p.key} plan={p} />)}
      <NewPlan />
    </div>
  );
}

function PlanCard({ plan }: { plan: PlanDef }) {
  const qc = useQueryClient();
  const [draft, setDraft] = useState<PlanDef | null>(null);
  const [saved, setSaved] = useState(false);
  const form = draft ?? plan;
  const set = (patch: Partial<PlanDef>) => setDraft({ ...form, ...patch });
  const setLimit = (key: keyof PlanLimits, value: number) => set({ limits: { ...form.limits, [key]: Math.max(0, Math.floor(value || 0)) } });
  const save = useMutation({
    mutationFn: () => api.updatePlan(plan.key, {
      name: form.name, description: form.description, self_service: form.self_service, limits: form.limits, limit_note: form.limit_note, sort_order: form.sort_order,
    }),
    onSuccess: () => { void qc.invalidateQueries({ queryKey: ["admin-plans"] }); setDraft(null); setSaved(true); setTimeout(() => setSaved(false), 2000); },
  });
  return (
    <Card>
      <div data-testid={`plan-${plan.key}`}>
        <div style={{ display: "flex", alignItems: "baseline", gap: 8, marginBottom: 8 }}>
          <span style={{ fontWeight: 700 }}>{plan.name}</span>
          <code className="mvx-admin-muted">{plan.key}</code>
        </div>
        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(180px, 1fr))", gap: 12, maxWidth: 900 }}>
          <Field label="Name"><TextInput value={form.name} onChange={(e) => set({ name: e.target.value })} aria-label={`Name of ${plan.key}`} /></Field>
          <Field label="Order"><NumberInput min={0} value={form.sort_order} onChange={(e) => set({ sort_order: Number(e.target.value) || 0 })} aria-label={`Order of ${plan.key}`} /></Field>
          <Field label="Description" ><TextInput value={form.description} onChange={(e) => set({ description: e.target.value })} aria-label={`Description of ${plan.key}`} /></Field>
        </div>
        <div style={{ margin: "10px 0" }}>
          <Checkbox checked={form.self_service} onChange={(e) => set({ self_service: e.target.checked })} label="Open to self-service sign-up" />
        </div>
        <div style={{ maxWidth: 900, marginBottom: 10 }}>
          <Field label="When a limit stops the tenant" description='Shown with every refusal instead of "Change the plan to add more." — where to go from here.'>
            <Textarea rows={2} value={form.limit_note} onChange={(e) => set({ limit_note: e.target.value })} aria-label={`Limit note of ${plan.key}`} />
          </Field>
        </div>
        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(150px, 1fr))", gap: 12, maxWidth: 900 }}>
          {LIMIT_FIELDS.map((f) => (
            <Field key={f.key} label={f.label}>
              <NumberInput min={0} value={form.limits[f.key]} onChange={(e) => setLimit(f.key, Number(e.target.value))} aria-label={`${f.label} limit of ${plan.key}`} />
            </Field>
          ))}
        </div>
        <div className="mvx-admin-inline-form" style={{ marginTop: 12 }}>
          <Button variant="primary" size="sm" loading={save.isPending} loadingLabel="Saving…" disabled={!draft} onClick={() => save.mutate()}>
            {saved ? "Saved" : "Save plan"}
          </Button>
          {draft && <Button size="sm" onClick={() => setDraft(null)}>Discard</Button>}
          {save.isError && <span className="mvx-admin-error">{(save.error as Error).message}</span>}
        </div>
      </div>
    </Card>
  );
}

function NewPlan() {
  const qc = useQueryClient();
  const [open, setOpen] = useState(false);
  const [key, setKey] = useState("");
  const [name, setName] = useState("");
  const create = useMutation({
    mutationFn: () => api.updatePlan(key.trim(), { name: name.trim(), description: "", self_service: false, limits: EMPTY_LIMITS, limit_note: "", sort_order: 100 }),
    onSuccess: () => { void qc.invalidateQueries({ queryKey: ["admin-plans"] }); setOpen(false); setKey(""); setName(""); },
  });
  if (!open) return <Button size="sm" variant="ghost" onClick={() => setOpen(true)} style={{ alignSelf: "flex-start" }}>New plan</Button>;
  return (
    <div className="mvx-admin-inline-form mvx-admin-inline-form--boxed">
      <TextInput value={key} onChange={(e) => setKey(e.target.value)} placeholder="key (e.g. team)" aria-label="New plan key" style={{ width: 160 }} autoFocus />
      <TextInput value={name} onChange={(e) => setName(e.target.value)} placeholder="Name" aria-label="New plan name" style={{ width: 200 }} />
      <Button variant="primary" size="sm" disabled={!key.trim() || !name.trim()} loading={create.isPending} loadingLabel="Creating…" onClick={() => create.mutate()}>Create</Button>
      <Button size="sm" onClick={() => setOpen(false)}>Cancel</Button>
      {create.isError && <span className="mvx-admin-error">{(create.error as Error).message}</span>}
    </div>
  );
}

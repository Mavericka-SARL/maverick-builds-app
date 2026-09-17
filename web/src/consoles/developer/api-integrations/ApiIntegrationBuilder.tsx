import { useEffect, useMemo, useRef, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type ApiIntegrationConfig, type ApiIntegrationDetail, type ApiSchedule, type IntegrationConnection } from "../../../api/client";
import { Button, InlineAlert, Stepper, useUnsavedGuard } from "../../../ui";
import { WIZARD_STEPS, type WizardStepID, defaultConfig, defaultSchedule, requestCriticalKey } from "./apiIntegrationTypes";
import type { InferredField } from "./integrationMapping";
import { BasicsStep } from "./BasicsStep";
import { RequestStep } from "./RequestStep";
import { AuthStep } from "./AuthStep";
import { ResponseStep } from "./ResponseStep";
import { MappingStep } from "./MappingStep";
import { ScheduleStep } from "./ScheduleStep";

// The six-step wizard. Drafts save from every step; activation stays
// disabled until validation + a successful test of the CURRENT request
// configuration + mapping pass (the server enforces all three — this UI
// surfaces the same gates without duplicating policy).
export function ApiIntegrationBuilder({
  existing, revisionId, onClose,
}: {
  existing: ApiIntegrationDetail | null;
  revisionId?: string;
  onClose: () => void;
}) {
  const qc = useQueryClient();
  const [step, setStep] = useState<WizardStepID>("basics");
  const [id, setID] = useState<string | null>(existing?.id ?? null);
  const [name, setName] = useState(existing?.name ?? "");
  const [description, setDescription] = useState(existing?.description ?? "");
  const [tags, setTags] = useState((existing?.tags ?? []).join(", "));
  const [connectionId, setConnectionId] = useState(existing?.connection_id ?? "");
  const [config, setConfig] = useState<ApiIntegrationConfig>(existing?.config ?? defaultConfig());
  const [schedule, setSchedule] = useState<ApiSchedule>(existing?.schedule ?? defaultSchedule());
  const [serverTested, setServerTested] = useState(existing?.tested ?? false);
  const [inferred, setInferred] = useState<InferredField[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [dirty, setDirty] = useState(false);
  const stepErrRef = useRef<HTMLDivElement>(null);

  useUnsavedGuard(dirty);

  // Request-critical edits invalidate the previous successful test locally
  // (the server's ConfigHash gate is authoritative; this mirrors it live).
  const [savedCritical, setSavedCritical] = useState(existing?.config ? requestCriticalKey(existing.config) : "");
  const criticalNow = requestCriticalKey(config);
  const tested = serverTested && criticalNow === savedCritical;

  const { data: connections = [] } = useQuery({ queryKey: ["integration-connections"], queryFn: () => api.listIntegrationConnections() });
  const connectionName = (connections as IntegrationConnection[]).find(c => c.id === connectionId)?.name;

  const onConfig = (patch: Partial<ApiIntegrationConfig>) => {
    setConfig(c => ({ ...c, ...patch }));
    setDirty(true);
  };

  useEffect(() => {
    if (error) stepErrRef.current?.focus();
  }, [error]);

  const body = useMemo(() => ({
    name,
    description,
    tags: tags.split(",").map(t => t.trim()).filter(Boolean),
    connection_id: connectionId,
    config,
    schedule,
  }), [name, description, tags, connectionId, config, schedule]);

  // saveDraft persists from any step; returns the id (creating on first use).
  const saveDraft = async (): Promise<string | null> => {
    setSaving(true);
    setError(null);
    try {
      let saved: ApiIntegrationDetail;
      if (id) {
        saved = await api.updateApiIntegration(id, body);
      } else {
        if (!name.trim() || !config.target_id) {
          setError("Name and a target are required before saving.");
          return null;
        }
        saved = await api.createApiIntegration({ ...body, status: "draft" });
        setID(saved.id);
      }
      setSavedCritical(saved.config ? requestCriticalKey(saved.config) : criticalNow);
      setServerTested(saved.tested);
      setDirty(false);
      qc.invalidateQueries({ queryKey: ["api-integrations"] });
      return saved.id;
    } catch (e) {
      setError((e as Error).message);
      return null;
    } finally {
      setSaving(false);
    }
  };

  const activate = async () => {
    const savedID = await saveDraft();
    if (!savedID) return;
    setSaving(true);
    try {
      const v = await api.validateApiIntegration(savedID);
      if (!v.valid) {
        setError("Validation failed: " + v.errors.join("; "));
        return;
      }
      await api.updateApiIntegration(savedID, { status: "active" });
      qc.invalidateQueries({ queryKey: ["api-integrations"] });
      onClose();
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setSaving(false);
    }
  };

  const idx = WIZARD_STEPS.findIndex(s => s.id === step);

  return (
    <div className="mvx-api-builder">
      <Stepper steps={WIZARD_STEPS.map(s => ({ id: s.id, label: s.label }))} current={step} />
      <nav aria-label="Wizard steps" style={{ display: "flex", gap: 6, flexWrap: "wrap", marginTop: 8 }}>
        {WIZARD_STEPS.map(s => (
          <Button key={s.id} size="sm" variant={s.id === step ? "primary" : "secondary"}
            aria-pressed={s.id === step} onClick={() => setStep(s.id)}>
            {s.label}
          </Button>
        ))}
      </nav>
      {error && (
        <div ref={stepErrRef} tabIndex={-1} role="alert" style={{ outline: "none", margin: "8px 0" }}>
          <InlineAlert tone="danger">{error}</InlineAlert>
        </div>
      )}

      <div style={{ margin: "16px 0" }}>
        {step === "basics" && (
          <BasicsStep name={name} setName={v => { setName(v); setDirty(true); }}
            description={description} setDescription={v => { setDescription(v); setDirty(true); }}
            tags={tags} setTags={v => { setTags(v); setDirty(true); }}
            config={config} onConfig={onConfig} revisionId={revisionId} />
        )}
        {step === "request" && <RequestStep config={config} onConfig={onConfig} />}
        {step === "auth" && (
          <AuthStep config={config} onConfig={onConfig}
            connectionId={connectionId} setConnectionId={v => { setConnectionId(v); setDirty(true); }} />
        )}
        {step === "response" && (
          <ResponseStep config={config} onConfig={onConfig} integrationId={id} tested={tested}
            onTestOutcome={ok => { if (ok) { setServerTested(true); setSavedCritical(criticalNow); } }}
            onFieldsInferred={f => setInferred(f)} saveDraft={saveDraft} />
        )}
        {step === "mapping" && (
          <MappingStep config={config} onConfig={onConfig} revisionId={revisionId}
            inferred={inferred} integrationId={id} saveDraft={saveDraft} />
        )}
        {step === "schedule" && (
          <ScheduleStep config={config} onConfig={onConfig} schedule={schedule}
            onSchedule={s => { setSchedule(s); setDirty(true); }} tested={tested}
            name={name} connectionName={connectionName} />
        )}
      </div>

      <div style={{ display: "flex", gap: 8, alignItems: "center", borderTop: "1px solid var(--color-border)", paddingTop: 12 }}>
        <Button variant="secondary" onClick={() => setStep(WIZARD_STEPS[Math.max(0, idx - 1)].id)} disabled={idx === 0}>
          Back
        </Button>
        <Button variant="secondary" onClick={() => setStep(WIZARD_STEPS[Math.min(WIZARD_STEPS.length - 1, idx + 1)].id)} disabled={idx === WIZARD_STEPS.length - 1}>
          Next
        </Button>
        <span style={{ flex: 1 }} />
        <Button variant="secondary" onClick={saveDraft} loading={saving} loadingLabel="Saving…">Save draft</Button>
        <Button onClick={activate} disabled={!tested || saving}
          title={tested ? "" : "Activation requires a successful test of the current configuration"}>
          Activate
        </Button>
        <Button variant="secondary" onClick={onClose}>Close</Button>
      </div>
    </div>
  );
}

import { useQuery } from "@tanstack/react-query";
import { api, type ApiIntegrationConfig, type ApiSchedule } from "../../../api/client";
import { Field, InlineAlert, NumberInput, Select, StatusBadge, Switch, TextInput } from "../../../ui";

const COMMON_TIMEZONES = [
  "UTC", "Europe/Berlin", "Europe/London", "Europe/Moscow", "America/New_York",
  "America/Chicago", "America/Los_Angeles", "Asia/Tokyo", "Asia/Singapore", "Australia/Sydney",
];

// Step 6 — run & schedule + the review summary. Daily/weekly compile to a
// cron expression; advanced mode exposes the five fields directly.
export function ScheduleStep({
  config, onConfig, schedule, onSchedule, tested, name, connectionName,
}: {
  config: ApiIntegrationConfig;
  onConfig: (patch: Partial<ApiIntegrationConfig>) => void;
  schedule: ApiSchedule;
  onSchedule: (s: ApiSchedule) => void;
  tested: boolean;
  name: string;
  connectionName?: string;
}) {
  const mode = schedule.kind === "manual" ? "manual"
    : schedule.kind === "interval" ? "interval"
    : /^0 \d+ \* \* \*$/.test(schedule.cron_expr ?? "") ? "daily"
    : /^0 \d+ \* \* [0-6]$/.test(schedule.cron_expr ?? "") ? "weekly"
    : "cron";

  const setMode = (m: string) => {
    switch (m) {
      case "manual": onSchedule({ ...schedule, kind: "manual", enabled: false }); break;
      case "interval": onSchedule({ ...schedule, kind: "interval", interval_seconds: schedule.interval_seconds || 3600 }); break;
      case "daily": onSchedule({ ...schedule, kind: "cron", cron_expr: "0 6 * * *" }); break;
      case "weekly": onSchedule({ ...schedule, kind: "cron", cron_expr: "0 6 * * 1" }); break;
      case "cron": onSchedule({ ...schedule, kind: "cron", cron_expr: schedule.cron_expr || "0 * * * *" }); break;
    }
  };

  const limits = config.limits ?? {};
  const patchLimits = (p: Partial<NonNullable<ApiIntegrationConfig["limits"]>>) => onConfig({ limits: { ...limits, ...p } });
  const pag = config.pagination ?? { mode: "none" as const };

  const { data: grids = [] } = useQuery({ queryKey: ["dev-grids-review"], queryFn: () => api.listGrids(), enabled: config.target_type === "grid" });
  const targetName = config.target_type === "grid"
    ? (grids as { id: string; name: string }[]).find(g => g.id === config.target_id)?.name ?? config.target_id
    : config.target_id;

  return (
    <div style={{ display: "grid", gap: 12, maxWidth: 640 }}>
      <Field label="Execution">
        <Select value={mode} aria-label="Execution mode" onChange={e => setMode(e.target.value)}>
          <option value="manual">Manual only</option>
          <option value="interval">Interval</option>
          <option value="daily">Daily</option>
          <option value="weekly">Weekly</option>
          <option value="cron">Advanced (cron)</option>
        </Select>
      </Field>
      {mode === "interval" && (
        <Field label="Every (seconds)" description="minimum 60">
          <NumberInput value={schedule.interval_seconds ?? 3600} min={60} max={86400 * 7}
            onChange={e => onSchedule({ ...schedule, interval_seconds: Number(e.target.value) || 0 })} aria-label="Interval seconds" />
        </Field>
      )}
      {(mode === "daily" || mode === "weekly" || mode === "cron") && (
        <Field label="Cron expression" description="five fields: minute hour day-of-month month day-of-week">
          <TextInput value={schedule.cron_expr ?? ""} onChange={e => onSchedule({ ...schedule, cron_expr: e.target.value })}
            aria-label="Cron expression" spellCheck={false} />
        </Field>
      )}
      {schedule.kind !== "manual" && (
        <>
          <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12 }}>
            <Field label="Timezone (IANA)">
              <TextInput value={schedule.timezone ?? "UTC"} list="tz-list"
                onChange={e => onSchedule({ ...schedule, timezone: e.target.value })} aria-label="Timezone" />
              <datalist id="tz-list">{COMMON_TIMEZONES.map(tz => <option key={tz} value={tz} />)}</datalist>
            </Field>
            <Field label="Overlap policy" description="what happens if the previous run is still going">
              <Select value={schedule.overlap_policy ?? "skip"} aria-label="Overlap policy"
                onChange={e => onSchedule({ ...schedule, overlap_policy: e.target.value })}>
                <option value="skip">Skip this tick (default)</option>
                <option value="queue">Queue behind it</option>
              </Select>
            </Field>
          </div>
          <label style={{ display: "flex", gap: 8, alignItems: "center", fontSize: 13 }}>
            <Switch checked={schedule.enabled} aria-label="Schedule enabled"
              onChange={checked => onSchedule({ ...schedule, enabled: checked })} />
            Schedule enabled
          </label>
        </>
      )}

      <h4 style={{ margin: "8px 0 0" }}>Limits</h4>
      <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(150px, 1fr))", gap: 12 }}>
        <Field label="Max pages">
          <NumberInput value={pag.max_pages ?? 0} min={0} max={1000} aria-label="Max pages"
            onChange={e => onConfig({ pagination: { ...pag, max_pages: Number(e.target.value) || 0 } })} />
        </Field>
        <Field label="Max records">
          <NumberInput value={limits.max_records ?? 0} min={0} max={1000000} aria-label="Max records"
            onChange={e => patchLimits({ max_records: Number(e.target.value) || 0 })} />
        </Field>
        <Field label="Max requests">
          <NumberInput value={limits.max_requests ?? 0} min={0} max={10000} aria-label="Max requests"
            onChange={e => patchLimits({ max_requests: Number(e.target.value) || 0 })} />
        </Field>
        <Field label="Failure threshold" description="abort after this many bad records">
          <NumberInput value={limits.failure_threshold ?? 0} min={0} max={100000} aria-label="Failure threshold"
            onChange={e => patchLimits({ failure_threshold: Number(e.target.value) || 0 })} />
        </Field>
      </div>

      <h4 style={{ margin: "8px 0 0" }}>Review</h4>
      <div className="mvx-admin-object" style={{ padding: 12, fontSize: 13, display: "grid", gap: 4 }}>
        <div><strong>{name || "(unnamed)"}</strong> · {config.direction === "pull" ? "Pull into" : "Push from"} {config.target_type} “{targetName}”</div>
        <div>{config.request.method} {sanitize(config.request.url)}</div>
        <div>Auth: {config.auth.type}{connectionName ? ` via “${connectionName}”` : ""} · Pagination: {pag.mode ?? "none"}</div>
        <div>Schedule: {schedule.kind === "manual" ? "manual only" : schedule.kind === "interval" ? `every ${schedule.interval_seconds}s` : schedule.cron_expr} ({schedule.timezone ?? "UTC"}){schedule.enabled ? "" : " — disabled"}</div>
        <div>
          {tested
            ? <StatusBadge tone="success">Request tested ✓</StatusBadge>
            : <StatusBadge tone="warning">Request not tested — activation stays disabled</StatusBadge>}
        </div>
      </div>
      {config.direction === "push" && (
        <InlineAlert tone="warning">
          This integration sends data to an external system. Every run performs real
          {" "}{config.request.method} requests against {sanitize(config.request.url)}.
        </InlineAlert>
      )}
      {config.direction === "pull" && config.request.method !== "GET" && (
        <InlineAlert tone="warning">
          Pulling with {config.request.method} may change data on the remote system on every run.
        </InlineAlert>
      )}
    </div>
  );
}

function sanitize(url: string): string {
  try {
    const u = new URL(url);
    return `${u.protocol}//${u.host}${u.pathname}`;
  } catch {
    return url;
  }
}

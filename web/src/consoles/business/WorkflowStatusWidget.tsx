import { useQuery } from "@tanstack/react-query";
import { api, type WorkflowStatusStep } from "../../api/client";
import { LoadingState } from "../../ui";

// The platform roles a step names, as people read them; a business role
// keeps its own name.
const ROLE_NAMES: Record<string, string> = {
  business_admin: "Business admin", business_user: "Business users", approver: "Approver",
  developer: "Developer", tenant_admin: "Tenant admin",
};
const roleName = (r: string) => ROLE_NAMES[r] ?? r;

const DAY = 24 * 3600 * 1000;
function days(n: number): string {
  return n === 1 ? "1 day" : `${n} days`;
}
function since(iso?: string): string {
  if (!iso) return "";
  const d = Math.floor((Date.now() - new Date(iso).getTime()) / DAY);
  return d <= 0 ? "since today" : `for ${days(d)}`;
}
function due(iso?: string): { text: string; late: boolean } | null {
  if (!iso) return null;
  const d = Math.ceil((new Date(iso).getTime() - Date.now()) / DAY);
  if (d < 0) return { text: `overdue by ${days(-d)}`, late: true };
  return { text: d === 0 ? "due today" : `due in ${days(d)}`, late: false };
}
const date = (iso: string) => new Date(iso).toLocaleDateString(undefined, { day: "numeric", month: "short", year: "numeric" });

const MARK: Record<WorkflowStatusStep["state"], string> = { done: "✓", current: "●", sent_back: "↺", skipped: "–", pending: "○" };

// Where the latest run of one workflow stands (GET /api/workflow/status):
// every step a person acts on, in order, the current one with its roles,
// how long it has waited and when it is due. Names no person.
export function WorkflowStatusWidget({ name }: { name: string }) {
  const { data, isLoading, error } = useQuery({
    queryKey: ["workflow-status", name],
    queryFn: () => api.workflowStatus(name),
    refetchInterval: 30_000,
  });
  if (isLoading) return <LoadingState />;
  if (error || !data) return <p className="mvx-admin-muted">The workflow's status could not be read.</p>;
  if (!data.found) return <p className="mvx-admin-muted">No published workflow named “{name}”.</p>;
  const inst = data.instance;
  const headline = !inst ? "Not started yet"
    : inst.status === "running" ? `Running since ${date(inst.started_at)}`
    : inst.status === "completed" ? `Completed on ${date(inst.completed_at ?? inst.started_at)}`
    : inst.status === "cancelled" ? `Cancelled — started ${date(inst.started_at)}`
    : `${inst.status[0].toUpperCase()}${inst.status.slice(1)} — started ${date(inst.started_at)}`;
  return (
    <section className="mvx-wfstatus" aria-label={`${data.workflow} status`}>
      <header className="mvx-wfstatus__head">
        <strong>{data.workflow}</strong>
        <span className="mvx-admin-muted">{headline}</span>
      </header>
      <ol className="mvx-wfstatus__steps">
        {data.steps.map(s => {
          const d = s.state === "current" ? due(s.due_at) : null;
          return (
            <li key={s.id} className={`mvx-wfstatus__step mvx-wfstatus__step--${s.state}`} aria-current={s.state === "current" ? "step" : undefined}>
              <span className="mvx-wfstatus__mark" aria-hidden>{MARK[s.state]}</span>
              <span className="mvx-wfstatus__name">{s.name}</span>
              {s.state === "current" && (
                <span className="mvx-wfstatus__meta">
                  {s.roles.map(roleName).join(", ")} · waiting {since(s.since)}
                  {d && <> · <span className={d.late ? "mvx-wfstatus__late" : undefined}>{d.text}</span></>}
                </span>
              )}
              {s.state === "sent_back" && <span className="mvx-wfstatus__meta">sent back</span>}
            </li>
          );
        })}
      </ol>
    </section>
  );
}

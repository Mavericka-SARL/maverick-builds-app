import { useQuery } from "@tanstack/react-query";
import { api } from "../api/client";
import { InlineAlert } from "../ui";

/**
 * One line at the top of the console for a tenant on a plan that needs
 * saying: a trial and how long is left, or a workspace that has become
 * read-only (trial ended, over a limit) and why. Nothing for everyone else.
 * The wording is the server's (GET /api/me → plan), so the console never
 * explains a state differently from the API that refused the request.
 */
export function PlanBanner() {
  const { data: me } = useQuery({ queryKey: ["me-plan"], queryFn: api.getMe, staleTime: 60_000 });
  const st = me?.plan;
  if (!st) return null;
  const change = me?.contact_url ? <> <a href={me.contact_url} target="_blank" rel="noreferrer">Change plan</a></> : null;
  if (st.read_only) {
    return (
      <div data-testid="plan-banner" data-state={st.code} style={{ marginBottom: 12 }}>
        <InlineAlert tone="danger">{st.reason}{change}</InlineAlert>
      </div>
    );
  }
  if (st.trial) {
    const days = st.days_left === 1 ? "1 day" : `${st.days_left} days`;
    return (
      <div data-testid="plan-banner" data-state="trial" style={{ marginBottom: 12 }}>
        <InlineAlert tone="info">{st.plan.name} · {days} left.{change}</InlineAlert>
      </div>
    );
  }
  return null;
}

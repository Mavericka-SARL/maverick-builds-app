import { useQuery } from "@tanstack/react-query";
import { api } from "../api/client";
import { InlineAlert } from "../ui";

/**
 * One line at the top of the console for a workspace that has become
 * read-only (over a limit of its plan) and why. Nothing for everyone else.
 * The wording is the server's (GET /api/me → plan), so the console never
 * explains a state differently from the API that refused the request.
 */
export function PlanBanner() {
  const { data: me } = useQuery({ queryKey: ["me-plan"], queryFn: api.getMe, staleTime: 60_000 });
  const st = me?.plan;
  if (!st) return null;
  // The reason already says where to go from here when the plan has a
  // note; the link then just leads there, without promising another plan.
  const change = me?.contact_url ? <> <a href={me.contact_url} target="_blank" rel="noreferrer">{st.plan.limit_note ? "Learn more" : "Change plan"}</a></> : null;
  if (st.read_only) {
    return (
      <div data-testid="plan-banner" data-state={st.code} style={{ marginBottom: 12 }}>
        <InlineAlert tone="danger">{st.reason}{change}</InlineAlert>
      </div>
    );
  }
  return null;
}

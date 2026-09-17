import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type ApiRun } from "../../../api/client";
import { Button, EmptyState, LoadingState, StatusBadge } from "../../../ui";

const TONE: Record<ApiRun["status"], "brand" | "success" | "warning" | "danger"> = {
  queued: "brand", running: "brand", success: "success",
  partial: "warning", failed: "danger", cancelled: "warning",
};

export function ApiRunHistory({ integrationId }: { integrationId: string }) {
  const qc = useQueryClient();
  const { data: runs, isPending } = useQuery({
    queryKey: ["api-integration-runs", integrationId],
    queryFn: () => api.listApiIntegrationRuns(integrationId),
    // Poll only while something is active — the spec's "poll run details
    // only while queued or running", applied at list granularity.
    refetchInterval: q => {
      const list = q.state.data as ApiRun[] | undefined;
      return list?.some(r => r.status === "queued" || r.status === "running") ? 1500 : false;
    },
  });

  if (isPending) return <LoadingState label="Loading run history…" />;
  if (!runs || runs.length === 0) return <EmptyState label="No runs yet." />;

  return (
    <div className="mvx-table-wrap" aria-live="polite">
      <table className="mvx-table">
        <thead>
          <tr>
            <th>Status</th><th>Trigger</th><th>Started</th><th>Duration</th><th>HTTP</th>
            <th>Pages</th><th>Requests</th><th>Retries</th><th>Read</th><th>Written</th><th>Skipped</th>
            <th>Error</th><th></th>
          </tr>
        </thead>
        <tbody>
          {runs.map(r => (
            <tr key={r.id}>
              <td><StatusBadge tone={TONE[r.status]}>{r.status}{r.dry_run ? " (dry)" : ""}</StatusBadge></td>
              <td>{r.trigger_type}</td>
              <td>{r.started_at ? new Date(r.started_at).toLocaleString() : new Date(r.created_at).toLocaleString()}</td>
              <td>{r.duration_ms} ms</td>
              <td>{r.http_status || "—"}</td>
              <td>{r.pages}</td>
              <td>{r.requests}</td>
              <td>{r.retries}</td>
              <td>{r.records_read}</td>
              <td>{r.records_written}</td>
              <td>{r.records_skipped}</td>
              <td style={{ maxWidth: 220 }}>
                {r.error_code && <StatusBadge tone="danger">{r.error_code}</StatusBadge>}
                {r.message && <span className="mvx-admin-muted" style={{ fontSize: 12, marginLeft: 4 }}>{r.message}</span>}
              </td>
              <td>
                {(r.status === "queued" || r.status === "running") && (
                  <Button variant="secondary" size="sm" onClick={async () => {
                    await api.cancelApiIntegrationRun(r.id);
                    qc.invalidateQueries({ queryKey: ["api-integration-runs", integrationId] });
                  }}>Cancel</Button>
                )}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

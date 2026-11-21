import { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { api, type ModelCheckFinding } from "../../api/client";
import { Button, EmptyState, ErrorState, InlineAlert, LoadingState, StatusBadge, TextInput, Toolbar, ToolbarGroup, Txt } from "../../ui";
import { objectLabel, SEVERITY_TONE, useSaveWarnings } from "./saveWarnings";

// Developer Models › Model check: what in the revision is legal but costs time
// later (internal/modelcheck), most serious first. A finding the developer
// decides to keep is ignored with a reason; the revision's copies and
// exports keep the ignore.
export function ModelCheckTab({ revisionId }: { revisionId?: string }) {
  const qc = useQueryClient();
  const [showIgnored, setShowIgnored] = useState(false);
  const { data, isLoading, error, refetch, isFetching } = useQuery({
    queryKey: ["model-check", revisionId],
    queryFn: () => api.getModelCheck(revisionId),
    enabled: !!revisionId,
  });
  const refresh = () => qc.invalidateQueries({ queryKey: ["model-check", revisionId] });
  if (!revisionId) return <EmptyState label="Choose a revision to check (Developer Models)." />;
  if (isLoading) return <LoadingState />;
  if (error) return <ErrorState message={(error as Error).message} />;
  const findings = data?.findings ?? [];
  const open = findings.filter(f => !f.ignored);
  const ignored = findings.filter(f => f.ignored);
  const counts = data?.counts ?? {};
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 12, maxWidth: 920 }}>
      <p className="mvx-admin-muted" style={{ margin: 0, fontSize: 13 }}>
        <Txt>What in this revision is allowed but costs time later: a field that posts nothing, a total worked out from blanks, a SUMIFS adding members that are never added up, member lists that miss new members, hidden members that blank figures, a schedule that cannot start.</Txt>
      </p>
      <Toolbar>
        <ToolbarGroup>
          <StatusBadge tone="danger">{`${counts.error ?? 0} errors`}</StatusBadge>
          <StatusBadge tone="warning">{`${counts.warning ?? 0} warnings`}</StatusBadge>
          <StatusBadge tone="info">{`${counts.info ?? 0} to look at`}</StatusBadge>
          {ignored.length > 0 && (
            <Button size="sm" variant="ghost" onClick={() => setShowIgnored(s => !s)}>
              {showIgnored ? "Hide ignored" : `Show ignored (${ignored.length})`}
            </Button>
          )}
        </ToolbarGroup>
        <ToolbarGroup>
          <Button size="sm" onClick={() => refetch()} loading={isFetching} loadingLabel="Checking…">Check again</Button>
        </ToolbarGroup>
      </Toolbar>
      {open.length === 0 && <EmptyState label="Nothing to look at: the revision passes every check." />}
      {open.map(f => <FindingCard key={f.key} f={f} revisionId={revisionId} onChange={refresh} />)}
      {showIgnored && ignored.map(f => <FindingCard key={f.key} f={f} revisionId={revisionId} onChange={refresh} />)}
    </div>
  );
}

function FindingCard({ f, revisionId, onChange }: { f: ModelCheckFinding; revisionId: string; onChange: () => void }) {
  const [reason, setReason] = useState<string | null>(null);
  const ignore = useMutation({
    mutationFn: () => api.ignoreModelCheckFinding(revisionId, f.key, reason ?? ""),
    onSuccess: () => { setReason(null); onChange(); },
  });
  const unignore = useMutation({ mutationFn: () => api.unignoreModelCheckFinding(revisionId, f.key), onSuccess: onChange });
  return (
    <div role="article" aria-label={f.title} style={{ border: "1px solid var(--color-border)", borderRadius: 8, padding: "10px 12px", opacity: f.ignored ? 0.7 : 1 }}>
      <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
        <StatusBadge tone={SEVERITY_TONE[f.severity]}>{f.severity}</StatusBadge>
        <strong style={{ fontSize: 14 }}>{f.title}</strong>
        <span className="mvx-admin-muted" style={{ fontSize: 12 }}>{objectLabel(f)}</span>
      </div>
      <p style={{ margin: "6px 0 4px", fontSize: 13 }}>{f.message}</p>
      <p style={{ margin: 0, fontSize: 13 }}><strong><Txt>Fix:</Txt></strong> {f.fix}</p>
      {f.ignored ? (
        <div style={{ display: "flex", gap: 8, alignItems: "center", marginTop: 8, flexWrap: "wrap" }}>
          <span className="mvx-admin-muted" style={{ fontSize: 12 }}>
            {`Ignored${f.ignored_by ? ` by ${f.ignored_by}` : ""}: ${f.ignore_reason ?? ""}`}
          </span>
          <Button size="sm" variant="ghost" onClick={() => unignore.mutate()} loading={unignore.isPending}>Bring back</Button>
        </div>
      ) : reason === null ? (
        <div style={{ marginTop: 8 }}>
          <Button size="sm" variant="ghost" onClick={() => setReason("")}>Ignore…</Button>
        </div>
      ) : (
        <div style={{ display: "flex", gap: 8, alignItems: "center", marginTop: 8, flexWrap: "wrap" }}>
          <div style={{ flex: "1 1 260px" }}>
            <TextInput aria-label="Why it is kept" placeholder="Why it is kept (required)" value={reason} onChange={e => setReason(e.target.value)} />
          </div>
          <Button size="sm" variant="primary" disabled={reason.trim() === ""} loading={ignore.isPending} onClick={() => ignore.mutate()}>Ignore</Button>
          <Button size="sm" variant="ghost" onClick={() => setReason(null)}>Cancel</Button>
        </div>
      )}
      {(ignore.isError || unignore.isError) && (
        <div style={{ marginTop: 6 }}>
          <InlineAlert tone="danger">{((ignore.error ?? unignore.error) as Error).message}</InlineAlert>
        </div>
      )}
    </div>
  );
}

/** The findings a save answered with, above a list, until dismissed. */
export function SaveWarnings() {
  const { warnings, clear } = useSaveWarnings();
  if (warnings.length === 0) return null;
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 6, marginBottom: 12 }}>
      {warnings.map(f => (
        <InlineAlert key={f.key} tone={f.severity === "error" ? "danger" : f.severity === "warning" ? "warning" : "info"}>
          <strong>{f.title}</strong> — {f.message} <strong><Txt>Fix:</Txt></strong> {f.fix}
        </InlineAlert>
      ))}
      <div><Button size="sm" variant="ghost" onClick={clear}>Dismiss</Button></div>
    </div>
  );
}

/** Developer Models › Model check: the working revision's counts, the findings opened below on demand. */
export function ModelCheckSection({ revisionId, revisionName }: { revisionId: string; revisionName: string }) {
  const [open, setOpen] = useState(false);
  const { data, isLoading } = useQuery({ queryKey: ["model-check", revisionId], queryFn: () => api.getModelCheck(revisionId) });
  const c = data?.counts ?? {};
  const issues = (c.error ?? 0) + (c.warning ?? 0) + (c.info ?? 0);
  return (
    <section aria-label="Model check" className="mvx-admin-object" style={{ padding: "10px 12px" }}>
      <div style={{ display: "flex", alignItems: "center", gap: 8, flexWrap: "wrap" }}>
        <strong style={{ fontSize: 14 }}><Txt>Model check</Txt></strong>
        <span className="mvx-admin-muted" style={{ fontSize: 12 }}>{revisionName}</span>
        {isLoading ? <span className="mvx-admin-muted" style={{ fontSize: 12 }}><Txt>checking…</Txt></span> : issues === 0 ? (
          <StatusBadge tone="success">{(c.ignored ?? 0) > 0 ? `passes (${c.ignored} ignored)` : "passes"}</StatusBadge>
        ) : (
          <>
            {(c.error ?? 0) > 0 && <StatusBadge tone="danger">{`${c.error} errors`}</StatusBadge>}
            {(c.warning ?? 0) > 0 && <StatusBadge tone="warning">{`${c.warning} warnings`}</StatusBadge>}
            {(c.info ?? 0) > 0 && <StatusBadge tone="info">{`${c.info} to look at`}</StatusBadge>}
          </>
        )}
        <Button size="sm" variant="ghost" onClick={() => setOpen(o => !o)}>{open ? "Hide" : "Show the findings"}</Button>
      </div>
      {open && <div style={{ marginTop: 10 }}><ModelCheckTab revisionId={revisionId} /></div>}
    </section>
  );
}

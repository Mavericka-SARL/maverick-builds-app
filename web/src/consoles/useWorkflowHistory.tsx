import { useMemo, useState } from "react";
import { useInfiniteQuery } from "@tanstack/react-query";
import type { HistoryInstance } from "../api/client";
import { Button, SearchInput, Select, Toolbar, ToolbarGroup, Txt } from "../ui";
import { matchesAllWords } from "./business/taskSearch";
import { HISTORY_PAGE_SIZE, currentStages, distinctValues, historySearchText } from "./historySearch";
import { useDateFormat } from "./dateFormat";
import { momentDate } from "./dateSerial";

const STATUSES = ["running", "completed", "cancelled", "reopened", "failed"];
const FILTER_STYLE = { width: 170, flex: "0 0 auto" } as const;

/**
 * A Workflow History's runs, a page at a time (newest first; "Load older
 * runs" asks for the next), and its search: words over everything a card
 * shows (historySearch.ts) plus status, model, submitter and the stage a run
 * waits at. The same in a person's own history and a business admin's.
 * A filter whose choices would be one value is not shown.
 */
export function useWorkflowHistory(queryKey: string, fetchPage: (before?: string) => Promise<HistoryInstance[]>) {
  const dateFormat = useDateFormat();
  const pages = useInfiniteQuery({
    queryKey: [queryKey],
    queryFn: ({ pageParam }) => fetchPage(pageParam || undefined),
    initialPageParam: "",
    getNextPageParam: (last: HistoryInstance[]) =>
      last.length >= HISTORY_PAGE_SIZE ? last[last.length - 1].id : undefined,
    refetchInterval: 10_000,
  });
  // A run started between two pages' reads can show up in both.
  const instances = useMemo(() => {
    const seen = new Set<string>();
    return (pages.data?.pages ?? []).flat().filter((i) => !seen.has(i.id) && !!seen.add(i.id));
  }, [pages.data]);

  const [query, setQuery] = useState("");
  const [status, setStatus] = useState("");
  const [model, setModel] = useState("");
  const [submitter, setSubmitter] = useState("");
  const [stage, setStage] = useState("");
  // The chosen value stays a choice when the runs that had it scroll out.
  const models = useMemo(() => distinctValues([...instances.map((i) => i.model_name), model]), [instances, model]);
  const submitters = useMemo(() => distinctValues([...instances.map((i) => i.submitted_by), submitter]), [instances, submitter]);
  const stages = useMemo(() => distinctValues([...instances.flatMap(currentStages), stage]), [instances, stage]);

  const shown = useMemo(() => instances.filter((i) =>
    (!status || i.status === status)
    && (!model || i.model_name === model)
    && (!submitter || i.submitted_by === submitter)
    && (!stage || currentStages(i).includes(stage))
    && matchesAllWords(historySearchText(i, (d) => momentDate(d, dateFormat)), query),
  ), [instances, status, model, submitter, stage, query, dateFormat]);
  const filtering = !!(query.trim() || status || model || submitter || stage);

  const toolbar = (
    <Toolbar>
      <ToolbarGroup>
        <SearchInput placeholder="Search by workflow, submitter, stage, model, status…" value={query}
          onChange={(e) => setQuery(e.target.value)} width={340} aria-label="Search workflow history" />
        <Select value={status} onChange={(e) => setStatus(e.target.value)} style={FILTER_STYLE} aria-label="Filter by status">
          <option value="">All statuses</option>
          {STATUSES.map((s) => <option key={s} value={s}>{s[0].toUpperCase() + s.slice(1)}</option>)}
        </Select>
        {stages.length > 0 && (
          <Select value={stage} onChange={(e) => setStage(e.target.value)} style={FILTER_STYLE} aria-label="Filter by stage">
            <option value="">Any stage</option>
            {stages.map((s) => <option key={s} value={s}>Waiting at: {s}</option>)}
          </Select>
        )}
        {models.length > 1 && (
          <Select value={model} onChange={(e) => setModel(e.target.value)} style={FILTER_STYLE} aria-label="Filter by model">
            <option value="">All models</option>
            {models.map((m) => <option key={m} value={m}>{m}</option>)}
          </Select>
        )}
        {submitters.length > 1 && (
          <Select value={submitter} onChange={(e) => setSubmitter(e.target.value)} style={FILTER_STYLE} aria-label="Filter by submitter">
            <option value="">Anyone submitted</option>
            {submitters.map((s) => <option key={s} value={s}>Submitted by {s}</option>)}
          </Select>
        )}
        {filtering && (
          <>
            <span className="mvx-admin-muted">{shown.length} of {instances.length}<Txt>{pages.hasNextPage ? "+" : ""}</Txt></span>
            <Button size="sm" variant="ghost" onClick={() => { setQuery(""); setStatus(""); setModel(""); setSubmitter(""); setStage(""); }}>
              Clear
            </Button>
          </>
        )}
      </ToolbarGroup>
    </Toolbar>
  );

  // Older runs are a page away: a search that finds nothing among the runs
  // read so far says so, rather than "nothing".
  const more = pages.hasNextPage ? (
    <div style={{ display: "flex", justifyContent: "center", gap: 8, alignItems: "center" }}>
      <Button size="sm" onClick={() => void pages.fetchNextPage()} loading={pages.isFetchingNextPage} loadingLabel="Loading…">
        Load older runs
      </Button>
    </div>
  ) : null;
  const noMatch = `No run matches${pages.hasNextPage ? ` among the latest ${instances.length} — load older runs to search further` : ""}.`;

  return { instances, shown, isLoading: pages.isLoading, filtering, toolbar, more, noMatch };
}

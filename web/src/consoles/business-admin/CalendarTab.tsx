import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type BACalendar } from "../../api/client";
import { Button, Card, EmptyState, Field, LoadingState, TextInput, Txt } from "../../ui";

/**
 * Business Admin › Calendar: extend a model's time window (owner decision,
 * 2026-11-07). Periods are added after the last one only, under quarters
 * and fiscal years named as the existing ones; nothing is renamed, moved or
 * deleted. A developer reshapes a calendar.
 */
export function CalendarTab() {
  const { data: calendars = [], isLoading } = useQuery({ queryKey: ["ba-calendars"], queryFn: api.listBACalendars });
  if (isLoading) return <LoadingState label="Loading calendars…" />;
  if (calendars.length === 0) return <EmptyState label="This model has no time dimension." />;
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 12, maxWidth: 640 }}>
      <p className="mvx-admin-muted" style={{ margin: 0, fontSize: 13 }}>
        Add periods after a calendar's last one, for the live revision. New months go under new quarters and fiscal
        years named like the existing ones. Planners can enter values in them once they are added.
      </p>
      {calendars.map(c => <CalendarCard key={c.id} calendar={c} />)}
    </div>
  );
}

function CalendarCard({ calendar }: { calendar: BACalendar }) {
  const qc = useQueryClient();
  const [through, setThrough] = useState("");
  const extend = useMutation({
    mutationFn: () => api.extendBACalendar(calendar.id, through),
    onSuccess: () => {
      setThrough("");
      qc.invalidateQueries({ queryKey: ["ba-calendars"] });
    },
  });
  return (
    <Card padding={14}>
      <div style={{ fontWeight: 600 }}>{calendar.name}</div>
      <div className="mvx-admin-muted" style={{ fontSize: 13, margin: "2px 0 10px" }}>
        {calendar.periods} {calendar.granularity || "period"} periods
        <Txt>{calendar.last ? `, through ${calendar.last_label || calendar.last}` : ""}</Txt>
      </div>
      <div style={{ display: "flex", gap: 8, alignItems: "flex-end" }}>
        <Field label="Extend through" style={{ margin: 0 }}>
          <TextInput type="date" value={through} min={calendar.last || undefined} aria-label={`Extend ${calendar.name} through`}
            onChange={e => setThrough(e.target.value)} />
        </Field>
        <Button variant="primary" disabled={!through || extend.isPending} loading={extend.isPending} onClick={() => extend.mutate()}>
          Extend
        </Button>
      </div>
      {extend.isSuccess && (
        <p role="status" style={{ fontSize: 13, margin: "8px 0 0" }}>
          Added {extend.data.periods} periods<Txt>{extend.data.aggregates ? ` and ${extend.data.aggregates} quarters or years` : ""}</Txt><Txt>.</Txt>
        </p>
      )}
      {extend.isError && <p className="mvx-admin-error" style={{ margin: "8px 0 0" }}>{(extend.error as Error).message}</p>}
    </Card>
  );
}

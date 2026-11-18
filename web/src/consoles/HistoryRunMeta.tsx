import type { HistoryInstance } from "../api/client";

/** Under a history run's name: the model it is about and who submitted it. */
export function HistoryRunMeta({ inst }: { inst: HistoryInstance }) {
  const parts = [inst.model_name, inst.submitted_by && `submitted by ${inst.submitted_by}`].filter(Boolean);
  if (parts.length === 0) return null;
  return <div className="mvx-admin-object__meta">{parts.join(" · ")}</div>;
}

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../../api/client";
import { ErrorState } from "../../ui";
import { ModelLinksTable } from "../ModelLinksTable";

// Tenant admin › Model links: every link between the models of the tenants
// administered here, with both sides' switches.
export function ModelLinksTab() {
  const qc = useQueryClient();
  const { data: links = [], isLoading, error } = useQuery({
    queryKey: ["admin-model-links"],
    queryFn: () => api.adminListModelLinks(),
  });
  if (error) return <ErrorState message={(error as Error).message} />;
  return (
    <div style={{ display: "grid", gap: 12 }}>
      <p className="mvx-admin-muted" style={{ margin: 0 }}>
        A model link imports a grid of one model into another model of the same tenant. It runs only while both
        sides are switched on: the link&apos;s side belongs to the model it imports into (one switch per revision),
        the source side to the model it reads (one switch for every revision of the link).
      </p>
      <ModelLinksTable links={links} loading={isLoading} sides={["target", "source"]} emptyTitle="No model links yet"
        onSwitch={async (link, side, on) => {
          await api.adminSwitchModelLink(link.id, side === "source" ? { source_enabled: on } : { enabled: on });
          await qc.invalidateQueries({ queryKey: ["admin-model-links"] });
        }} />
    </div>
  );
}

import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../../../api/client";
import { SectionHeader } from "../../../ui";
import { ModelLinksTable } from "../../ModelLinksTable";

// The source side of model links: the links of other models of this tenant
// that read this model, and the switch this model's developers hold over
// each. Shown only when there is one.
export function ModelLinksReadingPanel() {
  const qc = useQueryClient();
  const { data: links = [], isLoading } = useQuery({
    queryKey: ["model-links-reading"],
    queryFn: () => api.listModelLinksReadingModel(),
  });
  if (!isLoading && links.length === 0) return null;
  return (
    <div style={{ marginTop: 24 }}>
      <SectionHeader
        title="Links reading this model"
        subtitle="Other models of this tenant import this model's grids through these links. Switching one off here stops it in every revision of the model it imports into."
      />
      <ModelLinksTable links={links} loading={isLoading} sides={["source"]} emptyTitle="No link reads this model"
        onSwitch={async (link, _side, on) => {
          await api.switchModelLinkSource(link.id, on);
          await qc.invalidateQueries({ queryKey: ["model-links-reading"] });
        }} />
    </div>
  );
}

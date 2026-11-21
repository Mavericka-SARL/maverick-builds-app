import { Tabs } from "../../ui";
import { CopyValuesTab, FreezeValuesTab } from "./screens";

export type ValuesView = "copy" | "freeze";

/**
 * Business Admin › Manage values (owner, 2026-11-20: merge Copy values and
 * Freeze values into one tab): Copy values writes one member's typed values
 * into another; Freeze values keeps metrics' values as they are now. A
 * dashboard's link to either former page opens its view.
 */
export function ManageValues({ view, onView }: { view: ValuesView; onView: (view: ValuesView) => void }) {
  return (
    <div className="mvx-admin-stack">
      <Tabs<ValuesView>
        tabs={[{ id: "copy", label: "Copy values" }, { id: "freeze", label: "Freeze values" }]}
        active={view}
        onChange={onView}
      />
      {view === "freeze" ? <FreezeValuesTab /> : <CopyValuesTab />}
    </div>
  );
}

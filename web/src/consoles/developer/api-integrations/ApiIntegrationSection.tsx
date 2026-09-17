import { useState } from "react";
import { Plus } from "lucide-react";
import type { ApiIntegrationDetail } from "../../../api/client";
import { Button, SectionHeader } from "../../../ui";
import { ApiIntegrationBuilder } from "./ApiIntegrationBuilder";
import { ApiIntegrationList } from "./ApiIntegrationList";

// Developer Console → Integrations → REST API: the saved-connector list and
// the six-step visual constructor.
export function ApiIntegrationSection({ revisionId }: { revisionId?: string }) {
  const [editing, setEditing] = useState<ApiIntegrationDetail | null>(null);
  const [building, setBuilding] = useState(false);

  if (building || editing) {
    return (
      <div>
        <SectionHeader
          title={editing ? `Edit “${editing.name}”` : "New REST API integration"}
          subtitle="Connect any JSON API, configure authentication, map data, and run or schedule syncs."
        />
        <ApiIntegrationBuilder
          existing={editing}
          revisionId={revisionId}
          onClose={() => { setEditing(null); setBuilding(false); }}
        />
      </div>
    );
  }

  return (
    <div>
      <SectionHeader
        title="REST API"
        subtitle="Connect any JSON API, configure authentication, map data, and run or schedule syncs."
        actions={
          <Button icon={<Plus size={15} />} onClick={() => setBuilding(true)}>
            New integration
          </Button>
        }
      />
      <ApiIntegrationList revisionId={revisionId} onEdit={d => setEditing(d)} />
    </div>
  );
}

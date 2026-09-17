import { useEffect } from "react";
import { useQuery } from "@tanstack/react-query";
import { api } from "../api/client";

export const SELECTED_APP_KEY = "selected_app_id";

export function AppPicker() {
  const { data: apps = [] } = useQuery({
    queryKey: ["apps"],
    queryFn: api.getApps,
  });

  // Auto-select first app when nothing valid is stored (e.g. after persona switch).
  useEffect(() => {
    if (apps.length === 0) return;
    const saved = localStorage.getItem(SELECTED_APP_KEY) ?? "";
    if (!apps.some((a) => a.id === saved)) {
      localStorage.setItem(SELECTED_APP_KEY, apps[0].id);
      window.location.reload();
    }
  }, [apps]);

  if (apps.length <= 1) return null;

  const currentId = localStorage.getItem(SELECTED_APP_KEY) ?? "";
  const current = apps.find((a) => a.id === currentId);

  function select(id: string) {
    localStorage.setItem(SELECTED_APP_KEY, id);
    // A model selection belongs to ONE app — switching apps resets it to
    // the new app's default model.
    localStorage.removeItem("selected_model_id");
    window.location.reload();
  }

  return (
    <div
      style={{
        marginBottom: 4,
        padding: "8px 10px",
        background: "#f3f4f6",
        borderRadius: 6,
        border: "1px solid #e5e7eb",
      }}
    >
      <div
        style={{
          fontSize: 10,
          fontWeight: 700,
          color: "#9ca3af",
          textTransform: "uppercase",
          letterSpacing: "0.05em",
          marginBottom: 4,
        }}
      >
        Application
      </div>
      <select
        value={currentId}
        onChange={(e) => select(e.target.value)}
        style={{
          width: "100%",
          fontSize: 12,
          padding: "3px 6px",
          border: "1px solid #d1d5db",
          borderRadius: 4,
          background: "#fff",
          cursor: "pointer",
          color: "#111827",
        }}
      >
        {!currentId && <option value="">— select app —</option>}
        {apps.map((a) => (
          <option key={a.id} value={a.id}>
            {a.name}
          </option>
        ))}
      </select>
      {current && (
        <div style={{ fontSize: 10, color: "#9ca3af", marginTop: 3 }}>
          {current.workspace_name}
        </div>
      )}
    </div>
  );
}

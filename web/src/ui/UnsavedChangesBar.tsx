import { AlertCircle } from "lucide-react";
import { Button } from "./Button";

interface UnsavedChangesBarProps {
  onSave: () => void;
  onCancel: () => void;
  saveLabel?: string;
  saving?: boolean;
  /** When set, replaces the "Unsaved changes" label with this error message
   *  (e.g. after a failed save) so the failure is visible immediately —
   *  right where the user just clicked Save — rather than only after they
   *  give up and click Cancel. */
  error?: string;
}

export function UnsavedChangesBar({ onSave, onCancel, saveLabel = "Save", saving = false, error }: UnsavedChangesBarProps) {
  return (
    <div style={{
      display: "flex",
      alignItems: "center",
      justifyContent: "space-between",
      padding: "10px 16px",
      background: error ? "var(--color-danger-bg)" : "var(--color-warning-bg)",
      border: error ? "1px solid var(--color-danger-border)" : "1px solid var(--color-warning-border)",
      borderRadius: "var(--radius-button)",
      marginBottom: 16,
      gap: 12,
    }}>
      <span style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 13, color: error ? "var(--color-danger-text)" : "var(--color-warning-text)", fontWeight: 500 }}>
        <AlertCircle size={14} />
        {error ?? "Unsaved changes"}
      </span>
      <div style={{ display: "flex", gap: 8 }}>
        <Button variant="secondary" size="sm" onClick={onCancel} disabled={saving}>
          Cancel
        </Button>
        <Button variant="primary" size="sm" onClick={onSave} loading={saving}>
          {saveLabel}
        </Button>
      </div>
    </div>
  );
}

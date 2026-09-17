import { Button } from "./Button";

interface EmptyStateProps {
  label: string;
  action?: string;
  onAction?: () => void;
}

export function EmptyState({ label, action, onAction }: EmptyStateProps) {
  return (
    <div style={{ padding: "40px 24px", textAlign: "center", color: "var(--color-text-muted)", fontSize: 13 }}>
      <div style={{ marginBottom: action ? 12 : 0 }}>{label}</div>
      {action && onAction && (
        <Button variant="secondary" size="sm" onClick={onAction}>
          {action}
        </Button>
      )}
    </div>
  );
}

import { Loader2 } from "lucide-react";

interface LoadingStateProps {
  label?: string;
}

export function LoadingState({ label = "Loading..." }: LoadingStateProps) {
  return (
    <div style={{ display: "flex", alignItems: "center", justifyContent: "center", gap: 8, padding: "40px 24px", color: "var(--color-text-muted)", fontSize: 13 }}>
      <Loader2 size={16} style={{ animation: "spin 1s linear infinite", flexShrink: 0 }} />
      {label}
    </div>
  );
}

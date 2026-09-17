import React from "react";
import { AlertTriangle } from "lucide-react";

interface WidgetErrorBoundaryProps {
  /** Shown in the fallback so the viewer knows which widget broke. */
  widgetLabel: string;
  children: React.ReactNode;
}

interface WidgetErrorBoundaryState {
  error: Error | null;
}

// React only catches render errors via a class component's
// getDerivedStateFromError/componentDidCatch — no hook equivalent exists.
// Wraps EACH widget individually (see WidgetRenderer's consumers) so one
// widget with a malformed widget_props shape (a stale chart config, a
// ref_id pointing at a deleted grid, etc.) shows an isolated error tile
// instead of taking down the entire dashboard for every viewer.
export class WidgetErrorBoundary extends React.Component<WidgetErrorBoundaryProps, WidgetErrorBoundaryState> {
  state: WidgetErrorBoundaryState = { error: null };

  static getDerivedStateFromError(error: Error): WidgetErrorBoundaryState {
    return { error };
  }

  componentDidCatch(error: Error, info: React.ErrorInfo) {
    console.error(`Widget "${this.props.widgetLabel}" failed to render:`, error, info.componentStack);
  }

  render() {
    if (this.state.error) {
      return (
        <div style={{
          display: "flex", flexDirection: "column", alignItems: "center", justifyContent: "center",
          gap: 8, height: "100%", minHeight: 120, padding: 20,
          border: "1px dashed var(--color-danger-border)", borderRadius: "var(--radius-card)",
          background: "var(--color-danger-bg)", color: "var(--color-danger)", fontSize: 12.5, textAlign: "center",
        }}>
          <AlertTriangle size={18} />
          <span>
            <strong>{this.props.widgetLabel}</strong> couldn't render.
          </span>
          <span style={{ color: "var(--color-text-muted)" }}>{this.state.error.message}</span>
        </div>
      );
    }
    return this.props.children;
  }
}

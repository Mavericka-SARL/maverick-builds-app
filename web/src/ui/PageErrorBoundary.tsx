import React from "react";
import { AlertTriangle, RotateCw } from "lucide-react";
import { Button } from "./Button";

interface PageErrorBoundaryProps {
  children: React.ReactNode;
}

interface PageErrorBoundaryState {
  error: Error | null;
}

// Wraps the console's page (UnifiedConsole, keyed by the page, so choosing
// another one starts afresh). An error React could not handle used to
// unmount the whole console and leave a blank screen with no way on but a
// reload (reported 2026-10-23, a translated page); now the sidebar stays and
// the page says what happened.
export class PageErrorBoundary extends React.Component<PageErrorBoundaryProps, PageErrorBoundaryState> {
  state: PageErrorBoundaryState = { error: null };

  static getDerivedStateFromError(error: Error): PageErrorBoundaryState {
    return { error };
  }

  componentDidCatch(error: Error, info: React.ErrorInfo) {
    console.error("The page failed:", error, info.componentStack);
  }

  render() {
    const { error } = this.state;
    if (!error) return this.props.children;
    // NotFoundError from insertBefore/removeChild: something outside the
    // console changed the page under it — most often the browser's own
    // "translate this page", or an extension.
    const changedOutside = error.name === "NotFoundError";
    return (
      <div role="alert" style={{
        display: "grid", gap: 10, justifyItems: "start", maxWidth: 640, margin: "32px 0", padding: 20,
        border: "1px solid var(--color-danger-border)", borderRadius: "var(--radius-card)",
        background: "var(--color-danger-bg)", fontSize: 13,
      }}>
        <strong style={{ display: "flex", alignItems: "center", gap: 8, color: "var(--color-danger)" }}>
          <AlertTriangle size={16} /> This page stopped with an error
        </strong>
        <span style={{ color: "var(--color-text-muted)" }}>{error.message}</span>
        {changedOutside && (
          <span>
            Something outside the console changed this page — most often the browser translating it, or an
            extension. Turn translation off for this site, then reload.
          </span>
        )}
        <Button leadingIcon={<RotateCw size={14} />} onClick={() => window.location.reload()}>Reload</Button>
      </div>
    );
  }
}

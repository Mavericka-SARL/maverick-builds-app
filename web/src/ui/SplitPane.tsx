import type React from "react";

interface SplitPaneProps {
  main: React.ReactNode;
  side?: React.ReactNode;
  className?: string;
}

export function SplitPane({ main, side, className }: SplitPaneProps) {
  return (
    <div className={["mvx-split-pane", className].filter(Boolean).join(" ")}>
      <div className="mvx-split-pane__main">{main}</div>
      {side}
    </div>
  );
}

export type { SplitPaneProps };

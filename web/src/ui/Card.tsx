import React from "react";

interface CardProps {
  children: React.ReactNode;
  style?: React.CSSProperties;
  padding?: number | string;
  raised?: boolean;
  className?: string;
}

export function Card({ children, style, padding = 20, raised = false, className }: CardProps) {
  return (
    <div
      className={["mvx-card", raised ? "mvx-card--raised" : "", className].filter(Boolean).join(" ")}
      style={{ padding, ...style }}
    >
      {children}
    </div>
  );
}

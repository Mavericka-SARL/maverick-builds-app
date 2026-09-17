import React from "react";

export type BadgeColor = "success" | "warning" | "danger" | "info" | "neutral" | "brand" | "live" | "draft";

interface BadgeProps {
  color?: BadgeColor;
  children: React.ReactNode;
  style?: React.CSSProperties;
  className?: string;
}

export function Badge({ color = "neutral", children, style, className }: BadgeProps) {
  return (
    <span
      className={["mvx-badge", `mvx-badge--${color}`, className].filter(Boolean).join(" ")}
      style={style}
    >
      {children}
    </span>
  );
}

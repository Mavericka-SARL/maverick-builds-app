import type React from "react";
import type { DesignTone } from "./types";

interface InlineAlertProps {
  tone?: Extract<DesignTone, "info" | "success" | "warning" | "danger">;
  icon?: React.ReactNode;
  children: React.ReactNode;
  className?: string;
}

export function InlineAlert({ tone = "info", icon, children, className }: InlineAlertProps) {
  return (
    <div className={["mvx-inline-alert", `mvx-inline-alert--${tone}`, className].filter(Boolean).join(" ")}>
      {icon && <span aria-hidden="true">{icon}</span>}
      <div>{children}</div>
    </div>
  );
}

export type { InlineAlertProps };

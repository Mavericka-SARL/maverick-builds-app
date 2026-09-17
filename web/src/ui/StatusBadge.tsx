import { Badge } from "./Badge";
import type React from "react";
import type { DesignTone } from "./types";

interface StatusBadgeProps {
  children: React.ReactNode;
  tone?: DesignTone;
  className?: string;
}

const ROLE_TONE: Record<string, DesignTone> = {
  platform_admin: "danger",
  tenant_admin: "info",
  developer: "brand",
  business_admin: "warning",
  business_user: "success",
};

export function StatusBadge({ children, tone = "neutral", className }: StatusBadgeProps) {
  return (
    <Badge color={tone} className={className}>
      {children}
    </Badge>
  );
}

export function RoleBadge({ role }: { role: string }) {
  return (
    <StatusBadge tone={ROLE_TONE[role] ?? "neutral"}>
      {role.replace(/_/g, " ")}
    </StatusBadge>
  );
}

export function RevisionBadge({ status, label }: { status: "live" | "draft" | "working" | "inactive"; label?: string }) {
  const tone: DesignTone = status === "live" ? "live" : status === "inactive" ? "neutral" : "draft";
  return (
    <StatusBadge tone={tone}>
      {label ?? status}
    </StatusBadge>
  );
}

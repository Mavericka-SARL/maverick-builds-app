import type React from "react";

export type DesignTone =
  | "neutral"
  | "brand"
  | "info"
  | "success"
  | "warning"
  | "danger"
  | "live"
  | "draft";

export interface NavItem {
  id: string;
  label: string;
  icon?: React.ReactNode;
  badge?: React.ReactNode;
  disabled?: boolean;
  href?: string;
}

export interface NavGroup {
  id?: string;
  label?: string;
  items: NavItem[];
}

export interface ContextBarItem {
  id: string;
  /** Omit for a self-describing value that needs no uppercase caption. */
  label?: string;
  value: React.ReactNode;
  tone?: DesignTone;
}

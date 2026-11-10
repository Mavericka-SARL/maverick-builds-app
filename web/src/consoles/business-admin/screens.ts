import { lazy } from "react";

// Loaded when first opened, outside the console's main file (2026-11-09).
export const CopyValuesTab = lazy(() => import("./CopyValuesTab").then(m => ({ default: m.CopyValuesTab })));
export const WorkflowInbox = lazy(() => import("./BusinessAdminConsole").then(m => ({ default: m.WorkflowInbox })));
export const WorkflowHistory = lazy(() => import("./BusinessAdminConsole").then(m => ({ default: m.WorkflowHistory })));
export const RolesTab = lazy(() => import("./BusinessAdminConsole").then(m => ({ default: m.RolesTab })));
export const AccessRulesTab = lazy(() => import("./BusinessAdminConsole").then(m => ({ default: m.AccessRulesTab })));

import { lazy } from "react";

// The developer screens, loaded when first opened: in the console's one
// file they made it 1.55 MB for everyone, most of whom never open Build
// (2026-11-09).
export const WorkflowsTab = lazy(() => import("./WorkflowsTab").then(m => ({ default: m.WorkflowsTab })));
export const AIAssistant = lazy(() => import("./AIAssistant").then(m => ({ default: m.AIAssistant })));
export const DevApplicationsTab = lazy(() => import("./ApplicationsTab").then(m => ({ default: m.DevApplicationsTab })));
export const FormsTab = lazy(() => import("./FormsTab").then(m => ({ default: m.FormsTab })));
export const AutomationTab = lazy(() => import("./AutomationTab").then(m => ({ default: m.AutomationTab })));
export const GridsTab = lazy(() => import("./GridsTab").then(m => ({ default: m.GridsTab })));
export const DimensionsView = lazy(() => import("./DimensionsTab").then(m => ({ default: m.DimensionsView })));
export const MetricsTab = lazy(() => import("./MetricsTab").then(m => ({ default: m.MetricsTab })));
export const DepGraph = lazy(() => import("./DependencyGraphTab").then(m => ({ default: m.DepGraph })));
export const IntegrationsTab = lazy(() => import("./IntegrationsTab").then(m => ({ default: m.IntegrationsTab })));
export const DashboardsTab = lazy(() => import("./DashboardsTab").then(m => ({ default: m.DashboardsTab })));
export const UsersPanel = lazy(() => import("../admin/UsersPanel").then(m => ({ default: m.UsersPanel })));
export const RolesTab = lazy(() => import("../business-admin/BusinessAdminConsole").then(m => ({ default: m.RolesTab })));

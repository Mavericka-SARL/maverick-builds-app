import { lazy } from "react";

// The administration screens, loaded when first opened, outside the
// console's main file (2026-11-09).
export const InfraNodesTab = lazy(() => import("./InfraNodesTab").then(m => ({ default: m.InfraNodesTab })));
export const LicenseTab = lazy(() => import("./LicenseTab").then(m => ({ default: m.LicenseTab })));
export const PlansTab = lazy(() => import("./PlansTab").then(m => ({ default: m.PlansTab })));
export const NotificationSettingsTab = lazy(() => import("../admin/NotificationSettingsTab").then(m => ({ default: m.NotificationSettingsTab })));
export const ModelLinksTab = lazy(() => import("../admin/ModelLinksTab").then(m => ({ default: m.ModelLinksTab })));
export const ChatConnectorSettingsTab = lazy(() => import("../admin/ChatConnectorSettingsTab").then(m => ({ default: m.ChatConnectorSettingsTab })));
export const TenantAIKeysTab = lazy(() => import("../../ee/aikeys/TenantAIKeysTab").then(m => ({ default: m.TenantAIKeysTab })));
export const SsoTab = lazy(() => import("../../ee/sso/SsoTab").then(m => ({ default: m.SsoTab })));
export const ScimTab = lazy(() => import("../../ee/scim/ScimTab").then(m => ({ default: m.ScimTab })));
export const UsageTab = lazy(() => import("../../ee/usage/UsageTab").then(m => ({ default: m.UsageTab })));
export const BrandingTab = lazy(() => import("../../ee/branding/BrandingTab").then(m => ({ default: m.BrandingTab })));
export const ApplicationsView = lazy(() => import("./PlatformAdminConsole").then(m => ({ default: m.ApplicationsView })));
export const AuditView = lazy(() => import("./PlatformAdminConsole").then(m => ({ default: m.AuditView })));
export const UsersPanel = lazy(() => import("../admin/UsersPanel").then(m => ({ default: m.UsersPanel })));

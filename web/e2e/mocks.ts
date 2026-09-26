import type { Page } from "@playwright/test";

const ctx = {
  app_id: "app-1",
  model_id: "model-1",
  revision_id: "rev-1",
  revision: "FY2026 Budget",
  scenario: "FY2026 Budget",
  version: "draft",
  actor: {
    user_id: "u-1",
    email: "alex@example.com",
    display_name: "Alex",
    roles: ["business_user"],
  },
};

const personaRoles: Record<string, string[]> = {
  dept_head: ["business_user"],
  developer: ["developer"],
  finance: ["business_admin"],
  platform_admin: ["platform_admin"],
  tenant_admin: ["tenant_admin"],
};

function demoContextFor(persona: string) {
  return {
    ...ctx,
    actor: {
      ...ctx.actor,
      roles: personaRoles[persona] ?? ["business_user"],
    },
  };
}

const metrics = [
  { id: "m1", name: "revenue", label: "Revenue", is_input: true, agg_rule: "sum", value: null, depends_on: [], depended_by: ["gross_margin"] },
  { id: "m2", name: "opex_marketing", label: "Marketing OPEX", is_input: true, agg_rule: "sum", value: null, depends_on: [], depended_by: ["total_opex"] },
  { id: "m3", name: "opex_people", label: "People OPEX", is_input: true, agg_rule: "sum", value: null, depends_on: [], depended_by: ["total_opex"] },
  { id: "m4", name: "total_opex", label: "Total OPEX", is_input: false, formula: "{opex_marketing} + {opex_people}", agg_rule: "sum", value: null, depends_on: ["opex_marketing", "opex_people"], depended_by: ["gross_margin"] },
  { id: "m5", name: "gross_margin", label: "Gross Margin", is_input: false, formula: "{revenue} - {total_opex}", agg_rule: "sum", value: null, depends_on: ["revenue", "total_opex"], depended_by: [] },
  // agg_rule "formula": its total is the formula re-evaluated against
  // aggregated inputs, never a combination of its members. The cells and the
  // total below are deliberately inconsistent with each other so a client that
  // combines instead of reading the server's answer is caught.
  { id: "m6", name: "margin_pct", label: "Margin Pct", is_input: false, formula: "{gross_margin} / {revenue} * 100", agg_rule: "formula", value: null, depends_on: ["gross_margin", "revenue"], depended_by: [] },
];

const dimensions = [
  {
    id: "d1",
    name: "Department",
    agg_rule: "sum",
    members: [
      { id: "dm-root", code: "OPEX", label: "OPEX" },
      { id: "dm-mkt", code: "MKT", label: "Marketing", parent_member_id: "dm-root" },
      { id: "dm-sales", code: "SALES", label: "Sales", parent_member_id: "dm-root" },
      { id: "dm-ops", code: "OPS", label: "Operations", parent_member_id: "dm-root" },
    ],
  },
  {
    id: "d2",
    name: "Region",
    agg_rule: "sum",
    members: [
      { id: "rg-all", code: "GLOBAL", label: "Global" },
      { id: "rg-emea", code: "EMEA", label: "EMEA", parent_member_id: "rg-all" },
      { id: "rg-na", code: "NA", label: "North America", parent_member_id: "rg-all" },
    ],
  },
];

const forms = [
  {
    id: "form-1",
    model_id: "model-1",
    name: "purchase_request",
    label: "Purchase Request",
    fields: [
      { name: "vendor", label: "Vendor", type: "text", required: true },
      { name: "amount", label: "Amount", type: "number", required: true },
      { name: "category", label: "Category", type: "select", required: false, options: ["Software", "Services"] },
    ],
    created_at: "2026-05-01T09:00:00Z",
  },
];

const records = [
  { id: "rec-1", form_id: "form-1", data: { vendor: "Acme Cloud", amount: 24000, category: "Software" }, status: "submitted", created_by: "u-1", created_at: "2026-05-11T09:00:00Z", updated_at: "2026-05-11T09:00:00Z" },
];

const grids = [
  { id: "grid-1", name: "OPEX Planning Grid", metric_ids: ["m2", "m3", "m4"], dimension_ids: ["d1", "d2"] },
];

const dashboards = [
  {
    id: "dash-1",
    name: "OPEX 2",
    tags: ["finance"],
    folder_id: null,
    widgets: [
      // Explicit, non-overlapping geometry: without it every widget falls back
      // to the same default rect, so the canvas's overlap rule (correctly)
      // rejects any move or resize and keyboard tests can't observe one.
      { id: "w1", widget_type: "text", ref_id: null, content: "Review OPEX movement before submitting.", sort_order: 0, col_start: 1, col_span: 12, pos_x: 0, pos_y: 0, size_w: 400, size_h: 120 },
      { id: "w2", widget_type: "grid", ref_id: "grid-1", content: null, sort_order: 1, col_start: 1, col_span: 12, pos_x: 0, pos_y: 400, size_w: 400, size_h: 200 },
    ],
  },
  { id: "dash-2", name: "OPEX Dashboard", tags: ["finance", "actuals"], folder_id: null, widgets: [] },
  { id: "dash-3", name: "OPEX form 2", tags: ["finance"], widgets: [{ id: "w3", widget_type: "form", ref_id: "form-1", content: null, sort_order: 0, col_start: 1, col_span: 12 }] },
  { id: "dash-4", name: "Sales Pipeline", tags: ["sales"], widgets: [] },
  // Prose and a picture: what the sign-up tour is written in, and what any
  // tenant can write for its own people.
  {
    id: "dash-5",
    name: "Explainer",
    tags: ["guide"],
    folder_id: null,
    widgets: [
      {
        id: "w5", widget_type: "text", ref_id: null, sort_order: 0, col_start: 1, col_span: 12, pos_x: 0, pos_y: 0, size_w: 600, size_h: 240,
        content: "# How this works\n\nEvery number is **addressed** by a member of each dimension.\n\n- Input — someone types it\n- Calculated — the platform works it out\n\nSee [the handbook](https://example.com/handbook).",
      },
      {
        id: "w6", widget_type: "image", ref_id: null, sort_order: 1, col_start: 1, col_span: 12, pos_x: 0, pos_y: 260, size_w: 400, size_h: 160,
        content: "data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHZpZXdCb3g9IjAgMCAyMCAxMCI+PHJlY3Qgd2lkdGg9IjIwIiBoZWlnaHQ9IjEwIiBmaWxsPSIjNGY0NmU1Ii8+PC9zdmc+",
        widget_props: { alt: "A diagram of the model", image_fit: "contain" },
      },
    ],
  },
];

const gridData = {
  scenario: ctx.scenario,
  version: ctx.version,
  metrics: metrics.map((metric) => ({
    id: metric.id,
    name: metric.name,
    label: metric.label,
    is_input: metric.is_input,
    formula: metric.formula,
    agg_rule: metric.agg_rule,
    value: metric.value,
  })),
  dimensions: [
    {
      id: "d1",
      name: "Department",
      members: [
        { id: "dm-root", code: "OPEX", label: "OPEX" },
        { id: "dm-mkt", code: "MKT", label: "Marketing", parent_code: "OPEX" },
        { id: "dm-sales", code: "SALES", label: "Sales", parent_code: "OPEX" },
        { id: "dm-ops", code: "OPS", label: "Operations", parent_code: "OPEX" },
      ],
    },
    {
      id: "d2",
      name: "Region",
      members: [
        { id: "rg-all", code: "GLOBAL", label: "Global" },
        { id: "rg-emea", code: "EMEA", label: "EMEA", parent_code: "GLOBAL" },
        { id: "rg-na", code: "NA", label: "North America", parent_code: "GLOBAL" },
      ],
    },
  ],
  departments: [],
  cells: {
    "m2:OPEX:GLOBAL": 380000,
    "m3:OPEX:GLOBAL": 920000,
    "m2:MKT:GLOBAL": 240000,
    "m3:MKT:GLOBAL": 310000,
    "m2:SALES:GLOBAL": 90000,
    "m3:SALES:GLOBAL": 420000,
    "m2:OPS:GLOBAL": 50000,
    "m3:OPS:GLOBAL": 190000,
    "m6:MKT:GLOBAL": 10, "m6:SALES:GLOBAL": 20, "m6:OPS:GLOBAL": 30,
    "m6:MKT:EMEA": 10, "m6:SALES:EMEA": 20, "m6:OPS:EMEA": 30,
    "m6:MKT:NA": 10, "m6:SALES:NA": 20, "m6:NA:OPS": 30,
    // m4 (total_opex = opex_marketing + opex_people) and m5 (gross_margin =
    // revenue - total_opex, revenue always 0 since m1 has no cells of its
    // own) — the real backend now populates calc-metric cells the same way
    // it always has for inputs (see grid()'s calc block), so the mock
    // mirrors that instead of leaving BusinessConsole to compute them.
    "m4:OPEX:GLOBAL": 1300000,
    "m4:MKT:GLOBAL": 550000,
    "m4:SALES:GLOBAL": 510000,
    "m4:OPS:GLOBAL": 240000,
    "m5:OPEX:GLOBAL": -1300000,
    "m5:MKT:GLOBAL": -550000,
    "m5:SALES:GLOBAL": -510000,
    "m5:OPS:GLOBAL": -240000,
  },
  // m6's members sum to 60. Its own rule says the total is 42. A client that
  // aggregates children shows 60; one that reads the server shows 42.
  totals: { m2: 380000, m3: 920000, m4: 1300000, m5: -1300000, m6: 42 },
  access_rules: { dim_members: {}, metrics: {} },
};

const tasks = [
  { id: "task-1", step_def_id: "finance_review", workflow_name: "Budget Approval", instance_id: "inst-12345678", context: { scenario: ctx.scenario, version: ctx.version }, status: "pending", created_at: "2026-05-19T09:30:00Z" },
];

const history = [
  {
    id: "inst-1",
    workflow_name: "Budget Approval",
    status: "running",
    context: { scenario: ctx.scenario, version: ctx.version },
    started_at: "2026-05-18T10:00:00Z",
    steps: [
      { id: "s1", step_def_id: "dept_submit", status: "completed", decision: "sent", comment: "Ready" },
      { id: "s2", step_def_id: "finance_review", status: "in_progress" },
    ],
  },
];

const notifications = [
  {
    id: "notif-1",
    template_id: "workflow_step_notification",
    status: "delivered",
    template_vars: { subject: "Budget Approval needs your review", message: "Finance review step is waiting on you." },
    created_at: "2026-05-19T09:31:00Z",
    resource_type: "workflow_instance",
    resource_id: "inst-1",
  },
  {
    id: "notif-2",
    template_id: "workflow_step_notification",
    status: "read",
    template_vars: { subject: "Sales Pipeline dashboard updated", message: "New widgets were added." },
    created_at: "2026-05-18T14:00:00Z",
    resource_type: "",
    resource_id: "",
  },
];

const roles = [
  { id: "role-1", name: "Finance Editors", member_count: 2, dashboard_ids: ["dash-1", "dash-2"] },
  { id: "role-2", name: "Sales Viewers", member_count: 4, dashboard_ids: ["dash-4"] },
];

const baUsers = [
  { id: "u-1", display_name: "Alex Smith", email: "alex@example.com", role: "business_user" },
  { id: "u-2", display_name: "Jordan Lee", email: "jordan@example.com", role: "business_admin" },
];

const integrations = [
  { id: "int-1", name: "Import OPEX Data", type: "csv_import", target_type: "grid", target_id: "grid-1", config: { column_map: { Department: "department", Value: "value" } } },
];

const adminTenants = [
  {
    id: "tenant-1",
    name: "Acme Corp",
    plan: "enterprise",
    created_at: "2026-01-01T00:00:00Z",
    applications: [
      {
        id: "app-1",
        name: "Planning",
        mode: "planning",
        status: "active",
        models: [
          {
            id: "model-1",
            name: "Finance Model",
            storage_type: "oltp",
            active_revision: "FY2026 Budget",
            revisions: [
              { id: "rev-1", name: "FY2026 Budget", created_at: "2026-01-01T00:00:00Z" },
              { id: "rev-2", name: "FY2026 Forecast", created_at: "2026-01-02T00:00:00Z" },
            ],
            versions: [{ id: "v-1", name: "draft" }],
          },
        ],
      },
    ],
  },
];

const adminUsers = [
  {
    id: "u-1",
    email: "alex@example.com",
    display_name: "Alex Smith",
    created_at: "2026-01-05T00:00:00Z",
    assignments: [{ role: "business_user", workspace_id: "ws-1", workspace_name: "Finance", customer_name: "Acme Corp" }],
    app_ids: [],
    model_ids: ["model-1"],
  },
  {
    id: "u-2",
    email: "jordan@example.com",
    display_name: "Jordan Lee",
    created_at: "2026-01-06T00:00:00Z",
    assignments: [{ role: "business_admin", workspace_id: "ws-1", workspace_name: "Finance", customer_name: "Acme Corp" }],
    app_ids: [],
    model_ids: [],
  },
  {
    id: "u-3",
    email: "sam@example.com",
    display_name: "Sam Dev",
    created_at: "2026-01-07T00:00:00Z",
    assignments: [{ role: "developer", workspace_id: "ws-1", workspace_name: "Finance", customer_name: "Acme Corp" }],
    app_ids: ["app-1"],
    model_ids: [],
  },
];

const adminWorkspaces = [
  { id: "ws-1", name: "Finance", customer_name: "Acme Corp", customer_id: "tenant-1" },
];

const adminAudit = [
  { id: "a1", category: "model_change", event_type: "metric.updated", actor_name: "Sam Dev", actor_role: "developer", resource_type: "metric", resource_id: "m4", metadata: { name: "total_opex" }, occurred_at: "2026-05-20T08:20:00Z" },
  { id: "a2", category: "policy_change", event_type: "role.dashboard.updated", actor_name: "Jordan Lee", actor_role: "business_admin", resource_type: "role", resource_id: "role-1", metadata: { dashboards: 2 }, occurred_at: "2026-05-19T11:45:00Z" },
];

// ── Plans and sign-up ────────────────────────────────────────────────────────

const noLimits = { max_users: 0, max_applications: 0, max_models: 0, max_metrics_per_model: 0, max_members_per_dimension: 0, max_fact_rows_per_model: 0, max_ai_messages_per_day: 0, max_integration_runs_per_day: 0, max_storage_mb: 0 };
const testLimits = { ...noLimits, max_storage_mb: 100, max_ai_messages_per_day: 100, max_integration_runs_per_day: 50 };
const testNote = "A basic workspace holds up to 100 MB. To go further, run the platform on your own infrastructure — free of charge and without limits for non-commercial use, under a commercial licence otherwise — or get the enterprise edition.";
const smallLimits = { ...noLimits, max_users: 5, max_applications: 2, max_models: 3, max_metrics_per_model: 50, max_members_per_dimension: 500, max_fact_rows_per_model: 100000, max_ai_messages_per_day: 100, max_integration_runs_per_day: 50 };

/** GET /api/admin/plans as the seeded catalog answers it. */
export const adminPlans = [
  { key: "test", name: "Basic workspace", description: "Use the platform for as long as you like, with up to 100 MB of data.", self_service: true, limits: testLimits, limit_note: testNote, sort_order: 5, updated_at: "2026-09-19T00:00:00Z" },
  { key: "small", name: "Small", description: "A handful of everything.", self_service: false, limits: smallLimits, limit_note: "", sort_order: 10, updated_at: "2026-09-17T00:00:00Z" },
  { key: "starter", name: "Starter", description: "", self_service: false, limits: noLimits, limit_note: "", sort_order: 20, updated_at: "2026-09-17T00:00:00Z" },
  { key: "enterprise", name: "Enterprise", description: "No limits.", self_service: false, limits: noLimits, limit_note: "", sort_order: 40, updated_at: "2026-09-17T00:00:00Z" },
];

/** A tenant's plan state on the small plan, within its limits unless `extra` says otherwise. */
export function smallPlanState(extra: Record<string, unknown> = {}) {
  return { plan: adminPlans[1], plan_known: true, read_only: false, limit_state: "ok", ...extra };
}

/** GET /api/legal for a deployment that has named its operator. */
export const legalInfo = {
  published: true,
  builtin: true,
  terms_url: "/terms",
  privacy_url: "/privacy",
  entity: "Acme Software SARL",
  address: "1 Rue de Test, L-1000 Luxembourg",
  email: "legal@acme.test",
  jurisdiction: "Luxembourg",
  hosting: "Hetzner Online GmbH (Germany)",
  updated: "2026-09-18",
};

/** GET /api/legal for one that has published nothing. */
export const legalUnpublished = {
  published: false, builtin: false, terms_url: "", privacy_url: "",
  entity: "", address: "", email: "", jurisdiction: "", hosting: "", updated: "",
};

/** GET /api/signup/options with sign-up open: the basic workspace, as seeded. */
export const signupOptions = {
  enabled: true, contact_url: "https://example.test/pricing",
  plan: { key: "test", name: "Basic workspace", description: "Use the platform for as long as you like, with up to 100 MB of data.", limits: testLimits, limit_note: testNote },
};

/** A test-workspace tenant that has filled its 100 MB. */
export function overStoragePlanState() {
  return {
    plan: adminPlans[0], plan_known: true, read_only: true, code: "over_limit", limit_state: "over",
    reason: `This tenant is over its plan's limits (The Basic workspace plan allows 100 MB of storage; this tenant uses 104 MB. ${testNote}). The workspace is read-only, except for deleting, until it is back within them.`,
  };
}

/** The same with a plan that has no note of its own — the terms then fall back to the generic wording. */
export const signupOptionsSmall = {
  ...signupOptions,
  plan: { key: "small", name: "Small", description: "A handful of everything.", limits: smallLimits, limit_note: "" },
};

/** GET /api/license as a deployment without a key answers it. */
export const communityLicense = {
  edition: "community", state: "community", features: [], source: "none",
  catalog: [
    { key: "sso", name: "Single sign-on", description: "Sign in through the customer's SAML or OIDC identity provider.", min_edition: "enterprise" },
    { key: "audit_export", name: "Audit export", description: "Export or stream the audit log, with retention policies.", min_edition: "enterprise" },
    { key: "white_label", name: "White-labelling", description: "Custom logo, colours and domain for the console.", min_edition: "commercial" },
    { key: "tenant_ai_keys", name: "Tenant AI keys", description: "One AI provider key per tenant, managed by the tenant admin, used by every developer.", min_edition: "enterprise" },
    { key: "scim", name: "SCIM provisioning", description: "Create, update and deactivate users from a directory automatically.", min_edition: "enterprise" },
    { key: "cell_history", name: "Cell history", description: "Browse who changed which value, per intersection, with the full write history.", min_edition: "enterprise" },
    { key: "usage_analytics", name: "Usage analytics", description: "Active users, models, storage and integration runs per tenant.", min_edition: "enterprise" },
  ],
};

/** The same deployment with a commercial key: white-labelling only. */
export const commercialLicense = {
  ...communityLicense,
  edition: "commercial", state: "active", source: "env", features: ["white_label"],
  license_id: "lic-c2e", customer: "Acme Corp", issued_at: "2026-09-01T00:00:00Z", expires_at: "2027-09-01T00:00:00Z",
};

/** A tenant's brand as GET /api/branding reports it once configured. */
export const acmeBrand = {
  product_name: "Acme Planning", tagline: "Numbers you can sign.", brand_color: "#0f766e", configured: true, source: "tenant",
  logo_data_url: "data:image/svg+xml;base64," + btoa('<svg xmlns="http://www.w3.org/2000/svg" width="80" height="28"><rect width="80" height="28" fill="#0f766e"/></svg>'),
  favicon_data_url: "data:image/svg+xml;base64," + btoa('<svg xmlns="http://www.w3.org/2000/svg" width="16" height="16"><rect width="16" height="16" fill="#0f766e"/></svg>'),
};

/** The same deployment with an enterprise key installed. */
export const enterpriseLicense = {
  ...communityLicense,
  edition: "enterprise", state: "active", source: "env",
  features: ["sso", "audit_export", "white_label", "tenant_ai_keys", "scim", "cell_history", "usage_analytics"],
  license_id: "lic-e2e", customer: "Acme Corp", contact: "ops@acme.test",
  issued_at: "2026-09-01T00:00:00Z", expires_at: "2027-09-01T00:00:00Z",
  limits: { max_users: 50 },
};

/** GET /api/cells/history: two live values and one an import removed. */
export const cellHistory = [
  { id: "h3", value: 300, entered_at: "2026-09-16T10:00:00Z", entered_by: { id: "u1", name: "Alex Smith", email: "alex@example.com" }, source: { kind: "typed" }, current: true },
  { id: "h2", value: 250, entered_at: "2026-09-15T10:00:00Z", entered_by: { id: "u2", name: "Bob Lee", email: "bob@example.com" }, source: { kind: "typed" }, current: false,
    deleted_at: "2026-09-16T09:00:00Z", delete_reason: "removed by an import in full-reload mode" },
  { id: "h1", value: 100, entered_at: "2026-09-14T10:00:00Z", entered_by: { id: "u1", name: "Alex Smith", email: "alex@example.com" }, source: { kind: "form", label: "Post expenses" }, current: false },
];

/** GET /api/admin/sso for a tenant with no provider registered yet. */
export const ssoSettings = {
  alias: "", protocol: "oidc", display_name: "Company account", metadata_url: "", client_id: "",
  allowed_domains: [], jit_provisioning: true, default_role: "business_user", enabled: false, configured: false,
  broker_endpoint: "https://auth.test/realms/mavericks/broker/mvx-0000/endpoint",
  sp_entity_id: "https://auth.test/realms/mavericks", registry_configured: true,
};

/** GET /api/admin/scim/tokens: one active, one revoked. */
export const scimTokens = [
  { id: "tok-1", name: "Entra ID production", default_role: "business_user", created_at: "2026-09-01T10:00:00Z", last_used_at: "2026-09-15T08:30:00Z" },
  { id: "tok-2", name: "Old Okta", default_role: "business_user", created_at: "2026-06-01T10:00:00Z", revoked_at: "2026-08-01T10:00:00Z" },
];

/** GET /api/admin/ai-settings for a tenant that has not stored a key yet. */
export const tenantAISettings = {
  provider: "openai", model: "", has_key: false, enforced: false,
};

/** GET /api/notifications/settings for a deployment with no mail relay. */
export const notificationSettings = {
  email_enabled: false, webhook_enabled: false, webhook_url: "",
  has_webhook_secret: false, reminders_enabled: false, reminder_lead_hours: 0,
  mailer_configured: false,
};

export async function mockApi(page: Page, overrides: { license?: unknown; tenantAI?: unknown; sso?: unknown; scimTokens?: unknown; brand?: unknown; plan?: unknown; plans?: unknown; signup?: unknown; legal?: unknown } = {}) {
  await page.route("**", async (route) => {
    const url = new URL(route.request().url());
    const path = url.pathname;
    if (!path.startsWith("/api/")) return route.continue();
    const method = route.request().method();
    const ok = (body: unknown) => route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(body),
    });

    if (method === "POST" && path === "/api/admin/scim/tokens") {
      return route.fulfill({ status: 201, contentType: "application/json", body: JSON.stringify({
        id: "tok-3", name: "New directory", default_role: "business_user", created_at: "2026-09-16T00:00:00Z",
        token: "mvx_scim_00000000000000000000000000000000_secretsecretsecretsecretsecretsecretsecret1", base_url: "https://console.test/api/scim/v2",
      }) });
    }
    if (method === "POST" && path === "/api/signup") {
      const body = route.request().postDataJSON() as { email: string };
      return ok({ status: "invited", invited: true, email: body.email, tenant_id: "t-new", application_id: "app-new", model_id: "model-new", plan: "test" });
    }
    if (method === "PUT" && path.startsWith("/api/admin/plans/")) {
      return ok({ key: path.split("/").pop(), updated_at: "2026-09-17T00:00:00Z", ...(route.request().postDataJSON() as object) });
    }
    if (method !== "GET") return ok({ status: "ok", id: "new-id" });
    if (path === "/api/cells/history") return ok(cellHistory);
    if (path === "/api/admin/audit/settings") return ok({ retention_days: 0, min_retention_days: 30 });
    if (path === "/api/admin/usage") return ok({ period_days: 30, since: "2026-08-17T00:00:00Z", tenants: [
      { customer_id: "t-1", name: "Acme Corp", plan: "enterprise", created_at: "2026-01-01T00:00:00Z", users: 12, active_users: 7, applications: 2, models: 3, revisions: 9,
        fact_rows: 10119, calc_rows: 31168, form_records: 10, object_bytes: 0, db_bytes: 41943040, integration_runs: 4, workflow_instances: 94, ai_messages: 0, audit_events: 2975, last_activity_at: "2026-09-16T20:00:00Z" },
      { customer_id: "t-2", name: "Meridian Industries", plan: "starter", created_at: "2026-03-01T00:00:00Z", users: 3, active_users: 0, applications: 1, models: 1, revisions: 1,
        fact_rows: 0, calc_rows: 0, form_records: 0, object_bytes: 0, db_bytes: 0, integration_runs: 0, workflow_instances: 0, ai_messages: 0, audit_events: 0 },
    ] });
    if (path === "/api/admin/sso") return ok(overrides.sso ?? ssoSettings);
    if (path === "/api/admin/scim/tokens") return ok(overrides.scimTokens ?? scimTokens);
    if (path === "/api/license") return ok(overrides.license ?? communityLicense);
    if (path === "/api/signup/options") return ok(overrides.signup ?? signupOptions);
    if (path === "/api/legal") return ok(overrides.legal ?? legalInfo);
    if (path === "/api/admin/plans") return ok(overrides.plans ?? adminPlans);
    if (path === "/api/branding") return ok(overrides.brand ?? { product_name: "", tagline: "", logo_data_url: "", favicon_data_url: "", brand_color: "", configured: false, source: "default" });
    if (path === "/api/admin/branding") return ok(overrides.brand ? { ...(overrides.brand as object), email_from_name: "", custom_domain: "planning.acme.test" } : { product_name: "", tagline: "", logo_data_url: "", favicon_data_url: "", brand_color: "", email_from_name: "", custom_domain: "", configured: false });
    if (path === "/api/notifications/settings") {
      // Whose settings: the tenant named in the header, else the deployment's
      // own row — as the server answers a platform admin (migration 091).
      const tenant = route.request().headers()["x-tenant-id"] ?? "";
      const scope = tenant
        ? { customer_id: tenant, deployment: false, inherited: true, deployment_settings_available: true }
        : { deployment: true, inherited: false, deployment_settings_available: true };
      return ok({ ...notificationSettings, scope });
    }
    if (path === "/api/admin/ai-settings") return ok(overrides.tenantAI ?? tenantAISettings);
    if (path === "/api/developer/integrations/google-service-account") return ok({ configured: false });
    if (path === "/api/demo") return ok(demoContextFor(route.request().headers()["x-dev-user"] ?? "dept_head"));
    // Every console's shell asks who is signed in. Without this the catch-all
    // below answered with [], which is not an actor.
    if (path === "/api/me" || path === "/api/admin/me") {
      const persona = route.request().headers()["x-dev-user"] ?? "dept_head";
      return ok({
        ...ctx.actor, roles: personaRoles[persona] ?? ctx.actor.roles,
        // A tenant member on a plan worth mentioning; platform admins have none.
        ...(overrides.plan && persona !== "platform_admin" ? { plan: overrides.plan, contact_url: "https://example.test/pricing" } : {}),
      });
    }
    if (path === "/api/developer/model") return ok({ app_name: "Planning", model_name: "Finance Model", model_id: "model-1", metrics });
    if (path === "/api/developer/dimensions") return ok(dimensions);
    if (path.startsWith("/api/developer/dimensions/") && path.endsWith("/properties")) return ok([{ id: "prop-1", dimension_id: "d1", name: "owner", data_type: "text" }]);
    if (path === "/api/developer/grids") return ok(grids);
    if (path === "/api/developer/dashboards") return ok(dashboards);
    if (path === "/api/dashboards") return ok(dashboards);
    if (path.startsWith("/api/dashboards/")) return ok(dashboards.find((d) => d.id === path.split("/").pop()) ?? dashboards[0]);
    if (path === "/api/grid") return ok(gridData);
    if (path === "/api/forms") return ok(forms);
    if (path.endsWith("/records")) return ok(records);
    if (path === "/api/tasks") return ok(tasks);
    if (path === "/api/workflow/history" || path === "/api/workflow/my-history") return ok(history);
    if (path === "/api/notifications") return ok(notifications);
    if (path === "/api/business-admin/roles") return ok(roles);
    if (path.includes("/api/business-admin/roles/") && path.endsWith("/members")) return ok([{ user_id: "u-1", display_name: "Alex Smith", email: "alex@example.com" }]);
    if (path === "/api/business-admin/users") return ok(baUsers);
    if (path.includes("/access-rules")) return ok([{ rule_type: "metric", ref_id: "m2", ref_name: "Marketing OPEX", access: "read" }]);
    if (path === "/api/business-admin/available") {
      const type = url.searchParams.get("type");
      if (type === "dashboards") return ok(dashboards.map((d) => ({ id: d.id, name: d.name, group: d.tags[0] })));
      if (type === "metrics") return ok(metrics.map((m) => ({ id: m.id, name: m.label, group: m.is_input ? "Input" : "Calculated" })));
      if (type === "dimension_members") return ok(dimensions.flatMap((d) => d.members.map((m) => ({ id: m.id, name: m.label, group: d.name }))));
      return ok(dimensions.map((d) => ({ id: d.id, name: d.name })));
    }
    if (path === "/api/automation/rules") return ok([{ id: "rule-1", application_id: "app-1", name: "Submit Budget", description: "Starts approval", trigger_type: "manual", workflow_name: "Budget Approval", enabled: true, created_at: "2026-05-01T00:00:00Z" }]);
    if (path === "/api/automation/executions") return ok([{ id: "exec-1", rule_id: "rule-1", application_id: "app-1", status: "completed", trigger_payload: {}, instance_id: "inst-1", started_at: "2026-05-19T10:00:00Z" }]);
    if (path === "/api/developer/integrations" || path === "/api/integrations") return ok(integrations);
    if (path === "/api/admin/tenants") return ok(overrides.plan ? adminTenants.map((t, i) => (i === 0 ? { ...t, plan: "small", plan_state: overrides.plan } : t)) : adminTenants);
    if (path === "/api/admin/users") return ok(adminUsers);
    if (path === "/api/admin/workspaces") return ok(adminWorkspaces);
    if (path === "/api/admin/audit") return ok(adminAudit);
    if (path === "/api/import/jobs") return ok([]);
    return ok([]);
  });
}

export async function loadAs(page: Page, persona: string, query = "") {
  await page.addInitScript((p) => localStorage.setItem("dev_persona", p), persona);
  await page.goto("/" + query);
  await page.waitForLoadState("networkidle");
}


/**
 * At platform scope the per-tenant settings tabs (SSO, SCIM, branding, AI
 * keys, delivery, retention) show nothing until the administrator says whose
 * settings they mean — the platform admin has no tenant of their own.
 */
export async function chooseTenant(page: Page, tenantId = "tenant-1") {
  await page.getByLabel("Settings scope").selectOption(tenantId);
}

import keycloak from "../auth/keycloak";

export interface DevMetric {
  id: string;
  name: string;
  label: string;
  is_input: boolean;
  formula?: string;
  agg_rule?: string;
  // Operands for agg_rule "rate": the total is numerator ÷ denominator.
  agg_numerator_metric_id?: string;
  agg_denominator_metric_id?: string;
  format: string;           // "number" | "percentage" | "currency" | "boolean" | "text"
  format_decimals: number;
  format_currency: string;
  depends_on: string[];
  depended_by: string[];
  calc_error?: string;
}

export interface DevModel {
  app_name: string;
  model_name: string;
  model_id: string;
  metrics: DevMetric[];
}

export interface Department {
  code: string;
  label: string;
}

export interface DimMember {
  id: string;
  code: string;
  label: string;
  parent_code?: string; // set if this member rolls up to a parent; empty/absent = root
  readonly?: boolean;   // true = "read" access rule — visible but not editable
  properties?: Record<string, string>; // arbitrary per-member key/values (e.g. {"region":"LUX"}); consumed by dimensions with source_property set
}

export interface DimInfo {
  id: string;
  name: string;
  display_level: number | null; // null=all, 0=roots, -1=leaves, n=depth n
  parent_dimension_id?: string | null; // set when this whole dimension is a declared child of another (e.g. Cabinet -> Department)
  source_dimension_id?: string | null; // set when this dimension's members are a grouping of source_dimension_id's members by their properties[source_property] value
  source_property?: string | null;
  members: DimMember[];
}

export interface GridData {
  revision_id: string;
  revision: string;
  metrics: Metric[];
  all_metrics: Metric[];           // every metric in the revision, regardless of grid membership
  dimensions: DimInfo[];           // all configured dims, ordered
  all_dimensions: DimInfo[];       // every dimension in the revision, regardless of grid — needed to resolve cross-dimension formula refs
  departments: Department[];       // = first dim members (compat)
  cells: Record<string, number>;   // "metricId:code1[:code2...]" composite key, keyed per each metric's OWN grid dims
  totals: Record<string, number>;  // "metricId" -> aggregate
  access_rules: {
    dim_members: Record<string, string>; // memberID → "write"|"read"|"hidden"
    metrics:     Record<string, string>; // metricID  → "write"|"read"|"hidden"
  };
  rollup_source_grid_id?: string | null; // set = this grid mirrors another grid's metrics via cross-dimension rollup; its cells are read-only
}

export interface Actor {
  user_id: string;
  email: string;
  display_name: string;
  roles: string[];
}

/** Outbound notification delivery for this tenant (GET/PUT /api/notifications/settings).
 *  webhook_secret is write-only: it never comes back, and sending an empty one
 *  keeps the stored value. mailer_configured says whether the deployment has an
 *  SMTP relay at all. */
export interface NotificationSettings {
  email_enabled: boolean;
  webhook_enabled: boolean;
  webhook_url: string;
  webhook_secret?: string;
  has_webhook_secret?: boolean;
  reminders_enabled: boolean;
  reminder_lead_hours: number;
  mailer_configured?: boolean;
}

/** A tenant's single sign-on provider (enterprise `sso`). client_secret is
 *  write-only; broker_endpoint / sp_entity_id are what the tenant registers
 *  at their identity provider. */
export interface SsoSettings {
  alias?: string;
  protocol: "oidc" | "saml";
  display_name: string;
  metadata_url: string;
  client_id: string;
  client_secret?: string;
  allowed_domains: string[];
  jit_provisioning: boolean;
  default_role: string;
  enabled: boolean;
  configured?: boolean;
  broker_endpoint?: string;
  sp_entity_id?: string;
  registry_configured?: boolean;
}

export interface SsoDiscovery {
  sso: boolean;
  alias?: string;
  display_name?: string;
}

/** A SCIM bearer token; `token` is present only in the creation response. */
export interface ScimToken {
  id: string;
  name: string;
  default_role: string;
  workspace_id?: string;
  created_at: string;
  last_used_at?: string;
  revoked_at?: string;
  token?: string;
  base_url?: string;
}

/** One value a cell held (enterprise `cell_history`), newest first from the API. */
export interface CellHistoryEntry {
  id: string;
  value: number;
  entered_at: string;
  entered_by: { id: string; name: string; email: string };
  source: { kind: "typed" | "form" | "copied"; ref?: string; label?: string };
  current: boolean;
  deleted_at?: string;
  delete_reason?: string;
}

/** Audit retention (enterprise `audit_export`): 0 keeps events forever. */
export interface AuditSettings {
  retention_days: number;
  min_retention_days?: number;
  updated_at?: string;
}

/** One tenant's usage (enterprise `usage_analytics`). */
export interface TenantUsage {
  customer_id: string;
  name: string;
  plan: string;
  created_at: string;
  users: number;
  active_users: number;
  applications: number;
  models: number;
  revisions: number;
  fact_rows: number;
  calc_rows: number;
  form_records: number;
  object_bytes: number;
  db_bytes: number;
  integration_runs: number;
  workflow_instances: number;
  ai_messages: number;
  audit_events: number;
  last_activity_at?: string;
}

export interface UsageReport {
  period_days: number;
  since: string;
  tenants: TenantUsage[];
}

/** What the console applies (GET /api/branding, public). */
export interface BrandView {
  product_name: string;
  tagline: string;
  logo_data_url: string;
  favicon_data_url: string;
  brand_color: string;
  configured: boolean;
  source: "tenant" | "host" | "default";
}

/** A tenant's branding as its admin edits it (feature `white_label`). */
export interface Branding {
  product_name: string;
  tagline: string;
  logo_data_url: string;
  favicon_data_url: string;
  brand_color: string;
  email_from_name: string;
  custom_domain: string;
  configured?: boolean;
  updated_at?: string;
}

/** Product tier a deployment runs in (GET /api/license). */
export type Edition = "community" | "commercial" | "enterprise";
export type LicenseState = "community" | "active" | "expired" | "invalid";
export interface LicenseFeature {
  key: string;
  name: string;
  description: string;
  min_edition: Edition;
}
/** Mirrors pkg/license.Status: edition is the EFFECTIVE one (community when
 *  no key, or the key is invalid or expired); the key's own details stay
 *  filled so the console can say what ran out. */
// ── Plans, trials and sign-up (internal/plan, docs/PLANS_AND_TRIALS.md) ──────

/** What a plan allows; 0 means unlimited. Keys are the server's. */
export interface PlanLimits {
  max_users: number;
  max_applications: number;
  max_models: number;
  max_metrics_per_model: number;
  max_members_per_dimension: number;
  max_fact_rows_per_model: number;
  max_ai_messages_per_day: number;
  max_integration_runs_per_day: number;
}

export interface PlanDef {
  key: string;
  name: string;
  description: string;
  trial_days: number;
  self_service: boolean;
  limits: PlanLimits;
  sort_order: number;
  updated_at?: string;
}

/** A tenant's plan as it applies right now (GET /api/me, tenant listing). */
export interface PlanState {
  plan: PlanDef;
  plan_known: boolean;
  trial: boolean;
  trial_ends_at?: string;
  days_left: number;
  read_only: boolean;
  code?: "trial_expired" | "over_limit";
  reason?: string;
  limit_state: string;
  limit_reason?: string;
  usage_checked_at?: string;
}

export interface Me {
  user_id: string;
  email: string;
  display_name: string;
  roles: string[];
  customer_id?: string;
  plan?: PlanState;
  contact_url?: string;
}

export interface SignupOptions {
  enabled: boolean;
  reason?: string;
  contact_url?: string;
  plan?: { key: string; name: string; description: string; trial_days: number; limits: PlanLimits };
}

export interface SignupRequest {
  company: string;
  first_name: string;
  last_name: string;
  email: string;
}

export interface SignupResult {
  status: "created" | "invited";
  invited: boolean;
  email: string;
  tenant_id: string;
  application_id: string;
  model_id: string;
  plan: string;
  trial_ends_at?: string;
  /** Dev stack only: the persona the new account is reachable as. */
  dev_persona?: string;
}

export interface LicenseInfo {
  edition: Edition;
  state: LicenseState;
  features: string[];
  catalog: LicenseFeature[];
  source: "env" | "file" | "none";
  license_id?: string;
  customer?: string;
  contact?: string;
  issued_at?: string;
  expires_at?: string;
  limits?: Record<string, number>;
  error?: string;
}

export interface AppModelInfo {
  id: string;
  name: string;
  is_default: boolean;
  active_revision: string;
}

export interface AppInfo {
  id: string;
  name: string;
  mode: string;
  workspace_name: string;
  model_name: string;
  active_revision: string;
  models?: AppModelInfo[];
}

export interface DemoContext {
  app_id: string;
  model_id: string;
  revision_id: string;
  revision: string;
  actor: Actor;
}

export interface Metric {
  id: string;
  name: string;
  label: string;
  is_input: boolean;
  formula?: string;
  agg_rule: string;
  format: string;           // "number" | "percentage" | "currency" | "boolean" | "text"
  format_decimals: number;
  format_currency: string;
  value: number | null;
  readonly?: boolean;   // true = "read" access rule — visible but not editable
  dimension_ids?: string[]; // this metric's OWN grid's dimension IDs, ordered (populated by /api/metrics and /api/grid's all_metrics)
}

export interface Task {
  id: string;
  step_def_id: string;
  step_name: string;
  step_type: string;
  instructions: string;
  sla_hours?: number;
  required_comment: boolean;
  completion_label: string;
  workflow_name: string;
  instance_id: string;
  context: Record<string, string>;
  status: string;
  due_at?: string;
  created_at: string;
  requested_by?: string;
  context_display?: TaskContextEntry[];
  /** Set on a condition step that waits for a human to pick the branch. */
  condition?: StepCondition | null;
  /** > 0 when a later decision sent this step back; the note says which step and why. */
  rework_count?: number;
  rework_note?: string;
}

// One human-readable context line: value is the stable code, display the
// resolved name ("CA" → "Canada"), dimension_name the dimension it lives in.
export interface TaskContextEntry {
  key: string;
  value: string;
  display: string;
  dimension_name?: string;
}

export interface WorkflowContextVar {
  key: string;
  label?: string;
  data_type?: string;
  dimension_id?: string;
}

export interface WorkflowDefPublic {
  id: string;
  name: string;
  description: string;
  trigger_event: string;
  context_schema: WorkflowContextVar[];
}

export interface HistoryStep {
  id: string;
  step_def_id: string;
  step_name: string;
  step_type: string;
  status: string;
  decision?: string;
  comment?: string;
  completed_at?: string;
}

export interface HistoryInstance {
  id: string;
  workflow_name: string;
  status: string;
  context: Record<string, string>;
  started_at: string;
  completed_at?: string;
  steps: HistoryStep[];
  context_display?: TaskContextEntry[];
}

export interface DevDimensionMember {
  id: string;
  code: string;
  label: string;
  parent_member_id?: string | null;
  // Per-member property values (dimension_member.properties).
  properties?: Record<string, string>;
}

export interface DimProperty {
  id: string;
  dimension_id: string;
  name: string;
  data_type: string;
}

export interface DevDimension {
  id: string;
  name: string;
  agg_rule: string;
  parent_dimension_id?: string | null; // set when this whole dimension is a declared child of another (e.g. Cabinet -> Department)
  members: DevDimensionMember[];
}


export interface GridDef {
  id: string;
  name: string;
  revision_id: string;
  metric_ids: string[];
  dimension_ids: string[];
  dimension_levels: Record<string, number | null>; // dim_id → display_level
}

export interface GridDefaultView {
  rows: string[];     // ordered IDs: "__metrics__" or dimension id
  cols: string[];
  context: string[];
  filter_sel?: Record<string, string>; // dim_id → default member code
}

// ── Chart widget types ────────────────────────────────────────────────────────

export type GridChartType = "bar" | "line" | "pie" | "scatter" | "histogram";

export type WidgetBackground = "white" | "none";
export type SelectorsPosition = "top" | "bottom" | "left" | "right";

export interface GridChartConfig {
  chart_type: GridChartType;
  dimension_id: string;
  metric_ids: string[];
  x_metric_id?: string;
  y_metric_id?: string;
  context_defaults: Record<string, string>;
  bin_count?: number;
  show_legend?: boolean;
  show_values?: boolean;
  value_format?: "number" | "currency" | "percent" | "compact";
  refresh_seconds?: 2 | 5 | 10 | 30;
  // Drop the plotted dimension's rollup members ("All Regions" and the like).
  // A total is the sum of everything beside it, so plotting one next to its
  // own children produces a bar that dwarfs the rest. Context selectors keep
  // theirs — choosing "All Regions" there is meaningful, not a distortion.
  hide_rollup_members?: boolean;
}

export interface GridChartCategory {
  key: string;
  label: string;
}

export interface ChartMetricFormat {
  format: string;
  format_decimals: number;
  format_currency: string;
}

export interface GridChartSeries extends ChartMetricFormat {
  metric_id: string;
  label: string;
  values: Array<number | null>;
}

// ChartContextMember/ChartContextDim are the lean, server-resolved shape for
// a chart's context selectors — no id or other hierarchy internals, since
// rollup now happens server-side (see internal/rollup and chart.go). Still
// structurally compatible with dashboardLayout.ts's defaultLeafCode, which
// only ever reads code/parent_code.
export interface ChartContextMember {
  code: string;
  label: string;
  parent_code?: string;
}

export interface ChartContextDim {
  id: string;
  name: string;
  members: ChartContextMember[];
}

export interface CategoryChartData {
  chart_type: "bar" | "line" | "pie";
  as_of: string;
  categories: GridChartCategory[];
  series: GridChartSeries[];
  context_dims: ChartContextDim[];
  /** The context the data was resolved for — differs from the request when a
      requested member is hidden from this viewer and the server substituted
      the dimension's default visible member. */
  context?: Record<string, string>;
}

export interface ScatterChartData {
  chart_type: "scatter";
  as_of: string;
  x_metric: { id: string; label: string } & ChartMetricFormat;
  y_metric: { id: string; label: string } & ChartMetricFormat;
  points: Array<{ key: string; label: string; x: number; y: number }>;
  context_dims: ChartContextDim[];
  /** The context the data was resolved for — differs from the request when a
      requested member is hidden from this viewer and the server substituted
      the dimension's default visible member. */
  context?: Record<string, string>;
}

export interface HistogramChartData {
  chart_type: "histogram";
  as_of: string;
  metric: { id: string; label: string } & ChartMetricFormat;
  observation_count: number;
  bins: Array<{ min: number; max: number; count: number }>;
  context_dims: ChartContextDim[];
  /** The context the data was resolved for — differs from the request when a
      requested member is hidden from this viewer and the server substituted
      the dimension's default visible member. */
  context?: Record<string, string>;
}

export type ChartData = CategoryChartData | ScatterChartData | HistogramChartData;

export interface WidgetProps {
  // Where a grid/chart/KPI widget shows its context selectors: along the
  // top edge (default), the bottom edge, or as a column on the left/right.
  selectors_position?: SelectorsPosition;
  /** Widget surface: "white" draws the widget on a card (surface + border),
      "none" draws it straight on the page. Absent = the type's default
      (KPI tiles are cards, everything else is bare). */
  background?: WidgetBackground;
  font_size?: number;
  font_weight?: string;
  color?: string;
  font_family?: string;
  button_color?: string;
  default_view?: GridDefaultView;
  chart?: GridChartConfig;
  context?: Record<string, string>; // static workflow context merged in by automation_button widgets, e.g. {"target_revision_id": "<uuid>"}
  // metric_kpi: scope the shown value to one dimension member instead of
  // the metric's whole-model grand total, e.g. {dimension_id: region_id,
  // member_code: "APAC"} -> sums only cells where the metric's region
  // dimension equals APAC (other dimensions the metric has, if any, are
  // still summed over in full).
  kpi_scope?: { dimension_id: string; member_code: string };
  // metric_kpi: how the tile picks its default context, set by the developer
  // in the widget editor. "total" = whole-model grand total; "sync" = follow
  // the dashboard's shared context selectors; "pin" = fixed to kpi_scope's
  // member. Absent falls back to the legacy behaviour (pin if kpi_scope set,
  // else sync unless sync_context===false, else total).
  kpi_context_mode?: "total" | "sync" | "pin";
  // automation_button: when set, clicking asks for confirmation with this
  // message before firing the rule. Empty/absent fires immediately.
  confirm_text?: string;
  // chart/grid: when true, this widget's context-selector choices are
  // shared with every other sync_context widget on the same dashboard,
  // matched by dimension id (see DashboardContextSyncProvider).
  sync_context?: boolean;
}


// ── REST API connector (developer console → Integrations → REST API) ──────────
// Discriminated, typed DTOs mirroring internal/integration's Config document.

export type ApiDirection = "pull" | "push";
export type ApiTargetType = "grid" | "form" | "dimension";
export type ApiBodyMode = "none" | "json" | "form" | "raw";
export type ApiPaginationMode = "none" | "page_number" | "offset_limit" | "cursor" | "link_header";
export type ApiAuthType = "none" | "api_key" | "bearer" | "basic" | "oauth2_client_credentials";
export type ApiImportMode = "incremental" | "replace" | "full_reload";

export interface ApiKV { key: string; value: string; enabled: boolean }
export interface ApiTransform { kind: string; value?: string; lookup?: Record<string, string> }
export interface ApiFieldMap { source: string; target: string; target_kind?: string; transforms?: ApiTransform[] }

export interface ApiIntegrationConfig {
  kind: "rest_api/v1";
  direction: ApiDirection;
  target_type: ApiTargetType;
  target_id: string;
  import_mode?: ApiImportMode;
  request: {
    method: string; url: string;
    query?: ApiKV[]; headers?: ApiKV[];
    body_mode: ApiBodyMode; body_json?: string; body_form?: ApiKV[]; body_raw?: string; content_type?: string;
    timeout_seconds?: number; max_retries?: number; retry_backoff_ms?: number;
    rate_limit_rps?: number; idempotency_key_header?: string;
  };
  auth: { type: ApiAuthType; header_name?: string; query_param?: string };
  response?: { format: "json" | "csv"; records_path?: string };
  pagination?: { mode: ApiPaginationMode; start_page?: number; page_size?: number; cursor_path?: string; max_pages?: number };
  mapping: {
    fields: ApiFieldMap[]; shape?: "wide" | "long";
    metric_name_source?: string; value_source?: string;
    batch?: boolean; batch_property?: string; batch_size?: number;
  };
  limits?: { max_records?: number; max_requests?: number; failure_threshold?: number };
}

export interface ApiSchedule {
  integration_id?: string;
  kind: "manual" | "interval" | "cron";
  interval_seconds?: number; cron_expr?: string; timezone?: string;
  enabled: boolean; overlap_policy?: string; misfire_policy?: string;
  next_fire_at?: string | null; last_fire_at?: string | null;
}

export interface ApiIntegrationDetail {
  id: string; model_id: string; revision_id?: string;
  name: string; description: string; status: "draft" | "active";
  tags: string[]; direction: ApiDirection; enabled: boolean;
  connection_id?: string; config?: ApiIntegrationConfig;
  config_version: number; last_tested_at?: string | null;
  created_at: string; tested: boolean; schedule?: ApiSchedule;
}

export interface IntegrationConnection {
  id: string; application_id: string; name: string; auth_type: ApiAuthType;
  meta: Record<string, unknown>; has_secret: boolean; created_at: string; updated_at: string;
}

export interface ApiRun {
  id: string; integration_id: string;
  status: "queued" | "running" | "success" | "partial" | "failed" | "cancelled";
  trigger_type: string; dry_run: boolean; run_by?: string;
  created_at: string; started_at?: string; finished_at?: string;
  http_status?: number; duration_ms: number; pages: number; requests: number; retries: number;
  records_read: number; records_written: number; records_skipped: number;
  error_code?: string; message?: string; meta?: Record<string, string>;
}

export interface DashboardWidget {
  id: string;
  widget_type: string;
  ref_id: string | null;
  content: string | null;
  title: string | null;
  show_title: boolean;
  widget_props: WidgetProps | null;
  sort_order: number;
  col_start: number;
  col_span: number;
  pos_x: number;
  pos_y: number;
  size_w: number;
  size_h: number;
}

export interface DashboardFolder {
  id: string;
  name: string;
  parent_id: string | null;
}

export interface DashboardDef {
  id: string;
  name: string;
  tags: string[];
  folder_id: string | null;
  widgets: DashboardWidget[];
}

export interface AdminRevision {
  id: string;
  name: string;
  created_at: string;
}

export interface DevRevision {
  id: string;
  name: string;
  description: string;
  created_at: string;
  is_active: boolean;
}

export interface AdminModel {
  id: string;
  name: string;
  storage_type: string;
  active_revision: string | null;
  // The app's business-default model — what business consoles resolve.
  is_default?: boolean;
  revisions: AdminRevision[];
}

// A switchable dev-mode persona: every user in the database. `key` is the
// value to send as X-Dev-User (legacy short key or keycloak_sub).
export interface DevPersona {
  key: string;
  label: string;
  email: string;
  roles: string[];
  tenant: string;
}

// Self-contained model-at-a-revision package produced by exportModel and
// accepted by importModel. Entity payloads are opaque to the frontend.
export interface ModelExportPackage {
  format: string;
  version: number;
  model_name: string;
  revision_name: string;
  include_data?: boolean;
  [key: string]: unknown;
}

export interface AdminApp {
  id: string;
  name: string;
  mode: string;
  status: string;
  models: AdminModel[];
}

export interface AdminTenant {
  id: string;
  name: string;
  plan: string;
  created_at: string;
  applications: AdminApp[];
  /** The plan as it applies right now: trial, days left, read-only and why. */
  plan_state?: PlanState;
}

export interface UserAssignment {
  role: string;
  workspace_id: string;   // empty string = platform-level (no workspace)
  workspace_name: string;
  customer_name: string;
}

export interface AdminUser {
  id: string;
  email: string;
  display_name: string;
  created_at: string;
  /** Set when the account has been deactivated (SCIM); such a user cannot sign in. */
  disabled_at?: string | null;
  assignments: UserAssignment[];
  app_ids: string[];
  model_ids: string[];
}

export interface AdminWorkspace {
  id: string;
  name: string;
  customer_name: string;
  customer_id: string;
}

export interface AdminAuditEvent {
  id: string;
  category: string;
  event_type: string;
  actor_name: string;
  actor_role: string;
  application_id: string;
  application_name: string;
  resource_type: string;
  resource_id: string;
  revision_id: string;
  revision_name: string;
  metadata: Record<string, unknown>;
  occurred_at: string;
}

export interface Notification {
  id: string;
  template_id: string;
  status: string;
  template_vars: Record<string, string> | null;
  created_at: string;
  delivered_at?: string;
  resource_type?: string;
  resource_id?: string;
}

export interface FormField {
  name: string;
  label: string;
  type: "text" | "number" | "select" | "date" | "boolean" | "dimension" | "metric";
  required: boolean;
  options?: string[];
  // dimension type
  dimension_id?: string;
  allowed_members?: string[];
  // metric type
  metric_id?: string;
  value_field?: string;
}

export interface FormDef {
  id: string;
  model_id: string;
  name: string;
  label: string;
  fields: FormField[];
  created_at: string;
}

export interface FormRecord {
  id: string;
  form_id: string;
  data: Record<string, unknown>;
  status: string;
  created_by: string;
  created_at: string;
  updated_at: string;
}

export interface InfraNode {
  node: string;
  disk_total_gb: number;
  disk_used_gb: number;
  disk_pct: number;
  mem_total_mb: number;
  mem_available_mb: number;
  load1: number;
  collected_at: string;
  stale: boolean;
}

export interface IntegrationRun {
  id: string;
  status: string;
  rows_imported: number;
  error_rows: number;
  message: string;
  created_at: string;
  run_by: string;
}

export interface IntegrationDef {
  id: string;
  name: string;
  type: string;
  target_type: string;
  target_id: string;
  // "draft" = saved work-in-progress from the Import Wizard, not runnable;
  // "active" = complete. Tags are free-form labels for organizing the list.
  status?: string;
  tags?: string[];
  config: {
    column_map?: Record<string, string>;
    // google_sheets integrations only: the link-shared sheet re-fetched on
    // every run, and the commit mode (server defaults to "replace" so that
    // re-syncing converges instead of double-counting).
    sheet_url?: string;
    import_mode?: ImportModeParam;
  };
}

export interface FormMetricMapping {
  id: string;
  form_id: string;
  grid_id?: string;
  name: string;
  source_field: string;
  target_metric_id: string;
  aggregation: string;
  posting_statuses: string[];
  dimension_mappings: Record<string, string>;
  live_posting: boolean;
}

export interface FormRecordPosting {
  form_record_id: string;
  target_metric_id: string;
  revision_id: string;
  dim_members: Record<string, string>;
  posted_value: number | null;
  posted_at: string;
}

export interface AutomationRule {
  id: string;
  application_id: string;
  name: string;
  description: string;
  trigger_type: string;
  workflow_name: string;
  workflow_def_id?: string;
  source_form_id?: string;
  source_grid_id?: string;
  /** integration_completed / integration_failed rules: one integration, or any when empty. */
  source_integration_id?: string;
  enabled: boolean;
  created_at: string;
}

// ── Workflow Definitions ──────────────────────────────────────────────────────

export type WorkflowStatus = "draft" | "published" | "archived" | "invalid";

export type StepType = "task" | "approval" | "notification" | "condition" | "join";

export interface StepCondition {
  left: string;
  operator: string;
  right: string | number | boolean;
}

export interface StepNotification {
  recipient_type: "role" | "requester";
  recipient_role?: string;
  subject?: string;
  message?: string;
}

export interface WorkflowStepDef {
  id: string;
  name: string;
  type: StepType;
  instructions?: string;
  assignee_roles?: string[];
  sla_hours?: number;
  required_comment?: boolean;
  completion_label?: string;
  condition?: StepCondition;
  routes?: Record<string, string>;
  notification?: StepNotification;
}

export interface ContextVariable {
  key: string;
  label: string;
  data_type: string;
  required: boolean;
  default_value?: string;
  // How this variable's value is populated when the workflow starts.
  // "manual" (default) = caller/widget supplies it. "raci_responsible" =
  // server-resolved (and enforced) from the caller's security.raci_rule
  // "responsible" grant, overriding any client-supplied value — only
  // meaningful when data_type is "Dimension member".
  source_hint?: string;
  // Which dimension this variable refers to, when data_type is
  // "Dimension member" — lets the platform generically resolve/validate
  // the value against model.dimension_member and (for source_hint
  // "raci_responsible") derive it from RACI, and lets a running instance's
  // context be matched against a dimension_member's ancestor chain to
  // determine whether a write should be locked while the workflow runs.
  dimension_id?: string;
}

export interface WorkflowDefSummary {
  id: string;
  application_id: string;
  name: string;
  description: string;
  trigger_event: string;
  status: WorkflowStatus;
  step_count: number;
  created_at: string;
  updated_at: string;
  published_at?: string;
  archived_at?: string;
}

export type WorkflowSubjectType = "" | "grid" | "grid_metric" | "form_records" | "form_record";

export interface WorkflowDef {
  id: string;
  application_id: string;
  name: string;
  description: string;
  trigger_event: string;
  subject_type: WorkflowSubjectType;
  subject_config: Record<string, string>;
  status: WorkflowStatus;
  steps: WorkflowStepDef[];
  context_schema: ContextVariable[];
  /** One running instance per dimension-member scope (a second start with the
      same members is refused as a duplicate). Default true. */
  single_active_instance: boolean;
  created_at: string;
  updated_at: string;
  published_at?: string;
  archived_at?: string;
}

export interface WorkflowDefUsage {
  rule_id: string;
  rule_name: string;
  trigger_type: string;
  enabled: boolean;
}

export interface WorkflowValidationResult {
  valid: boolean;
  errors: string[];
}

export interface TriggerPayloadField {
  key: string;
  type: string;
  required: boolean;
}

export interface TriggerEventCatalogItem {
  key: string;
  label: string;
  description: string;
  category: "manual" | "api" | "form" | "integration" | "planning";
  source_type: string;
  source_id?: string;
  source_name?: string;
  payload_schema: TriggerPayloadField[];
  enabled: boolean;
  created_from: string;
}

export interface Execution {
  id: string;
  rule_id: string;
  application_id: string;
  status: string;
  trigger_payload: Record<string, string>;
  instance_id?: string;
  error?: string;
  started_at: string;
  completed_at?: string;
  /** Instance rows (GET /api/developer/workflows/{id}/instances) also carry
      the instance's resolved context and whether it was a developer test run. */
  context?: Record<string, string>;
  test_run?: boolean;
}

export interface ImportJob {
  id: string;
  model_id: string;
  revision_name: string;
  status: number;
  total_rows: number;
  valid_rows: number;
  error_rows: number;
  // Go serialises protobuf Timestamp as {seconds, nanos} via encoding/json
  created_at: string | { seconds: number; nanos: number };
}

export type ImportModeParam = "incremental" | "replace" | "full_reload";

export interface ImportUploadResult {
  job_id: string;
  status: string;
  total_rows: number;
  valid_rows: number;
  error_rows: number;
  revision_id?: string;
}

// Body of the 422 atomic-rejection response from /api/import/upload — every
// row that failed validation; nothing was staged or committed.
export interface ImportRowError {
  row: number;
  column: string;
  code: string;
  message: string;
  raw_value?: string;
}

export interface FormImportRowError {
  row: number;
  column: string;
  message: string;
}

export interface MigrationFile {
  filename: string;
  sql: string;
  rollback_sql: string;
  checksum: string;
}

export interface BARole {
  id: string;
  name: string;
  member_count: number;
  dashboard_ids: string[];
}

export interface BARoleMember {
  user_id: string;
  display_name: string;
  email: string;
}

export interface BAUser {
  id: string;
  display_name: string;
  email: string;
  role: string;
}

export interface UserAccessRule {
  rule_type: "dimension" | "metric" | "button";
  ref_id: string;
  ref_name: string;
  access: "write" | "read" | "hidden";
}

export interface BAAvailableItem {
  id: string;
  name: string;
  group?: string; // dimension name for dimension_members type
}

export interface AISession {
  id: string;
  app_id: string;
  llm_provider: string;
  llm_model: string;
  // Set once the session's first confirmed proposal creates an isolated
  // draft revision — AI writes land here, never directly in the active one.
  draft_revision_id?: string;
  // Auto-generated from the first request; renameable. Empty = unnamed.
  title?: string;
  created_at: string;
}

export interface AIMessage {
  id: string;
  session_id: string;
  role: "user" | "assistant" | "tool";
  content: string;
  tool_calls?: { id: string; name: string }[];
  created_at: string;
}

export interface AISettings {
  provider: string;
  model: string;
  has_key: boolean;
  /** True when this tenant supplies a key (enterprise `tenant_ai_keys`). */
  tenant_key?: boolean;
  /** True when the tenant key is the only key used and personal keys are ignored. */
  tenant_enforced?: boolean;
  tenant_provider?: string;
}

/** The tenant-level AI provider key (enterprise). `api_key` is write-only:
 *  it is never returned, and sending an empty one keeps the stored key. */
export interface TenantAISettings {
  provider: string;
  model: string;
  api_key?: string;
  has_key: boolean;
  enforced: boolean;
}

/** Result of a live probe that a provider/model/key can actually call tools. */
export interface ProviderProbeResult {
  ok: boolean;
  provider: string;
  model: string;
  reply?: string;
  error?: string;
}

export interface AIDocument {
  id: string;
  session_id: string;
  filename: string;
  mime_type: string;
  char_count: number;
  truncated: boolean;
  created_at: string;
}

export interface AITestResult {
  ok: boolean;
  provider: string;
  model: string;
  reply?: string;
  error?: string;
}

export interface AIProposalStep {
  tool: string;
  description: string;
  params: Record<string, unknown>;
  // set after execution:
  status?: "pending" | "success" | "failed";
  result?: string;
  created_id?: string;
}

export interface AIProposal {
  id: string;
  session_id: string;
  steps: AIProposalStep[];
  status: "pending" | "confirmed" | "rejected" | "executed" | "partial";
  created_at: string;
  executed_at?: string;
}

// AIProposal plus a human-readable one-liner — what GET .../proposals (the
// Activity panel's data source) returns for every proposal in a session,
// not just the still-pending ones.
export interface AIProposalWithSummary extends AIProposal {
  summary: string;
}

// One SSE event from the streaming /messages endpoint. Fields are populated
// according to `type` — see internal/gateway/ai_handler.go's sendSSE calls
// for the exact payload shape per event type.
export interface AISendMessageEvent {
  type: "delta" | "tool_status" | "proposal" | "done" | "error";
  content?: string; // delta
  tool?: string; // tool_status
  error?: string; // error
  proposal?: AIProposal; // proposal
  reply?: AIMessage; // done
  messages?: AIMessage[]; // proposal | done
  session?: AISession; // proposal | done
}

function persona(): string {
  return localStorage.getItem("dev_persona") ?? "dept_head";
}

// The tenant a request acts on, when it is not the caller's own. Only a
// platform admin has no tenant of their own, so only the platform console
// sets this — for everyone else the server routes by membership. Read
// synchronously while the request headers are built, so withTenant can wrap
// one call without leaking into concurrent ones.
let actingTenantId = "";

/**
 * Run `fn` with requests addressed to `tenantId`. Used by the platform
 * console, where one admin acts across tenants that each have their own
 * database and the id cannot be inferred from the request body.
 */
export function withTenant<T>(tenantId: string, fn: () => Promise<T>): Promise<T> {
  const previous = actingTenantId;
  actingTenantId = tenantId;
  try {
    return fn();
  } finally {
    actingTenantId = previous;
  }
}

// authHeader reads the Keycloak singleton's token directly (see
// ../auth/keycloak) rather than through React context, so it always has the
// latest value — including a token ProdAuthProvider's background refresh
// (AuthProvider.tsx) just rotated in — with no extra plumbing. In dev mode
// the singleton is never initialized, so this is always {} there.
function authHeader(): Record<string, string> {
  return keycloak.token ? { Authorization: `Bearer ${keycloak.token}` } : {};
}

// exportQuery builds the query string shared by the model export endpoints.
function exportQuery(revisionId?: string, includeData = true): string {
  const q = new URLSearchParams();
  if (revisionId) q.set("revision_id", revisionId);
  if (!includeData) q.set("include_data", "false");
  const str = q.toString();
  return str ? `?${str}` : "";
}

async function apiFetch<T>(path: string, init?: RequestInit, retrying = false): Promise<T> {
  const appId = localStorage.getItem("selected_app_id") ?? "";
  const modelId = localStorage.getItem("selected_model_id") ?? "";
  const res = await fetch(path, {
    ...init,
    headers: {
      "Content-Type": "application/json",
      "X-Dev-User": persona(),
      ...(actingTenantId ? { "X-Tenant-Id": actingTenantId } : {}),
      ...authHeader(),
      ...(appId ? { "X-App-Id": appId } : {}),
      ...(modelId ? { "X-Model-Id": modelId } : {}),
      ...(init?.headers ?? {}),
    },
  });
  if (!res.ok) {
    const body = await res.json().catch(() => ({ error: res.statusText }));
    // A stale selected_app_id (e.g. after the database was reseeded) makes the
    // backend fail app/model scoping on every request. Drop it and retry once
    // without the header so the app re-resolves the user's primary application.
    // Only the scoping errors self-heal — genuine permission 403s must surface.
    const scopingError = typeof body.error === "string" && /resolve (model|app)/.test(body.error);
    if (res.status === 403 && appId && !retrying && scopingError) {
      localStorage.removeItem("selected_app_id");
      return apiFetch<T>(path, init, true);
    }
    // Structured error bodies (e.g. import's per-row validation errors) are
    // attached to the thrown Error rather than discarded, so any caller that
    // needs more than the message can read err.body — generic to every
    // endpoint, not special-cased per response shape.
    throw Object.assign(new Error(`${res.status}: ${body.error ?? res.statusText}`), { status: res.status, body });
  }
  return res.json() as Promise<T>;
}

/** Thrown by apiFetch on a non-2xx response; carries the parsed JSON error body. */
export interface ApiError extends Error {
  status: number;
  body: Record<string, unknown>;
}

// apiFetchBlob is apiFetch's counterpart for file-download endpoints (CSV/
// XLSX export): same auth headers, but returns the raw Blob plus whatever
// filename the server chose (Content-Disposition), instead of parsing JSON.
async function apiFetchBlob(path: string): Promise<{ blob: Blob; filename: string }> {
  const appId = localStorage.getItem("selected_app_id") ?? "";
  const modelId = localStorage.getItem("selected_model_id") ?? "";
  const res = await fetch(path, {
    headers: {
      "X-Dev-User": persona(),
      ...(actingTenantId ? { "X-Tenant-Id": actingTenantId } : {}),
      ...authHeader(),
      ...(appId ? { "X-App-Id": appId } : {}),
      ...(modelId ? { "X-Model-Id": modelId } : {}),
    },
  });
  if (!res.ok) {
    const body = await res.json().catch(() => ({ error: res.statusText }));
    throw Object.assign(new Error(`${res.status}: ${body.error ?? res.statusText}`), { status: res.status, body });
  }
  const match = /filename="?([^"]+)"?/.exec(res.headers.get("Content-Disposition") ?? "");
  return { blob: await res.blob(), filename: match?.[1] ?? "export" };
}

// parseSSEFrame parses one "event: <type>\ndata: <json>" block (already
// split on the blank-line delimiter) into a typed event, or null for a
// frame with no data line (e.g. a bare comment/keepalive).
function parseSSEFrame(frame: string): AISendMessageEvent | null {
  let eventType = "message";
  let data = "";
  for (const line of frame.split("\n")) {
    if (line.startsWith("event:")) eventType = line.slice(6).trim();
    else if (line.startsWith("data:")) data += line.slice(5).trim();
  }
  if (!data) return null;
  const parsed = JSON.parse(data) as Record<string, unknown>;
  return { type: eventType as AISendMessageEvent["type"], ...parsed };
}

// streamSSE POSTs a JSON body and invokes onEvent for each SSE frame in the
// response as it arrives. Bypasses apiFetch since that assumes a single
// buffered JSON response.
async function streamSSE(
  path: string,
  body: unknown,
  onEvent: (event: AISendMessageEvent) => void,
): Promise<void> {
  const appId = localStorage.getItem("selected_app_id") ?? "";
  const modelId = localStorage.getItem("selected_model_id") ?? "";
  const res = await fetch(path, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "X-Dev-User": persona(),
      ...(actingTenantId ? { "X-Tenant-Id": actingTenantId } : {}),
      ...authHeader(),
      ...(appId ? { "X-App-Id": appId } : {}),
      ...(modelId ? { "X-Model-Id": modelId } : {}),
    },
    body: JSON.stringify(body),
  });
  if (!res.ok) {
    const errBody = await res.json().catch(() => ({ error: res.statusText }));
    throw new Error(`${res.status}: ${errBody.error ?? res.statusText}`);
  }
  if (!res.body) {
    throw new Error("streaming is not supported in this browser");
  }

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    let sep: number;
    while ((sep = buffer.indexOf("\n\n")) !== -1) {
      const frame = buffer.slice(0, sep);
      buffer = buffer.slice(sep + 2);
      const evt = parseSSEFrame(frame);
      if (evt) onEvent(evt);
    }
  }
}

export const api = {
  getApps: () => apiFetch<AppInfo[]>("/api/apps"),

  getDemo: () => apiFetch<DemoContext>("/api/demo"),

  getMetrics: (revisionId?: string) =>
    apiFetch<Metric[]>(revisionId ? `/api/metrics?revision_id=${encodeURIComponent(revisionId)}` : "/api/metrics"),

  submitBudget: (body: { model_id: string; revision_id: string }) =>
    apiFetch<{ instance_id: string; status: string }>("/api/workflow/submit", {
      method: "POST",
      body: JSON.stringify(body),
    }),

  getCellHistory: (params: { model_id: string; revision_id: string; metric_id: string; dim_codes: Record<string, string> }) =>
    apiFetch<CellHistoryEntry[]>(`/api/cells/history?model_id=${params.model_id}&revision_id=${params.revision_id}&metric_id=${params.metric_id}&dim_codes=${encodeURIComponent(JSON.stringify(params.dim_codes))}`),
  writeback: (body: { model_id: string; revision_id: string; metric_id: string; dim_code?: string; dim_codes?: Record<string, string>; value: number }) =>
    apiFetch<{ status: string }>("/api/cells", {
      method: "POST",
      body: JSON.stringify(body),
    }),

  // opts.metaOnly returns dimensions/metrics/access with NO cells (fast — the
  // client learns its context selectors first). opts.scope is a {dimId:code}
  // map of the pinned context; the server returns only that slice's cells
  // (each pinned member expanded to its subtree), keeping a 500-member grid
  // sub-second instead of returning the whole cross-product.
  getGrid: (revisionId: string, gridDefId?: string, opts?: { metaOnly?: boolean; totalsOnly?: boolean; scope?: Record<string, string> }) => {
    const params = new URLSearchParams({ revision_id: revisionId });
    if (gridDefId) params.set("grid_def_id", gridDefId);
    if (opts?.metaOnly) params.set("meta_only", "1");
    if (opts?.totalsOnly) params.set("totals_only", "1");
    if (opts?.scope && Object.keys(opts.scope).length > 0) params.set("scope", JSON.stringify(opts.scope));
    return apiFetch<GridData>(`/api/grid?${params.toString()}`);
  },

  // Exports this grid's raw input-metric values (one row per dimension-member
  // combination, dimension/metric names as columns) — the same shape
  // uploadImport/uploadImportXlsx expect back, so the file round-trips.
  exportGrid: (gridDefId: string, revisionId: string, format: "csv" | "xlsx" = "xlsx") =>
    apiFetchBlob(`/api/grid/export?grid_def_id=${encodeURIComponent(gridDefId)}&revision_id=${encodeURIComponent(revisionId)}&format=${format}`),

  getTasks: () => apiFetch<Task[]>("/api/tasks"),
  // Published workflow definitions with their context schemas — the
  // Planning workspace renders its start-workflow dialog from these.
  listWorkflowDefinitions: () => apiFetch<WorkflowDefPublic[]>("/api/workflow/definitions"),

  completeTask: (stepId: string, decision: string, comment: string) =>
    apiFetch<{ status: string }>(`/api/tasks/${stepId}/complete`, {
      method: "POST",
      body: JSON.stringify({ decision, comment }),
    }),

  getDevApplications: () => apiFetch<AdminTenant[]>("/api/developer/applications"),
  setDefaultModel: (modelId: string) =>
    apiFetch<{ status: string }>(`/api/developer/models/${modelId}/set-default`, { method: "POST" }),

  getDevRevisions: (modelId?: string) =>
    apiFetch<DevRevision[]>(`/api/developer/revisions${modelId ? `?model_id=${modelId}` : ""}`),

  createDevRevision: (name: string, sourceRevisionId?: string) =>
    apiFetch<{ id: string }>("/api/developer/revisions", { method: "POST", body: JSON.stringify({ name, source_revision_id: sourceRevisionId }) }),

  deleteDevRevision: (id: string) =>
    apiFetch<{ status: string }>(`/api/developer/revisions/${id}`, { method: "DELETE" }),

  activateDevRevision: (id: string) =>
    apiFetch<{ status: string }>(`/api/developer/revisions/${id}/activate`, { method: "PUT" }),

  getDevModel: (revisionId?: string) =>
    apiFetch<DevModel>(`/api/developer/model${revisionId ? `?revision_id=${revisionId}` : ""}`),

  getDevDimensions: (revisionId?: string) =>
    apiFetch<DevDimension[]>(`/api/developer/dimensions${revisionId ? `?revision_id=${revisionId}` : ""}`),

  addMetric: (body: { name: string; is_input: boolean; formula: string; revision_id?: string; agg_rule?: string; agg_numerator_metric_id?: string; agg_denominator_metric_id?: string; format?: string; format_decimals?: number; format_currency?: string }) =>
    apiFetch<{ id: string; status: string }>("/api/developer/metrics", {
      method: "POST",
      body: JSON.stringify(body),
    }),

  getWorkflowHistory: () => apiFetch<HistoryInstance[]>("/api/workflow/history"),
  getMyWorkflowHistory: () => apiFetch<HistoryInstance[]>("/api/workflow/my-history"),
  updateWorkflowInstance: (id: string, status: string) =>
    apiFetch<{ status: string }>(`/api/workflow/instances/${id}`, {
      method: "PATCH",
      body: JSON.stringify({ status }),
    }),
  startWorkflow: (workflowDefId: string, context?: Record<string, string>) =>
    apiFetch<{ instance_id: string }>("/api/workflow/instances", {
      method: "POST",
      body: JSON.stringify({ workflow_def_id: workflowDefId, context: context ?? {} }),
    }),

  getNotifications: () => apiFetch<Notification[]>("/api/notifications"),

  markRead: (ids: string[]) =>
    apiFetch<{ updated: number }>("/api/notifications/mark-read", {
      method: "POST",
      body: JSON.stringify({ ids }),
    }),

  getAdminMe: () => apiFetch<{ user_id: string; email: string; display_name: string; roles: string[] }>("/api/admin/me"),
  getMe: () => apiFetch<Me>("/api/me"),
  // Public: no account yet. The sign-up page reads the terms, then registers.
  signupOptions: () => apiFetch<SignupOptions>("/api/signup/options"),
  signup: (body: SignupRequest) => apiFetch<SignupResult>("/api/signup", { method: "POST", body: JSON.stringify(body) }),
  // The plan catalog: read by administrators, written by the platform admin.
  getPlans: () => apiFetch<PlanDef[]>("/api/admin/plans"),
  updatePlan: (key: string, body: Omit<PlanDef, "key" | "updated_at">) =>
    apiFetch<PlanDef>(`/api/admin/plans/${encodeURIComponent(key)}`, { method: "PUT", body: JSON.stringify(body) }),
  getLicense: () => apiFetch<LicenseInfo>("/api/license"),
  getNotificationSettings: () => apiFetch<NotificationSettings>("/api/notifications/settings"),
  updateNotificationSettings: (body: NotificationSettings) =>
    apiFetch<NotificationSettings>("/api/notifications/settings", { method: "PUT", body: JSON.stringify(body) }),
  getSsoSettings: () => apiFetch<SsoSettings>("/api/admin/sso"),
  updateSsoSettings: (body: SsoSettings) => apiFetch<SsoSettings>("/api/admin/sso", { method: "PUT", body: JSON.stringify(body) }),
  removeSso: () => apiFetch<SsoSettings>("/api/admin/sso", { method: "DELETE" }),
  testSsoProvider: (body: { protocol: string; metadata_url: string }) =>
    apiFetch<{ ok: boolean; details?: Record<string, string>; error?: string }>("/api/admin/sso/test", { method: "POST", body: JSON.stringify(body) }),
  listScimTokens: () => apiFetch<ScimToken[]>("/api/admin/scim/tokens"),
  createScimToken: (body: { name: string; default_role?: string; workspace_id?: string }) =>
    apiFetch<ScimToken>("/api/admin/scim/tokens", { method: "POST", body: JSON.stringify(body) }),
  revokeScimToken: (id: string) => apiFetch<{ status: string }>(`/api/admin/scim/tokens/${id}`, { method: "DELETE" }),
  getTenantAISettings: () => apiFetch<TenantAISettings>("/api/admin/ai-settings"),
  updateTenantAISettings: (body: TenantAISettings) =>
    apiFetch<TenantAISettings>("/api/admin/ai-settings", { method: "PUT", body: JSON.stringify(body) }),
  testTenantAISettings: (body: Partial<TenantAISettings>) =>
    apiFetch<ProviderProbeResult>("/api/admin/ai-settings/test", { method: "POST", body: JSON.stringify(body) }),
  clearTenantAIKey: () => apiFetch<TenantAISettings>("/api/admin/ai-settings/key", { method: "DELETE" }),
  getAdminTenants: () => apiFetch<AdminTenant[]>("/api/admin/tenants"),
  getAdminUsers: () => apiFetch<AdminUser[]>("/api/admin/users"),
  getAdminAudit: () => apiFetch<AdminAuditEvent[]>("/api/admin/audit"),
  getBranding: () => apiFetch<BrandView>("/api/branding"),
  getAdminBranding: () => apiFetch<Branding>("/api/admin/branding"),
  updateBranding: (body: Branding) => apiFetch<Branding>("/api/admin/branding", { method: "PUT", body: JSON.stringify(body) }),
  removeBranding: () => apiFetch<Branding>("/api/admin/branding", { method: "DELETE" }),
  getUsage: (period: string) => apiFetch<UsageReport>(`/api/admin/usage?period=${encodeURIComponent(period)}`),
  getAuditSettings: () => apiFetch<AuditSettings>("/api/admin/audit/settings"),
  updateAuditSettings: (body: { retention_days: number }) =>
    apiFetch<AuditSettings>("/api/admin/audit/settings", { method: "PUT", body: JSON.stringify(body) }),
  /** Download the audit export; the server names the file. */
  exportAudit: (params: { format: "csv" | "jsonl"; since?: string; until?: string; category?: string }) => {
    const q = new URLSearchParams({ format: params.format });
    if (params.since) q.set("since", params.since);
    if (params.until) q.set("until", params.until);
    if (params.category) q.set("category", params.category);
    return apiFetchBlob(`/api/admin/audit/export?${q}`);
  },

  uploadImport: (csv: string, revisionId?: string, importMode?: ImportModeParam) =>
    apiFetch<ImportUploadResult>(
      "/api/import/upload",
      { method: "POST", body: JSON.stringify({ csv, revision_id: revisionId, import_mode: importMode ?? "incremental" }) },
    ),
  // Native .xlsx upload — the file is sent as-is (base64) and parsed
  // server-side (column headers map to metric/dimension names there), not
  // transcoded to CSV client-side.
  uploadImportXlsx: (xlsxBase64: string, revisionId?: string, importMode?: ImportModeParam) =>
    apiFetch<ImportUploadResult>(
      "/api/import/upload",
      { method: "POST", body: JSON.stringify({ xlsx_base64: xlsxBase64, revision_id: revisionId, import_mode: importMode ?? "incremental" }) },
    ),
  // Server-side fetch of a link-shared Google Sheet as CSV text — Google's
  // export endpoint sends no CORS headers, so the browser cannot fetch it
  // directly. The returned CSV then flows through the normal import path.
  fetchSheetPreview: (sheetUrl: string) =>
    apiFetch<{ csv: string; spreadsheet_id: string; gid: string }>("/api/import/sheets/fetch", {
      method: "POST",
      body: JSON.stringify({ sheet_url: sheetUrl }),
    }),
  getImportJobs: () => apiFetch<ImportJob[]>("/api/import/jobs"),
  deleteImportJob: (id: string) =>
    apiFetch<{ status: string }>(`/api/import/jobs/${id}`, { method: "DELETE" }),

  listFormMappings: (revisionId?: string) =>
    apiFetch<FormMetricMapping[]>(`/api/developer/form-integrations${revisionId ? `?revision_id=${encodeURIComponent(revisionId)}` : ""}`),
  createFormMapping: (body: Omit<FormMetricMapping, "id">, revisionId?: string) =>
    apiFetch<{ id: string }>(`/api/developer/form-integrations${revisionId ? `?revision_id=${encodeURIComponent(revisionId)}` : ""}`, { method: "POST", body: JSON.stringify(body) }),
  updateFormMapping: (id: string, body: Omit<FormMetricMapping, "id">) =>
    apiFetch<{ status: string }>(`/api/developer/form-integrations/${id}`, { method: "PATCH", body: JSON.stringify(body) }),
  deleteFormMapping: (id: string) =>
    apiFetch<{ status: string }>(`/api/developer/form-integrations/${id}`, { method: "DELETE" }),
  backfillFormMapping: (id: string) =>
    apiFetch<{ status: string; records_processed: number }>(`/api/developer/form-integrations/${id}/backfill`, { method: "POST" }),
  previewFormMapping: (id: string) =>
    apiFetch<FormRecordPosting[]>(`/api/developer/form-integrations/${id}/preview`),

  listDevIntegrations: (revisionId?: string) =>
    apiFetch<IntegrationDef[]>(`/api/developer/integrations${revisionId ? `?revision_id=${encodeURIComponent(revisionId)}` : ""}`),
  createIntegration: (body: { name: string; type: string; target_type: string; target_id: string; status?: string; tags?: string[] }, revisionId?: string) =>
    apiFetch<{ id: string }>(`/api/developer/integrations${revisionId ? `?revision_id=${encodeURIComponent(revisionId)}` : ""}`, { method: "POST", body: JSON.stringify(body) }),
  updateIntegration: (id: string, body: { name: string; target_type: string; target_id: string; status?: string; tags?: string[] }) =>
    apiFetch<{ status: string }>(`/api/developer/integrations/${id}`, { method: "PATCH", body: JSON.stringify(body) }),
  deleteIntegration: (id: string) =>
    apiFetch<{ status: string }>(`/api/developer/integrations/${id}`, { method: "DELETE" }),
  getInfraNodes: () => apiFetch<InfraNode[]>("/api/admin/infra/nodes"),
  listIntegrationRuns: (id: string) =>
    apiFetch<IntegrationRun[]>(`/api/developer/integrations/${id}/runs`),
  importDimensionMembers: (dimensionId: string, csv: string) =>
    apiFetch<{ rows_imported: number; error_rows: number }>("/api/import/dimension-members", {
      method: "POST",
      body: JSON.stringify({ dimension_id: dimensionId, csv }),
    }),
  updateIntegrationConfig: (id: string, config: Record<string, unknown>) =>
    apiFetch<{ status: string }>(`/api/developer/integrations/${id}/config`, { method: "PATCH", body: JSON.stringify({ config }) }),
  listIntegrations: (revisionId?: string) =>
    apiFetch<IntegrationDef[]>(`/api/integrations${revisionId ? `?revision_id=${encodeURIComponent(revisionId)}` : ""}`),
  // csv is required for csv_import integrations; google_sheets ones carry no
  // body — the server re-fetches the configured sheet at run time.
  runIntegration: (id: string, csv?: string) =>
    apiFetch<{ rows_imported: number; error_rows: number; errors?: ImportRowError[] }>(`/api/integrations/${id}/run`, {
      method: "POST",
      body: JSON.stringify(csv ? { csv } : {}),
    }),

  getDimensions: () => apiFetch<DevDimension[]>("/api/dimensions"),
  listForms: (revisionId?: string) =>
    apiFetch<FormDef[]>(`/api/forms${revisionId ? `?revision_id=${encodeURIComponent(revisionId)}` : ""}`),
  createForm: (body: { name: string; label: string; fields: FormField[] }, revisionId?: string) =>
    apiFetch<FormDef>(`/api/forms${revisionId ? `?revision_id=${encodeURIComponent(revisionId)}` : ""}`, { method: "POST", body: JSON.stringify(body) }),
  updateForm: (id: string, body: { name: string; label: string; fields: FormField[] }) =>
    apiFetch<{ status: string }>(`/api/forms/${id}`, { method: "PATCH", body: JSON.stringify(body) }),
  deleteForm: (id: string) =>
    apiFetch<{ status: string }>(`/api/forms/${id}`, { method: "DELETE" }),
  listRecords: (formId: string) => apiFetch<FormRecord[]>(`/api/forms/${formId}/records`),
  createRecord: (formId: string, data: Record<string, unknown>, revisionId?: string) =>
    apiFetch<FormRecord>(`/api/forms/${formId}/records`, { method: "POST", body: JSON.stringify({ data, revision_id: revisionId }) }),
  updateRecord: (recordId: string, status: string, data: Record<string, unknown>) =>
    apiFetch<{ status: string }>(`/api/records/${recordId}`, { method: "PUT", body: JSON.stringify({ data, status }) }),
  syncForm: (formId: string) =>
    apiFetch<{ status: string; mappings: number; records_processed: number }>(`/api/forms/${formId}/sync`, { method: "POST" }),
  deleteRecord: (recordId: string) =>
    apiFetch<{ status: string }>(`/api/records/${recordId}`, { method: "DELETE" }),

  // Exports this form's records — one row per record, field names as columns
  // (plus id/status/created_at) — the shape importFormRecords/
  // importFormRecordsXlsx expect back.
  exportFormRecords: (formId: string, format: "csv" | "xlsx" = "xlsx") =>
    apiFetchBlob(`/api/forms/${formId}/export?format=${format}`),
  importFormRecords: (formId: string, csv: string) =>
    apiFetch<{ status: string; records_created: number }>(`/api/forms/${formId}/import`, {
      method: "POST",
      body: JSON.stringify({ csv }),
    }),
  importFormRecordsXlsx: (formId: string, xlsxBase64: string) =>
    apiFetch<{ status: string; records_created: number }>(`/api/forms/${formId}/import`, {
      method: "POST",
      body: JSON.stringify({ xlsx_base64: xlsxBase64 }),
    }),

  deleteMetric: (id: string) =>
    apiFetch<{ status: string }>(`/api/developer/metrics/${id}`, { method: "DELETE" }),
  updateMetric: (id: string, body: { name: string; formula: string; agg_rule?: string; agg_numerator_metric_id?: string; agg_denominator_metric_id?: string; format?: string; format_decimals?: number; format_currency?: string }) =>
    apiFetch<{ status: string; recalc: Array<{ revision: string; metric: string; value: number | null }> }>(
      `/api/developer/metrics/${id}`, { method: "PATCH", body: JSON.stringify(body) }),

  createDimension: (body: { name: string; agg_rule?: string; revision_id?: string; parent_dimension_id?: string | null }) =>
    apiFetch<{ id: string; status: string }>("/api/developer/dimensions", { method: "POST", body: JSON.stringify(body) }),
  updateDimension: (id: string, body: { name: string; agg_rule?: string; parent_dimension_id?: string | null }) =>
    apiFetch<{ status: string }>(`/api/developer/dimensions/${id}`, { method: "PATCH", body: JSON.stringify(body) }),
  deleteDimension: (id: string) =>
    apiFetch<{ status: string }>(`/api/developer/dimensions/${id}`, { method: "DELETE" }),
  addDimMember: (dimId: string, body: { code: string; label: string; parent_member_id?: string }) =>
    apiFetch<{ id: string }>(`/api/developer/dimensions/${dimId}/members`, { method: "POST", body: JSON.stringify(body) }),
  updateDimMember: (dimId: string, memberId: string, body: { code: string; label: string; parent_member_id?: string | null; properties?: Record<string, string> }) =>
    apiFetch<{ status: string }>(`/api/developer/dimensions/${dimId}/members/${memberId}`, { method: "PATCH", body: JSON.stringify(body) }),
  deleteDimMember: (dimId: string, memberId: string) =>
    apiFetch<{ status: string }>(`/api/developer/dimensions/${dimId}/members/${memberId}`, { method: "DELETE" }),

  listDimProperties: (dimId: string) =>
    apiFetch<DimProperty[]>(`/api/developer/dimensions/${dimId}/properties`),
  addDimProperty: (dimId: string, body: { name: string; data_type: string }) =>
    apiFetch<{ id: string }>(`/api/developer/dimensions/${dimId}/properties`, { method: "POST", body: JSON.stringify(body) }),
  updateDimProperty: (dimId: string, propId: string, body: { name: string; data_type: string }) =>
    apiFetch<{ status: string }>(`/api/developer/dimensions/${dimId}/properties/${propId}`, { method: "PATCH", body: JSON.stringify(body) }),
  deleteDimProperty: (dimId: string, propId: string) =>
    apiFetch<{ status: string }>(`/api/developer/dimensions/${dimId}/properties/${propId}`, { method: "DELETE" }),

  formulaRefs: (formulaStr: string) =>
    apiFetch<{ refs: string[]; error?: string }>("/api/formula/refs", { method: "POST", body: JSON.stringify({ formula: formulaStr }) }),

  deleteAutomationRule: (ruleId: string) =>
    apiFetch<{ status: string }>(`/api/automation/rules/${ruleId}`, { method: "DELETE" }),
  updateAutomationRule: (ruleId: string, body: { name?: string; description?: string; trigger_type?: string; workflow_name?: string; workflow_def_id?: string; source_form_id?: string; source_grid_id?: string; source_integration_id?: string; enabled?: boolean }) =>
    apiFetch<AutomationRule>(`/api/automation/rules/${ruleId}`, { method: "PATCH", body: JSON.stringify(body) }),

  createAdminUser: (body: { email: string; first_name: string; last_name: string; role: string; workspace_id?: string }) =>
    apiFetch<{ id: string; status: string }>("/api/admin/users", { method: "POST", body: JSON.stringify(body) }),
  updateAdminUser: (id: string, body: { email: string; display_name: string }) =>
    apiFetch<{ status: string }>(`/api/admin/users/${id}`, { method: "PATCH", body: JSON.stringify(body) }),
  getAdminWorkspaces: () => apiFetch<AdminWorkspace[]>("/api/admin/workspaces"),
  addAdminUserRole: (id: string, role: string, workspaceId?: string) =>
    apiFetch<{ status: string }>(`/api/admin/users/${id}/roles`, { method: "POST", body: JSON.stringify({ role, workspace_id: workspaceId ?? "" }) }),
  removeAdminUserRole: (id: string, role: string, workspaceId?: string) =>
    apiFetch<{ status: string }>(`/api/admin/users/${id}/roles/${role}${workspaceId ? `?workspace_id=${workspaceId}` : ""}`, { method: "DELETE" }),
  deleteAdminUser: (id: string) =>
    apiFetch<{ status: string }>(`/api/admin/users/${id}`, { method: "DELETE" }),
  grantUserAppAccess: (id: string, appId: string) =>
    apiFetch<{ status: string }>(`/api/admin/users/${id}/access/apps/${appId}`, { method: "POST" }),
  revokeUserAppAccess: (id: string, appId: string) =>
    apiFetch<{ status: string }>(`/api/admin/users/${id}/access/apps/${appId}`, { method: "DELETE" }),
  grantUserModelAccess: (id: string, modelId: string) =>
    apiFetch<{ status: string }>(`/api/admin/users/${id}/access/models/${modelId}`, { method: "POST" }),
  revokeUserModelAccess: (id: string, modelId: string) =>
    apiFetch<{ status: string }>(`/api/admin/users/${id}/access/models/${modelId}`, { method: "DELETE" }),
  createAdminTenant: (body: { name: string; plan: string }) =>
    apiFetch<{ id: string }>("/api/admin/tenants", { method: "POST", body: JSON.stringify(body) }),
  // name renames; plan / trial_ends_at (RFC 3339, "" ends the trial) are the
  // platform admin's to change.
  updateAdminTenant: (id: string, body: { name?: string; plan?: string; trial_ends_at?: string }) =>
    apiFetch<{ status: string }>(`/api/admin/tenants/${id}`, { method: "PATCH", body: JSON.stringify(body) }),
  deleteAdminTenant: (id: string) =>
    apiFetch<{ status: string }>(`/api/admin/tenants/${id}`, { method: "DELETE" }),
  createAdminApplication: (body: { customer_id: string; name: string; mode: string }) =>
    apiFetch<{ id: string }>("/api/admin/applications", { method: "POST", body: JSON.stringify(body) }),
  deleteAdminApplication: (id: string) =>
    apiFetch<{ status: string }>(`/api/admin/applications/${id}`, { method: "DELETE" }),
  createAdminModel: (body: { application_id: string; name: string; storage_type: string }) =>
    apiFetch<{ id: string }>("/api/admin/models", { method: "POST", body: JSON.stringify(body) }),
  deleteAdminModel: (id: string) =>
    apiFetch<{ status: string }>(`/api/admin/models/${id}`, { method: "DELETE" }),
  setActiveRevision: (modelId: string, body: { revision_name: string }) =>
    apiFetch<{ status: string }>(`/api/admin/models/${modelId}/active-revision`, { method: "PUT", body: JSON.stringify(body) }),
  listDevPersonas: () => apiFetch<DevPersona[]>("/api/dev/personas"),

  // Model export/import — tenant_admin only (403 for every other role).
  // includeData=false exports definitions only (no entered values, no form records).
  exportModel: (modelId: string, revisionId?: string, includeData = true) =>
    apiFetch<ModelExportPackage>(`/api/admin/models/${modelId}/export${exportQuery(revisionId, includeData)}`),
  importModel: (body: { application_id: string; model_name?: string; revision_name?: string; package: ModelExportPackage }) =>
    apiFetch<{ model_id: string; revision_id: string }>("/api/admin/models/import", { method: "POST", body: JSON.stringify(body) }),
  // Standalone deployment package (tar.gz): the model's full entity graph
  // plus the complete database migrations, a provenance manifest, and an
  // infra-only docker-compose — everything exportModel's plain JSON doesn't
  // carry. Same tenant_admin-only gate.
  exportModelPackage: (modelId: string, revisionId?: string, includeData = true) =>
    apiFetchBlob(`/api/admin/models/${modelId}/export/package${exportQuery(revisionId, includeData)}`),

  createAdminRevision: (body: { model_id: string; name: string }) =>
    apiFetch<{ id: string }>("/api/admin/revisions", { method: "POST", body: JSON.stringify({ ...body, description: "" }) }),
  deleteAdminRevision: (id: string) =>
    apiFetch<{ status: string }>(`/api/admin/revisions/${id}`, { method: "DELETE" }),

  listAutomationRules: (revisionId?: string) => apiFetch<AutomationRule[]>(`/api/automation/rules${revisionId ? `?revision_id=${encodeURIComponent(revisionId)}` : ""}`),
  createAutomationRule: (body: { name: string; description: string; trigger_type: string; workflow_name: string; workflow_def_id?: string; source_form_id?: string; source_grid_id?: string; source_integration_id?: string }) =>
    apiFetch<AutomationRule>("/api/automation/rules", { method: "POST", body: JSON.stringify(body) }),
  triggerRule: (ruleId: string, payload?: Record<string, string>) =>
    apiFetch<Execution>(`/api/automation/trigger/${ruleId}`, { method: "POST", body: JSON.stringify({ payload: payload ?? {} }) }),
  listExecutions: () => apiFetch<Execution[]>("/api/automation/executions"),

  generateMigration: () =>
    apiFetch<{ model_id: string; version_number: number; files: MigrationFile[] }>(
      "/api/developer/migration/generate",
      { method: "POST", body: JSON.stringify({}) },
    ),
  applyMigration: (model_id: string, version_number: number) =>
    apiFetch<{ migration_id: string; version_number: number; status: string; files_applied: number }>(
      "/api/developer/migration/apply",
      { method: "POST", body: JSON.stringify({ model_id, version_number }) },
    ),

  listGrids: (revisionId?: string) =>
    apiFetch<GridDef[]>(`/api/developer/grids${revisionId ? `?revision_id=${revisionId}` : ""}`),
  createGrid: (body: { name: string; revision_id?: string }) =>
    apiFetch<{ id: string }>("/api/developer/grids", { method: "POST", body: JSON.stringify(body) }),
  deleteGrid: (id: string) =>
    apiFetch<{ status: string }>(`/api/developer/grids/${id}`, { method: "DELETE" }),
  addGridMetric: (gridId: string, metricId: string) =>
    apiFetch<{ status: string }>(`/api/developer/grids/${gridId}/metrics/${metricId}`, { method: "POST" }),
  removeGridMetric: (gridId: string, metricId: string) =>
    apiFetch<{ status: string }>(`/api/developer/grids/${gridId}/metrics/${metricId}`, { method: "DELETE" }),
  addGridDimension: (gridId: string, dimId: string) =>
    apiFetch<{ status: string }>(`/api/developer/grids/${gridId}/dimensions/${dimId}`, { method: "POST" }),
  removeGridDimension: (gridId: string, dimId: string) =>
    apiFetch<{ status: string }>(`/api/developer/grids/${gridId}/dimensions/${dimId}`, { method: "DELETE" }),
  setGridDimLevel: (gridId: string, dimId: string, display_level: number | null) =>
    apiFetch<{ status: string }>(`/api/developer/grids/${gridId}/dimensions/${dimId}`, {
      method: "PATCH", body: JSON.stringify({ display_level }),
    }),

  listDashboards: (revisionId?: string) =>
    apiFetch<DashboardDef[]>(`/api/developer/dashboards${revisionId ? `?revision_id=${revisionId}` : ""}`),
  createDashboard: (body: { name: string; tags?: string[]; revision_id?: string; folder_id?: string }) =>
    apiFetch<{ id: string }>("/api/developer/dashboards", { method: "POST", body: JSON.stringify(body) }),
  // folder_id: omit to leave the dashboard where it is, "" or null to move it
  // to the root, an id to file it under that folder.
  updateDashboard: (id: string, body: { name: string; tags?: string[]; folder_id?: string | null }) =>
    apiFetch<{ status: string }>(`/api/developer/dashboards/${id}`, { method: "PATCH", body: JSON.stringify(body) }),

  // ── Dashboard folders ────────────────────────────────────────────────────
  listFolders: (revisionId?: string) =>
    apiFetch<DashboardFolder[]>(`/api/developer/folders${revisionId ? `?revision_id=${revisionId}` : ""}`),
  createFolder: (body: { name: string; parent_id?: string }) =>
    apiFetch<{ id: string }>("/api/developer/folders", { method: "POST", body: JSON.stringify(body) }),
  updateFolder: (id: string, body: { name: string; parent_id?: string }) =>
    apiFetch<{ status: string }>(`/api/developer/folders/${id}`, { method: "PATCH", body: JSON.stringify(body) }),
  deleteFolder: (id: string) =>
    apiFetch<{ status: string }>(`/api/developer/folders/${id}`, { method: "DELETE" }),
  listUserFolders: () => apiFetch<DashboardFolder[]>("/api/folders"),
  deleteDashboard: (id: string) =>
    apiFetch<{ status: string }>(`/api/developer/dashboards/${id}`, { method: "DELETE" }),
  addDashboardWidget: (dashId: string, body: { widget_type: string; ref_id?: string; content?: string; sort_order?: number; col_start?: number; col_span?: number; pos_x?: number; pos_y?: number; size_w?: number; size_h?: number; widget_props?: WidgetProps | null }) =>
    apiFetch<{ id: string }>(`/api/developer/dashboards/${dashId}/widgets`, { method: "POST", body: JSON.stringify(body) }),
  updateDashboardWidget: (dashId: string, widgetId: string, body: { sort_order?: number; col_start?: number; col_span?: number; ref_id?: string; content?: string; title?: string | null; show_title?: boolean; widget_props?: WidgetProps | null; pos_x?: number; pos_y?: number; size_w?: number; size_h?: number }) =>
    apiFetch<{ status: string }>(`/api/developer/dashboards/${dashId}/widgets/${widgetId}`, { method: "PATCH", body: JSON.stringify(body) }),
  removeDashboardWidget: (dashId: string, widgetId: string) =>
    apiFetch<{ status: string }>(`/api/developer/dashboards/${dashId}/widgets/${widgetId}`, { method: "DELETE" }),

  listUserDashboards: () => apiFetch<DashboardDef[]>("/api/dashboards"),
  getUserDashboard: (id: string) => apiFetch<DashboardDef>(`/api/dashboards/${id}`),

  getChartData: (widgetId: string, context: Record<string, string>) =>
    apiFetch<ChartData>(`/api/dashboard-widgets/${widgetId}/chart-data`, {
      method: "POST",
      body: JSON.stringify({ context }),
    }),


  // ── REST API connector ──────────────────────────────────────────────────────
  listIntegrationConnections: () =>
    apiFetch<IntegrationConnection[]>("/api/developer/integration-connections"),
  createIntegrationConnection: (body: { name: string; auth_type: ApiAuthType; meta?: Record<string, unknown>; secret?: Record<string, string> }) =>
    apiFetch<IntegrationConnection>("/api/developer/integration-connections", { method: "POST", body: JSON.stringify(body) }),
  // Secret semantics: omit `secret` = keep, null = remove, object = replace.
  updateIntegrationConnection: (id: string, body: { name?: string; auth_type?: ApiAuthType; meta?: Record<string, unknown>; secret?: Record<string, string> | null }) =>
    apiFetch<IntegrationConnection>(`/api/developer/integration-connections/${id}`, { method: "PATCH", body: JSON.stringify(body) }),
  deleteIntegrationConnection: (id: string) =>
    apiFetch<{ status: string }>(`/api/developer/integration-connections/${id}`, { method: "DELETE" }),
  testIntegrationConnection: (id: string) =>
    apiFetch<{ ok: boolean; error?: string }>(`/api/developer/integration-connections/${id}/test`, { method: "POST" }),

  createApiIntegration: (body: { name: string; description?: string; tags?: string[]; status?: string; connection_id?: string; config: ApiIntegrationConfig; schedule?: ApiSchedule }) =>
    apiFetch<ApiIntegrationDetail>("/api/developer/integrations", { method: "POST", body: JSON.stringify({ ...body, type: "rest_api" }) }),
  getApiIntegration: (id: string) =>
    apiFetch<ApiIntegrationDetail>(`/api/developer/integrations/${id}`),
  updateApiIntegration: (id: string, body: { name?: string; description?: string; tags?: string[]; status?: string; enabled?: boolean; connection_id?: string; config?: ApiIntegrationConfig; schedule?: ApiSchedule }) =>
    apiFetch<ApiIntegrationDetail>(`/api/developer/integrations/${id}`, { method: "PATCH", body: JSON.stringify(body) }),
  duplicateApiIntegration: (id: string) =>
    apiFetch<ApiIntegrationDetail>(`/api/developer/integrations/${id}/duplicate`, { method: "POST" }),
  validateApiIntegration: (id: string) =>
    apiFetch<{ valid: boolean; config_hash?: string; errors: string[] }>(`/api/developer/integrations/${id}/validate`, { method: "POST" }),
  testApiIntegration: (id: string, opts?: { dryRun?: boolean; acknowledgeSideEffects?: boolean }) => {
    const p = new URLSearchParams();
    if (opts?.dryRun) p.set("dry_run", "1");
    if (opts?.acknowledgeSideEffects) p.set("acknowledge_side_effects", "1");
    const qs = p.toString();
    return apiFetch<{ run_id: string; status: string }>(`/api/developer/integrations/${id}/test${qs ? "?" + qs : ""}`, { method: "POST" });
  },
  listApiIntegrationRuns: (id: string) =>
    apiFetch<ApiRun[]>(`/api/developer/integrations/${id}/runs`),
  getApiIntegrationRun: (runId: string) =>
    apiFetch<ApiRun>(`/api/developer/integration-runs/${runId}`),
  cancelApiIntegrationRun: (runId: string) =>
    apiFetch<{ status: string }>(`/api/developer/integration-runs/${runId}/cancel`, { method: "POST" }),

  // ── Business-Admin: roles ────────────────────────────────────────────────────
  listBARoles: () => apiFetch<BARole[]>("/api/business-admin/roles"),
  createBARole: (name: string) =>
    apiFetch<{ id: string }>("/api/business-admin/roles", { method: "POST", body: JSON.stringify({ name }) }),
  updateBARole: (id: string, name: string) =>
    apiFetch<{ status: string }>(`/api/business-admin/roles/${id}`, { method: "PATCH", body: JSON.stringify({ name }) }),
  deleteBARole: (id: string) =>
    apiFetch<{ status: string }>(`/api/business-admin/roles/${id}`, { method: "DELETE" }),
  setRoleDashboards: (roleId: string, dashboard_ids: string[]) =>
    apiFetch<{ status: string }>(`/api/business-admin/roles/${roleId}/dashboards`, { method: "PUT", body: JSON.stringify({ dashboard_ids }) }),
  listRoleMembers: (roleId: string) =>
    apiFetch<BARoleMember[]>(`/api/business-admin/roles/${roleId}/members`),
  addRoleMember: (roleId: string, user_id: string) =>
    apiFetch<{ status: string }>(`/api/business-admin/roles/${roleId}/members`, { method: "POST", body: JSON.stringify({ user_id }) }),
  removeRoleMember: (roleId: string, userId: string) =>
    apiFetch<{ status: string }>(`/api/business-admin/roles/${roleId}/members/${userId}`, { method: "DELETE" }),

  // ── Business-Admin: users + access rules ────────────────────────────────────
  listBAUsers: () => apiFetch<BAUser[]>("/api/business-admin/users"),
  getUserAccessRules: (userId: string) =>
    apiFetch<UserAccessRule[]>(`/api/business-admin/users/${userId}/access-rules`),
  putUserAccessRules: (userId: string, rules: UserAccessRule[]) =>
    apiFetch<{ status: string }>(`/api/business-admin/users/${userId}/access-rules`, { method: "PUT", body: JSON.stringify({ rules }) }),

  // ── Business-Admin: available resources ─────────────────────────────────────
  getBAAvailable: (type: "dashboards" | "metrics" | "dimensions" | "dimension_members") =>
    apiFetch<BAAvailableItem[]>(`/api/business-admin/available?type=${type}`),

  // ── Developer: Workflow Definitions ─────────────────────────────────────────
  listWorkflowDefs: (applicationId: string, revisionId?: string) =>
    apiFetch<WorkflowDefSummary[]>(`/api/developer/workflows?application_id=${encodeURIComponent(applicationId)}${revisionId ? `&revision_id=${encodeURIComponent(revisionId)}` : ""}`),
  getWorkflowDef: (id: string) =>
    apiFetch<WorkflowDef>(`/api/developer/workflows/${id}`),
  createWorkflowDef: (applicationId: string, body: { name: string; description?: string; trigger_event?: string }, revisionId?: string) =>
    apiFetch<WorkflowDef>(`/api/developer/workflows?application_id=${encodeURIComponent(applicationId)}${revisionId ? `&revision_id=${encodeURIComponent(revisionId)}` : ""}`, {
      method: "POST", body: JSON.stringify(body),
    }),
  updateWorkflowDef: (id: string, body: { name?: string; description?: string; trigger_event?: string; subject_type?: WorkflowSubjectType; subject_config?: Record<string, string>; steps?: WorkflowStepDef[]; context_schema?: ContextVariable[]; single_active_instance?: boolean }) =>
    apiFetch<WorkflowDef>(`/api/developer/workflows/${id}`, {
      method: "PATCH", body: JSON.stringify(body),
    }),
  deleteWorkflowDef: (id: string) =>
    apiFetch<{ status: string }>(`/api/developer/workflows/${id}`, { method: "DELETE" }),
  duplicateWorkflowDef: (id: string, name: string) =>
    apiFetch<WorkflowDef>(`/api/developer/workflows/${id}/duplicate`, {
      method: "POST", body: JSON.stringify({ name }),
    }),
  // draft: validate the editor's UNSAVED definition instead of the stored one (nothing is persisted).
  validateWorkflowDef: (id: string, draft?: { name?: string; steps?: WorkflowStepDef[]; context_schema?: ContextVariable[] }) =>
    apiFetch<WorkflowValidationResult>(`/api/developer/workflows/${id}/validate`, { method: "POST", ...(draft ? { body: JSON.stringify(draft) } : {}) }),
  publishWorkflowDef: (id: string) =>
    apiFetch<WorkflowDef>(`/api/developer/workflows/${id}/publish`, { method: "POST" }),
  archiveWorkflowDef: (id: string) =>
    apiFetch<WorkflowDef>(`/api/developer/workflows/${id}/archive`, { method: "POST" }),
  restoreWorkflowDef: (id: string) =>
    apiFetch<WorkflowDef>(`/api/developer/workflows/${id}/restore`, { method: "POST" }),
  getWorkflowDefUsage: (id: string) =>
    apiFetch<WorkflowDefUsage[]>(`/api/developer/workflows/${id}/usage`),
  getWorkflowInstances: (id: string) =>
    apiFetch<Execution[]>(`/api/developer/workflows/${id}/instances`),
  // test_run is always true here; current_step_* name the step the run
  // stopped on, so the designer can see where it got to.
  testRunWorkflowDef: (id: string, context?: Record<string, string>) =>
    apiFetch<{ instance_id: string; status: string; test_run: boolean; current_step_id?: string; current_step_status?: string }>(`/api/developer/workflows/${id}/test-run`, {
      method: "POST", body: JSON.stringify({ context: context ?? {} }),
    }),
  listWorkflowTriggerEvents: (applicationId?: string) => {
    const qs = applicationId ? `?application_id=${encodeURIComponent(applicationId)}` : "";
    return apiFetch<TriggerEventCatalogItem[]>(`/api/developer/workflow-trigger-events${qs}`);
  },

  listWorkflowRoles: (applicationId: string) =>
    apiFetch<{ id: string; name: string }[]>(
      `/api/developer/workflow-roles?application_id=${encodeURIComponent(applicationId)}`
    ),

  // ── AI Assistant ─────────────────────────────────────────────────────────────
  aiListSessions: () => apiFetch<AISession[]>("/api/ai/sessions"),
  aiCreateSession: () => apiFetch<AISession>("/api/ai/sessions", { method: "POST" }),
  aiDeleteSession: (id: string) =>
    apiFetch<{ status: string }>(`/api/ai/sessions/${id}`, { method: "DELETE" }),
  aiRenameSession: (id: string, title: string) =>
    apiFetch<{ id: string; title: string }>(`/api/ai/sessions/${id}`, {
      method: "PATCH",
      body: JSON.stringify({ title }),
    }),
  aiGetSession: (id: string) =>
    apiFetch<{ session: AISession; messages: AIMessage[]; documents: AIDocument[] }>(`/api/ai/sessions/${id}`),
  aiUploadDocument: async (sessionId: string, file: File): Promise<AIDocument> => {
    // multipart — bypass apiFetch so the browser sets the Content-Type boundary
    const appId = localStorage.getItem("selected_app_id") ?? "";
    const modelId = localStorage.getItem("selected_model_id") ?? "";
    const form = new FormData();
    form.append("file", file);
    const res = await fetch(`/api/ai/sessions/${sessionId}/documents`, {
      method: "POST",
      headers: {
        "X-Dev-User": persona(),
      ...(actingTenantId ? { "X-Tenant-Id": actingTenantId } : {}),
        ...authHeader(),
        ...(appId ? { "X-App-Id": appId } : {}),
      ...(modelId ? { "X-Model-Id": modelId } : {}),
      },
      body: form,
    });
    if (!res.ok) {
      const body = await res.json().catch(() => ({ error: res.statusText }));
      throw new Error(`${res.status}: ${body.error ?? res.statusText}`);
    }
    return res.json() as Promise<AIDocument>;
  },
  aiDeleteDocument: (sessionId: string, documentId: string) =>
    apiFetch<{ status: string }>(`/api/ai/sessions/${sessionId}/documents/${documentId}`, { method: "DELETE" }),
  aiTestSettings: (body: { provider?: string; model?: string; api_key?: string }) =>
    apiFetch<AITestResult>("/api/ai/settings/test", { method: "POST", body: JSON.stringify(body) }),
  // Streams the assistant's reply: onEvent fires for each SSE frame ("delta"
  // while text is generated, "tool_status" while a read tool runs, then a
  // terminal "proposal", "done", or "error"). Resolves once the stream ends.
  // revisionId pins WHICH model+revision the assistant works in (multi-model
  // apps resolve to the newest model otherwise) — pass the console's working
  // revision so the session touches what the console shows.
  aiSendMessage: (sessionId: string, content: string, onEvent: (event: AISendMessageEvent) => void, revisionId?: string) =>
    streamSSE(`/api/ai/sessions/${sessionId}/messages${revisionId ? `?revision_id=${encodeURIComponent(revisionId)}` : ""}`, { content }, onEvent),
  aiConfirmProposal: (sessionId: string, proposalId: string, revisionId?: string) =>
    apiFetch<{ proposal: AIProposal; messages: AIMessage[]; session?: AISession }>(
      `/api/ai/sessions/${sessionId}/proposals/${proposalId}/confirm${revisionId ? `?revision_id=${encodeURIComponent(revisionId)}` : ""}`,
      { method: "POST" },
    ),
  aiRejectProposal: (sessionId: string, proposalId: string) =>
    apiFetch<{ proposal: AIProposal; messages: AIMessage[] }>(
      `/api/ai/sessions/${sessionId}/proposals/${proposalId}/reject`,
      { method: "POST" },
    ),
  aiPromoteDraft: (sessionId: string) =>
    apiFetch<{ session: AISession; messages: AIMessage[] }>(
      `/api/ai/sessions/${sessionId}/promote-draft`,
      { method: "POST" },
    ),
  // Replaces the old deleteDevRevision(draftId) + local-state-only approach:
  // this clears session.draft_revision_id server-side too, so a reload (or
  // the session's next confirmed proposal) doesn't see a dangling draft id.
  aiDiscardDraft: (sessionId: string) =>
    apiFetch<{ session: AISession; messages: AIMessage[] }>(
      `/api/ai/sessions/${sessionId}/discard-draft`,
      { method: "POST" },
    ),
  aiListProposals: (sessionId: string) =>
    apiFetch<AIProposalWithSummary[]>(`/api/ai/sessions/${sessionId}/proposals`),
  aiGetSettings: () => apiFetch<AISettings>("/api/ai/settings"),
  aiSaveSettings: (body: { provider: string; model: string; api_key?: string }) =>
    apiFetch<{ status: string }>("/api/ai/settings", { method: "PUT", body: JSON.stringify(body) }),

};

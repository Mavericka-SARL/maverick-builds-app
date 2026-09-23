package gateway

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rs/zerolog"

	workflowv1 "github.com/mavericks-engine/mavericks/gen/go/workflow/v1"
	"github.com/mavericks-engine/mavericks/internal/aiassistant/providers"
	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/identity"
	"github.com/mavericks-engine/mavericks/internal/imagedata"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
	"github.com/mavericks-engine/mavericks/internal/metricformula"
	"github.com/mavericks-engine/mavericks/internal/modeltransfer"
	"github.com/mavericks-engine/mavericks/internal/notification"
	"github.com/mavericks-engine/mavericks/internal/plan"
	"github.com/mavericks-engine/mavericks/internal/query"
	"github.com/mavericks-engine/mavericks/internal/rollup"
	"github.com/mavericks-engine/mavericks/internal/schemamigration"
	"github.com/mavericks-engine/mavericks/internal/timedim"
	"github.com/mavericks-engine/mavericks/internal/workflow"
	"github.com/mavericks-engine/mavericks/internal/writeguard"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
	"github.com/mavericks-engine/mavericks/pkg/keycloak"
	"github.com/mavericks-engine/mavericks/pkg/license"
	"github.com/mavericks-engine/mavericks/pkg/objectstore"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

type handler struct {
	log zerolog.Logger
	// db routes every query: a request whose tenant was resolved (see
	// tenant.go) runs against that tenant's own database, everything else
	// against the control plane. Handlers use it exactly like a pool;
	// db.For(ctx) hands the chosen pool to packages that want one.
	db      *tenantdb.Handle
	devMode bool
	jwks    *identity.JWKSValidator // nil in dev mode
	// testProvider, when set, is returned by buildProviderForRequest instead
	// of resolving DB-stored settings and constructing a real network
	// client — the only seam for exercising aiSendMessage's real SSE
	// tool-call loop end to end without hitting a live LLM API. Never set
	// outside tests (NewHandler never sets it).
	testProvider providers.Provider
	// sheets, when set, replaces the real docs.google.com fetcher used by
	// the Google Sheets import endpoints — the seam for pointing sheet
	// fetches at an httptest server. Never set outside tests (NewHandler
	// never sets it); nil means the zero-value fetcher, which targets the
	// real docs.google.com.
	sheets *importpkg.SheetFetcher
	// sheetsTokenURL / sheetsAPIBase point a tenant's stored Google service
	// account at a test's fake Google; empty in production.
	sheetsTokenURL, sheetsAPIBase string
	// store backs the standalone deployment package export route
	// (adminModelExportPackage) — nil unless constructed via
	// NewHandlerWithObjectStore. A nil store means that one route 500s;
	// every other route is unaffected.
	store *objectstore.Store
	// kc provisions real Keycloak accounts for console-created users. nil
	// means "no identity provider wired", which is the normal state for the
	// dev stack: there, X-Dev-User personas stand in for real accounts and
	// user creation keeps writing the synthetic "admin-created-<email>"
	// subject that resolveDevActor understands.
	//
	// Outside dev mode a nil kc makes user creation fail loudly rather than
	// writing that synthetic subject, because under JWKS validation the token
	// subject is a UUID that can never match it — which is exactly how the
	// console came to create accounts that silently could not log in.
	kc *keycloak.Client
	// lic answers edition and feature questions for gated routes (see
	// license.go). nil behaves as the community edition, which is what a
	// handler built without Deps.License gets.
	lic *license.Manager
	// kcPublicURL/kcRealm name the identity-provider registry as the
	// world sees it, for the endpoints a tenant registers at their own
	// provider. publicURL is this deployment's own origin, for the SCIM
	// base URL shown to the tenant admin.
	kcPublicURL, kcRealm, publicURL string
	// discover throttles the unauthenticated sign-in discovery endpoint.
	discover discoverLimiter
	// lastSeen throttles the per-account last_seen_at refresh.
	lastSeenMu sync.Mutex
	lastSeen   map[string]time.Time
	// mailer is the deployment's SMTP relay, nil when none is configured —
	// the console warns before e-mail notifications are switched on, and an
	// administrator can send themselves a test message through it.
	mailer notification.Mailer
	// plans answers plan and trial questions per tenant (internal/plan,
	// see plan.go); signup is the public sign-up configuration and
	// signupLimit throttles it per address.
	plans       *plan.Enforcer
	signupCfg   SignupConfig
	signupLimit discoverLimiter
	// legalCfg is who operates this deployment and which documents it
	// publishes (legal.go).
	legalCfg LegalConfig
	// owners caches which tenant an application, model or workspace
	// belongs to (plan.go).
	ownerMu sync.Mutex
	owners  map[string]ownerEntry
}

// Deps are the optional collaborators a handler can be built with. Every field
// may be zero; each route that needs one says what happens when it is missing.
type Deps struct {
	ObjectStore *objectstore.Store
	Keycloak    *keycloak.Client
	// Router gives each tenant its own database. nil keeps every tenant in
	// the database DATABASE_URL points at, which is what the dev stack and
	// every existing deployment do until they are migrated.
	Router *tenantdb.Router
	// License is the key in force; nil means community edition.
	License *license.Manager
	// Mailer is the SMTP relay for outbound e-mail; nil when the deployment
	// has none.
	Mailer notification.Mailer
	// KeycloakPublicURL and KeycloakRealm name the identity-provider
	// registry as browsers reach it; PublicURL is this deployment's origin.
	KeycloakPublicURL string
	KeycloakRealm     string
	PublicURL         string
	// Signup opens public self-service sign-up (signup.go).
	Signup SignupConfig
	// Legal identifies the operator and its published documents (legal.go).
	// Zero means this deployment publishes none, and the sign-up page then
	// claims agreement to nothing.
	Legal LegalConfig
	// Plans is the enforcer shared with the usage sweep, so a sweep's
	// verdict reaches requests at once; nil builds a private one.
	Plans *plan.Enforcer
}

// inviteLifetime is how long a set-your-password link stays valid. Keycloak's
// default is 12 hours, which routinely expires before someone invited on a
// Friday afternoon reads their mail; three days spans a weekend without
// leaving the link usable indefinitely.
const inviteLifetime = 72 * time.Hour

var devPersonas = map[string]string{
	// OPEX Planning personas
	"dept_head":      "demo-dept-head-001",
	"finance":        "demo-finance-001",
	"developer":      "demo-dev-001",
	"tenant_admin":   "demo-admin-001",
	"platform_admin": "demo-platform-admin-001",
	// Budget Planning personas
	"cfo":         "budget-cfo-001",
	"finance_mgr": "budget-finmgr-001",
	"dept_eng":    "budget-depteng-001",
	"dept_sales":  "budget-deptsales-001",
	"dept_ops":    "budget-deptops-001",
	// Sales Tracker personas (TechFlow Inc)
	"sales_rep": "sales-rep-001",
	"sales_mgr": "sales-mgr-001",
	// Payroll (Salary Budgeting) personas — platform_admin/developer reuse
	// the OPEX seed's shared accounts (see cmd/seed-payroll/main.go)
	"payroll_admin": "payroll-tenant-admin-001",
	"general_mgr":   "payroll-gm-001",
	"cc_mgr_sales":  "payroll-ccm-sales-001",
	"cc_mgr_ops":    "payroll-ccm-ops-001",
	"cc_mgr_ga":     "payroll-ccm-ga-001",
}

func NewHandler(log zerolog.Logger, pool *pgxpool.Pool, jwks *identity.JWKSValidator) http.Handler {
	return NewHandlerWithObjectStore(log, pool, jwks, nil)
}

// NewHandlerWithObjectStore is NewHandler plus an object store for the
// standalone deployment package export route. store may be nil (e.g. if
// object store configuration/bootstrap failed) — every route except
// adminModelExportPackage is unaffected either way.
func NewHandlerWithObjectStore(log zerolog.Logger, pool *pgxpool.Pool, jwks *identity.JWKSValidator, store *objectstore.Store) http.Handler {
	return NewHandlerWithDeps(log, pool, jwks, Deps{ObjectStore: store})
}

// NewHandlerWithDeps is the full constructor; the two above delegate to it.
func NewHandlerWithDeps(log zerolog.Logger, pool *pgxpool.Pool, jwks *identity.JWKSValidator, deps Deps) http.Handler {
	h := &handler{
		log:         log,
		db:          tenantdb.NewHandle(pool, deps.Router),
		devMode:     os.Getenv("DEV_MODE") == "true",
		jwks:        jwks,
		store:       deps.ObjectStore,
		kc:          deps.Keycloak,
		lic:         deps.License,
		kcPublicURL: deps.KeycloakPublicURL, kcRealm: deps.KeycloakRealm, publicURL: deps.PublicURL,

		mailer:      deps.Mailer,
		signupCfg:   deps.Signup,
		legalCfg:    deps.Legal,
		signupLimit: discoverLimiter{rate: signupRate, burst: signupBurst},
	}
	h.plans = deps.Plans
	if h.plans == nil {
		h.plans = plan.NewEnforcer(h.db.Control())
	}
	mux := http.NewServeMux()
	h.registerRoutes(mux, nil)
	// appIDMiddleware first: tenant routing reads the application id it puts
	// in the context; the plan guard needs the routed tenant.
	return appIDMiddleware(h.tenantRouting(h.planGuard(mux)))
}

// RouteInfo describes one endpoint registerRoutes wires up: its HTTP method
// (empty if the underlying handler still dispatches multiple methods/
// sub-paths internally instead of exposing them as distinct patterns —
// see IMPLEMENTATION_PLAN.md's HTTP contract documentation item), its mux
// pattern, and the auth level required to reach it.
type RouteInfo struct {
	Method  string
	Pattern string
	Auth    string
}

// BuildRoutes returns the route table registerRoutes would wire into a real
// mux, without needing a database connection — route registration itself
// never touches h.db.For(ctx). Used by route_spec_parity_test.go to check the
// manual router against api/openapi.yaml.
func BuildRoutes() []RouteInfo {
	h := &handler{}
	var routes []RouteInfo
	h.registerRoutes(http.NewServeMux(), &routes)
	return routes
}

func (h *handler) registerRoutes(mux *http.ServeMux, routes *[]RouteInfo) {
	// register performs the real mux registration and, when routes is
	// non-nil (BuildRoutes' introspection path), records it for tooling. An
	// empty method registers a bare pattern matching any method, identically
	// to the historical mux.HandleFunc(pattern, fn) call it replaces.
	register := func(method, pattern, auth string, fn http.HandlerFunc) {
		if routes != nil {
			*routes = append(*routes, RouteInfo{Method: method, Pattern: pattern, Auth: auth})
		}
		if method == "" {
			mux.HandleFunc(pattern, fn)
		} else {
			mux.HandleFunc(method+" "+pattern, fn)
		}
	}

	// Helpers used below
	dev := func(fn http.HandlerFunc) http.HandlerFunc { return cors(h.guard(fn, "developer")) }
	adm := func(fn http.HandlerFunc) http.HandlerFunc { return cors(h.guard(fn, "platform_admin", "tenant_admin")) }
	ba := func(fn http.HandlerFunc) http.HandlerFunc { return cors(h.guard(fn, "business_admin")) }
	// baOrDev: named business roles (identity.business_role) are referenced
	// by workflow step assignee_roles, which a developer configures in the
	// Workflow editor — so a developer must also be able to create/manage
	// the roles their own workflow steps need, not just business_admin.
	// Scoped to the roles routes only (not the rest of the business-admin
	// surface — user management, workflow instance admin actions, etc. stay
	// business_admin-only) so this doesn't widen access beyond what's needed.
	baOrDev := func(fn http.HandlerFunc) http.HandlerFunc { return cors(h.guard(fn, "business_admin", "developer")) }
	devOrAdm := func(fn http.HandlerFunc) http.HandlerFunc {
		return cors(h.guard(fn, "developer", "platform_admin", "tenant_admin"))
	}
	userAdm := func(fn http.HandlerFunc) http.HandlerFunc {
		return cors(h.guard(fn, "developer", "platform_admin", "tenant_admin"))
	}

	// ── Core/misc + grid/cells/tasks/metrics ──────────────────────────────
	// Precise method+pattern registrations (HTTP contract documentation
	// Step 1); every other block below still registers bare patterns
	// pending the same treatment in a later batch.
	register("GET", "/healthz", "public", h.healthz)
	register("GET", "/api/dev/personas", "any", cors(h.devPersonaList))
	register("GET", "/api/apps", "any", cors(h.userApps))
	register("GET", "/api/demo", "any", cors(h.demo))
	register("GET", "/api/me", "any", cors(h.me))
	register("GET", "/api/license", "any", cors(h.licenseInfo))
	// Self-service sign-up (public; signup.go) and the plan catalog
	// (plan.go). Both handlers guard themselves.
	register("GET", "/api/signup/options", "public", cors(h.signupOptions))
	// The terms and the privacy notice are read before an account exists
	// (legal.go), so this is public too.
	register("GET", "/api/legal", "public", cors(h.legalInfo))
	register("POST", "/api/signup", "public", cors(h.signup))
	register("GET", "/api/admin/plans", "admin", cors(h.adminPlans))
	register("PUT", "/api/admin/plans/{key}", "admin", cors(h.adminPlanAction))
	register("POST", "/api/formula/refs", "any", cors(h.formulaRefs))
	register("GET", "/api/metrics", "any", cors(h.metrics))
	register("POST", "/api/cells", "any", cors(h.cells))
	// Enterprise: every value a cell has held (ee/cellhistory).
	register("GET", "/api/cells/history", "any", cors(h.requireFeature(license.FeatureCellHistory, h.cellHistory)))
	register("GET", "/api/tasks", "any", cors(h.tasks))
	register("POST", "/api/tasks/{stepId}/complete", "any", cors(h.taskAction))
	register("GET", "/api/grid", "any", cors(h.grid))
	register("GET", "/api/grid/export", "any", cors(h.gridExport))

	// Developer-only endpoints
	register("GET", "/api/developer/applications", "developer_or_admin", devOrAdm(h.adminTenants))
	register("POST", "/api/developer/models/{id}/set-default", "developer", dev(h.developerSetDefaultModel))
	register("POST", "/api/developer/applications", "developer_or_admin", devOrAdm(h.adminTenants))
	register("GET", "/api/developer/revisions", "developer", dev(h.developerRevisions))
	register("POST", "/api/developer/revisions", "developer", dev(h.developerRevisions))
	register("PUT", "/api/developer/revisions/{id}/activate", "developer", dev(h.developerRevisionAction))
	register("DELETE", "/api/developer/revisions/{id}", "developer", dev(h.developerRevisionAction))
	register("GET", "/api/developer/model", "developer", dev(h.developerModel))
	register("POST", "/api/developer/metrics", "developer", dev(h.developerMetrics))
	register("PATCH", "/api/developer/metrics/{id}", "developer", dev(h.developerMetricAction))
	register("DELETE", "/api/developer/metrics/{id}", "developer", dev(h.developerMetricAction))
	register("GET", "/api/developer/dimensions", "developer", dev(h.developerDimensions))
	register("POST", "/api/developer/dimensions", "developer", dev(h.developerDimensions))
	register("PATCH", "/api/developer/dimensions/{dimId}", "developer", dev(h.developerDimensionAction))
	register("DELETE", "/api/developer/dimensions/{dimId}", "developer", dev(h.developerDimensionAction))
	register("POST", "/api/developer/dimensions/{dimId}/members", "developer", dev(h.developerDimensionAction))
	register("POST", "/api/developer/dimensions/{dimId}/members/generate", "developer", dev(h.developerDimensionAction))
	register("PATCH", "/api/developer/dimensions/{dimId}/members/{memberId}", "developer", dev(h.developerDimensionAction))
	register("DELETE", "/api/developer/dimensions/{dimId}/members/{memberId}", "developer", dev(h.developerDimensionAction))
	register("GET", "/api/developer/dimensions/{dimId}/properties", "developer", dev(h.developerDimensionAction))
	register("POST", "/api/developer/dimensions/{dimId}/properties", "developer", dev(h.developerDimensionAction))
	register("PATCH", "/api/developer/dimensions/{dimId}/properties/{propId}", "developer", dev(h.developerDimensionAction))
	register("DELETE", "/api/developer/dimensions/{dimId}/properties/{propId}", "developer", dev(h.developerDimensionAction))
	register("GET", "/api/developer/grids", "developer", dev(h.developerGrids))
	register("POST", "/api/developer/grids", "developer", dev(h.developerGrids))
	register("PATCH", "/api/developer/grids/{id}", "developer", dev(h.developerGridAction))
	register("DELETE", "/api/developer/grids/{id}", "developer", dev(h.developerGridAction))
	register("POST", "/api/developer/grids/{id}/metrics/{metricId}", "developer", dev(h.developerGridAction))
	register("DELETE", "/api/developer/grids/{id}/metrics/{metricId}", "developer", dev(h.developerGridAction))
	register("POST", "/api/developer/grids/{id}/dimensions/{dimId}", "developer", dev(h.developerGridAction))
	register("PATCH", "/api/developer/grids/{id}/dimensions/{dimId}", "developer", dev(h.developerGridAction))
	register("DELETE", "/api/developer/grids/{id}/dimensions/{dimId}", "developer", dev(h.developerGridAction))
	register("GET", "/api/developer/folders", "developer", dev(h.developerFolders))
	register("POST", "/api/developer/folders", "developer", dev(h.developerFolders))
	register("PATCH", "/api/developer/folders/{id}", "developer", dev(h.developerFolderAction))
	register("DELETE", "/api/developer/folders/{id}", "developer", dev(h.developerFolderAction))
	register("GET", "/api/developer/dashboards", "developer", dev(h.developerDashboards))
	register("POST", "/api/developer/dashboards", "developer", dev(h.developerDashboards))
	register("PATCH", "/api/developer/dashboards/{id}", "developer", dev(h.developerDashboardAction))
	register("DELETE", "/api/developer/dashboards/{id}", "developer", dev(h.developerDashboardAction))
	register("POST", "/api/developer/dashboards/{id}/widgets", "developer", dev(h.developerDashboardAction))
	register("PATCH", "/api/developer/dashboards/{id}/widgets/{widgetId}", "developer", dev(h.developerDashboardAction))
	register("DELETE", "/api/developer/dashboards/{id}/widgets/{widgetId}", "developer", dev(h.developerDashboardAction))
	register("GET", "/api/folders", "any", cors(h.businessFolders))
	register("POST", "/api/developer/migration/generate", "developer", dev(h.migrationGenerate))
	register("POST", "/api/developer/migration/apply", "developer", dev(h.migrationApply))
	// Import is open to any authenticated actor, same as /api/cells — the
	// generic write guard (writeguard.CheckWrite, applied inside
	// importpkg.Store.CommitImport) is what decides whether a given write is
	// allowed, not a platform role. A cost-center manager must be able to
	// import their own scope's facts exactly as they can write cells
	// directly; gating this to "developer" would make that structurally
	// impossible regardless of any access rule granted to them.
	register("POST", "/api/import/upload", "any", cors(h.importUpload))
	register("POST", "/api/import/dimension-members", "developer", dev(h.importDimensionMembers))
	// "any" for the same reason as upload: fetching a link-shared sheet is
	// the read half of an import any cell-writer may perform; the commit
	// that follows is what writeguard polices.
	register("POST", "/api/import/sheets/fetch", "any", cors(h.importSheetFetch))
	register("GET", "/api/import/jobs", "any", cors(h.importJobs))
	register("DELETE", "/api/import/jobs/{id}", "any", cors(h.importJobAction))
	register("GET", "/api/developer/debug/facts", "developer", dev(h.debugFacts))
	register("GET", "/api/developer/debug/calc", "developer", dev(h.debugCalc))
	register("GET", "/api/developer/integrations", "developer", dev(h.developerIntegrations))
	register("POST", "/api/developer/integrations", "developer", dev(h.developerIntegrations))
	register("PATCH", "/api/developer/integrations/{id}/config", "developer", dev(h.developerIntegrationAction))
	register("GET", "/api/developer/integrations/{id}/runs", "developer", dev(h.developerIntegrationAction))
	register("GET", "/api/developer/integrations/{id}", "developer", dev(h.developerIntegrationAction))
	register("PATCH", "/api/developer/integrations/{id}", "developer", dev(h.developerIntegrationAction))
	register("DELETE", "/api/developer/integrations/{id}", "developer", dev(h.developerIntegrationAction))
	register("POST", "/api/developer/integrations/{id}/duplicate", "developer", dev(h.restAPIIntegrationSubAction))
	register("POST", "/api/developer/integrations/{id}/validate", "developer", dev(h.restAPIIntegrationSubAction))
	register("POST", "/api/developer/integrations/{id}/test", "developer", dev(h.restAPIIntegrationSubAction))
	register("GET", "/api/developer/integration-runs/{runId}", "developer", dev(h.integrationRunDetail))
	register("POST", "/api/developer/integration-runs/{runId}/cancel", "developer", dev(h.integrationRunDetail))
	register("GET", "/api/developer/integration-connections", "developer", dev(h.integrationConnections))
	register("POST", "/api/developer/integration-connections", "developer", dev(h.integrationConnections))
	register("PATCH", "/api/developer/integration-connections/{id}", "developer", dev(h.integrationConnectionAction))
	register("DELETE", "/api/developer/integration-connections/{id}", "developer", dev(h.integrationConnectionAction))
	register("POST", "/api/developer/integration-connections/{id}/test", "developer", dev(h.integrationConnectionAction))
	register("POST", "/api/developer/integration-connections/{id}/oauth/start", "developer", dev(h.integrationConnectionAction))
	register("POST", "/api/developer/integration-connections/{id}/oauth/disconnect", "developer", dev(h.integrationConnectionAction))
	// The provider sends the browser here after consent; the state is the
	// only credential it arrives with (rest_api_integrations.go).
	register("GET", "/api/integrations/oauth/callback", "public", cors(h.integrationOAuthCallback))
	register("GET", "/api/integrations", "any", cors(h.listIntegrations))
	register("POST", "/api/integrations/{id}/run", "any", cors(h.integrationRun))
	register("GET", "/api/developer/form-integrations", "developer", dev(h.developerFormIntegrations))
	register("POST", "/api/developer/form-integrations", "developer", dev(h.developerFormIntegrations))
	register("POST", "/api/developer/form-integrations/{id}/backfill", "developer", dev(h.developerFormIntegrationAction))
	register("GET", "/api/developer/form-integrations/{id}/preview", "developer", dev(h.developerFormIntegrationAction))
	register("PATCH", "/api/developer/form-integrations/{id}", "developer", dev(h.developerFormIntegrationAction))
	register("DELETE", "/api/developer/form-integrations/{id}", "developer", dev(h.developerFormIntegrationAction))
	register("GET", "/api/developer/workflows", "developer", dev(h.developerWorkflows))
	register("POST", "/api/developer/workflows", "developer", dev(h.developerWorkflows))
	register("GET", "/api/developer/workflows/{id}", "developer", dev(h.developerWorkflowAction))
	register("PATCH", "/api/developer/workflows/{id}", "developer", dev(h.developerWorkflowAction))
	register("DELETE", "/api/developer/workflows/{id}", "developer", dev(h.developerWorkflowAction))
	register("POST", "/api/developer/workflows/{id}/validate", "developer", dev(h.developerWorkflowAction))
	register("POST", "/api/developer/workflows/{id}/publish", "developer", dev(h.developerWorkflowAction))
	register("POST", "/api/developer/workflows/{id}/archive", "developer", dev(h.developerWorkflowAction))
	register("POST", "/api/developer/workflows/{id}/restore", "developer", dev(h.developerWorkflowAction))
	register("POST", "/api/developer/workflows/{id}/duplicate", "developer", dev(h.developerWorkflowAction))
	register("GET", "/api/developer/workflows/{id}/usage", "developer", dev(h.developerWorkflowAction))
	register("GET", "/api/developer/workflows/{id}/instances", "developer", dev(h.developerWorkflowAction))
	register("POST", "/api/developer/workflows/{id}/test-run", "developer", dev(h.developerWorkflowAction))
	register("GET", "/api/developer/workflow-trigger-events", "developer", dev(h.developerWorkflowTriggerEvents))
	register("GET", "/api/developer/workflow-roles", "developer", dev(h.developerWorkflowRoles))

	// AI assistant endpoints (developer only — tightened from developer_or_admin
	// to match every manual developer-console write endpoint exactly, closing
	// a real gap where an admin with no developer role could reach AI writes
	// that mutate the exact same model.* rows the dev()-gated manual endpoints
	// protect).
	register("GET", "/api/ai/sessions", "developer", dev(h.aiSessions))
	register("POST", "/api/ai/sessions", "developer", dev(h.aiSessions))
	register("DELETE", "/api/ai/sessions/{id}", "developer", dev(h.aiDeleteSession))
	register("PATCH", "/api/ai/sessions/{id}", "developer", dev(h.aiSessionDetail))
	register("GET", "/api/ai/sessions/{id}", "developer", dev(h.aiSessionDetail))
	register("POST", "/api/ai/sessions/{id}/messages", "developer", dev(h.aiSessionDetail))
	register("POST", "/api/ai/sessions/{id}/promote-draft", "developer", dev(h.aiSessionDetail))
	register("POST", "/api/ai/sessions/{id}/discard-draft", "developer", dev(h.aiSessionDetail))
	register("GET", "/api/ai/sessions/{id}/proposals", "developer", dev(h.aiSessionDetail))
	register("POST", "/api/ai/sessions/{id}/documents", "developer", dev(h.aiSessionDetail))
	register("DELETE", "/api/ai/sessions/{id}/documents/{docId}", "developer", dev(h.aiSessionDetail))
	register("POST", "/api/ai/sessions/{id}/proposals/{pid}/confirm", "developer", dev(h.aiSessionDetail))
	register("POST", "/api/ai/sessions/{id}/proposals/{pid}/reject", "developer", dev(h.aiSessionDetail))
	register("GET", "/api/ai/settings", "developer", dev(h.aiSettings))
	register("PUT", "/api/ai/settings", "developer", dev(h.aiSettings))
	register("POST", "/api/ai/settings/test", "developer", dev(h.aiTestSettings))

	// Admin endpoints. User-management routes are shared with developers; the
	// remaining admin routes stay restricted to platform/tenant admins.
	register("GET", "/api/admin/me", "admin", adm(h.adminMe))
	register("GET", "/api/admin/infra/nodes", "admin", adm(h.adminInfraNodes))
	register("GET", "/api/admin/tenants", "admin", adm(h.adminTenants))
	register("POST", "/api/admin/tenants", "admin", adm(h.adminTenants))
	register("PATCH", "/api/admin/tenants/{id}", "admin", adm(h.adminTenantAction))
	register("DELETE", "/api/admin/tenants/{id}", "admin", adm(h.adminTenantAction))
	register("GET", "/api/admin/applications", "admin", adm(h.adminApplications))
	register("POST", "/api/admin/applications", "admin", adm(h.adminApplications))
	register("PATCH", "/api/admin/applications/{id}", "admin", adm(h.adminApplicationAction))
	register("DELETE", "/api/admin/applications/{id}", "admin", adm(h.adminApplicationAction))
	register("POST", "/api/admin/models", "admin", adm(h.adminModels))
	register("PUT", "/api/admin/models/{id}/active-revision", "admin", adm(h.adminModelAction))
	register("DELETE", "/api/admin/models/{id}", "admin", adm(h.adminModelAction))
	// Model export/import: the tenant's own administrator and the platform
	// administrator (who has every tenant's capabilities, decided
	// 2026-09-20), never a developer — moving whole models across tenants
	// is an owner action.
	tenantAdm := func(fn http.HandlerFunc) http.HandlerFunc { return cors(h.guard(fn, "tenant_admin", "platform_admin")) }
	register("GET", "/api/admin/models/{id}/export", "admin", tenantAdm(h.adminModelExport))
	register("GET", "/api/admin/models/{id}/export/package", "admin", tenantAdm(h.adminModelExportPackage))
	register("POST", "/api/admin/models/import", "admin", tenantAdm(h.adminModelImport))
	register("POST", "/api/admin/revisions", "admin", adm(h.adminRevisions))
	register("PATCH", "/api/admin/revisions/{id}", "admin", adm(h.adminRevisionAction))
	register("DELETE", "/api/admin/revisions/{id}", "admin", adm(h.adminRevisionAction))
	register("GET", "/api/admin/users", "user_admin", userAdm(h.adminUsers))
	register("POST", "/api/admin/users", "user_admin", userAdm(h.adminUsers))
	// The exact trailing-slash path (no id segment) is also a real, directly
	// reachable creation route — adminUserAction's own create branch, not
	// just adminUsers' internal delegation to it. Covered by
	// admin_user_roles_test.go's TestCreateUserRoleGate, which posts here
	// directly.
	register("POST", "/api/admin/users/", "user_admin", userAdm(h.adminUserAction))
	register("PATCH", "/api/admin/users/{id}", "user_admin", userAdm(h.adminUserAction))
	register("DELETE", "/api/admin/users/{id}", "user_admin", userAdm(h.adminUserAction))
	// Invitations expire, and mail gets lost. Without a resend, the only way
	// back is to delete the user and re-create them, which discards their
	// roles and access grants.
	register("POST", "/api/admin/users/{id}/invite", "user_admin", userAdm(h.adminUserAction))
	register("POST", "/api/admin/users/{id}/roles", "user_admin", userAdm(h.adminUserAction))
	register("DELETE", "/api/admin/users/{id}/roles/{role}", "user_admin", userAdm(h.adminUserAction))
	register("POST", "/api/admin/users/{id}/access/apps/{appId}", "user_admin", userAdm(h.adminUserAction))
	register("DELETE", "/api/admin/users/{id}/access/apps/{appId}", "user_admin", userAdm(h.adminUserAction))
	register("POST", "/api/admin/users/{id}/access/models/{modelId}", "user_admin", userAdm(h.adminUserAction))
	register("DELETE", "/api/admin/users/{id}/access/models/{modelId}", "user_admin", userAdm(h.adminUserAction))
	register("GET", "/api/admin/workspaces", "user_admin", userAdm(h.adminWorkspaces))
	register("GET", "/api/admin/audit", "admin", adm(h.adminAudit))
	// Enterprise: export, collector pull and retention (ee/auditexport).
	auditGate := func(fn http.HandlerFunc) http.HandlerFunc {
		return adm(h.requireFeature(license.FeatureAuditExport, fn))
	}
	register("GET", "/api/admin/usage", "admin", adm(h.requireFeature(license.FeatureUsageAnalytics, h.adminUsage)))
	// White-labelling: the console reads /api/branding before sign-in (by
	// host) and after (by tenant); the tenant admin edits it (ee/branding).
	register("GET", "/api/branding", "public", cors(h.publicBranding))
	brandGate := func(fn http.HandlerFunc) http.HandlerFunc {
		return adm(h.requireFeature(license.FeatureWhiteLabel, fn))
	}
	register("GET", "/api/admin/branding", "admin", brandGate(h.adminBranding))
	register("PUT", "/api/admin/branding", "admin", brandGate(h.adminBranding))
	register("DELETE", "/api/admin/branding", "admin", brandGate(h.adminBranding))
	register("GET", "/api/admin/audit/export", "admin", auditGate(h.auditExport))
	register("GET", "/api/admin/audit/settings", "admin", auditGate(h.auditSettings))
	register("PUT", "/api/admin/audit/settings", "admin", auditGate(h.auditSettings))
	register("DELETE", "/api/admin/audit/settings", "admin", auditGate(h.auditSettings))

	// Business-admin-only endpoints
	register("GET", "/api/business-admin/roles", "developer_or_business_admin", baOrDev(h.baRoles))
	register("POST", "/api/business-admin/roles", "developer_or_business_admin", baOrDev(h.baRoles))
	register("PATCH", "/api/business-admin/roles/{id}", "developer_or_business_admin", baOrDev(h.baRoleAction))
	register("DELETE", "/api/business-admin/roles/{id}", "developer_or_business_admin", baOrDev(h.baRoleAction))
	register("GET", "/api/business-admin/roles/{id}/dashboards", "developer_or_business_admin", baOrDev(h.baRoleAction))
	register("PUT", "/api/business-admin/roles/{id}/dashboards", "developer_or_business_admin", baOrDev(h.baRoleAction))
	register("GET", "/api/business-admin/roles/{id}/members", "developer_or_business_admin", baOrDev(h.baRoleAction))
	register("POST", "/api/business-admin/roles/{id}/members", "developer_or_business_admin", baOrDev(h.baRoleAction))
	register("DELETE", "/api/business-admin/roles/{id}/members/{userId}", "developer_or_business_admin", baOrDev(h.baRoleAction))
	register("GET", "/api/business-admin/users", "business_admin", ba(h.baUsers))
	register("GET", "/api/business-admin/users/{id}/access-rules", "business_admin", ba(h.baUserAction))
	register("PUT", "/api/business-admin/users/{id}/access-rules", "business_admin", ba(h.baUserAction))
	register("GET", "/api/business-admin/available", "business_admin", ba(h.baAvailable))

	// Workflow / notifications (any authenticated user)
	register("POST", "/api/workflow/submit", "any", cors(h.workflowSubmit))
	register("GET", "/api/workflow/history", "business_admin", ba(h.workflowHistory))
	register("GET", "/api/workflow/my-history", "any", cors(h.workflowMyHistory))
	register("GET", "/api/workflow/definitions", "any", cors(h.workflowDefinitions))
	register("POST", "/api/workflow/instances", "any", cors(h.workflowStartInstance))
	register("PATCH", "/api/workflow/instances/{id}", "business_admin", ba(h.workflowInstanceAction))
	register("GET", "/api/notifications", "any", cors(h.notifications))
	register("POST", "/api/notifications/mark-read", "any", cors(h.markNotifRead))
	register("GET", "/api/notifications/settings", "admin", adm(h.notificationSettings))
	register("PUT", "/api/notifications/settings", "admin", adm(h.notificationSettings))
	register("DELETE", "/api/notifications/settings", "admin", adm(h.notificationSettings))
	register("POST", "/api/notifications/settings/test", "admin", adm(h.notificationTestSend))

	// Tenant-level AI provider key (enterprise). Community and commercial
	// deployments answer 403 here and keep per-user keys; see ee/aikeys.
	tenantAI := func(fn http.HandlerFunc) http.HandlerFunc {
		return adm(h.requireFeature(license.FeatureTenantAIKeys, fn))
	}
	// Enterprise identity: single sign-on and SCIM provisioning (ee/sso,
	// ee/scim). Discovery is public by nature — nobody is signed in yet.
	register("GET", "/api/sso/discover", "public", cors(h.ssoDiscover))
	ssoGate := func(fn http.HandlerFunc) http.HandlerFunc { return adm(h.requireFeature(license.FeatureSSO, fn)) }
	register("GET", "/api/admin/sso", "admin", ssoGate(h.ssoSettings))
	register("PUT", "/api/admin/sso", "admin", ssoGate(h.ssoSettings))
	register("DELETE", "/api/admin/sso", "admin", ssoGate(h.ssoSettings))
	register("POST", "/api/admin/sso/test", "admin", ssoGate(h.ssoTest))
	scimGate := func(fn http.HandlerFunc) http.HandlerFunc { return adm(h.requireFeature(license.FeatureSCIM, fn)) }
	register("GET", "/api/admin/scim/tokens", "admin", scimGate(h.scimTokens))
	register("POST", "/api/admin/scim/tokens", "admin", scimGate(h.scimTokens))
	register("DELETE", "/api/admin/scim/tokens/{id}", "admin", scimGate(h.scimTokenRevoke))
	for _, p := range []string{"/api/scim/v2/ServiceProviderConfig", "/api/scim/v2/ResourceTypes", "/api/scim/v2/Schemas"} {
		register("GET", p, "scim", h.scimEndpoint)
	}
	for _, res := range []string{"Users", "Groups"} {
		register("GET", "/api/scim/v2/"+res, "scim", h.scimEndpoint)
		register("POST", "/api/scim/v2/"+res, "scim", h.scimEndpoint)
		for _, m := range []string{"GET", "PUT", "PATCH", "DELETE"} {
			register(m, "/api/scim/v2/"+res+"/{id}", "scim", h.scimEndpoint)
		}
	}

	register("GET", "/api/admin/ai-settings", "admin", tenantAI(h.tenantAISettings))
	register("PUT", "/api/admin/ai-settings", "admin", tenantAI(h.tenantAISettings))
	register("POST", "/api/admin/ai-settings/test", "admin", tenantAI(h.tenantAITestSettings))
	register("DELETE", "/api/admin/ai-settings/key", "admin", tenantAI(h.tenantAIKeyClear))
	// A tenant's own credentials for external systems (tenant_connections.go).
	register("GET", "/api/developer/integrations/google-service-account", "developer", devOrAdm(h.googleConnection))
	register("PUT", "/api/developer/integrations/google-service-account", "developer", devOrAdm(h.googleConnection))
	register("DELETE", "/api/developer/integrations/google-service-account", "developer", devOrAdm(h.googleConnection))
	register("POST", "/api/developer/integrations/google-service-account/test", "developer", devOrAdm(h.googleConnectionTest))

	// CRUD forms + automation (authenticated users)
	register("GET", "/api/dimensions", "any", cors(h.publicDimensions))
	// Form *schema* CRUD (create/edit/delete the form definition itself) is
	// developer-only, matching the Business Console's own empty-state text
	// ("Ask a Developer to create a form"). Record-level endpoints below
	// (records, export/import/sync) stay "any" — that's the business-user
	// facing surface, already writeguard-gated on the write path.
	register("GET", "/api/forms", "any", cors(h.forms))
	register("POST", "/api/forms", "developer", dev(h.forms))
	register("GET", "/api/forms/{id}/export", "any", cors(h.formsRouter))
	register("POST", "/api/forms/{id}/import", "any", cors(h.formsRouter))
	register("POST", "/api/forms/{id}/sync", "any", cors(h.formsRouter))
	register("GET", "/api/forms/{id}/records", "any", cors(h.formsRouter))
	register("POST", "/api/forms/{id}/records", "any", cors(h.formsRouter))
	register("PATCH", "/api/forms/{id}", "developer", dev(h.formsRouter))
	register("DELETE", "/api/forms/{id}", "developer", dev(h.formsRouter))
	register("PUT", "/api/records/{id}", "any", cors(h.recordAction))
	register("DELETE", "/api/records/{id}", "any", cors(h.recordAction))
	// Rule *management* is developer-only, matching the Developer
	// Console's own AutomationTab.tsx (never referenced from any business
	// console) and the CategoryModelChange audit category these events
	// already use, same as dimension/metric changes. Triggering an
	// EXISTING rule stays "any": AutomationButtonWidget places a real
	// business-user-facing "run this automation" button on dashboards —
	// automationTrigger enforces ownership internally instead (see its
	// own comment) rather than a role gate.
	register("GET", "/api/automation/rules", "any", cors(h.automationRules))
	register("POST", "/api/automation/rules", "developer", dev(h.automationRules))
	register("PATCH", "/api/automation/rules/{id}", "developer", dev(h.automationRuleAction))
	register("DELETE", "/api/automation/rules/{id}", "developer", dev(h.automationRuleAction))
	register("POST", "/api/automation/trigger/{id}", "any", cors(h.automationTrigger))
	register("GET", "/api/automation/executions", "any", cors(h.automationExecutions))

	// Business-user dashboard rendering
	register("GET", "/api/dashboards", "any", cors(h.businessDashboards))
	register("GET", "/api/dashboards/{id}", "any", cors(h.businessDashboardDetail))

	// Chart-data endpoint (any authenticated user)
	register("POST", "/api/dashboard-widgets/{id}/chart-data", "any", cors(h.dashboardWidgetAction))
}

// ── middleware ─────────────────────────────────────────────────────────────────

type ctxKey int

const appIDCtxKey ctxKey = iota

// appIDMiddleware reads the X-App-Id header and injects the value into the
// request context so all downstream handlers can scope to the selected app.
func appIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get("X-App-Id"); id != "" {
			r = r.WithContext(context.WithValue(r.Context(), appIDCtxKey, id))
		}
		next.ServeHTTP(w, r)
	})
}

func cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Dev-User, Authorization, X-HTTP-Method-Override, X-App-Id")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

// ── actor resolution ──────────────────────────────────────────────────────────

type actor struct {
	UserID string   `json:"user_id"`
	Email  string   `json:"email"`
	Name   string   `json:"display_name"`
	Roles  []string `json:"roles"`
	// CustomerID is the tenant the account belongs to, when it has one of
	// its own (self-service and admin-created tenant users); empty for
	// platform-level accounts.
	CustomerID string `json:"customer_id,omitempty"`
}

var errAccessDenied = errors.New("access denied")

func (a *actor) hasRole(role string) bool {
	if a == nil {
		return false
	}
	for _, have := range a.Roles {
		if have == role {
			return true
		}
	}
	return false
}

func (h *handler) resolveActor(ctx context.Context, r *http.Request) (*actor, error) {
	// The plan guard resolved the actor of a mutating request already.
	if cached, ok := ctx.Value(actorCtxKey).(*actor); ok && cached != nil {
		return cached, nil
	}
	var a *actor
	var err error
	if h.devMode {
		a, err = h.resolveDevActor(ctx, r)
	} else if h.jwks == nil {
		return nil, fmt.Errorf("authentication not configured")
	} else {
		a, err = h.resolveJWTActor(ctx, r)
	}
	if err == nil {
		h.touchLastSeen(ctx, a.UserID)
	}
	return a, err
}

// lastSeenInterval is how often an account's last_seen_at is refreshed:
// a busy session costs one write per interval, not one per request.
const lastSeenInterval = 5 * time.Minute

// touchLastSeen records that an account was here, for the usage
// analytics' "active users". Throttled per account in memory; a restart
// costs at most one extra write per account.
func (h *handler) touchLastSeen(ctx context.Context, userID string) {
	if userID == "" {
		return
	}
	now := time.Now()
	h.lastSeenMu.Lock()
	if h.lastSeen == nil {
		h.lastSeen = map[string]time.Time{}
	}
	if last, ok := h.lastSeen[userID]; ok && now.Sub(last) < lastSeenInterval {
		h.lastSeenMu.Unlock()
		return
	}
	h.lastSeen[userID] = now
	if len(h.lastSeen) > 50000 {
		h.lastSeen = map[string]time.Time{userID: now}
	}
	h.lastSeenMu.Unlock()
	if _, err := h.db.Exec(ctx, `UPDATE identity.user SET last_seen_at = now() WHERE id = $1::uuid`, userID); err != nil {
		h.log.Debug().Err(err).Str("user_id", userID).Msg("last_seen_at not updated")
	}
}

func (h *handler) resolveDevActor(ctx context.Context, r *http.Request) (*actor, error) {
	persona := r.Header.Get("X-Dev-User")
	if persona == "" {
		persona = "dept_head"
	}
	sub, ok := devPersonas[persona]
	if !ok {
		// Not a predefined persona key: treat the header value as a
		// keycloak_sub directly, so users created through the admin console
		// (sub "admin-created-<email>") are switchable in dev mode.
		if a, err := h.actorByKeycloakSub(ctx, persona); err == nil {
			return a, nil
		}
		sub = "demo-dept-head-001"
	}
	return h.actorByKeycloakSub(ctx, sub)
}

// devPersonaList serves GET /api/dev/personas — every user in the database
// as a switchable dev persona. Predefined personas keep their legacy short
// key; everyone else is addressed by keycloak_sub (resolveDevActor accepts
// both). Dev mode only: 404s in production so it leaks nothing.
func (h *handler) devPersonaList(w http.ResponseWriter, r *http.Request) {
	if !h.devMode {
		jsonErr(w, fmt.Errorf("not found"), http.StatusNotFound)
		return
	}
	if r.Method != http.MethodGet {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()

	subToKey := make(map[string]string, len(devPersonas))
	for key, sub := range devPersonas {
		subToKey[sub] = key
	}

	rows, err := h.db.Query(ctx, `
		SELECT COALESCE(u.keycloak_sub,''), u.display_name, u.email,
		       COALESCE(string_agg(DISTINCT ra.role::text, ','), ''),
		       COALESCE(c.name, (
		           -- admin-created users have no customer_id; derive the
		           -- tenant from their workspace role assignments instead
		           SELECT c2.name
		           FROM identity.role_assignment ra2
		           JOIN core.workspace w2 ON w2.id = ra2.workspace_id
		           JOIN core.customer c2 ON c2.id = w2.customer_id
		           WHERE ra2.user_id = u.id
		           ORDER BY ra2.id LIMIT 1
		       ), (
		           -- …or from explicit app-access grants (same membership
		           -- definition as everywhere else)
		           SELECT c3.name
		           FROM identity.user_app_access ua3
		           JOIN core.application app3 ON app3.id = ua3.application_id
		           JOIN core.customer c3 ON c3.id = app3.customer_id
		           WHERE ua3.user_id = u.id
		           ORDER BY c3.name LIMIT 1
		       ), '')
		FROM identity.user u
		LEFT JOIN identity.role_assignment ra ON ra.user_id = u.id
		LEFT JOIN core.customer c ON c.id = u.customer_id
		GROUP BY u.id, u.keycloak_sub, u.display_name, u.email, c.name
		ORDER BY min(u.created_at)
	`)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type devPersona struct {
		Key    string   `json:"key"` // value to send as X-Dev-User
		Label  string   `json:"label"`
		Email  string   `json:"email"`
		Roles  []string `json:"roles"`
		Tenant string   `json:"tenant"`
	}
	var list []devPersona
	for rows.Next() {
		var sub, name, email, rolesCSV, tenant string
		if err := rows.Scan(&sub, &name, &email, &rolesCSV, &tenant); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if sub == "" {
			continue // no sub → not addressable via X-Dev-User
		}
		p := devPersona{Key: sub, Label: name, Email: email, Roles: []string{}, Tenant: tenant}
		if key, ok := subToKey[sub]; ok {
			p.Key = key
		}
		if rolesCSV != "" {
			p.Roles = strings.Split(rolesCSV, ",")
		}
		list = append(list, p)
	}
	if err := rows.Err(); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []devPersona{}
	}
	jsonOK(w, list)
}

func (h *handler) resolveJWTActor(ctx context.Context, r *http.Request) (*actor, error) {
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return nil, fmt.Errorf("missing Bearer token")
	}
	tokenStr := strings.TrimPrefix(authHeader, "Bearer ")

	if h.jwks == nil {
		return nil, fmt.Errorf("JWKS validator not configured")
	}
	claims, err := h.jwks.Validate(tokenStr)
	if err != nil {
		return nil, fmt.Errorf("invalid token: %w", err)
	}
	a, err := h.actorByKeycloakSub(ctx, claims.Subject)
	if err != nil {
		// A valid token for a subject the platform has never seen: a first
		// login through a tenant's own identity provider, if the tenant
		// allows accounts to be created that way (ee/sso).
		if jit, jitErr := h.jitProvision(ctx, claims); jitErr == nil {
			return jit, nil
		} else if np := (*errNotProvisionable)(nil); errors.As(jitErr, &np) {
			return nil, jitErr
		}
		return nil, err
	}
	return a, nil
}

func (h *handler) actorByKeycloakSub(ctx context.Context, sub string) (*actor, error) {
	var a actor
	var rolesCSV string
	err := h.db.QueryRow(ctx, `
		SELECT u.id::text, u.email, u.display_name, COALESCE(u.customer_id::text, ''),
		       COALESCE(string_agg(ra.role::text, ','), '') AS roles
		FROM identity.user u
		LEFT JOIN identity.role_assignment ra ON ra.user_id = u.id
		WHERE u.keycloak_sub = $1 AND u.disabled_at IS NULL
		GROUP BY u.id, u.email, u.display_name, u.customer_id
	`, sub).Scan(&a.UserID, &a.Email, &a.Name, &a.CustomerID, &rolesCSV)
	if err != nil {
		return nil, fmt.Errorf("resolve actor %q: %w", sub, err)
	}
	if rolesCSV != "" {
		a.Roles = strings.Split(rolesCSV, ",")
	} else {
		a.Roles = []string{}
	}
	return &a, nil
}

// guard wraps a handler so it is only reachable by actors holding the given role.
func (h *handler) guard(fn http.HandlerFunc, roles ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, ok := h.requireRole(w, r, roles...); !ok {
			return
		}
		fn(w, r)
	}
}

// requireRole resolves the actor and verifies they hold at least one of the
// required roles. Returns (actor, true) on success; writes 401/403 and returns
// (nil, false) on failure. Callers must return immediately when false.
func (h *handler) requireRole(w http.ResponseWriter, r *http.Request, roles ...string) (*actor, bool) {
	act, err := h.resolveActor(r.Context(), r)
	if err != nil {
		// A refused first login carries a reason the person can act on.
		if np := (*errNotProvisionable)(nil); errors.As(err, &np) {
			jsonErr(w, err, http.StatusForbidden)
			return nil, false
		}
		jsonErr(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return nil, false
	}
	for _, need := range roles {
		for _, have := range act.Roles {
			if have == need {
				return act, true
			}
		}
	}
	jsonErr(w, fmt.Errorf("forbidden: requires one of %v", roles), http.StatusForbidden)
	return nil, false
}

// validateAggRule rejects aggregation rules the calculation engine does not
// implement. There was no check at all before: rollup.combineAgg falls through
// to "sum" for anything it does not recognise, so a typo — or a value the UI
// offered but the engine never implemented — did not fail, it silently
// computed a wrong total that looked like a real number everywhere.
//
// "rate" is in this list only because it is already selectable in the console
// and may be stored against existing metrics; the engine has no case for it,
// so it currently behaves as sum. Rejecting it here would break saving those
// metrics, which is a product decision rather than a validation one.
func jsonAccessErr(w http.ResponseWriter, err error, label string) {
	if errors.Is(err, errAccessDenied) {
		jsonErr(w, fmt.Errorf("forbidden: %s", label), http.StatusForbidden)
		return
	}
	jsonErr(w, fmt.Errorf("%s: %w", label, err), http.StatusInternalServerError)
}

func (h *handler) adminScopeCustomerIDs(ctx context.Context, a *actor) (all bool, customerIDs []string, err error) {
	// Platform admins and platform-level developers (no workspace scope, no
	// tenant of their own) operate platform-wide.
	if a.hasRole("platform_admin") || h.isGlobalBuilder(ctx, a) {
		return true, nil, nil
	}
	rows, err := h.db.Query(ctx, `
		SELECT DISTINCT customer_id FROM (
		    SELECT u.customer_id::text AS customer_id
		    FROM identity.user u
		    WHERE u.id=$1::uuid
		      AND u.customer_id IS NOT NULL
		      AND EXISTS (
		          SELECT 1 FROM identity.role_assignment ra
		          WHERE ra.user_id=u.id AND ra.role IN ('tenant_admin', 'developer')
		      )
		    UNION
		    -- A workspace-scoped tenant_admin/developer grant scopes to that
		    -- workspace's tenant. An UNSCOPED (NULL workspace) tenant_admin/
		    -- developer grant — which the Users panel offers as a "platform
		    -- role" — means "admin of the tenants I belong to", and for an
		    -- admin-created user with no customer_id of their own, membership
		    -- is their OTHER workspace roles (same definition as
		    -- adminCanAccessUser and the users list: own customer_id, a
		    -- workspace role in the tenant, or an explicit app/model grant).
		    -- Ignoring those roles left such a user — tenant_admin without a
		    -- workspace plus business_admin in their tenant's workspace —
		    -- with NO tenant at all: empty Applications view, no model
		    -- export/import (found live in production, 2026-09-10).
		    SELECT w.customer_id::text AS customer_id
		    FROM identity.role_assignment ra
		    JOIN core.workspace w ON w.id=ra.workspace_id
		    WHERE ra.user_id=$1::uuid
		      AND (
		          ra.role IN ('tenant_admin', 'developer')
		          OR EXISTS (
		              SELECT 1 FROM identity.role_assignment ra2
		              WHERE ra2.user_id=$1::uuid
		                AND ra2.role IN ('tenant_admin', 'developer')
		                AND ra2.workspace_id IS NULL
		          )
		      )
		    UNION
		    -- Explicit app/model access grants also confer tenant scope: a
		    -- tenant_admin/developer created by the platform admin has no
		    -- customer_id of their own, so the tenants of the apps/models the
		    -- platform admin granted them ARE their scope.
		    SELECT app.customer_id::text AS customer_id
		    FROM identity.user_app_access ua
		    JOIN core.application app ON app.id = ua.application_id
		    WHERE ua.user_id=$1::uuid
		      AND EXISTS (
		          SELECT 1 FROM identity.role_assignment ra
		          WHERE ra.user_id=$1::uuid AND ra.role IN ('tenant_admin', 'developer')
		      )
		    UNION
		    SELECT app2.customer_id::text AS customer_id
		    FROM identity.user_model_access um
		    JOIN core.model m ON m.id = um.model_id
		    JOIN core.application app2 ON app2.id = m.application_id
		    WHERE um.user_id=$1::uuid
		      AND EXISTS (
		          SELECT 1 FROM identity.role_assignment ra
		          WHERE ra.user_id=$1::uuid AND ra.role IN ('tenant_admin', 'developer')
		      )
		) scoped
		WHERE customer_id IS NOT NULL
		ORDER BY customer_id
	`, a.UserID)
	if err != nil {
		return false, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return false, nil, err
		}
		customerIDs = append(customerIDs, id)
	}
	if err := rows.Err(); err != nil {
		return false, nil, err
	}
	return false, customerIDs, nil
}

func (h *handler) adminCanAccessUser(ctx context.Context, a *actor, userID string) (bool, error) {
	all, customerIDs, err := h.adminScopeCustomerIDs(ctx, a)
	if err != nil || all {
		return all, err
	}
	if len(customerIDs) == 0 {
		return false, nil
	}
	// Tenant membership uses one definition everywhere (same as the users
	// list): own customer_id, a workspace role in the tenant, or an explicit
	// app/model access grant into it.
	var ok bool
	err = h.db.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1
		    FROM identity.user u
		    WHERE u.id=$1::uuid
		      AND (
		          u.customer_id::text = ANY($2)
		          OR EXISTS (
		              SELECT 1
		              FROM identity.role_assignment ra
		              JOIN core.workspace w ON w.id=ra.workspace_id
		              WHERE ra.user_id=u.id AND w.customer_id::text = ANY($2)
		          )
		          OR EXISTS (
		              SELECT 1 FROM identity.user_app_access ua
		              JOIN core.application app ON app.id=ua.application_id
		              WHERE ua.user_id=u.id AND app.customer_id::text = ANY($2)
		          )
		          OR EXISTS (
		              SELECT 1 FROM identity.user_model_access um
		              JOIN core.model m ON m.id=um.model_id
		              JOIN core.application app2 ON app2.id=m.application_id
		              WHERE um.user_id=u.id AND app2.customer_id::text = ANY($2)
		          )
		      )
		)
	`, userID, customerIDs).Scan(&ok)
	return ok, err
}

func (h *handler) adminCanAccessWorkspace(ctx context.Context, a *actor, workspaceID string) (bool, error) {
	all, customerIDs, err := h.adminScopeCustomerIDs(ctx, a)
	if err != nil || all {
		return all, err
	}
	if len(customerIDs) == 0 {
		return false, nil
	}
	var ok bool
	err = h.db.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM core.workspace
		    WHERE id=$1::uuid AND customer_id::text = ANY($2)
		)
	`, workspaceID, customerIDs).Scan(&ok)
	return ok, err
}

func (h *handler) adminCanAccessApp(ctx context.Context, a *actor, appID string) (bool, error) {
	all, customerIDs, err := h.adminScopeCustomerIDs(ctx, a)
	if err != nil || all {
		return all, err
	}
	if len(customerIDs) == 0 {
		return false, nil
	}
	var ok bool
	err = h.db.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM core.application
		    WHERE id=$1::uuid AND customer_id::text = ANY($2)
		)
	`, appID, customerIDs).Scan(&ok)
	if err != nil || !ok || a.hasRole("tenant_admin") {
		return ok, err
	}
	return h.actorCanAccessApp(ctx, a, appID)
}

func (h *handler) adminCanAccessModel(ctx context.Context, a *actor, modelID string) (bool, error) {
	all, customerIDs, err := h.adminScopeCustomerIDs(ctx, a)
	if err != nil || all {
		return all, err
	}
	if len(customerIDs) == 0 {
		return false, nil
	}
	var ok bool
	err = h.db.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1
		    FROM core.model m
		    JOIN core.application app ON app.id=m.application_id
		    WHERE m.id=$1::uuid AND app.customer_id::text = ANY($2)
		)
	`, modelID, customerIDs).Scan(&ok)
	if err != nil || !ok || a.hasRole("tenant_admin") {
		return ok, err
	}
	return h.actorCanAccessModel(ctx, a, modelID)
}

// isGlobalBuilder reports whether the actor holds a developer role that is
// not scoped to any workspace — a platform-wide builder who can see and work
// on every tenant's models (the same reach platform_admin has, for building).
func (h *handler) isGlobalBuilder(ctx context.Context, a *actor) bool {
	// A platform-level developer has no tenant of their own. An account
	// that belongs to a tenant (self-service sign-up creates its developer
	// with customer_id set and no workspace scope) is that tenant's
	// developer, never a global one: without this test such an account
	// resolved another tenant's model on its first screen (found live,
	// 2026-09-17).
	if a == nil || !a.hasRole("developer") || a.CustomerID != "" {
		return false
	}
	var ok bool
	_ = h.db.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM identity.role_assignment
			WHERE user_id=$1::uuid AND role='developer' AND workspace_id IS NULL
		)`, a.UserID).Scan(&ok)
	return ok
}

func (h *handler) actorCanAccessApp(ctx context.Context, a *actor, appID string) (bool, error) {
	if a.hasRole("platform_admin") || h.isGlobalBuilder(ctx, a) {
		return true, nil
	}
	if a.hasRole("tenant_admin") {
		return h.adminCanAccessApp(ctx, a, appID)
	}
	var ok bool
	err := h.db.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1
		    FROM core.application app
		    LEFT JOIN core.workspace aw ON aw.id = app.workspace_id
		    WHERE app.id=$1::uuid
		      AND (EXISTS (
		              SELECT 1 FROM core.workspace w
		              JOIN identity.role_assignment ra ON ra.workspace_id=w.id
		              WHERE (w.id=app.workspace_id OR w.customer_id=app.customer_id) AND ra.user_id=$2::uuid
		           ) OR EXISTS (SELECT 1 FROM identity."user" u JOIN identity.role_assignment dra ON dra.user_id = u.id AND dra.role = 'developer'
				             WHERE u.id=$2::uuid AND u.customer_id = COALESCE(app.customer_id, aw.customer_id)))
		      AND (
		          NOT EXISTS (SELECT 1 FROM identity.user_app_access WHERE user_id=$2::uuid)
		          OR EXISTS (SELECT 1 FROM identity.user_app_access WHERE user_id=$2::uuid AND application_id=app.id)
		      )
		      AND EXISTS (
		          SELECT 1
		          FROM core.model m
		          WHERE m.application_id=app.id
		            AND (
		                NOT EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id=$2::uuid)
		                OR EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id=$2::uuid AND model_id=m.id)
		            )
		      )
		)
	`, appID, a.UserID).Scan(&ok)
	return ok, err
}

func (h *handler) actorCanAccessModel(ctx context.Context, a *actor, modelID string) (bool, error) {
	if a.hasRole("platform_admin") || h.isGlobalBuilder(ctx, a) {
		return true, nil
	}
	if a.hasRole("tenant_admin") {
		return h.adminCanAccessModel(ctx, a, modelID)
	}
	var ok bool
	err := h.db.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1
		    FROM core.model m
		    JOIN core.application app ON app.id=m.application_id
		    LEFT JOIN core.workspace aw ON aw.id = app.workspace_id
		    WHERE m.id=$1::uuid
		      AND (EXISTS (
		              SELECT 1 FROM core.workspace w
		              JOIN identity.role_assignment ra ON ra.workspace_id=w.id
		              WHERE (w.id=app.workspace_id OR w.customer_id=app.customer_id) AND ra.user_id=$2::uuid
		           ) OR EXISTS (SELECT 1 FROM identity."user" u JOIN identity.role_assignment dra ON dra.user_id = u.id AND dra.role = 'developer'
				             WHERE u.id=$2::uuid AND u.customer_id = COALESCE(app.customer_id, aw.customer_id)))
		      AND (
		          NOT EXISTS (SELECT 1 FROM identity.user_app_access WHERE user_id=$2::uuid)
		          OR EXISTS (SELECT 1 FROM identity.user_app_access WHERE user_id=$2::uuid AND application_id=app.id)
		      )
		      AND (
		          NOT EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id=$2::uuid)
		          OR EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id=$2::uuid AND model_id=m.id)
		      )
		)
	`, modelID, a.UserID).Scan(&ok)
	return ok, err
}

// ── resource → scope authorization ───────────────────────────────────────────
//
// Every developer/admin handler below takes its target's UUID straight from
// the URL. Route-level `guard(...)` proves the caller holds the developer or
// admin ROLE; on its own it proves nothing about whether the row that UUID
// names belongs to a tenant the caller may touch. requireResourceAccess
// closes that gap in one place: resolve the row's owning model (or
// application, for the two application-scoped kinds), then defer to the
// existing actorCanAccessModel/actorCanAccessApp, which already encode every
// per-role rule — platform_admin, tenant_admin customer scoping,
// developer tenant breadth, and the identity.user_app_access /
// user_model_access per-user restrictions.
//
// Add a row here rather than hand-rolling a check in a handler: a per-handler
// query is exactly how the surface drifted apart in the first place.

// modelScopedResourceSQL resolves a resource ID to the model that owns it.
var modelScopedResourceSQL = map[string]string{
	"dashboard":           `SELECT model_id::text FROM model.dashboard_def WHERE id=$1::uuid`,
	"dashboard_widget":    `SELECT d.model_id::text FROM model.dashboard_widget w JOIN model.dashboard_def d ON d.id = w.dashboard_id WHERE w.id=$1::uuid`,
	"dashboard_folder":    `SELECT model_id::text FROM model.dashboard_folder WHERE id=$1::uuid`,
	"grid":                `SELECT model_id::text FROM model.grid_def WHERE id=$1::uuid`,
	"metric":              `SELECT model_id::text FROM model.metric_def WHERE id=$1::uuid`,
	"dimension":           `SELECT model_id::text FROM model.dimension_def WHERE id=$1::uuid`,
	"dimension_member":    `SELECT d.model_id::text FROM model.dimension_member m JOIN model.dimension_def d ON d.id = m.dimension_id WHERE m.id=$1::uuid`,
	"form":                `SELECT model_id::text FROM model.form_def WHERE id=$1::uuid`,
	"integration":         `SELECT model_id::text FROM model.integration_def WHERE id=$1::uuid`,
	"form_metric_mapping": `SELECT model_id::text FROM model.form_metric_mapping WHERE id=$1::uuid`,
	"revision":            `SELECT model_id::text FROM model.revision WHERE id=$1::uuid`,
	"model":               `SELECT id::text FROM core.model WHERE id=$1::uuid`,
}

// appScopedResourceSQL resolves a resource ID to the application that owns
// it. Workflow definitions and automation rules are application-scoped rather
// than model-scoped, so they can't go through the model map — an application
// with no model yet would resolve to nothing and 404 a legitimate request.
var appScopedResourceSQL = map[string]string{
	"application":     `SELECT id::text FROM core.application WHERE id=$1::uuid`,
	"workflow_def":    `SELECT application_id::text FROM workflow.workflow_def WHERE id=$1::uuid`,
	"automation_rule": `SELECT application_id::text FROM workflow.automation_rule WHERE id=$1::uuid`,
}

// requireResourceAccess reports whether the caller may act on the named
// resource, writing the HTTP error itself when they may not: 404 when the row
// doesn't exist (so a probe can't use this endpoint to test ID existence
// across tenants), 403 when it exists but sits outside the caller's scope —
// matching the wording developerRevisions and baRoleAction already use.
// Callers use it as a guard: `if !h.requireResourceAccess(w, r, "grid", id) { return }`.
func (h *handler) requireResourceAccess(w http.ResponseWriter, r *http.Request, kind, id string) bool {
	ctx := r.Context()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return false
	}
	if id == "" {
		jsonErr(w, fmt.Errorf("not found"), http.StatusNotFound)
		return false
	}

	if query, ok := modelScopedResourceSQL[kind]; ok {
		var modelID string
		if err := h.db.QueryRow(ctx, query, id).Scan(&modelID); err != nil {
			jsonErr(w, fmt.Errorf("%s not found", strings.ReplaceAll(kind, "_", " ")), http.StatusNotFound)
			return false
		}
		allowed, err := h.actorCanAccessModel(ctx, act, modelID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return false
		}
		if !allowed {
			jsonErr(w, fmt.Errorf("forbidden: %s is outside your access scope", strings.ReplaceAll(kind, "_", " ")), http.StatusForbidden)
			return false
		}
		return true
	}

	if query, ok := appScopedResourceSQL[kind]; ok {
		var appID string
		if err := h.db.QueryRow(ctx, query, id).Scan(&appID); err != nil {
			jsonErr(w, fmt.Errorf("%s not found", strings.ReplaceAll(kind, "_", " ")), http.StatusNotFound)
			return false
		}
		allowed, err := h.actorCanAccessApp(ctx, act, appID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return false
		}
		if !allowed {
			jsonErr(w, fmt.Errorf("forbidden: %s is outside your access scope", strings.ReplaceAll(kind, "_", " ")), http.StatusForbidden)
			return false
		}
		return true
	}

	// An unknown kind is a programming error, not a caller error — fail
	// closed rather than silently authorizing.
	jsonErr(w, fmt.Errorf("unknown resource kind %q", kind), http.StatusInternalServerError)
	return false
}

// ── /healthz ──────────────────────────────────────────────────────────────────

func (h *handler) healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// ── /api/demo ─────────────────────────────────────────────────────────────────

type demoContext struct {
	AppID      string `json:"app_id"`
	ModelID    string `json:"model_id"`
	RevisionID string `json:"revision_id"`
	Revision   string `json:"revision"`
	Actor      *actor `json:"actor"`
}

// errRevisionNotInModel reports that the caller explicitly asked for a
// revision that does not belong to the model they were authorized for.
//
// This branch used to look an explicit revision_id up globally (`WHERE id=$1`),
// using modelID only for the fallback paths below. Any authenticated caller
// could therefore name another tenant's revision. Reads came back empty — but
// only because the cell queries filter on model_id and revision_id together,
// which is incidental, not an access check — while writes stored the value:
// POST /api/developer/folders inserted a dashboard_folder whose model_id was
// the caller's own and whose revision_id pointed into another tenant's model.
// It also made the two cases distinguishable to a caller (200 for a revision
// belonging to someone else, 500 for one that exists nowhere), which is an
// existence oracle. Both now resolve to the same 404.
var errRevisionNotInModel = errors.New("revision does not belong to this model")

// rejectForeignRevision writes the 404 and reports true when err says the
// caller's explicit ?revision_id= belongs to another model.
//
// Every handler that accepts a caller-supplied revision_id must funnel through
// this. Several used to discard the error entirely, so without an explicit
// rejection they would now carry on with an empty revision — which in queries
// shaped like `($2 = ” OR revision_id::text = $2)` silently widens the result
// to every revision instead of refusing the request.
func rejectForeignRevision(w http.ResponseWriter, err error) bool {
	if errors.Is(err, errRevisionNotInModel) {
		jsonErr(w, fmt.Errorf("revision not found"), http.StatusNotFound)
		return true
	}
	return false
}

// requireRevisionInModel is the same guard for handlers that take a revision_id
// from the request BODY instead of the ?revision_id= query parameter that
// resolveRevisionCtx covers. It writes the error response and returns false
// when the revision belongs to another model.
//
// The body paths matter more than the query ones: they are writes. Without
// this, POST /api/developer/grids stored a grid_def, and POST /api/import/upload
// stored fact rows, whose model_id was the caller's own but whose revision_id
// pointed into another tenant's model.
func (h *handler) requireRevisionInModel(w http.ResponseWriter, r *http.Request, revisionID, modelID string) bool {
	if revisionID == "" {
		return true
	}
	var found bool
	if err := h.db.QueryRow(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM model.revision WHERE id=$1::uuid AND model_id=$2::uuid)`,
		revisionID, modelID,
	).Scan(&found); err != nil {
		jsonErr(w, fmt.Errorf("resolve revision: %w", err), http.StatusInternalServerError)
		return false
	}
	if !found {
		jsonErr(w, fmt.Errorf("revision not found"), http.StatusNotFound)
		return false
	}
	return true
}

// resolveRevisionCtx returns (revisionID, scenarioName) for the given revisionID,
// or for the model's active revision if revisionID is "".
// Falls back to the most-recently-created revision if no active revision is set.
func (h *handler) resolveRevisionCtx(ctx context.Context, revisionID, modelID string) (revID, name string, err error) {
	if revisionID != "" {
		err = h.db.QueryRow(ctx,
			`SELECT id::text, name FROM model.revision WHERE id=$1::uuid AND model_id=$2::uuid`,
			revisionID, modelID,
		).Scan(&revID, &name)
		if errors.Is(err, pgx.ErrNoRows) {
			err = errRevisionNotInModel
		}
		return
	}
	// Try active revision
	err = h.db.QueryRow(ctx, `
		SELECT s.id::text, s.name
		FROM model.revision s
		JOIN core.model m ON m.active_revision_id = s.id
		WHERE m.id=$1::uuid`, modelID,
	).Scan(&revID, &name)
	if err != nil {
		// Fall back to most recent revision
		err = h.db.QueryRow(ctx,
			`SELECT id::text, name FROM model.revision WHERE model_id=$1::uuid ORDER BY created_at DESC LIMIT 1`,
			modelID,
		).Scan(&revID, &name)
	}
	return
}

// me returns the resolved actor for the current identity, with no model or
// application context required. The frontend uses it for role resolution —
// /api/demo can't serve that purpose because it 403s when the actor has no
// accessible model yet (e.g. a fresh developer in an empty tenant).
func (h *handler) me(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}
	// A member of a tenant learns what their plan means right now
	// (read-only and why); a platform admin has no plan.
	cid := ""
	if !act.hasRole("platform_admin") {
		cid = h.requestCustomerID(ctx, r, act)
	}
	jsonOK(w, meResponse{actor: act, Plan: h.planStateFor(ctx, cid), ContactURL: h.signupCfg.ContactURL})
}

func (h *handler) demo(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}

	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}

	var appID string
	err = h.db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID)
	if err != nil {
		jsonAccessErr(w, err, "resolve app")
		return
	}

	revisionID, scenario, err := h.resolveRevisionCtx(ctx, "", modelID)
	if err != nil {
		jsonErr(w, fmt.Errorf("no revision found for model: %w", err), http.StatusInternalServerError)
		return
	}

	jsonOK(w, demoContext{
		AppID:      appID,
		ModelID:    modelID,
		RevisionID: revisionID,
		Revision:   scenario,
		Actor:      a,
	})
}

// ── /api/metrics ──────────────────────────────────────────────────────────────

type metricRow struct {
	ID                     string   `json:"id"`
	Name                   string   `json:"name"`
	Label                  string   `json:"label"`
	IsInput                bool     `json:"is_input"`
	Formula                *string  `json:"formula,omitempty"`
	AggRule                string   `json:"agg_rule"`
	AggNumeratorMetricID   string   `json:"agg_numerator_metric_id"`
	AggDenominatorMetricID string   `json:"agg_denominator_metric_id"`
	Format                 string   `json:"format"`
	FormatDecimals         int      `json:"format_decimals"`
	FormatCurrency         string   `json:"format_currency"`
	TimeSummary            string   `json:"time_summary,omitempty"` // aggregation across a time dimension (sum|average|min|max|first|last|none)
	Value                  *float64 `json:"value"`
	Readonly               bool     `json:"readonly,omitempty"`
	// DimensionIDs is the ordered dimension IDs of the grid this metric belongs
	// to (via grid_metric -> grid_dimension). Only populated in the /api/grid
	// all_metrics list, so the frontend can resolve a cross-grid-referenced
	// metric's own cell keys instead of assuming the current grid's dims.
	DimensionIDs []string `json:"dimension_ids,omitempty"`
}

func (h *handler) metrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Needed for hidden-metric filtering below — this endpoint used to
	// serve EVERY metric to any authenticated caller, leaking the existence
	// of metrics an identity.user_access_rule (rule_type='metric',
	// access='hidden') hides; grid() has filtered these all along. "read"
	// stays listed — only writes are restricted, elsewhere.
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}

	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}

	revisionID, _, err := h.resolveRevisionCtx(ctx, r.URL.Query().Get("revision_id"), modelID)
	if rejectForeignRevision(w, err) {
		return
	}
	if err != nil {
		jsonErr(w, fmt.Errorf("no revision: %w", err), http.StatusInternalServerError)
		return
	}

	rows, err := h.db.Query(ctx, `
		WITH latest_input AS (
			SELECT DISTINCT ON (fi.metric_id)
				fi.metric_id, fi.value::float8
			FROM runtime.fact_input fi
			WHERE fi.model_id = $2::uuid
			  AND fi.dim_members = '{}'::jsonb
			  AND fi.revision_id = $1::uuid
			ORDER BY fi.metric_id, fi.entered_at DESC, fi.id DESC
		),
		latest_calc AS (
			SELECT DISTINCT ON (cr.metric_id)
				cr.metric_id, cr.value::float8
			FROM runtime.calc_result cr
			WHERE cr.model_id = $2::uuid
			  AND cr.dim_members = '{}'::jsonb
			  AND cr.revision_id = $1::uuid
			ORDER BY cr.metric_id, cr.calc_at DESC
		)
		SELECT m.id::text, m.name, m.is_input, m.formula,
		       m.format, m.format_decimals, m.format_currency, m.time_summary,
		       CASE WHEN m.is_input THEN li.value ELSE lc.value END AS value
		FROM model.metric_def m
		LEFT JOIN latest_input li ON li.metric_id = m.id
		LEFT JOIN latest_calc  lc ON lc.metric_id = m.id
		WHERE m.model_id = $2::uuid
		  AND m.revision_id = $1::uuid
		  AND NOT EXISTS (
		      SELECT 1 FROM identity.user_access_rule ar
		      WHERE ar.user_id = $3::uuid AND ar.rule_type = 'metric'
		        AND ar.ref_id = m.id::text AND ar.access = 'hidden'
		  )
		ORDER BY m.is_input DESC, m.name
	`, revisionID, modelID, a.UserID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var result []metricRow
	for rows.Next() {
		var mr metricRow
		if err := rows.Scan(&mr.ID, &mr.Name, &mr.IsInput, &mr.Formula, &mr.Format, &mr.FormatDecimals, &mr.FormatCurrency, &mr.TimeSummary, &mr.Value); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		mr.Label = toLabel(mr.Name)
		result = append(result, mr)
	}
	if err := rows.Err(); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if result == nil {
		result = []metricRow{}
	}

	// Populate each metric's own grid's dimension IDs (same source as
	// /api/grid's all_metrics) — lets callers like the dashboard KPI-scope
	// picker know which dimensions a metric can actually be scoped by,
	// without needing a whole-model grid fetch just for that.
	metricDims := map[string][]string{}
	mdRows, mdErr := h.db.Query(ctx, `
		SELECT gm.metric_id::text, gd.dimension_id::text
		FROM model.grid_metric gm
		JOIN model.grid_dimension gd ON gd.grid_id = gm.grid_id
		JOIN model.dimension_def d ON d.id = gd.dimension_id
		JOIN model.metric_def m ON m.id = gm.metric_id
		WHERE m.model_id=$1::uuid AND m.revision_id=$2::uuid
		ORDER BY gm.metric_id, d.name
	`, modelID, revisionID)
	if mdErr == nil {
		for mdRows.Next() {
			var metricID, dimID string
			if mdRows.Scan(&metricID, &dimID) == nil {
				metricDims[metricID] = append(metricDims[metricID], dimID)
			}
		}
		mdRows.Close()
	}
	for i := range result {
		result[i].DimensionIDs = metricDims[result[i].ID]
	}

	jsonOK(w, result)
}

// ── /api/cells (writeback) ────────────────────────────────────────────────────

type writebackReq struct {
	ModelID    string            `json:"model_id"`
	RevisionID string            `json:"revision_id"`
	MetricID   string            `json:"metric_id"`
	DimCode    string            `json:"dim_code"`  // legacy: single dim code
	DimCodes   map[string]string `json:"dim_codes"` // preferred: {dimId -> memberCode}
	Value      float64           `json:"value"`
}

func (h *handler) cells(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}

	var req writebackReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	if req.ModelID == "" || req.MetricID == "" {
		jsonErr(w, fmt.Errorf("model_id and metric_id are required"), http.StatusBadRequest)
		return
	}

	// Guard: the caller must have access to this model at all — a metric or
	// revision ID from a model the caller has no grant on must never reach
	// the checks below, regardless of whether those IDs happen to exist.
	if canAccess, caErr := h.actorCanAccessModel(ctx, a, req.ModelID); caErr != nil {
		jsonErr(w, caErr, http.StatusInternalServerError)
		return
	} else if !canAccess {
		jsonErr(w, fmt.Errorf("forbidden: model is outside your access scope"), http.StatusForbidden)
		return
	}

	// Resolve revision: fall back to model's active revision if not provided.
	if req.RevisionID == "" {
		_ = h.db.QueryRow(ctx,
			`SELECT COALESCE(active_revision_id::text, (SELECT id::text FROM model.revision WHERE model_id=$1::uuid ORDER BY created_at DESC LIMIT 1)) FROM core.model WHERE id=$1::uuid`,
			req.ModelID,
		).Scan(&req.RevisionID)
	}

	// Guard: the revision must actually belong to this model — a
	// client-supplied revision_id from a different model must be rejected,
	// not silently accepted. Checked even for the auto-resolved case above
	// (redundant there, since that path is already model-scoped by
	// construction, but harmless).
	var revisionBelongs bool
	if err := h.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM model.revision WHERE id=$1::uuid AND model_id=$2::uuid)`,
		req.RevisionID, req.ModelID,
	).Scan(&revisionBelongs); err != nil {
		jsonErr(w, fmt.Errorf("check revision ownership: %w", err), http.StatusInternalServerError)
		return
	}
	if !revisionBelongs {
		jsonErr(w, fmt.Errorf("revision is outside this model"), http.StatusForbidden)
		return
	}

	// Guard: metric must be flagged is_input=true AND belong to this model —
	// without the model_id predicate, an is_input metric from a completely
	// different tenant's model would otherwise pass.
	var isInput bool
	if err := h.db.QueryRow(ctx,
		`SELECT is_input FROM model.metric_def WHERE id=$1::uuid AND model_id=$2::uuid`, req.MetricID, req.ModelID,
	).Scan(&isInput); err != nil || !isInput {
		jsonErr(w, fmt.Errorf("metric is not writable"), http.StatusForbidden)
		return
	}

	// Guard: user must not be restricted to read-only or hidden access for this metric.
	metricAccess, maErr := writeguard.MetricAccess(ctx, h.db.For(ctx), a.UserID, req.MetricID)
	if maErr != nil {
		jsonErr(w, fmt.Errorf("check metric access: %w", maErr), http.StatusInternalServerError)
		return
	}
	if metricAccess == "hidden" || metricAccess == "read" {
		jsonErr(w, fmt.Errorf("access denied"), http.StatusForbidden)
		return
	}

	// Resolve dim_codes (preferred) or legacy dim_code into a {dimID: code} map.
	resolvedDims := req.DimCodes
	if len(resolvedDims) == 0 && req.DimCode != "" {
		var dimID string
		err = h.db.QueryRow(ctx, `
			SELECT id::text FROM model.dimension_def
			WHERE model_id = $1::uuid
			ORDER BY (name = 'department') DESC, name
			LIMIT 1
		`, req.ModelID).Scan(&dimID)
		if err != nil {
			jsonErr(w, fmt.Errorf("resolve dimension: %w", err), http.StatusInternalServerError)
			return
		}
		resolvedDims = map[string]string{dimID: req.DimCode}
	}

	// Resolve each written dim_code into its dimension_member id.
	writtenMemberIDs := make([]string, 0, len(resolvedDims))
	for dimID, code := range resolvedDims {
		var memberID string
		if err := h.db.QueryRow(ctx,
			`SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`,
			dimID, code,
		).Scan(&memberID); err != nil {
			continue // unknown member: nothing to restrict here
		}
		writtenMemberIDs = append(writtenMemberIDs, memberID)
	}

	// Generic write guard: system-managed revision, hidden/read-only access
	// (cascading through the dimension hierarchy — e.g. a cost center hidden
	// from this user also blocks a write to one of its employees, even
	// without a direct rule on that employee — see writeguard.ExpandHidden's
	// read-side equivalent in grid()), and workflow-lock. The single
	// implementation shared with every import path (HTTP and gRPC) — see
	// writeguard.CheckWrite's package doc.
	if reason, gErr := writeguard.CheckWriteMetrics(ctx, h.db.For(ctx), req.ModelID, req.RevisionID, a.UserID, writtenMemberIDs, []string{req.MetricID}); gErr != nil {
		jsonErr(w, fmt.Errorf("write guard: %w", gErr), http.StatusInternalServerError)
		return
	} else if reason != "" {
		jsonErr(w, fmt.Errorf("%s", reason), http.StatusForbidden)
		return
	}

	if cid := h.customerOfModel(ctx, req.ModelID); cid != "" && h.plans != nil {
		if err := cmp.Or(h.plans.CheckFactRows(ctx, h.db.For(ctx), cid, req.ModelID, 1), h.plans.CheckStorage(ctx, h.db.For(ctx), cid)); err != nil {
			h.jsonLimitErr(w, err)
			return
		}
	}

	dimMembers := "{}"
	if len(resolvedDims) > 0 {
		b, _ := json.Marshal(resolvedDims)
		dimMembers = string(b)
	}

	_, err = h.db.Exec(ctx, `
		INSERT INTO runtime.fact_input
		    (model_id, revision_id, metric_id, dim_members, value, entered_by)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, $5, $6::uuid)
	`, req.ModelID, req.RevisionID, req.MetricID, dimMembers, req.Value, a.UserID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}

	store := calculation.NewStore(h.db.For(ctx))
	calcLog := h.log.With().Str("op", "cells").Logger()
	scheduler := calculation.NewScheduler(calcLog, store, nil)
	if err := scheduler.RecalcAffected(ctx, req.ModelID, req.RevisionID, []string{req.MetricID}); err != nil {
		h.log.Warn().Err(err).Msg("recalc failed after writeback")
	}

	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryDataChange, EventType: auditlog.EventCellWritten,
		ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
		ResourceType: "metric", ResourceID: req.MetricID, RevisionID: req.RevisionID,
		Metadata: map[string]string{"model_id": req.ModelID, "revision_id": req.RevisionID},
	})

	// Dispatch grid_change automation rules fire-and-forget. The changed
	// metric's owning grid (grid_metric is unique per metric) is passed as
	// the event source so rules scoped via source_grid_id can match.
	go func() {
		bgCtx := context.WithoutCancel(ctx)
		appID, _ := h.appIDFromModelID(bgCtx, req.ModelID)
		if appID != "" {
			var sourceGridID string
			_ = h.db.QueryRow(bgCtx,
				`SELECT grid_id::text FROM model.grid_metric WHERE metric_id=$1::uuid LIMIT 1`,
				req.MetricID,
			).Scan(&sourceGridID)
			payload := map[string]string{
				"model_id":    req.ModelID,
				"revision_id": req.RevisionID,
				"metric_id":   req.MetricID,
				"grid_id":     sourceGridID,
			}
			workflow.NewStore(h.db.For(ctx)).DispatchEventRules(bgCtx, appID, req.RevisionID, "grid_change", sourceGridID, a.UserID, payload)
		}
	}()

	jsonOK(w, map[string]string{"status": "ok"})
}

// contextVarSchema mirrors one entry of a workflow_def's context_schema —
// only the fields redactHiddenContext needs.
type contextVarSchema struct {
	Key         string `json:"key"`
	DataType    string `json:"data_type"`
	DimensionID string `json:"dimension_id"`
}

// redactHiddenContext returns a copy of taskCtx with any "Dimension member"
// context value the given user is hidden from (per
// identity.user_access_rule, cascading via writeguard.AncestorChain
// exactly like writeguard.CheckWrite) removed. taskCtx is never mutated.
// tasks()/workflowMyHistory() used to return wi.context verbatim, role
// visibility only — a hidden-scoped assignee could still see the real
// dimension-member code in their own task list. The task/instance row
// itself stays visible (the assignee legitimately needs to know it
// exists); only the specific hidden value is redacted.
func (h *handler) redactHiddenContext(ctx context.Context, userID string, schemaJSON []byte, taskCtx map[string]any) map[string]any {
	if len(taskCtx) == 0 || len(schemaJSON) == 0 {
		return taskCtx
	}
	var vars []contextVarSchema
	if json.Unmarshal(schemaJSON, &vars) != nil {
		return taskCtx
	}
	out := make(map[string]any, len(taskCtx))
	for k, v := range taskCtx {
		out[k] = v
	}
	for _, v := range vars {
		if v.DataType != "Dimension member" || v.DimensionID == "" {
			continue
		}
		code, _ := out[v.Key].(string)
		if code == "" {
			continue
		}
		var memberID string
		if err := h.db.QueryRow(ctx,
			`SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`,
			v.DimensionID, code,
		).Scan(&memberID); err != nil {
			continue // unknown member: nothing to redact
		}
		hidden := false
		if access, aErr := writeguard.HiddenAccess(ctx, h.db.For(ctx), userID, memberID); aErr == nil && access == "hidden" {
			hidden = true
		}
		if !hidden {
			if chain, cErr := writeguard.AncestorChain(ctx, h.db.For(ctx), memberID); cErr == nil {
				for _, anc := range chain {
					if anc.ID == memberID {
						continue
					}
					if access, aErr := writeguard.HiddenAccess(ctx, h.db.For(ctx), userID, anc.ID); aErr == nil && access == "hidden" {
						hidden = true
						break
					}
				}
			}
		}
		if hidden {
			delete(out, v.Key)
		}
	}
	return out
}

// ── /api/tasks ────────────────────────────────────────────────────────────────

// taskContextEntry is one human-readable line of an approval's context: the
// raw value stays (codes are stable identifiers), but the display name is
// what a person reads — an approver shown "country: CA" could not know they
// were approving Canada without opening the model (reported live).
type taskContextEntry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// Display is the resolved human name (member label, metric name); falls
	// back to the raw value when nothing resolves.
	Display string `json:"display"`
	// DimensionName names the dimension a member value belongs to, so the
	// card can say "geography · Canada (CA)". Empty for non-member vars.
	DimensionName string `json:"dimension_name,omitempty"`
}

// contextDisplay resolves display names for a workflow context AFTER hidden
// redaction (never resolve what redactHiddenContext removed). Schema vars
// come first in schema order; context keys the schema doesn't know keep
// their raw value so nothing silently disappears from the card.
func (h *handler) contextDisplay(ctx context.Context, schemaJSON []byte, taskCtx map[string]any) []taskContextEntry {
	if len(taskCtx) == 0 {
		return nil
	}
	var vars []contextVarSchema
	if len(schemaJSON) > 0 {
		_ = json.Unmarshal(schemaJSON, &vars)
	}
	asString := func(v any) string {
		if s, ok := v.(string); ok {
			return s
		}
		b, _ := json.Marshal(v)
		return strings.Trim(string(b), `"`)
	}
	seen := make(map[string]bool, len(taskCtx))
	var out []taskContextEntry
	for _, v := range vars {
		raw, ok := taskCtx[v.Key]
		if !ok || strings.HasPrefix(v.Key, "_") {
			continue
		}
		seen[v.Key] = true
		e := taskContextEntry{Key: v.Key, Value: asString(raw)}
		e.Display = e.Value
		switch v.DataType {
		case "Dimension member":
			if v.DimensionID != "" && e.Value != "" {
				var label, dimName string
				if err := h.db.QueryRow(ctx, `
					SELECT COALESCE(NULLIF(m.label, ''), m.code), d.name
					FROM model.dimension_member m
					JOIN model.dimension_def d ON d.id = m.dimension_id
					WHERE m.dimension_id = $1::uuid AND m.code = $2
				`, v.DimensionID, e.Value).Scan(&label, &dimName); err == nil {
					e.Display = label
					e.DimensionName = dimName
				}
			}
		case "Metric":
			if e.Value != "" {
				// The designer stores a metric id; older/manual contexts may
				// hold a name — resolve either to the metric's display name.
				var name string
				if err := h.db.QueryRow(ctx, `
					SELECT COALESCE(NULLIF(label, ''), name)
					FROM model.metric_def
					WHERE id::text = $1 OR name = $1
					ORDER BY (id::text = $1) DESC
					LIMIT 1
				`, e.Value).Scan(&name); err == nil {
					e.Display = name
				}
			}
		}
		out = append(out, e)
	}
	// Unschema'd keys iterate in map order — sort them for a stable card;
	// schema entries keep their schema position at the front.
	schemaCount := len(out)
	for k, v := range taskCtx {
		if seen[k] || strings.HasPrefix(k, "_") {
			continue
		}
		s := asString(v)
		out = append(out, taskContextEntry{Key: k, Value: s, Display: s})
	}
	tail := out[schemaCount:]
	sort.Slice(tail, func(i, j int) bool { return tail[i].Key < tail[j].Key })
	return out
}

type taskRow struct {
	ID              string `json:"id"`
	StepDefID       string `json:"step_def_id"`
	StepName        string `json:"step_name"`
	StepType        string `json:"step_type"`
	Instructions    string `json:"instructions"`
	SLAHours        *int64 `json:"sla_hours,omitempty"`
	RequiredComment bool   `json:"required_comment"`
	CompletionLabel string `json:"completion_label"`
	// Condition is set for a condition step that could not be evaluated
	// automatically (its input is missing from the context) and therefore
	// waits for a human to pick the branch; the card shows what is asked.
	Condition json.RawMessage `json:"condition,omitempty"`
	// ReworkCount > 0: this step was sent back by a later decision; the
	// note says which step sent it back and why.
	ReworkCount int            `json:"rework_count"`
	ReworkNote  string         `json:"rework_note,omitempty"`
	WFName      string         `json:"workflow_name"`
	InstanceID  string         `json:"instance_id"`
	Context     map[string]any `json:"context"`
	Status      string         `json:"status"`
	DueAt       *string        `json:"due_at,omitempty"`
	CreatedAt   string         `json:"created_at"`
	// Who started the instance — an approver deciding "may this change
	// happen" needs to know who is asking, and the inbox card had no way
	// to say (found live: a business admin facing an Approve button with
	// nothing but a workflow name and a step id).
	RequestedBy string `json:"requested_by"`
	// Human-readable context: same values as Context, with dimension member
	// codes and metric ids resolved to their display names.
	ContextDisplay []taskContextEntry `json:"context_display,omitempty"`
}

func (h *handler) tasks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}

	// Only return steps whose assignee_roles intersect with the current user's
	// roles. assignee_roles may hold platform role enum values (seed-created
	// workflows) or identity.business_role names (what the Developer Console
	// workflow designer writes), so both are matched. Steps with no
	// assignee_roles defined are visible to everyone.
	rows, err := h.db.Query(ctx, `
		SELECT ws.id::text, ws.step_def_id, ws.status::text,
		       wd.name, ws.instance_id::text,
		       wi.context, ws.created_at::text,
		       COALESCE(step_def.elem->>'name', ws.step_def_id),
		       CASE WHEN step_def.elem->>'type' IN ('2','approval')      THEN 'approval'
		            WHEN step_def.elem->>'type' IN ('3','notification') THEN 'notification'
		            WHEN step_def.elem->>'type' IN ('4','condition')    THEN 'condition'
		            ELSE 'task' END,
		       COALESCE(step_def.elem->>'instructions', ''),
		       (step_def.elem->>'sla_hours')::bigint,
		       COALESCE((step_def.elem->>'required_comment')::boolean, false),
		       COALESCE(step_def.elem->>'completion_label', 'Complete'),
		       ws.due_at::text,
		       COALESCE(wi.context_schema_snapshot, wd.context_schema),
		       COALESCE(NULLIF(u.display_name, ''), u.email, ''),
		       step_def.elem->'condition',
		       ws.rework_count, CASE WHEN ws.rework_count > 0 THEN COALESCE(ws.comment, '') ELSE '' END
		FROM workflow.workflow_step ws
		JOIN workflow.workflow_instance wi ON wi.id = ws.instance_id
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		LEFT JOIN identity.user u ON u.id = wi.started_by
		CROSS JOIN LATERAL (
			SELECT elem
			FROM jsonb_array_elements(COALESCE(wi.steps_snapshot, wd.steps)) AS elem
			WHERE elem->>'id' = ws.step_def_id
			LIMIT 1
		) step_def
		WHERE ws.status = 'in_progress'
		  -- A developer's test run is a dry run: its human steps must not
		  -- land in anyone's real inbox (found live 2026-09-13: a business
		  -- user approved a test-run step and its notification was "skipped").
		  AND wi.test_run = false
		  AND (
		    (step_def.elem->'assignee_roles') IS NULL
		    OR (step_def.elem->'assignee_roles') = '[]'::jsonb
		    OR EXISTS (
		        -- Platform-role match, scoped to the task's application:
		        -- a role held in another workspace/customer must not grant
		        -- visibility here (NULL workspace = platform-wide role).
		        SELECT 1
		        FROM identity.role_assignment ra
		        LEFT JOIN core.workspace rws ON rws.id = ra.workspace_id
		        JOIN core.application app ON app.id = wd.application_id
		        WHERE ra.user_id = $1::uuid
		          AND ra.role::text IN (
		              SELECT jsonb_array_elements_text(step_def.elem->'assignee_roles')
		          )
		          AND (ra.workspace_id IS NULL
		               OR app.workspace_id = rws.id
		               OR app.customer_id = rws.customer_id)
		    )
		    OR EXISTS (
		        SELECT 1
		        FROM identity.business_role_member brm
		        JOIN identity.business_role br ON br.id = brm.role_id
		        JOIN core.workspace bws ON bws.id = br.workspace_id
		        JOIN core.application app ON app.id = wd.application_id
		             AND (app.workspace_id = bws.id OR app.customer_id = bws.customer_id)
		        WHERE brm.user_id = $1::uuid
		          AND br.name IN (
		              SELECT jsonb_array_elements_text(step_def.elem->'assignee_roles')
		          )
		    )
		  )
		ORDER BY ws.due_at ASC NULLS LAST, ws.created_at DESC
	`, a.UserID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var result []taskRow
	for rows.Next() {
		var t taskRow
		var ctxJSON, schemaJSON []byte
		var conditionJSON []byte
		if err := rows.Scan(&t.ID, &t.StepDefID, &t.Status, &t.WFName, &t.InstanceID, &ctxJSON, &t.CreatedAt,
			&t.StepName, &t.StepType, &t.Instructions, &t.SLAHours, &t.RequiredComment, &t.CompletionLabel, &t.DueAt, &schemaJSON, &t.RequestedBy, &conditionJSON, &t.ReworkCount, &t.ReworkNote); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if t.StepType == "condition" && len(conditionJSON) > 0 {
			t.Condition = json.RawMessage(conditionJSON)
		}
		_ = json.Unmarshal(ctxJSON, &t.Context)
		t.Context = h.redactHiddenContext(ctx, a.UserID, schemaJSON, t.Context)
		t.ContextDisplay = h.contextDisplay(ctx, schemaJSON, t.Context)
		result = append(result, t)
	}
	if err := rows.Err(); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if result == nil {
		result = []taskRow{}
	}
	jsonOK(w, result)
}

type completeTaskReq struct {
	Decision string `json:"decision"`
	Comment  string `json:"comment"`
}

func (h *handler) taskAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}

	// Parse /api/tasks/{stepId}/complete
	tail := strings.TrimPrefix(r.URL.Path, "/api/tasks/")
	parts := strings.SplitN(tail, "/", 2)
	if len(parts) != 2 || parts[1] != "complete" {
		jsonErr(w, fmt.Errorf("not found"), http.StatusNotFound)
		return
	}
	stepID := parts[0]

	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}

	var req completeTaskReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}

	// Look up instance ID, revision_id, and verify the user has an eligible role for this step.
	var instanceID, taskRevisionID string
	var instCtxJSON []byte
	if err := h.db.QueryRow(ctx,
		`SELECT ws.instance_id::text, wi.context FROM workflow.workflow_step ws JOIN workflow.workflow_instance wi ON wi.id = ws.instance_id WHERE ws.id=$1::uuid`,
		stepID,
	).Scan(&instanceID, &instCtxJSON); err != nil {
		jsonErr(w, fmt.Errorf("step not found"), http.StatusNotFound)
		return
	}

	// Re-check assignee_roles — the single shared implementation also used
	// by the gRPC WorkflowService.CompleteStep, so the two surfaces can't
	// drift onto different eligibility rules again.
	if eligible, err := workflow.NewStore(h.db.For(ctx)).IsAssigneeEligible(ctx, stepID, a.UserID); err != nil || !eligible {
		jsonErr(w, fmt.Errorf("forbidden"), http.StatusForbidden)
		return
	}
	var instCtx map[string]string
	if json.Unmarshal(instCtxJSON, &instCtx) == nil {
		taskRevisionID = instCtx["revision_id"]
	}

	// Guard: required_comment is already surfaced to the UI as a hint
	// (tasks(), above) but was never enforced server-side — a client that
	// skips the hint (or calls this endpoint directly) could complete a
	// step with no comment regardless. Blanket requirement (not just on
	// reject), matching the UI's own existing interpretation of the flag.
	var requiresComment bool
	var stepType string
	_ = h.db.QueryRow(ctx, `
		SELECT COALESCE((step_def.elem->>'required_comment')::boolean, false),
		       COALESCE(step_def.elem->>'type', '')
		FROM workflow.workflow_step ws
		JOIN workflow.workflow_instance wi ON wi.id = ws.instance_id
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		CROSS JOIN LATERAL (
			SELECT elem FROM jsonb_array_elements(COALESCE(wi.steps_snapshot, wd.steps)) AS elem
			WHERE elem->>'id' = ws.step_def_id LIMIT 1
		) step_def
		WHERE ws.id = $1::uuid
	`, stepID).Scan(&requiresComment, &stepType)
	if requiresComment && strings.TrimSpace(req.Comment) == "" {
		jsonErr(w, fmt.Errorf("a comment is required to complete this step"), http.StatusBadRequest)
		return
	}
	// A condition step reaches a human only when the engine could not
	// evaluate it (its input is missing from the context). The human picks
	// the branch — the engine routes a condition by decision "true"/"false"
	// and any other decision routes nowhere, which used to close the
	// instance silently when the inbox's generic "Complete" button sent
	// "complete" (found by the 2026-09-13 scenario run).
	if stepType == "condition" || stepType == "4" {
		if req.Decision != "true" && req.Decision != "false" {
			jsonErr(w, fmt.Errorf("a condition step is routed by deciding \"true\" or \"false\""), http.StatusBadRequest)
			return
		}
	}

	// Complete the step and advance the instance through the workflow
	// store's route-aware engine (approve/reject branch routing,
	// notification auto-dispatch, skipping untaken branches, instance
	// close with completed/cancelled) — the same code path the gRPC
	// workflow service uses, so a reject can never fall through to the
	// approve branch's steps. For an approval, this also atomically runs
	// the step's declared on_approve action (fact copy + dirty-marking), if
	// any — a no-op for any step with no such config.
	if _, err := workflow.NewStore(h.db.For(ctx)).WithLogger(h.log).CompleteStep(ctx, stepID, a.UserID, req.Decision, req.Comment); err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "cannot complete") {
			status = http.StatusConflict
		}
		jsonErr(w, err, status)
		return
	}

	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryDataChange, EventType: auditlog.EventTaskCompleted,
		ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
		ResourceType: "workflow_step", ResourceID: stepID, RevisionID: taskRevisionID,
		Metadata: map[string]string{"instance_id": instanceID, "decision": req.Decision},
	})

	jsonOK(w, map[string]string{"status": "ok"})
}

// ── /api/notifications ────────────────────────────────────────────────────────

type notifRow struct {
	ID           string  `json:"id"`
	TemplateID   string  `json:"template_id"`
	Status       string  `json:"status"`
	TemplateVar  any     `json:"template_vars"`
	CreatedAt    string  `json:"created_at"`
	DeliveredAt  *string `json:"delivered_at,omitempty"`
	ResourceType *string `json:"resource_type,omitempty"`
	ResourceID   *string `json:"resource_id,omitempty"`
}

func (h *handler) notifications(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}

	rows, err := h.db.Query(ctx, `
		SELECT id::text, template_id, status::text, template_vars,
		       created_at::text,
		       CASE WHEN delivered_at IS NOT NULL THEN delivered_at::text END,
		       resource_type, resource_id
		FROM notification.notification
		WHERE recipient_user_id = $1::uuid
		ORDER BY created_at DESC
		LIMIT 50
	`, a.UserID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var result []notifRow
	for rows.Next() {
		var n notifRow
		var varsJSON []byte
		if err := rows.Scan(&n.ID, &n.TemplateID, &n.Status, &varsJSON, &n.CreatedAt, &n.DeliveredAt, &n.ResourceType, &n.ResourceID); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		_ = json.Unmarshal(varsJSON, &n.TemplateVar)
		result = append(result, n)
	}
	if err := rows.Err(); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if result == nil {
		result = []notifRow{}
	}
	jsonOK(w, result)
}

type markReadReq struct {
	IDs []string `json:"ids"`
}

func (h *handler) markNotifRead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}

	var req markReadReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	if len(req.IDs) == 0 {
		jsonOK(w, map[string]int{"updated": 0})
		return
	}

	// Scoped to the caller's own notifications — without this, any
	// authenticated caller could mark another user's notification as read
	// by ID (guessed/enumerated/observed elsewhere).
	placeholders := make([]string, len(req.IDs))
	args := make([]any, len(req.IDs)+1)
	for i, id := range req.IDs {
		placeholders[i] = fmt.Sprintf("$%d::uuid", i+1)
		args[i] = id
	}
	args[len(req.IDs)] = a.UserID
	tag, err := h.db.Exec(ctx,
		fmt.Sprintf(`UPDATE notification.notification SET status = 'read'
		             WHERE id IN (%s) AND status != 'read' AND recipient_user_id = $%d::uuid`,
			strings.Join(placeholders, ","), len(req.IDs)+1),
		args...,
	)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]int64{"updated": tag.RowsAffected()})
}

// ── /api/grid ─────────────────────────────────────────────────────────────────

type deptRow struct {
	Code  string `json:"code"`
	Label string `json:"label"`
}

type gridDimMember struct {
	ID             string          `json:"id"`
	Code           string          `json:"code"`
	Label          string          `json:"label"`
	PeriodStart    *string         `json:"period_start,omitempty"` // time members only: YYYY-MM-DD
	PeriodEnd      *string         `json:"period_end,omitempty"`
	TimeIndex      *int            `json:"time_index,omitempty"`  // time members only: server-owned chronological ordinal
	ParentCode     string          `json:"parent_code,omitempty"` // parent member's code; empty = root node
	ParentMemberID string          `json:"-"`                     // parent member's ID (own dimension or, when the owning dimension declares parent_dimension_id, cross-dimension); internal only, drives writeguard.ExpandHidden — the frontend uses ParentCode instead
	Readonly       bool            `json:"readonly,omitempty"`    // true = "read" access rule applied
	Properties     json.RawMessage `json:"properties,omitempty"`  // arbitrary per-member key/values (e.g. {"region":"LUX"}); consumed by property-derived dimensions (source_property below)
}

type gridDimension struct {
	ID                string          `json:"id"`
	Name              string          `json:"name"`
	DimensionType     string          `json:"dimension_type"`                    // "standard" | "time"
	TimeGranularity   *string         `json:"time_granularity,omitempty"`        // time dimensions only
	FiscalYearStart   *int            `json:"fiscal_year_start_month,omitempty"` // time dimensions only
	DisplayLevel      *int            `json:"display_level"`                     // null=all, 0=roots, -1=leaves, n=depth n
	ParentDimensionID *string         `json:"parent_dimension_id"`
	SourceDimensionID *string         `json:"source_dimension_id,omitempty"` // set = this dimension's members are a grouping of SourceDimensionID's members by their properties[SourceProperty] value
	SourceProperty    *string         `json:"source_property,omitempty"`
	Members           []gridDimMember `json:"members"`
}

type gridAccessRules struct {
	DimMembers map[string]string `json:"dim_members"` // memberID → "write"|"read"|"hidden"
	Metrics    map[string]string `json:"metrics"`     // metricID → "write"|"read"|"hidden"
}

// filterHiddenMembers drops members whose dimRules entry is "hidden" and
// marks "read" ones Readonly. Applied to both a grid's own Dimensions and to
// AllDimensions, so a rollup grid's cross-dimension aggregation (which reads
// AllDimensions, not Dimensions) can never see a member the caller can't.
func filterHiddenMembers(members []gridDimMember, dimRules map[string]string) []gridDimMember {
	if len(dimRules) == 0 {
		return members
	}
	kept := members[:0]
	for _, m := range members {
		switch dimRules[m.ID] {
		case "hidden":
			// skip
		case "read":
			m.Readonly = true
			kept = append(kept, m)
		default:
			kept = append(kept, m)
		}
	}
	return kept
}

// hiddenCodesByDim converts member-ID-keyed dimRules ("hidden" entries only)
// into a dimensionID -> hidden-code-set map, scanning the full (unfiltered)
// dimension list. Must run before filterHiddenMembers removes the very rows
// this needs to read.
func hiddenCodesByDim(allDims []gridDimension, dimRules map[string]string) map[string]map[string]bool {
	if len(dimRules) == 0 {
		return nil
	}
	out := map[string]map[string]bool{}
	for _, d := range allDims {
		for _, m := range d.Members {
			if dimRules[m.ID] != "hidden" {
				continue
			}
			if out[d.ID] == nil {
				out[d.ID] = map[string]bool{}
			}
			out[d.ID][m.Code] = true
		}
	}
	return out
}

// factRowHidden reports whether a fact's dim_members map (dimensionID ->
// member code) touches any dimension member the caller has hidden access to.
func factRowHidden(dm map[string]string, hidden map[string]map[string]bool) bool {
	if len(hidden) == 0 {
		return false
	}
	for dimID, code := range dm {
		if hidden[dimID][code] {
			return true
		}
	}
	return false
}

// toRollupDims converts allDims (already hidden-member-filtered) into the
// map[string]*rollup.Dimension shape rollup.Resolve/rollup.LeafCombos need.
// Pure, in-memory — chart.go and calculation.Store each already load this
// shape directly from the DB; grid() builds its own richer []gridDimension
// shape for its JSON response regardless, so converting avoids a fourth
// near-duplicate query.
func toRollupDims(allDims []gridDimension) map[string]*rollup.Dimension {
	out := make(map[string]*rollup.Dimension, len(allDims))
	for _, d := range allDims {
		rd := &rollup.Dimension{ID: d.ID, IsTime: d.DimensionType == "time"}
		if d.TimeGranularity != nil {
			rd.TimeGranularity = *d.TimeGranularity
		}
		if d.FiscalYearStart != nil {
			rd.FiscalYearStartMonth = *d.FiscalYearStart
		}
		if d.ParentDimensionID != nil {
			rd.ParentDimensionID = *d.ParentDimensionID
		}
		if d.SourceDimensionID != nil {
			rd.SourceDimensionID = *d.SourceDimensionID
		}
		if d.SourceProperty != nil {
			rd.SourceProperty = *d.SourceProperty
		}
		rd.Members = make([]rollup.Member, 0, len(d.Members))
		for _, m := range d.Members {
			var props map[string]string
			if len(m.Properties) > 0 {
				_ = json.Unmarshal(m.Properties, &props)
			}
			rm := rollup.Member{ID: m.ID, Code: m.Code, ParentCode: m.ParentCode, Properties: props, TimeIndex: -1}
			if m.TimeIndex != nil {
				rm.TimeIndex = *m.TimeIndex
			}
			rd.Members = append(rd.Members, rm)
		}
		out[d.ID] = rd
	}
	return out
}

// scopedSeries is one time-series metric's persisted per-combo results plus
// what a scoped read needs to serve them safely: the time axis, the union
// window of every dependency (spec §3.4), the periods hidden from the
// caller, and the metric's time summary.
type scopedSeries struct {
	TimeDimID   string
	TimeSummary string
	Periods     []string           // every period code in chronological order (unscoped)
	Hidden      map[string]bool    // period codes hidden from this caller
	Rows        map[string]float64 // dimKey(combo) → persisted leaf value
	MinOffset   int
	MaxOffset   int
	UnbPast     bool
	UnbFuture   bool
}

// suppressed reports whether the cell at period t must be withheld because
// its source window reaches a hidden period.
func (ts *scopedSeries) suppressed(t int) bool {
	for i, code := range ts.Periods {
		if !ts.Hidden[code] {
			continue
		}
		if (ts.UnbPast && i < t) || (ts.UnbFuture && i > t) || (i >= t+ts.MinOffset && i <= t+ts.MaxOffset) {
			return true
		}
	}
	return false
}

// scoped returns the metric's cells (keyed code1:code2… in ownDims order)
// within the visible lattice — every leaf period, plus each aggregate
// period (H1, FY26) as its visible leaves reduced by the time summary — and
// its total: non-time dimensions combined by aggRule per period, periods
// combined by the time summary. ok=false when no total can be derived
// (formula/rate rules need the scheduler's own rollup rows; a scoped read
// has none).
func (ts *scopedSeries) scoped(rollupDims map[string]*rollup.Dimension, ownDims []string, aggRule string) (map[string]float64, float64, bool) {
	pos := make(map[string]int, len(ts.Periods))
	for i, c := range ts.Periods {
		pos[c] = i
	}
	cells := map[string]float64{}
	perPeriod := map[int][]float64{}
	timePos := -1
	nonTime := make([]string, 0, len(ownDims))
	for i, dimID := range ownDims {
		if dimID == ts.TimeDimID {
			timePos = i
		} else {
			nonTime = append(nonTime, dimID)
		}
	}
	keyOf := func(combo map[string]string) string {
		codes := make([]string, 0, len(ownDims))
		for _, dimID := range ownDims {
			codes = append(codes, combo[dimID])
		}
		return strings.Join(codes, ":")
	}
	// leafValue: the persisted value at combo, unless its window is hidden.
	leafValue := func(combo map[string]string) (float64, bool) {
		t, known := pos[combo[ts.TimeDimID]]
		if !known || ts.suppressed(t) {
			return 0, false
		}
		b, _ := json.Marshal(combo)
		v, has := ts.Rows[string(b)]
		return v, has
	}
	for _, combo := range rollup.LeafCombos(rollupDims, ownDims) {
		v, ok := leafValue(combo)
		if !ok {
			continue
		}
		cells[keyOf(combo)] = v
		perPeriod[pos[combo[ts.TimeDimID]]] = append(perPeriod[pos[combo[ts.TimeDimID]]], v)
	}
	// Aggregate periods: per non-time leaf group, the visible leaves beneath
	// the aggregate in chronological order; withheld if any is hidden.
	if axis := rollupDims[ts.TimeDimID]; axis != nil && timePos >= 0 && ts.TimeSummary != "none" &&
		aggRule != string(rollup.AggFormula) && aggRule != string(rollup.AggRate) {
		groups := rollup.LeafCombos(rollupDims, nonTime)
		if len(groups) == 0 {
			groups = []map[string]string{{}}
		}
		for _, m := range axis.Members {
			if m.IsLeafPeriod() {
				continue
			}
			var leaves []string
			for code := range subtreeCodesOf(axis, m.Code) {
				if _, ok := pos[code]; ok {
					leaves = append(leaves, code)
				}
			}
			sort.Slice(leaves, func(i, j int) bool { return pos[leaves[i]] < pos[leaves[j]] })
			for _, g := range groups {
				vals := make([]float64, 0, len(leaves))
				complete := true
				for _, code := range leaves {
					combo := make(map[string]string, len(g)+1)
					for k, v := range g {
						combo[k] = v
					}
					combo[ts.TimeDimID] = code
					v, ok := leafValue(combo)
					if !ok {
						complete = false
						break
					}
					vals = append(vals, v)
				}
				if !complete || len(vals) == 0 {
					continue
				}
				if v, ok := calculation.TimeSummary(ts.TimeSummary, vals); ok {
					combo := make(map[string]string, len(g)+1)
					for k, v := range g {
						combo[k] = v
					}
					combo[ts.TimeDimID] = m.Code
					cells[keyOf(combo)] = v
				}
			}
		}
	}
	if aggRule == string(rollup.AggFormula) || aggRule == string(rollup.AggRate) || len(perPeriod) == 0 || ts.TimeSummary == "none" {
		return cells, 0, false
	}
	var ordered []float64
	for i := range ts.Periods {
		if vals, ok := perPeriod[i]; ok {
			ordered = append(ordered, rollup.CombineAgg(vals, rollup.AggRule(aggRule)))
		}
	}
	total, ok := calculation.TimeSummary(ts.TimeSummary, ordered)
	return cells, total, ok
}

// subtreeCodesOf returns code and every descendant code within dim.
func subtreeCodesOf(dim *rollup.Dimension, code string) map[string]bool {
	out := map[string]bool{code: true}
	childrenOf := map[string][]string{}
	for _, m := range dim.Members {
		if m.ParentCode != "" {
			childrenOf[m.ParentCode] = append(childrenOf[m.ParentCode], m.Code)
		}
	}
	queue := []string{code}
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		for _, child := range childrenOf[c] {
			if !out[child] {
				out[child] = true
				queue = append(queue, child)
			}
		}
	}
	return out
}

// scopeCalcCells recomputes every calculated metric's value at each of its
// own leaf-member combos (and, from those, its scoped total) from
// already-scoped per-combo INPUT cells — replacing the old scopeCalcTotals
// entirely, not just extending it to a finer grain. This is necessary, not
// a stylistic choice: runtime.calc_result rows are written by the
// calculation scheduler with no per-user access concept at all, so a calc
// metric declared at a COARSER grain than one of its own dependencies can
// silently bake in a finer-grained hidden dependency's contribution even
// though the row's own dim_members never touches the hidden member
// directly — e.g. total_comp at [department] depending on bonus at
// [staff]: hiding one specific staff member (not the whole department)
// would slip straight through a naive per-row dim_members filter, since
// the row is keyed only by department. Recomputing per combo via
// rollup.Resolve against already-scoped inputs (exactly like the scheduler
// itself does at write time, just against a visibility-scoped input set)
// closes that leak.
//
// This also fixes a latent divergence for non-linear formulas: the old
// scopeCalcTotals evaluated a formula once against a SCALAR total, but the
// authoritative unscoped calc_result aggregate is always "evaluate per
// combo, then combine" (see executePartition). These disagreed whenever a
// formula like IF(revenue>threshold,...) only crosses the threshold in
// aggregate, not per combo. Deriving the scoped total via rollup.CombineAgg
// over this function's own per-combo results keeps both totals using the
// same aggregation model.
//
// A metric that can't be fully resolved (a dependency outside the visible
// universe, or an evaluation error) is omitted from both return values
// rather than guessed — fail closed, matching scopeCalcTotals's own
// existing convention.
func scopeCalcCells(
	ctx context.Context,
	rollupDims map[string]*rollup.Dimension,
	metricDimIDs map[string][]string,
	dimIDToName map[string]string,
	universe []metricRow, // allMetrics
	scopedInputCells map[string]float64, // this request's already hidden-member-scoped `cells`, input-only at this point
	series map[string]*scopedSeries, // time-series metrics: served from persisted rows, never re-evaluated (nil = none)
) (cells map[string]float64, totals map[string]float64) {
	byName := make(map[string]metricRow, len(universe))
	for _, m := range universe {
		byName[m.Name] = m
	}

	// working mirrors `cells`' own composite-key convention
	// (metricID:code1:code2...) so a calc metric resolved earlier in this
	// pass is immediately visible to a calc metric that depends on it,
	// through the exact same fetch path used for inputs.
	working := make(map[string]float64, len(scopedInputCells))
	for k, v := range scopedInputCells {
		working[k] = v
	}
	fetchFor := func(metricID string) rollup.RawValue {
		ownDims := metricDimIDs[metricID]
		return func(_ context.Context, _ string, combo map[string]string) (float64, bool, error) {
			key := metricID
			if len(ownDims) > 0 {
				codes := make([]string, 0, len(ownDims))
				for _, dimID := range ownDims {
					code, ok := combo[dimID]
					if !ok {
						return 0, false, nil
					}
					codes = append(codes, code)
				}
				key = metricID + ":" + strings.Join(codes, ":")
			}
			v, ok := working[key]
			return v, ok, nil
		}
	}

	cells = make(map[string]float64)
	totals = make(map[string]float64)

	var remaining []metricRow
	for _, m := range universe {
		if !m.IsInput && m.Formula != nil && *m.Formula != "" {
			remaining = append(remaining, m)
		}
	}
	for pass := 0; pass < len(remaining)+1 && len(remaining) > 0; pass++ {
		var unresolved []metricRow
		for _, m := range remaining {
			if ts := series[m.ID]; ts != nil {
				// A time-series metric is never re-evaluated here: its value
				// at a period depends on OTHER periods, which a scoped
				// (trimmed) lattice cannot reproduce — a pinned month would
				// make LAG see an empty past and answer a different number
				// than the grid everyone else reads. Serve the scheduler's
				// persisted leaf rows within the visible lattice, and
				// suppress any cell whose source window touches a period
				// this caller may not see (spec §10): omitting the hidden
				// source from the arithmetic would change the model.
				tsCells, total, ok := ts.scoped(rollupDims, metricDimIDs[m.ID], m.AggRule)
				for k, v := range tsCells {
					key := m.ID + ":" + k
					cells[key] = v
					working[key] = v
				}
				if ok {
					totals[m.ID] = total
					working[m.ID] = total
				}
				continue
			}
			refs, err := formula.ExtractRefs(*m.Formula)
			ready := err == nil
			if ready {
				for _, ref := range refs {
					refM, isMetric := byName[ref]
					if !isMetric || refM.IsInput {
						continue // not a metric reference, or an input (already fully available)
					}
					if _, done := totals[refM.ID]; !done {
						ready = false
						break
					}
				}
			}
			if !ready {
				unresolved = append(unresolved, m)
				continue
			}

			// Once a metric is structurally ready (every calc dependency it
			// references already fully resolved), each of ITS OWN combos is
			// evaluated independently — a runtime error on one combo (e.g. a
			// #DIV/0! from a dependency that's genuinely unentered for that
			// specific combo, not hidden) skips only that combo, exactly
			// like executePartition's own per-combo evaluation loop. An
			// all-or-nothing "one bad combo aborts the whole metric" design
			// was tried and rejected: it silently produced ZERO cells for
			// budget_variance_pct against real seed data, because most of
			// its combos' budget_target dependency was simply never entered
			// for months beyond the ones actually seeded — a real, normal
			// case, not a structural resolution failure.
			ownDims := metricDimIDs[m.ID]
			combos := rollup.LeafCombos(rollupDims, ownDims)
			if len(combos) == 0 {
				combos = []map[string]string{{}}
			}
			// Mirror executePartition's own collapse for a pure ratio
			// metric (agg_rule "average" whose formula isn't itself
			// dimension-conditional): a ratio-of-sums computed once, not
			// an average of per-combo ratios — otherwise a hidden-member-
			// restricted caller sees a numerically different value than
			// the unrestricted calc_result everyone else reads, since
			// average-of-ratios != ratio-of-sums in general.
			if m.AggRule == "average" && len(combos) > 1 && !calculation.FormulaReferencesDims(*m.Formula, dimIDToName) {
				combos = []map[string]string{{}}
			}
			evalCombo := func(combo map[string]string) (float64, bool) {
				values := make(map[string]float64, len(refs))
				for _, ref := range refs {
					refM, isMetric := byName[ref]
					if !isMetric {
						continue // function name, not a metric reference
					}
					v, _, resolveErr := rollup.Resolve(ctx, rollupDims, refM.ID, metricDimIDs[refM.ID], rollup.AggRule(refM.AggRule), combo, fetchFor(refM.ID))
					if resolveErr != nil {
						return 0, false
					}
					values[refM.Name] = v
				}
				namedDims := make(map[string]string, len(combo))
				for dimID, code := range combo {
					if name, found := dimIDToName[dimID]; found {
						namedDims[name] = code
					}
				}
				v, evalErr := calculation.EvaluateWithDims(*m.Formula, values, namedDims)
				if evalErr != nil {
					return 0, false // genuine per-combo error (e.g. #DIV/0!) — skip just this combo
				}
				return v, true
			}
			vals := make([]float64, 0, len(combos))
			for _, combo := range combos {
				v, ok := evalCombo(combo)
				if !ok {
					continue
				}
				vals = append(vals, v)
				if len(combo) > 0 {
					codes := make([]string, 0, len(ownDims))
					for _, dimID := range ownDims {
						codes = append(codes, combo[dimID])
					}
					key := m.ID + ":" + strings.Join(codes, ":")
					working[key] = v
					cells[key] = v
				} else {
					working[m.ID] = v
				}
			}
			totals[m.ID] = rollup.CombineAgg(vals, rollup.AggRule(m.AggRule))

			// Rollup cells for rules the client cannot combine — the scoped
			// twin of the scheduler's own rollup-row persistence: evaluated
			// from the SAME hidden-member-scoped inputs as everything above,
			// so a restricted user's World row aggregates exactly the members
			// they may see. Kept out of `vals` — a formula/rate total is
			// overridden (or served) separately, never combined from rollups.
			if m.AggRule == string(rollup.AggFormula) || m.AggRule == string(rollup.AggRate) {
				const rollupComboCap = 20000
				for _, combo := range rollup.RollupCombos(rollupDims, ownDims, rollupComboCap) {
					v, ok := evalCombo(combo)
					if !ok {
						continue // renders "—", the display contract's safe state
					}
					codes := make([]string, 0, len(ownDims))
					for _, dimID := range ownDims {
						codes = append(codes, combo[dimID])
					}
					cells[m.ID+":"+strings.Join(codes, ":")] = v
				}
			}

			// agg_rule 'formula': the total is the formula evaluated once
			// against fully-aggregated inputs, not a combination of the
			// per-combo results computed above. Mirrors executePartition for
			// the same reason the 'average' collapse above does — this path
			// serves hidden-member-restricted callers, and if it combined
			// while the scheduler evaluated, the two would report different
			// numbers for the same metric.
			//
			// 'rate' takes the same override: a calc rate metric's formula IS
			// its ratio, so evaluating it against fully-aggregated (scoped)
			// inputs yields numerator_total/denominator_total — true Anaplan
			// Ratio semantics, matching both the scheduler's own '{}' row
			// (rateTotal) and its persisted slice rows. The CombineAgg value
			// it replaces was a MEAN of per-combo ratios — an explicitly
			// documented approximation, and a number the precomputed slice
			// fast path would disagree with.
			if m.AggRule == string(rollup.AggFormula) || m.AggRule == string(rollup.AggRate) {
				totalValues := make(map[string]float64, len(refs))
				totalOK := true
				for _, ref := range refs {
					refM, isMetric := byName[ref]
					if !isMetric {
						continue
					}
					v, _, resolveErr := rollup.Resolve(ctx, rollupDims, refM.ID, metricDimIDs[refM.ID], rollup.AggRule(refM.AggRule), map[string]string{}, fetchFor(refM.ID))
					if resolveErr != nil {
						totalOK = false
						break
					}
					totalValues[refM.Name] = v
				}
				if totalOK {
					if v, evalErr := calculation.EvaluateWithDims(*m.Formula, totalValues, map[string]string{}); evalErr == nil {
						totals[m.ID] = v
						working[m.ID] = v
					}
				}
			}
		}
		remaining = unresolved
	}
	return cells, totals
}

type gridResponse struct {
	RevisionID         string             `json:"revision_id"`
	Metrics            []metricRow        `json:"metrics"`
	AllMetrics         []metricRow        `json:"all_metrics"`    // every metric in the revision, regardless of grid membership — lets formulas reference metrics outside this grid
	Dimensions         []gridDimension    `json:"dimensions"`     // all configured dims, ordered
	AllDimensions      []gridDimension    `json:"all_dimensions"` // every dimension in the revision, regardless of grid — lets cross-dimension formula refs resolve hierarchy/members outside this grid
	Departments        []deptRow          `json:"departments"`    // = first dim members (compat)
	Cells              map[string]float64 `json:"cells"`          // "metricId:code1[:code2...]" composite key, keyed per each metric's OWN grid dims
	Totals             map[string]float64 `json:"totals"`         // "metricId" -> aggregate
	AccessRules        gridAccessRules    `json:"access_rules"`
	RollupSourceGridID *string            `json:"rollup_source_grid_id,omitempty"` // set = this grid mirrors another grid's metrics via cross-dimension rollup; its cells are read-only
}

func (h *handler) grid(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	gridDefID := r.URL.Query().Get("grid_def_id")

	// ── resolve model ID ─────────────────────────────────────────────────────
	var modelID string
	if gridDefID != "" {
		if err := h.db.QueryRow(ctx,
			`SELECT model_id::text FROM model.grid_def WHERE id=$1::uuid`, gridDefID,
		).Scan(&modelID); err != nil {
			jsonErr(w, fmt.Errorf("resolve grid def: %w", err), http.StatusInternalServerError)
			return
		}
	} else {
		var mErr error
		modelID, mErr = h.resolveDemoModelID(ctx, r)
		if mErr != nil {
			jsonAccessErr(w, mErr, "resolve model")
			return
		}
	}
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	canAccessModel, err := h.actorCanAccessModel(ctx, act, modelID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !canAccessModel {
		jsonErr(w, fmt.Errorf("forbidden: model is outside your access scope"), http.StatusForbidden)
		return
	}

	// ── resolve revision ─────────────────────────────────────────────────────
	revisionID, _, err := h.resolveRevisionCtx(ctx, r.URL.Query().Get("revision_id"), modelID)
	if rejectForeignRevision(w, err) {
		return
	}
	if err != nil {
		jsonErr(w, fmt.Errorf("no revision: %w", err), http.StatusInternalServerError)
		return
	}

	// ── redirect stale grid_def to viewer's revision ──────────────────────────
	// Dashboard widgets store a fixed grid_def_id that may have been created for
	// an older revision. When the viewer's revision differs, find the same-named
	// grid_def in the viewer's revision so they always see the current config.
	if gridDefID != "" {
		var gridRevision string
		_ = h.db.QueryRow(ctx,
			`SELECT COALESCE(revision_id::text,'') FROM model.grid_def WHERE id=$1::uuid`, gridDefID,
		).Scan(&gridRevision)
		if gridRevision != "" && gridRevision != revisionID {
			var gridName string
			if err2 := h.db.QueryRow(ctx,
				`SELECT name FROM model.grid_def WHERE id=$1::uuid`, gridDefID,
			).Scan(&gridName); err2 == nil {
				var altID string
				if err3 := h.db.QueryRow(ctx,
					`SELECT id::text FROM model.grid_def WHERE model_id=$1::uuid AND name=$2 AND revision_id=$3::uuid`,
					modelID, gridName, revisionID,
				).Scan(&altID); err3 == nil {
					gridDefID = altID
				}
			}
		}
	}

	// ── all dimensions (ordered) with their members ──────────────────────────
	var dimQuery string
	var dimParam interface{}
	if gridDefID != "" {
		dimQuery = `
			SELECT d.id::text, d.name, gd.display_level, d.parent_dimension_id::text,
			       d.source_dimension_id::text, d.source_property,
			       d.dimension_type, d.time_granularity, d.fiscal_year_start_month,
			       m.id::text, m.code, m.label, m.properties,
			       COALESCE(pm.code, '') AS parent_code, COALESCE(pm.id::text, '') AS parent_member_id,
			       m.period_start::text, m.period_end::text, m.time_index
			FROM model.grid_dimension gd
			JOIN model.dimension_def d ON d.id = gd.dimension_id
			JOIN model.dimension_member m ON m.dimension_id = d.id
			LEFT JOIN model.dimension_member pm ON pm.id = m.parent_member_id
			WHERE gd.grid_id = $1::uuid
			ORDER BY d.name, m.time_index NULLS LAST, m.sort_order, m.code`
		dimParam = gridDefID
	} else {
		// Revision scoping is not optional here: without it every revision's
		// copy of every dimension shipped in one response — 27 entries with
		// five same-named geographies — and the hidden-member filter only
		// touches the ACTIVE revision's rows, so a user restricted to Canada
		// was served the stale copies' UK/DE/US members intact (found live
		// via a restricted user's own payload). NULL revision_id rows are
		// pre-revision legacy and stay visible.
		dimQuery = `
			SELECT d.id::text, d.name, NULL::int AS display_level, d.parent_dimension_id::text,
			       d.source_dimension_id::text, d.source_property,
			       d.dimension_type, d.time_granularity, d.fiscal_year_start_month,
			       m.id::text, m.code, m.label, m.properties,
			       COALESCE(pm.code, '') AS parent_code, COALESCE(pm.id::text, '') AS parent_member_id,
			       m.period_start::text, m.period_end::text, m.time_index
			FROM model.dimension_def d
			JOIN model.dimension_member m ON m.dimension_id = d.id
			LEFT JOIN model.dimension_member pm ON pm.id = m.parent_member_id
			WHERE d.model_id = $1::uuid
			  AND (d.revision_id IS NULL OR d.revision_id::text = $2)
			ORDER BY (d.name='department') DESC, d.name, m.time_index NULLS LAST, m.sort_order, m.code`
		dimParam = modelID
	}
	dimArgs := []any{dimParam}
	if gridDefID == "" {
		dimArgs = append(dimArgs, revisionID)
	}
	dimRows, err := h.db.Query(ctx, dimQuery, dimArgs...)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	dimMap := map[string]*gridDimension{}
	dimOrder := []string{}
	for dimRows.Next() {
		var dimID, dimName, memberID, code, label, parentCode, parentMemberID, dimType string
		var displayLevel, fiscalStart, timeIndex *int
		var parentDimensionID, sourceDimensionID, sourceProperty, granularity, periodStart, periodEnd *string
		var properties json.RawMessage
		if err := dimRows.Scan(&dimID, &dimName, &displayLevel, &parentDimensionID, &sourceDimensionID, &sourceProperty,
			&dimType, &granularity, &fiscalStart,
			&memberID, &code, &label, &properties, &parentCode, &parentMemberID, &periodStart, &periodEnd, &timeIndex); err != nil {
			dimRows.Close()
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if _, ok := dimMap[dimID]; !ok {
			dimMap[dimID] = &gridDimension{ID: dimID, Name: dimName, DisplayLevel: displayLevel, ParentDimensionID: parentDimensionID, SourceDimensionID: sourceDimensionID, SourceProperty: sourceProperty,
				DimensionType: dimType, TimeGranularity: granularity, FiscalYearStart: fiscalStart}
			dimOrder = append(dimOrder, dimID)
		}
		dimMap[dimID].Members = append(dimMap[dimID].Members, gridDimMember{
			ID: memberID, Code: code, Label: label, ParentCode: parentCode, ParentMemberID: parentMemberID, Properties: properties,
			PeriodStart: periodStart, PeriodEnd: periodEnd, TimeIndex: timeIndex,
		})
	}
	dimRows.Close()
	if err := dimRows.Err(); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	dims := make([]gridDimension, 0, len(dimOrder))
	for _, id := range dimOrder {
		dims = append(dims, *dimMap[id])
	}

	// ── all_dimensions: every dimension in the revision, regardless of grid ──
	// Needed so cross-dimension formula references (e.g. a Department metric
	// referencing a Cabinet metric) can resolve the referenced dimension's
	// members and parent_dimension_id even when it isn't configured on this grid.
	var allDims []gridDimension
	if gridDefID != "" {
		allDimRows, adErr := h.db.Query(ctx, `
			SELECT d.id::text, d.name, d.parent_dimension_id::text,
			       d.source_dimension_id::text, d.source_property,
			       d.dimension_type, d.time_granularity, d.fiscal_year_start_month,
			       m.id::text, m.code, m.label, m.properties,
			       COALESCE(pm.code, '') AS parent_code, COALESCE(pm.id::text, '') AS parent_member_id,
			       m.period_start::text, m.period_end::text, m.time_index
			FROM model.dimension_def d
			JOIN model.dimension_member m ON m.dimension_id = d.id
			LEFT JOIN model.dimension_member pm ON pm.id = m.parent_member_id
			WHERE d.model_id = $1::uuid AND (d.revision_id = $2::uuid OR d.revision_id IS NULL)
			ORDER BY d.name, m.time_index NULLS LAST, m.sort_order, m.code
		`, modelID, revisionID)
		if adErr != nil {
			jsonErr(w, adErr, http.StatusInternalServerError)
			return
		}
		allDimMap := map[string]*gridDimension{}
		allDimOrder := []string{}
		for allDimRows.Next() {
			var dimID, dimName, memberID, code, label, parentCode, parentMemberID, dimType string
			var parentDimensionID, sourceDimensionID, sourceProperty, granularity, periodStart, periodEnd *string
			var fiscalStart, timeIndex *int
			var properties json.RawMessage
			if err := allDimRows.Scan(&dimID, &dimName, &parentDimensionID, &sourceDimensionID, &sourceProperty,
				&dimType, &granularity, &fiscalStart,
				&memberID, &code, &label, &properties, &parentCode, &parentMemberID, &periodStart, &periodEnd, &timeIndex); err != nil {
				allDimRows.Close()
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			if _, ok := allDimMap[dimID]; !ok {
				allDimMap[dimID] = &gridDimension{ID: dimID, Name: dimName, ParentDimensionID: parentDimensionID, SourceDimensionID: sourceDimensionID, SourceProperty: sourceProperty,
					DimensionType: dimType, TimeGranularity: granularity, FiscalYearStart: fiscalStart}
				allDimOrder = append(allDimOrder, dimID)
			}
			allDimMap[dimID].Members = append(allDimMap[dimID].Members, gridDimMember{
				ID: memberID, Code: code, Label: label, ParentCode: parentCode, ParentMemberID: parentMemberID, Properties: properties,
				PeriodStart: periodStart, PeriodEnd: periodEnd, TimeIndex: timeIndex,
			})
		}
		allDimRows.Close()
		if err := allDimRows.Err(); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		allDims = make([]gridDimension, 0, len(allDimOrder))
		for _, id := range allDimOrder {
			allDims = append(allDims, *allDimMap[id])
		}
	} else {
		allDims = dims
	}

	// ── access rules for the requesting user ─────────────────────────────────
	// Resolved once (act, above) and applied to every representation of the
	// data below — dims/allDims member lists, cells, totals — so a caller
	// with a hidden dimension_member rule can never observe that member or
	// its facts through any path (direct grid, rollup grid, or a metric
	// total), matching the write-side check cells() already applies.
	dimRules := map[string]string{}
	metricRules := map[string]string{}
	{
		arRows, arErr := h.db.Query(ctx,
			`SELECT rule_type, ref_id, access FROM identity.user_access_rule WHERE user_id=$1::uuid`,
			act.UserID)
		if arErr == nil {
			for arRows.Next() {
				var ruleType, refID, access string
				if arRows.Scan(&ruleType, &refID, &access) == nil {
					switch ruleType {
					case "dimension_member":
						dimRules[refID] = access
					case "metric":
						metricRules[refID] = access
					}
				}
			}
			arRows.Close()
		}
	}
	// Cascade hidden-member rules down the dimension hierarchy: a rule set
	// directly on a cost_centers member must also hide every employees
	// member that rolls up to it (walking parent_member_id, which crosses
	// dimensions in one hop exactly like writeguard.AncestorChain), not just
	// the exact member_id the rule names. allDims — not dims — is the source
	// here because it's the full, unfiltered member universe regardless of
	// what this specific grid is dimensioned by. Expands dimRules in place
	// (most-restrictive-wins: a cascaded "hidden" overrides a member's own
	// direct "read") so hiddenCodesByDim/filterHiddenMembers below, and the
	// access_rules echoed in the response, need no changes of their own.
	{
		edges := make([]writeguard.MemberEdge, 0, len(allDims)*4)
		for _, d := range allDims {
			for _, m := range d.Members {
				edges = append(edges, writeguard.MemberEdge{ID: m.ID, ParentID: m.ParentMemberID, DimID: d.ID})
			}
		}
		for id := range writeguard.ExpandHidden(edges, dimRules) {
			dimRules[id] = "hidden"
		}
	}
	// ── context scoping ────────────────────────────────────────────────────
	// Optional `scope` param (URL-encoded JSON {dimensionID: memberCode}) is
	// the caller's pinned context — the dashboard's fixed selectors. When
	// present, the cell/calc queries below return ONLY the slice at that
	// context instead of the whole model, so a 500-product grid pinned to a
	// period+geography returns ~its row dimension's worth of cells rather
	// than the full 121k-cell cross-product (the whole-model read was ~9s;
	// scoped is sub-second).
	//
	// A pinned member is expanded to its SUBTREE codes, not matched exactly:
	// pinning a rollup node (period=FY26) must still return the leaf facts
	// (Q1..Q4) so input-metric rollups compute, plus that node's own calc
	// rollup rows. Built from the member tree already loaded above.
	var scopeConds []string
	var scopeArgs []any
	var scopePinned map[string]string            // dimID -> pinned code, for context-total lookup below
	var scopeSubtrees map[string]map[string]bool // dimID -> pinned member's subtree codes
	if rawScope := r.URL.Query().Get("scope"); rawScope != "" {
		var pinned map[string]string
		if json.Unmarshal([]byte(rawScope), &pinned) == nil && len(pinned) > 0 {
			scopePinned = map[string]string{}
			// children map per dim: parentCode -> []childCode, from allDims.
			for _, d := range allDims {
				code, ok := pinned[d.ID]
				if !ok || code == "" {
					continue
				}
				childrenOf := map[string][]string{}
				for _, m := range d.Members {
					if m.ParentCode != "" {
						childrenOf[m.ParentCode] = append(childrenOf[m.ParentCode], m.Code)
					}
				}
				// BFS the subtree rooted at the pinned code.
				subtree := []string{}
				seen := map[string]bool{}
				queue := []string{code}
				for len(queue) > 0 {
					c := queue[0]
					queue = queue[1:]
					if seen[c] {
						continue
					}
					seen[c] = true
					subtree = append(subtree, c)
					queue = append(queue, childrenOf[c]...)
				}
				// A pin whose subtree covers EVERY member (the dimension's sole
				// universal root, e.g. WORLD / ALL_PROD) constrains nothing —
				// drop it. This collapses a dashboard's "one selector drilled,
				// the rest on All" into the minimal real scope: all-roots →
				// unscoped fast path, one drill → a one-dimension slice. A dim
				// with several top-level members (DEPT_A, DEPT_B — each its own
				// root) never triggers this: one member's subtree ≠ all.
				if len(subtree) == len(d.Members) {
					continue
				}
				scopePinned[d.ID] = code
				if scopeSubtrees == nil {
					scopeSubtrees = map[string]map[string]bool{}
				}
				scopeSubtrees[d.ID] = seen
				// A pin reaches DOWN the cross-dimension hierarchy too: the
				// facts of a metric dimensioned by department, whose members
				// roll up into region (parent_dimension_id), carry no region
				// key at all — pinning region=APAC has to admit the facts of
				// the departments under APAC, or a regional total computes
				// from nothing (found live: "Regional Total Cost $0" on the
				// Regional Expense Planning demo). The same rule
				// rollup.descendantsInChain applies: level by level, a child
				// dimension's members whose parent code is in the level
				// above. Each dimension reached gets its own alternative in
				// the fact filter and its own subtree for the combo trim.
				alternatives := []string{}
				addAlt := func(dimID string, codes []string) {
					scopeArgs = append(scopeArgs, dimID, codes)
					n := len(scopeArgs)
					// $2+n-1 = dimID, $2+n = codes (args after modelID=$1,
					// revisionID=$2; scope args begin at $3).
					alternatives = append(alternatives,
						fmt.Sprintf("(dim_members ? $%d AND dim_members->>$%d = ANY($%d))", 2+n-1, 2+n-1, 2+n))
				}
				addAlt(d.ID, subtree)
				frontier := map[string]map[string]bool{d.ID: seen}
				for len(frontier) > 0 {
					next := map[string]map[string]bool{}
					for _, child := range allDims {
						if child.ParentDimensionID == nil {
							continue
						}
						parentCodes, ok := frontier[*child.ParentDimensionID]
						if !ok {
							continue
						}
						codes := map[string]bool{}
						list := []string{}
						for _, m := range child.Members {
							if m.ParentCode != "" && parentCodes[m.ParentCode] {
								codes[m.Code] = true
								list = append(list, m.Code)
							}
						}
						if _, done := scopeSubtrees[child.ID]; done {
							continue // a dimension is reached once (the dimension graph is a tree)
						}
						scopeSubtrees[child.ID] = codes
						addAlt(child.ID, list)
						next[child.ID] = codes
					}
					frontier = next
				}
				scopeConds = append(scopeConds, "("+strings.Join(alternatives, " OR ")+")")
			}
		}
	}
	scopeSQL := ""
	if len(scopeConds) > 0 {
		scopeSQL = " AND " + strings.Join(scopeConds, " AND ")
	}

	// Must run before filterHiddenMembers below removes the rows it reads.
	hiddenByDim := hiddenCodesByDim(allDims, dimRules)
	for i := range dims {
		dims[i].Members = filterHiddenMembers(dims[i].Members, dimRules)
	}
	if gridDefID != "" { // else allDims aliases dims, already filtered above
		for i := range allDims {
			allDims[i].Members = filterHiddenMembers(allDims[i].Members, dimRules)
		}
	}

	// backward compat: departments = first dim members (leaf members only)
	var depts []deptRow
	if len(dims) > 0 {
		for _, m := range dims[0].Members {
			if m.ParentCode == "" { // include root nodes (leaves + top-level agg nodes)
				depts = append(depts, deptRow{Code: m.Code, Label: m.Label})
			}
		}
		if len(depts) == 0 { // fallback: include all
			for _, m := range dims[0].Members {
				depts = append(depts, deptRow{Code: m.Code, Label: m.Label})
			}
		}
	}

	// ── metrics ──────────────────────────────────────────────────────────────
	// A grid with no grid_metric rows of its own can mirror another grid's
	// metrics (rollup_source_grid_id), rendering them via cross-dimension
	// rollup against its own dims instead of direct fact lookups — see
	// resolveCrossDimensionValue in BusinessConsole.tsx. Only the metrics
	// *list* comes from the source grid; dims/cells/access rules below all
	// stay scoped to the grid actually requested.
	metricsGridID := gridDefID
	var rollupSourceGridID *string
	if gridDefID != "" {
		if err := h.db.QueryRow(ctx,
			`SELECT rollup_source_grid_id::text FROM model.grid_def WHERE id=$1::uuid`, gridDefID,
		).Scan(&rollupSourceGridID); err == nil && rollupSourceGridID != nil {
			metricsGridID = *rollupSourceGridID
		}
	}

	var metrics []metricRow
	var metricRows2 interface {
		Next() bool
		Scan(...any) error
		Close()
		Err() error
	}
	if gridDefID != "" {
		// Join via metric name so the IDs returned belong to the requested revision,
		// not the revision the grid was originally built against.
		metricRows2, err = h.db.Query(ctx, `
			SELECT rev.id::text, rev.name, rev.is_input, rev.formula, rev.agg_rule,
			       rev.format, rev.format_decimals, rev.format_currency, rev.time_summary
			FROM model.grid_metric gm
			JOIN model.metric_def orig ON orig.id = gm.metric_id
			JOIN model.metric_def rev
			     ON rev.model_id = orig.model_id
			    AND rev.name     = orig.name
			    AND rev.revision_id = $2::uuid
			WHERE gm.grid_id = $1::uuid
			ORDER BY gm.sort_order, rev.is_input DESC, rev.name
		`, metricsGridID, revisionID)
	} else {
		metricRows2, err = h.db.Query(ctx, `
			SELECT id::text, name, is_input, formula, agg_rule,
			       format, format_decimals, format_currency, time_summary
			FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid
			ORDER BY is_input DESC, name
		`, modelID, revisionID)
	}
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	for metricRows2.Next() {
		var mr metricRow
		if err := metricRows2.Scan(&mr.ID, &mr.Name, &mr.IsInput, &mr.Formula, &mr.AggRule, &mr.Format, &mr.FormatDecimals, &mr.FormatCurrency, &mr.TimeSummary); err != nil {
			metricRows2.Close()
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		mr.Label = toLabel(mr.Name)
		metrics = append(metrics, mr)
	}
	metricRows2.Close()
	if err := metricRows2.Err(); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}

	// Each metric's OWN grid's dimension IDs (ordered like `dims` above), so the
	// frontend can build that metric's native cell key instead of assuming the
	// current grid's dims — this is what makes cross-grid/cross-dimension
	// formula references resolvable rather than falling back to a broadcast total.
	// Computed unconditionally: the `cells` map below is keyed per-metric using
	// this, regardless of whether the current request itself targets a specific grid.
	metricDims := map[string][]string{}
	mdRows, mdErr := h.db.Query(ctx, `
		SELECT gm.metric_id::text, gd.dimension_id::text
		FROM model.grid_metric gm
		JOIN model.grid_dimension gd ON gd.grid_id = gm.grid_id
		JOIN model.dimension_def d ON d.id = gd.dimension_id
		JOIN model.metric_def m ON m.id = gm.metric_id
		WHERE m.model_id=$1::uuid AND m.revision_id=$2::uuid
		ORDER BY gm.metric_id, d.name
	`, modelID, revisionID)
	if mdErr != nil {
		jsonErr(w, mdErr, http.StatusInternalServerError)
		return
	}
	for mdRows.Next() {
		var metricID, dimID string
		if err := mdRows.Scan(&metricID, &dimID); err != nil {
			mdRows.Close()
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		metricDims[metricID] = append(metricDims[metricID], dimID)
	}
	mdRows.Close()
	if err := mdRows.Err(); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}

	// ── all_metrics: every metric in the revision, regardless of grid ────────
	// Calculated metrics may reference metrics assigned to a different grid;
	// the frontend formula evaluator needs the full universe to resolve them.
	var allMetrics []metricRow
	if gridDefID != "" {
		allRows, aErr := h.db.Query(ctx, `
			SELECT id::text, name, is_input, formula, agg_rule,
			       format, format_decimals, format_currency, time_summary
			FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$2::uuid
			ORDER BY is_input DESC, name
		`, modelID, revisionID)
		if aErr != nil {
			jsonErr(w, aErr, http.StatusInternalServerError)
			return
		}
		for allRows.Next() {
			var mr metricRow
			if err := allRows.Scan(&mr.ID, &mr.Name, &mr.IsInput, &mr.Formula, &mr.AggRule, &mr.Format, &mr.FormatDecimals, &mr.FormatCurrency, &mr.TimeSummary); err != nil {
				allRows.Close()
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			mr.Label = toLabel(mr.Name)
			mr.DimensionIDs = metricDims[mr.ID]
			allMetrics = append(allMetrics, mr)
		}
		allRows.Close()
		if err := allRows.Err(); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
	} else {
		// Whole-model call (no grid_def_id) — `metrics` wasn't run through the
		// mr.DimensionIDs assignment above (that only happens in the gridDefID
		// branch), so without this, every all_metrics entry silently has a nil
		// DimensionIDs here even though metricDims has the real data — bit the
		// KPI dimension-scope picker, which reads all_metrics from exactly
		// this call shape (MetricKpiWidget fetches with no grid_def_id).
		allMetrics = make([]metricRow, len(metrics))
		for i, mr := range metrics {
			mr.DimensionIDs = metricDims[mr.ID]
			allMetrics[i] = mr
			// The `metrics` array itself carried nil dimension_ids on this
			// call shape, breaking any client that binds metrics to
			// dimensions from `metrics` rather than `all_metrics`.
			metrics[i].DimensionIDs = metricDims[mr.ID]
		}
	}

	// ── apply metric rules to metrics AND all_metrics ─────────────────────────
	// Must run before the cells/totals computation below: all_metrics feeds
	// scopeCalcCells' dependency resolution, and a metric_kpi widget reads
	// its value straight from all_metrics/totals — a "hidden" metric
	// surviving in either would leak its real value the same way an
	// un-filtered hidden dimension member would. Applying only to the old
	// end-of-function `metrics` list (kept below for compatibility, though
	// it's now redundant with this) left all_metrics, cells, and totals
	// completely unfiltered by metric-level rules.
	applyMetricRules := func(list []metricRow) []metricRow {
		if len(metricRules) == 0 {
			return list
		}
		kept := make([]metricRow, 0, len(list))
		for _, m := range list {
			switch metricRules[m.ID] {
			case "hidden":
				// omit entirely
			case "read":
				m.Readonly = true
				kept = append(kept, m)
			default:
				kept = append(kept, m)
			}
		}
		return kept
	}
	metrics = applyMetricRules(metrics)
	allMetrics = applyMetricRules(allMetrics)

	// ── cell values + is_input totals: single scoped pass ─────────────────────
	// Fetch every fact in the revision (scalar and per-member alike) and, for
	// each row not touching a hidden member (factRowHidden), build BOTH the
	// composite-key `cells` map (using that metric's OWN grid dims, not the
	// current request's `dims` — lets a cross-grid formula reference resolve
	// real per-member values instead of a flat broadcast) AND accumulate
	// `totals` for is_input metrics. One pass, one source of truth: a fact
	// row a hidden-member caller can't see contributes to neither.
	cells := make(map[string]float64)
	totals := make(map[string]float64)
	// meta_only skips ALL cell/calc work — the client fetches dimensions and
	// metrics first (cheap) to learn the context selectors, then makes a
	// scoped cells request. Splitting the one expensive whole-model read
	// into meta + scoped-slice is what keeps a 500-member grid sub-second.
	metaOnly := r.URL.Query().Get("meta_only") == "1"
	// totals_only returns per-metric totals but no per-combo cells — what a
	// KPI needs. Serializing/transferring tens of thousands of cells is the
	// dominant cost of a whole-model read on a large model. When the caller
	// is also unscoped with no hidden members (fastTotals), the DB work
	// collapses too: per-metric input SUMs and only the '{}' calc rows,
	// instead of the whole per-combo lattice.
	totalsOnly := r.URL.Query().Get("totals_only") == "1"
	fastTotals := !metaOnly && totalsOnly && len(scopePinned) == 0 && len(hiddenByDim) == 0

	// Single-dimension slice fast path: a totals_only read pinning EXACTLY one
	// dimension (no hidden members) can serve each calc metric's total from a
	// precomputed slice row instead of re-resolving the slice.
	//
	// Coverage is PARTIAL by design. Per metric:
	//   - has a slice row → served from it;
	//   - dimensionless → served its '{}' scalar (a pin can't change it);
	//   - not dimensioned by the pin, whole-model total zero → skipped (a sum
	//     of non-negative parts is zero only when every part is — the Test
	//     model's `sales`);
	//   - dimensioned by the pin but NO slice row → OMITTED from totals. The
	//     scheduler deliberately writes no row where the formula can't
	//     evaluate (no data at the slice), and "absent" is the platform's
	//     no-false-numbers answer — the KPI renders "—". This used to force
	//     the ENTIRE read onto the multi-second recompute for every
	//     sparse-data pin (a product item with facts in only some cells).
	// The one remaining full-fallback trigger: a metric not dimensioned by
	// the pin with a NON-zero total — its scoped value flows through its
	// inputs and genuinely needs the recompute.
	sliceFast := false
	var sliceVals map[string]float64
	if totalsOnly && len(scopePinned) == 1 && len(hiddenByDim) == 0 {
		var pinnedDim string
		for d := range scopePinned {
			pinnedDim = d
		}
		store := calculation.NewStore(h.db.For(ctx))
		sj, _ := json.Marshal(scopePinned)
		sv, sErr := store.LoadCalcSlice(ctx, modelID, revisionID, string(sj))
		var calcTotals map[string]float64 // '{}' rows, loaded lazily
		loadTotals := func() {
			if calcTotals == nil {
				calcTotals, _ = store.LoadCalcTotals(ctx, modelID, revisionID)
			}
		}
		if sErr == nil {
			served := make(map[string]float64, len(sv))
			for id, v := range sv {
				served[id] = v // slice rows
			}
			covered := true
			for _, m := range allMetrics {
				if m.IsInput || metricRules[m.ID] == "hidden" {
					continue
				}
				if _, ok := served[m.ID]; ok {
					continue // has a slice row
				}
				if len(metricDims[m.ID]) == 0 {
					// Dimensionless metric (no grid dims): it holds a single
					// '{}' scalar and has no per-member breakdown, so a pin on
					// any dimension leaves it unchanged — serve that scalar.
					loadTotals()
					if v, ok := calcTotals[m.ID]; ok {
						served[m.ID] = v
					}
					continue // no '{}' row → legitimately absent
				}
				dimmedByPin := false
				for _, did := range metricDims[m.ID] {
					if did == pinnedDim {
						dimmedByPin = true
						break
					}
				}
				if dimmedByPin {
					continue // no slice row → omitted from totals (renders "—")
				}
				// Dimensioned, but by other dimensions than the pin. Its
				// scoped value comes through its inputs and can't be read
				// off the '{}' row — unless it's zero everywhere (a sum of
				// non-negative parts), in which case its slice is zero too.
				loadTotals()
				if calcTotals[m.ID] == 0 {
					continue
				}
				h.log.Debug().Str("metric", m.Name).Str("metric_id", m.ID).
					Int("slice_rows", len(sv)).
					Msg("slice fast path: non-pinned-dim metric with nonzero total, falling back")
				covered = false
				break
			}
			if covered {
				sliceFast = true
				sliceVals = served
			}
		}
	}

	// Both fast paths read input totals as one grouped SUM per metric (no
	// per-combo cells). scopeSQL is empty for fastTotals (whole model) and the
	// one-dimension pin for sliceFast, injected into both fact branches.
	groupedInput := fastTotals || sliceFast
	if groupedInput {
		// One deduped SUM per input metric (latest-wins per combo, form
		// 'replace'/'last' overriding direct entry), no per-combo rows built.
		trows, terr := h.db.Query(ctx, `
			WITH covered AS MATERIALIZED (
				SELECT DISTINCT fi.metric_id, fi.dim_members
				FROM runtime.fact_input fi
				JOIN model.form_metric_mapping fmm ON fmm.id = fi.source_ref
				WHERE fi.model_id = $1::uuid AND fi.revision_id = $2::uuid
				  AND fmm.aggregation IN ('replace', 'last')
			)
			SELECT metric_id::text, SUM(v)::float8 FROM (
				SELECT metric_id, dim_members, SUM(value)::float8 AS v
				FROM (
					SELECT direct.metric_id, direct.dim_members, direct.value FROM (
						SELECT DISTINCT ON (metric_id, dim_members)
							metric_id, dim_members, value
						FROM runtime.fact_input
						WHERE model_id=$1::uuid AND revision_id=$2::uuid AND source_ref IS NULL`+scopeSQL+`
						ORDER BY metric_id, dim_members, entered_at DESC, id DESC
					) direct
					WHERE NOT EXISTS (
						SELECT 1 FROM covered c
						WHERE c.metric_id = direct.metric_id AND c.dim_members = direct.dim_members
					)
					UNION ALL
					SELECT metric_id, dim_members, value
					FROM runtime.fact_input
					WHERE model_id=$1::uuid AND revision_id=$2::uuid AND source_ref IS NOT NULL`+scopeSQL+`
				) fi
				GROUP BY metric_id, dim_members
			) per_combo
			GROUP BY metric_id
		`, append([]any{modelID, revisionID}, scopeArgs...)...)
		if terr != nil {
			jsonErr(w, terr, http.StatusInternalServerError)
			return
		}
		for trows.Next() {
			var metricID string
			var val float64
			if err := trows.Scan(&metricID, &val); err != nil {
				trows.Close()
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			if metricRules[metricID] == "hidden" {
				continue
			}
			totals[metricID] += val
		}
		trows.Close()
	} else if !metaOnly {
		// Latest direct-entry per cell + all active form-integration entries.
		// For cells covered by a "replace"/"last" mapping, the direct-entry row
		// is excluded so the form value fully replaces it (without deleting it).
		// `covered` is MATERIALIZED deliberately: it is the (normally empty,
		// always small) set of cells a replace/last form mapping owns, so the
		// direct-entry anti-join below hashes against a tiny precomputed
		// relation instead of correlating a per-row rescan of all of
		// fact_input. Without this, right after a bulk import — before
		// autovacuum has ANALYZEd the fresh rows — the planner estimated the
		// fact scan at 1 row, chose a nested-loop anti-join, and rescanned
		// 20k rows per distinct cell (80M form-mapping probes, ~15s for a
		// 10k-row import). Materializing removes the stats dependency: the
		// plan is robust whether or not statistics are current.
		cellArgs := append([]any{modelID, revisionID}, scopeArgs...)
		cellRows, err := h.db.Query(ctx, `
			WITH covered AS MATERIALIZED (
				SELECT DISTINCT fi.metric_id, fi.dim_members
				FROM runtime.fact_input fi
				JOIN model.form_metric_mapping fmm ON fmm.id = fi.source_ref
				WHERE fi.model_id = $1::uuid AND fi.revision_id = $2::uuid
				  AND fmm.aggregation IN ('replace', 'last')
			)
			SELECT metric_id::text, dim_members::text, SUM(value)::float8
			FROM (
				SELECT direct.metric_id, direct.dim_members, direct.value FROM (
					SELECT DISTINCT ON (metric_id, dim_members)
						metric_id, dim_members, value
					FROM runtime.fact_input
					WHERE model_id=$1::uuid AND revision_id=$2::uuid AND source_ref IS NULL`+scopeSQL+`
					ORDER BY metric_id, dim_members, entered_at DESC, id DESC
				) direct
				WHERE NOT EXISTS (
					SELECT 1 FROM covered c
					WHERE c.metric_id = direct.metric_id
					  AND c.dim_members = direct.dim_members
				)
				UNION ALL
				SELECT metric_id, dim_members, value
				FROM runtime.fact_input
				WHERE model_id=$1::uuid AND revision_id=$2::uuid AND source_ref IS NOT NULL`+scopeSQL+`
			) fi
			GROUP BY metric_id, dim_members
		`, cellArgs...)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		for cellRows.Next() {
			var metricID, dmJSON string
			var val float64
			if err := cellRows.Scan(&metricID, &dmJSON, &val); err != nil {
				cellRows.Close()
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			var dm map[string]string
			if err := json.Unmarshal([]byte(dmJSON), &dm); err != nil {
				continue
			}
			if factRowHidden(dm, hiddenByDim) {
				continue // hidden-member fact: excluded from both cells and totals
			}
			if metricRules[metricID] == "hidden" {
				continue // hidden-metric fact: this loop reads fact_input directly, bypassing all_metrics' own filtering above
			}
			totals[metricID] += val
			ownDims := metricDims[metricID]
			if len(ownDims) == 0 {
				continue // scalar-only metric: no composite cell key, covered by totals
			}
			codes := make([]string, 0, len(ownDims))
			valid := true
			for _, dimID := range ownDims {
				code, ok := dm[dimID]
				if !ok {
					valid = false
					break
				}
				codes = append(codes, code)
			}
			if !valid {
				continue
			}
			cells[metricID+":"+strings.Join(codes, ":")] = val
		}
		cellRows.Close()
	}

	// Inputs on a time dimension total by their time_summary (a closing
	// balance is its LAST period, not the sum of every period's balance):
	// non-time dimensions sum per period, then the periods reduce. The
	// per-period sums come from this request's own (scope/hidden-filtered)
	// cells, or — on the cell-less totals-only path — from the same
	// input-value map the scheduler reads.
	if !metaOnly {
		if err := h.applyInputTimeSummaries(ctx, modelID, revisionID, allMetrics, metricDims, allDims, cells, totals, groupedInput, scopeSubtrees); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
	}

	// ── calculated-metric cells + totals ──────────────────────────────────────
	// runtime.calc_result holds one row per leaf combo per calc metric (plus
	// one '{}' company-wide aggregate row), computed by the calculation
	// scheduler over ALL facts with no per-user access concept. That's correct
	// and cheap to reuse verbatim for a caller with no hidden-member rules —
	// merged into `cells`/`totals` exactly like input values already are. A
	// scoped caller instead gets every calc metric's per-combo value
	// re-resolved (scopeCalcCells, internal/rollup + internal/formula — the
	// same engines the calculation service itself runs) from the already
	// hidden-member-scoped input `cells` just built above: reusing
	// calc_result's own rows for a scoped caller would leak a hidden,
	// finer-grained dependency's contribution into a coarser calc metric's
	// value (see scopeCalcCells's doc comment).
	if fastTotals {
		// totals-only, unscoped, no hidden: each calc metric's total is its
		// '{}' aggregate row — read those rows alone, not the whole lattice.
		calcStore := calculation.NewStore(h.db.For(ctx))
		ct, ccErr := calcStore.LoadCalcTotals(ctx, modelID, revisionID)
		if ccErr != nil {
			jsonErr(w, ccErr, http.StatusInternalServerError)
			return
		}
		for metricID, v := range ct {
			if metricRules[metricID] == "hidden" {
				continue
			}
			totals[metricID] = v
		}
	} else if sliceFast {
		// totals-only, one-dimension pin: each calc metric's slice total is
		// its precomputed {oneDim: member} row (loaded above), consistent with
		// scopeCalcCells by construction (TestSalesDemoSliceRowsMatchScopedRead).
		for metricID, v := range sliceVals {
			if metricRules[metricID] == "hidden" {
				continue
			}
			totals[metricID] = v
		}
	} else if !metaOnly && len(hiddenByDim) == 0 && len(scopePinned) == 0 {
		// Fully-unscoped, no-hidden caller: reuse the scheduler's own
		// calc_result rows verbatim, including the '{}' company-wide
		// aggregate as each metric's total. Cheapest path.
		calcStore := calculation.NewStore(h.db.For(ctx))
		allCalc, ccErr := calcStore.LoadAllCalcValueMaps(ctx, modelID, revisionID)
		if ccErr != nil {
			jsonErr(w, ccErr, http.StatusInternalServerError)
			return
		}
		for metricID, byCombo := range allCalc {
			if metricRules[metricID] == "hidden" {
				continue // this branch reads calc_result directly, bypassing all_metrics' own filtering above
			}
			ownDims := metricDims[metricID]
			for dk, val := range byCombo {
				var dm map[string]string
				if json.Unmarshal([]byte(dk), &dm) != nil {
					continue
				}
				if len(dm) == 0 {
					totals[metricID] = val // the '{}' aggregate row
					continue
				}
				if len(ownDims) == 0 {
					continue // scalar-only metric: already covered by the aggregate above
				}
				codes := make([]string, 0, len(ownDims))
				valid := true
				for _, dimID := range ownDims {
					code, ok := dm[dimID]
					if !ok {
						valid = false
						break
					}
					codes = append(codes, code)
				}
				if valid {
					cells[metricID+":"+strings.Join(codes, ":")] = val
				}
			}
		}
	} else if !metaOnly {
		// Scoped and/or hidden-member caller: calc_result cannot be reused
		// verbatim. Its '{}' total is the WHOLE model (filtered out by a
		// scope pin anyway), and it holds only leaf combos — no partial
		// rollup rows — so a pinned slice has no precomputed total to read.
		// Re-resolve every calc metric per combo from the already
		// scope/hidden-filtered input `cells` (scopeCalcCells, same
		// rollup+formula engines as the scheduler) and derive the scoped
		// total via CombineAgg — correct ratio-of-sums for average metrics,
		// and no hidden-finer-grain dependency leak.
		rollupDims := toRollupDims(allDims)
		// A scope pin must restrict the COMBO UNIVERSE, not only the input
		// values: scopeCalcCells enumerates rollup.LeafCombos over these
		// dims, and a formula that doesn't read any scoped input — a
		// dimension-conditional SWITCH(period,...), an IFS over constants, a
		// COUNT of always-present refs — produces a real value at EVERY
		// combo. Left unrestricted, a period=Q1 pin still summed such a
		// metric over all four quarters' combos (found live: quarter_weight
		// 2016 vs the correct Q1-only 403.2, exactly 4x), disagreeing with
		// the precomputed slice rows, which evaluate at the pin. Trim each
		// pinned dimension's member list to the pinned subtree so the combo
		// enumeration matches the pin.
		for dimID, subtree := range scopeSubtrees {
			d := rollupDims[dimID]
			if d == nil {
				continue
			}
			kept := make([]rollup.Member, 0, len(subtree))
			for _, m := range d.Members {
				if subtree[m.Code] {
					kept = append(kept, m)
				}
			}
			trimmed := *d
			trimmed.Members = kept
			rollupDims[dimID] = &trimmed
		}
		dimIDToName := make(map[string]string, len(allDims))
		for _, d := range allDims {
			dimIDToName[d.ID] = d.Name
		}
		series, sErr := h.loadScopedSeries(ctx, modelID, revisionID, allDims, allMetrics, metricDims, hiddenByDim)
		if sErr != nil {
			jsonErr(w, sErr, http.StatusInternalServerError)
			return
		}
		scopedCells, scopedTotals := scopeCalcCells(ctx, rollupDims, metricDims, dimIDToName, allMetrics, cells, series)
		for k, v := range scopedCells {
			cells[k] = v
		}
		for id, v := range scopedTotals {
			totals[id] = v
		}
	}

	// totals_only under a scope/hidden caller still had to build per-combo
	// cells to aggregate the calc totals correctly — but the caller wants
	// only the numbers, so drop the cells before serializing. (fastTotals
	// never populated cells in the first place.)
	if totalsOnly {
		cells = map[string]float64{}
	}

	jsonOK(w, gridResponse{
		RevisionID:         revisionID,
		Metrics:            metrics,
		AllMetrics:         allMetrics,
		Dimensions:         dims,
		AllDimensions:      allDims,
		Departments:        depts,
		Cells:              cells,
		Totals:             totals,
		AccessRules:        gridAccessRules{DimMembers: dimRules, Metrics: metricRules},
		RollupSourceGridID: rollupSourceGridID,
	})
}

// ── /api/workflow/submit ──────────────────────────────────────────────────────

type submitReq struct {
	RevisionID string `json:"revision_id"`
	ModelID    string `json:"model_id"`
}

func (h *handler) workflowSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}

	var req submitReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	if req.ModelID == "" {
		jsonErr(w, fmt.Errorf("model_id required"), http.StatusBadRequest)
		return
	}

	// Guard: model_id arrives in the request body and this route is registered
	// for role "any", so holding an authenticated session proves nothing about
	// whether the caller may act on the model they named. Without this check a
	// user in one tenant could start a REAL workflow instance in another
	// tenant's application — instance, steps, assignments and the notifications
	// they dispatch — simply by naming its model_id.
	//
	// /api/cells, which takes model_id from the body in exactly the same shape,
	// has always guarded it this way; this handler was the outlier.
	if canAccess, caErr := h.actorCanAccessModel(ctx, a, req.ModelID); caErr != nil {
		jsonErr(w, caErr, http.StatusInternalServerError)
		return
	} else if !canAccess {
		jsonErr(w, fmt.Errorf("forbidden: model is outside your access scope"), http.StatusForbidden)
		return
	}
	// The revision is caller-supplied too, and is stored on the instance
	// context and used to select the workflow def below.
	if !h.requireRevisionInModel(w, r, req.RevisionID, req.ModelID) {
		return
	}

	// Resolve revision: fall back to active revision if not specified.
	if req.RevisionID == "" {
		_ = h.db.QueryRow(ctx,
			`SELECT COALESCE(active_revision_id::text,'') FROM core.model WHERE id=$1::uuid`,
			req.ModelID,
		).Scan(&req.RevisionID)
	}

	// Look up the workflow def for this application, preferring the
	// request's revision (defs are revision-scoped; NULL = revision-global)
	// Legacy convenience start ("submit this model/revision for approval"):
	// the newest PUBLISHED definition of the model's application at this
	// revision, started through the same engine path as every other start.
	// It used to pick the newest def regardless of status (a draft could
	// start), skip ResolveStartContext (no publish/required-context/dedup/
	// hidden-member checks) and hand-insert step rows without the routing
	// engine (auto steps never ran) — found by the 2026-09-13 scenario run.
	var wfDefID string
	err = h.db.QueryRow(ctx, `
		SELECT wd.id::text
		FROM workflow.workflow_def wd
		JOIN core.application app ON app.id = wd.application_id
		JOIN core.model m ON m.application_id = app.id
		WHERE m.id = $1::uuid
		  AND wd.status = 'published'
		  AND ($2 = '' OR wd.revision_id IS NULL OR wd.revision_id::text = $2)
		ORDER BY wd.created_at DESC LIMIT 1
	`, req.ModelID, req.RevisionID).Scan(&wfDefID)
	if err != nil {
		jsonErr(w, fmt.Errorf("no published workflow def found: %w", err), http.StatusNotFound)
		return
	}

	ws := workflow.NewStore(h.db.For(ctx))
	resolvedContext, err := ws.ResolveStartContext(ctx, wfDefID, a.UserID, map[string]string{
		"revision_id": req.RevisionID,
		"model_id":    req.ModelID,
	})
	if err != nil {
		switch {
		case errors.Is(err, workflow.ErrWorkflowNotPublished):
			jsonErr(w, err, http.StatusBadRequest)
		case errors.Is(err, workflow.ErrNoRACIScope), errors.Is(err, workflow.ErrHiddenScope):
			jsonErr(w, err, http.StatusForbidden)
		case errors.Is(err, workflow.ErrDuplicateInstance):
			jsonErr(w, err, http.StatusConflict)
		case errors.Is(err, workflow.ErrUnknownMetric), errors.Is(err, workflow.ErrMissingContext), errors.Is(err, workflow.ErrUnknownMember):
			jsonErr(w, err, http.StatusBadRequest)
		default:
			jsonErr(w, err, http.StatusInternalServerError)
		}
		return
	}
	instance, err := ws.StartWorkflow(ctx, wfDefID, a.UserID, resolvedContext)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	instanceID := instance.Id

	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryDataChange, EventType: auditlog.EventWorkflowSubmitted,
		ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
		ResourceType: "workflow_instance", ResourceID: instanceID, RevisionID: req.RevisionID,
		Metadata: map[string]string{"model_id": req.ModelID},
	})

	jsonOK(w, map[string]string{"instance_id": instanceID, "status": "running"})
}

// ── /api/developer/revisions ─────────────────────────────────────────────────

type devRevision struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	CreatedAt   string `json:"created_at"`
	IsActive    bool   `json:"is_active"`
}

func (h *handler) developerRevisions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}

	// Resolve model — use explicit model_id param or derive from selected app context
	modelID := r.URL.Query().Get("model_id")
	if modelID == "" {
		modelID, err = h.resolveDemoModelID(ctx, r)
		if err != nil {
			jsonAccessErr(w, err, "resolve model")
			return
		}
	} else {
		canAccessModel, err := h.actorCanAccessModel(ctx, act, modelID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if !canAccessModel {
			jsonErr(w, fmt.Errorf("forbidden: model is outside your access scope"), http.StatusForbidden)
			return
		}
	}
	var activeRevName *string
	var activeRevID *string
	_ = h.db.QueryRow(ctx,
		`SELECT active_revision_name, active_revision_id::text FROM core.model WHERE id=$1::uuid`, modelID,
	).Scan(&activeRevName, &activeRevID)

	if r.Method == http.MethodPost {
		var body struct {
			Name             string `json:"name"`
			SourceRevisionID string `json:"source_revision_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
			jsonErr(w, fmt.Errorf("name is required"), http.StatusBadRequest)
			return
		}

		tx, err := h.db.Begin(ctx)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck

		newID, err := h.duplicateRevision(ctx, tx, modelID, body.Name, body.SourceRevisionID, activeRevID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}

		// The copy carries facts but no calc_result rows (see Step B), and
		// nothing else ever recalculates a revision nobody has edited yet —
		// without this, every calc metric in the new revision rendered blank
		// until a user happened to write a cell (found live, 2026-09-10).
		if err := h.recalcRevisionFromInputs(ctx, modelID, newID); err != nil {
			h.log.Warn().Err(err).Str("revision_id", newID).Msg("recalc after revision duplication failed")
		}

		revAct, _ := h.resolveActor(ctx, r)
		revActorID, revActorRole := "", ""
		if revAct != nil {
			revActorID, revActorRole = revAct.UserID, strings.Join(revAct.Roles, ",")
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryModelChange, EventType: auditlog.EventRevisionCreated,
			ActorUserID: revActorID, ActorRole: revActorRole,
			ResourceType: "revision", ResourceID: newID, RevisionID: newID,
			Metadata: map[string]string{"name": body.Name, "model_id": modelID},
		})
		jsonOK(w, map[string]string{"id": newID})
		return
	}

	if r.Method != http.MethodGet {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}

	rows, err := h.db.Query(ctx, `
		SELECT id::text, name, description, created_at::text
		FROM model.revision WHERE model_id=$1::uuid ORDER BY created_at
	`, modelID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var revisions []devRevision
	for rows.Next() {
		var rev devRevision
		if err := rows.Scan(&rev.ID, &rev.Name, &rev.Description, &rev.CreatedAt); err != nil {
			continue
		}
		rev.IsActive = activeRevID != nil && rev.ID == *activeRevID
		revisions = append(revisions, rev)
	}
	if revisions == nil {
		revisions = []devRevision{}
	}
	jsonOK(w, revisions)
}

// duplicateRevision deep-copies all revision-scoped data (metrics,
// dependencies, dimensions with members and typed properties, grids,
// dashboards with widgets and folders, forms with records and metric
// mappings, integrations, workflow defs and automation rules, plus
// latest-wins fact values) from an existing revision into a freshly created
// one — entirely through tx, so any step failing leaves nothing behind (the
// caller rolls tx back). Mirrors the entity set of model_transfer.go's
// export/import package, which duplicates the same shape for a different
// transport.
func (h *handler) duplicateRevision(ctx context.Context, tx pgx.Tx, modelID, name, sourceRevisionID string, activeRevID *string) (string, error) {
	var newID string
	if err := tx.QueryRow(ctx,
		`INSERT INTO model.revision (model_id, name, description) VALUES ($1::uuid, $2, '') RETURNING id::text`,
		modelID, name).Scan(&newID); err != nil {
		return "", err
	}

	// Resolve copy source: prefer explicit source_revision_id from request body,
	// then fall back to the model's active_revision_id, then the oldest revision.
	srcRevID := sourceRevisionID
	// Validate that the requested source revision actually belongs to this model.
	// If it doesn't (e.g. a revision from a different model was passed), clear it
	// so we fall back to a safe default. This prevents silently copying from the
	// wrong model which results in empty dimensions/metrics.
	if srcRevID != "" {
		var belongs bool
		_ = tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM model.revision WHERE id=$1::uuid AND model_id=$2::uuid)`,
			srcRevID, modelID).Scan(&belongs)
		if !belongs {
			srcRevID = ""
		}
	}
	if srcRevID == "" && activeRevID != nil {
		srcRevID = *activeRevID
	}
	if srcRevID == "" {
		// Fall back to the oldest revision for this model
		_ = tx.QueryRow(ctx,
			`SELECT id::text FROM model.revision WHERE model_id=$1::uuid ORDER BY created_at ASC LIMIT 1`,
			modelID).Scan(&srcRevID)
	}
	// Deep-copy all revision data: metrics, dimensions, members, facts, calc results,
	// dependencies, and dashboards. Uses a CTE chain so ID remapping is done atomically.
	// $1=modelID  $2=newID (destination)  $3=srcRevID (source)
	srcArgs := []any{modelID, newID, srcRevID}

	// Step A: structural copy — metrics, deps, dimensions, members, dashboards, widgets.
	// Kept in one CTE so ID remapping (metric_map/dim_map) is consistent.
	// calc_result is intentionally excluded: it has a NOT NULL partition_key with no default
	// and will be recalculated by the engine once the new revision is activated.
	if _, err := tx.Exec(ctx, `
		WITH
		-- 1. Copy metrics; capture old→new ID mapping via name join
		new_metrics AS (
			INSERT INTO model.metric_def
			  (model_id, name, formula, storage_type, is_input, agg_rule, format, format_decimals, format_currency, time_summary, revision_id)
			SELECT model_id, name, formula, storage_type, is_input, agg_rule, format, format_decimals, format_currency, time_summary, $2::uuid
			FROM model.metric_def WHERE model_id=$1::uuid AND revision_id=$3::uuid
			RETURNING id AS new_id, name
		),
		metric_map AS (
			SELECT o.id AS old_id, n.new_id
			FROM model.metric_def o
			JOIN new_metrics n ON n.name = o.name
			WHERE o.model_id=$1::uuid AND o.revision_id=$3::uuid
		),
		-- 2. Copy calc_dependency with remapped metric IDs
		new_deps AS (
			INSERT INTO model.calc_dependency
			  (metric_id, depends_on_metric_id, min_time_offset, max_time_offset, unbounded_past, unbounded_future)
			SELECT mm.new_id, dm.new_id, cd.min_time_offset, cd.max_time_offset, cd.unbounded_past, cd.unbounded_future
			FROM model.calc_dependency cd
			JOIN metric_map mm ON mm.old_id = cd.metric_id
			JOIN metric_map dm ON dm.old_id = cd.depends_on_metric_id
			RETURNING metric_id
		),
		-- 3. Copy dimensions; capture old→new ID mapping via name join
		new_dims AS (
			INSERT INTO model.dimension_def
			  (model_id, name, agg_rule, properties, revision_id, source_property,
			   dimension_type, time_granularity, fiscal_year_start_month)
			SELECT model_id, name, agg_rule, properties, $2::uuid, source_property,
			       dimension_type, time_granularity, fiscal_year_start_month
			FROM model.dimension_def WHERE model_id=$1::uuid AND revision_id=$3::uuid
			RETURNING id AS new_id, name
		),
		dim_map AS (
			SELECT o.id AS old_id, n.new_id
			FROM model.dimension_def o
			JOIN new_dims n ON n.name = o.name
			WHERE o.model_id=$1::uuid AND o.revision_id=$3::uuid
		),
		-- 4. Copy dimension members. parent_dimension_id / parent_member_id are
		-- remapped in a SEPARATE follow-up Exec below, not here: within a single
		-- WITH query every sub-statement shares one pre-statement snapshot, so an
		-- UPDATE CTE in THIS statement cannot see rows new_dims/new_members just
		-- inserted (same reasoning as the "Step B" fact copy below, which already
		-- has to rebuild its own mapping for the same reason).
		new_members AS (
			INSERT INTO model.dimension_member
			  (dimension_id, code, label, properties, sort_order, period_start, period_end, time_index)
			SELECT dm.new_id, m.code, m.label, m.properties, m.sort_order, m.period_start, m.period_end, m.time_index
			FROM model.dimension_member m
			JOIN dim_map dm ON dm.old_id = m.dimension_id
			RETURNING id
		),
		-- 5. Copy dashboards (folder_id is remapped in Step F below, after
		-- folders themselves are copied)
		new_dashes AS (
			INSERT INTO model.dashboard_def (model_id, name, tags, category, revision_id)
			SELECT model_id, name, tags, category, $2::uuid
			FROM model.dashboard_def WHERE model_id=$1::uuid AND revision_id=$3::uuid
			RETURNING id AS new_id, name
		),
		-- 6. Copy widgets for the new dashboards
		new_widgets AS (
			INSERT INTO model.dashboard_widget
			  (dashboard_id, widget_type, ref_id, content, sort_order, col_start, col_span, pos_x, pos_y, size_w, size_h, title, show_title, widget_props)
			SELECT nd.new_id, w.widget_type, w.ref_id, w.content,
			       w.sort_order, w.col_start, w.col_span, w.pos_x, w.pos_y, w.size_w, w.size_h,
			       w.title, w.show_title, w.widget_props
			FROM new_dashes nd
			JOIN model.dashboard_def old_d
			  ON old_d.model_id=$1::uuid AND old_d.name=nd.name AND old_d.revision_id=$3::uuid
			JOIN model.dashboard_widget w ON w.dashboard_id = old_d.id
			RETURNING id
		)
		-- Reference all CTEs so PostgreSQL executes every INSERT branch
		SELECT
			(SELECT count(*) FROM new_metrics) +
			(SELECT count(*) FROM new_deps) +
			(SELECT count(*) FROM new_members) +
			(SELECT count(*) FROM new_dashes) +
			(SELECT count(*) FROM new_widgets)
	`, srcArgs...); err != nil {
		return "", fmt.Errorf("revision structural copy failed: %w", err)
	}

	// Step A2: remap parent_dimension_id / parent_member_id in a SEPARATE Exec so
	// the mapping queries run against the now-inserted rows from Step A above
	// (see comment on new_members).
	if _, err := tx.Exec(ctx, `
		WITH dim_map AS (
			SELECT o.id AS old_id, n.id AS new_id
			FROM model.dimension_def o
			JOIN model.dimension_def n ON n.name = o.name AND n.revision_id = $2::uuid
			WHERE o.model_id=$1::uuid AND o.revision_id=$3::uuid
		),
		dim_parent_fix AS (
			UPDATE model.dimension_def nd
			SET parent_dimension_id = pm.new_id
			FROM model.dimension_def od
			JOIN dim_map dm ON dm.old_id = od.id
			JOIN dim_map pm ON pm.old_id = od.parent_dimension_id
			WHERE nd.id = dm.new_id AND od.parent_dimension_id IS NOT NULL
			RETURNING nd.id
		),
		-- A copied revision must be fully self-contained (no references
		-- back into the source revision it was duplicated from), so
		-- source_dimension_id (the property-derived-dimension link,
		-- migration 055) is remapped the same way parent_dimension_id
		-- is above.
		dim_source_fix AS (
			UPDATE model.dimension_def nd
			SET source_dimension_id = sm.new_id
			FROM model.dimension_def od
			JOIN dim_map dm ON dm.old_id = od.id
			JOIN dim_map sm ON sm.old_id = od.source_dimension_id
			WHERE nd.id = dm.new_id AND od.source_dimension_id IS NOT NULL
			RETURNING nd.id
		),
		member_map AS (
			SELECT om.id AS old_id, nm.id AS new_id
			FROM model.dimension_member om
			JOIN dim_map dm ON dm.old_id = om.dimension_id
			JOIN model.dimension_member nm ON nm.dimension_id = dm.new_id AND nm.code = om.code
		),
		member_parent_fix AS (
			UPDATE model.dimension_member nmem
			SET parent_member_id = pmm.new_id
			FROM model.dimension_member omem
			JOIN member_map mm ON mm.old_id = omem.id
			JOIN member_map pmm ON pmm.old_id = omem.parent_member_id
			WHERE nmem.id = mm.new_id AND omem.parent_member_id IS NOT NULL
			RETURNING nmem.id
		)
		SELECT (SELECT count(*) FROM dim_parent_fix) + (SELECT count(*) FROM dim_source_fix) + (SELECT count(*) FROM member_parent_fix)
	`, srcArgs...); err != nil {
		return "", fmt.Errorf("revision dimension-hierarchy remap failed: %w", err)
	}

	// Step A3: remap agg_rule='rate' operands, for the same self-containment
	// reason as A2. Step A's metric INSERT deliberately omits these two
	// columns, because the new metric they must point at does not exist until
	// that INSERT has run — copying the old ids verbatim would leave a copied
	// ratio reading the SOURCE revision's numerator and denominator, so it
	// would keep answering with the old revision's numbers while every other
	// metric moved on. Matched by name, exactly as metric_map does.
	if _, err := tx.Exec(ctx, `
		UPDATE model.metric_def nm
		SET agg_numerator_metric_id = (
		        SELECT n.id FROM model.metric_def n
		        WHERE n.model_id = nm.model_id AND n.revision_id = nm.revision_id
		          AND n.name = (SELECT o.name FROM model.metric_def o WHERE o.id = om.agg_numerator_metric_id)
		    ),
		    agg_denominator_metric_id = (
		        SELECT n.id FROM model.metric_def n
		        WHERE n.model_id = nm.model_id AND n.revision_id = nm.revision_id
		          AND n.name = (SELECT o.name FROM model.metric_def o WHERE o.id = om.agg_denominator_metric_id)
		    )
		FROM model.metric_def om
		WHERE nm.model_id = $1::uuid AND nm.revision_id = $2::uuid
		  AND om.model_id = $1::uuid AND om.revision_id = $3::uuid
		  AND om.name = nm.name
		  AND (om.agg_numerator_metric_id IS NOT NULL OR om.agg_denominator_metric_id IS NOT NULL)
	`, srcArgs...); err != nil {
		return "", fmt.Errorf("revision ratio-operand remap failed: %w", err)
	}

	// Step B: fact copy (user-entered values). dim_map must be rebuilt here
	// via a join since it's not shared across Exec calls. source_ref and
	// entered_at are copied verbatim (not defaulted) — dropping them here
	// used to misclassify every copied form-posted row as direct-entry
	// (source_ref becomes NULL), which then collapsed a summed
	// multi-record cell down to one arbitrary row's value via the grid
	// read's "latest direct entry wins" ORDER BY entered_at DESC. The
	// sibling internal/modeltransfer package documents this exact hazard
	// by name (FactsPolicy) and avoids it.
	if _, err := tx.Exec(ctx, `
		INSERT INTO runtime.fact_input
		  (model_id, revision_name, metric_id, dim_members, value, entered_by, revision_id, source_ref, entered_at)
		SELECT
			fi.model_id, fi.revision_name,
			new_m.id,
			(SELECT COALESCE(jsonb_object_agg(new_d.id::text, kv.val), '{}'::jsonb)
			 FROM jsonb_each_text(fi.dim_members) kv(old_key, val)
			 JOIN model.dimension_def old_d ON old_d.id::text = kv.old_key
			 JOIN model.dimension_def new_d ON new_d.model_id = old_d.model_id
			                               AND new_d.name = old_d.name
			                               AND new_d.revision_id = $2::uuid),
			fi.value, fi.entered_by,
			$2::uuid,
			fi.source_ref, fi.entered_at
		FROM runtime.fact_input fi
		JOIN model.metric_def old_m ON old_m.id = fi.metric_id
		JOIN model.metric_def new_m ON new_m.model_id = old_m.model_id
		                           AND new_m.name = old_m.name
		                           AND new_m.revision_id = $2::uuid
		WHERE fi.revision_id = $3::uuid AND fi.model_id = $1::uuid
	`, srcArgs...); err != nil {
		return "", fmt.Errorf("revision fact copy failed: %w", err)
	}

	// Step C: copy grid definitions with remapped metric/dimension IDs
	if _, err := tx.Exec(ctx, `
		WITH
		new_grids AS (
			INSERT INTO model.grid_def (model_id, name, revision_id)
			SELECT model_id, name, $2::uuid
			FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$3::uuid
			RETURNING id AS new_id, name
		),
		new_grid_metrics AS (
			INSERT INTO model.grid_metric (grid_id, metric_id, sort_order)
			SELECT ng.new_id, new_m.id, gm.sort_order
			FROM model.grid_metric gm
			JOIN model.grid_def old_g ON old_g.id = gm.grid_id AND old_g.revision_id = $3::uuid
			JOIN new_grids ng ON ng.name = old_g.name
			JOIN model.metric_def old_m ON old_m.id = gm.metric_id
			JOIN model.metric_def new_m ON new_m.model_id = old_m.model_id
			                          AND new_m.name = old_m.name
			                          AND new_m.revision_id = $2::uuid
			RETURNING grid_id
		),
		new_grid_dims AS (
			INSERT INTO model.grid_dimension (grid_id, dimension_id, display_level)
			SELECT ng.new_id, new_d.id, gd.display_level
			FROM model.grid_dimension gd
			JOIN model.grid_def old_g ON old_g.id = gd.grid_id AND old_g.revision_id = $3::uuid
			JOIN new_grids ng ON ng.name = old_g.name
			JOIN model.dimension_def old_d ON old_d.id = gd.dimension_id
			JOIN model.dimension_def new_d ON new_d.model_id = old_d.model_id
			                             AND new_d.name = old_d.name
			                             AND new_d.revision_id = $2::uuid
			RETURNING grid_id
		)
		SELECT (SELECT count(*) FROM new_grid_metrics) + (SELECT count(*) FROM new_grid_dims)
	`, srcArgs...); err != nil {
		return "", fmt.Errorf("revision grid copy failed: %w", err)
	}

	// Step C2: remap rollup_source_grid_id in a separate Exec, same
	// reasoning as Step A2 — the referenced grid (e.g. Grid 1) and the
	// referencing grid (e.g. Grid 2) are both new rows from Step C
	// above, so this can't run in the same WITH query that inserted
	// them. A copied revision must be fully self-contained: Grid 2's
	// copy should mirror the copied Grid 1, never the source revision's.
	if _, err := tx.Exec(ctx, `
		WITH grid_map AS (
			SELECT o.id AS old_id, n.id AS new_id
			FROM model.grid_def o
			JOIN model.grid_def n ON n.name = o.name AND n.revision_id = $2::uuid
			WHERE o.model_id=$1::uuid AND o.revision_id=$3::uuid
		)
		UPDATE model.grid_def ng
		SET rollup_source_grid_id = sm.new_id
		FROM model.grid_def og
		JOIN grid_map gm ON gm.old_id = og.id
		JOIN grid_map sm ON sm.old_id = og.rollup_source_grid_id
		WHERE ng.id = gm.new_id AND og.rollup_source_grid_id IS NOT NULL
	`, srcArgs...); err != nil {
		return "", fmt.Errorf("revision grid rollup-source remap failed: %w", err)
	}

	// Step D: copy dimension properties (typed member-attribute schemas,
	// model.dimension_property — distinct from the properties JSONB copied
	// with dimension_def in Step A).
	if _, err := tx.Exec(ctx, `
		INSERT INTO model.dimension_property (dimension_id, name, data_type)
		SELECT nd.id, p.name, p.data_type
		FROM model.dimension_property p
		JOIN model.dimension_def od ON od.id = p.dimension_id
		JOIN model.dimension_def nd ON nd.model_id = od.model_id AND nd.name = od.name AND nd.revision_id = $2::uuid
		WHERE od.model_id=$1::uuid AND od.revision_id=$3::uuid
	`, srcArgs...); err != nil {
		return "", fmt.Errorf("revision dimension-property copy failed: %w", err)
	}

	// Step E: copy forms, their records, and form-metric mappings. Field
	// definitions embed dimension_id/metric_id refs and mapping rows embed
	// form/grid/metric/dimension refs — all remapped by name join against
	// the rows Steps A/C inserted. Unresolvable refs (e.g. a field
	// pointing at a dimension deleted from the source revision) are kept
	// as-is rather than dropped.
	if _, err := tx.Exec(ctx, `
		WITH
		dim_map AS (
			SELECT o.id AS old_id, n.id AS new_id
			FROM model.dimension_def o
			JOIN model.dimension_def n ON n.model_id = o.model_id AND n.name = o.name AND n.revision_id = $2::uuid
			WHERE o.model_id=$1::uuid AND o.revision_id=$3::uuid
		),
		metric_map AS (
			SELECT o.id AS old_id, n.id AS new_id
			FROM model.metric_def o
			JOIN model.metric_def n ON n.model_id = o.model_id AND n.name = o.name AND n.revision_id = $2::uuid
			WHERE o.model_id=$1::uuid AND o.revision_id=$3::uuid
		),
		new_forms AS (
			INSERT INTO model.form_def (model_id, name, label, fields, revision_id)
			SELECT f.model_id, f.name, f.label,
				COALESCE((
					SELECT jsonb_agg(
						e.elem
						|| COALESCE((SELECT jsonb_build_object('dimension_id', dm.new_id::text) FROM dim_map dm WHERE dm.old_id::text = e.elem->>'dimension_id'), '{}'::jsonb)
						|| COALESCE((SELECT jsonb_build_object('metric_id', mm.new_id::text) FROM metric_map mm WHERE mm.old_id::text = e.elem->>'metric_id'), '{}'::jsonb)
						ORDER BY e.ord)
					FROM jsonb_array_elements(CASE WHEN jsonb_typeof(f.fields) = 'array' THEN f.fields ELSE '[]'::jsonb END) WITH ORDINALITY AS e(elem, ord)
				), '[]'::jsonb),
				$2::uuid
			FROM model.form_def f
			WHERE f.model_id=$1::uuid AND f.revision_id=$3::uuid
			RETURNING id AS new_id, name
		),
		form_map AS (
			SELECT o.id AS old_id, n.new_id
			FROM model.form_def o
			JOIN new_forms n ON n.name = o.name
			WHERE o.model_id=$1::uuid AND o.revision_id=$3::uuid
		),
		new_records AS (
			INSERT INTO runtime.form_record (form_id, data, status, created_by)
			SELECT fm.new_id, rec.data, rec.status, rec.created_by
			FROM runtime.form_record rec
			JOIN form_map fm ON fm.old_id = rec.form_id
			RETURNING id
		),
		new_mappings AS (
			INSERT INTO model.form_metric_mapping
			  (model_id, form_id, grid_id, name, source_field, target_metric_id, aggregation,
			   posting_statuses, dimension_mappings, live_posting, revision_id)
			SELECT fmm.model_id, fm.new_id,
				(SELECT ng.id FROM model.grid_def og
				 JOIN model.grid_def ng ON ng.model_id = og.model_id AND ng.name = og.name AND ng.revision_id = $2::uuid
				 WHERE og.id = fmm.grid_id),
				fmm.name, fmm.source_field,
				mm.new_id, fmm.aggregation, fmm.posting_statuses,
				COALESCE((
					SELECT jsonb_object_agg(COALESCE(dm.new_id::text, kv.key), kv.value)
					FROM jsonb_each(fmm.dimension_mappings) kv
					LEFT JOIN dim_map dm ON dm.old_id::text = kv.key
				), '{}'::jsonb),
				fmm.live_posting, $2::uuid
			FROM model.form_metric_mapping fmm
			JOIN form_map fm ON fm.old_id = fmm.form_id
			JOIN metric_map mm ON mm.old_id = fmm.target_metric_id
			RETURNING id
		)
		SELECT
			(SELECT count(*) FROM new_forms) +
			(SELECT count(*) FROM new_records) +
			(SELECT count(*) FROM new_mappings)
	`, srcArgs...); err != nil {
		return "", fmt.Errorf("revision form copy failed: %w", err)
	}

	// Step E2: point the copied form-posted facts (Step B copied source_ref
	// verbatim — see the comment there for why it must not be NULLed) at
	// the NEW revision's mapping instead of the source revision's. Left
	// verbatim, every copied revision's form-posted rows referenced a
	// mapping in a different revision: a model export of that revision
	// tagged them with a mapping ID that wasn't in the package and the
	// import silently dropped them (found live, 2026-09-10), and a re-post
	// from the copied mapping could not find and replace its own rows.
	// Same snapshot-visibility reason as Step A2 for being a separate Exec.
	if _, err := tx.Exec(ctx, `
		UPDATE runtime.fact_input nf
		SET source_ref = n.id
		FROM model.form_metric_mapping o
		JOIN model.form_def od ON od.id = o.form_id
		JOIN model.form_def nd ON nd.model_id = od.model_id AND nd.name = od.name AND nd.revision_id = $2::uuid
		JOIN model.form_metric_mapping n ON n.form_id = nd.id AND n.name = o.name AND n.source_field = o.source_field
		WHERE nf.model_id = $1::uuid AND nf.revision_id = $2::uuid
		  AND nf.source_ref = o.id AND o.id <> n.id
	`, modelID, newID); err != nil {
		return "", fmt.Errorf("revision fact source_ref remap failed: %w", err)
	}

	// Step F: copy dashboard folders, then remap folder parents and the
	// copied dashboards' folder assignments. Two Execs for the same
	// snapshot-visibility reason as Step A2.
	if _, err := tx.Exec(ctx, `
		INSERT INTO model.dashboard_folder (model_id, name, revision_id)
		SELECT model_id, name, $2::uuid
		FROM model.dashboard_folder WHERE model_id=$1::uuid AND revision_id=$3::uuid
	`, srcArgs...); err != nil {
		return "", fmt.Errorf("revision dashboard-folder copy failed: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		WITH folder_map AS (
			SELECT o.id AS old_id, n.id AS new_id
			FROM model.dashboard_folder o
			JOIN model.dashboard_folder n ON n.model_id = o.model_id AND n.name = o.name AND n.revision_id = $2::uuid
			WHERE o.model_id=$1::uuid AND o.revision_id=$3::uuid
		),
		parent_fix AS (
			UPDATE model.dashboard_folder nf
			SET parent_id = pm.new_id
			FROM model.dashboard_folder ofo
			JOIN folder_map fmap ON fmap.old_id = ofo.id
			JOIN folder_map pm ON pm.old_id = ofo.parent_id
			WHERE nf.id = fmap.new_id AND ofo.parent_id IS NOT NULL
			RETURNING nf.id
		),
		dash_folder_fix AS (
			UPDATE model.dashboard_def nd
			SET folder_id = fmap.new_id
			FROM model.dashboard_def od
			JOIN folder_map fmap ON fmap.old_id = od.folder_id
			WHERE od.model_id=$1::uuid AND od.revision_id=$3::uuid
			  AND nd.model_id = od.model_id AND nd.name = od.name AND nd.revision_id = $2::uuid
			RETURNING nd.id
		)
		SELECT (SELECT count(*) FROM parent_fix) + (SELECT count(*) FROM dash_folder_fix)
	`, srcArgs...); err != nil {
		return "", fmt.Errorf("revision dashboard-folder remap failed: %w", err)
	}

	// Step G: copy integrations, remapping the target to the new
	// revision's copy of the grid/dashboard/form it points at.
	if _, err := tx.Exec(ctx, `
		INSERT INTO model.integration_def (model_id, name, type, target_type, target_id, config, revision_id)
		SELECT i.model_id, i.name, i.type, i.target_type,
			COALESCE(
				(SELECT ng.id FROM model.grid_def og
				 JOIN model.grid_def ng ON ng.model_id = og.model_id AND ng.name = og.name AND ng.revision_id = $2::uuid
				 WHERE og.id = i.target_id AND i.target_type = 'grid'),
				(SELECT ndd.id FROM model.dashboard_def odd
				 JOIN model.dashboard_def ndd ON ndd.model_id = odd.model_id AND ndd.name = odd.name AND ndd.revision_id = $2::uuid
				 WHERE odd.id = i.target_id AND i.target_type = 'dashboard'),
				(SELECT nf.id FROM model.form_def ofd
				 JOIN model.form_def nf ON nf.model_id = ofd.model_id AND nf.name = ofd.name AND nf.revision_id = $2::uuid
				 WHERE ofd.id = i.target_id AND i.target_type = 'form'),
				i.target_id),
			i.config, $2::uuid
		FROM model.integration_def i
		WHERE i.model_id=$1::uuid AND i.revision_id=$3::uuid
	`, srcArgs...); err != nil {
		return "", fmt.Errorf("revision integration copy failed: %w", err)
	}

	// Step H: copy workflow definitions and automation rules (application-
	// scoped, resolved through this model's application). context_schema
	// entries binding a context variable to a dimension are remapped to
	// the new revision's dimension; rules' workflow/form/grid refs are
	// remapped the same way.
	if _, err := tx.Exec(ctx, `
		WITH
		dim_map AS (
			SELECT o.id AS old_id, n.id AS new_id
			FROM model.dimension_def o
			JOIN model.dimension_def n ON n.model_id = o.model_id AND n.name = o.name AND n.revision_id = $2::uuid
			WHERE o.model_id=$1::uuid AND o.revision_id=$3::uuid
		),
		app AS (SELECT application_id FROM core.model WHERE id=$1::uuid),
		new_wfs AS (
			INSERT INTO workflow.workflow_def
			  (application_id, name, description, trigger_event, subject_type, subject_config, steps,
			   status, created_by, updated_by, published_at, archived_at, context_schema, revision_id)
			SELECT wd.application_id, wd.name, wd.description, wd.trigger_event, wd.subject_type,
				-- subject_config binds the workflow to a form ({"form_id"}) or a
				-- grid metric ({"grid_id","metric_id"}) of THIS revision; copied
				-- verbatim it kept pointing at the source revision's objects.
				COALESCE(wd.subject_config, '{}'::jsonb)
				|| COALESCE((SELECT jsonb_build_object('form_id', nfd.id::text) FROM model.form_def ofd
				             JOIN model.form_def nfd ON nfd.model_id = ofd.model_id AND nfd.name = ofd.name AND nfd.revision_id = $2::uuid
				             WHERE ofd.id::text = wd.subject_config->>'form_id'), '{}'::jsonb)
				|| COALESCE((SELECT jsonb_build_object('grid_id', ngd.id::text) FROM model.grid_def ogd
				             JOIN model.grid_def ngd ON ngd.model_id = ogd.model_id AND ngd.name = ogd.name AND ngd.revision_id = $2::uuid
				             WHERE ogd.id::text = wd.subject_config->>'grid_id'), '{}'::jsonb)
				|| COALESCE((SELECT jsonb_build_object('metric_id', nmd.id::text) FROM model.metric_def omd
				             JOIN model.metric_def nmd ON nmd.model_id = omd.model_id AND nmd.name = omd.name AND nmd.revision_id = $2::uuid
				             WHERE omd.id::text = wd.subject_config->>'metric_id'), '{}'::jsonb),
				wd.steps,
				wd.status, wd.created_by, wd.updated_by, wd.published_at, wd.archived_at,
				COALESCE((
					SELECT jsonb_agg(
						e.elem
						|| COALESCE((SELECT jsonb_build_object('dimension_id', dm.new_id::text) FROM dim_map dm WHERE dm.old_id::text = e.elem->>'dimension_id'), '{}'::jsonb)
						ORDER BY e.ord)
					FROM jsonb_array_elements(wd.context_schema) WITH ORDINALITY AS e(elem, ord)
				), '[]'::jsonb),
				$2::uuid
			FROM workflow.workflow_def wd
			WHERE wd.application_id = (SELECT application_id FROM app)
			  AND wd.revision_id = $3::uuid
			RETURNING id AS new_id, name
		),
		wf_map AS (
			SELECT o.id AS old_id, n.new_id
			FROM workflow.workflow_def o
			JOIN new_wfs n ON n.name = o.name
			WHERE o.application_id = (SELECT application_id FROM app)
			  AND o.revision_id = $3::uuid
		),
		new_rules AS (
			INSERT INTO workflow.automation_rule
			  (application_id, name, description, trigger_type, workflow_name, enabled,
			   workflow_def_id, source_form_id, source_grid_id, revision_id)
			SELECT ar.application_id, ar.name, ar.description, ar.trigger_type, ar.workflow_name, ar.enabled,
				wm.new_id,
				(SELECT nf.id FROM model.form_def ofd
				 JOIN model.form_def nf ON nf.model_id = ofd.model_id AND nf.name = ofd.name AND nf.revision_id = $2::uuid
				 WHERE ofd.id = ar.source_form_id),
				(SELECT ng.id FROM model.grid_def og
				 JOIN model.grid_def ng ON ng.model_id = og.model_id AND ng.name = og.name AND ng.revision_id = $2::uuid
				 WHERE og.id = ar.source_grid_id),
				$2::uuid
			FROM workflow.automation_rule ar
			LEFT JOIN wf_map wm ON wm.old_id = ar.workflow_def_id
			WHERE ar.application_id = (SELECT application_id FROM app)
			  AND ar.revision_id = $3::uuid
			RETURNING id
		)
		SELECT (SELECT count(*) FROM new_wfs) + (SELECT count(*) FROM new_rules)
	`, srcArgs...); err != nil {
		return "", fmt.Errorf("revision workflow/automation copy failed: %w", err)
	}

	// Step I: remap dashboard_widget.ref_id to the new revision's copy of
	// whatever it points at. Must run LAST — ref_id can point at a grid
	// (widget_type grid/chart/import — "import" resolves against grid_def
	// too, not integration_def, confirmed against
	// web/src/consoles/business/DashboardWidgets.tsx's ImportWidget), a
	// form, a metric, an integration, or an automation rule, and none of
	// those have their new-revision copies until Steps C/E/G/H above have
	// all run. Widgets are copied verbatim in Step A (before any of those
	// exist yet), so this step corrects ref_id afterward instead. Widgets
	// have no name, so old→new correlation uses
	// (dashboard, widget_type, sort_order) via the dashboard name-map;
	// unresolvable refs are left as-is via COALESCE, same tolerance Step E
	// already documents for form fields pointing at a deleted dimension.
	if _, err := tx.Exec(ctx, `
		WITH
		app AS (SELECT application_id FROM core.model WHERE id=$1::uuid),
		dash_map AS (
			SELECT od.id AS old_id, nd.id AS new_id
			FROM model.dashboard_def od
			JOIN model.dashboard_def nd ON nd.model_id = od.model_id AND nd.name = od.name AND nd.revision_id = $2::uuid
			WHERE od.model_id=$1::uuid AND od.revision_id=$3::uuid
		),
		widget_map AS (
			SELECT nw.id AS new_widget_id, ow.widget_type, ow.ref_id AS old_ref_id
			FROM model.dashboard_widget ow
			JOIN dash_map dm ON dm.old_id = ow.dashboard_id
			JOIN model.dashboard_widget nw ON nw.dashboard_id = dm.new_id
			                               AND nw.widget_type = ow.widget_type
			                               AND nw.sort_order = ow.sort_order
			WHERE ow.ref_id IS NOT NULL AND ow.ref_id <> ''
		),
		remap AS (
			UPDATE model.dashboard_widget w
			SET ref_id = COALESCE(
				(SELECT ng.id::text FROM model.grid_def og
				 JOIN model.grid_def ng ON ng.model_id = og.model_id AND ng.name = og.name AND ng.revision_id = $2::uuid
				 WHERE og.id::text = wm.old_ref_id AND wm.widget_type IN ('grid', 'chart', 'import')),
				(SELECT nf.id::text FROM model.form_def ofd
				 JOIN model.form_def nf ON nf.model_id = ofd.model_id AND nf.name = ofd.name AND nf.revision_id = $2::uuid
				 WHERE ofd.id::text = wm.old_ref_id AND wm.widget_type = 'form'),
				(SELECT nm.id::text FROM model.metric_def om
				 JOIN model.metric_def nm ON nm.model_id = om.model_id AND nm.name = om.name AND nm.revision_id = $2::uuid
				 WHERE om.id::text = wm.old_ref_id AND wm.widget_type = 'metric_kpi'),
				(SELECT ni.id::text FROM model.integration_def oi
				 JOIN model.integration_def ni ON ni.model_id = oi.model_id AND ni.name = oi.name AND ni.revision_id = $2::uuid
				 WHERE oi.id::text = wm.old_ref_id AND wm.widget_type = 'integration_button'),
				(SELECT nr.id::text FROM workflow.automation_rule oar
				 JOIN workflow.automation_rule nr ON nr.application_id = oar.application_id AND nr.name = oar.name AND nr.revision_id = $2::uuid
				 WHERE oar.id::text = wm.old_ref_id AND wm.widget_type = 'automation_button'
				   AND oar.application_id = (SELECT application_id FROM app)),
				wm.old_ref_id
			)
			FROM widget_map wm
			WHERE w.id = wm.new_widget_id
			RETURNING w.id
		)
		SELECT count(*) FROM remap
	`, srcArgs...); err != nil {
		return "", fmt.Errorf("revision dashboard-widget ref_id remap failed: %w", err)
	}

	// Step J: remap the metric/dimension IDs that live INSIDE
	// dashboard_widget.widget_props. Step I above covers ref_id only, but a
	// chart widget also stores its plotted dimension, its series metrics and
	// its saved context defaults in the JSON blob, and a metric_kpi widget
	// stores the dimension it is scoped to — all of them revision-scoped rows
	// that Step A gave new IDs. A stale chart config is rejected outright by
	// the chart endpoint; a stale kpi_scope fails silently, showing the
	// metric's whole-model total in a widget that promised one member's.
	// Done in Go rather than as jsonb surgery so the rewrite stays readable
	// and shares one implementation with the package-import path.
	{
		metricMap, err := loadRevisionNameMap(ctx, tx,
			`SELECT o.id::text, n.id::text FROM model.metric_def o
			 JOIN model.metric_def n ON n.model_id = o.model_id AND n.name = o.name AND n.revision_id = $2::uuid
			 WHERE o.model_id = $1::uuid AND o.revision_id = $3::uuid`, srcArgs)
		if err != nil {
			return "", fmt.Errorf("revision widget-props metric map failed: %w", err)
		}
		dimMap, err := loadRevisionNameMap(ctx, tx,
			`SELECT o.id::text, n.id::text FROM model.dimension_def o
			 JOIN model.dimension_def n ON n.model_id = o.model_id AND n.name = o.name AND n.revision_id = $2::uuid
			 WHERE o.model_id = $1::uuid AND o.revision_id = $3::uuid`, srcArgs)
		if err != nil {
			return "", fmt.Errorf("revision widget-props dimension map failed: %w", err)
		}

		type widgetProps struct {
			id    string
			props []byte
		}
		var pending []widgetProps
		propRows, err := tx.Query(ctx, `
			SELECT w.id::text, w.widget_props
			FROM model.dashboard_widget w
			JOIN model.dashboard_def d ON d.id = w.dashboard_id
			WHERE d.model_id = $1::uuid AND d.revision_id = $2::uuid AND w.widget_props IS NOT NULL
		`, modelID, newID)
		if err != nil {
			return "", fmt.Errorf("revision widget-props load failed: %w", err)
		}
		for propRows.Next() {
			var wp widgetProps
			if err := propRows.Scan(&wp.id, &wp.props); err != nil {
				propRows.Close()
				return "", fmt.Errorf("revision widget-props scan failed: %w", err)
			}
			pending = append(pending, wp)
		}
		propRows.Close()
		if err := propRows.Err(); err != nil {
			return "", fmt.Errorf("revision widget-props load failed: %w", err)
		}

		for _, wp := range pending {
			remapped, changed := modeltransfer.RemapWidgetPropsIDs(wp.props, metricMap, dimMap)
			if !changed {
				continue
			}
			if _, err := tx.Exec(ctx,
				`UPDATE model.dashboard_widget SET widget_props = $2::jsonb WHERE id = $1::uuid`,
				wp.id, string(remapped)); err != nil {
				return "", fmt.Errorf("revision widget-props remap failed: %w", err)
			}
		}
	}

	// Step K: carry business-role dashboard grants onto the copies. Business
	// users only ever see dashboards belonging to their model's ACTIVE
	// revision, so without this the first activation of a duplicated revision
	// revokes every dashboard from everyone at once — silently, since an
	// empty grant set is indistinguishable from "no dashboards yet".
	// Old→new correlation is by dashboard name, the same key Steps F and I
	// already use.
	if _, err := tx.Exec(ctx, `
		INSERT INTO identity.business_role_dashboard (role_id, dashboard_id)
		SELECT brd.role_id, nd.id
		FROM identity.business_role_dashboard brd
		JOIN model.dashboard_def od ON od.id = brd.dashboard_id
		JOIN model.dashboard_def nd ON nd.model_id = od.model_id AND nd.name = od.name AND nd.revision_id = $2::uuid
		WHERE od.model_id = $1::uuid AND od.revision_id = $3::uuid
		ON CONFLICT DO NOTHING
	`, srcArgs...); err != nil {
		return "", fmt.Errorf("revision dashboard-grant copy failed: %w", err)
	}

	return newID, nil
}

// loadRevisionNameMap runs a two-column (old_id, new_id) mapping query
// against the duplication transaction and collects it into a map. The query
// takes duplicateRevision's standard $1=modelID/$2=newRevisionID/$3=sourceRevisionID
// argument triple.
func loadRevisionNameMap(ctx context.Context, tx pgx.Tx, query string, args []any) (map[string]string, error) {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var oldID, newID string
		if err := rows.Scan(&oldID, &newID); err != nil {
			return nil, err
		}
		out[oldID] = newID
	}
	return out, rows.Err()
}

// activateRevision makes revisionID its model's active revision. Returns
// the model ID so callers that already need it (audit logging, etc.) don't
// have to look it up again.
func (h *handler) activateRevision(ctx context.Context, revisionID string) (modelID string, err error) {
	if err := h.db.QueryRow(ctx, `SELECT model_id::text FROM model.revision WHERE id=$1::uuid`, revisionID).Scan(&modelID); err != nil {
		return "", fmt.Errorf("revision not found")
	}
	// Dimensional time validation (spec §4.4): a revision cannot become
	// active while a time-series metric has no (or more than one) time
	// dimension, reads a series on another calendar, or sits in a cycle
	// time does not break. Formula save cannot check these — a metric's
	// dimensions come from grid placement — so publication does.
	if err := metricformula.ValidateTime(ctx, h.db.For(ctx), modelID, revisionID); err != nil {
		return "", err
	}
	if _, err := h.db.Exec(ctx, `
		UPDATE core.model
		SET active_revision_id   = $2::uuid,
		    active_revision_name = (SELECT name FROM model.revision WHERE id=$2::uuid)
		WHERE id=$1::uuid
	`, modelID, revisionID); err != nil {
		return "", err
	}
	if err := h.remapAccessRulesToRevision(ctx, modelID, revisionID); err != nil {
		return "", fmt.Errorf("remap access rules: %w", err)
	}
	return modelID, nil
}

// remapAccessRulesToRevision re-points identity.user_access_rule rows at the
// newly-activated revision's rows, matched by identity — (dimension name,
// member code) for member rules, metric name for metric rules.
//
// Rules store raw ref_id UUIDs, and every revision copy re-mints those UUIDs
// — so before this, each activation quietly stranded every member- and
// metric-level access rule on the previous revision's rows: writeguard and
// the hidden-member filters resolve refs against live definitions, a
// stranded ref matches nothing, and a "sees only Canada" user silently
// regained the whole world. Same disease as the widget-ref and rate-operand
// copy bugs fixed 2026-08-25, one layer up.
//
// A rule with no same-named counterpart in the new revision (member deleted
// or renamed there) is left untouched: it points at the old row, matches
// nothing, and therefore grants nothing — restrictions can only be lost by
// remapping wrongly, not by leaving a dangling restriction in place.
func (h *handler) remapAccessRulesToRevision(ctx context.Context, modelID, revisionID string) error {
	if _, err := h.db.Exec(ctx, `
		UPDATE identity.user_access_rule r
		SET ref_id = nm.id::text
		FROM model.dimension_member om
		JOIN model.dimension_def od ON od.id = om.dimension_id
		JOIN model.dimension_def nd ON nd.model_id = od.model_id
		                           AND lower(nd.name) = lower(od.name)
		                           AND nd.revision_id = $2::uuid
		JOIN model.dimension_member nm ON nm.dimension_id = nd.id AND nm.code = om.code
		WHERE r.rule_type = 'dimension_member'
		  AND r.ref_id = om.id::text
		  AND od.model_id = $1::uuid
		  AND od.revision_id IS DISTINCT FROM $2::uuid
	`, modelID, revisionID); err != nil {
		return fmt.Errorf("member rules: %w", err)
	}
	if _, err := h.db.Exec(ctx, `
		UPDATE identity.user_access_rule r
		SET ref_id = nm.id::text
		FROM model.metric_def om
		JOIN model.metric_def nm ON nm.model_id = om.model_id
		                        AND lower(nm.name) = lower(om.name)
		                        AND nm.revision_id = $2::uuid
		WHERE r.rule_type = 'metric'
		  AND r.ref_id = om.id::text
		  AND om.model_id = $1::uuid
		  AND om.revision_id IS DISTINCT FROM $2::uuid
	`, modelID, revisionID); err != nil {
		return fmt.Errorf("metric rules: %w", err)
	}
	return nil
}

func (h *handler) developerRevisionAction(w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, "/api/developer/revisions/")
	parts := strings.SplitN(tail, "/", 2)
	id := parts[0]
	subPath := ""
	if len(parts) == 2 {
		subPath = parts[1]
	}
	ctx := r.Context()

	// Activating a revision decides which definitions every business user of
	// that model sees, so an unscoped {id} here was a cross-tenant WRITE, not
	// just a read.
	if !h.requireResourceAccess(w, r, "revision", id) {
		return
	}

	if subPath == "activate" && r.Method == http.MethodPut {
		if _, err := h.activateRevision(ctx, id); err != nil {
			if metricformula.IsValidationError(err) {
				jsonErr(w, err, http.StatusBadRequest)
				return
			}
			jsonErr(w, err, http.StatusNotFound)
			return
		}
		act, _ := h.resolveActor(ctx, r)
		actorID, actorRole := "", ""
		if act != nil {
			actorID, actorRole = act.UserID, strings.Join(act.Roles, ",")
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryModelChange, EventType: auditlog.EventRevisionActivated,
			ActorUserID: actorID, ActorRole: actorRole,
			ResourceType: "revision", ResourceID: id, RevisionID: id,
		})
		jsonOK(w, map[string]string{"status": "activated"})
		return
	}

	if r.Method != http.MethodDelete {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	// Prevent deleting the active revision
	var isActive bool
	_ = h.db.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM core.model WHERE active_revision_id=$1::uuid
		)`, id).Scan(&isActive)
	if isActive {
		jsonErr(w, fmt.Errorf("cannot delete the active revision"), http.StatusConflict)
		return
	}
	if _, err := h.db.Exec(ctx, `DELETE FROM model.revision WHERE id=$1::uuid`, id); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	delAct, _ := h.resolveActor(ctx, r)
	delActorID, delActorRole := "", ""
	if delAct != nil {
		delActorID, delActorRole = delAct.UserID, strings.Join(delAct.Roles, ",")
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryModelChange, EventType: auditlog.EventRevisionDeleted,
		ActorUserID: delActorID, ActorRole: delActorRole,
		// RevisionID intentionally left unset: the FK on audit_event.revision_id
		// (migration 060) would reject a reference to the revision this same
		// row is announcing the deletion of. resource_id already carries it
		// (plain TEXT, no FK) — that's the durable record of which revision
		// this was.
		ResourceType: "revision", ResourceID: id,
	})
	jsonOK(w, map[string]string{"status": "deleted"})
}

// ── /api/developer/model ─────────────────────────────────────────────────────

type devMetric struct {
	ID             string  `json:"id"`
	Name           string  `json:"name"`
	Label          string  `json:"label"`
	IsInput        bool    `json:"is_input"`
	Formula        *string `json:"formula,omitempty"`
	AggRule        string  `json:"agg_rule"`
	Format         string  `json:"format"`
	FormatDecimals int     `json:"format_decimals"`
	FormatCurrency string  `json:"format_currency"`
	// TimeSummary is how the metric aggregates across a time dimension
	// (spec §3.3); agg_rule stays the rule for every other dimension.
	TimeSummary string `json:"time_summary"`
	// Operands for agg_rule "rate", so the console can show which two metrics
	// the ratio currently divides.
	AggNumeratorMetricID   string   `json:"agg_numerator_metric_id,omitempty"`
	AggDenominatorMetricID string   `json:"agg_denominator_metric_id,omitempty"`
	DependsOn              []string `json:"depends_on"`
	DependedBy             []string `json:"depended_by"`
	// CalcError is set when this metric's most recent calculation attempt
	// failed (runtime.metric_partition_state.status='error') — e.g. a
	// formula referencing an identifier that no longer resolves after a
	// rename. RecalcAffected already persists this via Store.MarkError,
	// but nothing read it back out before, so a frozen calc value had no
	// visible indication anything was wrong.
	CalcError *string `json:"calc_error,omitempty"`
}

type devModelResponse struct {
	AppName   string      `json:"app_name"`
	ModelName string      `json:"model_name"`
	ModelID   string      `json:"model_id"`
	Metrics   []devMetric `json:"metrics"`
}

func (h *handler) developerModel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	var appName, modelName string
	if err := h.db.QueryRow(ctx, `
		SELECT a.name, m.name
		FROM core.application a
		JOIN core.model m ON m.application_id = a.id
		WHERE m.id = $1::uuid
	`, modelID).Scan(&appName, &modelName); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}

	// Filter by the requested revision; when none specified, show all metrics for the model
	revisionID := r.URL.Query().Get("revision_id")
	revFilter := `TRUE`
	revArgs := []any{modelID}
	if revisionID != "" {
		revFilter = `m.revision_id = $2::uuid`
		revArgs = []any{modelID, revisionID}
	}
	rows, err := h.db.Query(ctx, `
		SELECT m.id::text, m.name, m.is_input, m.formula, m.agg_rule,
		       COALESCE(m.agg_numerator_metric_id::text,''), COALESCE(m.agg_denominator_metric_id::text,''),
		       m.format, m.format_decimals, m.format_currency, m.time_summary,
		       COALESCE(
		           (SELECT string_agg(dep.name, ',')
		            FROM model.calc_dependency cd
		            JOIN model.metric_def dep ON dep.id = cd.depends_on_metric_id
		            WHERE cd.metric_id = m.id), ''
		       ) AS depends_on,
		       COALESCE(
		           (SELECT string_agg(parent.name, ',')
		            FROM model.calc_dependency cd2
		            JOIN model.metric_def parent ON parent.id = cd2.metric_id
		            WHERE cd2.depends_on_metric_id = m.id), ''
		       ) AS depended_by,
		       (SELECT s.error FROM runtime.metric_partition_state s
		        WHERE s.metric_id = m.id AND s.revision_id = m.revision_id AND s.status = 'error'
		        ORDER BY s.updated_at DESC LIMIT 1) AS calc_error
		FROM model.metric_def m
		WHERE m.model_id = $1::uuid AND `+revFilter+`
		ORDER BY m.is_input DESC, m.name
	`, revArgs...)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var metrics []devMetric
	for rows.Next() {
		var dm devMetric
		var dependsOnCSV, dependedByCSV string
		if err := rows.Scan(&dm.ID, &dm.Name, &dm.IsInput, &dm.Formula, &dm.AggRule,
			&dm.AggNumeratorMetricID, &dm.AggDenominatorMetricID,
			&dm.Format, &dm.FormatDecimals, &dm.FormatCurrency, &dm.TimeSummary, &dependsOnCSV, &dependedByCSV, &dm.CalcError); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		dm.Label = toLabel(dm.Name)
		if dependsOnCSV != "" {
			dm.DependsOn = strings.Split(dependsOnCSV, ",")
		} else {
			dm.DependsOn = []string{}
		}
		if dependedByCSV != "" {
			dm.DependedBy = strings.Split(dependedByCSV, ",")
		} else {
			dm.DependedBy = []string{}
		}
		metrics = append(metrics, dm)
	}
	if err := rows.Err(); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if metrics == nil {
		metrics = []devMetric{}
	}

	jsonOK(w, devModelResponse{
		AppName:   appName,
		ModelName: modelName,
		ModelID:   modelID,
		Metrics:   metrics,
	})
}

// ── /api/developer/metrics ────────────────────────────────────────────────────

type addMetricReq struct {
	Name       string `json:"name"`
	IsInput    bool   `json:"is_input"`
	Formula    string `json:"formula"` // empty for input metrics
	RevisionID string `json:"revision_id"`
	AggRule    string `json:"agg_rule"`
	// Required when AggRule is "rate": the total becomes numerator ÷
	// denominator, taken from these two metrics rather than from this one's
	// own member values.
	AggNumeratorMetricID   string `json:"agg_numerator_metric_id"`
	AggDenominatorMetricID string `json:"agg_denominator_metric_id"`
	Format                 string `json:"format"`
	FormatDecimals         int    `json:"format_decimals"`
	FormatCurrency         string `json:"format_currency"`
	// TimeSummary: aggregation across a time dimension (spec §3.3). Empty
	// defaults to sum.
	TimeSummary string `json:"time_summary"`
}

func (h *handler) developerMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	var req addMetricReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		jsonErr(w, fmt.Errorf("name is required"), http.StatusBadRequest)
		return
	}
	if !req.IsInput && req.Formula == "" {
		jsonErr(w, fmt.Errorf("formula is required for calculated metrics"), http.StatusBadRequest)
		return
	}

	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}

	// Validate the formula before inserting — parse, function names,
	// self-reference, cycles and revision-scoped references, in one shared
	// service (see internal/metricformula). The previous check ran through
	// extractFormulaRefs, which returns nil on a parse error, so anything
	// that didn't parse produced zero refs and was accepted.
	var formulaEdges []metricformula.Edge
	if !req.IsInput && req.Formula != "" {
		res, vErr := metricformula.Validate(ctx, h.db.For(ctx), metricformula.Request{
			ModelID: modelID, RevisionID: req.RevisionID, Name: req.Name, Formula: req.Formula,
		})
		if vErr != nil {
			var invalid *metricformula.ValidationError
			if errors.As(vErr, &invalid) {
				jsonErr(w, vErr, http.StatusBadRequest)
			} else {
				jsonErr(w, vErr, http.StatusInternalServerError)
			}
			return
		}
		formulaEdges = res.Edges
	}

	// Insert metric
	var newID string
	formulaVal := &req.Formula
	if req.IsInput {
		formulaVal = nil
	}
	if req.Format == "" {
		req.Format = "number"
	}
	if req.FormatCurrency == "" {
		req.FormatCurrency = "$"
	}
	if req.AggRule == "" {
		req.AggRule = "sum"
	}
	if req.TimeSummary == "" {
		req.TimeSummary = "sum"
	}
	if !timedim.ValidTimeSummary(req.TimeSummary) {
		jsonErr(w, fmt.Errorf("time_summary must be one of %s", strings.Join(timedim.TimeSummaries, ", ")), http.StatusBadRequest)
		return
	}
	if err := metricformula.ValidateAggRule(req.AggRule, req.IsInput, req.AggNumeratorMetricID, req.AggDenominatorMetricID, ""); err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	if err := metricformula.ValidateAggOperands(ctx, h.db.For(ctx), req.AggRule, modelID, req.RevisionID, req.AggNumeratorMetricID, req.AggDenominatorMetricID); err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}

	if cid := h.customerOfModel(ctx, modelID); cid != "" && h.plans != nil {
		if err := h.plans.CheckMetrics(ctx, h.db.For(ctx), cid, modelID, 1); err != nil {
			h.jsonLimitErr(w, err)
			return
		}
	}

	// The metric and its dependency edges go in together: the edges used to
	// be a best-effort follow-up that resolved each reference again (this
	// time model-wide, so it could bind to another revision's metric) and
	// discarded every insert error, leaving a saved metric with a silently
	// incomplete graph.
	tx, err := h.db.Begin(ctx)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck

	var metricInsertErr error
	if req.RevisionID != "" {
		metricInsertErr = tx.QueryRow(ctx, `
			INSERT INTO model.metric_def (model_id, name, formula, is_input, revision_id, agg_rule, format, format_decimals, format_currency,
			                              agg_numerator_metric_id, agg_denominator_metric_id, time_summary)
			VALUES ($1::uuid, $2, $3, $4, $5::uuid, $6, $7, $8, $9, NULLIF($10,'')::uuid, NULLIF($11,'')::uuid, $12)
			RETURNING id::text
		`, modelID, req.Name, formulaVal, req.IsInput, req.RevisionID, req.AggRule, req.Format, req.FormatDecimals, req.FormatCurrency,
			req.AggNumeratorMetricID, req.AggDenominatorMetricID, req.TimeSummary).Scan(&newID)
	} else {
		metricInsertErr = tx.QueryRow(ctx, `
			INSERT INTO model.metric_def (model_id, name, formula, is_input, agg_rule, format, format_decimals, format_currency,
			                              agg_numerator_metric_id, agg_denominator_metric_id, time_summary)
			VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, NULLIF($9,'')::uuid, NULLIF($10,'')::uuid, $11)
			RETURNING id::text
		`, modelID, req.Name, formulaVal, req.IsInput, req.AggRule, req.Format, req.FormatDecimals, req.FormatCurrency,
			req.AggNumeratorMetricID, req.AggDenominatorMetricID, req.TimeSummary).Scan(&newID)
	}
	if metricInsertErr != nil {
		jsonErr(w, fmt.Errorf("insert metric: %w", metricInsertErr), http.StatusInternalServerError)
		return
	}

	if err := metricformula.WriteDependencies(ctx, tx, newID, formulaEdges); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}

	act, _ := h.resolveActor(ctx, r)
	actorID, actorRole := "", ""
	if act != nil {
		actorID, actorRole = act.UserID, strings.Join(act.Roles, ",")
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryModelChange, EventType: auditlog.EventMetricCreated,
		ActorUserID: actorID, ActorRole: actorRole,
		ResourceType: "metric", ResourceID: newID, RevisionID: req.RevisionID,
		Metadata: map[string]string{"name": req.Name, "revision_id": req.RevisionID},
	})

	go h.autoMigrate(context.Background(), modelID) //nolint:contextcheck
	jsonOK(w, map[string]string{"id": newID, "status": "created"})
}

// extractFormulaRefs returns all identifier references from a formula string.
// Handles both new Excel-style syntax (=hc_cost + hr_cost) and legacy {name} syntax.
// ── /api/formula/refs ─────────────────────────────────────────────────────────

func (h *handler) formulaRefs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Formula string `json:"formula"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
		return
	}
	refs, err := formula.ExtractRefs(body.Formula)
	if err != nil {
		jsonOK(w, map[string]any{"refs": []string{}, "error": err.Error()})
		return
	}
	if refs == nil {
		refs = []string{}
	}
	jsonOK(w, map[string]any{"refs": refs})
}

// ── /api/workflow/history ─────────────────────────────────────────────────────

type historyStep struct {
	ID          string  `json:"id"`
	StepDefID   string  `json:"step_def_id"`
	StepName    string  `json:"step_name"`
	StepType    string  `json:"step_type"`
	Status      string  `json:"status"`
	Decision    *string `json:"decision,omitempty"`
	Comment     *string `json:"comment,omitempty"`
	AssigneeID  *string `json:"assignee_user_id,omitempty"`
	CompletedAt *string `json:"completed_at,omitempty"`
}

type historyInstance struct {
	ID          string         `json:"id"`
	WFName      string         `json:"workflow_name"`
	Status      string         `json:"status"`
	Context     map[string]any `json:"context"`
	StartedAt   string         `json:"started_at"`
	CompletedAt *string        `json:"completed_at,omitempty"`
	Steps       []historyStep  `json:"steps"`
	// Same resolution as taskRow.ContextDisplay — history readers need
	// "Canada", not "CA", exactly as much as approvers do.
	ContextDisplay []taskContextEntry `json:"context_display,omitempty"`
}

func (h *handler) workflowHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rows, err := h.db.Query(ctx, `
		SELECT wi.id::text, wd.name, wi.status::text, wi.context,
		       wi.started_at::text,
		       CASE WHEN wi.completed_at IS NOT NULL THEN wi.completed_at::text END,
		       COALESCE(wi.context_schema_snapshot, wd.context_schema)
		FROM workflow.workflow_instance wi
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		ORDER BY wi.started_at DESC
		LIMIT 50
	`)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var instances []historyInstance
	for rows.Next() {
		var inst historyInstance
		var ctxJSON, schemaJSON []byte
		if err := rows.Scan(&inst.ID, &inst.WFName, &inst.Status, &ctxJSON, &inst.StartedAt, &inst.CompletedAt, &schemaJSON); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		_ = json.Unmarshal(ctxJSON, &inst.Context)
		inst.ContextDisplay = h.contextDisplay(ctx, schemaJSON, inst.Context)
		instances = append(instances, inst)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}

	// Load steps for each instance
	for i, inst := range instances {
		sRows, err := h.db.Query(ctx, `
			SELECT ws.id::text, ws.step_def_id, ws.status::text, ws.decision, ws.comment,
			       ws.assignee_user_id::text,
			       CASE WHEN ws.completed_at IS NOT NULL THEN ws.completed_at::text END,
			       COALESCE(
			           (SELECT elem->>'name' FROM jsonb_array_elements(COALESCE(wi.steps_snapshot, wd.steps)) AS elem
			            WHERE elem->>'id' = ws.step_def_id LIMIT 1),
			           ws.step_def_id),
			       CASE (SELECT elem->>'type' FROM jsonb_array_elements(COALESCE(wi.steps_snapshot, wd.steps)) AS elem
			                  WHERE elem->>'id' = ws.step_def_id LIMIT 1)
			            WHEN '2' THEN 'approval'   WHEN 'approval'      THEN 'approval'
			            WHEN '3' THEN 'notification' WHEN 'notification' THEN 'notification'
			            WHEN '4' THEN 'condition'  WHEN 'condition'     THEN 'condition'
			            ELSE 'task' END
			FROM workflow.workflow_step ws
			JOIN workflow.workflow_instance wi ON wi.id = ws.instance_id
			JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
			WHERE ws.instance_id = $1::uuid
			ORDER BY ws.created_at
		`, inst.ID)
		if err != nil {
			continue
		}
		for sRows.Next() {
			var s historyStep
			if err := sRows.Scan(&s.ID, &s.StepDefID, &s.Status, &s.Decision, &s.Comment, &s.AssigneeID, &s.CompletedAt, &s.StepName, &s.StepType); err != nil {
				continue
			}
			instances[i].Steps = append(instances[i].Steps, s)
		}
		sRows.Close()
	}

	if instances == nil {
		instances = []historyInstance{}
	}
	jsonOK(w, instances)
}

// workflowMyHistory returns only instances where the caller is a participant.
// workflowDefinitions serves GET /api/workflow/definitions — the published
// workflow definitions of the caller's application, WITH their context
// schemas. This is what lets a business user define what they are
// submitting: the Planning workspace renders a start dialog from each def's
// context_schema (a "Dimension member" variable becomes a member picker,
// already filtered by the caller's hidden rules via /api/dimensions), so
// scope is chosen at submit time instead of being frozen into a
// per-scope dashboard button. Read-only and role "any" for the same reason
// /api/workflow/instances is: starting a workflow is a business action, and
// ResolveStartContext re-validates whatever context arrives.
func (h *handler) workflowDefinitions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, err := h.resolveActor(ctx, r); err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	var appID, activeRev string
	if err := h.db.QueryRow(ctx, `
		SELECT application_id::text, COALESCE(active_revision_id::text,'')
		FROM core.model WHERE id=$1::uuid
	`, modelID).Scan(&appID, &activeRev); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	rows, err := h.db.Query(ctx, `
		SELECT id::text, name, COALESCE(description,''), trigger_event, COALESCE(context_schema, '[]'::jsonb)
		FROM workflow.workflow_def
		WHERE application_id=$1::uuid AND status='published'
		  AND (revision_id IS NULL OR revision_id::text=$2)
		ORDER BY name
	`, appID, activeRev)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type defRow struct {
		ID            string          `json:"id"`
		Name          string          `json:"name"`
		Description   string          `json:"description"`
		TriggerEvent  string          `json:"trigger_event"`
		ContextSchema json.RawMessage `json:"context_schema"`
	}
	out := []defRow{}
	for rows.Next() {
		var d defRow
		if err := rows.Scan(&d.ID, &d.Name, &d.Description, &d.TriggerEvent, &d.ContextSchema); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, out)
}

func (h *handler) workflowMyHistory(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}

	rows, err := h.db.Query(ctx, `
		SELECT wi.id::text, wd.name, wi.status::text, wi.context,
		       wi.started_at::text,
		       CASE WHEN wi.completed_at IS NOT NULL THEN wi.completed_at::text END,
		       COALESCE(wi.context_schema_snapshot, wd.context_schema)
		FROM workflow.workflow_instance wi
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		WHERE wi.started_by = $1::uuid
		   OR EXISTS (
		       SELECT 1 FROM workflow.workflow_step ws
		       WHERE ws.instance_id = wi.id AND ws.assignee_user_id = $1::uuid
		   )
		   -- A role-matched notification recipient (business_role member or
		   -- platform-role match, resolveNotificationRecipients in
		   -- internal/workflow/store.go) is neither the requester nor a step
		   -- assignee — without this, "navigate to workflow" from their
		   -- notification center would silently find nothing here.
		   OR EXISTS (
		       SELECT 1 FROM notification.notification n
		       WHERE n.recipient_user_id = $1::uuid
		         AND n.resource_type = 'workflow_instance' AND n.resource_id = wi.id::text
		   )
		ORDER BY wi.started_at DESC
		LIMIT 50
	`, a.UserID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var instances []historyInstance
	for rows.Next() {
		var inst historyInstance
		var ctxJSON, schemaJSON []byte
		if err := rows.Scan(&inst.ID, &inst.WFName, &inst.Status, &ctxJSON, &inst.StartedAt, &inst.CompletedAt, &schemaJSON); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		_ = json.Unmarshal(ctxJSON, &inst.Context)
		inst.Context = h.redactHiddenContext(ctx, a.UserID, schemaJSON, inst.Context)
		inst.ContextDisplay = h.contextDisplay(ctx, schemaJSON, inst.Context)
		instances = append(instances, inst)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}

	for i, inst := range instances {
		sRows, err := h.db.Query(ctx, `
			SELECT ws.id::text, ws.step_def_id, ws.status::text, ws.decision, ws.comment,
			       ws.assignee_user_id::text,
			       CASE WHEN ws.completed_at IS NOT NULL THEN ws.completed_at::text END,
			       COALESCE(
			           (SELECT elem->>'name' FROM jsonb_array_elements(COALESCE(wi.steps_snapshot, wd.steps)) AS elem
			            WHERE elem->>'id' = ws.step_def_id LIMIT 1),
			           ws.step_def_id),
			       CASE (SELECT elem->>'type' FROM jsonb_array_elements(COALESCE(wi.steps_snapshot, wd.steps)) AS elem
			                  WHERE elem->>'id' = ws.step_def_id LIMIT 1)
			            WHEN '2' THEN 'approval'   WHEN 'approval'      THEN 'approval'
			            WHEN '3' THEN 'notification' WHEN 'notification' THEN 'notification'
			            WHEN '4' THEN 'condition'  WHEN 'condition'     THEN 'condition'
			            ELSE 'task' END
			FROM workflow.workflow_step ws
			JOIN workflow.workflow_instance wi ON wi.id = ws.instance_id
			JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
			WHERE ws.instance_id = $1::uuid
			ORDER BY ws.created_at
		`, inst.ID)
		if err != nil {
			continue
		}
		for sRows.Next() {
			var s historyStep
			if err := sRows.Scan(&s.ID, &s.StepDefID, &s.Status, &s.Decision, &s.Comment, &s.AssigneeID, &s.CompletedAt, &s.StepName, &s.StepType); err != nil {
				continue
			}
			instances[i].Steps = append(instances[i].Steps, s)
		}
		sRows.Close()
	}

	if instances == nil {
		instances = []historyInstance{}
	}
	jsonOK(w, instances)
}

// workflowStartInstance handles POST /api/workflow/instances — starts a workflow instance by def ID.
func (h *handler) workflowStartInstance(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}
	// Separation of duties (owner-decided): business ADMINS decide approval
	// requests, they do not submit them. An actor whose business-facing role
	// is only business_admin is refused; someone who also holds a submitter
	// or builder role (business_user, developer, admins) legitimately wears
	// that other hat.
	if a.hasRole("business_admin") && !a.hasRole("business_user") && !a.hasRole("developer") && !a.hasRole("tenant_admin") && !a.hasRole("platform_admin") {
		jsonErr(w, fmt.Errorf("approvers do not start approval workflows — a business user submits the request, you decide it"), http.StatusForbidden)
		return
	}

	var body struct {
		WorkflowDefID string            `json:"workflow_def_id"`
		Context       map[string]string `json:"context"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.WorkflowDefID == "" {
		jsonErr(w, fmt.Errorf("workflow_def_id is required"), http.StatusBadRequest)
		return
	}

	ws := workflow.NewStore(h.db.For(ctx))

	if body.Context == nil {
		body.Context = map[string]string{}
	}
	// ResolveStartContext is the one shared implementation of "is this
	// workflow startable right now, and with what final context" — also
	// used by every automation_button click (TriggerRule). See its doc
	// comment in internal/workflow/store.go for the published/RACI/dedup
	// checks it runs.
	resolvedContext, err := ws.ResolveStartContext(ctx, body.WorkflowDefID, a.UserID, body.Context)
	if err != nil {
		switch {
		case errors.Is(err, workflow.ErrWorkflowNotPublished):
			jsonErr(w, err, http.StatusBadRequest)
		case errors.Is(err, workflow.ErrNoRACIScope):
			jsonErr(w, err, http.StatusForbidden)
		case errors.Is(err, workflow.ErrHiddenScope):
			jsonErr(w, err, http.StatusForbidden)
		case errors.Is(err, workflow.ErrDuplicateInstance):
			jsonErr(w, err, http.StatusConflict)
		case errors.Is(err, workflow.ErrUnknownMetric), errors.Is(err, workflow.ErrMissingContext), errors.Is(err, workflow.ErrUnknownMember):
			// Bad start context, not a missing workflow: these used to fall
			// through to 404 "workflow not found".
			jsonErr(w, err, http.StatusBadRequest)
		default:
			jsonErr(w, fmt.Errorf("workflow not found: %w", err), http.StatusNotFound)
		}
		return
	}

	instance, err := ws.StartWorkflow(ctx, body.WorkflowDefID, a.UserID, resolvedContext)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}

	var appID string
	_ = h.db.QueryRow(ctx, `SELECT application_id::text FROM workflow.workflow_def WHERE id=$1::uuid`, body.WorkflowDefID).Scan(&appID)
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryDataChange, EventType: auditlog.EventWorkflowInstanceStarted,
		ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
		ApplicationID: appID, ResourceType: "workflow_instance", ResourceID: instance.Id,
		Metadata: map[string]string{"workflow_def_id": body.WorkflowDefID},
	})
	jsonOK(w, map[string]string{"instance_id": instance.Id})
}

// workflowInstanceAction handles PATCH /api/workflow/instances/{id} — admin status override.
func (h *handler) workflowInstanceAction(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPatch {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	instanceID := strings.TrimPrefix(r.URL.Path, "/api/workflow/instances/")
	if instanceID == "" {
		jsonErr(w, fmt.Errorf("missing instance id"), http.StatusBadRequest)
		return
	}

	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Status == "" {
		jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
		return
	}

	allowed := map[string]bool{"running": true, "completed": true, "cancelled": true, "failed": true}
	if !allowed[body.Status] {
		jsonErr(w, fmt.Errorf("invalid status"), http.StatusBadRequest)
		return
	}

	var appID string
	_ = h.db.QueryRow(ctx, `
		SELECT wd.application_id::text FROM workflow.workflow_instance wi
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		WHERE wi.id = $1::uuid`, instanceID).Scan(&appID)

	tx, err := h.db.Begin(ctx)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if body.Status == "running" {
		// Clear completed_at and set instance back to running
		_, err = tx.Exec(ctx, `
			UPDATE workflow.workflow_instance
			SET status = 'running', completed_at = NULL
			WHERE id = $1::uuid
		`, instanceID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		// Reactivate the most recent non-completed step so it appears in Inbox
		_, _ = tx.Exec(ctx, `
			UPDATE workflow.workflow_step
			SET status = 'in_progress', completed_at = NULL
			WHERE id = (
				SELECT id FROM workflow.workflow_step
				WHERE instance_id = $1::uuid AND status != 'in_progress'
				ORDER BY created_at DESC LIMIT 1
			)
		`, instanceID)
	} else {
		completedAt := "NULL"
		var execErr error
		if body.Status == "completed" {
			_, execErr = tx.Exec(ctx, `
				UPDATE workflow.workflow_instance
				SET status = $2, completed_at = now()
				WHERE id = $1::uuid
			`, instanceID, body.Status)
		} else {
			_, execErr = tx.Exec(ctx, `
				UPDATE workflow.workflow_instance
				SET status = $2, completed_at = NULL
				WHERE id = $1::uuid
			`, instanceID, body.Status)
			_ = completedAt
		}
		if execErr != nil {
			jsonErr(w, execErr, http.StatusInternalServerError)
			return
		}
		// Cancel any remaining in_progress or pending steps. workflow.step_status
		// has no 'cancelled' value (pending/in_progress/completed/rejected/
		// skipped) — 'skipped' is the established status for "never executed
		// because the instance moved on without it" (see the condition/approval
		// branch-not-taken case in internal/workflow/store.go).
		_, _ = tx.Exec(ctx, `
			UPDATE workflow.workflow_step
			SET status = 'skipped'
			WHERE instance_id = $1::uuid AND status IN ('in_progress', 'pending')
		`, instanceID)
	}

	if err := tx.Commit(ctx); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if a, e := h.resolveActor(ctx, r); e == nil {
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryDataChange, EventType: auditlog.EventWorkflowInstanceStatusOverridden,
			ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
			ApplicationID: appID, ResourceType: "workflow_instance", ResourceID: instanceID,
			Metadata: map[string]string{"status": body.Status},
		})
	}
	jsonOK(w, map[string]string{"status": "ok"})
}

// ── /api/developer/dimensions ────────────────────────────────────────────────

type devMember struct {
	ID             string  `json:"id"`
	Code           string  `json:"code"`
	Label          string  `json:"label"`
	ParentMemberID *string `json:"parent_member_id"`
	// Time members only (spec §3.2): the period and the server-owned
	// chronological ordinal time functions move by.
	PeriodStart *string `json:"period_start,omitempty"`
	PeriodEnd   *string `json:"period_end,omitempty"`
	TimeIndex   *int    `json:"time_index,omitempty"`
	// Per-member property values (dimension_member.properties) — the
	// Dimensions tab shows them and property-derived dimensions group by
	// them. Populated by the developer endpoint; omitted elsewhere.
	Properties map[string]string `json:"properties,omitempty"`
}

type devDimension struct {
	ID                string      `json:"id"`
	Name              string      `json:"name"`
	AggRule           string      `json:"agg_rule"`
	ParentDimensionID *string     `json:"parent_dimension_id"`
	DimensionType     string      `json:"dimension_type"` // "standard" | "time" — explicit, immutable
	TimeGranularity   *string     `json:"time_granularity,omitempty"`
	FiscalYearStart   *int        `json:"fiscal_year_start_month,omitempty"`
	Members           []devMember `json:"members"`
}

func (h *handler) developerDimensions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}

	revisionID := r.URL.Query().Get("revision_id")

	if r.Method == http.MethodPost {
		var body struct {
			Name              string  `json:"name"`
			AggRule           string  `json:"agg_rule"`
			RevisionID        string  `json:"revision_id"`
			ParentDimensionID *string `json:"parent_dimension_id"`
			// Time dimension marker (spec §4.1). Omitted = standard; a
			// name like "month" never implies time.
			DimensionType   string `json:"dimension_type"`
			TimeGranularity string `json:"time_granularity"`
			FiscalYearStart int    `json:"fiscal_year_start_month"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
		if body.Name == "" {
			jsonErr(w, fmt.Errorf("name required"), http.StatusBadRequest)
			return
		}
		if body.AggRule == "" {
			body.AggRule = "sum"
		}
		timeCfg := timedim.Config{Type: body.DimensionType, Granularity: body.TimeGranularity, FiscalYearStartMonth: body.FiscalYearStart}
		if err := timedim.ValidateConfig(&timeCfg); err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		if timeCfg.Type == timedim.TypeTime && body.ParentDimensionID != nil {
			jsonErr(w, fmt.Errorf("a time dimension cannot have a parent dimension"), http.StatusBadRequest)
			return
		}
		if body.ParentDimensionID != nil {
			var parentModelID string
			if err := h.db.QueryRow(ctx, `SELECT model_id::text FROM model.dimension_def WHERE id=$1::uuid`, *body.ParentDimensionID).Scan(&parentModelID); err != nil {
				jsonErr(w, fmt.Errorf("parent dimension not found"), http.StatusBadRequest)
				return
			}
			if parentModelID != modelID {
				jsonErr(w, fmt.Errorf("parent dimension must belong to the same model"), http.StatusBadRequest)
				return
			}
		}
		var newID string
		var dimInsertErr error
		var granularity *string
		var fiscalStart *int
		if timeCfg.Type == timedim.TypeTime {
			granularity, fiscalStart = &timeCfg.Granularity, &timeCfg.FiscalYearStartMonth
		}
		if body.RevisionID != "" {
			dimInsertErr = h.db.QueryRow(ctx, `
				INSERT INTO model.dimension_def (model_id, name, agg_rule, revision_id, parent_dimension_id, dimension_type, time_granularity, fiscal_year_start_month)
				VALUES ($1::uuid, $2, $3, $4::uuid, $5::uuid, $6, $7, $8) RETURNING id::text
			`, modelID, body.Name, body.AggRule, body.RevisionID, body.ParentDimensionID, timeCfg.Type, granularity, fiscalStart).Scan(&newID)
		} else {
			dimInsertErr = h.db.QueryRow(ctx, `
				INSERT INTO model.dimension_def (model_id, name, agg_rule, parent_dimension_id, dimension_type, time_granularity, fiscal_year_start_month)
				VALUES ($1::uuid, $2, $3, $4::uuid, $5, $6, $7) RETURNING id::text
			`, modelID, body.Name, body.AggRule, body.ParentDimensionID, timeCfg.Type, granularity, fiscalStart).Scan(&newID)
		}
		if dimInsertErr != nil {
			jsonErr(w, fmt.Errorf("insert dimension: %w", dimInsertErr), http.StatusInternalServerError)
			return
		}
		dimAct, _ := h.resolveActor(ctx, r)
		dimActorID, dimActorRole := "", ""
		if dimAct != nil {
			dimActorID, dimActorRole = dimAct.UserID, strings.Join(dimAct.Roles, ",")
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryModelChange, EventType: auditlog.EventDimensionCreated,
			ActorUserID: dimActorID, ActorRole: dimActorRole,
			ResourceType: "dimension", ResourceID: newID, RevisionID: body.RevisionID,
			Metadata: map[string]string{"name": body.Name, "revision_id": body.RevisionID},
		})
		go h.autoMigrate(context.Background(), modelID) //nolint:contextcheck
		jsonOK(w, map[string]string{"id": newID, "status": "created"})
		return
	}

	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}

	dimFilter := `TRUE`
	dimArgs := []any{modelID}
	if revisionID != "" {
		dimFilter = `(revision_id=$2::uuid OR revision_id IS NULL)`
		dimArgs = []any{modelID, revisionID}
	}
	dimRows, err := h.db.Query(ctx,
		`SELECT id::text, name, agg_rule, parent_dimension_id::text, dimension_type, time_granularity, fiscal_year_start_month
		 FROM model.dimension_def WHERE model_id=$1::uuid AND `+dimFilter+` ORDER BY created_at`,
		dimArgs...)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer dimRows.Close()

	var dims []devDimension
	for dimRows.Next() {
		var d devDimension
		if err := dimRows.Scan(&d.ID, &d.Name, &d.AggRule, &d.ParentDimensionID, &d.DimensionType, &d.TimeGranularity, &d.FiscalYearStart); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		dims = append(dims, d)
	}
	dimRows.Close()

	// Resolve + cascade the caller's own dimension_member access rules
	// (same ExpandHidden/MemberEdge pattern grid()/publicDimensions use)
	// so a developer whose own account carries a hidden rule sees the
	// same restricted view here as everywhere else — this endpoint used
	// to apply no filtering at all.
	dimRules := map[string]string{}
	{
		arRows, arErr := h.db.Query(ctx,
			`SELECT ref_id, access FROM identity.user_access_rule WHERE user_id=$1::uuid AND rule_type='dimension_member'`, act.UserID)
		if arErr == nil {
			for arRows.Next() {
				var refID, access string
				if arRows.Scan(&refID, &access) == nil {
					dimRules[refID] = access
				}
			}
			arRows.Close()
		}
	}
	if len(dimRules) > 0 {
		allRows, aErr := h.db.Query(ctx, `
			SELECT m.id::text, m.dimension_id::text, COALESCE(m.parent_member_id::text,'')
			FROM model.dimension_member m JOIN model.dimension_def d ON d.id = m.dimension_id
			WHERE d.model_id = $1::uuid`, modelID)
		if aErr == nil {
			edges := make([]writeguard.MemberEdge, 0, 256)
			for allRows.Next() {
				var id, dimID, parentID string
				if allRows.Scan(&id, &dimID, &parentID) == nil {
					edges = append(edges, writeguard.MemberEdge{ID: id, ParentID: parentID, DimID: dimID})
				}
			}
			allRows.Close()
			for id := range writeguard.ExpandHidden(edges, dimRules) {
				dimRules[id] = "hidden"
			}
		}
	}

	for i, d := range dims {
		// Time members come back in chronological order (time_index), never
		// in code order — the console shows periods as the engine walks them.
		mRows, err := h.db.Query(ctx, `
			SELECT id::text, code, label, parent_member_id::text, properties,
			       period_start::text, period_end::text, time_index
			FROM model.dimension_member
			WHERE dimension_id=$1::uuid ORDER BY time_index NULLS LAST, sort_order, code
		`, d.ID)
		if err != nil {
			continue
		}
		for mRows.Next() {
			var m devMember
			var propsJSON []byte
			if err := mRows.Scan(&m.ID, &m.Code, &m.Label, &m.ParentMemberID, &propsJSON, &m.PeriodStart, &m.PeriodEnd, &m.TimeIndex); err != nil {
				continue
			}
			if len(propsJSON) > 0 {
				_ = json.Unmarshal(propsJSON, &m.Properties)
			}
			if dimRules[m.ID] == "hidden" {
				continue
			}
			dims[i].Members = append(dims[i].Members, m)
		}
		mRows.Close()
		if dims[i].Members == nil {
			dims[i].Members = []devMember{}
		}
	}

	if dims == nil {
		dims = []devDimension{}
	}
	jsonOK(w, dims)
}

// ── /api/admin/* ──────────────────────────────────────────────────────────────

type adminRevisionItem struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

type adminModelItem struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	StorageType    string `json:"storage_type"`
	ActiveRevision string `json:"active_revision"`
	// The app's business-default model: what every business console
	// resolves when no revision pins another model.
	IsDefault bool                `json:"is_default"`
	Revisions []adminRevisionItem `json:"revisions"`
}

type adminAppItem struct {
	ID     string           `json:"id"`
	Name   string           `json:"name"`
	Mode   string           `json:"mode"`
	Status string           `json:"status"`
	Models []adminModelItem `json:"models"`
}

type adminTenantItem struct {
	ID           string         `json:"id"`
	Name         string         `json:"name"`
	Plan         string         `json:"plan"`
	CreatedAt    string         `json:"created_at"`
	Applications []adminAppItem `json:"applications"`
	// PlanState is the plan as it applies right now: trial, days left,
	// read-only and why (plan.go).
	PlanState *plan.State `json:"plan_state,omitempty"`
}

func (h *handler) adminMe(w http.ResponseWriter, r *http.Request) {
	act, ok := h.requireRole(w, r, "platform_admin", "tenant_admin")
	if !ok {
		return
	}
	jsonOK(w, map[string]any{
		"user_id":      act.UserID,
		"email":        act.Email,
		"display_name": act.Name,
		"roles":        act.Roles,
	})
}

// adminInfraNodes serves GET /api/admin/infra/nodes — the latest health
// sample per cluster node (disk/memory/load), fed by the node-stats
// DaemonSet (deploy/k8s/base/infra/node-stats.yaml). Built after two CI
// disk-full outages showed nothing was watching node disks at all. A node
// whose newest sample is older than 15 minutes is flagged stale — a stale
// collector is itself a signal (node down, DaemonSet broken).
func (h *handler) adminInfraNodes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	// Node health is the platform's, not any tenant's: the console shows
	// the tab at platform scope only, and the API says the same.
	if _, ok := h.requireRole(w, r, "platform_admin"); !ok {
		return
	}
	ctx := r.Context()
	type nodeHealth struct {
		Node           string  `json:"node"`
		DiskTotalGB    float64 `json:"disk_total_gb"`
		DiskUsedGB     float64 `json:"disk_used_gb"`
		DiskPct        int     `json:"disk_pct"`
		MemTotalMB     int     `json:"mem_total_mb"`
		MemAvailableMB int     `json:"mem_available_mb"`
		Load1          float64 `json:"load1"`
		CollectedAt    string  `json:"collected_at"`
		Stale          bool    `json:"stale"`
	}
	rows, err := h.db.Query(ctx, `
		SELECT DISTINCT ON (node) node, disk_total_gb::float8, disk_used_gb::float8, disk_pct,
		       mem_total_mb, mem_available_mb, load1::float8, collected_at::text,
		       collected_at < now() - interval '15 minutes'
		FROM ops.node_stat
		ORDER BY node, collected_at DESC
	`)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []nodeHealth{}
	for rows.Next() {
		var n nodeHealth
		if rows.Scan(&n.Node, &n.DiskTotalGB, &n.DiskUsedGB, &n.DiskPct, &n.MemTotalMB, &n.MemAvailableMB, &n.Load1, &n.CollectedAt, &n.Stale) == nil {
			out = append(out, n)
		}
	}
	jsonOK(w, out)
}

func (h *handler) adminTenants(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	act, ok := h.requireRole(w, r, "platform_admin", "tenant_admin", "developer")
	if !ok {
		return
	}
	isPlatformAdmin := false
	for _, role := range act.Roles {
		if role == "platform_admin" {
			isPlatformAdmin = true
		}
	}

	if r.Method == http.MethodPost {
		if !isPlatformAdmin {
			jsonErr(w, fmt.Errorf("forbidden: only platform_admin can create tenants"), http.StatusForbidden)
			return
		}
		var body struct {
			Name string `json:"name"`
			Plan string `json:"plan"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
			jsonErr(w, fmt.Errorf("name is required"), http.StatusBadRequest)
			return
		}
		if body.Plan == "" {
			body.Plan = "standard"
		}
		var id, wsID string
		if h.db.Dedicated() {
			// Dedicated mode: the tenant gets its own database, carrying the
			// customer row and default workspace. Everything below (audit,
			// response) is identical either way.
			var pErr error
			id, wsID, pErr = h.provisionTenant(ctx, body.Name, body.Plan)
			if pErr != nil {
				jsonErr(w, pErr, http.StatusInternalServerError)
				return
			}
		} else {
			if err := h.db.QueryRow(ctx,
				`INSERT INTO core.customer (name, plan) VALUES ($1, $2) RETURNING id::text`,
				body.Name, body.Plan).Scan(&id); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			// A tenant needs at least one workspace before workspace-scoped
			// roles can be assigned to its users — create a default one right
			// away so a fresh tenant is immediately usable.
			if err := h.db.QueryRow(ctx,
				`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Default') RETURNING id::text`,
				id).Scan(&wsID); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: auditlog.EventTenantCreated,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ResourceType: "tenant", ResourceID: id,
			Metadata: map[string]string{"name": body.Name, "plan": body.Plan},
		})
		jsonOK(w, map[string]string{"id": id, "workspace_id": wsID})
		return
	}

	isTenantAdmin := false
	isDeveloper := false
	for _, role := range act.Roles {
		if role == "tenant_admin" {
			isTenantAdmin = true
		}
		if role == "developer" {
			isDeveloper = true
		}
	}

	// GET: platform_admin sees all tenants; tenant_admin sees only their own;
	// developer sees tenants for every workspace they have any role assignment in.
	var cRows interface {
		Next() bool
		Scan(...any) error
		Close()
	}
	if isPlatformAdmin || h.isGlobalBuilder(ctx, act) {
		// Platform admins and platform-level developers (no workspace scope)
		// see every tenant.
		rows, err := h.db.Query(ctx, `SELECT id::text, name, plan, created_at::text FROM core.customer ORDER BY created_at`)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		cRows = rows
	} else if isTenantAdmin {
		// tenant_admin: scoped to their tenants — own customer, workspace
		// roles, or explicit app/model access grants (adminScopeCustomerIDs
		// derives all three).
		_, customerIDs, scopeErr := h.adminScopeCustomerIDs(ctx, act)
		if scopeErr != nil {
			jsonErr(w, scopeErr, http.StatusInternalServerError)
			return
		}
		rows, err := h.db.Query(ctx,
			`SELECT id::text, name, plan, created_at::text FROM core.customer WHERE id::text = ANY($1) ORDER BY created_at`,
			customerIDs)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		cRows = rows
	} else {
		// developer: customers reachable through accessible workspace/app/model scope
		rows, err := h.db.Query(ctx, `
				SELECT DISTINCT c.id::text, c.name, c.plan, c.created_at::text
				FROM core.customer c
				JOIN core.workspace w ON w.customer_id = c.id
				JOIN core.application app ON (app.workspace_id = w.id OR app.customer_id = w.customer_id)
				LEFT JOIN identity.role_assignment ra ON ra.workspace_id = w.id AND ra.user_id = $1::uuid
				WHERE (ra.id IS NOT NULL
				       -- or the tenant is the developer's own (identity.user.customer_id)
				       OR c.id = (SELECT customer_id FROM identity."user" WHERE id = $1::uuid))
				  AND (
				      NOT EXISTS (SELECT 1 FROM identity.user_app_access WHERE user_id=$1::uuid)
				      OR EXISTS (SELECT 1 FROM identity.user_app_access WHERE user_id=$1::uuid AND application_id=app.id)
				  )
				  AND (NOT EXISTS (SELECT 1 FROM core.model WHERE application_id=app.id)
				       OR EXISTS (
				      SELECT 1 FROM core.model m
				      WHERE m.application_id=app.id
				        AND (
				            NOT EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id=$1::uuid)
				            OR EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id=$1::uuid AND model_id=m.id)
				        )
				  ))
				ORDER BY c.name
			`, act.UserID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		cRows = rows
	}
	defer cRows.Close()
	var tenants []adminTenantItem
	for cRows.Next() {
		var t adminTenantItem
		if err := cRows.Scan(&t.ID, &t.Name, &t.Plan, &t.CreatedAt); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		tenants = append(tenants, t)
	}
	cRows.Close()

	// Tenants with their own database have no row here — the catalog is
	// their listing. Their applications are read below, each in its own
	// database.
	if dedicated, cErr := h.catalogTenants(ctx, act, h.subjectOf(r)); cErr == nil {
		// A member of a dedicated tenant is already routed to its database,
		// where the query above found its one customer row — appending the
		// catalog entry as well would list that tenant twice.
		already := map[string]bool{}
		for _, t := range tenants {
			already[t.ID] = true
		}
		for _, d := range dedicated {
			if already[d.CustomerID] {
				continue
			}
			tenants = append(tenants, adminTenantItem{
				ID: d.CustomerID, Name: d.Name, Plan: d.Plan,
				CreatedAt: d.CreatedAt.Format(time.RFC3339),
			})
		}
	} else {
		h.log.Warn().Err(cErr).Msg("tenant catalog listing failed")
	}

	// isScopedDeveloper is hoisted: inside the loop ctx is a tenant's
	// database, where a platform-level actor has no row to look up.
	isScopedDeveloper := isDeveloper && !isPlatformAdmin && !h.isGlobalBuilder(ctx, act)
	for i, t := range tenants {
		// Shadowed on purpose: every query in this iteration runs against
		// the database that holds this tenant.
		ctx := h.tenantCtx(ctx, t.ID)
		tenants[i].PlanState = h.planStateFor(ctx, t.ID)
		var aRows interface {
			Next() bool
			Scan(...any) error
			Close()
		}
		var err error
		if isScopedDeveloper {
			// Workspace-scoped developer: apps of tenants where they hold a
			// role in any of the tenant's workspaces, filtered by explicit
			// app/model access grants when present. An application belongs to
			// this tenant either directly (customer_id set — customer-wide
			// apps) or via its workspace (workspace_id set, customer_id
			// NULL — the common case for workspace-scoped apps); matching
			// customer_id alone made every workspace-scoped app invisible
			// here regardless of role.
			aRows, err = h.db.Query(ctx, `
				SELECT DISTINCT a.id::text, a.name, a.mode::text, a.status::text
					FROM core.application a
					LEFT JOIN core.workspace aw ON aw.id = a.workspace_id
					WHERE (a.customer_id=$1::uuid OR aw.customer_id=$1::uuid)
					  AND (EXISTS (
					      SELECT 1 FROM identity.role_assignment ra
					      JOIN core.workspace rw ON rw.id = ra.workspace_id
					      WHERE ra.user_id=$2::uuid AND rw.customer_id = $1::uuid
					  ) OR EXISTS (SELECT 1 FROM identity."user" u WHERE u.id=$2::uuid AND u.customer_id=$1::uuid))
					  AND (
					      NOT EXISTS (SELECT 1 FROM identity.user_app_access WHERE user_id=$2::uuid)
					      OR EXISTS (SELECT 1 FROM identity.user_app_access WHERE user_id=$2::uuid AND application_id=a.id)
					  )
					  -- An application without a model is still the developer's to
					  -- see: creating that first model is their job.
					  AND (NOT EXISTS (SELECT 1 FROM core.model WHERE application_id=a.id)
					       OR EXISTS (
					      SELECT 1 FROM core.model m
					      WHERE m.application_id=a.id
					        AND (
					            NOT EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id=$2::uuid)
					            OR EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id=$2::uuid AND model_id=m.id)
					        )
					  ))
					ORDER BY a.name
				`, t.ID, act.UserID)
		} else {
			// Same customer_id-or-workspace fallback for platform_admin/tenant_admin.
			aRows, err = h.db.Query(ctx, `
				SELECT a.id::text, a.name, a.mode::text, a.status::text
				FROM core.application a
				LEFT JOIN core.workspace aw ON aw.id = a.workspace_id
				WHERE a.customer_id=$1::uuid OR aw.customer_id=$1::uuid
				ORDER BY a.created_at`, t.ID)
		}
		if err != nil {
			continue
		}
		for aRows.Next() {
			var ap adminAppItem
			if err := aRows.Scan(&ap.ID, &ap.Name, &ap.Mode, &ap.Status); err != nil {
				continue
			}
			var mRows interface {
				Next() bool
				Scan(...any) error
				Close()
			}
			if isDeveloper {
				mRows, _ = h.db.Query(ctx, `
						SELECT id::text, name, storage_type::text, COALESCE(active_revision_name,''),
						       m.id = (SELECT default_model_id FROM core.application WHERE id=$1::uuid)
						FROM core.model m
						WHERE m.application_id=$1::uuid
						  AND (
						      NOT EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id=$2::uuid)
						      OR EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id=$2::uuid AND model_id=m.id)
						  )
						ORDER BY m.created_at
					`, ap.ID, act.UserID)
			} else {
				mRows, _ = h.db.Query(ctx,
					`SELECT id::text, name, storage_type::text, COALESCE(active_revision_name,''),
					        id = (SELECT default_model_id FROM core.application WHERE id=$1::uuid)
					 FROM core.model WHERE application_id=$1::uuid ORDER BY created_at`, ap.ID)
			}
			if mRows != nil {
				for mRows.Next() {
					var m adminModelItem
					_ = mRows.Scan(&m.ID, &m.Name, &m.StorageType, &m.ActiveRevision, &m.IsDefault)
					rRows, _ := h.db.Query(ctx,
						`SELECT id::text, name, created_at::text FROM model.revision WHERE model_id=$1::uuid ORDER BY created_at`, m.ID)
					if rRows != nil {
						for rRows.Next() {
							var r adminRevisionItem
							_ = rRows.Scan(&r.ID, &r.Name, &r.CreatedAt)
							m.Revisions = append(m.Revisions, r)
						}
						rRows.Close()
					}
					if m.Revisions == nil {
						m.Revisions = []adminRevisionItem{}
					}
					ap.Models = append(ap.Models, m)
				}
				mRows.Close()
			}
			if ap.Models == nil {
				ap.Models = []adminModelItem{}
			}
			tenants[i].Applications = append(tenants[i].Applications, ap)
		}
		aRows.Close()
		if tenants[i].Applications == nil {
			tenants[i].Applications = []adminAppItem{}
		}
	}

	if tenants == nil {
		tenants = []adminTenantItem{}
	}
	jsonOK(w, tenants)
}

type userAssignment struct {
	Role          string `json:"role"`
	WorkspaceID   string `json:"workspace_id"`
	WorkspaceName string `json:"workspace_name"`
	CustomerName  string `json:"customer_name"`
}

type adminUserItem struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	// DisabledAt is set when the account was deactivated (SCIM); such a
	// user cannot sign in.
	DisabledAt  *string          `json:"disabled_at,omitempty"`
	CreatedAt   string           `json:"created_at"`
	Assignments []userAssignment `json:"assignments"`
	AppIDs      []string         `json:"app_ids"`
	ModelIDs    []string         `json:"model_ids"`
}

func (h *handler) adminUsers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}

	if r.Method == http.MethodPost {
		// delegate to action handler with empty id (create path)
		r2 := r.Clone(ctx)
		r2.URL.Path = "/api/admin/users/"
		h.adminUserAction(w, r2)
		return
	}

	all, customerIDs, err := h.adminScopeCustomerIDs(ctx, act)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !all && len(customerIDs) == 0 {
		jsonOK(w, []adminUserItem{})
		return
	}
	scopeWhere := ""
	args := []any{}
	if !all {
		scopeWhere = `
			WHERE (
			    u.customer_id::text = ANY($1)
			    OR u.id = $2::uuid
			    OR EXISTS (
			        SELECT 1
			        FROM identity.role_assignment ra_scope
			        JOIN core.workspace w_scope ON w_scope.id=ra_scope.workspace_id
			        WHERE ra_scope.user_id=u.id AND w_scope.customer_id::text = ANY($1)
			    )
			    OR EXISTS (
			        SELECT 1 FROM identity.user_app_access ua_scope
			        JOIN core.application app_scope ON app_scope.id=ua_scope.application_id
			        WHERE ua_scope.user_id=u.id AND app_scope.customer_id::text = ANY($1)
			    )
			    OR EXISTS (
			        SELECT 1 FROM identity.user_model_access um_scope
			        JOIN core.model m_scope ON m_scope.id=um_scope.model_id
			        JOIN core.application app_scope2 ON app_scope2.id=m_scope.application_id
			        WHERE um_scope.user_id=u.id AND app_scope2.customer_id::text = ANY($1)
			    )
			)`
		args = append(args, customerIDs, act.UserID)
	}

	rows, err := h.db.Query(ctx, `
			SELECT u.id::text, u.email, u.display_name, u.created_at::text, u.disabled_at::text,
			       COALESCE(
			           json_agg(
		               json_build_object(
		                   'role',           ra.role::text,
		                   'workspace_id',   COALESCE(ra.workspace_id::text, ''),
		                   'workspace_name', COALESCE(w.name, ''),
		                   'customer_name',  COALESCE(cust.name, '')
		               ) ORDER BY cust.name, w.name, ra.role
		           ) FILTER (WHERE ra.id IS NOT NULL),
		           '[]'::json
		       ) AS assignments,
		       COALESCE(
		           (SELECT ARRAY_AGG(application_id::text) FROM identity.user_app_access WHERE user_id = u.id),
		           ARRAY[]::text[]
		       ) AS app_ids,
		       COALESCE(
		           (SELECT ARRAY_AGG(model_id::text) FROM identity.user_model_access WHERE user_id = u.id),
		           ARRAY[]::text[]
		       ) AS model_ids
			FROM identity.user u
			LEFT JOIN identity.role_assignment ra ON ra.user_id = u.id
			LEFT JOIN core.workspace w ON w.id = ra.workspace_id
			LEFT JOIN core.customer cust ON cust.id = w.customer_id
			`+scopeWhere+`
			GROUP BY u.id, u.email, u.display_name, u.created_at, u.disabled_at
			ORDER BY u.created_at
		`, args...)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var users []adminUserItem
	for rows.Next() {
		var u adminUserItem
		var assignmentsJSON []byte
		if err := rows.Scan(&u.ID, &u.Email, &u.DisplayName, &u.CreatedAt, &u.DisabledAt, &assignmentsJSON, &u.AppIDs, &u.ModelIDs); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if err := json.Unmarshal(assignmentsJSON, &u.Assignments); err != nil || u.Assignments == nil {
			u.Assignments = []userAssignment{}
		}
		if u.AppIDs == nil {
			u.AppIDs = []string{}
		}
		if u.ModelIDs == nil {
			u.ModelIDs = []string{}
		}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if users == nil {
		users = []adminUserItem{}
	}
	jsonOK(w, users)
}

type auditEventItem struct {
	ID              string `json:"id"`
	Category        string `json:"category"`
	EventType       string `json:"event_type"`
	ActorName       string `json:"actor_name"`
	ActorRole       string `json:"actor_role"`
	ApplicationID   string `json:"application_id"`
	ApplicationName string `json:"application_name"`
	ResourceType    string `json:"resource_type"`
	ResourceID      string `json:"resource_id"`
	RevisionID      string `json:"revision_id"`
	RevisionName    string `json:"revision_name"`
	Metadata        any    `json:"metadata"`
	OccurredAt      string `json:"occurred_at"`
}

func (h *handler) adminAudit(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// The admin gate admits tenant_admin as well as platform_admin, and this
	// endpoint used to return the most recent 200 events across EVERY tenant
	// with no filter at all — leaking other customers' event types, actor
	// names, resource IDs and metadata. platform_admin keeps the global view;
	// a tenant admin sees only events attributable to their own customers,
	// via the same adminScopeCustomerIDs the rest of the admin surface uses.
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	cond, args, visible, err := h.auditScope(ctx, act)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !visible {
		jsonOK(w, []auditEventItem{})
		return
	}
	scopeWhere := ""
	if cond != "" {
		scopeWhere = " WHERE " + cond
	}

	rows, err := h.db.Query(ctx, `
		SELECT ae.id::text, ae.category::text, ae.event_type,
		       COALESCE(u.display_name, 'system') AS actor_name,
		       COALESCE(ae.actor_role, '') AS actor_role,
		       COALESCE(ae.application_id::text, '') AS application_id,
		       COALESCE(app.name, '') AS application_name,
		       COALESCE(ae.resource_type, '') AS resource_type,
		       COALESCE(ae.resource_id, '') AS resource_id,
		       COALESCE(ae.revision_id::text, '') AS revision_id,
		       COALESCE(s.name, '') AS revision_name,
		       ae.metadata,
		       ae.occurred_at::text
		FROM audit.audit_event ae
		LEFT JOIN identity.user u ON u.id = ae.actor_user_id
		LEFT JOIN core.application app ON app.id = ae.application_id
		LEFT JOIN model.revision s ON s.id = ae.revision_id`+scopeWhere+`
		ORDER BY ae.occurred_at DESC
		LIMIT 200
	`, args...)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var events []auditEventItem
	for rows.Next() {
		var e auditEventItem
		var metaJSON []byte
		if err := rows.Scan(&e.ID, &e.Category, &e.EventType, &e.ActorName, &e.ActorRole, &e.ApplicationID, &e.ApplicationName, &e.ResourceType, &e.ResourceID, &e.RevisionID, &e.RevisionName, &metaJSON, &e.OccurredAt); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		_ = json.Unmarshal(metaJSON, &e.Metadata)
		events = append(events, e)
	}
	if err := rows.Err(); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if events == nil {
		events = []auditEventItem{}
	}
	jsonOK(w, events)
}

// auditScope is the one definition of which audit events an administrator
// may see, shared by the listing and the enterprise export so the two can
// never disagree. It returns a boolean SQL condition over the aliases ae
// (audit.audit_event), u (the actor) and app (the application), with its
// arguments numbered from $1; an empty condition means everything (a
// platform admin). visible is false for an admin with no tenant at all.
//
// An event is in scope when its application belongs to one of the admin's
// customers, or (for events with no application — user/tenant admin
// actions) when its actor is a user of one of them. Events with neither are
// platform-level and stay platform_admin-only.
func (h *handler) auditScope(ctx context.Context, act *actor) (cond string, args []any, visible bool, err error) {
	all, customerIDs, err := h.adminScopeCustomerIDs(ctx, act)
	if err != nil {
		return "", nil, false, err
	}
	if all {
		return "", nil, true, nil
	}
	if len(customerIDs) == 0 {
		return "", nil, false, nil
	}
	return `(
		    app.customer_id::text = ANY($1)
		    OR EXISTS (
		        SELECT 1 FROM core.workspace w_scope
		        WHERE w_scope.id = app.workspace_id AND w_scope.customer_id::text = ANY($1)
		    )
		    OR (ae.application_id IS NULL AND u.customer_id::text = ANY($1))
		)`, []any{customerIDs}, true, nil
}

// ── /api/admin/workspaces ─────────────────────────────────────────────────────

func (h *handler) adminWorkspaces(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	all, customerIDs, err := h.adminScopeCustomerIDs(ctx, act)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !all && len(customerIDs) == 0 {
		jsonOK(w, []struct{}{})
		return
	}
	scopeWhere := ""
	args := []any{}
	if !all {
		scopeWhere = `WHERE c.id::text = ANY($1)`
		args = append(args, customerIDs)
	}
	rows, err := h.db.Query(ctx, `
		SELECT w.id::text, w.name, c.name AS customer_name, c.id::text AS customer_id
		FROM core.workspace w
		JOIN core.customer c ON c.id = w.customer_id
		`+scopeWhere+`
		ORDER BY c.name, w.name
	`, args...)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type wsItem struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		CustomerName string `json:"customer_name"`
		CustomerID   string `json:"customer_id"`
	}
	var out []wsItem
	for rows.Next() {
		var item wsItem
		if err := rows.Scan(&item.ID, &item.Name, &item.CustomerName, &item.CustomerID); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		out = append(out, item)
	}
	if out == nil {
		out = []wsItem{}
	}
	jsonOK(w, out)
}

// ── Import ────────────────────────────────────────────────────────────────────

// importUpload serves POST /api/import/upload. Open to any authenticated
// actor (no role gate) — a cost-center manager must be able to import their
// own scope's facts exactly as they can write cells directly; the only
// generic write guard is writeguard.CheckWrite (called from inside
// importpkg.Store.CommitImport), applied identically regardless of caller.
//
// Accepts either "csv" (text) or "xlsx_base64" (a native .xlsx workbook,
// base64-encoded — multipart/form-data would be the more natural transport
// but this keeps the endpoint's existing pure-JSON contract). Either way,
// rows are resolved through importpkg.ResolveRows: column headers map to
// metric/dimension NAMES (no UUID pasting required — see its doc comment),
// every referenced member is validated as a leaf, every value must be a
// non-negative number, and if ANY row fails validation the WHOLE file is
// rejected atomically — nothing is staged or committed.
func (h *handler) importUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<20) // 16 MB limit
	ctx := r.Context()

	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}

	var body struct {
		CSV        string `json:"csv"`
		XLSXBase64 string `json:"xlsx_base64"`
		RevisionID string `json:"revision_id"`
		ImportMode string `json:"import_mode"` // "incremental" (default) | "replace" | "full_reload"
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || (body.CSV == "" && body.XLSXBase64 == "") {
		jsonErr(w, fmt.Errorf("csv or xlsx_base64 field required"), http.StatusBadRequest)
		return
	}

	// The body's revision picks WHICH of the app's models this import is
	// for (multi-model apps) — must happen before the revision/model
	// consistency check below.
	modelID = h.pinModelForBodyRevision(ctx, r, modelID, body.RevisionID)

	// Resolve revision — use provided revision_id or fall back to model's active revision.
	if !h.requireRevisionInModel(w, r, body.RevisionID, modelID) {
		return
	}
	revisionID := body.RevisionID
	if revisionID == "" {
		err = h.db.QueryRow(ctx, `
			SELECT COALESCE(active_revision_id::text,
			       (SELECT id::text FROM model.revision WHERE model_id=$1::uuid ORDER BY created_at LIMIT 1))
			FROM core.model WHERE id=$1::uuid
		`, modelID).Scan(&revisionID)
		if err != nil {
			jsonErr(w, fmt.Errorf("resolve revision: %w", err), http.StatusInternalServerError)
			return
		}
	}

	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	userID := act.UserID

	// Check system_managed FIRST, before any file parsing: a system_managed
	// revision typically has no metric_def/dimension_def rows of its own
	// (it's only ever populated by an approval-triggered copy), so column
	// classification against it would fail with a confusing "unknown
	// column" error that masks the real reason — this revision isn't
	// writable through import at all, regardless of the file's contents.
	systemManaged, smErr := writeguard.SystemManaged(ctx, h.db.For(ctx), revisionID)
	if smErr != nil {
		jsonErr(w, fmt.Errorf("check system-managed: %w", smErr), http.StatusInternalServerError)
		return
	}
	if systemManaged {
		jsonErr(w, fmt.Errorf("this revision is system-managed and read-only"), http.StatusForbidden)
		return
	}

	var header []string
	var rawRows []importpkg.RawRow
	if body.XLSXBase64 != "" {
		raw, decErr := base64.StdEncoding.DecodeString(body.XLSXBase64)
		if decErr != nil {
			jsonErr(w, fmt.Errorf("xlsx_base64: %w", decErr), http.StatusBadRequest)
			return
		}
		header, rawRows, err = importpkg.ParseXLSXRows(raw)
	} else {
		header, rawRows, err = importpkg.ParseCSVRows([]byte(body.CSV))
	}
	if err != nil {
		jsonErr(w, fmt.Errorf("parse file: %w", err), http.StatusBadRequest)
		return
	}

	staged, importErrs, err := importpkg.ResolveRows(ctx, h.db.For(ctx), modelID, revisionID, header, rawRows)
	if err != nil {
		jsonErr(w, fmt.Errorf("parse file: %w", err), http.StatusBadRequest)
		return
	}
	// Atomic: any row failing validation rejects the whole file. Nothing is
	// staged or committed — the caller fixes the file and re-uploads, rather
	// than silently getting a partial import. jsonErr can only carry a
	// message string, so the per-row detail the UI needs to actually show
	// the user what to fix is written directly here instead.
	if len(importErrs) > 0 {
		rows := make([]map[string]any, 0, len(importErrs))
		for _, e := range importErrs {
			rows = append(rows, map[string]any{
				"row": e.RowNumber, "column": e.Column, "code": e.ErrorCode,
				"message": e.Message, "raw_value": e.RawValue,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":      fmt.Sprintf("import rejected: %d row(s) failed validation, nothing was imported", len(importErrs)),
			"error_rows": len(importErrs),
			"errors":     rows,
		})
		return
	}
	if len(staged) == 0 {
		jsonErr(w, fmt.Errorf("file contained no data rows"), http.StatusBadRequest)
		return
	}

	if cid := h.customerOfModel(ctx, modelID); cid != "" && h.plans != nil {
		if err := cmp.Or(h.plans.CheckFactRows(ctx, h.db.For(ctx), cid, modelID, len(staged)), h.plans.CheckStorage(ctx, h.db.For(ctx), cid)); err != nil {
			h.jsonLimitErr(w, err)
			return
		}
	}
	store := importpkg.NewStore(h.db.For(ctx))
	job, err := store.CreateImportJob(ctx, modelID, revisionID, "upload", userID, nil)
	if err != nil {
		jsonErr(w, fmt.Errorf("create job: %w", err), http.StatusInternalServerError)
		return
	}
	if err := store.StageRows(ctx, job.Id, staged, nil); err != nil {
		jsonErr(w, fmt.Errorf("stage: %w", err), http.StatusInternalServerError)
		return
	}

	mode := importpkg.ImportMode(body.ImportMode)
	metricIDs, err := store.CommitImport(ctx, job.Id, modelID, revisionID, userID, mode)
	if err != nil {
		if errors.Is(err, importpkg.ErrWriteDenied) {
			jsonErr(w, err, http.StatusForbidden)
			return
		}
		jsonErr(w, fmt.Errorf("commit: %w", err), http.StatusInternalServerError)
		return
	}

	// Best-effort recalc (no NATS in gateway — call calculation scheduler directly)
	if len(metricIDs) > 0 {
		calcStore := calculation.NewStore(h.db.For(ctx))
		sched := calculation.NewScheduler(h.log, calcStore, nil)
		go func() { //nolint:contextcheck
			_ = sched.RecalcAffected(context.Background(), modelID, revisionID, metricIDs)
		}()
	}

	var appID string
	_ = h.db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID)
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryDataChange, EventType: auditlog.EventImportUploaded,
		ActorUserID: userID, ActorRole: strings.Join(act.Roles, ","),
		ApplicationID: appID, ResourceType: "import_job", ResourceID: job.Id, RevisionID: revisionID,
		Metadata: map[string]string{"rows": strconv.Itoa(len(staged)), "import_mode": string(mode)},
	})
	jsonOK(w, map[string]any{
		"job_id":      job.Id,
		"status":      "committed",
		"total_rows":  len(staged),
		"valid_rows":  len(staged),
		"error_rows":  0,
		"revision_id": revisionID,
	})
}

func (h *handler) importJobs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}

	store := importpkg.NewStore(h.db.For(ctx))
	jobs, err := store.ListImportJobs(ctx, modelID, 20)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if jobs == nil {
		jsonOK(w, []any{})
		return
	}
	jsonOK(w, jobs)
}

func (h *handler) importJobAction(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/import/jobs/")
	if id == "" {
		jsonErr(w, fmt.Errorf("missing job id"), http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	store := importpkg.NewStore(h.db.For(ctx))
	if err := store.DeleteImportJob(ctx, id, modelID); err != nil {
		if errors.Is(err, importpkg.ErrImportJobNotFound) {
			jsonErr(w, err, http.StatusNotFound)
			return
		}
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if a, e := h.resolveActor(ctx, r); e == nil {
		var appID string
		_ = h.db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID)
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryDataChange, EventType: auditlog.EventImportJobDeleted,
			ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
			ApplicationID: appID, ResourceType: "import_job", ResourceID: id,
		})
	}
	jsonOK(w, map[string]string{"status": "deleted"})
}

func (h *handler) debugFacts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	revisionID, _, revErr := h.resolveRevisionCtx(ctx, r.URL.Query().Get("revision_id"), modelID)
	if rejectForeignRevision(w, revErr) {
		return
	}
	hiddenByDim, metricRules, err := h.hiddenMemberFilter(ctx, act, modelID, revisionID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}

	rows, err := h.db.Query(ctx, `
		SELECT fi.metric_id::text, md.name AS metric_name,
		       fi.dim_members::text, fi.value::float8,
		       COALESCE(fi.revision_name, '') AS revision_name, fi.revision_id::text,
		       fi.entered_at
		FROM runtime.fact_input fi
		LEFT JOIN model.metric_def md ON md.id = fi.metric_id
		WHERE fi.model_id = $1::uuid
		ORDER BY fi.entered_at DESC
		LIMIT 50
	`, modelID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type factRow struct {
		MetricID     string  `json:"metric_id"`
		MetricName   string  `json:"metric_name"`
		DimMembers   string  `json:"dim_members"`
		Value        float64 `json:"value"`
		RevisionName string  `json:"revision_name"`
		RevisionID   *string `json:"revision_id"`
		EnteredAt    string  `json:"entered_at"`
	}
	var facts []factRow
	for rows.Next() {
		var f factRow
		var t interface{}
		if err := rows.Scan(&f.MetricID, &f.MetricName, &f.DimMembers, &f.Value, &f.RevisionName, &f.RevisionID, &t); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if metricRules[f.MetricID] == "hidden" {
			continue
		}
		var dm map[string]string
		if json.Unmarshal([]byte(f.DimMembers), &dm) == nil && factRowHidden(dm, hiddenByDim) {
			continue
		}
		f.EnteredAt = fmt.Sprintf("%v", t)
		facts = append(facts, f)
	}
	if facts == nil {
		facts = []factRow{}
	}
	jsonOK(w, facts)
}

// debugCalc is debugFacts's runtime.calc_result twin: the developer's window
// into what the calculation scheduler actually persisted. With ?dim_members=
// (exact JSON, e.g. {"<dimID>":"Q1"}) it lists every metric's row AT that
// combo — the way to check whether a precomputed slice exists. Without it, a
// per-metric summary: total rows, whether the '{}' aggregate exists, and how
// many single-dimension slice rows are present.
func (h *handler) debugCalc(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	revisionID, _, revErr := h.resolveRevisionCtx(ctx, r.URL.Query().Get("revision_id"), modelID)
	if rejectForeignRevision(w, revErr) {
		return
	}
	_, metricRules, err := h.hiddenMemberFilter(ctx, act, modelID, revisionID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}

	if dmFilter := r.URL.Query().Get("dim_members"); dmFilter != "" {
		var probe map[string]string // validate it's a flat JSON object
		if json.Unmarshal([]byte(dmFilter), &probe) != nil {
			jsonErr(w, fmt.Errorf("dim_members must be a JSON object of dimension id -> member code"), http.StatusBadRequest)
			return
		}
		rows, qErr := h.db.Query(ctx, `
			SELECT DISTINCT ON (cr.metric_id) cr.metric_id::text, COALESCE(md.name,''), cr.value::float8, cr.calc_at::text
			FROM runtime.calc_result cr
			LEFT JOIN model.metric_def md ON md.id = cr.metric_id
			WHERE cr.model_id=$1::uuid AND cr.revision_id=$2::uuid AND cr.dim_members=$3::jsonb
			ORDER BY cr.metric_id, cr.calc_at DESC
		`, modelID, revisionID, dmFilter)
		if qErr != nil {
			jsonErr(w, qErr, http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		type calcRow struct {
			MetricID   string  `json:"metric_id"`
			MetricName string  `json:"metric_name"`
			Value      float64 `json:"value"`
			CalcAt     string  `json:"calc_at"`
		}
		out := []calcRow{}
		for rows.Next() {
			var c calcRow
			if err := rows.Scan(&c.MetricID, &c.MetricName, &c.Value, &c.CalcAt); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			if metricRules[c.MetricID] == "hidden" {
				continue
			}
			out = append(out, c)
		}
		jsonOK(w, out)
		return
	}

	rows, qErr := h.db.Query(ctx, `
		SELECT cr.metric_id::text, COALESCE(md.name,''),
		       COUNT(*)::int,
		       COUNT(*) FILTER (WHERE cr.dim_members = '{}'::jsonb)::int,
		       COUNT(*) FILTER (WHERE cr.dim_members != '{}'::jsonb
		           AND (SELECT COUNT(*) FROM jsonb_object_keys(cr.dim_members)) = 1)::int
		FROM runtime.calc_result cr
		LEFT JOIN model.metric_def md ON md.id = cr.metric_id
		WHERE cr.model_id=$1::uuid AND cr.revision_id=$2::uuid
		GROUP BY cr.metric_id, md.name
		ORDER BY md.name
	`, modelID, revisionID)
	if qErr != nil {
		jsonErr(w, qErr, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type summaryRow struct {
		MetricID     string `json:"metric_id"`
		MetricName   string `json:"metric_name"`
		TotalRows    int    `json:"total_rows"`
		HasAggregate bool   `json:"has_aggregate"`
		OneDimSlices int    `json:"one_dim_slices"`
	}
	out := []summaryRow{}
	for rows.Next() {
		var s summaryRow
		var aggCount int
		if err := rows.Scan(&s.MetricID, &s.MetricName, &s.TotalRows, &aggCount, &s.OneDimSlices); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if metricRules[s.MetricID] == "hidden" {
			continue
		}
		s.HasAggregate = aggCount > 0
		out = append(out, s)
	}
	jsonOK(w, out)
}

// ── Integrations ──────────────────────────────────────────────────────────────

type integrationDef struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
	// Status "draft" = saved work-in-progress from the Import Wizard, not
	// runnable yet; "active" = a complete, runnable integration.
	Status string          `json:"status"`
	Tags   []string        `json:"tags"`
	Config json.RawMessage `json:"config"`
}

// importDimensionMembersCSV upserts dimension members from CSV records.
// colIdx maps LOGICAL field name (lowercased) → column index; logical
// fields are "code", "label", "parent_code" and any number of
// "property:<name>" entries (stored into dimension_member.properties,
// merged over existing keys so re-imports converge). code is optional:
// a row without one gets a code auto-generated from its label — an
// uppercased slug, reused when the same label was auto-coded before (so
// re-imports stay idempotent) and suffixed when a different label
// collides. Rows with neither code nor label count as errors.

// importDimensionMembers serves POST /api/import/dimension-members — the
// Import Wizard's one-off dimension import. This used to be impossible: the
// wizard routed EVERY no-integration commit to /api/import/upload, the GRID
// importer, which rejected dimension CSVs with 'column header "label"
// matches no metric or dimension' (reported live). The CSV's headers are
// already logical field names (code, label, parent_code, property:<name>);
// the same shared importer backs saved-integration runs.
func (h *handler) importDimensionMembers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	var body struct {
		DimensionID string `json:"dimension_id"`
		CSV         string `json:"csv"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.DimensionID == "" || body.CSV == "" {
		jsonErr(w, fmt.Errorf("dimension_id and csv are required"), http.StatusBadRequest)
		return
	}
	if !h.requireResourceAccess(w, r, "dimension", body.DimensionID) {
		return
	}

	cr := csv.NewReader(strings.NewReader(body.CSV))
	header, err := cr.Read()
	if err != nil {
		jsonErr(w, fmt.Errorf("empty csv"), http.StatusBadRequest)
		return
	}
	colIdx := make(map[string]int, len(header))
	for i, col := range header {
		colIdx[strings.ToLower(strings.TrimSpace(col))] = i
	}
	imported, errs, importErr := h.importDimensionMembersCSV(ctx, body.DimensionID, cr, colIdx)
	if importErr != nil {
		// A time dimension imports as one unit: an invalid period set
		// (overlap, gap, bad boundary) rejects the whole file.
		jsonErr(w, importErr, http.StatusBadRequest)
		return
	}

	var appID string
	_ = h.db.QueryRow(ctx, `
		SELECT m.application_id::text FROM model.dimension_def d
		JOIN core.model m ON m.id = d.model_id WHERE d.id = $1::uuid
	`, body.DimensionID).Scan(&appID)
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryDataChange, EventType: auditlog.EventImportUploaded,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ApplicationID: appID, ResourceType: "dimension", ResourceID: body.DimensionID,
		Metadata: map[string]string{"rows_imported": fmt.Sprintf("%d", imported), "error_rows": fmt.Sprintf("%d", errs)},
	})
	jsonOK(w, map[string]any{"rows_imported": imported, "error_rows": errs})
}

func (h *handler) importDimensionMembersCSV(ctx context.Context, dimensionID string, cr *csv.Reader, colIdx map[string]int) (imported, errs int, fatal error) {
	codeCol, hasCode := colIdx["code"]
	labelCol, hasLabel := colIdx["label"]
	parentCol, hasParent := colIdx["parent_code"]
	// Time dimensions (spec §4.2): period_start / period_end columns are
	// required (a row with both empty is an aggregate period, parent_code
	// works as on any hierarchy), and the whole file is validated and
	// reindexed together in one transaction.
	startCol, hasStart := colIdx["period_start"]
	endCol, hasEnd := colIdx["period_end"]
	timeCfg, cfgErr := timedim.LoadConfig(ctx, h.db.For(ctx), dimensionID)
	if cfgErr != nil {
		return 0, 0, fmt.Errorf("dimension not found")
	}
	isTime := timeCfg.Type == timedim.TypeTime
	if isTime && (!hasStart || !hasEnd) {
		return 0, 0, &timedim.Error{Code: timedim.CodeInvalidTimeMember, Message: "a time dimension import needs period_start and period_end columns"}
	}
	if !isTime && (hasStart || hasEnd) {
		return 0, 0, &timedim.Error{Code: timedim.CodeInvalidTimeMember, Message: "period_start/period_end apply only to a time dimension"}
	}
	var tx pgx.Tx
	if isTime {
		var err error
		if tx, err = h.db.Begin(ctx); err != nil {
			return 0, 0, err
		}
		defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	}
	type propCol struct {
		name string
		idx  int
	}
	var propCols []propCol
	for field, idx := range colIdx {
		if name, ok := strings.CutPrefix(field, "property:"); ok && strings.TrimSpace(name) != "" {
			propCols = append(propCols, propCol{name: strings.TrimSpace(name), idx: idx})
		}
	}
	// Register each mapped property as a declared dimension_property, not just
	// a value on members. Without this a property:X import wrote values that
	// showed as a column but were absent from the dimension's property
	// definitions — unmanageable and confusing ("category — what is it, I
	// can't find it in the properties"). Idempotent via the (dimension_id,
	// name) unique constraint; default type "text".
	for _, pc := range propCols {
		if _, err := h.db.Exec(ctx,
			`INSERT INTO model.dimension_property (dimension_id, name, data_type)
			 VALUES ($1::uuid, $2, 'text') ON CONFLICT (dimension_id, name) DO NOTHING`,
			dimensionID, pc.name); err != nil {
			h.log.Warn().Err(err).Str("property", pc.name).Msg("declare imported dimension property")
		}
	}

	cell := func(record []string, idx int) string {
		if idx < 0 || idx >= len(record) {
			return ""
		}
		return strings.TrimSpace(record[idx])
	}

	for {
		record, err := cr.Read()
		if err != nil {
			break
		}
		code := ""
		if hasCode {
			code = cell(record, codeCol)
		}
		label := ""
		if hasLabel {
			label = cell(record, labelCol)
		}
		if label == "" {
			label = code
		}
		if code == "" {
			if label == "" {
				errs++
				continue
			}
			code, err = h.autoMemberCode(ctx, dimensionID, label)
			if err != nil {
				errs++
				continue
			}
		}
		var parentID *string
		if hasParent {
			if parentCode := cell(record, parentCol); parentCode != "" {
				var pid string
				if err := h.db.QueryRow(ctx,
					`SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`,
					dimensionID, parentCode,
				).Scan(&pid); err == nil {
					parentID = &pid
				}
			}
		}
		props := map[string]string{}
		for _, pc := range propCols {
			if v := cell(record, pc.idx); v != "" {
				props[pc.name] = v
			}
		}
		propsJSON, _ := json.Marshal(props)
		if isTime {
			var start, end *time.Time
			var idx *int
			if cell(record, startCol) != "" || cell(record, endCol) != "" {
				ps, perr := timedim.ParseDate(cell(record, startCol))
				pe, perr2 := timedim.ParseDate(cell(record, endCol))
				if perr != nil || perr2 != nil {
					return 0, 0, &timedim.Error{Code: timedim.CodeInvalidTimeMember, Message: fmt.Sprintf("member %q: period_start and period_end must be YYYY-MM-DD dates (both empty for an aggregate period)", code)}
				}
				start, end = &ps, &pe
				zero := 0
				idx = &zero
			}
			var parentID *string
			if hasParent {
				if parentCode := cell(record, parentCol); parentCode != "" {
					var pid string
					if err := tx.QueryRow(ctx,
						`SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`,
						dimensionID, parentCode).Scan(&pid); err != nil {
						return 0, 0, &timedim.Error{Code: timedim.CodeInvalidTimeMember, Message: fmt.Sprintf("member %q: parent %q not found (list parents before their children)", code, parentCode)}
					}
					parentID = &pid
				}
			}
			if _, err = tx.Exec(ctx,
				`INSERT INTO model.dimension_member (dimension_id, code, label, properties, period_start, period_end, time_index, parent_member_id)
				 VALUES ($1::uuid, $2, $3, $4::jsonb, $5::date, $6::date, $7, $8::uuid)
				 ON CONFLICT (dimension_id, code) DO UPDATE SET
				   label = EXCLUDED.label,
				   period_start = EXCLUDED.period_start, period_end = EXCLUDED.period_end, time_index = EXCLUDED.time_index,
				   parent_member_id = COALESCE(EXCLUDED.parent_member_id, model.dimension_member.parent_member_id),
				   properties = model.dimension_member.properties || EXCLUDED.properties`,
				dimensionID, code, label, string(propsJSON), start, end, idx, parentID); err != nil {
				return 0, 0, fmt.Errorf("member %q: %w", code, err)
			}
			imported++
			continue
		}
		_, err = h.db.Exec(ctx,
			`INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id, properties)
			 VALUES ($1::uuid, $2, $3, NULLIF($4,'')::uuid, $5::jsonb)
			 ON CONFLICT (dimension_id, code) DO UPDATE SET
			   label = EXCLUDED.label,
			   parent_member_id = COALESCE(EXCLUDED.parent_member_id, model.dimension_member.parent_member_id),
			   properties = model.dimension_member.properties || EXCLUDED.properties`,
			dimensionID, code, label, deref(parentID), string(propsJSON))
		if err != nil {
			errs++
		} else {
			imported++
		}
	}
	if isTime {
		if err := timedim.ValidateAndReindex(ctx, tx, dimensionID); err != nil {
			return 0, 0, err
		}
		if err := tx.Commit(ctx); err != nil {
			return 0, 0, err
		}
		var modelID string
		_ = h.db.QueryRow(ctx, `SELECT model_id::text FROM model.dimension_def WHERE id=$1::uuid`, dimensionID).Scan(&modelID)
		if modelID != "" && imported > 0 {
			go h.recalcAllInputsAcrossRevisions(context.WithoutCancel(ctx), modelID) //nolint:contextcheck
		}
	}
	return imported, errs, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// autoMemberCode derives a stable member code from a label: an uppercased
// alphanumeric slug. If the slug is already taken by a member with the SAME
// label the existing code is reused (idempotent re-import); a different
// label gets numeric suffixes until a free (or same-labeled) code is found.
func (h *handler) autoMemberCode(ctx context.Context, dimensionID, label string) (string, error) {
	slug := strings.ToUpper(strings.TrimSpace(label))
	var b strings.Builder
	lastUnderscore := false
	for _, r := range slug {
		switch {
		case (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastUnderscore = false
		case (r >= 'a' && r <= 'z'):
			b.WriteRune(r - 32)
			lastUnderscore = false
		default:
			// Any other rune (spaces, punctuation, non-latin letters kept
			// out of codes) collapses to a single underscore.
			if !lastUnderscore && b.Len() > 0 {
				b.WriteByte('_')
				lastUnderscore = true
			}
		}
	}
	base := strings.Trim(b.String(), "_")
	if base == "" {
		// A label with no ASCII-representable characters (e.g. fully
		// non-latin) hashes to a stable short code instead.
		sum := sha256.Sum256([]byte(label))
		base = "M_" + strings.ToUpper(hex.EncodeToString(sum[:4]))
	}
	if len(base) > 40 {
		base = base[:40]
	}
	candidate := base
	for i := 2; i < 100; i++ {
		var existingLabel string
		err := h.db.QueryRow(ctx,
			`SELECT label FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`,
			dimensionID, candidate,
		).Scan(&existingLabel)
		if errors.Is(err, pgx.ErrNoRows) {
			return candidate, nil // free
		}
		if err != nil {
			return "", err
		}
		if existingLabel == label {
			return candidate, nil // same member, upsert will update in place
		}
		candidate = fmt.Sprintf("%s_%d", base, i)
	}
	return "", fmt.Errorf("could not derive a unique code for label %q", label)
}

// developerSetDefaultModel serves POST /api/developer/models/{id}/set-default
// — marks the model as its application's business default: what every
// business console resolves when nothing pins another model. Without an
// explicit default, "newest model wins" made creating an experimental model
// blank every business surface (reported live).
func (h *handler) developerSetDefaultModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	modelID := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/developer/models/"), "/set-default")
	if modelID == "" {
		jsonErr(w, fmt.Errorf("missing model id"), http.StatusBadRequest)
		return
	}
	allowed, aErr := h.actorCanAccessModel(ctx, act, modelID)
	if aErr != nil {
		jsonErr(w, aErr, http.StatusInternalServerError)
		return
	}
	if !allowed {
		jsonErr(w, fmt.Errorf("forbidden: model is outside your access scope"), http.StatusForbidden)
		return
	}
	tag, err := h.db.Exec(ctx, `
		UPDATE core.application SET default_model_id = $1::uuid
		WHERE id = (SELECT application_id FROM core.model WHERE id = $1::uuid)
	`, modelID)
	if err != nil || tag.RowsAffected() == 0 {
		jsonErr(w, fmt.Errorf("model not found"), http.StatusNotFound)
		return
	}
	var appID string
	_ = h.db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID)
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryModelChange, EventType: auditlog.EventApplicationUpdated,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ApplicationID: appID, ResourceType: "model", ResourceID: modelID,
		Metadata: map[string]string{"action": "set_default_model"},
	})
	jsonOK(w, map[string]string{"status": "ok"})
}

func (h *handler) developerIntegrations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	revisionID, _, revErr := h.resolveRevisionCtx(ctx, r.URL.Query().Get("revision_id"), modelID)
	if rejectForeignRevision(w, revErr) {
		return
	}

	if r.Method == http.MethodPost {
		raw, rerr := readAll(r)
		if rerr != nil {
			jsonErr(w, rerr, http.StatusBadRequest)
			return
		}
		// rest_api integrations take the atomic typed path (metadata +
		// config + schedule in one call) — see rest_api_integrations.go.
		var probe struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &probe)
		if probe.Type == "rest_api" {
			h.restAPICreate(w, r, modelID, revisionID, raw)
			return
		}
		var body struct {
			Name       string   `json:"name"`
			Type       string   `json:"type"`
			TargetType string   `json:"target_type"`
			TargetID   string   `json:"target_id"`
			Status     string   `json:"status"`
			Tags       []string `json:"tags"`
		}
		if err := json.Unmarshal(raw, &body); err != nil || body.Name == "" {
			jsonErr(w, fmt.Errorf("name required"), http.StatusBadRequest)
			return
		}
		if body.Type == "" {
			body.Type = "csv_import"
		}
		if body.Status != "draft" {
			body.Status = "active"
		}
		if body.Tags == nil {
			body.Tags = []string{}
		}
		var newID string
		if err := h.db.QueryRow(ctx,
			`INSERT INTO model.integration_def (model_id, name, type, target_type, target_id, revision_id, status, tags)
			 VALUES ($1::uuid, $2, $3, $4, NULLIF($5,'')::uuid, NULLIF($6,'')::uuid, $7, $8) RETURNING id::text`,
			modelID, body.Name, body.Type, body.TargetType, body.TargetID, revisionID, body.Status, body.Tags,
		).Scan(&newID); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			var appID string
			_ = h.db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID)
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryModelChange, EventType: auditlog.EventIntegrationCreated,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: appID, ResourceType: "integration", ResourceID: newID, RevisionID: revisionID,
				Metadata: map[string]string{"name": body.Name, "type": body.Type},
			})
		}
		jsonOK(w, map[string]string{"id": newID, "status": "created"})
		return
	}

	rows, err := h.db.Query(ctx,
		`SELECT id::text, name, type, target_type, COALESCE(target_id::text,''), status, tags, config
		 FROM model.integration_def
		 WHERE model_id=$1::uuid
		   AND ($2 = '' OR revision_id IS NULL OR revision_id::text = $2)
		 ORDER BY created_at`, modelID, revisionID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var list []integrationDef
	for rows.Next() {
		var d integrationDef
		_ = rows.Scan(&d.ID, &d.Name, &d.Type, &d.TargetType, &d.TargetID, &d.Status, &d.Tags, &d.Config)
		if d.Config == nil {
			d.Config = json.RawMessage("{}")
		}
		list = append(list, d)
	}
	if list == nil {
		list = []integrationDef{}
	}
	jsonOK(w, list)
}

func (h *handler) developerIntegrationAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tail := strings.TrimPrefix(r.URL.Path, "/api/developer/integrations/")
	if tail == "" {
		jsonErr(w, fmt.Errorf("missing id"), http.StatusBadRequest)
		return
	}
	// Support /{id}/config sub-path
	parts := strings.SplitN(tail, "/", 2)
	intID := parts[0]
	subPath := ""
	if len(parts) == 2 {
		subPath = parts[1]
	}

	if !h.requireResourceAccess(w, r, "integration", intID) {
		return
	}

	// rest_api integrations: typed GET/PATCH and the extended run shape.
	var intType string
	_ = h.db.QueryRow(ctx, `SELECT type FROM model.integration_def WHERE id=$1::uuid`, intID).Scan(&intType)
	if intType == "rest_api" {
		var restModelID string
		_ = h.db.QueryRow(ctx, `SELECT model_id::text FROM model.integration_def WHERE id=$1::uuid`, intID).Scan(&restModelID)
		switch {
		case subPath == "runs" && r.Method == http.MethodGet:
			h.restAPIListRuns(w, r, intID)
			return
		case subPath == "" && r.Method == http.MethodGet:
			h.restAPIRespond(w, r, restModelID, intID)
			return
		case subPath == "" && r.Method == http.MethodPatch:
			h.restAPIUpdate(w, r, restModelID, intID)
			return
		}
		// DELETE falls through to the shared legacy path (cascade + audit).
	}

	if subPath == "runs" && r.Method == http.MethodGet {
		type runRow struct {
			ID           string `json:"id"`
			Status       string `json:"status"`
			RowsImported int    `json:"rows_imported"`
			ErrorRows    int    `json:"error_rows"`
			Message      string `json:"message"`
			CreatedAt    string `json:"created_at"`
			RunBy        string `json:"run_by"`
		}
		rows, err := h.db.Query(ctx, `
			SELECT ir.id::text, ir.status, ir.rows_imported, ir.error_rows, ir.message,
			       ir.created_at::text, COALESCE(NULLIF(u.display_name,''), u.email, '')
			FROM model.integration_run ir
			LEFT JOIN identity.user u ON u.id = ir.run_by
			WHERE ir.integration_id = $1::uuid
			ORDER BY ir.created_at DESC LIMIT 50
		`, intID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		out := []runRow{}
		for rows.Next() {
			var rr runRow
			if rows.Scan(&rr.ID, &rr.Status, &rr.RowsImported, &rr.ErrorRows, &rr.Message, &rr.CreatedAt, &rr.RunBy) == nil {
				out = append(out, rr)
			}
		}
		jsonOK(w, out)
		return
	}

	if subPath == "config" && r.Method == http.MethodPatch {
		var body struct {
			Config json.RawMessage `json:"config"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Config) == 0 {
			jsonErr(w, fmt.Errorf("config required"), http.StatusBadRequest)
			return
		}
		if _, err := h.db.Exec(ctx,
			`UPDATE model.integration_def SET config=$2 WHERE id=$1::uuid`,
			intID, body.Config,
		); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			revisionID, appID := h.integrationScope(ctx, intID)
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryModelChange, EventType: auditlog.EventIntegrationUpdated,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: appID, ResourceType: "integration", ResourceID: intID, RevisionID: revisionID,
				Metadata: map[string]string{"sub_action": "config"},
			})
		}
		jsonOK(w, map[string]string{"status": "ok"})
		return
	}

	switch r.Method {
	case http.MethodPatch:
		var body struct {
			Name       string    `json:"name"`
			TargetType string    `json:"target_type"`
			TargetID   string    `json:"target_id"`
			Status     *string   `json:"status"`
			Tags       *[]string `json:"tags"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
		if body.Status != nil && *body.Status != "draft" && *body.Status != "active" {
			jsonErr(w, fmt.Errorf("status must be draft or active"), http.StatusBadRequest)
			return
		}
		if _, err := h.db.Exec(ctx,
			`UPDATE model.integration_def SET name=$2, target_type=$3, target_id=NULLIF($4,'')::uuid,
			   status = COALESCE($5, status), tags = COALESCE($6, tags)
			 WHERE id=$1::uuid`,
			intID, body.Name, body.TargetType, body.TargetID, body.Status, body.Tags,
		); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			revisionID, appID := h.integrationScope(ctx, intID)
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryModelChange, EventType: auditlog.EventIntegrationUpdated,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: appID, ResourceType: "integration", ResourceID: intID, RevisionID: revisionID,
				Metadata: map[string]string{"name": body.Name},
			})
		}
		jsonOK(w, map[string]string{"status": "ok"})
	case http.MethodDelete:
		revisionID, appID := h.integrationScope(ctx, intID)
		h.dropWidgetsReferencing(ctx, intID)
		if _, err := h.db.Exec(ctx, `DELETE FROM model.integration_def WHERE id=$1::uuid`, intID); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryModelChange, EventType: auditlog.EventIntegrationDeleted,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: appID, ResourceType: "integration", ResourceID: intID, RevisionID: revisionID,
			})
		}
		jsonOK(w, map[string]string{"status": "deleted"})
	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// integrationScope resolves the revision_id/application_id an integration
// belongs to, for audit logging call sites in developerIntegrationAction.
func (h *handler) integrationScope(ctx context.Context, intID string) (revisionID, appID string) {
	_ = h.db.QueryRow(ctx, `
		SELECT COALESCE(i.revision_id::text,''), COALESCE(m.application_id::text,'')
		FROM model.integration_def i JOIN core.model m ON m.id = i.model_id
		WHERE i.id = $1::uuid`, intID).Scan(&revisionID, &appID)
	return
}

// listIntegrations returns integrations visible to business users (for button labels).
func (h *handler) listIntegrations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	revisionID, _, revErr := h.resolveRevisionCtx(ctx, r.URL.Query().Get("revision_id"), modelID)
	if rejectForeignRevision(w, revErr) {
		return
	}
	rows, err := h.db.Query(ctx,
		`SELECT id::text, name, type, target_type, COALESCE(target_id::text,''), status, tags, config
		 FROM model.integration_def
		 WHERE model_id=$1::uuid
		   AND ($2 = '' OR revision_id IS NULL OR revision_id::text = $2)
		 ORDER BY created_at`, modelID, revisionID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var list []integrationDef
	for rows.Next() {
		var d integrationDef
		_ = rows.Scan(&d.ID, &d.Name, &d.Type, &d.TargetType, &d.TargetID, &d.Status, &d.Tags, &d.Config)
		if d.Config == nil {
			d.Config = json.RawMessage("{}")
		}
		list = append(list, d)
	}
	if list == nil {
		list = []integrationDef{}
	}
	jsonOK(w, list)
}

// integrationRun handles POST /api/integrations/{id}/run — executes a CSV import.
func (h *handler) integrationRun(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	tail := strings.TrimPrefix(r.URL.Path, "/api/integrations/")
	parts := strings.SplitN(tail, "/", 2)
	intID := parts[0]
	if len(parts) < 2 || parts[1] != "run" || r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("not found"), http.StatusNotFound)
		return
	}

	ctx := r.Context()

	// Load integration definition
	var intg integrationDef
	if err := h.db.QueryRow(ctx,
		`SELECT id::text, name, type, target_type, COALESCE(target_id::text,''), status, config
		 FROM model.integration_def WHERE id=$1::uuid`, intID,
	).Scan(&intg.ID, &intg.Name, &intg.Type, &intg.TargetType, &intg.TargetID, &intg.Status, &intg.Config); err != nil {
		jsonErr(w, fmt.Errorf("integration not found"), http.StatusNotFound)
		return
	}
	if intg.Status == "draft" {
		jsonErr(w, fmt.Errorf("this integration is a draft — finish it in the Import Wizard (Continue draft) before running"), http.StatusBadRequest)
		return
	}

	// Authenticate and verify ownership. This used to discard the
	// resolveActor error (`act, _ := ...`) and fall back to a synthetic
	// zero-UUID actor while still executing the write — the one confirmed
	// unauthenticated write path found across two audits this session. It
	// also never checked that the integration's app belongs to the
	// caller, unlike every other mutating endpoint touched this session.
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	var appID string
	if err := h.db.QueryRow(ctx, `
		SELECT m.application_id::text FROM model.integration_def i
		JOIN core.model m ON m.id = i.model_id WHERE i.id=$1::uuid`, intID).Scan(&appID); err != nil {
		jsonErr(w, fmt.Errorf("integration not found"), http.StatusNotFound)
		return
	}
	if canAccess, caErr := h.actorCanAccessApp(ctx, act, appID); caErr != nil {
		jsonErr(w, caErr, http.StatusInternalServerError)
		return
	} else if !canAccess {
		jsonErr(w, fmt.Errorf("forbidden: integration is outside your access scope"), http.StatusForbidden)
		return
	}
	if cid := h.customerOfApplication(ctx, appID); cid != "" && h.plans != nil {
		if err := h.plans.CheckIntegrationRuns(ctx, h.db.For(ctx), cid); err != nil {
			h.jsonLimitErr(w, err)
			return
		}
	}

	// rest_api runs are asynchronous: the worker executes them (the gateway
	// never makes external requests). 202 + run id; poll the run endpoint.
	if intg.Type == "rest_api" {
		h.restAPIRunEnqueue(w, r, intID)
		return
	}

	if intg.Config == nil {
		intg.Config = json.RawMessage("{}")
	}
	// Parse optional settings from config. sheet_url/import_mode are only
	// meaningful for type "google_sheets"; column_map applies to both types.
	var cfg struct {
		ColumnMap  map[string]string `json:"column_map"`
		SheetURL   string            `json:"sheet_url"`
		ImportMode string            `json:"import_mode"`
	}
	_ = json.Unmarshal(intg.Config, &cfg)

	// A csv_import integration carries its data in the request body; a
	// google_sheets one carries none — the sheet named by its config is
	// re-fetched at run time, which is what makes "run" a sync.
	var body struct {
		CSV string `json:"csv"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	csvText := body.CSV
	if intg.Type == "google_sheets" {
		if strings.TrimSpace(cfg.SheetURL) == "" {
			jsonErr(w, fmt.Errorf("integration has no sheet_url configured"), http.StatusBadRequest)
			return
		}
		sheetID, gid, perr := importpkg.ParseSheetURL(cfg.SheetURL)
		if perr != nil {
			jsonErr(w, perr, http.StatusBadRequest)
			return
		}
		data, ferr := h.fetchSheetCSV(ctx, h.requestCustomerID(ctx, r, act), sheetID, gid)
		if ferr != nil {
			jsonSheetErr(w, ferr)
			return
		}
		csvText = string(data)
	} else if csvText == "" {
		jsonErr(w, fmt.Errorf("csv field required"), http.StatusBadRequest)
		return
	}

	cr := csv.NewReader(bytes.NewReader([]byte(csvText)))
	cr.TrimLeadingSpace = true
	header, err := cr.Read()
	if err != nil {
		jsonErr(w, fmt.Errorf("csv header: %w", err), http.StatusBadRequest)
		return
	}
	// Build colIdx from original headers, then apply column_map to produce logical names.
	// column_map maps CSV header → logical field name (e.g. "Dept Name" → "code")
	rawColIdx := make(map[string]int, len(header))
	for i, col := range header {
		rawColIdx[strings.TrimSpace(col)] = i
	}
	colIdx := make(map[string]int, len(header))
	for rawCol, idx := range rawColIdx {
		if mapped, ok := cfg.ColumnMap[rawCol]; ok && mapped != "" {
			colIdx[strings.ToLower(mapped)] = idx
		} else {
			colIdx[strings.ToLower(rawCol)] = idx
		}
	}

	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryDataChange, EventType: auditlog.EventIntegrationRun,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ApplicationID: appID, ResourceType: "integration", ResourceID: intID,
		Metadata: map[string]string{"target_type": intg.TargetType, "target_id": intg.TargetID, "integration_type": intg.Type},
	})

	switch intg.TargetType {
	case "form":
		imported, errs := 0, 0
		for {
			record, err := cr.Read()
			if err != nil {
				break
			}
			data := make(map[string]any, len(header))
			for col, idx := range colIdx {
				if idx < len(record) {
					data[col] = strings.TrimSpace(record[idx])
				}
			}
			if _, err := h.db.Exec(ctx,
				`INSERT INTO model.form_record (form_id, data, status, created_by)
				 VALUES ($1::uuid, $2, 'submitted', $3::uuid)`,
				intg.TargetID, mustJSON(data), act.UserID,
			); err != nil {
				errs++
			} else {
				imported++
			}
		}
		h.recordIntegrationRun(ctx, intID, act.UserID, imported, errs, "success", "")
		jsonOK(w, map[string]any{"rows_imported": imported, "error_rows": errs})

	case "dimension":
		imported, errs, importErr := h.importDimensionMembersCSV(ctx, intg.TargetID, cr, colIdx)
		if importErr != nil {
			h.recordIntegrationRun(ctx, intID, act.UserID, 0, 0, "error", importErr.Error())
			jsonErr(w, importErr, http.StatusBadRequest)
			return
		}
		h.recordIntegrationRun(ctx, intID, act.UserID, imported, errs, "success", "")
		jsonOK(w, map[string]any{"rows_imported": imported, "error_rows": errs})

	default: // "grid"
		if intg.Type == "google_sheets" {
			// The legacy path below stages raw metric_id cell values, so it
			// requires UUIDs; a sheet a business team maintains holds metric
			// and member names. Route through ResolveRows instead.
			h.runSheetsGridImport(w, r, act, cfg.ColumnMap, cfg.ImportMode, csvText)
			return
		}
		modelID, err := h.resolveDemoModelID(ctx, r)
		if err != nil {
			jsonAccessErr(w, err, "resolve model")
			return
		}
		var intRevisionID string
		_ = h.db.QueryRow(ctx, `
			SELECT COALESCE(active_revision_id::text, (SELECT id::text FROM model.revision WHERE model_id = m.id ORDER BY created_at LIMIT 1), '')
			FROM core.model m WHERE m.id = $1::uuid
		`, modelID).Scan(&intRevisionID)

		metricCol, hasMetric := colIdx["metric_id"]
		valueCol, hasValue := colIdx["value"]
		if !hasMetric || !hasValue {
			jsonErr(w, fmt.Errorf("grid csv must have 'metric_id' and 'value' columns"), http.StatusBadRequest)
			return
		}
		dimCols := make(map[string]int)
		for col, idx := range colIdx {
			if col != "metric_id" && col != "value" {
				dimCols[col] = idx
			}
		}

		// Resolve dimension column names → UUIDs (same format as writeback).
		intDimNameToID := make(map[string]string, len(dimCols))
		for colName := range dimCols {
			var dimID string
			if err := h.db.QueryRow(ctx,
				`SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND lower(name)=$2 LIMIT 1`,
				modelID, colName,
			).Scan(&dimID); err == nil {
				intDimNameToID[colName] = dimID
			} else {
				intDimNameToID[colName] = colName
			}
		}

		store := importpkg.NewStore(h.db.For(ctx))
		job, err := store.CreateImportJob(ctx, modelID, intRevisionID, "integration", act.UserID, nil)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}

		var staged []importpkg.StagingRow
		totalRows, errorRows := 0, 0
		for {
			record, err := cr.Read()
			if err != nil {
				break
			}
			totalRows++
			val, err := strconv.ParseFloat(strings.TrimSpace(record[valueCol]), 64)
			if err != nil {
				errorRows++
				continue
			}
			dims := make(map[string]string, len(dimCols))
			for col, idx := range dimCols {
				dims[intDimNameToID[col]] = strings.TrimSpace(record[idx])
			}
			staged = append(staged, importpkg.StagingRow{
				MetricID: strings.TrimSpace(record[metricCol]), DimMembers: dims,
				Value: val, RowNumber: totalRows,
			})
		}
		if len(staged) == 0 {
			h.recordIntegrationRun(ctx, intID, act.UserID, 0, errorRows, "error", "no valid rows")
			jsonOK(w, map[string]any{"rows_imported": 0, "error_rows": errorRows})
			return
		}
		if err := store.StageRows(ctx, job.Id, staged, nil); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		metricIDs, err := store.CommitImport(ctx, job.Id, modelID, intRevisionID, act.UserID, importpkg.ModeIncremental)
		if err != nil {
			h.recordIntegrationRun(ctx, intID, act.UserID, 0, totalRows, "error", err.Error())
			if errors.Is(err, importpkg.ErrWriteDenied) {
				jsonErr(w, err, http.StatusForbidden)
				return
			}
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if len(metricIDs) > 0 {
			calcStore := calculation.NewStore(h.db.For(ctx))
			sched := calculation.NewScheduler(h.log, calcStore, nil)
			go func() { _ = sched.RecalcAffected(context.Background(), modelID, intRevisionID, metricIDs) }() //nolint:contextcheck
		}
		h.recordIntegrationRun(ctx, intID, act.UserID, totalRows-errorRows, errorRows, "success", "")
		jsonOK(w, map[string]any{"rows_imported": totalRows - errorRows, "error_rows": errorRows})
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// recordIntegrationRun appends one row of a saved integration's run history
// (model.integration_run) — best-effort: a history write must never fail
// the run whose outcome it records.
func (h *handler) recordIntegrationRun(ctx context.Context, intID, userID string, imported, errRows int, status, message string) {
	if _, err := h.db.Exec(ctx, `
		INSERT INTO model.integration_run (integration_id, run_by, rows_imported, error_rows, status, message)
		VALUES ($1::uuid, NULLIF($2,'')::uuid, $3, $4, $5, $6)
	`, intID, userID, imported, errRows, status, message); err != nil {
		h.log.Warn().Err(err).Str("integration_id", intID).Msg("record integration run")
	}
}

// ── Form-to-Metric Integrations (Phase 3B) ────────────────────────────────────

func (h *handler) developerFormIntegrations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	revisionID, _, revErr := h.resolveRevisionCtx(ctx, r.URL.Query().Get("revision_id"), modelID)
	if rejectForeignRevision(w, revErr) {
		return
	}

	if r.Method == http.MethodPost {
		var body struct {
			FormID            string            `json:"form_id"`
			GridID            string            `json:"grid_id"`
			Name              string            `json:"name"`
			SourceField       string            `json:"source_field"`
			TargetMetricID    string            `json:"target_metric_id"`
			Aggregation       string            `json:"aggregation"`
			PostingStatuses   []string          `json:"posting_statuses"`
			DimensionMappings map[string]string `json:"dimension_mappings"`
			LivePosting       *bool             `json:"live_posting"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.FormID == "" || body.Name == "" {
			jsonErr(w, fmt.Errorf("form_id and name required"), http.StatusBadRequest)
			return
		}
		if body.Aggregation == "" {
			body.Aggregation = "sum"
		}
		if len(body.PostingStatuses) == 0 {
			body.PostingStatuses = []string{"approved"}
		}
		if body.DimensionMappings == nil {
			body.DimensionMappings = map[string]string{}
		}
		dimJSON, _ := json.Marshal(body.DimensionMappings)

		livePosting := true
		if body.LivePosting != nil {
			livePosting = *body.LivePosting
		}
		var newID string
		if err := h.db.QueryRow(ctx, `
			INSERT INTO model.form_metric_mapping
			  (model_id, form_id, grid_id, name, source_field, target_metric_id, aggregation,
			   posting_statuses, dimension_mappings, live_posting, revision_id)
			VALUES ($1::uuid, $2::uuid, NULLIF($3,'')::uuid, $4, $5, $6::uuid, $7, $8, $9, $10, NULLIF($11,'')::uuid)
			RETURNING id::text`,
			modelID, body.FormID, body.GridID, body.Name, body.SourceField, body.TargetMetricID,
			body.Aggregation, body.PostingStatuses, dimJSON, livePosting, revisionID,
		).Scan(&newID); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			var appID string
			_ = h.db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID)
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryModelChange, EventType: auditlog.EventFormIntegrationCreated,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: appID, ResourceType: "form_metric_mapping", ResourceID: newID, RevisionID: revisionID,
				Metadata: map[string]string{"name": body.Name, "form_id": body.FormID},
			})
		}
		jsonOK(w, map[string]string{"id": newID, "status": "created"})
		return
	}

	rows, err := h.db.Query(ctx, `
		SELECT id::text, form_id::text, COALESCE(grid_id::text,''), name, source_field,
		       target_metric_id::text, aggregation, posting_statuses, dimension_mappings, live_posting
		FROM model.form_metric_mapping
		WHERE model_id=$1::uuid
		  AND ($2 = '' OR revision_id IS NULL OR revision_id::text = $2)
		ORDER BY created_at`, modelID, revisionID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var list []map[string]any
	for rows.Next() {
		var (
			id, formID, gridID, name, sourceField, targetMetricID string
			aggregation                                           string
			postingStatuses                                       []string
			dimMappingsRaw                                        []byte
			livePosting                                           bool
		)
		if err := rows.Scan(&id, &formID, &gridID, &name, &sourceField, &targetMetricID,
			&aggregation, &postingStatuses, &dimMappingsRaw, &livePosting,
		); err != nil {
			continue
		}
		var dimMappings map[string]string
		_ = json.Unmarshal(dimMappingsRaw, &dimMappings)
		list = append(list, map[string]any{
			"id": id, "form_id": formID, "grid_id": gridID, "name": name,
			"source_field": sourceField, "target_metric_id": targetMetricID,
			"aggregation": aggregation, "posting_statuses": postingStatuses,
			"dimension_mappings": dimMappings, "live_posting": livePosting,
		})
	}
	if list == nil {
		list = []map[string]any{}
	}
	jsonOK(w, list)
}

func (h *handler) developerFormIntegrationAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	tail := strings.TrimPrefix(r.URL.Path, "/api/developer/form-integrations/")
	parts := strings.SplitN(tail, "/", 2)
	mappingID := parts[0]

	if !h.requireResourceAccess(w, r, "form_metric_mapping", mappingID) {
		return
	}
	subPath := ""
	if len(parts) == 2 {
		subPath = parts[1]
	}

	if subPath == "backfill" && r.Method == http.MethodPost {
		count, err := h.backfillFormMapping(ctx, mappingID, h.resolveUserID(r))
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			revisionID, appID := h.formIntegrationScope(ctx, mappingID)
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryDataChange, EventType: auditlog.EventFormIntegrationBackfilled,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: appID, ResourceType: "form_metric_mapping", ResourceID: mappingID, RevisionID: revisionID,
				Metadata: map[string]string{"records_processed": strconv.Itoa(count)},
			})
		}
		jsonOK(w, map[string]any{"status": "ok", "records_processed": count})
		return
	}

	if subPath == "preview" && r.Method == http.MethodGet {
		previews, err := h.previewFormMapping(ctx, mappingID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		jsonOK(w, previews)
		return
	}

	switch r.Method {
	case http.MethodPatch:
		var body struct {
			GridID            string            `json:"grid_id"`
			Name              string            `json:"name"`
			SourceField       string            `json:"source_field"`
			TargetMetricID    string            `json:"target_metric_id"`
			Aggregation       string            `json:"aggregation"`
			PostingStatuses   []string          `json:"posting_statuses"`
			DimensionMappings map[string]string `json:"dimension_mappings"`
			LivePosting       *bool             `json:"live_posting"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
		dimJSON, _ := json.Marshal(body.DimensionMappings)
		livePosting := true
		if body.LivePosting != nil {
			livePosting = *body.LivePosting
		}
		if _, err := h.db.Exec(ctx, `
			UPDATE model.form_metric_mapping SET
			  grid_id=NULLIF($2,'')::uuid, name=$3, source_field=$4,
			  target_metric_id=$5::uuid, aggregation=$6,
			  posting_statuses=$7, dimension_mappings=$8, live_posting=$9
			WHERE id=$1::uuid`,
			mappingID, body.GridID, body.Name, body.SourceField, body.TargetMetricID, body.Aggregation,
			body.PostingStatuses, dimJSON, livePosting,
		); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		// Re-apply immediately so changes (aggregation, posting statuses, etc.) take effect.
		// Resolve actor/scope synchronously (before spawning) so the
		// goroutine below can log the backfill it performs — this write is
		// identical to what the dedicated POST .../backfill endpoint does,
		// which already audits it; this call site previously didn't.
		userID := h.resolveUserID(r)
		auditActor, _ := h.resolveActor(ctx, r)
		revisionID, appID := h.formIntegrationScope(ctx, mappingID)
		go func(ctx context.Context) {
			count, err := h.backfillFormMapping(ctx, mappingID, userID)
			if err != nil {
				h.log.Warn().Err(err).Str("mapping_id", mappingID).Msg("re-apply after mapping update failed")
				return
			}
			if auditActor != nil {
				auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
					Category: auditlog.CategoryDataChange, EventType: auditlog.EventFormIntegrationBackfilled,
					ActorUserID: auditActor.UserID, ActorRole: strings.Join(auditActor.Roles, ","),
					ApplicationID: appID, ResourceType: "form_metric_mapping", ResourceID: mappingID, RevisionID: revisionID,
					Metadata: map[string]string{"records_processed": strconv.Itoa(count), "trigger": "mapping_updated"},
				})
			}
		}(context.WithoutCancel(ctx))
		if auditActor != nil {
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryModelChange, EventType: auditlog.EventFormIntegrationUpdated,
				ActorUserID: auditActor.UserID, ActorRole: strings.Join(auditActor.Roles, ","),
				ApplicationID: appID, ResourceType: "form_metric_mapping", ResourceID: mappingID, RevisionID: revisionID,
				Metadata: map[string]string{"name": body.Name},
			})
		}
		jsonOK(w, map[string]string{"status": "ok"})
	case http.MethodDelete:
		revisionID, appID := h.formIntegrationScope(ctx, mappingID)
		if _, err := h.db.Exec(ctx, `DELETE FROM model.form_metric_mapping WHERE id=$1::uuid`, mappingID); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryModelChange, EventType: auditlog.EventFormIntegrationDeleted,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: appID, ResourceType: "form_metric_mapping", ResourceID: mappingID, RevisionID: revisionID,
			})
		}
		jsonOK(w, map[string]string{"status": "deleted"})
	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// formIntegrationScope resolves the revision_id/application_id a
// form-metric mapping belongs to, for audit logging call sites in
// developerFormIntegrationAction.
func (h *handler) formIntegrationScope(ctx context.Context, mappingID string) (revisionID, appID string) {
	_ = h.db.QueryRow(ctx, `
		SELECT COALESCE(fm.revision_id::text,''), COALESCE(m.application_id::text,'')
		FROM model.form_metric_mapping fm JOIN core.model m ON m.id = fm.model_id
		WHERE fm.id = $1::uuid`, mappingID).Scan(&revisionID, &appID)
	return
}

// applyFormMappings evaluates all live form-metric mappings for a given record
// and posts (or retracts) values to runtime.fact_input based on status rules.
// retryTransient runs op up to attempts times, pausing between tries.
//
// applyFormMappings and recomputeFactInput run in a goroutine detached from
// the request (`go func()` per record status-transition), so there is no
// caller to return an error to and nothing schedules a second attempt. Every
// failure path in them therefore used to `continue` or `return` — which left
// the mapping's aggregate PERMANENTLY wrong, not merely late: a posting that
// failed to insert is never re-posted, and a recompute that failed leaves
// whatever the previous one wrote. One transient error under load (a pool
// acquire timeout, a lock wait) silently under-counts a metric with nothing
// surfaced to any user.
//
// That is what CI caught in TestFormMappingConcurrentPostingsConverge: eight
// concurrent approvals settled at 300 instead of 360 and STAYED there for the
// full 10s the test polls — a stable wrong value, with rowCount=1 ruling out
// the duplicate-snapshot race the test was originally written for.
//
// Retrying is safe because every operation wrapped here is idempotent: the
// access checks are pure reads, the posting insert is ON CONFLICT DO UPDATE,
// and the recompute derives the aggregate from all posting rows rather than
// adjusting it incrementally.
func retryTransient(ctx context.Context, attempts int, op func() error) error {
	var err error
	for i := 0; i < attempts; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(i) * 100 * time.Millisecond):
			}
		}
		if err = op(); err == nil {
			return nil
		}
	}
	return err
}

func (h *handler) applyFormMappings(ctx context.Context, recordID, formID, status string, data map[string]any, userID string) error {
	// Every caller fires this from its own `go func()` per record
	// status-transition, so two calls for the SAME record (e.g. its
	// creation-as-draft call and a subsequent approve call) race with no
	// ordering guarantee — a stale call captured at spawn time can run
	// AFTER a chronologically later one and wrongly retract its posting.
	// Re-reading the record's current status/data here (rather than
	// trusting the possibly-stale status/data parameters) makes every
	// invocation reconcile against ground truth: a late-arriving stale
	// call sees the record's real current state and becomes a no-op or a
	// harmless re-post of the same value, instead of undoing a newer
	// call's work.
	var rawData []byte
	if err := h.db.QueryRow(ctx,
		`SELECT status::text, data FROM runtime.form_record WHERE id=$1::uuid`, recordID,
	).Scan(&status, &rawData); err != nil {
		h.log.Warn().Err(err).Str("record_id", recordID).Msg("applyFormMappings: re-read current record state failed")
		return err
	}
	data = map[string]any{}
	_ = json.Unmarshal(rawData, &data)

	rows, err := h.db.Query(ctx, `
		SELECT id::text, source_field, target_metric_id::text,
		       aggregation, posting_statuses, dimension_mappings,
		       model_id::text, COALESCE(revision_id::text,'')
		FROM model.form_metric_mapping
		WHERE form_id=$1::uuid AND live_posting = TRUE`, formID)
	if err != nil {
		return err
	}
	defer rows.Close()

	type mapping struct {
		id, sourceField, targetMetricID string
		aggregation                     string
		postingStatuses                 []string
		dimMappings                     map[string]string
		modelID                         string
		revisionID                      string
	}

	var mappings []mapping
	for rows.Next() {
		var m mapping
		var dimRaw []byte
		if err := rows.Scan(
			&m.id, &m.sourceField, &m.targetMetricID,
			&m.aggregation, &m.postingStatuses, &dimRaw,
			&m.modelID, &m.revisionID,
		); err != nil {
			h.log.Warn().Err(err).Str("form_id", formID).Msg("applyFormMappings: scan mapping row")
			continue
		}
		_ = json.Unmarshal(dimRaw, &m.dimMappings)
		mappings = append(mappings, m)
	}
	rows.Close()

	var affected []struct{ RevisionID, MetricID string }
	var affectedModelID string

	for _, m := range mappings {
		eligible := false
		for _, s := range m.postingStatuses {
			if s == status {
				eligible = true
				break
			}
		}

		// Post into the mapping's own revision; revision-global (legacy)
		// mappings fall back to the model's active revision, then most-recent.
		revisionID := m.revisionID
		if revisionID == "" {
			_ = h.db.QueryRow(ctx, `
				SELECT COALESCE(
					m.active_revision_id::text,
					(SELECT s.id::text FROM model.revision s WHERE s.model_id=m.id ORDER BY s.created_at DESC LIMIT 1)
				) FROM core.model m WHERE m.id=$1::uuid`, m.modelID,
			).Scan(&revisionID)
		}

		// Always remove old posting first (retraction on status change).
		_, _ = h.db.Exec(ctx,
			`DELETE FROM runtime.form_record_posting WHERE mapping_id=$1::uuid AND form_record_id=$2::uuid`,
			m.id, recordID)

		if !eligible {
			// Status is not eligible — delete any fact_input row this mapping produced
			// and re-aggregate remaining postings for this intersection.
			h.recomputeFactInput(ctx, m.id, m.targetMetricID, m.aggregation, m.modelID, revisionID, userID)
			affected = append(affected, struct{ RevisionID, MetricID string }{revisionID, m.targetMetricID})
			affectedModelID = m.modelID
			continue
		}

		// Resolve source value
		raw, ok := data[m.sourceField]
		if !ok || raw == nil {
			continue
		}
		var val float64
		switch v := raw.(type) {
		case float64:
			val = v
		case json.Number:
			val, _ = v.Float64()
		case int:
			val = float64(v)
		default:
			continue
		}

		// Build dim_members from dimension_mappings {dim_id: form_field_name}
		dimMembers := map[string]string{}
		for dimID, fieldName := range m.dimMappings {
			if fv, ok := data[fieldName]; ok && fv != nil {
				dimMembers[dimID] = fmt.Sprintf("%v", fv)
			}
		}

		// Generic write guard — the same check cells() applies to every
		// interactive grid writeback (writeguard.MetricAccess +
		// writeguard.CheckWrite), never previously wired into the forms
		// path: a user hidden/read-restricted from this metric or from one
		// of the dimension members this posting targets must not have that
		// restriction silently bypassed by submitting a form instead of
		// writing the cell directly. Skip-and-log rather than surface an
		// error: this runs from a detached goroutine after the HTTP
		// response has already returned (the record itself always saves;
		// only this mapping's derived posting is withheld), matching the
		// existing skip-and-continue handling a few lines up for a missing
		// source value.
		// A transient error here is NOT a denial, but both used to `continue`
		// and drop the posting for good. Retry the read; only give up — and
		// say so at error level — once it keeps failing.
		var access string
		if maErr := retryTransient(ctx, 3, func() error {
			a, e := writeguard.MetricAccess(ctx, h.db.For(ctx), userID, m.targetMetricID)
			access = a
			return e
		}); maErr != nil {
			h.log.Error().Err(maErr).Str("mapping_id", m.id).Str("record_id", recordID).Msg("applyFormMappings: metric access check kept failing — posting dropped, aggregate now under-counts")
			continue
		} else if access == "hidden" || access == "read" {
			h.log.Warn().Str("mapping_id", m.id).Str("metric_id", m.targetMetricID).Msg("applyFormMappings: posting rejected, caller lacks write access to metric")
			continue
		}
		memberIDs := make([]string, 0, len(dimMembers))
		for dimID, code := range dimMembers {
			var memberID string
			if err := h.db.QueryRow(ctx,
				`SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`,
				dimID, code,
			).Scan(&memberID); err != nil {
				continue // unknown member: nothing to restrict here, mirrors cells()
			}
			memberIDs = append(memberIDs, memberID)
		}
		var reason string
		if gErr := retryTransient(ctx, 3, func() error {
			r, e := writeguard.CheckWriteMetrics(ctx, h.db.For(ctx), m.modelID, revisionID, userID, memberIDs, []string{m.targetMetricID})
			reason = r
			return e
		}); gErr != nil {
			h.log.Error().Err(gErr).Str("mapping_id", m.id).Str("record_id", recordID).Msg("applyFormMappings: write guard check kept failing — posting dropped, aggregate now under-counts")
			continue
		} else if reason != "" {
			h.log.Warn().Str("mapping_id", m.id).Str("reason", reason).Msg("applyFormMappings: posting rejected by write guard")
			continue
		}

		dimJSON, _ := json.Marshal(dimMembers)

		// Insert posting record. This is the failure that most directly
		// under-counts a total: the posting row IS the record's contribution,
		// so losing it loses that record's value until something else happens
		// to re-post it. ON CONFLICT DO UPDATE makes the retry idempotent.
		if postErr := retryTransient(ctx, 3, func() error {
			_, e := h.db.Exec(ctx, `
				INSERT INTO runtime.form_record_posting
				  (mapping_id, form_record_id, target_metric_id, revision_id, dim_members, posted_value)
				VALUES ($1::uuid, $2::uuid, $3::uuid, NULLIF($4,'')::uuid, $5, $6)
				ON CONFLICT (mapping_id, form_record_id) DO UPDATE
				  SET target_metric_id=$3::uuid, revision_id=NULLIF($4,'')::uuid,
				      dim_members=$5, posted_value=$6, posted_at=now()`,
				m.id, recordID, m.targetMetricID, revisionID, dimJSON, val)
			return e
		}); postErr != nil {
			h.log.Error().Err(postErr).Str("mapping_id", m.id).Str("record_id", recordID).Msg("form posting insert kept failing — this record's value is missing from the metric total")
			continue
		}

		h.recomputeFactInput(ctx, m.id, m.targetMetricID, m.aggregation, m.modelID, revisionID, userID)

		affected = append(affected, struct{ RevisionID, MetricID string }{revisionID, m.targetMetricID})
		affectedModelID = m.modelID
	}

	// Recalc against each mapping's OWN revision (captured above, per
	// mapping — revision-scoped mappings can legitimately target a
	// different revision than the model's current active one), not a
	// single re-derived active_revision_id: that discarded per-mapping
	// scoping entirely, silently skipping recalc altogether whenever the
	// model had no active revision yet (e.g. pre-promote), or recalculating
	// the wrong revision when a mapping's own revision differed from it.
	h.recalcAfterDimChange(ctx, affectedModelID, affected)
	return nil
}

// recomputeFactInput aggregates all active postings for a mapping/metric
// and upserts a single fact_input row tagged with source_ref=mapping_id.
//
// Runs inside a transaction holding a Postgres advisory lock keyed on
// mappingID for its duration: applyFormMappings is invoked from an
// unguarded `go func()` per record status-transition (see recordAction/the
// /api/forms/{id}/records POST handler), so e.g. approving several records
// against the same mapping in quick succession schedules multiple
// concurrent calls here. Without serialization, two overlapping
// delete-then-insert passes can interleave (both DELETE, then both INSERT)
// and leave more than one stale snapshot behind — and since the grid read
// path sums every source_ref-tagged fact_input row for a cell (form
// postings are additive by design, unlike direct-entry rows which are
// latest-wins), a leftover stale snapshot silently gets counted again on
// top of the correct one. Found via cmd/qa-engine-test: 3 records posting
// 10/20/30 into a sum-aggregation mapping intermittently produced 2-3
// distinct fact_input rows for the one mapping (e.g. 30 AND 60, timestamps
// microseconds apart) instead of one settled 60, inflating the metric's
// total by the stale rows' values.
// recomputeFactInput rebuilds the mapping's aggregate from every posting row.
//
// Retried because a failed recompute is not a delayed one: nothing else
// recalculates this mapping until the next posting happens to arrive, so a
// single transient failure leaves the metric showing whatever the previous
// recompute wrote — silently stale, indistinguishable from a correct total.
// The whole operation is idempotent (it derives the value from ground truth
// inside one transaction), so re-running it is always safe.
func (h *handler) recomputeFactInput(ctx context.Context, mappingID, targetMetricID, aggregation, modelID, revisionID, userID string) {
	if err := retryTransient(ctx, 3, func() error {
		return h.recomputeFactInputOnce(ctx, mappingID, targetMetricID, aggregation, modelID, revisionID, userID)
	}); err != nil {
		h.log.Error().Err(err).Str("mapping_id", mappingID).Str("metric_id", targetMetricID).Msg("recomputeFactInput kept failing — this metric's form-posted total is now stale")
	}
}

func (h *handler) recomputeFactInputOnce(ctx context.Context, mappingID, targetMetricID, aggregation, modelID, revisionID, userID string) error {
	tx, err := h.db.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, mappingID); err != nil {
		return fmt.Errorf("advisory lock: %w", err)
	}

	rows, err := tx.Query(ctx, `
		SELECT revision_id::text, dim_members, SUM(posted_value), AVG(posted_value),
		       MIN(posted_value), MAX(posted_value), COUNT(*),
		       MAX(posted_value) FILTER (WHERE posted_at = (SELECT MAX(p2.posted_at) FROM runtime.form_record_posting p2 WHERE p2.mapping_id=$1::uuid))
		FROM runtime.form_record_posting
		WHERE mapping_id=$1::uuid
		GROUP BY revision_id, dim_members`, mappingID)
	if err != nil {
		return fmt.Errorf("query postings: %w", err)
	}

	type aggRow struct {
		revisionID                     string
		dimMembers                     []byte
		sumVal, avgVal, minVal, maxVal *float64
		cnt                            int64
		lastVal                        *float64
	}
	var aggs []aggRow
	for rows.Next() {
		var ag aggRow
		if err := rows.Scan(&ag.revisionID, &ag.dimMembers,
			&ag.sumVal, &ag.avgVal, &ag.minVal, &ag.maxVal, &ag.cnt, &ag.lastVal,
		); err == nil {
			aggs = append(aggs, ag)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate postings: %w", err)
	}

	// Delete existing fact_input rows for this mapping source.
	// For "replace" aggregation also remove direct-entry rows for the same
	// (metric, dim) combos so the form value fully replaces them.
	// The reason travels with the transaction: the archive trigger on
	// runtime.fact_input records it against every row it keeps.
	_, _ = tx.Exec(ctx, `SET LOCAL mvx.delete_reason = 'form_reposted'`)
	if _, err := tx.Exec(ctx,
		`DELETE FROM runtime.fact_input WHERE source_ref=$1::uuid AND metric_id=$2::uuid`,
		mappingID, targetMetricID); err != nil {
		return fmt.Errorf("delete existing fact rows: %w", err)
	}

	if len(aggs) > 0 {
		// Resolve user ID for entered_by FK — use passed userID if valid, else any real user.
		systemUserID := userID
		if systemUserID == "" {
			_ = tx.QueryRow(ctx, `SELECT id::text FROM identity.user LIMIT 1`).Scan(&systemUserID)
		}

		for _, ag := range aggs {
			var val float64
			switch aggregation {
			case "sum", "":
				if ag.sumVal != nil {
					val = *ag.sumVal
				}
			case "average":
				if ag.avgVal != nil {
					val = *ag.avgVal
				}
			case "min":
				if ag.minVal != nil {
					val = *ag.minVal
				}
			case "max":
				if ag.maxVal != nil {
					val = *ag.maxVal
				}
			case "count":
				val = float64(ag.cnt)
			case "last", "replace":
				if ag.lastVal != nil {
					val = *ag.lastVal
				} else if ag.sumVal != nil {
					val = *ag.sumVal
				}
			}

			useRevID := ag.revisionID
			if useRevID == "" {
				useRevID = revisionID
			}
			if _, err := tx.Exec(ctx, `
				INSERT INTO runtime.fact_input
				  (model_id, revision_id, metric_id, dim_members, value, entered_by, source_ref)
				VALUES ($1::uuid, NULLIF($2,'')::uuid, $3::uuid, $4, $5, $6::uuid, $7::uuid)`,
				modelID, useRevID, targetMetricID, ag.dimMembers, val, systemUserID, mappingID); err != nil {
				return fmt.Errorf("insert aggregate row: %w", err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// backfillFormMapping processes all existing records that match the posting status rule.
func (h *handler) backfillFormMapping(ctx context.Context, mappingID, userID string) (int, error) {
	var formIDStr, status0 string
	var postingStatuses []string
	if err := h.db.QueryRow(ctx, `
		SELECT form_id::text, posting_statuses[1]
		FROM model.form_metric_mapping WHERE id=$1::uuid`, mappingID,
	).Scan(&formIDStr, &status0); err != nil {
		return 0, fmt.Errorf("mapping not found: %w", err)
	}
	// Get all posting statuses for the mapping
	rows, err := h.db.Query(ctx, `
		SELECT fr.id::text, fr.data, fr.status::text
		FROM runtime.form_record fr
		JOIN model.form_metric_mapping m ON m.form_id = fr.form_id
		WHERE m.id=$1::uuid`, mappingID)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	_ = status0
	_ = formIDStr
	_ = postingStatuses

	type recRow struct {
		id, status string
		data       []byte
	}
	var recs []recRow
	for rows.Next() {
		var rec recRow
		if err := rows.Scan(&rec.id, &rec.data, &rec.status); err == nil {
			recs = append(recs, rec)
		}
	}
	rows.Close()

	// Resolve a valid entered_by user — prefer the passed userID, fall back to first user in DB.
	resolvedUser := userID
	if resolvedUser == "" {
		_ = h.db.QueryRow(ctx, `SELECT id::text FROM identity.user LIMIT 1`).Scan(&resolvedUser)
	}

	count := 0
	for _, rec := range recs {
		var data map[string]any
		if err := json.Unmarshal(rec.data, &data); err != nil {
			continue
		}
		var fid string
		_ = h.db.QueryRow(ctx, `SELECT form_id::text FROM runtime.form_record WHERE id=$1::uuid`, rec.id).Scan(&fid)
		if err := h.applyFormMappings(ctx, rec.id, fid, rec.status, data, resolvedUser); err == nil {
			count++
		}
	}
	return count, nil
}

// previewFormMapping returns what postings would be created for eligible records.
func (h *handler) previewFormMapping(ctx context.Context, mappingID string) ([]map[string]any, error) {
	rows, err := h.db.Query(ctx, `
		SELECT p.form_record_id::text, p.target_metric_id::text,
		       COALESCE(p.revision_id::text,''), p.dim_members, p.posted_value, p.posted_at
		FROM runtime.form_record_posting p
		WHERE p.mapping_id=$1::uuid
		ORDER BY p.posted_at DESC LIMIT 100`, mappingID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []map[string]any
	for rows.Next() {
		var recID, metricID, revisionID string
		var dimRaw []byte
		var val *float64
		var postedAt time.Time
		if err := rows.Scan(&recID, &metricID, &revisionID, &dimRaw, &val, &postedAt); err != nil {
			continue
		}
		var dims map[string]string
		_ = json.Unmarshal(dimRaw, &dims)
		result = append(result, map[string]any{
			"form_record_id":   recID,
			"target_metric_id": metricID,
			"revision_id":      revisionID,
			"dim_members":      dims,
			"posted_value":     val,
			"posted_at":        postedAt.Format(time.RFC3339),
		})
	}
	if result == nil {
		result = []map[string]any{}
	}
	return result, nil
}

// ── Schema Migration ──────────────────────────────────────────────────────────

// autoMigrate regenerates and applies the schema migration silently after model changes.
func (h *handler) autoMigrate(ctx context.Context, modelID string) {
	store := schemamigration.NewStore(h.db.For(ctx))
	gen := schemamigration.NewGenerator(h.db.For(ctx))

	nextVer, err := store.NextVersion(ctx, modelID)
	if err != nil {
		h.log.Warn().Err(err).Msg("autoMigrate: next version")
		return
	}
	files, err := gen.Generate(ctx, modelID, nextVer)
	if err != nil {
		h.log.Warn().Err(err).Msg("autoMigrate: generate")
		return
	}
	rec, err := store.Create(ctx, modelID, "", nextVer, files)
	if err != nil {
		h.log.Warn().Err(err).Msg("autoMigrate: create record")
		return
	}
	if err := store.Apply(ctx, rec.ID); err != nil {
		h.log.Warn().Err(err).Msg("autoMigrate: apply")
	}
}

func (h *handler) migrationGenerate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()

	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}

	store := schemamigration.NewStore(h.db.For(ctx))
	gen := schemamigration.NewGenerator(h.db.For(ctx))

	nextVer, err := store.NextVersion(ctx, modelID)
	if err != nil {
		jsonErr(w, fmt.Errorf("next version: %w", err), http.StatusInternalServerError)
		return
	}

	files, err := gen.Generate(ctx, modelID, nextVer)
	if err != nil {
		jsonErr(w, fmt.Errorf("generate: %w", err), http.StatusInternalServerError)
		return
	}

	if a, e := h.resolveActor(ctx, r); e == nil {
		var appID string
		_ = h.db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID)
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryModelChange, EventType: auditlog.EventSchemaMigrationGenerated,
			ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
			ApplicationID: appID, ResourceType: "model", ResourceID: modelID,
			Metadata: map[string]string{"version_number": strconv.Itoa(int(nextVer))},
		})
	}
	jsonOK(w, map[string]any{
		"model_id":       modelID,
		"version_number": nextVer,
		"files":          files,
	})
}

func (h *handler) migrationApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()

	var body struct {
		ModelID       string `json:"model_id"`
		VersionNumber int32  `json:"version_number"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
		return
	}
	if body.ModelID == "" {
		jsonErr(w, fmt.Errorf("model_id required"), http.StatusBadRequest)
		return
	}
	// The model arrives in the body, so route-level guard(..., "developer")
	// proves only that the caller is a developer SOMEWHERE. This endpoint
	// generates and APPLIES a schema migration — DDL against the model's
	// physical tables — so an unchecked model_id let a developer in one tenant
	// alter another tenant's schema. The P0 sweep that introduced
	// requireResourceAccess covered IDs taken from the URL path and missed the
	// handful, like this one, that take them from the body.
	if !h.requireResourceAccess(w, r, "model", body.ModelID) {
		return
	}

	store := schemamigration.NewStore(h.db.For(ctx))
	gen := schemamigration.NewGenerator(h.db.For(ctx))

	files, err := gen.Generate(ctx, body.ModelID, body.VersionNumber)
	if err != nil {
		jsonErr(w, fmt.Errorf("generate: %w", err), http.StatusInternalServerError)
		return
	}

	rec, err := store.Create(ctx, body.ModelID, "", body.VersionNumber, files)
	if err != nil {
		jsonErr(w, fmt.Errorf("create record: %w", err), http.StatusInternalServerError)
		return
	}

	if err := store.Apply(ctx, rec.ID); err != nil {
		jsonErr(w, fmt.Errorf("apply: %w", err), http.StatusInternalServerError)
		return
	}

	if a, e := h.resolveActor(ctx, r); e == nil {
		var appID string
		_ = h.db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, body.ModelID).Scan(&appID)
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryModelChange, EventType: auditlog.EventSchemaMigrationApplied,
			ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
			ApplicationID: appID, ResourceType: "model", ResourceID: body.ModelID,
			Metadata: map[string]string{"version_number": strconv.Itoa(int(body.VersionNumber)), "migration_id": rec.ID},
		})
	}
	jsonOK(w, map[string]any{
		"migration_id":   rec.ID,
		"version_number": body.VersionNumber,
		"status":         "applied",
		"files_applied":  len(files),
	})
}

// ── CRUD Forms ────────────────────────────────────────────────────────────────

func (h *handler) appIDFromFormID(ctx context.Context, formID string) (string, error) {
	var appID string
	err := h.db.QueryRow(ctx, `
		SELECT a.id::text
		FROM model.form_def fd
		JOIN core.model m ON m.id = fd.model_id
		JOIN core.application a ON a.id = m.application_id
		WHERE fd.id = $1::uuid
	`, formID).Scan(&appID)
	return appID, err
}

// resolveAppRevisionID resolves the working revision for application-scoped
// endpoints (workflows, automation rules): explicit ?revision_id wins, else
// the active revision of the app's model. Returns "" (revision-global) when
// the app has no revisions yet.
func (h *handler) resolveAppRevisionID(ctx context.Context, r *http.Request, appID string) string {
	if rev := r.URL.Query().Get("revision_id"); rev != "" {
		return rev
	}
	var modelID string
	if err := h.db.QueryRow(ctx,
		`SELECT id::text FROM core.model WHERE application_id=$1::uuid ORDER BY created_at ASC LIMIT 1`, appID,
	).Scan(&modelID); err != nil {
		return ""
	}
	revID, _, _ := h.resolveRevisionCtx(ctx, "", modelID)
	return revID
}

// revisionIDFromFormID returns the revision a form belongs to ("" for
// revision-global forms), so form events only fire that revision's rules.
func (h *handler) revisionIDFromFormID(ctx context.Context, formID string) string {
	var revID string
	_ = h.db.QueryRow(ctx,
		`SELECT COALESCE(revision_id::text,'') FROM model.form_def WHERE id=$1::uuid`, formID,
	).Scan(&revID)
	return revID
}

func (h *handler) appIDFromModelID(ctx context.Context, modelID string) (string, error) {
	var appID string
	err := h.db.QueryRow(ctx, `
		SELECT application_id::text FROM core.model WHERE id = $1::uuid
	`, modelID).Scan(&appID)
	return appID, err
}

// ── /api/apps ─────────────────────────────────────────────────────────────────

type appModelInfo struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	IsDefault      bool   `json:"is_default"`
	ActiveRevision string `json:"active_revision"`
}

type appInfo struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Mode           string `json:"mode"`
	WorkspaceName  string `json:"workspace_name"`
	ModelName      string `json:"model_name"`
	ActiveRevision string `json:"active_revision"`
	// Every model of the app the caller may access — the business Models
	// tab switches between them (X-Model-Id header); default first.
	Models []appModelInfo `json:"models"`
}

// userApps returns applications accessible to the current user, scoped by
// tenant/workspace membership plus app/model whitelists.
func (h *handler) userApps(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}

	var rows interface {
		Next() bool
		Scan(...any) error
		Close()
	}

	if a.hasRole("platform_admin") || h.isGlobalBuilder(ctx, a) {
		rows, err = h.db.Query(ctx, `
			SELECT a.id::text, a.name, COALESCE(a.mode, 'planning'), w.name,
			       COALESCE((SELECT name FROM core.model WHERE application_id = a.id LIMIT 1), ''),
			       COALESCE((SELECT active_revision_name FROM core.model WHERE application_id = a.id LIMIT 1), '')
			FROM core.application a
			JOIN core.workspace w ON w.id = COALESCE(a.workspace_id,
			    (SELECT w2.id FROM core.workspace w2 WHERE w2.customer_id = a.customer_id ORDER BY w2.created_at LIMIT 1))
			ORDER BY w.name, a.name
		`)
	} else if a.hasRole("tenant_admin") {
		_, customerIDs, scopeErr := h.adminScopeCustomerIDs(ctx, a)
		if scopeErr != nil {
			jsonErr(w, scopeErr, http.StatusInternalServerError)
			return
		}
		if len(customerIDs) == 0 {
			jsonOK(w, []appInfo{})
			return
		}
		rows, err = h.db.Query(ctx, `
			SELECT a.id::text, a.name, COALESCE(a.mode, 'planning'), w.name,
			       COALESCE((SELECT name FROM core.model WHERE application_id = a.id LIMIT 1), ''),
			       COALESCE((SELECT active_revision_name FROM core.model WHERE application_id = a.id LIMIT 1), '')
			FROM core.application a
			JOIN core.workspace w ON w.id = COALESCE(a.workspace_id,
			    (SELECT w2.id FROM core.workspace w2 WHERE w2.customer_id = a.customer_id ORDER BY w2.created_at LIMIT 1))
			WHERE a.customer_id::text = ANY($1)
			ORDER BY w.name, a.name
		`, customerIDs)
	} else {
		rows, err = h.db.Query(ctx, `
			SELECT DISTINCT a.id::text, a.name, COALESCE(a.mode, 'planning'), w.name,
			       COALESCE((
			           SELECT m.name FROM core.model m
			           WHERE m.application_id = a.id
			             AND (
			                 NOT EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id = $1::uuid)
			                 OR EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id = $1::uuid AND model_id = m.id)
			             )
			           LIMIT 1
			       ), ''),
			       COALESCE((
			           SELECT m.active_revision_name FROM core.model m
			           WHERE m.application_id = a.id
			             AND (
			                 NOT EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id = $1::uuid)
			                 OR EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id = $1::uuid AND model_id = m.id)
			             )
			           LIMIT 1
			       ), '')
			FROM core.application a
			JOIN core.workspace w ON w.id = COALESCE(a.workspace_id,
			    (SELECT w2.id FROM core.workspace w2 WHERE w2.customer_id = a.customer_id ORDER BY w2.created_at LIMIT 1))
			WHERE (EXISTS (
			      SELECT 1 FROM identity.role_assignment ra
			      JOIN core.workspace rw ON rw.id = ra.workspace_id
			      WHERE ra.user_id = $1::uuid
			        AND (rw.id = a.workspace_id OR rw.customer_id = a.customer_id)
			  ) OR EXISTS (SELECT 1 FROM identity."user" u JOIN identity.role_assignment dra ON dra.user_id = u.id AND dra.role = 'developer'
				             WHERE u.id=$1::uuid AND u.customer_id = COALESCE(a.customer_id, w.customer_id)))
			  AND (
			      NOT EXISTS (SELECT 1 FROM identity.user_app_access WHERE user_id = $1::uuid)
			      OR EXISTS (SELECT 1 FROM identity.user_app_access WHERE user_id = $1::uuid AND application_id = a.id)
			  )
			  AND EXISTS (
			      SELECT 1 FROM core.model m
			      WHERE m.application_id = a.id
			        AND (
			            NOT EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id = $1::uuid)
			            OR EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id = $1::uuid AND model_id = m.id)
			        )
			  )
			ORDER BY w.name, a.name
		`, a.UserID)
	}
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	apps := []appInfo{}
	for rows.Next() {
		var app appInfo
		if err := rows.Scan(&app.ID, &app.Name, &app.Mode, &app.WorkspaceName, &app.ModelName, &app.ActiveRevision); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		apps = append(apps, app)
	}
	// Per-app accessible models (default first) for the model switcher.
	for i := range apps {
		mRows, mErr := h.db.Query(ctx, `
			SELECT m.id::text, m.name,
			       m.id = (SELECT default_model_id FROM core.application WHERE id = $1::uuid),
			       COALESCE(m.active_revision_name, '')
			FROM core.model m
			WHERE m.application_id = $1::uuid
			  AND (
			      NOT EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id = $2::uuid)
			      OR EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id = $2::uuid AND model_id = m.id)
			  )
			ORDER BY (m.id = (SELECT default_model_id FROM core.application WHERE id = $1::uuid)) DESC, m.created_at
		`, apps[i].ID, a.UserID)
		if mErr != nil {
			continue
		}
		for mRows.Next() {
			var mi appModelInfo
			if mRows.Scan(&mi.ID, &mi.Name, &mi.IsDefault, &mi.ActiveRevision) == nil {
				apps[i].Models = append(apps[i].Models, mi)
			}
		}
		mRows.Close()
	}
	jsonOK(w, apps)
}

// pinModelForRevision switches a resolved model to the one owning revID —
// but only within the same, already-authorized application, honoring
// user_model_access. A foreign or unknown revision leaves modelID as-is.
// See resolveDemoModelID for why: multi-model apps need the request's
// revision to identify WHICH model the caller works in.
func (h *handler) pinModelForRevision(ctx context.Context, userID, appID, modelID, revID string) string {
	if revID == "" || appID == "" {
		return modelID
	}
	var revModel string
	if err := h.db.QueryRow(ctx, `
		SELECT rev.model_id::text
		FROM model.revision rev
		JOIN core.model m ON m.id = rev.model_id
		WHERE rev.id::text = $1 AND m.application_id = $2::uuid
		  AND (
		      NOT EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id=$3::uuid)
		      OR EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id=$3::uuid AND model_id=rev.model_id)
		  )
	`, revID, appID, userID).Scan(&revModel); err == nil {
		return revModel
	}
	return modelID
}

// pinModelForBodyRevision is pinModelForRevision for endpoints whose
// revision arrives in the request BODY rather than the query string (e.g.
// /api/import/upload) — without it a grid import scoped to an older
// model's revision resolved the app's newest model and rejected every
// column ("matches no metric or dimension", reported live with a second
// model in the app).
func (h *handler) pinModelForBodyRevision(ctx context.Context, r *http.Request, modelID, revID string) string {
	appID, _ := ctx.Value(appIDCtxKey).(string)
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		return modelID
	}
	return h.pinModelForRevision(ctx, act.UserID, appID, modelID, revID)
}

func (h *handler) resolveDemoModelID(ctx context.Context, r *http.Request) (string, error) {
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		return "", err
	}
	if appID, _ := ctx.Value(appIDCtxKey).(string); appID != "" {
		var modelID string
		if act.hasRole("platform_admin") || h.isGlobalBuilder(ctx, act) {
			err = h.db.QueryRow(ctx, `
				SELECT m.id::text FROM core.model m
				WHERE m.application_id = $1::uuid
				ORDER BY (m.id = (SELECT default_model_id FROM core.application WHERE id=$1::uuid)) DESC, m.created_at DESC LIMIT 1
			`, appID).Scan(&modelID)
		} else if act.hasRole("tenant_admin") {
			all, customerIDs, scopeErr := h.adminScopeCustomerIDs(ctx, act)
			if scopeErr != nil {
				return "", scopeErr
			}
			if all {
				err = h.db.QueryRow(ctx, `
					SELECT m.id::text FROM core.model m
					WHERE m.application_id = $1::uuid
					ORDER BY (m.id = (SELECT default_model_id FROM core.application WHERE id=$1::uuid)) DESC, m.created_at DESC LIMIT 1
				`, appID).Scan(&modelID)
			} else if len(customerIDs) > 0 {
				err = h.db.QueryRow(ctx, `
					SELECT m.id::text
					FROM core.model m
					JOIN core.application app ON app.id=m.application_id
					WHERE app.id=$1::uuid AND app.customer_id::text = ANY($2)
					ORDER BY (m.id = (SELECT default_model_id FROM core.application WHERE id=$1::uuid)) DESC, m.created_at DESC LIMIT 1
				`, appID, customerIDs).Scan(&modelID)
			} else {
				return "", errAccessDenied
			}
		} else {
			// developer is a tenant-wide trusted role — the same breadth
			// adminTenants already grants it when LISTING apps ("any workspace
			// of this customer"). Matching only the app's own workspace here
			// made a workspace-scoped app (customer_id NULL, the common case)
			// visible in the Developer Console's app list but unopenable for
			// any developer whose role_assignment lives in a different
			// workspace of the same customer. business_user/business_admin
			// stay workspace-scoped — that boundary between workspaces of one
			// corporate tenant is intentional for operational roles.
			err = h.db.QueryRow(ctx, `
				SELECT m.id::text
				FROM core.model m
				JOIN core.application app ON app.id=m.application_id
				LEFT JOIN core.workspace appws ON appws.id = app.workspace_id
				WHERE app.id=$1::uuid
				  AND (EXISTS (
				      SELECT 1 FROM identity.role_assignment ra
				      JOIN core.workspace rw ON rw.id = ra.workspace_id
				      WHERE ra.user_id=$2::uuid
				        AND rw.customer_id = COALESCE(app.customer_id, appws.customer_id)
				        AND (
				            ra.role::text = 'developer'
				            OR app.workspace_id IS NULL
				            OR ra.workspace_id = app.workspace_id
				        )
				  ) OR EXISTS (SELECT 1 FROM identity."user" u JOIN identity.role_assignment dra ON dra.user_id = u.id AND dra.role = 'developer'
				             WHERE u.id=$2::uuid AND u.customer_id = COALESCE(app.customer_id, appws.customer_id)))
				  AND (
				      NOT EXISTS (SELECT 1 FROM identity.user_app_access WHERE user_id=$2::uuid)
				      OR EXISTS (SELECT 1 FROM identity.user_app_access WHERE user_id=$2::uuid AND application_id=app.id)
				  )
				  AND (
				      NOT EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id=$2::uuid)
				      OR EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id=$2::uuid AND model_id=m.id)
				  )
				ORDER BY (m.id = (SELECT default_model_id FROM core.application WHERE id=$1::uuid)) DESC, m.created_at DESC LIMIT 1
			`, appID, act.UserID).Scan(&modelID)
		}
		if err != nil {
			return "", errAccessDenied
		}
		// Every branch above resolves the app's NEWEST model — with no way
		// for the caller to pick another one, creating a second model in an
		// app made the first one's data vanish from every console view (the
		// selected revision belonged to the "wrong" model and each query
		// came back foreign/empty — reported live). When the request names
		// a revision, that revision identifies the model the caller is
		// actually working in; honoring it is safe because the branches
		// above already authorized the APP, and the join pins the revision's
		// model to that same app (a foreign revision changes nothing).
		// Explicit model selection (X-Model-Id, set by the Models tab's
		// switcher): validated to the SAME already-authorized app and the
		// caller's user_model_access — a foreign or inaccessible model id
		// changes nothing. The revision pin below still wins when a request
		// names a revision (the developer console's more specific signal).
		if hdrModel := r.Header.Get("X-Model-Id"); hdrModel != "" && hdrModel != modelID {
			var ok bool
			if err := h.db.QueryRow(ctx, `
				SELECT EXISTS (
					SELECT 1 FROM core.model m
					WHERE m.id::text = $1 AND m.application_id = $2::uuid
					  AND (
					      NOT EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id=$3::uuid)
					      OR EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id=$3::uuid AND model_id=m.id)
					  )
				)
			`, hdrModel, appID, act.UserID).Scan(&ok); err == nil && ok {
				modelID = hdrModel
			}
		}
		modelID = h.pinModelForRevision(ctx, act.UserID, appID, modelID, r.URL.Query().Get("revision_id"))
		return modelID, nil
	}
	var modelID string
	if act.hasRole("platform_admin") || h.isGlobalBuilder(ctx, act) {
		err = h.db.QueryRow(ctx, `
			SELECT m.id::text
			FROM core.application a
			JOIN core.model m ON m.application_id = a.id
			ORDER BY (SELECT COUNT(*) FROM model.metric_def WHERE model_id = m.id) DESC,
			         a.created_at DESC
			LIMIT 1
		`).Scan(&modelID)
	} else if act.hasRole("tenant_admin") {
		all, customerIDs, scopeErr := h.adminScopeCustomerIDs(ctx, act)
		if scopeErr != nil {
			return "", scopeErr
		}
		if all {
			err = h.db.QueryRow(ctx, `
				SELECT m.id::text
				FROM core.application a
				JOIN core.model m ON m.application_id = a.id
				ORDER BY (SELECT COUNT(*) FROM model.metric_def WHERE model_id = m.id) DESC,
				         a.created_at DESC
				LIMIT 1
			`).Scan(&modelID)
		} else if len(customerIDs) > 0 {
			err = h.db.QueryRow(ctx, `
				SELECT m.id::text
				FROM core.application a
				JOIN core.model m ON m.application_id = a.id
				WHERE a.customer_id::text = ANY($1)
				ORDER BY (SELECT COUNT(*) FROM model.metric_def WHERE model_id = m.id) DESC,
				         a.created_at DESC
				LIMIT 1
			`, customerIDs).Scan(&modelID)
		} else {
			return "", errAccessDenied
		}
	} else {
		// Same developer/customer-wide broadening as the appID branch above.
		err = h.db.QueryRow(ctx, `
			SELECT m.id::text
			FROM core.application a
			JOIN core.model m ON m.application_id = a.id
			LEFT JOIN core.workspace aws ON aws.id = a.workspace_id
			WHERE (EXISTS (
			      SELECT 1 FROM identity.role_assignment ra
			      JOIN core.workspace rw ON rw.id = ra.workspace_id
			      WHERE ra.user_id=$1::uuid
			        AND rw.customer_id = COALESCE(a.customer_id, aws.customer_id)
			        AND (
			            ra.role::text = 'developer'
			            OR a.workspace_id IS NULL
			            OR ra.workspace_id = a.workspace_id
			        )
			  ) OR EXISTS (SELECT 1 FROM identity."user" u JOIN identity.role_assignment dra ON dra.user_id = u.id AND dra.role = 'developer'
				             WHERE u.id=$1::uuid AND u.customer_id = COALESCE(a.customer_id, aws.customer_id)))
			  AND (
			      NOT EXISTS (SELECT 1 FROM identity.user_app_access WHERE user_id=$1::uuid)
			      OR EXISTS (SELECT 1 FROM identity.user_app_access WHERE user_id=$1::uuid AND application_id=a.id)
			  )
			  AND (
			      NOT EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id=$1::uuid)
			      OR EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id=$1::uuid AND model_id=m.id)
			  )
			ORDER BY (SELECT COUNT(*) FROM model.metric_def WHERE model_id = m.id) DESC,
			         a.created_at DESC
			LIMIT 1
		`, act.UserID).Scan(&modelID)
	}
	if err != nil {
		return "", errAccessDenied
	}
	return modelID, nil
}

func (h *handler) resolveDemoAppID(ctx context.Context, r *http.Request) (string, error) {
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		return "", err
	}
	if appID, _ := ctx.Value(appIDCtxKey).(string); appID != "" {
		ok, err := h.actorCanAccessApp(ctx, act, appID)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", errAccessDenied
		}
		return appID, nil
	}
	var appID string
	if act.hasRole("platform_admin") || h.isGlobalBuilder(ctx, act) {
		err = h.db.QueryRow(ctx, `
			SELECT a.id::text
			FROM core.application a
			JOIN core.model m ON m.application_id = a.id
			ORDER BY (SELECT COUNT(*) FROM model.metric_def WHERE model_id = m.id) DESC,
			         a.created_at DESC
			LIMIT 1
		`).Scan(&appID)
	} else if act.hasRole("tenant_admin") {
		all, customerIDs, scopeErr := h.adminScopeCustomerIDs(ctx, act)
		if scopeErr != nil {
			return "", scopeErr
		}
		if all {
			err = h.db.QueryRow(ctx, `
				SELECT a.id::text
				FROM core.application a
				JOIN core.model m ON m.application_id = a.id
				ORDER BY (SELECT COUNT(*) FROM model.metric_def WHERE model_id = m.id) DESC,
				         a.created_at DESC
				LIMIT 1
			`).Scan(&appID)
		} else if len(customerIDs) > 0 {
			err = h.db.QueryRow(ctx, `
				SELECT a.id::text
				FROM core.application a
				JOIN core.model m ON m.application_id = a.id
				WHERE a.customer_id::text = ANY($1)
				ORDER BY (SELECT COUNT(*) FROM model.metric_def WHERE model_id = m.id) DESC,
				         a.created_at DESC
				LIMIT 1
			`, customerIDs).Scan(&appID)
		} else {
			return "", errAccessDenied
		}
	} else {
		err = h.db.QueryRow(ctx, `
			SELECT a.id::text
			FROM core.application a
			JOIN core.model m ON m.application_id = a.id
			JOIN core.workspace w ON (w.id=a.workspace_id OR w.customer_id=a.customer_id)
			JOIN identity.role_assignment ra ON ra.workspace_id=w.id
			WHERE ra.user_id=$1::uuid
			  AND (
			      NOT EXISTS (SELECT 1 FROM identity.user_app_access WHERE user_id=$1::uuid)
			      OR EXISTS (SELECT 1 FROM identity.user_app_access WHERE user_id=$1::uuid AND application_id=a.id)
			  )
			  AND (
			      NOT EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id=$1::uuid)
			      OR EXISTS (SELECT 1 FROM identity.user_model_access WHERE user_id=$1::uuid AND model_id=m.id)
			  )
			ORDER BY (SELECT COUNT(*) FROM model.metric_def WHERE model_id = m.id) DESC,
			         a.created_at DESC
			LIMIT 1
		`, act.UserID).Scan(&appID)
	}
	if err != nil {
		return "", errAccessDenied
	}
	return appID, nil
}

func (h *handler) forms(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	store := crudapp.NewStore(h.db.For(ctx))

	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	// Forms are revision-scoped; default to the active revision when the
	// caller doesn't pass one.
	revisionID, _, revErr := h.resolveRevisionCtx(ctx, r.URL.Query().Get("revision_id"), modelID)
	if rejectForeignRevision(w, revErr) {
		return
	}

	if r.Method == http.MethodPost {
		var body struct {
			Name   string              `json:"name"`
			Label  string              `json:"label"`
			Fields []crudapp.FormField `json:"fields"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
		form, err := store.CreateForm(ctx, modelID, revisionID, body.Name, body.Label, body.Fields)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			var appID string
			_ = h.db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID)
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryModelChange, EventType: auditlog.EventFormCreated,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: appID, ResourceType: "form", ResourceID: form.ID, RevisionID: revisionID,
				Metadata: map[string]string{"name": body.Name},
			})
		}
		jsonOK(w, form)
		return
	}

	forms, err := store.ListForms(ctx, modelID, revisionID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if forms == nil {
		jsonOK(w, []any{})
		return
	}
	jsonOK(w, forms)
}

// publicDimensions serves GET /api/dimensions — returns all dimensions with
// their members for the current demo model. Available to all authenticated users.
func (h *handler) publicDimensions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}

	revisionID, _, revErr := h.resolveRevisionCtx(ctx, r.URL.Query().Get("revision_id"), modelID)
	if rejectForeignRevision(w, revErr) {
		return
	}
	dimRows, err := h.db.Query(ctx,
		`SELECT id::text, name, agg_rule FROM model.dimension_def
		 WHERE model_id=$1::uuid
		   AND ($2 = '' OR revision_id IS NULL OR revision_id::text = $2)
		 ORDER BY created_at`,
		modelID, revisionID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer dimRows.Close()

	var dims []devDimension
	for dimRows.Next() {
		var d devDimension
		if err := dimRows.Scan(&d.ID, &d.Name, &d.AggRule); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		dims = append(dims, d)
	}
	dimRows.Close()

	for i, d := range dims {
		mRows, err := h.db.Query(ctx,
			`SELECT id::text, code, label, parent_member_id::text FROM model.dimension_member
			 WHERE dimension_id=$1::uuid ORDER BY sort_order, code`,
			d.ID)
		if err != nil {
			continue
		}
		for mRows.Next() {
			var m devMember
			if err := mRows.Scan(&m.ID, &m.Code, &m.Label, &m.ParentMemberID); err != nil {
				continue
			}
			dims[i].Members = append(dims[i].Members, m)
		}
		mRows.Close()
		if dims[i].Members == nil {
			dims[i].Members = []devMember{}
		}
	}

	// Filter hidden members — this feeds the Forms UI's dimension-member
	// picker, which (unlike grid()) had never applied
	// identity.user_access_rule at all, leaking the existence of every
	// member of every dimension to any authenticated user regardless of
	// hidden/read restrictions. Mirrors grid()'s own dimRules+cascade
	// block (handler.go ~2139-2196): a "hidden" rule, direct or cascaded
	// through the dimension hierarchy via ExpandHidden, removes the
	// member; "read" leaves it visible in the picker (only the actual
	// write is blocked, in applyFormMappings).
	dimRules := map[string]string{}
	if arRows, arErr := h.db.Query(ctx,
		`SELECT ref_id, access FROM identity.user_access_rule WHERE user_id=$1::uuid AND rule_type='dimension_member'`,
		a.UserID,
	); arErr == nil {
		for arRows.Next() {
			var refID, access string
			if arRows.Scan(&refID, &access) == nil {
				dimRules[refID] = access
			}
		}
		arRows.Close()
	}
	if len(dimRules) > 0 {
		edges := make([]writeguard.MemberEdge, 0, len(dims)*4)
		for _, d := range dims {
			for _, m := range d.Members {
				parentID := ""
				if m.ParentMemberID != nil {
					parentID = *m.ParentMemberID
				}
				edges = append(edges, writeguard.MemberEdge{ID: m.ID, ParentID: parentID, DimID: d.ID})
			}
		}
		for id := range writeguard.ExpandHidden(edges, dimRules) {
			dimRules[id] = "hidden"
		}
		for i, d := range dims {
			kept := d.Members[:0]
			for _, m := range d.Members {
				if dimRules[m.ID] != "hidden" {
					kept = append(kept, m)
				}
			}
			dims[i].Members = kept
		}
	}

	if dims == nil {
		dims = []devDimension{}
	}
	jsonOK(w, dims)
}

// formsRouter handles /api/forms/{id} and /api/forms/{id}/records
func (h *handler) formsRouter(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	path := strings.TrimPrefix(r.URL.Path, "/api/forms/")
	parts := strings.SplitN(path, "/", 2)
	formID := parts[0]
	store := crudapp.NewStore(h.db.For(ctx))

	// /api/forms/{id}/export — export this form's records as CSV or XLSX
	if len(parts) == 2 && parts[1] == "export" && r.Method == http.MethodGet {
		h.formExport(w, r, formID)
		return
	}

	// /api/forms/{id}/import — bulk-create records from an uploaded CSV or XLSX
	if len(parts) == 2 && parts[1] == "import" && r.Method == http.MethodPost {
		h.formImport(w, r, formID)
		return
	}

	// /api/forms/{id}/sync — re-apply all live mappings for every record in this form
	if len(parts) == 2 && parts[1] == "sync" && r.Method == http.MethodPost {
		userID := h.resolveUserID(r)

		// Find all live mappings for this form
		mrows, err := h.db.Query(ctx, `
			SELECT id::text FROM model.form_metric_mapping
			WHERE form_id=$1::uuid AND live_posting = TRUE`, formID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		var mappingIDs []string
		for mrows.Next() {
			var mid string
			if err := mrows.Scan(&mid); err == nil {
				mappingIDs = append(mappingIDs, mid)
			}
		}
		mrows.Close()

		totalProcessed := 0
		for _, mid := range mappingIDs {
			n, err := h.backfillFormMapping(ctx, mid, h.resolveUserID(r))
			if err != nil {
				h.log.Warn().Err(err).Str("mapping_id", mid).Msg("form sync: backfill failed")
				continue
			}
			totalProcessed += n
		}
		_ = userID
		if a, e := h.resolveActor(ctx, r); e == nil {
			appID, _ := h.appIDFromFormID(ctx, formID)
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryDataChange, EventType: auditlog.EventFormSynced,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: appID, ResourceType: "form", ResourceID: formID,
				Metadata: map[string]string{"mappings": strconv.Itoa(len(mappingIDs)), "records_processed": strconv.Itoa(totalProcessed)},
			})
		}
		jsonOK(w, map[string]any{"status": "ok", "mappings": len(mappingIDs), "records_processed": totalProcessed})
		return
	}

	// /api/forms/{id}/records
	if len(parts) == 2 && parts[1] == "records" {
		userID := h.resolveUserID(r)
		if r.Method == http.MethodPost {
			var body struct {
				Data       map[string]any `json:"data"`
				RevisionID string         `json:"revision_id"`
				Revision   string         `json:"revision"`
				Version    string         `json:"version"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
				return
			}
			rec, err := store.CreateRecord(ctx, formID, userID, body.Data)
			if err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			bgCtx := context.WithoutCancel(ctx)
			// Apply form-metric mappings (status-based posting rules).
			go func() { _ = h.applyFormMappings(bgCtx, rec.ID, formID, rec.Status, body.Data, userID) }()
			// Dispatch form_submit automation rules.
			go func() {
				appID, _ := h.appIDFromFormID(bgCtx, formID)
				if appID != "" {
					payload := map[string]string{"form_id": formID, "record_id": rec.ID, "status": rec.Status}
					workflow.NewStore(h.db.For(ctx)).DispatchEventRules(bgCtx, appID, h.revisionIDFromFormID(bgCtx, formID), "form_submit", formID, userID, payload)
				}
			}()
			if a, e := h.resolveActor(ctx, r); e == nil {
				appID, _ := h.appIDFromFormID(ctx, formID)
				auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
					Category: auditlog.CategoryDataChange, EventType: auditlog.EventFormRecordCreated,
					ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
					ApplicationID: appID, ResourceType: "form_record", ResourceID: rec.ID,
					Metadata: map[string]string{"form_id": formID, "status": rec.Status},
				})
			}
			jsonOK(w, rec)
			return
		}
		records, err := store.ListRecords(ctx, formID, 100)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		act, err := h.resolveActor(ctx, r)
		if err != nil {
			jsonErr(w, err, http.StatusUnauthorized)
			return
		}
		form, err := store.GetForm(ctx, formID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		records, err = h.filterFormRecords(ctx, act, form, records)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if records == nil {
			jsonOK(w, []any{})
			return
		}
		jsonOK(w, records)
		return
	}

	// /api/forms/{id} — PATCH or DELETE
	switch r.Method {
	case http.MethodPatch:
		var body struct {
			Name   string              `json:"name"`
			Label  string              `json:"label"`
			Fields []crudapp.FormField `json:"fields"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
		if err := store.UpdateForm(ctx, formID, body.Name, body.Label, body.Fields); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			appID, _ := h.appIDFromFormID(ctx, formID)
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryModelChange, EventType: auditlog.EventFormUpdated,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: appID, ResourceType: "form", ResourceID: formID,
				Metadata: map[string]string{"name": body.Name},
			})
		}
		jsonOK(w, map[string]string{"status": "ok"})
	case http.MethodDelete:
		appID, _ := h.appIDFromFormID(ctx, formID)
		if err := store.DeleteForm(ctx, formID); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryModelChange, EventType: auditlog.EventFormDeleted,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: appID, ResourceType: "form", ResourceID: formID,
			})
		}
		jsonOK(w, map[string]string{"status": "deleted"})
	default:
		jsonErr(w, fmt.Errorf("not found"), http.StatusNotFound)
	}
}

// recordAction handles /api/records/{recordId} (PUT, DELETE)
func (h *handler) recordAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	recordID := strings.TrimPrefix(r.URL.Path, "/api/records/")
	store := crudapp.NewStore(h.db.For(ctx))

	switch r.Method {
	case http.MethodPut:
		var body struct {
			Data   map[string]any `json:"data"`
			Status string         `json:"status"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
		if body.Status == "" {
			body.Status = "draft"
		}
		// Capture the pre-update status so event rules fire on transitions,
		// not on every save of a record already in that status.
		var formID, oldStatus string
		_ = h.db.QueryRow(ctx, `SELECT form_id::text, status FROM runtime.form_record WHERE id=$1::uuid`, recordID).Scan(&formID, &oldStatus)
		if err := store.UpdateRecord(ctx, recordID, body.Status, body.Data); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		userID := h.resolveUserID(r)
		if formID != "" {
			bgCtx := context.WithoutCancel(ctx)
			go func() { _ = h.applyFormMappings(bgCtx, recordID, formID, body.Status, body.Data, userID) }()
			statusChanged := body.Status != oldStatus
			// form_submit fires when the record transitions INTO submitted.
			if statusChanged && body.Status == "submitted" {
				go func() {
					appID, _ := h.appIDFromFormID(bgCtx, formID)
					if appID != "" {
						payload := map[string]string{"form_id": formID, "record_id": recordID, "status": body.Status}
						workflow.NewStore(h.db.For(ctx)).DispatchEventRules(bgCtx, appID, h.revisionIDFromFormID(bgCtx, formID), "form_submit", formID, userID, payload)
					}
				}()
			}
			// Dispatch form_approval when record reaches an approval terminal state.
			if statusChanged && (body.Status == "approved" || body.Status == "closed" || body.Status == "completed") {
				go func() {
					appID, _ := h.appIDFromFormID(bgCtx, formID)
					if appID != "" {
						payload := map[string]string{"form_id": formID, "record_id": recordID, "status": body.Status}
						workflow.NewStore(h.db.For(ctx)).DispatchEventRules(bgCtx, appID, h.revisionIDFromFormID(bgCtx, formID), "form_approval", formID, userID, payload)
					}
				}()
			}
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			var appID string
			if formID != "" {
				appID, _ = h.appIDFromFormID(ctx, formID)
			}
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryDataChange, EventType: auditlog.EventRecordUpdated,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: appID, ResourceType: "form_record", ResourceID: recordID,
				Metadata: map[string]string{"form_id": formID, "status": body.Status},
			})
		}
		jsonOK(w, map[string]string{"status": "ok"})

	case http.MethodDelete:
		var recFormID, appID string
		_ = h.db.QueryRow(ctx, `SELECT form_id::text FROM runtime.form_record WHERE id=$1::uuid`, recordID).Scan(&recFormID)
		if recFormID != "" {
			appID, _ = h.appIDFromFormID(ctx, recFormID)
		}
		if err := store.DeleteRecord(ctx, recordID); err != nil {
			jsonErr(w, err, http.StatusNotFound)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryDataChange, EventType: auditlog.EventRecordDeleted,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: appID, ResourceType: "form_record", ResourceID: recordID,
				Metadata: map[string]string{"form_id": recFormID},
			})
		}
		jsonOK(w, map[string]string{"status": "deleted"})

	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// resolveUserID extracts the acting user UUID from the request.
func (h *handler) resolveUserID(r *http.Request) string {
	a, err := h.resolveActor(r.Context(), r)
	if err != nil {
		return "00000000-0000-0000-0000-000000000001"
	}
	return a.UserID
}

// ── Automation / Execution ────────────────────────────────────────────────────

func (h *handler) automationRules(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	store := workflow.NewStore(h.db.For(ctx))

	appID, err := h.resolveDemoAppID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve app")
		return
	}
	revisionID := h.resolveAppRevisionID(ctx, r, appID)

	if r.Method == http.MethodPost {
		var body struct {
			Name                string `json:"name"`
			Description         string `json:"description"`
			TriggerType         string `json:"trigger_type"`
			WorkflowName        string `json:"workflow_name"`
			WorkflowDefID       string `json:"workflow_def_id"`
			SourceFormID        string `json:"source_form_id"`
			SourceGridID        string `json:"source_grid_id"`
			SourceIntegrationID string `json:"source_integration_id"`
			CronExpr            string `json:"cron_expr"`
			Timezone            string `json:"timezone"`
			MisfirePolicy       string `json:"misfire_policy"`
			MaxRetries          int    `json:"max_retries"`
			RetryBackoffSeconds int    `json:"retry_backoff_seconds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
		if body.TriggerType == "" {
			body.TriggerType = "manual"
		}
		var sched *workflow.ScheduleConfig
		if body.TriggerType == "schedule" {
			sched = &workflow.ScheduleConfig{
				CronExpr:            body.CronExpr,
				Timezone:            body.Timezone,
				MisfirePolicy:       body.MisfirePolicy,
				MaxRetries:          body.MaxRetries,
				RetryBackoffSeconds: body.RetryBackoffSeconds,
			}
		}
		rule, err := store.CreateAutomationRuleScoped(ctx, appID, revisionID, body.Name, body.Description, body.TriggerType, body.WorkflowName, body.WorkflowDefID, body.SourceFormID, body.SourceGridID, body.SourceIntegrationID, sched)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryModelChange, EventType: auditlog.EventAutomationRuleCreated,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: rule.ApplicationID, ResourceType: "automation_rule", ResourceID: rule.ID, RevisionID: rule.RevisionID,
				Metadata: map[string]string{"name": body.Name, "trigger_type": body.TriggerType},
			})
		}
		jsonOK(w, rule)
		return
	}

	rules, err := store.ListAutomationRules(ctx, appID, revisionID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if rules == nil {
		jsonOK(w, []any{})
		return
	}
	jsonOK(w, rules)
}

func (h *handler) automationTrigger(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	ruleID := strings.TrimPrefix(r.URL.Path, "/api/automation/trigger/")
	store := workflow.NewStore(h.db.For(ctx))

	// Ownership guard: this route is deliberately "any" (AutomationButtonWidget
	// puts a real business-user-facing "run this automation" button on
	// dashboards — a role gate would break that), so it can't rely on
	// dev()/ba() the way rule management now does. Without this, any
	// authenticated caller could fire ANY tenant's rule by guessing its
	// UUID. Reuses actorCanAccessApp, same as automationRuleAction.
	// Deliberately does NOT restrict by trigger_type — real dashboard data
	// has manual-trigger buttons wired to non-"manual"-type rules (e.g.
	// form_submit) as an intentional "manual override" pattern, and
	// store.TriggerRule is also called internally for real event dispatch
	// (internal/workflow/store.go's DispatchEventRules), so any
	// restriction belongs here at the HTTP layer only, and only on
	// ownership, not trigger_type.
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	// Same separation of duties as POST /api/workflow/instances: a caller who
	// is ONLY an approver does not start approval workflows, whichever door
	// they use — this one was left open (found by the 2026-09-13 scenario
	// run). Holding business_user (an admin who is also a business user),
	// developer or an admin role alongside lifts it, as it does there.
	if act.hasRole("business_admin") && !act.hasRole("business_user") && !act.hasRole("developer") && !act.hasRole("tenant_admin") && !act.hasRole("platform_admin") {
		jsonErr(w, fmt.Errorf("approvers do not start approval workflows — a business user submits the request, you decide it"), http.StatusForbidden)
		return
	}
	var ruleAppID string
	if err := h.db.QueryRow(ctx,
		`SELECT application_id::text FROM workflow.automation_rule WHERE id=$1::uuid`, ruleID,
	).Scan(&ruleAppID); err != nil {
		jsonErr(w, fmt.Errorf("automation rule not found"), http.StatusNotFound)
		return
	}
	if canAccess, caErr := h.actorCanAccessApp(ctx, act, ruleAppID); caErr != nil {
		jsonErr(w, caErr, http.StatusInternalServerError)
		return
	} else if !canAccess {
		jsonErr(w, fmt.Errorf("forbidden: automation rule is outside your access scope"), http.StatusForbidden)
		return
	}
	userID := act.UserID

	var body struct {
		Payload map[string]string `json:"payload"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Payload == nil {
		body.Payload = map[string]string{}
	}

	exec, err := store.TriggerRule(ctx, ruleID, userID, body.Payload)
	if err != nil {
		switch {
		case errors.Is(err, workflow.ErrWorkflowNotPublished):
			jsonErr(w, err, http.StatusBadRequest)
		case errors.Is(err, workflow.ErrNoRACIScope):
			jsonErr(w, err, http.StatusForbidden)
		case errors.Is(err, workflow.ErrHiddenScope):
			jsonErr(w, err, http.StatusForbidden)
		case errors.Is(err, workflow.ErrDuplicateInstance):
			jsonErr(w, err, http.StatusConflict)
		default:
			jsonErr(w, err, http.StatusInternalServerError)
		}
		return
	}
	var revisionID string
	_ = h.db.QueryRow(ctx, `SELECT COALESCE(revision_id::text,'') FROM workflow.automation_rule WHERE id=$1::uuid`, ruleID).Scan(&revisionID)
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryDataChange, EventType: auditlog.EventAutomationRuleTriggeredManually,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ApplicationID: ruleAppID, ResourceType: "automation_rule", ResourceID: ruleID, RevisionID: revisionID,
	})
	jsonOK(w, exec)
}

func (h *handler) automationExecutions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	store := workflow.NewStore(h.db.For(ctx))

	appID, err := h.resolveDemoAppID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve app")
		return
	}

	execs, err := store.ListExecutions(ctx, appID, 50)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if execs == nil {
		jsonOK(w, []any{})
		return
	}
	jsonOK(w, execs)
}

// ── /api/developer/metrics/{id} ───────────────────────────────────────────────

func (h *handler) developerMetricAction(w http.ResponseWriter, r *http.Request) {
	metricID := strings.TrimPrefix(r.URL.Path, "/api/developer/metrics/")
	if metricID == "" {
		jsonErr(w, fmt.Errorf("metric id required"), http.StatusBadRequest)
		return
	}
	ctx := r.Context()

	if !h.requireResourceAccess(w, r, "metric", metricID) {
		return
	}

	switch r.Method {
	case http.MethodPatch:
		var body struct {
			Name                   string `json:"name"`
			Formula                string `json:"formula"`
			AggRule                string `json:"agg_rule"`
			AggNumeratorMetricID   string `json:"agg_numerator_metric_id"`
			AggDenominatorMetricID string `json:"agg_denominator_metric_id"`
			Format                 string `json:"format"`
			FormatDecimals         int    `json:"format_decimals"`
			FormatCurrency         string `json:"format_currency"`
			TimeSummary            string `json:"time_summary"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
		if body.AggRule == "" {
			body.AggRule = "sum"
		}
		if body.TimeSummary == "" {
			body.TimeSummary = "sum"
		}
		if !timedim.ValidTimeSummary(body.TimeSummary) {
			jsonErr(w, fmt.Errorf("time_summary must be one of %s", strings.Join(timedim.TimeSummaries, ", ")), http.StatusBadRequest)
			return
		}
		if body.Format == "" {
			body.Format = "number"
		}
		if body.FormatCurrency == "" {
			body.FormatCurrency = "$"
		}
		var formulaPtr *string
		if body.Formula != "" {
			formulaPtr = &body.Formula
		}
		// Resolve model for dependency lookup and formula validation
		var modelID, metricRevisionID, metricAppID string
		var metricIsInput bool
		if err := h.db.QueryRow(ctx, `
			SELECT md.model_id::text, COALESCE(md.revision_id::text,''), COALESCE(m.application_id::text,''), md.is_input
			FROM model.metric_def md JOIN core.model m ON m.id = md.model_id
			WHERE md.id = $1::uuid`, metricID,
		).Scan(&modelID, &metricRevisionID, &metricAppID, &metricIsInput); err != nil {
			jsonErr(w, fmt.Errorf("metric not found"), http.StatusNotFound)
			return
		}
		// is_input comes from the stored row, not the request: this endpoint
		// cannot change it, so the request has no say in whether 'formula' is
		// a legal rule here.
		if err := metricformula.ValidateAggRule(body.AggRule, metricIsInput, body.AggNumeratorMetricID, body.AggDenominatorMetricID, metricID); err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		if err := metricformula.ValidateAggOperands(ctx, h.db.For(ctx), body.AggRule, modelID, metricRevisionID, body.AggNumeratorMetricID, body.AggDenominatorMetricID); err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		// Same shared validation as create, with MetricID set so self-reference
		// and cycles are caught here too — the scheduler would otherwise only
		// discover a cycle at calculation time.
		var formulaEdges []metricformula.Edge
		if body.Formula != "" {
			res, vErr := metricformula.Validate(ctx, h.db.For(ctx), metricformula.Request{
				ModelID: modelID, RevisionID: metricRevisionID, MetricID: metricID,
				Name: body.Name, Formula: body.Formula,
			})
			if vErr != nil {
				var invalid *metricformula.ValidationError
				if errors.As(vErr, &invalid) {
					jsonErr(w, vErr, http.StatusBadRequest)
				} else {
					jsonErr(w, vErr, http.StatusInternalServerError)
				}
				return
			}
			formulaEdges = res.Edges
		}

		// Metric row and dependency graph move together, so a failed edge
		// write can't leave the metric claiming a formula the graph doesn't
		// reflect.
		tx, txErr := h.db.Begin(ctx)
		if txErr != nil {
			jsonErr(w, txErr, http.StatusInternalServerError)
			return
		}
		defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck

		if _, err := tx.Exec(ctx, `
			UPDATE model.metric_def
			SET name=$2, formula=$3, agg_rule=$4, format=$5, format_decimals=$6, format_currency=$7,
			    agg_numerator_metric_id=NULLIF($8,'')::uuid, agg_denominator_metric_id=NULLIF($9,'')::uuid,
			    time_summary=$10
			WHERE id=$1::uuid`,
			metricID, body.Name, formulaPtr, body.AggRule, body.Format, body.FormatDecimals, body.FormatCurrency,
			body.AggNumeratorMetricID, body.AggDenominatorMetricID, body.TimeSummary); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if body.Formula != "" {
			if err := metricformula.WriteDependencies(ctx, tx, metricID, formulaEdges); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
		}
		if err := tx.Commit(ctx); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}

		// Trigger recalculation for every scenario/version that has data in this model.
		// Collect all input metric IDs so we re-derive every calc metric.
		inputRows, _ := h.db.Query(ctx,
			`SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid AND is_input=true`, modelID)
		var inputIDs []string
		for inputRows.Next() {
			var id string
			_ = inputRows.Scan(&id)
			inputIDs = append(inputIDs, id)
		}
		inputRows.Close()

		// Distinct revisions that have written data for this model
		revRows, _ := h.db.Query(ctx,
			`SELECT DISTINCT revision_id::text FROM runtime.fact_input WHERE model_id=$1::uuid AND revision_id IS NOT NULL`, modelID)
		var revIDs []string
		for revRows.Next() {
			var rid string
			_ = revRows.Scan(&rid)
			revIDs = append(revIDs, rid)
		}
		revRows.Close()

		calcStore := calculation.NewStore(h.db.For(ctx))
		sched := calculation.NewScheduler(h.log, calcStore, nil)
		type recalcResult struct {
			RevisionID string   `json:"revision_id"`
			Metric     string   `json:"metric"`
			Value      *float64 `json:"value"`
		}
		var results []recalcResult
		for _, rid := range revIDs {
			if len(inputIDs) > 0 {
				_ = sched.RecalcAffected(ctx, modelID, rid, inputIDs)
			}
			// Read the just-updated metric's new aggregate value
			var val *float64
			_ = h.db.QueryRow(ctx, `
				SELECT value::float8 FROM runtime.calc_result
				WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid
				  AND dim_members='{}'
				ORDER BY calc_at DESC LIMIT 1
			`, modelID, rid, metricID).Scan(&val)
			var mName string
			_ = h.db.QueryRow(ctx, `SELECT name FROM model.metric_def WHERE id=$1::uuid`, metricID).Scan(&mName)
			results = append(results, recalcResult{
				RevisionID: rid,
				Metric:     mName,
				Value:      val,
			})
		}

		if a, e := h.resolveActor(ctx, r); e == nil {
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryModelChange, EventType: auditlog.EventMetricUpdated,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: metricAppID, ResourceType: "metric", ResourceID: metricID, RevisionID: metricRevisionID,
				Metadata: map[string]string{"name": body.Name},
			})
		}
		jsonOK(w, map[string]any{"status": "ok", "recalc": results})

	case http.MethodDelete:
		var modelID, metricRevisionID, metricAppID string
		_ = h.db.QueryRow(ctx, `
			SELECT md.model_id::text, COALESCE(md.revision_id::text,''), COALESCE(m.application_id::text,'')
			FROM model.metric_def md JOIN core.model m ON m.id = md.model_id
			WHERE md.id=$1::uuid`, metricID).Scan(&modelID, &metricRevisionID, &metricAppID)

		// Capture the full transitive-dependent set BEFORE deleting: the
		// DELETE cascades away every model.calc_dependency row pointing at
		// this metric, so once it's gone, RecalcAffected's own graph walk
		// can no longer discover these dependents from any input at all —
		// they'd stay silently frozen at their stale pre-deletion value
		// forever instead of surfacing a #NAME? error. RecalcSpecific below
		// force-recomputes exactly this captured set regardless of the
		// (now-severed) graph.
		var dependentIDs []string
		if depRows, depErr := h.db.Query(ctx, `
			WITH RECURSIVE deps AS (
				SELECT metric_id FROM model.calc_dependency WHERE depends_on_metric_id = $1::uuid
				UNION
				SELECT cd.metric_id FROM model.calc_dependency cd JOIN deps d ON cd.depends_on_metric_id = d.metric_id
			)
			SELECT metric_id::text FROM deps`, metricID); depErr == nil {
			for depRows.Next() {
				var id string
				if depRows.Scan(&id) == nil {
					dependentIDs = append(dependentIDs, id)
				}
			}
			depRows.Close()
		}

		h.dropWidgetsReferencing(ctx, metricID)
		if _, err := h.db.Exec(ctx, `DELETE FROM model.metric_def WHERE id=$1::uuid`, metricID); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if modelID != "" {
			go h.autoMigrate(context.Background(), modelID) //nolint:contextcheck
		}
		if modelID != "" && len(dependentIDs) > 0 {
			revRows, _ := h.db.Query(ctx,
				`SELECT DISTINCT revision_id::text FROM runtime.fact_input WHERE model_id=$1::uuid AND revision_id IS NOT NULL`, modelID)
			var revIDs []string
			for revRows.Next() {
				var rid string
				_ = revRows.Scan(&rid)
				revIDs = append(revIDs, rid)
			}
			revRows.Close()
			calcStore := calculation.NewStore(h.db.For(ctx))
			sched := calculation.NewScheduler(h.log, calcStore, nil)
			for _, rid := range revIDs {
				if err := sched.RecalcSpecific(ctx, modelID, rid, dependentIDs); err != nil {
					h.log.Warn().Err(err).Str("model", modelID).Str("revision", rid).
						Msg("recalc dependents after metric deletion failed")
				}
			}
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryModelChange, EventType: auditlog.EventMetricDeleted,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: metricAppID, ResourceType: "metric", ResourceID: metricID, RevisionID: metricRevisionID,
			})
		}
		jsonOK(w, map[string]string{"status": "deleted"})

	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// dimensionScope resolves the model_id/revision_id/application_id a
// dimension (or one of its members/properties) belongs to, for audit
// logging call sites in developerDimensionAction below.
func (h *handler) dimensionScope(ctx context.Context, dimID string) (modelID, revisionID, appID string) {
	_ = h.db.QueryRow(ctx, `
		SELECT dd.model_id::text, COALESCE(dd.revision_id::text,''), COALESCE(m.application_id::text,'')
		FROM model.dimension_def dd JOIN core.model m ON m.id = dd.model_id
		WHERE dd.id = $1::uuid`, dimID).Scan(&modelID, &revisionID, &appID)
	return
}

// auditDimensionUpdated logs a dimension.updated event with a metadata
// discriminator identifying which sub-action (dimension itself, a member,
// or a property) actually changed — member/property edits deliberately
// fold into this one event type rather than getting their own constants.
func (h *handler) auditDimensionUpdated(ctx context.Context, r *http.Request, dimID, subAction string, extra map[string]string) {
	a, e := h.resolveActor(ctx, r)
	if e != nil {
		return
	}
	_, revisionID, appID := h.dimensionScope(ctx, dimID)
	meta := map[string]string{"sub_action": subAction}
	for k, v := range extra {
		meta[k] = v
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryModelChange, EventType: auditlog.EventDimensionUpdated,
		ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
		ApplicationID: appID, ResourceType: "dimension", ResourceID: dimID, RevisionID: revisionID,
		Metadata: meta,
	})
}

// ── /api/developer/dimensions/{id}[/members|/properties[/{subId}]] ───────────

func (h *handler) developerDimensionAction(w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, "/api/developer/dimensions/")
	parts := strings.SplitN(tail, "/", 3)
	dimID := parts[0]
	ctx := r.Context()

	if !h.requireResourceAccess(w, r, "dimension", dimID) {
		return
	}

	// PATCH/DELETE on the dimension itself
	if len(parts) == 1 {
		switch r.Method {
		case http.MethodPatch:
			var body struct {
				Name              string  `json:"name"`
				AggRule           string  `json:"agg_rule"`
				ParentDimensionID *string `json:"parent_dimension_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
				return
			}
			if body.AggRule == "" {
				body.AggRule = "sum"
			}
			if body.ParentDimensionID != nil {
				if *body.ParentDimensionID == dimID {
					jsonErr(w, fmt.Errorf("a dimension cannot be its own parent"), http.StatusBadRequest)
					return
				}
				if h.dimensionHasAncestor(ctx, *body.ParentDimensionID, dimID) {
					jsonErr(w, fmt.Errorf("this would create a dimension hierarchy cycle"), http.StatusBadRequest)
					return
				}
				if cfg, cErr := timedim.LoadConfig(ctx, h.db.For(ctx), dimID); cErr == nil && cfg.Type == timedim.TypeTime {
					jsonErr(w, fmt.Errorf("a time dimension cannot have a parent dimension"), http.StatusBadRequest)
					return
				}
			}
			// dimension_type / time_granularity / fiscal_year_start_month are
			// deliberately not updatable: they are immutable after creation
			// (a DB trigger enforces it too).
			if _, err := h.db.Exec(ctx, `UPDATE model.dimension_def SET name=$2, agg_rule=$3, parent_dimension_id=$4::uuid WHERE id=$1::uuid`,
				dimID, body.Name, body.AggRule, body.ParentDimensionID); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			h.auditDimensionUpdated(ctx, r, dimID, "dimension", map[string]string{"name": body.Name})
			jsonOK(w, map[string]string{"status": "ok"})
		case http.MethodDelete:
			_, dimRevisionID, dimAppID := h.dimensionScope(ctx, dimID)
			if _, err := h.db.Exec(ctx, `DELETE FROM model.dimension_def WHERE id=$1::uuid`, dimID); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			if a, e := h.resolveActor(ctx, r); e == nil {
				auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
					Category: auditlog.CategoryModelChange, EventType: auditlog.EventDimensionDeleted,
					ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
					ApplicationID: dimAppID, ResourceType: "dimension", ResourceID: dimID, RevisionID: dimRevisionID,
				})
			}
			jsonOK(w, map[string]string{"status": "deleted"})
		default:
			jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		}
		return
	}

	subResource := parts[1]
	subID := ""
	if len(parts) == 3 {
		subID = parts[2]
	}

	switch subResource {
	case "members":
		switch {
		case r.Method == http.MethodPost && subID == "generate":
			// Bulk period generator (spec §4.2): start, end → one member per
			// period of the dimension's granularity, through the same
			// validation + reindex path as a single member.
			h.generateTimeMembers(w, r, dimID)

		case r.Method == http.MethodPost && subID == "":
			var body struct {
				Code           string  `json:"code"`
				Label          string  `json:"label"`
				ParentMemberID *string `json:"parent_member_id"`
				PeriodStart    string  `json:"period_start"`
				PeriodEnd      string  `json:"period_end"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
				return
			}
			if err := h.validateMemberParent(ctx, dimID, body.ParentMemberID); err != nil {
				jsonErr(w, err, http.StatusBadRequest)
				return
			}
			period, isTime, dated, err := h.memberPeriod(ctx, dimID, body.PeriodStart, body.PeriodEnd)
			if err != nil {
				jsonErr(w, err, http.StatusBadRequest)
				return
			}
			if cid, _ := h.customerOfDimension(ctx, dimID); cid != "" && h.plans != nil {
				if err := h.plans.CheckMembers(ctx, h.db.For(ctx), cid, dimID, 1); err != nil {
					h.jsonLimitErr(w, err)
					return
				}
			}
			var newID string
			if isTime {
				// Insert + full-dimension validation + reindex in one
				// transaction: the deferred unique constraints let the
				// renumbering happen, and an invalid period (overlap, gap,
				// bad boundary, a leaf under a leaf) rolls the insert back.
				newID, err = h.writeTimeMember(ctx, dimID, "", body.Code, body.Label, period, dated, body.ParentMemberID)
			} else {
				err = h.db.QueryRow(ctx, `
					INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id)
					VALUES ($1::uuid, $2, $3, $4::uuid) RETURNING id::text
				`, dimID, body.Code, body.Label, body.ParentMemberID).Scan(&newID)
			}
			if err != nil {
				var te *timedim.Error
				if errors.As(err, &te) {
					jsonErr(w, err, http.StatusBadRequest)
					return
				}
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			if isTime {
				// Every metric on this time dimension and their dependents
				// move with the period set (spec §8.3).
				var modelID string
				_ = h.db.QueryRow(ctx, `SELECT model_id::text FROM model.dimension_def WHERE id=$1::uuid`, dimID).Scan(&modelID)
				if modelID != "" {
					go h.recalcAllInputsAcrossRevisions(context.WithoutCancel(ctx), modelID) //nolint:contextcheck
				}
			}
			if body.ParentMemberID != nil {
				var childCount int
				_ = h.db.QueryRow(ctx,
					`SELECT COUNT(*) FROM model.dimension_member WHERE dimension_id=$1::uuid AND parent_member_id=$2::uuid`,
					dimID, *body.ParentMemberID).Scan(&childCount)
				if childCount == 1 {
					var modelID string
					_ = h.db.QueryRow(ctx, `SELECT model_id::text FROM model.dimension_def WHERE id=$1::uuid`, dimID).Scan(&modelID)
					if affected, splitErr := h.splitParentFactData(ctx, dimID, *body.ParentMemberID, body.Code, modelID); splitErr == nil {
						go h.recalcAfterDimChange(context.WithoutCancel(ctx), modelID, affected)
					}
				}
			}
			h.auditDimensionUpdated(ctx, r, dimID, "member_added", map[string]string{"member_id": newID, "code": body.Code})
			jsonOK(w, map[string]string{"id": newID})

		case r.Method == http.MethodPatch && subID != "":
			var body struct {
				Code           string  `json:"code"`
				Label          string  `json:"label"`
				ParentMemberID *string `json:"parent_member_id"`
				PeriodStart    string  `json:"period_start"`
				PeriodEnd      string  `json:"period_end"`
				// Optional per-member property values. Nil = leave properties
				// untouched (a rename/reparent must not blank them); a map
				// merges over existing keys, so setting one property keeps the
				// rest. Feeds property-derived dimensions and metric formulas.
				Properties map[string]string `json:"properties"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
				return
			}
			if err := h.validateMemberParent(ctx, dimID, body.ParentMemberID); err != nil {
				jsonErr(w, err, http.StatusBadRequest)
				return
			}
			period, isTime, dated, err := h.memberPeriod(ctx, dimID, body.PeriodStart, body.PeriodEnd)
			if err != nil {
				jsonErr(w, err, http.StatusBadRequest)
				return
			}
			if body.ParentMemberID != nil {
				if *body.ParentMemberID == subID {
					jsonErr(w, fmt.Errorf("a member cannot be its own parent"), http.StatusBadRequest)
					return
				}
				if h.memberHasAncestor(ctx, *body.ParentMemberID, subID) {
					jsonErr(w, fmt.Errorf("this would create a member hierarchy cycle"), http.StatusBadRequest)
					return
				}
			}
			var prevParentID *string
			var oldCode string
			_ = h.db.QueryRow(ctx,
				`SELECT parent_member_id::text, code FROM model.dimension_member WHERE id=$1::uuid`, subID,
			).Scan(&prevParentID, &oldCode)
			if isTime {
				if _, err := h.writeTimeMember(ctx, dimID, subID, body.Code, body.Label, period, dated, body.ParentMemberID); err != nil {
					var te *timedim.Error
					if errors.As(err, &te) {
						jsonErr(w, err, http.StatusBadRequest)
						return
					}
					jsonErr(w, err, http.StatusInternalServerError)
					return
				}
			} else if _, err := h.db.Exec(ctx, `
				UPDATE model.dimension_member SET code=$2, label=$3, parent_member_id=$4::uuid WHERE id=$1::uuid
			`, subID, body.Code, body.Label, body.ParentMemberID); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			if body.Properties != nil {
				pj, _ := json.Marshal(body.Properties)
				if _, err := h.db.Exec(ctx,
					`UPDATE model.dimension_member SET properties = properties || $2::jsonb WHERE id=$1::uuid`,
					subID, string(pj)); err != nil {
					jsonErr(w, err, http.StatusInternalServerError)
					return
				}
			}
			// runtime.fact_input/calc_result store dim_members as JSONB
			// keyed by {dimension_id: member_CODE}, not member ID —
			// renaming a code here would otherwise silently and
			// permanently orphan every existing row that referenced the
			// old code (not deleted, just unreachable via any
			// current-code-based query). Re-key both tables under the new
			// code. Best-effort: a failure here doesn't undo the rename
			// itself, matching this handler's existing style for
			// non-critical side effects (e.g. the recalc trigger below).
			if body.Code != "" && body.Code != oldCode {
				if _, err := h.db.Exec(ctx, `
					UPDATE runtime.fact_input
					SET dim_members = jsonb_set(dim_members, ARRAY[$1::text], to_jsonb($2::text))
					WHERE dim_members->>$1 = $3
				`, dimID, body.Code, oldCode); err != nil {
					h.log.Error().Err(err).Str("member_id", subID).Msg("re-key fact_input after member rename")
				}
				if _, err := h.db.Exec(ctx, `
					UPDATE runtime.calc_result
					SET dim_members = jsonb_set(dim_members, ARRAY[$1::text], to_jsonb($2::text))
					WHERE dim_members->>$1 = $3
				`, dimID, body.Code, oldCode); err != nil {
					h.log.Error().Err(err).Str("member_id", subID).Msg("re-key calc_result after member rename")
				}
				// widget_props holds member CODES in three places (SYNC-02 —
				// facts/calc re-keyed above but widgets kept the old code, so
				// a chart's saved context or a pinned KPI silently fell back
				// to defaults after a rename). Paths are keyed by dimension
				// ID, so no cross-model false positives are possible.
				for _, wq := range []struct{ q, tag string }{
					{`UPDATE model.dashboard_widget
					  SET widget_props = jsonb_set(widget_props, ARRAY['default_view','filter_sel',$1::text], to_jsonb($2::text))
					  WHERE widget_props #>> ARRAY['default_view','filter_sel',$1::text] = $3`, "default_view.filter_sel"},
					{`UPDATE model.dashboard_widget
					  SET widget_props = jsonb_set(widget_props, ARRAY['chart','context_defaults',$1::text], to_jsonb($2::text))
					  WHERE widget_props #>> ARRAY['chart','context_defaults',$1::text] = $3`, "chart.context_defaults"},
					{`UPDATE model.dashboard_widget
					  SET widget_props = jsonb_set(widget_props, ARRAY['kpi_scope','member_code'], to_jsonb($2::text))
					  WHERE widget_props->'kpi_scope'->>'dimension_id' = $1 AND widget_props->'kpi_scope'->>'member_code' = $3`, "kpi_scope"},
				} {
					if _, err := h.db.Exec(ctx, wq.q, dimID, body.Code, oldCode); err != nil {
						h.log.Error().Err(err).Str("member_id", subID).Str("path", wq.tag).Msg("re-key widget_props after member rename")
					}
				}
			}
			if body.ParentMemberID != nil && prevParentID == nil {
				var childCount int
				_ = h.db.QueryRow(ctx,
					`SELECT COUNT(*) FROM model.dimension_member WHERE dimension_id=$1::uuid AND parent_member_id=$2::uuid`,
					dimID, *body.ParentMemberID).Scan(&childCount)
				if childCount == 1 {
					var modelID string
					_ = h.db.QueryRow(ctx, `SELECT model_id::text FROM model.dimension_def WHERE id=$1::uuid`, dimID).Scan(&modelID)
					if affected, splitErr := h.splitParentFactData(ctx, dimID, *body.ParentMemberID, body.Code, modelID); splitErr == nil {
						go h.recalcAfterDimChange(context.WithoutCancel(ctx), modelID, affected)
					}
				}
			}
			if isTime {
				// A re-dated period shifts every position after it.
				var modelID string
				_ = h.db.QueryRow(ctx, `SELECT model_id::text FROM model.dimension_def WHERE id=$1::uuid`, dimID).Scan(&modelID)
				if modelID != "" {
					go h.recalcAllInputsAcrossRevisions(context.WithoutCancel(ctx), modelID) //nolint:contextcheck
				}
			}
			h.auditDimensionUpdated(ctx, r, dimID, "member_updated", map[string]string{"member_id": subID, "code": body.Code})
			jsonOK(w, map[string]string{"status": "ok"})

		case r.Method == http.MethodDelete && subID != "":
			var memberModelID string
			_ = h.db.QueryRow(ctx, `SELECT model_id::text FROM model.dimension_def WHERE id=$1::uuid`, dimID).Scan(&memberModelID)
			if err := h.deleteMemberReindexed(ctx, dimID, subID); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			// Every aggregate calc metric declared over this dimension
			// stays frozen at its last computed value until something
			// forces a recalc — unlike the rename/create/reparent cases
			// just above, deleting a member never triggered one.
			if memberModelID != "" {
				go h.recalcAllInputsAcrossRevisions(context.WithoutCancel(ctx), memberModelID) //nolint:contextcheck
			}
			h.auditDimensionUpdated(ctx, r, dimID, "member_deleted", map[string]string{"member_id": subID})
			jsonOK(w, map[string]string{"status": "deleted"})

		default:
			jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		}

	case "properties":
		propID := subID
		switch {
		case r.Method == http.MethodGet && propID == "":
			rows, err := h.db.Query(ctx,
				`SELECT id::text, dimension_id::text, name, data_type FROM model.dimension_property WHERE dimension_id=$1 ORDER BY name`, dimID)
			if err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			defer rows.Close()
			type prop struct {
				ID          string `json:"id"`
				DimensionID string `json:"dimension_id"`
				Name        string `json:"name"`
				DataType    string `json:"data_type"`
			}
			var props []prop
			for rows.Next() {
				var p prop
				if err := rows.Scan(&p.ID, &p.DimensionID, &p.Name, &p.DataType); err != nil {
					jsonErr(w, err, http.StatusInternalServerError)
					return
				}
				props = append(props, p)
			}
			if props == nil {
				props = []prop{}
			}
			jsonOK(w, props)

		case r.Method == http.MethodPost && propID == "":
			var body struct {
				Name     string `json:"name"`
				DataType string `json:"data_type"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
				return
			}
			if body.DataType == "" {
				body.DataType = "text"
			}
			var newID string
			err := h.db.QueryRow(ctx, `
				INSERT INTO model.dimension_property (dimension_id, name, data_type)
				VALUES ($1, $2, $3) RETURNING id::text
			`, dimID, body.Name, body.DataType).Scan(&newID)
			if err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			h.auditDimensionUpdated(ctx, r, dimID, "property_added", map[string]string{"property_id": newID, "name": body.Name})
			jsonOK(w, map[string]string{"id": newID})

		case r.Method == http.MethodPatch && propID != "":
			var body struct {
				Name     string `json:"name"`
				DataType string `json:"data_type"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
				return
			}
			if _, err := h.db.Exec(ctx, `UPDATE model.dimension_property SET name=$2, data_type=$3 WHERE id=$1::uuid`,
				propID, body.Name, body.DataType); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			h.auditDimensionUpdated(ctx, r, dimID, "property_updated", map[string]string{"property_id": propID, "name": body.Name})
			jsonOK(w, map[string]string{"status": "ok"})

		case r.Method == http.MethodDelete && propID != "":
			if _, err := h.db.Exec(ctx, `DELETE FROM model.dimension_property WHERE id=$1::uuid`, propID); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			h.auditDimensionUpdated(ctx, r, dimID, "property_deleted", map[string]string{"property_id": propID})
			jsonOK(w, map[string]string{"status": "deleted"})

		default:
			jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		}

	default:
		jsonErr(w, fmt.Errorf("not found"), http.StatusNotFound)
	}
}

// ── /api/admin/tenants/{id} ───────────────────────────────────────────────────

// removeUserAccount is the one way a person leaves the platform: their
// tenant membership (dedicated databases), their identity row — what they
// entered, posted, started or imported stays with the tenant, authored by
// "a former user" (migration 094) — and, last, their identity-provider
// account. Deleting only the row would leave an account that can still
// authenticate against Keycloak: it resolves to no user so requests 401,
// but it keeps the address occupied, so the same person could never sign
// up again and re-creating them later would silently adopt the old
// account. Keycloak is best effort, and deliberately after the row is
// gone: a Keycloak that is briefly unreachable must not block removing
// someone's access. Synthetic dev subjects have no account to remove.
func (h *handler) removeUserAccount(ctx context.Context, userID, keycloakSub string) error {
	h.forgetUser(ctx, keycloakSub)
	if _, err := h.db.Exec(ctx, `DELETE FROM identity.user WHERE id=$1::uuid`, userID); err != nil {
		return err
	}
	if h.kc != nil && keycloakSub != "" && !strings.HasPrefix(keycloakSub, "admin-created-") {
		if err := h.kc.DeleteUser(ctx, keycloakSub); err != nil {
			h.log.Error().Err(err).Str("user_id", userID).
				Msg("application user deleted but the identity provider account remains; remove it by hand")
		}
	}
	return nil
}

func (h *handler) adminTenantAction(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/tenants/")
	ctx := r.Context()
	// Same split as above: data in the tenant's database, audit where the
	// actor is.
	tctx := h.tenantCtx(ctx, id)
	act, ok := h.requireRole(w, r, "platform_admin", "tenant_admin")
	if !ok {
		return
	}
	isPlatformAdmin := false
	for _, role := range act.Roles {
		if role == "platform_admin" {
			isPlatformAdmin = true
			break
		}
	}
	// tenant_admin can only touch their own tenant
	if !isPlatformAdmin {
		var ownerID string
		_ = h.db.QueryRow(ctx,
			`SELECT COALESCE(customer_id::text,'') FROM identity.user WHERE id=$1::uuid`, act.UserID,
		).Scan(&ownerID)
		if ownerID != id {
			jsonErr(w, fmt.Errorf("forbidden: not your tenant"), http.StatusForbidden)
			return
		}
	}
	switch r.Method {
	case http.MethodPatch:
		var body struct {
			Name string `json:"name"`
			// The plan half (plan.go): a plan key. Platform administrators
			// only.
			Plan *string `json:"plan"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
		meta := map[string]string{}
		if body.Name != "" {
			if _, err := h.db.Exec(tctx, `UPDATE core.customer SET name=$2 WHERE id=$1::uuid`, id, body.Name); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			// Keep the catalog's copy of the name in step, since that is what
			// tenant listings read for a dedicated tenant.
			if router := h.db.Router(); router != nil {
				if t, gErr := router.Catalog().Get(ctx, id); gErr == nil {
					_ = router.Catalog().Rename(ctx, id, body.Name, t.Plan)
				}
			}
			meta["name"] = body.Name
		}
		if body.Plan != nil {
			// The plan is the platform's side of the relationship: a tenant
			// admin cannot lift their own tenant's limits.
			if !isPlatformAdmin {
				jsonErr(w, fmt.Errorf("forbidden: only platform_admin can change a tenant's plan"), http.StatusForbidden)
				return
			}
			changed, err := h.applyTenantPlan(ctx, tctx, id, body.Plan)
			if err != nil {
				jsonErr(w, err, http.StatusBadRequest)
				return
			}
			for k, v := range changed {
				meta[k] = v
			}
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: auditlog.EventTenantUpdated,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ResourceType: "tenant", ResourceID: id,
			Metadata: meta,
		})
		jsonOK(w, map[string]string{"status": "ok"})
	case http.MethodDelete:
		if !isPlatformAdmin {
			jsonErr(w, fmt.Errorf("forbidden: only platform_admin can delete tenants"), http.StatusForbidden)
			return
		}
		// Cascade: delete applications (models/scenarios cascade from there)
		if _, err := h.db.Exec(tctx, `DELETE FROM core.application WHERE customer_id=$1::uuid`, id); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		// The tenant's people go with it — the ones it owns (signed up
		// into it or created by its administrators), each removed exactly
		// as deleting them one by one would, Keycloak account included.
		// The customer's ON DELETE SET NULL alone would leave orphan
		// logins that resolve to no tenant and keep their addresses. A
		// platform administrator is never a tenant's to delete, and the
		// caller never deletes themselves this way.
		rows, err := h.db.Query(tctx, `
			SELECT u.id::text, u.keycloak_sub FROM identity.user u
			WHERE u.customer_id = $1::uuid AND u.id <> $2::uuid
			  AND NOT EXISTS (SELECT 1 FROM identity.role_assignment ra WHERE ra.user_id = u.id AND ra.role = 'platform_admin')`,
			id, act.UserID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		type member struct{ id, sub string }
		var members []member
		for rows.Next() {
			var m member
			if err := rows.Scan(&m.id, &m.sub); err == nil {
				members = append(members, m)
			}
		}
		rows.Close()
		for _, m := range members {
			if err := h.removeUserAccount(tctx, m.id, m.sub); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryAdmin, EventType: auditlog.EventUserDeleted,
				ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
				ResourceType: "identity_user", ResourceID: m.id,
				Metadata: map[string]string{"tenant_id": id, "reason": "tenant deleted"},
			})
		}
		if _, err := h.db.Exec(tctx, `DELETE FROM core.customer WHERE id=$1::uuid`, id); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: auditlog.EventTenantDeleted,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ResourceType: "tenant", ResourceID: id,
			Metadata: map[string]string{"users_removed": strconv.Itoa(len(members))},
		})
		if router := h.db.Router(); router != nil {
			// For a dedicated tenant the deletion IS dropping the database;
			// the statements above ran inside it and are moot, while keeping
			// the shared-database path working unchanged.
			if err := router.Deprovision(ctx, id); err != nil {
				h.log.Error().Err(err).Str("tenant", id).Msg("tenant database was not dropped")
			}
		}
		jsonOK(w, map[string]string{"status": "deleted"})
	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// ── /api/admin/applications[/{id}] ───────────────────────────────────────────

func (h *handler) adminApplications(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method == http.MethodPost {
		var body struct {
			CustomerID string `json:"customer_id"`
			Name       string `json:"name"`
			Mode       string `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
		if body.Mode == "" {
			body.Mode = "planning"
		}
		// The tenant comes from the body, so the body is where the scope
		// check belongs: a tenant admin creates applications in their own
		// tenant only (parity audit, 2026-09-20).
		if act, err := h.resolveActor(ctx, r); err != nil {
			jsonErr(w, err, http.StatusUnauthorized)
			return
		} else if all, mine, err := h.adminScopeCustomerIDs(ctx, act); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		} else if !all && !slices.Contains(mine, body.CustomerID) {
			jsonErr(w, fmt.Errorf("forbidden: not your tenant"), http.StatusForbidden)
			return
		}
		var id, createdAt string
		// A platform admin creating an application for a tenant is not routed
		// to it by the middleware (they have no membership), so the target
		// comes from the request itself. ctx keeps pointing at the database
		// that holds the actor, which is where the audit row belongs.
		tctx := h.tenantCtx(ctx, body.CustomerID)
		if h.plans != nil {
			if err := h.plans.CheckApplications(tctx, h.db.For(tctx), body.CustomerID, 1); err != nil {
				h.jsonLimitErr(w, err)
				return
			}
		}
		if err := h.db.QueryRow(tctx,
			`INSERT INTO core.application (customer_id, name, mode) VALUES ($1::uuid, $2, $3::core.application_mode) RETURNING id::text, created_at::text`,
			body.CustomerID, body.Name, body.Mode).Scan(&id, &createdAt); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryAdmin, EventType: auditlog.EventApplicationCreated,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: id, ResourceType: "application", ResourceID: id,
				Metadata: map[string]string{"name": body.Name, "mode": body.Mode},
			})
		}
		h.noteApplication(tctx, id)
		jsonOK(w, map[string]string{"id": id, "created_at": createdAt})
		return
	}
	// GET: every application in the caller's scope — all of them for a
	// platform admin, their own tenants' for a tenant admin.
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	all, mine, err := h.adminScopeCustomerIDs(ctx, act)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !all && len(mine) == 0 {
		jsonOK(w, []struct{}{})
		return
	}
	rows, err := h.db.Query(ctx, `
		SELECT id::text, COALESCE(customer_id::text,''), name, mode::text, status::text, created_at::text
		FROM core.application
		WHERE $1 OR customer_id::text = ANY($2)
		ORDER BY created_at DESC`, all, mine)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type app struct {
		ID         string `json:"id"`
		CustomerID string `json:"customer_id"`
		Name       string `json:"name"`
		Mode       string `json:"mode"`
		Status     string `json:"status"`
		CreatedAt  string `json:"created_at"`
	}
	var list []app
	for rows.Next() {
		var item app
		if err := rows.Scan(&item.ID, &item.CustomerID, &item.Name, &item.Mode, &item.Status, &item.CreatedAt); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		list = append(list, item)
	}
	if list == nil {
		list = []app{}
	}
	jsonOK(w, list)
}

func (h *handler) adminApplicationAction(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/applications/")
	ctx := r.Context()
	// An admin acting on an application of a tenant they are not a member of
	// is not routed by the middleware; the directory says which database
	// holds it. ctx itself stays where the actor is, for the audit row.
	tctx := h.tenantCtxForApplication(ctx, id)
	// A tenant admin renames or deletes applications of their own tenant
	// only; the platform admin any (parity audit, 2026-09-20).
	if !h.requireResourceAccess(w, r, "application", id) {
		return
	}
	switch r.Method {
	case http.MethodPatch:
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
		if _, err := h.db.Exec(tctx, `UPDATE core.application SET name=$2 WHERE id=$1::uuid`, id, body.Name); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryAdmin, EventType: auditlog.EventApplicationUpdated,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: id, ResourceType: "application", ResourceID: id,
				Metadata: map[string]string{"name": body.Name},
			})
		}
		jsonOK(w, map[string]string{"status": "ok"})
	case http.MethodDelete:
		h.forgetApplication(ctx, id)
		if _, err := h.db.Exec(tctx, `DELETE FROM core.application WHERE id=$1::uuid`, id); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryAdmin, EventType: auditlog.EventApplicationDeleted,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				// ApplicationID intentionally left unset: the FK (migration
				// 060) would reject a reference to the application this row
				// is announcing the deletion of. resource_id already carries
				// it (plain TEXT, no FK).
				ResourceType: "application", ResourceID: id,
			})
		}
		jsonOK(w, map[string]string{"status": "deleted"})
	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// ── /api/admin/models[/{id}] ──────────────────────────────────────────────────

func (h *handler) adminModels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// Resolved after the body is read, below: the application decides the
	// database.
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ApplicationID string `json:"application_id"`
		Name          string `json:"name"`
		StorageType   string `json:"storage_type"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
		return
	}
	if body.StorageType == "" {
		body.StorageType = "oltp"
	}
	tctx := h.tenantCtxForApplication(ctx, body.ApplicationID)
	// The application is named in the body, so it is checked here: a tenant
	// admin adds models to their own tenant's applications only.
	if act, err := h.resolveActor(ctx, r); err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	} else if ok, err := h.actorCanAccessApp(tctx, act, body.ApplicationID); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	} else if !ok {
		jsonErr(w, fmt.Errorf("forbidden: application is outside your access scope"), http.StatusForbidden)
		return
	}
	if cid := h.customerOfApplication(tctx, body.ApplicationID); cid != "" && h.plans != nil {
		if err := h.plans.CheckModels(tctx, h.db.For(tctx), cid, 1); err != nil {
			h.jsonLimitErr(w, err)
			return
		}
	}
	var id, createdAt string
	if err := h.db.QueryRow(tctx,
		`INSERT INTO core.model (application_id, name, storage_type) VALUES ($1::uuid, $2, $3::core.storage_type) RETURNING id::text, created_at::text`, //nolint:lll
		body.ApplicationID, body.Name, body.StorageType).Scan(&id, &createdAt); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if a, e := h.resolveActor(ctx, r); e == nil {
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: auditlog.EventModelCreated,
			ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
			ApplicationID: body.ApplicationID, ResourceType: "model", ResourceID: id,
			Metadata: map[string]string{"name": body.Name, "storage_type": body.StorageType},
		})
	}
	jsonOK(w, map[string]string{"id": id, "created_at": createdAt})
}

func (h *handler) adminModelAction(w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, "/api/admin/models/")
	parts := strings.SplitN(tail, "/", 2)
	id := parts[0]
	subPath := ""
	if len(parts) == 2 {
		subPath = parts[1]
	}
	ctx := r.Context()

	// The admin gate admits tenant_admin as well as platform_admin, so
	// without this a tenant admin could delete another customer's model or
	// repoint its active revision.
	if !h.requireResourceAccess(w, r, "model", id) {
		return
	}

	if subPath == "active-revision" && r.Method == http.MethodPut {
		var body struct {
			Revision string `json:"revision_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
		var newActiveRevisionID string
		if err := h.db.QueryRow(ctx, `
			UPDATE core.model
			SET active_revision_name    = $2,
			    active_revision_id      = (SELECT id FROM model.revision WHERE model_id=$1::uuid AND name=$2 LIMIT 1)
			WHERE id=$1::uuid
			RETURNING COALESCE(active_revision_id::text, '')`,
			id, body.Revision).Scan(&newActiveRevisionID); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		// Same remap the activateRevision path runs — this admin by-name
		// route is the third door to activation and must not be the one that
		// still strands member/metric access rules on the old revision.
		if newActiveRevisionID != "" {
			if err := h.remapAccessRulesToRevision(ctx, id, newActiveRevisionID); err != nil {
				jsonErr(w, fmt.Errorf("remap access rules: %w", err), http.StatusInternalServerError)
				return
			}
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryAdmin, EventType: auditlog.EventModelActiveRevisionSet,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ResourceType: "model", ResourceID: id, RevisionID: newActiveRevisionID,
				Metadata: map[string]string{"revision_name": body.Revision},
			})
		}
		jsonOK(w, map[string]string{"status": "ok"})
		return
	}

	if r.Method != http.MethodDelete {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	var deletedAppID string
	_ = h.db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, id).Scan(&deletedAppID)
	if _, err := h.db.Exec(ctx, `DELETE FROM core.model WHERE id=$1::uuid`, id); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if a, e := h.resolveActor(ctx, r); e == nil {
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: auditlog.EventModelDeleted,
			ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
			ApplicationID: deletedAppID, ResourceType: "model", ResourceID: id,
		})
	}
	jsonOK(w, map[string]string{"status": "deleted"})
}

// ── /api/admin/revisions[/{id}] ────────────────────────────────────────────────

func (h *handler) adminRevisions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		ModelID     string `json:"model_id"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
		return
	}
	if !h.requireResourceAccess(w, r, "model", body.ModelID) {
		return
	}

	// One implementation of "copy a revision", shared with the developer
	// console's POST /api/developer/revisions. This handler used to carry its
	// own: it copied metric_def and dimension_def only — no members, grids,
	// dashboards, widgets, forms, facts or calc_dependency — discarded every
	// copy error (`_, _ = Exec`), ran outside a transaction, and dropped the
	// metrics' format columns. The result looked like a revision and blanked
	// the model when activated.
	var activeRevID *string
	_ = h.db.QueryRow(ctx,
		`SELECT active_revision_id::text FROM core.model WHERE id=$1::uuid`, body.ModelID,
	).Scan(&activeRevID)

	tx, err := h.db.Begin(ctx)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck

	id, err := h.duplicateRevision(ctx, tx, body.ModelID, body.Name, "", activeRevID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if body.Description != "" {
		if _, err := tx.Exec(ctx, `UPDATE model.revision SET description=$2 WHERE id=$1::uuid`, id, body.Description); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}

	if a, e := h.resolveActor(ctx, r); e == nil {
		var appID string
		_ = h.db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, body.ModelID).Scan(&appID)
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: auditlog.EventRevisionCreated,
			ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
			ApplicationID: appID, ResourceType: "revision", ResourceID: id, RevisionID: id,
			Metadata: map[string]string{"name": body.Name, "model_id": body.ModelID},
		})
	}
	jsonOK(w, map[string]string{"id": id})
}

func (h *handler) adminRevisionAction(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/revisions/")
	ctx := r.Context()

	if !h.requireResourceAccess(w, r, "revision", id) {
		return
	}

	switch r.Method {
	case http.MethodDelete:
		var appID string
		_ = h.db.QueryRow(ctx, `
			SELECT m.application_id::text FROM model.revision r
			JOIN core.model m ON m.id = r.model_id WHERE r.id=$1::uuid`, id).Scan(&appID)
		if _, err := h.db.Exec(ctx, `DELETE FROM model.revision WHERE id=$1::uuid`, id); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryAdmin, EventType: auditlog.EventRevisionDeleted,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				// RevisionID intentionally left unset — see the identical
				// note on the developer-console revision-delete path above;
				// the FK would reject a self-reference to the just-deleted row.
				ApplicationID: appID, ResourceType: "revision", ResourceID: id,
			})
		}
		jsonOK(w, map[string]string{"status": "deleted"})
	case http.MethodPatch:
		var body struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
		if _, err := h.db.Exec(ctx,
			`UPDATE model.revision SET name=$2, description=$3 WHERE id=$1::uuid`,
			id, body.Name, body.Description); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			var appID string
			_ = h.db.QueryRow(ctx, `
				SELECT m.application_id::text FROM model.revision r
				JOIN core.model m ON m.id = r.model_id WHERE r.id=$1::uuid`, id).Scan(&appID)
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryAdmin, EventType: auditlog.EventRevisionUpdated,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: appID, ResourceType: "revision", ResourceID: id, RevisionID: id,
				Metadata: map[string]string{"name": body.Name},
			})
		}
		jsonOK(w, map[string]string{"status": "ok"})
	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// assignableRoles returns the identity.user_role values an actor holding
// actorRoles may grant to (or revoke from) another user, regardless of
// whether the grant is workspace-scoped or platform-level. Strictly below
// the actor's own tier: a tenant_admin can never mint another tenant_admin,
// a developer can never mint another developer or a tenant_admin — otherwise
// any tenant_admin/developer account could self-replicate or escalate peers
// to platform_admin via the workspace-scoped role-grant path.
func assignableRoles(actorRoles []string) []string {
	has := func(role string) bool {
		for _, r := range actorRoles {
			if r == role {
				return true
			}
		}
		return false
	}
	switch {
	case has("platform_admin"):
		return []string{"platform_admin", "tenant_admin", "developer", "business_admin", "business_user"}
	case has("tenant_admin"):
		return []string{"developer", "business_admin", "business_user"}
	case has("developer"):
		return []string{"business_admin", "business_user"}
	default:
		return nil
	}
}

// businessRoles are the operational roles whose whole purpose is to be scoped
// to a workspace: unlike the platform tiers, a NULL-workspace grant of one is
// inert, because actorCanAccessApp resolves access by joining
// role_assignment.workspace_id to the application's workspace.
func isBusinessRole(role string) bool {
	return role == "business_admin" || role == "business_user"
}

// canGrantWorkspaceRole reports whether an actor may grant or revoke role
// inside a specific workspace.
//
// canManageResourceAccess holds developers out of resource decisions on the
// grounds that "developers build within a model, while deciding who may open
// it belongs to whoever owns the tenant". That is still the rule for
// applications and models. It cannot be the rule for business roles, though,
// because it left a developer able to assign business_admin/business_user —
// roleIsAssignableBy has always permitted it — but only unscoped, and an
// unscoped business role grants no application access at all. The capability
// existed and produced users who could not open anything.
//
// So developers may place people in workspaces, and only in workspaces they
// already reach (adminCanAccessWorkspace, checked by the caller), and only
// with the business roles already below their tier. Granting developer or
// tenant_admin inside a workspace stays closed to them, as does app/model
// access itself.
func canGrantWorkspaceRole(actorRoles []string, role string) bool {
	if canManageResourceAccess(actorRoles) {
		return true
	}
	for _, r := range actorRoles {
		if r == "developer" && isBusinessRole(role) {
			return true
		}
	}
	return false
}

func roleIsAssignableBy(actorRoles []string, role string) bool {
	for _, r := range assignableRoles(actorRoles) {
		if r == role {
			return true
		}
	}
	return false
}

// canManageResourceAccess reports whether an actor may grant or revoke access
// to specific applications and models, and workspace-scoped roles — the
// "Resource access" section of the users screen.
//
// A developer may administer users (create, invite, rename, grant the business
// roles below their tier) but not decide who reaches which application or
// model. That is a tenant-shaped decision: developers build within a model,
// while deciding who may open it belongs to whoever owns the tenant.
//
// Enforced here rather than only in the console, because hiding a control is
// not the same as refusing the request behind it.
func canManageResourceAccess(actorRoles []string) bool {
	for _, r := range actorRoles {
		if r == "platform_admin" || r == "tenant_admin" {
			return true
		}
	}
	return false
}

// ── /api/admin/users/{id} ─────────────────────────────────────────────────────

func (h *handler) adminUserAction(w http.ResponseWriter, r *http.Request) {
	// Also handles POST on /api/admin/users/ (trailing slash)
	path := strings.TrimPrefix(r.URL.Path, "/api/admin/users/")
	ctx := r.Context()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	allAdminScope, adminCustomerIDs, err := h.adminScopeCustomerIDs(ctx, act)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}

	// POST with no ID = create user
	if r.Method == http.MethodPost && path == "" {
		var body struct {
			Email string `json:"email"`
			// Both are required. Keycloak's realm marks first and last name
			// mandatory, so an account created without them is met with an
			// "Update Account Information" form the moment the invitation is
			// opened — asking the new user for something whoever invited them
			// already knew. DisplayName remains accepted for older callers and
			// is split when the two are not given separately.
			FirstName   string `json:"first_name"`
			LastName    string `json:"last_name"`
			DisplayName string `json:"display_name"`
			Role        string `json:"role"`
			// Optional. A business role only does anything when it is scoped
			// to a workspace, so an invite that assigns one has to be able to
			// say which — otherwise the new user is created with a role that
			// grants no application access.
			WorkspaceID string `json:"workspace_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
		if body.Role != "" && !roleIsAssignableBy(act.Roles, body.Role) {
			jsonErr(w, fmt.Errorf("forbidden: you may not assign the %q role", body.Role), http.StatusForbidden)
			return
		}
		if body.WorkspaceID != "" {
			// Same two checks the standalone grant endpoint applies, for the
			// same reason: creating a user is not a way around them.
			if body.Role == "" {
				jsonErr(w, fmt.Errorf("workspace_id given without a role"), http.StatusBadRequest)
				return
			}
			if !canGrantWorkspaceRole(act.Roles, body.Role) {
				jsonErr(w, fmt.Errorf("forbidden: you may not grant the %q role inside a workspace", body.Role), http.StatusForbidden)
				return
			}
			canTouchWorkspace, wsErr := h.adminCanAccessWorkspace(ctx, act, body.WorkspaceID)
			if wsErr != nil {
				jsonErr(w, wsErr, http.StatusInternalServerError)
				return
			}
			if !canTouchWorkspace {
				jsonErr(w, fmt.Errorf("forbidden: workspace is outside your tenant scope"), http.StatusForbidden)
				return
			}
		}
		customerID := ""
		if !allAdminScope {
			if len(adminCustomerIDs) == 0 {
				jsonErr(w, fmt.Errorf("forbidden: no tenant scope available for user creation"), http.StatusForbidden)
				return
			}
			customerID = adminCustomerIDs[0]
		}
		if body.Email == "" {
			jsonErr(w, fmt.Errorf("email is required"), http.StatusBadRequest)
			return
		}
		firstName, lastName := strings.TrimSpace(body.FirstName), strings.TrimSpace(body.LastName)
		if firstName == "" && lastName == "" {
			firstName, lastName = keycloak.SplitDisplayName(body.DisplayName)
		}
		if firstName == "" || lastName == "" {
			jsonErr(w, fmt.Errorf("first_name and last_name are both required: Keycloak asks the invited user "+
				"for whichever is missing before they can finish signing in"), http.StatusBadRequest)
			return
		}
		if body.DisplayName == "" {
			body.DisplayName = firstName + " " + lastName
		}
		// The tenant's plan may cap its users. The tenant is the admin's
		// own scope, the routed one, or the workspace the role goes into;
		// a platform-level creation with none of those is not capped.
		if cid := firstNonEmpty(customerID, tenantdb.TenantFrom(ctx), h.customerOfWorkspace(ctx, body.WorkspaceID)); cid != "" && h.plans != nil {
			if err := h.plans.CheckUsers(ctx, h.db.For(ctx), cid, 1); err != nil {
				h.jsonLimitErr(w, err)
				return
			}
		}

		// Resolve the subject this user will authenticate as.
		//
		// The synthetic "admin-created-<email>" form below is a DEV-ONLY
		// arrangement: resolveDevActor accepts it as an X-Dev-User persona.
		// Under JWKS validation the subject in a real token is a UUID minted
		// by Keycloak, so a synthetic one matches nothing and the account is
		// unusable — silently, because the console reports success either way.
		// Outside dev mode, therefore, provisioning must actually happen.
		sub := "admin-created-" + body.Email
		createdInKeycloak := false
		if h.kc == nil && !h.devMode {
			jsonErr(w, fmt.Errorf("identity provider is not configured: cannot create a user who would be able to sign in "+
				"(set KEYCLOAK_ADMIN_CLIENT_ID and KEYCLOAK_ADMIN_CLIENT_SECRET)"), http.StatusServiceUnavailable)
			return
		}
		if h.kc != nil {
			// An account may already exist from a previous attempt that failed
			// after this point; adopt it rather than erroring, and do not
			// delete it during cleanup since this request did not create it.
			existing, err := h.kc.FindUserByEmail(ctx, body.Email)
			if err != nil {
				jsonErr(w, fmt.Errorf("look up identity provider account: %w", err), http.StatusBadGateway)
				return
			}
			if existing == "" {
				existing, err = h.kc.CreateUser(ctx, body.Email, firstName, lastName)
				if err != nil {
					jsonErr(w, fmt.Errorf("create identity provider account: %w", err), http.StatusBadGateway)
					return
				}
				createdInKeycloak = true
			}
			sub = existing
		}

		// Undo the identity-provider side of a half-finished creation. Without
		// this, a failure below strands an account that can authenticate but
		// resolves to no application user — and nothing in the console shows it.
		abort := func(status int, err error) {
			if createdInKeycloak {
				if delErr := h.kc.DeleteUser(ctx, sub); delErr != nil {
					h.log.Error().Err(delErr).Str("email", body.Email).
						Msg("could not remove identity provider account after a failed user creation; it is now an orphan")
				}
			}
			jsonErr(w, err, status)
		}

		if h.kc != nil && body.Role != "" {
			if err := h.kc.AssignRealmRole(ctx, sub, body.Role); err != nil {
				abort(http.StatusBadGateway, fmt.Errorf("assign role in identity provider: %w", err))
				return
			}
		}

		var userID string
		if err := h.db.QueryRow(ctx, `
			INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
			VALUES ($1, $2, $3, NULLIF($4, '')::uuid)
			ON CONFLICT (keycloak_sub) DO UPDATE
			SET email=EXCLUDED.email, display_name=EXCLUDED.display_name, customer_id=COALESCE(identity.user.customer_id, EXCLUDED.customer_id)
			RETURNING id::text
		`, sub, body.Email, body.DisplayName, customerID).Scan(&userID); err != nil {
			abort(http.StatusInternalServerError, err)
			return
		}
		// The directory is what routes this person to their database on
		// their next request.
		h.noteUser(ctx, sub, body.Email)
		if body.Role != "" {
			_, _ = h.db.Exec(ctx, `
				INSERT INTO identity.role_assignment (user_id, role, workspace_id)
				VALUES ($1::uuid, $2::identity.user_role, NULLIF($3,'')::uuid)
				ON CONFLICT DO NOTHING
			`, userID, body.Role, body.WorkspaceID)
		}

		// The invitation is what makes the account reachable: creation sets no
		// password, so until the recipient completes it they cannot sign in.
		// A failure here therefore rolls the whole thing back rather than
		// reporting a success that leaves an account nobody can use.
		invited := false
		if h.kc != nil {
			if err := h.kc.SendInvite(ctx, sub, inviteLifetime); err != nil {
				_, _ = h.db.Exec(ctx, `DELETE FROM identity.user WHERE id=$1::uuid`, userID)
				abort(http.StatusBadGateway, fmt.Errorf("%w — no user was created; "+
					"check the realm's SMTP settings", err))
				return
			}
			invited = true
		}

		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: auditlog.EventUserCreated,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ResourceType: "identity_user", ResourceID: userID,
			Metadata: map[string]string{"email": body.Email, "role": body.Role,
				"invited": strconv.FormatBool(invited)},
		})
		jsonOK(w, map[string]any{"id": userID, "status": "created", "invited": invited})
		return
	}

	// Split path into all segments: "{id}", "{id}/roles", "{id}/roles/{role}",
	// "{id}/access/apps/{appId}", "{id}/access/models/{modelId}"
	parts := strings.Split(path, "/")
	userID := parts[0]
	if userID == "" {
		jsonErr(w, fmt.Errorf("user id required"), http.StatusBadRequest)
		return
	}
	canTouchUser, err := h.adminCanAccessUser(ctx, act, userID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !canTouchUser {
		jsonErr(w, fmt.Errorf("forbidden: user is outside your tenant scope"), http.StatusForbidden)
		return
	}

	// A platform admin may only be modified by another platform admin —
	// developers and tenant admins can see them but never edit, re-role,
	// change access for, or delete them.
	if r.Method != http.MethodGet && !act.hasRole("platform_admin") {
		var targetIsPlatformAdmin bool
		_ = h.db.QueryRow(ctx, `
			SELECT EXISTS(SELECT 1 FROM identity.role_assignment WHERE user_id=$1::uuid AND role='platform_admin')
		`, userID).Scan(&targetIsPlatformAdmin)
		if targetIsPlatformAdmin {
			jsonErr(w, fmt.Errorf("forbidden: only a platform admin can modify a platform admin"), http.StatusForbidden)
			return
		}
	}

	// POST /api/admin/users/{id}/invite — resend the set-your-password email
	if len(parts) == 2 && parts[1] == "invite" && r.Method == http.MethodPost {
		if h.kc == nil {
			jsonErr(w, fmt.Errorf("identity provider is not configured; cannot send invitations"), http.StatusServiceUnavailable)
			return
		}
		var inviteSub, inviteEmail string
		if err := h.db.QueryRow(ctx,
			`SELECT keycloak_sub, email FROM identity.user WHERE id=$1::uuid`, userID,
		).Scan(&inviteSub, &inviteEmail); err != nil {
			jsonErr(w, fmt.Errorf("user not found"), http.StatusNotFound)
			return
		}
		// Users predating identity-provider provisioning carry a synthetic
		// subject and have no account to invite. Say so plainly rather than
		// letting Keycloak fail on a subject it has never seen.
		if strings.HasPrefix(inviteSub, "admin-created-") {
			jsonErr(w, fmt.Errorf("this user has no identity provider account (created before provisioning existed); "+
				"delete and re-create them to issue an invitation"), http.StatusConflict)
			return
		}
		if err := h.kc.SendInvite(ctx, inviteSub, inviteLifetime); err != nil {
			jsonErr(w, fmt.Errorf("%w — check the realm's SMTP settings", err), http.StatusBadGateway)
			return
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: auditlog.EventUserUpdated,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ResourceType: "identity_user", ResourceID: userID,
			Metadata: map[string]string{"action": "invitation_resent", "email": inviteEmail},
		})
		jsonOK(w, map[string]string{"status": "invited", "email": inviteEmail})
		return
	}

	// POST /api/admin/users/{id}/roles — add a role (optionally scoped to a workspace)
	if len(parts) == 2 && parts[1] == "roles" && r.Method == http.MethodPost {
		var body struct {
			Role        string `json:"role"`
			WorkspaceID string `json:"workspace_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Role == "" {
			jsonErr(w, fmt.Errorf("role required"), http.StatusBadRequest)
			return
		}
		if !roleIsAssignableBy(act.Roles, body.Role) {
			jsonErr(w, fmt.Errorf("forbidden: you may not assign the %q role", body.Role), http.StatusForbidden)
			return
		}
		if body.WorkspaceID != "" {
			// A workspace-scoped grant is resource access, not a platform
			// role — it decides which workspace someone reaches. Developers
			// are admitted for business roles only; see canGrantWorkspaceRole.
			if !canGrantWorkspaceRole(act.Roles, body.Role) {
				jsonErr(w, fmt.Errorf("forbidden: you may not grant the %q role inside a workspace", body.Role), http.StatusForbidden)
				return
			}
			canTouchWorkspace, err := h.adminCanAccessWorkspace(ctx, act, body.WorkspaceID)
			if err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			if !canTouchWorkspace {
				jsonErr(w, fmt.Errorf("forbidden: workspace is outside your tenant scope"), http.StatusForbidden)
				return
			}
		}
		var execErr error
		if body.WorkspaceID != "" {
			_, execErr = h.db.Exec(ctx, `
				INSERT INTO identity.role_assignment (user_id, role, workspace_id)
				VALUES ($1::uuid, $2::identity.user_role, $3::uuid)
				ON CONFLICT DO NOTHING
			`, userID, body.Role, body.WorkspaceID)
		} else {
			_, execErr = h.db.Exec(ctx, `
				INSERT INTO identity.role_assignment (user_id, role)
				VALUES ($1::uuid, $2::identity.user_role)
				ON CONFLICT DO NOTHING
			`, userID, body.Role)
		}
		if execErr != nil {
			jsonErr(w, execErr, http.StatusInternalServerError)
			return
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: auditlog.EventUserRoleGranted,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ResourceType: "identity_user", ResourceID: userID,
			Metadata: map[string]string{"role": body.Role, "workspace_id": body.WorkspaceID},
		})
		jsonOK(w, map[string]string{"status": "ok"})
		return
	}

	// DELETE /api/admin/users/{id}/roles/{role}[?workspace_id=...] — remove a role
	if len(parts) == 3 && parts[1] == "roles" && r.Method == http.MethodDelete {
		role := parts[2]
		wsID := r.URL.Query().Get("workspace_id")
		if !roleIsAssignableBy(act.Roles, role) {
			jsonErr(w, fmt.Errorf("forbidden: you may not remove the %q role", role), http.StatusForbidden)
			return
		}
		if wsID != "" {
			// Symmetric with the grant above: revoking workspace-scoped access
			// is the same decision as granting it.
			if !canGrantWorkspaceRole(act.Roles, role) {
				jsonErr(w, fmt.Errorf("forbidden: you may not remove the %q role from a workspace", role), http.StatusForbidden)
				return
			}
			canTouchWorkspace, err := h.adminCanAccessWorkspace(ctx, act, wsID)
			if err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			if !canTouchWorkspace {
				jsonErr(w, fmt.Errorf("forbidden: workspace is outside your tenant scope"), http.StatusForbidden)
				return
			}
		}
		// The same lock-yourself-out hazard as self-deletion, reached through
		// the other control on the same screen. roleIsAssignableBy already
		// stops a developer or tenant_admin from revoking their own tier, so
		// the one case that actually gets here is a platform_admin removing
		// their own platform_admin grant — and for the sole platform admin
		// that ends user administration for the whole deployment as surely as
		// deleting the account would. Stated as "your last admin-capable
		// grant" rather than naming platform_admin, so it keeps holding if the
		// assignable-role matrix changes.
		var keepsAdminAccess bool
		if err := h.db.QueryRow(ctx, `
			SELECT $1::uuid <> $2::uuid OR EXISTS (
			    SELECT 1 FROM identity.role_assignment
			    WHERE user_id=$2::uuid
			      AND role IN ('developer','platform_admin','tenant_admin')
			      AND NOT (role=$3::identity.user_role
			               AND workspace_id IS NOT DISTINCT FROM NULLIF($4,'')::uuid)
			)
		`, userID, act.UserID, role, wsID).Scan(&keepsAdminAccess); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if !keepsAdminAccess {
			jsonErr(w, fmt.Errorf("you cannot remove your own %q role: it is what grants you user administration; "+
				"ask another administrator to do it", role), http.StatusForbidden)
			return
		}

		var execErr error
		if wsID != "" {
			_, execErr = h.db.Exec(ctx, `
				DELETE FROM identity.role_assignment
				WHERE user_id=$1::uuid AND role=$2::identity.user_role AND workspace_id=$3::uuid
			`, userID, role, wsID)
		} else {
			_, execErr = h.db.Exec(ctx, `
				DELETE FROM identity.role_assignment
				WHERE user_id=$1::uuid AND role=$2::identity.user_role AND workspace_id IS NULL
			`, userID, role)
		}
		if execErr != nil {
			jsonErr(w, execErr, http.StatusInternalServerError)
			return
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: auditlog.EventUserRoleRevoked,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ResourceType: "identity_user", ResourceID: userID,
			Metadata: map[string]string{"role": role, "workspace_id": wsID},
		})
		jsonOK(w, map[string]string{"status": "ok"})
		return
	}

	// POST/DELETE /api/admin/users/{id}/access/apps/{appId}
	if len(parts) == 4 && parts[1] == "access" && parts[2] == "apps" {
		if !canManageResourceAccess(act.Roles) {
			jsonErr(w, fmt.Errorf("forbidden: you may not manage resource access"), http.StatusForbidden)
			return
		}
		appID := parts[3]
		canTouchApp, err := h.adminCanAccessApp(ctx, act, appID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if !canTouchApp {
			jsonErr(w, fmt.Errorf("forbidden: app is outside your tenant scope"), http.StatusForbidden)
			return
		}
		var event auditlog.EventType
		switch r.Method {
		case http.MethodPost:
			if _, err := h.db.Exec(ctx, `
				INSERT INTO identity.user_app_access (user_id, application_id)
				VALUES ($1::uuid, $2::uuid) ON CONFLICT DO NOTHING
			`, userID, appID); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			event = auditlog.EventUserAppAccessGranted
		case http.MethodDelete:
			if _, err := h.db.Exec(ctx, `
				DELETE FROM identity.user_app_access WHERE user_id=$1::uuid AND application_id=$2::uuid
			`, userID, appID); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			event = auditlog.EventUserAppAccessRevoked
		default:
			jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
			return
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: event,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ApplicationID: appID, ResourceType: "identity_user", ResourceID: userID,
			Metadata: map[string]string{"application_id": appID},
		})
		jsonOK(w, map[string]string{"status": "ok"})
		return
	}

	// POST/DELETE /api/admin/users/{id}/access/models/{modelId}
	if len(parts) == 4 && parts[1] == "access" && parts[2] == "models" {
		if !canManageResourceAccess(act.Roles) {
			jsonErr(w, fmt.Errorf("forbidden: you may not manage resource access"), http.StatusForbidden)
			return
		}
		modelID := parts[3]
		canTouchModel, err := h.adminCanAccessModel(ctx, act, modelID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if !canTouchModel {
			jsonErr(w, fmt.Errorf("forbidden: model is outside your tenant scope"), http.StatusForbidden)
			return
		}
		var event auditlog.EventType
		switch r.Method {
		case http.MethodPost:
			if _, err := h.db.Exec(ctx, `
				INSERT INTO identity.user_model_access (user_id, model_id)
				VALUES ($1::uuid, $2::uuid) ON CONFLICT DO NOTHING
			`, userID, modelID); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			event = auditlog.EventUserModelAccessGranted
		case http.MethodDelete:
			if _, err := h.db.Exec(ctx, `
				DELETE FROM identity.user_model_access WHERE user_id=$1::uuid AND model_id=$2::uuid
			`, userID, modelID); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			event = auditlog.EventUserModelAccessRevoked
		default:
			jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
			return
		}
		var appID string
		_ = h.db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID)
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: event,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ApplicationID: appID, ResourceType: "identity_user", ResourceID: userID,
			Metadata: map[string]string{"model_id": modelID},
		})
		jsonOK(w, map[string]string{"status": "ok"})
		return
	}

	switch r.Method {
	case http.MethodPatch:
		var body struct {
			Email       string `json:"email"`
			DisplayName string `json:"display_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}

		// The address is the sign-in identity in the identity provider, and
		// this endpoint only ever wrote the application's copy. Changing it
		// here would leave the account signing in under the old address while
		// the console displayed the new one, and invitations would go to
		// whichever the provider still held. Rejected rather than silently
		// ignored, so a caller that tries is told why.
		var currentEmail string
		if err := h.db.QueryRow(ctx, `SELECT email FROM identity.user WHERE id=$1::uuid`, userID).Scan(&currentEmail); err != nil {
			jsonErr(w, fmt.Errorf("user not found"), http.StatusNotFound)
			return
		}
		if body.Email != "" && body.Email != currentEmail {
			jsonErr(w, fmt.Errorf("email cannot be changed: it is the sign-in identity; "+
				"delete this user and invite them at the new address"), http.StatusBadRequest)
			return
		}

		// display_name only. The previous statement also wrote email=$2, so a
		// request carrying just a display name blanked the address — the
		// column is NOT NULL but "" satisfies it, which would have broken
		// sign-in for that user with nothing to show why.
		if body.DisplayName != "" {
			if _, err := h.db.Exec(ctx, `UPDATE identity.user SET display_name=$2 WHERE id=$1::uuid`,
				userID, body.DisplayName); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryAdmin, EventType: auditlog.EventUserUpdated,
				ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
				ResourceType: "identity_user", ResourceID: userID,
				Metadata: map[string]string{"display_name": body.DisplayName},
			})
		}
		jsonOK(w, map[string]string{"status": "ok"})

	case http.MethodDelete:
		// Deleting your own account is not undoable from the console: the
		// application row and the identity-provider account both go, and the
		// session that asked for it no longer resolves to anyone. For the sole
		// platform admin — the shape a fresh deployment starts in — that locks
		// everyone out of user administration permanently, with no path back
		// through the product. Another administrator can always do it instead.
		//
		// Compared as uuid rather than as text: the ::uuid casts below accept
		// uppercase, braced and unhyphenated spellings of the same id, so a
		// string comparison here would miss a self-delete written any of those
		// ways while the DELETE still found the row.
		var isSelf bool
		if err := h.db.QueryRow(ctx, `SELECT $1::uuid = $2::uuid`, userID, act.UserID).Scan(&isSelf); err != nil {
			jsonErr(w, fmt.Errorf("invalid user id"), http.StatusBadRequest)
			return
		}
		if isSelf {
			jsonErr(w, fmt.Errorf("you cannot delete your own account; ask another administrator to remove it"),
				http.StatusForbidden)
			return
		}

		// Remove the identity-provider account too. Deleting only the
		// application row leaves an account that can still authenticate
		// against Keycloak — it resolves to no user so requests 401, but it
		// keeps the address occupied, so re-creating the same person later
		// silently adopts the old account instead of provisioning a new one.
		var delSub string
		_ = h.db.QueryRow(ctx, `SELECT keycloak_sub FROM identity.user WHERE id=$1::uuid`, userID).Scan(&delSub)
		if err := h.removeUserAccount(ctx, userID, delSub); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: auditlog.EventUserDeleted,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ResourceType: "identity_user", ResourceID: userID,
		})
		jsonOK(w, map[string]string{"status": "deleted"})

	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// ── /api/automation/rules/{id} ────────────────────────────────────────────────

func (h *handler) automationRuleAction(w http.ResponseWriter, r *http.Request) {
	ruleID := strings.TrimPrefix(r.URL.Path, "/api/automation/rules/")
	ctx := r.Context()
	store := workflow.NewStore(h.db.For(ctx))

	// Ownership guard: the route's dev() gate only confirms the caller
	// holds "developer" globally — it says nothing about which tenant/app
	// this specific rule belongs to. Without this, a developer in one
	// tenant could PATCH/DELETE another tenant's automation rule just by
	// guessing its UUID. Reuses the same actorCanAccessApp helper
	// resolveDemoAppID already calls elsewhere in this file.
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	var ruleAppID string
	if err := h.db.QueryRow(ctx,
		`SELECT application_id::text FROM workflow.automation_rule WHERE id=$1::uuid`, ruleID,
	).Scan(&ruleAppID); err != nil {
		jsonErr(w, fmt.Errorf("automation rule not found"), http.StatusNotFound)
		return
	}
	if canAccess, caErr := h.actorCanAccessApp(ctx, act, ruleAppID); caErr != nil {
		jsonErr(w, caErr, http.StatusInternalServerError)
		return
	} else if !canAccess {
		jsonErr(w, fmt.Errorf("forbidden: automation rule is outside your access scope"), http.StatusForbidden)
		return
	}

	switch r.Method {
	case http.MethodPatch:
		var body struct {
			Name                string `json:"name"`
			Description         string `json:"description"`
			TriggerType         string `json:"trigger_type"`
			WorkflowName        string `json:"workflow_name"`
			WorkflowDefID       string `json:"workflow_def_id"`
			SourceFormID        string `json:"source_form_id"`
			SourceGridID        string `json:"source_grid_id"`
			SourceIntegrationID string `json:"source_integration_id"`
			Enabled             *bool  `json:"enabled"`
			CronExpr            string `json:"cron_expr"`
			Timezone            string `json:"timezone"`
			MisfirePolicy       string `json:"misfire_policy"`
			MaxRetries          int    `json:"max_retries"`
			RetryBackoffSeconds int    `json:"retry_backoff_seconds"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
		// A CronExpr in the body means the caller means to (re)configure
		// scheduling; omitting it leaves an existing schedule untouched.
		var sched *workflow.ScheduleConfig
		if body.CronExpr != "" {
			sched = &workflow.ScheduleConfig{
				CronExpr:            body.CronExpr,
				Timezone:            body.Timezone,
				MisfirePolicy:       body.MisfirePolicy,
				MaxRetries:          body.MaxRetries,
				RetryBackoffSeconds: body.RetryBackoffSeconds,
			}
		}
		updated, err := store.UpdateAutomationRuleScoped(ctx, ruleID, body.Name, body.Description, body.TriggerType, body.WorkflowName, body.WorkflowDefID, body.SourceFormID, body.SourceGridID, body.SourceIntegrationID, body.Enabled, sched)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryModelChange, EventType: auditlog.EventAutomationRuleUpdated,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: updated.ApplicationID, ResourceType: "automation_rule", ResourceID: ruleID, RevisionID: updated.RevisionID,
				Metadata: map[string]string{"name": body.Name},
			})
		}
		jsonOK(w, updated)

	case http.MethodDelete:
		var delAppID, delRevisionID string
		_ = h.db.QueryRow(ctx, `SELECT application_id::text, COALESCE(revision_id::text,'') FROM workflow.automation_rule WHERE id=$1::uuid`, ruleID).Scan(&delAppID, &delRevisionID)
		if err := store.DeleteAutomationRule(ctx, ruleID); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryModelChange, EventType: auditlog.EventAutomationRuleDeleted,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: delAppID, ResourceType: "automation_rule", ResourceID: ruleID, RevisionID: delRevisionID,
			})
		}
		jsonOK(w, map[string]string{"status": "deleted"})

	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// ── helpers ───────────────────────────────────────────────────────────────────

func jsonOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, err error, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func toLabel(name string) string {
	parts := strings.Split(name, "_")
	for i, p := range parts {
		if len(p) > 0 {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

// ── /api/developer/grids ─────────────────────────────────────────────────────

type gridDefResponse struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	RevisionID      string          `json:"revision_id"`
	MetricIDs       []string        `json:"metric_ids"`
	DimensionIDs    []string        `json:"dimension_ids"`
	DimensionLevels map[string]*int `json:"dimension_levels"` // dim_id → display_level (null=all)
}

func (h *handler) developerGrids(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}

	revisionID := r.URL.Query().Get("revision_id")

	switch r.Method {
	case http.MethodGet:
		gridQ := `SELECT id::text, name, COALESCE(revision_id::text,'') FROM model.grid_def WHERE model_id=$1::uuid ORDER BY created_at`
		gridArgs := []any{modelID}
		if revisionID != "" {
			gridQ = `SELECT id::text, name, COALESCE(revision_id::text,'') FROM model.grid_def WHERE model_id=$1::uuid AND revision_id=$2::uuid ORDER BY created_at`
			gridArgs = []any{modelID, revisionID}
		}
		rows, err := h.db.Query(ctx, gridQ, gridArgs...)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		var grids []gridDefResponse
		for rows.Next() {
			var g gridDefResponse
			if err := rows.Scan(&g.ID, &g.Name, &g.RevisionID); err != nil {
				continue
			}
			grids = append(grids, g)
		}
		rows.Close()
		for i, g := range grids {
			mRows, _ := h.db.Query(ctx, `SELECT metric_id::text FROM model.grid_metric WHERE grid_id=$1::uuid ORDER BY sort_order`, g.ID)
			if mRows != nil {
				for mRows.Next() {
					var id string
					_ = mRows.Scan(&id)
					grids[i].MetricIDs = append(grids[i].MetricIDs, id)
				}
				mRows.Close()
			}
			if grids[i].MetricIDs == nil {
				grids[i].MetricIDs = []string{}
			}
			grids[i].DimensionLevels = map[string]*int{}
			dRows, _ := h.db.Query(ctx, `SELECT dimension_id::text, display_level FROM model.grid_dimension WHERE grid_id=$1::uuid`, g.ID)
			if dRows != nil {
				for dRows.Next() {
					var id string
					var lvl *int
					_ = dRows.Scan(&id, &lvl)
					grids[i].DimensionIDs = append(grids[i].DimensionIDs, id)
					grids[i].DimensionLevels[id] = lvl
				}
				dRows.Close()
			}
			if grids[i].DimensionIDs == nil {
				grids[i].DimensionIDs = []string{}
			}
		}
		if grids == nil {
			grids = []gridDefResponse{}
		}
		jsonOK(w, grids)

	case http.MethodPost:
		var body struct {
			Name       string `json:"name"`
			RevisionID string `json:"revision_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
			jsonErr(w, fmt.Errorf("name required"), http.StatusBadRequest)
			return
		}
		modelID = h.pinModelForBodyRevision(ctx, r, modelID, body.RevisionID)
		if !h.requireRevisionInModel(w, r, body.RevisionID, modelID) {
			return
		}
		if body.RevisionID == "" {
			body.RevisionID = revisionID
		}
		var newID string
		var gridErr error
		if body.RevisionID != "" {
			gridErr = h.db.QueryRow(ctx,
				`INSERT INTO model.grid_def (model_id, name, revision_id) VALUES ($1::uuid,$2,$3::uuid) RETURNING id::text`,
				modelID, body.Name, body.RevisionID).Scan(&newID)
		} else {
			gridErr = h.db.QueryRow(ctx,
				`INSERT INTO model.grid_def (model_id, name) VALUES ($1::uuid,$2) RETURNING id::text`,
				modelID, body.Name).Scan(&newID)
		}
		if gridErr != nil {
			jsonErr(w, gridErr, http.StatusInternalServerError)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			var appID string
			_ = h.db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID)
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryModelChange, EventType: auditlog.EventGridCreated,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: appID, ResourceType: "grid", ResourceID: newID, RevisionID: body.RevisionID,
				Metadata: map[string]string{"name": body.Name},
			})
		}
		jsonOK(w, map[string]string{"id": newID, "status": "created"})

	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// gridScope resolves the revision_id/application_id a grid belongs to, for
// audit logging call sites in developerGridAction below.
func (h *handler) gridScope(ctx context.Context, gridID string) (revisionID, appID string) {
	_ = h.db.QueryRow(ctx, `
		SELECT COALESCE(gd.revision_id::text,''), COALESCE(m.application_id::text,'')
		FROM model.grid_def gd JOIN core.model m ON m.id = gd.model_id
		WHERE gd.id = $1::uuid`, gridID).Scan(&revisionID, &appID)
	return
}

// auditGridUpdated logs a grid.updated event with a metadata discriminator,
// mirroring auditDimensionUpdated's fold-in convention for sub-actions
// (metric/dimension attach-detach) that aren't independently
// security-sensitive enough to deserve their own event type.
func (h *handler) auditGridUpdated(ctx context.Context, r *http.Request, gridID, subAction string, extra map[string]string) {
	a, e := h.resolveActor(ctx, r)
	if e != nil {
		return
	}
	revisionID, appID := h.gridScope(ctx, gridID)
	meta := map[string]string{"sub_action": subAction}
	for k, v := range extra {
		meta[k] = v
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryModelChange, EventType: auditlog.EventGridUpdated,
		ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
		ApplicationID: appID, ResourceType: "grid", ResourceID: gridID, RevisionID: revisionID,
		Metadata: meta,
	})
}

// ── /api/developer/grids/{id}[/metrics/{mId}|/dimensions/{dId}] ─────────────

func (h *handler) developerGridAction(w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, "/api/developer/grids/")
	parts := strings.SplitN(tail, "/", 3)
	gridID := parts[0]
	ctx := r.Context()

	if !h.requireResourceAccess(w, r, "grid", gridID) {
		return
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodPatch:
			var body struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
				jsonErr(w, fmt.Errorf("name required"), http.StatusBadRequest)
				return
			}
			if _, err := h.db.Exec(ctx, `UPDATE model.grid_def SET name=$2 WHERE id=$1::uuid`, gridID, body.Name); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			h.auditGridUpdated(ctx, r, gridID, "grid", map[string]string{"name": body.Name})
			jsonOK(w, map[string]string{"status": "ok"})
		case http.MethodDelete:
			gridRevisionID, gridAppID := h.gridScope(ctx, gridID)
			h.dropWidgetsReferencing(ctx, gridID)
			if _, err := h.db.Exec(ctx, `DELETE FROM model.grid_def WHERE id=$1::uuid`, gridID); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			if a, e := h.resolveActor(ctx, r); e == nil {
				auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
					Category: auditlog.CategoryModelChange, EventType: auditlog.EventGridDeleted,
					ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
					ApplicationID: gridAppID, ResourceType: "grid", ResourceID: gridID, RevisionID: gridRevisionID,
				})
			}
			jsonOK(w, map[string]string{"status": "deleted"})
		default:
			jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		}
		return
	}

	sub := parts[1]
	subID := ""
	if len(parts) == 3 {
		subID = parts[2]
	}

	switch sub {
	case "metrics":
		switch {
		case r.Method == http.MethodPost && subID != "":
			// Validate that the metric belongs to the same revision as the grid.
			var gridRev, metricRev string
			_ = h.db.QueryRow(ctx, `SELECT COALESCE(revision_id::text,'') FROM model.grid_def WHERE id=$1::uuid`, gridID).Scan(&gridRev)
			_ = h.db.QueryRow(ctx, `SELECT COALESCE(revision_id::text,'') FROM model.metric_def WHERE id=$1::uuid`, subID).Scan(&metricRev)
			if gridRev != metricRev {
				jsonErr(w, fmt.Errorf("metric does not belong to the same revision as the grid"), http.StatusBadRequest)
				return
			}
			// A metric may only belong to one grid at a time. Block the add and
			// name the grid that already owns it rather than silently moving it.
			var existingGridName string
			gmErr := h.db.QueryRow(ctx, `
				SELECT gd.name FROM model.grid_metric gm
				JOIN model.grid_def gd ON gd.id = gm.grid_id
				WHERE gm.metric_id=$1::uuid AND gm.grid_id!=$2::uuid
				LIMIT 1
			`, subID, gridID).Scan(&existingGridName)
			if gmErr == nil {
				jsonErr(w, fmt.Errorf("metric is already in grid %q — a metric can only belong to one grid; remove it there first", existingGridName), http.StatusConflict)
				return
			}
			if err := h.gridMembershipTx(ctx, gridID,
				`INSERT INTO model.grid_metric (grid_id, metric_id, sort_order) VALUES ($1::uuid,$2::uuid,0) ON CONFLICT DO NOTHING`,
				gridID, subID); err != nil {
				if metricformula.IsValidationError(err) {
					jsonErr(w, err, http.StatusBadRequest)
					return
				}
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			h.auditGridUpdated(ctx, r, gridID, "metric_attached", map[string]string{"metric_id": subID})
			jsonOK(w, map[string]string{"status": "ok"})
		case r.Method == http.MethodDelete && subID != "":
			if _, err := h.db.Exec(ctx,
				`DELETE FROM model.grid_metric WHERE grid_id=$1::uuid AND metric_id=$2::uuid`,
				gridID, subID); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			h.auditGridUpdated(ctx, r, gridID, "metric_detached", map[string]string{"metric_id": subID})
			jsonOK(w, map[string]string{"status": "deleted"})
		default:
			jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		}

	case "dimensions":
		switch {
		case r.Method == http.MethodPost && subID != "":
			if err := h.gridMembershipTx(ctx, gridID,
				`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid,$2::uuid) ON CONFLICT DO NOTHING`,
				gridID, subID); err != nil {
				if metricformula.IsValidationError(err) {
					jsonErr(w, err, http.StatusBadRequest)
					return
				}
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			h.auditGridUpdated(ctx, r, gridID, "dimension_attached", map[string]string{"dimension_id": subID})
			jsonOK(w, map[string]string{"status": "ok"})
		case r.Method == http.MethodPatch && subID != "":
			// Update display_level for this grid-dimension pair.
			// Body: {"display_level": null | 0 | -1 | n}
			var body struct {
				DisplayLevel *int `json:"display_level"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
				return
			}
			if _, err := h.db.Exec(ctx,
				`UPDATE model.grid_dimension SET display_level=$3 WHERE grid_id=$1::uuid AND dimension_id=$2::uuid`,
				gridID, subID, body.DisplayLevel); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			h.auditGridUpdated(ctx, r, gridID, "dimension_display_level_updated", map[string]string{"dimension_id": subID})
			jsonOK(w, map[string]string{"status": "ok"})
		case r.Method == http.MethodDelete && subID != "":
			if _, err := h.db.Exec(ctx,
				`DELETE FROM model.grid_dimension WHERE grid_id=$1::uuid AND dimension_id=$2::uuid`,
				gridID, subID); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			h.auditGridUpdated(ctx, r, gridID, "dimension_detached", map[string]string{"dimension_id": subID})
			jsonOK(w, map[string]string{"status": "deleted"})
		default:
			jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		}

	default:
		jsonErr(w, fmt.Errorf("not found"), http.StatusNotFound)
	}
}

// ── /api/developer/folders ────────────────────────────────────────────────────

type dashboardFolderItem struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	ParentID *string `json:"parent_id"`
}

func (h *handler) developerFolders(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	revisionID, _, revErr := h.resolveRevisionCtx(ctx, r.URL.Query().Get("revision_id"), modelID)
	if rejectForeignRevision(w, revErr) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		rows, err := h.db.Query(ctx,
			`SELECT id::text, name, parent_id::text FROM model.dashboard_folder
			 WHERE model_id=$1::uuid
			   AND ($2 = '' OR revision_id IS NULL OR revision_id::text = $2)
			 ORDER BY created_at`,
			modelID, revisionID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		var folders []dashboardFolderItem
		for rows.Next() {
			var f dashboardFolderItem
			_ = rows.Scan(&f.ID, &f.Name, &f.ParentID)
			folders = append(folders, f)
		}
		if folders == nil {
			folders = []dashboardFolderItem{}
		}
		jsonOK(w, folders)

	case http.MethodPost:
		var body struct {
			Name     string `json:"name"`
			ParentID string `json:"parent_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
			jsonErr(w, fmt.Errorf("name required"), http.StatusBadRequest)
			return
		}
		var newID string
		if err := h.db.QueryRow(ctx,
			`INSERT INTO model.dashboard_folder (model_id, name, parent_id, revision_id) VALUES ($1::uuid, $2, NULLIF($3,'')::uuid, NULLIF($4,'')::uuid) RETURNING id::text`,
			modelID, body.Name, body.ParentID, revisionID).Scan(&newID); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			var appID string
			_ = h.db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID)
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryModelChange, EventType: auditlog.EventFolderCreated,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: appID, ResourceType: "dashboard_folder", ResourceID: newID, RevisionID: revisionID,
				Metadata: map[string]string{"name": body.Name},
			})
		}
		jsonOK(w, map[string]string{"id": newID, "status": "created"})

	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// folderScope resolves the revision_id/application_id a dashboard folder
// belongs to, for audit logging call sites in developerFolderAction below.
func (h *handler) folderScope(ctx context.Context, folderID string) (revisionID, appID string) {
	_ = h.db.QueryRow(ctx, `
		SELECT COALESCE(f.revision_id::text,''), COALESCE(m.application_id::text,'')
		FROM model.dashboard_folder f JOIN core.model m ON m.id = f.model_id
		WHERE f.id = $1::uuid`, folderID).Scan(&revisionID, &appID)
	return
}

func (h *handler) developerFolderAction(w http.ResponseWriter, r *http.Request) {
	folderID := strings.TrimPrefix(r.URL.Path, "/api/developer/folders/")
	ctx := r.Context()

	if !h.requireResourceAccess(w, r, "dashboard_folder", folderID) {
		return
	}
	switch r.Method {
	case http.MethodPatch:
		var body struct {
			Name     string `json:"name"`
			ParentID string `json:"parent_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
			jsonErr(w, fmt.Errorf("name required"), http.StatusBadRequest)
			return
		}
		if _, err := h.db.Exec(ctx,
			`UPDATE model.dashboard_folder SET name=$2, parent_id=NULLIF($3,'')::uuid WHERE id=$1::uuid`,
			folderID, body.Name, body.ParentID); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			revisionID, appID := h.folderScope(ctx, folderID)
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryModelChange, EventType: auditlog.EventFolderUpdated,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: appID, ResourceType: "dashboard_folder", ResourceID: folderID, RevisionID: revisionID,
				Metadata: map[string]string{"name": body.Name},
			})
		}
		jsonOK(w, map[string]string{"status": "ok"})

	case http.MethodDelete:
		folderRevisionID, folderAppID := h.folderScope(ctx, folderID)
		if _, err := h.db.Exec(ctx, `DELETE FROM model.dashboard_folder WHERE id=$1::uuid`, folderID); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryModelChange, EventType: auditlog.EventFolderDeleted,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: folderAppID, ResourceType: "dashboard_folder", ResourceID: folderID, RevisionID: folderRevisionID,
			})
		}
		jsonOK(w, map[string]string{"status": "deleted"})

	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

func (h *handler) businessFolders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	revisionID, _, revErr := h.resolveRevisionCtx(ctx, r.URL.Query().Get("revision_id"), modelID)
	if rejectForeignRevision(w, revErr) {
		return
	}
	// There's no per-folder ACL table, so a folder is visible if it either
	// has no dashboards yet (an empty/new folder — benign) or contains at
	// least one dashboard the caller can see, using the same
	// business_role_member/business_role_dashboard/workspace-fallback
	// check as businessDashboards. Without this, GET /api/folders leaked
	// every folder name regardless of business-role assignment.
	rows, err := h.db.Query(ctx,
		`SELECT f.id::text, f.name, f.parent_id::text FROM model.dashboard_folder f
		 WHERE f.model_id=$1::uuid
		   AND ($2 = '' OR f.revision_id IS NULL OR f.revision_id::text = $2)
		   AND (
		       NOT EXISTS (SELECT 1 FROM model.dashboard_def dd WHERE dd.folder_id = f.id)
		       OR EXISTS (
		           SELECT 1 FROM model.dashboard_def dd
		           WHERE dd.folder_id = f.id
		             AND (
		                 EXISTS (
		                     SELECT 1 FROM identity.business_role_member brm
		                     JOIN identity.business_role_dashboard brd ON brd.role_id = brm.role_id
		                     WHERE brm.user_id = $3::uuid AND brd.dashboard_id = dd.id
		                 )
		                 OR NOT EXISTS (
		                     SELECT 1 FROM identity.business_role_member brm2
		                     JOIN identity.business_role br ON br.id = brm2.role_id
		                     JOIN core.workspace w ON w.id = br.workspace_id
		                     JOIN core.application app ON app.workspace_id = w.id
		                                               OR (app.workspace_id IS NULL AND app.customer_id = w.customer_id)
		                     JOIN core.model m ON m.application_id = app.id
		                     WHERE brm2.user_id = $3::uuid AND m.id = dd.model_id
		                 )
		             )
		       )
		   )
		 ORDER BY f.created_at`,
		modelID, revisionID, a.UserID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var folders []dashboardFolderItem
	for rows.Next() {
		var f dashboardFolderItem
		_ = rows.Scan(&f.ID, &f.Name, &f.ParentID)
		folders = append(folders, f)
	}
	if folders == nil {
		folders = []dashboardFolderItem{}
	}
	jsonOK(w, folders)
}

// ── /api/developer/dashboards ─────────────────────────────────────────────────

type dashboardWidget struct {
	ID          string          `json:"id"`
	WidgetType  string          `json:"widget_type"`
	RefID       *string         `json:"ref_id"`
	Content     *string         `json:"content"`
	Title       *string         `json:"title"`
	ShowTitle   bool            `json:"show_title"`
	WidgetProps json.RawMessage `json:"widget_props,omitempty"`
	SortOrder   int             `json:"sort_order"`
	ColStart    int             `json:"col_start"`
	ColSpan     int             `json:"col_span"`
	PosX        int             `json:"pos_x"`
	PosY        int             `json:"pos_y"`
	SizeW       int             `json:"size_w"`
	SizeH       int             `json:"size_h"`
}

type dashboardDefResponse struct {
	ID       string            `json:"id"`
	Name     string            `json:"name"`
	Tags     []string          `json:"tags"`
	FolderID *string           `json:"folder_id"`
	Widgets  []dashboardWidget `json:"widgets"`
}

func (h *handler) developerDashboards(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}

	switch r.Method {
	case http.MethodGet:
		revisionID := r.URL.Query().Get("revision_id")
		// When no revision specified, fall back to the active revision so developers
		// always see the same dashboards as business users.
		if revisionID == "" {
			_ = h.db.QueryRow(ctx,
				`SELECT COALESCE(active_revision_id::text,'') FROM core.model WHERE id=$1::uuid`, modelID,
			).Scan(&revisionID)
		}
		var rows interface {
			Next() bool
			Scan(...any) error
			Close()
		}
		if revisionID != "" {
			rows, err = h.db.Query(ctx,
				`SELECT id::text, name, COALESCE(tags, '{}'), folder_id::text FROM model.dashboard_def WHERE model_id=$1::uuid AND revision_id=$2::uuid ORDER BY name`,
				modelID, revisionID)
		} else {
			rows, err = h.db.Query(ctx,
				`SELECT id::text, name, COALESCE(tags, '{}'), folder_id::text FROM model.dashboard_def WHERE model_id=$1::uuid ORDER BY name`,
				modelID)
		}
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		var dashboards []dashboardDefResponse
		for rows.Next() {
			var d dashboardDefResponse
			if err := rows.Scan(&d.ID, &d.Name, &d.Tags, &d.FolderID); err != nil {
				continue
			}
			if d.Tags == nil {
				d.Tags = []string{}
			}
			dashboards = append(dashboards, d)
		}
		rows.Close()
		for i, d := range dashboards {
			wRows, _ := h.db.Query(ctx,
				`SELECT id::text, widget_type, ref_id, content, title, show_title, widget_props, sort_order, col_start, col_span, pos_x, pos_y, size_w, size_h FROM model.dashboard_widget WHERE dashboard_id=$1::uuid ORDER BY pos_y, pos_x`, d.ID)
			if wRows != nil {
				for wRows.Next() {
					var w dashboardWidget
					_ = wRows.Scan(&w.ID, &w.WidgetType, &w.RefID, &w.Content, &w.Title, &w.ShowTitle, &w.WidgetProps, &w.SortOrder, &w.ColStart, &w.ColSpan, &w.PosX, &w.PosY, &w.SizeW, &w.SizeH)
					dashboards[i].Widgets = append(dashboards[i].Widgets, w)
				}
				wRows.Close()
			}
			if dashboards[i].Widgets == nil {
				dashboards[i].Widgets = []dashboardWidget{}
			}
		}
		if dashboards == nil {
			dashboards = []dashboardDefResponse{}
		}
		jsonOK(w, dashboards)

	case http.MethodPost:
		var body struct {
			Name       string   `json:"name"`
			Tags       []string `json:"tags"`
			RevisionID string   `json:"revision_id"`
			FolderID   string   `json:"folder_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
			jsonErr(w, fmt.Errorf("name required"), http.StatusBadRequest)
			return
		}
		if body.Tags == nil {
			body.Tags = []string{}
		}
		// Fall back to model's active revision when not specified
		if body.RevisionID == "" {
			_ = h.db.QueryRow(ctx,
				`SELECT COALESCE(active_revision_id::text,'') FROM core.model WHERE id=$1::uuid`, modelID,
			).Scan(&body.RevisionID)
		}
		if err := h.validateDashboardFolder(ctx, modelID, body.FolderID); err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		var newID string
		if body.RevisionID != "" {
			if err := h.db.QueryRow(ctx,
				`INSERT INTO model.dashboard_def (model_id, name, tags, revision_id, folder_id) VALUES ($1::uuid,$2,$3,$4::uuid,NULLIF($5,'')::uuid) RETURNING id::text`,
				modelID, body.Name, body.Tags, body.RevisionID, body.FolderID).Scan(&newID); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
		} else {
			if err := h.db.QueryRow(ctx,
				`INSERT INTO model.dashboard_def (model_id, name, tags, folder_id) VALUES ($1::uuid,$2,$3,NULLIF($4,'')::uuid) RETURNING id::text`,
				modelID, body.Name, body.Tags, body.FolderID).Scan(&newID); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			var appID string
			_ = h.db.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID)
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryModelChange, EventType: auditlog.EventDashboardCreated,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: appID, ResourceType: "dashboard", ResourceID: newID, RevisionID: body.RevisionID,
				Metadata: map[string]string{"name": body.Name},
			})
		}
		jsonOK(w, map[string]string{"id": newID, "status": "created"})

	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// widgetRefOwnerSQL resolves a widget's ref_id to the model that owns the
// referenced row, per widget_type. "import" and "chart" both point at a
// grid_def (confirmed against DashboardWidgets.tsx's ImportWidget/ChartWidget),
// not an integration. automation_button is application-scoped and handled
// separately below.
var widgetRefOwnerSQL = map[string]string{
	"grid":               `SELECT model_id::text, COALESCE(revision_id::text,'') FROM model.grid_def WHERE id=$1::uuid`,
	"chart":              `SELECT model_id::text, COALESCE(revision_id::text,'') FROM model.grid_def WHERE id=$1::uuid`,
	"import":             `SELECT model_id::text, COALESCE(revision_id::text,'') FROM model.grid_def WHERE id=$1::uuid`,
	"form":               `SELECT model_id::text, COALESCE(revision_id::text,'') FROM model.form_def WHERE id=$1::uuid`,
	"metric_kpi":         `SELECT model_id::text, COALESCE(revision_id::text,'') FROM model.metric_def WHERE id=$1::uuid`,
	"integration_button": `SELECT model_id::text, COALESCE(revision_id::text,'') FROM model.integration_def WHERE id=$1::uuid`,
}

// validateWidgetRef rejects a widget whose ref_id points outside its own
// dashboard's model (and, when both sides are revision-scoped, outside its
// revision). ref_id is a bare TEXT column with no foreign key, so nothing in
// the schema stops a widget on your dashboard from naming another tenant's
// grid — and chart-data resolves the model to query FROM that grid, so such a
// widget reads the other tenant's numbers. Unknown widget types and empty
// refs are allowed through: text widgets carry no ref, and a new widget type
// shouldn't fail closed on a check that doesn't know about it yet.
// validateWidgetContent checks what a widget's content column is allowed to
// hold. Only the image widget constrains it: its content IS the picture, a
// base64 data URL kept inline so the image travels with the dashboard
// through revision copies, exports and imports (internal/imagedata). Text
// widgets take prose, and every other type ignores the column.
func validateWidgetContent(widgetType string, content *string) error {
	if widgetType != "image" || content == nil {
		return nil
	}
	return imagedata.Validate("the image", *content, imagedata.MaxWidgetBytes)
}

func (h *handler) validateWidgetRef(ctx context.Context, dashID, widgetType string, refID *string) error {
	if refID == nil || *refID == "" {
		return nil
	}
	var dashModelID, dashRevisionID, dashAppID string
	if err := h.db.QueryRow(ctx, `
		SELECT d.model_id::text, COALESCE(d.revision_id::text,''), COALESCE(m.application_id::text,'')
		FROM model.dashboard_def d JOIN core.model m ON m.id = d.model_id
		WHERE d.id=$1::uuid`, dashID).Scan(&dashModelID, &dashRevisionID, &dashAppID); err != nil {
		return fmt.Errorf("dashboard not found")
	}

	if widgetType == "automation_button" {
		var ruleAppID string
		if err := h.db.QueryRow(ctx,
			`SELECT application_id::text FROM workflow.automation_rule WHERE id=$1::uuid`, *refID,
		).Scan(&ruleAppID); err != nil {
			return fmt.Errorf("automation rule not found")
		}
		if ruleAppID != dashAppID {
			return fmt.Errorf("automation rule belongs to a different application")
		}
		return nil
	}

	query, ok := widgetRefOwnerSQL[widgetType]
	if !ok {
		return nil
	}
	var refModelID, refRevisionID string
	if err := h.db.QueryRow(ctx, query, *refID).Scan(&refModelID, &refRevisionID); err != nil {
		return fmt.Errorf("%s widget references a row that does not exist", widgetType)
	}
	if refModelID != dashModelID {
		return fmt.Errorf("%s widget references a resource in a different model", widgetType)
	}
	// Legacy rows carry a NULL revision and are visible in every revision, so
	// only compare when both sides are actually revision-scoped.
	if dashRevisionID != "" && refRevisionID != "" && dashRevisionID != refRevisionID {
		return fmt.Errorf("%s widget references a resource in a different revision", widgetType)
	}
	return nil
}

// validateDashboardFolder rejects a folder_id that isn't a folder of this
// model. "" means "no folder" (the dashboard sits at the root) and is always
// legal. model.dashboard_def.folder_id has an ON DELETE SET NULL FK but no
// same-model constraint, so without this a dashboard could be filed under
// another model's — or another tenant's — folder and then vanish from its own
// tree.
func (h *handler) validateDashboardFolder(ctx context.Context, modelID, folderID string) error {
	if folderID == "" {
		return nil
	}
	var ok bool
	if err := h.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM model.dashboard_folder WHERE id=$1::uuid AND model_id=$2::uuid)`,
		folderID, modelID).Scan(&ok); err != nil {
		return fmt.Errorf("folder not found")
	}
	if !ok {
		return fmt.Errorf("folder does not belong to this model")
	}
	return nil
}

// dashboardModelInScope reports whether the dashboard's own model is inside
// the caller's access scope, reusing the same actorCanAccessModel check the
// developer endpoints use. The business-user dashboard endpoints that take a
// dashboard ID directly (detail, chart-data) resolve no model of their own,
// so without this their business-role check alone decides access — and its
// "this workspace has no business roles" fallback grants every tenant.
// An unknown dashboard ID returns false, so callers can 404 uniformly.
func (h *handler) dashboardModelInScope(ctx context.Context, a *actor, dashID string) bool {
	var modelID string
	if err := h.db.QueryRow(ctx,
		`SELECT model_id::text FROM model.dashboard_def WHERE id=$1::uuid`, dashID,
	).Scan(&modelID); err != nil {
		return false
	}
	ok, err := h.actorCanAccessModel(ctx, a, modelID)
	return err == nil && ok
}

// dashboardScope resolves the revision_id/application_id a dashboard (or
// one of its widgets) belongs to, for audit logging call sites in
// developerDashboardAction below.
// dropWidgetsReferencing removes dashboard widgets whose ref_id names a
// just-deleted entity (SYNC-01). ref_id is bare TEXT with no FK, so deletes
// of grids/metrics/forms/integrations/rules used to leave dangling widgets —
// a chart over a deleted grid 500'd, a KPI over a deleted metric showed a
// confident 0. Policy: cascade — a widget is cheap and re-creatable, and a
// blocking 409 would need affordances across six delete paths. UUIDs are
// globally unique, so matching ref_id alone cannot hit another entity.
func (h *handler) dropWidgetsReferencing(ctx context.Context, refID string) {
	if refID == "" {
		return
	}
	if _, err := h.db.Exec(ctx, `DELETE FROM model.dashboard_widget WHERE ref_id = $1`, refID); err != nil {
		h.log.Warn().Err(err).Str("ref_id", refID).Msg("drop widgets referencing deleted entity")
	}
	// A chart names its metrics inside widget_props, not in ref_id: a
	// deleted metric left there made the whole chart fail as "metric not
	// accessible". Drop the id from every chart's list, and the chart
	// itself once it plots nothing.
	// Two statements, not one CTE: Postgres will not update and delete the
	// same row in one statement (only one of the two silently happens).
	rows, err := h.db.Query(ctx, `
		UPDATE model.dashboard_widget
		SET widget_props = jsonb_set(widget_props, '{chart,metric_ids}', (widget_props->'chart'->'metric_ids') - $1::text)
		WHERE widget_props->'chart'->'metric_ids' ? $1::text
		RETURNING id::text, jsonb_array_length(widget_props->'chart'->'metric_ids')`, refID)
	if err != nil {
		h.log.Warn().Err(err).Str("ref_id", refID).Msg("drop deleted metric from chart widgets")
		return
	}
	var emptied []string
	for rows.Next() {
		var id string
		var left int
		if rows.Scan(&id, &left) == nil && left == 0 {
			emptied = append(emptied, id)
		}
	}
	rows.Close()
	if len(emptied) > 0 {
		if _, err := h.db.Exec(ctx, `DELETE FROM model.dashboard_widget WHERE id::text = ANY($1)`, emptied); err != nil {
			h.log.Warn().Err(err).Msg("drop charts left without metrics")
		}
	}
}

func (h *handler) dashboardScope(ctx context.Context, dashID string) (revisionID, appID string) {
	_ = h.db.QueryRow(ctx, `
		SELECT COALESCE(d.revision_id::text,''), COALESCE(m.application_id::text,'')
		FROM model.dashboard_def d JOIN core.model m ON m.id = d.model_id
		WHERE d.id = $1::uuid`, dashID).Scan(&revisionID, &appID)
	return
}

// ── /api/developer/dashboards/{id}[/widgets[/{wId}]] ────────────────────────

func (h *handler) developerDashboardAction(w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, "/api/developer/dashboards/")
	parts := strings.SplitN(tail, "/", 3)
	dashID := parts[0]
	ctx := r.Context()

	// Holding the developer role says nothing about whether THIS dashboard
	// is yours — see requireResourceAccess. Widgets are covered by their
	// parent dashboard's scope, which is the same check their own
	// dashboard_widget row would resolve to.
	if !h.requireResourceAccess(w, r, "dashboard", dashID) {
		return
	}

	if len(parts) == 1 {
		switch r.Method {
		case http.MethodPatch:
			var body struct {
				Name string   `json:"name"`
				Tags []string `json:"tags"`
				// RawMessage, not *string: encoding/json can't tell an absent
				// field from an explicit null through a pointer, and all three
				// cases mean different things here — omitted leaves the
				// dashboard where it is, null or "" moves it to the root, an
				// ID files it in that folder.
				FolderID json.RawMessage `json:"folder_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
				jsonErr(w, fmt.Errorf("name required"), http.StatusBadRequest)
				return
			}
			if body.Tags == nil {
				body.Tags = []string{}
			}
			moveFolder, folderTarget := false, ""
			if len(body.FolderID) > 0 {
				moveFolder = true
				if string(body.FolderID) != "null" {
					if err := json.Unmarshal(body.FolderID, &folderTarget); err != nil {
						jsonErr(w, fmt.Errorf("folder_id must be a folder id, an empty string, or null"), http.StatusBadRequest)
						return
					}
				}
			}
			var dashModelID string
			if err := h.db.QueryRow(ctx, `SELECT model_id::text FROM model.dashboard_def WHERE id=$1::uuid`, dashID).Scan(&dashModelID); err != nil {
				jsonErr(w, fmt.Errorf("dashboard not found"), http.StatusNotFound)
				return
			}
			if moveFolder {
				if err := h.validateDashboardFolder(ctx, dashModelID, folderTarget); err != nil {
					jsonErr(w, err, http.StatusBadRequest)
					return
				}
			}
			if _, err := h.db.Exec(ctx,
				`UPDATE model.dashboard_def
				 SET name=$2, tags=$3,
				     folder_id = CASE WHEN $4::boolean THEN NULLIF($5,'')::uuid ELSE folder_id END
				 WHERE id=$1::uuid`,
				dashID, body.Name, body.Tags, moveFolder, folderTarget); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			if a, e := h.resolveActor(ctx, r); e == nil {
				revisionID, appID := h.dashboardScope(ctx, dashID)
				meta := map[string]string{"name": body.Name}
				if moveFolder {
					meta["folder_id"] = folderTarget
				}
				auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
					Category: auditlog.CategoryModelChange, EventType: auditlog.EventDashboardUpdated,
					ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
					ApplicationID: appID, ResourceType: "dashboard", ResourceID: dashID, RevisionID: revisionID,
					Metadata: meta,
				})
			}
			jsonOK(w, map[string]string{"status": "ok"})
		case http.MethodDelete:
			dashRevisionID, dashAppID := h.dashboardScope(ctx, dashID)
			if _, err := h.db.Exec(ctx, `DELETE FROM model.dashboard_def WHERE id=$1::uuid`, dashID); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			if a, e := h.resolveActor(ctx, r); e == nil {
				auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
					Category: auditlog.CategoryModelChange, EventType: auditlog.EventDashboardDeleted,
					ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
					ApplicationID: dashAppID, ResourceType: "dashboard", ResourceID: dashID, RevisionID: dashRevisionID,
				})
			}
			jsonOK(w, map[string]string{"status": "deleted"})
		default:
			jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		}
		return
	}

	if parts[1] != "widgets" {
		jsonErr(w, fmt.Errorf("not found"), http.StatusNotFound)
		return
	}
	widgetID := ""
	if len(parts) == 3 {
		widgetID = parts[2]
	}

	switch {
	case r.Method == http.MethodPost && widgetID == "":
		var body struct {
			WidgetType  string          `json:"widget_type"`
			RefID       *string         `json:"ref_id"`
			Content     *string         `json:"content"`
			SortOrder   int             `json:"sort_order"`
			ColStart    int             `json:"col_start"`
			ColSpan     int             `json:"col_span"`
			PosX        int             `json:"pos_x"`
			PosY        int             `json:"pos_y"`
			SizeW       int             `json:"size_w"`
			SizeH       int             `json:"size_h"`
			WidgetProps json.RawMessage `json:"widget_props"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.WidgetType == "" {
			jsonErr(w, fmt.Errorf("widget_type required"), http.StatusBadRequest)
			return
		}
		if body.SizeW < 20 {
			body.SizeW = 200
		}
		if body.SizeH < 20 {
			body.SizeH = 60
		}
		if err := h.validateWidgetRef(ctx, dashID, body.WidgetType, body.RefID); err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		if err := validateWidgetContent(body.WidgetType, body.Content); err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		var widgetPropsForInsert *string
		if len(body.WidgetProps) > 0 && string(body.WidgetProps) != "null" {
			s := string(body.WidgetProps)
			widgetPropsForInsert = &s
		}
		var newID string
		if err := h.db.QueryRow(ctx,
			`INSERT INTO model.dashboard_widget
			  (dashboard_id, widget_type, ref_id, content, sort_order, col_start, col_span, pos_x, pos_y, size_w, size_h, widget_props)
			 VALUES ($1::uuid,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::jsonb) RETURNING id::text`,
			dashID, body.WidgetType, body.RefID, body.Content,
			body.SortOrder, body.ColStart, body.ColSpan,
			body.PosX, body.PosY, body.SizeW, body.SizeH, widgetPropsForInsert).Scan(&newID); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		// Resolve revision for audit from the parent dashboard
		var dashRevisionID string
		_ = h.db.QueryRow(ctx,
			`SELECT COALESCE(revision_id::text,'') FROM model.dashboard_def WHERE id=$1::uuid`, dashID,
		).Scan(&dashRevisionID)
		act, _ := h.resolveActor(ctx, r)
		actorID, actorRole := "", ""
		if act != nil {
			actorID, actorRole = act.UserID, strings.Join(act.Roles, ",")
		}
		_, widgetAppID := h.dashboardScope(ctx, dashID)
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryModelChange, EventType: auditlog.EventWidgetCreated,
			ActorUserID: actorID, ActorRole: actorRole, ApplicationID: widgetAppID,
			ResourceType: "dashboard_widget", ResourceID: newID, RevisionID: dashRevisionID,
			Metadata: map[string]string{"dashboard_id": dashID, "widget_type": body.WidgetType},
		})
		jsonOK(w, map[string]string{"id": newID, "status": "created"})

	case r.Method == http.MethodPatch && widgetID != "":
		var body struct {
			RefID       *string         `json:"ref_id"`
			Content     *string         `json:"content"`
			Title       *string         `json:"title"`
			ShowTitle   *bool           `json:"show_title"`
			WidgetProps json.RawMessage `json:"widget_props"`
			// Vestiges of a pre-free-canvas column layout — nothing reads
			// them today (widget order/placement come from pos_x/pos_y),
			// and the canvas editor never sends them on a layout save.
			// Pointers + COALESCE so an ordinary drag/resize/property save
			// doesn't silently reset them to 0.
			SortOrder *int `json:"sort_order"`
			ColStart  *int `json:"col_start"`
			ColSpan   *int `json:"col_span"`
			// Pointers: a PATCH that carries only widget_props (or only a
			// title) must leave geometry and content alone. Decoding absent
			// geometry as 0 and clamping it to 20 shrank the widget to
			// 20×20 and blanked its content (found live, 2026-09-11 — a
			// chart collapsed to a few pixels after a props-only update).
			PosX  *int `json:"pos_x"`
			PosY  *int `json:"pos_y"`
			SizeW *int `json:"size_w"`
			SizeH *int `json:"size_h"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
		if body.SizeW != nil && *body.SizeW < 20 {
			*body.SizeW = 20
		}
		if body.SizeH != nil && *body.SizeH < 20 {
			*body.SizeH = 20
		}
		if (body.RefID != nil && *body.RefID != "") || body.Content != nil {
			var existingType string
			if err := h.db.QueryRow(ctx,
				`SELECT widget_type FROM model.dashboard_widget WHERE id=$1::uuid AND dashboard_id=$2::uuid`,
				widgetID, dashID).Scan(&existingType); err != nil {
				jsonErr(w, fmt.Errorf("widget not found on this dashboard"), http.StatusNotFound)
				return
			}
			if body.RefID != nil && *body.RefID != "" {
				if err := h.validateWidgetRef(ctx, dashID, existingType, body.RefID); err != nil {
					jsonErr(w, err, http.StatusBadRequest)
					return
				}
			}
			if err := validateWidgetContent(existingType, body.Content); err != nil {
				jsonErr(w, err, http.StatusBadRequest)
				return
			}
		}
		var showTitle *bool
		if body.ShowTitle != nil {
			showTitle = body.ShowTitle
		}
		var widgetProps *string
		if len(body.WidgetProps) > 0 && string(body.WidgetProps) != "null" {
			s := string(body.WidgetProps)
			widgetProps = &s
		}
		updated, err := h.db.Exec(ctx,
			`UPDATE model.dashboard_widget
			 SET ref_id=COALESCE($2, ref_id), content=COALESCE($3, content),
			     sort_order=COALESCE($4, sort_order), col_start=COALESCE($5, col_start), col_span=COALESCE($6, col_span),
			     pos_x=COALESCE($7, pos_x), pos_y=COALESCE($8, pos_y), size_w=COALESCE($9, size_w), size_h=COALESCE($10, size_h),
			     title=COALESCE($11, title),
			     show_title=COALESCE($12, show_title),
			     widget_props=COALESCE($13::jsonb, widget_props)
			 WHERE id=$1::uuid AND dashboard_id=$14::uuid`,
			widgetID, body.RefID, body.Content, body.SortOrder, body.ColStart, body.ColSpan,
			body.PosX, body.PosY, body.SizeW, body.SizeH,
			body.Title, showTitle, widgetProps, dashID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		// Zero rows means the widget isn't on this dashboard — report that
		// rather than a cheerful "ok" for a write that never happened.
		if updated.RowsAffected() == 0 {
			jsonErr(w, fmt.Errorf("widget not found on this dashboard"), http.StatusNotFound)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			revisionID, appID := h.dashboardScope(ctx, dashID)
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryModelChange, EventType: auditlog.EventWidgetUpdated,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: appID, ResourceType: "dashboard_widget", ResourceID: widgetID, RevisionID: revisionID,
				Metadata: map[string]string{"dashboard_id": dashID},
			})
		}
		jsonOK(w, map[string]string{"status": "ok"})

	case r.Method == http.MethodDelete && widgetID != "":
		revisionID, appID := h.dashboardScope(ctx, dashID)
		// Scoped to the dashboard in the path, which requireResourceAccess
		// above has already authorized. Keying on the widget ID alone let a
		// caller pass their OWN dashboard with someone else's widget ID and
		// mutate it straight through the guard.
		deleted, err := h.db.Exec(ctx, `DELETE FROM model.dashboard_widget WHERE id=$1::uuid AND dashboard_id=$2::uuid`, widgetID, dashID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if deleted.RowsAffected() == 0 {
			jsonErr(w, fmt.Errorf("widget not found on this dashboard"), http.StatusNotFound)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryModelChange, EventType: auditlog.EventWidgetDeleted,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ApplicationID: appID, ResourceType: "dashboard_widget", ResourceID: widgetID, RevisionID: revisionID,
				Metadata: map[string]string{"dashboard_id": dashID},
			})
		}
		jsonOK(w, map[string]string{"status": "deleted"})

	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// ── /api/dashboards  (business-user read) ────────────────────────────────────

func (h *handler) businessDashboards(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	modelID, err := h.resolveDemoModelID(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve model")
		return
	}
	// Resolve the active revision so business users only see dashboards for their revision
	var activeRevisionID string
	_ = h.db.QueryRow(ctx,
		`SELECT COALESCE(active_revision_id::text,'') FROM core.model WHERE id=$1::uuid`, modelID,
	).Scan(&activeRevisionID)

	// Filter by business-role dashboard assignments. Owner-decided
	// semantics (2026-08-30): a user who is a member of NO business role
	// sees every dashboard by default; role membership is what OPTS a user
	// INTO restriction (their roles' grants become their whole world).
	// This replaces the earlier "workspace has roles → unassigned users see
	// nothing" posture, which stranded new users with blank consoles.
	//
	// Business admins (and the developer/admin roles above them) bypass the
	// role filter entirely: they administer the roles and grants themselves,
	// and a business admin with no CFO membership seeing ZERO dashboards was
	// reported live as simply broken.
	adminBypass := a.hasRole("business_admin") || a.hasRole("developer") || a.hasRole("tenant_admin") || a.hasRole("platform_admin")
	roleFilter := func(userParam string) string {
		if adminBypass {
			// Always-true clause that still consumes the user parameter, so
			// both query shapes keep their placeholder/argument counts.
			return ` AND (` + userParam + `::uuid IS NOT NULL)`
		}
		return `
		AND (
		    EXISTS (
		        SELECT 1 FROM identity.business_role_member brm
		        JOIN identity.business_role_dashboard brd ON brd.role_id = brm.role_id
		        WHERE brm.user_id = ` + userParam + `::uuid AND brd.dashboard_id = dd.id
		    )
		    OR NOT EXISTS (
		        SELECT 1 FROM identity.business_role_member brm2
		        JOIN identity.business_role br ON br.id = brm2.role_id
		        JOIN core.workspace w ON w.id = br.workspace_id
		        JOIN core.application app ON app.workspace_id = w.id
		                                  OR (app.workspace_id IS NULL AND app.customer_id = w.customer_id)
		        JOIN core.model m ON m.application_id = app.id
		        WHERE brm2.user_id = ` + userParam + `::uuid AND m.id = dd.model_id
		    )
		)`
	}

	var rows interface {
		Next() bool
		Scan(...any) error
		Close()
	}
	var qErr error
	if activeRevisionID != "" {
		rows, qErr = h.db.Query(ctx,
			`SELECT dd.id::text, dd.name, COALESCE(dd.tags, '{}'), dd.folder_id::text FROM model.dashboard_def dd
			 WHERE dd.model_id=$1::uuid AND dd.revision_id=$2::uuid`+roleFilter("$3")+` ORDER BY dd.name`,
			modelID, activeRevisionID, a.UserID)
	} else {
		rows, qErr = h.db.Query(ctx,
			`SELECT dd.id::text, dd.name, COALESCE(dd.tags, '{}'), dd.folder_id::text FROM model.dashboard_def dd
			 WHERE dd.model_id=$1::uuid`+roleFilter("$2")+` ORDER BY dd.name`,
			modelID, a.UserID)
	}
	if qErr != nil {
		jsonErr(w, qErr, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	type item struct {
		ID       string   `json:"id"`
		Name     string   `json:"name"`
		Tags     []string `json:"tags"`
		FolderID *string  `json:"folder_id"`
	}
	var list []item
	for rows.Next() {
		var it item
		_ = rows.Scan(&it.ID, &it.Name, &it.Tags, &it.FolderID)
		if it.Tags == nil {
			it.Tags = []string{}
		}
		list = append(list, it)
	}
	if list == nil {
		list = []item{}
	}
	jsonOK(w, list)
}

func (h *handler) businessDashboardDetail(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	dashID := strings.TrimPrefix(r.URL.Path, "/api/dashboards/")
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}

	// Tenancy gate first. The business-role check below is deliberately
	// permissive — a workspace with no business roles yet lets everyone
	// through — but that fallback is evaluated against the DASHBOARD's
	// workspace and says nothing about the caller's, so on its own it let
	// any authenticated user of any tenant read any dashboard belonging to
	// a role-less workspace (the default state of every new workspace).
	// businessDashboards never had the hole because it filters dd.model_id
	// to the caller's resolved model; this endpoint and the chart-data one
	// resolve no model at all, so they check the dashboard's model against
	// the caller's access scope explicitly.
	if !h.dashboardModelInScope(ctx, a, dashID) {
		jsonErr(w, fmt.Errorf("dashboard not found"), http.StatusNotFound)
		return
	}

	// Verify the user may access this dashboard: either a business role of
	// theirs includes it, or they are a member of no business role at all
	// (unassigned users see everything — mirrors businessDashboards).
	var allowed bool
	if err := h.db.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1 FROM model.dashboard_def dd
		    WHERE dd.id = $1::uuid
		      AND (
		          EXISTS (
		              SELECT 1 FROM identity.business_role_member brm
		              JOIN identity.business_role_dashboard brd ON brd.role_id = brm.role_id
		              WHERE brm.user_id = $2::uuid AND brd.dashboard_id = dd.id
		          )
		          OR NOT EXISTS (
		              SELECT 1 FROM identity.business_role_member brm2
		              JOIN identity.business_role br ON br.id = brm2.role_id
		              JOIN core.workspace w ON w.id = br.workspace_id
		              JOIN core.application app ON app.workspace_id = w.id
		                                        OR (app.workspace_id IS NULL AND app.customer_id = w.customer_id)
		              JOIN core.model m ON m.application_id = app.id
		              WHERE brm2.user_id = $2::uuid AND m.id = dd.model_id
		          )
		      )
		)
	`, dashID, a.UserID).Scan(&allowed); err != nil || !allowed {
		jsonErr(w, fmt.Errorf("dashboard not found"), http.StatusNotFound)
		return
	}

	var name string
	if err := h.db.QueryRow(ctx, `SELECT name FROM model.dashboard_def WHERE id=$1::uuid`, dashID).Scan(&name); err != nil {
		jsonErr(w, fmt.Errorf("dashboard not found"), http.StatusNotFound)
		return
	}

	wRows, err := h.db.Query(ctx,
		`SELECT id::text, widget_type, ref_id, content, title, show_title, widget_props, sort_order, col_start, col_span, pos_x, pos_y, size_w, size_h FROM model.dashboard_widget WHERE dashboard_id=$1::uuid ORDER BY pos_y, pos_x`, dashID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer wRows.Close()
	var widgets []dashboardWidget
	for wRows.Next() {
		var w dashboardWidget
		_ = wRows.Scan(&w.ID, &w.WidgetType, &w.RefID, &w.Content, &w.Title, &w.ShowTitle, &w.WidgetProps, &w.SortOrder, &w.ColStart, &w.ColSpan, &w.PosX, &w.PosY, &w.SizeW, &w.SizeH)
		widgets = append(widgets, w)
	}
	if widgets == nil {
		widgets = []dashboardWidget{}
	}
	jsonOK(w, dashboardDefResponse{ID: dashID, Name: name, Widgets: widgets})
}

// validateMemberParent checks that a candidate parent_member_id is legal for a member
// being created/updated in dimension dimID:
//   - if dimID has no parent_dimension_id, the candidate must belong to dimID itself
//     (same-dimension hierarchy — this was previously unchecked).
//   - if dimID has a parent_dimension_id, the candidate must belong to that dimension
//     (cross-dimension hierarchy).
func (h *handler) validateMemberParent(ctx context.Context, dimID string, parentMemberID *string) error {
	if parentMemberID == nil {
		return nil
	}
	var parentDimensionID *string
	if err := h.db.QueryRow(ctx, `SELECT parent_dimension_id::text FROM model.dimension_def WHERE id=$1::uuid`, dimID).Scan(&parentDimensionID); err != nil {
		return fmt.Errorf("dimension not found: %w", err)
	}
	expectedDim := dimID
	if parentDimensionID != nil {
		expectedDim = *parentDimensionID
	}
	var candidateDim string
	if err := h.db.QueryRow(ctx, `SELECT dimension_id::text FROM model.dimension_member WHERE id=$1::uuid`, *parentMemberID).Scan(&candidateDim); err != nil {
		return fmt.Errorf("parent member not found: %w", err)
	}
	if candidateDim != expectedDim {
		if parentDimensionID != nil {
			return fmt.Errorf("parent member must belong to the declared parent dimension")
		}
		return fmt.Errorf("parent member must belong to the same dimension")
	}
	return nil
}

// applyInputTimeSummaries rewrites totals for input metrics whose grid has a
// time dimension and whose time_summary is not the plain sum every reader
// assumed before time dimensions existed.
func (h *handler) applyInputTimeSummaries(
	ctx context.Context, modelID, revisionID string,
	allMetrics []metricRow, metricDims map[string][]string, allDims []gridDimension,
	cells, totals map[string]float64, fromStore bool, scopeSubtrees map[string]map[string]bool,
) error {
	timeDims := map[string][]string{} // dim id → member codes in chronological order
	for _, d := range allDims {
		if d.DimensionType != "time" {
			continue
		}
		members := append([]gridDimMember(nil), d.Members...)
		sort.SliceStable(members, func(i, j int) bool {
			ti, tj := 0, 0
			if members[i].TimeIndex != nil {
				ti = *members[i].TimeIndex
			}
			if members[j].TimeIndex != nil {
				tj = *members[j].TimeIndex
			}
			return ti < tj
		})
		for _, m := range members {
			timeDims[d.ID] = append(timeDims[d.ID], m.Code)
		}
	}
	if len(timeDims) == 0 {
		return nil
	}
	var store *calculation.Store
	for _, m := range allMetrics {
		if !m.IsInput || m.TimeSummary == "" || m.TimeSummary == "sum" {
			continue
		}
		ownDims := metricDims[m.ID]
		axisPos := -1
		for i, dimID := range ownDims {
			if _, ok := timeDims[dimID]; ok {
				axisPos = i
			}
		}
		if axisPos < 0 {
			continue
		}
		axisID := ownDims[axisPos]
		perPeriod := map[string]float64{}
		if fromStore {
			if store == nil {
				store = calculation.NewStore(h.db.For(ctx))
			}
			valueMap, err := store.LoadInputValueMap(ctx, modelID, revisionID, m.ID)
			if err != nil {
				return err
			}
			for key, v := range valueMap {
				var dm map[string]string
				if json.Unmarshal([]byte(key), &dm) != nil {
					continue
				}
				inScope := true
				for dimID, sub := range scopeSubtrees {
					if code, pinned := dm[dimID]; pinned && !sub[code] {
						inScope = false
						break
					}
				}
				if !inScope {
					continue
				}
				if code, ok := dm[axisID]; ok {
					perPeriod[code] += v
				}
			}
		} else {
			prefix := m.ID + ":"
			for key, v := range cells {
				if !strings.HasPrefix(key, prefix) {
					continue
				}
				codes := strings.Split(key[len(prefix):], ":")
				if axisPos < len(codes) {
					perPeriod[codes[axisPos]] += v
				}
			}
		}
		var vals []float64
		for _, code := range timeDims[axisID] {
			if v, ok := perPeriod[code]; ok {
				vals = append(vals, v)
			}
		}
		if total, ok := calculation.TimeSummary(m.TimeSummary, vals); ok && len(vals) > 0 {
			totals[m.ID] = total
		} else {
			delete(totals, m.ID)
		}
	}
	return nil
}

// loadScopedSeries prepares every time-series metric of the revision for a
// scoped read: its persisted leaf rows, its time axis and the caller's
// hidden periods on it, and the union window of its dependencies.
func (h *handler) loadScopedSeries(
	ctx context.Context, modelID, revisionID string,
	allDims []gridDimension, allMetrics []metricRow, metricDims map[string][]string, hiddenByDim map[string]map[string]bool,
) (map[string]*scopedSeries, error) {
	timeDims := map[string]gridDimension{}
	for _, d := range allDims {
		if d.DimensionType == "time" {
			timeDims[d.ID] = d
		}
	}
	if len(timeDims) == 0 {
		return nil, nil
	}
	var defs map[string]*calculation.MetricDef
	store := calculation.NewStore(h.db.For(ctx))
	out := map[string]*scopedSeries{}
	for _, m := range allMetrics {
		if m.IsInput || m.Formula == nil || *m.Formula == "" {
			continue
		}
		an, err := formula.Analyze(*m.Formula)
		if err != nil || !an.UsesTimeSeries {
			continue
		}
		var axis *gridDimension
		for _, dimID := range metricDims[m.ID] {
			if d, ok := timeDims[dimID]; ok {
				dd := d
				axis = &dd
			}
		}
		if axis == nil {
			continue // fails at calculation with TIME_CONTEXT_REQUIRED; nothing persisted to serve
		}
		if defs == nil {
			if defs, err = store.LoadModelMetrics(ctx, modelID, revisionID); err != nil {
				return nil, err
			}
		}
		rows, err := store.LoadCalcValueMap(ctx, modelID, revisionID, m.ID)
		if err != nil {
			return nil, err
		}
		ts := &scopedSeries{TimeDimID: axis.ID, TimeSummary: m.TimeSummary, Rows: rows, Hidden: hiddenByDim[axis.ID]}
		if ts.TimeSummary == "" {
			ts.TimeSummary = "sum"
		}
		members := append([]gridDimMember(nil), axis.Members...)
		sort.SliceStable(members, func(i, j int) bool {
			ti, tj := 0, 0
			if members[i].TimeIndex != nil {
				ti = *members[i].TimeIndex
			}
			if members[j].TimeIndex != nil {
				tj = *members[j].TimeIndex
			}
			return ti < tj
		})
		for _, mem := range members {
			ts.Periods = append(ts.Periods, mem.Code)
		}
		if def := defs[m.ID]; def != nil {
			for _, e := range def.DependsOn {
				if e.MinTimeOffset < ts.MinOffset {
					ts.MinOffset = e.MinTimeOffset
				}
				if e.MaxTimeOffset > ts.MaxOffset {
					ts.MaxOffset = e.MaxTimeOffset
				}
				ts.UnbPast = ts.UnbPast || e.UnboundedPast
				ts.UnbFuture = ts.UnbFuture || e.UnboundedFuture
			}
		}
		out[m.ID] = ts
	}
	return out, nil
}

// gridMembershipTx applies one grid_metric / grid_dimension change and
// re-runs the revision's time validation (spec §4.4) in the same
// transaction, so a grid never ends up with two time dimensions or a
// time-series metric off its axis — the change rolls back with the reason.
func (h *handler) gridMembershipTx(ctx context.Context, gridID, sql string, args ...any) error {
	tx, err := h.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	if _, err := tx.Exec(ctx, sql, args...); err != nil {
		return err
	}
	var modelID, revisionID string
	if err := tx.QueryRow(ctx,
		`SELECT model_id::text, COALESCE(revision_id::text,'') FROM model.grid_def WHERE id=$1::uuid`, gridID,
	).Scan(&modelID, &revisionID); err != nil {
		return err
	}
	if err := metricformula.ValidateGridTime(ctx, tx, modelID, revisionID, gridID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// memberPeriod parses and checks the period fields of a member write against
// its dimension's type. On a time dimension a member WITH dates is a leaf
// period and a member without dates is an aggregate period (H1, FY26): a
// grouping whose value is its descendants reduced by the metric's time
// summary. A standard dimension's members carry no dates at all. Returns
// the parsed period, whether the dimension is a time dimension, and whether
// the member is a dated leaf.
func (h *handler) memberPeriod(ctx context.Context, dimID, start, end string) (timedim.Period, bool, bool, error) {
	cfg, err := timedim.LoadConfig(ctx, h.db.For(ctx), dimID)
	if err != nil {
		return timedim.Period{}, false, false, fmt.Errorf("dimension not found")
	}
	if cfg.Type != timedim.TypeTime {
		if start != "" || end != "" {
			return timedim.Period{}, false, false, &timedim.Error{Code: timedim.CodeInvalidTimeMember,
				Message: "period_start/period_end apply only to a time dimension's members"}
		}
		return timedim.Period{}, false, false, nil
	}
	if start == "" && end == "" {
		return timedim.Period{}, true, false, nil // aggregate period
	}
	if start == "" || end == "" {
		return timedim.Period{}, true, true, &timedim.Error{Code: timedim.CodeInvalidTimeMember,
			Message: "a leaf period needs both period_start and period_end (leave both empty for an aggregate period such as H1 or FY26)"}
	}
	ps, err := timedim.ParseDate(start)
	if err != nil {
		return timedim.Period{}, true, true, err
	}
	pe, err := timedim.ParseDate(end)
	if err != nil {
		return timedim.Period{}, true, true, err
	}
	p := timedim.Period{Start: ps, End: pe}
	if err := timedim.ValidatePeriod(cfg, p); err != nil {
		return timedim.Period{}, true, true, err
	}
	return p, true, true, nil
}

// writeTimeMember inserts (memberID == "") or updates one time member —
// a dated leaf period (dated=true) or an aggregate period (no dates, no
// ordinal) — and re-validates + re-indexes the whole dimension in the same
// transaction.
func (h *handler) writeTimeMember(ctx context.Context, dimID, memberID, code, label string, p timedim.Period, dated bool, parentID *string) (string, error) {
	tx, err := h.db.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	var start, end *time.Time
	var idx *int
	if dated {
		start, end = &p.Start, &p.End
		zero := 0
		idx = &zero
	}
	id := memberID
	if memberID == "" {
		if err := tx.QueryRow(ctx, `
			INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end, time_index, parent_member_id)
			VALUES ($1::uuid, $2, $3, $4::date, $5::date, $6, $7::uuid) RETURNING id::text
		`, dimID, code, label, start, end, idx, parentID).Scan(&id); err != nil {
			return "", err
		}
	} else if _, err := tx.Exec(ctx, `
		UPDATE model.dimension_member SET code=$2, label=$3, period_start=$4::date, period_end=$5::date, time_index=$6, parent_member_id=$7::uuid
		WHERE id=$1::uuid AND dimension_id=$8::uuid
	`, memberID, code, label, start, end, idx, parentID, dimID); err != nil {
		return "", err
	}
	if err := timedim.ValidateAndReindex(ctx, tx, dimID); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return id, nil
}

// deleteMemberReindexed deletes a member and, for a time dimension, closes
// the gap in time_index so positions stay dense from zero.
func (h *handler) deleteMemberReindexed(ctx context.Context, dimID, memberID string) error {
	tx, err := h.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	if _, err := tx.Exec(ctx, `DELETE FROM model.dimension_member WHERE id=$1::uuid`, memberID); err != nil {
		return err
	}
	cfg, err := timedim.LoadConfig(ctx, tx, dimID)
	if err != nil {
		return err
	}
	if cfg.Type == timedim.TypeTime {
		// Removing a period may open a gap in a regular calendar; the
		// remaining members still reindex densely so time functions keep a
		// consistent order. The gap itself is reported when the next member
		// is written.
		if _, err := tx.Exec(ctx, `
			WITH ordered AS (
				SELECT id, row_number() OVER (ORDER BY period_start, period_end, code) - 1 AS idx
				FROM model.dimension_member WHERE dimension_id=$1::uuid AND period_start IS NOT NULL
			)
			UPDATE model.dimension_member m SET time_index = o.idx FROM ordered o
			WHERE o.id = m.id AND m.time_index IS DISTINCT FROM o.idx
		`, dimID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// generateTimeMembers is POST /api/developer/dimensions/{id}/members/generate
// {start, end}: contiguous periods of the dimension's granularity, validated
// together with any existing members and indexed by the shared service.
func (h *handler) generateTimeMembers(w http.ResponseWriter, r *http.Request, dimID string) {
	ctx := r.Context()
	var body struct {
		Start string `json:"start"`
		End   string `json:"end"`
		// Optional aggregate period (H1, FY26) the generated leaves go under.
		ParentMemberID *string `json:"parent_member_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
		return
	}
	if body.ParentMemberID != nil {
		if err := h.validateMemberParent(ctx, dimID, body.ParentMemberID); err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
	}
	cfg, err := timedim.LoadConfig(ctx, h.db.For(ctx), dimID)
	if err != nil {
		jsonErr(w, fmt.Errorf("dimension not found"), http.StatusNotFound)
		return
	}
	if cfg.Type != timedim.TypeTime {
		jsonErr(w, fmt.Errorf("periods can only be generated for a time dimension"), http.StatusBadRequest)
		return
	}
	start, err := timedim.ParseDate(body.Start)
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	end, err := timedim.ParseDate(body.End)
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	periods, err := timedim.GeneratePeriods(cfg, start, end)
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	if cid, _ := h.customerOfDimension(ctx, dimID); cid != "" && h.plans != nil {
		if err := h.plans.CheckMembers(ctx, h.db.For(ctx), cid, dimID, len(periods)); err != nil {
			h.jsonLimitErr(w, err)
			return
		}
	}
	tx, err := h.db.Begin(ctx)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck
	created := 0
	for _, p := range periods {
		tag, err := tx.Exec(ctx, `
			INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end, time_index, parent_member_id)
			VALUES ($1::uuid, $2, $3, $4::date, $5::date, 0, $6::uuid)
			ON CONFLICT (dimension_id, code) DO NOTHING
		`, dimID, p.Code, periodLabel(cfg.Granularity, p), p.Start, p.End, body.ParentMemberID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		created += int(tag.RowsAffected())
	}
	if err := timedim.ValidateAndReindex(ctx, tx, dimID); err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	var modelID string
	_ = h.db.QueryRow(ctx, `SELECT model_id::text FROM model.dimension_def WHERE id=$1::uuid`, dimID).Scan(&modelID)
	if modelID != "" && created > 0 {
		go h.recalcAllInputsAcrossRevisions(context.WithoutCancel(ctx), modelID) //nolint:contextcheck
	}
	h.auditDimensionUpdated(ctx, r, dimID, "members_generated", map[string]string{"count": strconv.Itoa(created), "start": body.Start, "end": body.End})
	jsonOK(w, map[string]any{"created": created})
}

// periodLabel is the human label of a generated period.
func periodLabel(granularity string, p timedim.Period) string {
	switch granularity {
	case timedim.GranMonth:
		return p.Start.Format("Jan 2006")
	case timedim.GranDay, timedim.GranWeek:
		return p.Start.Format("2 Jan 2006")
	default:
		return p.Code
	}
}

// memberHasAncestor walks the parent_member_id chain starting at startID (capped at depth
// 20, matching the frontend's recursion guard) and reports whether targetID appears in it.
// Used to reject cycles when re-parenting a member to one of its own descendants.
func (h *handler) memberHasAncestor(ctx context.Context, startID, targetID string) bool {
	cur := startID
	for i := 0; i < 20; i++ {
		if cur == targetID {
			return true
		}
		var next *string
		if err := h.db.QueryRow(ctx, `SELECT parent_member_id::text FROM model.dimension_member WHERE id=$1::uuid`, cur).Scan(&next); err != nil || next == nil {
			return false
		}
		cur = *next
	}
	return false
}

// dimensionHasAncestor walks the parent_dimension_id chain starting at startID (capped at
// depth 20) and reports whether targetID appears in it. Used to reject cycles when
// re-parenting a dimension to one of its own descendant dimensions.
func (h *handler) dimensionHasAncestor(ctx context.Context, startID, targetID string) bool {
	cur := startID
	for i := 0; i < 20; i++ {
		if cur == targetID {
			return true
		}
		var next *string
		if err := h.db.QueryRow(ctx, `SELECT parent_dimension_id::text FROM model.dimension_def WHERE id=$1::uuid`, cur).Scan(&next); err != nil || next == nil {
			return false
		}
		cur = *next
	}
	return false
}

// recalcAfterDimChange triggers RecalcAffected for every (revision, input-metric) pair that
// was touched by a dimension-member change, ensuring calc results stay consistent.
func (h *handler) recalcAfterDimChange(ctx context.Context, modelID string, affected []struct{ RevisionID, MetricID string }) {
	if len(affected) == 0 {
		return
	}
	// Group metric IDs by revision so we make one RecalcAffected call per revision.
	byRev := make(map[string][]string)
	for _, a := range affected {
		byRev[a.RevisionID] = append(byRev[a.RevisionID], a.MetricID)
	}
	calcStore := calculation.NewStore(h.db.For(ctx))
	sched := calculation.NewScheduler(h.log, calcStore, nil)
	for revID, metricIDs := range byRev {
		if err := sched.RecalcAffected(ctx, modelID, revID, metricIDs); err != nil {
			h.log.Warn().Err(err).Str("model", modelID).Str("revision", revID).
				Msg("recalc after dim change failed")
		}
	}
}

// recalcAllInputsAcrossRevisions recalculates every calculated metric
// downstream of any input, for every revision with fact data — the same
// blunt-but-correct "recalc everything" pattern developerMetricAction's own
// PATCH case already uses after a formula edit. Used for changes (like
// dimension-member deletion) that can affect an aggregate calc metric's
// value without touching model.calc_dependency at all, so there's no
// specific affected-metric list to target precisely.
func (h *handler) recalcAllInputsAcrossRevisions(ctx context.Context, modelID string) {
	inputRows, _ := h.db.Query(ctx,
		`SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid AND is_input=true`, modelID)
	var inputIDs []string
	for inputRows.Next() {
		var id string
		_ = inputRows.Scan(&id)
		inputIDs = append(inputIDs, id)
	}
	inputRows.Close()
	if len(inputIDs) == 0 {
		return
	}

	revRows, _ := h.db.Query(ctx,
		`SELECT DISTINCT revision_id::text FROM runtime.fact_input WHERE model_id=$1::uuid AND revision_id IS NOT NULL`, modelID)
	var revIDs []string
	for revRows.Next() {
		var rid string
		_ = revRows.Scan(&rid)
		revIDs = append(revIDs, rid)
	}
	revRows.Close()

	calcStore := calculation.NewStore(h.db.For(ctx))
	sched := calculation.NewScheduler(h.log, calcStore, nil)
	for _, rid := range revIDs {
		if err := sched.RecalcAffected(ctx, modelID, rid, inputIDs); err != nil {
			h.log.Warn().Err(err).Str("model", modelID).Str("revision", rid).
				Msg("recalc all inputs failed")
		}
	}
}

// splitParentFactData moves existing fact_input rows from a parent member to its first child
// when that parent gains its very first child. The parent's stored values transfer in full
// to the sole child so the aggregate (rollup) remains identical.
// Returns the distinct (revision_id, metric_id) pairs that were affected so the caller
// can trigger recalculation.
func (h *handler) splitParentFactData(ctx context.Context, dimID, parentMemberID, childCode, modelID string) ([]struct{ RevisionID, MetricID string }, error) {
	var parentCode string
	if err := h.db.QueryRow(ctx,
		`SELECT code FROM model.dimension_member WHERE id=$1::uuid`, parentMemberID,
	).Scan(&parentCode); err != nil {
		return nil, err
	}

	filterBytes, _ := json.Marshal(map[string]string{dimID: parentCode})
	parentFilter := string(filterBytes)

	tx, err := h.db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	_, err = tx.Exec(ctx, `
		INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value, entered_by)
		SELECT model_id, revision_id, metric_id,
		       jsonb_set(dim_members, ARRAY[$1::text], to_jsonb($2::text)),
		       value::float8,
		       entered_by
		FROM (
			SELECT DISTINCT ON (revision_id, metric_id, dim_members)
			    model_id, revision_id, metric_id, dim_members, value, entered_by
			FROM runtime.fact_input
			WHERE model_id=$3::uuid AND dim_members @> $4::jsonb
			ORDER BY revision_id, metric_id, dim_members, entered_at DESC, id DESC
		) latest
	`, dimID, childCode, modelID, parentFilter)
	if err != nil {
		return nil, err
	}

	// Collect (revision_id, metric_id) pairs before deleting so the caller can recalc.
	affRows, err := tx.Query(ctx, `
		SELECT DISTINCT revision_id::text, metric_id::text
		FROM runtime.fact_input
		WHERE model_id=$1::uuid AND dim_members @> $2::jsonb AND revision_id IS NOT NULL
	`, modelID, parentFilter)
	if err != nil {
		return nil, err
	}
	var affected []struct{ RevisionID, MetricID string }
	for affRows.Next() {
		var r, m string
		if scanErr := affRows.Scan(&r, &m); scanErr == nil {
			affected = append(affected, struct{ RevisionID, MetricID string }{r, m})
		}
	}
	affRows.Close()

	_, _ = tx.Exec(ctx, `SET LOCAL mvx.delete_reason = 'member_reparented'`)
	_, err = tx.Exec(ctx, `
		DELETE FROM runtime.fact_input
		WHERE model_id=$1::uuid AND dim_members @> $2::jsonb
	`, modelID, parentFilter)
	if err != nil {
		return nil, err
	}

	return affected, tx.Commit(ctx)
}

// ── Business-Admin: helpers ────────────────────────────────────────────────────

// baWorkspaceModel resolves (workspaceID, modelID) for the acting business admin.
func (h *handler) baWorkspaceModel(ctx context.Context, r *http.Request) (wsID, modelID string, err error) {
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		return "", "", err
	}
	if appID, _ := ctx.Value(appIDCtxKey).(string); appID != "" {
		canAccessApp, err := h.actorCanAccessApp(ctx, act, appID)
		if err != nil {
			return "", "", err
		}
		if !canAccessApp {
			return "", "", errAccessDenied
		}
		var appWorkspaceID *string
		var customerID string
		if err := h.db.QueryRow(ctx, `
			SELECT app.workspace_id::text, app.customer_id::text, m.id::text
			FROM core.application app
			JOIN core.model m ON m.application_id = app.id
			WHERE app.id = $1::uuid LIMIT 1
		`, appID).Scan(&appWorkspaceID, &customerID, &modelID); err != nil {
			return "", "", err
		}
		if appWorkspaceID != nil {
			return *appWorkspaceID, modelID, nil
		}
		// App has no legacy workspace_id — it was created via
		// POST /api/admin/applications, which scopes an app directly to a
		// customer (migration 030_flatten_hierarchy.sql), not a workspace.
		// identity.business_role/role_assignment are still workspace-scoped,
		// so resolve one via the customer instead: prefer a workspace the
		// actor already holds a role in, else the customer's oldest
		// ("Default", auto-created at tenant creation) workspace.
		if err := h.db.QueryRow(ctx, `
			SELECT ws.id::text FROM core.workspace ws
			JOIN identity.role_assignment ra ON ra.workspace_id = ws.id
			WHERE ws.customer_id = $1::uuid AND ra.user_id = $2::uuid
			ORDER BY ws.created_at LIMIT 1
		`, customerID, act.UserID).Scan(&wsID); err != nil {
			if err := h.db.QueryRow(ctx, `
				SELECT id::text FROM core.workspace WHERE customer_id=$1::uuid ORDER BY created_at LIMIT 1
			`, customerID).Scan(&wsID); err != nil {
				return "", modelID, fmt.Errorf("no workspace for customer: %w", err)
			}
		}
		return wsID, modelID, nil
	}
	// Prefer the workspace where the actor holds business_admin, so Dana's
	// admin operations always land in Finance & Budget rather than Finance.
	err = h.db.QueryRow(ctx,
		`SELECT workspace_id::text FROM identity.role_assignment
		 WHERE user_id=$1::uuid AND workspace_id IS NOT NULL AND role='business_admin'
		 LIMIT 1`, act.UserID,
	).Scan(&wsID)
	if err != nil {
		// Fallback: any workspace with a role assignment
		err = h.db.QueryRow(ctx,
			`SELECT workspace_id::text FROM identity.role_assignment
			 WHERE user_id=$1::uuid AND workspace_id IS NOT NULL LIMIT 1`, act.UserID,
		).Scan(&wsID)
		if err != nil {
			return "", "", fmt.Errorf("no workspace for actor: %w", err)
		}
	}
	err = h.db.QueryRow(ctx,
		`SELECT m.id::text FROM core.model m
		 JOIN core.application app ON app.id = m.application_id
		 WHERE app.workspace_id=$1::uuid LIMIT 1`, wsID,
	).Scan(&modelID)
	if err != nil {
		return wsID, "", fmt.Errorf("no model for workspace: %w", err)
	}
	return wsID, modelID, nil
}

// ── Business-Admin: GET /api/business-admin/available ─────────────────────────

func (h *handler) baAvailable(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	_, modelID, err := h.baWorkspaceModel(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve workspace model")
		return
	}
	kind := r.URL.Query().Get("type") // dashboards | metrics | dimensions

	type item struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Group string `json:"group,omitempty"`
	}

	switch kind {
	case "dimension_members":
		// Filtered by the calling admin's OWN identity.user_access_rule —
		// this picker configures OTHER users' access rules, but was itself
		// unfiltered, the same publicDimensions gap fixed earlier this
		// session, never carried over here.
		dimRules := map[string]string{}
		{
			arRows, arErr := h.db.Query(ctx,
				`SELECT ref_id, access FROM identity.user_access_rule WHERE user_id=$1::uuid AND rule_type='dimension_member'`, act.UserID)
			if arErr == nil {
				for arRows.Next() {
					var refID, access string
					if arRows.Scan(&refID, &access) == nil {
						dimRules[refID] = access
					}
				}
				arRows.Close()
			}
		}
		if len(dimRules) > 0 {
			allRows, aErr := h.db.Query(ctx, `
				SELECT m.id::text, m.dimension_id::text, COALESCE(m.parent_member_id::text,'')
				FROM model.dimension_member m JOIN model.dimension_def d ON d.id = m.dimension_id
				WHERE d.model_id = $1::uuid`, modelID)
			if aErr == nil {
				edges := make([]writeguard.MemberEdge, 0, 256)
				for allRows.Next() {
					var id, dimID, parentID string
					if allRows.Scan(&id, &dimID, &parentID) == nil {
						edges = append(edges, writeguard.MemberEdge{ID: id, ParentID: parentID, DimID: dimID})
					}
				}
				allRows.Close()
				for id := range writeguard.ExpandHidden(edges, dimRules) {
					dimRules[id] = "hidden"
				}
			}
		}

		rows, err := h.db.Query(ctx, `
			SELECT dm.id::text, dm.label, dd.name
			FROM model.dimension_member dm
			JOIN model.dimension_def dd ON dd.id = dm.dimension_id
			JOIN core.model m ON m.id = dd.model_id
			WHERE dd.model_id=$1::uuid
			  AND dd.revision_id = m.active_revision_id
			ORDER BY dd.name, dm.sort_order, dm.label
		`, modelID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		var out []item
		for rows.Next() {
			var it item
			_ = rows.Scan(&it.ID, &it.Name, &it.Group)
			if dimRules[it.ID] == "hidden" {
				continue
			}
			out = append(out, it)
		}
		if out == nil {
			out = []item{}
		}
		jsonOK(w, out)

	case "dashboards":
		rows, err := h.db.Query(ctx, `
			SELECT dd.id::text, dd.name, COALESCE(array_to_string(dd.tags, ', '), '')
			FROM model.dashboard_def dd
			JOIN core.model m ON m.id = dd.model_id
			WHERE dd.model_id=$1::uuid
			  AND dd.revision_id = m.active_revision_id
			ORDER BY dd.name`, modelID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		var out []item
		for rows.Next() {
			var it item
			_ = rows.Scan(&it.ID, &it.Name, &it.Group)
			out = append(out, it)
		}
		if out == nil {
			out = []item{}
		}
		jsonOK(w, out)

	case "metrics":
		metricRules := map[string]string{}
		arRows, arErr := h.db.Query(ctx,
			`SELECT ref_id, access FROM identity.user_access_rule WHERE user_id=$1::uuid AND rule_type='metric'`, act.UserID)
		if arErr == nil {
			for arRows.Next() {
				var refID, access string
				if arRows.Scan(&refID, &access) == nil {
					metricRules[refID] = access
				}
			}
			arRows.Close()
		}

		rows, err := h.db.Query(ctx, `
			SELECT md.id::text, md.name
			FROM model.metric_def md
			JOIN core.model m ON m.id = md.model_id
			WHERE md.model_id=$1::uuid
			  AND md.revision_id = m.active_revision_id
			ORDER BY md.name
		`, modelID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		var out []item
		for rows.Next() {
			var it item
			_ = rows.Scan(&it.ID, &it.Name)
			if metricRules[it.ID] == "hidden" {
				continue
			}
			out = append(out, it)
		}
		if out == nil {
			out = []item{}
		}
		jsonOK(w, out)

	case "dimensions":
		rows, err := h.db.Query(ctx,
			`SELECT id::text, name FROM model.dimension_def WHERE model_id=$1::uuid ORDER BY name`, modelID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		var out []item
		for rows.Next() {
			var it item
			_ = rows.Scan(&it.ID, &it.Name)
			out = append(out, it)
		}
		if out == nil {
			out = []item{}
		}
		jsonOK(w, out)

	default:
		jsonErr(w, fmt.Errorf("type must be dashboards|metrics|dimensions|dimension_members"), http.StatusBadRequest)
	}
}

// ── Business-Admin: /api/business-admin/roles ─────────────────────────────────

type baRoleRow struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	MemberCount  int      `json:"member_count"`
	DashboardIDs []string `json:"dashboard_ids"`
}

func (h *handler) baRoles(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	wsID, _, err := h.baWorkspaceModel(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve workspace model")
		return
	}

	switch r.Method {
	case http.MethodGet:
		rows, err := h.db.Query(ctx, `
			SELECT br.id::text, br.name,
			       COUNT(DISTINCT brm.user_id) AS member_count,
			       COALESCE(array_agg(brd.dashboard_id::text) FILTER (WHERE brd.dashboard_id IS NOT NULL), '{}') AS dashboard_ids
			FROM identity.business_role br
			LEFT JOIN identity.business_role_member brm ON brm.role_id = br.id
			LEFT JOIN identity.business_role_dashboard brd ON brd.role_id = br.id
			WHERE br.workspace_id=$1::uuid
			GROUP BY br.id, br.name ORDER BY br.name
		`, wsID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		var out []baRoleRow
		for rows.Next() {
			var role baRoleRow
			_ = rows.Scan(&role.ID, &role.Name, &role.MemberCount, &role.DashboardIDs)
			out = append(out, role)
		}
		if out == nil {
			out = []baRoleRow{}
		}
		jsonOK(w, out)

	case http.MethodPost:
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
			jsonErr(w, fmt.Errorf("name required"), http.StatusBadRequest)
			return
		}
		var newID string
		err := h.db.QueryRow(ctx,
			`INSERT INTO identity.business_role (workspace_id, name) VALUES ($1::uuid, $2) RETURNING id::text`,
			wsID, body.Name).Scan(&newID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryAdmin, EventType: auditlog.EventRoleCreated,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ResourceType: "business_role", ResourceID: newID,
				Metadata: map[string]string{"name": body.Name, "workspace_id": wsID},
			})
		}
		jsonOK(w, map[string]string{"id": newID})

	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// ── Business-Admin: /api/business-admin/roles/{id}[/dashboards|/members[/{userId}]] ──

func (h *handler) baRoleAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Parse path: /api/business-admin/roles/{id}[/subResource[/subID]]
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/business-admin/roles/")
	parts := strings.SplitN(trimmed, "/", 3)
	roleID := parts[0]
	subResource := ""
	subID := ""
	if len(parts) >= 2 {
		subResource = parts[1]
	}
	if len(parts) >= 3 {
		subID = parts[2]
	}

	// Scope to the caller's workspace — unlike baRoles/baUsers (which call
	// baWorkspaceModel to list only their own workspace's data), this
	// handler used to take roleID straight from the URL with no check it
	// belongs to the caller's workspace, letting a business_admin in one
	// workspace read/rewrite another workspace's business roles by
	// guessing a UUID.
	wsID, _, err := h.baWorkspaceModel(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve workspace model")
		return
	}
	var roleWsID string
	if err := h.db.QueryRow(ctx,
		`SELECT workspace_id::text FROM identity.business_role WHERE id=$1::uuid`, roleID,
	).Scan(&roleWsID); err != nil {
		jsonErr(w, fmt.Errorf("business role not found"), http.StatusNotFound)
		return
	}
	if roleWsID != wsID {
		jsonErr(w, fmt.Errorf("forbidden: business role is outside your workspace"), http.StatusForbidden)
		return
	}

	switch subResource {
	case "":
		// PATCH or DELETE /api/business-admin/roles/{id}
		switch r.Method {
		case http.MethodPatch:
			var body struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
				jsonErr(w, fmt.Errorf("name required"), http.StatusBadRequest)
				return
			}
			if _, err := h.db.Exec(ctx,
				`UPDATE identity.business_role SET name=$2 WHERE id=$1::uuid`, roleID, body.Name); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			if a, e := h.resolveActor(ctx, r); e == nil {
				auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
					Category: auditlog.CategoryAdmin, EventType: auditlog.EventRoleUpdated,
					ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
					ResourceType: "business_role", ResourceID: roleID,
					Metadata: map[string]string{"name": body.Name},
				})
			}
			jsonOK(w, map[string]string{"status": "ok"})

		case http.MethodDelete:
			if _, err := h.db.Exec(ctx,
				`DELETE FROM identity.business_role WHERE id=$1::uuid`, roleID); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			if a, e := h.resolveActor(ctx, r); e == nil {
				auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
					Category: auditlog.CategoryAdmin, EventType: auditlog.EventRoleDeleted,
					ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
					ResourceType: "business_role", ResourceID: roleID,
				})
			}
			jsonOK(w, map[string]string{"status": "deleted"})

		default:
			jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		}

	case "dashboards":
		switch r.Method {
		case http.MethodGet:
			rows, err := h.db.Query(ctx,
				`SELECT dashboard_id::text FROM identity.business_role_dashboard WHERE role_id=$1::uuid`, roleID)
			if err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			defer rows.Close()
			var ids []string
			for rows.Next() {
				var id string
				_ = rows.Scan(&id)
				ids = append(ids, id)
			}
			if ids == nil {
				ids = []string{}
			}
			jsonOK(w, ids)

		case http.MethodPut:
			// Replace full dashboard list
			var body struct {
				DashboardIDs []string `json:"dashboard_ids"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
				return
			}
			tx, err := h.db.Begin(ctx)
			if err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			defer tx.Rollback(ctx) //nolint:errcheck
			if _, err := tx.Exec(ctx,
				`DELETE FROM identity.business_role_dashboard WHERE role_id=$1::uuid`, roleID); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			for _, did := range body.DashboardIDs {
				// Every ID must belong to a dashboard in THIS role's own
				// workspace. Without the check any existing dashboard UUID
				// was accepted, so a business admin could grant their own
				// role another customer's dashboard — and the dashboard
				// detail/chart-data endpoints honour a grant on sight.
				// The workspace join mirrors the one businessDashboards
				// uses, including the NULL-workspace_id app shape.
				var inWorkspace bool
				if err := tx.QueryRow(ctx, `
					SELECT EXISTS (
					    SELECT 1 FROM model.dashboard_def dd
					    JOIN core.model m ON m.id = dd.model_id
					    JOIN core.application app ON app.id = m.application_id
					    JOIN core.workspace w ON w.id = app.workspace_id
					                          OR (app.workspace_id IS NULL AND app.customer_id = w.customer_id)
					    WHERE dd.id = $1::uuid AND w.id = $2::uuid
					)
				`, did, wsID).Scan(&inWorkspace); err != nil {
					jsonErr(w, err, http.StatusInternalServerError)
					return
				}
				if !inWorkspace {
					jsonErr(w, fmt.Errorf("dashboard %s is not in this workspace", did), http.StatusForbidden)
					return
				}
				if _, err := tx.Exec(ctx,
					`INSERT INTO identity.business_role_dashboard (role_id, dashboard_id) VALUES ($1::uuid,$2::uuid) ON CONFLICT DO NOTHING`,
					roleID, did); err != nil {
					jsonErr(w, err, http.StatusInternalServerError)
					return
				}
			}
			if err := tx.Commit(ctx); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			if a, e := h.resolveActor(ctx, r); e == nil {
				auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
					Category: auditlog.CategoryAdmin, EventType: auditlog.EventRoleDashboardsUpdated,
					ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
					ResourceType: "business_role", ResourceID: roleID,
					Metadata: map[string]string{"dashboard_count": strconv.Itoa(len(body.DashboardIDs))},
				})
			}
			jsonOK(w, map[string]string{"status": "ok"})

		default:
			jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		}

	case "members":
		switch {
		case r.Method == http.MethodGet && subID == "":
			type memberRow struct {
				UserID      string `json:"user_id"`
				DisplayName string `json:"display_name"`
				Email       string `json:"email"`
			}
			rows, err := h.db.Query(ctx, `
				SELECT u.id::text, u.display_name, u.email
				FROM identity.business_role_member brm
				JOIN identity.user u ON u.id = brm.user_id
				WHERE brm.role_id=$1::uuid ORDER BY u.display_name
			`, roleID)
			if err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			defer rows.Close()
			var out []memberRow
			for rows.Next() {
				var m memberRow
				_ = rows.Scan(&m.UserID, &m.DisplayName, &m.Email)
				out = append(out, m)
			}
			if out == nil {
				out = []memberRow{}
			}
			jsonOK(w, out)

		case r.Method == http.MethodPost && subID == "":
			var body struct {
				UserID string `json:"user_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.UserID == "" {
				jsonErr(w, fmt.Errorf("user_id required"), http.StatusBadRequest)
				return
			}
			// The user must actually belong to this role's workspace.
			// Membership is what grants dashboard visibility, so accepting
			// any user UUID let a business admin hand another tenant's user
			// a seat in their workspace's roles. Mirrors baUsers' own
			// "who is in my workspace" query (a role_assignment there),
			// widened to any role rather than business_user only, since a
			// developer or business_admin of the same workspace is equally
			// legitimate in a business role.
			var userInWorkspace bool
			if err := h.db.QueryRow(ctx, `
				SELECT EXISTS (
				    SELECT 1 FROM identity.role_assignment ra
				    WHERE ra.user_id = $1::uuid AND ra.workspace_id = $2::uuid
				)
			`, body.UserID, wsID).Scan(&userInWorkspace); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			if !userInWorkspace {
				jsonErr(w, fmt.Errorf("user is not a member of this workspace"), http.StatusForbidden)
				return
			}
			if _, err := h.db.Exec(ctx,
				`INSERT INTO identity.business_role_member (role_id, user_id) VALUES ($1::uuid,$2::uuid) ON CONFLICT DO NOTHING`,
				roleID, body.UserID); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			if a, e := h.resolveActor(ctx, r); e == nil {
				auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
					Category: auditlog.CategoryAdmin, EventType: auditlog.EventRoleMemberAdded,
					ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
					ResourceType: "business_role", ResourceID: roleID,
					Metadata: map[string]string{"user_id": body.UserID},
				})
			}
			jsonOK(w, map[string]string{"status": "ok"})

		case r.Method == http.MethodDelete && subID != "":
			if _, err := h.db.Exec(ctx,
				`DELETE FROM identity.business_role_member WHERE role_id=$1::uuid AND user_id=$2::uuid`,
				roleID, subID); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			if a, e := h.resolveActor(ctx, r); e == nil {
				auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
					Category: auditlog.CategoryAdmin, EventType: auditlog.EventRoleMemberRemoved,
					ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
					ResourceType: "business_role", ResourceID: roleID,
					Metadata: map[string]string{"user_id": subID},
				})
			}
			jsonOK(w, map[string]string{"status": "deleted"})

		default:
			jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		}

	default:
		jsonErr(w, fmt.Errorf("unknown sub-resource"), http.StatusNotFound)
	}
}

// ── Business-Admin: GET /api/business-admin/users ─────────────────────────────

func (h *handler) baUsers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	wsID, _, err := h.baWorkspaceModel(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve workspace model")
		return
	}

	type userRow struct {
		ID          string `json:"id"`
		DisplayName string `json:"display_name"`
		Email       string `json:"email"`
		Role        string `json:"role"`
	}
	rows, err := h.db.Query(ctx, `
		SELECT DISTINCT ON (u.id) u.id::text, u.display_name, u.email, ra.role::text
		FROM identity.user u
		JOIN identity.role_assignment ra ON ra.user_id = u.id
		WHERE ra.workspace_id=$1::uuid AND ra.role IN ('business_user','business_admin')
		ORDER BY u.id, ra.role
	`, wsID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	var out []userRow
	for rows.Next() {
		var u userRow
		_ = rows.Scan(&u.ID, &u.DisplayName, &u.Email, &u.Role)
		out = append(out, u)
	}
	if out == nil {
		out = []userRow{}
	}
	jsonOK(w, out)
}

// ── Business-Admin: /api/business-admin/users/{id}/access-rules ───────────────

func (h *handler) baUserAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Path: /api/business-admin/users/{id}/access-rules
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/business-admin/users/")
	parts := strings.SplitN(trimmed, "/", 2)
	userID := parts[0]

	// Scope to the caller's workspace, same reasoning as baRoleAction —
	// unlike baUsers (which lists only ra.workspace_id=$1 users), this
	// handler used to take userID straight from the URL with no check the
	// target user even belongs to the caller's workspace, letting a
	// business_admin in one workspace read/overwrite another workspace's
	// user access rules by guessing a UUID.
	wsID, _, err := h.baWorkspaceModel(ctx, r)
	if err != nil {
		jsonAccessErr(w, err, "resolve workspace model")
		return
	}
	var inWorkspace bool
	if err := h.db.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM identity.role_assignment WHERE user_id=$1::uuid AND workspace_id=$2::uuid)`,
		userID, wsID,
	).Scan(&inWorkspace); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !inWorkspace {
		jsonErr(w, fmt.Errorf("forbidden: user is outside your workspace"), http.StatusForbidden)
		return
	}

	type ruleRow struct {
		RuleType string `json:"rule_type"`
		RefID    string `json:"ref_id"`
		RefName  string `json:"ref_name"`
		Access   string `json:"access"`
	}

	switch r.Method {
	case http.MethodGet:
		rows, err := h.db.Query(ctx,
			`SELECT rule_type, ref_id, '', access FROM identity.user_access_rule
			 WHERE user_id=$1::uuid ORDER BY rule_type, ref_id`, userID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		defer rows.Close()
		var out []ruleRow
		for rows.Next() {
			var rule ruleRow
			_ = rows.Scan(&rule.RuleType, &rule.RefID, &rule.RefName, &rule.Access)
			out = append(out, rule)
		}
		if out == nil {
			out = []ruleRow{}
		}
		jsonOK(w, out)

	case http.MethodPut:
		var body struct {
			Rules []struct {
				RuleType string `json:"rule_type"`
				RefID    string `json:"ref_id"`
				Access   string `json:"access"`
			} `json:"rules"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
		tx, err := h.db.Begin(ctx)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if _, err := tx.Exec(ctx,
			`DELETE FROM identity.user_access_rule WHERE user_id=$1::uuid`, userID); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		for _, rule := range body.Rules {
			if _, err := tx.Exec(ctx,
				`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access)
				 VALUES ($1::uuid, $2, $3, $4)`,
				userID, rule.RuleType, rule.RefID, rule.Access); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
		}
		if err := tx.Commit(ctx); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if a, e := h.resolveActor(ctx, r); e == nil {
			auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
				Category: auditlog.CategoryAdmin, EventType: auditlog.EventUserAccessRulesUpdated,
				ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
				ResourceType: "identity_user", ResourceID: userID,
			})
		}
		jsonOK(w, map[string]string{"status": "ok"})

	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// ── Developer Workflows ───────────────────────────────────────────────────────

func (h *handler) developerWorkflows(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	appID := r.URL.Query().Get("application_id")
	if appID == "" {
		jsonErr(w, fmt.Errorf("application_id required"), http.StatusBadRequest)
		return
	}
	revisionID := h.resolveAppRevisionID(ctx, r, appID)

	ws := workflow.NewStore(h.db.For(ctx))

	switch r.Method {
	case http.MethodGet:
		list, err := ws.ListWorkflowDefs(ctx, appID, revisionID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if list == nil {
			list = []*workflow.WorkflowDefSummary{}
		}
		jsonOK(w, list)

	case http.MethodPost:
		var body struct {
			Name         string `json:"name"`
			Description  string `json:"description"`
			TriggerEvent string `json:"trigger_event"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		if body.Name == "" {
			jsonErr(w, fmt.Errorf("name required"), http.StatusBadRequest)
			return
		}
		if body.TriggerEvent == "" {
			body.TriggerEvent = "manual"
		}
		def, err := ws.CreateWorkflowDefFull(ctx, appID, revisionID, body.Name, body.Description, body.TriggerEvent, a.UserID)
		if errors.Is(err, workflow.ErrWorkflowDefNameTaken) {
			jsonErr(w, err, http.StatusConflict)
			return
		}
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryModelChange, EventType: auditlog.EventWorkflowDefCreated,
			ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
			ApplicationID: def.ApplicationID, ResourceType: "workflow_def", ResourceID: def.ID,
			Metadata: map[string]string{"name": body.Name},
		})
		jsonOK(w, def)

	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

func (h *handler) developerWorkflowAction(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}

	// Path: /api/developer/workflows/{id}[/action]
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/developer/workflows/"), "/")
	defID := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	// Workflow definitions are application-scoped, so this resolves through
	// the app rather than a model.
	if !h.requireResourceAccess(w, r, "workflow_def", defID) {
		return
	}

	if defID == "" {
		jsonErr(w, fmt.Errorf("workflow id required"), http.StatusBadRequest)
		return
	}

	ws := workflow.NewStore(h.db.For(ctx))

	switch action {
	case "validate":
		if r.Method != http.MethodPost {
			jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
			return
		}
		def, err := ws.GetWorkflowDefFull(ctx, defID)
		if err != nil {
			jsonErr(w, err, http.StatusNotFound)
			return
		}
		// An optional body carries the editor's UNSAVED draft: validate that
		// instead of the stored definition, so "Validate" never has to save
		// behind the user's back (it used to persist a steps-only PATCH first).
		if r.ContentLength != 0 {
			var draft struct {
				Name          *string         `json:"name"`
				Steps         json.RawMessage `json:"steps"`
				ContextSchema json.RawMessage `json:"context_schema"`
			}
			if err := json.NewDecoder(r.Body).Decode(&draft); err != nil && err != io.EOF {
				jsonErr(w, fmt.Errorf("invalid draft: %w", err), http.StatusBadRequest)
				return
			}
			if draft.Name != nil {
				def.Name = *draft.Name
			}
			if len(draft.Steps) > 0 {
				def.Steps = draft.Steps
			}
			if len(draft.ContextSchema) > 0 {
				def.ContextSchema = draft.ContextSchema
			}
		}
		verrs := workflow.ValidateDef(def)
		jsonOK(w, map[string]any{"valid": len(verrs) == 0, "errors": verrs})
		return

	case "publish":
		if r.Method != http.MethodPost {
			jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
			return
		}
		def, err := ws.GetWorkflowDefFull(ctx, defID)
		if err != nil {
			jsonErr(w, err, http.StatusNotFound)
			return
		}
		if verrs := workflow.ValidateDef(def); len(verrs) > 0 {
			jsonErr(w, fmt.Errorf("workflow has validation errors: %s", strings.Join(verrs, "; ")), http.StatusBadRequest)
			return
		}
		published, err := ws.PublishWorkflowDef(ctx, defID, a.UserID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryModelChange, EventType: auditlog.EventWorkflowPublished,
			ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
			ApplicationID: published.ApplicationID, ResourceType: "workflow_def", ResourceID: defID,
		})
		jsonOK(w, published)
		return

	case "archive":
		if r.Method != http.MethodPost {
			jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
			return
		}
		archived, err := ws.ArchiveWorkflowDef(ctx, defID, a.UserID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryModelChange, EventType: auditlog.EventWorkflowArchived,
			ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
			ApplicationID: archived.ApplicationID, ResourceType: "workflow_def", ResourceID: defID,
		})
		jsonOK(w, archived)
		return

	case "restore":
		// Archive used to be a one-way door: the list offered an archived
		// workflow nothing but "Duplicate" (found live, 2026-09-10). Restore
		// puts it back into draft so it can be edited and published again.
		if r.Method != http.MethodPost {
			jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
			return
		}
		restored, err := ws.RestoreWorkflowDef(ctx, defID, a.UserID)
		if err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryModelChange, EventType: auditlog.EventWorkflowDefUpdated,
			ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
			ApplicationID: restored.ApplicationID, ResourceType: "workflow_def", ResourceID: defID,
			Metadata: map[string]string{"action": "restored_from_archive"},
		})
		jsonOK(w, restored)
		return

	case "duplicate":
		if r.Method != http.MethodPost {
			jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Name == "" {
			body.Name = "Copy"
		}
		dup, err := ws.DuplicateWorkflowDef(ctx, defID, body.Name, a.UserID)
		if errors.Is(err, workflow.ErrWorkflowDefNameTaken) {
			jsonErr(w, err, http.StatusConflict)
			return
		}
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryModelChange, EventType: auditlog.EventWorkflowDefCreated,
			ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
			ApplicationID: dup.ApplicationID, ResourceType: "workflow_def", ResourceID: dup.ID,
			Metadata: map[string]string{"name": body.Name, "duplicated_from": defID},
		})
		jsonOK(w, dup)
		return

	case "usage":
		if r.Method != http.MethodGet {
			jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
			return
		}
		usage, err := ws.GetWorkflowDefUsage(ctx, defID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if usage == nil {
			usage = []*workflow.WorkflowDefUsage{}
		}
		jsonOK(w, usage)
		return

	case "instances":
		if r.Method != http.MethodGet {
			jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
			return
		}
		instances, err := ws.ListWorkflowInstancesByDef(ctx, defID, 20)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if instances == nil {
			instances = []*workflow.Execution{}
		}
		jsonOK(w, instances)
		return

	case "test-run":
		if r.Method != http.MethodPost {
			jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
			return
		}
		var body struct {
			Context map[string]string `json:"context"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Context == nil {
			body.Context = map[string]string{}
		}
		// A real execution mode on the instance, not a context flag: this used
		// to set context["_test_mode"]="true", which nothing read, so a test
		// run sent real notifications and wrote real facts.
		instance, err := ws.StartTestRun(ctx, defID, a.UserID, body.Context)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		// Report where the run actually got to, so the designer can see which
		// step it stopped on rather than just "running".
		inst, steps, ferr := ws.GetWorkflowInstance(ctx, instance.Id)
		resp := map[string]any{"instance_id": instance.Id, "status": "running", "test_run": true}
		if ferr == nil {
			resp["status"] = strings.ToLower(strings.TrimPrefix(inst.Status.String(), "WORKFLOW_STATUS_"))
			for _, st := range steps {
				if st.Status == workflowv1.StepStatus_STEP_STATUS_IN_PROGRESS {
					resp["current_step_id"] = st.StepDefId
					resp["current_step_status"] = "in_progress"
					break
				}
			}
		}
		jsonOK(w, resp)
		return
	}

	// Base resource: GET / PATCH / DELETE
	switch r.Method {
	case http.MethodGet:
		def, err := ws.GetWorkflowDefFull(ctx, defID)
		if err != nil {
			jsonErr(w, err, http.StatusNotFound)
			return
		}
		jsonOK(w, def)

	case http.MethodPatch:
		// Every field is optional: a PATCH that carries only `steps` must
		// leave name, description, trigger_event and subject_type as they
		// are. They used to be overwritten with "" (the editor's Validate
		// sent a steps-only PATCH and silently wiped the workflow's name,
		// trigger and subject — found live, 2026-09-10).
		var body struct {
			Name          *string         `json:"name"`
			Description   *string         `json:"description"`
			TriggerEvent  *string         `json:"trigger_event"`
			SubjectType   *string         `json:"subject_type"`
			SubjectConfig json.RawMessage `json:"subject_config"`
			// Per-definition dedup choice; absent = unchanged.
			SingleActiveInstance *bool           `json:"single_active_instance"`
			Steps                json.RawMessage `json:"steps"`
			ContextSchema        json.RawMessage `json:"context_schema"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		current, err := ws.GetWorkflowDefFull(ctx, defID)
		if err != nil {
			jsonErr(w, err, http.StatusNotFound)
			return
		}
		keep := func(p *string, cur string) string {
			if p == nil {
				return cur
			}
			return *p
		}
		updated, err := ws.UpdateWorkflowDefFull(ctx, defID,
			keep(body.Name, current.Name), keep(body.Description, current.Description),
			keep(body.TriggerEvent, current.TriggerEvent), keep(body.SubjectType, current.SubjectType),
			a.UserID, body.Steps, body.ContextSchema, body.SubjectConfig)
		if err != nil {
			if errors.Is(err, workflow.ErrWorkflowDefNotEditable) || errors.Is(err, workflow.ErrWorkflowDefNameTaken) {
				jsonErr(w, err, http.StatusConflict)
				return
			}
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if body.SingleActiveInstance != nil && *body.SingleActiveInstance != updated.SingleActiveInstance {
			if err := ws.SetWorkflowDefSingleActiveInstance(ctx, defID, *body.SingleActiveInstance); err != nil {
				jsonErr(w, err, http.StatusInternalServerError)
				return
			}
			updated.SingleActiveInstance = *body.SingleActiveInstance
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryModelChange, EventType: auditlog.EventWorkflowDefUpdated,
			ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
			ApplicationID: updated.ApplicationID, ResourceType: "workflow_def", ResourceID: defID,
			Metadata: map[string]string{"name": updated.Name},
		})
		jsonOK(w, updated)

	case http.MethodDelete:
		var deletedAppID string
		_ = h.db.QueryRow(ctx, `SELECT application_id::text FROM workflow.workflow_def WHERE id=$1::uuid`, defID).Scan(&deletedAppID)
		if err := ws.DeleteWorkflowDef(ctx, defID); err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryModelChange, EventType: auditlog.EventWorkflowDefDeleted,
			ActorUserID: a.UserID, ActorRole: strings.Join(a.Roles, ","),
			ApplicationID: deletedAppID, ResourceType: "workflow_def", ResourceID: defID,
		})
		jsonOK(w, map[string]string{"status": "deleted"})

	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// ── /api/developer/workflow-trigger-events ────────────────────────────────────

// TriggerPayloadField describes one context variable produced by a trigger event.
type TriggerPayloadField struct {
	Key      string `json:"key"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
}

// TriggerEventCatalogItem is one entry in the trigger event catalog.
type TriggerEventCatalogItem struct {
	Key           string                `json:"key"`
	Label         string                `json:"label"`
	Description   string                `json:"description"`
	Category      string                `json:"category"`
	SourceType    string                `json:"source_type"`
	SourceID      string                `json:"source_id,omitempty"`
	SourceName    string                `json:"source_name,omitempty"`
	PayloadSchema []TriggerPayloadField `json:"payload_schema"`
	Enabled       bool                  `json:"enabled"`
	CreatedFrom   string                `json:"created_from"`
}

func formFieldTypeToPayloadType(t string) string {
	switch t {
	case "number":
		return "number"
	case "boolean":
		return "boolean"
	case "date":
		return "date"
	case "dimension":
		return "dimension_member"
	case "metric":
		return "metric"
	default:
		return "text"
	}
}

func toEventKey(name string) string {
	return strings.ToLower(strings.NewReplacer(" ", "_", "-", "_").Replace(name))
}

// buildWorkflowTriggerEventCatalog assembles the trigger event catalog for an
// application by combining static system events with form- and integration-
// derived events read from the database. revisionID (when non-empty) limits
// form/integration events to that revision plus revision-global rows.
func buildWorkflowTriggerEventCatalog(ctx context.Context, pool *pgxpool.Pool, appID, revisionID string) ([]TriggerEventCatalogItem, error) {
	// Resolve model (may be absent for brand-new applications).
	var modelID string
	_ = pool.QueryRow(ctx, `
		SELECT m.id::text FROM core.model m WHERE m.application_id = $1::uuid
		ORDER BY (SELECT COUNT(*) FROM model.metric_def WHERE model_id = m.id) DESC,
		         m.created_at DESC LIMIT 1
	`, appID).Scan(&modelID)

	catalog := []TriggerEventCatalogItem{
		{
			Key:         "manual",
			Label:       "Manual",
			Description: "Started manually by a user or via an automation rule.",
			Category:    "manual",
			SourceType:  "system",
			PayloadSchema: []TriggerPayloadField{
				{Key: "started_by_user_id", Type: "user", Required: true},
			},
			Enabled:     true,
			CreatedFrom: "system",
		},
		{
			Key:         "api.workflow.start",
			Label:       "API trigger",
			Description: "Started via a direct API call to the workflow start endpoint.",
			Category:    "api",
			SourceType:  "system",
			PayloadSchema: []TriggerPayloadField{
				{Key: "started_by_user_id", Type: "user", Required: true},
			},
			Enabled:     true,
			CreatedFrom: "system",
		},
	}

	if modelID != "" {
		// ── Form-generated events ──────────────────────────────────────────────
		catalog = append(catalog, TriggerEventCatalogItem{
			Key:         "form.submit",
			Label:       "Any form submitted",
			Description: "Starts when any form record in this application is submitted.",
			Category:    "form",
			SourceType:  "system",
			PayloadSchema: []TriggerPayloadField{
				{Key: "record_id", Type: "form_record", Required: true},
				{Key: "submitted_by_user_id", Type: "user", Required: true},
				{Key: "form_id", Type: "text", Required: true},
			},
			Enabled:     true,
			CreatedFrom: "system",
		})

		formRows, err := pool.Query(ctx, `
			SELECT id::text, name, label, COALESCE(fields, '[]'::jsonb)
			FROM model.form_def
			WHERE model_id = $1::uuid
			  AND ($2 = '' OR revision_id IS NULL OR revision_id::text = $2)
			ORDER BY label
		`, modelID, revisionID)
		if err == nil {
			defer formRows.Close()
			for formRows.Next() {
				var fid, fname, flabel string
				var fieldsRaw []byte
				_ = formRows.Scan(&fid, &fname, &flabel, &fieldsRaw)

				var fields []struct {
					Name     string `json:"name"`
					Type     string `json:"type"`
					Required bool   `json:"required"`
				}
				_ = json.Unmarshal(fieldsRaw, &fields)

				payload := []TriggerPayloadField{
					{Key: "record_id", Type: "form_record", Required: true},
					{Key: "submitted_by_user_id", Type: "user", Required: true},
				}
				for _, f := range fields {
					payload = append(payload, TriggerPayloadField{
						Key:      f.Name,
						Type:     formFieldTypeToPayloadType(f.Type),
						Required: f.Required,
					})
				}

				eventKey := toEventKey(fname) + ".submitted"
				catalog = append(catalog, TriggerEventCatalogItem{
					Key:           eventKey,
					Label:         flabel + " submitted",
					Description:   "Starts when a " + flabel + " form record is submitted.",
					Category:      "form",
					SourceType:    "form",
					SourceID:      fid,
					SourceName:    flabel,
					PayloadSchema: payload,
					Enabled:       true,
					CreatedFrom:   "form",
				})
			}
		}

		// ── Integration-generated events ───────────────────────────────────────
		catalog = append(catalog, TriggerEventCatalogItem{
			Key:         "integration.import.completed",
			Label:       "Any import completed",
			Description: "Starts when any integration import finishes successfully.",
			Category:    "integration",
			SourceType:  "system",
			PayloadSchema: []TriggerPayloadField{
				{Key: "integration_id", Type: "text", Required: true},
				{Key: "record_count", Type: "number", Required: true},
			},
			Enabled:     true,
			CreatedFrom: "system",
		})

		intRows, err := pool.Query(ctx, `
			SELECT id::text, name FROM model.integration_def
			WHERE model_id = $1::uuid
			  AND ($2 = '' OR revision_id IS NULL OR revision_id::text = $2)
			ORDER BY name
		`, modelID, revisionID)
		if err == nil {
			defer intRows.Close()
			for intRows.Next() {
				var iid, iname string
				_ = intRows.Scan(&iid, &iname)

				ikey := toEventKey(iname)
				completedPayload := []TriggerPayloadField{
					{Key: "integration_id", Type: "text", Required: true},
					{Key: "record_count", Type: "number", Required: true},
					{Key: "source_file", Type: "text", Required: false},
				}
				failedPayload := []TriggerPayloadField{
					{Key: "integration_id", Type: "text", Required: true},
					{Key: "error_message", Type: "text", Required: true},
					{Key: "source_file", Type: "text", Required: false},
				}

				catalog = append(catalog,
					TriggerEventCatalogItem{
						Key:           ikey + ".import.completed",
						Label:         iname + " import completed",
						Description:   "Starts when a " + iname + " import finishes successfully.",
						Category:      "integration",
						SourceType:    "integration",
						SourceID:      iid,
						SourceName:    iname,
						PayloadSchema: completedPayload,
						Enabled:       true,
						CreatedFrom:   "integration",
					},
					TriggerEventCatalogItem{
						Key:           ikey + ".import.failed",
						Label:         iname + " import failed",
						Description:   "Starts when a " + iname + " import fails.",
						Category:      "integration",
						SourceType:    "integration",
						SourceID:      iid,
						SourceName:    iname,
						PayloadSchema: failedPayload,
						Enabled:       true,
						CreatedFrom:   "integration",
					},
				)
			}
		}

		// Planning events (budget.submitted, forecast.submitted,
		// grid.review_requested) were listed here as static placeholders
		// for months while nothing ever dispatched them — a rule bound to
		// one never fired, silently. They return when something emits them.
	}

	return catalog, nil
}

func (h *handler) developerWorkflowTriggerEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()

	appID := r.URL.Query().Get("application_id")
	if appID == "" {
		var err error
		appID, err = h.resolveDemoAppID(ctx, r)
		if err != nil {
			jsonAccessErr(w, err, "resolve app")
			return
		}
	} else {
		act, err := h.resolveActor(ctx, r)
		if err != nil {
			jsonErr(w, err, http.StatusUnauthorized)
			return
		}
		canAccessApp, err := h.actorCanAccessApp(ctx, act, appID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if !canAccessApp {
			jsonErr(w, fmt.Errorf("forbidden: app is outside your access scope"), http.StatusForbidden)
			return
		}
	}

	catalog, err := buildWorkflowTriggerEventCatalog(ctx, h.db.For(ctx), appID, h.resolveAppRevisionID(ctx, r, appID))
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, catalog)
}

// developerWorkflowRoles returns the business roles defined in the workspace
// that contains the given application. These are the roles selectable as
// approver/assignee in workflow step definitions.
func (h *handler) developerWorkflowRoles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()

	appID := r.URL.Query().Get("application_id")
	if appID == "" {
		jsonErr(w, fmt.Errorf("application_id required"), http.StatusBadRequest)
		return
	}
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, err, http.StatusUnauthorized)
		return
	}
	canAccessApp, err := h.actorCanAccessApp(ctx, act, appID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !canAccessApp {
		jsonErr(w, fmt.Errorf("forbidden: app is outside your access scope"), http.StatusForbidden)
		return
	}

	type roleItem struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}

	rows, err := h.db.Query(ctx, `
		SELECT br.id::text, br.name
		FROM identity.business_role br
		JOIN core.workspace ws ON ws.id = br.workspace_id
		JOIN core.application app ON (app.workspace_id = ws.id OR app.customer_id = ws.customer_id)
		WHERE app.id = $1::uuid
		ORDER BY br.name
	`, appID)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var out []roleItem
	for rows.Next() {
		var role roleItem
		if err := rows.Scan(&role.ID, &role.Name); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		out = append(out, role)
	}
	if out == nil {
		out = []roleItem{}
	}
	jsonOK(w, out)
}

// ── /api/dashboard-widgets/{widgetId}/chart-data ─────────────────────────────

func (h *handler) dashboardWidgetAction(w http.ResponseWriter, r *http.Request) {
	tail := strings.TrimPrefix(r.URL.Path, "/api/dashboard-widgets/")
	parts := strings.SplitN(tail, "/", 2)
	if len(parts) != 2 || parts[1] != "chart-data" {
		jsonErr(w, fmt.Errorf("not found"), http.StatusNotFound)
		return
	}
	widgetID := parts[0]
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()

	// Authenticate
	a, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}

	// Load widget
	var dashID, widgetType string
	var refID *string
	var widgetPropsRaw []byte
	if err := h.db.QueryRow(ctx, `
		SELECT w.dashboard_id::text, w.widget_type, w.ref_id, w.widget_props
		FROM model.dashboard_widget w
		WHERE w.id = $1::uuid
	`, widgetID).Scan(&dashID, &widgetType, &refID, &widgetPropsRaw); err != nil {
		jsonErr(w, fmt.Errorf("widget not found"), http.StatusNotFound)
		return
	}

	if widgetType != "chart" {
		jsonErr(w, fmt.Errorf("widget is not a chart"), http.StatusBadRequest)
		return
	}
	if refID == nil || *refID == "" {
		jsonErr(w, fmt.Errorf("chart widget has no grid source"), http.StatusBadRequest)
		return
	}

	// Tenancy gate, same reasoning as businessDashboardDetail's: the
	// role fallback below is scoped to the DASHBOARD's workspace, so
	// without this a role-less workspace's chart data (real numbers, not
	// just widget metadata) is readable by any authenticated user of any
	// tenant.
	if !h.dashboardModelInScope(ctx, a, dashID) {
		jsonErr(w, fmt.Errorf("dashboard not accessible"), http.StatusForbidden)
		return
	}

	// Verify dashboard access — same semantics as businessDashboards/
	// folders/businessDashboardDetail (owner-decided 2026-08-30): admin
	// roles see everything; a user in NO business role sees everything;
	// role membership restricts you to your roles' grants. This endpoint
	// was the fourth copy of the check and kept the OLD rule after the
	// other three changed — a business admin's dashboards then listed fine
	// while every chart widget 403'd in a retry loop ("doesn't load
	// properly", reported live with the exact console trace). The tenancy
	// gate above (dashboardModelInScope) still bounds all of it.
	adminBypass := a.hasRole("business_admin") || a.hasRole("developer") || a.hasRole("tenant_admin") || a.hasRole("platform_admin")
	if !adminBypass {
		var allowed bool
		if err := h.db.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM model.dashboard_def dd
				WHERE dd.id = $1::uuid
				  AND (
				      EXISTS (
				          SELECT 1 FROM identity.business_role_member brm
				          JOIN identity.business_role_dashboard brd ON brd.role_id = brm.role_id
				          WHERE brm.user_id = $2::uuid AND brd.dashboard_id = dd.id
				      )
				      OR NOT EXISTS (
				          SELECT 1 FROM identity.business_role_member brm2
				          JOIN identity.business_role br ON br.id = brm2.role_id
				          JOIN core.workspace w ON w.id = br.workspace_id
				          JOIN core.application app ON app.workspace_id = w.id
				                                    OR (app.workspace_id IS NULL AND app.customer_id = w.customer_id)
				          JOIN core.model m ON m.application_id = app.id
				          WHERE brm2.user_id = $2::uuid AND m.id = dd.model_id
				      )
				  )
			)
		`, dashID, a.UserID).Scan(&allowed); err != nil || !allowed {
			jsonErr(w, fmt.Errorf("dashboard not accessible"), http.StatusForbidden)
			return
		}
	}

	// Parse widget_props.chart
	if len(widgetPropsRaw) == 0 {
		jsonErr(w, fmt.Errorf("chart configuration missing"), http.StatusBadRequest)
		return
	}
	var widgetProps struct {
		Chart *query.ChartConfig `json:"chart"`
	}
	if err := json.Unmarshal(widgetPropsRaw, &widgetProps); err != nil || widgetProps.Chart == nil {
		jsonErr(w, fmt.Errorf("chart configuration malformed"), http.StatusBadRequest)
		return
	}
	cfg := widgetProps.Chart

	// Validate chart config basics
	if cfg.DimensionID == "" {
		jsonErr(w, fmt.Errorf("chart dimension_id missing"), http.StatusBadRequest)
		return
	}
	switch cfg.ChartType {
	case query.ChartBar, query.ChartLine:
		if len(cfg.MetricIDs) < 1 || len(cfg.MetricIDs) > 5 {
			jsonErr(w, fmt.Errorf("bar/line charts require 1–5 metrics"), http.StatusBadRequest)
			return
		}
	case query.ChartPie, query.ChartHistogram:
		if len(cfg.MetricIDs) != 1 {
			jsonErr(w, fmt.Errorf("pie/histogram charts require exactly 1 metric"), http.StatusBadRequest)
			return
		}
	case query.ChartScatter:
		if cfg.XMetricID == "" || cfg.YMetricID == "" {
			jsonErr(w, fmt.Errorf("scatter charts require x_metric_id and y_metric_id"), http.StatusBadRequest)
			return
		}
		if cfg.XMetricID == cfg.YMetricID {
			jsonErr(w, fmt.Errorf("scatter X and Y metrics must be different"), http.StatusBadRequest)
			return
		}
	default:
		jsonErr(w, fmt.Errorf("unsupported chart type: %s", cfg.ChartType), http.StatusBadRequest)
		return
	}
	if cfg.BinCount != 0 && (cfg.BinCount < 3 || cfg.BinCount > 30) {
		jsonErr(w, fmt.Errorf("bin_count must be between 3 and 30"), http.StatusBadRequest)
		return
	}

	// Decode runtime context override from request body
	var req struct {
		Context map[string]string `json:"context"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		req.Context = map[string]string{}
	}
	if req.Context == nil {
		req.Context = map[string]string{}
	}

	// Resolve model ID and active revision from the grid def — but only
	// after confirming the grid belongs to the same model as the dashboard
	// we just authorized. ref_id has no foreign key and predates
	// validateWidgetRef, so a row written before that check (or by any
	// future path that forgets it) could still name another tenant's grid,
	// and everything below would then resolve against THAT model.
	var modelID string
	if err := h.db.QueryRow(ctx,
		`SELECT g.model_id::text
		 FROM model.grid_def g
		 JOIN model.dashboard_def d ON d.model_id = g.model_id
		 WHERE g.id=$1::uuid AND d.id=$2::uuid`, *refID, dashID,
	).Scan(&modelID); err != nil {
		jsonErr(w, fmt.Errorf("grid source not found"), http.StatusBadRequest)
		return
	}

	// The chart is resolved in ITS grid's revision. A dashboard is designed
	// and previewed in a specific revision, not necessarily the active one;
	// resolving the viewer's active revision here and then redirecting the
	// grid to that revision's same-named grid broke every chart outside the
	// active revision with "plotted dimension not found in grid" — the
	// config's dimension/metric IDs are the designed revision's, the
	// redirected grid's are another's (found live, 2026-09-11, "Sales
	// Overview"). Only a legacy revision-global grid still falls back to the
	// dashboard's revision, then to the viewer's.
	gridDefID := *refID
	var revisionID string
	_ = h.db.QueryRow(ctx,
		`SELECT COALESCE(revision_id::text,'') FROM model.grid_def WHERE id=$1::uuid`, gridDefID,
	).Scan(&revisionID)
	if revisionID == "" {
		revisionID, _ = h.dashboardScope(ctx, dashID)
	}
	if revisionID == "" {
		var err error
		revisionID, _, err = h.resolveRevisionCtx(ctx, "", modelID)
		if err != nil {
			jsonErr(w, fmt.Errorf("no active revision"), http.StatusInternalServerError)
			return
		}
	}

	resolver := query.NewChartResolver(h.db.For(ctx))
	result, err := resolver.Resolve(ctx, cfg, req.Context, modelID, revisionID, gridDefID, a.UserID)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "not accessible") || strings.Contains(msg, "hidden") {
			jsonErr(w, err, http.StatusForbidden)
			return
		}
		jsonErr(w, err, http.StatusBadRequest)
		return
	}

	jsonOK(w, result)
}

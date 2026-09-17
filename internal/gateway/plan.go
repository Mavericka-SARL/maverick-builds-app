package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/internal/plan"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// Plans and trials (internal/plan) as the gateway applies them.
//
// Two mechanisms, both keyed on the tenant a request acts on:
//
//   - planGuard, a middleware ahead of every route, refuses mutating
//     requests from a tenant that is read-only — its trial ended, or the
//     usage sweep found it over a limit — with 402 and the reason. Platform
//     administrators are never refused: they are who fixes it.
//   - the CheckX calls in the creation handlers refuse the one creation that
//     would cross a limit, so a person is told "the Trial plan allows 3
//     models" at the moment they try, not by a sweep minutes later.
//
// The tenant of a request is the routed tenant in dedicated mode, else the
// application named by X-App-Id, else the actor's own tenant. Nothing found
// (a platform-level developer, an admin acting across tenants) means no
// plan applies.

// SignupConfig is what public sign-up needs from the deployment.
type SignupConfig struct {
	// Enabled opens POST /api/signup. Off by default: a self-hosted
	// deployment must not grow a public registration page by accident.
	Enabled bool
	// ContactURL is where "change the plan" leads — a pricing page or a
	// mailto:. Shown with every plan limit and trial notice.
	ContactURL string
}

// actorCtxKey caches the actor planGuard resolved, so the handler's own
// resolveActor is a map lookup rather than a second query.
const actorCtxKey ctxKey = iota + 100

// requestCustomerID is the tenant a request acts on, or "" when none does.
func (h *handler) requestCustomerID(ctx context.Context, r *http.Request, act *actor) string {
	if id := tenantdb.TenantFrom(ctx); id != "" {
		return id
	}
	if appID, _ := ctx.Value(appIDCtxKey).(string); appID != "" {
		if id := h.customerOfApplication(ctx, appID); id != "" {
			return id
		}
	}
	if act != nil {
		return act.CustomerID
	}
	return ""
}

// The owner lookups below answer "which tenant does this object belong to"
// from a small cache: the same application or model is asked about on
// every request of a session.
const ownerCacheTTL = 5 * time.Minute

type ownerEntry struct {
	customerID string
	expires    time.Time
}

func (h *handler) cachedOwner(kind, id string, lookup func() string) string {
	key := kind + ":" + id
	now := time.Now()
	h.ownerMu.Lock()
	if e, ok := h.owners[key]; ok && now.Before(e.expires) {
		h.ownerMu.Unlock()
		return e.customerID
	}
	h.ownerMu.Unlock()
	cid := lookup()
	if cid == "" {
		return ""
	}
	h.ownerMu.Lock()
	if h.owners == nil || len(h.owners) > 20000 {
		h.owners = map[string]ownerEntry{}
	}
	h.owners[key] = ownerEntry{customerID: cid, expires: now.Add(ownerCacheTTL)}
	h.ownerMu.Unlock()
	return cid
}

// customerOfApplication is the tenant an application belongs to, directly
// or through its workspace.
func (h *handler) customerOfApplication(ctx context.Context, appID string) string {
	return h.cachedOwner("app", appID, func() string {
		var cid string
		_ = h.db.QueryRow(ctx, `
			SELECT COALESCE(a.customer_id::text, w.customer_id::text, '')
			FROM core.application a LEFT JOIN core.workspace w ON w.id = a.workspace_id
			WHERE a.id = $1::uuid`, appID).Scan(&cid)
		return cid
	})
}

// customerOfModel is the tenant a model belongs to.
func (h *handler) customerOfModel(ctx context.Context, modelID string) string {
	return h.cachedOwner("model", modelID, func() string {
		var cid string
		_ = h.db.QueryRow(ctx, `
			SELECT COALESCE(a.customer_id::text, w.customer_id::text, '')
			FROM core.model m JOIN core.application a ON a.id = m.application_id
			LEFT JOIN core.workspace w ON w.id = a.workspace_id
			WHERE m.id = $1::uuid`, modelID).Scan(&cid)
		return cid
	})
}

// customerOfDimension is the tenant a dimension's model belongs to, with
// the model id alongside (member checks need both).
func (h *handler) customerOfDimension(ctx context.Context, dimensionID string) (customerID, modelID string) {
	_ = h.db.QueryRow(ctx, `SELECT model_id::text FROM model.dimension_def WHERE id = $1::uuid`, dimensionID).Scan(&modelID)
	if modelID == "" {
		return "", ""
	}
	return h.customerOfModel(ctx, modelID), modelID
}

// customerOfWorkspace is the tenant a workspace belongs to.
func (h *handler) customerOfWorkspace(ctx context.Context, workspaceID string) string {
	if workspaceID == "" {
		return ""
	}
	return h.cachedOwner("ws", workspaceID, func() string {
		var cid string
		_ = h.db.QueryRow(ctx, `SELECT customer_id::text FROM core.workspace WHERE id = $1::uuid`, workspaceID).Scan(&cid)
		return cid
	})
}

func isMutation(method string) bool {
	switch method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	}
	return false
}

// planGuard refuses mutating requests from a read-only tenant. Paths that
// have no signed-in actor (sign-up, SCIM's token auth) pass through: their
// handlers authenticate themselves, and SCIM's user creation meets the
// user limit inside the service.
func (h *handler) planGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h.plans == nil || !isMutation(r.Method) || !strings.HasPrefix(r.URL.Path, "/api/") ||
			strings.HasPrefix(r.URL.Path, "/api/signup") || strings.HasPrefix(r.URL.Path, "/api/scim/") {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		act, err := h.resolveActor(ctx, r)
		if err != nil {
			next.ServeHTTP(w, r) // the handler answers 401 with its own message
			return
		}
		r = r.WithContext(context.WithValue(ctx, actorCtxKey, act))
		if !act.hasRole("platform_admin") {
			if cid := h.requestCustomerID(ctx, r, act); cid != "" {
				st, err := h.plans.State(ctx, h.db.For(ctx), cid)
				if err != nil {
					h.log.Warn().Err(err).Str("tenant", cid).Msg("plan state unavailable — request allowed")
				} else if st.ReadOnly && (st.Code != plan.CodeOverLimit || r.Method != http.MethodDelete) {
					h.jsonReadOnly(w, st)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// jsonReadOnly answers a refused mutation on a read-only tenant.
func (h *handler) jsonReadOnly(w http.ResponseWriter, st plan.State) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": st.Reason, "code": st.Code, "plan": st.Plan.Key, "contact_url": h.signupCfg.ContactURL,
	})
}

// jsonLimitErr answers a creation the plan refused (402 with the limit),
// or any other error as 500.
func (h *handler) jsonLimitErr(w http.ResponseWriter, err error) {
	var le *plan.LimitError
	if errors.As(err, &le) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": le.Error(), "code": "plan_limit", "limit": le.Limit, "max": le.Max, "current": le.Current,
			"plan": le.Plan, "contact_url": h.signupCfg.ContactURL,
		})
		return
	}
	jsonErr(w, err, http.StatusInternalServerError)
}

// planStateFor is the state /api/me and the tenant listing report; nil when
// no tenant applies or the row cannot be read.
func (h *handler) planStateFor(ctx context.Context, customerID string) *plan.State {
	if h.plans == nil || customerID == "" {
		return nil
	}
	st, err := h.plans.State(ctx, h.db.For(ctx), customerID)
	if err != nil {
		return nil
	}
	return &st
}

// meResponse is /api/me: the actor plus, for a member of a tenant, what
// their plan means right now.
type meResponse struct {
	*actor
	Plan       *plan.State `json:"plan,omitempty"`
	ContactURL string      `json:"contact_url,omitempty"`
}

// ── administration ───────────────────────────────────────────────────────────

// adminPlans serves GET /api/admin/plans: the catalog, for the platform
// administrator to edit and for a tenant admin to see what an upgrade is.
func (h *handler) adminPlans(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.requireRole(w, r, "platform_admin", "tenant_admin"); !ok {
		return
	}
	plans, err := plan.List(r.Context(), h.db.Control())
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if plans == nil {
		plans = []plan.Plan{}
	}
	jsonOK(w, plans)
}

// adminPlanAction serves PUT /api/admin/plans/{key}: create or change a
// plan (platform administrator). Every tenant on the plan feels the new
// limits within a minute; the read-only verdict follows at the next sweep.
func (h *handler) adminPlanAction(w http.ResponseWriter, r *http.Request) {
	act, ok := h.requireRole(w, r, "platform_admin")
	if !ok {
		return
	}
	if r.Method != http.MethodPut {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/api/admin/plans/")
	var body plan.Plan
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
		return
	}
	body.Key = key
	ctx := r.Context()
	saved, err := plan.Upsert(ctx, h.db.Control(), body)
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	h.plans.InvalidateAll()
	limits, _ := json.Marshal(saved.Limits)
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryAdmin, EventType: auditlog.EventPlanUpdated,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ResourceType: "plan", ResourceID: saved.Key,
		Metadata: map[string]string{"name": saved.Name, "trial_days": fmt.Sprint(saved.TrialDays),
			"self_service": fmt.Sprint(saved.SelfService), "limits": string(limits)},
	})
	jsonOK(w, saved)
}

// applyTenantPlan handles the plan half of PATCH /api/admin/tenants/{id}:
// a new plan key, a trial end (RFC 3339, or "" to end the trial), or both.
// Returns what changed for the audit row.
func (h *handler) applyTenantPlan(ctx, tctx context.Context, customerID string, planKey *string, trialEndsAt *string) (map[string]string, error) {
	cur, err := plan.LoadTenant(tctx, h.db.For(tctx), customerID)
	if err != nil {
		return nil, err
	}
	key, ends := cur.Plan, cur.TrialEndsAt
	changed := map[string]string{}
	if planKey != nil && *planKey != cur.Plan {
		p, err := plan.Get(ctx, h.db.Control(), *planKey)
		if err != nil {
			return nil, err
		}
		key = p.Key
		changed["plan"] = p.Key
		// A move onto a trial plan starts its clock; a move off one ends
		// the trial unless the request says otherwise.
		if p.TrialDays > 0 {
			t := time.Now().Add(time.Duration(p.TrialDays) * 24 * time.Hour)
			ends = &t
		} else {
			ends = nil
		}
	}
	if trialEndsAt != nil {
		if *trialEndsAt == "" {
			ends = nil
			changed["trial_ends_at"] = ""
		} else {
			t, err := time.Parse(time.RFC3339, *trialEndsAt)
			if err != nil {
				return nil, fmt.Errorf("trial_ends_at must be an RFC 3339 timestamp")
			}
			ends = &t
			changed["trial_ends_at"] = t.UTC().Format(time.RFC3339)
		}
	}
	if len(changed) == 0 {
		return changed, nil
	}
	if err := plan.SetPlan(tctx, h.db.For(tctx), customerID, key, ends); err != nil {
		return nil, err
	}
	if router := h.db.Router(); router != nil {
		if t, gErr := router.Catalog().Get(ctx, customerID); gErr == nil {
			_ = router.Catalog().Rename(ctx, customerID, t.Name, key)
		}
	}
	h.plans.Invalidate(customerID)
	return changed, nil
}

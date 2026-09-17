package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mavericks-engine/mavericks/ee/sso"
	"github.com/mavericks-engine/mavericks/internal/identity"
	"github.com/mavericks-engine/mavericks/internal/plan"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
	"github.com/mavericks-engine/mavericks/pkg/keycloak"
	"github.com/mavericks-engine/mavericks/pkg/license"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// Single sign-on (enterprise). The tenant admin registers their identity
// provider under Admin › Single sign-on; the sign-in page asks
// /api/sso/discover which provider an address belongs to; a first brokered
// login creates the account. The store and the Keycloak orchestration live
// in ee/sso; this file is the HTTP surface and the JIT hook.

// currentCustomerID is the tenant an administrative request acts on: the
// routed tenant when dedicated databases are on, otherwise the X-Tenant-Id
// header (a platform admin choosing, or a member naming their own), else
// the one tenant a tenant admin belongs to.
func (h *handler) currentCustomerID(ctx context.Context, r *http.Request, act *actor) (string, error) {
	if id := tenantdb.TenantFrom(ctx); id != "" {
		return id, nil
	}
	all, ids, err := h.adminScopeCustomerIDs(ctx, act)
	if err != nil {
		return "", err
	}
	if want := r.Header.Get(tenantHeader); want != "" {
		if all || memberOf(ids, want) {
			var n int
			if err := h.db.QueryRow(ctx, `SELECT count(*) FROM core.customer WHERE id = $1::uuid`, want).Scan(&n); err != nil || n == 0 {
				return "", fmt.Errorf("tenant %s not found", want)
			}
			return want, nil
		}
		return "", fmt.Errorf("forbidden: you are not a member of tenant %s", want)
	}
	if !all && len(ids) == 1 {
		return ids[0], nil
	}
	if all {
		return "", fmt.Errorf("choose a tenant first (send X-Tenant-Id)")
	}
	return "", fmt.Errorf("your account belongs to %d tenants — choose one (send X-Tenant-Id)", len(ids))
}

// ssoBroker is the Keycloak client as ee/sso sees it; nil when the gateway
// has no service account, in which case registration is refused with a
// message rather than half-saved.
func (h *handler) ssoBroker() sso.Broker {
	if h.kc == nil {
		return nil
	}
	return h.kc
}

type ssoResponse struct {
	sso.Settings
	BrokerEndpoint     string `json:"broker_endpoint"`
	SPEntityID         string `json:"sp_entity_id"`
	RegistryConfigured bool   `json:"registry_configured"`
}

func (h *handler) ssoView(s sso.Settings, customerID string) ssoResponse {
	s.ClientSecret = ""
	alias := s.Alias
	if alias == "" {
		alias = sso.Alias(customerID)
	}
	return ssoResponse{
		Settings:           s,
		BrokerEndpoint:     keycloak.BrokerEndpoint(h.kcPublicURL, h.kcRealm, alias),
		SPEntityID:         keycloak.SAMLEntityID(h.kcPublicURL, h.kcRealm),
		RegistryConfigured: h.kc != nil,
	}
}

// ssoSettings handles GET, PUT and DELETE on /api/admin/sso.
func (h *handler) ssoSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	act, ok := h.requireRole(w, r, "platform_admin", "tenant_admin")
	if !ok {
		return
	}
	customerID, err := h.currentCustomerID(ctx, r, act)
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	tctx := h.tenantCtx(ctx, customerID)
	store := sso.NewStore(h.db.For(tctx))
	domains := sso.NewDomains(h.db.Control())

	switch r.Method {
	case http.MethodGet:
		s, err := store.Get(tctx)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		jsonOK(w, h.ssoView(s, customerID))
	case http.MethodPut:
		var body sso.Settings
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
		if h.kc == nil && !h.devMode {
			jsonErr(w, fmt.Errorf("the identity-provider registry is not configured on this gateway (KEYCLOAK_ADMIN_CLIENT_ID/SECRET), so a provider cannot be registered"), http.StatusServiceUnavailable)
			return
		}
		s, err := sso.Register(tctx, store, domains, h.ssoBroker(), customerID, body)
		if err != nil {
			h.log.Warn().Err(err).Str("tenant", customerID).Msg("sso: provider registration failed")
			status := http.StatusBadRequest
			if strings.Contains(err.Error(), "identity provider") && strings.Contains(err.Error(), ":") && h.kc != nil {
				status = http.StatusBadGateway
			}
			jsonErr(w, err, status)
			return
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: auditlog.EventSSOProviderUpdated,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ResourceType: "sso_provider", ResourceID: customerID,
			Metadata: map[string]string{"protocol": s.Protocol, "alias": s.Alias, "enabled": boolWord(s.Enabled), "domains": strings.Join(s.AllowedDomains, ",")},
		})
		jsonOK(w, h.ssoView(s, customerID))
	case http.MethodDelete:
		s, err := sso.Remove(tctx, store, domains, h.ssoBroker(), customerID)
		if err != nil {
			jsonErr(w, err, http.StatusBadGateway)
			return
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: auditlog.EventSSOProviderRemoved,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ResourceType: "sso_provider", ResourceID: customerID,
		})
		jsonOK(w, h.ssoView(s, customerID))
	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// ssoTest handles POST /api/admin/sso/test: read the provider's document
// without registering anything. Always 200 with {ok, ...} so the console
// renders a failure inline.
func (h *handler) ssoTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	if _, ok := h.requireRole(w, r, "platform_admin", "tenant_admin"); !ok {
		return
	}
	var body struct {
		Protocol    string `json:"protocol"`
		MetadataURL string `json:"metadata_url"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	details, err := sso.Probe(r.Context(), h.ssoBroker(), body.Protocol, body.MetadataURL)
	if err != nil {
		jsonOK(w, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	jsonOK(w, map[string]any{"ok": true, "details": details})
}

// ── discovery (public) ───────────────────────────────────────────────────────

// discoverLimiter caps /api/sso/discover per client address. The endpoint is
// unauthenticated by nature (nobody is signed in yet), and it must not be a
// free oracle for which domains are customers.
type discoverLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	// rate/burst override the discovery defaults (sign-up throttles harder).
	rate, burst float64
}

type bucket struct {
	tokens float64
	last   time.Time
}

const (
	discoverRate  = 0.5 // tokens per second (30/minute)
	discoverBurst = 10
)

func (l *discoverLimiter) allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		l.buckets = map[string]*bucket{}
	}
	rate, burst := l.rate, l.burst
	if rate == 0 {
		rate = discoverRate
	}
	if burst == 0 {
		burst = discoverBurst
	}
	now := time.Now()
	b := l.buckets[ip]
	if b == nil {
		b = &bucket{tokens: burst, last: now}
		l.buckets[ip] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * rate
	if b.tokens > burst {
		b.tokens = burst
	}
	b.last = now
	if len(l.buckets) > 10000 { // bound memory under a flood
		for k, v := range l.buckets {
			if now.Sub(v.last) > time.Minute {
				delete(l.buckets, k)
			}
		}
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		return strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ssoDiscover handles GET /api/sso/discover[?email=]. Without an e-mail it
// says only whether single sign-on exists on this deployment at all — what
// the sign-in page needs to decide whether to offer the option. With one,
// it returns the provider for that address's domain, or sso:true with no
// alias when the domain is not registered (never a different answer for
// "unknown" and "known but disabled").
func (h *handler) ssoDiscover(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	if !h.discover.allow(clientIP(r)) {
		jsonErr(w, fmt.Errorf("too many requests"), http.StatusTooManyRequests)
		return
	}
	ctx := r.Context()
	domains := sso.NewDomains(h.db.Control())
	hasSSO, err := domains.Any(ctx)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	out := map[string]any{"sso": hasSSO}
	email := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("email")))
	if hasSSO && strings.Contains(email, "@") {
		customerID, alias, err := domains.Lookup(ctx, email[strings.LastIndex(email, "@")+1:])
		if err == nil && alias != "" {
			tctx := h.tenantCtx(ctx, customerID)
			if s, err := sso.NewStore(h.db.For(tctx)).Get(tctx); err == nil && s.Enabled && s.Configured {
				out["alias"] = alias
				out["display_name"] = s.DisplayName
			}
		}
	}
	jsonOK(w, out)
}

// ── first login ──────────────────────────────────────────────────────────────

// errNotProvisionable carries the reason a brokered first login was
// refused, so the person sees it instead of a bare 401.
type errNotProvisionable struct{ reason string }

func (e *errNotProvisionable) Error() string { return e.reason }

// jitProvision creates the account behind a valid token whose subject the
// platform has never seen. It applies only to brokered logins: Keycloak
// tells us which identity provider authenticated the person, the alias
// names the tenant, and the tenant's settings decide whether and how the
// account is created. Anyone else with a valid-but-unknown token — a
// realm user created outside the console — is still refused, as before.
func (h *handler) jitProvision(ctx context.Context, claims *identity.Claims) (*actor, error) {
	if h.kc == nil || !h.lic.Has(license.FeatureSSO) {
		return nil, fmt.Errorf("unknown subject")
	}
	links, err := h.kc.FederatedIdentities(ctx, claims.Subject)
	if err != nil || len(links) == 0 {
		return nil, fmt.Errorf("unknown subject")
	}
	for _, link := range links {
		customerID, ok := sso.CustomerFromAlias(link.IdentityProvider)
		if !ok {
			continue
		}
		tctx := h.tenantCtx(ctx, customerID)
		name := strings.TrimSpace(claims.Name)
		if name == "" {
			name = strings.TrimSpace(claims.GivenName + " " + claims.FamilyName)
		}
		email := strings.ToLower(claims.Email)
		if email == "" {
			return nil, &errNotProvisionable{"your identity provider did not share an e-mail address, which this platform needs to create an account"}
		}
		// A first login is a user creation: it counts against the plan.
		if h.plans != nil {
			if err := h.plans.CheckUsers(tctx, h.db.For(tctx), customerID, 1); err != nil {
				if plan.IsLimit(err) {
					return nil, &errNotProvisionable{err.Error()}
				}
				return nil, err
			}
		}
		userID, err := sso.ProvisionFirstLogin(tctx, h.db.For(tctx), customerID, claims.Subject, email, name)
		var np *sso.ErrNotProvisionable
		if errors.As(err, &np) {
			return nil, &errNotProvisionable{np.Reason}
		}
		if err != nil {
			return nil, err
		}
		h.noteUser(tctx, claims.Subject, email)
		auditlog.Log(tctx, h.db.For(tctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: auditlog.EventSSOUserProvisioned,
			ActorUserID: userID, ActorRole: "sso",
			ResourceType: "identity_user", ResourceID: userID,
			Metadata: map[string]string{"email": email, "provider": link.IdentityProvider},
		})
		return h.actorByKeycloakSub(tctx, claims.Subject)
	}
	return nil, fmt.Errorf("unknown subject")
}

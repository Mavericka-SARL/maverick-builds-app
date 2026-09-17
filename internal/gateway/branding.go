package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/mavericks-engine/mavericks/ee/branding"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
	"github.com/mavericks-engine/mavericks/pkg/license"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// White-labelling (commercial and enterprise). The public endpoint is what
// the console reads before and after sign-in: a signed-in caller gets their
// tenant's brand; an anonymous request at a registered custom host gets
// that host's tenant's brand; anyone else gets the platform's own look.

// brandView is the public shape: what the console applies, nothing about
// e-mail or domains.
type brandView struct {
	ProductName    string `json:"product_name"`
	Tagline        string `json:"tagline"`
	LogoDataURL    string `json:"logo_data_url"`
	FaviconDataURL string `json:"favicon_data_url"`
	BrandColor     string `json:"brand_color"`
	Configured     bool   `json:"configured"`
	Source         string `json:"source"` // tenant | host | default
}

func toView(s branding.Settings, source string) brandView {
	if !s.Configured {
		return brandView{Source: "default"}
	}
	return brandView{ProductName: s.ProductName, Tagline: s.Tagline, LogoDataURL: s.LogoDataURL, FaviconDataURL: s.FaviconDataURL, BrandColor: s.BrandColor, Configured: true, Source: source}
}

// actorTenantID is the tenant a signed-in person belongs to: the routed
// tenant with dedicated databases, else their customer, else the customer
// of the first workspace they hold a role in.
func (h *handler) actorTenantID(ctx context.Context, userID string) string {
	if id := tenantdb.TenantFrom(ctx); id != "" {
		return id
	}
	var id string
	_ = h.db.QueryRow(ctx, `
		SELECT COALESCE(u.customer_id::text, (
		    SELECT w.customer_id::text FROM identity.role_assignment ra JOIN core.workspace w ON w.id = ra.workspace_id
		    WHERE ra.user_id = u.id ORDER BY ra.assigned_at LIMIT 1), '')
		FROM identity."user" u WHERE u.id = $1::uuid`, userID).Scan(&id)
	return id
}

// publicBranding handles GET /api/branding.
func (h *handler) publicBranding(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	if !h.lic.Has(license.FeatureWhiteLabel) {
		jsonOK(w, brandView{Source: "default"})
		return
	}
	if act, err := h.resolveActor(ctx, r); err == nil {
		if tenant := h.actorTenantID(ctx, act.UserID); tenant != "" {
			tctx := h.tenantCtx(ctx, tenant)
			if s, err := branding.NewStore(h.db.For(tctx), tenant).Get(tctx); err == nil && s.Configured {
				jsonOK(w, toView(s, "tenant"))
				return
			}
		}
	}
	host := r.Host
	if fwd := r.Header.Get("X-Forwarded-Host"); fwd != "" {
		host = strings.TrimSpace(strings.Split(fwd, ",")[0])
	}
	if hn, _, err := net.SplitHostPort(host); err == nil {
		host = hn
	}
	if tenant, err := branding.NewDomains(h.db.Control()).Lookup(ctx, host); err == nil && tenant != "" {
		tctx := h.tenantCtx(ctx, tenant)
		if s, err := branding.NewStore(h.db.For(tctx), tenant).Get(tctx); err == nil && s.Configured {
			jsonOK(w, toView(s, "host"))
			return
		}
	}
	jsonOK(w, brandView{Source: "default"})
}

// adminBranding handles GET, PUT and DELETE on /api/admin/branding.
func (h *handler) adminBranding(w http.ResponseWriter, r *http.Request) {
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
	store := branding.NewStore(h.db.For(tctx), customerID)
	domains := branding.NewDomains(h.db.Control())

	switch r.Method {
	case http.MethodGet:
		s, err := store.Get(tctx)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		jsonOK(w, s)
	case http.MethodPut:
		var body branding.Settings
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body (images must be data URLs within the size limits)"), http.StatusBadRequest)
			return
		}
		validated, err := branding.Validate(body)
		if err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		if err := domains.Set(ctx, customerID, validated.CustomDomain); err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		s, err := store.Update(tctx, validated)
		if err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: auditlog.EventBrandingUpdated,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ResourceType: "branding", ResourceID: customerID,
			Metadata: map[string]string{"product_name": s.ProductName, "custom_domain": s.CustomDomain, "has_logo": boolWord(s.LogoDataURL != "")},
		})
		jsonOK(w, s)
	case http.MethodDelete:
		if err := domains.Set(ctx, customerID, ""); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		s, err := store.Clear(tctx)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: auditlog.EventBrandingRemoved,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ResourceType: "branding", ResourceID: customerID,
		})
		jsonOK(w, s)
	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

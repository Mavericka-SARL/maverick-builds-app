package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/mavericks-engine/mavericks/ee/scim"
	"github.com/mavericks-engine/mavericks/ee/sso"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
	"github.com/mavericks-engine/mavericks/pkg/license"
)

// SCIM provisioning (enterprise). Token administration is a console
// concern for the tenant admin; the SCIM endpoint itself is called by the
// tenant's identity provider with one of those tokens and never by a
// person. The service lives in ee/scim; this file authenticates the token,
// routes to the tenant and hands over.

// scimTokens handles GET and POST on /api/admin/scim/tokens.
func (h *handler) scimTokens(w http.ResponseWriter, r *http.Request) {
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
	store := scim.NewTokenStore(h.db.For(tctx))

	switch r.Method {
	case http.MethodGet:
		list, err := store.List(tctx, customerID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		jsonOK(w, list)
	case http.MethodPost:
		var body struct {
			Name        string `json:"name"`
			DefaultRole string `json:"default_role"`
			WorkspaceID string `json:"workspace_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
		if body.WorkspaceID != "" {
			var n int
			if err := h.db.For(tctx).QueryRow(tctx, `SELECT count(*) FROM core.workspace WHERE id = $1::uuid AND customer_id = $2::uuid`, body.WorkspaceID, customerID).Scan(&n); err != nil || n == 0 {
				jsonErr(w, fmt.Errorf("workspace %s does not belong to this tenant", body.WorkspaceID), http.StatusBadRequest)
				return
			}
		}
		tok, plain, err := store.Issue(tctx, customerID, body.Name, body.DefaultRole, body.WorkspaceID, act.UserID)
		if err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: auditlog.EventScimTokenIssued,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ResourceType: "scim_token", ResourceID: tok.ID,
			Metadata: map[string]string{"name": tok.Name, "default_role": tok.DefaultRole, "tenant": customerID},
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": tok.ID, "name": tok.Name, "default_role": tok.DefaultRole, "workspace_id": tok.WorkspaceID,
			"created_at": tok.CreatedAt, "token": plain, "base_url": strings.TrimSuffix(h.publicURL, "/") + "/api/scim/v2",
		})
	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// scimTokenRevoke handles DELETE /api/admin/scim/tokens/{id}.
func (h *handler) scimTokenRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
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
	id := strings.TrimPrefix(r.URL.Path, "/api/admin/scim/tokens/")
	tctx := h.tenantCtx(ctx, customerID)
	if err := scim.NewTokenStore(h.db.For(tctx)).Revoke(tctx, customerID, id); err != nil {
		jsonErr(w, err, http.StatusNotFound)
		return
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryAdmin, EventType: auditlog.EventScimTokenRevoked,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ResourceType: "scim_token", ResourceID: id,
	})
	jsonOK(w, map[string]string{"status": "revoked"})
}

// scimEndpoint serves every /api/scim/v2 route: licence, token, tenant,
// then the service. Errors are SCIM-shaped so the identity provider logs
// something it understands.
func (h *handler) scimEndpoint(w http.ResponseWriter, r *http.Request) {
	scimErr := func(status int, detail string) {
		w.Header().Set("Content-Type", "application/scim+json; charset=utf-8")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"schemas": []string{"urn:ietf:params:scim:api:messages:2.0:Error"}, "status": fmt.Sprint(status), "detail": detail,
		})
	}
	if err := h.lic.Require(license.FeatureSCIM); err != nil {
		scimErr(http.StatusForbidden, err.Error())
		return
	}
	plain := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
	customerID, ok := scim.ParseToken(plain)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="scim"`)
		scimErr(http.StatusUnauthorized, "a SCIM bearer token is required")
		return
	}
	ctx := r.Context()
	tctx := h.tenantCtx(ctx, customerID)
	pool := h.db.For(tctx)
	tok, err := scim.NewTokenStore(pool).Verify(tctx, plain)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="scim"`)
		scimErr(http.StatusUnauthorized, "invalid or revoked SCIM token")
		return
	}
	// A tenant that signs in through its own provider gets no invitation
	// mail: those users never have a password here.
	invite := true
	if s, err := sso.NewStore(pool).Get(tctx, customerID); err == nil && s.Enabled && s.Configured {
		invite = false
	}
	svc := &scim.Service{
		Pool: pool, CustomerID: customerID, DefaultRole: tok.DefaultRole, WorkspaceID: tok.WorkspaceID,
		InviteNewUsers: invite, InviteLifetime: inviteLifetime, Log: h.log,
		OnUserCreated: func(c context.Context, sub, email string) { h.noteUser(c, sub, email) },
		OnUserDeleted: func(c context.Context, sub string) { h.forgetUser(c, sub) },
	}
	if h.kc != nil {
		svc.IdP = h.kc
	}
	if h.plans != nil {
		// A provisioned user counts against the tenant's plan like any other.
		svc.CanCreateUser = func(c context.Context) error { return h.plans.CheckUsers(c, pool, customerID, 1) }
	}
	svc.ServeHTTP(w, r.WithContext(tctx))
}

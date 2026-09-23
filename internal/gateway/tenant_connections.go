package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/internal/importpkg"
	"github.com/mavericks-engine/mavericks/internal/integration"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// Integrations › Google Sheets: the tenant's Google service account, which
// makes private Google Sheets importable (decided 2026-09-21: one account
// per tenant, not one for the deployment; moved 2026-09-22 from a separate
// Admin › Connections tab to the Google Sheets integration itself, where
// the person who imports sheets is). The row belongs to the tenant of the
// application the developer has open — the same tenant every Sheets import
// of that application reads through — so anyone who may build in the
// application may store it: developers, and the tenant's administrators.
//
//   GET    /api/developer/integrations/google-service-account        the public half: address, project
//   PUT    /api/developer/integrations/google-service-account        {"key_file": "<the JSON Google downloaded>"}
//   POST   /api/developer/integrations/google-service-account/test   obtains an access token: proves the key
//   DELETE /api/developer/integrations/google-service-account

type googleConnectionResponse struct {
	Configured  bool       `json:"configured"`
	ClientEmail string     `json:"client_email,omitempty"`
	ProjectID   string     `json:"project_id,omitempty"`
	UpdatedAt   *time.Time `json:"updated_at,omitempty"`
}

func (h *handler) googleConnectionView(ctx context.Context, store *integration.TenantCredentialStore, customerID string) (googleConnectionResponse, error) {
	cred, err := store.Get(ctx, customerID, integration.KindGoogleServiceAccount)
	if errors.Is(err, integration.ErrNoTenantCredential) {
		return googleConnectionResponse{}, nil
	}
	if err != nil {
		return googleConnectionResponse{}, err
	}
	at := cred.UpdatedAt
	return googleConnectionResponse{Configured: true, ClientEmail: cred.Meta["client_email"], ProjectID: cred.Meta["project_id"], UpdatedAt: &at}, nil
}

// googleConnectionScope is the tenant of the application the caller has
// open, once the caller is allowed into that application.
func (h *handler) googleConnectionScope(w http.ResponseWriter, r *http.Request, act *actor) (settingsScope, bool) {
	ctx := r.Context()
	appID, _ := ctx.Value(appIDCtxKey).(string)
	if appID == "" {
		jsonErr(w, fmt.Errorf("open an application first: the account belongs to its tenant"), http.StatusBadRequest)
		return settingsScope{}, false
	}
	if ok, err := h.actorCanAccessApp(ctx, act, appID); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return settingsScope{}, false
	} else if !ok {
		jsonErr(w, fmt.Errorf("forbidden: not your application"), http.StatusForbidden)
		return settingsScope{}, false
	}
	customerID := h.requestCustomerID(ctx, r, act)
	if customerID == "" {
		jsonErr(w, fmt.Errorf("this application belongs to no tenant"), http.StatusConflict)
		return settingsScope{}, false
	}
	return settingsScope{CustomerID: customerID}, true
}

func (h *handler) googleConnection(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	act, ok := h.requireRole(w, r, "developer", "platform_admin", "tenant_admin")
	if !ok {
		return
	}
	scope, ok := h.googleConnectionScope(w, r, act)
	if !ok {
		return
	}
	store := integration.NewTenantCredentialStore(h.db.For(ctx))
	audit := func(event auditlog.EventType, meta map[string]string) {
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: event,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ResourceType: "tenant_credential", ResourceID: scope.CustomerID,
			Metadata: meta,
		})
	}

	switch r.Method {
	case http.MethodGet:
		view, err := h.googleConnectionView(ctx, store, scope.CustomerID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		jsonOK(w, view)
	case http.MethodPut:
		var body struct {
			KeyFile string `json:"key_file"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil || strings.TrimSpace(body.KeyFile) == "" {
			jsonErr(w, fmt.Errorf("key_file required: the JSON key Google Cloud downloaded for the service account"), http.StatusBadRequest)
			return
		}
		sa, err := importpkg.ParseServiceAccountKey([]byte(body.KeyFile))
		if err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		// Store only what the fetch needs — never the whole file.
		secret, _ := json.Marshal(map[string]string{"client_email": sa.ClientEmail, "private_key": sa.PrivateKey})
		meta := map[string]string{"client_email": sa.ClientEmail, "project_id": sa.ProjectID}
		if err := store.Put(ctx, scope.CustomerID, integration.KindGoogleServiceAccount, meta, secret, act.UserID); err != nil {
			if errors.Is(err, integration.ErrNoEncryptionKey) {
				jsonErr(w, fmt.Errorf("this deployment cannot store credentials: %w", err), http.StatusServiceUnavailable)
				return
			}
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		audit(auditlog.EventTenantCredentialUpdated, map[string]string{"kind": integration.KindGoogleServiceAccount, "client_email": sa.ClientEmail})
		view, err := h.googleConnectionView(ctx, store, scope.CustomerID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		jsonOK(w, view)
	case http.MethodDelete:
		if err := store.Delete(ctx, scope.CustomerID, integration.KindGoogleServiceAccount); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		audit(auditlog.EventTenantCredentialDeleted, map[string]string{"kind": integration.KindGoogleServiceAccount})
		jsonOK(w, googleConnectionResponse{})
	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// googleConnectionTest proves the stored key by obtaining an access token
// from Google — the same call every import makes first. Nothing is read.
func (h *handler) googleConnectionTest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	act, ok := h.requireRole(w, r, "developer", "platform_admin", "tenant_admin")
	if !ok {
		return
	}
	scope, ok := h.googleConnectionScope(w, r, act)
	if !ok {
		return
	}
	sa, err := h.googleServiceAccount(ctx, scope.CustomerID)
	if errors.Is(err, integration.ErrNoTenantCredential) {
		jsonErr(w, fmt.Errorf("no Google service account is stored for this tenant"), http.StatusConflict)
		return
	}
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	_, err = sa.AccessToken(ctx, h.sheetFetcher().Client)
	outcome := "ok"
	if err != nil {
		outcome = "failed"
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryAdmin, EventType: auditlog.EventTenantCredentialTested,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ResourceType: "tenant_credential", ResourceID: scope.CustomerID,
		Metadata: map[string]string{"kind": integration.KindGoogleServiceAccount, "outcome": outcome},
	})
	if err != nil {
		jsonErr(w, err, http.StatusBadGateway)
		return
	}
	jsonOK(w, map[string]string{"status": "ok", "client_email": sa.ClientEmail})
}

// googleServiceAccount opens a tenant's stored account for a fetch.
func (h *handler) googleServiceAccount(ctx context.Context, customerID string) (importpkg.ServiceAccount, error) {
	if customerID == "" {
		return importpkg.ServiceAccount{}, integration.ErrNoTenantCredential
	}
	secret, err := integration.NewTenantCredentialStore(h.db.For(ctx)).Secret(ctx, customerID, integration.KindGoogleServiceAccount)
	if err != nil {
		return importpkg.ServiceAccount{}, err
	}
	var s struct {
		ClientEmail string `json:"client_email"`
		PrivateKey  string `json:"private_key"`
	}
	if err := json.Unmarshal(secret, &s); err != nil {
		return importpkg.ServiceAccount{}, fmt.Errorf("stored google credential: %w", err)
	}
	sa := importpkg.ServiceAccount{ClientEmail: s.ClientEmail, PrivateKey: s.PrivateKey}
	if h.sheets != nil {
		// Tests point the account at their fake Google.
		sa.TokenURL, sa.APIBase = h.sheetsTokenURL, h.sheetsAPIBase
	}
	return sa, nil
}

// fetchSheetCSV reads a sheet the way the tenant can: through its own
// service account when it has one (private sheets), else the link-shared
// export. customerID is the tenant the request acts for.
func (h *handler) fetchSheetCSV(ctx context.Context, customerID, spreadsheetID, gid string) ([]byte, error) {
	sa, err := h.googleServiceAccount(ctx, customerID)
	switch {
	case err == nil:
		return h.sheetFetcher().FetchCSVWithServiceAccount(ctx, sa, spreadsheetID, gid)
	case errors.Is(err, integration.ErrNoTenantCredential):
		return h.sheetFetcher().FetchCSV(ctx, spreadsheetID, gid)
	default:
		return nil, err
	}
}

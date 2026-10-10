package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// Chat connector settings (GET/PUT /api/admin/connector-settings): whether
// a tenant's people may change grid data from a chat connection (ChatGPT,
// Claude — mcp.go). On by default (migration 128); a tenant administrator,
// or the platform administrator for a tenant they pick, turns it off for the
// whole tenant. Reads through a connection are not affected, and nothing is
// changed for anyone in the console.
//
// The setting is one tenant's: there is no deployment row to inherit, since
// a write is the tenant's data and the tenant's call.

type connectorSettings struct {
	ChatWrites bool          `json:"chat_writes"`
	Scope      settingsScope `json:"scope"`
}

func (h *handler) connectorSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	act, ok := h.requireRole(w, r, "platform_admin", "tenant_admin")
	if !ok {
		return
	}
	scope, ok := h.settingsScopeFor(w, r, act, false)
	if !ok {
		return
	}
	switch r.Method {
	case http.MethodGet:
		on, err := h.chatWritesOn(ctx, scope.CustomerID)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		jsonOK(w, connectorSettings{ChatWrites: on, Scope: scope})
	case http.MethodPut:
		var body struct {
			ChatWrites *bool `json:"chat_writes"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ChatWrites == nil {
			jsonErr(w, fmt.Errorf("invalid body: chat_writes (true or false) is required"), http.StatusBadRequest)
			return
		}
		tag, err := h.db.For(ctx).Exec(ctx,
			`UPDATE core.customer SET chat_writes = $2, updated_at = now() WHERE id = $1::uuid`, scope.CustomerID, *body.ChatWrites)
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if tag.RowsAffected() == 0 {
			jsonErr(w, fmt.Errorf("tenant not found"), http.StatusNotFound)
			return
		}
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: auditlog.EventConnectorSettingsUpdated,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ResourceType: "connector_settings", ResourceID: scopeResourceID(scope),
			Metadata: map[string]string{"chat_writes": boolWord(*body.ChatWrites)},
		})
		jsonOK(w, connectorSettings{ChatWrites: *body.ChatWrites, Scope: scope})
	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

// chatWritesOn reports whether tenant cid lets its people change grid data
// from a chat connection. A tenant with no row (none on this database)
// keeps the default, on.
func (h *handler) chatWritesOn(ctx context.Context, cid string) (bool, error) {
	on := true
	err := h.db.For(ctx).QueryRow(ctx, `SELECT chat_writes FROM core.customer WHERE id = $1::uuid`, cid).Scan(&on)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	return on, nil
}

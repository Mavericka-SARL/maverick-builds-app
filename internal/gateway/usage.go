package gateway

import (
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"net/http"
	"time"

	"github.com/mavericks-engine/mavericks/ee/usage"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// Usage analytics (enterprise): a platform admin sees every tenant, a
// tenant admin their own. With dedicated databases each tenant is counted in
// its own database, and that database's size is reported; on a shared
// database the size is left at zero rather than reporting everyone's.
func (h *handler) adminUsage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	act, ok := h.requireRole(w, r, "platform_admin", "tenant_admin")
	if !ok {
		return
	}
	days, err := usage.ParsePeriod(r.URL.Query().Get("period"))
	if err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	since := time.Now().Add(-time.Duration(days) * 24 * time.Hour)

	var tenants []usage.Tenant
	if act.hasRole("platform_admin") {
		if router := h.db.Router(); router != nil {
			err = router.Each(ctx, func(t tenantdb.Tenant, pool *pgxpool.Pool) error {
				u, err := usage.Snapshot(ctx, pool, t.CustomerID, since, t.Database)
				if err != nil {
					h.log.Warn().Err(err).Str("tenant", t.CustomerID).Msg("usage: tenant skipped")
					return nil
				}
				tenants = append(tenants, u)
				return nil
			})
		} else {
			rows, qerr := h.db.Query(ctx, `SELECT id::text FROM core.customer ORDER BY created_at`)
			if qerr != nil {
				jsonErr(w, qerr, http.StatusInternalServerError)
				return
			}
			var ids []string
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					ids = append(ids, id)
				}
			}
			rows.Close()
			for _, id := range ids {
				u, serr := usage.Snapshot(ctx, h.db.For(ctx), id, since, "")
				if serr != nil {
					err = serr
					break
				}
				tenants = append(tenants, u)
			}
		}
	} else {
		customerID, cerr := h.currentCustomerID(ctx, r, act)
		if cerr != nil {
			jsonErr(w, cerr, http.StatusBadRequest)
			return
		}
		tctx := h.tenantCtx(ctx, customerID)
		dbName := ""
		if s, ok := tenantdb.ScopeFrom(tctx); ok && s.CustomerID == customerID && h.db.Router() != nil {
			dbName = tenantdb.DatabaseName(customerID)
		}
		var u usage.Tenant
		u, err = usage.Snapshot(tctx, h.db.For(tctx), customerID, since, dbName)
		tenants = []usage.Tenant{u}
	}
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if tenants == nil {
		tenants = []usage.Tenant{}
	}
	jsonOK(w, map[string]any{"period_days": days, "since": since, "tenants": tenants})
}

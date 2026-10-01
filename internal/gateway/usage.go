package gateway

import (
	"fmt"
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
		// Every tenant: those the control plane holds — all of them with a
		// single database, those created in shared mode with dedicated
		// ones — then each dedicated tenant in its own database. One that
		// cannot be counted is listed with why. In dedicated mode the
		// control plane's tenants were left out, a tenant that was not
		// ready silently, and one that failed to open stopped the report.
		control := h.db.Control()
		rows, qerr := control.Query(ctx, `SELECT id::text, name, plan, created_at FROM core.customer ORDER BY created_at`)
		if qerr != nil {
			jsonErr(w, qerr, http.StatusInternalServerError)
			return
		}
		type customer struct {
			id, name, plan string
			createdAt      time.Time
		}
		var inControl []customer
		for rows.Next() {
			var c customer
			if rows.Scan(&c.id, &c.name, &c.plan, &c.createdAt) == nil {
				inControl = append(inControl, c)
			}
		}
		rows.Close()
		for _, c := range inControl {
			u, serr := usage.Snapshot(controlCtx(ctx), control, c.id, since, "")
			if serr != nil {
				h.log.Warn().Err(serr).Str("tenant", c.id).Msg("usage: tenant not counted")
				u = usage.Tenant{CustomerID: c.id, Name: c.name, Plan: c.plan, CreatedAt: c.createdAt,
					Status: "failed", Error: serr.Error()}
			}
			tenants = append(tenants, u)
		}
		if router := h.db.Router(); router != nil {
			catalog, cerr := router.Catalog().List(ctx)
			if cerr != nil {
				jsonErr(w, cerr, http.StatusInternalServerError)
				return
			}
			for _, t := range catalog {
				unavailable := usage.Tenant{CustomerID: t.CustomerID, Name: t.Name, Plan: t.Plan, CreatedAt: t.CreatedAt,
					Status: t.Status, Error: t.Error}
				if t.Status != tenantdb.StatusReady {
					tenants = append(tenants, unavailable)
					continue
				}
				pool, perr := router.Pool(ctx, t.CustomerID)
				if perr != nil {
					unavailable.Status, unavailable.Error = tenantdb.StatusFailed, perr.Error()
					tenants = append(tenants, unavailable)
					continue
				}
				tctx := tenantdb.WithScope(ctx, tenantdb.Scope{CustomerID: t.CustomerID, Pool: pool})
				u, serr := usage.Snapshot(tctx, pool, t.CustomerID, since, t.Database)
				if serr != nil {
					h.log.Warn().Err(serr).Str("tenant", t.CustomerID).Msg("usage: tenant not counted")
					unavailable.Status, unavailable.Error = "failed", serr.Error()
					u = unavailable
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

package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog"

	"github.com/mavericks-engine/mavericks/internal/plan"
	"github.com/mavericks-engine/mavericks/internal/secretbox"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
	"github.com/mavericks-engine/mavericks/pkg/license"
)

// A license key installed in the console (owner, 2026-11-22): a platform
// administrator pastes it under Platform › License, the gateway verifies it
// against the compiled-in keys, keeps it on the control plane
// (platform.license_key, migration 197) and puts it in force at once —
// every other replica within licenseSyncInterval, and every gateway at its
// next start. A key in the server's configuration (MAVERICKS_LICENSE_KEY or
// _FILE) wins: the console then says where it is set, and refuses both
// install and removal.

// licenseDB is what the store needs of the control plane.
type licenseDB = plan.DB

// LicenseSyncInterval is how often a gateway re-reads the installed key, so
// a key installed through one replica reaches the others.
const LicenseSyncInterval = 30 * time.Second

// ApplyStoredLicense puts the key installed in the console in force, or
// takes away one that was removed. changed says it did either. Nothing
// happens while the configuration names a key. Called at start-up and by
// RunLicenseSync.
func ApplyStoredLicense(ctx context.Context, db licenseDB, lic *license.Manager) (changed bool, err error) {
	if lic.FromConfig() {
		return false, nil
	}
	var stored, licenseID string
	err = db.QueryRow(ctx, `SELECT token, license_id FROM platform.license_key`).Scan(&stored, &licenseID)
	cur := lic.Stored()
	if errors.Is(err, pgx.ErrNoRows) {
		if cur == nil {
			return false, nil
		}
		return true, lic.UseStored(nil)
	}
	if err != nil {
		return false, err
	}
	if cur != nil && cur.ID == licenseID {
		return false, nil
	}
	token, err := secretbox.Decrypt(stored)
	if err != nil {
		return false, err
	}
	claims, err := lic.Verify(token)
	if err != nil {
		return false, fmt.Errorf("the installed license key could not be verified: %w", err)
	}
	return true, lic.UseStored(claims)
}

// RunLicenseSync re-reads the installed key every interval until ctx ends,
// and moves tenants down whenever the edition in force drops — a key that
// expired (owner, 2026-11-22: when its period is over, the deployment and
// every tenant run the Community Edition at once), or one removed or
// replaced through another replica. Outside the Playground only; with a
// database per tenant, the directory moves now and each tenant's database
// at the gateway's next start.
func RunLicenseSync(ctx context.Context, db licenseDB, lic *license.Manager, playground bool, interval time.Duration, log zerolog.Logger) {
	t := time.NewTicker(interval)
	defer t.Stop()
	last := lic.Edition()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		changed, err := ApplyStoredLicense(ctx, db, lic)
		if err != nil {
			if ctx.Err() == nil {
				log.Warn().Err(err).Msg("installed license key not re-read")
			}
			continue
		}
		if changed {
			st := lic.Status()
			log.Info().Str("edition", string(st.Edition)).Str("license_id", st.LicenseID).Msg("license key installed in the console is now in force")
		}
		cur := lic.Edition()
		if !playground && slices.Index(license.Editions, cur) < slices.Index(license.Editions, last) {
			if moved, err := plan.CapTenants(ctx, db, cur, true); err != nil {
				log.Warn().Err(err).Msg("tenants above the deployment's edition were not moved to it; retrying")
				continue // keep last, so the next tick tries again
			} else {
				log.Info().Int64("tenants", moved).Str("edition", string(cur)).Str("was", string(last)).Msg("the deployment's edition dropped; tenants above it moved to it")
			}
		}
		last = cur
	}
}

// capTenantsToLicense moves tenants above the edition now in force down to
// it, as the gateway does at start-up (plan.CapTenants): after a key is
// removed, or replaced by a lower one, Change edition is no longer there to
// do it by hand (owner, 2026-11-22). Outside the Playground only. With a
// database per tenant, the directory moves now and each tenant's database
// at the gateway's next start.
func (h *handler) capTenantsToLicense(ctx context.Context) {
	if h.signupCfg.Playground {
		return
	}
	ed := h.lic.Edition()
	moved, err := plan.CapTenants(ctx, h.db.Control(), ed, true)
	if err != nil {
		h.log.Warn().Err(err).Msg("tenants above the deployment's edition were not moved to it")
		return
	}
	if moved > 0 {
		h.log.Info().Int64("tenants", moved).Str("edition", string(ed)).Msg("tenants above the deployment's edition moved to it")
	}
	if h.plans != nil {
		h.plans.InvalidateAll()
	}
}

// adminLicense serves PUT and DELETE /api/admin/license (platform
// administrators): install a pasted key, or remove the installed one.
func (h *handler) adminLicense(w http.ResponseWriter, r *http.Request) {
	act, ok := h.requireRole(w, r, "platform_admin")
	if !ok {
		return
	}
	ctx := r.Context()
	if h.lic.FromConfig() {
		jsonErr(w, license.ErrConfigured, http.StatusConflict)
		return
	}
	db := h.db.Control()
	audit := func(event auditlog.EventType, meta map[string]string) {
		auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
			Category: auditlog.CategoryAdmin, EventType: event,
			ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
			ResourceType: "license", ResourceID: meta["license_id"], Metadata: meta,
		})
	}
	switch r.Method {
	case http.MethodPut:
		var body struct {
			Key string `json:"key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
			return
		}
		claims, err := h.lic.Check(body.Key)
		if err != nil {
			jsonErr(w, err, http.StatusBadRequest)
			return
		}
		stored, err := secretbox.Encrypt(strings.TrimSpace(body.Key))
		if err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if _, err := db.Exec(ctx, `
			INSERT INTO platform.license_key (one, token, license_id, edition, expires_at, installed_by, installed_at)
			VALUES (TRUE, $1, $2, $3, $4, NULLIF($5, '')::uuid, now())
			ON CONFLICT (one) DO UPDATE SET token = EXCLUDED.token, license_id = EXCLUDED.license_id,
			    edition = EXCLUDED.edition, expires_at = EXCLUDED.expires_at,
			    installed_by = EXCLUDED.installed_by, installed_at = now()`,
			stored, claims.ID, string(claims.Edition), claims.ExpiresAt, act.UserID); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if err := h.lic.UseStored(claims); err != nil {
			jsonErr(w, err, http.StatusConflict)
			return
		}
		h.capTenantsToLicense(ctx)
		audit(auditlog.EventLicenseInstalled, map[string]string{"license_id": claims.ID, "edition": string(claims.Edition),
			"customer": claims.Customer, "order": claims.Order, "expires_at": claims.ExpiresAt.UTC().Format(time.RFC3339)})
		jsonOK(w, h.lic.Status())
	case http.MethodDelete:
		cur := h.lic.Stored()
		if _, err := db.Exec(ctx, `DELETE FROM platform.license_key`); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		if err := h.lic.UseStored(nil); err != nil {
			jsonErr(w, err, http.StatusConflict)
			return
		}
		h.capTenantsToLicense(ctx)
		meta := map[string]string{}
		if cur != nil {
			meta["license_id"], meta["edition"] = cur.ID, string(cur.Edition)
		}
		audit(auditlog.EventLicenseRemoved, meta)
		jsonOK(w, h.lic.Status())
	default:
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
	}
}

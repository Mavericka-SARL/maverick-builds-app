package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/plan"
	"github.com/mavericks-engine/mavericks/internal/startersync"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// Self-service sign-up: a visitor with a company name and a work address
// gets a tenant on the self-service plan, the "Getting started" application
// (installGettingStarted: the "Learn the platform" tour, what they first
// see, and one guide per role), and an invitation to set their password.
// They arrive holding every tenant role (grantEveryTenantRole), which is
// everything one person needs to build and run an application on their own.
//
// Public by nature (nobody is signed in yet), so it is rate-limited per
// address, refuses an address that already has an account, and creates
// nothing until the identity provider has agreed to the account. A failure
// after that point undoes everything it made: a half-created tenant would
// be a support ticket, not a lead.

const (
	signupRate     = 1.0 / 300 // one attempt per five minutes per address, sustained
	signupBurst    = 3
	signupAppName  = startersync.AppName
	signupMaxField = 80
)

type signupRequest struct {
	Company   string `json:"company"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Email     string `json:"email"`
}

// signupOptions serves GET /api/signup/options: whether sign-up is open
// here and on what terms, which is all the page needs before it renders.
func (h *handler) signupOptions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := map[string]any{"enabled": false, "contact_url": h.signupCfg.ContactURL}
	p, ok, err := h.selfServicePlan(ctx)
	// The hosted Playground, where unused workspaces are retired: the
	// terms and the console's edition settings say so only there.
	out["playground"] = h.signupCfg.Playground
	switch {
	case !h.signupCfg.Enabled:
		out["reason"] = "Self-service sign-up is not enabled on this deployment."
	case h.kc == nil && !h.devMode:
		out["reason"] = "Sign-up is not available: the identity provider is not configured."
	case err != nil:
		out["reason"] = "Sign-up is temporarily unavailable."
	case !ok:
		out["reason"] = "No edition is open for self-service sign-up."
	default:
		out["enabled"] = true
		offered := map[string]any{"key": p.Key, "name": p.Name, "description": p.Description, "limits": p.Limits, "limit_note": p.LimitNote}
		if h.signupCfg.Playground {
			// The terms quote these, in the Playground only: when a
			// workspace nobody uses is deleted, and how long a backup can
			// bring it back.
			offered["dormant_warn_days"], offered["dormant_delete_days"] = p.DormantWarnDays, p.DormantDeleteDays
			offered["backup_retention_days"] = h.signupCfg.BackupRetentionDays
		}
		out["plan"] = offered
	}
	jsonOK(w, out)
}

func (h *handler) selfServicePlan(ctx context.Context) (plan.Plan, bool, error) {
	return plan.SelfService(ctx, h.db.Control())
}

// signup serves POST /api/signup.
func (h *handler) signup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonErr(w, fmt.Errorf("method not allowed"), http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	if !h.signupCfg.Enabled {
		jsonErr(w, fmt.Errorf("self-service sign-up is not enabled on this deployment"), http.StatusServiceUnavailable)
		return
	}
	if h.kc == nil && !h.devMode {
		jsonErr(w, fmt.Errorf("sign-up is not available: the identity provider is not configured"), http.StatusServiceUnavailable)
		return
	}
	ip := clientIP(r)
	if !h.signupLimit.allow(ip) {
		jsonErr(w, fmt.Errorf("too many sign-up attempts from your address — try again in a few minutes"), http.StatusTooManyRequests)
		return
	}
	var req signupRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		jsonErr(w, fmt.Errorf("invalid body"), http.StatusBadRequest)
		return
	}
	req.Company, req.FirstName, req.LastName = strings.TrimSpace(req.Company), strings.TrimSpace(req.FirstName), strings.TrimSpace(req.LastName)
	req.Email = strings.ToLower(strings.TrimSpace(req.Email))
	if err := validateSignup(req); err != nil {
		jsonErr(w, err, http.StatusBadRequest)
		return
	}
	p, ok, err := h.selfServicePlan(ctx)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if !ok {
		jsonErr(w, fmt.Errorf("no edition is open for self-service sign-up"), http.StatusServiceUnavailable)
		return
	}

	// An address that already has an account signs in instead. Checked in
	// the control plane (every shared-mode user, and the directory of every
	// dedicated tenant) and at the identity provider.
	if exists, err := h.emailRegistered(ctx, req.Email); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	} else if exists {
		jsonErr(w, fmt.Errorf("an account with this e-mail address already exists — sign in instead"), http.StatusConflict)
		return
	}

	// Everything created from here on is undone if a later step fails.
	var undo []func()
	rollback := func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}
	fail := func(status int, err error) {
		rollback()
		jsonErr(w, err, status)
	}
	bg := context.WithoutCancel(ctx)

	// 1. The identity-provider account and its roles. Nothing else exists
	//    yet, so a refusal here costs nothing.
	sub := "signup-" + req.Email
	if h.kc != nil {
		created, err := h.kc.CreateUser(ctx, req.Email, req.FirstName, req.LastName)
		if err != nil {
			jsonErr(w, fmt.Errorf("create identity provider account: %w", err), http.StatusBadGateway)
			return
		}
		sub = created
		undo = append(undo, func() {
			if err := h.kc.DeleteUser(bg, sub); err != nil {
				h.log.Error().Err(err).Str("email", req.Email).Msg("sign-up rollback: identity provider account not removed")
			}
		})
		for _, role := range everyTenantRole {
			if err := h.kc.AssignRealmRole(ctx, sub, role); err != nil {
				fail(http.StatusBadGateway, fmt.Errorf("assign %s role in identity provider: %w", role, err))
				return
			}
		}
	}

	// 2. The tenant: its own database when the deployment runs that way.
	var customerID, workspaceID string
	tctx := ctx
	if h.db.Dedicated() {
		customerID, workspaceID, err = h.provisionTenant(ctx, req.Company, p.Key)
		if err != nil {
			fail(http.StatusInternalServerError, fmt.Errorf("create tenant: %w", err))
			return
		}
		undo = append(undo, func() {
			if err := h.db.Router().Deprovision(bg, customerID); err != nil {
				h.log.Error().Err(err).Str("tenant", customerID).Msg("sign-up rollback: tenant database not dropped")
			}
		})
		tctx = h.tenantCtx(ctx, customerID)
	} else {
		if err := h.db.QueryRow(ctx, `INSERT INTO core.customer (name, plan) VALUES ($1, $2) RETURNING id::text`, req.Company, p.Key).Scan(&customerID); err != nil {
			fail(http.StatusInternalServerError, fmt.Errorf("create tenant: %w", err))
			return
		}
		undo = append(undo, func() {
			// Applications first: the starter model's rows name the person
			// who "entered" them, so the user row can only go once the
			// model has.
			for _, stmt := range []string{
				`DELETE FROM core.application WHERE customer_id = $1::uuid`,
				`DELETE FROM identity.user WHERE customer_id = $1::uuid`,
				`DELETE FROM core.customer WHERE id = $1::uuid`,
			} {
				if _, err := h.db.Exec(bg, stmt, customerID); err != nil {
					h.log.Error().Err(err).Str("tenant", customerID).Str("stmt", stmt).Msg("sign-up rollback: statement failed")
				}
			}
		})
		if err := h.db.QueryRow(ctx, `INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Default') RETURNING id::text`, customerID).Scan(&workspaceID); err != nil {
			fail(http.StatusInternalServerError, fmt.Errorf("create workspace: %w", err))
			return
		}
	}
	if err := plan.SetPlan(tctx, h.db.For(tctx), customerID, p.Key); err != nil {
		fail(http.StatusInternalServerError, fmt.Errorf("assign edition: %w", err))
		return
	}

	// 3. The person, every tenant role, and the "Getting started"
	//    application, in one transaction on the tenant's database: all of
	//    them or none.
	var userID string
	var gs gettingStarted
	err = pgx.BeginFunc(tctx, h.db.For(tctx), func(tx pgx.Tx) error {
		if err := tx.QueryRow(tctx, `
			INSERT INTO identity.user (keycloak_sub, email, display_name, customer_id)
			VALUES ($1, $2, $3, $4::uuid) RETURNING id::text`,
			sub, req.Email, req.FirstName+" "+req.LastName, customerID).Scan(&userID); err != nil {
			return fmt.Errorf("create user: %w", err)
		}
		if err := grantEveryTenantRole(tctx, tx, userID, workspaceID, userID); err != nil {
			return err
		}
		var err error
		gs, err = installGettingStarted(tctx, tx, customerID, workspaceID, userID)
		return err
	})
	if err != nil {
		fail(http.StatusInternalServerError, err)
		return
	}
	appID, modelID := gs.appID, gs.landingModelID()
	h.noteUser(tctx, sub, req.Email)
	h.noteApplication(tctx, appID)
	h.recalcGettingStarted(tctx, gs)

	// 4. The invitation, which is what makes the account usable. Without
	//    it there is nothing to show for the sign-up, so its failure undoes
	//    all of the above.
	invited := false
	if h.kc != nil {
		if err := h.kc.SendInvite(ctx, sub, inviteLifetime); err != nil {
			fail(http.StatusBadGateway, fmt.Errorf("%w — nothing was created; the deployment's mail relay may be down", err))
			return
		}
		invited = true
	}

	meta := map[string]string{"email": req.Email, "company": req.Company, "plan": p.Key, "ip": ip, "application_id": appID,
		"model_id": modelID, "guide_model_ids": strings.Join(gs.guideModelIDs(), ",")}
	auditlog.Log(tctx, h.db.For(tctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryAdmin, EventType: auditlog.EventTenantSignedUp,
		ActorUserID: userID, ActorRole: "signup",
		ResourceType: "tenant", ResourceID: customerID, ApplicationID: appID,
		Metadata: meta,
	})
	h.log.Info().Str("tenant", customerID).Str("email", req.Email).Str("plan", p.Key).Msg("self-service sign-up")

	out := map[string]any{
		"status": "created", "invited": invited, "email": req.Email, "tenant_id": customerID,
		"application_id": appID, "model_id": modelID, "plan": p.Key,
	}
	if invited {
		out["status"] = "invited"
	}
	if h.devMode && h.kc == nil {
		// The dev stack has no identity provider: the new account is
		// reachable as an X-Dev-User persona instead of by invitation.
		out["dev_persona"] = sub
	}
	jsonOK(w, out)
}

func validateSignup(req signupRequest) error {
	switch {
	case len(req.Company) < 2 || len(req.Company) > signupMaxField:
		return fmt.Errorf("company name must be between 2 and %d characters", signupMaxField)
	case req.FirstName == "" || len(req.FirstName) > signupMaxField:
		return fmt.Errorf("first name is required")
	case req.LastName == "" || len(req.LastName) > signupMaxField:
		return fmt.Errorf("last name is required")
	case req.Email == "" || len(req.Email) > 254:
		return fmt.Errorf("e-mail address is required")
	}
	addr, err := mail.ParseAddress(req.Email)
	if err != nil || addr.Address != req.Email || !strings.Contains(strings.SplitN(req.Email, "@", 2)[1], ".") {
		return fmt.Errorf("that does not look like a valid e-mail address")
	}
	return nil
}

// emailRegistered reports whether an account with this address exists in
// the control plane, any dedicated tenant's directory, or the identity
// provider.
func (h *handler) emailRegistered(ctx context.Context, email string) (bool, error) {
	var n int
	if err := h.db.Control().QueryRow(ctx, `
		SELECT (SELECT count(*) FROM identity."user" WHERE lower(email) = $1)
		     + (SELECT count(*) FROM platform.user_directory WHERE lower(email) = $1)`, email).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return true, nil
	}
	if h.kc != nil {
		existing, err := h.kc.FindUserByEmail(ctx, email)
		if err != nil {
			return false, fmt.Errorf("look up identity provider account: %w", err)
		}
		return existing != "", nil
	}
	return false, nil
}

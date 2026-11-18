package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mavericks-engine/mavericks/internal/identity"
	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// Sign in with Google (owner, 2026-11-18). The realm brokers Google as the
// identity provider "google" (scripts/keycloak-google-setup.sh), whose first
// login links a Google account to the realm account with the same address —
// Google has verified it — or creates a realm account. A Google account the
// platform knows then signs in like anyone else. One it does not know is a
// self-service sign-up, on the form's terms (POST /api/signup): a tenant on
// the self-service edition with the "Getting started" application and every
// tenant role, throttled per address, and refused — with the reason — where
// sign-up is closed.

const googleIdPAlias = "google"

// googleOffer caches whether the realm offers Google sign-in, which the
// public sign-up and sign-in pages ask on every visit.
type googleOffer struct {
	mu      sync.Mutex
	offered bool
	until   time.Time
}

const googleOfferTTL = time.Minute

// googleSignInOffered reports whether the realm has the Google identity
// provider, enabled. Keycloak is the one place it is configured.
func (h *handler) googleSignInOffered(ctx context.Context) bool {
	if h.kc == nil {
		return false
	}
	h.google.mu.Lock()
	defer h.google.mu.Unlock()
	if time.Now().Before(h.google.until) {
		return h.google.offered
	}
	idp, err := h.kc.GetIdentityProvider(ctx, googleIdPAlias)
	if err != nil {
		// Asked again soon; meanwhile the button is not offered.
		h.log.Warn().Err(err).Msg("look up the Google identity provider")
		h.google.offered, h.google.until = false, time.Now().Add(googleOfferTTL/6)
		return false
	}
	h.google.offered, h.google.until = idp != nil && idp.Enabled, time.Now().Add(googleOfferTTL)
	return h.google.offered
}

// googleSignUp makes the account behind a valid token whose subject the
// platform has never seen, when that realm account came from Google: a
// self-service sign-up. Any other unknown subject answers "unknown subject",
// as before. A refusal deletes the realm account the first login made, so
// an invitation to the same address later is not blocked by it.
func (h *handler) googleSignUp(ctx context.Context, r *http.Request, claims *identity.Claims) (*actor, error) {
	if h.kc == nil {
		return nil, fmt.Errorf("unknown subject")
	}
	links, err := h.kc.FederatedIdentities(ctx, claims.Subject)
	if err != nil {
		return nil, fmt.Errorf("unknown subject")
	}
	fromGoogle := false
	for _, l := range links {
		fromGoogle = fromGoogle || l.IdentityProvider == googleIdPAlias
	}
	if !fromGoogle {
		return nil, fmt.Errorf("unknown subject")
	}
	email := strings.ToLower(strings.TrimSpace(claims.Email))
	if email == "" {
		return nil, &errNotProvisionable{"Google did not share your e-mail address, which this platform needs to create an account"}
	}

	// The console's first requests arrive together: one sign-up per Google
	// account, on every gateway replica.
	unlock, err := h.lockControl(ctx, "google-signup:"+claims.Subject)
	if err != nil {
		return nil, err
	}
	defer unlock()
	// …and the request that waited finds what the first one made.
	if a, err := h.actorByKeycloakSub(ctx, claims.Subject); err == nil {
		return a, nil
	}

	bg := context.WithoutCancel(ctx)
	refuse := func(reason string) (*actor, error) {
		if err := h.kc.DeleteUser(bg, claims.Subject); err != nil {
			h.log.Warn().Err(err).Str("email", email).Msg("Google sign-in refused: its realm account was not removed")
		}
		return nil, &errNotProvisionable{reason}
	}
	notHere := "No account here uses " + email + ". Ask your administrator to invite you, then sign in with Google."
	if !h.signupCfg.Enabled {
		return refuse(notHere)
	}
	p, ok, err := h.selfServicePlan(ctx)
	if err != nil {
		return nil, err
	}
	if !ok {
		return refuse(notHere)
	}
	if !h.signupLimit.allow(clientIP(r)) {
		return refuse("Too many sign-ups from your address — try again in a few minutes.")
	}
	// The realm found no account with this address to link, so the platform
	// should have none either; one left behind would be a second account.
	if taken, err := h.emailOnPlatform(ctx, email); err != nil {
		return nil, err
	} else if taken {
		return refuse("An account with " + email + " already exists. Sign in with your password.")
	}

	// From here on the sign-up finishes even if the browser gives up.
	ctx, cancel := context.WithTimeout(bg, signupProvisionTimeout)
	defer cancel()
	first, last := googleNames(claims, email)
	for _, role := range everyTenantRole {
		if err := h.kc.AssignRealmRole(ctx, claims.Subject, role); err != nil {
			return nil, fmt.Errorf("assign %s role in identity provider: %w", role, err)
		}
	}
	made, _, err := h.createSignupTenant(ctx, googleCompany(email, first, last), email, strings.TrimSpace(first+" "+last), claims.Subject, p)
	if err != nil {
		return nil, err
	}
	tctx := h.signupCtx(ctx, made.customerID)
	auditlog.Log(tctx, h.db.For(tctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryAdmin, EventType: auditlog.EventTenantSignedUp,
		ActorUserID: made.userID, ActorRole: "signup",
		ResourceType: "tenant", ResourceID: made.customerID, ApplicationID: made.gs.appID,
		Metadata: map[string]string{"email": email, "plan": p.Key, "ip": clientIP(r), "provider": googleIdPAlias,
			"application_id": made.gs.appID, "model_id": made.gs.landingModelID(), "guide_model_ids": strings.Join(made.gs.guideModelIDs(), ",")},
	})
	h.log.Info().Str("tenant", made.customerID).Str("email", email).Str("plan", p.Key).Msg("self-service sign-up with Google")
	return h.actorByKeycloakSub(tctx, claims.Subject)
}

// googleNames are the person's first and last names as Google gave them,
// else the name split at its first space, else the address's local part.
func googleNames(claims *identity.Claims, email string) (string, string) {
	first, last := strings.TrimSpace(claims.GivenName), strings.TrimSpace(claims.FamilyName)
	if first == "" && last == "" {
		first, last, _ = strings.Cut(strings.TrimSpace(claims.Name), " ")
	}
	if first == "" {
		first = email[:strings.Index(email, "@")]
	}
	return first, strings.TrimSpace(last)
}

// publicMailDomains are where a person's address says nothing about their
// company: a sign-up from one is named after the person instead.
var publicMailDomains = map[string]bool{
	"gmail.com": true, "googlemail.com": true, "outlook.com": true, "hotmail.com": true, "live.com": true,
	"yahoo.com": true, "icloud.com": true, "me.com": true, "proton.me": true, "protonmail.com": true,
	"gmx.com": true, "gmx.de": true, "mail.ru": true, "yandex.ru": true, "aol.com": true,
}

// googleCompany names the tenant of a Google sign-up, which has no company
// field: the address's domain ("acme.com"), or "First Last" for an address
// at a public mail service. Its tenant administrator renames it.
func googleCompany(email, first, last string) string {
	domain := email[strings.LastIndex(email, "@")+1:]
	name := domain
	if publicMailDomains[domain] {
		name = strings.TrimSpace(first + " " + last)
	}
	if len(name) > signupMaxField {
		name = name[:signupMaxField]
	}
	if len(name) < 2 {
		name = domain
	}
	return name
}

// lockControl holds a control-plane advisory lock on key until the returned
// function is called: one holder across every gateway replica.
func (h *handler) lockControl(ctx context.Context, key string) (func(), error) {
	conn, err := h.db.Control().Acquire(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(hashtextextended($1, 0))`, key); err != nil {
		conn.Release()
		return nil, err
	}
	return func() {
		if _, err := conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock(hashtextextended($1, 0))`, key); err != nil {
			// A session lock outlives a failed unlock: close the
			// connection rather than return it to the pool holding it.
			_ = conn.Conn().Close(context.WithoutCancel(ctx))
		}
		conn.Release()
	}, nil
}

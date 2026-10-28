package gateway

// Temporary passwords: how a deployment with no SMTP relay lets people in.
//
// Everywhere else a new account gets an invitation e-mail whose link lets its
// owner choose their password, so the credential never passes through anyone
// (keycloak.SendInvite). Without a relay that mail cannot be sent, and a
// self-hosted deployment could add nobody at all. With TEMPORARY_PASSWORDS the
// gateway sets a random password on the account instead, returns it once to
// the administrator who made the account, and Keycloak makes the person choose
// their own at their first sign-in.
//
// What that gives up, and why it is a deployment's opt-in and never the
// Playground's or a deployment with public sign-up (Deps.TemporaryPasswords):
// whoever hands the password over can sign in first, and the address is never
// proven. Such an account, added later to another tenant, is reachable by
// whoever knew its first password there. And an invitation's answer no longer
// looks the same for an address that already has an account — that one gets
// no password.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"

	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

// temporaryPasswordAlphabet leaves out characters read alike (0 O o, 1 l I),
// since the password is read out or retyped.
const temporaryPasswordAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789"

// newTemporaryPassword is four groups of four random characters joined by
// hyphens (about 92 bits), with an upper-case letter, a lower-case letter
// and a digit in it, so a realm password policy asking for those, a special
// character or up to 19 characters accepts it.
func newTemporaryPassword() (string, error) {
	n := big.NewInt(int64(len(temporaryPasswordAlphabet)))
	for {
		var b strings.Builder
		for i := range 16 {
			if i > 0 && i%4 == 0 {
				b.WriteByte('-')
			}
			k, err := rand.Int(rand.Reader, n)
			if err != nil {
				return "", fmt.Errorf("generate temporary password: %w", err)
			}
			b.WriteByte(temporaryPasswordAlphabet[k.Int64()])
		}
		pw := b.String()
		if strings.ContainsAny(pw, "ABCDEFGHJKLMNPQRSTUVWXYZ") &&
			strings.ContainsAny(pw, "abcdefghijkmnpqrstuvwxyz") &&
			strings.ContainsAny(pw, "23456789") {
			return pw, nil
		}
	}
}

// temporaryPasswordFor sets a temporary password on the identity-provider
// account sub and returns it, or returns "" when the account already signs
// in its own way. An account this request created is new; one it adopted
// gets a password only while nobody has signed in to it with one of their
// own (keycloak.AwaitsFirstSignIn).
func (h *handler) temporaryPasswordFor(ctx context.Context, sub string, created bool) (string, error) {
	if !created {
		awaits, err := h.kc.AwaitsFirstSignIn(ctx, sub)
		if err != nil || !awaits {
			return "", err
		}
	}
	pw, err := newTemporaryPassword()
	if err != nil {
		return "", err
	}
	if err := h.kc.SetTemporaryPassword(ctx, sub, pw); err != nil {
		return "", err
	}
	return pw, nil
}

// errOwnPassword refuses a new temporary password for an account that signs
// in its own way: whoever set it could sign in as the owner.
var errOwnPassword = errors.New("this person already signs in with a password of their own (or through another " +
	"identity provider), so no temporary password is given: on this deployment, which sends no e-mail, " +
	"the operator resets a forgotten password in the sign-in service's administration")

// adminTemporaryPassword serves POST /api/admin/users/{id}/temporary-password:
// a new temporary password for an account that has not signed in yet — one
// whose first was lost before it was used. Whoever may re-invite the account
// elsewhere may (accountPermissions.Reinvite); the list offers it as
// temporary_password (withUserPermissions).
func (h *handler) adminTemporaryPassword(ctx context.Context, w http.ResponseWriter, act *actor, tiers adminTiers, userID string) {
	if h.kc == nil || !h.tempPasswords {
		jsonErr(w, errors.New("this deployment gives new accounts an invitation e-mail, not a temporary password"), http.StatusConflict)
		return
	}
	if !h.accountActionAllowed(ctx, w, act, tiers, userID, "give a temporary password to",
		func(p accountPermissions) bool { return p.Reinvite }) {
		return
	}
	var sub, email string
	if err := h.db.QueryRow(ctx, `SELECT keycloak_sub, email FROM identity.user WHERE id = $1::uuid`, userID).Scan(&sub, &email); err != nil {
		jsonErr(w, fmt.Errorf("user not found"), http.StatusNotFound)
		return
	}
	if strings.HasPrefix(sub, "admin-created-") {
		jsonErr(w, fmt.Errorf("this user has no identity provider account (created before provisioning existed); "+
			"delete and re-create them to give them a temporary password"), http.StatusConflict)
		return
	}
	pw, err := h.temporaryPasswordFor(ctx, sub, false)
	if err != nil {
		jsonErr(w, err, http.StatusBadGateway)
		return
	}
	if pw == "" {
		jsonErr(w, errOwnPassword, http.StatusConflict)
		return
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryAdmin, EventType: auditlog.EventUserUpdated,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ResourceType: "identity_user", ResourceID: userID,
		Metadata: map[string]string{"action": "temporary_password_issued", "email": email},
	})
	jsonOK(w, map[string]string{"status": "ok", "email": email, "temporary_password": pw})
}

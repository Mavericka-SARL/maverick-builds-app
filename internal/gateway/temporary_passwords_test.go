// Tests that a deployment with no e-mail (TEMPORARY_PASSWORDS) can add people:
// a new account gets a temporary password instead of an invitation, and no
// account that already signs in its own way is ever given one.
package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"testing"
)

func withTemporaryPasswords(d *Deps) { d.TemporaryPasswords = true }

var temporaryPasswordShape = regexp.MustCompile(`^[A-HJ-NP-Za-km-np-z2-9]{4}(-[A-HJ-NP-Za-km-np-z2-9]{4}){3}$`)

func TestNewTemporaryPasswordShape(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		pw, err := newTemporaryPassword()
		if err != nil {
			t.Fatal(err)
		}
		if !temporaryPasswordShape.MatchString(pw) {
			t.Fatalf("password %q is not four groups of four unambiguous characters", pw)
		}
		if seen[pw] {
			t.Fatalf("password %q generated twice", pw)
		}
		seen[pw] = true
	}
}

// The reported case: a self-hosted deployment with no SMTP relay could add
// nobody, because the invitation e-mail failed and rolled the creation back.
func TestCreateUserGivesTemporaryPasswordWithoutEmail(t *testing.T) {
	idp := newFakeIDP(t)
	idp.failInvite = true // no relay: any invitation would fail
	f := setupProvFixture(t, idp, withTemporaryPasswords)

	status, body := f.do(t, "POST", "/api/admin/users", map[string]any{
		"email": "anna@temp.test", "first_name": "Anna", "last_name": "V", "role": "developer",
	})
	if status != http.StatusOK {
		t.Fatalf("create user = %d, body %v", status, body)
	}
	pw, _ := body["temporary_password"].(string)
	if !temporaryPasswordShape.MatchString(pw) {
		t.Fatalf("temporary_password = %q, want a generated password", body["temporary_password"])
	}
	if body["invited"] != false {
		t.Errorf("invited = %v, want false: no e-mail was sent", body["invited"])
	}
	sub := f.subFor(t, "anna@temp.test")
	if idp.passwords[sub] != pw || !idp.pending[sub] {
		t.Errorf("identity provider holds password %q (temporary %v), want the returned one, temporary",
			idp.passwords[sub], idp.pending[sub])
	}
	if len(idp.invited) != 0 {
		t.Errorf("invitations sent = %v, want none", idp.invited)
	}
}

// A refused password undoes the creation as a failed invitation does.
func TestCreateUserRollsBackWhenTemporaryPasswordIsRefused(t *testing.T) {
	idp := newFakeIDP(t)
	idp.failReset = true
	f := setupProvFixture(t, idp, withTemporaryPasswords)

	status, body := f.do(t, "POST", "/api/admin/users", map[string]any{
		"email": "refused@temp.test", "first_name": "Re", "last_name": "Fused", "role": "developer",
	})
	if status != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502; body %v", status, body)
	}
	if sub := f.subFor(t, "refused@temp.test"); sub != "" {
		t.Errorf("an application user row survived (sub %q)", sub)
	}
	if len(idp.users) != 0 {
		t.Errorf("identity provider still holds %v", idp.users)
	}
}

// An account that already exists and signs in with its own password is
// adopted without one: setting it would let the caller sign in as its owner.
// One left without a sign-in of its own (a half-finished creation) gets one.
func TestCreateUserGivesNoTemporaryPasswordToAnAccountThatSignsIn(t *testing.T) {
	idp := newFakeIDP(t)
	f := setupProvFixture(t, idp, withTemporaryPasswords)
	ctx := context.Background()

	owned, err := idp.client().CreateUser(ctx, "owned@temp.test", "Own", "Password")
	if err != nil {
		t.Fatal(err)
	}
	idp.passwords[owned] = "chosen-by-owner"
	status, body := f.do(t, "POST", "/api/admin/users", map[string]any{
		"email": "owned@temp.test", "first_name": "Own", "last_name": "Password", "role": "developer",
	})
	if status != http.StatusOK {
		t.Fatalf("create over an owned account = %d, body %v", status, body)
	}
	if _, ok := body["temporary_password"]; ok || idp.passwords[owned] != "chosen-by-owner" {
		t.Errorf("an account with its own password was given one: reply %v, password now %q", body, idp.passwords[owned])
	}

	bare, err := idp.client().CreateUser(ctx, "bare@temp.test", "Half", "Finished")
	if err != nil {
		t.Fatal(err)
	}
	status, body = f.do(t, "POST", "/api/admin/users", map[string]any{
		"email": "bare@temp.test", "first_name": "Half", "last_name": "Finished", "role": "developer",
	})
	if status != http.StatusOK || body["temporary_password"] == nil || idp.passwords[bare] != body["temporary_password"] {
		t.Errorf("an account with no sign-in of its own: status %d, reply %v, password %q", status, body, idp.passwords[bare])
	}
}

func TestReissueTemporaryPassword(t *testing.T) {
	idp := newFakeIDP(t)
	f := setupProvFixture(t, idp, withTemporaryPasswords)

	_, body := f.do(t, "POST", "/api/admin/users", map[string]any{
		"email": "lost@temp.test", "first_name": "Lost", "last_name": "It", "role": "developer",
	})
	userID, _ := body["id"].(string)
	first, _ := body["temporary_password"].(string)
	sub := f.subFor(t, "lost@temp.test")

	// Lost before it was used: a new one replaces it.
	status, out := f.do(t, "POST", "/api/admin/users/"+userID+"/temporary-password", nil)
	if status != http.StatusOK {
		t.Fatalf("reissue = %d, body %v", status, out)
	}
	second, _ := out["temporary_password"].(string)
	if second == "" || second == first || idp.passwords[sub] != second || !idp.pending[sub] {
		t.Errorf("reissued %q (first %q); identity provider holds %q, temporary %v", second, first, idp.passwords[sub], idp.pending[sub])
	}

	// Once they have chosen their own, it is theirs.
	idp.passwords[sub], idp.pending[sub] = "chosen-by-owner", false
	status, out = f.do(t, "POST", "/api/admin/users/"+userID+"/temporary-password", nil)
	if status != http.StatusConflict || idp.passwords[sub] != "chosen-by-owner" {
		t.Errorf("reissue over an own password = %d (%v), password now %q", status, out, idp.passwords[sub])
	}

	// Nor to someone who signs in through another identity provider.
	idp.passwords[sub] = ""
	idp.linked[sub] = true
	if status, out = f.do(t, "POST", "/api/admin/users/"+userID+"/temporary-password", nil); status != http.StatusConflict {
		t.Errorf("reissue for a brokered account = %d (%v), want 409", status, out)
	}
}

// Without the setting, and wherever tenants do not trust one another, the
// invitation e-mail stays the only way in.
func TestTemporaryPasswordsOnlyWhereOptedIn(t *testing.T) {
	for name, opt := range map[string]func(*Deps){
		"not set":    func(*Deps) {},
		"sign-up":    func(d *Deps) { d.TemporaryPasswords, d.Signup.Enabled = true, true },
		"playground": func(d *Deps) { d.TemporaryPasswords, d.Signup.Playground = true, true },
	} {
		t.Run(name, func(t *testing.T) {
			idp := newFakeIDP(t)
			f := setupProvFixture(t, idp, opt)
			status, body := f.do(t, "POST", "/api/admin/users", map[string]any{
				"email": "invited@temp.test", "first_name": "In", "last_name": "Vited", "role": "developer",
			})
			if status != http.StatusOK || body["invited"] != true || body["temporary_password"] != nil || len(idp.invited) != 1 {
				t.Fatalf("create = %d, body %v, invitations %v: want an invitation and no password", status, body, idp.invited)
			}
			userID, _ := body["id"].(string)
			if status, out := f.do(t, "POST", "/api/admin/users/"+userID+"/temporary-password", nil); status != http.StatusConflict {
				t.Errorf("reissue = %d (%v), want 409", status, out)
			}
			if len(idp.passwords) != 0 {
				t.Errorf("passwords set: %v", idp.passwords)
			}
		})
	}
}

// The list offers the new password where it would offer a new invitation.
func TestUserListOffersTemporaryPasswordInsteadOfReinvite(t *testing.T) {
	idp := newFakeIDP(t)
	f := setupProvFixture(t, idp, withTemporaryPasswords)
	_, _ = f.do(t, "POST", "/api/admin/users", map[string]any{
		"email": "listed@temp.test", "first_name": "Li", "last_name": "Sted", "role": "developer",
	})

	req, _ := http.NewRequestWithContext(context.Background(), "GET", f.srv.URL+"/api/admin/users", nil)
	req.Header.Set("X-Dev-User", f.adminSub)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var users []struct {
		Email       string             `json:"email"`
		Permissions accountPermissions `json:"permissions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&users); err != nil {
		t.Fatal(err)
	}
	for _, u := range users {
		if u.Email == "listed@temp.test" {
			if !u.Permissions.TemporaryPassword || u.Permissions.Reinvite {
				t.Errorf("permissions = %+v, want temporary_password and no reinvite", u.Permissions)
			}
			return
		}
	}
	t.Fatalf("listed@temp.test not in the list")
}

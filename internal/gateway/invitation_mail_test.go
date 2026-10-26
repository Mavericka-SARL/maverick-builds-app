package gateway

import (
	"encoding/json"
	"net/http"
	"slices"
	"testing"
)

// An invitation mail says who it is from (the realm's email theme reads the
// account's mvx_* attributes): an administrator's names them and the
// tenant, again when they resend it; a sign-up's says the person signed up
// (TestSignupWithIdentityProviderAndRollback). Marking the account must
// keep its name and address: Keycloak takes an update naming attributes as
// the whole profile.
func TestAnAdministratorsInvitationSaysWhoSentIt(t *testing.T) {
	f := newDedicatedFixture(t, Deps{})
	tenant, ws, _ := f.tenant(t, "Acme & Co")
	as := call{persona: "held-pa", tenant: tenant}

	code, body := do(t, f.srv, as, http.MethodPost, "/api/admin/users", map[string]string{
		"email": "ivy@acme.test", "first_name": "Ivy", "last_name": "Invited", "role": "business_user", "workspace_id": ws})
	if code != http.StatusOK {
		t.Fatalf("invite: %d %s", code, body)
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	marked := func(when string) {
		t.Helper()
		f.broker.mu.Lock()
		defer f.broker.mu.Unlock()
		var u *fakeKCUser
		for _, c := range f.broker.users {
			if c.Email == "ivy@acme.test" {
				u = c
			}
		}
		if u == nil {
			t.Fatalf("%s: no account for ivy@acme.test (its name or address was cleared?)", when)
		}
		if u.First != "Ivy" || u.Last != "Invited" {
			t.Errorf("%s: name %q %q, want Ivy Invited", when, u.First, u.Last)
		}
		a := u.Attributes
		if !slices.Equal(a["mvx_invitation"], []string{"invite"}) || len(a["mvx_invited_by"]) != 1 || a["mvx_invited_by"][0] == "" ||
			!slices.Equal(a["mvx_workspace"], []string{"Acme & Co"}) {
			t.Errorf("%s: attributes %v, want an invitation from the platform admin to Acme & Co", when, a)
		}
	}
	marked("invited")

	if code, body := do(t, f.srv, as, http.MethodPost, "/api/admin/users/"+out.ID+"/invite", nil); code != http.StatusOK {
		t.Fatalf("resend: %d %s", code, body)
	}
	marked("resent")
	f.broker.mu.Lock()
	n := len(f.broker.invited)
	f.broker.mu.Unlock()
	if n != 2 {
		t.Errorf("invitation mails: %d, want 2", n)
	}
}

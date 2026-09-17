package keycloak

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeKeycloak stands in for the admin API. It asserts the shapes this package
// actually sends, so a change that would break against a real server fails
// here rather than in production.
type fakeKeycloak struct {
	srv         *httptest.Server
	tokenIssued int32
	tokenTTL    int
	created     []User
	roleGrants  map[string][]string
	invites     map[string]string // sub -> raw query string of the invite call
	deleted     []string
	usersByMail map[string]string // email -> sub
	failCreate  bool
	failInvite  bool
}

func newFakeKeycloak(t *testing.T) *fakeKeycloak {
	t.Helper()
	f := &fakeKeycloak{
		tokenTTL:    60,
		roleGrants:  map[string][]string{},
		invites:     map[string]string{},
		usersByMail: map[string]string{},
	}
	mux := http.NewServeMux()

	mux.HandleFunc("/realms/test/protocol/openid-connect/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "client_credentials" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Form.Get("client_secret") != "s3cret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		atomic.AddInt32(&f.tokenIssued, 1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "fake-admin-token", "expires_in": f.tokenTTL,
		})
	})

	mux.HandleFunc("/admin/realms/test/users", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fake-admin-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodGet:
			email := r.URL.Query().Get("email")
			w.Header().Set("Content-Type", "application/json")
			if sub, ok := f.usersByMail[email]; ok {
				_ = json.NewEncoder(w).Encode([]User{{ID: sub, Email: email}})
				return
			}
			_ = json.NewEncoder(w).Encode([]User{})
		case http.MethodPost:
			if f.failCreate {
				http.Error(w, `{"errorMessage":"User exists with same username"}`, http.StatusConflict)
				return
			}
			var u User
			_ = json.NewDecoder(r.Body).Decode(&u)
			sub := fmt.Sprintf("sub-%d", len(f.created)+1)
			f.created = append(f.created, u)
			f.usersByMail[u.Email] = sub
			w.Header().Set("Location", f.srv.URL+"/admin/realms/test/users/"+sub)
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/admin/realms/test/roles/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/admin/realms/test/roles/")
		if name != "developer" && name != "platform_admin" {
			http.Error(w, `{"error":"Could not find role"}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"id": "role-" + name, "name": name})
	})

	mux.HandleFunc("/admin/realms/test/users/", func(w http.ResponseWriter, r *http.Request) {
		rest := strings.TrimPrefix(r.URL.Path, "/admin/realms/test/users/")
		switch {
		case strings.HasSuffix(rest, "/role-mappings/realm"):
			sub := strings.TrimSuffix(rest, "/role-mappings/realm")
			var roles []map[string]string
			_ = json.NewDecoder(r.Body).Decode(&roles)
			for _, role := range roles {
				f.roleGrants[sub] = append(f.roleGrants[sub], role["name"])
			}
			w.WriteHeader(http.StatusNoContent)
		case strings.HasSuffix(rest, "/execute-actions-email"):
			if f.failInvite {
				http.Error(w, `{"errorMessage":"Failed to send execute actions email"}`, http.StatusInternalServerError)
				return
			}
			sub := strings.TrimSuffix(rest, "/execute-actions-email")
			var actions []string
			_ = json.NewDecoder(r.Body).Decode(&actions)
			f.invites[sub] = r.URL.RawQuery + "|actions=" + strings.Join(actions, ",")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete:
			f.deleted = append(f.deleted, rest)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})

	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeKeycloak) client(consoleURL string) *Client {
	return New(f.srv.URL, "test", "mavericks-admin", "s3cret", consoleURL)
}

func TestCreateUserReturnsSubjectFromLocation(t *testing.T) {
	f := newFakeKeycloak(t)
	c := f.client("https://console.example.com")

	sub, err := c.CreateUser(context.Background(), "ana@example.com", "Ana", "María Gómez")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if sub != "sub-1" {
		t.Errorf("sub = %q, want %q", sub, "sub-1")
	}
	got := f.created[0]
	if got.Username != "ana@example.com" || got.Email != "ana@example.com" {
		t.Errorf("username/email = %q/%q, want the address in both", got.Username, got.Email)
	}
	if !got.Enabled {
		t.Error("account created disabled; the invitation flow cannot enable it")
	}
	// The invitation's VERIFY_EMAIL action is what proves the address.
	if got.EmailVerified {
		t.Error("emailVerified = true, which skips the verification the invite performs")
	}
	// A multi-part surname must survive rather than being truncated.
	if got.FirstName != "Ana" || got.LastName != "María Gómez" {
		t.Errorf("name split = %q / %q, want %q / %q", got.FirstName, got.LastName, "Ana", "María Gómez")
	}
}

// A created account must carry no credential — otherwise a provisioning that
// fails partway leaves a working login nobody intended to hand out.
func TestCreateUserSetsNoCredential(t *testing.T) {
	f := newFakeKeycloak(t)
	c := f.client("")

	if _, err := c.CreateUser(context.Background(), "no-pw@example.com", "No", "Password"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	// User has no credentials field at all; assert the serialized request never
	// carried one, since a zero-value struct field would be easy to add later.
	b, _ := json.Marshal(f.created[0])
	if strings.Contains(string(b), "credential") {
		t.Errorf("create payload mentions credentials: %s", b)
	}
}

func TestAssignRealmRole(t *testing.T) {
	f := newFakeKeycloak(t)
	c := f.client("")
	ctx := context.Background()

	sub, err := c.CreateUser(ctx, "dev@example.com", "Dev", "Eloper")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := c.AssignRealmRole(ctx, sub, "developer"); err != nil {
		t.Fatalf("AssignRealmRole: %v", err)
	}
	if got := f.roleGrants[sub]; len(got) != 1 || got[0] != "developer" {
		t.Errorf("role grants = %v, want [developer]", got)
	}
}

func TestAssignRealmRoleUnknownRoleFails(t *testing.T) {
	f := newFakeKeycloak(t)
	c := f.client("")
	ctx := context.Background()

	sub, _ := c.CreateUser(ctx, "x@example.com", "X", "Y")
	err := c.AssignRealmRole(ctx, sub, "not_a_role")
	if err == nil {
		t.Fatal("AssignRealmRole accepted a role that does not exist in the realm")
	}
	if !strings.Contains(err.Error(), "not_a_role") {
		t.Errorf("error = %v, want it to name the missing role", err)
	}
}

func TestSendInviteRequestsPasswordAndVerification(t *testing.T) {
	f := newFakeKeycloak(t)
	c := f.client("https://console.example.com")
	ctx := context.Background()

	sub, _ := c.CreateUser(ctx, "invitee@example.com", "In", "Vitee")
	if err := c.SendInvite(ctx, sub, 48*time.Hour); err != nil {
		t.Fatalf("SendInvite: %v", err)
	}
	got := f.invites[sub]
	for _, want := range []string{
		"UPDATE_PASSWORD", "VERIFY_EMAIL",
		"redirect_uri=https%3A%2F%2Fconsole.example.com%2F",
		"client_id=mavericks-web",
		"lifespan=172800",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("invite call %q missing %q", got, want)
		}
	}
}

// redirect_uri without client_id is rejected by Keycloak, so an unset console
// URL must omit both rather than sending a half-specified redirect.
func TestSendInviteOmitsRedirectWhenConsoleURLUnset(t *testing.T) {
	f := newFakeKeycloak(t)
	c := f.client("")
	ctx := context.Background()

	sub, _ := c.CreateUser(ctx, "no-redirect@example.com", "N", "R")
	if err := c.SendInvite(ctx, sub, 0); err != nil {
		t.Fatalf("SendInvite: %v", err)
	}
	got := f.invites[sub]
	if strings.Contains(got, "redirect_uri") || strings.Contains(got, "client_id") {
		t.Errorf("invite call %q sent a redirect despite no console URL", got)
	}
}

// An unsendable invitation must surface. Silently swallowing it produces an
// account nobody can reach that looks identical to an ignored email.
func TestSendInviteSurfacesSMTPFailure(t *testing.T) {
	f := newFakeKeycloak(t)
	f.failInvite = true
	c := f.client("https://console.example.com")
	ctx := context.Background()

	sub, _ := c.CreateUser(ctx, "smtp-down@example.com", "S", "D")
	err := c.SendInvite(ctx, sub, 0)
	if err == nil {
		t.Fatal("SendInvite reported success when Keycloak could not send the mail")
	}
	if !strings.Contains(err.Error(), "invitation") {
		t.Errorf("error = %v, want it to mention the invitation", err)
	}
}

func TestFindUserByEmail(t *testing.T) {
	f := newFakeKeycloak(t)
	c := f.client("")
	ctx := context.Background()

	sub, err := c.FindUserByEmail(ctx, "nobody@example.com")
	if err != nil {
		t.Fatalf("FindUserByEmail: %v", err)
	}
	if sub != "" {
		t.Errorf("sub = %q for a missing user, want empty (not an error)", sub)
	}

	created, _ := c.CreateUser(ctx, "someone@example.com", "Someone", "Surname")
	found, err := c.FindUserByEmail(ctx, "someone@example.com")
	if err != nil {
		t.Fatalf("FindUserByEmail: %v", err)
	}
	if found != created {
		t.Errorf("found %q, want %q", found, created)
	}
}

func TestDeleteUser(t *testing.T) {
	f := newFakeKeycloak(t)
	c := f.client("")
	ctx := context.Background()

	sub, _ := c.CreateUser(ctx, "gone@example.com", "Gone", "Surname")
	if err := c.DeleteUser(ctx, sub); err != nil {
		t.Fatalf("DeleteUser: %v", err)
	}
	if len(f.deleted) != 1 || f.deleted[0] != sub {
		t.Errorf("deleted = %v, want [%s]", f.deleted, sub)
	}
}

// The admin token is reused across calls; Keycloak's default is 60s and a
// fresh token per request would triple the traffic for every user created.
func TestAccessTokenIsCached(t *testing.T) {
	f := newFakeKeycloak(t)
	c := f.client("")
	ctx := context.Background()

	for i := 0; i < 4; i++ {
		if _, err := c.FindUserByEmail(ctx, "cache@example.com"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if n := atomic.LoadInt32(&f.tokenIssued); n != 1 {
		t.Errorf("token requests = %d, want 1 (token is not being cached)", n)
	}
}

// A token whose remaining life is inside the refresh margin must be replaced,
// or a long-running request can present one that expires mid-flight.
func TestAccessTokenRefreshesInsideMargin(t *testing.T) {
	f := newFakeKeycloak(t)
	f.tokenTTL = 20 // shorter than the 30s margin, so every call must refresh
	c := f.client("")
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := c.FindUserByEmail(ctx, "short@example.com"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if n := atomic.LoadInt32(&f.tokenIssued); n != 3 {
		t.Errorf("token requests = %d, want 3 (near-expiry token was reused)", n)
	}
}

func TestBadClientSecretIsReported(t *testing.T) {
	f := newFakeKeycloak(t)
	c := New(f.srv.URL, "test", "mavericks-admin", "wrong", "")

	_, err := c.FindUserByEmail(context.Background(), "a@b.c")
	if err == nil {
		t.Fatal("expected an error with the wrong client secret")
	}
	if !strings.Contains(err.Error(), "KEYCLOAK_ADMIN_CLIENT_SECRET") {
		t.Errorf("error = %v, want it to name the setting to check", err)
	}
}

func TestCreateUserSurfacesConflict(t *testing.T) {
	f := newFakeKeycloak(t)
	f.failCreate = true
	c := f.client("")

	_, err := c.CreateUser(context.Background(), "dupe@example.com", "Dupe", "Surname")
	if err == nil {
		t.Fatal("CreateUser reported success on a 409")
	}
	if !strings.Contains(err.Error(), "User exists") {
		t.Errorf("error = %v, want Keycloak's own message preserved", err)
	}
}

func TestSplitDisplayName(t *testing.T) {
	for _, tc := range []struct{ in, first, last string }{
		{"", "", ""},
		{"Cher", "Cher", ""},
		{"Ada Lovelace", "Ada", "Lovelace"},
		{"Ana María Gómez", "Ana", "María Gómez"},
		{"  Grace Hopper  ", "Grace", "Hopper"},
	} {
		first, last := SplitDisplayName(tc.in)
		if first != tc.first || last != tc.last {
			t.Errorf("SplitDisplayName(%q) = %q/%q, want %q/%q", tc.in, first, last, tc.first, tc.last)
		}
	}
}

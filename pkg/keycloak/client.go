// Package keycloak is a minimal admin client covering exactly what the
// console needs to manage users: create, look up, assign a realm role, send an
// invitation, and delete.
//
// It exists because creating a user in the console used to write only an
// application-side row, with a synthetic keycloak_sub of "admin-created-<email>".
// In dev that string doubles as an X-Dev-User persona, so the flow appeared to
// work. Under real authentication the token's `sub` is a UUID that can never
// equal it, so every console-created user got a bare 401 and the console gave
// no hint anything was wrong.
//
// Deliberately not a general-purpose Keycloak SDK. A handful of typed calls
// over net/http is easier to audit than a dependency that can do everything,
// and it keeps the blast radius of the credentials this holds obvious.
package keycloak

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Client talks to one realm's admin API as a service account.
//
// The service account is a confidential client granted only manage-users and
// view-users on realm-management — NOT the master realm administrator. A
// gateway compromised with master admin credentials owns every realm on the
// server, including the one protecting the admin console itself.
type Client struct {
	baseURL      string // in-cluster address, e.g. http://keycloak:8080
	realm        string
	clientID     string
	clientSecret string
	consoleURL   string // where an invitation link should land
	http         *http.Client

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

// New builds a client. All arguments are required except consoleURL, which is
// only used for invitation redirects; without it Keycloak falls back to the
// client's own configured base URL.
func New(baseURL, realm, clientID, clientSecret, consoleURL string) *Client {
	return &Client{
		baseURL:      strings.TrimSuffix(baseURL, "/"),
		realm:        realm,
		clientID:     clientID,
		clientSecret: clientSecret,
		consoleURL:   strings.TrimSuffix(consoleURL, "/"),
		http:         &http.Client{Timeout: 15 * time.Second},
	}
}

// User is the subset of Keycloak's user representation this package uses.
type User struct {
	ID            string `json:"id,omitempty"`
	Username      string `json:"username"`
	Email         string `json:"email"`
	FirstName     string `json:"firstName,omitempty"`
	LastName      string `json:"lastName,omitempty"`
	Enabled       bool   `json:"enabled"`
	EmailVerified bool   `json:"emailVerified"`
}

// accessToken returns a cached service-account token, refreshing when it is
// within 30s of expiry. Keycloak's default admin token lives 60s, so caching
// without a margin would routinely hand out a token that expires mid-request.
func (c *Client) accessToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.tokenExp.Add(-30*time.Second)) {
		return c.token, nil
	}

	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.clientID},
		"client_secret": {c.clientSecret},
	}
	endpoint := fmt.Sprintf("%s/realms/%s/protocol/openid-connect/token", c.baseURL, c.realm)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("request service-account token: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("service-account token: %s (is KEYCLOAK_ADMIN_CLIENT_SECRET correct?)", resp.Status)
	}

	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("service-account token response contained no access_token")
	}
	c.token = out.AccessToken
	c.tokenExp = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	return c.token, nil
}

// result is what callers need from a response once its body has been consumed.
// Returning this rather than *http.Response means do always owns closing the
// body — with the response escaping, closing was split between do (when it
// decoded a body) and each caller (when it did not), which is the kind of
// divided ownership that eventually leaks one.
type result struct {
	StatusCode int
	// Location carries a created resource's URL; Keycloak returns the new
	// user's subject only in this header, with no response body.
	Location string
}

// do issues an authenticated admin-API request. body may be nil. The response
// body is decoded into out when out is non-nil, and closed either way.
func (c *Client) do(ctx context.Context, method, path string, body, out any) (result, error) {
	tok, err := c.accessToken(ctx)
	if err != nil {
		return result{}, err
	}

	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return result{}, fmt.Errorf("marshal request body: %w", err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}

	u := fmt.Sprintf("%s/admin/realms/%s%s", c.baseURL, c.realm, path)
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return result{}, fmt.Errorf("build %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return result{}, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	res := result{StatusCode: resp.StatusCode, Location: resp.Header.Get("Location")}

	if resp.StatusCode >= 300 {
		buf := new(bytes.Buffer)
		_, _ = buf.ReadFrom(resp.Body)
		return res, fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(buf.String()))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return res, fmt.Errorf("decode %s %s response: %w", method, path, err)
		}
	}
	return res, nil
}

// FindUserByEmail returns the Keycloak subject for an email, or "" if no such
// user exists. Used to make user creation idempotent: an account that already
// exists in Keycloak but not in the application database is a normal state
// after a partial failure, and must not become a hard error.
func (c *Client) FindUserByEmail(ctx context.Context, email string) (string, error) {
	var users []User
	path := "/users?exact=true&email=" + url.QueryEscape(email)
	if _, err := c.do(ctx, http.MethodGet, path, nil, &users); err != nil {
		return "", err
	}
	if len(users) == 0 {
		return "", nil
	}
	return users[0].ID, nil
}

// CreateUser creates an enabled account with no credentials and returns its
// subject. No password is set on purpose — the account is unusable until the
// invitation from SendInvite is completed, so a half-finished provisioning
// never leaves a login that works.
//
// emailVerified is false: the invitation's VERIFY_EMAIL action is what proves
// the address, and marking it verified up front would skip that.
func (c *Client) CreateUser(ctx context.Context, email, firstName, lastName string) (string, error) {
	first, last := strings.TrimSpace(firstName), strings.TrimSpace(lastName)
	res, err := c.do(ctx, http.MethodPost, "/users", User{
		Username:      email,
		Email:         email,
		FirstName:     first,
		LastName:      last,
		Enabled:       true,
		EmailVerified: false,
	}, nil)
	if err != nil {
		return "", err
	}

	// Keycloak returns 201 with the new user's URL in Location and no body.
	if loc := res.Location; loc != "" {
		if i := strings.LastIndex(loc, "/"); i >= 0 && i+1 < len(loc) {
			return loc[i+1:], nil
		}
	}
	// Some proxies strip Location; fall back to a lookup rather than failing a
	// creation that actually succeeded.
	return c.FindUserByEmail(ctx, email)
}

// AssignRealmRole grants a realm role by name. Roles the application uses map
// one-to-one onto identity.user_role.
func (c *Client) AssignRealmRole(ctx context.Context, sub, role string) error {
	var r struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if _, err := c.do(ctx, http.MethodGet, "/roles/"+url.PathEscape(role), nil, &r); err != nil {
		return fmt.Errorf("look up realm role %q: %w", role, err)
	}
	if _, err := c.do(ctx, http.MethodPost, "/users/"+url.PathEscape(sub)+"/role-mappings/realm",
		[]map[string]string{{"id": r.ID, "name": r.Name}}, nil); err != nil {
		return fmt.Errorf("assign realm role %q: %w", role, err)
	}
	return nil
}

// SendInvite emails a set-your-password link. This is the whole point of
// provisioning through Keycloak rather than handing out a generated password:
// the credential is chosen by its owner and never transits this system.
//
// Requires SMTP configured on the realm. Without it Keycloak returns an error
// rather than failing silently, which callers should surface — an invitation
// that was never sent looks exactly like one the recipient ignored.
func (c *Client) SendInvite(ctx context.Context, sub string, lifetime time.Duration) error {
	q := url.Values{}
	if c.consoleURL != "" {
		// Both are required together; Keycloak ignores redirect_uri without
		// a client_id and rejects a redirect the client does not permit.
		q.Set("redirect_uri", c.consoleURL+"/")
		q.Set("client_id", "mavericks-web")
	}
	if lifetime > 0 {
		q.Set("lifespan", fmt.Sprintf("%d", int(lifetime.Seconds())))
	}
	path := "/users/" + url.PathEscape(sub) + "/execute-actions-email"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	if _, err := c.do(ctx, http.MethodPut, path, []string{"UPDATE_PASSWORD", "VERIFY_EMAIL"}, nil); err != nil {
		return fmt.Errorf("send invitation email: %w", err)
	}
	return nil
}

// DeleteUser removes the account. Used both when the console deletes a user
// and to undo a half-finished provisioning, so a failure partway through does
// not strand an orphan account that nothing in the application knows about.
func (c *Client) DeleteUser(ctx context.Context, sub string) error {
	_, err := c.do(ctx, http.MethodDelete, "/users/"+url.PathEscape(sub), nil, nil)
	return err
}

// SplitDisplayName maps a single display name onto Keycloak's first/last
// fields. Everything after the first space becomes the surname, so "Ana María
// Gómez" keeps "María Gómez" together rather than dropping a name part.
func SplitDisplayName(displayName string) (first, last string) {
	dn := strings.TrimSpace(displayName)
	if dn == "" {
		return "", ""
	}
	if i := strings.Index(dn, " "); i > 0 {
		return dn[:i], strings.TrimSpace(dn[i+1:])
	}
	return dn, ""
}

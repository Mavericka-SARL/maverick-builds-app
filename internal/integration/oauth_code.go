package integration

// OAuth 2.0 authorization code (RFC 6749 §4.1, with PKCE, RFC 7636) for a
// connection. Decided 2026-09-21: the connection is what gets connected —
// one developer consents at the provider on the tenant's behalf, the
// tokens belong to the connection, and scheduled runs use them exactly as
// they use a bearer token; this matches how scheduled runs already act
// under developer visibility rather than any one person's.
//
// Public meta (what the console shows, what the browser is sent to):
//   authorization_url, token_url, scope, client_id, token_client_auth
//   ("basic", the default, or "post"), connected_at, connected_by.
// Sealed secret:
//   client_secret, access_token, refresh_token, expires_at (RFC 3339).

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// AuthTypeOAuthCode is the connection auth_type this file serves.
const AuthTypeOAuthCode = "oauth2_authorization_code"

// oauthStateTTL bounds one consent round trip.
const oauthStateTTL = 15 * time.Minute

// ErrOAuthNotConnected says the connection has a client but no tokens yet.
var ErrOAuthNotConnected = errors.New("the connection has not been authorised yet — press Connect")

// OAuthCodeMeta is the public half, as stored in Connection.Meta.
type OAuthCodeMeta struct {
	AuthorizationURL string `json:"authorization_url"`
	TokenURL         string `json:"token_url"`
	Scope            string `json:"scope,omitempty"`
	ClientID         string `json:"client_id"`
	// TokenClientAuth is how the client authenticates at the token
	// endpoint: "basic" (HTTP Basic, the RFC default) or "post" (client_id
	// and client_secret in the form) for providers that only take the latter.
	TokenClientAuth string `json:"token_client_auth,omitempty"`
	ConnectedAt     string `json:"connected_at,omitempty"`
	ConnectedBy     string `json:"connected_by,omitempty"`
}

// ValidateOAuthCodeMeta checks the fields a start needs, with the same
// destination rules as any connector URL.
func ValidateOAuthCodeMeta(meta json.RawMessage, allowInsecure bool) (OAuthCodeMeta, error) {
	var m OAuthCodeMeta
	if len(meta) > 0 {
		if err := json.Unmarshal(meta, &m); err != nil {
			return m, fmt.Errorf("meta: %w", err)
		}
	}
	if m.ClientID == "" {
		return m, errors.New("client_id is required")
	}
	if _, err := ValidateURL(m.AuthorizationURL, allowInsecure); err != nil {
		return m, fmt.Errorf("authorization_url: %w", err)
	}
	if _, err := ValidateURL(m.TokenURL, allowInsecure); err != nil {
		return m, fmt.Errorf("token_url: %w", err)
	}
	switch m.TokenClientAuth {
	case "", "basic", "post":
	default:
		return m, fmt.Errorf("token_client_auth must be basic or post")
	}
	return m, nil
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// StartOAuth records a pending authorisation and returns the URL the
// browser is sent to. redirectURI is the gateway's public callback.
func (s *Store) StartOAuth(ctx context.Context, appID, connectionID, userID, redirectURI, returnTo string, allowInsecure bool) (string, error) {
	conn, err := s.GetConnection(ctx, appID, connectionID)
	if err != nil {
		return "", err
	}
	if conn.AuthType != AuthTypeOAuthCode {
		return "", fmt.Errorf("connection %q is not an OAuth authorization-code connection", conn.Name)
	}
	meta, err := ValidateOAuthCodeMeta(conn.Meta, allowInsecure)
	if err != nil {
		return "", err
	}
	state, err := randomToken(24)
	if err != nil {
		return "", err
	}
	verifier, err := randomToken(48)
	if err != nil {
		return "", err
	}
	if _, err := s.pool.Exec(ctx, `
		INSERT INTO model.integration_oauth_state (state, connection_id, application_id, user_id, code_verifier, return_to, expires_at)
		VALUES ($1, $2::uuid, $3::uuid, NULLIF($4,'')::uuid, $5, $6, now() + $7::interval)`,
		state, connectionID, appID, userID, verifier, returnTo, oauthStateTTL.String()); err != nil {
		return "", fmt.Errorf("record oauth state: %w", err)
	}
	// Any query the developer put on the authorization URL (a provider's
	// own parameters, such as access_type=offline) is kept.
	u, _ := url.Parse(meta.AuthorizationURL)
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", meta.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	if meta.Scope != "" {
		q.Set("scope", meta.Scope)
	}
	sum := sha256.Sum256([]byte(verifier))
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// OAuthCallbackResult is what the callback learned, for the redirect back
// to the console.
type OAuthCallbackResult struct {
	ConnectionID  string
	ApplicationID string
	ReturnTo      string
}

// CompleteOAuth is the callback: it matches the state, exchanges the code
// at the provider's token endpoint with the connection's client secret,
// and seals the tokens into the connection. The state is consumed whether
// or not the exchange succeeds — a code is single-use anyway.
func (s *Store) CompleteOAuth(ctx context.Context, client *http.Client, state, code, redirectURI, connectedBy string, allowInsecure bool) (OAuthCallbackResult, error) {
	var res OAuthCallbackResult
	var verifier string
	var userID *string
	err := s.pool.QueryRow(ctx, `
		DELETE FROM model.integration_oauth_state
		WHERE state = $1 AND expires_at > now()
		RETURNING connection_id::text, application_id::text, user_id::text, code_verifier, return_to`, state,
	).Scan(&res.ConnectionID, &res.ApplicationID, &userID, &verifier, &res.ReturnTo)
	if errors.Is(err, pgx.ErrNoRows) {
		return res, errors.New("this authorisation is unknown or has expired — start again from the connection")
	}
	if err != nil {
		return res, fmt.Errorf("oauth state: %w", err)
	}
	authType, metaRaw, secretRaw, err := s.OpenCredential(ctx, res.ApplicationID, res.ConnectionID)
	if err != nil {
		return res, fmt.Errorf("open connection credential: %w", err)
	}
	if authType != AuthTypeOAuthCode {
		return res, errors.New("the connection is no longer an OAuth authorization-code connection")
	}
	meta, err := ValidateOAuthCodeMeta(metaRaw, allowInsecure)
	if err != nil {
		return res, err
	}
	secret := map[string]string{}
	if len(secretRaw) > 0 {
		_ = json.Unmarshal(secretRaw, &secret)
	}
	form := url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI}, "code_verifier": {verifier},
	}
	tok, err := oauthTokenRequest(ctx, client, meta, secret["client_secret"], form)
	if err != nil {
		return res, err
	}
	secret["access_token"] = tok.AccessToken
	if tok.RefreshToken != "" {
		secret["refresh_token"] = tok.RefreshToken
	}
	secret["expires_at"] = tok.expiresAt().Format(time.RFC3339)
	meta.ConnectedAt = time.Now().UTC().Format(time.RFC3339)
	meta.ConnectedBy = connectedBy
	if err := s.storeOAuth(ctx, res.ApplicationID, res.ConnectionID, meta, secret); err != nil {
		return res, err
	}
	return res, nil
}

// DisconnectOAuth forgets the tokens, keeping the client so a developer
// can connect again.
func (s *Store) DisconnectOAuth(ctx context.Context, appID, connectionID string) error {
	authType, metaRaw, secretRaw, err := s.OpenCredential(ctx, appID, connectionID)
	if err != nil {
		return err
	}
	if authType != AuthTypeOAuthCode {
		return errors.New("not an OAuth authorization-code connection")
	}
	var meta OAuthCodeMeta
	_ = json.Unmarshal(metaRaw, &meta)
	secret := map[string]string{}
	_ = json.Unmarshal(secretRaw, &secret)
	for _, k := range []string{"access_token", "refresh_token", "expires_at"} {
		delete(secret, k)
	}
	meta.ConnectedAt, meta.ConnectedBy = "", ""
	return s.storeOAuth(ctx, appID, connectionID, meta, secret)
}

func (s *Store) storeOAuth(ctx context.Context, appID, connectionID string, meta OAuthCodeMeta, secret map[string]string) error {
	metaJSON, _ := json.Marshal(meta)
	secretJSON, _ := json.Marshal(secret)
	sealed, err := EncryptCredential(secretJSON, appID, connectionID)
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE model.integration_connection SET meta = $3::jsonb, secret_enc = $4, updated_at = now()
		WHERE id = $1::uuid AND application_id = $2::uuid`, connectionID, appID, string(metaJSON), sealed); err != nil {
		return fmt.Errorf("store oauth tokens: %w", err)
	}
	return nil
}

type oauthToken struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

func (t oauthToken) expiresAt() time.Time {
	if t.ExpiresIn <= 0 {
		return time.Now().Add(time.Hour) // a provider that says nothing: assume an hour, refresh then
	}
	return time.Now().Add(time.Duration(t.ExpiresIn) * time.Second)
}

// oauthTokenRequest posts form to the token endpoint with the client
// authenticated the way the connection says.
func oauthTokenRequest(ctx context.Context, client *http.Client, meta OAuthCodeMeta, clientSecret string, form url.Values) (oauthToken, error) {
	var tok oauthToken
	if meta.TokenClientAuth == "post" {
		form.Set("client_id", meta.ClientID)
		form.Set("client_secret", clientSecret)
	} else if _, has := form["client_id"]; !has {
		// PKCE public clients send client_id in the body as well; harmless
		// alongside Basic for confidential ones.
		form.Set("client_id", meta.ClientID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, meta.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tok, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if meta.TokenClientAuth != "post" {
		req.SetBasicAuth(url.QueryEscape(meta.ClientID), url.QueryEscape(clientSecret))
	}
	resp, err := client.Do(req)
	if err != nil {
		return tok, fmt.Errorf("token endpoint: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return tok, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, truncate(strings.TrimSpace(string(body)), 300))
	}
	if json.Unmarshal(body, &tok) != nil || tok.AccessToken == "" {
		return tok, errors.New("token endpoint returned no access_token")
	}
	return tok, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// OAuthBearer is what a run uses: the connection's access token, refreshed
// first when it is (about to be) expired. A refreshed token is stored, so
// the next run starts from it.
func (s *Store) OAuthBearer(ctx context.Context, client *http.Client, appID, connectionID string, metaRaw json.RawMessage, secret map[string]string, allowInsecure bool) (string, error) {
	if secret["access_token"] == "" && secret["refresh_token"] == "" {
		return "", ErrOAuthNotConnected
	}
	meta, err := ValidateOAuthCodeMeta(metaRaw, allowInsecure)
	if err != nil {
		return "", err
	}
	fresh := false
	if at, perr := time.Parse(time.RFC3339, secret["expires_at"]); perr == nil && time.Until(at) > time.Minute && secret["access_token"] != "" {
		fresh = true
	}
	if fresh {
		return secret["access_token"], nil
	}
	if secret["refresh_token"] == "" {
		return "", errors.New("the access token has expired and the provider issued no refresh token — connect again")
	}
	tok, err := oauthTokenRequest(ctx, client, meta, secret["client_secret"], url.Values{"grant_type": {"refresh_token"}, "refresh_token": {secret["refresh_token"]}})
	if err != nil {
		return "", fmt.Errorf("refresh: %w", err)
	}
	secret["access_token"] = tok.AccessToken
	if tok.RefreshToken != "" {
		secret["refresh_token"] = tok.RefreshToken // rotation
	}
	secret["expires_at"] = tok.expiresAt().Format(time.RFC3339)
	if err := s.storeOAuth(ctx, appID, connectionID, meta, secret); err != nil {
		return "", err
	}
	return tok.AccessToken, nil
}

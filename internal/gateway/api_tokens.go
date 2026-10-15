package gateway

// Personal API tokens (migration 141): a credential a person makes for a
// script. A script could load data only with a sign-in token copied from the
// browser, which expires within the hour and which the script cannot renew
// (a tester's bulk load stopped there, 2026-10-14).
//
// A token acts as the person who made it: it is resolved to their identity
// (the Keycloak subject) before the request is routed, and from there the
// request is the person's own — their tenant, their roles, their access
// rules, their account being disabled. A read-only token reads; a read and
// write token does what the person may. Tokens are made, listed and revoked
// only by a person signed in to the console, never with a token, so a token
// that leaks cannot make itself another. Only a hash of each is kept.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/pkg/auditlog"
)

const (
	// apiTokenPrefix starts every token, so a leaked one is recognisable
	// (secret scanners, logs) and the gateway tells it from a sign-in token.
	apiTokenPrefix = "mvx_pat_"
	// A token expires; a person picks when, up to a year.
	apiTokenDefaultDays = 90
	apiTokenMaxDays     = 365
	// apiTokensPerPerson bounds the tokens a person holds at once.
	apiTokensPerPerson = 20
	// apiTokenUseInterval is how often a token's last use is recorded.
	apiTokenUseInterval = time.Minute
)

type apiTokenKey struct{}

// apiTokenUse is the token a request was made with.
type apiTokenUse struct {
	ID, Name, Subject, Access string
}

func withAPIToken(ctx context.Context, t *apiTokenUse) context.Context {
	return context.WithValue(ctx, apiTokenKey{}, t)
}

// apiTokenOf is the token ctx's request was made with, if it was.
func apiTokenOf(ctx context.Context) (*apiTokenUse, bool) {
	t, ok := ctx.Value(apiTokenKey{}).(*apiTokenUse)
	return t, ok && t != nil
}

func hashAPIToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// newAPIToken is a token (256 random bits) and its hint, the part a list
// shows to tell tokens apart.
func newAPIToken() (raw, hint string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	raw = apiTokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return raw, raw[:len(apiTokenPrefix)+4], nil
}

// apiTokenRoute matches the routes that manage tokens: a token reaches none.
var apiTokenRoute = regexp.MustCompile(`^/api/me/api-tokens(/|$)`)

// readOnlyPost matches the POST routes that only read: a read-only token may
// send them.
var readOnlyPost = regexp.MustCompile(`^/api/forms/[^/]+/records/query$`)

// readsOnly reports whether r only reads.
func readsOnly(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	case http.MethodPost:
		return readOnlyPost.MatchString(r.URL.Path)
	}
	return false
}

// apiTokenAuth resolves a request carrying a personal API token to the
// person it acts as, before the request is routed to their tenant. A
// request with any other credential passes through untouched.
func (h *handler) apiTokenAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !strings.HasPrefix(raw, apiTokenPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		ctx := r.Context()
		t, err := h.lookupAPIToken(ctx, raw)
		if err != nil {
			if !errors.Is(err, pgx.ErrNoRows) {
				h.log.Warn().Err(err).Msg("API token lookup failed")
			}
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			jsonErr(w, fmt.Errorf("this API token is not valid: it was revoked, it expired, or it never existed"), http.StatusUnauthorized)
			return
		}
		if apiTokenRoute.MatchString(r.URL.Path) {
			jsonErr(w, fmt.Errorf("API tokens are made and revoked by signing in to the console, not with a token"), http.StatusForbidden)
			return
		}
		if t.Access != "write" && !readsOnly(r) {
			jsonErr(w, fmt.Errorf("this API token is read-only: make a read and write token to change data"), http.StatusForbidden)
			return
		}
		h.touchAPIToken(ctx, t.ID)
		ctx = auditlog.WithVia(withAPIToken(ctx, t), "api_token:"+t.Name)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// lookupAPIToken is the live token raw names: not revoked, not expired.
func (h *handler) lookupAPIToken(ctx context.Context, raw string) (*apiTokenUse, error) {
	var t apiTokenUse
	err := h.db.Control().QueryRow(ctx, `
		SELECT id::text, name, keycloak_sub, access FROM platform.api_token
		WHERE token_hash = $1 AND revoked_at IS NULL AND expires_at > now()`,
		hashAPIToken(raw)).Scan(&t.ID, &t.Name, &t.Subject, &t.Access)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

var (
	apiTokenUsedMu sync.Mutex
	apiTokenUsed   = map[string]time.Time{}
)

// touchAPIToken records a token's use, at most once per apiTokenUseInterval.
func (h *handler) touchAPIToken(ctx context.Context, id string) {
	now := time.Now()
	apiTokenUsedMu.Lock()
	if last, ok := apiTokenUsed[id]; ok && now.Sub(last) < apiTokenUseInterval {
		apiTokenUsedMu.Unlock()
		return
	}
	apiTokenUsed[id] = now
	if len(apiTokenUsed) > 10000 {
		clear(apiTokenUsed)
		apiTokenUsed[id] = now
	}
	apiTokenUsedMu.Unlock()
	if _, err := h.db.Control().Exec(ctx, `UPDATE platform.api_token SET last_used_at = now() WHERE id = $1::uuid`, id); err != nil {
		h.log.Debug().Err(err).Str("api_token", id).Msg("API token use not recorded")
	}
}

// apiTokenItem is one token as its owner's list shows it; the token itself
// is in the answer that made it, and nowhere else.
type apiTokenItem struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Hint       string     `json:"hint"`
	Access     string     `json:"access"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	Expired    bool       `json:"expired"`
}

// tokenOwner is the signed-in person a token route is for: their actor (for
// the audit log) and their subject (what a token acts as).
func (h *handler) tokenOwner(w http.ResponseWriter, r *http.Request) (*actor, string, bool) {
	act, err := h.resolveActor(r.Context(), r)
	if err != nil {
		jsonErr(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return nil, "", false
	}
	sub := h.subjectOf(r)
	if sub == "" {
		jsonErr(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return nil, "", false
	}
	return act, sub, true
}

// listAPITokens serves GET /api/me/api-tokens: the caller's tokens not
// revoked, newest first, expired ones marked.
func (h *handler) listAPITokens(w http.ResponseWriter, r *http.Request) {
	_, sub, ok := h.tokenOwner(w, r)
	if !ok {
		return
	}
	rows, err := h.db.Control().Query(r.Context(), `
		SELECT id::text, name, hint, access, created_at, expires_at, last_used_at, expires_at <= now()
		FROM platform.api_token WHERE keycloak_sub = $1 AND revoked_at IS NULL
		ORDER BY created_at DESC`, sub)
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []apiTokenItem{}
	for rows.Next() {
		var t apiTokenItem
		if err := rows.Scan(&t.ID, &t.Name, &t.Hint, &t.Access, &t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt, &t.Expired); err != nil {
			jsonErr(w, err, http.StatusInternalServerError)
			return
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	jsonOK(w, out)
}

// createAPIToken serves POST /api/me/api-tokens {name, access, expires_in_days}:
// a token acting as the caller. The answer carries the token, once.
func (h *handler) createAPIToken(w http.ResponseWriter, r *http.Request) {
	act, sub, ok := h.tokenOwner(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	var body struct {
		Name          string `json:"name"`
		Access        string `json:"access"`
		ExpiresInDays int    `json:"expires_in_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonErr(w, fmt.Errorf("invalid JSON body"), http.StatusBadRequest)
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	switch {
	case body.Name == "":
		jsonErr(w, fmt.Errorf("name the token after what will use it"), http.StatusBadRequest)
		return
	case len([]rune(body.Name)) > 100:
		jsonErr(w, fmt.Errorf("a token's name is at most 100 characters"), http.StatusBadRequest)
		return
	case body.Access != "read" && body.Access != "write":
		jsonErr(w, fmt.Errorf(`access is "read" or "write"`), http.StatusBadRequest)
		return
	}
	if body.ExpiresInDays == 0 {
		body.ExpiresInDays = apiTokenDefaultDays
	}
	if body.ExpiresInDays < 1 || body.ExpiresInDays > apiTokenMaxDays {
		jsonErr(w, fmt.Errorf("a token expires in 1 to %d days", apiTokenMaxDays), http.StatusBadRequest)
		return
	}
	var held int
	if err := h.db.Control().QueryRow(ctx, `
		SELECT count(*) FROM platform.api_token
		WHERE keycloak_sub = $1 AND revoked_at IS NULL AND expires_at > now()`, sub).Scan(&held); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	if held >= apiTokensPerPerson {
		jsonErr(w, fmt.Errorf("you hold %d API tokens, the most a person may: revoke one you no longer use", held), http.StatusConflict)
		return
	}
	raw, hint, err := newAPIToken()
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	var t apiTokenItem
	if err := h.db.Control().QueryRow(ctx, `
		INSERT INTO platform.api_token (keycloak_sub, name, token_hash, hint, access, expires_at)
		VALUES ($1, $2, $3, $4, $5, now() + make_interval(days => $6))
		RETURNING id::text, name, hint, access, created_at, expires_at`,
		sub, body.Name, hashAPIToken(raw), hint, body.Access, body.ExpiresInDays,
	).Scan(&t.ID, &t.Name, &t.Hint, &t.Access, &t.CreatedAt, &t.ExpiresAt); err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryAuth, EventType: auditlog.EventAPITokenCreated,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ResourceType: "api_token", ResourceID: t.ID,
		Metadata: map[string]string{"name": t.Name, "access": t.Access, "expires_at": t.ExpiresAt.UTC().Format(time.RFC3339)},
	})
	jsonOK(w, map[string]any{"token": raw, "api_token": t})
}

// revokeAPIToken serves DELETE /api/me/api-tokens/{id}: the caller's token
// stops working at once.
func (h *handler) revokeAPIToken(w http.ResponseWriter, r *http.Request) {
	act, sub, ok := h.tokenOwner(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	var name string
	err := h.db.Control().QueryRow(ctx, `
		UPDATE platform.api_token SET revoked_at = now()
		WHERE id::text = $1 AND keycloak_sub = $2 AND revoked_at IS NULL
		RETURNING name`, id, sub).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		jsonErr(w, fmt.Errorf("no such API token"), http.StatusNotFound)
		return
	}
	if err != nil {
		jsonErr(w, err, http.StatusInternalServerError)
		return
	}
	auditlog.Log(ctx, h.db.For(ctx), h.log, auditlog.Fields{
		Category: auditlog.CategoryAuth, EventType: auditlog.EventAPITokenRevoked,
		ActorUserID: act.UserID, ActorRole: strings.Join(act.Roles, ","),
		ResourceType: "api_token", ResourceID: id,
		Metadata: map[string]string{"name": name},
	})
	jsonOK(w, map[string]any{"revoked": true})
}

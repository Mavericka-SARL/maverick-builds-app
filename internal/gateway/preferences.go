package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"

	"github.com/jackc/pgx/v5"
)

// ── per-person preferences ───────────────────────────────────────────────────
//
// Display choices a signed-in person makes for themselves, kept on their own
// identity.user row (migration 097) so they follow the person across browsers
// and devices. Every role reaches them: the only row touched is the caller's.
//
// Only keys registered here are accepted, each with its own validator, so the
// column never holds anything the console does not understand; adding a
// preference is one entry below plus the console reading it. A null value
// resets a key to the console's default.
//
// Not audited: a colour scheme is neither a data nor a security change, and
// every toggle would be a row in the tenant's audit log for nothing.

var preferenceKeys = map[string]func(json.RawMessage) error{
	"theme": oneOfStrings("light", "dark", "system"),
}

func oneOfStrings(allowed ...string) func(json.RawMessage) error {
	return func(raw json.RawMessage) error {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return fmt.Errorf("must be one of %q", allowed)
		}
		for _, a := range allowed {
			if s == a {
				return nil
			}
		}
		return fmt.Errorf("must be one of %q", allowed)
	}
}

// knownPreferences drops anything the registry no longer accepts, so a key
// retired from preferenceKeys stops being served without a migration.
func knownPreferences(raw []byte) map[string]json.RawMessage {
	out := map[string]json.RawMessage{}
	var stored map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &stored) != nil {
		return out
	}
	for k, v := range stored {
		if validate, ok := preferenceKeys[k]; ok && validate(v) == nil {
			out[k] = v
		}
	}
	return out
}

// preferencesFor reads a person's preferences for /api/me. A failed read
// returns nil, sent as null — "unknown", which the console must not mistake
// for {} ("nothing chosen") and answer by saving this browser's older copy
// over the account's. It does not fail the request the whole console
// depends on.
func (h *handler) preferencesFor(ctx context.Context, userID string) map[string]json.RawMessage {
	var raw []byte
	if err := h.db.QueryRow(ctx, `SELECT preferences FROM identity.user WHERE id = $1::uuid`, userID).Scan(&raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return map[string]json.RawMessage{}
		}
		h.log.Warn().Err(err).Str("user_id", userID).Msg("preferences not read")
		return nil
	}
	return knownPreferences(raw)
}

type preferencesResponse struct {
	Preferences map[string]json.RawMessage `json:"preferences"`
}

// updateMyPreferences serves PATCH /api/me/preferences: a partial update of
// the caller's own preferences. Keys absent from the body are left alone.
func (h *handler) updateMyPreferences(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	act, err := h.resolveActor(ctx, r)
	if err != nil {
		jsonErr(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body == nil {
		jsonErr(w, fmt.Errorf("body must be a JSON object of preferences"), http.StatusBadRequest)
		return
	}
	set := map[string]json.RawMessage{}
	reset := []string{}
	for k, v := range body {
		validate, ok := preferenceKeys[k]
		if !ok {
			jsonErr(w, fmt.Errorf("unknown preference %q", k), http.StatusBadRequest)
			return
		}
		if string(v) == "null" {
			reset = append(reset, k)
			continue
		}
		if err := validate(v); err != nil {
			jsonErr(w, fmt.Errorf("preference %q %v", k, err), http.StatusBadRequest)
			return
		}
		set[k] = v
	}
	sort.Strings(reset)
	setJSON, _ := json.Marshal(set)

	// One statement, so two tabs saving different keys cannot lose either.
	var raw []byte
	err = h.db.QueryRow(personalCtx(ctx, act),
		`UPDATE identity.user SET preferences = (preferences || $2::jsonb) - $3::text[]
		 WHERE id = $1::uuid RETURNING preferences`,
		act.UserID, string(setJSON), reset,
	).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		jsonErr(w, fmt.Errorf("account not found"), http.StatusNotFound)
		return
	}
	if err != nil {
		jsonErr(w, fmt.Errorf("save preferences: %w", err), http.StatusInternalServerError)
		return
	}
	jsonOK(w, preferencesResponse{Preferences: knownPreferences(raw)})
}

// Package license implements the offline, signed license key that unlocks the
// commercial and enterprise editions of maverickbuilds.app.
//
// A key is a self-contained token: a JSON payload (edition, feature schedule,
// customer, order references, expiry, optional extra features) signed with
// the vendor's Ed25519 private key. The gateway verifies it against the public
// keys compiled into the binary — a deployment cannot add its own — and never
// calls home. Without a key, or with an invalid or expired one, the deployment
// runs the community edition — it never refuses to start.
//
// A key is a technical credential implementing a contract; it grants no
// rights by itself (docs/LICENSING.md).
//
// Token format:
//
//	MVX1.<base64url(payload JSON)>.<base64url(ed25519 signature)>
//
// The signature covers the literal string "MVX1.<payload>" so the version
// prefix cannot be swapped underneath a valid signature.
package license

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Prefix identifies the token format; bump it if the payload or signature
// scheme ever changes so old verifiers reject new tokens explicitly.
const Prefix = "MVX1"

// Edition is the product tier a deployment runs in.
type Edition string

const (
	EditionCommunity  Edition = "community"
	EditionCommercial Edition = "commercial"
	EditionEnterprise Edition = "enterprise"
)

// Editions in ascending order of entitlement.
var Editions = []Edition{EditionCommunity, EditionCommercial, EditionEnterprise}

func (e Edition) valid() bool {
	for _, known := range Editions {
		if known == e {
			return true
		}
	}
	return false
}

// Claims is the signed payload of a license key.
type Claims struct {
	// ID identifies the license (a UUID or any vendor-side reference).
	ID string `json:"id"`
	// Edition the key unlocks: commercial or enterprise. A community key
	// makes no sense and is refused.
	Edition Edition `json:"edition"`
	// Customer is the licensee's name, shown in the console.
	Customer string `json:"customer"`
	// Contact is an optional e-mail for the licensee.
	Contact string `json:"contact,omitempty"`
	// IssuedAt and ExpiresAt bound the validity window. A key is refused
	// before IssuedAt (minus a day of clock skew) and reported as expired
	// after ExpiresAt; both are compared against the gateway's clock.
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// Schedule is the feature schedule the key was sold under (see
	// Schedule): the edition's default features come from it. Empty on keys
	// issued before schedules existed, which read as FirstSchedule.
	Schedule string `json:"schedule,omitempty"`
	// Features lists additional features beyond the edition's defaults, for
	// tailored contracts. Unknown names are ignored so a newer key still
	// verifies on an older binary.
	Features []Feature `json:"features,omitempty"`
	// Order, Agreement and Deployment tie the key to the contract it
	// implements: the order number, the reference of the agreement text the
	// customer accepted, and the deployment(s) the order covers. They are
	// records for both parties, shown in the console; the rights themselves
	// are the contract's. (Keys issued before 2026-10-07 could also carry
	// "limits"; nothing enforced them and they are now ignored.)
	Order      string `json:"order,omitempty"`
	Agreement  string `json:"agreement,omitempty"`
	Deployment string `json:"deployment,omitempty"`
	// Notes is free text for the vendor's records (never shown to users).
	Notes string `json:"notes,omitempty"`
}

// clockSkew is how far before IssuedAt a key is still accepted, so a key
// issued a few minutes ahead of a slow clock does not bounce.
const clockSkew = 24 * time.Hour

// Sentinel errors returned by Parse. Callers that only need "valid or not"
// can ignore the distinction; the console shows the message verbatim.
var (
	ErrMalformed = errors.New("license key is malformed")
	ErrSignature = errors.New("license key signature is invalid")
	ErrEdition   = errors.New("license key names an unknown edition")
)

// Parse verifies token against pub and returns its claims. Expiry is NOT
// checked here — a parsed key can be expired; see Claims.Expired — so the
// caller can distinguish "tampered" from "ran out".
func Parse(token string, pub ed25519.PublicKey) (*Claims, error) {
	token = strings.TrimSpace(token)
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != Prefix {
		return nil, ErrMalformed
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: payload is not base64url", ErrMalformed)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: signature is not base64url", ErrMalformed)
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: verifier has no valid public key", ErrSignature)
	}
	if !ed25519.Verify(pub, []byte(parts[0]+"."+parts[1]), sig) {
		return nil, ErrSignature
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, fmt.Errorf("%w: payload is not valid JSON", ErrMalformed)
	}
	if c.Edition == EditionCommunity || !c.Edition.valid() {
		return nil, fmt.Errorf("%w: %q", ErrEdition, c.Edition)
	}
	if c.ExpiresAt.IsZero() {
		return nil, fmt.Errorf("%w: expires_at is required", ErrMalformed)
	}
	if _, ok := resolveSchedule(c.Schedule); !ok {
		return nil, fmt.Errorf("%w: unknown feature schedule %q", ErrMalformed, c.Schedule)
	}
	return &c, nil
}

// ParseAny verifies token against each trusted key in turn — the compiled-in
// set holds more than one while a signing key is being rotated — and
// returns the first verification. A malformed token fails at once.
func ParseAny(token string, keys []ed25519.PublicKey) (*Claims, error) {
	for _, pub := range keys {
		c, err := Parse(token, pub)
		if err == nil || !errors.Is(err, ErrSignature) {
			return c, err
		}
	}
	return nil, ErrSignature
}

// Sign produces a token for claims with the vendor's private key. It is the
// inverse of Parse and is what cmd/license uses.
func Sign(claims Claims, priv ed25519.PrivateKey) (string, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return "", errors.New("private key has the wrong size")
	}
	if claims.Edition == EditionCommunity || !claims.Edition.valid() {
		return "", fmt.Errorf("%w: %q", ErrEdition, claims.Edition)
	}
	if claims.ExpiresAt.IsZero() {
		return "", errors.New("expires_at is required")
	}
	if claims.Schedule == "" {
		claims.Schedule = LatestSchedule()
	}
	if !knownSchedule(claims.Schedule) {
		return "", fmt.Errorf("unknown feature schedule %q; known: %s", claims.Schedule, scheduleIDs())
	}
	if claims.IssuedAt.IsZero() {
		claims.IssuedAt = time.Now().UTC()
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	body := Prefix + "." + base64.RawURLEncoding.EncodeToString(payload)
	sig := ed25519.Sign(priv, []byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Expired reports whether the key has run out at now.
func (c *Claims) Expired(now time.Time) bool {
	return !now.Before(c.ExpiresAt)
}

// NotYetValid reports whether now is before the key's issue time by more
// than the tolerated clock skew.
func (c *Claims) NotYetValid(now time.Time) bool {
	return !c.IssuedAt.IsZero() && now.Add(clockSkew).Before(c.IssuedAt)
}

// EffectiveFeatures is the edition's default feature set under the key's
// schedule plus any extra features the key names, sorted, with unknown names
// dropped.
func (c *Claims) EffectiveFeatures() []Feature {
	set := map[Feature]bool{}
	sched, _ := resolveSchedule(c.Schedule)
	for _, f := range sched.Editions[c.Edition] {
		set[f] = true
	}
	for _, f := range c.Features {
		if _, known := catalog[f]; known {
			set[f] = true
		}
	}
	out := make([]Feature, 0, len(set))
	for f := range set {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// DecodePublicKey parses a base64 (standard or URL, padded or not) Ed25519
// public key as produced by cmd/license keygen.
func DecodePublicKey(s string) (ed25519.PublicKey, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("public key is empty")
	}
	var raw []byte
	var err error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if raw, err = enc.DecodeString(s); err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("public key is not base64: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key must be %d bytes, got %d", ed25519.PublicKeySize, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}

// EncodeKey renders a key as standard base64, the form keygen prints and
// DecodePublicKey/DecodePrivateKey read back.
func EncodeKey(k []byte) string { return base64.StdEncoding.EncodeToString(k) }

// DecodePrivateKey parses a base64 Ed25519 private key (the 64-byte form
// keygen writes).
func DecodePrivateKey(s string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("private key is not base64: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("private key must be %d bytes, got %d", ed25519.PrivateKeySize, len(raw))
	}
	return ed25519.PrivateKey(raw), nil
}

func scheduleIDs() string {
	ids := make([]string, len(schedules))
	for i, s := range schedules {
		ids[i] = s.ID
	}
	return strings.Join(ids, ", ")
}

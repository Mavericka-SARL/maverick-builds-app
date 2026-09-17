package license

import (
	"crypto/ed25519"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// State says how the deployment arrived at its effective edition.
type State string

const (
	// StateCommunity: no key configured.
	StateCommunity State = "community"
	// StateActive: a valid key is in force.
	StateActive State = "active"
	// StateExpired: a valid key whose expiry has passed; the deployment runs
	// as community until a new key is installed.
	StateExpired State = "expired"
	// StateInvalid: a key was configured but could not be verified; the
	// deployment runs as community and the reason is reported.
	StateInvalid State = "invalid"
)

// Status is what the gateway reports about the license in force. It is the
// public shape of GET /api/license, so field names are part of the contract.
type Status struct {
	// Edition is the EFFECTIVE edition: community whenever the key is
	// missing, invalid or expired.
	Edition Edition `json:"edition"`
	State   State   `json:"state"`
	// Features are the capabilities unlocked right now (empty for community).
	Features []Feature `json:"features"`
	// Catalog lists every gated feature so the console can show locked ones.
	Catalog []Info `json:"catalog"`
	// Source says where the key came from: "env", "file" or "none".
	Source string `json:"source"`
	// The remaining fields describe the configured key, if any. They are
	// filled for expired keys too, so the console can say what ran out.
	LicenseID string           `json:"license_id,omitempty"`
	Customer  string           `json:"customer,omitempty"`
	Contact   string           `json:"contact,omitempty"`
	IssuedAt  *time.Time       `json:"issued_at,omitempty"`
	ExpiresAt *time.Time       `json:"expires_at,omitempty"`
	Limits    map[string]int64 `json:"limits,omitempty"`
	// Error is the verification failure for StateInvalid.
	Error string `json:"error,omitempty"`
}

// Options configures Load. Zero values mean "not configured".
type Options struct {
	// Key is the token itself (MAVERICKS_LICENSE_KEY).
	Key string
	// File is a path to a file holding the token (MAVERICKS_LICENSE_FILE).
	// Key wins when both are set.
	File string
	// PublicKey overrides the compiled-in verifier key, base64 encoded
	// (MAVERICKS_LICENSE_PUBLIC_KEY). Meant for tests and for forks that
	// issue their own keys.
	PublicKey string
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// Manager holds the parsed key and answers edition/feature questions. A nil
// *Manager behaves as the community edition, so code paths that were built
// without one keep working.
type Manager struct {
	mu     sync.RWMutex
	now    func() time.Time
	source string
	claims *Claims
	err    error
}

// Load reads the configured key once. It never fails: a missing key is the
// community edition, a bad key is StateInvalid with the reason in Status.
func Load(opts Options) *Manager {
	m := &Manager{now: opts.Now}
	if m.now == nil {
		m.now = time.Now
	}
	pub, pubErr := verifierKey(opts.PublicKey)

	token := strings.TrimSpace(opts.Key)
	if token != "" {
		m.source = "env"
	} else if opts.File != "" {
		raw, err := os.ReadFile(opts.File)
		if err != nil {
			m.source = "file"
			m.err = fmt.Errorf("read license file: %w", err)
			return m
		}
		token = strings.TrimSpace(string(raw))
		m.source = "file"
	} else {
		m.source = "none"
		return m
	}
	if pubErr != nil {
		m.err = pubErr
		return m
	}
	claims, err := Parse(token, pub)
	if err != nil {
		m.err = err
		return m
	}
	if claims.NotYetValid(m.now()) {
		m.err = fmt.Errorf("license key is not valid before %s", claims.IssuedAt.Format(time.RFC3339))
		return m
	}
	m.claims = claims
	return m
}

// Static builds a Manager around already-verified claims (tests, tooling).
func Static(claims *Claims) *Manager {
	return &Manager{now: time.Now, source: "env", claims: claims}
}

func verifierKey(override string) (ed25519.PublicKey, error) {
	if strings.TrimSpace(override) != "" {
		return DecodePublicKey(override)
	}
	return DecodePublicKey(defaultPublicKeyB64)
}

// Status evaluates expiry against the clock at call time, so a long-running
// gateway drops to community when its key runs out without a restart.
func (m *Manager) Status() Status {
	st := Status{Edition: EditionCommunity, State: StateCommunity, Features: []Feature{}, Catalog: Catalog(), Source: "none"}
	if m == nil {
		return st
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	st.Source = m.source
	if m.err != nil {
		st.State = StateInvalid
		st.Error = m.err.Error()
		return st
	}
	if m.claims == nil {
		return st
	}
	c := m.claims
	st.LicenseID, st.Customer, st.Contact, st.Limits = c.ID, c.Customer, c.Contact, c.Limits
	issued, expires := c.IssuedAt, c.ExpiresAt
	st.IssuedAt, st.ExpiresAt = &issued, &expires
	if c.Expired(m.now()) {
		st.State = StateExpired
		return st
	}
	st.State = StateActive
	st.Edition = c.Edition
	st.Features = c.EffectiveFeatures()
	return st
}

// Edition is the effective edition (see Status).
func (m *Manager) Edition() Edition { return m.Status().Edition }

// Has reports whether f is unlocked right now.
func (m *Manager) Has(f Feature) bool {
	for _, have := range m.Status().Features {
		if have == f {
			return true
		}
	}
	return false
}

// Require returns nil when f is unlocked, else the error a gated route
// should answer with.
func (m *Manager) Require(f Feature) error {
	if m.Has(f) {
		return nil
	}
	return UnavailableError(f, m.Edition())
}

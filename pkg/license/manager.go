package license

import (
	"crypto/ed25519"
	"errors"
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
	// StateExpired: a valid key past its expiry date. The deployment runs
	// as community from that moment, with no transition period (owner,
	// 2026-11-22; until then 30 days of read-and-export followed expiry),
	// until a new key is installed. Nothing is deleted.
	StateExpired State = "expired"
	// StateInvalid: a key was configured but could not be verified; the
	// deployment runs as community and the reason is reported.
	StateInvalid State = "invalid"
)

// RenewalNotice is how long before expiry Status starts reporting
// RenewalDue, so the console warns while there is time to renew.
const RenewalNotice = 30 * 24 * time.Hour

// Status is what the gateway reports about the license in force. It is the
// public shape of GET /api/license, so field names are part of the contract.
type Status struct {
	// Edition is the EFFECTIVE edition: community whenever the key is
	// missing, invalid or expired.
	Edition Edition `json:"edition"`
	State   State   `json:"state"`
	// Features are the capabilities in force right now (empty for
	// community, an expired key included); see Manager.Has.
	Features []Feature `json:"features"`
	// Catalog lists every gated feature so the console can show locked ones.
	Catalog []Info `json:"catalog"`
	// Source says where the key came from: "env", "file", "console" (a
	// platform administrator installed it under Platform › License) or
	// "none".
	Source string `json:"source"`
	// The remaining fields describe the configured key, if any. They are
	// filled for expired keys too, so the console can say what ran out.
	LicenseID  string     `json:"license_id,omitempty"`
	Customer   string     `json:"customer,omitempty"`
	Contact    string     `json:"contact,omitempty"`
	Schedule   string     `json:"schedule,omitempty"`
	Order      string     `json:"order,omitempty"`
	Agreement  string     `json:"agreement,omitempty"`
	Deployment string     `json:"deployment,omitempty"`
	IssuedAt   *time.Time `json:"issued_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	// RenewalDue is set while an active key is within RenewalNotice of
	// its expiry.
	RenewalDue bool `json:"renewal_due,omitempty"`
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
	// PublicKey replaces the compiled-in trusted keys, base64 encoded. Tests
	// and the vendor's own inspect tool only: the gateway never sets it and
	// no environment variable reaches it (see trustedPublicKeys).
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
	// keys verify a key installed later (Check); config is true when the
	// server's configuration names a key, which then wins over the console.
	keys   []ed25519.PublicKey
	config bool
}

// Load reads the configured key once. It never fails: a missing key is the
// community edition, a bad key is StateInvalid with the reason in Status.
func Load(opts Options) *Manager {
	m := &Manager{now: opts.Now}
	if m.now == nil {
		m.now = time.Now
	}
	keys, keysErr := verifierKeys(opts.PublicKey)
	m.keys = keys

	token := strings.TrimSpace(opts.Key)
	m.config = token != "" || opts.File != ""
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
	if keysErr != nil {
		m.err = keysErr
		return m
	}
	claims, err := ParseAny(token, keys)
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
	return &Manager{now: time.Now, source: "env", claims: claims, config: true}
}

// ErrConfigured refuses a console install or removal while the server's
// configuration names the key (MAVERICKS_LICENSE_KEY or _FILE): that one
// wins, and is changed where it is set.
var ErrConfigured = errors.New("this deployment's license key is set in the server's configuration (MAVERICKS_LICENSE_KEY or MAVERICKS_LICENSE_FILE); change it there, or remove that setting to install keys in the console")

// FromConfig reports whether the server's configuration names the key.
func (m *Manager) FromConfig() bool {
	if m == nil {
		return false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.config
}

// Verify checks a key's signature, format, edition and schedule against the
// trusted keys, as Load does: what a key installed earlier is read back
// with, expired or not, so its transition still runs.
func (m *Manager) Verify(token string) (*Claims, error) {
	if m == nil {
		return nil, errors.New("no license manager")
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("paste the license key, the line that starts with MVX1")
	}
	claims, err := ParseAny(token, m.keys)
	if err != nil {
		return nil, err
	}
	if claims.NotYetValid(m.now()) {
		return nil, fmt.Errorf("this license key is not valid before %s", claims.IssuedAt.UTC().Format("2006-01-02"))
	}
	return claims, nil
}

// Check is Verify for a key someone means to install now: one that has
// already expired is refused too, since installing it would change nothing
// a customer wants.
func (m *Manager) Check(token string) (*Claims, error) {
	claims, err := m.Verify(token)
	if err != nil {
		return nil, err
	}
	if claims.Expired(m.now()) {
		return nil, fmt.Errorf("this license key expired on %s; install the renewed key", claims.ExpiresAt.UTC().Format("2006-01-02"))
	}
	return claims, nil
}

// UseStored puts a key installed in the console in force (Check has
// verified it), or with nil takes it away: the deployment runs the
// Community Edition again. A key from the server's configuration wins over
// both, so UseStored leaves it alone and says so.
func (m *Manager) UseStored(claims *Claims) error {
	if m == nil {
		return errors.New("no license manager")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.config {
		return ErrConfigured
	}
	m.err = nil
	m.claims = claims
	if claims == nil {
		m.source = "none"
	} else {
		m.source = "console"
	}
	return nil
}

// Stored is the claims of the key installed in the console, nil when none
// is (or the key in force came from the configuration).
func (m *Manager) Stored() *Claims {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.source != "console" {
		return nil
	}
	return m.claims
}

func verifierKeys(override string) ([]ed25519.PublicKey, error) {
	if strings.TrimSpace(override) != "" {
		k, err := DecodePublicKey(override)
		return []ed25519.PublicKey{k}, err
	}
	keys := make([]ed25519.PublicKey, 0, len(trustedPublicKeys))
	for _, s := range trustedPublicKeys {
		k, err := DecodePublicKey(s)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// Status evaluates expiry against the clock at call time, so a long-running
// gateway moves through renewal and on to community without a restart.
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
	st.LicenseID, st.Customer, st.Contact = c.ID, c.Customer, c.Contact
	st.Schedule, st.Order, st.Agreement, st.Deployment = c.Schedule, c.Order, c.Agreement, c.Deployment
	if st.Schedule == "" {
		st.Schedule = FirstSchedule
	}
	issued, expires := c.IssuedAt, c.ExpiresAt
	st.IssuedAt, st.ExpiresAt = &issued, &expires
	now := m.now()
	if c.Expired(now) {
		st.State = StateExpired
		return st
	}
	st.State = StateActive
	st.RenewalDue = !now.Add(RenewalNotice).Before(c.ExpiresAt)
	st.Edition = c.Edition
	st.Features = c.EffectiveFeatures()
	return st
}

// Edition is the effective edition (see Status).
func (m *Manager) Edition() Edition { return m.Status().Edition }

// Has reports whether the key in force unlocks f right now. An expired key
// unlocks nothing: there is no transition period (owner, 2026-11-22).
func (m *Manager) Has(f Feature) bool {
	st := m.Status()
	return st.State == StateActive && hasFeature(st.Features, f)
}

// Require returns nil when Has(f), else the error a gated route should
// answer with.
func (m *Manager) Require(f Feature) error {
	if m.Has(f) {
		return nil
	}
	return UnavailableError(f, m.Status().Edition)
}

func hasFeature(fs []Feature, f Feature) bool {
	for _, have := range fs {
		if have == f {
			return true
		}
	}
	return false
}

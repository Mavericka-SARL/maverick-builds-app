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
	// StateTransition: a valid key has expired less than TransitionPeriod
	// ago. Its paid features keep working as configured and can be read and
	// exported, so a customer can renew or move off them, but nothing paid
	// can be newly configured or changed.
	StateTransition State = "transition"
	// StateExpired: a valid key whose transition period has ended too; the
	// deployment runs as community until a new key is installed.
	StateExpired State = "expired"
	// StateInvalid: a key was configured but could not be verified; the
	// deployment runs as community and the reason is reported.
	StateInvalid State = "invalid"
)

// RenewalNotice is how long before expiry Status starts reporting
// RenewalDue, so the console warns while there is time to renew.
const RenewalNotice = 30 * 24 * time.Hour

// TransitionPeriod is how long after expiry a key stays in StateTransition.
// It is a technical grace for export and migration, not a contractual
// right; the paid agreement says what the customer may do in it.
const TransitionPeriod = 30 * 24 * time.Hour

// Status is what the gateway reports about the license in force. It is the
// public shape of GET /api/license, so field names are part of the contract.
type Status struct {
	// Edition is the EFFECTIVE edition: community whenever the key is
	// missing, invalid or expired; the key's own during its transition.
	Edition Edition `json:"edition"`
	State   State   `json:"state"`
	// Features are the capabilities in force right now (empty for
	// community). During the transition they work as configured and can be
	// read and exported but not reconfigured; see Manager.Has and Usable.
	Features []Feature `json:"features"`
	// Catalog lists every gated feature so the console can show locked ones.
	Catalog []Info `json:"catalog"`
	// Source says where the key came from: "env", "file" or "none".
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
	// TransitionEndsAt is when the read-and-export period after expiry
	// ends (or ended), for transition and expired keys.
	TransitionEndsAt *time.Time `json:"transition_ends_at,omitempty"`
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
}

// Load reads the configured key once. It never fails: a missing key is the
// community edition, a bad key is StateInvalid with the reason in Status.
func Load(opts Options) *Manager {
	m := &Manager{now: opts.Now}
	if m.now == nil {
		m.now = time.Now
	}
	keys, keysErr := verifierKeys(opts.PublicKey)

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
	return &Manager{now: time.Now, source: "env", claims: claims}
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
// gateway moves through renewal, transition and community without a restart.
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
		ends := c.ExpiresAt.Add(TransitionPeriod)
		st.TransitionEndsAt = &ends
		if !now.Before(ends) {
			st.State = StateExpired
			return st
		}
		st.State = StateTransition
	} else {
		st.State = StateActive
		st.RenewalDue = !now.Add(RenewalNotice).Before(c.ExpiresAt)
	}
	st.Edition = c.Edition
	st.Features = c.EffectiveFeatures()
	return st
}

// Edition is the effective edition (see Status).
func (m *Manager) Edition() Edition { return m.Status().Edition }

// Has reports whether f is unlocked with full rights right now: a key in
// force, not in its transition. It is the strict check — anything that
// configures, changes or irreversibly acts on a paid feature uses it, so a
// caller that forgets to choose fails closed.
func (m *Manager) Has(f Feature) bool {
	st := m.Status()
	return st.State == StateActive && hasFeature(st.Features, f)
}

// Usable reports whether f may keep working as configured: Has, or the
// key's transition period. Sign-in, branding, inherited settings and reads
// and exports of a paid feature use it, so an expired key does not lock
// people out or strand their data while they renew or migrate.
func (m *Manager) Usable(f Feature) bool {
	st := m.Status()
	return (st.State == StateActive || st.State == StateTransition) && hasFeature(st.Features, f)
}

// Require returns nil when Has(f), else the error a gated route should
// answer with.
func (m *Manager) Require(f Feature) error {
	if m.Has(f) {
		return nil
	}
	return m.refusal(f)
}

// RequireUse returns nil when Usable(f), else the error a gated route
// should answer with.
func (m *Manager) RequireUse(f Feature) error {
	if m.Usable(f) {
		return nil
	}
	return m.refusal(f)
}

func (m *Manager) refusal(f Feature) error {
	st := m.Status()
	if st.State == StateTransition && hasFeature(st.Features, f) {
		return TransitionError(f, *st.ExpiresAt, *st.TransitionEndsAt)
	}
	return UnavailableError(f, st.Edition)
}

// TransitionError explains why a change to a paid feature is refused while
// its key is in the transition period.
func TransitionError(f Feature, expired, ends time.Time) error {
	return fmt.Errorf("%s is read and export only: the license key expired on %s, and until %s nothing paid can be changed; install a renewed key to change it",
		Describe(f), expired.UTC().Format("2006-01-02"), ends.UTC().Format("2006-01-02"))
}

func hasFeature(fs []Feature, f Feature) bool {
	for _, have := range fs {
		if have == f {
			return true
		}
	}
	return false
}

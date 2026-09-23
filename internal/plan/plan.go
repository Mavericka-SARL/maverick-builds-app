// Package plan is what a tenant's plan means: the limits it carries, and
// whether the tenant is read-only because a limit was exceeded. A plan
// bounds how much a tenant may use, never for how long — there is no trial
// (migration 090).
//
// Plans are rows in platform.plan, edited by the platform administrator; a
// tenant names one in core.customer.plan (migration 085). Nothing here is a
// license question — the license key decides the edition a deployment runs,
// a plan decides how much one tenant may use. A community deployment with no
// key still has plans, because a hosted sign-up funnel is not an enterprise
// feature.
//
// Enforcement has two halves. The request-time checks (Enforcer.CheckX)
// refuse a creation that would cross a limit, with a message naming the plan
// and the number; they sit in the handlers people use. The sweep
// (Enforcer.Sweep) recounts every tenant periodically and marks a tenant
// that is over a limit — however it got there — read-only until it is back
// under. That is what makes the limits hold against every write path without
// every write path having to know about them.
package plan

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DB is the slice of a pool (or tenantdb.Handle) this package uses.
type DB interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Limits is what a plan allows. Zero means unlimited. The JSON names are the
// keys stored in platform.plan.limits and shown in the console; they are a
// contract with saved rows, so never rename one.
type Limits struct {
	MaxUsers                 int `json:"max_users"`
	MaxApplications          int `json:"max_applications"`
	MaxModels                int `json:"max_models"`
	MaxMetricsPerModel       int `json:"max_metrics_per_model"`
	MaxMembersPerDimension   int `json:"max_members_per_dimension"`
	MaxFactRowsPerModel      int `json:"max_fact_rows_per_model"`
	MaxAIMessagesPerDay      int `json:"max_ai_messages_per_day"`
	MaxIntegrationRunsPerDay int `json:"max_integration_runs_per_day"`
	// MaxStorageMB bounds the bytes a tenant's data occupies — exact in a
	// dedicated tenant database, an estimate over the data tables in a
	// shared one (see StorageBytes).
	MaxStorageMB int `json:"max_storage_mb"`
}

// Any reports whether any limit is set at all; a plan without one never
// needs counting.
func (l Limits) Any() bool {
	return l.MaxUsers > 0 || l.MaxApplications > 0 || l.MaxModels > 0 || l.MaxMetricsPerModel > 0 ||
		l.MaxMembersPerDimension > 0 || l.MaxFactRowsPerModel > 0 || l.MaxAIMessagesPerDay > 0 || l.MaxIntegrationRunsPerDay > 0 ||
		l.MaxStorageMB > 0
}

// Plan is one row of platform.plan.
type Plan struct {
	Key         string `json:"key"`
	Name        string `json:"name"`
	Description string `json:"description"`
	SelfService bool   `json:"self_service"`
	Limits      Limits `json:"limits"`
	// LimitNote is what a tenant is told when a limit stops it — where to go
	// from here. Empty means "change the plan": the words the engine uses
	// for a deployment whose answer is another plan. A deployment whose
	// answer is elsewhere (run it yourself, a licence) says so here.
	LimitNote string    `json:"limit_note"`
	SortOrder int       `json:"sort_order"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NextStep is the sentence appended to every refusal: the plan's note, or
// the default for a deployment where the next step is another plan.
func (p Plan) NextStep() string {
	if n := strings.TrimSpace(p.LimitNote); n != "" {
		return n
	}
	return "Change the plan to add more."
}

// ErrUnknownPlan is returned by Get for a key with no row.
var ErrUnknownPlan = errors.New("unknown plan")

var keyRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// Validate checks a plan before it is stored.
func (p Plan) Validate() error {
	if !keyRe.MatchString(p.Key) {
		return fmt.Errorf("plan key must be 1-32 lower-case letters, digits, '-' or '_'")
	}
	if n := strings.TrimSpace(p.Name); n == "" || len(n) > 60 {
		return fmt.Errorf("plan name is required (at most 60 characters)")
	}
	if len(p.Description) > 300 {
		return fmt.Errorf("description is at most 300 characters")
	}
	if len(p.LimitNote) > 500 {
		return fmt.Errorf("limit_note is at most 500 characters")
	}
	for _, v := range []int{p.Limits.MaxUsers, p.Limits.MaxApplications, p.Limits.MaxModels, p.Limits.MaxMetricsPerModel,
		p.Limits.MaxMembersPerDimension, p.Limits.MaxFactRowsPerModel, p.Limits.MaxAIMessagesPerDay, p.Limits.MaxIntegrationRunsPerDay, p.Limits.MaxStorageMB} {
		if v < 0 {
			return fmt.Errorf("a limit cannot be negative (0 means unlimited)")
		}
	}
	return nil
}

const planColumns = `key, name, description, self_service, limits, limit_note, sort_order, updated_at`

func scanPlan(row pgx.Row) (Plan, error) {
	var p Plan
	var limits []byte
	if err := row.Scan(&p.Key, &p.Name, &p.Description, &p.SelfService, &limits, &p.LimitNote, &p.SortOrder, &p.UpdatedAt); err != nil {
		return Plan{}, err
	}
	if len(limits) > 0 {
		if err := json.Unmarshal(limits, &p.Limits); err != nil {
			return Plan{}, fmt.Errorf("plan %s: limits: %w", p.Key, err)
		}
	}
	return p, nil
}

// List returns every plan in display order. db is the control plane.
func List(ctx context.Context, db DB) ([]Plan, error) {
	rows, err := db.Query(ctx, `SELECT `+planColumns+` FROM platform.plan ORDER BY sort_order, key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Plan
	for rows.Next() {
		p, err := scanPlan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Get returns one plan by key.
func Get(ctx context.Context, db DB, key string) (Plan, error) {
	p, err := scanPlan(db.QueryRow(ctx, `SELECT `+planColumns+` FROM platform.plan WHERE key = $1`, key))
	if errors.Is(err, pgx.ErrNoRows) {
		return Plan{}, fmt.Errorf("%w: %q", ErrUnknownPlan, key)
	}
	return p, err
}

// SelfService returns the plan public sign-up assigns: the first
// self-service plan in display order. ok is false when there is none, which
// is how sign-up is switched off on the data side.
func SelfService(ctx context.Context, db DB) (p Plan, ok bool, err error) {
	p, err = scanPlan(db.QueryRow(ctx, `SELECT `+planColumns+` FROM platform.plan WHERE self_service ORDER BY sort_order, key LIMIT 1`))
	if errors.Is(err, pgx.ErrNoRows) {
		return Plan{}, false, nil
	}
	if err != nil {
		return Plan{}, false, err
	}
	return p, true, nil
}

// Upsert stores a plan (insert or update by key) and returns the row.
func Upsert(ctx context.Context, db DB, p Plan) (Plan, error) {
	if err := p.Validate(); err != nil {
		return Plan{}, err
	}
	limits, _ := json.Marshal(p.Limits)
	return scanPlan(db.QueryRow(ctx, `
		INSERT INTO platform.plan (key, name, description, self_service, limits, limit_note, sort_order, updated_at)
		VALUES ($1, $2, $3, $4, $5::jsonb, $6, $7, now())
		ON CONFLICT (key) DO UPDATE SET name = EXCLUDED.name, description = EXCLUDED.description,
		    self_service = EXCLUDED.self_service, limits = EXCLUDED.limits,
		    limit_note = EXCLUDED.limit_note, sort_order = EXCLUDED.sort_order, updated_at = now()
		RETURNING `+planColumns,
		p.Key, strings.TrimSpace(p.Name), strings.TrimSpace(p.Description), p.SelfService, limits, strings.TrimSpace(p.LimitNote), p.SortOrder))
}

// Tenant is the plan-related part of one core.customer row.
type Tenant struct {
	CustomerID     string     `json:"customer_id"`
	Name           string     `json:"name"`
	Plan           string     `json:"plan"`
	LimitState     string     `json:"limit_state"`
	LimitReason    string     `json:"limit_reason,omitempty"`
	UsageCheckedAt *time.Time `json:"usage_checked_at,omitempty"`
}

// LoadTenant reads the tenant's row from the database that holds it.
func LoadTenant(ctx context.Context, db DB, customerID string) (Tenant, error) {
	var t Tenant
	err := db.QueryRow(ctx, `
		SELECT id::text, name, plan, limit_state, limit_reason, usage_checked_at
		FROM core.customer WHERE id = $1::uuid`, customerID,
	).Scan(&t.CustomerID, &t.Name, &t.Plan, &t.LimitState, &t.LimitReason, &t.UsageCheckedAt)
	if err != nil {
		return Tenant{}, fmt.Errorf("tenant %s: %w", customerID, err)
	}
	return t, nil
}

// SetPlan puts a tenant on a plan. The caller has already checked the key
// exists.
func SetPlan(ctx context.Context, db DB, customerID, planKey string) error {
	_, err := db.Exec(ctx, `UPDATE core.customer SET plan = $2, updated_at = now() WHERE id = $1::uuid`, customerID, planKey)
	return err
}

// CodeOverLimit is the one reason a tenant is read-only.
const CodeOverLimit = "over_limit"

// State is what a tenant's plan means right now — what /api/me reports and
// what the request guard acts on.
type State struct {
	Plan Plan `json:"plan"`
	// PlanKnown is false when the tenant names a plan that has no row. Such
	// a tenant is treated as unlimited: a catalog gap must never lock a
	// paying customer out.
	PlanKnown bool `json:"plan_known"`
	// ReadOnly means mutating requests are refused; Code says why and
	// Reason is the sentence shown to the person.
	ReadOnly       bool       `json:"read_only"`
	Code           string     `json:"code,omitempty"`
	Reason         string     `json:"reason,omitempty"`
	LimitState     string     `json:"limit_state"`
	LimitReason    string     `json:"limit_reason,omitempty"`
	UsageCheckedAt *time.Time `json:"usage_checked_at,omitempty"`
}

// Evaluate combines a plan and a tenant row into the state.
func Evaluate(p Plan, known bool, t Tenant) State {
	st := State{Plan: p, PlanKnown: known, LimitState: t.LimitState, LimitReason: t.LimitReason, UsageCheckedAt: t.UsageCheckedAt}
	if t.LimitState == "over" {
		st.ReadOnly = true
		st.Code = CodeOverLimit
		st.Reason = fmt.Sprintf("This tenant is over its plan's limits (%s). The workspace is read-only, except for deleting, until it is back within them.", t.LimitReason)
	}
	return st
}

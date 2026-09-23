package plan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
)

// Enforcer answers "may this tenant do that?" cheaply: the plan and tenant
// row are cached per tenant for a minute, so a request costs nothing until a
// limit has to be counted, and a count only happens for a limit the plan
// actually sets.
type Enforcer struct {
	control DB // where platform.plan lives
	ttl     time.Duration
	now     func() time.Time

	mu     sync.Mutex
	states map[string]cachedState
}

type cachedState struct {
	state   State
	expires time.Time
}

// NewEnforcer builds an enforcer reading plans from the control plane.
func NewEnforcer(control DB) *Enforcer {
	return &Enforcer{control: control, ttl: time.Minute, now: time.Now, states: map[string]cachedState{}}
}

// State returns the tenant's current state. tenantDB is the database holding
// the tenant's core.customer row (the tenant's own in dedicated mode).
func (e *Enforcer) State(ctx context.Context, tenantDB DB, customerID string) (State, error) {
	now := e.now()
	e.mu.Lock()
	if c, ok := e.states[customerID]; ok && now.Before(c.expires) {
		e.mu.Unlock()
		return c.state, nil
	}
	e.mu.Unlock()

	t, err := LoadTenant(ctx, tenantDB, customerID)
	if err != nil {
		return State{}, err
	}
	p, err := Get(ctx, e.control, t.Plan)
	known := true
	if errors.Is(err, ErrUnknownPlan) {
		p, known = Plan{Key: t.Plan, Name: t.Plan}, false
	} else if err != nil {
		return State{}, err
	}
	st := Evaluate(p, known, t)
	e.mu.Lock()
	if len(e.states) > 10000 {
		e.states = map[string]cachedState{}
	}
	e.states[customerID] = cachedState{state: st, expires: now.Add(e.ttl)}
	e.mu.Unlock()
	return st, nil
}

// Invalidate drops the cached state of one tenant (after its plan changed).
func (e *Enforcer) Invalidate(customerID string) {
	e.mu.Lock()
	delete(e.states, customerID)
	e.mu.Unlock()
}

// InvalidateAll drops every cached state (after a plan's limits changed).
func (e *Enforcer) InvalidateAll() {
	e.mu.Lock()
	e.states = map[string]cachedState{}
	e.mu.Unlock()
}

// LimitError is a refused creation: which limit, the plan's number and the
// tenant's. HTTP maps it to 402 so the console can offer the upgrade path.
type LimitError struct {
	Plan    string `json:"plan"`
	Limit   string `json:"limit"`
	Max     int    `json:"max"`
	Current int    `json:"current"`
	// Next is the plan's NextStep: where to go from here.
	Next string `json:"-"`
}

// Error is the sentence shown to the person, naming plan and numbers.
func (e *LimitError) Error() string {
	n := func(count int, noun string) string { return fmt.Sprintf("%d %s", count, plural(count, noun)) }
	more := " " + e.Next
	if e.Next == "" {
		more = " " + Plan{}.NextStep()
	}
	switch e.Limit {
	case "max_users":
		return fmt.Sprintf("The %s plan allows %s; this tenant has %d.%s", e.Plan, n(e.Max, "user"), e.Current, more)
	case "max_applications":
		return fmt.Sprintf("The %s plan allows %s; this tenant has %d.%s", e.Plan, n(e.Max, "application"), e.Current, more)
	case "max_models":
		return fmt.Sprintf("The %s plan allows %s; this tenant has %d.%s", e.Plan, n(e.Max, "model"), e.Current, more)
	case "max_metrics_per_model":
		return fmt.Sprintf("The %s plan allows %s per model; this model has %d.%s", e.Plan, n(e.Max, "metric"), e.Current, more)
	case "max_members_per_dimension":
		return fmt.Sprintf("The %s plan allows %s per dimension; this dimension has %d.%s", e.Plan, n(e.Max, "member"), e.Current, more)
	case "max_fact_rows_per_model":
		return fmt.Sprintf("The %s plan allows %s per model; this model has %d.%s", e.Plan, n(e.Max, "data row"), e.Current, more)
	case "max_ai_messages_per_day":
		return fmt.Sprintf("The %s plan allows %s per day and this tenant has sent %d today. Try again tomorrow.%s", e.Plan, n(e.Max, "AI message"), e.Current, more)
	case "max_integration_runs_per_day":
		return fmt.Sprintf("The %s plan allows %s per day and this tenant has run %d today. Try again tomorrow.%s", e.Plan, n(e.Max, "integration run"), e.Current, more)
	case "max_storage_mb":
		return fmt.Sprintf("The %s plan allows %d MB of storage; this tenant uses %d MB.%s", e.Plan, e.Max, e.Current, more)
	}
	return fmt.Sprintf("The %s plan allows %d (%s); this tenant has %d.%s", e.Plan, e.Max, e.Limit, e.Current, more)
}

func plural(n int, noun string) string {
	if n == 1 {
		return noun
	}
	return noun + "s"
}

// IsLimit reports whether err is a refused creation.
func IsLimit(err error) bool {
	var le *LimitError
	return errors.As(err, &le)
}

// check is every CheckX: skip when the plan has no such limit, else count
// and compare. adding is how many the caller is about to create.
func (e *Enforcer) check(ctx context.Context, db DB, customerID, limit string, maxOf func(Limits) int, count func(ctx context.Context) (int, error), adding int) error {
	st, err := e.State(ctx, db, customerID)
	if err != nil {
		return err
	}
	limitMax := maxOf(st.Plan.Limits)
	if limitMax <= 0 {
		return nil
	}
	current, err := count(ctx)
	if err != nil {
		return err
	}
	if current+adding > limitMax {
		return &LimitError{Plan: st.Plan.Name, Limit: limit, Max: limitMax, Current: current, Next: st.Plan.NextStep()}
	}
	return nil
}

// appsCTE scopes a query to one tenant's applications in a shared database
// (in a dedicated one every application is the tenant's, and the predicate
// is simply true for all of them).
const appsCTE = `WITH apps AS (
	SELECT app.id FROM core.application app
	WHERE app.customer_id = $1::uuid
	   OR app.workspace_id IN (SELECT id FROM core.workspace WHERE customer_id = $1::uuid)
)`

func countRow(ctx context.Context, db DB, sql string, args ...any) (int, error) {
	var n int
	err := db.QueryRow(ctx, sql, args...).Scan(&n)
	return n, err
}

// CheckUsers refuses when adding users would exceed max_users. A user of a
// tenant is one with the tenant as their own or a role in one of its
// workspaces — the same definition the usage analytics count with.
func (e *Enforcer) CheckUsers(ctx context.Context, db DB, customerID string, adding int) error {
	return e.check(ctx, db, customerID, "max_users", func(l Limits) int { return l.MaxUsers }, func(ctx context.Context) (int, error) {
		return countRow(ctx, db, `
			SELECT count(*) FROM identity."user" u
			WHERE u.disabled_at IS NULL
			  AND (u.customer_id = $1::uuid
			       OR EXISTS (SELECT 1 FROM identity.role_assignment ra JOIN core.workspace w ON w.id = ra.workspace_id
			                  WHERE ra.user_id = u.id AND w.customer_id = $1::uuid))`, customerID)
	}, adding)
}

// CheckApplications refuses when adding applications would exceed max_applications.
func (e *Enforcer) CheckApplications(ctx context.Context, db DB, customerID string, adding int) error {
	return e.check(ctx, db, customerID, "max_applications", func(l Limits) int { return l.MaxApplications }, func(ctx context.Context) (int, error) {
		return countRow(ctx, db, appsCTE+` SELECT count(*) FROM apps`, customerID)
	}, adding)
}

// CheckModels refuses when adding models would exceed max_models.
func (e *Enforcer) CheckModels(ctx context.Context, db DB, customerID string, adding int) error {
	return e.check(ctx, db, customerID, "max_models", func(l Limits) int { return l.MaxModels }, func(ctx context.Context) (int, error) {
		return countRow(ctx, db, appsCTE+` SELECT count(*) FROM core.model m WHERE m.application_id IN (SELECT id FROM apps)`, customerID)
	}, adding)
}

// CheckMetrics refuses when adding metrics to modelID would exceed
// max_metrics_per_model. Metrics are per revision, so the model's count is
// its largest revision. An empty modelID is a model about to be created.
func (e *Enforcer) CheckMetrics(ctx context.Context, db DB, customerID, modelID string, adding int) error {
	return e.check(ctx, db, customerID, "max_metrics_per_model", func(l Limits) int { return l.MaxMetricsPerModel }, func(ctx context.Context) (int, error) {
		if modelID == "" {
			return 0, nil
		}
		return countRow(ctx, db, `SELECT COALESCE(max(n), 0) FROM (SELECT count(*) AS n FROM model.metric_def WHERE model_id = $1::uuid GROUP BY revision_id) s`, modelID)
	}, adding)
}

// CheckMembers refuses when adding members to dimensionID would exceed
// max_members_per_dimension. An empty dimensionID is one about to be created.
func (e *Enforcer) CheckMembers(ctx context.Context, db DB, customerID, dimensionID string, adding int) error {
	return e.check(ctx, db, customerID, "max_members_per_dimension", func(l Limits) int { return l.MaxMembersPerDimension }, func(ctx context.Context) (int, error) {
		if dimensionID == "" {
			return 0, nil
		}
		return countRow(ctx, db, `SELECT count(*) FROM model.dimension_member WHERE dimension_id = $1::uuid`, dimensionID)
	}, adding)
}

// CheckFactRows refuses when adding rows to modelID would exceed
// max_fact_rows_per_model (all revisions together — it is a storage bound).
func (e *Enforcer) CheckFactRows(ctx context.Context, db DB, customerID, modelID string, adding int) error {
	return e.check(ctx, db, customerID, "max_fact_rows_per_model", func(l Limits) int { return l.MaxFactRowsPerModel }, func(ctx context.Context) (int, error) {
		if modelID == "" {
			return 0, nil
		}
		return countRow(ctx, db, `SELECT count(*) FROM runtime.fact_input WHERE model_id = $1::uuid`, modelID)
	}, adding)
}

// CheckStorage refuses a write once the tenant's data already fills
// max_storage_mb. It does not know how much the write adds — the sweep
// catches an overshoot within minutes — so it is the cheap request-time
// half of the storage bound, called where data rows arrive in bulk.
func (e *Enforcer) CheckStorage(ctx context.Context, db DB, customerID string) error {
	return e.check(ctx, db, customerID, "max_storage_mb", func(l Limits) int { return l.MaxStorageMB }, func(ctx context.Context) (int, error) {
		b, err := StorageBytes(ctx, db, customerID)
		return megabytes(b), err
	}, 1)
}

// megabytes rounds bytes up to whole MB, so 1 byte over a limit counts.
func megabytes(b int64) int {
	const mb = 1 << 20
	return int((b + mb - 1) / mb)
}

// StorageBytes measures the tenant's data. In a dedicated tenant database
// (pkg/tenantdb) it is the size of every table there, indexes and TOAST
// included — exact, and a catalog read. In a shared database the tables
// belong to every tenant at once, so the tenant's share is estimated: its
// row count in each of the data-bearing tables (facts, calculated results,
// cell history) times that table's average bytes per row from the planner
// statistics. Definitions, users and audit rows are small next to those
// and are left out of the estimate.
func StorageBytes(ctx context.Context, db DB, customerID string) (int64, error) {
	var current string
	if err := db.QueryRow(ctx, `SELECT current_database()`).Scan(&current); err != nil {
		return 0, err
	}
	if current == "tenant_"+strings.ReplaceAll(customerID, "-", "") {
		return dedicatedStorageBytes(ctx, db)
	}
	return sharedStorageBytes(ctx, db, customerID)
}

// dedicatedStorageBytes is every table of the database, indexes and TOAST
// included, catalogs excluded.
func dedicatedStorageBytes(ctx context.Context, db DB) (int64, error) {
	var b int64
	err := db.QueryRow(ctx, `
		SELECT COALESCE(sum(pg_total_relation_size(c.oid)), 0)::bigint
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'm') AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast')`).Scan(&b)
	return b, err
}

// sharedStorageBytes is the tenant's rows in each data table times that
// table's bytes per row (200 while the planner has no statistics yet).
func sharedStorageBytes(ctx context.Context, db DB, customerID string) (int64, error) {
	var b int64
	err := db.QueryRow(ctx, appsCTE+`,
		models AS (SELECT id FROM core.model WHERE application_id IN (SELECT id FROM apps)),
		width AS (
		    SELECT t.name,
		           COALESCE(sum(pg_total_relation_size(pt.relid))::numeric / NULLIF(sum(GREATEST(c.reltuples, 0)), 0), 200) AS bytes
		    FROM (VALUES ('runtime.fact_input'), ('runtime.calc_result'), ('runtime.fact_input_history')) t(name)
		    -- pg_partition_tree is empty for a plain table, so the table
		    -- itself is added; for a partitioned one it is the 0-byte parent.
		    CROSS JOIN LATERAL (SELECT t.name::regclass::oid AS relid UNION SELECT relid FROM pg_partition_tree(t.name::regclass)) pt
		    JOIN pg_class c ON c.oid = pt.relid
		    GROUP BY t.name),
		rows_of AS (
		    SELECT 'runtime.fact_input' AS name, count(*) AS n FROM runtime.fact_input WHERE model_id IN (SELECT id FROM models)
		    UNION ALL SELECT 'runtime.calc_result', count(*) FROM runtime.calc_result WHERE model_id IN (SELECT id FROM models)
		    UNION ALL SELECT 'runtime.fact_input_history', count(*) FROM runtime.fact_input_history WHERE model_id IN (SELECT id FROM models))
		SELECT COALESCE(sum(r.n * w.bytes), 0)::bigint FROM rows_of r JOIN width w ON w.name = r.name`, customerID).Scan(&b)
	return b, err
}

// CheckAIMessages refuses once the tenant has sent max_ai_messages_per_day
// user messages today (UTC day). Each message is what costs provider money;
// the per-user call caps in the AI handler stay as they are.
func (e *Enforcer) CheckAIMessages(ctx context.Context, db DB, customerID string) error {
	return e.check(ctx, db, customerID, "max_ai_messages_per_day", func(l Limits) int { return l.MaxAIMessagesPerDay }, func(ctx context.Context) (int, error) {
		return countRow(ctx, db, appsCTE+`
			SELECT count(*) FROM ai_assistant.message msg
			JOIN ai_assistant.session s ON s.id = msg.session_id
			WHERE msg.role = 'user' AND msg.created_at >= date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
			  AND s.application_id IN (SELECT id FROM apps)`, customerID)
	}, 1)
}

// CheckIntegrationRuns refuses once the tenant has run
// max_integration_runs_per_day integrations today (UTC day).
func (e *Enforcer) CheckIntegrationRuns(ctx context.Context, db DB, customerID string) error {
	return e.check(ctx, db, customerID, "max_integration_runs_per_day", func(l Limits) int { return l.MaxIntegrationRunsPerDay }, func(ctx context.Context) (int, error) {
		return countRow(ctx, db, appsCTE+`
			SELECT count(*) FROM model.integration_run ir
			JOIN model.integration_def d ON d.id = ir.integration_id
			JOIN core.model m ON m.id = d.model_id
			WHERE ir.created_at >= date_trunc('day', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC'
			  AND m.application_id IN (SELECT id FROM apps)`, customerID)
	}, 1)
}

// Sweep recounts one tenant against its plan and records the verdict on its
// row: limit_state 'over' with the first violation as the reason, or 'ok'.
// Daily limits are not part of it — they refuse at request time and reset
// by themselves. Returns the tenant as stored.
func (e *Enforcer) Sweep(ctx context.Context, db DB, customerID string) (Tenant, error) {
	t, err := LoadTenant(ctx, db, customerID)
	if err != nil {
		return Tenant{}, err
	}
	p, err := Get(ctx, e.control, t.Plan)
	if errors.Is(err, ErrUnknownPlan) {
		p = Plan{Key: t.Plan, Name: t.Plan}
	} else if err != nil {
		return Tenant{}, err
	}
	reason := ""
	if p.Limits.Any() {
		reason, err = e.firstViolation(ctx, db, customerID, p)
		if err != nil {
			return Tenant{}, fmt.Errorf("sweep tenant %s: %w", customerID, err)
		}
	}
	state := "ok"
	if reason != "" {
		state = "over"
	}
	if _, err := db.Exec(ctx, `UPDATE core.customer SET limit_state = $2, limit_reason = $3, usage_checked_at = now() WHERE id = $1::uuid`,
		customerID, state, reason); err != nil {
		return Tenant{}, err
	}
	e.Invalidate(customerID)
	return LoadTenant(ctx, db, customerID)
}

// firstViolation returns a sentence for the first limit the tenant exceeds,
// or "" when it is within every limit.
func (e *Enforcer) firstViolation(ctx context.Context, db DB, customerID string, p Plan) (string, error) {
	l := p.Limits
	over := func(limit string, max, current int) string {
		return (&LimitError{Plan: p.Name, Limit: limit, Max: max, Current: current, Next: p.NextStep()}).Error()
	}
	if l.MaxUsers > 0 {
		n, err := countRow(ctx, db, `
			SELECT count(*) FROM identity."user" u WHERE u.disabled_at IS NULL AND (u.customer_id = $1::uuid
			  OR EXISTS (SELECT 1 FROM identity.role_assignment ra JOIN core.workspace w ON w.id = ra.workspace_id
			             WHERE ra.user_id = u.id AND w.customer_id = $1::uuid))`, customerID)
		if err != nil {
			return "", err
		}
		if n > l.MaxUsers {
			return over("max_users", l.MaxUsers, n), nil
		}
	}
	if l.MaxApplications > 0 {
		n, err := countRow(ctx, db, appsCTE+` SELECT count(*) FROM apps`, customerID)
		if err != nil {
			return "", err
		}
		if n > l.MaxApplications {
			return over("max_applications", l.MaxApplications, n), nil
		}
	}
	if l.MaxModels > 0 {
		n, err := countRow(ctx, db, appsCTE+` SELECT count(*) FROM core.model m WHERE m.application_id IN (SELECT id FROM apps)`, customerID)
		if err != nil {
			return "", err
		}
		if n > l.MaxModels {
			return over("max_models", l.MaxModels, n), nil
		}
	}
	// The per-model and per-dimension limits: one query each over every
	// model of the tenant, reporting the worst offender.
	if l.MaxMetricsPerModel > 0 {
		var name string
		var n int
		err := db.QueryRow(ctx, appsCTE+`
			SELECT m.name, s.n FROM (
			    SELECT md.model_id, count(*) AS n FROM model.metric_def md
			    WHERE md.model_id IN (SELECT id FROM core.model WHERE application_id IN (SELECT id FROM apps))
			    GROUP BY md.model_id, md.revision_id) s
			JOIN core.model m ON m.id = s.model_id
			WHERE s.n > $2 ORDER BY s.n DESC LIMIT 1`, customerID, l.MaxMetricsPerModel).Scan(&name, &n)
		if err == nil {
			return over("max_metrics_per_model", l.MaxMetricsPerModel, n) + fmt.Sprintf(" (model %q)", name), nil
		} else if !isNoRows(err) {
			return "", err
		}
	}
	if l.MaxMembersPerDimension > 0 {
		var name string
		var n int
		err := db.QueryRow(ctx, appsCTE+`
			SELECT d.name, count(mem.id) AS n FROM model.dimension_def d
			JOIN model.dimension_member mem ON mem.dimension_id = d.id
			WHERE d.model_id IN (SELECT id FROM core.model WHERE application_id IN (SELECT id FROM apps))
			GROUP BY d.id, d.name HAVING count(mem.id) > $2 ORDER BY n DESC LIMIT 1`, customerID, l.MaxMembersPerDimension).Scan(&name, &n)
		if err == nil {
			return over("max_members_per_dimension", l.MaxMembersPerDimension, n) + fmt.Sprintf(" (dimension %q)", name), nil
		} else if !isNoRows(err) {
			return "", err
		}
	}
	if l.MaxFactRowsPerModel > 0 {
		var name string
		var n int
		err := db.QueryRow(ctx, appsCTE+`
			SELECT m.name, count(fi.model_id) AS n FROM core.model m
			JOIN runtime.fact_input fi ON fi.model_id = m.id
			WHERE m.application_id IN (SELECT id FROM apps)
			GROUP BY m.id, m.name HAVING count(fi.model_id) > $2 ORDER BY n DESC LIMIT 1`, customerID, l.MaxFactRowsPerModel).Scan(&name, &n)
		if err == nil {
			return over("max_fact_rows_per_model", l.MaxFactRowsPerModel, n) + fmt.Sprintf(" (model %q)", name), nil
		} else if !isNoRows(err) {
			return "", err
		}
	}
	if l.MaxStorageMB > 0 {
		b, err := StorageBytes(ctx, db, customerID)
		if err != nil {
			return "", err
		}
		if used := megabytes(b); used > l.MaxStorageMB {
			return over("max_storage_mb", l.MaxStorageMB, used), nil
		}
	}
	return "", nil
}

func isNoRows(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no rows in result set")
}

// RunSweep sweeps every tenant in db at every interval (and once at start).
// In dedicated mode a tenant database holds one customer; the control plane
// holds the tenants created in shared mode. Errors are logged, never fatal:
// a sweep that cannot run leaves the last verdict standing.
func RunSweep(ctx context.Context, db DB, e *Enforcer, interval time.Duration, log zerolog.Logger) {
	sweepAll := func() {
		rows, err := db.Query(ctx, `SELECT id::text FROM core.customer`)
		if err != nil {
			log.Warn().Err(err).Msg("plan sweep: could not list tenants")
			return
		}
		var ids []string
		for rows.Next() {
			var id string
			if rows.Scan(&id) == nil {
				ids = append(ids, id)
			}
		}
		rows.Close()
		for _, id := range ids {
			t, err := e.Sweep(ctx, db, id)
			if err != nil {
				log.Warn().Err(err).Str("tenant", id).Msg("plan sweep failed")
				continue
			}
			if t.LimitState == "over" {
				log.Info().Str("tenant", id).Str("reason", t.LimitReason).Msg("tenant is over its plan limits and read-only")
			}
		}
	}
	sweepAll()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepAll()
		}
	}
}

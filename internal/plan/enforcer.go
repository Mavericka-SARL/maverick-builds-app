package plan

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/mavericks-engine/mavericks/pkg/wake"
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

// StorageGuardCode is the SQLSTATE the storage guard (migration 135) refuses
// a write with: one that would take a workspace past its edition's limit.
const StorageGuardCode = "MV402"

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
		return fmt.Sprintf("The %s Edition allows %s; this tenant has %d.%s", e.Plan, n(e.Max, "user"), e.Current, more)
	case "max_applications":
		return fmt.Sprintf("The %s Edition allows %s; this tenant has %d.%s", e.Plan, n(e.Max, "application"), e.Current, more)
	case "max_models":
		return fmt.Sprintf("The %s Edition allows %s; this tenant has %d.%s", e.Plan, n(e.Max, "model"), e.Current, more)
	case "max_metrics_per_model":
		return fmt.Sprintf("The %s Edition allows %s per model; this model has %d.%s", e.Plan, n(e.Max, "metric"), e.Current, more)
	case "max_members_per_dimension":
		return fmt.Sprintf("The %s Edition allows %s per dimension; this dimension has %d.%s", e.Plan, n(e.Max, "member"), e.Current, more)
	case "max_fact_rows_per_model":
		return fmt.Sprintf("The %s Edition allows %s per model; this model has %d.%s", e.Plan, n(e.Max, "data row"), e.Current, more)
	case "max_ai_messages_per_day":
		return fmt.Sprintf("The %s Edition allows %s per day and this tenant has sent %d today. Try again tomorrow.%s", e.Plan, n(e.Max, "AI message"), e.Current, more)
	case "max_integration_runs_per_day":
		return fmt.Sprintf("The %s Edition allows %s per day and this tenant has run %d today. Try again tomorrow.%s", e.Plan, n(e.Max, "integration run"), e.Current, more)
	case "max_storage_mb":
		return fmt.Sprintf("The %s Edition allows %d MB of storage; this tenant uses %d MB.%s", e.Plan, e.Max, e.Current, more)
	}
	return fmt.Sprintf("The %s Edition allows %d (%s); this tenant has %d.%s", e.Plan, e.Max, e.Limit, e.Current, more)
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

// StorageBytes measures the tenant's data: the rows it holds, not the files
// they sit in. PostgreSQL keeps a deleted row's space in a table's files
// for later writes, so the files do not shrink when data is deleted —
// measured by them, a workspace that deleted a model stayed the size it
// was. Each table counts its live rows (pg_stat_user_tables, which a
// delete changes at once) times the row width the planner statistics give
// (pg_stats), and its indexes and TOAST in the same proportion of the
// table's files (runtime.live_tables); the data-bearing tables (facts,
// calculated results, cell history) count the tenant's rows exactly, times
// the table's live bytes per row (ModelStorage). In a dedicated tenant
// database (pkg/tenantdb) every other table counts too; in a shared one the
// tables belong to every tenant at once, so only the data-bearing ones do. Definitions, users and
// audit rows are small next to those and are left out of the shared
// estimate.
func StorageBytes(ctx context.Context, db DB, customerID string) (int64, error) {
	dedicated, err := dedicatedTo(ctx, db, customerID)
	if err != nil {
		return 0, err
	}
	if dedicated {
		return dedicatedStorageBytes(ctx, db, customerID)
	}
	return sharedStorageBytes(ctx, db, customerID)
}

// dedicatedTo reports whether db is customerID's own database (pkg/tenantdb).
func dedicatedTo(ctx context.Context, db DB, customerID string) (bool, error) {
	var current string
	if err := db.QueryRow(ctx, `SELECT current_database()`).Scan(&current); err != nil {
		return false, err
	}
	return current == "tenant_"+strings.ReplaceAll(customerID, "-", ""), nil
}

// liveTablesCTE is every ordinary table of the database with its live rows
// and the bytes they take (runtime.live_tables, migration 135: the same
// measure the storage guard refuses writes by).
const liveTablesCTE = `WITH live_tables AS (SELECT * FROM runtime.live_tables())`

// dedicatedStorageBytes is the live data of every table in the database:
// the models' data as ModelStorage counts it — rows counted exactly, so a
// statistics count an ANALYZE has just reset cannot leave a deleted model
// in the figure — and every other table as its statistics give it.
func dedicatedStorageBytes(ctx context.Context, db DB, customerID string) (int64, error) {
	models, err := ModelStorage(ctx, db, customerID)
	if err != nil {
		return 0, err
	}
	other, err := otherTablesBytes(ctx, db)
	return total(models) + other, err
}

// dataTables are the tables a model's data lives in (ModelStorage counts
// their rows), as partition roots.
const dataTables = `('runtime.fact_input'::regclass::oid, 'runtime.calc_result'::regclass::oid, 'runtime.fact_input_history'::regclass::oid)`

// otherTablesBytes is the live data of every table but the dataTables.
func otherTablesBytes(ctx context.Context, db DB) (int64, error) {
	var b int64
	err := db.QueryRow(ctx, liveTablesCTE+`
		SELECT COALESCE(sum(bytes), 0)::bigint FROM live_tables WHERE root NOT IN `+dataTables).Scan(&b)
	return b, err
}

// sharedStorageBytes is the sum of the tenant's models (ModelStorage).
func sharedStorageBytes(ctx context.Context, db DB, customerID string) (int64, error) {
	models, err := ModelStorage(ctx, db, customerID)
	return total(models), err
}

func total(models map[string]int64) int64 {
	var b int64
	for _, n := range models {
		b += n
	}
	return b
}

// ModelStorage is each of the tenant's models with its rows in each data
// table times that table's bytes per row (runtime.bytes_per_row) — the
// model's share of the data those tables hold, indexes included.
// Definitions are small next to the data and left out.
func ModelStorage(ctx context.Context, db DB, customerID string) (map[string]int64, error) {
	rows, err := db.Query(ctx, liveTablesCTE+`,
		`+strings.TrimPrefix(appsCTE, "WITH ")+`,
		models AS (SELECT id FROM core.model WHERE application_id IN (SELECT id FROM apps)),
		width AS (
		    SELECT t.name, runtime.bytes_per_row(t.name::regclass::oid) AS bytes
		    FROM (VALUES ('runtime.fact_input'), ('runtime.calc_result'), ('runtime.fact_input_history')) t(name)),
		rows_of AS (
		    SELECT 'runtime.fact_input' AS name, model_id, count(*) AS n FROM runtime.fact_input WHERE model_id IN (SELECT id FROM models) GROUP BY model_id
		    UNION ALL SELECT 'runtime.calc_result', model_id, count(*) FROM runtime.calc_result WHERE model_id IN (SELECT id FROM models) GROUP BY model_id
		    UNION ALL SELECT 'runtime.fact_input_history', model_id, count(*) FROM runtime.fact_input_history WHERE model_id IN (SELECT id FROM models) GROUP BY model_id)
		SELECT m.id::text, COALESCE(sum(r.n * w.bytes), 0)::bigint
		FROM models m LEFT JOIN rows_of r ON r.model_id = m.id LEFT JOIN width w ON w.name = r.name
		GROUP BY m.id`, customerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var b int64
		if err := rows.Scan(&id, &b); err != nil {
			return nil, err
		}
		out[id] = b
	}
	return out, rows.Err()
}

// recordModelStorage keeps the measurement in runtime.model_storage for the
// console; a row changes only when its size did.
func recordModelStorage(ctx context.Context, db DB, models map[string]int64) error {
	ids := make([]string, 0, len(models))
	sizes := make([]int64, 0, len(models))
	for id, b := range models {
		ids = append(ids, id)
		sizes = append(sizes, b)
	}
	_, err := db.Exec(ctx, `
		INSERT INTO runtime.model_storage (model_id, bytes)
		SELECT * FROM unnest($1::uuid[], $2::bigint[])
		ON CONFLICT (model_id) DO UPDATE SET bytes = EXCLUDED.bytes
		WHERE runtime.model_storage.bytes <> EXCLUDED.bytes`, ids, sizes)
	return err
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
// row: limit_state 'over' with the first violation as the reason, or 'ok',
// and the storage it measured.
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
	// Storage is recorded so a tenant admin sees the space used, the space
	// left under a limit, and each model's share. Measured where it is cheap
	// or needed anyway: a dedicated database's live data is a catalog read,
	// while the shared-mode estimate counts the tenant's rows, so there it
	// runs only for a plan that limits storage.
	var storage *int64
	dedicated, err := dedicatedTo(ctx, db, customerID)
	if err != nil {
		return Tenant{}, fmt.Errorf("sweep tenant %s: %w", customerID, err)
	}
	if dedicated || p.Limits.MaxStorageMB > 0 {
		// The storage guard counts from this measurement: each table's insert
		// and delete counters now, before the rows are counted, so a row
		// written in between is counted twice rather than not at all
		// (runtime.storage_used, migration 135).
		if dedicated {
			if _, err := db.Exec(ctx, `
				INSERT INTO runtime.storage_mark (relid, ins, del)
				SELECT relid, n_tup_ins, n_tup_del FROM pg_stat_user_tables
				ON CONFLICT (relid) DO UPDATE SET ins = EXCLUDED.ins, del = EXCLUDED.del`); err != nil {
				return Tenant{}, fmt.Errorf("sweep tenant %s: storage mark: %w", customerID, err)
			}
		}
		models, err := ModelStorage(ctx, db, customerID)
		if err == nil {
			err = recordModelStorage(ctx, db, models)
		}
		b := total(models)
		if err == nil && dedicated {
			var other int64
			other, err = otherTablesBytes(ctx, db)
			b += other
		}
		if err != nil {
			return Tenant{}, fmt.Errorf("sweep tenant %s: storage: %w", customerID, err)
		}
		storage = &b
	}
	reason := ""
	if p.Limits.Any() {
		reason, err = e.firstViolation(ctx, db, customerID, p, storage)
		if err != nil {
			return Tenant{}, fmt.Errorf("sweep tenant %s: %w", customerID, err)
		}
	}
	state := "ok"
	if reason != "" {
		state = "over"
	}
	// The limit goes on the tenant's own row for the storage guard, which
	// refuses a write that would cross it (migration 135) — in a dedicated
	// database only: a shared one holds every tenant's rows at once.
	var limitBytes *int64
	var limitText *string
	if dedicated && p.Limits.MaxStorageMB > 0 {
		b := int64(p.Limits.MaxStorageMB) << 20
		txt := fmt.Sprintf("The %s Edition allows %d MB of storage. %s", p.Name, p.Limits.MaxStorageMB, p.NextStep())
		limitBytes, limitText = &b, &txt
	}
	if _, err := db.Exec(ctx, `UPDATE core.customer SET limit_state = $2, limit_reason = $3, storage_bytes = $4, usage_checked_at = now(),
		storage_limit_bytes = $5, storage_limit_text = $6 WHERE id = $1::uuid`,
		customerID, state, reason, storage, limitBytes, limitText); err != nil {
		return Tenant{}, err
	}
	if dedicated {
		if _, err := db.Exec(ctx, `SELECT runtime.set_storage_guard($1)`, limitBytes != nil); err != nil {
			return Tenant{}, fmt.Errorf("sweep tenant %s: storage guard: %w", customerID, err)
		}
	}
	e.Invalidate(customerID)
	t, err = LoadTenant(ctx, db, customerID)
	if err != nil {
		return Tenant{}, err
	}
	// Deleted rows' disk goes back to the server, in every edition.
	if dedicated {
		if _, err := Compact(ctx, db, false); err != nil {
			return t, fmt.Errorf("sweep tenant %s: compact: %w", customerID, err)
		}
	}
	return t, nil
}

// firstViolation returns a sentence for the first limit the tenant exceeds,
// or "" when it is within every limit.
func (e *Enforcer) firstViolation(ctx context.Context, db DB, customerID string, p Plan, storageBytes *int64) (string, error) {
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
	if l.MaxStorageMB > 0 && storageBytes != nil {
		if used := megabytes(*storageBytes); used > l.MaxStorageMB {
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
func RunSweep(ctx context.Context, db DB, e *Enforcer, interval time.Duration, log zerolog.Logger, gate wake.Gate) {
	sweepAll := func(ctx context.Context) bool {
		rows, err := db.Query(ctx, `SELECT id::text FROM core.customer`)
		if err != nil {
			log.Warn().Err(err).Msg("edition limits sweep: could not list tenants")
			return false
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
				log.Warn().Err(err).Str("tenant", id).Msg("edition limits sweep failed")
				continue
			}
			if t.LimitState == "over" {
				log.Info().Str("tenant", id).Str("reason", t.LimitReason).Msg("tenant is over its edition limits and read-only")
			}
		}
		return false
	}
	// A sweep changes no work for other loops: it touches nothing.
	wake.Loop(ctx, wake.OrTimer(gate), interval, sweepAll)
}

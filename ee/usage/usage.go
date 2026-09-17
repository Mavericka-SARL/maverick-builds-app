// Package usage is the enterprise per-tenant usage view: who has been
// active, how much of the model exists, what ran, and how much space it
// takes. Licensed under ee/LICENSE; gated by license.FeatureUsageAnalytics.
//
// Every number is a count over tables the platform already keeps; nothing
// is sampled or estimated, and nothing here writes. "Active" means an
// account the gateway saw (identity.user.last_seen_at) or that signed in
// (last_login_at) within the period.
package usage

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Tenant is one customer's usage.
type Tenant struct {
	CustomerID string    `json:"customer_id"`
	Name       string    `json:"name"`
	Plan       string    `json:"plan"`
	CreatedAt  time.Time `json:"created_at"`
	// People.
	Users       int `json:"users"`
	ActiveUsers int `json:"active_users"`
	// What exists.
	Applications int   `json:"applications"`
	Models       int   `json:"models"`
	Revisions    int   `json:"revisions"`
	FactRows     int64 `json:"fact_rows"`
	CalcRows     int64 `json:"calc_rows"`
	FormRecords  int64 `json:"form_records"`
	// Space: files people uploaded, and the database when the tenant has
	// its own (0 on a shared database — the number would be everyone's).
	ObjectBytes int64 `json:"object_bytes"`
	DBBytes     int64 `json:"db_bytes"`
	// What ran in the period.
	IntegrationRuns   int `json:"integration_runs"`
	WorkflowInstances int `json:"workflow_instances"`
	AIMessages        int `json:"ai_messages"`
	AuditEvents       int `json:"audit_events"`
	// The tenant's newest audit event, whenever it was.
	LastActivityAt *time.Time `json:"last_activity_at,omitempty"`
}

// ParsePeriod accepts "7d", "30d", "90d", "365d" (default 30d).
func ParsePeriod(s string) (int, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 30, nil
	}
	n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
	if err != nil || n < 1 || n > 365 {
		return 0, fmt.Errorf("period must be a number of days between 1 and 365, e.g. 30d")
	}
	return n, nil
}

// Snapshot counts one tenant's usage in the database that holds it. dbName,
// when set, is the tenant's own database, measured with pg_database_size.
func Snapshot(ctx context.Context, pool *pgxpool.Pool, customerID string, since time.Time, dbName string) (Tenant, error) {
	var t Tenant
	err := pool.QueryRow(ctx, `
		WITH apps AS (
		    SELECT app.id FROM core.application app
		    WHERE app.customer_id = $1::uuid
		       OR app.workspace_id IN (SELECT id FROM core.workspace WHERE customer_id = $1::uuid)
		), people AS (
		    SELECT u.id, u.last_seen_at, u.last_login_at FROM identity."user" u
		    WHERE u.customer_id = $1::uuid
		       OR EXISTS (SELECT 1 FROM identity.role_assignment ra JOIN core.workspace w ON w.id = ra.workspace_id
		                  WHERE ra.user_id = u.id AND w.customer_id = $1::uuid)
		), models AS (
		    SELECT m.id FROM core.model m JOIN apps ON apps.id = m.application_id
		)
		SELECT c.id::text, c.name, c.plan, c.created_at,
		       (SELECT count(*) FROM people),
		       (SELECT count(*) FROM people WHERE last_seen_at >= $2 OR last_login_at >= $2),
		       (SELECT count(*) FROM apps),
		       (SELECT count(*) FROM models),
		       (SELECT count(*) FROM model.revision r JOIN models ON models.id = r.model_id),
		       (SELECT count(*) FROM runtime.fact_input fi JOIN models ON models.id = fi.model_id),
		       (SELECT count(*) FROM runtime.calc_result cr JOIN models ON models.id = cr.model_id),
		       (SELECT count(*) FROM runtime.form_record fr JOIN model.form_def fd ON fd.id = fr.form_id JOIN models ON models.id = fd.model_id),
		       (SELECT COALESCE(sum(o.size_bytes),0) FROM storage.object o JOIN people ON people.id = o.created_by),
		       (SELECT count(*) FROM model.integration_run ir JOIN model.integration_def d ON d.id = ir.integration_id JOIN models ON models.id = d.model_id WHERE ir.created_at >= $2),
		       (SELECT count(*) FROM workflow.workflow_instance wi JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id JOIN apps ON apps.id = wd.application_id WHERE wi.started_at >= $2),
		       (SELECT count(*) FROM ai_assistant.message msg JOIN ai_assistant.session s ON s.id = msg.session_id JOIN apps ON apps.id = s.application_id WHERE msg.created_at >= $2),
		       (SELECT count(*) FROM audit.audit_event ae LEFT JOIN identity."user" u ON u.id = ae.actor_user_id
		         WHERE ae.occurred_at >= $2 AND (ae.application_id IN (SELECT id FROM apps) OR (ae.application_id IS NULL AND u.id IN (SELECT id FROM people)))),
		       (SELECT max(ae.occurred_at) FROM audit.audit_event ae LEFT JOIN identity."user" u ON u.id = ae.actor_user_id
		         WHERE ae.application_id IN (SELECT id FROM apps) OR (ae.application_id IS NULL AND u.id IN (SELECT id FROM people)))
		FROM core.customer c WHERE c.id = $1::uuid
	`, customerID, since).Scan(&t.CustomerID, &t.Name, &t.Plan, &t.CreatedAt,
		&t.Users, &t.ActiveUsers, &t.Applications, &t.Models, &t.Revisions, &t.FactRows, &t.CalcRows, &t.FormRecords,
		&t.ObjectBytes, &t.IntegrationRuns, &t.WorkflowInstances, &t.AIMessages, &t.AuditEvents, &t.LastActivityAt)
	if err != nil {
		return Tenant{}, fmt.Errorf("usage for tenant %s: %w", customerID, err)
	}
	if dbName != "" {
		_ = pool.QueryRow(ctx, `SELECT pg_database_size($1)`, dbName).Scan(&t.DBBytes)
	}
	return t, nil
}

// Package approvalsnap keeps the figures an approved workflow run approved
// and says which have changed since (owner decision 2026-11-13).
//
// A run whose context names Dimension members (a period, a region) locks
// that scope while its approval is open and once it is approved. Inputs the
// scope's figures read but that lack its dimension — a tax rate by entity, a
// posting account, a setting — stay editable by design, so an approved
// figure can still move: in the consolidation kit a tax rate changed after the
// CFO's release moved September's total assets 1,946.75 → 1,946.50. When such
// a run finishes approved, Take keeps the scope's inputs and calculated
// values; Changes compares them with the current ones, for the workflow and
// the dashboards to say "changed since approval".
package approvalsnap

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/mavericks-engine/mavericks/internal/writeguard"
	"github.com/mavericks-engine/mavericks/pkg/dbx"
)

type contextVar struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	DataType    string `json:"data_type"`
	DimensionID string `json:"dimension_id"`
}

// binding is one Dimension member field of a run with its value.
type binding struct{ dimID, code, label string }

// scope is what an approved run locked: its member bindings and, when its
// Metric fields name any, those metrics (lowercased names); none is every
// metric.
type scope struct {
	appID    string
	bindings []binding
	metrics  []string
}

// runScope reads instanceID's scope, if it is a finished, approved, real run
// that names members; ok false otherwise.
func runScope(ctx context.Context, db dbx.DB, instanceID string) (scope, bool, error) {
	var ctxJSON, schemaJSON []byte
	var sc scope
	var approved, testRun bool
	err := db.QueryRow(ctx, `
		SELECT COALESCE(wi.context, '{}'::jsonb), COALESCE(wi.context_schema_snapshot, wd.context_schema, '[]'::jsonb),
		       wd.application_id::text, wi.test_run, `+writeguard.ReopenableSQL+`
		FROM workflow.workflow_instance wi
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		`+writeguard.LastApprovalJoinSQL+`
		WHERE wi.id = $1::uuid`, instanceID).Scan(&ctxJSON, &schemaJSON, &sc.appID, &testRun, &approved)
	if err != nil {
		return sc, false, err
	}
	if testRun || !approved {
		return sc, false, nil
	}
	values := map[string]any{}
	_ = json.Unmarshal(ctxJSON, &values)
	var vars []contextVar
	_ = json.Unmarshal(schemaJSON, &vars)
	for _, v := range vars {
		val := strings.TrimSpace(fmt.Sprint(values[v.Key]))
		if values[v.Key] == nil || val == "" {
			continue
		}
		switch v.DataType {
		case "Dimension member":
			if v.DimensionID != "" {
				sc.bindings = append(sc.bindings, binding{dimID: v.DimensionID, code: val, label: v.Label})
			}
		case "Metric":
			sc.metrics = append(sc.metrics, strings.ToLower(val))
		}
	}
	return sc, len(sc.bindings) > 0, nil
}

// scopeSQL is the condition, on alias t (a fact_input or calc_result row),
// that it lies in sc: each binding's dimension absent or at its member, and
// at least one present — as a write is inside a lock (writeguard). Arguments
// are appended to args from position len(args)+1.
func scopeSQL(sc scope, args *[]any) string {
	var each, some []string
	for _, b := range sc.bindings {
		*args = append(*args, b.dimID, b.code)
		d, c := len(*args)-1, len(*args)
		each = append(each, fmt.Sprintf("(NOT t.dim_members ? $%d OR t.dim_members->>$%d = $%d)", d, d, c))
		some = append(some, fmt.Sprintf("t.dim_members ? $%d", d))
	}
	cond := strings.Join(each, " AND ") + " AND (" + strings.Join(some, " OR ") + ")"
	if len(sc.metrics) > 0 {
		*args = append(*args, sc.metrics)
		cond += fmt.Sprintf(` AND EXISTS (SELECT 1 FROM model.metric_def md WHERE md.id = t.metric_id
			AND (lower(md.name) = ANY($%d) OR md.id::text = ANY($%d)))`, len(*args), len(*args))
	}
	return cond
}

// Take keeps the figures instanceID approved: the inputs and calculated
// values of its scope in the active revision of each of its application's
// models, except metrics whose approval_watch is off. A run that is no approved real run, or names no member, keeps
// nothing. It answers how many values it kept.
func Take(ctx context.Context, db dbx.DB, instanceID string) (int64, error) {
	sc, ok, err := runScope(ctx, db, instanceID)
	if err != nil || !ok {
		return 0, err
	}
	args := []any{instanceID, sc.appID}
	// A metric a developer marked as no approved figure — a view that moves
	// with the calendar (year to date at the last closed month, a rolling
	// window) — is not kept (migration 189).
	cond := scopeSQL(sc, &args) + `
		AND EXISTS (SELECT 1 FROM model.metric_def w WHERE w.id = t.metric_id AND w.approval_watch)`
	var kept int64
	for _, src := range []struct{ table, kind string }{{"runtime.fact_input", "input"}, {"runtime.calc_result", "calc"}} {
		tag, err := db.Exec(ctx, `
			INSERT INTO workflow.approval_snapshot (instance_id, revision_id, kind, metric_id, dim_members, value, text_value)
			SELECT $1::uuid, t.revision_id, '`+src.kind+`', t.metric_id, t.dim_members, sum(t.value), max(t.text_value)
			FROM `+src.table+` t
			JOIN core.model cm ON cm.active_revision_id = t.revision_id AND cm.application_id = $2::uuid
			WHERE `+cond+`
			GROUP BY t.revision_id, t.metric_id, t.dim_members
			ON CONFLICT DO NOTHING`, args...)
		if err != nil {
			return kept, fmt.Errorf("keep the approved %s values: %w", src.kind, err)
		}
		kept += tag.RowsAffected()
	}
	return kept, nil
}

// Retake replaces what instanceID kept with its figures as they are now:
// for an approval whose own on_approve freeze wrote values after the run
// completed (Take ran in CompleteStep, before the freeze), so the run does not
// report its own frozen values as "changed since approval" — every approval
// with a freeze did, at once (55 figures on a project plan, 2026-11-27).
func Retake(ctx context.Context, db dbx.DB, instanceID string) (int64, error) {
	if _, err := db.Exec(ctx, `DELETE FROM workflow.approval_snapshot WHERE instance_id = $1::uuid`, instanceID); err != nil {
		return 0, fmt.Errorf("drop the kept values: %w", err)
	}
	return Take(ctx, db, instanceID)
}

// Change is one approved figure that is not what was approved.
type Change struct {
	MetricID     string            `json:"metric_id"`
	Metric       string            `json:"metric"`
	Members      map[string]string `json:"members"` // dimension label → member label
	memberIDs    []string
	Approved     *float64 `json:"approved,omitempty"`
	Now          *float64 `json:"now,omitempty"`
	ApprovedText string   `json:"approved_text,omitempty"`
	NowText      string   `json:"now_text,omitempty"`
}

// MemberIDs are the ids of the members the change is at, for a caller that
// leaves out what a reader may not see.
func (c Change) MemberIDs() []string { return c.memberIDs }

// RunChanges is an approved run whose figures changed since its approval.
type RunChanges struct {
	InstanceID string    `json:"instance_id"`
	Workflow   string    `json:"workflow"`
	Scope      string    `json:"scope"` // "Period: Sep 2026"
	ApprovedAt time.Time `json:"approved_at"`
	Changed    int       `json:"changed"`
	Examples   []Change  `json:"examples"`
	// CanReopen: the reader may reopen the run (set by the caller); a reader
	// who may not is told who can — ReopenRoles, or the application's
	// business admins and developers when it names none.
	CanReopen   bool     `json:"can_reopen"`
	ReopenRoles []string `json:"reopen_roles,omitempty"`
}

// Changes are the approved runs of appID (instanceID: that one only) whose
// kept figures differ from the current ones, newest first, each with up to
// examples changes, the largest first.
func Changes(ctx context.Context, db dbx.DB, appID, instanceID string, examples int) ([]RunChanges, error) {
	rows, err := db.Query(ctx, `
		SELECT wi.id::text, wd.name, COALESCE(wi.completed_at, wi.started_at),
		       COALESCE(wi.context, '{}'::jsonb), COALESCE(wi.context_schema_snapshot, wd.context_schema, '[]'::jsonb)
		FROM workflow.workflow_instance wi
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		`+writeguard.LastApprovalJoinSQL+`
		WHERE wd.application_id = $1::uuid AND NOT wi.test_run AND `+writeguard.ReopenableSQL+`
		  AND ($2 = '' OR wi.id::text = $2)
		  AND EXISTS (SELECT 1 FROM workflow.approval_snapshot s WHERE s.instance_id = wi.id)
		ORDER BY wi.completed_at DESC NULLS LAST
		LIMIT 20`, appID, instanceID)
	if err != nil {
		return nil, err
	}
	var runs []RunChanges
	var contexts, schemas [][]byte
	for rows.Next() {
		var r RunChanges
		var c, s []byte
		if err := rows.Scan(&r.InstanceID, &r.Workflow, &r.ApprovedAt, &c, &s); err != nil {
			rows.Close()
			return nil, err
		}
		runs = append(runs, r)
		contexts, schemas = append(contexts, c), append(schemas, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []RunChanges{}
	for i := range runs {
		r := &runs[i]
		r.Scope = scopeLabel(ctx, db, contexts[i], schemas[i])
		changes, n, err := runChanges(ctx, db, r.InstanceID, examples)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			continue
		}
		r.Changed, r.Examples = n, changes
		out = append(out, *r)
	}
	return out, nil
}

// runChanges compares instanceID's kept figures with the current ones: how
// many differ, and up to limit of them, the largest change first.
func runChanges(ctx context.Context, db dbx.DB, instanceID string, limit int) ([]Change, int, error) {
	rows, err := db.Query(ctx, `
		WITH snap AS (
			-- A metric no longer watched (approval_watch false: a view that
			-- moves with the calendar) is not reported, kept or not.
			SELECT a.revision_id, a.kind, a.metric_id, a.dim_members, a.value, a.text_value
			FROM workflow.approval_snapshot a
			JOIN model.metric_def w ON w.id = a.metric_id AND w.approval_watch
			WHERE a.instance_id = $1::uuid
		), cur AS (
			SELECT 'input' AS kind, t.revision_id, t.metric_id, t.dim_members, sum(t.value) AS value, max(t.text_value) AS text_value
			FROM runtime.fact_input t
			JOIN (SELECT DISTINCT revision_id, metric_id FROM snap WHERE kind = 'input') k USING (revision_id, metric_id)
			GROUP BY t.revision_id, t.metric_id, t.dim_members
			UNION ALL
			SELECT 'calc', t.revision_id, t.metric_id, t.dim_members, sum(t.value), max(t.text_value)
			FROM runtime.calc_result t
			JOIN (SELECT DISTINCT revision_id, metric_id FROM snap WHERE kind = 'calc') k USING (revision_id, metric_id)
			GROUP BY t.revision_id, t.metric_id, t.dim_members
		)
		SELECT s.metric_id::text, s.dim_members, s.value::float8, c.value::float8,
		       COALESCE(s.text_value, ''), COALESCE(c.text_value, ''),
		       COALESCE(NULLIF(md.label, ''), md.name, '')
		FROM snap s
		LEFT JOIN cur c ON c.kind = s.kind AND c.revision_id = s.revision_id AND c.metric_id = s.metric_id AND c.dim_members = s.dim_members
		LEFT JOIN model.metric_def md ON md.id = s.metric_id
		WHERE abs(COALESCE(s.value, 0) - COALESCE(c.value, 0)) > 1e-9
		   OR COALESCE(s.text_value, '') <> COALESCE(c.text_value, '')`, instanceID)
	if err != nil {
		return nil, 0, err
	}
	var all []Change
	for rows.Next() {
		var ch Change
		var dims []byte
		var approved, now *float64
		if err := rows.Scan(&ch.MetricID, &dims, &approved, &now, &ch.ApprovedText, &ch.NowText, &ch.Metric); err != nil {
			rows.Close()
			return nil, 0, err
		}
		ch.Approved, ch.Now = approved, now
		codes := map[string]string{}
		_ = json.Unmarshal(dims, &codes)
		ch.Members = codes // dimension id → code; labelled below
		all = append(all, ch)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	diff := func(c Change) float64 {
		a, b := 0.0, 0.0
		if c.Approved != nil {
			a = *c.Approved
		}
		if c.Now != nil {
			b = *c.Now
		}
		return math.Abs(a - b)
	}
	sort.SliceStable(all, func(i, j int) bool { return diff(all[i]) > diff(all[j]) })
	n := len(all)
	if limit >= 0 && len(all) > limit {
		all = all[:limit]
	}
	for i := range all {
		if err := labelMembers(ctx, db, &all[i]); err != nil {
			return nil, 0, err
		}
	}
	return all, n, nil
}

// labelMembers turns a change's dimension id → code into dimension label →
// member label, and keeps the member ids.
func labelMembers(ctx context.Context, db dbx.DB, c *Change) error {
	out := map[string]string{}
	for dimID, code := range c.Members {
		var dim, member, id string
		err := db.QueryRow(ctx, `
			SELECT COALESCE(NULLIF(d.label, ''), d.name), COALESCE(NULLIF(m.label, ''), m.code), m.id::text
			FROM model.dimension_def d JOIN model.dimension_member m ON m.dimension_id = d.id
			WHERE d.id::text = $1 AND m.code = $2`, dimID, code).Scan(&dim, &member, &id)
		if err != nil {
			dim, member = dimID, code
		} else {
			c.memberIDs = append(c.memberIDs, id)
		}
		out[dim] = member
	}
	c.Members = out
	return nil
}

// scopeLabel is a run's member fields as "Label: member", joined.
func scopeLabel(ctx context.Context, db dbx.DB, ctxJSON, schemaJSON []byte) string {
	values := map[string]any{}
	_ = json.Unmarshal(ctxJSON, &values)
	var vars []contextVar
	_ = json.Unmarshal(schemaJSON, &vars)
	var parts []string
	for _, v := range vars {
		if v.DataType != "Dimension member" || values[v.Key] == nil {
			continue
		}
		code := fmt.Sprint(values[v.Key])
		member := code
		_ = db.QueryRow(ctx, `SELECT COALESCE(NULLIF(label, ''), code) FROM model.dimension_member
			WHERE dimension_id::text = $1 AND code = $2`, v.DimensionID, code).Scan(&member)
		label := v.Label
		if label == "" {
			label = v.Key
		}
		if i := strings.Index(label, " ("); i > 0 {
			label = label[:i]
		}
		parts = append(parts, label+": "+member)
	}
	return strings.Join(parts, " · ")
}

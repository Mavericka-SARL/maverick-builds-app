package writeguard

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// A workflow instance locks the crossing of its "Dimension member" fields,
// not each of them alone: a budget approval for Region = Europe and Version
// = Budget locks Europe's budget. Until 2026-10-25 the fields were OR-ed, so
// approving Europe's budget also refused every actual written for France
// or Germany, and a Version field locked that version in every region. A
// field left empty does not narrow the scope; a field whose dimension the
// write does not have does not either — a France driver with no version is
// part of Europe's budget — but at least one field's dimension must be in
// the write, so a model-wide value stays outside every regional lock.

// workflowLock is one workflow instance of an application as a lock.
type workflowLock struct {
	bindings    []lockBinding   // its Dimension member fields with a value, in the schema's order
	metricNames map[string]bool // lowercased names from its Metric fields; empty = every metric
	locked      bool            // awaiting an approval, or approved
}

// lockBinding is one Dimension member field's value.
type lockBinding struct{ dimID, code string }

// scopeKey identifies the member scope: a newer instance of the same scope
// supersedes an older one for the metrics it covers.
func (l workflowLock) scopeKey() string {
	parts := make([]string, len(l.bindings))
	for i, b := range l.bindings {
		parts[i] = b.dimID + "=" + b.code
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x1f")
}

// covers reports whether a write is inside l's member scope. covered is the
// write's members and their ancestors: dimension ID → codes.
func (l workflowLock) covers(covered map[string]map[string]bool) bool {
	present := false
	for _, b := range l.bindings {
		codes, ok := covered[b.dimID]
		if !ok {
			continue
		}
		present = true
		if !codes[b.code] {
			return false
		}
	}
	return present
}

// coversMetric reports whether l locks the metric named name (lowercased;
// "" = the metric is not known, which every lock covers).
func (l workflowLock) coversMetric(name string) bool {
	return len(l.metricNames) == 0 || name == "" || l.metricNames[name]
}

// loadWorkflowLocks reads the workflow instances of modelID's application,
// newest first, as locks.
func loadWorkflowLocks(ctx context.Context, pool *pgxpool.Pool, modelID string) ([]workflowLock, error) {
	var appID string
	if err := pool.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID); err != nil {
		return nil, err
	}
	// awaiting_approval: an approval step of the instance is open, read
	// from the definition the instance started with (steps_snapshot, else
	// the definition's steps). A step's type is spelled as the workflow
	// store reads it (workflowStepTypeFromJSON): "approval",
	// "STEP_TYPE_APPROVAL", or the enum's number, 2.
	rows, err := pool.Query(ctx, `
		SELECT wi.context, wd.context_schema, wi.status::text, ws.decision,
		       EXISTS (
		           SELECT 1 FROM workflow.workflow_step s,
		                jsonb_array_elements(CASE WHEN jsonb_typeof(COALESCE(wi.steps_snapshot, wd.steps)) = 'array'
		                                          THEN COALESCE(wi.steps_snapshot, wd.steps) ELSE '[]'::jsonb END) d
		           WHERE s.instance_id = wi.id AND s.status = 'in_progress'
		             AND d->>'id' = s.step_def_id AND lower(d->>'type') IN ('approval', 'step_type_approval', '2')
		       ) AS awaiting_approval
		FROM workflow.workflow_instance wi
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		-- The decision of the last APPROVAL step decided. A notification,
		-- task or condition step after it records a decision of its own
		-- (sent, done, a branch), and reading that one released the agreed
		-- numbers as soon as "approve, then tell the requester" sent its
		-- mail (decided 2026-10-06: an approved instance stays locked
		-- whatever follows). A step its definition no longer names (edited
		-- since, with no snapshot) counts when it recorded an approval's
		-- decision.
		LEFT JOIN LATERAL (
			SELECT s.decision FROM workflow.workflow_step s
			LEFT JOIN LATERAL (
				SELECT lower(d->>'type') AS type
				FROM jsonb_array_elements(CASE WHEN jsonb_typeof(COALESCE(wi.steps_snapshot, wd.steps)) = 'array'
				                               THEN COALESCE(wi.steps_snapshot, wd.steps) ELSE '[]'::jsonb END) d
				WHERE d->>'id' = s.step_def_id LIMIT 1
			) sd ON true
			WHERE s.instance_id = wi.id AND s.decision IS NOT NULL AND s.completed_at IS NOT NULL
			  AND (sd.type IN ('approval', 'step_type_approval', '2')
			       OR (sd.type IS NULL AND s.decision IN ('approve', 'approved', 'reject', 'rejected')))
			ORDER BY s.completed_at DESC LIMIT 1
		) ws ON true
		WHERE wd.application_id = $1::uuid
		ORDER BY wi.started_at DESC
	`, appID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var locks []workflowLock
	// Metric context values may be ids — collected for one batched id→name
	// resolution below.
	metricValueIsRaw := map[string]bool{}
	for rows.Next() {
		var ctxJSON, schemaJSON []byte
		var status string
		var decision *string
		var awaitingApproval bool
		if err := rows.Scan(&ctxJSON, &schemaJSON, &status, &decision, &awaitingApproval); err != nil {
			return nil, fmt.Errorf("scan workflow instance: %w", err)
		}
		var values map[string]string
		_ = json.Unmarshal(ctxJSON, &values)
		var vars []contextVarDef
		_ = json.Unmarshal(schemaJSON, &vars)
		l := workflowLock{metricNames: map[string]bool{}}
		for _, v := range vars {
			if v.DataType == "Dimension member" && v.DimensionID != "" && values[v.Key] != "" {
				l.bindings = append(l.bindings, lockBinding{dimID: v.DimensionID, code: values[v.Key]})
			}
			if v.DataType == "Metric" {
				if raw := strings.TrimSpace(values[v.Key]); raw != "" {
					l.metricNames[strings.ToLower(raw)] = true
					metricValueIsRaw[raw] = true
				}
			}
		}
		// A running instance locks its scope only while one of its approval
		// steps is open: the approver decides on numbers that cannot move
		// under them, while its tasks — a correction round's "enter
		// regional corrections" — can change the data they are about. Until
		// 2026-10-05 a running instance locked for its whole run, so a round
		// scoped to a region refused every correction its own tasks asked
		// for. A terminal instance is locked only if its last decision was
		// approve AND it wasn't later cancelled/failed: an admin cancelling
		// an already-approved instance left the approve decision on the
		// step, and the lock persisted forever (found live).
		terminalApproved := status != "running" && status != "cancelled" && status != "failed" && decision != nil && *decision == "approve"
		l.locked = (status == "running" && awaitingApproval) || terminalApproved
		locks = append(locks, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Id-shaped metric context values resolve to names, so every scope is
	// name-keyed whatever the start path stored.
	if len(metricValueIsRaw) > 0 {
		rawVals := make([]string, 0, len(metricValueIsRaw))
		for v := range metricValueIsRaw {
			rawVals = append(rawVals, v)
		}
		idName, err := pool.Query(ctx, `SELECT id::text, lower(name) FROM model.metric_def WHERE id::text = ANY($1)`, rawVals)
		if err == nil {
			resolved := map[string]string{}
			for idName.Next() {
				var id, name string
				if idName.Scan(&id, &name) == nil {
					resolved[strings.ToLower(id)] = name
				}
			}
			idName.Close()
			for i := range locks {
				for key := range locks[i].metricNames {
					if name, ok := resolved[key]; ok {
						delete(locks[i].metricNames, key)
						locks[i].metricNames[name] = true
					}
				}
			}
		}
	}
	return locks, nil
}

// lockReason says what l locks, by the members' labels: "Europe · Budget
// is locked: …", and the metric when l is scoped to metrics.
func lockReason(ctx context.Context, pool *pgxpool.Pool, l workflowLock, metric string) string {
	parts := make([]string, 0, len(l.bindings)+1)
	for _, b := range l.bindings {
		label := b.code
		var stored string
		if pool.QueryRow(ctx, `SELECT label FROM model.dimension_member WHERE dimension_id = $1::uuid AND code = $2`, b.dimID, b.code).Scan(&stored) == nil && stored != "" {
			label = stored
		}
		parts = append(parts, label)
	}
	if len(l.metricNames) > 0 && metric != "" {
		parts = append(parts, metric)
	}
	return strings.Join(parts, " · ") + " is locked: a workflow about it is awaiting approval or was approved"
}

// LockScope is a workflow lock in force over some of a grid's metrics, for
// the grid to draw the cells it covers read-only.
type LockScope struct {
	// Fields are the instance's Dimension member fields: for each, the
	// member codes it covers by dimension ID — the named member and every
	// member under it, in any dimension. A cell is inside when, for every
	// field whose dimensions the cell has, its member on one of them is
	// covered, and at least one field's are in the cell.
	Fields []map[string][]string `json:"fields"`
	// MetricIDs are the grid's metrics it locks.
	MetricIDs []string `json:"metric_ids"`
	Reason    string   `json:"reason"`
}

// LockMetric is a metric a grid shows, for WorkflowLockScopes.
type LockMetric struct{ ID, Name string }

// WorkflowLockScopes is the workflow locks in force over metrics of
// modelID, as the writes of those metrics would meet them
// (WorkflowLockReasonForMetrics): for each metric, the newest instance of
// each member scope that covers it decides, and only locked ones are
// returned.
func WorkflowLockScopes(ctx context.Context, pool *pgxpool.Pool, modelID string, metrics []LockMetric) ([]LockScope, error) {
	locks, err := loadWorkflowLocks(ctx, pool, modelID)
	if err != nil || len(locks) == 0 {
		return nil, err
	}
	lockedMetrics := map[int][]string{}
	for _, m := range metrics {
		name := strings.ToLower(m.Name)
		seen := map[string]bool{}
		for i, l := range locks {
			if len(l.bindings) == 0 || !l.coversMetric(name) {
				continue
			}
			key := l.scopeKey()
			if seen[key] {
				continue
			}
			seen[key] = true
			if l.locked {
				lockedMetrics[i] = append(lockedMetrics[i], m.ID)
			}
		}
	}
	if len(lockedMetrics) == 0 {
		return nil, nil
	}
	order := make([]int, 0, len(lockedMetrics))
	for i := range lockedMetrics {
		order = append(order, i)
	}
	sort.Ints(order)
	out := make([]LockScope, 0, len(order))
	for _, i := range order {
		l := locks[i]
		scope := LockScope{MetricIDs: lockedMetrics[i], Reason: lockReason(ctx, pool, l, "")}
		for _, b := range l.bindings {
			covered, err := memberSubtree(ctx, pool, b)
			if err != nil {
				return nil, err
			}
			scope.Fields = append(scope.Fields, covered)
		}
		out = append(out, scope)
	}
	return out, nil
}

// memberSubtree is b's member and every member under it (parent_member_id,
// across dimensions), as dimension ID → codes.
func memberSubtree(ctx context.Context, pool *pgxpool.Pool, b lockBinding) (map[string][]string, error) {
	rows, err := pool.Query(ctx, `
		WITH RECURSIVE sub AS (
		    SELECT id, dimension_id, code FROM model.dimension_member WHERE dimension_id = $1::uuid AND code = $2
		    UNION
		    SELECT m.id, m.dimension_id, m.code FROM model.dimension_member m JOIN sub ON m.parent_member_id = sub.id
		)
		SELECT dimension_id::text, code FROM sub ORDER BY dimension_id, code`, b.dimID, b.code)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{b.dimID: {}}
	for rows.Next() {
		var dimID, code string
		if err := rows.Scan(&dimID, &code); err != nil {
			return nil, err
		}
		out[dimID] = append(out[dimID], code)
	}
	if len(out[b.dimID]) == 0 {
		out[b.dimID] = []string{b.code} // the member is gone: its code still names the scope
	}
	return out, rows.Err()
}

package writeguard

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// A later approved run of a workflow supersedes the earlier approved runs of
// that workflow about the same scope (migration 201, owner decision
// 2026-11-27): a project's second budget revision is approved over the
// first, never leaving the budget unlocked in between.

// ApprovalScopeKey is what a run of a workflow locks, as text: its
// Dimension member and Metric context fields with their values, in key
// order. Two runs of one workflow with the same key are about the same scope.
func ApprovalScopeKey(values map[string]string, vars []contextVarDef) string {
	var parts []string
	for _, v := range vars {
		if v.DataType == "Dimension member" || v.DataType == "Metric" {
			parts = append(parts, strings.ToLower(v.Key)+"="+strings.TrimSpace(values[v.Key]))
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x1f")
}

// ApprovalScopeKeyJSON is ApprovalScopeKey of a run's context and its
// workflow's context schema as stored.
func ApprovalScopeKeyJSON(ctxJSON, schemaJSON []byte) string {
	var values map[string]string
	_ = json.Unmarshal(ctxJSON, &values)
	var vars []contextVarDef
	_ = json.Unmarshal(schemaJSON, &vars)
	return ApprovalScopeKey(values, vars)
}

// decidingScope is the run ctx decides for (WithDecidingRun): its
// workflow, when it started and its scope.
type decidingScope struct {
	name    string
	started time.Time
	key     string
}

func loadDecidingScope(ctx context.Context, pool ReadDB, instanceID string) (*decidingScope, error) {
	if instanceID == "" {
		return nil, nil
	}
	var d decidingScope
	var ctxJSON, schemaJSON []byte
	err := pool.QueryRow(ctx, `
		SELECT COALESCE(wi.context, '{}'::jsonb), COALESCE(wd.context_schema, '[]'::jsonb), wd.name, wi.started_at
		FROM workflow.workflow_instance wi
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		WHERE wi.id::text = $1`, instanceID).Scan(&ctxJSON, &schemaJSON, &d.name, &d.started)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d.key = ApprovalScopeKeyJSON(ctxJSON, schemaJSON)
	return &d, nil
}

// supersedes reports whether the deciding run will supersede an approved
// run of workflow name, started then, about the scope values and vars name:
// its own work then goes through that run's lock.
func (d *decidingScope) supersedes(name string, started time.Time, values map[string]string, vars []contextVarDef) bool {
	return d != nil && strings.EqualFold(d.name, name) && started.Before(d.started) && ApprovalScopeKey(values, vars) == d.key
}

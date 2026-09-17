// Package writeguard holds the generic, model-agnostic checks that decide
// whether a write to runtime.fact_input is allowed: is the target revision
// system-managed (read-only), does the caller's identity.user_access_rule
// hide or read-restrict the dimension member being written, and is that
// member (or an ancestor of it) currently locked by an in-progress or
// approved workflow instance.
//
// This logic used to live only inline in internal/gateway's HTTP handlers
// (cells() and importUpload()), which meant the gRPC ImportService
// (internal/importpkg) could commit facts that bypassed every one of these
// checks — an actor blocked from writing a cell interactively could still
// write it via the gRPC import path. Both entry points now call the same
// functions here, so there is exactly one implementation of "is this write
// allowed" for the whole engine, not one per transport.
//
// Every function takes a *pgxpool.Pool directly rather than a request/handler
// type, so it has no dependency on gateway (or any other caller), avoiding an
// import cycle with internal/importpkg.
package writeguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// AncestorRef identifies one dimension_member by its ID, owning dimension,
// and code.
type AncestorRef struct {
	ID          string
	DimensionID string
	Code        string
}

// AncestorChain returns memberID's ancestor chain, starting with itself,
// walking parent_member_id (which crosses dimensions in one hop when the
// owning dimension declares parent_dimension_id — e.g. an employee's
// parent_member_id points directly at its cost-center member). A member with
// no further parent (pgx.ErrNoRows on the next hop, or a NULL
// parent_member_id) legitimately ends the chain there; any other error is
// propagated rather than silently truncating the walk.
func AncestorChain(ctx context.Context, pool *pgxpool.Pool, memberID string) ([]AncestorRef, error) {
	chain := make([]AncestorRef, 0, 4)
	cur := memberID
	for i := 0; i < 20; i++ {
		var dimID, code string
		var parentID *string
		err := pool.QueryRow(ctx,
			`SELECT dimension_id::text, code, parent_member_id::text FROM model.dimension_member WHERE id=$1::uuid`,
			cur,
		).Scan(&dimID, &code, &parentID)
		if errors.Is(err, pgx.ErrNoRows) {
			break
		}
		if err != nil {
			return chain, err
		}
		chain = append(chain, AncestorRef{ID: cur, DimensionID: dimID, Code: code})
		if parentID == nil {
			break
		}
		cur = *parentID
	}
	return chain, nil
}

// IsLeafMember reports whether memberID has no children (no dimension_member
// row has parent_member_id = memberID). Leaf-ness is purely structural — no
// "is_leaf" flag exists on the table — so a hierarchy's root/parent members
// (e.g. a year in a months dimension) never pass, with no dimension-specific
// configuration required.
func IsLeafMember(ctx context.Context, pool *pgxpool.Pool, memberID string) (bool, error) {
	var hasChildren bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM model.dimension_member WHERE parent_member_id=$1::uuid)`,
		memberID,
	).Scan(&hasChildren)
	return !hasChildren, err
}

// accessRule returns the identity.user_access_rule entry for
// (userID, ruleType, refID) — "hidden", "read", or "" (unrestricted /
// write). Only a genuine absence of any matching rule (pgx.ErrNoRows) means
// unrestricted; any other query error is propagated so callers fail closed
// instead of silently treating a DB failure as "nothing to restrict."
func accessRule(ctx context.Context, pool *pgxpool.Pool, userID, ruleType, refID string) (string, error) {
	var access string
	err := pool.QueryRow(ctx,
		`SELECT access FROM identity.user_access_rule WHERE user_id=$1::uuid AND rule_type=$2 AND ref_id=$3`,
		userID, ruleType, refID,
	).Scan(&access)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return access, nil
}

// HiddenAccess returns the access.rule_type='dimension_member' entry for
// (userID, memberID) — "hidden", "read", or "" (unrestricted / write).
func HiddenAccess(ctx context.Context, pool *pgxpool.Pool, userID, memberID string) (string, error) {
	return accessRule(ctx, pool, userID, "dimension_member", memberID)
}

// MetricAccess is HiddenAccess for rule_type='metric' — used by cell
// writeback to check whether a user is hidden/read-restricted from a
// specific metric, with the same fail-closed-on-real-error semantics.
func MetricAccess(ctx context.Context, pool *pgxpool.Pool, userID, metricID string) (string, error) {
	return accessRule(ctx, pool, userID, "metric", metricID)
}

// SystemManaged reports whether revisionID is marked system_managed (only
// ever populated by an approval-triggered copy — see model.revision — never
// directly writable through normal cell writeback or import). A caller with
// no revision scoping at all (revisionID == "") has nothing to reject.
func SystemManaged(ctx context.Context, pool *pgxpool.Pool, revisionID string) (bool, error) {
	if revisionID == "" {
		return false, nil
	}
	var systemManaged bool
	err := pool.QueryRow(ctx,
		`SELECT system_managed FROM model.revision WHERE id=$1::uuid`, revisionID,
	).Scan(&systemManaged)
	return systemManaged, err
}

type contextVarDef struct {
	Key         string `json:"key"`
	DataType    string `json:"data_type"`
	DimensionID string `json:"dimension_id"`
}

// WorkflowLockReason reports whether any of memberIDs (or one of their
// ancestors) is the declared scope of a workflow instance on modelID's
// application that's either still running or was approved — and if so, a
// human-readable reason. Entirely generic: driven by "Dimension member" +
// dimension_id context_schema declarations, not any specific dimension or
// workflow by name, so it's a no-op for any model/workflow that doesn't use
// that pattern.
//
// A terminal instance's lock state is derived from its last decided step's
// decision, not just the instance status, because the workflow engine always
// leaves a completed instance at status "completed" regardless of decision:
// running = locked (awaiting decision), a terminal instance whose last
// decision was "approve" = locked (permanently, until a new instance
// starts), anything else (rejected, cancelled, no decision) = unlocked.
//
// This wrapper checks with UNKNOWN written metrics — deliberately
// conservative: a metric-scoped instance (one with "Metric" context vars)
// still matches, so no caller can bypass a narrower lock by not saying
// which metric it writes. Callers that know their metrics should use
// WorkflowLockReasonForMetrics for crossing precision.
func WorkflowLockReason(ctx context.Context, pool *pgxpool.Pool, modelID string, memberIDs []string) (locked bool, reason string, err error) {
	return WorkflowLockReasonForMetrics(ctx, pool, modelID, memberIDs, nil)
}

// WorkflowLockReasonForMetrics is WorkflowLockReason with the written
// metrics known: an instance whose context declares "Metric" vars locks
// only the CROSSING of its member scope and those metrics — approving
// "revenue for Canada" freezes exactly revenue×Canada, leaving cost×Canada
// editable. An instance with no Metric vars keeps locking every metric at
// its member scope (the original behavior). Metric context values may be a
// metric_def id or a metric name; matching is by NAME (case-insensitive)
// because names are the identity that survives revision copies re-minting
// ids. metricIDs nil/empty = metrics unknown = conservative (see wrapper).
func WorkflowLockReasonForMetrics(ctx context.Context, pool *pgxpool.Pool, modelID string, memberIDs []string, metricIDs []string) (locked bool, reason string, err error) {
	var appID string
	if err := pool.QueryRow(ctx, `SELECT application_id::text FROM core.model WHERE id=$1::uuid`, modelID).Scan(&appID); err != nil {
		return false, "", err
	}

	rows, err := pool.Query(ctx, `
		SELECT wi.context, wd.context_schema, wi.status::text, ws.decision
		FROM workflow.workflow_instance wi
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		LEFT JOIN LATERAL (
			SELECT decision FROM workflow.workflow_step
			WHERE instance_id = wi.id AND decision IS NOT NULL AND completed_at IS NOT NULL
			ORDER BY completed_at DESC LIMIT 1
		) ws ON true
		WHERE wd.application_id = $1::uuid
		ORDER BY wi.started_at DESC
	`, appID)
	if err != nil {
		return false, "", err
	}
	defer rows.Close()

	type scopedInstance struct {
		context     map[string]string
		dimBindings map[string]string // context key -> dimension_id, for "Dimension member" vars
		metricNames map[string]bool   // lowercased metric names from "Metric" vars; empty = all metrics
		locked      bool
	}
	var instances []scopedInstance
	// Metric context values may be ids — collect them for one batched
	// id→name resolution below.
	metricValueIsRaw := map[string]bool{}
	for rows.Next() {
		var ctxJSON, schemaJSON []byte
		var status string
		var decision *string
		if scanErr := rows.Scan(&ctxJSON, &schemaJSON, &status, &decision); scanErr != nil {
			return false, "", fmt.Errorf("scan workflow instance: %w", scanErr)
		}
		var inst scopedInstance
		_ = json.Unmarshal(ctxJSON, &inst.context)
		var vars []contextVarDef
		_ = json.Unmarshal(schemaJSON, &vars)
		inst.dimBindings = map[string]string{}
		inst.metricNames = map[string]bool{}
		for _, v := range vars {
			if v.DataType == "Dimension member" && v.DimensionID != "" {
				inst.dimBindings[v.Key] = v.DimensionID
			}
			if v.DataType == "Metric" {
				if raw := strings.TrimSpace(inst.context[v.Key]); raw != "" {
					inst.metricNames[strings.ToLower(raw)] = true
					metricValueIsRaw[raw] = true
				}
			}
		}
		// running = locked (awaiting decision); a terminal instance is locked
		// only if its last decision was approve AND it wasn't later
		// cancelled/failed. The status guard is what the doc comment above
		// always promised ("cancelled = unlocked") but the code omitted: an
		// admin cancelling an already-approved instance left the approve
		// decision on the step, so the lock persisted forever (found live —
		// a cancelled approval kept blocking writes to its scope).
		terminalApproved := status != "cancelled" && status != "failed" && decision != nil && *decision == "approve"
		inst.locked = status == "running" || terminalApproved
		instances = append(instances, inst)
	}
	if err := rows.Err(); err != nil {
		return false, "", err
	}
	if len(instances) == 0 {
		return false, "", nil
	}

	// Resolve any id-shaped metric context values to names, so instance
	// scopes are uniformly name-keyed no matter what the start path stored.
	if len(metricValueIsRaw) > 0 {
		rawVals := make([]string, 0, len(metricValueIsRaw))
		for v := range metricValueIsRaw {
			rawVals = append(rawVals, v)
		}
		idName, rErr := pool.Query(ctx,
			`SELECT id::text, lower(name) FROM model.metric_def WHERE id::text = ANY($1)`, rawVals)
		if rErr == nil {
			resolved := map[string]string{}
			for idName.Next() {
				var id, name string
				if idName.Scan(&id, &name) == nil {
					resolved[strings.ToLower(id)] = name
				}
			}
			idName.Close()
			if len(resolved) > 0 {
				for i := range instances {
					for key := range instances[i].metricNames {
						if name, ok := resolved[key]; ok {
							delete(instances[i].metricNames, key)
							instances[i].metricNames[name] = true
						}
					}
				}
			}
		}
	}

	// The metrics being written, as lowercased names. nil written list =
	// metrics unknown: represented as a single wildcard so metric-scoped
	// instances still match (conservative — never a bypass).
	type writtenMetric struct{ name, display string }
	var written []writtenMetric
	if len(metricIDs) > 0 {
		mRows, mErr := pool.Query(ctx,
			`SELECT DISTINCT lower(name), name FROM model.metric_def WHERE id::text = ANY($1)`, metricIDs)
		if mErr != nil {
			return false, "", fmt.Errorf("resolve written metrics: %w", mErr)
		}
		for mRows.Next() {
			var lower, display string
			if mRows.Scan(&lower, &display) == nil {
				written = append(written, writtenMetric{name: lower, display: display})
			}
		}
		mRows.Close()
	}
	if len(written) == 0 {
		written = []writtenMetric{{name: "", display: ""}} // wildcard
	}

	for _, memberID := range memberIDs {
		chain, chainErr := AncestorChain(ctx, pool, memberID)
		if chainErr != nil {
			return false, "", fmt.Errorf("check ancestor chain: %w", chainErr)
		}
		for _, anc := range chain {
			// Per (member, metric) crossing: instances are ordered
			// most-recent-first, and the first instance whose member scope
			// AND metric scope both cover this crossing is authoritative —
			// older instances for the same crossing are superseded, while a
			// revenue-only instance never supersedes (nor unlocks) a cost
			// crossing it doesn't cover.
			for _, wm := range written {
				for _, inst := range instances {
					matched := false
					for key, dimID := range inst.dimBindings {
						if dimID == anc.DimensionID && inst.context[key] == anc.Code {
							matched = true
							break
						}
					}
					if !matched {
						continue
					}
					if len(inst.metricNames) > 0 && wm.name != "" && !inst.metricNames[wm.name] {
						continue // instance is metric-scoped and doesn't cover this metric
					}
					if inst.locked {
						if len(inst.metricNames) > 0 && wm.display != "" {
							return true, fmt.Sprintf("%s · %s is locked by an in-progress or approved workflow", anc.Code, wm.display), nil
						}
						return true, fmt.Sprintf("%s is locked by an in-progress or approved workflow", anc.Code), nil
					}
					break
				}
			}
		}
	}
	return false, "", nil
}

// CheckWrite runs the full generic write-guard for a batch of dimension
// members being written into revisionID: system_managed rejection, then
// per-member hidden/read-only access, then workflow-lock. Returns a single
// human-readable reason on the first violation found, or ("", nil) if the
// write is allowed. This is the one function both the interactive cell
// writeback path and every import path (HTTP and gRPC) should call — see
// package doc.
func CheckWrite(ctx context.Context, pool *pgxpool.Pool, modelID, revisionID, userID string, memberIDs []string) (reason string, err error) {
	return CheckWriteMetrics(ctx, pool, modelID, revisionID, userID, memberIDs, nil)
}

// CheckWriteMetrics is CheckWrite with the written metrics known, so
// metric-scoped workflow locks apply to exactly the member×metric crossing
// being written (see WorkflowLockReasonForMetrics). metricIDs nil = metrics
// unknown = conservative: metric-scoped locks still block.
func CheckWriteMetrics(ctx context.Context, pool *pgxpool.Pool, modelID, revisionID, userID string, memberIDs []string, metricIDs []string) (reason string, err error) {
	systemManaged, err := SystemManaged(ctx, pool, revisionID)
	if err != nil {
		return "", fmt.Errorf("check system_managed: %w", err)
	}
	if systemManaged {
		return "this revision is system-managed and read-only", nil
	}

	seen := make(map[string]bool, len(memberIDs))
	var uniqueMembers []string
	for _, id := range memberIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		uniqueMembers = append(uniqueMembers, id)
	}

	for _, memberID := range uniqueMembers {
		access, err := HiddenAccess(ctx, pool, userID, memberID)
		if err != nil {
			return "", fmt.Errorf("check access rule: %w", err)
		}
		if access == "hidden" || access == "read" {
			return "access denied: the write references a dimension member outside your access scope", nil
		}
		// A member with no rule of its own still isn't writable if an
		// ancestor (structural or cross-dimension, walking parent_member_id
		// exactly like WorkflowLockReason below) is hidden — e.g. an
		// employee under a cost center hidden from this user, even though
		// only the cost center itself carries an explicit rule. Mirrors
		// ExpandHidden's read-side cascade; "read" on an ancestor does not
		// cascade (only a direct rule on the member itself blocks a
		// read-only write), matching ExpandHidden's scope.
		chain, chainErr := AncestorChain(ctx, pool, memberID)
		if chainErr != nil {
			return "", fmt.Errorf("check ancestor chain: %w", chainErr)
		}
		for _, anc := range chain {
			if anc.ID == memberID {
				continue // already checked above
			}
			ancAccess, err := HiddenAccess(ctx, pool, userID, anc.ID)
			if err != nil {
				return "", fmt.Errorf("check access rule: %w", err)
			}
			if ancAccess == "hidden" {
				return "access denied: the write references a dimension member outside your access scope", nil
			}
		}
	}

	if len(uniqueMembers) > 0 {
		locked, lockReason, err := WorkflowLockReasonForMetrics(ctx, pool, modelID, uniqueMembers, metricIDs)
		if err != nil {
			return "", fmt.Errorf("check workflow lock: %w", err)
		}
		if locked {
			return lockReason, nil
		}
	}

	return "", nil
}

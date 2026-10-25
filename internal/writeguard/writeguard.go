// Package writeguard holds the generic, model-agnostic checks that decide
// whether a write to runtime.fact_input is allowed: is the target revision
// system-managed (read-only), does the caller's identity.user_access_rule
// hide or read-restrict the dimension member being written, and is that
// member (or an ancestor of it) currently locked by an in-progress or
// approved workflow instance.
//
// Every write entry point (interactive cells, imports, form postings) calls
// the functions here, so there is exactly one implementation of "is this
// write allowed" for the whole engine.
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
	memo := memoOf(ctx)
	if memo != nil {
		if chain, ok := memo.chain(memberID); ok {
			return chain, nil
		}
	}
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
	if memo != nil {
		memo.setChain(memberID, chain)
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
//
// refID is matched by LINEAGE, not only by id: a rule whose ref_lineage_id
// is the lineage of refID's row (migration 099) applies, because a rule's
// stored ref_id lives in whichever revision was active when it was last
// written or remapped — or has since been deleted — while refID may belong
// to any revision being read or written, under any name. A rule stored
// without a lineage takes the lineage of the row its ref_id points at, as
// in RulesForRevision. When several rules match, the stricter access wins.
func accessRule(ctx context.Context, pool *pgxpool.Pool, userID, ruleType, refID string) (string, error) {
	memo, key := memoOf(ctx), [3]string{userID, ruleType, refID}
	if memo != nil {
		if access, ok := memo.rule(key); ok {
			return access, nil
		}
	}
	var access string
	var err error
	switch {
	case (ruleType == "dimension_member" || ruleType == "metric") && uuidRE.MatchString(refID):
		err = pool.QueryRow(ctx, `
			SELECT r.access
			FROM identity.user_access_rule r
			WHERE r.user_id = $1::uuid AND r.rule_type = $2::text
			  AND (r.ref_id = $3::text
			       OR COALESCE(r.ref_lineage_id, CASE $2::text
			            WHEN 'dimension_member' THEN (SELECT lineage_id FROM model.dimension_member
			                 WHERE id = CASE WHEN r.ref_id ~* `+uuidPattern+` THEN r.ref_id::uuid END)
			            WHEN 'metric' THEN (SELECT lineage_id FROM model.metric_def
			                 WHERE id = CASE WHEN r.ref_id ~* `+uuidPattern+` THEN r.ref_id::uuid END)
			          END) = CASE $2::text
			            WHEN 'dimension_member' THEN (SELECT lineage_id FROM model.dimension_member WHERE id = $4::uuid)
			            WHEN 'metric' THEN (SELECT lineage_id FROM model.metric_def WHERE id = $4::uuid)
			          END)
			ORDER BY CASE r.access WHEN 'hidden' THEN 0 WHEN 'read' THEN 1 ELSE 2 END
			LIMIT 1`,
			userID, ruleType, refID, refID,
		).Scan(&access)
	default:
		err = pool.QueryRow(ctx,
			`SELECT access FROM identity.user_access_rule WHERE user_id=$1::uuid AND rule_type=$2 AND ref_id=$3`,
			userID, ruleType, refID,
		).Scan(&access)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		access, err = "", nil
	}
	if err != nil {
		return "", err
	}
	if memo != nil {
		memo.setRule(key, access)
	}
	return access, nil
}

// HiddenAccess returns the access.rule_type='dimension_member' entry for
// (userID, memberID) — "hidden", "read", or "" (unrestricted / write).
func HiddenAccess(ctx context.Context, pool *pgxpool.Pool, userID, memberID string) (string, error) {
	return accessRule(ctx, pool, userID, "dimension_member", memberID)
}

// HiddenInChain reports whether memberID is hidden from userID, either by a
// rule of its own or by a "hidden" rule on any ancestor (structural or
// cross-dimension, via AncestorChain) — the single-member form of
// ExpandHidden's read-side cascade. Use it wherever a read path resolves one
// member at a time, so it hides exactly what the grid hides.
func HiddenInChain(ctx context.Context, pool *pgxpool.Pool, userID, memberID string) (bool, error) {
	access, err := HiddenAccess(ctx, pool, userID, memberID)
	if err != nil {
		return false, err
	}
	if access == "hidden" {
		return true, nil
	}
	chain, err := AncestorChain(ctx, pool, memberID)
	if err != nil {
		return false, err
	}
	for _, anc := range chain {
		if anc.ID == memberID {
			continue
		}
		ancAccess, err := HiddenAccess(ctx, pool, userID, anc.ID)
		if err != nil {
			return false, err
		}
		if ancAccess == "hidden" {
			return true, nil
		}
	}
	return false, nil
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
	memo := memoOf(ctx)
	if memo != nil {
		if v, ok := memo.systemManaged(revisionID); ok {
			return v, nil
		}
	}
	var systemManaged bool
	err := pool.QueryRow(ctx,
		`SELECT system_managed FROM model.revision WHERE id=$1::uuid`, revisionID,
	).Scan(&systemManaged)
	if err == nil && memo != nil {
		memo.setSystemManaged(revisionID, systemManaged)
	}
	return systemManaged, err
}

type contextVarDef struct {
	Key         string `json:"key"`
	DataType    string `json:"data_type"`
	DimensionID string `json:"dimension_id"`
}

// WorkflowLockReason reports whether any of memberIDs (or one of their
// ancestors) is the declared scope of a workflow instance on modelID's
// application that's either awaiting an approval or was approved — and if so, a
// human-readable reason. Entirely generic: driven by "Dimension member" +
// dimension_id context_schema declarations, not any specific dimension or
// workflow by name, so it's a no-op for any model/workflow that doesn't use
// that pattern.
//
// A terminal instance's lock state is derived from its last decided step's
// decision, not just the instance status, because the workflow engine always
// leaves a completed instance at status "completed" regardless of decision:
// a running instance with an approval step open = locked (awaiting the
// decision), a running instance at a task, condition or notification =
// unlocked (its tasks may change the data), a terminal instance whose last
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
// metrics known. An instance's member scope is the crossing of its
// "Dimension member" fields (workflowlocks.go): Region = Europe and Version
// = Budget locks Europe's budget, not Europe's actuals nor every budget. An
// instance whose context declares "Metric" vars locks
// only the CROSSING of its member scope and those metrics — approving
// "revenue for Canada" freezes exactly revenue×Canada, leaving cost×Canada
// editable. An instance with no Metric vars keeps locking every metric at
// its member scope (the original behavior). Metric context values may be a
// metric_def id or a metric name; matching is by NAME (case-insensitive)
// because names are the identity that survives revision copies re-minting
// ids. metricIDs nil/empty = metrics unknown = conservative (see wrapper).
func WorkflowLockReasonForMetrics(ctx context.Context, pool *pgxpool.Pool, modelID string, memberIDs []string, metricIDs []string) (locked bool, reason string, err error) {
	locks, err := loadWorkflowLocks(ctx, pool, modelID)
	if err != nil || len(locks) == 0 {
		return false, "", err
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

	// The write's members and their ancestors, by dimension: a write on
	// France is inside Europe.
	covered := map[string]map[string]bool{}
	for _, memberID := range memberIDs {
		chain, chainErr := AncestorChain(ctx, pool, memberID)
		if chainErr != nil {
			return false, "", fmt.Errorf("check ancestor chain: %w", chainErr)
		}
		for _, anc := range chain {
			if covered[anc.DimensionID] == nil {
				covered[anc.DimensionID] = map[string]bool{}
			}
			covered[anc.DimensionID][anc.Code] = true
		}
	}

	// Per written metric, instances newest first: the first instance of
	// each member scope that covers the write and the metric decides —
	// older instances of that scope are superseded, while a revenue-only
	// instance never supersedes (nor unlocks) a cost write it doesn't cover.
	for _, wm := range written {
		seen := map[string]bool{}
		for _, l := range locks {
			if !l.covers(covered) || !l.coversMetric(wm.name) {
				continue
			}
			key := l.scopeKey()
			if seen[key] {
				continue
			}
			seen[key] = true
			if l.locked {
				return true, lockReason(ctx, pool, l, wm.display), nil
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
// writeback path and every import path should call — see
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

	// A calculated member takes no input: its value is computed from the
	// other members of its dimension (dimension_member.formula).
	if len(uniqueMembers) > 0 {
		var calcCode string
		if err := pool.QueryRow(ctx, `SELECT code FROM model.dimension_member WHERE id = ANY($1::uuid[]) AND NULLIF(btrim(formula),'') IS NOT NULL LIMIT 1`,
			uniqueMembers).Scan(&calcCode); err == nil {
			return fmt.Sprintf("%s is a calculated member: its values are computed from the other members of its dimension, so it takes no input", calcCode), nil
		}
	}

	for _, memberID := range uniqueMembers {
		access, err := HiddenAccess(ctx, pool, userID, memberID)
		if err != nil {
			return "", fmt.Errorf("check access rule: %w", err)
		}
		if access == "hidden" || access == "read" {
			return memberRefusal(ctx, pool, memberID, access), nil
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
				return memberRefusal(ctx, pool, memberID, "under hidden"), nil
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
	if locked, lockReason, err := RevisionLockReason(ctx, pool, modelID, revisionID, metricIDs); err != nil {
		return "", fmt.Errorf("check workflow lock: %w", err)
	} else if locked {
		return lockReason, nil
	}

	return "", nil
}

// memberRefusal says which member of a write an access rule refuses, and
// how. A read-only member, which the writer sees, is named by its dimension
// and code; a hidden one — by a rule of its own or under a hidden ancestor —
// only by its dimension, confirming nothing about it. A refusal used to
// name neither (2026-10-20): "the write references a dimension member
// outside your access scope".
func memberRefusal(ctx context.Context, pool *pgxpool.Pool, memberID, access string) string {
	var dim, code string
	if err := pool.QueryRow(ctx, `
		SELECT COALESCE(NULLIF(d.name, ''), 'a dimension'), m.code
		FROM model.dimension_member m JOIN model.dimension_def d ON d.id = m.dimension_id
		WHERE m.id = $1::uuid`, memberID).Scan(&dim, &code); err != nil {
		return "access denied: the write references a dimension member outside your access scope"
	}
	if access == "read" {
		return fmt.Sprintf("access denied: %s %s is read-only for you (an access rule), so its cells take no input from you", dim, code)
	}
	return fmt.Sprintf("access denied: the member of %s this write names is hidden from you (an access rule)", dim)
}

// RevisionLockReason reports whether revisionID is locked by a company-wide
// round: a running workflow instance whose context names no dimension
// members (no "Dimension member" vars) and no record, about this revision
// (its context's revision_id, else the revision its workflow belongs to),
// with an approval step open. While that approval waits for a decision
// nobody changes the revision's values — every scenario — so the approver
// decides on numbers that cannot move under them; approve or reject opens
// it again. A member-scoped round locks only its members
// (WorkflowLockReasonForMetrics) and keeps them locked once approved; a
// company-wide round does not stay locked (user's decision, 2026-10-16:
// a finished round must not freeze the whole model). One that declares
// "Metric" vars locks only those metrics (unknown written metrics = locked,
// as elsewhere). Until 2026-10-16 such a round locked nothing: a business
// user changed a Base assumption during the HR model's management review.
func RevisionLockReason(ctx context.Context, pool *pgxpool.Pool, modelID, revisionID string, metricIDs []string) (locked bool, reason string, err error) {
	if revisionID == "" {
		return false, "", nil
	}
	rows, err := pool.Query(ctx, `
		SELECT wd.name, COALESCE(d->>'name', s.step_def_id), wd.context_schema, wi.context
		FROM workflow.workflow_instance wi
		JOIN workflow.workflow_def wd ON wd.id = wi.workflow_def_id
		JOIN workflow.workflow_step s ON s.instance_id = wi.id AND s.status = 'in_progress'
		CROSS JOIN LATERAL jsonb_array_elements(CASE WHEN jsonb_typeof(COALESCE(wi.steps_snapshot, wd.steps)) = 'array'
		                                            THEN COALESCE(wi.steps_snapshot, wd.steps) ELSE '[]'::jsonb END) d
		WHERE wd.application_id = (SELECT application_id FROM core.model WHERE id = $1::uuid)
		  AND wi.status = 'running'
		  AND d->>'id' = s.step_def_id AND lower(d->>'type') IN ('approval', 'step_type_approval', '2')
		  AND COALESCE(NULLIF(wi.context->>'revision_id', ''), wd.revision_id::text) = $2
		  AND NOT (COALESCE(wi.context, '{}'::jsonb) ? 'record_id')
		  AND COALESCE(wd.subject_type, '') = ''
		ORDER BY wi.started_at DESC`, modelID, revisionID)
	if err != nil {
		return false, "", err
	}
	type round struct {
		workflow, step string
		metrics        []string
	}
	var rounds []round
	for rows.Next() {
		var wf, step string
		var schemaJSON, ctxJSON []byte
		if err := rows.Scan(&wf, &step, &schemaJSON, &ctxJSON); err != nil {
			rows.Close()
			return false, "", err
		}
		var vars []contextVarDef
		_ = json.Unmarshal(schemaJSON, &vars)
		var instCtx map[string]any
		_ = json.Unmarshal(ctxJSON, &instCtx)
		scoped := false
		var metrics []string
		for _, v := range vars {
			switch v.DataType {
			case "Dimension member":
				if v.DimensionID != "" {
					scoped = true
				}
			case "Metric":
				if raw, ok := instCtx[v.Key].(string); ok && strings.TrimSpace(raw) != "" {
					metrics = append(metrics, strings.TrimSpace(raw))
				}
			}
		}
		if !scoped {
			rounds = append(rounds, round{workflow: wf, step: step, metrics: metrics})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, "", err
	}
	for _, r := range rounds {
		if len(r.metrics) > 0 && len(metricIDs) > 0 {
			var crosses bool
			if err := pool.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM model.metric_def w, model.metric_def l
				               WHERE w.id::text = ANY($1) AND (l.id::text = ANY($2) OR lower(l.name) = ANY(SELECT lower(x) FROM unnest($2::text[]) x))
				                 AND lower(w.name) = lower(l.name))`, metricIDs, r.metrics).Scan(&crosses); err != nil {
				return false, "", err
			}
			if !crosses {
				continue
			}
		}
		return true, fmt.Sprintf("%q is waiting for approval (%s): this revision's values are locked until it is approved or rejected", r.workflow, r.step), nil
	}
	return false, "", nil
}

package writeguard

import (
	"context"
	"fmt"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Querier is the one method the rule resolver needs — satisfied by
// *pgxpool.Pool, pgx.Tx and the gateway's tenant-routing handle alike.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// AccessRule is one identity.user_access_rule row as it applies to a
// particular revision: for 'dimension_member' and 'metric' rules RefID is
// the id of the matching row IN THAT REVISION, not the id stored on the
// rule.
type AccessRule struct {
	Type   string
	RefID  string
	Access string
}

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// accessRank orders access levels from most to least restrictive, so two
// rules that land on the same row keep the stricter one.
func accessRank(access string) int {
	switch access {
	case "hidden":
		return 0
	case "read":
		return 1
	default:
		return 2
	}
}

// Stricter reports whether access a restricts more than access b.
func Stricter(a, b string) bool { return accessRank(a) < accessRank(b) }

// uuidPattern is uuidRE for SQL: a ref_id is cast to uuid only after
// matching it, so one malformed rule cannot fail a whole read.
const uuidPattern = `'^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'`

// UUIDPatternSQL is uuidPattern for other packages' rule queries: match a
// ref_id with ~* before casting it to uuid.
const UUIDPatternSQL = uuidPattern

// rulesForRevisionSQL translates user $1's rules into revision $2 by
// LINEAGE (migration 099). A rule stores the id of one member / metric row
// and, in ref_lineage_id, that row's lineage — the identity it shares with
// its copies in every revision of the model, whatever they are named. A
// 'dimension_member' rule applies to the member of that lineage in a
// dimension of the requested revision; a 'metric' rule to the metric of
// that lineage in the requested revision. Dimensions and metrics with a
// NULL revision_id are visible in every revision of their model and count
// as part of it. A rule whose lineage has no row in the requested revision
// yields nothing (nothing there to restrict) — including a member or metric
// deleted and re-added, which is a new lineage. Other rule types ('button')
// are not revision-scoped and pass through unchanged.
//
// A rule with no ref_lineage_id (every writer sets it; this is the
// belt-and-braces case) takes the lineage of the row its ref_id points at.
const rulesForRevisionSQL = `
WITH r AS MATERIALIZED (
	SELECT rule_type, ref_id, access, ref_lineage_id
	FROM identity.user_access_rule
	WHERE user_id = $1::uuid
), lin AS (
	SELECT r.rule_type, r.access, COALESCE(r.ref_lineage_id, om.lineage_id, omd.lineage_id) AS lineage
	FROM r
	LEFT JOIN model.dimension_member om
	       ON r.rule_type = 'dimension_member' AND r.ref_lineage_id IS NULL
	      AND om.id = CASE WHEN r.ref_id ~* ` + uuidPattern + ` THEN r.ref_id::uuid END
	LEFT JOIN model.metric_def omd
	       ON r.rule_type = 'metric' AND r.ref_lineage_id IS NULL
	      AND omd.id = CASE WHEN r.ref_id ~* ` + uuidPattern + ` THEN r.ref_id::uuid END
	WHERE r.rule_type IN ('dimension_member', 'metric')
), rev AS (
	SELECT model_id FROM model.revision WHERE id = $2::uuid
)
SELECT 'dimension_member', tm.id::text, lin.access
FROM lin
JOIN model.dimension_member tm ON tm.lineage_id = lin.lineage
JOIN model.dimension_def td ON td.id = tm.dimension_id
WHERE lin.rule_type = 'dimension_member'
  AND (td.revision_id = $2::uuid OR (td.revision_id IS NULL AND td.model_id IN (SELECT model_id FROM rev)))
UNION ALL
SELECT 'metric', tm.id::text, lin.access
FROM lin
JOIN model.metric_def tm ON tm.lineage_id = lin.lineage
WHERE lin.rule_type = 'metric'
  AND (tm.revision_id = $2::uuid OR (tm.revision_id IS NULL AND tm.model_id IN (SELECT model_id FROM rev)))
UNION ALL
SELECT rule_type, ref_id, access
FROM r
WHERE rule_type NOT IN ('dimension_member', 'metric')
`

// rulesAsStoredSQL is the revision-less form (a model without revisions):
// every rule as stored.
const rulesAsStoredSQL = `
SELECT rule_type, ref_id, access FROM identity.user_access_rule WHERE user_id = $1::uuid
`

// RulesForRevision loads userID's access rules and resolves each one
// against revisionID by lineage (see rulesForRevisionSQL): the one resolver
// every read and write path uses, so a restricted user keeps the same
// members and metrics hidden or read-only in any revision they read, not
// only the active one, and through any rename. When two rules land on the
// same row the stricter access wins. revisionID "" (a model without
// revisions) returns the rules as stored. Any error is returned — callers
// fail closed.
func RulesForRevision(ctx context.Context, q Querier, userID, revisionID string) ([]AccessRule, error) {
	if userID == "" {
		return nil, nil
	}
	var rows pgx.Rows
	var err error
	if revisionID == "" {
		rows, err = q.Query(ctx, rulesAsStoredSQL, userID)
	} else {
		if !uuidRE.MatchString(revisionID) {
			return nil, fmt.Errorf("access rules: malformed revision id %q", revisionID)
		}
		rows, err = q.Query(ctx, rulesForRevisionSQL, userID, revisionID)
	}
	if err != nil {
		return nil, fmt.Errorf("access rules: %w", err)
	}
	defer rows.Close()
	type key struct{ t, id string }
	idx := map[key]int{}
	var out []AccessRule
	for rows.Next() {
		var ar AccessRule
		if err := rows.Scan(&ar.Type, &ar.RefID, &ar.Access); err != nil {
			return nil, fmt.Errorf("access rules: scan: %w", err)
		}
		k := key{ar.Type, ar.RefID}
		if i, ok := idx[k]; ok {
			if Stricter(ar.Access, out[i].Access) {
				out[i].Access = ar.Access
			}
			continue
		}
		idx[k] = len(out)
		out = append(out, ar)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("access rules: %w", err)
	}
	return out, nil
}

// RuleMaps is RulesForRevision split into the two maps most callers want:
// member id -> access and metric id -> access, both in revisionID.
func RuleMaps(ctx context.Context, q Querier, userID, revisionID string) (dimRules, metricRules map[string]string, err error) {
	rules, err := RulesForRevision(ctx, q, userID, revisionID)
	if err != nil {
		return nil, nil, err
	}
	dimRules = map[string]string{}
	metricRules = map[string]string{}
	for _, ar := range rules {
		switch ar.Type {
		case "dimension_member":
			dimRules[ar.RefID] = ar.Access
		case "metric":
			metricRules[ar.RefID] = ar.Access
		}
	}
	return dimRules, metricRules, nil
}

// Execer is the one method ReplaceUserRules needs — satisfied by pgx.Tx.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// RuleInput is one rule as an admin sets it.
type RuleInput struct {
	Type   string
	RefID  string
	Access string
}

// refLineageSQL is the lineage of the row a rule points at: rule type $2,
// ref_id $5 as a uuid (nil when the ref_id is not one — see refUUID). NULL
// for rule types without a lineage ('button') or a row that does not exist.
const refLineageSQL = `CASE $2::text
	WHEN 'dimension_member' THEN (SELECT lineage_id FROM model.dimension_member WHERE id = $5::uuid)
	WHEN 'metric' THEN (SELECT lineage_id FROM model.metric_def WHERE id = $5::uuid)
END`

// refUUID is refID as a query argument for a uuid column: nil unless it is
// a well-formed uuid, so a malformed ref_id never fails a statement.
func refUUID(refID string) any {
	if !uuidRE.MatchString(refID) {
		return nil
	}
	return refID
}

// ReplaceUserRules makes userID's rules exactly rules, inside the caller's
// transaction. Every rule written gets ref_lineage_id — the lineage of the
// row it points at (migration 099), which is what it resolves by in every
// revision. A rule that is kept (same rule_type and ref_id) is updated in
// place rather than deleted and re-inserted, and keeps the lineage it has
// when its row no longer exists: a rule whose member was deleted in the
// active revision still restricts the old revisions' copies of it, and an
// admin re-saving the listed rules must not wipe that.
func ReplaceUserRules(ctx context.Context, tx Execer, userID string, rules []RuleInput) error {
	return replaceRules(ctx, tx, userID, rules, "")
}

// memberInRevisionScopeSQL limits a replace to the member rules that resolve
// in revision $4: a 'dimension_member' rule whose lineage (or, lacking one,
// whose ref_id row) is a member of a dimension of that revision — or of a
// revision-less dimension of the same model.
const memberInRevisionScopeSQL = `
		  AND rule_type = 'dimension_member'
		  AND EXISTS (
		      SELECT 1 FROM model.dimension_member m
		      JOIN model.dimension_def d ON d.id = m.dimension_id
		      WHERE (d.revision_id = $4::uuid
		             OR (d.revision_id IS NULL
		                 AND d.model_id = (SELECT model_id FROM model.revision WHERE id = $4::uuid)))
		        AND (m.lineage_id = identity.user_access_rule.ref_lineage_id
		             OR m.id::text = identity.user_access_rule.ref_id))`

// ReplaceUserMemberRulesInRevision is ReplaceUserRules scoped to what a
// caller that names members of one revision can express — the AI's
// set_user_access_rules, which resolves (dimension, code) in the active
// revision. Only the user's member rules that resolve in revisionID are
// replaced; every other rule is kept untouched: metric and button rules, and
// member rules whose row is gone from that revision (deleted, or deleted and
// re-added under a new lineage) — those still restrict the old revisions'
// copies, and a caller that cannot name them must not wipe them. Rules on
// other models' members are out of scope for the same reason. Every rule in
// rules must be a 'dimension_member' rule.
func ReplaceUserMemberRulesInRevision(ctx context.Context, tx Execer, userID, revisionID string, rules []RuleInput) error {
	for _, r := range rules {
		if r.Type != "dimension_member" {
			return fmt.Errorf("rule %s %s: only dimension_member rules can be replaced per revision", r.Type, r.RefID)
		}
	}
	if !uuidRE.MatchString(revisionID) {
		return fmt.Errorf("revision id %q is not a uuid", revisionID)
	}
	return replaceRules(ctx, tx, userID, rules, memberInRevisionScopeSQL, revisionID)
}

// replaceRules deletes userID's rules that match scopeSQL (all of them when
// it is empty) and are not in rules, then upserts rules. scopeArgs are $4...
func replaceRules(ctx context.Context, tx Execer, userID string, rules []RuleInput, scopeSQL string, scopeArgs ...any) error {
	types := make([]string, len(rules))
	refs := make([]string, len(rules))
	for i, r := range rules {
		types[i], refs[i] = r.Type, r.RefID
	}
	args := append([]any{userID, types, refs}, scopeArgs...)
	if _, err := tx.Exec(ctx, `
		DELETE FROM identity.user_access_rule
		WHERE user_id = $1::uuid
		  AND (rule_type, ref_id) NOT IN (SELECT t, r FROM unnest($2::text[], $3::text[]) AS u(t, r))`+scopeSQL,
		args...); err != nil {
		return fmt.Errorf("clear removed rules: %w", err)
	}
	for _, r := range rules {
		if _, err := tx.Exec(ctx, `
			INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access, ref_lineage_id)
			VALUES ($1::uuid, $2::text, $3::text, $4::text, `+refLineageSQL+`)
			ON CONFLICT (user_id, rule_type, ref_id) DO UPDATE
			SET access = EXCLUDED.access,
			    ref_lineage_id = COALESCE(EXCLUDED.ref_lineage_id, identity.user_access_rule.ref_lineage_id)`,
			userID, r.Type, r.RefID, r.Access, refUUID(r.RefID)); err != nil {
			return fmt.Errorf("rule %s %s: %w", r.Type, r.RefID, err)
		}
	}
	return nil
}

// RemapRulesToRevision re-points identity.user_access_rule rows at the
// newly-activated revision's rows, matched by lineage (migration 099): the
// member or metric of the rule's ref_lineage_id in that revision.
//
// Rules store raw ref_id UUIDs, and every revision copy re-mints those UUIDs
// — so before this, each activation quietly stranded every member- and
// metric-level access rule on the previous revision's rows. Enforcement no
// longer depends on the stored ref_id (every path resolves the rule by its
// lineage in whichever revision is read, writeguard.RulesForRevision); the
// remap keeps ref_id pointing at the live row for the admin's rule listing.
//
// A rule whose lineage has no row in the new revision (its member or metric
// was deleted there) is left untouched: it keeps pointing at the old row
// and keeps its lineage, so the old revisions stay restricted. A row
// re-added under the same code or name is a new lineage and is not
// restricted until an admin sets a rule on it.
//
// Every activation runs it: the gateway's Set active and AI promotion,
// and internal/startersync bringing a guide model up to date.
func RemapRulesToRevision(ctx context.Context, db Execer, modelID, revisionID string) error {
	// Every rule writer sets ref_lineage_id; a rule stored without one (the
	// resolver's belt-and-braces case) takes the lineage of the row its
	// ref_id points at, here as in writeguard.RulesForRevision, so it is
	// moved too.
	if _, err := db.Exec(ctx, `
		UPDATE identity.user_access_rule r
		SET ref_lineage_id = COALESCE(
		        (SELECT m.lineage_id FROM model.dimension_member m
		         WHERE r.rule_type = 'dimension_member' AND m.id = CASE WHEN r.ref_id ~* `+UUIDPatternSQL+` THEN r.ref_id::uuid END),
		        (SELECT md.lineage_id FROM model.metric_def md
		         WHERE r.rule_type = 'metric' AND md.id = CASE WHEN r.ref_id ~* `+UUIDPatternSQL+` THEN r.ref_id::uuid END))
		WHERE r.ref_lineage_id IS NULL AND r.rule_type IN ('dimension_member', 'metric')`); err != nil {
		return fmt.Errorf("rule lineages: %w", err)
	}
	// One rule per (user, lineage) is moved — the strictest — and never
	// onto a row the user already has a rule for: (user_id, rule_type,
	// ref_id) is unique. A rule left behind still applies by lineage, and
	// the stricter access wins wherever two land on the same row.
	for _, q := range []struct{ kind, target string }{
		{"dimension_member", `model.dimension_member nm
				JOIN model.dimension_def nd ON nd.id = nm.dimension_id
				 AND nd.model_id = $1::uuid AND nd.revision_id = $2::uuid`},
		{"metric", `model.metric_def nm`},
	} {
		scope := ""
		if q.kind == "metric" {
			scope = "AND nm.model_id = $1::uuid AND nm.revision_id = $2::uuid"
		}
		if _, err := db.Exec(ctx, `
			UPDATE identity.user_access_rule r
			SET ref_id = c.new_ref
			FROM (
				SELECT DISTINCT ON (o.user_id, o.ref_lineage_id) o.id, nm.id::text AS new_ref
				FROM identity.user_access_rule o
				JOIN `+q.target+` ON nm.lineage_id = o.ref_lineage_id
				WHERE o.rule_type = $3::text `+scope+`
				  AND o.ref_id <> nm.id::text
				  AND NOT EXISTS (
				      SELECT 1 FROM identity.user_access_rule x
				      WHERE x.user_id = o.user_id AND x.rule_type = o.rule_type AND x.ref_id = nm.id::text)
				ORDER BY o.user_id, o.ref_lineage_id,
				         CASE o.access WHEN 'hidden' THEN 0 WHEN 'read' THEN 1 ELSE 2 END, o.id
			) c
			WHERE r.id = c.id
		`, modelID, revisionID, q.kind); err != nil {
			return fmt.Errorf("%s rules: %w", q.kind, err)
		}
	}
	return nil
}

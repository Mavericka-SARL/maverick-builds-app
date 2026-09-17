// Package metricformula is the one implementation of "is this calculated
// metric's formula savable, and what does it depend on".
//
// Before it existed, the gateway's metric-create and metric-update handlers
// and the AI assistant's create_metric/update_metric tools each carried their
// own partial version of the check, and all three shared the same holes:
//
//   - Parse errors were silently swallowed. The reference check ran through
//     extractFormulaRefs, which returns nil on a parse failure, so a formula
//     that doesn't parse produced zero references, sailed through the
//     "do all references exist" loop, and was saved. It then failed at
//     evaluation time, per combo, as a runtime #NAME?/#VALUE! on the grid.
//   - Unknown functions were never checked at all, so =TOTAL(x) saved happily
//     and produced "#NAME?: Unknown function: TOTAL" for every cell.
//   - A metric could reference itself, or form a cycle with another metric.
//     The scheduler detects cycles at calculation time ("cycle at %s"), which
//     is far too late and reports the problem to nobody who can act on it.
//   - References were resolved model-wide (WHERE model_id=$1 AND name=$2)
//     rather than within the metric's own revision, so a formula could name a
//     metric that exists only in some other revision and be accepted.
//
// Validate returns the dependency edges it resolved, so the caller can write
// the metric and its calc_dependency rows in a single transaction rather than
// leaving the graph to a best-effort follow-up that discards its errors.
package metricformula

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// Result carries what a caller needs after a successful validation.
type Result struct {
	// DependsOnMetricIDs are the metric IDs this formula references, ready to
	// be written to model.calc_dependency. References that resolve to a
	// dimension rather than a metric are legal (formulas can test dimension
	// membership) and simply don't produce an edge.
	DependsOnMetricIDs []string
}

// Request describes the metric being saved.
type Request struct {
	ModelID    string
	RevisionID string // "" for a pre-revision (revision-global) metric
	// MetricID is empty on create and set on update — an update must not be
	// allowed to reference itself, directly or through a cycle.
	MetricID string
	Name     string
	Formula  string
}

// ValidationError is a rejection a user can act on: it names what is wrong
// with the formula rather than surfacing a database or parser internal.
type ValidationError struct{ Message string }

func (e *ValidationError) Error() string { return e.Message }

func invalid(format string, args ...any) error {
	return &ValidationError{Message: fmt.Sprintf(format, args...)}
}

// Validate checks a calculated metric's formula and resolves its dependency
// edges. An input metric (no formula) validates trivially with no edges.
func Validate(ctx context.Context, pool *pgxpool.Pool, req Request) (*Result, error) {
	if strings.TrimSpace(req.Formula) == "" {
		return &Result{}, nil
	}

	// 1. It must parse. Everything below depends on a real AST, and this is
	//    the check whose absence let malformed formulas through.
	if _, err := formula.Parse(req.Formula); err != nil {
		return nil, invalid("formula does not parse: %v", err)
	}

	// 2. Every called function must exist.
	calls, err := formula.ExtractCalls(req.Formula)
	if err != nil {
		return nil, invalid("formula does not parse: %v", err)
	}
	for _, call := range calls {
		if !formula.IsBuiltin(call) {
			known := formula.BuiltinNames()
			sort.Strings(known)
			return nil, invalid("formula calls unknown function %q (available: %s)", call, strings.Join(known, ", "))
		}
	}

	refs, err := formula.ExtractIdents(req.Formula)
	if err != nil {
		return nil, invalid("formula does not parse: %v", err)
	}

	// 3. No self-reference. Checked by name, since that's how formulas
	//    address other metrics.
	for _, ref := range refs {
		if strings.EqualFold(ref, req.Name) {
			return nil, invalid("formula references the metric it defines (%q) — a metric cannot depend on itself", req.Name)
		}
	}

	// 4. Every reference must resolve inside THIS metric's revision, as a
	//    metric or a dimension. Metrics additionally become dependency edges.
	edges := make([]string, 0, len(refs))
	for _, ref := range refs {
		var metricID string
		err := pool.QueryRow(ctx, `
			SELECT id::text FROM model.metric_def
			WHERE model_id=$1::uuid AND name=$2
			  AND (revision_id IS NOT DISTINCT FROM NULLIF($3,'')::uuid OR revision_id IS NULL)
			LIMIT 1
		`, req.ModelID, ref, req.RevisionID).Scan(&metricID)
		if err == nil {
			if metricID == req.MetricID {
				return nil, invalid("formula references the metric it defines (%q) — a metric cannot depend on itself", ref)
			}
			edges = append(edges, metricID)
			continue
		}

		var dimExists bool
		if dErr := pool.QueryRow(ctx, `
			SELECT EXISTS (
			    SELECT 1 FROM model.dimension_def
			    WHERE model_id=$1::uuid AND name=$2
			      AND (revision_id IS NOT DISTINCT FROM NULLIF($3,'')::uuid OR revision_id IS NULL)
			)
		`, req.ModelID, ref, req.RevisionID).Scan(&dimExists); dErr != nil {
			return nil, dErr
		}
		if !dimExists {
			return nil, invalid("formula references unknown metric or dimension %q in this revision", ref)
		}
	}

	// 5. No cycles. Walk the existing graph from each new dependency looking
	//    for a path back to this metric — the scheduler would otherwise only
	//    discover it at calculation time, long after the save.
	if req.MetricID != "" {
		for _, dep := range edges {
			cyclic, err := reaches(ctx, pool, dep, req.MetricID)
			if err != nil {
				return nil, err
			}
			if cyclic {
				var depName string
				_ = pool.QueryRow(ctx, `SELECT name FROM model.metric_def WHERE id=$1::uuid`, dep).Scan(&depName)
				return nil, invalid("formula would create a dependency cycle: %q already depends on %q, directly or indirectly", depName, req.Name)
			}
		}
	}

	return &Result{DependsOnMetricIDs: edges}, nil
}

// reaches reports whether targetID is reachable from startID by following
// calc_dependency edges (start depends on … depends on target).
func reaches(ctx context.Context, pool *pgxpool.Pool, startID, targetID string) (bool, error) {
	var found bool
	err := pool.QueryRow(ctx, `
		WITH RECURSIVE deps AS (
			SELECT depends_on_metric_id FROM model.calc_dependency WHERE metric_id = $1::uuid
			UNION
			SELECT cd.depends_on_metric_id
			FROM model.calc_dependency cd
			JOIN deps d ON cd.metric_id = d.depends_on_metric_id
		)
		SELECT EXISTS (SELECT 1 FROM deps WHERE depends_on_metric_id = $2::uuid)
		   OR $1::uuid = $2::uuid
	`, startID, targetID).Scan(&found)
	return found, err
}

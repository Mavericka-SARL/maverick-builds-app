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
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// Result carries what a caller needs after a successful validation.
type Result struct {
	// DependsOnMetricIDs are the metric IDs this formula references, ready to
	// be written to model.calc_dependency. References that resolve to a
	// dimension rather than a metric are legal (formulas can test dimension
	// membership) and simply don't produce an edge.
	DependsOnMetricIDs []string
	// Edges are the same dependencies with the time offsets each is read at
	// (spec §3.4). Write them with WriteDependencies.
	Edges []Edge
	// UsesTimeSeries is true when the formula calls a time function and so
	// needs the metric to be dimensioned by exactly one time dimension.
	UsesTimeSeries bool
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
// Code, when set, is one of the stable identifiers from spec §9.
type ValidationError struct {
	Code    string
	Message string
}

func (e *ValidationError) Error() string {
	if e.Code != "" && !strings.HasPrefix(e.Message, e.Code) {
		return e.Code + ": " + e.Message
	}
	return e.Message
}

func invalid(format string, args ...any) error {
	return &ValidationError{Message: fmt.Sprintf(format, args...)}
}

func invalidCode(code, format string, args ...any) error {
	return &ValidationError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Querier is what Validate needs from the database: a pool or a transaction.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Validate checks a calculated metric's formula and resolves its dependency
// edges. An input metric (no formula) validates trivially with no edges.
func Validate(ctx context.Context, pool Querier, req Request) (*Result, error) {
	if strings.TrimSpace(req.Formula) == "" {
		return &Result{}, nil
	}

	// 1. It must parse and analyze. Everything below depends on a real AST,
	//    and this is the check whose absence let malformed formulas through.
	//    Analysis also validates time-function signatures: literal offsets,
	//    keyword arguments, argument counts.
	an, err := formula.Analyze(req.Formula)
	if err != nil {
		var ae *formula.AnalysisError
		if errors.As(err, &ae) {
			return nil, invalidCode(ae.Code, "%s", ae.Message)
		}
		return nil, invalid("formula does not parse: %v", err)
	}

	// 2. Every called function must exist.
	for _, call := range an.Calls {
		if !formula.IsBuiltin(call) {
			known := formula.BuiltinNames()
			sort.Strings(known)
			return nil, invalid("formula calls unknown function %q (available: %s)", call, strings.Join(known, ", "))
		}
	}

	// 3. Every reference must resolve inside THIS metric's revision, as a
	//    metric or a dimension. Metrics additionally become dependency edges,
	//    each carrying the union of the time offsets it is read at. A
	//    self-reference is a legal edge only when time breaks it (checked in
	//    step 4 with every other cycle).
	var edges []Edge
	var edgeIDs []string
	for _, ref := range an.References {
		var metricID string
		err := pool.QueryRow(ctx, `
			SELECT id::text FROM model.metric_def
			WHERE model_id=$1::uuid AND name=$2
			  AND (revision_id IS NOT DISTINCT FROM NULLIF($3,'')::uuid OR revision_id IS NULL)
			LIMIT 1
		`, req.ModelID, ref.Name, req.RevisionID).Scan(&metricID)
		if err == nil {
			isSelf := metricID == req.MetricID || (req.MetricID == "" && strings.EqualFold(ref.Name, req.Name))
			if isSelf && ref.IsDirect() {
				return nil, invalid("formula references the metric it defines (%q) — a metric can only read its own value at another period (PREVIOUS, LAG, ...)", ref.Name)
			}
			edges = append(edges, Edge{To: metricID, MinTimeOffset: ref.MinTimeOffset, MaxTimeOffset: ref.MaxTimeOffset,
				UnboundedPast: ref.UnboundedPast, UnboundedFuture: ref.UnboundedFuture})
			edgeIDs = append(edgeIDs, metricID)
			continue
		}
		if strings.EqualFold(ref.Name, req.Name) {
			// A brand-new metric naming itself: no row exists yet to resolve to.
			if ref.IsDirect() {
				return nil, invalid("formula references the metric it defines (%q) — a metric can only read its own value at another period (PREVIOUS, LAG, ...)", ref.Name)
			}
			edges = append(edges, Edge{To: selfPlaceholder, MinTimeOffset: ref.MinTimeOffset, MaxTimeOffset: ref.MaxTimeOffset,
				UnboundedPast: ref.UnboundedPast, UnboundedFuture: ref.UnboundedFuture})
			continue
		}

		var dimExists bool
		if dErr := pool.QueryRow(ctx, `
			SELECT EXISTS (
			    SELECT 1 FROM model.dimension_def
			    WHERE model_id=$1::uuid AND name=$2
			      AND (revision_id IS NOT DISTINCT FROM NULLIF($3,'')::uuid OR revision_id IS NULL)
			)
		`, req.ModelID, ref.Name, req.RevisionID).Scan(&dimExists); dErr != nil {
			return nil, dErr
		}
		if !dimExists {
			return nil, invalid("formula references unknown metric or dimension %q in this revision", ref.Name)
		}
	}

	// 4. Cycles must be causal. Load the revision's whole graph, substitute
	//    this metric's new edges, and validate every component it touches:
	//    an opening/closing balance pair is legal, a same-period cycle is
	//    not, and the scheduler must never be the first to find out.
	graph, names, err := LoadGraph(ctx, pool, req.ModelID, req.RevisionID)
	if err != nil {
		return nil, err
	}
	self := req.MetricID
	if self == "" {
		self = selfPlaceholder
	}
	names[self] = req.Name
	own := make([]Edge, 0, len(edges))
	for _, e := range edges {
		if e.To == selfPlaceholder {
			e.To = self
		}
		own = append(own, e)
	}
	graph[self] = own
	if _, err := Plan(graph, []string{self}, names); err != nil {
		var te *TemporalError
		if errors.As(err, &te) {
			return nil, invalidCode(formula.CodeTemporalCycleNotCausal, "%s", te.Detail)
		}
		return nil, err
	}

	// A self-edge on a new metric resolves once the row exists; the caller
	// writes it with the real ID via WriteDependencies.
	for i := range edges {
		if edges[i].To == selfPlaceholder {
			edges[i].To = SelfReference
		}
	}
	return &Result{DependsOnMetricIDs: edgeIDs, Edges: edges, UsesTimeSeries: an.UsesTimeSeries}, nil
}

// SelfReference is the Edge.To value of a time-shifted self-reference on a
// metric that does not exist yet (create). WriteDependencies substitutes the
// metric's own ID.
const SelfReference = "self"

const selfPlaceholder = "\x00self"

// LoadGraph loads the revision's dependency graph with offsets, plus an
// id → name map for messages.
func LoadGraph(ctx context.Context, q Querier, modelID, revisionID string) (Graph, map[string]string, error) {
	rows, err := q.Query(ctx, `
		SELECT m.id::text, m.name, d.depends_on_metric_id::text,
		       COALESCE(d.min_time_offset,0), COALESCE(d.max_time_offset,0),
		       COALESCE(d.unbounded_past,false), COALESCE(d.unbounded_future,false)
		FROM model.metric_def m
		LEFT JOIN model.calc_dependency d ON d.metric_id = m.id
		WHERE m.model_id=$1::uuid
		  AND (m.revision_id IS NOT DISTINCT FROM NULLIF($2,'')::uuid OR m.revision_id IS NULL)
	`, modelID, revisionID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	g := Graph{}
	names := map[string]string{}
	for rows.Next() {
		var id, name string
		var to *string
		var e Edge
		if err := rows.Scan(&id, &name, &to, &e.MinTimeOffset, &e.MaxTimeOffset, &e.UnboundedPast, &e.UnboundedFuture); err != nil {
			return nil, nil, err
		}
		names[id] = name
		if _, ok := g[id]; !ok {
			g[id] = nil
		}
		if to != nil {
			e.To = *to
			g[id] = append(g[id], e)
		}
	}
	return g, names, rows.Err()
}

// Execer is what WriteDependencies needs: a transaction (preferred) or pool.
type Execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// WriteDependencies replaces metricID's calc_dependency rows with edges —
// the one write path for all four save paths (developer create/update, AI
// create_metric/update_metric), so offsets are never dropped by one of them.
func WriteDependencies(ctx context.Context, tx Execer, metricID string, edges []Edge) error {
	if _, err := tx.Exec(ctx, `DELETE FROM model.calc_dependency WHERE metric_id=$1::uuid`, metricID); err != nil {
		return err
	}
	for _, e := range edges {
		to := e.To
		if to == SelfReference {
			to = metricID
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO model.calc_dependency
			    (metric_id, depends_on_metric_id, min_time_offset, max_time_offset, unbounded_past, unbounded_future)
			VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6)
			ON CONFLICT (metric_id, depends_on_metric_id) DO UPDATE SET
			    min_time_offset = LEAST(model.calc_dependency.min_time_offset, EXCLUDED.min_time_offset),
			    max_time_offset = GREATEST(model.calc_dependency.max_time_offset, EXCLUDED.max_time_offset),
			    unbounded_past = model.calc_dependency.unbounded_past OR EXCLUDED.unbounded_past,
			    unbounded_future = model.calc_dependency.unbounded_future OR EXCLUDED.unbounded_future
		`, metricID, to, e.MinTimeOffset, e.MaxTimeOffset, e.UnboundedPast, e.UnboundedFuture); err != nil {
			return fmt.Errorf("record formula dependency: %w", err)
		}
	}
	return nil
}

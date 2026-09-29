package calculation

import (
	"context"
	"fmt"
	"sort"

	"github.com/mavericks-engine/mavericks/internal/metricformula"
)

// RecalcDimensionDependents recomputes every calculated metric whose formula
// references the dimension or a dimension related to it (a property
// grouping, a parent/child chain) — as a bare name, dim.property, PARENT, or
// a LOOKUP / criteria-range dimension — plus every metric depending on one of
// them, in each revision the dimension is visible in (contract C8). Those
// references create no calc_dependency edges, so RecalcAffected's walk from
// changed inputs cannot find them: a member created, edited or deleted, a
// property renamed, retyped or deleted, or a member import must call this.
func (s *Scheduler) RecalcDimensionDependents(ctx context.Context, dimensionID string) (err error) {
	defer s.recoverAsError(&err, "recalculation after a dimension change")
	var modelID, revisionID, name string
	if err := s.store.pool.QueryRow(ctx, `
		SELECT model_id::text, COALESCE(revision_id::text,''), name
		FROM model.dimension_def WHERE id=$1::uuid
	`, dimensionID).Scan(&modelID, &revisionID, &name); err != nil {
		return fmt.Errorf("load dimension: %w", err)
	}
	// A formula naming a dimension RELATED to this one (a source_property
	// grouping of it, or a structural parent/child chain through it) reads
	// this dimension's members too: SUMIFS(salary, area, "EMEA") over
	// salary[employees] regroups when an employee's area property changes.
	related, err := s.relatedDimensions(ctx, modelID, dimensionID)
	if err != nil {
		return fmt.Errorf("find dimensions related to %s: %w", name, err)
	}
	byRev := map[string][]string{}
	// The dimension and every related one, by revision: a calculated metric
	// PLACED on a grid with one of them reads through it too, formula or
	// not — area_sal = salary on a grid by area sums the employees whose
	// area property names each area member, so an employee's area edit
	// (or a new area member) changes it.
	placedOn := map[string]map[string]bool{}
	for _, d := range append([]relatedDim{{id: dimensionID, revisionID: revisionID, name: name}}, related...) {
		found, err := metricformula.MetricsReferencingDimension(ctx, s.store.pool, modelID, d.revisionID, d.name)
		if err != nil {
			return fmt.Errorf("find metrics reading dimension %s: %w", d.name, err)
		}
		for rev, ids := range found {
			byRev[rev] = append(byRev[rev], ids...)
		}
		if d.revisionID != "" {
			if placedOn[d.revisionID] == nil {
				placedOn[d.revisionID] = map[string]bool{}
			}
			placedOn[d.revisionID][d.id] = true
		}
	}
	for rev, dimIDs := range placedOn {
		metricDims, err := s.store.LoadMetricDimensionIDs(ctx, modelID, rev)
		if err != nil {
			return fmt.Errorf("load metric dimensions: %w", err)
		}
		for metricID, ids := range metricDims {
			for _, id := range ids {
				if dimIDs[id] {
					byRev[rev] = append(byRev[rev], metricID)
					break
				}
			}
		}
	}
	revs := make([]string, 0, len(byRev))
	for rev := range byRev {
		revs = append(revs, rev)
	}
	sort.Strings(revs)
	var firstErr error
	for _, rev := range revs {
		defs, err := s.store.LoadModelMetrics(ctx, modelID, rev)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("load metrics: %w", err)
			}
			continue
		}
		seen := map[string]bool{}
		var targets []string
		for _, id := range append(append([]string(nil), byRev[rev]...), AffectedMetricIDs(defs, byRev[rev])...) {
			if def, ok := defs[id]; ok && def.IsInput {
				continue // placed on a related grid, but nothing to compute
			}
			if !seen[id] {
				seen[id] = true
				targets = append(targets, id)
			}
		}
		if err := s.RecalcSpecific(ctx, modelID, rev, targets); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

type relatedDim struct{ id, revisionID, name string }

// relatedDimensions returns every other dimension of the model connected to
// dimensionID through source_dimension_id (a property grouping) or
// parent_dimension_id (a structural chain), in either direction and
// transitively — the dimensions rollup.Relates can relate to it.
func (s *Scheduler) relatedDimensions(ctx context.Context, modelID, dimensionID string) ([]relatedDim, error) {
	rows, err := s.store.pool.Query(ctx, `
		SELECT id::text, COALESCE(revision_id::text,''), name,
		       COALESCE(parent_dimension_id::text,''), COALESCE(source_dimension_id::text,'')
		FROM model.dimension_def WHERE model_id=$1::uuid
	`, modelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type dim struct{ rev, name string }
	dims := map[string]dim{}
	adj := map[string][]string{}
	for rows.Next() {
		var id, rev, dname, parent, source string
		if err := rows.Scan(&id, &rev, &dname, &parent, &source); err != nil {
			return nil, err
		}
		dims[id] = dim{rev, dname}
		for _, other := range []string{parent, source} {
			if other != "" && other != id {
				adj[id] = append(adj[id], other)
				adj[other] = append(adj[other], id)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	seen := map[string]bool{dimensionID: true}
	queue := []string{dimensionID}
	var out []relatedDim
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range adj[cur] {
			if seen[next] {
				continue
			}
			seen[next] = true
			queue = append(queue, next)
			if d, ok := dims[next]; ok {
				out = append(out, relatedDim{id: next, revisionID: d.rev, name: d.name})
			}
		}
	}
	return out, nil
}

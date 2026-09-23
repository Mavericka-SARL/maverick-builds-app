package metricformula

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/formula"
)

// Revision-level time validation (spec §4.4). A metric's dimensions are
// derived from grid placement (grid_metric ⋈ grid_dimension), so the checks
// that need dimensions — does a time-series formula have exactly one time
// dimension, do the series it reads live on the same one, is every
// recurrence member on that axis — run when a grid gains a metric or a
// dimension and when a revision is published, not only at formula save.

type metricTimeInfo struct {
	id, name, formula string
	isInput           bool
	timeDims          []string // time dimension IDs among its grid dims
	gridID            string
}

// ValidateTime checks every metric of the revision and returns the first
// problem as a ValidationError carrying its identifier. Run at publication.
func ValidateTime(ctx context.Context, q Querier, modelID, revisionID string) error {
	return validateTime(ctx, q, modelID, revisionID, "")
}

// ValidateGridTime is ValidateTime restricted to one grid: its own time
// dimension count and the metrics placed on it. Run when a grid gains a
// metric or a dimension, so configuring one grid is never blocked by a
// metric still stranded elsewhere — publication catches those.
func ValidateGridTime(ctx context.Context, q Querier, modelID, revisionID, gridID string) error {
	return validateTime(ctx, q, modelID, revisionID, gridID)
}

func validateTime(ctx context.Context, q Querier, modelID, revisionID, gridID string) error {
	// Every grid (or just the one) may carry at most one time dimension —
	// checked on the grid itself so an empty grid is covered too.
	grows, err := q.Query(ctx, `
		SELECT g.name, COUNT(*) FILTER (WHERE d.dimension_type = 'time')
		FROM model.grid_def g
		LEFT JOIN model.grid_dimension gd ON gd.grid_id = g.id
		LEFT JOIN model.dimension_def d ON d.id = gd.dimension_id
		WHERE g.model_id=$1::uuid
		  AND (g.revision_id IS NOT DISTINCT FROM NULLIF($2,'')::uuid OR g.revision_id IS NULL)
		  AND ($3 = '' OR g.id = $3::uuid)
		GROUP BY g.id, g.name
	`, modelID, revisionID, gridID)
	if err != nil {
		return err
	}
	for grows.Next() {
		var name string
		var n int
		if err := grows.Scan(&name, &n); err != nil {
			grows.Close()
			return err
		}
		if n > 1 {
			grows.Close()
			return invalidCode(formula.CodeMultipleTimeDimensions,
				"grid %q has %d time dimensions; a grid may contain at most one", name, n)
		}
	}
	grows.Close()
	if err := grows.Err(); err != nil {
		return err
	}

	rows, err := q.Query(ctx, `
		SELECT m.id::text, m.name, COALESCE(m.formula,''), m.is_input,
		       COALESCE(gm.grid_id::text,''),
		       COALESCE((
		           SELECT string_agg(d.id::text, ',' ORDER BY d.id)
		           FROM model.grid_dimension gd
		           JOIN model.dimension_def d ON d.id = gd.dimension_id
		           WHERE gd.grid_id = gm.grid_id AND d.dimension_type = 'time'
		       ), '')
		FROM model.metric_def m
		LEFT JOIN model.grid_metric gm ON gm.metric_id = m.id
		WHERE m.model_id=$1::uuid
		  AND (m.revision_id IS NOT DISTINCT FROM NULLIF($2,'')::uuid OR m.revision_id IS NULL)
	`, modelID, revisionID)
	if err != nil {
		return err
	}
	// All metrics are loaded (a reference must resolve whichever grid it is
	// on); checked is the subset the per-metric rules apply to.
	infos := map[string]*metricTimeInfo{}
	for rows.Next() {
		var mi metricTimeInfo
		var dims string
		if err := rows.Scan(&mi.id, &mi.name, &mi.formula, &mi.isInput, &mi.gridID, &dims); err != nil {
			rows.Close()
			return err
		}
		if dims != "" {
			mi.timeDims = strings.Split(dims, ",")
		}
		infos[mi.id] = &mi
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	// Members per time dimension, to reject an empty axis.
	memberCount := map[string]int{}
	mrows, err := q.Query(ctx, `
		SELECT d.id::text, COUNT(m.id)
		FROM model.dimension_def d
		LEFT JOIN model.dimension_member m ON m.dimension_id = d.id AND m.period_start IS NOT NULL
		WHERE d.model_id=$1::uuid AND d.dimension_type='time'
		GROUP BY d.id
	`, modelID)
	if err != nil {
		return err
	}
	for mrows.Next() {
		var id string
		var n int
		if err := mrows.Scan(&id, &n); err != nil {
			mrows.Close()
			return err
		}
		memberCount[id] = n
	}
	mrows.Close()

	ids := make([]string, 0, len(infos))
	for id, mi := range infos {
		if gridID == "" || mi.gridID == gridID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	byName := map[string]*metricTimeInfo{}
	for _, mi := range infos {
		byName[strings.ToUpper(mi.name)] = mi
	}

	for _, id := range ids {
		mi := infos[id]
		if len(mi.timeDims) > 1 {
			return invalidCode(formula.CodeMultipleTimeDimensions,
				"grid of metric %q has %d time dimensions; a grid may contain at most one", mi.name, len(mi.timeDims))
		}
		if mi.isInput || strings.TrimSpace(mi.formula) == "" {
			continue
		}
		an, err := formula.Analyze(mi.formula)
		if err != nil {
			continue // formula-level problems are reported at save time
		}
		if !an.UsesTimeSeries {
			continue
		}
		if len(mi.timeDims) == 0 {
			return invalidCode(formula.CodeTimeDimensionRequired,
				"metric %q uses a time-series function but is not on a grid with a time dimension", mi.name)
		}
		axis := mi.timeDims[0]
		if memberCount[axis] == 0 {
			return invalidCode(formula.CodeInvalidTimeMember,
				"metric %q uses a time-series function but its time dimension has no periods", mi.name)
		}
		for _, ref := range an.References {
			dep, ok := byName[strings.ToUpper(ref.Name)]
			if !ok || len(dep.timeDims) == 0 {
				continue // a dimension name, or a metric without a time axis (resolved by rollup)
			}
			if dep.timeDims[0] != axis {
				return invalidCode(formula.CodeTimeDimensionMismatch,
					"metric %q reads %q over a different time dimension; Phase 1 does not map one calendar onto another", mi.name, dep.name)
			}
		}
	}

	// Cycles: causal, and every recurrence member on one shared time axis.
	graph, names, err := LoadGraph(ctx, q, modelID, revisionID)
	if err != nil {
		return err
	}
	comps, err := Plan(graph, ids, names)
	if err != nil {
		var te *TemporalError
		if asTemporal(err, &te) {
			return invalidCode(formula.CodeTemporalCycleNotCausal, "%s", te.Detail)
		}
		return err
	}
	for _, c := range comps {
		if !c.Recurrence {
			continue
		}
		axis := ""
		for _, id := range c.Members {
			mi := infos[id]
			if mi == nil {
				continue
			}
			if len(mi.timeDims) == 0 {
				return invalidCode(formula.CodeTimeDimensionRequired,
					"metric %q is part of a time-broken dependency cycle but is not on a grid with a time dimension", mi.name)
			}
			if axis == "" {
				axis = mi.timeDims[0]
			} else if axis != mi.timeDims[0] {
				return invalidCode(formula.CodeTimeDimensionMismatch,
					"metrics in one dependency cycle (%s) must share a time dimension", strings.Join(memberNames(c.Members, names), ", "))
			}
		}
	}
	return nil
}

func memberNames(ids []string, names map[string]string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, label(names, id))
	}
	return out
}

func asTemporal(err error, target **TemporalError) bool {
	return errors.As(err, target)
}

// IsValidationError reports whether err is a rejection the caller can act
// on (400) rather than an internal failure.
func IsValidationError(err error) bool {
	var ve *ValidationError
	return errors.As(err, &ve)
}

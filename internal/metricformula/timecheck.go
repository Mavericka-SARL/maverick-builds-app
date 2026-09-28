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

	// All metrics are loaded (a reference must resolve whichever grid it is
	// on); checked is the subset the per-metric rules apply to.
	infos, byName, err := loadMetricTimeInfos(ctx, q, modelID, revisionID)
	if err != nil {
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

	if err := validateDimensionalPlacement(ctx, q, modelID, revisionID, gridID, infos, byName); err != nil {
		return err
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

// loadMetricTimeInfos loads every metric of the revision with its grid and
// the time dimensions of that grid, indexed by ID and by UPPER-CASE name.
func loadMetricTimeInfos(ctx context.Context, q Querier, modelID, revisionID string) (map[string]*metricTimeInfo, map[string]*metricTimeInfo, error) {
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
		return nil, nil, err
	}
	infos := map[string]*metricTimeInfo{}
	for rows.Next() {
		var mi metricTimeInfo
		var dims string
		if err := rows.Scan(&mi.id, &mi.name, &mi.formula, &mi.isInput, &mi.gridID, &dims); err != nil {
			rows.Close()
			return nil, nil, err
		}
		if dims != "" {
			mi.timeDims = strings.Split(dims, ",")
		}
		infos[mi.id] = &mi
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	byName := map[string]*metricTimeInfo{}
	for _, mi := range infos {
		byName[strings.ToUpper(mi.name)] = mi
	}
	return infos, byName, nil
}

// ValidateGridDimensional re-runs only the dimensional placement checks
// (DIMENSION_NOT_ON_SOURCE, CONFLICTING_DIMENSION_ARGUMENTS, TIMESUM period
// codes) for one grid and every metric, on any grid, reading a source
// placed on it. Run when a grid LOSES a dimension: the grid's metrics lose
// that dimension, so a LOOKUP or criteria range along it elsewhere no
// longer relates to its source. (The time rules are not re-run on removal;
// publication still checks them.)
func ValidateGridDimensional(ctx context.Context, q Querier, modelID, revisionID, gridID string) error {
	infos, byName, err := loadMetricTimeInfos(ctx, q, modelID, revisionID)
	if err != nil {
		return err
	}
	return validateDimensionalPlacement(ctx, q, modelID, revisionID, gridID, infos, byName)
}

// validateDimensionalPlacement re-checks, now that grid placement fixes
// metrics' dimensions, what formula save could only check for placed
// sources: every LOOKUP / *IFS dimension is on its source or related to one
// (DIMENSION_NOT_ON_SOURCE), no two select members of the same own
// dimension (CONFLICTING_DIMENSION_ARGUMENTS), every literal LOOKUP member
// still exists and TIMESUM's literal period codes are members of the
// metric's time axis (UNKNOWN_MEMBER).
//
// Restricted to one grid, it checks the grid's own metrics and every
// metric — on any grid — whose dimensional call reads a source placed on
// this grid, since changing the grid changes that source's dimensions.
func validateDimensionalPlacement(ctx context.Context, q Querier, modelID, revisionID, gridID string,
	infos map[string]*metricTimeInfo, byName map[string]*metricTimeInfo) error {
	type candidate struct {
		mi *metricTimeInfo
		an *formula.Analysis
	}
	sourceID := func(name string) string {
		if dep, ok := byName[strings.ToUpper(name)]; ok {
			return dep.id
		}
		return ""
	}
	var cands []candidate
	for _, mi := range infos {
		if mi.isInput || strings.TrimSpace(mi.formula) == "" {
			continue
		}
		an, err := formula.Analyze(mi.formula)
		if err != nil || (len(an.DimensionalCalls) == 0 && len(an.TimeSums) == 0) {
			continue
		}
		relevant := gridID == "" || mi.gridID == gridID
		for _, c := range an.DimensionalCalls {
			if relevant {
				break
			}
			if dep, ok := byName[strings.ToUpper(c.Source)]; ok && c.Source != "" && dep.gridID == gridID {
				relevant = true
			}
		}
		if relevant {
			cands = append(cands, candidate{mi: mi, an: an})
		}
	}
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].mi.name < cands[j].mi.name })
	rd, err := loadRevisionDims(ctx, q, modelID, revisionID)
	if err != nil {
		return err
	}
	metricDims, err := loadMetricGridDims(ctx, q, modelID, revisionID)
	if err != nil {
		return err
	}
	for _, c := range cands {
		if err := checkSourceDimensions(c.mi.name, c.an, rd, sourceID, metricDims); err != nil {
			return err
		}
		if err := checkLiteralMembers(ctx, q, c.mi.name, c.an, rd); err != nil {
			return err
		}
		if len(c.mi.timeDims) == 1 {
			if err := checkTimeSumCodes(ctx, q, c.mi.name, c.an, rd, c.mi.timeDims[0]); err != nil {
				return err
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

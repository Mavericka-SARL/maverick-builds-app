package metricformula

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/rollup"
)

// Save-time and placement-time checks of dimensional references (contract
// C1–C5, C10): dim.property, PARENT, LOOKUP, the *IFS/*IF family and
// TIMESUM's literal period codes. What can be checked depends on what is
// known: a formula save always knows the revision's dimensions, members and
// property declarations; a source metric's own dimensions are known only
// once it is placed on a grid, so DIMENSION_NOT_ON_SOURCE and literal
// CONFLICTING_DIMENSION_ARGUMENTS are checked at save when the source is
// placed, and again at grid placement and revision activation.

// dimInfo is one dimension row in scope for a revision.
type dimInfo struct {
	id, name string
	isTime   bool
	// owned is true for the revision's own row, false for a revision-less
	// legacy row that may share its name.
	owned bool
}

// revisionDims is the revision's dimensions, indexed for name resolution
// and, as rollup.Dimension structure (no members), for rollup.Relates.
type revisionDims struct {
	byName    map[string][]*dimInfo // UPPER-CASE name
	byID      map[string]*dimInfo
	structure map[string]*rollup.Dimension
}

func loadRevisionDims(ctx context.Context, q Querier, modelID, revisionID string) (*revisionDims, error) {
	rows, err := q.Query(ctx, `
		SELECT id::text, name, dimension_type = 'time', revision_id IS NOT NULL,
		       COALESCE(parent_dimension_id::text,''), COALESCE(source_dimension_id::text,''),
		       COALESCE(source_property,'')
		FROM model.dimension_def
		WHERE model_id=$1::uuid
		  AND (revision_id IS NOT DISTINCT FROM NULLIF($2,'')::uuid OR revision_id IS NULL)
		ORDER BY id
	`, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	rd := &revisionDims{byName: map[string][]*dimInfo{}, byID: map[string]*dimInfo{}, structure: map[string]*rollup.Dimension{}}
	for rows.Next() {
		var d dimInfo
		var parentID, sourceID, sourceProp string
		if err := rows.Scan(&d.id, &d.name, &d.isTime, &d.owned, &parentID, &sourceID, &sourceProp); err != nil {
			return nil, err
		}
		di := d
		rd.byID[d.id] = &di
		key := strings.ToUpper(d.name)
		rd.byName[key] = append(rd.byName[key], &di)
		rd.structure[d.id] = &rollup.Dimension{ID: d.id, ParentDimensionID: parentID, SourceDimensionID: sourceID,
			SourceProperty: sourceProp, IsTime: d.isTime}
	}
	return rd, rows.Err()
}

// lookup resolves a dimension name as written in a formula,
// case-insensitively, preferring the revision's own row over a
// revision-less legacy row of the same name. nil when none matches.
func (rd *revisionDims) lookup(name string) *dimInfo {
	cands := rd.byName[strings.ToUpper(name)]
	for _, d := range cands {
		if d.owned {
			return d
		}
	}
	if len(cands) > 0 {
		return cands[0]
	}
	return nil
}

// loadMetricGridDims returns each placed metric's own dimension IDs
// (grid_metric ⋈ grid_dimension — a metric belongs to at most one grid).
// A metric absent from the map is not placed: its dimensions are unknown.
func loadMetricGridDims(ctx context.Context, q Querier, modelID, revisionID string) (map[string][]string, error) {
	rows, err := q.Query(ctx, `
		SELECT gm.metric_id::text, COALESCE(gd.dimension_id::text,'')
		FROM model.grid_metric gm
		JOIN model.metric_def m ON m.id = gm.metric_id
		LEFT JOIN model.grid_dimension gd ON gd.grid_id = gm.grid_id
		WHERE m.model_id=$1::uuid
		  AND (m.revision_id IS NOT DISTINCT FROM NULLIF($2,'')::uuid OR m.revision_id IS NULL)
		ORDER BY gm.metric_id, gd.dimension_id
	`, modelID, revisionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var metricID, dimID string
		if err := rows.Scan(&metricID, &dimID); err != nil {
			return nil, err
		}
		if _, ok := out[metricID]; !ok {
			out[metricID] = []string{}
		}
		if dimID != "" {
			out[metricID] = append(out[metricID], dimID)
		}
	}
	return out, rows.Err()
}

func propertyDeclared(ctx context.Context, q Querier, dimID, prop string) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM model.dimension_property
		               WHERE dimension_id=$1::uuid AND lower(name)=lower($2))
	`, dimID, prop).Scan(&ok)
	return ok, err
}

// memberExists reports whether dimID has a member with exactly code, at
// any level (a parent member and an aggregate period count).
func memberExists(ctx context.Context, q Querier, dimID, code string) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2)
	`, dimID, code).Scan(&ok)
	return ok, err
}

// checkDimensionNames checks the formula's dimension-shaped arguments
// against the revision: every dim.property names a dimension and a declared
// property (UNKNOWN_PROPERTY), every PARENT/LOOKUP/criteria dimension
// argument is a dimension — never a metric of the same name
// (DIMENSION_ARGUMENT_REQUIRED) — and every literal LOOKUP member exists in
// its dimension (UNKNOWN_MEMBER). isMetric reports whether a name is a
// metric of the revision, for a message that says so; suggest says what an
// unknown name most likely meant (suggestName).
func checkDimensionNames(ctx context.Context, q Querier, an *formula.Analysis, rd *revisionDims, isMetric func(string) bool, suggest func(string) string) error {
	for _, arg := range an.DimensionArgs {
		if rd.lookup(arg) != nil {
			continue
		}
		if isMetric(arg) {
			return invalidCode(formula.CodeDimensionArgRequired,
				"%s is a metric, but this argument must be a dimension name (PARENT, LOOKUP and criteria ranges take a dimension)", arg)
		}
		return invalidCode(formula.CodeDimensionArgRequired,
			"there is no dimension named %s in this revision (PARENT, LOOKUP and criteria ranges take a dimension name)%s", arg, suggest(arg))
	}
	for _, p := range an.PropertyRefs {
		d := rd.lookup(p.Dim)
		if d == nil {
			if isMetric(p.Dim) {
				return invalidCode(formula.CodeDimensionArgRequired,
					"%s.%s: %s is a metric; the dimension.property form reads a property of a dimension's member", p.Dim, p.Property, p.Dim)
			}
			return invalidCode(formula.CodeDimensionArgRequired,
				"%s.%s: there is no dimension named %s in this revision%s", p.Dim, p.Property, p.Dim, suggest(p.Dim))
		}
		ok, err := propertyDeclared(ctx, q, d.id, p.Property)
		if err != nil {
			return err
		}
		if !ok {
			return invalidCode(formula.CodeUnknownProperty,
				"%s.%s: dimension %s has no property named %s; declare it on the dimension first", p.Dim, p.Property, d.name, p.Property)
		}
	}
	return checkLiteralMembers(ctx, q, "", an, rd)
}

// checkLiteralMembers checks every literal member a LOOKUP names exists in
// its dimension (UNKNOWN_MEMBER) — at save, and again at placement and
// activation, since a member can be deleted after the formula was saved.
// metricName, when known, is named in the message.
func checkLiteralMembers(ctx context.Context, q Querier, metricName string, an *formula.Analysis, rd *revisionDims) error {
	if err := checkCriteriaLeaves(ctx, q, metricName, an, rd); err != nil {
		return err
	}
	for _, call := range an.DimensionalCalls {
		for _, m := range call.Members {
			code, ok := m.LiteralCode()
			if !ok {
				continue
			}
			d := rd.lookup(m.Dim)
			if d == nil {
				continue // reported as a DimensionArg
			}
			exists, err := memberExists(ctx, q, d.id, code)
			if err != nil {
				return err
			}
			if !exists {
				where := call.Func
				if metricName != "" {
					where = call.Func + " in " + metricName
				}
				return invalidCode(formula.CodeUnknownMember,
					"%s: dimension %s has no member %q — add the member first (a total such as All Regions is a parent member: add it, then move the members under it)", where, d.name, code)
			}
			var calcFormula string
			_ = q.QueryRow(ctx, `SELECT COALESCE(btrim(formula),'') FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`, d.id, code).Scan(&calcFormula)
			if calcFormula != "" {
				where := call.Func
				if metricName != "" {
					where = call.Func + " in " + metricName
				}
				return invalidCode(formula.CodeUnknownMember,
					"%s: %s of %s is a calculated member (= %s), computed from its siblings when shown — read those members instead", where, code, d.name, calcFormula)
			}
		}
	}
	return nil
}

// overriddenTimeDim reports whether a dimensional reference overrides (or
// ranges over) a time dimension: the source is then read at other periods,
// so its dependency is unbounded past and future.
func overriddenTimeDim(ref formula.ReferenceUse, rd *revisionDims) bool {
	for _, name := range ref.OverriddenDims {
		if d := rd.lookup(name); d != nil && d.isTime {
			return true
		}
	}
	return false
}

// checkSourceDimensions checks each LOOKUP and *IFS/*IF call whose source
// is placed (its dimensions are known): every override or range dimension
// is one of the source's dimensions or related to one
// (DIMENSION_NOT_ON_SOURCE, the rollup.Relates rule runtime uses; a time
// dimension the source does not carry is unrelated), and no two of them
// select members of the same own dimension of the source
// (CONFLICTING_DIMENSION_ARGUMENTS, the rollup.NormalizeCombo rule).
// sourceID resolves a source metric name to its ID ("" when unknown).
// COUNTIFS/COUNTIF have no source and may range over any dimension.
func checkSourceDimensions(metricName string, an *formula.Analysis, rd *revisionDims,
	sourceID func(string) string, metricDims map[string][]string) error {
	for _, call := range an.DimensionalCalls {
		if call.Source == "" {
			continue
		}
		srcDims, placed := metricDims[sourceID(call.Source)]
		if !placed {
			continue
		}
		var ids []string
		var names []string
		for _, name := range call.Dims {
			d := rd.lookup(name)
			if d == nil {
				// A metric criteria range (SUMIFS(sales, act_region, region))
				// is read at the source's members: each of its dimensions
				// must be one of the source's, or related to one.
				rangeDims, placedRange := metricDims[sourceID(name)]
				if sourceID(name) == "" || !placedRange {
					continue
				}
				for _, id := range rangeDims {
					rdim := rd.byID[id]
					if rdim == nil || onSource(rd, srcDims, rdim) {
						continue
					}
					return invalidCode(formula.CodeDimensionNotOnSource,
						"%s in %s tests %s, which is dimensioned by %s, but %s is not one of %s's dimensions (%s) and is not related to one — a metric used as a criteria range must sit on the source's dimensions",
						call.Func, metricName, name, rdim.name, rdim.name, call.Source, dimNames(rd, srcDims))
				}
				continue
			}
			if !onSource(rd, srcDims, d) {
				return invalidCode(formula.CodeDimensionNotOnSource,
					"%s in %s reads %s along %s, but %s is not one of %s's dimensions (%s) and is not related to one",
					call.Func, metricName, call.Source, d.name, d.name, call.Source, dimNames(rd, srcDims))
			}
			ids = append(ids, d.id)
			names = append(names, d.name)
		}
		for i := 0; i < len(ids); i++ {
			for j := i + 1; j < len(ids); j++ {
				_, err := rollup.NormalizeCombo(rd.structure, srcDims, nil, map[string]string{ids[i]: "", ids[j]: ""})
				if errors.Is(err, rollup.ErrConflictingOverrides) {
					return invalidCode(formula.CodeConflictingDimensions,
						"%s in %s sets both %s and %s, which select members of the same dimension of %s; use only one of them",
						call.Func, metricName, names[i], names[j], call.Source)
				}
			}
		}
	}
	return nil
}

func onSource(rd *revisionDims, srcDims []string, d *dimInfo) bool {
	for _, own := range srcDims {
		if own == d.id {
			return true
		}
		if d.isTime {
			continue
		}
		if rollup.Relates(rd.structure, own, d.id) {
			return true
		}
	}
	return false
}

func dimNames(rd *revisionDims, ids []string) string {
	if len(ids) == 0 {
		return "none"
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if d := rd.byID[id]; d != nil {
			out = append(out, d.name)
		} else {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

// checkTimeSumCodes checks TIMESUM's literal period codes against the
// metric's time axis (UNKNOWN_MEMBER). axisID is the time dimension of the
// metric's grid; "" when it has none (TIME_DIMENSION_REQUIRED covers that).
func checkTimeSumCodes(ctx context.Context, q Querier, metricName string, an *formula.Analysis, rd *revisionDims, axisID string) error {
	if axisID == "" {
		return nil
	}
	for _, ts := range an.TimeSums {
		if !ts.Ranged {
			continue
		}
		for _, arg := range []formula.DimensionalArg{ts.Start, ts.End} {
			code, ok := arg.LiteralCode()
			if !ok {
				continue
			}
			exists, err := memberExists(ctx, q, axisID, code)
			if err != nil {
				return err
			}
			if !exists {
				axis := axisID
				if d := rd.byID[axisID]; d != nil {
					axis = d.name
				}
				return invalidCode(formula.CodeUnknownMember,
					"TIMESUM in %s names period %q, which is not a member of its time dimension %s", metricName, code, axis)
			}
		}
	}
	return nil
}

// timeAxisOf returns the time dimension among a metric's dimensions ("" if
// none; more than one is reported by the time checks).
func timeAxisOf(rd *revisionDims, dimIDs []string) string {
	for _, id := range dimIDs {
		if d := rd.byID[id]; d != nil && d.isTime {
			return id
		}
	}
	return ""
}

// dimensionalSelfMessage explains why a metric may not read itself through
// LOOKUP, a conditional aggregation or a *VALUE function.
func dimensionalSelfMessage(an *formula.Analysis, name string) string {
	fn := ""
	for _, c := range an.DimensionalCalls {
		if strings.EqualFold(c.Source, name) {
			fn = c.Func
			break
		}
	}
	for _, c := range an.Calls {
		if fn != "" {
			break
		}
		switch strings.ToUpper(c) {
		case "YEARVALUE", "HALFYEARVALUE", "QUARTERVALUE", "MONTHVALUE":
			fn = strings.ToUpper(c)
		}
	}
	if fn == "" {
		fn = "a dimensional function"
	}
	return fmt.Sprintf("formula reads the metric it defines (%q) through %s; a metric cannot read its own values at other members or periods this way", name, fn)
}

// resolveMetricRanges turns the criteria ranges of an that name a metric of
// the revision (and no dimension) into what they are: a Dimensional
// reference to that metric, read at every leaf combination of its
// dimensions — a dependency, recalculating this metric when it changes —
// instead of a dimension argument. A name that is neither stays a
// dimension argument, for checkDimensionNames to report.
func resolveMetricRanges(ctx context.Context, q Querier, req Request, an *formula.Analysis, rd *revisionDims) {
	for _, name := range an.RangeNames {
		if rd.lookup(name) != nil {
			continue
		}
		var metricID string
		if err := q.QueryRow(ctx, `
			SELECT id::text FROM model.metric_def
			WHERE model_id=$1::uuid AND lower(name)=lower($2)
			  AND (revision_id IS NOT DISTINCT FROM NULLIF($3,'')::uuid OR revision_id IS NULL)
			ORDER BY (name = $2) DESC LIMIT 1
		`, req.ModelID, name, req.RevisionID).Scan(&metricID); err != nil {
			continue
		}
		// Its dimensions are ranged over: a time one makes the read
		// unbounded past and future (overriddenTimeDim).
		var over []string
		if rows, err := q.Query(ctx, `
			SELECT gd.dimension_id::text FROM model.grid_metric gm
			JOIN model.grid_dimension gd ON gd.grid_id = gm.grid_id
			WHERE gm.metric_id = $1::uuid`, metricID); err == nil {
			for rows.Next() {
				var id string
				if rows.Scan(&id) == nil {
					if d := rd.byID[id]; d != nil {
						over = append(over, d.name)
					}
				}
			}
			rows.Close()
		}
		kept := an.DimensionArgs[:0]
		for _, d := range an.DimensionArgs {
			if !strings.EqualFold(d, name) {
				kept = append(kept, d)
			}
		}
		an.DimensionArgs = kept
		found := false
		for i := range an.References {
			if strings.EqualFold(an.References[i].Name, name) {
				an.References[i].Dimensional = true
				an.References[i].OverriddenDims = append(an.References[i].OverriddenDims, over...)
				found = true
			}
		}
		if !found {
			an.References = append(an.References, formula.ReferenceUse{Name: name, Dimensional: true, OverriddenDims: over})
		}
	}
}

// checkCriteriaLeaves refuses a criterion that is a literal code of a member
// with members under it (SUMIFS(sales, month, "FY2027")): a criteria range
// runs over its dimension's leaf members, so nothing matches and the result
// is a silent 0 — the AI Developer wrote exactly this for a full-year total.
// The message names the reads that do mean the total.
func checkCriteriaLeaves(ctx context.Context, q Querier, metricName string, an *formula.Analysis, rd *revisionDims) error {
	for _, call := range an.DimensionalCalls {
		for _, c := range call.Criteria {
			code, ok := c.LiteralCode()
			if !ok || c.Property != "" {
				continue
			}
			d := rd.lookup(c.Dim)
			if d == nil {
				continue // a metric range, or reported as a DimensionArg
			}
			var parent bool
			if err := q.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM model.dimension_member p JOIN model.dimension_member ch ON ch.parent_member_id = p.id
				               WHERE p.dimension_id = $1::uuid AND p.code = $2)`, d.id, code).Scan(&parent); err != nil {
				return err
			}
			if !parent {
				continue
			}
			where := call.Func
			if metricName != "" {
				where = call.Func + " in " + metricName
			}
			source := call.Source
			if source == "" {
				source = "<metric>"
			}
			return invalidCode(formula.CodeUnknownMember,
				"%s: %s of %s has members under it, and a criteria range runs over leaf members only, so %q matches nothing — "+
					"for its total read LOOKUP(%s, %s, %q), or %s itself on a grid without %s (it reads the total)",
				where, code, d.name, code, source, d.name, code, source, d.name)
		}
	}
	return nil
}

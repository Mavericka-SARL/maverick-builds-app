package metricformula

import (
	"context"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/rollup"
)

// FormatPicklist is the format of a metric whose cells hold members of one
// dimension (formula.PicklistKey).
const FormatPicklist = "picklist"

// FormatText is the format of an input metric whose cells hold free text (a
// comment, an owner): stored in fact_input.text_value beside a 0.
const FormatText = "text"

// FormatDate is the format of a metric whose cells hold a date: the serial
// number DATE() gives (days since 1899-12-30, the spreadsheet's), shown and
// typed as yyyy-mm-dd. Formulas read the number (DAYS, comparisons).
const FormatDate = "date"

// Picklist is a metric's pick-list setting as resolved by ResolvePicklist:
// the dimension (ID; "" for any other format) and the aggregation rule and
// time summary to store.
type Picklist struct {
	DimensionID string
	AggRule     string
	TimeSummary string
}

// ResolvePicklist checks a metric's pick-list settings and fills their
// defaults. Format "picklist" needs dimension — an ID or a name of a
// dimension of the metric's revision — and every other format must not
// name one. A pick-list's members are never added up: an input gives no
// total (agg_rule "none", its default), a calculated one evaluates its
// formula at the total ("formula") or gives none — its default is
// PicklistTotalDefault of formulaText — and neither totals time
// (time_summary "none", the default). aggRule and timeSummary are the
// requested values ("" for the default); for any other format they are
// returned unchanged.
func ResolvePicklist(ctx context.Context, q Querier, modelID, revisionID, format, dimension, aggRule, timeSummary string, isInput bool, formulaText string) (Picklist, error) {
	dimension = strings.TrimSpace(dimension)
	if format != FormatPicklist {
		if dimension != "" {
			return Picklist{}, invalid("picklist_dimension is for a metric whose format is %q; this metric's format is %q", FormatPicklist, format)
		}
		if format == FormatText && isInput {
			return textInput(aggRule, timeSummary)
		}
		if format == FormatText {
			return textCalc(aggRule, timeSummary)
		}
		if format == FormatDate {
			return dateMetric(aggRule, timeSummary)
		}
		if !isInput && formula.IsRankFormula(formulaText) {
			// Ranks never add up: a new rank metric gives no total and
			// no time summary unless it asks for one.
			if aggRule == "" {
				aggRule = string(rollup.AggNone)
			}
			if timeSummary == "" {
				timeSummary = "none"
			}
		}
		return Picklist{AggRule: aggRule, TimeSummary: timeSummary}, nil
	}
	if dimension == "" {
		return Picklist{}, invalid("a pick-list metric needs the dimension whose members its cells hold (picklist_dimension: a dimension's id or name)")
	}
	var dimID string
	err := q.QueryRow(ctx, `
		SELECT id::text FROM model.dimension_def
		WHERE model_id=$1::uuid AND (revision_id IS NOT DISTINCT FROM NULLIF($2,'')::uuid OR revision_id IS NULL)
		  AND (id::text = $3 OR lower(name) = lower($3))
		ORDER BY (id::text = $3) DESC, (revision_id IS NOT NULL) DESC LIMIT 1`,
		modelID, revisionID, dimension).Scan(&dimID)
	if err != nil {
		return Picklist{}, invalid("there is no dimension %q in this revision for the pick-list to hold members of", dimension)
	}
	switch aggRule {
	case "":
		// An input gives no total; a calculated pick-list is its formula
		// at the total, as a spreadsheet's total row computes its status —
		// when what it reads has a total (PicklistTotalDefault).
		aggRule = string(rollup.AggNone)
		if !isInput {
			aggRule = PicklistTotalDefault(ctx, q, modelID, revisionID, formulaText)
		}
	case string(rollup.AggNone):
	case string(rollup.AggFormula):
		if isInput {
			return Picklist{}, invalid("a pick-list input has no total: its agg_rule is %q", rollup.AggNone)
		}
	default:
		return Picklist{}, invalid("a pick-list's members are never added up: its agg_rule is %q (no total) or, for a calculated pick-list, %q (the formula evaluated at the total), not %q",
			rollup.AggNone, rollup.AggFormula, aggRule)
	}
	switch timeSummary {
	case "", "none":
		timeSummary = "none"
	default:
		return Picklist{}, invalid("a pick-list's members are never added up over time: its time_summary is \"none\", not %q", timeSummary)
	}
	return Picklist{DimensionID: dimID, AggRule: aggRule, TimeSummary: timeSummary}, nil
}

// textInput is ResolvePicklist for a text input: notes are never added up,
// across members or time.
func textInput(aggRule, timeSummary string) (Picklist, error) {
	if aggRule != "" && aggRule != string(rollup.AggNone) {
		return Picklist{}, invalid("a text metric's notes are never added up: its agg_rule is %q, not %q", rollup.AggNone, aggRule)
	}
	if timeSummary != "" && timeSummary != "none" {
		return Picklist{}, invalid("a text metric's notes are never added up over time: its time_summary is \"none\", not %q", timeSummary)
	}
	return Picklist{AggRule: string(rollup.AggNone), TimeSummary: "none"}, nil
}

// textCalc is ResolvePicklist for a calculated text (an action key, a
// status word): text is never added up. It has no total ("none", the
// default) or its formula at every total ("formula": a status of the whole).
func textCalc(aggRule, timeSummary string) (Picklist, error) {
	switch aggRule {
	case "":
		aggRule = string(rollup.AggNone)
	case string(rollup.AggNone), string(rollup.AggFormula):
	default:
		return Picklist{}, invalid("text is never added up: a calculated text's agg_rule is %q (no total) or %q (its formula at the total), not %q", rollup.AggNone, rollup.AggFormula, aggRule)
	}
	if timeSummary != "" && timeSummary != "none" {
		return Picklist{}, invalid("text is never added up over time: a calculated text's time_summary is \"none\", not %q", timeSummary)
	}
	return Picklist{AggRule: aggRule, TimeSummary: "none"}, nil
}

// dateMetric is ResolvePicklist for a date: dates are never added up. With
// no rule given a date has no total; the earliest, latest, average or count
// of them are totals that mean something.
func dateMetric(aggRule, timeSummary string) (Picklist, error) {
	switch aggRule {
	case "":
		aggRule = string(rollup.AggNone)
	case string(rollup.AggSum), string(rollup.AggRate):
		return Picklist{}, invalid("dates are never added up: a date metric's agg_rule is none, average, count or formula, not %q", aggRule)
	}
	switch timeSummary {
	case "":
		timeSummary = "none"
	case "sum":
		return Picklist{}, invalid("dates are never added up over time: a date metric's time_summary is none, first, last, min, max or average, not \"sum\"")
	}
	return Picklist{AggRule: aggRule, TimeSummary: timeSummary}, nil
}

// PicklistTotalDefault is the aggregation a calculated pick-list gets when
// none is chosen. "formula" — its formula evaluated at the total, as a
// spreadsheet's total row computes a status — when every name the formula
// reads is a metric of the revision that has a total of its own; "none"
// when it reads one that has none (aggregation none: a mean, a date, a
// text, a class), or the cell's own member (a dimension, dim.property,
// PARENT), which a total leaves blank. Evaluated over those blanks, the
// total named a class or an action no member has: an inventory model's
// "SKU001 / All locations" read "Expedite supply" and every product total
// XYZ "None" (2026-11-21). A chosen aggregation is never replaced, and a
// metric saved before keeps its own.
func PicklistTotalDefault(ctx context.Context, q Querier, modelID, revisionID, formulaText string) string {
	an, err := formula.Analyze(formulaText)
	if err != nil || len(an.CellDims) > 0 || len(an.PropertyRefs) > 0 {
		return string(rollup.AggNone)
	}
	names := make([]string, 0, len(an.References))
	for _, r := range an.References {
		names = append(names, strings.ToLower(r.Name))
	}
	if len(names) == 0 {
		return string(rollup.AggFormula)
	}
	rows, err := q.Query(ctx, `
		SELECT lower(name), COALESCE(agg_rule, '') FROM model.metric_def
		WHERE model_id = $1::uuid AND revision_id IS NOT DISTINCT FROM NULLIF($2, '')::uuid AND lower(name) = ANY($3)`,
		modelID, revisionID, names)
	if err != nil {
		return string(rollup.AggNone)
	}
	defer rows.Close()
	hasTotal := map[string]bool{}
	for rows.Next() {
		var name, agg string
		if rows.Scan(&name, &agg) == nil {
			hasTotal[name] = agg != string(rollup.AggNone)
		}
	}
	for _, n := range names {
		// A name that is not a metric is a dimension read at the cell.
		if !hasTotal[n] {
			return string(rollup.AggNone)
		}
	}
	return string(rollup.AggFormula)
}

package calculation

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/rollup"
)

// Member metadata for the dimensional references of the formula language
// (FORMULA_CALCULATION_INSTRUCTIONS.md, contract C1-C3).
//
// DimMetadata is the cell-local half: dim.property, PARENT, member checks
// and criteria leaves, all answered from member metadata alone. Every
// evaluator builds it the same way — the scheduler here, and the gateway's
// scoped recompute and the chart recompute through the exported
// NewDimMetadata/CellContext — so those functions give one answer
// everywhere. It must be built from UNFILTERED dimension metadata: a
// member's property or parent does not change with who is looking.
//
// dimReads (below) is the scheduler's other half: LOOKUP and the
// conditional aggregations reading source metrics at other members.

// DimMetadata indexes one revision's dimensions for dimensional references.
// It is safe for concurrent use.
type DimMetadata struct {
	dims    map[string]*rollup.Dimension
	byName  map[string][]string // UPPER(name) -> dimension IDs, preferred first
	props   map[string]map[string]PropertyDecl
	members map[string]map[string]*rollup.Member // dimension ID -> code -> member

	mu     sync.Mutex
	leaves map[string][]formula.DimMember

	// names maps a dimension ID to its name; metrics describes the
	// revision's metrics for pick-lists and metric criteria ranges
	// (SetMetric), keyed by UPPER(name).
	names   map[string]string
	metrics map[string]metricShape
	codecs  map[string]formula.PicklistCodec // pick-list dimension ID -> codec, built once
}

// metricShape is what a formula needs to know about a metric it names
// outside its own value: its dimensions (a criteria range) and, for a
// pick-list, the dimension whose members its cells hold.
type metricShape struct {
	dimIDs        []string
	picklistDimID string
}

// NewDimMetadata indexes dims (UNFILTERED: every member, hidden ones
// included), the dimension names (dimension ID -> name, as LoadDimIDToName
// returns them) and the revision's property declarations. schema may be
// nil (no declared properties: every dim.property is UNKNOWN_PROPERTY).
// When two dimension rows share a name, the one pinned in the cell wins,
// then the revision's own row (schema.RevisionOwned), then the lower ID.
func NewDimMetadata(dims map[string]*rollup.Dimension, dimIDToName map[string]string, schema *DimensionSchema) *DimMetadata {
	m := &DimMetadata{
		dims:    dims,
		byName:  make(map[string][]string, len(dimIDToName)),
		props:   map[string]map[string]PropertyDecl{},
		members: make(map[string]map[string]*rollup.Member, len(dims)),
		leaves:  map[string][]formula.DimMember{},
		names:   dimIDToName,
		metrics: map[string]metricShape{},
		codecs:  map[string]formula.PicklistCodec{},
	}
	var owned map[string]bool
	if schema != nil {
		m.props = schema.Properties
		owned = schema.RevisionOwned
	}
	for id, name := range dimIDToName {
		key := strings.ToUpper(name)
		m.byName[key] = append(m.byName[key], id)
	}
	for _, ids := range m.byName {
		sort.Slice(ids, func(i, j int) bool {
			if owned[ids[i]] != owned[ids[j]] {
				return owned[ids[i]]
			}
			return ids[i] < ids[j]
		})
	}
	for id, d := range dims {
		idx := make(map[string]*rollup.Member, len(d.Members))
		for i := range d.Members {
			if _, dup := idx[d.Members[i].Code]; !dup {
				idx[d.Members[i].Code] = &d.Members[i]
			}
		}
		m.members[id] = idx
	}
	return m
}

// SetMetric describes one metric of the revision: its dimensions (as
// placed on its grid) and, when its format is "picklist", the dimension its
// cells hold members of (else ""). Formulas then read the pick-list as its
// member's code and may use the metric as a criteria range. Call it for
// every metric before the first CellContext.
func (m *DimMetadata) SetMetric(name string, dimIDs []string, picklistDimID string) {
	m.metrics[strings.ToUpper(name)] = metricShape{dimIDs: dimIDs, picklistDimID: picklistDimID}
}

// DescribeMetrics calls SetMetric for every definition, with its dimensions
// from metricDimIDs (metric ID -> dimension IDs).
func DescribeMetrics(m *DimMetadata, defs map[string]*MetricDef, metricDimIDs map[string][]string) {
	for id, d := range defs {
		m.SetMetric(d.Name, metricDimIDs[id], d.PicklistDimID)
	}
}

// PicklistDimension returns the dimension ID of a pick-list metric, "" for
// any other.
func (m *DimMetadata) PicklistDimension(name string) string {
	return m.metrics[strings.ToUpper(name)].picklistDimID
}

// Picklist returns the codec of a pick-list metric.
func (m *DimMetadata) Picklist(name string) (formula.PicklistCodec, bool) {
	dimID := m.PicklistDimension(name)
	if dimID == "" {
		return formula.PicklistCodec{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.codecs[dimID]; ok {
		return c, true
	}
	byKey := map[float64]string{}
	exact := m.members[dimID]
	fold := make(map[string]string, len(exact))
	for code := range exact {
		byKey[formula.PicklistKey(code)] = code
		if _, dup := fold[strings.ToUpper(code)]; !dup {
			fold[strings.ToUpper(code)] = code
		}
	}
	c := formula.PicklistCodec{
		Dim: m.names[dimID],
		Code: func(key float64) (string, bool) {
			code, ok := byKey[key]
			return code, ok
		},
		Key: func(code string) (float64, bool) {
			if _, ok := exact[code]; ok {
				return formula.PicklistKey(code), true
			}
			if stored, ok := fold[strings.ToUpper(code)]; ok {
				return formula.PicklistKey(stored), true
			}
			return 0, false
		},
	}
	m.codecs[dimID] = c
	return c, true
}

// metricDimNames returns the dimension names of a metric SetMetric
// described.
func (m *DimMetadata) metricDimNames(name string) ([]string, bool) {
	shape, ok := m.metrics[strings.ToUpper(name)]
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(shape.dimIDs))
	for _, id := range shape.dimIDs {
		if n := m.names[id]; n != "" {
			out = append(out, n)
		}
	}
	return out, true
}

// EncodeResult converts the formula result of metric into the value its
// cell stores: a pick-list's member code becomes its key
// (formula.PicklistResult); every other metric's result is unchanged.
func (m *DimMetadata) EncodeResult(metric string, v formula.Value) formula.Value {
	if m == nil {
		return v
	}
	codec, ok := m.Picklist(metric)
	if !ok {
		return v
	}
	return formula.PicklistResult(codec, metric, v)
}

// dimID maps a dimension name as written in a formula to its ID.
func (m *DimMetadata) dimID(name string, combo map[string]string) (string, *formula.FormulaError) {
	ids := m.byName[strings.ToUpper(name)]
	switch len(ids) {
	case 0:
		return "", &formula.FormulaError{Code: formula.ErrName.Code, Message: fmt.Sprintf("unknown dimension %s", name)}
	case 1:
		return ids[0], nil
	}
	for _, id := range ids {
		if _, pinned := combo[id]; pinned {
			return id, nil
		}
	}
	return ids[0], nil
}

// member returns dim's member with exactly code, or nil (rollup.FindMember
// semantics, indexed).
func (m *DimMetadata) member(dimID, code string) *rollup.Member {
	return m.members[dimID][code]
}

// property returns the declaration of prop on dimID, matched
// case-insensitively.
func (m *DimMetadata) property(dimID, prop string) (PropertyDecl, bool) {
	decl, ok := m.props[dimID][strings.ToUpper(prop)]
	return decl, ok
}

// rawProperty reads a member's stored value of a declared property: the
// exact key first, then a case-insensitive match.
func rawProperty(mem *rollup.Member, name string) string {
	if v, ok := mem.Properties[name]; ok {
		return v
	}
	for k, v := range mem.Properties {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

// leafMembers returns dimID's leaves with their typed declared properties,
// built once.
func (m *DimMetadata) leafMembers(dimID string) []formula.DimMember {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cached, ok := m.leaves[dimID]; ok {
		return cached
	}
	decls := m.props[dimID]
	codes := rollup.LeafCodes(m.dims[dimID])
	out := make([]formula.DimMember, 0, len(codes))
	for _, code := range codes {
		dm := formula.DimMember{Code: code}
		if mem := m.member(dimID, code); mem != nil && len(decls) > 0 {
			dm.Properties = make(map[string]formula.Value, len(decls))
			for key, decl := range decls {
				dm.Properties[key] = formula.TypedPropertyValue(rawProperty(mem, decl.Name), decl.DataType)
			}
		}
		out = append(out, dm)
	}
	m.leaves[dimID] = out
	return out
}

// errUnknownProperty is dim.property naming a property the dimension does
// not declare.
func errUnknownProperty(dim, prop string) *formula.FormulaError {
	return &formula.FormulaError{Code: formula.CodeUnknownProperty,
		Message: fmt.Sprintf("dimension %s has no property %s", dim, prop)}
}

// CellContext returns the member-metadata context of the cell at combo
// (dimension ID -> member code; a dimension absent from combo is not
// pinned). Current, Property, Parent, Member, Leaves and CoordKey are
// answered from the metadata. Resolve, which reads other cells, fails with
// DIM_CONTEXT_REQUIRED, never a silent 0: an evaluator that can read
// source metrics (the scheduler) replaces it. Memo is nil (no memoisation)
// unless the caller sets one.
func (m *DimMetadata) CellContext(combo map[string]string) *formula.DimEvalContext {
	return &formula.DimEvalContext{
		Current: func(dim string) (string, bool, *formula.FormulaError) {
			id, ferr := m.dimID(dim, combo)
			if ferr != nil {
				return "", false, ferr
			}
			code, pinned := combo[id]
			return code, pinned, nil
		},
		Property: func(dim, prop string) formula.Value {
			id, ferr := m.dimID(dim, combo)
			if ferr != nil {
				return formula.ErrorVal(ferr)
			}
			decl, ok := m.property(id, prop)
			if !ok {
				return formula.ErrorVal(errUnknownProperty(dim, prop))
			}
			code, pinned := combo[id]
			if !pinned {
				return formula.BlankVal()
			}
			mem := m.member(id, code)
			if mem == nil {
				return formula.BlankVal()
			}
			return formula.TypedPropertyValue(rawProperty(mem, decl.Name), decl.DataType)
		},
		Parent: func(dim string) (string, *formula.FormulaError) {
			id, ferr := m.dimID(dim, combo)
			if ferr != nil {
				return "", ferr
			}
			code, pinned := combo[id]
			if !pinned {
				return "", nil
			}
			if mem := m.member(id, code); mem != nil {
				return mem.ParentCode, nil
			}
			return "", nil
		},
		Member: func(dim, code string) (string, bool, *formula.FormulaError) {
			id, ferr := m.dimID(dim, combo)
			if ferr != nil {
				return "", false, ferr
			}
			if m.member(id, code) == nil {
				return "", false, nil
			}
			return code, true, nil
		},
		Leaves: func(dim string) ([]formula.DimMember, *formula.FormulaError) {
			id, ferr := m.dimID(dim, combo)
			if ferr != nil {
				return nil, ferr
			}
			return m.leafMembers(id), nil
		},
		Resolve: func(metric string, _ map[string]string) (float64, bool, *formula.FormulaError) {
			return 0, false, &formula.FormulaError{Code: formula.CodeDimContextRequired,
				Message: fmt.Sprintf("reading %s at other members needs the calculation engine's data, which this evaluation does not have", metric)}
		},
		CoordKey: func(exclude []string) string {
			return coordKey(m, combo, exclude)
		},
		Picklist:   m.Picklist,
		MetricDims: m.metricDimNames,
	}
}

// coordKey is combo without the named dimensions (every row of a name),
// canonicalised.
func coordKey(m *DimMetadata, combo map[string]string, exclude []string) string {
	if len(exclude) == 0 {
		return dimKey(combo)
	}
	drop := map[string]bool{}
	for _, name := range exclude {
		for _, id := range m.byName[strings.ToUpper(name)] {
			drop[id] = true
		}
	}
	kept := make(map[string]string, len(combo))
	for id, code := range combo {
		if !drop[id] {
			kept[id] = code
		}
	}
	return dimKey(kept)
}

// ── Scheduler reads ─────────────────────────────────────────────────────────

// dimReads fulfils DimEvalContext.Resolve for one metric's evaluations in
// the scheduler: a source metric read at the cell's coordinates with
// overrides, over the same prefetched value maps its plain references use.
// It also records whether any value read through it was non-zero, which
// feeds the "no data at this intersection" rule.
type dimReads struct {
	ctx        context.Context
	meta       *DimMetadata
	metricDims map[string][]string
	defs       map[string]*MetricDef      // dependency ID -> definition
	byName     map[string]string          // UPPER(dependency name) -> dependency ID
	fetch      map[string]rollup.RawValue // dependency ID -> prefetched values
	// rows holds, for a calculated dependency whose agg_rule is formula or
	// rate, its reader: persisted rows, then the formula on demand
	// (exactSource).
	rows    map[string]*exactSource
	related map[[2]string]bool // (source ID, dimension ID) -> relates to one of the source's dimensions
	// memo is the conditional-aggregation memo for this metric's pass (nil
	// disables it).
	memo map[string]formula.Value
	// data is set when a read returns a recorded non-zero value.
	data bool
}

func newDimReads(ctx context.Context, meta *DimMetadata, def *MetricDef, allDefs map[string]*MetricDef,
	metricDims map[string][]string, fetch map[string]rollup.RawValue, rows map[string]*exactSource, memo bool,
) *dimReads {
	r := &dimReads{
		ctx: ctx, meta: meta, metricDims: metricDims, fetch: fetch, rows: rows,
		defs:    make(map[string]*MetricDef, len(def.DependsOnID)),
		byName:  make(map[string]string, len(def.DependsOnID)),
		related: map[[2]string]bool{},
	}
	for _, depID := range def.DependsOnID {
		if d, ok := allDefs[depID]; ok {
			r.defs[depID] = d
			key := strings.ToUpper(d.Name)
			if _, dup := r.byName[key]; !dup {
				r.byName[key] = depID
			}
		}
	}
	if memo {
		r.memo = map[string]formula.Value{}
	}
	return r
}

// context is the full dimensional context of the cell at combo.
func (r *dimReads) context(combo map[string]string, useMemo bool) *formula.DimEvalContext {
	d := r.meta.CellContext(combo)
	d.Resolve = func(metric string, overrides map[string]string) (float64, bool, *formula.FormulaError) {
		return r.resolve(combo, metric, overrides)
	}
	if useMemo {
		d.Memo = r.memo
	}
	return d
}

// source maps a source metric name to its dependency ID. A source the
// metric has no recorded dependency on has no prefetched values; it is an
// error, never a read of nothing.
func (r *dimReads) source(metric string) (string, *formula.FormulaError) {
	depID, ok := r.byName[strings.ToUpper(metric)]
	if !ok || r.fetch[depID] == nil {
		return "", &formula.FormulaError{Code: formula.ErrRef.Code,
			Message: fmt.Sprintf("%s is not a recorded dependency of this metric — save the formula again", metric)}
	}
	return depID, nil
}

func (r *dimReads) relates(depID, dimID string) bool {
	k := [2]string{depID, dimID}
	if v, ok := r.related[k]; ok {
		return v
	}
	v := false
	for _, own := range r.metricDims[depID] {
		if rollup.Relates(r.meta.dims, own, dimID) {
			v = true
			break
		}
	}
	r.related[k] = v
	return v
}

// resolve reads metric at combo with overrides (dimension name -> member
// code) applied (contract C2): member check, NormalizeCombo, then
// ResolveTime with the source's own rules.
func (r *dimReads) resolve(combo map[string]string, metric string, overrides map[string]string) (float64, bool, *formula.FormulaError) {
	depID, ferr := r.source(metric)
	if ferr != nil {
		return 0, false, ferr
	}
	byID := make(map[string]string, len(overrides))
	for dim, code := range overrides {
		id, ferr := r.meta.dimID(dim, combo)
		if ferr != nil {
			return 0, false, ferr
		}
		if code == "" {
			return 0, false, formula.MemberNotAvailable(fmt.Sprintf("the %s member is blank", dim))
		}
		if r.meta.member(id, code) == nil {
			return 0, false, formula.MemberNotAvailable(fmt.Sprintf("%s has no member %q", dim, code))
		}
		if !r.relates(depID, id) {
			// NormalizeCombo would drop the override and answer the
			// source's value everywhere — a LOOKUP ignoring its member, a
			// SUMIFS multiplying one value by the member count.
			return 0, false, &formula.FormulaError{Code: formula.CodeDimensionNotOnSource,
				Message: fmt.Sprintf("%s is not dimensioned by %s or by a dimension related to it", metric, dim)}
		}
		byID[id] = code
	}
	srcDims := r.metricDims[depID]
	norm, err := rollup.NormalizeCombo(r.meta.dims, srcDims, combo, byID)
	if err != nil {
		if errors.Is(err, rollup.ErrConflictingOverrides) {
			return 0, false, &formula.FormulaError{Code: formula.CodeConflictingDimensions,
				Message: fmt.Sprintf("reading %s: two dimension arguments select members of the same dimension of %s, which one read cannot express", metric, metric)}
		}
		return 0, false, &formula.FormulaError{Code: formula.ErrRef.Code, Message: fmt.Sprintf("reading %s: %v", metric, err)}
	}
	def := r.defs[depID]
	v, found, handled, err := r.rows[depID].at(srcDims, norm)
	if !handled {
		v, found, err = rollup.ResolveTime(r.ctx, r.meta.dims, depID, srcDims, rollup.AggRule(def.AggRule),
			rollup.TimeSummaryRule(def.TimeSummary), norm, r.fetch[depID])
	}
	if err != nil {
		return 0, false, &formula.FormulaError{Code: formula.ErrRef.Code, Message: fmt.Sprintf("reading %s: %v", metric, err)}
	}
	if found && v != 0 {
		r.data = true
	}
	return v, found, nil
}

// plainReferences returns the UPPER-CASE identifiers node reads as plain
// values at the cell — every identifier except the source and dimension
// arguments of the dimensional functions, the source of the *VALUE family
// and PARENT's dimension. Those are read through DimEvalContext and
// TimeEvalContext.Summarize instead, never bound as a variable.
func plainReferences(node formula.Node, out map[string]bool) {
	switch n := node.(type) {
	case *formula.Ident:
		out[strings.ToUpper(n.Name)] = true
	case *formula.UnaryExpr:
		plainReferences(n.Expr, out)
	case *formula.BinaryExpr:
		plainReferences(n.Left, out)
		plainReferences(n.Right, out)
	case *formula.CallExpr:
		for i, a := range n.Args {
			if !nonValueArg(n.Name, i) {
				plainReferences(a, out)
			}
		}
	}
}

// nonValueArg reports whether argument i of fn names a metric or a
// dimension rather than being evaluated as a value.
func nonValueArg(fn string, i int) bool {
	switch strings.ToUpper(fn) {
	case "LOOKUP", "SUMIFS", "AVERAGEIFS", "MINIFS", "MAXIFS":
		return i == 0 || i%2 == 1 // source, then dimension/range at odd positions
	case "COUNTIFS":
		return i%2 == 0 // ranges
	case "COUNTIF":
		return i == 0
	case "SUMIF", "AVERAGEIF":
		return i == 0 || i == 2 // range, source
	case "YEARVALUE", "HALFYEARVALUE", "QUARTERVALUE", "MONTHVALUE", "PARENT":
		return i == 0
	}
	return false
}

// ── Dependency reads ────────────────────────────────────────────────────────

// exactRows returns the persisted rows of dependency dep (valueMap, as
// LoadCalcValueMap returns them) when they are the only true source of its
// value above the leaves: a CALCULATED metric whose agg_rule is formula or
// rate. Its value at a parent member, a one-dimension slice, an aggregate
// period or the total is the formula re-evaluated there — which the
// scheduler persisted when it computed dep — and never the average of its
// children that rollup.Resolve falls back to for those rules. nil for any
// other dependency: its rows above the leaves combine the leaves, which
// rollup.ResolveTime reproduces.
func exactRows(dep *MetricDef, valueMap map[string]float64) map[string]float64 {
	if dep == nil || dep.IsInput {
		return nil
	}
	if dep.AggRule == string(rollup.AggFormula) || dep.AggRule == string(rollup.AggRate) {
		return valueMap
	}
	return nil
}

// resolveDependency reads dependency dep at the cell combo — the read of a
// plain reference: a formula/rate calculated dependency through its
// exactSource (its persisted row, else its formula evaluated there),
// otherwise rollup.ResolveTime with dep's own rules over its prefetched
// values.
func resolveDependency(ctx context.Context, dims map[string]*rollup.Dimension, dep *MetricDef, srcDims []string,
	combo map[string]string, fetch rollup.RawValue, src *exactSource,
) (float64, bool, error) {
	if src != nil {
		if norm, err := rollup.NormalizeCombo(dims, srcDims, combo, nil); err == nil {
			if v, ok, handled, err := src.at(srcDims, norm); handled {
				return v, ok, err
			}
		}
	}
	return rollup.ResolveTime(ctx, dims, dep.ID, srcDims, rollup.AggRule(dep.AggRule),
		rollup.TimeSummaryRule(dep.TimeSummary), combo, fetch)
}

// Package readset decides which persisted calculated values a restricted
// viewer may be shown (FORMULA_CALCULATION_INSTRUCTIONS.md, contracts C6
// and C7).
//
// The calculation scheduler computes every metric over ALL members, with no
// notion of who will look. A metric that reads other members — LOOKUP, the
// *IFS/*IF family, a time window, a coarser grain than its inputs — is
// served from those persisted rows on a scoped read (C6), never
// re-evaluated over the viewer's visible data. A persisted cell is therefore
// only safe to show when nothing it read was hidden from the viewer: its
// READ SET must not touch a hidden member. When it does, the cell is
// withheld — absent, and never recomputed over the visible members.
//
// The read set is static per metric: derived from the formula AST, the
// metric's dimensions and, transitively, the calculated metrics it reads.
// Each metric reached gets one access spec per dimension (Own, Window,
// Literal, Member-local, All — see spec below), evaluated per cell against
// UNFILTERED dimension metadata, so a hidden member that a criterion
// matches is seen. The gateway's scoped grid read and chart-data both use
// it, so the two apply one rule.
package readset

import (
	"sort"
	"strconv"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/rollup"
)

// Metric is one metric of the revision as the read set needs it.
type Metric struct {
	ID      string
	Name    string
	IsInput bool
	Formula string
	// Dims are the metric's own dimension IDs.
	Dims []string
}

// Served reports whether a calculated metric's formula makes it served from
// persisted rows on every scoped read (C6): it calls a time function, or
// LOOKUP, the *IFS/*IF family, a dynamic offset, the *VALUE family or
// TIMESUM. A formula that does not analyse is not served: it never
// computed, and the ordinary recompute reports its error.
func Served(formulaText string) bool {
	if strings.TrimSpace(formulaText) == "" {
		return false
	}
	an, err := formula.Analyze(formulaText)
	return err == nil && (an.UsesTimeSeries || an.ServedFromRows)
}

// Limits on the static walk. A footprint that exceeds them is unknown, and
// an unknown footprint withholds every cell of a restricted viewer —
// fail-closed, never a guess.
const (
	maxDepth      = 32
	maxReads      = 5000
	maxHostCombos = 256
)

// kind is how one dimension of one read is accessed.
type kind int

const (
	// own: the cell's member of this very dimension — its leaves when it is
	// an aggregate, every leaf when the cell does not pin the dimension.
	kindOwn kind = iota
	// lit: fixed members (a literal LOOKUP member, a TIMESUM range, the
	// matches of literal-only criteria), expanded to their leaves.
	kindLit
	// local: a member or criterion expression of member metadata only,
	// evaluated per cell from unfiltered metadata.
	kindLocal
	// all: every leaf.
	kindAll
)

// spec is one dimension's access by one read. On a time dimension the
// members it resolves to are then widened by the window [min, max]
// (unbounded ends reach the first or last period).
type spec struct {
	kind  kind
	codes []string // lit: member codes; with span, the first and last period of a range
	span  bool     // lit on a time dimension: codes[0]..codes[1], an inclusive TIMESUM range
	loc   *local

	min, max           int
	unbPast, unbFuture bool
}

func (s spec) key() string {
	var b strings.Builder
	b.WriteString(strconv.Itoa(int(s.kind)))
	b.WriteByte('|')
	b.WriteString(strings.Join(s.codes, ","))
	if s.span {
		b.WriteString("|span")
	}
	if s.loc != nil {
		b.WriteString("|loc")
		b.WriteString(strconv.Itoa(s.loc.id))
	}
	b.WriteString("|" + strconv.Itoa(s.min) + ":" + strconv.Itoa(s.max))
	if s.unbPast {
		b.WriteString("<")
	}
	if s.unbFuture {
		b.WriteString(">")
	}
	return b.String()
}

// local is a member-local expression and where to evaluate it: in the
// formula of host, at host's combo, which pins hostOwn from the cell and
// leaves every other dimension unpinned. A time dimension of hostOwn is
// taken at the cell's period moved by wins[d] — the window the expression
// sits in (PREVIOUS(LOOKUP(x, period, period)) evaluates its member at the
// previous period, as the scheduler rebinds the shifted cell); with no
// entry, at the cell's own period.
type local struct {
	id       int
	hostOwn  []string
	wins     map[string]window
	dim      string // the argument's dimension, as written
	member   *formula.DimensionalArg
	criteria []formula.DimensionalArg
}

// read is one metric (or, for a dimensional call's own arguments, one set
// of dimensions) read, with one spec per dimension.
type read struct {
	dims  []string
	specs []spec
}

// footprint is a metric's static read set.
type footprint struct {
	reads []read
	// cellDims are the dimensions whose spec depends on the cell — the memo
	// key of Withheld.
	cellDims []string
	// cyclePast, cycleFuture: a calculated reference closes a loop (an
	// opening/closing recurrence) that moves its reads back (forward) in
	// time on every turn; every time window of the footprint then reaches
	// the first (last) period.
	cyclePast, cycleFuture bool
	// unknown: the walk failed or exceeded its limits.
	unknown bool
}

// Set answers Withheld for one viewer over one revision. It is not safe
// for concurrent use.
type Set struct {
	dims     map[string]*rollup.Dimension
	meta     *calculation.DimMetadata
	hidden   map[string]map[string]bool
	byID     map[string]*Metric
	byName   map[string]*Metric   // UPPER(name) -> metric
	dimsByNm map[string][]string  // UPPER(dimension name) -> dimension IDs
	nameOf   map[string]string    // dimension ID -> name
	parsed   map[string]parsedUse // metric ID -> its reads
	prints   map[string]*footprint
	memo     map[string]bool
	leaves   map[string][]string       // dimension ID -> leaf codes (time: chronological)
	pos      map[string]map[string]int // time dimension ID -> leaf code -> position
	subtree  map[[2]string][]string    // (dimension ID, code) -> leaf descendants
	staticCr map[string]staticCriteria // literal-criteria matches, by local id
	nextLoc  int
}

type parsedUse struct {
	uses []use
	err  error
}

type staticCriteria struct {
	codes []string
	ok    bool
}

// New prepares the read sets of metrics for a viewer whose hidden members
// are hidden (dimension ID -> member code; subtree-closed and cascaded, as
// the gateway's hiddenCodesByDim builds it). dims must be UNFILTERED — every
// member, hidden ones included — and meta built over the same dims
// (calculation.NewDimMetadata); dimIDToName names every dimension of dims.
// metrics is the whole revision, hidden metrics included: a read reaches
// them whatever the viewer may see.
func New(dims map[string]*rollup.Dimension, dimIDToName map[string]string, meta *calculation.DimMetadata,
	metrics []Metric, hidden map[string]map[string]bool) *Set {
	s := &Set{
		dims:     dims,
		meta:     meta,
		hidden:   map[string]map[string]bool{},
		byID:     make(map[string]*Metric, len(metrics)),
		byName:   make(map[string]*Metric, len(metrics)),
		dimsByNm: map[string][]string{},
		nameOf:   dimIDToName,
		parsed:   map[string]parsedUse{},
		prints:   map[string]*footprint{},
		memo:     map[string]bool{},
		leaves:   map[string][]string{},
		pos:      map[string]map[string]int{},
		subtree:  map[[2]string][]string{},
		staticCr: map[string]staticCriteria{},
	}
	for d, codes := range hidden {
		if len(codes) > 0 {
			s.hidden[d] = codes
		}
	}
	for i := range metrics {
		m := &metrics[i]
		s.byID[m.ID] = m
		if _, dup := s.byName[strings.ToUpper(m.Name)]; !dup {
			s.byName[strings.ToUpper(m.Name)] = m
		}
	}
	for id, name := range dimIDToName {
		key := strings.ToUpper(name)
		s.dimsByNm[key] = append(s.dimsByNm[key], id)
	}
	for _, ids := range s.dimsByNm {
		sort.Strings(ids)
	}
	return s
}

// Restricted reports whether anything can be withheld: a nil Set, or one
// whose viewer has no hidden member, withholds nothing.
func (s *Set) Restricted() bool {
	return s != nil && len(s.hidden) > 0
}

// Withheld reports whether metricID's persisted value at combo (dimension
// ID -> member code; a dimension of the metric absent from combo is
// aggregated over, as in the '{}' total) reads a member hidden from the
// viewer — directly, through a window, a literal or computed member, a
// criterion, a coarser grain, or any calculated metric it reads. A metric
// the Set does not know is withheld whenever anything is hidden.
func (s *Set) Withheld(metricID string, combo map[string]string) bool {
	if !s.Restricted() {
		return false
	}
	fp := s.footprint(metricID)
	if fp == nil || fp.unknown {
		return true
	}
	var key strings.Builder
	key.WriteString(metricID)
	for _, d := range fp.cellDims {
		key.WriteByte('\x1f')
		if code, ok := combo[d]; ok {
			key.WriteString(code)
		} else {
			key.WriteByte('\x1e')
		}
	}
	k := key.String()
	if v, ok := s.memo[k]; ok {
		return v
	}
	v := s.evaluate(fp, combo)
	s.memo[k] = v
	return v
}

func (s *Set) evaluate(fp *footprint, combo map[string]string) bool {
	for _, r := range fp.reads {
		for i, d := range r.dims {
			if s.touches(d, r.specs[i], combo, fp) {
				return true
			}
		}
	}
	return false
}

// ── Footprints ──────────────────────────────────────────────────────────────

func (s *Set) isTime(dimID string) bool {
	d := s.dims[dimID]
	return d != nil && d.IsTime
}

func (s *Set) usesOf(m *Metric) ([]use, error) {
	if p, ok := s.parsed[m.ID]; ok {
		return p.uses, p.err
	}
	node, err := formula.Parse(m.Formula)
	var us []use
	if err == nil {
		us, err = uses(node)
	}
	s.parsed[m.ID] = parsedUse{uses: us, err: err}
	return us, err
}

func (s *Set) footprint(metricID string) *footprint {
	if fp, ok := s.prints[metricID]; ok {
		return fp
	}
	m := s.byID[metricID]
	if m == nil {
		s.prints[metricID] = nil
		return nil
	}
	fp := &footprint{}
	b := &builder{s: s, fp: fp, seen: map[string]bool{}, path: map[string]map[string]spec{}}
	root := make(map[string]spec, len(m.Dims))
	for _, d := range m.Dims {
		root[d] = spec{kind: kindOwn}
	}
	b.visit(m, root, 0)
	cell := map[string]bool{}
	for _, r := range fp.reads {
		for i, sp := range r.specs {
			switch sp.kind {
			case kindOwn:
				cell[r.dims[i]] = true
			case kindLocal:
				for _, d := range sp.loc.hostOwn {
					cell[d] = true
				}
			case kindLit, kindAll:
			}
		}
	}
	for d := range cell {
		fp.cellDims = append(fp.cellDims, d)
	}
	sort.Strings(fp.cellDims)
	s.prints[metricID] = fp
	return fp
}

type builder struct {
	s    *Set
	fp   *footprint
	seen map[string]bool
	// path holds, for each metric on the current chain of references, the
	// specs it is being read with — a reference back to one of them closes
	// a recurrence.
	path map[string]map[string]spec
}

// add records a read of dims with specs, unless an identical read is
// already recorded, and returns its key.
func (b *builder) add(dims []string, specs map[string]spec) string {
	r := read{dims: dims, specs: make([]spec, len(dims))}
	var key strings.Builder
	for i, d := range dims {
		r.specs[i] = specs[d]
		key.WriteString(d)
		key.WriteByte('=')
		key.WriteString(r.specs[i].key())
		key.WriteByte(';')
	}
	k := key.String()
	if !b.seen["read\x00"+k] {
		b.seen["read\x00"+k] = true
		b.fp.reads = append(b.fp.reads, r)
	}
	return k
}

// visit records metric x read with specs sx, then follows every read x's
// formula makes. A metric already visited with the same specs reads the
// same members again and is not followed twice.
func (b *builder) visit(x *Metric, sx map[string]spec, depth int) {
	key := "visit\x00" + x.ID + "\x00" + b.add(x.Dims, sx)
	if b.seen[key] {
		return
	}
	b.seen[key] = true
	if depth > maxDepth || len(b.fp.reads) > maxReads {
		b.fp.unknown = true
		return
	}
	if x.IsInput || strings.TrimSpace(x.Formula) == "" {
		return
	}
	us, err := b.s.usesOf(x)
	if err != nil {
		b.fp.unknown = true
		return
	}
	prev, had := b.path[x.ID]
	b.path[x.ID] = sx
	defer func() {
		if had {
			b.path[x.ID] = prev
		} else {
			delete(b.path, x.ID)
		}
	}()
	for _, u := range us {
		b.follow(x, sx, u, depth)
		if b.fp.unknown {
			return
		}
	}
}

// follow maps one read u of x's formula onto the read metric's dimensions.
func (b *builder) follow(x *Metric, sx map[string]spec, u use, depth int) {
	s := b.s
	// The dimension arguments themselves: the members a LOOKUP names or the
	// criteria match are read whether or not the source carries that very
	// dimension (a related one is resolved through it), so a hidden one
	// withholds the cell.
	argSpecs := map[string]spec{}
	var argDims []string
	for _, a := range u.args {
		for _, id := range s.dimsByNm[strings.ToUpper(a.dim)] {
			if _, done := argSpecs[id]; done {
				continue
			}
			argSpecs[id] = b.argSpec(x, sx, a, u.win)
			argDims = append(argDims, id)
		}
	}
	if len(argDims) > 0 {
		b.add(argDims, argSpecs)
	}
	// A criteria range that names a metric (SUMIFS(sales, act_region,
	// region)) is read at every leaf combination of its dimensions.
	for _, a := range u.args {
		if len(s.dimsByNm[strings.ToUpper(a.dim)]) > 0 {
			continue
		}
		if rm := s.byName[strings.ToUpper(a.dim)]; rm != nil {
			all := make(map[string]spec, len(rm.Dims))
			for _, e := range rm.Dims {
				all[e] = spec{kind: kindAll}
			}
			b.visit(rm, all, depth+1)
			if b.fp.unknown {
				return
			}
		}
	}
	r := s.byName[strings.ToUpper(u.source)]
	if u.source == "" || r == nil {
		return // COUNTIFS (member-only), or a bare dimension name
	}
	sr := make(map[string]spec, len(r.Dims))
	for _, e := range r.Dims {
		sr[e] = b.mapSpec(x, sx, u, argSpecs, e)
	}
	if on, ok := b.path[r.ID]; ok && b.closeLoop(r, on, sr) {
		return
	}
	b.visit(r, sr, depth+1)
}

// closeLoop handles a reference back to r, which is on the current chain
// with specs on and is now read again with specs sr: a recurrence
// (opening/closing balances). When the loop only moves time — every other
// dimension is read exactly as before — each further turn repeats the
// reads already recorded under r, shifted the same way again, so the
// footprint's time windows are extended in the direction the loop moves
// (a backward loop reaches the first period, never the future) and the
// loop is closed. Otherwise it returns false, and r is visited with sr: a
// loop whose specs never settle runs into maxDepth and makes the
// footprint unknown — fail-closed.
func (b *builder) closeLoop(r *Metric, on, sr map[string]spec) bool {
	past, future := false, false
	for _, e := range r.Dims {
		p, n := on[e], sr[e]
		if b.s.isTime(e) && p.kind == kindOwn && n.kind == kindOwn {
			past = past || n.min < p.min || (n.unbPast && !p.unbPast)
			future = future || n.max > p.max || (n.unbFuture && !p.unbFuture)
			continue
		}
		if n.key() != p.key() {
			return false
		}
	}
	b.fp.cyclePast = b.fp.cyclePast || past
	b.fp.cycleFuture = b.fp.cycleFuture || future
	return true
}

// mapSpec is the spec of source dimension e for read u made at x's
// coordinates (specs sx).
func (b *builder) mapSpec(x *Metric, sx map[string]spec, u use, argSpecs map[string]spec, e string) spec {
	s := b.s
	if sp, ok := argSpecs[e]; ok {
		return sp // moved to the argument's members; its own window is absolute
	}
	for id := range argSpecs {
		if rollup.Relates(s.dims, e, id) {
			return spec{kind: kindAll} // resolved through a related dimension
		}
	}
	isTime := s.isTime(e)
	if isTime && u.win.abs {
		return spec{kind: kindLit, codes: []string{u.win.absStart, u.win.absEnd}, span: true,
			min: u.win.min, max: u.win.max, unbPast: u.win.unbPast, unbFuture: u.win.unbFuture}
	}
	parent, onCell := sx[e]
	if !onCell || !contains(x.Dims, e) {
		// The reading metric does not carry e: its value aggregated every
		// leaf of e (or resolved e through a relation) — hidden ones
		// included.
		return spec{kind: kindAll}
	}
	if isTime {
		parent.min += u.win.min
		parent.max += u.win.max
		parent.unbPast = parent.unbPast || u.win.unbPast
		parent.unbFuture = parent.unbFuture || u.win.unbFuture
	}
	return parent
}

// argSpec is the spec of one dimension argument of a dimensional call in
// x's formula (x read with specs sx).
//
// uw is the window the call sits in, relative to x's cell: a member-local
// expression is evaluated at every period it reaches.
func (b *builder) argSpec(x *Metric, sx map[string]spec, a dimArg, uw window) spec {
	if a.member != nil {
		switch a.member.Kind {
		case formula.ArgLiteral:
			code, ok := a.member.LiteralCode()
			if !ok {
				return spec{kind: kindAll}
			}
			return spec{kind: kindLit, codes: []string{code}}
		case formula.ArgMemberLocal:
			if host, wins, ok := b.hostOwn(x, sx, a.member.Names, uw); ok {
				return spec{kind: kindLocal, loc: b.newLocal(host, wins, a.dim, a.member, nil)}
			}
		case formula.ArgValueDependent:
		}
		return spec{kind: kindAll}
	}
	literal := true
	var names []string
	for _, c := range a.criteria {
		switch c.Kind {
		case formula.ArgValueDependent:
			return spec{kind: kindAll}
		case formula.ArgMemberLocal:
			literal = false
			names = append(names, c.Names...)
		case formula.ArgLiteral:
		}
	}
	if literal {
		loc := b.newLocal([]string{}, nil, a.dim, nil, a.criteria)
		codes, ok := b.s.criteriaMatches(loc, map[string]string{})
		if !ok {
			return spec{kind: kindAll}
		}
		return spec{kind: kindLit, codes: codes}
	}
	if host, wins, ok := b.hostOwn(x, sx, names, uw); ok {
		return spec{kind: kindLocal, loc: b.newLocal(host, wins, a.dim, nil, a.criteria)}
	}
	return spec{kind: kindAll}
}

func (b *builder) newLocal(hostOwn []string, wins map[string]window, dim string, member *formula.DimensionalArg, criteria []formula.DimensionalArg) *local {
	b.s.nextLoc++
	return &local{id: b.s.nextLoc, hostOwn: hostOwn, wins: wins, dim: dim, member: member, criteria: criteria}
}

// hostOwn returns the dimensions of x a member-local expression reading
// names needs pinned from the cell and, for a time dimension among them,
// the window its period is taken at (x's own window on it composed with
// uw, the window the expression sits in inside x's formula; uw's absolute
// TIMESUM range replaces it). ok is false when the expression is not
// member-local here: a name that is not a dimension (a metric's value), or
// a dimension of x that x is not read at the cell's own member of (time:
// its own member moved by a window).
func (b *builder) hostOwn(x *Metric, sx map[string]spec, names []string, uw window) (host []string, wins map[string]window, ok bool) {
	host = []string{}
	for _, name := range names {
		ids := b.s.dimsByNm[strings.ToUpper(name)]
		if len(ids) == 0 {
			return nil, nil, false
		}
		for _, id := range ids {
			if !contains(x.Dims, id) {
				continue // unpinned where x is evaluated: blank, not a read
			}
			if contains(host, id) {
				continue
			}
			sp := sx[id]
			if b.s.isTime(id) {
				w := uw
				if !uw.abs {
					if sp.kind != kindOwn {
						return nil, nil, false
					}
					w = window{min: sp.min, max: sp.max, unbPast: sp.unbPast, unbFuture: sp.unbFuture}.
						widen(uw.min, uw.max, uw.unbPast, uw.unbFuture)
				}
				if !w.trivial() {
					if wins == nil {
						wins = map[string]window{}
					}
					wins[id] = w
				}
			} else if sp.kind != kindOwn || sp.min != 0 || sp.max != 0 || sp.unbPast || sp.unbFuture {
				return nil, nil, false
			}
			host = append(host, id)
		}
	}
	return host, wins, true
}

// ── Per-cell evaluation ─────────────────────────────────────────────────────

func (s *Set) leafCodes(dimID string) []string {
	if l, ok := s.leaves[dimID]; ok {
		return l
	}
	d := s.dims[dimID]
	var codes []string
	if d != nil {
		codes = rollup.LeafCodes(d)
		if d.IsTime {
			idx := map[string]int{}
			for _, m := range d.Members {
				idx[m.Code] = m.TimeIndex
			}
			sort.SliceStable(codes, func(i, j int) bool { return idx[codes[i]] < idx[codes[j]] })
			p := make(map[string]int, len(codes))
			for i, c := range codes {
				p[c] = i
			}
			s.pos[dimID] = p
		}
	}
	s.leaves[dimID] = codes
	return codes
}

// leavesUnder returns the leaves at or under code; nil when code is not a
// member of the dimension.
func (s *Set) leavesUnder(dimID, code string) []string {
	k := [2]string{dimID, code}
	if l, ok := s.subtree[k]; ok {
		return l
	}
	l := rollup.LeafDescendants(s.dims[dimID], code)
	s.subtree[k] = l
	return l
}

// touches reports whether spec sp of dimension d, at the cell combo, reads
// a member hidden from the viewer.
func (s *Set) touches(d string, sp spec, combo map[string]string, fp *footprint) bool {
	hidden := s.hidden[d]
	if len(hidden) == 0 {
		return false
	}
	all := s.leafCodes(d)
	var codes []string
	switch sp.kind {
	case kindOwn:
		code, pinned := combo[d]
		if !pinned {
			codes = all
		} else if codes = s.leavesUnder(d, code); codes == nil {
			codes = all // a code the dimension does not have: assume the worst
		}
	case kindLit:
		if sp.span {
			codes = s.span(d, sp.codes[0], sp.codes[1])
			break
		}
		for _, c := range sp.codes {
			codes = append(codes, s.leavesUnder(d, c)...)
		}
	case kindLocal:
		got, ok := s.evalLocal(sp.loc, combo, fp)
		if !ok {
			codes = all
			break
		}
		for _, c := range got {
			codes = append(codes, s.leavesUnder(d, c)...)
		}
	case kindAll:
		codes = all
	}
	if s.isTime(d) {
		sp.unbPast = sp.unbPast || fp.cyclePast
		sp.unbFuture = sp.unbFuture || fp.cycleFuture
		if sp.min != 0 || sp.max != 0 || sp.unbPast || sp.unbFuture {
			return s.windowTouches(d, codes, sp, hidden)
		}
	}
	for _, c := range codes {
		if hidden[c] {
			return true
		}
	}
	return false
}

// span returns the leaf periods from start's first leaf to end's last
// (TIMESUM's range): empty when start comes after end or either is
// unknown.
func (s *Set) span(d, start, end string) []string {
	all := s.leafCodes(d)
	first := s.leavesUnder(d, start)
	last := s.leavesUnder(d, end)
	if len(first) == 0 || len(last) == 0 {
		return nil
	}
	pos := s.pos[d]
	lo, hi := pos[first[0]], pos[last[0]]
	for _, c := range first {
		if pos[c] < lo {
			lo = pos[c]
		}
	}
	for _, c := range last {
		if pos[c] > hi {
			hi = pos[c]
		}
	}
	if lo > hi {
		return nil
	}
	return all[lo : hi+1]
}

// windowTouches widens each period in codes by the spec's window and
// reports whether any period reached is hidden.
func (s *Set) windowTouches(d string, codes []string, sp spec, hidden map[string]bool) bool {
	all := s.leafCodes(d)
	pos := s.pos[d]
	n := len(all)
	for _, c := range codes {
		p, ok := pos[c]
		if !ok {
			continue
		}
		lo, hi := p+sp.min, p+sp.max
		if sp.unbPast {
			lo = 0
		}
		if sp.unbFuture {
			hi = n - 1
		}
		if lo < 0 {
			lo = 0
		}
		if hi > n-1 {
			hi = n - 1
		}
		for i := lo; i <= hi; i++ {
			if hidden[all[i]] {
				return true
			}
		}
	}
	return false
}

// evalLocal evaluates a member-local expression for the cell: at every
// combo of host it can be evaluated at — each hostOwn dimension at the
// cell's member and at every leaf under it (an aggregate cell combines its
// leaves; a formula metric's rollup row evaluates at the aggregate itself),
// and a windowed time dimension at every leaf period its window reaches
// from there (a recurrence of fp extends it as for every other read).
// ok is false when it cannot be evaluated (the caller then reads every
// leaf).
func (s *Set) evalLocal(loc *local, combo map[string]string, fp *footprint) ([]string, bool) {
	hostCombos := []map[string]string{{}}
	for _, d := range loc.hostOwn {
		var options []string
		w, windowed := loc.wins[d]
		if s.isTime(d) && (fp.cyclePast || fp.cycleFuture) {
			w.unbPast = w.unbPast || fp.cyclePast
			w.unbFuture = w.unbFuture || fp.cycleFuture
			windowed = true
		}
		if windowed {
			options = s.windowPeriods(d, w, combo)
			if _, pinned := combo[d]; !pinned {
				options = append(options, "")
			}
		} else if code, pinned := combo[d]; pinned {
			options = append(options, code)
			for _, l := range s.leavesUnder(d, code) {
				if l != code {
					options = append(options, l)
				}
			}
		} else {
			options = append(options, "")
			options = append(options, s.leafCodes(d)...)
		}
		if len(hostCombos)*len(options) > maxHostCombos {
			return nil, false
		}
		next := make([]map[string]string, 0, len(hostCombos)*len(options))
		for _, hc := range hostCombos {
			for _, o := range options {
				c := make(map[string]string, len(hc)+1)
				for k, v := range hc {
					c[k] = v
				}
				if o != "" {
					c[d] = o
				}
				next = append(next, c)
			}
		}
		hostCombos = next
	}
	var out []string
	for _, hc := range hostCombos {
		if loc.member != nil {
			v := formula.EvalNode(&formula.EvalContext{Vars: s.vars(hc), Dim: s.meta.CellContext(hc)}, loc.member.Expr)
			if v.IsError() {
				return nil, false
			}
			if code, ok := (formula.DimensionalArg{Kind: formula.ArgLiteral, Value: v}).LiteralCode(); ok {
				out = append(out, code)
			}
			continue // a blank member is #N/A: nothing read
		}
		codes, ok := s.criteriaMatches(loc, hc)
		if !ok {
			return nil, false
		}
		out = append(out, codes...)
	}
	return out, true
}

// windowPeriods returns the leaf periods window w reaches from the cell's
// period of time dimension d (every leaf under it; every leaf when the cell
// does not pin d), or from w's absolute range.
func (s *Set) windowPeriods(d string, w window, combo map[string]string) []string {
	all := s.leafCodes(d)
	var base []string
	if w.abs {
		base = s.span(d, w.absStart, w.absEnd)
	} else if code, pinned := combo[d]; !pinned {
		base = all
	} else if base = s.leavesUnder(d, code); base == nil {
		base = all
	}
	pos := s.pos[d]
	n := len(all)
	reached := make([]bool, n)
	for _, c := range base {
		p, ok := pos[c]
		if !ok {
			continue
		}
		lo, hi := p+w.min, p+w.max
		if w.unbPast || lo < 0 {
			lo = 0
		}
		if w.unbFuture || hi > n-1 {
			hi = n - 1
		}
		for i := lo; i <= hi; i++ {
			reached[i] = true
		}
	}
	var out []string
	for i, r := range reached {
		if r {
			out = append(out, all[i])
		}
	}
	return out
}

// vars binds the host combo's members as the formula's bare dimension
// names, as the scheduler binds them.
func (s *Set) vars(hc map[string]string) map[string]formula.Value {
	v := make(map[string]formula.Value, len(hc))
	for d, code := range hc {
		if name, ok := s.nameOf[d]; ok {
			v[strings.ToUpper(name)] = formula.StringVal(code)
		}
	}
	return v
}

// probeSource names the source of the synthetic conditional aggregation
// criteriaMatches evaluates; it is never resolved as a metric.
const probeSource = "__readset_probe__"

// criteriaMatches returns the leaves of loc's dimension that satisfy every
// one of its criteria, evaluated at host combo hc over UNFILTERED leaves —
// by running the formula package's own conditional aggregation with a
// recording source, so matching can never drift from C3.
func (s *Set) criteriaMatches(loc *local, hc map[string]string) ([]string, bool) {
	literal := len(hc) == 0
	if literal {
		if c, ok := s.staticCr[strconv.Itoa(loc.id)]; ok {
			return c.codes, c.ok
		}
	}
	args := []formula.Node{&formula.Ident{Name: probeSource}}
	for _, c := range loc.criteria {
		var rng formula.Node = &formula.Ident{Name: c.Dim}
		if c.Property != "" {
			rng = &formula.DimProperty{Dim: c.Dim, Property: c.Property}
		}
		args = append(args, rng, c.Expr)
	}
	d := s.meta.CellContext(hc)
	var codes []string
	d.Resolve = func(_ string, overrides map[string]string) (float64, bool, *formula.FormulaError) {
		for _, code := range overrides {
			codes = append(codes, code)
		}
		return 0, false, nil
	}
	d.Memo = nil
	v := formula.EvalNode(&formula.EvalContext{Vars: s.vars(hc), Dim: d}, &formula.CallExpr{Name: "SUMIFS", Args: args})
	ok := !v.IsError()
	if literal {
		s.staticCr[strconv.Itoa(loc.id)] = staticCriteria{codes: codes, ok: ok}
	}
	return codes, ok
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

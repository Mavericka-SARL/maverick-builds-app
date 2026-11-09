package modeltransfer

import (
	"fmt"
)

// packageMember is where a package's member is: its dimension and code.
type packageMember struct {
	dim  *Dimension
	code string
}

// members maps each package member ID to where it is.
func (pkg *Package) members() map[string]packageMember {
	out := map[string]packageMember{}
	for i := range pkg.Dimensions {
		d := &pkg.Dimensions[i]
		for _, m := range d.Members {
			out[m.ID] = packageMember{dim: d, code: m.Code}
		}
	}
	return out
}

// strayParent is m's parent when the import leaves it out: a member of the
// package in neither d, m's dimension, nor d's declared parent dimension —
// where every member edit requires a parent to be (validateMemberParent
// accepts the parent dimension's; an engine export can also carry a
// same-dimension parent, when the parent dimension was declared after the
// members existed). A hand-written mistake, or what a model keeps when its
// developer clears a dimension's parent dimension. The member is imported
// without it, at the top level, its values kept (owner's decision,
// 2026-11-09), and DroppedReferences names it. ok is false for any other
// member, and for a parent the package does not hold (refused).
func strayParent(d *Dimension, m Member, members map[string]packageMember) (parent packageMember, ok bool) {
	if m.ParentMemberID == nil || *m.ParentMemberID == "" {
		return packageMember{}, false
	}
	p, held := members[*m.ParentMemberID]
	if !held || p.dim.ID == d.ID || (d.ParentDimensionID != nil && *d.ParentDimensionID == p.dim.ID) {
		return packageMember{}, false
	}
	return p, true
}

// checkMemberParents refuses a member that is its own ancestor through the
// parents an import keeps, as every member edit refuses a hierarchy cycle.
// A parent the package does not hold is refused where it is resolved.
func (pkg *Package) checkMemberParents() error {
	members := pkg.members()
	parent := map[string]string{}
	var order []string
	for i := range pkg.Dimensions {
		d := &pkg.Dimensions[i]
		for _, m := range d.Members {
			order = append(order, m.ID)
			if m.ParentMemberID == nil || *m.ParentMemberID == "" {
				continue
			}
			if _, held := members[*m.ParentMemberID]; !held {
				continue
			}
			if _, stray := strayParent(d, m, members); stray {
				continue
			}
			parent[m.ID] = *m.ParentMemberID
		}
	}
	// Each member is walked up once: 1 on the walk under way, 2 done. A
	// walk that meets its own path has found a cycle.
	state := make(map[string]int, len(order))
	for _, start := range order {
		var path []string
		id, cycle := start, false
		for {
			if s := state[id]; s != 0 {
				cycle = s == 1
				break
			}
			state[id] = 1
			path = append(path, id)
			next, ok := parent[id]
			if !ok {
				break
			}
			id = next
		}
		if cycle {
			at := members[id]
			return fmt.Errorf("member %q of %q is its own ancestor", at.code, at.dim.Name)
		}
		for _, p := range path {
			state[p] = 2
		}
	}
	return nil
}

// DroppedReferences says, a note per link, what an import of the package
// leaves out: every reference to a row the package does not hold — a typo
// in a hand-edited package, or a row of the exporting model it did not
// carry — and a member's parent outside the member's dimension and its
// parent dimension (strayParent). Each note names where the link was and
// what it named. It reads the package alone: whether a row of that ID
// exists anywhere else is not looked up, so a note tells nothing about rows
// outside the importer's own package. A reference that spells no row ID is
// stored as written by the import, naming nothing, and is named here as
// well. The package's dependencies are not read (an import derives them),
// so they are not named.
func (pkg *Package) DroppedReferences() []string {
	var notes []string
	seen := map[string]bool{}
	note := func(format string, args ...any) {
		if n := fmt.Sprintf(format, args...); !seen[n] {
			seen[n] = true
			notes = append(notes, n)
		}
	}
	set := func() map[string]bool { return map[string]bool{} }
	dims, metrics, grids, forms, folders, integrations := set(), set(), set(), set(), set(), set()
	held := set()
	for _, d := range pkg.Dimensions {
		dims[d.ID], held[d.ID] = true, true
		for _, m := range d.Members {
			held[m.ID] = true
		}
	}
	for _, m := range pkg.Metrics {
		metrics[m.ID], held[m.ID] = true, true
	}
	for _, g := range pkg.Grids {
		grids[g.ID], held[g.ID] = true, true
	}
	for _, f := range pkg.Forms {
		forms[f.ID], held[f.ID] = true, true
	}
	for _, f := range pkg.Folders {
		folders[f.ID], held[f.ID] = true, true
	}
	for _, d := range pkg.Dashboards {
		held[d.ID] = true
	}
	for _, ig := range pkg.Integrations {
		if ig.ID != "" {
			integrations[ig.ID], held[ig.ID] = true, true
		}
	}
	for _, wf := range pkg.Workflows {
		held[wf.ID] = true
	}
	for _, ar := range pkg.AutomationRules {
		if ar.ID != "" {
			held[ar.ID] = true
		}
	}
	// A mapping is imported when its table and its metric are.
	mappings := set()
	mappingNames := map[string]string{}
	var keptMappings []FormMapping
	for _, m := range pkg.FormMappings {
		switch {
		case !forms[m.FormID]:
			note("table mapping %q: table %q is not in the package, so the mapping is left out", m.Name, m.FormID)
		case !metrics[m.TargetMetricID]:
			note("table mapping %q: metric %q is not in the package, so the mapping is left out", m.Name, m.TargetMetricID)
		default:
			keptMappings = append(keptMappings, m)
			if m.ID != "" {
				mappings[m.ID], held[m.ID] = true, true
			}
			continue
		}
		mappingNames[m.ID] = m.Name
	}

	members := pkg.members()
	for i := range pkg.Dimensions {
		d := &pkg.Dimensions[i]
		for _, m := range d.Members {
			if p, stray := strayParent(d, m, members); stray {
				note("dimension %q, member %q: its parent %q is a member of %q, which is neither %q nor its parent dimension, so the member is imported without a parent",
					d.Name, m.Code, p.code, p.dim.Name, d.Name)
			}
		}
	}
	for _, g := range pkg.Grids {
		for _, gm := range g.Metrics {
			if !metrics[gm.MetricID] {
				note("grid %q: metric %q is not in the package, so the grid leaves it out", g.Name, gm.MetricID)
			}
		}
		for _, gd := range g.Dimensions {
			if !dims[gd.DimensionID] {
				note("grid %q: dimension %q is not in the package, so the grid leaves it out", g.Name, gd.DimensionID)
			}
		}
	}
	for _, f := range pkg.Forms {
		if f.KeepsDimensionID != "" && !dims[f.KeepsDimensionID] {
			note("table %q: dimension %q is not in the package, so the table keeps no dimension's members", f.Name, f.KeepsDimensionID)
		}
	}
	tally(pkg.FormRecords, func(r FormRecord) (string, bool) { return r.FormID, !forms[r.FormID] }, func(form string, n int) {
		note("%s of table %q, which is not in the package, %s left out", count(n, "record"), form, isAre(n))
	})
	for _, d := range pkg.Dashboards {
		if d.FolderID != nil && *d.FolderID != "" && !folders[*d.FolderID] {
			note("dashboard %q: folder %q is not in the package, so the dashboard is outside any folder", d.Name, *d.FolderID)
		}
	}
	for _, ar := range pkg.AutomationRules {
		if ar.SourceIntegrationID != nil && *ar.SourceIntegrationID != "" && !integrations[*ar.SourceIntegrationID] {
			note("automation rule %q: integration %q is not in the package, so the rule arrives switched off", ar.Name, *ar.SourceIntegrationID)
		}
	}

	// The references held without a foreign key, walked as the import
	// resolves them, on the mappings it imports; facts are summed up below.
	walked := *pkg
	walked.FormMappings, walked.Facts = keptMappings, nil
	walked.looseRefs(func(at string) refFunc {
		return func(kind, id string) (string, bool) {
			if id == "" || id == "__metrics__" || held[id] {
				return id, true
			}
			// A widget type whose ref_id is not a row's (widgetRefKinds)
			// may name something: a workflow_status widget's is a
			// workflow's name, which the import keeps as written.
			if _, isID := rowIDOf(id); !isID && kind == "" {
				return id, true
			}
			note("%s: %s %q is not in the package, so it is not linked", at, kindWord(kind), id)
			return id, true
		}
	})

	// Facts: left out with their metric or the mapping that posted them;
	// one keyed by a dimension the package does not hold is kept apart
	// under that key (importRefs.cellDoc).
	tally(pkg.Facts, func(f Fact) (string, bool) { return f.MetricID, !metrics[f.MetricID] }, func(metric string, n int) {
		note("%s of metric %q, which is not in the package, %s left out", count(n, "value"), metric, isAre(n))
	})
	tally(pkg.Facts, func(f Fact) (string, bool) {
		if !metrics[f.MetricID] || f.SourceMappingID == nil {
			return "", false
		}
		return *f.SourceMappingID, !mappings[*f.SourceMappingID]
	}, func(mapping string, n int) {
		if name, ok := mappingNames[mapping]; ok {
			note("%s posted by table mapping %q %s left out with the mapping", count(n, "value"), name, isAre(n))
		} else {
			note("%s posted by table mapping %q, which is not in the package, %s left out", count(n, "value"), mapping, isAre(n))
		}
	})
	var keys []string
	byKey := map[string]int{}
	for _, f := range pkg.Facts {
		if !metrics[f.MetricID] || (f.SourceMappingID != nil && !mappings[*f.SourceMappingID]) {
			continue
		}
		rewriteRefs(f.DimMembers, jsonRefs{keys: true}, func(kind, id string) (string, bool) {
			if kind == "dimension" && !held[id] {
				if byKey[id] == 0 {
					keys = append(keys, id)
				}
				byKey[id]++
			}
			return id, true
		})
	}
	for _, k := range keys {
		n := byKey[k]
		verb := "names"
		if n != 1 {
			verb = "name"
		}
		note("%s %s dimension %q, which is not in the package, so %s kept under a dimension the model does not have",
			count(n, "value"), verb, k, map[bool]string{true: "it is", false: "they are"}[n == 1])
	}
	return notes
}

// tally counts the rows for which key says true, by key, and reports each
// key once, in the order first met.
func tally[T any](rows []T, key func(T) (string, bool), report func(key string, n int)) {
	var order []string
	n := map[string]int{}
	for _, r := range rows {
		k, ok := key(r)
		if !ok {
			continue
		}
		if n[k] == 0 {
			order = append(order, k)
		}
		n[k]++
	}
	for _, k := range order {
		report(k, n[k])
	}
}

func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func isAre(n int) string {
	if n == 1 {
		return "is"
	}
	return "are"
}

// kindWord is how a note names a kind of row (refKind's words).
func kindWord(kind string) string {
	switch kind {
	case "form":
		return "table"
	case "rule":
		return "automation rule"
	case "mapping":
		return "table mapping"
	case "", "target":
		return "row"
	}
	return kind
}

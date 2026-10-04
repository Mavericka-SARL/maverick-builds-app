package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"

	"github.com/mavericks-engine/mavericks/internal/formula"
	"github.com/mavericks-engine/mavericks/internal/rollup"
)

// addCalculatedMemberCells puts a value at each calculated member of every
// metric's dimensions (rollup.CalculatedMember), at every leaf cell of the
// other dimensions that has a sibling value: computed from those siblings'
// cells by the member's formula. Totals are left as they are — a calculated
// member is in no total — and nothing is stored. A cell whose formula reads
// a withheld cell is withheld too, so a restricted viewer learns nothing
// through a variance. The planning grid computes the same at its own totals
// (a FY Variance % from the FY RF and LY), and returns withheld extended.
func addCalculatedMemberCells(cells map[string]float64, withheld []string, metrics []metricRow, metricDims map[string][]string, dims []gridDimension, pinned map[string]string) []string {
	calcByDim := map[string][]gridDimMember{}
	for _, d := range dims {
		if _, ok := pinned[d.ID]; ok {
			continue // pinned to one member: its siblings are not in this read (gridEntry answers a calculated pin)
		}
		for _, m := range d.Members {
			if m.Formula != "" {
				calcByDim[d.ID] = append(calcByDim[d.ID], m)
			}
		}
	}
	if len(calcByDim) == 0 {
		return withheld
	}
	hidden := make(map[string]bool, len(withheld))
	for _, k := range withheld {
		hidden[k] = true
	}
	seen := map[string]bool{}
	for _, m := range metrics {
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		own := metricDims[m.ID]
		for pos, dimID := range own {
			calcs := calcByDim[dimID]
			if len(calcs) == 0 {
				continue
			}
			// The metric's cells grouped by their coordinates on the other
			// dimensions: group key (the codes with this one blanked) → code
			// → cell key.
			prefix := m.ID + ":"
			groups := map[string]map[string]string{}
			for k := range cells {
				if !strings.HasPrefix(k, prefix) {
					continue
				}
				codes := strings.Split(k[len(prefix):], ":")
				if len(codes) != len(own) {
					continue
				}
				code := codes[pos]
				codes[pos] = ""
				g := strings.Join(codes, ":")
				if groups[g] == nil {
					groups[g] = map[string]string{}
				}
				groups[g][strings.ToUpper(code)] = k
			}
			for g, byCode := range groups {
				keyAt := func(code string) string {
					codes := strings.Split(g, ":")
					codes[pos] = code
					return prefix + strings.Join(codes, ":")
				}
				computed := map[string]float64{}
				readsWithheld := map[string]bool{}
				state := map[string]int{} // 1 computing, 2 done
				var valueOf func(code string, withheldRead *bool) (float64, bool)
				valueOf = func(code string, withheldRead *bool) (float64, bool) {
					up := strings.ToUpper(code)
					// A calculated member first: a value stored at its code
					// before it became calculated never wins over the formula.
					for _, c := range calcs {
						if !strings.EqualFold(c.Code, code) {
							continue
						}
						switch state[up] {
						case 2:
							if readsWithheld[up] {
								*withheldRead = true
							}
							v, ok := computed[up]
							return v, ok
						case 1:
							return 0, false // a cycle, refused at save
						}
						state[up] = 1
						own := false
						v, ok, _ := rollup.EvalCalculated(c.Formula, m.Format, func(ref string) (float64, bool, error) {
							v, ok := valueOf(ref, &own)
							return v, ok, nil
						})
						state[up] = 2
						readsWithheld[up] = own
						if own {
							*withheldRead = true
						}
						if ok {
							computed[up] = v
						}
						return v, ok
					}
					if k, ok := byCode[up]; ok {
						if hidden[k] {
							*withheldRead = true
							return 0, false
						}
						v, ok := cells[k]
						return v, ok
					}
					return 0, false
				}
				for _, c := range calcs {
					withheldRead := false
					v, ok := valueOf(c.Code, &withheldRead)
					k := keyAt(c.Code)
					switch {
					case withheldRead:
						delete(cells, k)
						if !hidden[k] {
							hidden[k] = true
							withheld = append(withheld, k)
						}
					case ok:
						cells[k] = v
					default:
						delete(cells, k) // nothing to compute from
					}
				}
			}
		}
	}
	return withheld
}

// gridEntry is GET /api/grid. A scope pinning a calculated member — a KPI
// tile showing the variance, scope {scenario: VAR} — is answered from the
// responses pinned at the members its formula reads ({RF} and {LY}): each
// total and cell is the formula over theirs, so a total is computed from
// totals, never added up from a calculated member's own cells. Any other
// request is grid() itself.
func (h *handler) gridEntry(w http.ResponseWriter, r *http.Request) {
	h.gridPinned(w, r, 0)
}

func (h *handler) gridPinned(w http.ResponseWriter, r *http.Request, depth int) {
	ctx := r.Context()
	var pinned map[string]string
	if raw := r.URL.Query().Get("scope"); raw == "" || json.Unmarshal([]byte(raw), &pinned) != nil || len(pinned) == 0 || depth > 8 {
		h.grid(w, r)
		return
	}
	dimIDs := make([]string, 0, len(pinned))
	for id := range pinned {
		dimIDs = append(dimIDs, id)
	}
	sort.Strings(dimIDs)
	var calcDim, calcCode, text string
	for _, id := range dimIDs {
		var f string
		if h.db.QueryRow(ctx, `SELECT COALESCE(btrim(formula),'') FROM model.dimension_member WHERE dimension_id::text=$1 AND code=$2`, id, pinned[id]).Scan(&f) == nil && f != "" {
			calcDim, calcCode, text = id, pinned[id], f
			break
		}
	}
	if text == "" {
		h.grid(w, r)
		return
	}
	refs, _ := formula.ExtractRefs(text)
	codes := map[string]string{} // upper-cased ref → the member's code
	if rows, err := h.db.Query(ctx, `SELECT code FROM model.dimension_member WHERE dimension_id::text=$1`, calcDim); err == nil {
		for rows.Next() {
			var c string
			if rows.Scan(&c) == nil {
				codes[strings.ToUpper(c)] = c
			}
		}
		rows.Close()
	}
	sibling := map[string]*gridResponse{} // member code → its response
	for _, ref := range refs {
		code, ok := codes[strings.ToUpper(ref)]
		if !ok || sibling[code] != nil {
			continue
		}
		scope := make(map[string]string, len(pinned))
		for k, v := range pinned {
			scope[k] = v
		}
		scope[calcDim] = code
		b, _ := json.Marshal(scope)
		sub := r.Clone(ctx)
		q := sub.URL.Query()
		q.Set("scope", string(b))
		sub.URL.RawQuery = q.Encode()
		rec := httptest.NewRecorder()
		h.gridPinned(rec, sub, depth+1)
		if rec.Code != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rec.Code)
			_, _ = w.Write(rec.Body.Bytes())
			return
		}
		var resp gridResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			jsonErr(w, fmt.Errorf("read the %s response: %w", code, err), http.StatusInternalServerError)
			return
		}
		sibling[code] = &resp
	}
	var out *gridResponse
	for _, ref := range refs {
		if resp := sibling[codes[strings.ToUpper(ref)]]; resp != nil {
			copied := *resp
			out = &copied
			break
		}
	}
	if out == nil {
		h.grid(w, r)
		return
	}
	withheldIn := map[string]map[string]bool{}
	for code, resp := range sibling {
		withheldIn[code] = map[string]bool{}
		for _, k := range resp.Withheld {
			withheldIn[code][k] = true
		}
	}
	// eval is the formula over the siblings' values at keyFor(code), read by
	// read; withheld when one of them is.
	eval := func(format string, keyFor func(code string) string, read func(*gridResponse, string) (float64, bool)) (float64, bool, bool) {
		withheld := false
		v, ok, _ := rollup.EvalCalculated(text, format, func(ref string) (float64, bool, error) {
			code := codes[strings.ToUpper(ref)]
			resp := sibling[code]
			if resp == nil {
				return 0, false, nil
			}
			k := keyFor(code)
			if withheldIn[code][k] {
				withheld = true
				return 0, false, nil
			}
			v, ok := read(resp, k)
			return v, ok, nil
		})
		return v, ok, withheld
	}
	totals := map[string]float64{}
	cells := map[string]float64{}
	var withheld []string
	seen := map[string]bool{}
	for _, m := range append(append([]metricRow{}, out.Metrics...), out.AllMetrics...) {
		if seen[m.ID] {
			continue
		}
		seen[m.ID] = true
		v, ok, wh := eval(m.Format, func(string) string { return m.ID }, func(g *gridResponse, k string) (float64, bool) {
			v, ok := g.Totals[k]
			return v, ok
		})
		switch {
		case wh:
			withheld = append(withheld, m.ID)
		case ok:
			totals[m.ID] = v
		}
		pos := -1
		for i, d := range m.DimensionIDs {
			if d == calcDim {
				pos = i
			}
		}
		if pos < 0 {
			continue
		}
		// Every coordinate a sibling has a cell at, with the calculated member
		// in the pinned dimension's place.
		targets := map[string][]string{}
		prefix := m.ID + ":"
		for code, resp := range sibling {
			for k := range resp.Cells {
				if !strings.HasPrefix(k, prefix) {
					continue
				}
				parts := strings.Split(k[len(prefix):], ":")
				if len(parts) != len(m.DimensionIDs) || parts[pos] != code {
					continue
				}
				parts[pos] = calcCode
				targets[strings.Join(parts, ":")] = parts
			}
		}
		for t, parts := range targets {
			v, ok, wh := eval(m.Format, func(code string) string {
				p := append([]string{}, parts...)
				p[pos] = code
				return prefix + strings.Join(p, ":")
			}, func(g *gridResponse, k string) (float64, bool) {
				v, ok := g.Cells[k]
				return v, ok
			})
			switch {
			case wh:
				withheld = append(withheld, prefix+t)
			case ok:
				cells[prefix+t] = v
			}
		}
	}
	out.Totals, out.Cells, out.Withheld = totals, cells, withheld
	jsonOK(w, out)
}

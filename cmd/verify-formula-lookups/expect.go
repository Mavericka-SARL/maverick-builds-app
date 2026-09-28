package main

import (
	"fmt"
	"math"
)

// The facts the harness writes and the values it expects back, computed here
// from those facts with no help from the engine.

var (
	leafRegions = []string{"DE", "UK", "US"}
	months      = []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
	fxRates     = map[string]float64{"EUR": 1.1, "GBP": 1.25, "USD": 1.0}
	// days in each month of 2026 (not a leap year).
	daysIn2026 = []int{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}
)

// regionProps are the member property values written, by member code. The
// numeric factor is kept as a number here; it is written as text, which is
// how member property values are stored.
type regionProps struct {
	segment  string
	factor   float64
	hasFac   bool
	currency string
}

func initialRegionProps() map[string]*regionProps {
	return map[string]*regionProps{
		"DE":    {segment: "Enterprise", factor: 1.5, hasFac: true, currency: "EUR"},
		"UK":    {segment: "SMB", factor: 2, hasFac: true, currency: "GBP"},
		"US":    {segment: "Enterprise", factor: 3, hasFac: true, currency: "USD"},
		"EMEA":  {factor: 10, hasFac: true, currency: "EUR"},
		"AMER":  {currency: "USD"},
		"World": {},
	}
}

func monthCode(m int) string { return fmt.Sprintf("2026-%02d", m) }

// rev is revenue[region, month].
func rev(r string, m int) float64 {
	switch r {
	case "DE":
		return 100 + 10*float64(m-1)
	case "UK":
		return 50 + 5*float64(m-1)
	case "US":
		return 200 + 20*float64(m-1)
	}
	return 0
}

// lagN is lag_n[region, month], the dynamic LAG offset.
func lagN(r string, m int) float64 {
	switch r {
	case "DE":
		if m%2 == 1 {
			return 1
		}
		return 2
	case "UK":
		return 2
	}
	return 1
}

func world(m int) float64 { return rev("DE", m) + rev("UK", m) + rev("US", m) }

func sumRev(r string, from, to int) float64 {
	s := 0.0
	for m := from; m <= to; m++ {
		s += rev(r, m)
	}
	return s
}

// viewer classes for the restricted business user who cannot see US.
const (
	vPresent  = 'P' // the cell is served with the exact value
	vWithheld = 'W' // the cell is listed in "withheld" and absent from cells
)

type calcDef struct {
	name    string
	formula string
	agg     string // "" = engine default (sum)
	// leaf is the expected value at a leaf (region, month).
	leaf func(env *environment, r string, m int) float64
	// total is the expected grand total for the developer; nil = sum of the
	// leaves.
	total func(env *environment) float64
	// viewer is the expected treatment of the DE and UK cells for the
	// business user who cannot see US: [DE, UK].
	viewer [2]rune
	// viewerTotal: 'S' = sum of the visible leaves, 'W' = withheld, 0 = not
	// asserted.
	viewerTotal rune
}

func calcDefs() []calcDef {
	return []calcDef{
		{name: "rev_factor", formula: "revenue * region.factor",
			leaf:   func(e *environment, r string, m int) float64 { return rev(r, m) * e.props[r].factor },
			viewer: [2]rune{vPresent, vPresent}, viewerTotal: 'S'},
		{name: "parent_emea", formula: `IF(PARENT(region) = "EMEA", revenue, 0)`,
			leaf: func(_ *environment, r string, m int) float64 {
				if r == "US" {
					return 0
				}
				return rev(r, m)
			},
			viewer: [2]rune{vPresent, vPresent}, viewerTotal: 'S'},
		{name: "share_world", formula: `revenue / LOOKUP(revenue, region, "World")`, agg: "formula",
			leaf:   func(_ *environment, r string, m int) float64 { return rev(r, m) / world(m) },
			total:  func(*environment) float64 { return 1 },
			viewer: [2]rune{vWithheld, vWithheld}, viewerTotal: 'W'},
		{name: "de_rev", formula: `LOOKUP(revenue, region, "DE")`,
			leaf:   func(_ *environment, _ string, m int) float64 { return rev("DE", m) },
			viewer: [2]rune{vPresent, vPresent}, viewerTotal: 'S'},
		{name: "fx_region", formula: `LOOKUP(fx_rate, currency, region.currency)`,
			leaf:   func(e *environment, r string, _ int) float64 { return fxRates[e.props[r].currency] },
			viewer: [2]rune{vPresent, vPresent}, viewerTotal: 'S'},
		{name: "rev_usd", formula: `revenue * LOOKUP(fx_rate, currency, region.currency)`,
			leaf:   func(e *environment, r string, m int) float64 { return rev(r, m) * fxRates[e.props[r].currency] },
			viewer: [2]rune{vPresent, vPresent}, viewerTotal: 'S'},
		{name: "ent_rev", formula: `SUMIFS(revenue, region.segment, "Enterprise")`,
			leaf:   func(_ *environment, _ string, m int) float64 { return rev("DE", m) + rev("US", m) },
			viewer: [2]rune{vWithheld, vWithheld}, viewerTotal: 'W'},
		{name: "avg_ent", formula: `AVERAGEIFS(revenue, region.segment, "Enterprise")`,
			leaf:   func(_ *environment, _ string, m int) float64 { return (rev("DE", m) + rev("US", m)) / 2 },
			viewer: [2]rune{vWithheld, vWithheld}, viewerTotal: 'W'},
		{name: "cnt_ent", formula: `COUNTIFS(region.segment, "Enterprise")`,
			leaf:   func(*environment, string, int) float64 { return 2 },
			viewer: [2]rune{vWithheld, vWithheld}, viewerTotal: 'W'},
		{name: "min_ent", formula: `MINIFS(revenue, region.segment, "Enterprise")`,
			leaf:   func(_ *environment, _ string, m int) float64 { return math.Min(rev("DE", m), rev("US", m)) },
			viewer: [2]rune{vWithheld, vWithheld}, viewerTotal: 'W'},
		{name: "max_big", formula: `MAXIFS(revenue, region.factor, ">=2")`,
			leaf: func(e *environment, _ string, m int) float64 {
				best := 0.0
				for _, r := range leafRegions {
					if e.props[r].factor >= 2 && rev(r, m) > best {
						best = rev(r, m)
					}
				}
				return best
			},
			viewer: [2]rune{vWithheld, vWithheld}, viewerTotal: 'W'},
		{name: "sum_u", formula: `SUMIFS(revenue, region, "U*")`,
			leaf:   func(_ *environment, _ string, m int) float64 { return rev("UK", m) + rev("US", m) },
			viewer: [2]rune{vWithheld, vWithheld}, viewerTotal: 'W'},
		{name: "sum_de_code", formula: `SUMIFS(revenue, region, "DE")`,
			leaf:   func(_ *environment, _ string, m int) float64 { return rev("DE", m) },
			viewer: [2]rune{vPresent, vPresent}, viewerTotal: 'S'},
		{name: "sumif_smb", formula: `SUMIF(region.segment, "SMB", revenue)`,
			leaf:   func(_ *environment, _ string, m int) float64 { return rev("UK", m) },
			viewer: [2]rune{vPresent, vPresent}, viewerTotal: 'S'},
		{name: "same_seg", formula: `SUMIFS(revenue, region.segment, region.segment)`,
			leaf: func(e *environment, r string, m int) float64 {
				s := 0.0
				for _, o := range leafRegions {
					if e.props[o].segment == e.props[r].segment {
						s += rev(o, m)
					}
				}
				return s
			},
			viewer: [2]rune{vWithheld, vPresent}, viewerTotal: 'W'},
		{name: "cnt_not_de", formula: `COUNTIFS(region, "<>DE")`,
			leaf:   func(*environment, string, int) float64 { return 2 },
			viewer: [2]rune{vWithheld, vWithheld}, viewerTotal: 'W'},
		{name: "lag_rev", formula: `LAG(revenue, lag_n, 0)`,
			leaf: func(_ *environment, r string, m int) float64 {
				t := m - int(lagN(r, m))
				if t < 1 {
					return 0
				}
				return rev(r, t)
			},
			viewer: [2]rune{vPresent, vPresent}, viewerTotal: 'S'},
		{name: "yv", formula: `YEARVALUE(revenue)`,
			leaf:   func(_ *environment, r string, _ int) float64 { return sumRev(r, 1, 12) },
			viewer: [2]rune{vPresent, vPresent}, viewerTotal: 'S'},
		{name: "qv", formula: `QUARTERVALUE(revenue)`,
			leaf: func(_ *environment, r string, m int) float64 {
				q0 := (m-1)/3*3 + 1
				return sumRev(r, q0, q0+2)
			},
			viewer: [2]rune{vPresent, vPresent}, viewerTotal: 'S'},
		{name: "hytd", formula: `HALFYEARTODATE(revenue)`,
			leaf: func(_ *environment, r string, m int) float64 {
				h0 := 1
				if m > 6 {
					h0 = 7
				}
				return sumRev(r, h0, m)
			},
			viewer: [2]rune{vPresent, vPresent}, viewerTotal: 'S'},
		{name: "ts_q2", formula: `TIMESUM(revenue, "Q2", "Q2")`,
			leaf:   func(_ *environment, r string, _ int) float64 { return sumRev(r, 4, 6) },
			viewer: [2]rune{vPresent, vPresent}, viewerTotal: 'S'},
		{name: "ts_avg", formula: `TIMESUM(revenue, "2026-02", "Q3", AVERAGE)`,
			leaf:   func(_ *environment, r string, _ int) float64 { return sumRev(r, 2, 9) / 8 },
			viewer: [2]rune{vPresent, vPresent}, viewerTotal: 'S'},
		{name: "days", formula: `END() - START() + 1`,
			leaf:   func(_ *environment, _ string, m int) float64 { return float64(daysIn2026[m-1]) },
			viewer: [2]rune{vPresent, vPresent}, viewerTotal: 'S'},
		{name: "dim_fn", formula: `DAYSINMONTH(2026, 2) + DAYSINYEAR(2024)`,
			leaf:   func(*environment, string, int) float64 { return 28 + 366 },
			viewer: [2]rune{vPresent, vPresent}, viewerTotal: 'S'},
		// Bare dimension at a total: region is unpinned there, so it reads
		// blank — never #NAME? (which used to leave no total at all).
		{name: "emea_flag", formula: `IF(region = "EMEA", revenue, 0)`, agg: "formula",
			leaf:   func(*environment, string, int) float64 { return 0 },
			total:  func(*environment) float64 { return 0 },
			viewer: [2]rune{vPresent, vPresent}},
		{name: "is_total", formula: `IF(region = "", revenue, 0)`, agg: "formula",
			leaf: func(*environment, string, int) float64 { return 0 },
			total: func(*environment) float64 {
				s := 0.0
				for _, r := range leafRegions {
					s += sumRev(r, 1, 12)
				}
				return s
			},
			viewer: [2]rune{vPresent, vPresent}},
	}
}

func approx(got, want float64) bool {
	d := math.Abs(got - want)
	return d <= 1e-6 || d <= 1e-9*math.Max(math.Abs(got), math.Abs(want))
}

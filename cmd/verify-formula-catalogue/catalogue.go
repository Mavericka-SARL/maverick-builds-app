package main

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ── facts ──────────────────────────────────────────────────────────────────

var leafRegions = []string{"DE", "UK", "US"}

type regionProps struct {
	parent, segment, currency string
	factor                    float64
	opened                    time.Time
}

var props = map[string]regionProps{
	"DE": {parent: "EMEA", segment: "Enterprise", currency: "EUR", factor: 1.5, opened: date(2019, 4, 1)},
	"UK": {parent: "EMEA", segment: "SMB", currency: "GBP", factor: 2, opened: date(2021, 7, 15)},
	"US": {parent: "AMER", segment: "Enterprise", currency: "USD", factor: 0.5, opened: date(2020, 1, 10)},
}

var fxRates = map[string]float64{"EUR": 1.1, "GBP": 1.25, "USD": 1}

const days = 59 // 2026-01-01 .. 2026-02-28

func date(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC) }
func daysIn(m int) int           { return date(2026, m+1, 0).Day() }
func monthCode(m int) string     { return fmt.Sprintf("2026-%02d", m) }
func dayCode(k int) string       { return date(2026, 1, k).Format("2006-01-02") }

func sales(r string, m int) float64 {
	switch r {
	case "DE":
		return 100 + 10*float64(m)
	case "UK":
		return 50 + 5*float64(m)
	}
	return 200 - 3*float64(m)
}
func cost(r string, m int) float64 { return sales(r, m)/2 + float64(m) }
func units(r string, m int) float64 {
	return float64(m) + map[string]float64{"DE": 2, "UK": 1, "US": 3}[r]
}
func lagN(r string, m int) float64 {
	switch r {
	case "DE":
		return float64(m % 3)
	case "UK":
		return 1
	}
	return float64(m % 2)
}
func flow(r string, m int) float64 {
	switch r {
	case "DE":
		if m%2 == 0 {
			return 10
		}
		return -5
	case "UK":
		return 3
	}
	return -float64(m)
}

// ── expected-value helpers (independent of the engine) ─────────────────────

func round(x float64, p int) float64 {
	f := math.Pow(10, float64(p))
	return math.Round(x*f) / f
}

// sumRange sums f over the months from..to, clipped to 1..12.
func sumRange(f func(int) float64, from, to int) float64 {
	s := 0.0
	for k := max(from, 1); k <= min(to, 12); k++ {
		s += f(k)
	}
	return s
}

func of(r string) func(int) float64 { return func(k int) float64 { return sales(r, k) } }
func quarterStart(m int) int        { return (m-1)/3*3 + 1 }
func halfStart(m int) int           { return (m-1)/6*6 + 1 }
func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func worldSales(m int) float64 { return sales("DE", m) + sales("UK", m) + sales("US", m) }
func parentSales(r string, m int) float64 {
	if props[r].parent == "EMEA" {
		return sales("DE", m) + sales("UK", m)
	}
	return sales("US", m)
}

func closing(r string, m int) float64 {
	c := 100.0
	for k := 1; k <= m; k++ {
		c += flow(r, k)
	}
	return c
}

// ── the catalogue ──────────────────────────────────────────────────────────

type calcDef struct {
	name, formula string
	agg           string // "" = sum
	summary       string // "" = the engine default (sum)
	daily         bool   // on Daily [day] instead of Plan [region, period]
	// first is a placeholder formula to create the metric with, when its
	// real formula names a metric created after it (a recurrence); the
	// real formula is saved once every metric exists.
	first string
	// leaf is the expected value at a Plan leaf; day at a Daily leaf.
	// present false: the cell must be absent (a real formula failure).
	leaf func(r string, m int) (float64, bool)
	day  func(k int) float64
	// total is the expected grand total; nil = the sum of the leaves.
	total func() float64
	// noTotal: the grand total is not asserted.
	noTotal bool
}

func (d calcDef) aggRule() string {
	if d.agg == "" {
		return "sum"
	}
	return d.agg
}

func v(x float64) (float64, bool) { return x, true }

func catalogue() []calcDef {
	return []calcDef{
		// ── Arithmetic, logic and error handling ────────────────────────
		{name: "margin", formula: "sales - cost", leaf: func(r string, m int) (float64, bool) { return v(sales(r, m) - cost(r, m)) }},
		{name: "margin_pct", formula: "IF(sales = 0, 0, ROUND((sales - cost) / sales * 100, 1))", agg: "formula",
			leaf: func(r string, m int) (float64, bool) {
				return v(round((sales(r, m)-cost(r, m))/sales(r, m)*100, 1))
			},
			total: func() float64 {
				var s, c float64
				for _, r := range leafRegions {
					for m := 1; m <= 12; m++ {
						s += sales(r, m)
						c += cost(r, m)
					}
				}
				return round((s-c)/s*100, 1)
			}},
		{name: "price", formula: "ROUND(sales / units, 2)", leaf: func(r string, m int) (float64, bool) { return v(round(sales(r, m)/units(r, m), 2)) }},
		{name: "logic_mix", formula: `IFS(sales > 250, 3, sales > 150, 2, TRUE, 1) + IF(AND(units > 5, OR(region = "UK", region = "us")), 10, 0) + IF(NOT(sales > cost), 100, 0)`,
			leaf: func(r string, m int) (float64, bool) {
				s := sales(r, m)
				x := 1.0
				if s > 250 {
					x = 3
				} else if s > 150 {
					x = 2
				}
				if units(r, m) > 5 && (r == "UK" || r == "US") {
					x += 10
				}
				if !(s > cost(r, m)) {
					x += 100
				}
				return v(x)
			}},
		{name: "err_guard", formula: "IFERROR(sales / (units - units), -1)", leaf: func(string, int) (float64, bool) { return v(-1) }},
		{name: "na_guard", formula: `IFNA(SWITCH(region, "DE", 1, "UK", 2), 0)`,
			leaf: func(r string, _ int) (float64, bool) { return v(map[string]float64{"DE": 1, "UK": 2}[r]) }},
		{name: "err_cell", formula: "sales / (units - units)", noTotal: true,
			leaf: func(string, int) (float64, bool) { return 0, false }},

		// ── Math and rounding ───────────────────────────────────────────
		{name: "math_mix", formula: "ABS(sales - 200) + INT(units / 4) + MOD(units, 5) + POWER(2, MOD(units, 3)) + SQRT(units * units)",
			leaf: func(r string, m int) (float64, bool) {
				u := units(r, m)
				return v(math.Abs(sales(r, m)-200) + math.Floor(u/4) + math.Mod(u, 5) + math.Pow(2, math.Mod(u, 3)) + u)
			}},
		{name: "rounding", formula: "ROUNDUP(sales / 7, 1) - ROUNDDOWN(sales / 7, 1) + CEILING(sales, 25) - FLOOR(sales, 25)",
			leaf: func(r string, m int) (float64, bool) {
				s := sales(r, m)
				x := 0.0
				if int(s)%7 != 0 {
					x = 0.1
				}
				if int(s)%25 != 0 {
					x += 25
				}
				return v(x)
			}},
		{name: "round_half", formula: "ROUND(sales / 8, 2) + ROUND(1.005, 2) + ROUNDUP(0.1 + 0.2, 1)",
			leaf: func(r string, m int) (float64, bool) {
				return v(round(sales(r, m)/8, 2) + 1.01 + 0.3)
			}},
		{name: "cmp_15", formula: `IF(0.1 + 0.2 = 0.3, 1, 0) + IF(sales / 3 * 3 = sales, 10, 0)`,
			leaf: func(string, int) (float64, bool) { return v(11) }},
		{name: "crit_15", formula: `SUMIFS(sales, region.factor, "=" & (0.1 + 0.2) / 0.3 * 1.5)`,
			leaf: func(_ string, m int) (float64, bool) { return v(sales("DE", m)) }}, // 1.5000000000000002 matches DE's 1.5
		{name: "arg_aggs", formula: `SUM(sales, cost, units) + AVERAGE(sales, cost) + MIN(sales, cost) + MAX(sales, cost) + COUNT(sales, cost, "x") + COUNTA(sales, "x")`,
			leaf: func(r string, m int) (float64, bool) {
				s, c, u := sales(r, m), cost(r, m), units(r, m)
				return v(s + c + u + (s+c)/2 + math.Min(s, c) + math.Max(s, c) + 2 + 2)
			}},

		// ── Text (inside numbers: results persist as numbers) ───────────
		{name: "text_mix", formula: `LEN(CONCAT(region, "-", UPPER(LEFT(region.segment, 3)))) + LEN(TEXTJOIN(",", TRUE, region, region.currency, "")) + IF(RIGHT(region.currency, 2) = "UR", 1, 0) + LEN(MID(region.segment, 2, 3)) + IF(LOWER(TRIM("  " & region & "  ")) = "de", 5, 0) + LEN(SUBSTITUTE(region.segment, "e", ""))`,
			leaf: func(r string, _ int) (float64, bool) {
				p := props[r]
				x := float64(len(r) + 1 + 3)
				x += float64(len(r) + 1 + len(p.currency))
				if strings.HasSuffix(p.currency, "UR") {
					x++
				}
				x += float64(min(3, len(p.segment)-1))
				if r == "DE" {
					x += 5
				}
				x += float64(len(strings.ReplaceAll(p.segment, "e", "")))
				return v(x)
			}},
		{name: "text_num", formula: `LEN("x" & sales * 10000)`,
			leaf: func(r string, m int) (float64, bool) {
				return v(float64(1 + len(strconv.FormatFloat(sales(r, m)*10000, 'f', -1, 64))))
			}},
		{name: "text_fmt", formula: `IF(TEXT(sales / 3, "0.00") = TEXT(ROUND(sales / 3, 2), "0.00"), 1, 0) + IF(TEXT(START(), "yyyy-mm") = period, 1, 0)`,
			leaf: func(string, int) (float64, bool) { return v(2) }},

		// ── Dates ───────────────────────────────────────────────────────
		{name: "date_mix", formula: "YEAR(START()) * 10000 + MONTH(START()) * 100 + DAY(END())",
			leaf: func(_ string, m int) (float64, bool) { return v(float64(2026*10000 + m*100 + daysIn(m))) }},
		{name: "date_fns", formula: "DAYS(EOMONTH(START(), 0), START()) + 1 - DAY(EOMONTH(START(), 0)) + MONTH(EDATE(START(), 2)) + DAYSINMONTH(2026, MONTH(START())) + DAYSINYEAR(YEAR(END()))",
			leaf: func(_ string, m int) (float64, bool) { return v(float64((m+1)%12 + 1 + daysIn(m) + 365)) }},
		{name: "date_ctor", formula: "DATE(2026, MONTH(START()), 15) - START()", leaf: func(string, int) (float64, bool) { return v(14) }},
		{name: "today_ok", formula: "IF(YEAR(TODAY()) >= 2026, 1, 0)", leaf: func(string, int) (float64, bool) { return v(1) }},
		{name: "tenure_days", formula: "DAYS(START(), region.opened)",
			leaf: func(r string, m int) (float64, bool) { return v(date(2026, m, 1).Sub(props[r].opened).Hours() / 24) }},
		{name: "eom_is_end", formula: "IF(EOMONTH(END(), 0) = END(), 1, 0)", leaf: func(string, int) (float64, bool) { return v(1) }},
		{name: "edate_end", formula: "DAY(EDATE(END(), 1))",
			leaf: func(_ string, m int) (float64, bool) {
				next := date(2026, m+2, 0).Day() // days in the month after
				return v(float64(min(daysIn(m), next)))
			}},

		// ── Member properties, PARENT, LOOKUP ───────────────────────────
		{name: "seg_flag", formula: `IF(region.segment = "enterprise", 1, 0)`,
			leaf: func(r string, _ int) (float64, bool) { return v(b2f(props[r].segment == "Enterprise")) }},
		{name: "scaled", formula: "sales * region.factor", leaf: func(r string, m int) (float64, bool) { return v(sales(r, m) * props[r].factor) }},
		{name: "parent_is_emea", formula: `IF(PARENT(region) = "EMEA", 1, 0)`,
			leaf: func(r string, _ int) (float64, bool) { return v(b2f(props[r].parent == "EMEA")) }},
		{name: "fx_sales", formula: "ROUND(sales * LOOKUP(fx_rate, currency, region.currency), 2)",
			leaf: func(r string, m int) (float64, bool) { return v(round(sales(r, m)*fxRates[props[r].currency], 2)) }},
		{name: "share_parent", formula: "sales / LOOKUP(sales, region, PARENT(region))",
			leaf: func(r string, m int) (float64, bool) { return v(sales(r, m) / parentSales(r, m)) }},
		{name: "vs_de", formula: `sales - LOOKUP(sales, region, "DE")`, leaf: func(r string, m int) (float64, bool) { return v(sales(r, m) - sales("DE", m)) }},
		{name: "lk_na", formula: `IFNA(LOOKUP(sales, region, region & "X"), -1)`, leaf: func(string, int) (float64, bool) { return v(-1) }},
		{name: "lk_jan", formula: `LOOKUP(sales, period, "2026-01")`, leaf: func(r string, _ int) (float64, bool) { return v(sales(r, 1)) }},

		// ── Conditional aggregation ─────────────────────────────────────
		{name: "ent_sales", formula: `SUMIFS(sales, region.segment, "Enterprise")`,
			leaf: func(_ string, m int) (float64, bool) { return v(sales("DE", m) + sales("US", m)) }},
		{name: "same_seg_avg", formula: "AVERAGEIFS(sales, region.segment, region.segment)",
			leaf: func(r string, m int) (float64, bool) {
				if r == "UK" {
					return v(sales("UK", m))
				}
				return v((sales("DE", m) + sales("US", m)) / 2)
			}},
		{name: "cnt", formula: `COUNTIFS(region.segment, "Enterprise", region.factor, ">=1") + COUNTIF(region, "U*")`,
			leaf: func(string, int) (float64, bool) { return v(3) }},
		{name: "min_max", formula: `MAXIFS(sales, region.currency, "<>GBP") - MINIFS(sales, region.currency, "<>GBP")`,
			leaf: func(_ string, m int) (float64, bool) { return v(math.Abs(sales("DE", m) - sales("US", m))) }},
		{name: "sumif_mix", formula: `SUMIF(region.opened, ">=2020-01-01", sales) + AVERAGEIF(region, "<>" & region, sales)`,
			leaf: func(r string, m int) (float64, bool) {
				x := sales("UK", m) + sales("US", m)
				o := 0.0
				for _, q := range leafRegions {
					if q != r {
						o += sales(q, m)
					}
				}
				return v(x + o/2)
			}},
		{name: "share_all", formula: `ROUND(sales / SUMIFS(sales, region, "*") * 100, 3)`,
			leaf: func(r string, m int) (float64, bool) { return v(round(sales(r, m)/worldSales(m)*100, 3)) }},
		{name: "sumifs_time", formula: `SUMIFS(sales, period, "<=" & period)`,
			leaf: func(r string, m int) (float64, bool) { return v(sumRange(of(r), 1, m)) }},

		// ── Time series ─────────────────────────────────────────────────
		{name: "prev", formula: "PREVIOUS(sales)", leaf: func(r string, m int) (float64, bool) { return v(sumRange(of(r), m-1, m-1)) }},
		{name: "nxt", formula: "NEXT(sales)", leaf: func(r string, m int) (float64, bool) { return v(sumRange(of(r), m+1, m+1)) }},
		{name: "lag2", formula: "LAG(sales, 2, -1)", leaf: func(r string, m int) (float64, bool) {
			if m <= 2 {
				return v(-1)
			}
			return v(sales(r, m-2))
		}},
		{name: "lead1", formula: "LEAD(sales, 1, 0)", leaf: func(r string, m int) (float64, bool) { return v(sumRange(of(r), m+1, m+1)) }},
		{name: "off_back", formula: "OFFSET(sales, -3, sales)", leaf: func(r string, m int) (float64, bool) {
			if m <= 3 {
				return v(sales(r, m))
			}
			return v(sales(r, m-3))
		}},
		{name: "dyn_lag", formula: "LAG(sales, lag_n, 0)", leaf: func(r string, m int) (float64, bool) {
			t := m - int(lagN(r, m))
			if t < 1 {
				return v(0)
			}
			return v(sales(r, t))
		}},
		{name: "dyn_strict", formula: "LAG(sales, lag_n, -5, STRICT)", leaf: func(r string, m int) (float64, bool) {
			n := int(lagN(r, m))
			if n <= 0 || m-n < 1 {
				return v(-5)
			}
			return v(sales(r, m-n))
		}},
		{name: "lag_prop", formula: "LAG(sales, ROUND(region.factor, 0), 0)", leaf: func(r string, m int) (float64, bool) {
			n := int(math.Round(props[r].factor)) // 1.5 -> 2, 2 -> 2, 0.5 -> 1 (half away from zero)
			if m-n < 1 {
				return v(0)
			}
			return v(sales(r, m-n))
		}},
		{name: "ms3", formula: "MOVINGSUM(sales, -2, 0)", leaf: func(r string, m int) (float64, bool) { return v(sumRange(of(r), m-2, m)) }},
		{name: "ms_avg", formula: "MOVINGSUM(sales, -2, 0, AVERAGE)", leaf: func(r string, m int) (float64, bool) {
			n := float64(m - max(m-2, 1) + 1)
			return v(sumRange(of(r), m-2, m) / n)
		}},
		{name: "ms_fwd_max", formula: "MOVINGSUM(sales, 1, 3, MAX)", leaf: func(r string, m int) (float64, bool) {
			x, any := 0.0, false
			for k := m + 1; k <= min(m+3, 12); k++ {
				if !any || sales(r, k) > x {
					x, any = sales(r, k), true
				}
			}
			return v(x)
		}},
		{name: "ms_all", formula: "MOVINGSUM(sales)", leaf: func(r string, _ int) (float64, bool) { return v(sumRange(of(r), 1, 12)) }},
		{name: "cum", formula: "CUMULATE(sales)", leaf: func(r string, m int) (float64, bool) { return v(sumRange(of(r), 1, m)) }},
		{name: "cum_q", formula: "CUMULATE(sales, MOD(MONTH(START()), 3) = 1)", leaf: func(r string, m int) (float64, bool) {
			return v(sumRange(of(r), quarterStart(m), m))
		}},
		{name: "decum", formula: "DECUMULATE(sales)", leaf: func(r string, m int) (float64, bool) {
			return v(sales(r, m) - sumRange(of(r), m-1, m-1))
		}},
		{name: "qtd", formula: "QUARTERTODATE(sales)", leaf: func(r string, m int) (float64, bool) { return v(sumRange(of(r), quarterStart(m), m)) }},
		{name: "htd", formula: "HALFYEARTODATE(sales)", leaf: func(r string, m int) (float64, bool) { return v(sumRange(of(r), halfStart(m), m)) }},
		{name: "ytd", formula: "YEARTODATE(sales)", leaf: func(r string, m int) (float64, bool) { return v(sumRange(of(r), 1, m)) }},
		{name: "yv", formula: "YEARVALUE(sales)", leaf: func(r string, _ int) (float64, bool) { return v(sumRange(of(r), 1, 12)) }},
		{name: "qv", formula: "QUARTERVALUE(sales)", leaf: func(r string, m int) (float64, bool) {
			return v(sumRange(of(r), quarterStart(m), quarterStart(m)+2))
		}},
		{name: "hv", formula: "HALFYEARVALUE(sales)", leaf: func(r string, m int) (float64, bool) {
			return v(sumRange(of(r), halfStart(m), halfStart(m)+5))
		}},
		{name: "share_year", formula: "ROUND(sales / YEARVALUE(sales) * 100, 4)", leaf: func(r string, m int) (float64, bool) {
			return v(round(sales(r, m)/sumRange(of(r), 1, 12)*100, 4))
		}},
		{name: "last_lag_n", formula: "YEARVALUE(lag_n)", leaf: func(r string, _ int) (float64, bool) {
			for k := 12; k >= 1; k-- { // time_summary last: the last recorded (non-zero) value
				if lagN(r, k) != 0 {
					return v(lagN(r, k))
				}
			}
			return 0, false
		}},
		{name: "ts_q2", formula: `TIMESUM(sales, "Q2", "Q2")`, leaf: func(r string, _ int) (float64, bool) { return v(sumRange(of(r), 4, 6)) }},
		{name: "ts_h1_max", formula: `TIMESUM(sales, "H1", "H1", MAX)`, leaf: func(r string, _ int) (float64, bool) {
			x := sales(r, 1)
			for k := 2; k <= 6; k++ {
				x = math.Max(x, sales(r, k))
			}
			return v(x)
		}},
		{name: "ts_all", formula: "TIMESUM(sales)", leaf: func(r string, _ int) (float64, bool) { return v(sumRange(of(r), 1, 12)) }},
		{name: "ts_q1_avg", formula: `TIMESUM(sales, "2026-01", "2026-03", AVERAGE)`, leaf: func(r string, _ int) (float64, bool) {
			return v(sumRange(of(r), 1, 3) / 3)
		}},
		{name: "ts_empty", formula: `TIMESUM(sales, "Q2", "Q1")`, leaf: func(string, int) (float64, bool) { return v(0) }},

		// ── Recurrence (time breaks the cycle) ──────────────────────────
		{name: "opening", formula: "LAG(closing, 1, 100)", first: "flow * 0", summary: "first",
			leaf:  func(r string, m int) (float64, bool) { return v(closing(r, m-1)) },
			total: func() float64 { return 3 * 100 }},
		{name: "closing", formula: "opening + flow", summary: "last",
			leaf: func(r string, m int) (float64, bool) { return v(closing(r, m)) },
			total: func() float64 {
				s := 0.0
				for _, r := range leafRegions {
					s += closing(r, 12)
				}
				return s
			}},

		// ── Combinations across families ────────────────────────────────
		{name: "growth", formula: "IF(PREVIOUS(sales) = 0, 0, ROUND(sales / PREVIOUS(sales) - 1, 4))",
			leaf: func(r string, m int) (float64, bool) {
				if m == 1 {
					return v(0)
				}
				return v(round(sales(r, m)/sales(r, m-1)-1, 4))
			}},
		{name: "prev_share", formula: "LAG(share_parent, 1, 0)", leaf: func(r string, m int) (float64, bool) {
			if m == 1 {
				return v(0)
			}
			return v(sales(r, m-1) / parentSales(r, m-1))
		}},
		{name: "prev_ent", formula: `PREVIOUS(SUMIFS(sales, region.segment, "Enterprise"))`, leaf: func(_ string, m int) (float64, bool) {
			if m == 1 {
				return v(0)
			}
			return v(sales("DE", m-1) + sales("US", m-1))
		}},
		{name: "ytd_fx", formula: "YEARTODATE(sales * LOOKUP(fx_rate, currency, region.currency))", leaf: func(r string, m int) (float64, bool) {
			return v(sumRange(of(r), 1, m) * fxRates[props[r].currency])
		}},
		{name: "ms_de", formula: `MOVINGSUM(LOOKUP(sales, region, "DE"), -1, 0)`, leaf: func(_ string, m int) (float64, bool) {
			return v(sumRange(of("DE"), m-1, m))
		}},
		{name: "qtd_share", formula: "ROUND(qtd / QUARTERVALUE(sales), 4)", leaf: func(r string, m int) (float64, bool) {
			return v(round(sumRange(of(r), quarterStart(m), m)/sumRange(of(r), quarterStart(m), quarterStart(m)+2), 4))
		}},
		{name: "per_day", formula: "ROUND(sales / DAYSINMONTH(YEAR(START()), MONTH(START())) * 30, 2)", leaf: func(r string, m int) (float64, bool) {
			return v(round(sales(r, m)/float64(daysIn(m))*30, 2))
		}},
		{name: "rank_like", formula: `COUNTIFS(region, "<>" & region) - SUMIFS(units, region, region) + units`,
			leaf: func(string, int) (float64, bool) { return v(2) }},
		{name: "ifs_time", formula: `IFS(START() < DATE(2026, 4, 1), "Q1", END() > DATE(2026, 9, 30), "Q4", TRUE, "mid") = "Q1"`,
			leaf: func(_ string, m int) (float64, bool) { return v(b2f(m <= 3)) }},

		// ── Daily granularity: MONTHTODATE, MONTHVALUE ──────────────────
		{name: "mtd", formula: "MONTHTODATE(daily_sales)", daily: true, day: func(k int) float64 {
			start := 1
			if k > 31 {
				start = 32
			}
			return float64((start + k) * (k - start + 1) / 2)
		}},
		{name: "mv", formula: "MONTHVALUE(daily_sales)", daily: true, day: func(k int) float64 {
			if k > 31 {
				return float64((32 + 59) * 28 / 2)
			}
			return float64((1 + 31) * 31 / 2)
		}},
		{name: "d_qtd", formula: "QUARTERTODATE(daily_sales)", daily: true, day: func(k int) float64 { return float64((1 + k) * k / 2) }},
	}
}

// ── checks ─────────────────────────────────────────────────────────────────

func expectVal(problems *[]string, where string, got float64, ok bool, want float64, present bool) {
	switch {
	case !present && ok:
		*problems = append(*problems, fmt.Sprintf("%s = %g, want no value (the formula fails there)", where, got))
	case !present:
	case !ok:
		*problems = append(*problems, fmt.Sprintf("%s missing, want %g", where, want))
	case !approx(got, want):
		*problems = append(*problems, fmt.Sprintf("%s = %g, want %g", where, got, want))
	}
}

func catalogueProblems(e *environment) map[string][]string {
	out := map[string][]string{}
	plan := fetchGrid(e.dev, e.planGrid, e.rev)
	daily := fetchGrid(e.dev, e.dailyGrid, e.rev)
	if plan.status != 200 || daily.status != 200 {
		out["grid read"] = []string{fmt.Sprintf("Plan -> %d, Daily -> %d: %s", plan.status, daily.status, clip(plan.body+daily.body, 300))}
		return out
	}
	for _, d := range catalogue() {
		mid := e.metric[d.name]
		var p []string
		sum := 0.0
		if d.daily {
			for k := 1; k <= days; k++ {
				want := d.day(k)
				sum += want
				got, ok := daily.cell(mid, dayCode(k))
				expectVal(&p, dayCode(k), got, ok, want, true)
			}
		} else {
			for _, r := range leafRegions {
				for m := 1; m <= 12; m++ {
					want, present := d.leaf(r, m)
					sum += want
					got, ok := plan.cell(mid, r, monthCode(m))
					expectVal(&p, r+" "+monthCode(m), got, ok, want, present)
				}
			}
		}
		if !d.noTotal {
			want := sum
			if d.total != nil {
				want = d.total()
			}
			g := plan
			if d.daily {
				g = daily
			}
			got, ok := g.totals[mid]
			expectVal(&p, "grand total", got, ok, want, true)
		}
		out[fmt.Sprintf("%s = %s", d.name, d.formula)] = p
	}
	return out
}

func checkCatalogue(e *environment) {
	deadline := time.Now().Add(*flagTimeout)
	start := time.Now()
	var last map[string][]string
	for {
		last = catalogueProblems(e)
		bad := 0
		for _, p := range last {
			if len(p) > 0 {
				bad++
			}
		}
		if bad == 0 {
			step("recalculation settled after %s", time.Since(start).Round(100*time.Millisecond))
			break
		}
		if time.Now().After(deadline) {
			step("%d metrics still wrong after %s", bad, *flagTimeout)
			break
		}
		time.Sleep(2 * time.Second)
	}
	names := make([]string, 0, len(last))
	for n := range last {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := last[n]
		more := ""
		if len(p) > 4 {
			more = fmt.Sprintf(" (+%d more)", len(p)-4)
			p = p[:4]
		}
		check(n, len(p) == 0, "%s%s", strings.Join(p, "; "), more)
	}
}

// refusal is a formula the save must refuse (HTTP 400) with a message
// containing want.
type refusal struct {
	name, formula, want string
}

func refusals() []refusal {
	return []refusal{
		{"bad_arity", "ROUND(sales)", "wrong number of arguments"},
		{"bad_arity_nested", "IF(sales > 0, ROUND(sales), 0)", "ROUND: wrong number of arguments"},
		{"bad_ifs_pairs", "IFS(sales > 0, 1, FALSE)", "condition, value pairs"},
		{"bad_today", "TODAY(1)", "wrong number of arguments"},
		{"bad_unknown_fn", "VLOOKUP(sales, 1, 2)", "unknown function"},
		{"bad_later_parity", "POST(sales, 1)", "unknown function"},
		{"bad_unknown_name", "nonexistent * 2", "unknown metric or dimension"},
		{"bad_syntax", "sales +", "parse"},
		{"bad_unterminated", `IF(region = "DE, 1, 0)`, "closing quote"},
		{"bad_percent", "sales * 50%", "parse"},
		{"bad_source", `LOOKUP(sales + 1, region, "DE")`, "SOURCE_MUST_BE_METRIC"},
		{"bad_member", `LOOKUP(sales, region, "ZZ")`, "UNKNOWN_MEMBER"},
		{"bad_property", "sales * region.nope", "UNKNOWN_PROPERTY"},
		{"bad_dim_arg", `LOOKUP(sales, region.segment, "SMB")`, "DIMENSION_ARGUMENT_REQUIRED"},
		{"bad_not_on_source", `LOOKUP(sales, currency, "EUR")`, "DIMENSION_NOT_ON_SOURCE"},
		{"bad_offset", "LAG(sales, 1.5, 0)", "TIME_OFFSET_NOT_INTEGER"},
		{"bad_window", "MOVINGSUM(sales, lag_n, 0)", "MOVING_WINDOW_NOT_LITERAL"},
		{"bad_keyword", "MOVINGSUM(sales, -1, 0, MEDIAN)", "unknown keyword"},
		{"bad_self", "bad_self + 1", "references the metric it defines"},
	}
}

func checkRefusals(e *environment) {
	for _, r := range refusals() {
		raw, status := e.dev.try("POST", "/api/developer/metrics", map[string]any{
			"name": r.name, "is_input": false, "formula": "=" + r.formula, "agg_rule": "sum", "revision_id": e.rev,
		})
		check(fmt.Sprintf("refused at save: %s", r.formula), status == 400 && strings.Contains(raw, r.want),
			"-> %d %s (want 400 containing %q)", status, clip(raw, 220), r.want)
	}

	// Edits go through the same checks; a same-period cycle is refused.
	raw, status := e.dev.try("PATCH", "/api/developer/metrics/"+e.metric["price"], map[string]any{"formula": "=ROUND(sales / units, 2) + growth2"})
	check("refused at save: an unknown name in an edit", status == 400 && strings.Contains(raw, "unknown"), "-> %d %s", status, clip(raw, 200))
	raw, status = e.dev.try("PATCH", "/api/developer/metrics/"+e.metric["margin"], map[string]any{"formula": "=sales - cost + margin_pct"})
	check("accepted: margin reading margin_pct (no cycle yet)", status == 200, "-> %d %s", status, clip(raw, 200))
	raw, status = e.dev.try("PATCH", "/api/developer/metrics/"+e.metric["margin_pct"], map[string]any{"formula": "=margin / sales"})
	check("refused at save: a same-period cycle margin <-> margin_pct", status == 400, "-> %d %s", status, clip(raw, 220))
	e.dev.call("PATCH", "/api/developer/metrics/"+e.metric["margin"], map[string]any{"formula": "=sales - cost"})

	// The recurrence may not use LOOKUP.
	raw, status = e.dev.try("PATCH", "/api/developer/metrics/"+e.metric["closing"], map[string]any{"formula": `=opening + flow + LOOKUP(sales, region, "DE") * 0`})
	check("refused at save: LOOKUP inside an opening/closing recurrence", status == 400 && strings.Contains(raw, "TEMPORAL_CYCLE_NOT_CAUSAL"),
		"-> %d %s", status, clip(raw, 220))
	raw, status = e.dev.try("PATCH", "/api/developer/metrics/"+e.metric["opening"], map[string]any{"formula": "=LAG(closing, lag_n, 100)"})
	check("refused at save: a dynamic offset inside a recurrence", status == 400 && strings.Contains(raw, "TEMPORAL_CYCLE_NOT_CAUSAL"),
		"-> %d %s", status, clip(raw, 220))

	// A literal TIMESUM period is checked once the metric's time axis is
	// known: at placement on a grid with a time dimension.
	raw, status = e.dev.try("POST", "/api/developer/metrics", map[string]any{
		"name": "bad_period", "is_input": false, "formula": `=TIMESUM(sales, "Q9", "Q1")`, "agg_rule": "sum", "revision_id": e.rev,
	})
	if status == 200 {
		mid := idFromRaw(raw)
		raw, status = e.dev.try("POST", "/api/developer/grids/"+e.planGrid+"/metrics/"+mid, nil)
		check("refused at placement: TIMESUM of an unknown period code", status == 400 && strings.Contains(raw, "UNKNOWN_MEMBER"),
			"-> %d %s", status, clip(raw, 220))
		e.dev.try("DELETE", "/api/developer/metrics/"+mid, nil)
	} else {
		check("refused at placement: TIMESUM of an unknown period code", strings.Contains(raw, "UNKNOWN_MEMBER"), "create -> %d %s", status, clip(raw, 220))
	}

	// Names are case-insensitive (documented): a reference in another case
	// resolves to the metric.
	raw, status = e.dev.try("POST", "/api/developer/metrics", map[string]any{
		"name": "case_ref", "is_input": false, "formula": "=SALES * 2 + Cost", "agg_rule": "sum", "revision_id": e.rev,
	})
	check("accepted: metric names referenced in another case (SALES, Cost)", status == 200, "-> %d %s", status, clip(raw, 220))
	if status == 200 {
		e.dev.try("DELETE", "/api/developer/metrics/"+idFromRaw(raw), nil)
	}

	// Dimension names too (migration 101).
	raw, status = e.dev.try("POST", "/api/developer/dimensions", map[string]any{"name": "Region", "agg_rule": "sum", "revision_id": e.rev})
	check("refused: a dimension name differing only in case from an existing one (Region beside region)",
		status == 409 && strings.Contains(raw, "DIMENSION_NAME_TAKEN"), "-> %d %s", status, clip(raw, 220))

	// Names are unique regardless of case (migration 100), so a reference
	// in any case is unambiguous: a second metric differing only in case is
	// refused, while renaming a metric to another case of its own name is
	// allowed.
	raw, status = e.dev.try("POST", "/api/developer/metrics", map[string]any{"name": "Sales", "is_input": true, "revision_id": e.rev})
	check("refused: a metric name differing only in case from an existing one (Sales beside sales)",
		status == 409 && strings.Contains(raw, "METRIC_NAME_TAKEN"), "-> %d %s", status, clip(raw, 220))
	raw, status = e.dev.try("PATCH", "/api/developer/metrics/"+e.metric["price"], map[string]any{"name": "Margin"})
	check("refused: renaming price to Margin (margin exists)", status == 409 && strings.Contains(raw, "METRIC_NAME_TAKEN"),
		"-> %d %s", status, clip(raw, 220))
	raw, status = e.dev.try("PATCH", "/api/developer/metrics/"+e.metric["margin"], map[string]any{"name": "Margin"})
	check("accepted: renaming margin to Margin (its own name in another case)", status == 200, "-> %d %s", status, clip(raw, 220))
	e.dev.try("PATCH", "/api/developer/metrics/"+e.metric["margin"], map[string]any{"name": "margin"})

	// A metric another metric reads — in a formula or as a Rate operand —
	// cannot be deleted until that metric no longer reads it.
	newMetric := func(body map[string]any) string {
		body["revision_id"] = e.rev
		mid := idFromRaw(mustRaw(e.dev.try("POST", "/api/developer/metrics", body)))
		e.dev.call("POST", "/api/developer/grids/"+e.planGrid+"/metrics/"+mid, nil)
		return mid
	}
	src := newMetric(map[string]any{"name": "tmp_src", "is_input": true})
	den := newMetric(map[string]any{"name": "tmp_den", "is_input": true})
	dep := newMetric(map[string]any{"name": "tmp_dep", "is_input": false, "formula": "=tmp_src * 2", "agg_rule": "sum"})
	rate := newMetric(map[string]any{"name": "tmp_rate", "is_input": false, "formula": "=1", "agg_rule": "rate",
		"agg_numerator_metric_id": src, "agg_denominator_metric_id": den})
	raw, status = e.dev.try("DELETE", "/api/developer/metrics/"+src, nil)
	check("refused: deleting tmp_src while tmp_dep = tmp_src * 2 reads it and tmp_rate totals by it",
		status == 409 && strings.Contains(raw, "METRIC_IN_USE") && strings.Contains(raw, "tmp_dep") && strings.Contains(raw, "tmp_rate (its Rate total)"),
		"-> %d %s", status, clip(raw, 260))
	e.dev.call("PATCH", "/api/developer/metrics/"+dep, map[string]any{"formula": "=2"})
	e.dev.call("DELETE", "/api/developer/metrics/"+rate, nil)
	raw, status = e.dev.try("DELETE", "/api/developer/metrics/"+src, nil)
	check("accepted: deleting tmp_src once no metric reads it", status == 200, "-> %d %s", status, clip(raw, 200))
	for _, mid := range []string{dep, den} {
		e.dev.try("DELETE", "/api/developer/metrics/"+mid, nil)
	}
}

func mustRaw(raw string, status int) string {
	if status != 200 {
		fatalf("setup call -> %d: %s", status, clip(raw, 300))
	}
	return raw
}

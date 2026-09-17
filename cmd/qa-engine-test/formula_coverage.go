package main

import (
	"fmt"
	"time"
)

// formulaCase is one function/operator test. Every formula is written as
// =IF(<check>, seed, 0) so a pass always evaluates to exactly 1 (seed's
// value) — this also guarantees a dependency edge on "seed", so writing
// seed once triggers RecalcAffected across all of them in one shot (a
// formula with zero dependencies never gets picked up by the reverse-graph
// BFS in calculation.Scheduler.RecalcAffected, since it can never appear as
// a transitive dependent of any input metric — see scheduler.go).
type formulaCase struct {
	name    string
	formula string
}

// errorCase is a formula expected to evaluate to an ERROR (e.g. #DIV/0!) —
// checked separately: it must NOT appear in totals (calc engine should mark
// the partition 'error' and leave it unset, not crash or write garbage).
type errorCase struct {
	name    string
	formula string
}

func formulaCases() []formulaCase {
	return []formulaCase{
		// Logic
		{"if_true", `IF(TRUE,seed,0)`},
		{"if_false_else", `IF(FALSE,0,seed)`},
		{"if_2arg_false_is_zero", `IF(IF(FALSE,99)=0,seed,0)`},
		{"ifs_second_match", `IF(IFS(FALSE,1,TRUE,2)=2,seed,0)`},
		{"and_true", `IF(AND(TRUE,1=1,2>1),seed,0)`},
		{"and_false_caught_by_not", `IF(NOT(AND(TRUE,FALSE)),seed,0)`},
		{"or_true", `IF(OR(FALSE,FALSE,1=1),seed,0)`},
		{"not", `IF(NOT(FALSE),seed,0)`},
		{"iferror_catches_div0", `IF(IFERROR(1/0,99)=99,seed,0)`},
		{"ifna_catches_na_not_div0", `IF(IFNA(IFS(FALSE,1),55)=55,seed,0)`},
		{"switch_match", `IF(SWITCH(2,1,"one",2,"two",3,"three")="two",seed,0)`},
		{"switch_default", `IF(SWITCH(99,1,"one",2,"two","fallback")="fallback",seed,0)`},

		// Math
		{"abs", `IF(ABS(-5)=5,seed,0)`},
		{"int_floor_positive", `IF(INT(7.8)=7,seed,0)`},
		{"int_floor_negative", `IF(INT(-7.2)=-8,seed,0)`},
		{"round", `IF(ROUND(3.14159,2)=3.14,seed,0)`},
		{"round_negative_places", `IF(ROUND(1234.5,-2)=1200,seed,0)`},
		{"roundup", `IF(ROUNDUP(3.111,2)=3.12,seed,0)`},
		{"roundup_negative_away_from_zero", `IF(ROUNDUP(-3.111,2)=-3.12,seed,0)`},
		{"rounddown", `IF(ROUNDDOWN(3.199,2)=3.19,seed,0)`},
		{"ceiling", `IF(CEILING(22,5)=25,seed,0)`},
		{"floor", `IF(FLOOR(22,5)=20,seed,0)`},
		{"mod", `IF(MOD(23,5)=3,seed,0)`},
		{"mod_negative_sign_of_divisor", `IF(MOD(-7,3)=2,seed,0)`},
		{"power", `IF(POWER(2,10)=1024,seed,0)`},
		{"sqrt", `IF(SQRT(144)=12,seed,0)`},

		// Aggregation
		{"sum", `IF(SUM(1,2,3,4,5)=15,seed,0)`},
		{"average", `IF(AVERAGE(2,4,6)=4,seed,0)`},
		{"min", `IF(MIN(5,2,9,-3)=-3,seed,0)`},
		{"max", `IF(MAX(5,2,9,-3)=9,seed,0)`},
		{"count_numbers_only", `IF(COUNT(1,2,3)=3,seed,0)`},
		{"counta_counts_empty_string_literal", `IF(COUNTA(1,"",2)=3,seed,0)`},

		// Text (wrapped in IF/comparison since calc metrics must resolve to a number)
		{"concat", `IF(CONCAT("a","b","c")="abc",seed,0)`},
		{"textjoin_ignore_empty", `IF(TEXTJOIN("-",TRUE,"a","","b")="a-b",seed,0)`},
		{"len", `IF(LEN("hello")=5,seed,0)`},
		{"left", `IF(LEFT("hello",2)="he",seed,0)`},
		{"left_default_1char", `IF(LEFT("hello")="h",seed,0)`},
		{"right", `IF(RIGHT("hello",2)="lo",seed,0)`},
		{"mid", `IF(MID("hello",2,3)="ell",seed,0)`},
		{"upper", `IF(UPPER("abc")="ABC",seed,0)`},
		{"lower", `IF(LOWER("ABC")="abc",seed,0)`},
		{"trim_collapses_internal_runs", `IF(TRIM("  a   b  ")="a b",seed,0)`},
		{"text_format_2dp", `IF(TEXT(3.14159,"0.00")="3.14",seed,0)`},
		{"substitute_all", `IF(SUBSTITUTE("aaa","a","b")="bbb",seed,0)`},
		{"substitute_nth_occurrence", `IF(SUBSTITUTE("aaa","a","b",2)="aba",seed,0)`},

		// Date (DATE/YEAR/MONTH/DAY round-trip; EDATE/EOMONTH derived from it)
		{"date_year_roundtrip", `IF(YEAR(DATE(2026,7,22))=2026,seed,0)`},
		{"date_month_roundtrip", `IF(MONTH(DATE(2026,7,22))=7,seed,0)`},
		{"date_day_roundtrip", `IF(DAY(DATE(2026,7,22))=22,seed,0)`},
		{"days_between", `IF(DAYS(DATE(2026,7,22),DATE(2026,7,1))=21,seed,0)`},
		{"edate_month_forward", `IF(MONTH(EDATE(DATE(2026,7,22),1))=8,seed,0)`},
		{"edate_day_preserved", `IF(DAY(EDATE(DATE(2026,7,22),1))=22,seed,0)`},
		{"edate_year_rollover", `IF(YEAR(EDATE(DATE(2026,7,22),6))=2027,seed,0)`},
		{"eomonth_same_month_end", `IF(DAY(EOMONTH(DATE(2026,7,22),0))=31,seed,0)`},
		{"eomonth_next_month", `IF(MONTH(EOMONTH(DATE(2026,7,22),1))=8,seed,0)`},

		// Operators
		{"op_add", `IF(2+3=5,seed,0)`},
		{"op_sub", `IF(10-4=6,seed,0)`},
		{"op_mul", `IF(6*7=42,seed,0)`},
		{"op_div", `IF(20/4=5,seed,0)`},
		{"op_pow", `IF(3^3=27,seed,0)`},
		{"op_unary_minus", `IF(-(-5)=5,seed,0)`},
		{"op_concat_ampersand", `IF("foo"&"bar"="foobar",seed,0)`},
		{"op_precedence_mul_before_add", `IF(2+3*4=14,seed,0)`},
		{"op_parens_override_precedence", `IF((2+3)*4=20,seed,0)`},
		{"op_not_equal", `IF(5<>4,seed,0)`},
		{"op_less_equal", `IF(5<=5,seed,0)`},
		{"op_greater_equal_false_branch", `IF(5>=6,0,seed)`},
	}
}

// todayYearCase is generated at run time (uses the harness's own clock, not
// a hardcoded year) so the test never goes stale.
func todayYearCase() formulaCase {
	y := time.Now().UTC().Year()
	return formulaCase{"today_year_matches_clock", fmt.Sprintf(`IF(YEAR(TODAY())=%d,seed,0)`, y)}
}

// errorCases are formulas that are ACCEPTED at creation and then fail at
// calculation time — the engine cannot know in advance that seed_zero will
// hold 0.
//
// err_unknown_function used to live here. It moved to a creation-time
// rejection check (below) when formula validation landed on 2026-08-13
// (bff774c): an unknown function name is decidable from the formula alone, so
// the engine now refuses to store it at all rather than accepting it and
// failing every recalculation forever. This harness had not been run between
// that change and 2026-08-15, so it was still asserting the old contract and
// aborted on the 400.
func errorCases() []errorCase {
	return []errorCase{
		{"err_div_by_zero", `1/seed_zero`},
	}
}

// runFormulaCoverage creates the "seed" input metric plus every formula
// test/error-case metric, writes seed=1 (which synchronously triggers
// RecalcAffected across everything depending on it), then reads the whole
// model's totals back and checks each one. Returns seed's metric ID for
// reuse by later phases if needed.
func runFormulaCoverage(dev *api) string {
	step("creating input metric \"seed\" (=1) and \"seed_zero\" (=0, for the div-by-zero case)")
	seedID := createInputMetric(dev, "seed")
	seedZeroID := createInputMetric(dev, "seed_zero")

	cases := formulaCases()
	cases = append(cases, todayYearCase())

	step("creating %d formula-coverage calc metrics", len(cases))
	nameToID := map[string]string{}
	for _, c := range cases {
		nameToID[c.name] = createCalcMetric(dev, c.name, c.formula, "")
	}

	ecases := errorCases()
	step("creating %d deliberately-erroring calc metrics (expect graceful failure, not a crash)", len(ecases))
	errNameToID := map[string]string{}
	for _, c := range ecases {
		errNameToID[c.name] = createCalcMetric(dev, c.name, c.formula, "")
	}

	// Negative tests: anything decidable from the formula text alone must be
	// REJECTED at metric-creation time (400), never even inserted.
	step("negative test: formula referencing an unknown metric name")
	_, status := dev.callStatus("POST", "/api/developer/metrics", map[string]any{
		"name": "bad_ref_test", "is_input": false,
		"formula": "=totally_made_up_metric_name*2", "revision_id": dev.revisionID,
	})
	record("bad-ref formula rejected at creation (400)", status == 400, fmt.Sprintf("got HTTP %d", status))

	step("negative test: formula calling a function that does not exist")
	_, status = dev.callStatus("POST", "/api/developer/metrics", map[string]any{
		"name": "bad_fn_test", "is_input": false,
		"formula": "=NOTAFUNCTION(1)+0*seed", "revision_id": dev.revisionID,
	})
	record("unknown-function formula rejected at creation (400)", status == 400, fmt.Sprintf("got HTTP %d", status))

	step("writing seed=1 (synchronous recalc — every dependent metric computes before this returns)")
	t0 := time.Now()
	writeCell(dev, seedID, nil, 1)
	writeCell(dev, seedZeroID, nil, 0)
	step("recalc after seed writeback took %s", time.Since(t0))

	step("fetching whole-model totals")
	grid := dev.call("GET", fmt.Sprintf("/api/grid?revision_id=%s", dev.revisionID), nil)
	totals, _ := grid["totals"].(map[string]any)
	if totals == nil {
		totals = map[string]any{}
	}

	for _, c := range cases {
		mid := nameToID[c.name]
		v, ok := totals[mid]
		if !ok {
			record(c.name, false, "missing from totals (calc never ran or errored) — formula: "+c.formula)
			continue
		}
		f, ok := v.(float64)
		if !ok || f != 1 {
			record(c.name, false, fmt.Sprintf("expected 1, got %v — formula: %s", v, c.formula))
			continue
		}
		record(c.name, true, "")
	}

	for _, c := range ecases {
		mid := errNameToID[c.name]
		_, present := totals[mid]
		record(c.name+" (should NOT compute)", !present, map[bool]string{true: "correctly absent from totals", false: "unexpectedly present — engine should have marked this an error"}[!present])
	}

	return seedID
}

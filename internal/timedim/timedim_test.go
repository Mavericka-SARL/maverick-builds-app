package timedim

import (
	"strings"
	"testing"
	"time"
)

func d(s string) time.Time {
	t, err := ParseDate(s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestValidateConfig(t *testing.T) {
	c := Config{}
	if err := ValidateConfig(&c); err != nil || c.Type != TypeStandard {
		t.Fatalf("omitted type must default to standard: %v %+v", err, c)
	}
	if err := ValidateConfig(&Config{Type: TypeStandard, Granularity: GranMonth}); err == nil {
		t.Error("standard with granularity must be rejected")
	}
	if err := ValidateConfig(&Config{Type: TypeTime, Granularity: GranMonth}); err == nil {
		t.Error("time without fiscal month must be rejected")
	}
	if err := ValidateConfig(&Config{Type: TypeTime, FiscalYearStartMonth: 1}); err == nil {
		t.Error("time without granularity must be rejected")
	}
	if err := ValidateConfig(&Config{Type: TypeTime, Granularity: GranMonth, FiscalYearStartMonth: 4}); err != nil {
		t.Errorf("valid time config rejected: %v", err)
	}
	if err := ValidateConfig(&Config{Type: "period"}); err == nil {
		t.Error("unknown type must be rejected")
	}
}

func TestValidatePeriodsMonthly(t *testing.T) {
	cfg := Config{Type: TypeTime, Granularity: GranMonth, FiscalYearStartMonth: 1}
	ok := []Period{
		{"2026-02", d("2026-02-01"), d("2026-02-28")},
		{"2026-01", d("2026-01-01"), d("2026-01-31")},
		{"2026-03", d("2026-03-01"), d("2026-03-31")},
	}
	if err := ValidatePeriods(cfg, ok); err != nil {
		t.Fatalf("contiguous months rejected: %v", err)
	}
	if ok[0].Code != "2026-01" || ok[2].Code != "2026-03" {
		t.Errorf("not sorted chronologically: %v", ok)
	}
	for name, ps := range map[string][]Period{
		"gap":       {{"2026-01", d("2026-01-01"), d("2026-01-31")}, {"2026-03", d("2026-03-01"), d("2026-03-31")}},
		"overlap":   {{"a", d("2026-01-01"), d("2026-01-31")}, {"b", d("2026-01-15"), d("2026-02-14")}},
		"duplicate": {{"a", d("2026-01-01"), d("2026-01-31")}, {"b", d("2026-01-01"), d("2026-01-31")}},
		"bad end":   {{"a", d("2026-01-01"), d("2026-01-30")}},
		"mid-month": {{"a", d("2026-01-15"), d("2026-02-14")}},
		"reversed":  {{"a", d("2026-01-31"), d("2026-01-01")}},
	} {
		if err := ValidatePeriods(cfg, ps); err == nil {
			t.Errorf("%s: expected rejection", name)
		} else if !strings.HasPrefix(err.Error(), CodeInvalidTimeMember) {
			t.Errorf("%s: error should carry %s: %v", name, CodeInvalidTimeMember, err)
		}
	}
}

func TestValidatePeriodsFiscalQuarter(t *testing.T) {
	cfg := Config{Type: TypeTime, Granularity: GranQuarter, FiscalYearStartMonth: 4}
	if err := ValidatePeriods(cfg, []Period{
		{"FY26-Q1", d("2026-04-01"), d("2026-06-30")},
		{"FY26-Q2", d("2026-07-01"), d("2026-09-30")},
	}); err != nil {
		t.Errorf("fiscal quarters rejected: %v", err)
	}
	if err := ValidatePeriods(cfg, []Period{{"Q", d("2026-02-01"), d("2026-04-30")}}); err == nil {
		t.Error("a quarter off the fiscal boundary must be rejected")
	}
}

func TestCustomAllowsUnequalButNotOverlap(t *testing.T) {
	cfg := Config{Type: TypeTime, Granularity: GranCustom, FiscalYearStartMonth: 1}
	if err := ValidatePeriods(cfg, []Period{
		{"a", d("2026-01-01"), d("2026-01-10")},
		{"b", d("2026-02-01"), d("2026-02-02")},
	}); err != nil {
		t.Errorf("custom with a gap and unequal lengths must be accepted: %v", err)
	}
	if err := ValidatePeriods(cfg, []Period{
		{"a", d("2026-01-01"), d("2026-01-10")},
		{"b", d("2026-01-10"), d("2026-01-12")},
	}); err == nil {
		t.Error("custom overlap must be rejected")
	}
}

func TestIntervalKeys(t *testing.T) {
	jan := Period{"2026-01", d("2026-01-01"), d("2026-01-31")}
	mar := Period{"2026-03", d("2026-03-01"), d("2026-03-31")}
	apr := Period{"2026-04", d("2026-04-01"), d("2026-04-30")}
	k1, ok1 := IntervalKey(LevelQuarter, 1, jan)
	k2, _ := IntervalKey(LevelQuarter, 1, mar)
	k3, _ := IntervalKey(LevelQuarter, 1, apr)
	if !ok1 || k1 != k2 || k1 == k3 {
		t.Errorf("calendar quarter keys: %s %s %s", k1, k2, k3)
	}
	// April fiscal year: Jan and Mar are in the same quarter (Q4 of FY25), Apr starts FY26.
	y1, _ := IntervalKey(LevelYear, 4, mar)
	y2, _ := IntervalKey(LevelYear, 4, apr)
	if y1 == y2 {
		t.Errorf("April fiscal year: Mar and Apr must be in different years (%s)", y1)
	}
	// A period straddling a quarter boundary is not attributable.
	if _, ok := IntervalKey(LevelQuarter, 1, Period{"x", d("2026-03-15"), d("2026-04-14")}); ok {
		t.Error("straddling period must be reported, not prorated")
	}
	if GranularityFitsLevel(LevelMonth, GranMonth) || !GranularityFitsLevel(LevelYear, GranQuarter) {
		t.Error("granularity fit rules")
	}
}

func TestGeneratePeriods(t *testing.T) {
	ps, err := GeneratePeriods(Config{Type: TypeTime, Granularity: GranMonth, FiscalYearStartMonth: 1}, d("2026-01-01"), d("2026-04-30"))
	if err != nil || len(ps) != 4 || ps[3].Code != "2026-04" || !ps[3].End.Equal(d("2026-04-30")) {
		t.Fatalf("monthly generator: %v %+v", err, ps)
	}
	if err := ValidatePeriods(Config{Type: TypeTime, Granularity: GranMonth, FiscalYearStartMonth: 1}, ps); err != nil {
		t.Errorf("generated periods must validate: %v", err)
	}
	if _, err := GeneratePeriods(Config{Type: TypeTime, Granularity: GranQuarter, FiscalYearStartMonth: 4}, d("2026-02-01"), d("2026-12-31")); err == nil {
		t.Error("quarter generator must refuse a non-fiscal-quarter start")
	}
}

func TestValidateHierarchy(t *testing.T) {
	cfg := Config{Type: TypeTime, Granularity: GranQuarter, FiscalYearStartMonth: 1}
	q1s, q1e := d("2026-01-01"), d("2026-03-31")
	q2s, q2e := d("2026-04-01"), d("2026-06-30")
	fy := MemberShape{ID: "fy", Code: "FY26"}
	h1 := MemberShape{ID: "h1", Code: "H1", ParentID: "fy"}
	q1 := MemberShape{ID: "q1", Code: "Q1", ParentID: "h1", Start: &q1s, End: &q1e}
	q2 := MemberShape{ID: "q2", Code: "Q2", ParentID: "h1", Start: &q2s, End: &q2e}
	leaves, err := ValidateHierarchy(cfg, []MemberShape{fy, h1, q1, q2})
	if err != nil || len(leaves) != 2 || leaves[0].Code != "Q1" {
		t.Fatalf("valid hierarchy rejected: %v %v", err, leaves)
	}
	// An aggregate with no children yet is allowed (the build is in progress).
	if _, err := ValidateHierarchy(cfg, []MemberShape{fy}); err != nil {
		t.Errorf("empty aggregate must be allowed: %v", err)
	}
	// A dated period cannot have children.
	datedH1 := h1
	datedH1.Start, datedH1.End = &q1s, &q2e
	if _, err := ValidateHierarchy(cfg, []MemberShape{fy, datedH1, q1, q2}); err == nil {
		t.Error("dated parent must be rejected")
	}
	// A leaf cannot be a child of a leaf.
	child := MemberShape{ID: "x", Code: "X", ParentID: "q1", Start: &q2s, End: &q2e}
	if _, err := ValidateHierarchy(cfg, []MemberShape{fy, h1, q1, child}); err == nil {
		t.Error("leaf under a leaf must be rejected")
	}
	// Leaf rules still apply across the leaves.
	q3s, q3e := d("2026-10-01"), d("2026-12-31")
	gap := MemberShape{ID: "q4", Code: "Q4", ParentID: "h1", Start: &q3s, End: &q3e}
	if _, err := ValidateHierarchy(cfg, []MemberShape{fy, h1, q1, q2, gap}); err == nil {
		t.Error("gap between leaves must be rejected")
	}
}

// Package timedim owns the invariants of a time dimension: which
// granularities exist, what a valid period looks like at each, that the
// periods of one dimension never overlap or (for regular granularities)
// leave gaps, and the server-owned chronological ordinal (time_index) that
// time-series formulas move by.
//
// A time dimension may be a hierarchy (Q1 → H1 → FY26). Only LEAF periods
// carry dates and an ordinal: a member with dates is a leaf period and can
// have no children; a member without dates is an aggregate period (a
// grouping of the periods below it) and is what a leaf's parent must be.
// Time functions move along leaves; an aggregate's value is its descendants
// reduced by the metric's time summary.
//
// Every writer of time members — the developer API, CSV import, the AI
// assistant, package import, revision copy — validates through this package
// and re-indexes through Reindex, so there is exactly one definition of
// "these periods are well-formed" and exactly one order.
package timedim

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Dimension types.
const (
	TypeStandard = "standard"
	TypeTime     = "time"
)

// Granularities.
const (
	GranDay      = "day"
	GranWeek     = "week"
	GranMonth    = "month"
	GranQuarter  = "quarter"
	GranHalfYear = "half_year"
	GranYear     = "year"
	GranCustom   = "custom"
)

// Granularities lists every accepted granularity, in coarseness order.
var Granularities = []string{GranDay, GranWeek, GranMonth, GranQuarter, GranHalfYear, GranYear, GranCustom}

// TimeSummaries lists every accepted metric time-summary method.
var TimeSummaries = []string{"sum", "average", "min", "max", "first", "last", "none"}

// Error identifiers (spec §9). Surfaced verbatim in messages so a caller can
// match on them.
const (
	CodeInvalidTimeMember = "INVALID_TIME_MEMBER"
)

// Error is a validation rejection with a stable identifier.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

func invalid(format string, args ...any) error {
	return &Error{Code: CodeInvalidTimeMember, Message: fmt.Sprintf(format, args...)}
}

// Config is the time configuration of a dimension.
type Config struct {
	Type                 string
	Granularity          string
	FiscalYearStartMonth int
}

// ValidateConfig checks a dimension's time configuration at creation. An
// omitted type defaults to standard; a standard dimension must carry no time
// settings; a time dimension must carry both.
func ValidateConfig(c *Config) error {
	if c.Type == "" {
		c.Type = TypeStandard
	}
	switch c.Type {
	case TypeStandard:
		if c.Granularity != "" || c.FiscalYearStartMonth != 0 {
			return invalid("time_granularity and fiscal_year_start_month apply only to a time dimension")
		}
		return nil
	case TypeTime:
		if !validGranularity(c.Granularity) {
			return invalid("time_granularity is required for a time dimension (one of %v)", Granularities)
		}
		if c.FiscalYearStartMonth < 1 || c.FiscalYearStartMonth > 12 {
			return invalid("fiscal_year_start_month is required for a time dimension (1-12)")
		}
		return nil
	default:
		return invalid("dimension_type must be %q or %q", TypeStandard, TypeTime)
	}
}

// ValidTimeSummary reports whether s is an accepted metric time summary.
func ValidTimeSummary(s string) bool {
	for _, k := range TimeSummaries {
		if k == s {
			return true
		}
	}
	return false
}

func validGranularity(g string) bool {
	for _, k := range Granularities {
		if k == g {
			return true
		}
	}
	return false
}

// Period is one member's date range.
type Period struct {
	Code  string
	Start time.Time
	End   time.Time
}

// ParseDate parses a YYYY-MM-DD date as UTC midnight.
func ParseDate(s string) (time.Time, error) {
	t, err := time.ParseInLocation("2006-01-02", s, time.UTC)
	if err != nil {
		return time.Time{}, invalid("invalid date %q (want YYYY-MM-DD)", s)
	}
	return t, nil
}

// SortPeriods orders periods chronologically by (start, end, code) — the one
// deterministic order the ordinal follows.
func SortPeriods(ps []Period) {
	sort.SliceStable(ps, func(i, j int) bool {
		if !ps[i].Start.Equal(ps[j].Start) {
			return ps[i].Start.Before(ps[j].Start)
		}
		if !ps[i].End.Equal(ps[j].End) {
			return ps[i].End.Before(ps[j].End)
		}
		return ps[i].Code < ps[j].Code
	})
}

// ValidatePeriods checks the complete period set of one time dimension:
// each period's boundaries fit the granularity, no two overlap, and regular
// granularities leave no gaps. ps is sorted in place.
func ValidatePeriods(cfg Config, ps []Period) error {
	for i := range ps {
		if err := ValidatePeriod(cfg, ps[i]); err != nil {
			return err
		}
	}
	SortPeriods(ps)
	for i := 1; i < len(ps); i++ {
		prev, cur := ps[i-1], ps[i]
		if !cur.Start.After(prev.End) {
			return invalid("periods %q (%s..%s) and %q (%s..%s) overlap",
				prev.Code, ymd(prev.Start), ymd(prev.End), cur.Code, ymd(cur.Start), ymd(cur.End))
		}
		if cfg.Granularity != GranCustom && !cur.Start.Equal(prev.End.AddDate(0, 0, 1)) {
			return invalid("gap between periods %q (ends %s) and %q (starts %s): a %s dimension must be contiguous",
				prev.Code, ymd(prev.End), cur.Code, ymd(cur.Start), cfg.Granularity)
		}
	}
	return nil
}

// ValidatePeriod checks one period's boundaries against the granularity.
func ValidatePeriod(cfg Config, p Period) error {
	if p.End.Before(p.Start) {
		return invalid("period %q ends (%s) before it starts (%s)", p.Code, ymd(p.End), ymd(p.Start))
	}
	switch cfg.Granularity {
	case GranCustom:
		return nil
	case GranDay:
		if !p.End.Equal(p.Start) {
			return invalid("period %q: a day period must start and end on the same date", p.Code)
		}
	case GranWeek:
		if !p.End.Equal(p.Start.AddDate(0, 0, 6)) {
			return invalid("period %q: a week period must span exactly 7 days", p.Code)
		}
	case GranMonth:
		if p.Start.Day() != 1 || !p.End.Equal(p.Start.AddDate(0, 1, -1)) {
			return invalid("period %q: a month period must run from the 1st to the last day of one month", p.Code)
		}
	case GranQuarter:
		if p.Start.Day() != 1 || monthsFromFiscalStart(p.Start, cfg.FiscalYearStartMonth)%3 != 0 ||
			!p.End.Equal(p.Start.AddDate(0, 3, -1)) {
			return invalid("period %q: a quarter period must start on a fiscal quarter boundary and span 3 months", p.Code)
		}
	case GranHalfYear:
		if p.Start.Day() != 1 || monthsFromFiscalStart(p.Start, cfg.FiscalYearStartMonth)%6 != 0 ||
			!p.End.Equal(p.Start.AddDate(0, 6, -1)) {
			return invalid("period %q: a half-year period must start on a fiscal half-year boundary and span 6 months", p.Code)
		}
	case GranYear:
		if p.Start.Day() != 1 || monthsFromFiscalStart(p.Start, cfg.FiscalYearStartMonth) != 0 ||
			!p.End.Equal(p.Start.AddDate(1, 0, -1)) {
			return invalid("period %q: a year period must start in fiscal month %d and span 12 months", p.Code, cfg.FiscalYearStartMonth)
		}
	default:
		return invalid("unknown time granularity %q", cfg.Granularity)
	}
	return nil
}

func monthsFromFiscalStart(t time.Time, fiscalStart int) int {
	if fiscalStart < 1 {
		fiscalStart = 1
	}
	return ((int(t.Month()) - fiscalStart) + 12) % 12
}

func ymd(t time.Time) string { return t.Format("2006-01-02") }

// Interval identifies the calendar interval a period belongs to at one of
// the period-to-date functions' levels, as a comparable key. Two periods
// with the same key are in the same month/quarter/year.
type IntervalLevel int

const (
	LevelMonth IntervalLevel = iota
	LevelQuarter
	LevelYear
)

// IntervalKey returns the key of the interval containing p at level, and
// whether p lies entirely inside one interval. A period that straddles an
// interval boundary (a custom period spanning two months, a week across a
// quarter end) makes the dimension invalid for that period-to-date function
// — reported, never prorated.
func IntervalKey(level IntervalLevel, fiscalStart int, p Period) (string, bool) {
	ks, oks := intervalKeyAt(level, fiscalStart, p.Start)
	ke, oke := intervalKeyAt(level, fiscalStart, p.End)
	if !oks || !oke || ks != ke {
		return "", false
	}
	return ks, true
}

func intervalKeyAt(level IntervalLevel, fiscalStart int, t time.Time) (string, bool) {
	switch level {
	case LevelMonth:
		return fmt.Sprintf("%04d-%02d", t.Year(), int(t.Month())), true
	case LevelQuarter:
		fy, offset := fiscalYearAndOffset(t, fiscalStart)
		return fmt.Sprintf("FY%04d-Q%d", fy, offset/3+1), true
	case LevelYear:
		fy, _ := fiscalYearAndOffset(t, fiscalStart)
		return fmt.Sprintf("FY%04d", fy), true
	}
	return "", false
}

// fiscalYearAndOffset returns the fiscal year t falls in (named by the
// calendar year in which that fiscal year STARTS) and t's month offset from
// the fiscal year start (0-11).
func fiscalYearAndOffset(t time.Time, fiscalStart int) (int, int) {
	if fiscalStart < 1 {
		fiscalStart = 1
	}
	offset := monthsFromFiscalStart(t, fiscalStart)
	fy := t.Year()
	if int(t.Month()) < fiscalStart {
		fy--
	}
	return fy, offset
}

// GranularityFitsLevel reports whether a source granularity is fine enough
// for a period-to-date function at level (spec §5.4): MONTHTODATE needs
// days; QUARTERTODATE days/weeks/months; YEARTODATE anything below a year.
func GranularityFitsLevel(level IntervalLevel, gran string) bool {
	switch level {
	case LevelMonth:
		return gran == GranDay
	case LevelQuarter:
		return gran == GranDay || gran == GranWeek || gran == GranMonth
	case LevelYear:
		return gran == GranDay || gran == GranWeek || gran == GranMonth || gran == GranQuarter || gran == GranHalfYear
	}
	return false
}

// GeneratePeriods produces contiguous periods of the configured granularity
// from start to end (inclusive of the period containing end), for the bulk
// period generator. Codes are derived from the start date; labels are left
// to the caller. Custom granularity has no generator.
func GeneratePeriods(cfg Config, start, end time.Time) ([]Period, error) {
	if cfg.Granularity == GranCustom {
		return nil, invalid("custom granularity has no period generator")
	}
	if end.Before(start) {
		return nil, invalid("end before start")
	}
	var out []Period
	cur := start
	for !cur.After(end) {
		var next time.Time
		var code string
		switch cfg.Granularity {
		case GranDay:
			next = cur.AddDate(0, 0, 1)
			code = cur.Format("2006-01-02")
		case GranWeek:
			next = cur.AddDate(0, 0, 7)
			code = cur.Format("2006-01-02")
		case GranMonth:
			if cur.Day() != 1 {
				return nil, invalid("start must be the first day of a month")
			}
			next = cur.AddDate(0, 1, 0)
			code = cur.Format("2006-01")
		case GranQuarter:
			if cur.Day() != 1 || monthsFromFiscalStart(cur, cfg.FiscalYearStartMonth)%3 != 0 {
				return nil, invalid("start must be the first day of a fiscal quarter")
			}
			next = cur.AddDate(0, 3, 0)
			fy, off := fiscalYearAndOffset(cur, cfg.FiscalYearStartMonth)
			code = fmt.Sprintf("FY%04d-Q%d", fy, off/3+1)
		case GranHalfYear:
			if cur.Day() != 1 || monthsFromFiscalStart(cur, cfg.FiscalYearStartMonth)%6 != 0 {
				return nil, invalid("start must be the first day of a fiscal half-year")
			}
			next = cur.AddDate(0, 6, 0)
			fy, off := fiscalYearAndOffset(cur, cfg.FiscalYearStartMonth)
			code = fmt.Sprintf("FY%04d-H%d", fy, off/6+1)
		case GranYear:
			if cur.Day() != 1 || monthsFromFiscalStart(cur, cfg.FiscalYearStartMonth) != 0 {
				return nil, invalid("start must be the first day of a fiscal year")
			}
			next = cur.AddDate(1, 0, 0)
			fy, _ := fiscalYearAndOffset(cur, cfg.FiscalYearStartMonth)
			code = fmt.Sprintf("FY%04d", fy)
		default:
			return nil, invalid("unknown time granularity %q", cfg.Granularity)
		}
		out = append(out, Period{Code: code, Start: cur, End: next.AddDate(0, 0, -1)})
		cur = next
		if len(out) > 10000 {
			return nil, invalid("too many periods (limit 10000)")
		}
	}
	return out, nil
}

// Querier is the subset of pgx used by the DB helpers: a pool or a tx.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// LoadConfig reads a dimension's time configuration.
func LoadConfig(ctx context.Context, q Querier, dimensionID string) (Config, error) {
	var c Config
	var gran *string
	var fy *int16
	err := q.QueryRow(ctx,
		`SELECT dimension_type, time_granularity, fiscal_year_start_month FROM model.dimension_def WHERE id=$1::uuid`,
		dimensionID).Scan(&c.Type, &gran, &fy)
	if err != nil {
		return c, err
	}
	if gran != nil {
		c.Granularity = *gran
	}
	if fy != nil {
		c.FiscalYearStartMonth = int(*fy)
	}
	return c, nil
}

// LoadPeriods reads every member of dimensionID that carries a period.
func LoadPeriods(ctx context.Context, q Querier, dimensionID string) ([]Period, error) {
	rows, err := q.Query(ctx,
		`SELECT code, period_start, period_end FROM model.dimension_member
		 WHERE dimension_id=$1::uuid AND period_start IS NOT NULL`, dimensionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Period
	for rows.Next() {
		var p Period
		if err := rows.Scan(&p.Code, &p.Start, &p.End); err != nil {
			return nil, err
		}
		p.Start, p.End = p.Start.UTC(), p.End.UTC()
		out = append(out, p)
	}
	return out, rows.Err()
}

// MemberShape is one member as ValidateAndReindex sees it.
type MemberShape struct {
	ID, Code, ParentID string
	Start, End         *time.Time
}

// ValidateHierarchy checks the leaf/aggregate rules of a time dimension's
// member set and returns the leaf periods. Dated members are leaves and
// may not have children; a member's parent must be an aggregate (undated).
func ValidateHierarchy(cfg Config, members []MemberShape) ([]Period, error) {
	byID := make(map[string]MemberShape, len(members))
	hasChildren := map[string]bool{}
	for _, m := range members {
		byID[m.ID] = m
		if m.ParentID != "" {
			hasChildren[m.ParentID] = true
		}
	}
	var leaves []Period
	for _, m := range members {
		dated := m.Start != nil && m.End != nil
		if (m.Start == nil) != (m.End == nil) {
			return nil, invalid("period %q needs both period_start and period_end", m.Code)
		}
		if dated && hasChildren[m.ID] {
			return nil, invalid("period %q has child periods, so it is an aggregate and cannot carry its own dates — its range comes from its children", m.Code)
		}
		if m.ParentID != "" {
			p, ok := byID[m.ParentID]
			if !ok {
				return nil, invalid("period %q: parent not found in this dimension", m.Code)
			}
			if p.Start != nil {
				return nil, invalid("period %q cannot be a child of %q: a dated period is a leaf; make %q an aggregate (no dates) first", m.Code, p.Code, p.Code)
			}
		}
		if dated {
			leaves = append(leaves, Period{Code: m.Code, Start: m.Start.UTC(), End: m.End.UTC()})
		}
	}
	if err := ValidatePeriods(cfg, leaves); err != nil {
		return nil, err
	}
	return leaves, nil
}

// LoadMembers reads every member of a dimension in ValidateHierarchy's shape.
func LoadMembers(ctx context.Context, q Querier, dimensionID string) ([]MemberShape, error) {
	rows, err := q.Query(ctx,
		`SELECT id::text, code, COALESCE(parent_member_id::text,''), period_start, period_end
		 FROM model.dimension_member WHERE dimension_id=$1::uuid`, dimensionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MemberShape
	for rows.Next() {
		var m MemberShape
		if err := rows.Scan(&m.ID, &m.Code, &m.ParentID, &m.Start, &m.End); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ValidateAndReindex is what every member writer calls after changing a time
// dimension's member set, inside the same transaction as the change: it
// re-validates the complete member set (leaf/aggregate shape, period rules)
// and renumbers the leaves' time_index densely from zero in (period_start,
// period_end, code) order. Aggregates keep a NULL ordinal. The transaction's
// deferred unique constraints let the renumbering happen in one statement.
func ValidateAndReindex(ctx context.Context, tx Querier, dimensionID string) error {
	cfg, err := LoadConfig(ctx, tx, dimensionID)
	if err != nil {
		return err
	}
	if cfg.Type != TypeTime {
		// A standard dimension must carry no periods at all.
		var n int
		if err := tx.QueryRow(ctx,
			`SELECT COUNT(*) FROM model.dimension_member WHERE dimension_id=$1::uuid AND period_start IS NOT NULL`,
			dimensionID).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return invalid("a standard dimension's members cannot carry period dates")
		}
		return nil
	}
	members, err := LoadMembers(ctx, tx, dimensionID)
	if err != nil {
		return err
	}
	if _, err := ValidateHierarchy(cfg, members); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		WITH ordered AS (
			SELECT id, row_number() OVER (ORDER BY period_start, period_end, code) - 1 AS idx
			FROM model.dimension_member
			WHERE dimension_id=$1::uuid AND period_start IS NOT NULL
		)
		UPDATE model.dimension_member m SET time_index = o.idx
		FROM ordered o WHERE o.id = m.id AND m.time_index IS DISTINCT FROM o.idx
	`, dimensionID)
	return err
}

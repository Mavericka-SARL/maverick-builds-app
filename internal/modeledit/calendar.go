package modeledit

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/timedim"
)

// ExtendCalendar adds a time dimension's periods after its last one, through
// the period that contains through, placed as the last period is: under
// aggregates of the same kind (a quarter, a fiscal year), reusing one that
// already covers the new period and otherwise creating it, named after its
// existing peers (FY2027 → FY2028, 2027-Q4 → 2028-Q1). It is how a business
// admin extends a model's time window (owner decision, 2026-11-07): the
// Guide gives that to FP&A, and only a developer could add periods.
//
// A calendar whose grouping it cannot read (an aggregate of periods from two
// years, aggregates of one kind at different depths) is refused rather than
// guessed. It returns the number of periods and aggregates created.
func ExtendCalendar(ctx context.Context, tx pgx.Tx, dimID string, through time.Time) (periods, aggregates int, err error) {
	cfg, err := timedim.LoadConfig(ctx, tx, dimID)
	if err != nil {
		return 0, 0, err
	}
	if cfg.Type != timedim.TypeTime {
		return 0, 0, calendarErr("only a time dimension has a calendar to extend")
	}
	members, err := loadCalendar(ctx, tx, dimID)
	if err != nil {
		return 0, 0, err
	}
	cal := newCalendar(members)
	last := cal.lastLeaf()
	if last == nil {
		return 0, 0, calendarErr("the calendar has no periods yet: a developer sets up its first ones")
	}
	if !through.After(*last.End) {
		return 0, 0, calendarErr(fmt.Sprintf("the calendar already runs to %s", last.End.Format("2 January 2006")))
	}
	newPeriods, err := timedim.GeneratePeriods(cfg, last.End.AddDate(0, 0, 1), through)
	if err != nil {
		return 0, 0, err
	}

	// The last period's ancestors, nearest first, and the interval each
	// groups by: the finest above the periods' own that every aggregate at
	// its depth keeps whole.
	chain := cal.ancestors(last.ID)
	levels := make([]timedim.IntervalLevel, len(chain))
	for i, a := range chain {
		lvl, ok := cal.levelAt(cal.depth[a.ID], cfg)
		if !ok {
			return 0, 0, calendarErr(fmt.Sprintf("the calendar groups its periods under %q in a way this cannot follow: a developer extends it", a.Code))
		}
		levels[i] = lvl
	}
	// Existing aggregates, by their level and interval.
	byKey := map[string]string{}
	codes := map[string]bool{}
	for _, m := range members {
		codes[m.Code] = true
	}
	for i := range chain {
		for _, m := range cal.atDepth[cal.depth[chain[i].ID]] {
			if k, ok := cal.intervalOf(m.ID, levels[i], cfg); ok {
				byKey[levelKey(levels[i], k)] = m.ID
			}
		}
	}

	for _, p := range newPeriods {
		parent := ""
		for i := len(chain) - 1; i >= 0; i-- { // the outermost first
			key, ok := timedim.IntervalKey(levels[i], cfg.FiscalYearStartMonth, p)
			if !ok {
				return 0, 0, calendarErr(fmt.Sprintf("period %s does not fit one interval of the calendar", p.Code))
			}
			id, found := byKey[levelKey(levels[i], key)]
			if !found {
				peerKey, _ := cal.intervalOf(chain[i].ID, levels[i], cfg)
				code, label := analogName(chain[i], peerKey, key)
				if codes[code] {
					return 0, 0, calendarErr(fmt.Sprintf("a new %s would be named %q, which is taken: a developer extends this calendar", levelName(levels[i]), code))
				}
				var parentArg any
				if parent != "" {
					parentArg = parent
				}
				if err := tx.QueryRow(ctx, `
					INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id)
					VALUES ($1::uuid, $2, $3, $4::uuid) RETURNING id::text
				`, dimID, code, label, parentArg).Scan(&id); err != nil {
					return 0, 0, err
				}
				codes[code] = true
				byKey[levelKey(levels[i], key)] = id
				aggregates++
			}
			parent = id
		}
		var parentArg any
		if parent != "" {
			parentArg = parent
		}
		if codes[p.Code] {
			return 0, 0, calendarErr(fmt.Sprintf("a period %q already exists", p.Code))
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO model.dimension_member (dimension_id, code, label, period_start, period_end, time_index, parent_member_id)
			VALUES ($1::uuid, $2, $3, $4::date, $5::date, 0, $6::uuid)
		`, dimID, p.Code, PeriodLabel(cfg.Granularity, p), p.Start, p.End, parentArg); err != nil {
			return 0, 0, err
		}
		codes[p.Code] = true
		periods++
	}
	if err := timedim.ValidateAndReindex(ctx, tx, dimID); err != nil {
		return 0, 0, err
	}
	return periods, aggregates, nil
}

// CalendarError is a calendar ExtendCalendar refuses to extend: the caller's
// mistake or a shape it cannot read, never a failure of the database.
type CalendarError struct{ Message string }

func (e *CalendarError) Error() string { return e.Message }

func calendarErr(msg string) error { return &CalendarError{Message: msg} }

// calMember is one member of a time dimension, with its label.
type calMember struct {
	ID, Code, Label, ParentID string
	Start, End                *time.Time
}

func loadCalendar(ctx context.Context, tx pgx.Tx, dimID string) ([]calMember, error) {
	rows, err := tx.Query(ctx, `
		SELECT id::text, code, COALESCE(label, ''), COALESCE(parent_member_id::text, ''), period_start, period_end
		FROM model.dimension_member WHERE dimension_id = $1::uuid`, dimID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []calMember
	for rows.Next() {
		var m calMember
		if err := rows.Scan(&m.ID, &m.Code, &m.Label, &m.ParentID, &m.Start, &m.End); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// calendar is a time dimension's member tree.
type calendar struct {
	byID     map[string]calMember
	children map[string][]string
	depth    map[string]int
	atDepth  map[int][]calMember // aggregates (undated members) by depth
	leaves   []calMember
}

func newCalendar(members []calMember) *calendar {
	c := &calendar{byID: map[string]calMember{}, children: map[string][]string{}, depth: map[string]int{}, atDepth: map[int][]calMember{}}
	for _, m := range members {
		c.byID[m.ID] = m
		if m.ParentID != "" {
			c.children[m.ParentID] = append(c.children[m.ParentID], m.ID)
		}
	}
	for _, m := range members {
		d, cur := 0, m.ParentID
		for cur != "" && d < 32 {
			d++
			cur = c.byID[cur].ParentID
		}
		c.depth[m.ID] = d
		if m.Start == nil {
			c.atDepth[d] = append(c.atDepth[d], m)
		} else {
			c.leaves = append(c.leaves, m)
		}
	}
	sort.Slice(c.leaves, func(i, j int) bool { return c.leaves[i].End.Before(*c.leaves[j].End) })
	return c
}

func (c *calendar) lastLeaf() *calMember {
	if len(c.leaves) == 0 {
		return nil
	}
	return &c.leaves[len(c.leaves)-1]
}

func (c *calendar) ancestors(id string) []calMember {
	var out []calMember
	for cur := c.byID[id].ParentID; cur != "" && len(out) < 32; cur = c.byID[cur].ParentID {
		out = append(out, c.byID[cur])
	}
	return out
}

// periodsUnder lists the dated members beneath id.
func (c *calendar) periodsUnder(id string) []timedim.Period {
	var out []timedim.Period
	var walk func(string, int)
	walk = func(cur string, n int) {
		if n > 32 {
			return
		}
		for _, ch := range c.children[cur] {
			m := c.byID[ch]
			if m.Start != nil && m.End != nil {
				out = append(out, timedim.Period{Code: m.Code, Start: m.Start.UTC(), End: m.End.UTC()})
			}
			walk(ch, n+1)
		}
	}
	walk(id, 0)
	return out
}

// intervalOf is the one interval at level that every period under an
// aggregate falls in, if they share one.
func (c *calendar) intervalOf(id string, level timedim.IntervalLevel, cfg timedim.Config) (string, bool) {
	ps := c.periodsUnder(id)
	if len(ps) == 0 {
		return "", false
	}
	key := ""
	for _, p := range ps {
		k, ok := timedim.IntervalKey(level, cfg.FiscalYearStartMonth, p)
		if !ok || (key != "" && k != key) {
			return "", false
		}
		key = k
	}
	return key, true
}

// levelAt is the interval the aggregates at depth group by: the finest
// coarser than the periods themselves that each of them falls within, which
// at least one of them fills (so a half-filled year is not read as a half).
func (c *calendar) levelAt(depth int, cfg timedim.Config) (timedim.IntervalLevel, bool) {
	aggs := c.atDepth[depth]
	if len(aggs) == 0 {
		return 0, false
	}
	for _, lvl := range coarserLevels(cfg.Granularity) {
		fits, filled := true, false
		for _, a := range aggs {
			key, ok := c.intervalOf(a.ID, lvl, cfg)
			if !ok {
				fits = false
				break
			}
			if len(c.periodsUnder(a.ID)) == periodsIn(lvl, key, cfg) {
				filled = true
			}
		}
		if fits && filled {
			return lvl, true
		}
	}
	return 0, false
}

// coarserLevels are the intervals above a granularity, finest first.
func coarserLevels(gran string) []timedim.IntervalLevel {
	switch gran {
	case timedim.GranDay, timedim.GranWeek:
		return []timedim.IntervalLevel{timedim.LevelMonth, timedim.LevelQuarter, timedim.LevelHalfYear, timedim.LevelYear}
	case timedim.GranMonth:
		return []timedim.IntervalLevel{timedim.LevelQuarter, timedim.LevelHalfYear, timedim.LevelYear}
	case timedim.GranQuarter:
		return []timedim.IntervalLevel{timedim.LevelHalfYear, timedim.LevelYear}
	case timedim.GranHalfYear:
		return []timedim.IntervalLevel{timedim.LevelYear}
	}
	return nil
}

// periodsIn counts the periods of cfg's granularity in one interval, given
// its key ("FY2027", "FY2027-Q4", "FY2027-H1", "2027-03").
func periodsIn(level timedim.IntervalLevel, key string, cfg timedim.Config) int {
	start, end, ok := intervalRange(level, key, cfg.FiscalYearStartMonth)
	if !ok {
		return -1
	}
	ps, err := timedim.GeneratePeriods(cfg, start, end)
	if err != nil {
		return -1
	}
	return len(ps)
}

// intervalRange is an interval's first and last day.
func intervalRange(level timedim.IntervalLevel, key string, fiscalStart int) (time.Time, time.Time, bool) {
	if fiscalStart < 1 {
		fiscalStart = 1
	}
	if level == timedim.LevelMonth {
		t, err := time.Parse("2006-01", key)
		if err != nil {
			return time.Time{}, time.Time{}, false
		}
		return t, t.AddDate(0, 1, -1), true
	}
	if len(key) < 6 || !strings.HasPrefix(key, "FY") {
		return time.Time{}, time.Time{}, false
	}
	fy, err := strconv.Atoi(key[2:6])
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	yearStart := time.Date(fy, time.Month(fiscalStart), 1, 0, 0, 0, 0, time.UTC)
	months, part := 12, 1
	switch level {
	case timedim.LevelQuarter:
		months = 3
		part, err = strconv.Atoi(strings.TrimPrefix(key[6:], "-Q"))
	case timedim.LevelHalfYear:
		months = 6
		part, err = strconv.Atoi(strings.TrimPrefix(key[6:], "-H"))
	case timedim.LevelMonth, timedim.LevelYear:
		// a month was read above; a year spans the whole fiscal year
	}
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	start := yearStart.AddDate(0, (part-1)*months, 0)
	return start, start.AddDate(0, months, -1), true
}

func levelKey(level timedim.IntervalLevel, key string) string {
	return strconv.Itoa(int(level)) + "|" + key
}

func levelName(level timedim.IntervalLevel) string {
	switch level {
	case timedim.LevelMonth:
		return "month"
	case timedim.LevelQuarter:
		return "quarter"
	case timedim.LevelHalfYear:
		return "half-year"
	case timedim.LevelYear:
	}
	return "year"
}

// analogName names a new aggregate after a peer: every year in the peer's
// code and label moves by the years between the two intervals, and a quarter
// or half number (Q4, H2) moves as the interval does. A peer whose code
// holds no year gives the interval's own key.
func analogName(peer calMember, peerKey, key string) (string, string) {
	py, pPart := splitKey(peerKey)
	ny, nPart := splitKey(key)
	years := ny - py
	code, codeYear := shiftYears(peer.Code, years)
	label, _ := shiftYears(peer.Label, years)
	if pPart != "" && nPart != "" && pPart[0] == nPart[0] {
		code = shiftPart(code, pPart, nPart)
		label = shiftPart(label, pPart, nPart)
	}
	if !codeYear {
		return key, key
	}
	if label == "" {
		label = code
	}
	return code, label
}

// splitKey reads "FY2027-Q4" as 2027 and "Q4".
func splitKey(key string) (int, string) {
	if !strings.HasPrefix(key, "FY") || len(key) < 6 {
		if t, err := time.Parse("2006-01", key); err == nil {
			return t.Year(), ""
		}
		return 0, ""
	}
	y, _ := strconv.Atoi(key[2:6])
	return y, strings.TrimPrefix(key[6:], "-")
}

// shiftYears moves every four-digit year (19xx, 20xx) in s by delta, and
// reports whether s held one.
func shiftYears(s string, delta int) (string, bool) {
	var b strings.Builder
	found := false
	for i := 0; i < len(s); {
		j := i
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j == i {
			b.WriteByte(s[i])
			i++
			continue
		}
		run := s[i:j]
		if len(run) == 4 && (strings.HasPrefix(run, "19") || strings.HasPrefix(run, "20")) {
			y, _ := strconv.Atoi(run)
			b.WriteString(strconv.Itoa(y + delta))
			found = true
		} else {
			b.WriteString(run)
		}
		i = j
	}
	return b.String(), found
}

// shiftPart moves a quarter or half number written as its letter and digit
// (Q4, H2) by as many as the intervals differ, wrapping within the year.
func shiftPart(s, from, to string) string {
	letter := from[0]
	f, err1 := strconv.Atoi(from[1:])
	t, err2 := strconv.Atoi(to[1:])
	if err1 != nil || err2 != nil {
		return s
	}
	n := 4
	if letter == 'H' {
		n = 2
	}
	for i := 0; i+1 < len(s); i++ {
		if s[i] != letter || s[i+1] < '1' || s[i+1] > '9' || (i+2 < len(s) && s[i+2] >= '0' && s[i+2] <= '9') {
			continue
		}
		cur := int(s[i+1] - '0')
		moved := ((cur-1+(t-f))%n+n)%n + 1
		return s[:i+1] + strconv.Itoa(moved) + s[i+2:]
	}
	return s
}

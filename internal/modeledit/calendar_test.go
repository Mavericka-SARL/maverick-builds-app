package modeledit

import (
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/timedim"
)

func TestAnalogNameFollowsItsPeer(t *testing.T) {
	for _, c := range []struct{ code, label, peerKey, key, wantCode, wantLabel string }{
		{"FY2027", "FY 2027", "FY2027", "FY2028", "FY2028", "FY 2028"},
		{"2027-Q4", "Q4 2027", "FY2027-Q4", "FY2028-Q1", "2028-Q1", "Q1 2028"},
		{"2027-Q2", "", "FY2027-Q2", "FY2027-Q3", "2027-Q3", "2027-Q3"},
		// A fiscal year named by the year it ends (key FY2026 = Jul 2026 – Jun 2027).
		{"FY2027", "FY2027", "FY2026", "FY2027", "FY2028", "FY2028"},
		{"H2 2027", "H2 2027", "FY2027-H2", "FY2028-H1", "H1 2028", "H1 2028"},
		// No year in the code: the interval's own key.
		{"Year two", "Year two", "FY2027", "FY2028", "FY2028", "FY2028"},
	} {
		code, label := analogName(calMember{Code: c.code, Label: c.label}, c.peerKey, c.key)
		if code != c.wantCode || label != c.wantLabel {
			t.Errorf("%s (%s) from %s to %s: %q %q, want %q %q", c.code, c.label, c.peerKey, c.key, code, label, c.wantCode, c.wantLabel)
		}
	}
}

func TestCalendarReadsItsGrouping(t *testing.T) {
	d := func(s string) *time.Time { v, _ := time.Parse("2006-01-02", s); return &v }
	members := []calMember{{ID: "fy26", Code: "FY2026"}, {ID: "fy27", Code: "FY2027"}}
	add := func(id, code, parent string) {
		members = append(members, calMember{ID: id, Code: code, ParentID: parent})
	}
	for y, fy := range map[int]string{2026: "fy26", 2027: "fy27"} {
		for q := 1; q <= 4; q++ {
			if y == 2027 && q > 2 {
				break // FY2027 is half filled: still a year
			}
			qid := fy + "q" + string(rune('0'+q))
			add(qid, "", fy)
			for m := 0; m < 3; m++ {
				start := time.Date(y, time.Month((q-1)*3+m+1), 1, 0, 0, 0, 0, time.UTC)
				end := start.AddDate(0, 1, -1)
				members = append(members, calMember{ID: start.Format("2006-01"), Code: start.Format("2006-01"), ParentID: qid, Start: d(start.Format("2006-01-02")), End: d(end.Format("2006-01-02"))})
			}
		}
	}
	cfg := timedim.Config{Type: timedim.TypeTime, Granularity: timedim.GranMonth, FiscalYearStartMonth: 1}
	c := newCalendar(members)
	if got := c.lastLeaf().Code; got != "2027-06" {
		t.Fatalf("last period %s", got)
	}
	if lvl, ok := c.levelAt(0, cfg); !ok || lvl != timedim.LevelYear {
		t.Errorf("depth 0 groups by %v %v, want years", lvl, ok)
	}
	if lvl, ok := c.levelAt(1, cfg); !ok || lvl != timedim.LevelQuarter {
		t.Errorf("depth 1 groups by %v %v, want quarters", lvl, ok)
	}
}

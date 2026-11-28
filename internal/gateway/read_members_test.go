package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// A grid following the dashboard's selection names the members it shows on
// an axis (members): a read served from stored rows alone reads only those —
// the same cells for them, none for the others (a project's activity grid
// read the portfolio's 1,200 activities and showed eleven, 2026-11-27). A
// scoped read (a pin) takes the dimension whole, as before.
func TestReadNarrowedToShownMembers(t *testing.T) {
	f := setupRestrictedFixture(t)
	f.hide() // no rules: the read is served from stored rows
	shown := map[string]bool{"UK": true, "DE": true}
	read := func(members, scope string) gridRead {
		t.Helper()
		path := "/api/grid?grid_def_id=" + f.plan + "&members=" + url.QueryEscape(members)
		if scope != "" {
			path += "&scope=" + url.QueryEscape(scope)
		}
		status, raw := f.req("GET", path, f.viewer, nil)
		if status != http.StatusOK {
			t.Fatalf("grid %s: %d %s", path, status, raw)
		}
		var g gridRead
		if err := json.Unmarshal(raw, &g); err != nil {
			t.Fatal(err)
		}
		return g
	}
	regionAt := func(key string) string {
		parts := strings.Split(key, ":")
		id := parts[0]
		for i, d := range f.dims[id] {
			if d == f.region && 1+i < len(parts) {
				return parts[1+i]
			}
		}
		return "" // not dimensioned by region
	}

	full := f.grid(f.viewer, f.plan, "")
	narrowed := read(fmt.Sprintf(`{%q:["UK","DE"]}`, f.region), "")
	compared := 0
	for k, want := range full.Cells {
		r := regionAt(k)
		if r != "" && !shown[r] {
			continue
		}
		compared++
		if got, ok := narrowed.Cells[k]; !ok || got != want {
			t.Errorf("%s = %v (present=%v), the whole read %v", k, got, ok, want)
		}
	}
	if compared == 0 {
		t.Fatal("no cell of UK or DE compared")
	}
	for k := range narrowed.Cells {
		if r := regionAt(k); r != "" && !shown[r] {
			t.Errorf("%s: a region the grid does not show was read", k)
		}
	}

	// Under a pin, the metrics served as stored are read at the shown members
	// only (what is worked out reads its inputs whole): every shown cell is
	// the pinned read's, and nothing is added.
	pin := fmt.Sprintf(`{%q:"2026-02"}`, f.period)
	pinned := f.grid(f.viewer, f.plan, pin)
	withMembers := read(fmt.Sprintf(`{%q:["UK"]}`, f.region), pin)
	compared = 0
	for k, want := range pinned.Cells {
		if r := regionAt(k); r != "" && r != "UK" {
			continue
		}
		compared++
		if got, ok := withMembers.Cells[k]; !ok || got != want {
			t.Errorf("under a pin: %s = %v (present=%v), %v without members", k, got, ok, want)
		}
	}
	if compared == 0 {
		t.Fatal("under a pin: no UK cell compared")
	}
	for k, got := range withMembers.Cells {
		if want, ok := pinned.Cells[k]; !ok || got != want {
			t.Errorf("under a pin: %s = %v, the pinned read has %v (present=%v)", k, got, want, ok)
		}
	}
	narrowedServed := false
	for k := range pinned.Cells {
		if r := regionAt(k); r != "" && r != "UK" {
			if _, ok := withMembers.Cells[k]; !ok {
				narrowedServed = true
				break
			}
		}
	}
	if !narrowedServed {
		t.Error("under a pin: no metric was narrowed to the shown members")
	}
	// A malformed list is no narrowing.
	if bad := read(`{"x":`, ""); len(bad.Cells) != len(full.Cells) {
		t.Errorf("malformed members: %d cells, the whole read %d", len(bad.Cells), len(full.Cells))
	}
}

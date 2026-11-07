package gateway

import (
	"encoding/json"
	"net/http"
	"testing"
)

// A business admin extends the model's calendar (owner decision,
// 2026-11-07): the new months go under new quarters and a new fiscal year,
// named as the existing ones are. A developer keeps their own tools; a
// business user may not.
func TestBusinessAdminExtendsTheCalendar(t *testing.T) {
	d := setupSalesDemo(t)
	dimID := idOf(d.call("POST", "/api/developer/dimensions", d.dev, map[string]any{
		"name": "Month", "revision_id": d.revID, "dimension_type": "time",
		"time_granularity": "month", "fiscal_year_start_month": 1,
	}))
	fy := idOf(d.call("POST", "/api/developer/dimensions/"+dimID+"/members", d.dev, map[string]any{"code": "FY2026", "label": "FY 2026"}))
	for q, months := range [][2]string{{"2026-01-01", "2026-03-31"}, {"2026-04-01", "2026-06-30"}, {"2026-07-01", "2026-09-30"}, {"2026-10-01", "2026-12-31"}} {
		qid := idOf(d.call("POST", "/api/developer/dimensions/"+dimID+"/members", d.dev, map[string]any{
			"code": "2026-Q" + string(rune('1'+q)), "label": "Q" + string(rune('1'+q)) + " 2026", "parent_member_id": fy,
		}))
		d.call("POST", "/api/developer/dimensions/"+dimID+"/members/generate", d.dev, map[string]any{
			"start": months[0], "end": months[1], "parent_member_id": qid,
		})
	}

	status, raw := d.req("GET", "/api/business-admin/calendars", d.admin, nil)
	var cals []calendarRow
	if status != http.StatusOK || json.Unmarshal(raw, &cals) != nil {
		t.Fatalf("calendars: %d %s", status, raw)
	}
	var found *calendarRow
	for i := range cals {
		if cals[i].ID == dimID {
			found = &cals[i]
		}
	}
	if found == nil || found.Last != "2026-12-31" || found.Periods != 12 {
		t.Fatalf("calendars %+v, want Month through 2026-12-31 with 12 periods", cals)
	}

	path := "/api/business-admin/calendars/" + dimID + "/extend"
	if status, raw := d.req("POST", path, d.dev, map[string]string{"through": "2027-06-30"}); status != http.StatusForbidden {
		t.Errorf("developer on the business admin route: %d %s, want 403", status, raw)
	}
	if status, raw := d.req("POST", path, d.westRep, map[string]string{"through": "2027-06-30"}); status != http.StatusForbidden {
		t.Errorf("business user: %d %s, want 403", status, raw)
	}
	res := d.call("POST", path, d.admin, map[string]string{"through": "2027-06-30"})
	if res["periods"] != float64(6) || res["aggregates"] != float64(3) {
		t.Fatalf("extend: %v, want 6 months under FY2027, its Q1 and Q2", res)
	}
	var chain string
	if err := d.pool.QueryRow(t.Context(), `
		SELECT m.label || ' < ' || q.code || ' ' || q.label || ' < ' || y.code || ' ' || y.label
		FROM model.dimension_member m
		JOIN model.dimension_member q ON q.id = m.parent_member_id
		JOIN model.dimension_member y ON y.id = q.parent_member_id
		WHERE m.dimension_id = $1::uuid AND m.code = '2027-05'`, dimID).Scan(&chain); err != nil {
		t.Fatalf("2027-05: %v", err)
	}
	if chain != "May 2027 < 2027-Q2 Q2 2027 < FY2027 FY 2027" {
		t.Errorf("2027-05 sits at %q", chain)
	}
	if status, raw := d.req("POST", path, d.admin, map[string]string{"through": "2027-03-31"}); status != http.StatusBadRequest {
		t.Errorf("a date the calendar already reaches: %d %s, want 400", status, raw)
	}
	if status, raw := d.req("POST", "/api/business-admin/calendars/"+d.geoDim+"/extend", d.admin, map[string]string{"through": "2027-12-31"}); status != http.StatusNotFound {
		t.Errorf("a standard dimension: %d %s, want 404", status, raw)
	}
}

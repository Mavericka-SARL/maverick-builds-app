// An access rule keeps applying through the routine developer edits that
// change a row's name: renaming a dimension, a member code or a metric.
// Rules resolve by LINEAGE (migration 099) against whichever revision is
// read (writeguard.RulesForRevision / accessRule): every revision copy
// carries a row's lineage_id and a rename never changes it, so the old
// revision and the renamed active one both stay restricted. A member or
// metric deleted and re-added is a NEW lineage: the old revisions' copies
// stay restricted, the re-added row is unrestricted until an admin sets a
// rule on it. These tests drive the edits over HTTP.
package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// idsIn returns, for revision rev, the region dimension's ID (whatever it
// is called now), member code -> ID and metric name -> ID.
func (f *restrictedFixture) idsIn(rev string) (dim string, members, metrics map[string]string) {
	f.t.Helper()
	ctx := context.Background()
	if err := f.pool.QueryRow(ctx,
		`SELECT id::text FROM model.dimension_def WHERE revision_id=$1::uuid`, rev).Scan(&dim); err != nil {
		f.t.Fatalf("dimension in %s: %v", rev, err)
	}
	members, metrics = map[string]string{}, map[string]string{}
	rows, err := f.pool.Query(ctx, `SELECT code, id::text FROM model.dimension_member WHERE dimension_id=$1::uuid`, dim)
	if err != nil {
		f.t.Fatal(err)
	}
	for rows.Next() {
		var c, id string
		if err := rows.Scan(&c, &id); err != nil {
			f.t.Fatal(err)
		}
		members[c] = id
	}
	rows.Close()
	rows, err = f.pool.Query(ctx, `SELECT name, id::text FROM model.metric_def WHERE revision_id=$1::uuid`, rev)
	if err != nil {
		f.t.Fatal(err)
	}
	for rows.Next() {
		var n, id string
		if err := rows.Scan(&n, &id); err != nil {
			f.t.Fatal(err)
		}
		metrics[n] = id
	}
	rows.Close()
	return dim, members, metrics
}

func (f *restrictedFixture) planIn(rev string) string {
	f.t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT id::text FROM model.grid_def WHERE revision_id=$1::uuid AND name='Plan'`, rev).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

// viewerWrite writes revenue (or another metric) at code into rev as the
// viewer and returns the status.
func (f *restrictedFixture) viewerWrite(rev, metricID, dim, code string) int {
	f.t.Helper()
	status, _ := f.req("POST", "/api/cells", f.viewer, map[string]any{"model_id": f.modelID, "metric_id": metricID,
		"revision_id": rev, "dim_codes": map[string]string{dim: code}, "value": 999})
	return status
}

// assertOldRevisionRestricted checks revision A (the fixture's original
// revision, no longer active): grid, totals and access_rules apply US
// hidden, DE read-only and cost hidden; /api/metrics omits cost; writes to
// DE, US and cost are refused.
func assertOldRevisionRestricted(f *restrictedFixture, label, revA string) {
	f.t.Helper()
	q := "grid_def_id=" + f.plan + "&revision_id=" + revA
	assertRestrictedGrid(f, label+": old A grid", f.gridAt(f.viewer, q), f.members, f.metric, false)
	assertRestrictedGrid(f, label+": old A totals_only", f.gridAt(f.viewer, q+"&totals_only=1"), f.members, f.metric, true)
	_, raw := f.req("GET", "/api/metrics?revision_id="+revA, f.viewer, nil)
	if strings.Contains(string(raw), `"cost"`) {
		f.t.Errorf("%s: /api/metrics of old A lists hidden metric cost", label)
	}
	for _, w := range []struct{ metric, code string }{{"revenue", "DE"}, {"revenue", "US"}, {"cost", "UK"}} {
		if s := f.viewerWrite(revA, f.metric[w.metric], f.region, w.code); s < 400 {
			f.errf(label, "viewer write of %s at %s into old A: status %d, want refused", w.metric, w.code, s)
		}
	}
}

func (f *restrictedFixture) errf(label, format string, args ...any) {
	f.t.Helper()
	f.t.Errorf(label+": "+format, args...)
}

// assertActiveAccess checks the active revision's grid echoes the wanted
// access for the given member / metric IDs and serves no cell of a member
// or metric that should be hidden.
func assertActiveAccess(f *restrictedFixture, label, gridID string, members, metrics map[string]string) {
	f.t.Helper()
	g := f.gridAt(f.viewer, "grid_def_id="+gridID)
	for id, want := range members {
		if got := g.AccessRules.DimMembers[id]; got != want {
			f.errf(label, "member %s access = %q, want %q", id, got, want)
		}
	}
	for id, want := range metrics {
		if got := g.AccessRules.Metrics[id]; got != want {
			f.errf(label, "metric %s access = %q, want %q", id, got, want)
		}
	}
	for k, v := range g.Cells {
		for id, want := range metrics {
			if want == "hidden" && strings.HasPrefix(k, id) {
				f.errf(label, "hidden metric served: %s = %v", k, v)
			}
		}
	}
}

// TestAccessRulesSurviveRenames: after the rules are remapped onto the
// active revision B, the developer renames B's dimension, then a member
// code, then a metric. Revision A keeps its old names; it must stay
// restricted after each rename, for reads and for writes, and B must stay
// restricted under the new names.
func TestAccessRulesSurviveRenames(t *testing.T) {
	f, _, _ := setupOldRevisionFixture(t)
	revA := f.revID
	restrictViewer(f)
	revB := newRevisionAndActivate(f)
	dimB, membersB, metricsB := f.idsIn(revB)
	gridB := f.planIn(revB)

	f.dev_("PATCH", "/api/developer/dimensions/"+dimB, map[string]any{"name": "geo"})
	assertOldRevisionRestricted(f, "dimension renamed", revA)

	f.dev_("PATCH", "/api/developer/dimensions/"+dimB+"/members/"+membersB["US"], map[string]any{"code": "USA"})
	assertOldRevisionRestricted(f, "member renamed", revA)

	f.dev_("PATCH", "/api/developer/metrics/"+metricsB["cost"], map[string]any{"name": "costs"})
	assertOldRevisionRestricted(f, "metric renamed", revA)

	// The active revision, under its new names.
	assertActiveAccess(f, "active B renamed", gridB,
		map[string]string{membersB["US"]: "hidden", membersB["DE"]: "read"},
		map[string]string{metricsB["cost"]: "hidden"})
	if s := f.viewerWrite(revB, metricsB["revenue"], dimB, "USA"); s < 400 {
		t.Errorf("viewer write at renamed USA into active B: status %d, want refused", s)
	}
	if s := f.viewerWrite(revB, metricsB["revenue"], dimB, "UK"); s >= 300 {
		t.Errorf("viewer write at UK into active B: status %d, want accepted", s)
	}
}

// TestAccessRulesFollowRenameBeforeActivation: the mirror case. The member
// is renamed in the draft B before activation, so activation cannot remap
// the rule by name and it stays on A's row; B's renamed member must still
// be restricted once B is active. And a rule written AFTER a rename, on the
// renamed row, must apply to the older revision's copy under its old code.
func TestAccessRulesFollowRenameBeforeActivation(t *testing.T) {
	f, _, _ := setupOldRevisionFixture(t)
	revA := f.revID
	restrictViewer(f)
	revB := f.dev_("POST", "/api/developer/revisions", map[string]any{"name": "Next", "source_revision_id": revA})
	dimB, membersB, metricsB := f.idsIn(revB)
	f.dev_("PATCH", "/api/developer/dimensions/"+dimB+"/members/"+membersB["US"], map[string]any{"code": "USA"})
	f.dev_("PATCH", "/api/developer/dimensions/"+dimB+"/members/"+membersB["UK"], map[string]any{"code": "GB"})
	f.dev_("PUT", "/api/developer/revisions/"+revB+"/activate", nil)
	gridB := f.planIn(revB)

	assertActiveAccess(f, "active B, renamed before activation", gridB,
		map[string]string{membersB["US"]: "hidden", membersB["DE"]: "read", membersB["UK"]: ""},
		map[string]string{metricsB["cost"]: "hidden"})
	if s := f.viewerWrite(revB, metricsB["revenue"], dimB, "USA"); s < 400 {
		t.Errorf("viewer write at USA (renamed US) into active B: status %d, want refused", s)
	}
	assertOldRevisionRestricted(f, "renamed before activation", revA)

	// A rule written now, on B's GB (membersB["UK"] — the same row, renamed;
	// A's UK), hides UK in A as well.
	f.call("PUT", "/api/business-admin/users/"+f.viewerID+"/access-rules", f.admin, map[string]any{"rules": []map[string]string{
		{"rule_type": "dimension_member", "ref_id": membersB["UK"], "access": "hidden"},
	}})
	g := f.gridAt(f.viewer, "grid_def_id="+f.plan+"&revision_id="+revA)
	for k, v := range g.Cells {
		if strings.HasSuffix(k, ":UK") {
			t.Errorf("old A serves UK after a rule on its renamed copy GB hid it: %s = %v", k, v)
		}
	}
	if got := g.AccessRules.DimMembers[f.members["UK"]]; got != "hidden" {
		t.Errorf("old A: UK access = %q, want hidden (rule written on renamed GB)", got)
	}
}

// TestAccessRulesAfterDeleteAndReAdd: the developer deletes the hidden
// member and the hidden metric in the active revision and re-adds them
// under the same code / name. The re-added rows are new lineages, so they
// are unrestricted until the business admin sets a rule on them; the old
// revision's copies stay restricted throughout — also after the business
// admin re-saves the rules exactly as listed (the deleted rows' ref_ids
// included), which must keep their lineage.
func TestAccessRulesAfterDeleteAndReAdd(t *testing.T) {
	f, _, _ := setupOldRevisionFixture(t)
	revA := f.revID
	restrictViewer(f)
	revB := newRevisionAndActivate(f)
	dimB, membersB, metricsB := f.idsIn(revB)
	gridB := f.planIn(revB)

	if status, raw := f.req("DELETE", "/api/developer/dimensions/"+dimB+"/members/"+membersB["US"], f.dev, nil); status >= 300 {
		t.Fatalf("delete member US: %d %s", status, raw)
	}
	if status, raw := f.req("DELETE", "/api/developer/metrics/"+metricsB["cost"], f.dev, nil); status >= 300 {
		t.Fatalf("delete metric cost: %d %s", status, raw)
	}
	assertOldRevisionRestricted(f, "deleted", revA)

	newUS := f.dev_("POST", "/api/developer/dimensions/"+dimB+"/members", map[string]any{"code": "US", "label": "US"})
	newCost := f.dev_("POST", "/api/developer/metrics", map[string]any{"name": "cost", "is_input": true,
		"revision_id": revB, "agg_rule": "sum", "format": "number"})
	f.dev_("POST", "/api/developer/grids/"+gridB+"/metrics/"+newCost, nil)
	var sameLineage bool
	if err := f.pool.QueryRow(context.Background(), `
		SELECT EXISTS (SELECT 1 FROM model.dimension_member a JOIN model.dimension_member b ON a.lineage_id = b.lineage_id
		               WHERE a.id = $1::uuid AND b.id = $2::uuid)
		    OR EXISTS (SELECT 1 FROM model.metric_def a JOIN model.metric_def b ON a.lineage_id = b.lineage_id
		               WHERE a.id = $3::uuid AND b.id = $4::uuid)`,
		newUS, f.members["US"], newCost, f.metric["cost"]).Scan(&sameLineage); err != nil {
		t.Fatal(err)
	}
	if sameLineage {
		t.Error("a re-added member or metric took the deleted row's lineage; want a new lineage")
	}
	assertOldRevisionRestricted(f, "deleted and re-added", revA)
	// New lineages: unrestricted in B until the admin sets a rule. DE (never
	// deleted) keeps its rule.
	assertActiveAccess(f, "active B re-added", gridB,
		map[string]string{newUS: "", membersB["DE"]: "read"},
		map[string]string{newCost: ""})
	if s := f.viewerWrite(revB, metricsB["revenue"], dimB, "US"); s >= 300 {
		t.Errorf("viewer write at re-added US into active B: status %d, want accepted (new lineage, no rule)", s)
	}

	// The business admin re-saves the rules exactly as listed (the deleted
	// rows' ref_ids included): their lineage survives, so old A stays
	// restricted, and the re-added rows stay unrestricted.
	status, raw := f.req("GET", "/api/business-admin/users/"+f.viewerID+"/access-rules", f.admin, nil)
	if status != http.StatusOK {
		t.Fatalf("list rules: %d %s", status, raw)
	}
	var listed []map[string]any
	if err := json.Unmarshal(raw, &listed); err != nil {
		t.Fatal(err)
	}
	resave := make([]map[string]any, 0, len(listed)+2)
	for _, r := range listed {
		resave = append(resave, map[string]any{"rule_type": r["rule_type"], "ref_id": r["ref_id"], "access": r["access"]})
	}
	f.call("PUT", "/api/business-admin/users/"+f.viewerID+"/access-rules", f.admin, map[string]any{"rules": resave})
	assertOldRevisionRestricted(f, "re-saved", revA)
	assertActiveAccess(f, "active B re-saved", gridB,
		map[string]string{newUS: "", membersB["DE"]: "read"},
		map[string]string{newCost: ""})

	// The admin restricts the re-added rows: B follows, A is unchanged.
	resave = append(resave,
		map[string]any{"rule_type": "dimension_member", "ref_id": newUS, "access": "hidden"},
		map[string]any{"rule_type": "metric", "ref_id": newCost, "access": "hidden"})
	f.call("PUT", "/api/business-admin/users/"+f.viewerID+"/access-rules", f.admin, map[string]any{"rules": resave})
	assertOldRevisionRestricted(f, "re-added restricted", revA)
	assertActiveAccess(f, "active B re-added restricted", gridB,
		map[string]string{newUS: "hidden", membersB["DE"]: "read"},
		map[string]string{newCost: "hidden"})
	if s := f.viewerWrite(revB, metricsB["revenue"], dimB, "US"); s < 400 {
		t.Errorf("viewer write at restricted re-added US into active B: status %d, want refused", s)
	}
	if s := f.viewerWrite(revB, newCost, dimB, "UK"); s < 400 {
		t.Errorf("viewer write of restricted re-added cost into active B: status %d, want refused", s)
	}
}

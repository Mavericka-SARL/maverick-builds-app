package gateway

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A full reload makes the grids its file writes into the file: every typed
// value of those grids goes first, the file's own metrics and the ones it
// leaves out. Another grid keeps its values, the calculated metrics of what
// was cleared are recalculated, and a reload that would clear a value a
// running approval locks is refused. It used to delete every typed value of
// the revision — a file of the Plan grid's revenue emptied the HR grid's
// headcount — and the calculated metrics of the cleared values kept their
// old ones; nothing checked the cleared values against approval locks
// (2026-11-09).
func TestFullReloadClearsItsGridsAndRecalculates(t *testing.T) {
	f := setupDevAuthoringFixture(t)
	ctx := context.Background()
	post := func(path string, body any) (int, map[string]any) {
		t.Helper()
		return f.do(t, http.MethodPost, path, body)
	}
	mk := func(path string, body map[string]any) string {
		t.Helper()
		status, out := post(path, body)
		if status != http.StatusOK {
			t.Fatalf("POST %s: %d %v", path, status, out)
		}
		id, _ := out["id"].(string)
		return id
	}
	for _, code := range []string{"DE", "FR"} {
		mk(fmt.Sprintf("/api/developer/dimensions/%s/members", f.dimID), map[string]any{"code": code, "label": code})
	}
	metric := func(name, formula string) string {
		t.Helper()
		body := map[string]any{"name": name, "is_input": formula == "", "format": "number", "revision_id": f.revID}
		if formula != "" {
			body["formula"] = formula
		}
		return mk("/api/developer/metrics", body)
	}
	cost := metric("Cost", "")
	costX2 := metric("CostX2", "Cost * 2")
	headcount := metric("Headcount", "")
	hcX2 := metric("HcX2", "Headcount * 2")
	grid := func(name string, metrics ...string) string {
		t.Helper()
		id := mk("/api/developer/grids", map[string]any{"name": name, "revision_id": f.revID})
		for _, p := range append([]string{"/dimensions/" + f.dimID}, func() []string {
			var out []string
			for _, m := range metrics {
				out = append(out, "/metrics/"+m)
			}
			return out
		}()...) {
			if status, out := post("/api/developer/grids/"+id+p, nil); status != http.StatusOK {
				t.Fatalf("assign %s: %d %v", p, status, out)
			}
		}
		return id
	}
	grid("Plan", f.metricID, cost, costX2)
	grid("HR", headcount, hcX2)

	value := func(metric, code string) (float64, bool) {
		var v float64
		err := f.pool.QueryRow(ctx, `SELECT value::float8 FROM runtime.fact_input WHERE metric_id=$1::uuid AND dim_members->>$2 = $3 ORDER BY entered_at DESC LIMIT 1`, metric, f.dimID, code).Scan(&v)
		return v, err == nil
	}
	total := func(metric string) float64 {
		var v float64
		_ = f.pool.QueryRow(ctx, `SELECT value::float8 FROM runtime.calc_result WHERE metric_id=$1::uuid AND dim_members='{}'::jsonb ORDER BY calc_at DESC LIMIT 1`, metric).Scan(&v)
		return v
	}
	waitTotal := func(metric, name string, want float64) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for total(metric) != want {
			if time.Now().After(deadline) {
				t.Fatalf("%s's total = %v, want %v", name, total(metric), want)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	upload := func(csv, mode string) (int, map[string]any) {
		t.Helper()
		return post("/api/import/upload", map[string]string{"csv": csv, "revision_id": f.revID, "import_mode": mode})
	}

	if status, out := upload("FixtureRegion,FixtureRevenue,Cost,Headcount\nDE,10,1,5\nFR,20,2,6\n", "replace"); status != http.StatusOK {
		t.Fatalf("import: %d %v", status, out)
	}
	waitTotal(costX2, "CostX2", 6)
	waitTotal(hcX2, "HcX2", 22)

	// A running approval locks FR: a full reload of Plan would clear FR's
	// revenue and cost, though its file names only DE — refused, nothing
	// cleared.
	var defID, instID string
	schema := `[{"key":"region","data_type":"Dimension member","dimension_id":"` + f.dimID + `"}]`
	if err := f.pool.QueryRow(ctx, `INSERT INTO workflow.workflow_def (application_id, name, trigger_event, steps, context_schema, status)
		VALUES ($1::uuid, 'Region approval', 'manual', '[]', $2::jsonb, 'published') RETURNING id::text`, f.appID, schema).Scan(&defID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO workflow.workflow_instance (workflow_def_id, status, context, started_by)
		VALUES ($1::uuid, 'completed', '{"region":"FR"}'::jsonb, (SELECT id FROM identity.user WHERE keycloak_sub='dev-audit-pa')) RETURNING id::text`, defID).Scan(&instID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO workflow.workflow_step (instance_id, step_def_id, status, decision, completed_at)
		VALUES ($1::uuid, 'a1', 'completed', 'approve', now())`, instID); err != nil {
		t.Fatal(err)
	}
	if status, out := upload("FixtureRegion,FixtureRevenue\nDE,11\n", "full_reload"); status != http.StatusForbidden {
		t.Fatalf("a full reload clearing a locked member's values: %d %v, want 403", status, out)
	} else if msg, _ := out["error"].(string); !strings.Contains(msg, "full reload") {
		t.Errorf("the refusal should say a full reload clears the grid: %q", msg)
	}
	if v, ok := value(cost, "FR"); !ok || v != 2 {
		t.Errorf("FR cost = %v (%v) after the refused reload, want 2 kept", v, ok)
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM workflow.workflow_instance WHERE id=$1::uuid`, instID); err != nil {
		t.Fatal(err)
	}

	// Unlocked: Plan becomes the file — DE's revenue 11, every other Plan
	// value gone — while HR keeps its headcount.
	if status, out := upload("FixtureRegion,FixtureRevenue\nDE,11\n", "full_reload"); status != http.StatusOK {
		t.Fatalf("full reload: %d %v", status, out)
	}
	if v, _ := value(f.metricID, "DE"); v != 11 {
		t.Errorf("DE revenue = %v, want 11", v)
	}
	for _, c := range []struct{ metric, name, code string }{{f.metricID, "revenue", "FR"}, {cost, "cost", "DE"}, {cost, "cost", "FR"}} {
		if _, ok := value(c.metric, c.code); ok {
			t.Errorf("%s at %s survived the full reload of its grid", c.name, c.code)
		}
	}
	for _, c := range []struct {
		code string
		want float64
	}{{"DE", 5}, {"FR", 6}} {
		if v, ok := value(headcount, c.code); !ok || v != c.want {
			t.Errorf("HR headcount at %s = %v (%v), want %v kept — another grid's value", c.code, v, ok, c.want)
		}
	}
	// The cleared cost's calculated metric follows it; HR's is untouched.
	waitTotal(costX2, "CostX2", 0)
	if v := total(hcX2); v != 22 {
		t.Errorf("HcX2's total = %v, want 22", v)
	}
}

// A closed cell takes no value from a file, but a full reload deleted the
// one it held: it checked a running approval's locks and not the metric's
// closed rules (2026-11-09). It now refuses and deletes nothing. A
// developer's Clear values still clears a closed cell, as the manual says
// (it moves values as a whole, like a revision copy).
func TestFullReloadRefusesClosedCells(t *testing.T) {
	f := setupDevAuthoringFixture(t)
	ctx := context.Background()
	post := func(path string, body any) (int, map[string]any) {
		t.Helper()
		return f.do(t, http.MethodPost, path, body)
	}
	mk := func(path string, body map[string]any) string {
		t.Helper()
		status, out := post(path, body)
		if status != http.StatusOK {
			t.Fatalf("POST %s: %d %v", path, status, out)
		}
		id, _ := out["id"].(string)
		return id
	}
	for _, code := range []string{"DE", "FR"} {
		mk(fmt.Sprintf("/api/developer/dimensions/%s/members", f.dimID), map[string]any{"code": code, "label": code})
	}
	cost := mk("/api/developer/metrics", map[string]any{"name": "Cost", "is_input": true, "format": "number", "revision_id": f.revID})
	// FR is closed: its cost is 2, so its flag calculates to 1.
	flag := mk("/api/developer/metrics", map[string]any{"name": "closed_flag", "is_input": false, "formula": "IF(Cost = 2, 1, 0)", "format": "number", "revision_id": f.revID})
	grid := mk("/api/developer/grids", map[string]any{"name": "Plan", "revision_id": f.revID})
	for _, p := range []string{"/dimensions/" + f.dimID, "/metrics/" + f.metricID, "/metrics/" + cost, "/metrics/" + flag} {
		if status, out := post("/api/developer/grids/"+grid+p, nil); status != http.StatusOK {
			t.Fatalf("assign %s: %d %v", p, status, out)
		}
	}
	upload := func(csv, mode string) (int, map[string]any) {
		t.Helper()
		return post("/api/import/upload", map[string]string{"csv": csv, "revision_id": f.revID, "import_mode": mode})
	}
	if status, out := upload("FixtureRegion,FixtureRevenue,Cost\nDE,10,1\nFR,20,2\n", "replace"); status != http.StatusOK {
		t.Fatalf("import: %d %v", status, out)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		var v float64
		_ = f.pool.QueryRow(ctx, `SELECT value::float8 FROM runtime.calc_result WHERE metric_id=$1::uuid AND dim_members->>$2 = 'FR' ORDER BY calc_at DESC LIMIT 1`, flag, f.dimID).Scan(&v)
		if v == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("closed_flag never calculated 1 at FR")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE model.metric_def SET closed_when = '[{"metric":"closed_flag","op":"=","value":1}]' WHERE id=$1::uuid`, cost); err != nil {
		t.Fatal(err)
	}
	value := func(code string) (float64, bool) {
		var v float64
		err := f.pool.QueryRow(ctx, `SELECT value::float8 FROM runtime.fact_input WHERE metric_id=$1::uuid AND dim_members->>$2 = $3 ORDER BY entered_at DESC LIMIT 1`, cost, f.dimID, code).Scan(&v)
		return v, err == nil
	}

	if status, out := upload("FixtureRegion,FixtureRevenue\nDE,11\n", "full_reload"); status != http.StatusForbidden {
		t.Errorf("a full reload clearing a closed cell: %d %v, want 403", status, out)
	} else if msg, _ := out["error"].(string); !strings.Contains(msg, "takes no input here") {
		t.Errorf("the refusal should say the cell is closed: %q", msg)
	}
	if v, ok := value("FR"); !ok || v != 2 {
		t.Errorf("FR cost = %v (%v) after the refused reload, want 2 kept", v, ok)
	}

	if status, out := post("/api/developer/metrics/"+cost+"/clear-values", map[string]any{"members": map[string][]string{f.dimID: {"DE", "FR"}}}); status != http.StatusOK {
		t.Fatalf("a developer's clear: %d %v", status, out)
	}
	if _, ok := value("FR"); ok {
		t.Error("FR cost kept after a developer's clear")
	}
	if _, ok := value("DE"); ok {
		t.Error("DE cost kept after clearing DE")
	}
}

package gateway

// A table's loader that replaces by a field (2026-11-07): loading a month's
// file again added every row again. With replace_by the run deletes the
// table's records holding the file's values of that field, posts the new
// ones, and says how many it replaced.

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestTableLoaderReplacesByField(t *testing.T) {
	f := setupRoundTripFixture(t)
	ctx := context.Background()
	dev := "rollup-test-approver"
	call := func(method, path string, body any) (int, string) {
		t.Helper()
		return doAs(t, f.rollupFixture, method, path, dev, f.appID, body)
	}
	posted := func(want float64) float64 {
		t.Helper()
		var v float64
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			if err := f.pool.QueryRow(ctx, `SELECT COALESCE(sum(value),0) FROM runtime.fact_input
				WHERE metric_id=$1::uuid AND source_ref=$2::uuid`, f.amountMetricID, f.mappingID).Scan(&v); err != nil {
				t.Fatal(err)
			}
			if v == want || time.Now().After(deadline) {
				return v
			}
		}
	}
	status, raw := call("POST", "/api/developer/integrations?revision_id="+f.workingRevID, map[string]any{
		"name": "Expenses by staff", "type": "csv_import", "target_type": "form", "target_id": f.formID,
	})
	if status != http.StatusOK {
		t.Fatalf("create integration: %d %s", status, raw)
	}
	var intg struct{ ID string }
	_ = json.Unmarshal([]byte(raw), &intg)

	// Only a field naming a slice of the table may be replaced by.
	if status, raw := call("PATCH", "/api/developer/integrations/"+intg.ID+"/config", map[string]any{"config": map[string]any{"replace_by": "amt"}}); status != http.StatusBadRequest || !strings.Contains(raw, "a load replaces by a dimension") {
		t.Fatalf("replace_by a number field: %d %s, want 400", status, raw)
	}
	if status, raw := call("PATCH", "/api/developer/integrations/"+intg.ID+"/config", map[string]any{"config": map[string]any{"replace_by": "staff"}}); status != http.StatusOK {
		t.Fatalf("replace_by staff: %d %s", status, raw)
	}
	run := func(csv, query string) (int, string) {
		t.Helper()
		return call("POST", "/api/integrations/"+intg.ID+"/run"+query, map[string]any{"csv": csv})
	}
	if status, raw := run("staff,amt,status\nSTAFF_A2,12,approved\nSTAFF_A1,5,approved\n", ""); status != http.StatusOK || !strings.Contains(raw, `"rows_replaced":0`) {
		t.Fatalf("first run: %d %s", status, raw)
	}
	if got := posted(17); got != 17 {
		t.Fatalf("posted after the first run = %v, want 17", got)
	}
	// The same staff member's file again: a dry run says what it would
	// replace, the run replaces it.
	if status, raw := run("staff,amt,status\nSTAFF_A2,3,approved\n", "?dry_run=true"); status != http.StatusOK || !strings.Contains(raw, `"rows_replaced":1`) {
		t.Fatalf("dry run: %d %s, want rows_replaced 1", status, raw)
	}
	if status, raw := run("staff,amt,status\nSTAFF_A2,3,approved\n", ""); status != http.StatusOK || !strings.Contains(raw, `"rows_replaced":1`) {
		t.Fatalf("second run: %d %s, want rows_replaced 1", status, raw)
	}
	if got := posted(8); got != 8 {
		t.Errorf("posted after the reload = %v, want 8 (5 + the corrected 3), not 20", got)
	}
	if n := countRows(t, f.rollupFixture, `SELECT count(*) FROM runtime.form_record WHERE form_id=$1::uuid`, f.formID); n != 2 {
		t.Errorf("records after the reload = %d, want 2", n)
	}
	var msg string
	if err := f.pool.QueryRow(ctx, `SELECT message FROM model.integration_run WHERE integration_id=$1::uuid ORDER BY started_at DESC LIMIT 1`, intg.ID).Scan(&msg); err != nil || msg != "replaced 1 earlier record" {
		t.Errorf("the run's message = %q (%v), want it to say what it replaced", msg, err)
	}
}

// A table's loader is offered only to someone it works for: who may add the
// table's records and write a metric they post into. A plant manager saw the
// GL loader, chose a file, and every row was refused (2026-11-07).
func TestTableLoaderIsOfferedToWhoCanLoadIt(t *testing.T) {
	f := setupRoundTripFixture(t)
	ctx := context.Background()
	dev, user := "rollup-test-approver", "rollup-test-manager"
	status, raw := doAs(t, f.rollupFixture, "POST", "/api/developer/integrations?revision_id="+f.workingRevID, dev, f.appID, map[string]any{
		"name": "Load expenses", "type": "csv_import", "target_type": "form", "target_id": f.formID,
	})
	if status != http.StatusOK {
		t.Fatalf("create integration: %d %s", status, raw)
	}
	var intg struct{ ID string }
	_ = json.Unmarshal([]byte(raw), &intg)
	mayRun := func() bool {
		t.Helper()
		status, raw := doAs(t, f.rollupFixture, "GET", "/api/integrations?revision_id="+f.workingRevID, user, f.appID, nil)
		if status != http.StatusOK {
			t.Fatalf("list: %d %s", status, raw)
		}
		var list []struct {
			ID     string `json:"id"`
			MayRun *bool  `json:"may_run"`
		}
		_ = json.Unmarshal([]byte(raw), &list)
		for _, it := range list {
			if it.ID == intg.ID {
				return it.MayRun == nil || *it.MayRun
			}
		}
		t.Fatalf("the loader is not listed: %s", raw)
		return false
	}
	if !mayRun() {
		t.Fatal("before any rule the manager may run the loader")
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, 'metric', $2::uuid, 'read')`,
		f.managerID, f.amountMetricID); err != nil {
		t.Fatal(err)
	}
	if mayRun() {
		t.Error("the table posts only into a metric that is read-only for the manager: the loader is still offered")
	}
}

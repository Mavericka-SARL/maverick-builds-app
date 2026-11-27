package gateway

import (
	"context"

	"encoding/json"
	"github.com/mavericks-engine/mavericks/internal/modeltransfer"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
	"net/http"
	"strings"
	"testing"
	"time"
)

// A record_updated rule (migration 199) fires when a record of its table
// changes — for a rule watching fields, only when one of them does — with
// the changed fields and their old and new values; a misspelt watched field
// is refused, and nothing fires when nothing changed.
func TestRecordUpdatedRuleFiresForWatchedFields(t *testing.T) {
	f := setupRoundTripFixture(t)
	ctx := context.Background()
	dev := "rollup-test-approver"
	call := func(method, path string, body any) (int, string) {
		t.Helper()
		return doAs(t, f.rollupFixture, method, path, dev, f.appID, body)
	}
	rules := "/api/automation/rules?revision_id=" + f.workingRevID
	rule := map[string]any{"name": "Amount changed", "trigger_type": "record_updated", "workflow_name": "Expense Approval",
		"workflow_def_id": f.wfID, "source_form_id": f.formID, "watch_fields": []string{"amount"}}
	if st, raw := call("POST", rules, rule); st != http.StatusBadRequest || !strings.Contains(raw, `amount\" is no field of Expense (its fields: amt, staff, status)`) {
		t.Fatalf("a misspelt watched field: %d %s, want 400", st, raw)
	}
	rule["watch_fields"] = []string{"amt"}
	st, raw := call("POST", rules, rule)
	if st != http.StatusOK && st != http.StatusCreated {
		t.Fatalf("create the rule: %d %s", st, raw)
	}
	var created struct {
		ID          string   `json:"id"`
		WatchFields []string `json:"watch_fields"`
	}
	_ = json.Unmarshal([]byte(raw), &created)
	if created.ID == "" || len(created.WatchFields) != 1 || created.WatchFields[0] != "amt" {
		t.Fatalf("created rule: %s", raw)
	}

	st, raw = call("POST", "/api/forms/"+f.formID+"/records", map[string]any{"data": map[string]any{"staff": "STAFF_A1", "amt": 5}})
	if st != http.StatusOK && st != http.StatusCreated {
		t.Fatalf("create a record: %d %s", st, raw)
	}
	var rec struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal([]byte(raw), &rec)
	runs := func() []map[string]string {
		rows, err := f.pool.Query(ctx, `SELECT trigger_payload FROM workflow.execution WHERE rule_id = $1::uuid ORDER BY started_at`, created.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []map[string]string
		for rows.Next() {
			var b []byte
			if err := rows.Scan(&b); err != nil {
				t.Fatal(err)
			}
			var p map[string]string
			_ = json.Unmarshal(b, &p)
			out = append(out, p)
		}
		return out
	}
	waitRuns := func(n int) []map[string]string {
		for deadline := time.Now().Add(10 * time.Second); len(runs()) < n && time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		}
		time.Sleep(300 * time.Millisecond) // nothing more on its way
		return runs()
	}
	save := func(data map[string]any) {
		t.Helper()
		if st, raw := call("PUT", "/api/records/"+rec.ID, map[string]any{"data": data}); st != http.StatusOK {
			t.Fatalf("save %v: %d %s", data, st, raw)
		}
	}

	save(map[string]any{"staff": "STAFF_A2", "amt": 5}) // a field the rule does not watch
	save(map[string]any{"staff": "STAFF_A2", "amt": 5}) // nothing changed
	if got := waitRuns(0); len(got) != 0 {
		t.Fatalf("runs after changing only staff and saving unchanged: %v, want none", got)
	}
	save(map[string]any{"staff": "STAFF_A2", "amt": 9})
	got := waitRuns(1)
	if len(got) != 1 {
		t.Fatalf("runs after changing amt: %d, want 1 (%v)", len(got), got)
	}
	if p := got[0]; p["changed_fields"] != "amt" || p["old_amt"] != "5" || p["new_amt"] != "9" || p["record_id"] != rec.ID {
		t.Errorf("the run's payload: %v, want changed_fields amt, old_amt 5, new_amt 9", p)
	}

	// A copy of the revision keeps the rule's watched fields, and the
	// model's package carries them as version 5 (migration 199).
	h := &handler{db: tenantdb.NewHandle(f.pool, nil), log: logger.New("test")}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	copyID, err := h.duplicateRevision(ctx, tx, f.modelID, "Copy", f.workingRevID, &f.workingRevID)
	if err != nil {
		t.Fatalf("duplicateRevision: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var watched []string
	if err := f.pool.QueryRow(ctx, `SELECT watch_fields FROM workflow.automation_rule WHERE revision_id = $1::uuid AND name = 'Amount changed'`, copyID).Scan(&watched); err != nil || len(watched) != 1 || watched[0] != "amt" {
		t.Errorf("the copy's rule watches %v (%v), want [amt]", watched, err)
	}
	pkg, err := modeltransfer.CollectExport(ctx, f.pool, f.modelID, f.workingRevID, "Working")
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	found := false
	for _, ar := range pkg.AutomationRules {
		if ar.Name == "Amount changed" && len(ar.WatchFields) == 1 && ar.WatchFields[0] == "amt" {
			found = true
		}
	}
	if !found || pkg.MinimumVersion() != 5 {
		t.Errorf("the package: rule with watched fields %v, minimum version %d, want true and 5", found, pkg.MinimumVersion())
	}
}

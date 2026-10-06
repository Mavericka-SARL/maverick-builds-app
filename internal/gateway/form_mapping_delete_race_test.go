package gateway

// A form mapping deleted while a recompute of it is still writing. The
// delete used to withdraw the mapping's totals in one statement and delete
// the mapping in another, so a recompute that had already read the mapping
// committed its total after the withdrawal, where it stayed under the id of
// a mapping that no longer existed (public CI, 2026-10-06:
// TestFormPostingsFollowRunsAndDeletes, "posted after the mapping delete =
// 17"). The delete now deletes the mapping row first, in the same
// transaction as the withdrawal, and the recompute holds that row FOR SHARE.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

func TestMappingDeleteWaitsForAnInFlightRecompute(t *testing.T) {
	f := setupRoundTripFixture(t)
	ctx := context.Background()
	sum := func() float64 {
		t.Helper()
		var v float64
		if err := f.pool.QueryRow(ctx, `SELECT COALESCE(sum(value),0) FROM runtime.fact_input WHERE source_ref=$1::uuid`, f.mappingID).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if got := sum(); got != 7 {
		t.Fatalf("fixture posting = %v, want 7", got)
	}

	// What recomputeFactInputOnce does, stopped before its commit: the
	// mapping's lock, its row FOR SHARE, and a new total in place of the old.
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`SELECT pg_advisory_xact_lock(hashtext($1))`, []any{f.mappingID}},
		{`SELECT 1 FROM model.form_metric_mapping WHERE id=$1::uuid FOR SHARE`, []any{f.mappingID}},
		{`SET LOCAL mvx.delete_reason = 'form_reposted'`, nil},
		{`DELETE FROM runtime.fact_input WHERE source_ref=$1::uuid`, []any{f.mappingID}},
		{`INSERT INTO runtime.fact_input (model_id, revision_id, revision_name, metric_id, dim_members, value, entered_by, source_ref)
		  VALUES ($1::uuid, $2::uuid, 'Working', $3::uuid, $4::jsonb, 17, $5::uuid, $6::uuid)`,
			[]any{f.modelID, f.workingRevID, f.amountMetricID, fmt.Sprintf(`{"%s":"STAFF_A1"}`, f.staffDimID), f.managerID, f.mappingID}},
	} {
		if _, err := tx.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}

	type answer struct {
		status int
		body   string
		err    error
	}
	done := make(chan answer, 1)
	go func() {
		req, err := http.NewRequestWithContext(ctx, http.MethodDelete, f.srv.URL+"/api/developer/form-integrations/"+f.mappingID, nil)
		if err != nil {
			done <- answer{err: err}
			return
		}
		req.Header.Set("X-Dev-User", "rollup-test-approver")
		req.Header.Set("X-App-Id", f.appID)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			done <- answer{err: err}
			return
		}
		defer resp.Body.Close() //nolint:errcheck
		b, _ := io.ReadAll(resp.Body)
		done <- answer{status: resp.StatusCode, body: string(b)}
	}()

	// The delete has to wait for the recompute's transaction.
	select {
	case a := <-done:
		t.Fatalf("the delete answered (%d %s %v) while a recompute of its mapping was still writing", a.status, a.body, a.err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case a := <-done:
		if a.err != nil || a.status != http.StatusOK {
			t.Fatalf("delete mapping: %d %s %v", a.status, a.body, a.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the delete did not answer once the recompute committed")
	}
	if got := sum(); got != 0 {
		t.Errorf("posted after the mapping delete = %v, want 0: the in-flight recompute's total outlived its mapping", got)
	}

	// A recompute that starts after the delete finds the mapping gone and
	// writes nothing.
	h := &handler{db: tenantdb.NewHandle(f.pool, nil), log: logger.New("test")}
	h.recomputeFactInput(ctx, f.mappingID, f.workingRevID, "")
	if got := sum(); got != 0 {
		t.Errorf("posted by a recompute after the delete = %v, want 0", got)
	}
}

package gateway

import (
	"testing"
	"time"
)

// TestActivationRecalcIsRecordedPending: activating a revision recomputes
// every calculated metric after the request answers; that pass is recorded
// before the answer (runtime.revision_recalc_state), so the grid and the KPI
// tiles read recalc_pending from the first read on and mark interim totals —
// and the mark lifts when the pass lands. It used to run unrecorded: tiles
// showed half-computed figures (EBITDA equal to gross profit) as final.
func TestActivationRecalcIsRecordedPending(t *testing.T) {
	f := setupRestrictedFixture(t)
	revB := f.dev_("POST", "/api/developer/revisions", map[string]any{"name": "Next", "source_revision_id": f.revID})
	f.dev_("PUT", "/api/developer/revisions/"+revB+"/activate", nil)

	var requested, done int64
	if err := f.pool.QueryRow(t.Context(), `SELECT requested, done FROM runtime.revision_recalc_state WHERE revision_id=$1::uuid`,
		revB).Scan(&requested, &done); err != nil {
		t.Fatalf("activation recorded no pending recalculation for the revision: %v", err)
	}
	if requested < 1 {
		t.Fatalf("requested = %d after activation", requested)
	}
	deadline := time.Now().Add(30 * time.Second)
	for done < requested && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		_ = f.pool.QueryRow(t.Context(), `SELECT requested, done FROM runtime.revision_recalc_state WHERE revision_id=$1::uuid`,
			revB).Scan(&requested, &done)
	}
	if done < requested {
		t.Fatalf("the activation's recalculation never landed: requested %d, done %d", requested, done)
	}
}

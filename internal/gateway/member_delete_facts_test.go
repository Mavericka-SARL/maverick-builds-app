package gateway

import (
	"context"
	"net/http"
	"testing"
)

// TestMemberDeleteRemovesItsFacts: deleting a member takes its input facts
// with it (kept in fact_input_history as 'member_deleted'), so the grid no
// longer serves a cell for a code no row shows, and the other members' facts
// are untouched. Before, the facts stayed and every reader that builds cells
// from fact rows kept serving them.
func TestMemberDeleteRemovesItsFacts(t *testing.T) {
	f := setupDimFormulaFixture(t)
	ctx := context.Background()
	f.writeCell("revenue", "EMEA", 100)
	f.writeCell("revenue", "US", 50)
	if got := f.gridCells(f.gridID)[cellKey(f.metric["revenue"], "US")]; got != 50 {
		t.Fatalf("before the delete: revenue US = %v, want 50", got)
	}

	path := "/api/developer/dimensions/" + f.region + "/members/" + f.members["US"]
	if status, raw := f.req("DELETE", path, nil); status != http.StatusOK {
		t.Fatalf("DELETE US: status %d %s", status, raw)
	}

	cells := f.gridCells(f.gridID)
	if v, ok := cells[cellKey(f.metric["revenue"], "US")]; ok {
		t.Errorf("the grid still serves revenue at the deleted member US (%v)", v)
	}
	if got := cells[cellKey(f.metric["revenue"], "EMEA")]; got != 100 {
		t.Errorf("revenue EMEA = %v, want 100 (another member's facts must stay)", got)
	}

	var left, kept int
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM runtime.fact_input WHERE model_id=$1::uuid AND dim_members->>$2 = 'US'
	`, f.modelID, f.region).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("%d fact row(s) at US survived the member delete", left)
	}
	if err := f.pool.QueryRow(ctx, `
		SELECT count(*) FROM runtime.fact_input_history
		WHERE model_id=$1::uuid AND dim_members->>$2 = 'US' AND delete_reason = 'member_deleted'
	`, f.modelID, f.region).Scan(&kept); err != nil {
		t.Fatal(err)
	}
	if kept == 0 {
		t.Error("the deleted facts were not kept in fact_input_history with reason member_deleted")
	}
}

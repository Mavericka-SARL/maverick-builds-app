package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/modeledit"
	"github.com/mavericks-engine/mavericks/pkg/logger"
	"github.com/mavericks-engine/mavericks/pkg/tenantdb"
)

// A dashboard's selector bar (model.dashboard_def.selector_bar): set by a
// developer, validated against the dashboard's revision, served to the
// people who open the dashboard, and carried by every copy of it.
func TestDashboardSelectorBar(t *testing.T) {
	f := setupRollupFixture(t)
	ctx := context.Background()
	dev, user := "rollup-test-approver", "rollup-test-manager"

	var dashID string
	if err := f.pool.QueryRow(ctx,
		`INSERT INTO model.dashboard_def (model_id, name, revision_id) VALUES ($1::uuid, 'Scenarios', $2::uuid) RETURNING id::text`,
		f.modelID, f.workingRevID).Scan(&dashID); err != nil {
		t.Fatalf("seed dashboard: %v", err)
	}
	path := "/api/developer/dashboards/" + dashID
	bar := func(id string) string {
		t.Helper()
		var raw string
		if err := f.pool.QueryRow(ctx, `SELECT selector_bar::text FROM model.dashboard_def WHERE id=$1::uuid`, id).Scan(&raw); err != nil {
			t.Fatalf("read bar: %v", err)
		}
		return raw
	}

	// A dimension named by name is stored by id; the default is kept.
	status, body := f.do(t, "PATCH", path, dev, map[string]any{
		"selector_bar": []map[string]string{{"dimension_id": "departments", "default": "DEPT_B"}, {"dimension_id": f.staffDimID}},
	})
	if status != http.StatusOK {
		t.Fatalf("set bar: %d %v", status, body)
	}
	var got []modeledit.SelectorBarEntry
	_ = json.Unmarshal([]byte(bar(dashID)), &got)
	if len(got) != 2 || got[0].DimensionID != f.deptsDimID || got[0].Default != "DEPT_B" || got[1].DimensionID != f.staffDimID || got[1].Default != "" {
		t.Fatalf("stored bar = %+v", got)
	}

	// Refused: an unknown dimension, a member code the dimension lacks, a
	// dimension twice, a dimension of another revision. The bar is unchanged.
	var otherRevDeptsID string
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='departments'`,
		f.modelID, f.annualRevID).Scan(&otherRevDeptsID); err != nil {
		otherRevDeptsID = ""
	}
	bad := map[string][]map[string]string{
		"unknown dimension": {{"dimension_id": "no_such_dimension"}},
		"unknown member":    {{"dimension_id": f.deptsDimID, "default": "NOPE"}},
		"twice":             {{"dimension_id": f.deptsDimID}, {"dimension_id": "departments"}},
	}
	if otherRevDeptsID != "" {
		bad["other revision"] = []map[string]string{{"dimension_id": otherRevDeptsID}}
	}
	before := bar(dashID)
	for name, entries := range bad {
		if status, body := f.do(t, "PATCH", path, dev, map[string]any{"selector_bar": entries}); status != http.StatusBadRequest {
			t.Errorf("%s: status %d (%v), want 400", name, status, body)
		}
	}
	if after := bar(dashID); after != before {
		t.Errorf("a refused bar changed the stored one: %s -> %s", before, after)
	}

	// A rename leaves the bar alone; the developer list serves it.
	if status, body := f.do(t, "PATCH", path, dev, map[string]any{"name": "Scenario comparison"}); status != http.StatusOK {
		t.Fatalf("rename: %d %v", status, body)
	}
	if after := bar(dashID); after != before {
		t.Errorf("rename changed the bar: %s -> %s", before, after)
	}
	_, list := doAs(t, f, "GET", "/api/developer/dashboards?revision_id="+f.workingRevID, dev, f.appID, nil)
	var listed []struct {
		ID          string                       `json:"id"`
		SelectorBar []modeledit.SelectorBarEntry `json:"selector_bar"`
	}
	_ = json.Unmarshal([]byte(list), &listed)
	if len(listed) != 1 || len(listed[0].SelectorBar) != 2 || listed[0].SelectorBar[0] != (modeledit.SelectorBarEntry{DimensionID: f.deptsDimID, Default: "DEPT_B"}) {
		t.Errorf("developer list does not serve the bar: %s", list)
	}

	// The person opening the dashboard gets it too.
	status, detail := f.do(t, "GET", "/api/dashboards/"+dashID, user, nil)
	if status != http.StatusOK {
		t.Fatalf("user detail: %d %v", status, detail)
	}
	if sb, _ := detail["selector_bar"].([]any); len(sb) != 2 {
		t.Errorf("user detail selector_bar = %v, want 2 entries", detail["selector_bar"])
	}

	// A member code rename re-keys the default.
	if err := modeledit.RekeyMemberCode(ctx, f.pool, f.deptsDimID, "DEPT_B", "DEPT_BETA"); err != nil {
		t.Fatalf("rekey: %v", err)
	}
	_ = json.Unmarshal([]byte(bar(dashID)), &got)
	if got[0].Default != "DEPT_BETA" || got[1].Default != "" {
		t.Errorf("after rename bar = %+v, want departments=DEPT_BETA", got)
	}

	// A revision duplicate's dashboard names the copy's dimensions.
	h := &handler{db: tenantdb.NewHandle(f.pool, nil), log: logger.New("test")}
	tx, err := f.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	newRevID, err := h.duplicateRevision(ctx, tx, f.modelID, "Copy", f.workingRevID, &f.workingRevID)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("duplicateRevision: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	var copyBar string
	var copyDepts, copyStaff string
	if err := f.pool.QueryRow(ctx, `
		SELECT d.selector_bar::text,
		       (SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='departments'),
		       (SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='staff')
		FROM model.dashboard_def d WHERE d.model_id=$1::uuid AND d.revision_id=$2::uuid AND d.name='Scenario comparison'`,
		f.modelID, newRevID).Scan(&copyBar, &copyDepts, &copyStaff); err != nil {
		t.Fatalf("copied dashboard: %v", err)
	}
	_ = json.Unmarshal([]byte(copyBar), &got)
	if len(got) != 2 || got[0].DimensionID != copyDepts || got[0].Default != "DEPT_BETA" || got[1].DimensionID != copyStaff {
		t.Errorf("copied bar = %s, want departments %s (DEPT_BETA), staff %s", copyBar, copyDepts, copyStaff)
	}

	// [] removes it.
	if status, body := f.do(t, "PATCH", path, dev, map[string]any{"selector_bar": []any{}}); status != http.StatusOK {
		t.Fatalf("clear bar: %d %v", status, body)
	}
	if b := bar(dashID); b != "[]" {
		t.Errorf("cleared bar = %s, want []", b)
	}

	// Only a developer sets it.
	if status, _ := f.do(t, "PATCH", path, user, map[string]any{"selector_bar": []map[string]string{{"dimension_id": f.deptsDimID}}}); status != http.StatusForbidden {
		t.Errorf("business user set a bar: status %d, want 403", status)
	}
}

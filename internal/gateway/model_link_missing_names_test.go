package gateway

import (
	"context"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/integration"
)

// TestModelLinkFailsOnAMissingName: a model link names the source grid's
// metrics and filter members by text, and a rename in the source model —
// another model, whose working revision the link cannot rewrite — leaves
// it naming what is no longer there. The run fails with the names it
// cannot find, as a grid renamed away fails it; it used to succeed with the
// column dropped, or with no rows at all (the owner's decision, 2026-11-09).
func TestModelLinkFailsOnAMissingName(t *testing.T) {
	f := setupLinkFixture(t)
	ctx := context.Background()
	cfg := f.linkConfig(f.srcModel)
	cfg["model"] = map[string]any{"model_id": f.srcModel, "grid": "Sales",
		"metrics": []string{"revenue", "double"}, "filters": map[string][]string{"region": {"UK", "DE"}}}
	id := f.ok("POST", "/api/developer/integrations", linkDev, f.tgtApp, f.tgtRev,
		map[string]any{"type": "rest_api", "name": "Sales into P&L", "status": "draft", "config": cfg})
	f.ok("POST", "/api/developer/integrations/"+id+"/test", linkDev, f.tgtApp, f.tgtRev, nil)
	if run := f.runQueued(); run.Status != "success" {
		t.Fatalf("test run: %+v", run)
	}
	f.ok("PATCH", "/api/developer/integrations/"+id, linkDev, f.tgtApp, f.tgtRev, map[string]any{"status": "active"})
	f.ok("POST", "/api/integrations/"+id+"/run", linkDev, f.tgtApp, f.tgtRev, nil)
	if run := f.runQueued(); run.Status != "success" || run.RecordsRead != 2 {
		t.Fatalf("the run before any rename: %+v", run)
	}

	var srcDim, ukID, doubleID string
	if err := f.pool.QueryRow(ctx, `
		SELECT d.id::text, mb.id::text FROM model.dimension_def d JOIN model.dimension_member mb ON mb.dimension_id = d.id
		WHERE d.model_id=$1::uuid AND d.name='region' AND mb.code='UK'`, f.srcModel).Scan(&srcDim, &ukID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM model.metric_def WHERE model_id=$1::uuid AND name='double'`, f.srcModel).Scan(&doubleID); err != nil {
		t.Fatal(err)
	}

	// A filter member renamed in the source.
	f.ok("PATCH", "/api/developer/dimensions/"+srcDim+"/members/"+ukID, linkDev, f.srcApp, f.srcRev, map[string]any{"code": "GB", "label": "GB"})
	f.ok("POST", "/api/integrations/"+id+"/run", linkDev, f.tgtApp, f.tgtRev, nil)
	if run := f.runQueued(); run.Status != "failed" || run.ErrorCode != integration.ErrCodeInvalidData || !strings.Contains(run.Message, "UK") {
		t.Errorf("a run whose filter member was renamed away: %+v; want failed, %s, naming UK", run, integration.ErrCodeInvalidData)
	}
	f.ok("PATCH", "/api/developer/dimensions/"+srcDim+"/members/"+ukID, linkDev, f.srcApp, f.srcRev, map[string]any{"code": "UK", "label": "UK"})

	// A metric renamed in the source.
	f.ok("PATCH", "/api/developer/metrics/"+doubleID, linkDev, f.srcApp, f.srcRev, map[string]any{"name": "twice"})
	f.ok("POST", "/api/integrations/"+id+"/run", linkDev, f.tgtApp, f.tgtRev, nil)
	if run := f.runQueued(); run.Status != "failed" || run.ErrorCode != integration.ErrCodeInvalidData || !strings.Contains(run.Message, "double") {
		t.Errorf("a run whose metric was renamed away: %+v; want failed, %s, naming double", run, integration.ErrCodeInvalidData)
	}
}

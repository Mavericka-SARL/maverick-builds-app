package gateway

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// A model link imports with replace, which overwrote the cells a run held
// and left the rest: a value emptied in the source gave no record, so the
// target kept its last copy (2026-10-09). A run now clears the cells whose
// latest value the link wrote and its source no longer holds — in the
// target and in a revision copy of it — and keeps a cell someone typed
// over since the link wrote it.
func TestModelLinkClearsWhatTheSourceEmptied(t *testing.T) {
	f := setupLinkFixture(t)
	ctx := context.Background()
	id := f.ok("POST", "/api/developer/integrations", linkDev, f.tgtApp, f.tgtRev,
		map[string]any{"type": "rest_api", "name": "Sales into P&L", "status": "draft", "config": f.linkConfig(f.srcModel)})
	f.ok("POST", "/api/developer/integrations/"+id+"/test", linkDev, f.tgtApp, f.tgtRev, nil)
	if run := f.runQueued(); run.Status != "success" {
		t.Fatalf("test run: %+v", run)
	}
	f.ok("PATCH", "/api/developer/integrations/"+id, linkDev, f.tgtApp, f.tgtRev, map[string]any{"status": "active"})
	runLink := func(integrationID, rev string) map[string]string {
		t.Helper()
		f.ok("POST", "/api/integrations/"+integrationID+"/run", linkDev, f.tgtApp, rev, nil)
		if run := f.runQueued(); run.Status != "success" {
			t.Fatalf("run: %+v", run)
		}
		meta := map[string]string{}
		_ = f.pool.QueryRow(ctx, `SELECT meta FROM model.integration_run ORDER BY created_at DESC LIMIT 1`).Scan(&meta)
		return meta
	}
	runLink(id, f.tgtRev)
	if v, ok := f.inboundValue("rev_in", "UK"); !ok || v != 100 {
		t.Fatalf("first run: rev_in at UK = %v (%v), want 100", v, ok)
	}

	var srcDim, revenue, double string
	if err := f.pool.QueryRow(ctx, `SELECT d.id::text, r.id::text, x.id::text FROM model.dimension_def d
		JOIN model.metric_def r ON r.model_id = d.model_id AND r.name = 'revenue'
		JOIN model.metric_def x ON x.model_id = d.model_id AND x.name = 'double'
		WHERE d.model_id = $1::uuid AND d.name = 'region'`, f.srcModel).Scan(&srcDim, &revenue, &double); err != nil {
		t.Fatal(err)
	}
	// emptySource clears revenue at region in the source, and waits for
	// double to follow.
	emptySource := func(region string) {
		t.Helper()
		f.ok("POST", "/api/cells", linkDev, f.srcApp, f.srcRev, map[string]any{"model_id": f.srcModel, "metric_id": revenue,
			"revision_id": f.srcRev, "dim_codes": map[string]string{srcDim: region}, "clear": true})
		for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(50 * time.Millisecond) {
			var v float64
			err := f.pool.QueryRow(ctx, `SELECT value FROM runtime.calc_result WHERE metric_id=$1::uuid AND dim_members=$2::jsonb ORDER BY calc_at DESC LIMIT 1`,
				double, fmt.Sprintf(`{%q: %q}`, srcDim, region)).Scan(&v)
			if err != nil || v == 0 {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("double at %s never followed the emptied revenue (%v)", region, v)
			}
		}
	}

	emptySource("UK")
	meta := runLink(id, f.tgtRev)
	if v, ok := f.inboundValue("rev_in", "UK"); ok && v != 0 {
		t.Errorf("rev_in at UK = %v after the source emptied it; want it cleared", v)
	}
	if meta["cells_cleared"] == "" || meta["cells_cleared"] == "0" {
		t.Errorf("run meta = %v; want cells_cleared counted", meta)
	}
	if v, ok := f.inboundValue("rev_in", "DE"); !ok || v != 200 {
		t.Errorf("rev_in at DE = %v (%v); want the source's 200 kept", v, ok)
	}

	// A revision copy of the target keeps the link and what it wrote.
	copyRev := f.ok("POST", "/api/developer/revisions", linkDev, f.tgtApp, f.tgtRev, map[string]any{"name": "Next", "source_revision_id": f.tgtRev})
	// Typed over in the target since the link wrote it: kept.
	f.ok("POST", "/api/cells", linkDev, f.tgtApp, f.tgtRev, map[string]any{"model_id": f.tgtModel, "metric_id": f.tgtMetric["rev_in"],
		"revision_id": f.tgtRev, "dim_codes": map[string]string{f.tgtDim(): "DE"}, "value": 999})
	emptySource("DE")
	runLink(id, f.tgtRev)
	if v, ok := f.inboundValue("rev_in", "DE"); !ok || v != 999 {
		t.Errorf("rev_in at DE = %v (%v); want the 999 typed over the link's value kept", v, ok)
	}
	var copyID string
	if err := f.pool.QueryRow(ctx, `SELECT c.id::text FROM model.integration_def c JOIN model.integration_def o ON o.link_id = c.link_id
		WHERE o.id = $1::uuid AND c.revision_id = $2::uuid`, id, copyRev).Scan(&copyID); err != nil {
		t.Fatalf("the link's revision copy: %v", err)
	}
	runLink(copyID, copyRev)
	var left int
	if err := f.pool.QueryRow(ctx, `
		WITH latest AS (SELECT DISTINCT ON (metric_id, dim_members) metric_id, value FROM runtime.fact_input
			WHERE revision_id = $1::uuid AND source_ref IS NULL ORDER BY metric_id, dim_members, entered_at DESC, id DESC)
		SELECT count(*) FROM latest l JOIN model.metric_def m ON m.id = l.metric_id WHERE m.name = 'rev_in' AND l.value <> 0`, copyRev).Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left != 0 {
		t.Errorf("the revision copy holds %d rev_in value(s) its source emptied; want none", left)
	}
}

// tgtDim is the target model's region dimension.
func (f *linkFixture) tgtDim() string {
	f.t.Helper()
	var id string
	if err := f.pool.QueryRow(context.Background(), `SELECT id::text FROM model.dimension_def WHERE model_id=$1::uuid AND revision_id=$2::uuid AND name='region'`,
		f.tgtModel, f.tgtRev).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

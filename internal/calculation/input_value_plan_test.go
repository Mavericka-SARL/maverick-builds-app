package calculation_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/calculation"
)

// TestInputValueMapReadsFactsOnceUnderGenericPlan guards LoadInputValueMap
// against the plan that made a recalculation 4.4s instead of 0.8s
// (2026-10-19): pgx caches its statements, Postgres switches a cached
// statement to a generic plan, and the generic plan of a NOT EXISTS
// correlated to fact_input rescanned the metric's facts once per combo.
// The plan is checked, not a timing: no scan of the facts may run more than
// once, whatever the statistics say.
func TestInputValueMapReadsFactsOnceUnderGenericPlan(t *testing.T) {
	store, cleanup := setupDB(t)
	defer cleanup()
	ctx := context.Background()
	pool := store.Pool()

	modelID := "00000000-0000-0000-0000-0000000000a1"
	revisionID := "00000000-0000-0000-0001-0000000000a1"
	var metricID, mappingID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO model.metric_def (model_id, revision_id, name, is_input)
		VALUES ($1::uuid, $2::uuid, 'plan_pct', true) RETURNING id::text`, modelID, revisionID).Scan(&metricID); err != nil {
		t.Fatalf("insert metric: %v", err)
	}
	// 400 typed combos, then a form 'replace' posting that covers one of them.
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value)
		SELECT $1::uuid, $2::uuid, $3::uuid, jsonb_build_object('month', 'M' || g), g
		FROM generate_series(1, 400) g`, modelID, revisionID, metricID); err != nil {
		t.Fatalf("insert facts: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO model.form_metric_mapping (aggregation) VALUES ('replace') RETURNING id::text`).Scan(&mappingID); err != nil {
		t.Fatalf("insert mapping: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO runtime.fact_input (model_id, revision_id, metric_id, dim_members, value, source_ref)
		VALUES ($1::uuid, $2::uuid, $3::uuid, '{"month":"M1"}', 99, $4::uuid)`, modelID, revisionID, metricID, mappingID); err != nil {
		t.Fatalf("insert form posting: %v", err)
	}

	// The values are what the grid shows: the form's 99 replaces the typed 1.
	values, err := store.LoadInputValueMap(ctx, modelID, revisionID, metricID)
	if err != nil {
		t.Fatalf("LoadInputValueMap: %v", err)
	}
	if len(values) != 400 || values[`{"month":"M1"}`] != 99 || values[`{"month":"M2"}`] != 2 {
		t.Fatalf("values: %d combos, M1=%v M2=%v; want 400, 99, 2", len(values), values[`{"month":"M1"}`], values[`{"month":"M2"}`])
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET plan_cache_mode = force_generic_plan`); err != nil {
		t.Fatalf("set plan_cache_mode: %v", err)
	}
	defer conn.Exec(ctx, `RESET plan_cache_mode`) //nolint:errcheck
	if _, err := conn.Exec(ctx, `PREPARE input_map(uuid, uuid, uuid) AS `+calculation.InputValueMapSQL); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer conn.Exec(ctx, `DEALLOCATE input_map`) //nolint:errcheck
	var raw []byte
	if err := conn.QueryRow(ctx, fmt.Sprintf(`EXPLAIN (ANALYZE, FORMAT JSON) EXECUTE input_map('%s', '%s', '%s')`,
		modelID, revisionID, metricID)).Scan(&raw); err != nil {
		t.Fatalf("explain: %v", err)
	}
	var plans []struct {
		Plan planNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &plans); err != nil || len(plans) != 1 {
		t.Fatalf("plan: %v (%s)", err, raw)
	}
	scans := 0
	var walk func(n planNode)
	walk = func(n planNode) {
		if n.Relation == "fact_input" || n.Relation == "fact_input_default" {
			scans++
			if n.Loops > 1 {
				t.Errorf("%s on %s runs %d times: the facts are rescanned per combo", n.Type, n.Relation, n.Loops)
			}
		}
		for _, c := range n.Plans {
			walk(c)
		}
	}
	walk(plans[0].Plan)
	if scans == 0 {
		t.Fatalf("no scan of fact_input in the plan: %s", raw)
	}
}

type planNode struct {
	Type     string     `json:"Node Type"`
	Relation string     `json:"Relation Name"`
	Loops    int        `json:"Actual Loops"`
	Plans    []planNode `json:"Plans"`
}

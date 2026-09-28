package integration_test

import (
	"context"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/calculation"
	"github.com/mavericks-engine/mavericks/internal/integration"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	"github.com/mavericks-engine/mavericks/pkg/logger"

	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// A connector pull into a dimension changes member properties that
// formulas read (region.factor); those references have no calc_dependency
// edge, so the commit itself must recompute the metric (contract C8).
func TestCommitDimensionRecalculatesMetricsReadingIt(t *testing.T) {
	pool := testdb.New(t, migrationfs.FS, ".")
	ctx := context.Background()
	q := func(sql string, args ...any) string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
		return id
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
	}
	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('DimCo','enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid,'ws') RETURNING id::text`, custID)
	appID := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid,$2::uuid,'App','planning') RETURNING id::text`, wsID, custID)
	modelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid,'M') RETURNING id::text`, appID)
	revID := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid,'Working') RETURNING id::text`, modelID)
	region := q(`INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type) VALUES ($1::uuid,$2::uuid,'region','standard') RETURNING id::text`, modelID, revID)
	exec(`INSERT INTO model.dimension_property (dimension_id, name, data_type) VALUES ($1::uuid,'factor','number')`, region)
	exec(`INSERT INTO model.dimension_member (dimension_id, code, label, properties) VALUES ($1::uuid,'EMEA','EMEA','{"factor":"2"}')`, region)
	metricID := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, formula) VALUES ($1::uuid,$2::uuid,'factor_x10',false,'region.factor * 10') RETURNING id::text`, modelID, revID)
	gridID := q(`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid,$2::uuid,'G') RETURNING id::text`, modelID, revID)
	exec(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid,$2::uuid)`, gridID, region)
	exec(`INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid,$2::uuid)`, gridID, metricID)

	log := logger.New("test")
	valueAtEMEA := func() float64 {
		t.Helper()
		var v float64
		if err := pool.QueryRow(ctx, `
			SELECT value FROM runtime.calc_result
			WHERE model_id=$1::uuid AND revision_id=$2::uuid AND metric_id=$3::uuid AND dim_members=jsonb_build_object($4::text,'EMEA')
		`, modelID, revID, metricID, region).Scan(&v); err != nil {
			t.Fatalf("read factor_x10 at EMEA: %v", err)
		}
		return v
	}
	sched := calculation.NewScheduler(log, calculation.NewStore(pool), nil)
	if err := sched.RecalcSpecific(ctx, modelID, revID, []string{metricID}); err != nil {
		t.Fatalf("initial recalc: %v", err)
	}
	if got := valueAtEMEA(); got != 20 {
		t.Fatalf("before the pull: factor_x10 at EMEA = %v, want 20", got)
	}

	c := &integration.DBCommitter{Pool: pool, Log: log}
	def := &integration.Definition{ModelID: modelID, RevisionID: revID,
		Config: &integration.Config{TargetType: integration.TargetDimension, TargetID: region}}
	written, _, err := c.CommitPull(ctx, def, []string{"code", "property:factor"}, [][]string{{"EMEA", "7"}}, false, "dev-user")
	if err != nil || written != 1 {
		t.Fatalf("commit pull: written=%d err=%v", written, err)
	}
	if got := valueAtEMEA(); got != 70 {
		t.Errorf("after the pull set factor=7: factor_x10 at EMEA = %v, want 70", got)
	}
}

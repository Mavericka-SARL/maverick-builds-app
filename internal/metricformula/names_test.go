package metricformula

import (
	"context"
	"testing"
	"time"
)

// TestValidateNamesIgnoreCase: formulas name metrics and dimensions without
// regard to case, as the evaluator binds them, and metric names are unique
// regardless of case (migration 100), so such a reference is never
// ambiguous.
func TestValidateNamesIgnoreCase(t *testing.T) {
	f := setupDimFixture(t)
	ctx := context.Background()

	res, err := f.validate(t, "", "doubled", "REVENUE * 2 + Flow + Region.Factor")
	if err != nil {
		t.Fatalf("a reference in another case is refused: %v", err)
	}
	got := map[string]bool{}
	for _, id := range res.DependsOnMetricIDs {
		got[id] = true
	}
	if !got[f.revenue] || !got[f.flow] || len(got) != 2 {
		t.Errorf("edges %v, want revenue %s and flow %s", res.DependsOnMetricIDs, f.revenue, f.flow)
	}

	// The save refuses a wrong number of arguments.
	if _, err := f.validate(t, "", "rounded", "ROUND(revenue)"); err == nil || codeOf(err) != "#VALUE!" {
		t.Errorf("ROUND(revenue): want a #VALUE! refusal, got %v", err)
	}

	insert := func(name string) error {
		_, err := f.pool.Exec(ctx, `INSERT INTO model.metric_def (model_id, revision_id, name, is_input) VALUES ($1::uuid, $2::uuid, $3, true)`,
			f.modelID, f.revID, name)
		return err
	}
	if err := insert("Revenue"); err == nil || codeOf(MetricNameTaken(err, "Revenue")) != CodeMetricNameTaken {
		t.Errorf("a metric named Revenue beside revenue: want METRIC_NAME_TAKEN, got %v", err)
	}
	rename := func(id, name string) error {
		_, err := f.pool.Exec(ctx, `UPDATE model.metric_def SET name=$2 WHERE id=$1::uuid`, id, name)
		return err
	}
	if err := rename(f.other, "OTHER"); err != nil {
		t.Errorf("renaming other to OTHER (its own name) is refused: %v", err)
	}
	if err := rename(f.other, "Flow"); err == nil || codeOf(MetricNameTaken(err, "Flow")) != CodeMetricNameTaken {
		t.Errorf("renaming other to Flow beside flow: want METRIC_NAME_TAKEN, got %v", err)
	}
	// Another revision may reuse the name in any case.
	var rev2 string
	if err := f.pool.QueryRow(ctx, `INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Next') RETURNING id::text`, f.modelID).Scan(&rev2); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO model.metric_def (model_id, revision_id, name, is_input) VALUES ($1::uuid, $2::uuid, 'REVENUE', true)`, f.modelID, rev2); err != nil {
		t.Errorf("REVENUE in another revision is refused: %v", err)
	}
}

// TestDimensionNamesIgnoreCase: dimension names are unique regardless of case
// (migration 101), as formulas match them.
func TestDimensionNamesIgnoreCase(t *testing.T) {
	f := setupDimFixture(t)
	ctx := context.Background()
	insert := func(revID, name string) error {
		_, err := f.pool.Exec(ctx, `INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type) VALUES ($1::uuid, $2::uuid, $3, 'standard')`,
			f.modelID, revID, name)
		return err
	}
	if err := insert(f.revID, "Region"); err == nil || codeOf(DimensionNameTaken(err, "Region")) != CodeDimensionNameTaken {
		t.Errorf("a dimension named Region beside region: want DIMENSION_NAME_TAKEN, got %v", err)
	}
	rename := func(id, name string) error {
		_, err := f.pool.Exec(ctx, `UPDATE model.dimension_def SET name=$2 WHERE id=$1::uuid`, id, name)
		return err
	}
	if err := rename(f.channel, "CHANNEL"); err != nil {
		t.Errorf("renaming channel to CHANNEL (its own name) is refused: %v", err)
	}
	if err := rename(f.channel, "Product"); err == nil || codeOf(DimensionNameTaken(err, "Product")) != CodeDimensionNameTaken {
		t.Errorf("renaming channel to Product beside product: want DIMENSION_NAME_TAKEN, got %v", err)
	}
	var rev2 string
	if err := f.pool.QueryRow(ctx, `INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Next') RETURNING id::text`, f.modelID).Scan(&rev2); err != nil {
		t.Fatal(err)
	}
	if err := insert(rev2, "REGION"); err != nil {
		t.Errorf("REGION in another revision is refused: %v", err)
	}
	// A dimension may still share a metric's name (documented).
	if err := insert(f.revID, "Revenue"); err != nil {
		t.Errorf("a dimension named like the metric revenue is refused: %v", err)
	}
}

// TestNameCheckSerialisesConcurrentWriters: two transactions creating names
// that differ only in case cannot both pass the check (the triggers' advisory
// lock): the second waits for the first and is refused once it commits.
func TestNameCheckSerialisesConcurrentWriters(t *testing.T) {
	f := setupDimFixture(t)
	ctx := context.Background()
	for _, c := range []struct{ table, extra, first, second string }{
		{"model.metric_def", "is_input", "Margin", "margin"},
		{"model.dimension_def", "dimension_type", "Channel2", "CHANNEL2"},
	} {
		val := "true"
		if c.extra == "dimension_type" {
			val = "'standard'"
		}
		sql := `INSERT INTO ` + c.table + ` (model_id, revision_id, name, ` + c.extra + `) VALUES ($1::uuid, $2::uuid, $3, ` + val + `)`
		tx1, err := f.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx1.Rollback(ctx) //nolint:errcheck // a no-op after Commit; frees the connection if the test stops early
		if _, err := tx1.Exec(ctx, sql, f.modelID, f.revID, c.first); err != nil {
			t.Fatalf("%s: first insert: %v", c.table, err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := f.pool.Exec(ctx, sql, f.modelID, f.revID, c.second)
			done <- err
		}()
		select {
		case err := <-done:
			t.Errorf("%s: the second writer did not wait for the first (err %v)", c.table, err)
			continue
		case <-time.After(300 * time.Millisecond):
		}
		if err := tx1.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			if !IsUniqueViolation(err) {
				t.Errorf("%s: %s beside %s created concurrently: want a unique violation, got %v", c.table, c.second, c.first, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the second writer never finished", c.table)
		}
	}
}

package recordpost_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/recordpost"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// A text field posts into a text metric: each cell takes the latest record's
// text (migration 194), as an approved demand override's reason goes beside
// its units. Before 2026-11-21 such a mapping saved without a word and posted
// nothing; a field that holds no number is now refused into a number metric
// unless the mapping counts records.
func TestTextFieldPostsIntoATextMetric(t *testing.T) {
	f := setupImport(t)
	ctx := context.Background()
	var revID, gridID, noteID string
	if err := f.pool.QueryRow(ctx, `SELECT revision_id::text FROM model.form_def WHERE id=$1::uuid`, f.form.ID).Scan(&revID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT grid_id::text FROM model.grid_metric WHERE metric_id=$1::uuid`, f.metricID).Scan(&gridID); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule, format)
		VALUES ($1::uuid,$2::uuid,'note',true,'none','text') RETURNING id::text`, f.form.ModelID, revID).Scan(&noteID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid,$2::uuid)`, gridID, noteID); err != nil {
		t.Fatal(err)
	}

	// What may be saved: the autocode field holds no number.
	refs := crudapp.MappingRefs{FormID: f.form.ID, TargetMetricID: f.metricID, SourceField: "code",
		DimensionMappings: map[string]string{f.regionDim: "region"}, Aggregation: "sum"}
	if err := crudapp.CheckMappingRefs(ctx, f.pool, f.form.ModelID, revID, refs); !crudapp.IsMappingRefError(err) ||
		!strings.Contains(err.Error(), "holds no number") {
		t.Errorf("a code into a number metric: %v, want a refusal saying it holds no number", err)
	}
	refs.Aggregation = "count"
	if err := crudapp.CheckMappingRefs(ctx, f.pool, f.form.ModelID, revID, refs); err != nil {
		t.Errorf("counting the records by their code: %v", err)
	}
	refs.TargetMetricID, refs.Aggregation = noteID, crudapp.MappingAggregation(ctx, f.pool, noteID, "")
	if refs.Aggregation != "last" {
		t.Errorf("a new mapping into a text metric stores %q, want last", refs.Aggregation)
	}
	if err := crudapp.CheckMappingRefs(ctx, f.pool, f.form.ModelID, revID, refs); err != nil {
		t.Errorf("a code into a text metric: %v", err)
	}

	if _, err := f.pool.Exec(ctx, `INSERT INTO model.form_metric_mapping (model_id, revision_id, form_id, grid_id, name, source_field,
		target_metric_id, aggregation, dimension_mappings)
		VALUES ($1::uuid,$2::uuid,$3::uuid,$4::uuid,'note','code',$5::uuid,'last',jsonb_build_object($6::text,'region'))`,
		f.form.ModelID, revID, f.form.ID, gridID, noteID, f.regionDim); err != nil {
		t.Fatal(err)
	}
	ps := recordpost.OnPool(f.pool, logger.New("test"))
	if _, err := ps.Import(ctx, recordpost.ImportRequest{Form: f.form, CreatorID: f.userID,
		Header: []string{"region", "amount"}, Rows: spendRows("10", "20", "5"), Inline: true}); err != nil { // EU, US, EU
		t.Fatal(err)
	}
	text := func(region string) string {
		t.Helper()
		var s string
		_ = f.pool.QueryRow(ctx, `SELECT COALESCE(text_value,'') FROM runtime.fact_input
			WHERE metric_id=$1::uuid AND dim_members->>$2 = $3`, noteID, f.regionDim, region).Scan(&s)
		return s
	}
	var codes []string
	rows, err := f.pool.Query(ctx, `SELECT data->>'code' FROM runtime.form_record WHERE form_id=$1::uuid ORDER BY data->>'code'`, f.form.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var c string
		_ = rows.Scan(&c)
		codes = append(codes, c)
	}
	rows.Close()
	if len(codes) != 3 {
		t.Fatalf("records: %v", codes)
	}
	// One import posts every record at once: the cell takes the greatest of
	// the latest texts, as a number's "last" takes the greatest.
	if eu, us := text("EU"), text("US"); eu != codes[2] || us != codes[1] {
		t.Errorf("note by region: EU %q, US %q; want %q and %q", eu, us, codes[2], codes[1])
	}
	if eu, us := f.spend(t, "EU"), f.spend(t, "US"); eu != 15 || us != 20 {
		t.Errorf("the number posting beside it: EU %v, US %v; want 15 and 20", eu, us)
	}
}

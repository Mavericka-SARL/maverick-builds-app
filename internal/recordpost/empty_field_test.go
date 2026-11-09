package recordpost_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
	"github.com/mavericks-engine/mavericks/internal/recordpost"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// A record whose posting cannot be held names what it posts into — the
// metric and the grid — not the table it is a record of: an AI-built
// import was refused as posting "into Import_BS", while the cell it
// missed was on the grid "Actuals - BS" (2026-11-07).
func TestPostingRefusalNamesTheGrid(t *testing.T) {
	f := setupImport(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE model.metric_def SET label='Spend amount' WHERE id=$1::uuid`, f.metricID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE model.grid_def SET name='Spending' WHERE id=(SELECT grid_id FROM model.grid_metric WHERE metric_id=$1::uuid)`, f.metricID); err != nil {
		t.Fatal(err)
	}
	ps := recordpost.OnPool(f.pool, logger.New("test"))
	code, msg, err := ps.PostingRefusal(ctx, f.userID, true, f.form, "", &recordpost.RecordState{Data: map[string]any{"region": "NOPE", "amount": 5.0}})
	if err != nil {
		t.Fatal(err)
	}
	if code != http.StatusBadRequest || !strings.Contains(msg, "Spend amount") || !strings.Contains(msg, "Spending") || strings.Contains(msg, "posts into Spend,") {
		t.Fatalf("refusal = %d %q; want 400 naming the metric Spend amount and the grid Spending, not the table", code, msg)
	}
}

// A record holding a value whose mapped field is empty posted at a partial
// coordinate: counted in the metric's grid total, in no cell and in no
// calculation. Its save is refused naming the field, in any status in which
// it would post; a file holding such a row is refused naming the row and
// the column; and a record saved before the rule posts nothing.
func TestEmptyMappedFieldIsRefusedAndPostsNothing(t *testing.T) {
	f := setupImport(t)
	ctx := context.Background()
	ps := recordpost.OnPool(f.pool, logger.New("test"))

	code, msg, err := ps.PostingRefusal(ctx, f.userID, true, f.form, "", &recordpost.RecordState{Data: map[string]any{"amount": 40.0}})
	if err != nil {
		t.Fatal(err)
	}
	if code != http.StatusBadRequest || !strings.Contains(msg, "Region is empty") {
		t.Fatalf("save without a region = %d %q; want 400 saying Region is empty", code, msg)
	}
	// A record without a value posts nothing, so it needs no region.
	if code, msg, err := ps.PostingRefusal(ctx, f.userID, true, f.form, "", &recordpost.RecordState{Data: map[string]any{"code": "x"}}); err != nil || code != 0 {
		t.Fatalf("save without a value = %d %q %v; want it allowed", code, msg, err)
	}

	rows := []importpkg.RawRow{
		{RowNumber: 2, Cells: map[string]string{"region": "EU", "amount": "10"}},
		{RowNumber: 3, Cells: map[string]string{"region": "", "amount": "20"}},
	}
	created, errs, err := ps.ImportRows(ctx, recordpost.ImportRequest{Form: f.form, CreatorID: f.userID, Header: []string{"region", "amount"}, Rows: rows, Inline: true})
	if err != nil {
		t.Fatal(err)
	}
	if created != 0 || len(errs) != 1 || errs[0].Row != 3 || errs[0].Column != "region" || !strings.Contains(errs[0].Message, "Region is empty") {
		t.Fatalf("import: created=%d errs=%+v; want the file refused at row 3, column region", created, errs)
	}

	// A record saved before the rule, without a region, posts nothing:
	// no posting at {} that the total counts and no cell shows.
	rec, err := crudapp.NewStore(f.pool).CreateRecord(ctx, f.form.ID, f.userID, map[string]any{"amount": 40.0})
	if err != nil {
		t.Fatal(err)
	}
	if err := ps.Apply(ctx, rec.ID, f.form.ID, "", nil, f.userID); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM runtime.form_record_posting WHERE form_record_id=$1::uuid`, rec.ID); n != 0 {
		t.Errorf("a record without a region has %v posting(s); want none", n)
	}
	if n := f.count(t, `SELECT count(*) FROM runtime.fact_input WHERE metric_id=$1::uuid AND dim_members='{}'::jsonb`, f.metricID); n != 0 {
		t.Errorf("spend has %v value(s) at no region; want none", n)
	}

	// A new record of a table keeping the dimension it posts by, mapped by
	// its auto code, has no code until it is saved: that is not an empty
	// field.
	if _, err := f.pool.Exec(ctx, `UPDATE model.form_def SET keeps_dimension_id=$1::uuid WHERE id=$2::uuid`, f.regionDim, f.form.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE model.form_metric_mapping SET dimension_mappings=jsonb_build_object($1::text,'code') WHERE id=$2::uuid`, f.regionDim, f.mapping); err != nil {
		t.Fatal(err)
	}
	kept, err := crudapp.NewStore(f.pool).GetForm(ctx, f.form.ID)
	if err != nil {
		t.Fatal(err)
	}
	if code, msg, err := ps.PostingRefusal(ctx, f.userID, true, kept, "", &recordpost.RecordState{Data: map[string]any{"amount": 5.0}}); err != nil || code != 0 {
		t.Fatalf("new auto-code record of a kept table = %d %q %v; want it allowed", code, msg, err)
	}
}

package recordpost_test

import (
	"context"
	"strings"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
	"github.com/mavericks-engine/mavericks/internal/recordpost"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// batchRows are spend rows of one batch (a text field standing in for a
// ledger's period): region:amount pairs.
func batchRows(batch string, pairs ...string) []importpkg.RawRow {
	rows := make([]importpkg.RawRow, len(pairs))
	for i, p := range pairs {
		region, amount, _ := strings.Cut(p, ":")
		rows[i] = importpkg.RawRow{RowNumber: i + 2, Cells: map[string]string{"batch": batch, "region": region, "amount": amount}}
	}
	return rows
}

// withBatchField gives the fixture's table a text field "batch".
func withBatchField(t *testing.T, f *importFixture) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE model.form_def SET fields = fields || jsonb_build_array(
		jsonb_build_object('name','batch','label','Batch','type','text')) WHERE id = $1::uuid`, f.form.ID); err != nil {
		t.Fatal(err)
	}
	form, err := crudapp.NewStore(f.pool).GetForm(ctx, f.form.ID)
	if err != nil {
		t.Fatal(err)
	}
	f.form = form
}

func (f importFixture) spend(t *testing.T, region string) float64 {
	t.Helper()
	return f.count(t, `SELECT COALESCE(sum(value), 0) FROM runtime.fact_input WHERE source_ref=$1::uuid AND dim_members->>$2 = $3`,
		f.mapping, f.regionDim, region)
}

// Loading a batch's file again replaces the batch: its records go, the
// file's come, other batches stay, and a cell only the old records posted
// into empties — the file of July loaded twice used to double July.
func TestImportReplacesByField(t *testing.T) {
	f := setupImport(t)
	withBatchField(t, &f)
	ctx := context.Background()
	ps := recordpost.OnPool(f.pool, logger.New("test"))
	header := []string{"batch", "region", "amount"}
	load := func(rows []importpkg.RawRow, replaceBy string, dry bool) (recordpost.ImportResult, error) {
		return ps.Import(ctx, recordpost.ImportRequest{Form: f.form, CreatorID: f.userID, Header: header, Rows: rows,
			Inline: true, ReplaceBy: replaceBy, DryRun: dry})
	}
	if res, err := load(batchRows("JUL", "EU:10", "US:20"), "batch", false); err != nil || res.Created != 2 || res.Replaced != 0 {
		t.Fatalf("first load: %+v %v", res, err)
	}
	if res, err := load(batchRows("JUN", "EU:100"), "batch", false); err != nil || res.Created != 1 || res.Replaced != 0 {
		t.Fatalf("June: %+v %v", res, err)
	}
	// A dry run says what it would replace and changes nothing.
	if res, err := load(batchRows("JUL", "EU:7"), "Batch", true); err != nil || res.Created != 1 || res.Replaced != 2 {
		t.Fatalf("dry run: %+v %v, want 1 created and 2 replaced", res, err)
	}
	if got := f.spend(t, "EU"); got != 110 {
		t.Fatalf("after a dry run EU = %v, want 110", got)
	}
	// The corrected July file holds no US row: July's US record goes and
	// the US cell empties.
	res, err := load(batchRows("JUL", "EU:7"), "batch", false)
	if err != nil || res.Created != 1 || res.Replaced != 2 {
		t.Fatalf("reload: %+v %v, want 1 created and 2 replaced", res, err)
	}
	if eu, us := f.spend(t, "EU"), f.spend(t, "US"); eu != 107 || us != 0 {
		t.Errorf("after the reload EU = %v (want 100 June + 7 July), US = %v (want 0)", eu, us)
	}
	if n := f.count(t, `SELECT count(*) FROM runtime.form_record WHERE form_id=$1::uuid`, f.form.ID); n != 2 {
		t.Errorf("records = %v, want June's and the corrected July's", n)
	}
	// Without replace_by a load adds, as before.
	if res, err := load(batchRows("JUL", "EU:7"), "", false); err != nil || res.Replaced != 0 || res.Created != 1 {
		t.Fatalf("adding load: %+v %v", res, err)
	}
	if got := f.spend(t, "EU"); got != 114 {
		t.Errorf("an adding load: EU = %v, want 114", got)
	}
}

// A replace the loader may not make refuses the whole load: nothing is
// deleted or created. A row without the field is refused, and only a field
// naming a slice of the table may be replaced by.
func TestImportReplaceRefusals(t *testing.T) {
	f := setupImport(t)
	withBatchField(t, &f)
	ctx := context.Background()
	ps := recordpost.OnPool(f.pool, logger.New("test"))
	header := []string{"batch", "region", "amount"}
	if _, err := ps.Import(ctx, recordpost.ImportRequest{Form: f.form, CreatorID: f.userID, Header: header,
		Rows: batchRows("JUL", "EU:10"), Inline: true}); err != nil {
		t.Fatal(err)
	}
	res, err := ps.Import(ctx, recordpost.ImportRequest{Form: f.form, CreatorID: f.userID, Header: header,
		Rows: batchRows("JUL", "EU:3"), Inline: true, ReplaceBy: "batch",
		DeleteRefusal: func(*crudapp.FormRecord) string { return "you may not delete it" }})
	if !recordpost.IsReplaceRefused(err) || !strings.Contains(err.Error(), "Batch JUL") || res.Created != 0 {
		t.Fatalf("refused replace: %+v %v", res, err)
	}
	if got := f.spend(t, "EU"); got != 10 {
		t.Errorf("a refused load changed EU to %v", got)
	}
	if n := f.count(t, `SELECT count(*) FROM runtime.form_record WHERE form_id=$1::uuid`, f.form.ID); n != 1 {
		t.Errorf("records after a refused load = %v, want 1", n)
	}

	rows := batchRows("JUL", "EU:3", "US:4")
	rows[1].Cells["batch"] = ""
	res, err = ps.Import(ctx, recordpost.ImportRequest{Form: f.form, CreatorID: f.userID, Header: header, Rows: rows, Inline: true, ReplaceBy: "batch"})
	if err != nil || len(res.Errors) != 1 || res.Errors[0].Row != 3 || res.Created != 0 {
		t.Fatalf("a row without the field: %+v %v", res, err)
	}
	// Partial (a connector's) does not apply to a replacing load.
	res, err = ps.Import(ctx, recordpost.ImportRequest{Form: f.form, CreatorID: f.userID, Header: header, Rows: rows, Inline: true, ReplaceBy: "batch", Partial: true})
	if err != nil || res.Created != 0 || res.Replaced != 0 {
		t.Fatalf("a partial replacing load with a failing row: %+v %v, want nothing done", res, err)
	}

	for name, want := range map[string]string{"amount": "number field", "nope": "no field"} {
		if _, err := recordpost.ReplaceField(f.form, name); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("ReplaceField(%q) = %v, want %q", name, err, want)
		}
	}
	if fld, err := recordpost.ReplaceField(f.form, "Region"); err != nil || fld.Name != "region" {
		t.Errorf("ReplaceField(Region) = %v, %v", fld, err)
	}
}

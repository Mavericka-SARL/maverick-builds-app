package recordpost_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/crudapp"
	"github.com/mavericks-engine/mavericks/internal/importpkg"
	"github.com/mavericks-engine/mavericks/internal/recordpost"
	"github.com/mavericks-engine/mavericks/internal/testdb"
	"github.com/mavericks-engine/mavericks/pkg/logger"

	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

type importFixture struct {
	pool                *pgxpool.Pool
	form                *crudapp.FormDef
	mapping, userID     string
	regionDim, metricID string
}

// A table of spend records (region, amount) posting into spend[region].
func setupImport(t *testing.T) importFixture {
	t.Helper()
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
	f := importFixture{pool: pool}
	custID := q(`INSERT INTO core.customer (name, plan) VALUES ('ImportCo','enterprise') RETURNING id::text`)
	wsID := q(`INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid,'ws') RETURNING id::text`, custID)
	appID := q(`INSERT INTO core.application (workspace_id, customer_id, name, mode) VALUES ($1::uuid,$2::uuid,'App','planning') RETURNING id::text`, wsID, custID)
	modelID := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid,'M') RETURNING id::text`, appID)
	revID := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid,'Working') RETURNING id::text`, modelID)
	q(`UPDATE core.model SET active_revision_id=$1::uuid WHERE id=$2::uuid RETURNING id::text`, revID, modelID)
	f.regionDim = q(`INSERT INTO model.dimension_def (model_id, revision_id, name, dimension_type) VALUES ($1::uuid,$2::uuid,'region','standard') RETURNING id::text`, modelID, revID)
	q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid,'EU','Europe') RETURNING id::text`, f.regionDim)
	q(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid,'US','United States') RETURNING id::text`, f.regionDim)
	f.metricID = q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input, agg_rule) VALUES ($1::uuid,$2::uuid,'spend',true,'sum') RETURNING id::text`, modelID, revID)
	grid := q(`INSERT INTO model.grid_def (model_id, revision_id, name) VALUES ($1::uuid,$2::uuid,'G') RETURNING id::text`, modelID, revID)
	q(`INSERT INTO model.grid_dimension (grid_id, dimension_id) VALUES ($1::uuid,$2::uuid) RETURNING grid_id::text`, grid, f.regionDim)
	q(`INSERT INTO model.grid_metric (grid_id, metric_id) VALUES ($1::uuid,$2::uuid) RETURNING grid_id::text`, grid, f.metricID)
	formID := q(`INSERT INTO model.form_def (model_id, revision_id, name, label, fields) VALUES ($1::uuid,$2::uuid,'spend','Spend',
		jsonb_build_array(
			jsonb_build_object('name','code','label','Code','type','autocode','code_prefix','S-'),
			jsonb_build_object('name','region','label','Region','type','dimension','dimension_id',$3::text),
			jsonb_build_object('name','amount','label','Amount','type','number'))) RETURNING id::text`, modelID, revID, f.regionDim)
	f.mapping = q(`INSERT INTO model.form_metric_mapping (model_id, revision_id, form_id, grid_id, name, source_field, target_metric_id, dimension_mappings)
		VALUES ($1::uuid,$2::uuid,$3::uuid,$4::uuid,'spend','amount',$5::uuid,jsonb_build_object($6::text,'region')) RETURNING id::text`,
		modelID, revID, formID, grid, f.metricID, f.regionDim)
	f.userID = q(`INSERT INTO identity.user (keycloak_sub, email, customer_id) VALUES ('ctl','ctl@t.co',$1::uuid) RETURNING id::text`, custID)
	form, err := crudapp.NewStore(pool).GetForm(ctx, formID)
	if err != nil {
		t.Fatal(err)
	}
	f.form = form
	return f
}

func (f importFixture) count(t *testing.T, sql string, args ...any) float64 {
	t.Helper()
	var n float64
	if err := f.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func spendRows(amounts ...string) []importpkg.RawRow {
	rows := make([]importpkg.RawRow, len(amounts))
	for i, a := range amounts {
		region := "EU"
		if i%2 == 1 {
			region = "US"
		}
		rows[i] = importpkg.RawRow{RowNumber: i + 2, Cells: map[string]string{"region": region, "amount": a}}
	}
	return rows
}

// A file imports whole or not at all: a row that fails as it is written —
// after every row passed the checks — leaves none of the file's records.
// Each record used to commit on its own, so a request cut off part-way (a
// client's timeout) kept the rows made so far: 2,568 of a 9,000-row GL file.
func TestImportRowsIsAllOrNothing(t *testing.T) {
	f := setupImport(t)
	ctx := context.Background()
	// The database refuses the record with amount 13 — a stand-in for any
	// failure while the rows are written (a cancelled request, a deadlock).
	if _, err := f.pool.Exec(ctx, `
		CREATE FUNCTION refuse_thirteen() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN IF NEW.data->>'amount' = '13' THEN RAISE EXCEPTION 'refused'; END IF; RETURN NEW; END $$;
		CREATE TRIGGER refuse_thirteen BEFORE INSERT ON runtime.form_record FOR EACH ROW EXECUTE FUNCTION refuse_thirteen();`); err != nil {
		t.Fatal(err)
	}
	ps := recordpost.OnPool(f.pool, logger.New("test"))
	created, errs, err := ps.ImportRows(ctx, recordpost.ImportRequest{
		Form: f.form, CreatorID: f.userID, Header: []string{"region", "amount"},
		Rows: spendRows("10", "20", "13", "40"), Inline: true,
	})
	if err == nil || len(errs) != 0 {
		t.Fatalf("import: created=%d errs=%v err=%v; want the write failure", created, errs, err)
	}
	if n := f.count(t, `SELECT count(*) FROM runtime.form_record WHERE form_id=$1::uuid`, f.form.ID); n != 0 {
		t.Fatalf("a failed import left %v record(s); want none", n)
	}
	if n := f.count(t, `SELECT COALESCE(sum(value), 0) FROM runtime.fact_input WHERE source_ref=$1::uuid`, f.mapping); n != 0 {
		t.Fatalf("a failed import posted %v; want nothing", n)
	}
	// The codes it gave out are not used up by the rows it rolled back.
	created, errs, err = ps.ImportRows(ctx, recordpost.ImportRequest{
		Form: f.form, CreatorID: f.userID, Header: []string{"region", "amount"},
		Rows: spendRows("10", "20"), Inline: true,
	})
	if err != nil || len(errs) != 0 || created != 2 {
		t.Fatalf("second import: created=%d errs=%v err=%v", created, errs, err)
	}
	var first string
	if err := f.pool.QueryRow(ctx, `SELECT min(data->>'code') FROM runtime.form_record WHERE form_id=$1::uuid`, f.form.ID).Scan(&first); err != nil || first != "S-0001" {
		t.Errorf("first code after a rolled-back import = %q (%v), want S-0001", first, err)
	}
}

// A file's records post as one batch: every record's posting is there and
// each total holds the sum of them, rebuilt once.
func TestImportRowsPostsTheWholeFile(t *testing.T) {
	f := setupImport(t)
	ctx := context.Background()
	amounts := make([]string, 50)
	want := map[string]float64{}
	for i := range amounts {
		amounts[i] = fmt.Sprint(i + 1)
		region := "EU"
		if i%2 == 1 {
			region = "US"
		}
		want[region] += float64(i + 1)
	}
	ps := recordpost.OnPool(f.pool, logger.New("test"))
	created, errs, err := ps.ImportRows(ctx, recordpost.ImportRequest{
		Form: f.form, CreatorID: f.userID, Header: []string{"region", "amount"}, Rows: spendRows(amounts...), Inline: true,
	})
	if err != nil || len(errs) != 0 || created != 50 {
		t.Fatalf("import: created=%d errs=%v err=%v", created, errs, err)
	}
	if n := f.count(t, `SELECT count(*) FROM runtime.form_record_posting WHERE mapping_id=$1::uuid`, f.mapping); n != 50 {
		t.Fatalf("postings = %v, want 50", n)
	}
	for region, sum := range want {
		got := f.count(t, `SELECT COALESCE(sum(value), 0) FROM runtime.fact_input WHERE source_ref=$1::uuid AND dim_members->>$2 = $3`,
			f.mapping, f.regionDim, region)
		if got != sum {
			t.Errorf("spend[%s] = %v, want %v", region, got, sum)
		}
	}
}

// A posting marks the revisions it posts into as awaiting a recalculation
// and lifts the mark when it has recalculated, as a cell write does: the
// grid says the totals are about to change instead of showing the old ones
// as final. Imported records used to post with nothing recorded.
func TestPostingRecordsItsRecalculation(t *testing.T) {
	f := setupImport(t)
	ctx := context.Background()
	state := func() (requested, done float64) {
		t.Helper()
		_ = f.pool.QueryRow(ctx, `SELECT requested, done FROM runtime.revision_recalc_state
			WHERE revision_id = (SELECT revision_id FROM model.form_metric_mapping WHERE id = $1::uuid)`, f.mapping).Scan(&requested, &done)
		return requested, done
	}
	ps := recordpost.OnPool(f.pool, logger.New("test"))
	if _, errs, err := ps.ImportRows(ctx, recordpost.ImportRequest{
		Form: f.form, CreatorID: f.userID, Header: []string{"region", "amount"}, Rows: spendRows("5", "7"), Inline: true,
	}); err != nil || len(errs) != 0 {
		t.Fatalf("import: %v %v", errs, err)
	}
	if requested, done := state(); requested != 1 || done != 1 {
		t.Errorf("after the posting: requested %v, done %v; want one recalculation asked for and finished", requested, done)
	}
}

// A file may name a dimension member by its label: "Europe" posts to EU.
// A text that is no code and the label of no member, or of two, is still
// refused, naming the text as written.
func TestImportNamesMembersByLabel(t *testing.T) {
	f := setupImport(t)
	ctx := context.Background()
	ps := recordpost.OnPool(f.pool, logger.New("test"))
	rows := []importpkg.RawRow{
		{RowNumber: 2, Cells: map[string]string{"region": "Europe", "amount": "10"}},
		{RowNumber: 3, Cells: map[string]string{"region": "US", "amount": "5"}},
	}
	created, errs, err := ps.ImportRows(ctx, recordpost.ImportRequest{Form: f.form, CreatorID: f.userID, Header: []string{"region", "amount"}, Rows: rows, Inline: true})
	if err != nil || len(errs) != 0 || created != 2 {
		t.Fatalf("import by label: created=%d errs=%v err=%v", created, errs, err)
	}
	if got := f.count(t, `SELECT COALESCE(sum(value), 0) FROM runtime.fact_input WHERE source_ref=$1::uuid AND dim_members->>$2 = 'EU'`, f.mapping, f.regionDim); got != 10 {
		t.Errorf("spend[EU] = %v, want 10 from the row naming Europe", got)
	}

	if _, err := f.pool.Exec(ctx, `INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, 'EU2', 'Europe')`, f.regionDim); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Europe", "Mars"} {
		_, errs, err = ps.ImportRows(ctx, recordpost.ImportRequest{Form: f.form, CreatorID: f.userID, Header: []string{"region", "amount"},
			Rows: []importpkg.RawRow{{RowNumber: 2, Cells: map[string]string{"region": text, "amount": "1"}}}, Inline: true})
		if err != nil || len(errs) != 1 || !strings.Contains(fmt.Sprint(errs), `"`+text+`" is not a member of`) {
			t.Errorf("%s: errs=%v err=%v, want the row refused naming %q", text, errs, err, text)
		}
	}
}

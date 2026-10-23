package recordpost_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mavericks-engine/mavericks/internal/recordpost"
	"github.com/mavericks-engine/mavericks/pkg/logger"
)

// postings is every posting of f's mapping and the totals they make, as
// text: what two ways of posting the same records are compared by.
func (f importFixture) postings(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	var b strings.Builder
	for _, sql := range []string{
		`SELECT p.form_record_id::text || ' ' || p.dim_members::text || ' ' || COALESCE(p.posted_value::text, '') || ' ' ||
		        p.target_metric_id::text || ' ' || COALESCE(p.revision_id::text, '')
		 FROM runtime.form_record_posting p WHERE p.mapping_id = $1::uuid ORDER BY p.form_record_id`,
		`SELECT dim_members::text || ' ' || value::text FROM runtime.fact_input WHERE source_ref = $1::uuid ORDER BY dim_members::text`,
	} {
		rows, err := f.pool.Query(ctx, sql, f.mapping)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				t.Fatal(err)
			}
			b.WriteString(line + "\n")
		}
		rows.Close()
	}
	return b.String()
}

func (f importFixture) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// A file's records posted as one batch hold exactly what each posted on its
// own holds: the same postings, to the last digit of each amount, and the
// same totals. A record with no amount posts nothing either way.
func TestBatchPostsWhatOneAtATimePosts(t *testing.T) {
	f := setupImport(t)
	ctx := context.Background()
	ps := recordpost.OnPool(f.pool, logger.New("test"))
	created, errs, err := ps.ImportRows(ctx, recordpost.ImportRequest{
		Form: f.form, CreatorID: f.userID, Header: []string{"region", "amount"},
		Rows:   spendRows("0.1", "0.2", "1234.5678901234567", "-3", "1e-7", "", "99999999.99", "7"),
		Inline: true,
	})
	if err != nil || len(errs) != 0 || created != 8 {
		t.Fatalf("import: created=%d errs=%v err=%v", created, errs, err)
	}
	batch := f.postings(t)
	if !strings.Contains(batch, " 1234.5678901234567 ") || !strings.Contains(batch, " 0.0000001 ") {
		t.Fatalf("an amount lost digits on its way into the posting:\n%s", batch)
	}
	if n := f.count(t, `SELECT count(*) FROM runtime.form_record_posting WHERE mapping_id=$1::uuid`, f.mapping); n != 7 {
		t.Fatalf("postings = %v, want 7 (the record without an amount posts nothing)", n)
	}

	f.exec(t, `DELETE FROM runtime.form_record_posting WHERE mapping_id=$1::uuid`, f.mapping)
	rows, err := f.pool.Query(ctx, `SELECT id::text FROM runtime.form_record WHERE form_id=$1::uuid ORDER BY created_at, id`, f.form.ID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if err := ps.Apply(ctx, id, f.form.ID, "", nil, f.userID); err != nil {
			t.Fatal(err)
		}
	}
	if one := f.postings(t); one != batch {
		t.Fatalf("posted one at a time:\n%s\nposted as a batch:\n%s", one, batch)
	}
}

// A sync re-posts every record within its creator's access, each cell
// checked once for the whole table: a member hidden from the creator, or a
// metric they may only read, leaves the record's posting as it was; a
// member that stopped being a leaf retracts it. Each sync reads the rules
// afresh.
func TestSyncPostsWithinTheCreatorsAccess(t *testing.T) {
	f := setupImport(t)
	ctx := context.Background()
	ps := recordpost.OnPool(f.pool, logger.New("test"))
	if _, errs, err := ps.ImportRows(ctx, recordpost.ImportRequest{
		Form: f.form, CreatorID: f.userID, Header: []string{"region", "amount"},
		Rows: spendRows("10", "20", "30", "40"), Inline: true,
	}); err != nil || len(errs) != 0 {
		t.Fatalf("import: errs=%v err=%v", errs, err)
	}
	member := func(code string) string {
		var id string
		if err := f.pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`, f.regionDim, code).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	raise := func() {
		f.exec(t, `UPDATE runtime.form_record SET data = jsonb_set(data, '{amount}', to_jsonb((data->>'amount')::numeric + 1)), updated_at = now()
			WHERE form_id=$1::uuid`, f.form.ID)
	}
	check := func(step string, wantEU, wantUS float64) {
		t.Helper()
		if mappings, processed := ps.RepostTable(ctx, f.form.ID, f.userID); mappings != 1 || processed != 4 {
			t.Fatalf("%s: sync went through %d mappings, %d postings; want 1, 4", step, mappings, processed)
		}
		for region, want := range map[string]float64{"EU": wantEU, "US": wantUS} {
			posted := f.count(t, `SELECT COALESCE(sum(p.posted_value), 0) FROM runtime.form_record_posting p
				JOIN runtime.form_record r ON r.id = p.form_record_id WHERE p.mapping_id=$1::uuid AND r.data->>'region' = $2`, f.mapping, region)
			total := f.count(t, `SELECT COALESCE(sum(value), 0) FROM runtime.fact_input WHERE source_ref=$1::uuid AND dim_members->>$2 = $3`,
				f.mapping, f.regionDim, region)
			if posted != want || total != want {
				t.Errorf("%s: %s posts %v, totals %v; want %v", step, region, posted, total, want)
			}
		}
	}

	raise() // EU 11+31, US 21+41
	f.exec(t, `INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid,'dimension_member',$2,'hidden')`, f.userID, member("US"))
	check("US hidden", 42, 60)

	raise() // EU 12+32, US 22+42
	f.exec(t, `DELETE FROM identity.user_access_rule WHERE user_id=$1::uuid`, f.userID)
	f.exec(t, `INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid,'metric',$2,'read')`, f.userID, f.metricID)
	check("metric read-only", 42, 60)

	f.exec(t, `DELETE FROM identity.user_access_rule WHERE user_id=$1::uuid`, f.userID)
	f.exec(t, `INSERT INTO model.dimension_member (dimension_id, code, label, parent_member_id) VALUES ($1::uuid,'EU-N','Nordics',$2::uuid)`, f.regionDim, member("EU"))
	check("EU no longer a leaf", 0, 64)
}

// A batch that read a record before it changed leaves the record's newer
// posting as it is — neither overwritten with what it read nor retracted —
// as each record's own re-read ensured when records posted one at a time.
func TestABatchLeavesARecordChangedSinceAlone(t *testing.T) {
	f := setupImport(t)
	ctx := context.Background()
	ps := recordpost.OnPool(f.pool, logger.New("test"))
	if _, errs, err := ps.ImportRows(ctx, recordpost.ImportRequest{
		Form: f.form, CreatorID: f.userID, Header: []string{"region", "amount"}, Rows: spendRows("10"), Inline: true,
	}); err != nil || len(errs) != 0 {
		t.Fatalf("import: errs=%v err=%v", errs, err)
	}
	var id string
	var readAt time.Time
	if err := f.pool.QueryRow(ctx, `SELECT id::text, updated_at FROM runtime.form_record WHERE form_id=$1::uuid`, f.form.ID).Scan(&id, &readAt); err != nil {
		t.Fatal(err)
	}
	f.exec(t, `UPDATE runtime.form_record SET data = jsonb_set(data, '{amount}', '99'), updated_at = now() WHERE id=$1::uuid`, id)
	if err := ps.Apply(ctx, id, f.form.ID, "", nil, f.userID); err != nil {
		t.Fatal(err)
	}
	posted := func() float64 {
		return f.count(t, `SELECT COALESCE(sum(posted_value), 0) FROM runtime.form_record_posting WHERE form_record_id=$1::uuid`, id)
	}
	for _, data := range []map[string]any{{"region": "EU", "amount": 10.0}, {"region": "EU"}} {
		if err := ps.PostAsRead(ctx, f.form, id, "", data, readAt, f.userID); err != nil {
			t.Fatal(err)
		}
		if got := posted(); got != 99 {
			t.Fatalf("a batch that read %v before the change left a posting of %v; want the change's 99", data, got)
		}
	}
	var now time.Time
	if err := f.pool.QueryRow(ctx, `SELECT updated_at FROM runtime.form_record WHERE id=$1::uuid`, id).Scan(&now); err != nil {
		t.Fatal(err)
	}
	if err := ps.PostAsRead(ctx, f.form, id, "", map[string]any{"region": "EU", "amount": 5.0}, now, f.userID); err != nil {
		t.Fatal(err)
	}
	if got := posted(); got != 5 {
		t.Fatalf("read as it is now, the record posts %v; want 5", got)
	}
}

// Each record of a sync posts within its own creator's access: a member
// hidden from one creator leaves their record's posting as it was, while
// another creator's record at the same member posts.
func TestASyncChecksEachCreatorsOwnAccess(t *testing.T) {
	f := setupImport(t)
	ctx := context.Background()
	ps := recordpost.OnPool(f.pool, logger.New("test"))
	var other, us string
	if err := f.pool.QueryRow(ctx, `INSERT INTO identity.user (keycloak_sub, email, customer_id)
		SELECT 'other', 'other@t.co', customer_id FROM identity.user WHERE id=$1::uuid RETURNING id::text`, f.userID).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if err := f.pool.QueryRow(ctx, `SELECT id::text FROM model.dimension_member WHERE dimension_id=$1::uuid AND code='US'`, f.regionDim).Scan(&us); err != nil {
		t.Fatal(err)
	}
	for creator, amounts := range map[string][]string{f.userID: {"10", "20"}, other: {"1", "2"}} {
		if _, errs, err := ps.ImportRows(ctx, recordpost.ImportRequest{
			Form: f.form, CreatorID: creator, Header: []string{"region", "amount"}, Rows: spendRows(amounts...), Inline: true,
		}); err != nil || len(errs) != 0 {
			t.Fatalf("import: errs=%v err=%v", errs, err)
		}
	}
	f.exec(t, `INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid,'dimension_member',$2,'hidden')`, f.userID, us)
	f.exec(t, `UPDATE runtime.form_record SET data = jsonb_set(data, '{amount}', to_jsonb((data->>'amount')::numeric + 100)), updated_at = now()
		WHERE form_id=$1::uuid`, f.form.ID)
	ps.RepostTable(ctx, f.form.ID, f.userID)
	for creator, want := range map[string]float64{f.userID: 110 + 20, other: 101 + 102} {
		got := f.count(t, `SELECT COALESCE(sum(p.posted_value), 0) FROM runtime.form_record_posting p
			JOIN runtime.form_record r ON r.id = p.form_record_id WHERE p.mapping_id=$1::uuid AND r.created_by=$2::uuid`, f.mapping, creator)
		if got != want {
			t.Errorf("records of %s post %v, want %v", creator, got, want)
		}
	}
}

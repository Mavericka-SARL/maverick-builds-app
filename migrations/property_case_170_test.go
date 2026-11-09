package migrations_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	"github.com/mavericks-engine/mavericks/migrations"
)

// TestPropertyCase170 applies migrations 001–169, gives a dimension three
// declarations of one property differing only in case (the developer's
// Segment, then an import's segment and SEGMENT) with member values split
// between them, a grouping by the import's spelling, and another dimension
// with a property declared once; then applies 170. The oldest declaration
// is kept with its type, every member's value moves under it — the newest
// declaration's value winning where a member holds several — the grouping
// reads the kept spelling, a notice names the dimension, the other
// dimension is untouched, and a declaration differing only in case is then
// refused by the new index.
func TestPropertyCase170(t *testing.T) {
	ctx := context.Background()
	adminDSN := testdb.AdminDSN(t)
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("propcase170_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	_ = admin.Close(ctx)
	cfg, err := pgx.ParseConfig(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = name
	var notices []string
	cfg.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) { notices = append(notices, n.Message) }
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	apply := func(f string) {
		t.Helper()
		sql, err := fs.ReadFile(migrations.FS, f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	for _, f := range files {
		if f >= "170" {
			break
		}
		apply(f)
	}
	// Foreign keys to the model and revision are not what this test is about.
	if _, err := conn.Exec(ctx, `SET session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := conn.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
	}
	modelID, revID := uuid.NewString(), uuid.NewString()
	region, other, grouping := uuid.NewString(), uuid.NewString(), uuid.NewString()
	exec(`INSERT INTO model.dimension_def (id, model_id, revision_id, name, dimension_type) VALUES ($1::uuid,$2::uuid,$3::uuid,'region','standard')`, region, modelID, revID)
	exec(`INSERT INTO model.dimension_def (id, model_id, revision_id, name, dimension_type) VALUES ($1::uuid,$2::uuid,$3::uuid,'channel','standard')`, other, modelID, revID)
	exec(`INSERT INTO model.dimension_def (id, model_id, revision_id, name, dimension_type, source_dimension_id, source_property)
	      VALUES ($1::uuid,$2::uuid,$3::uuid,'segments','standard',$4::uuid,'segment')`, grouping, modelID, revID, region)
	declare := func(dim, prop, dataType string, age int) {
		t.Helper()
		exec(`INSERT INTO model.dimension_property (dimension_id, name, data_type, created_at)
		      VALUES ($1::uuid, $2, $3, now() - make_interval(mins => $4))`, dim, prop, dataType, age)
	}
	declare(region, "Segment", "number", 30)
	declare(region, "segment", "text", 20)
	declare(region, "SEGMENT", "text", 10)
	declare(region, "factor", "number", 40)
	declare(other, "Segment", "text", 30)
	member := func(dim, code, props string) {
		t.Helper()
		exec(`INSERT INTO model.dimension_member (dimension_id, code, label, properties) VALUES ($1::uuid, $2, $2, $3::jsonb)`, dim, code, props)
	}
	member(region, "A", `{"Segment":"1","segment":"2"}`)
	member(region, "B", `{"segment":"3","factor":"7"}`)
	member(region, "C", `{"Segment":"4"}`)
	member(region, "D", `{"SEGMENT":"9","Segment":"5","segment":"6"}`)
	member(region, "E", `{"factor":"2"}`)
	member(other, "W", `{"Segment":"x","segment":"y"}`)

	apply("170_property_names_unique_regardless_of_case.sql")

	props := func(dim string) map[string]string {
		t.Helper()
		rows, err := conn.Query(ctx, `SELECT name, data_type FROM model.dimension_property WHERE dimension_id=$1::uuid`, dim)
		if err != nil {
			t.Fatal(err)
		}
		out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) ([2]string, error) {
			var p [2]string
			return p, r.Scan(&p[0], &p[1])
		})
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		for _, p := range out {
			m[p[0]] = p[1]
		}
		return m
	}
	if got, want := props(region), map[string]string{"Segment": "number", "factor": "number"}; !reflect.DeepEqual(got, want) {
		t.Errorf("region's declarations = %v, want %v (the oldest kept with its type)", got, want)
	}
	if got, want := props(other), map[string]string{"Segment": "text"}; !reflect.DeepEqual(got, want) {
		t.Errorf("channel's declarations = %v, want %v (untouched)", got, want)
	}
	values := func(dim, code string) map[string]string {
		t.Helper()
		var raw []byte
		if err := conn.QueryRow(ctx, `SELECT properties FROM model.dimension_member WHERE dimension_id=$1::uuid AND code=$2`, dim, code).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		m := map[string]string{}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	for code, want := range map[string]map[string]string{
		"A": {"Segment": "2"},
		"B": {"Segment": "3", "factor": "7"},
		"C": {"Segment": "4"},
		"D": {"Segment": "9"},
		"E": {"factor": "2"},
	} {
		if got := values(region, code); !reflect.DeepEqual(got, want) {
			t.Errorf("region member %s holds %v, want %v", code, got, want)
		}
	}
	if got, want := values(other, "W"), map[string]string{"Segment": "x", "segment": "y"}; !reflect.DeepEqual(got, want) {
		t.Errorf("channel member W holds %v, want %v (a dimension with one declaration is untouched)", got, want)
	}
	var source string
	if err := conn.QueryRow(ctx, `SELECT source_property FROM model.dimension_def WHERE id=$1::uuid`, grouping).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if source != "Segment" {
		t.Errorf("the grouping reads %q, want the kept spelling Segment", source)
	}
	found := false
	for _, n := range notices {
		if strings.Contains(n, "region") && strings.Contains(n, region) && strings.Contains(n, "Segment") {
			found = true
		}
	}
	if !found {
		t.Errorf("no notice names the merged dimension region (%s); notices: %q", region, notices)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO model.dimension_property (dimension_id, name) VALUES ($1::uuid, 'FACTOR')`, region); err == nil {
		t.Error("a declaration FACTOR next to factor was accepted after 170")
	}
	if _, err := conn.Exec(ctx, `INSERT INTO model.dimension_property (dimension_id, name) VALUES ($1::uuid, 'factor')`, other); err != nil {
		t.Errorf("declaring factor on another dimension: %v", err)
	}
}

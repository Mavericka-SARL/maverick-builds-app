package migrations_test

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	"github.com/mavericks-engine/mavericks/migrations"
)

// TestBackfill099Lineage applies migrations 001–098 to an empty database,
// writes rows the way they stood before lineage existed, applies 099 and
// checks its backfill: rows sharing an identity across the revisions of a
// model — dimensions by name (case-insensitive), members by (dimension
// name, code), metrics by name, revision-less rows included — share one
// lineage, other rows and other models do not, and each access rule takes
// the lineage of the row its ref_id points at (NULL for a button, a
// malformed ref_id or a row that is gone). It then re-applies 099: the
// migration must be re-runnable and leave the settled lineages alone.
func TestBackfill099Lineage(t *testing.T) {
	ctx := context.Background()
	adminDSN := testdb.AdminDSN(t)
	admin, err := pgx.Connect(ctx, adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("backfill099_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	_ = admin.Close(ctx)
	cfg, err := pgx.ParseConfig(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = name
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
		if f >= "099" {
			break
		}
		apply(f)
	}

	// Rows as they stood before 099. Foreign keys are not what this test is
	// about, so the seed skips them (replica role: FK triggers off).
	if _, err := conn.Exec(ctx, `SET session_replication_role = replica`); err != nil {
		t.Fatal(err)
	}
	id := func(sql string, args ...any) string {
		t.Helper()
		var out string
		if err := conn.QueryRow(ctx, sql, args...).Scan(&out); err != nil {
			t.Fatalf("seed %q: %v", sql, err)
		}
		return out
	}
	model := func() string {
		return id(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M') RETURNING id::text`, uuid.NewString())
	}
	rev := func(m string) string {
		return id(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, $2) RETURNING id::text`, m, uuid.NewString())
	}
	dim := func(m, rv, n string) string {
		var r any
		if rv != "" {
			r = rv
		}
		return id(`INSERT INTO model.dimension_def (model_id, name, revision_id) VALUES ($1::uuid, $2, $3::uuid) RETURNING id::text`, m, n, r)
	}
	member := func(d, code string) string {
		return id(`INSERT INTO model.dimension_member (dimension_id, code, label) VALUES ($1::uuid, $2, $2) RETURNING id::text`, d, code)
	}
	metric := func(m, rv, n string) string {
		return id(`INSERT INTO model.metric_def (model_id, name, is_input, agg_rule, revision_id) VALUES ($1::uuid, $2, true, 'sum', $3::uuid) RETURNING id::text`, m, n, rv)
	}

	m1, m2 := model(), model()
	revA, revB := rev(m1), rev(m1)
	regA, regB := dim(m1, revA, "Region"), dim(m1, revB, "region")
	usA, usB, deA := member(regA, "US"), member(regB, "US"), member(regA, "DE")
	usLower := member(regB, "us") // codes are case-sensitive: a different member
	chanDim := dim(m1, "", "channel")
	web := member(chanDim, "WEB")
	revenueA, revenueB, costA := metric(m1, revA, "Revenue"), metric(m1, revB, "revenue"), metric(m1, revA, "cost")
	rev2 := rev(m2)
	reg2 := dim(m2, rev2, "Region")
	us2 := member(reg2, "US")
	revenue2 := metric(m2, rev2, "Revenue")

	user := uuid.NewString()
	rule := func(typ, ref string) string {
		return id(`INSERT INTO identity.user_access_rule (user_id, rule_type, ref_id, access) VALUES ($1::uuid, $2, $3, 'hidden') RETURNING id::text`, user, typ, ref)
	}
	ruleUS, ruleRevenue, ruleWeb := rule("dimension_member", usB), rule("metric", revenueA), rule("dimension_member", web)
	ruleMalformed, ruleGone, ruleButton := rule("dimension_member", "not-a-uuid"), rule("metric", uuid.NewString()), rule("button", uuid.NewString())

	if _, err := conn.Exec(ctx, `SET session_replication_role = DEFAULT`); err != nil {
		t.Fatal(err)
	}
	apply("099_access_rule_identity.sql")

	lineage := func(table, rowID string) string {
		t.Helper()
		return id(`SELECT lineage_id::text FROM model.`+table+` WHERE id = $1::uuid`, rowID)
	}
	same := func(label, table string, ids ...string) {
		t.Helper()
		want := lineage(table, ids[0])
		for _, i := range ids[1:] {
			if got := lineage(table, i); got != want {
				t.Errorf("%s: lineage %s, want %s (shared)", label, got, want)
			}
		}
	}
	distinct := func(label, table string, ids ...string) {
		t.Helper()
		seen := map[string]bool{}
		for _, i := range ids {
			l := lineage(table, i)
			if seen[l] {
				t.Errorf("%s: lineage %s shared, want distinct", label, l)
			}
			seen[l] = true
		}
	}
	same("Region across revisions (case-insensitive)", "dimension_def", regA, regB)
	distinct("dimensions of different identity or model", "dimension_def", regA, chanDim, reg2)
	same("US across revisions", "dimension_member", usA, usB)
	distinct("members of different identity or model", "dimension_member", usA, deA, usLower, web, us2)
	same("revenue across revisions (case-insensitive)", "metric_def", revenueA, revenueB)
	distinct("metrics of different identity or model", "metric_def", revenueA, costA, revenue2)

	ruleLineage := func(ruleID string) *string {
		t.Helper()
		var l *string
		if err := conn.QueryRow(ctx, `SELECT ref_lineage_id::text FROM identity.user_access_rule WHERE id = $1::uuid`, ruleID).Scan(&l); err != nil {
			t.Fatal(err)
		}
		return l
	}
	for _, c := range []struct {
		label, rule, want string
	}{
		{"member rule", ruleUS, lineage("dimension_member", usB)},
		{"metric rule", ruleRevenue, lineage("metric_def", revenueA)},
		{"revision-less member rule", ruleWeb, lineage("dimension_member", web)},
		{"malformed ref_id", ruleMalformed, ""},
		{"row gone", ruleGone, ""},
		{"button", ruleButton, ""},
	} {
		got := ruleLineage(c.rule)
		switch {
		case c.want == "" && got != nil:
			t.Errorf("%s: ref_lineage_id %s, want NULL", c.label, *got)
		case c.want != "" && (got == nil || *got != c.want):
			t.Errorf("%s: ref_lineage_id %v, want %s", c.label, got, c.want)
		}
	}

	// Re-runnable: pkg/migrate applies a file and records it in two separate
	// statements, so a crash between them re-applies 099 on the next start.
	// It must succeed and change nothing the first run settled.
	snapshot := func() string {
		t.Helper()
		return id(`SELECT concat_ws('|',
		    (SELECT string_agg(id::text || '=' || lineage_id::text, ',' ORDER BY id) FROM model.dimension_def),
		    (SELECT string_agg(id::text || '=' || lineage_id::text, ',' ORDER BY id) FROM model.dimension_member),
		    (SELECT string_agg(id::text || '=' || lineage_id::text, ',' ORDER BY id) FROM model.metric_def),
		    (SELECT string_agg(id::text || '=' || COALESCE(ref_lineage_id::text, '-'), ',' ORDER BY id) FROM identity.user_access_rule))`)
	}
	before := snapshot()
	apply("099_access_rule_identity.sql")
	if after := snapshot(); after != before {
		t.Errorf("re-applying 099 changed lineages:\nbefore %s\nafter  %s", before, after)
	}
}

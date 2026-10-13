package migrations_test

import (
	"context"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	"github.com/mavericks-engine/mavericks/migrations"
)

// TestForeignKeysIntoMetricsAndMembersAreIndexed holds 137 in place for
// every future table: a foreign key into model.metric_def or
// model.dimension_member runs once per deleted metric or member when a
// revision goes, so the referencing columns must lead an index — or that
// delete reads the whole referencing table once per row (2026-10-13: a
// 618-metric revision took minutes and was rolled back).
func TestForeignKeysIntoMetricsAndMembersAreIndexed(t *testing.T) {
	pool := testdb.New(t, migrations.FS, ".")
	rows, err := pool.Query(context.Background(), `
		SELECT c.conrelid::regclass::text, c.conname, c.confrelid::regclass::text
		FROM pg_constraint c
		WHERE c.contype = 'f' AND c.conparentid = 0
		  AND c.confrelid IN ('model.metric_def'::regclass, 'model.dimension_member'::regclass)
		  AND NOT EXISTS (
		      SELECT 1 FROM pg_index i
		      WHERE i.indrelid = c.conrelid
		        AND (i.indkey::int2[])[0:array_length(c.conkey, 1) - 1] = c.conkey::int2[])
		ORDER BY 1, 2`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var table, constraint, ref string
		if err := rows.Scan(&table, &constraint, &ref); err != nil {
			t.Fatal(err)
		}
		t.Errorf("%s.%s references %s without an index led by its columns", table, constraint, ref)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

package plan

import (
	"context"
	"testing"

	"github.com/mavericks-engine/mavericks/internal/testdb"
	migrationfs "github.com/mavericks-engine/mavericks/migrations"
)

// An import's staged rows go once nothing will read them: an import left
// unconfirmed for over a day is ended (aborted) with its rows, and the rows
// a committed, failed or aborted import kept go; an import checked minutes
// ago keeps its rows for its confirmation.
func TestReclaimImportStagingEndsLeftImportsAndKeepsAWaitingOne(t *testing.T) {
	ctx := context.Background()
	pool := testdb.New(t, migrationfs.FS, ".")
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := pool.QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	cust := q(`INSERT INTO core.customer (name) VALUES ('Imports') RETURNING id::text`)
	app := q(`INSERT INTO core.application (customer_id, name, mode) VALUES ($1::uuid, 'App', 'planning') RETURNING id::text`, cust)
	model := q(`INSERT INTO core.model (application_id, name) VALUES ($1::uuid, 'M') RETURNING id::text`, app)
	rev := q(`INSERT INTO model.revision (model_id, name) VALUES ($1::uuid, 'Working') RETURNING id::text`, model)
	metric := q(`INSERT INTO model.metric_def (model_id, revision_id, name, is_input) VALUES ($1::uuid, $2::uuid, 'Amount', true) RETURNING id::text`, model, rev)
	job := func(status, age string, rows int) string {
		t.Helper()
		id := q(`INSERT INTO import.import_job (model_id, status, file_url, created_at)
		         VALUES ($1::uuid, $2::import.import_status, 'f.csv', now() - $3::interval) RETURNING id::text`, model, status, age)
		if _, err := pool.Exec(ctx, `INSERT INTO import.import_staging (job_id, metric_id, value, row_number)
		    SELECT $1::uuid, $2::uuid, i, i FROM generate_series(1, $3::int) i`, id, metric, rows); err != nil {
			t.Fatal(err)
		}
		return id
	}
	left := job("staged", "3 days", 5)
	waiting := job("staged", "5 minutes", 4)
	committed := job("committed", "2 hours", 3)
	refused := job("aborted", "1 hour", 2)

	removed, err := ReclaimImportStaging(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 10 {
		t.Errorf("removed %d staged rows, want 10 (the left, committed and aborted imports')", removed)
	}
	for id, want := range map[string]string{left: "0", waiting: "4", committed: "0", refused: "0"} {
		if n := q(`SELECT count(*)::text FROM import.import_staging WHERE job_id = $1::uuid`, id); n != want {
			t.Errorf("import %s keeps %s staged rows, want %s", id, n, want)
		}
	}
	if s := q(`SELECT status::text FROM import.import_job WHERE id = $1::uuid`, left); s != "aborted" {
		t.Errorf("the import left for three days is %s, want aborted", s)
	}
	if s := q(`SELECT status::text FROM import.import_job WHERE id = $1::uuid`, waiting); s != "staged" {
		t.Errorf("the import checked five minutes ago is %s, want staged", s)
	}
}
